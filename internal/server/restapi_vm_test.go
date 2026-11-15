package server_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// Unirea portal: virtual machine jobs on Proxmox VE nodes and single Hyper-V
// hosts through the REST API; clusters and plain agents are refused.
func TestAPIVirtualMachineJobs(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	op, view := apiToken(t, c, "operator"), apiToken(t, c, "viewer")
	target, err := e.store.CreateTarget(e.ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(host, osName, inv string) int64 {
		token, _, _ := e.store.CreateEnrollmentToken(e.ctx, time.Hour)
		resp, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: token, Hostname: host, OS: osName, Version: "test", Capabilities: []string{api.CapabilityJobOptions}})
		if err != nil {
			t.Fatal(err)
		}
		a, err := e.store.AuthenticateAgent(e.ctx, resp.AgentUUID, resp.Secret)
		if err != nil {
			t.Fatal(err)
		}
		req := api.PollRequest{Hostname: host, OS: osName, Version: "test"}
		if inv != "" {
			req.Hypervisor = json.RawMessage(inv)
		}
		if err := e.store.TouchAgent(e.ctx, a.ID, req, ""); err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	pve := enroll("pve1", "Linux", `{"node":"pve1","guests":[{"vmid":100,"name":"erp","type":"qemu","node":"pve1","status":"running","maxdisk":34359738368,"maxmem":4294967296},{"vmid":101,"name":"dns","type":"lxc","node":"pve1","status":"stopped"},{"vmid":200,"name":"other-node","type":"qemu","node":"pve2","status":"running"}],"storage":["local-lvm"]}`)
	hv := enroll("HV9", "Windows Server 2022", `{"platform":"hyperv","node":"HV9","guests":[{"vmid":1000000009,"id":"guid-lab","name":"lab","type":"qemu","node":"HV9","status":"running"}],"storage":["D:\\VMs"]}`)
	clustered := enroll("HV1", "Windows Server 2022", `{"platform":"hyperv","node":"HV1","cluster_name":"HVC","guests":[{"vmid":1000000001,"id":"guid-web","name":"web","type":"qemu","node":"HV1","status":"running","clustered":true}],"storage":["C:\\ClusterStorage\\Volume1"]}`)
	plain := enroll("pc", "Windows 11", "")

	// The agent list carries each node's own guests, nothing from other nodes.
	code, body := apiJSONCall(t, e, view, "GET", fmt.Sprintf("/api/v1/agents/%d", pve), "")
	var agent struct {
		Hypervisor *struct {
			Platform string
			Node     string
			Guests   []struct {
				ID, Name, Type string
				DiskBytes      uint64 `json:"disk_bytes"`
			}
		}
	}
	if json.Unmarshal([]byte(body), &agent) != nil || code != 200 || agent.Hypervisor == nil || agent.Hypervisor.Platform != "proxmox" ||
		len(agent.Hypervisor.Guests) != 2 || agent.Hypervisor.Guests[0].ID != "100" || agent.Hypervisor.Guests[0].DiskBytes != 34359738368 {
		t.Fatalf("pve agent: %d %s", code, body)
	}
	if code, body := apiJSONCall(t, e, view, "GET", fmt.Sprintf("/api/v1/agents/%d", plain), ""); code != 200 || strings.Contains(body, "hypervisor") {
		t.Fatalf("plain agent: %d %s", code, body)
	}

	job := func(agentID int64, extra string) string {
		return fmt.Sprintf(`{"kind":"vm","name":"VMs","agent_id":%d,"target_id":%d,"schedule":"0 1 * * *"%s}`, agentID, target, extra)
	}
	refused := map[string]string{
		"plain agent":         job(plain, `,"paths":["*"]`),
		"failover cluster":    job(clustered, `,"paths":["*"]`),
		"guest on other node": job(pve, `,"paths":["200"]`),
		"unknown guest":       job(pve, `,"paths":["999"]`),
		"no guests":           job(pve, `,"paths":[]`),
		"excludes with list":  job(pve, `,"paths":["100"],"excludes":["101"]`),
		"image disk":          job(pve, `,"paths":["*"],"image_disk":0`),
		"vss":                 job(pve, `,"paths":["*"],"options":{"vss":true}`),
	}
	for name, b := range refused {
		if code, out := apiJSONCall(t, e, op, "POST", "/api/v1/jobs", b); code != 400 {
			t.Errorf("%s accepted: %d %s", name, code, out)
		}
	}
	if code, out := apiJSONCall(t, e, view, "POST", "/api/v1/jobs", job(pve, `,"paths":["*"]`)); code != 403 {
		t.Fatalf("viewer created vm job: %d %s", code, out)
	}
	code, body = apiJSONCall(t, e, op, "POST", "/api/v1/jobs", job(pve, `,"paths":["*"],"excludes":["101"]`))
	var created struct {
		ID       int64
		Kind     string
		Paths    []string
		Excludes []string
	}
	if json.Unmarshal([]byte(body), &created) != nil || code != 201 || created.Kind != "vm" || created.Paths[0] != "*" || created.Excludes[0] != "101" {
		t.Fatalf("vm job: %d %s", code, body)
	}
	path := fmt.Sprintf("/api/v1/jobs/%d", created.ID)
	if code, out := apiJSONCall(t, e, op, "PATCH", path, `{"paths":["100","101"],"excludes":[]}`); code != 200 || !strings.Contains(out, `"paths":["100","101"]`) {
		t.Fatalf("patch guests: %d %s", code, out)
	}
	if code, out := apiJSONCall(t, e, op, "PATCH", path, `{"paths":["200"]}`); code != 400 {
		t.Fatalf("patched to another node's guest: %d %s", code, out)
	}
	code, body = apiJSONCall(t, e, op, "POST", "/api/v1/jobs", job(hv, `,"paths":["1000000009"]`))
	if code != 201 || !strings.Contains(body, `"kind":"vm"`) {
		t.Fatalf("hyper-v job: %d %s", code, body)
	}
	if code, out := apiJSONCall(t, e, op, "POST", path+"/run", ""); code != 202 && code != 200 && code != 201 {
		t.Fatalf("run vm job: %d %s", code, out)
	}
}

// Unirea portal: a virtual machine brought back from a VM backup through the
// REST API, as a second VM or in place of the original.
func TestAPIVirtualMachineRestore(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	op, view := apiToken(t, c, "operator"), apiToken(t, c, "viewer")
	target, err := e.store.CreateTarget(e.ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(host, osName, inv string) int64 {
		token, _, _ := e.store.CreateEnrollmentToken(e.ctx, time.Hour)
		resp, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: token, Hostname: host, OS: osName, Version: "test", Capabilities: []string{api.CapabilityJobOptions}})
		if err != nil {
			t.Fatal(err)
		}
		a, err := e.store.AuthenticateAgent(e.ctx, resp.AgentUUID, resp.Secret)
		if err != nil {
			t.Fatal(err)
		}
		req := api.PollRequest{Hostname: host, OS: osName, Version: "test"}
		if inv != "" {
			req.Hypervisor = json.RawMessage(inv)
		}
		if err := e.store.TouchAgent(e.ctx, a.ID, req, ""); err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	hv := enroll("HV9", "Windows Server 2025", `{"platform":"hyperv","node":"HV9","guests":[{"vmid":1000000009,"id":"guid-lab","name":"lab","type":"qemu","node":"HV9","status":"stopped"}],"storage":["D:\\VMs"]}`)
	pc := enroll("pc", "Windows 11", "")
	code, body := apiJSONCall(t, e, op, "POST", "/api/v1/jobs",
		fmt.Sprintf(`{"kind":"vm","name":"VMs","agent_id":%d,"target_id":%d,"schedule":"0 1 * * *","paths":["1000000009"]}`, hv, target))
	var job struct{ ID int64 }
	if code != 201 || json.Unmarshal([]byte(body), &job) != nil {
		t.Fatalf("vm job: %d %s", code, body)
	}
	backup := func(status string) int64 {
		id, err := e.store.QueueBackup(e.ctx, job.ID, "manual")
		if err != nil {
			t.Fatal(err)
		}
		if r, err := e.store.ClaimRun(e.ctx, hv); err != nil || r == nil || r.ID != id {
			t.Fatalf("claim backup: %v %+v", err, r)
		}
		res := api.RunResult{Status: status, SnapshotID: strings.Repeat("c3", 32),
			Details: json.RawMessage(`{"guests":[{"vmid":1000000009,"type":"qemu","name":"lab","disks":1,"size":161061273600,"platform":"hyperv"}]}`)}
		if status == api.StatusFailed {
			res.SnapshotID, res.Message = "", "x"
		}
		if err := e.store.FinishRun(e.ctx, hv, id, res); err != nil {
			t.Fatal(err)
		}
		return id
	}
	run := backup(api.StatusSuccess)
	failed := backup(api.StatusFailed)

	// The run lists the VMs it holds.
	code, body = apiJSONCall(t, e, view, "GET", fmt.Sprintf("/api/v1/runs/%d", run), "")
	if code != 200 || !strings.Contains(body, `"guests":[{"vmid":1000000009,"name":"lab","type":"qemu","platform":"hyperv","disks":1,"size":161061273600}]`) {
		t.Fatalf("run guests: %d %s", code, body)
	}

	restore := func(token string, runID int64, req string) (int, string) {
		return apiJSONCall(t, e, token, "POST", fmt.Sprintf("/api/v1/runs/%d/vm-restore", runID), req)
	}
	queued := func(body string) *server.Run {
		var x struct {
			ID   int64
			Kind string
		}
		if json.Unmarshal([]byte(body), &x) != nil || x.Kind != "vm-restore" {
			t.Fatalf("queued: %s", body)
		}
		r, err := e.store.GetRun(e.ctx, x.ID)
		if err != nil {
			t.Fatal(err)
		}
		return &r
	}
	options := func(r *server.Run) api.VMRestore {
		var o api.VMRestore
		if err := json.Unmarshal(r.VMRestore, &o); err != nil {
			t.Fatal(err)
		}
		return o
	}

	// A second VM next to the original, on the host of the backup.
	code, body = restore(op, run, `{"vmid":1000000009,"mode":"new"}`)
	if code != 202 {
		t.Fatalf("restore as new: %d %s", code, body)
	}
	r := queued(body)
	if o := options(r); r.AgentID != hv || o.VMID != 1000000009 || o.NewVMID != -1 || o.Overwrite || o.Start || r.SnapshotID != strings.Repeat("c3", 32) {
		t.Fatalf("as new: agent %d %+v", r.AgentID, o)
	}
	// In place of the original, started afterwards.
	code, body = restore(op, run, `{"vmid":1000000009,"mode":"replace","start":true,"agent_id":`+fmt.Sprint(hv)+`}`)
	if code != 202 {
		t.Fatalf("restore in place: %d %s", code, body)
	}
	if o := options(queued(body)); o.NewVMID != 0 || !o.Overwrite || !o.Start {
		t.Fatalf("in place: %+v", o)
	}

	refused := []struct {
		name  string
		token string
		run   int64
		req   string
		code  int
	}{
		{"viewer", view, run, `{"vmid":1000000009,"mode":"new"}`, 403},
		{"no mode", op, run, `{"vmid":1000000009}`, 400},
		{"unknown mode", op, run, `{"vmid":1000000009,"mode":"clone"}`, 400},
		{"vm not in the backup", op, run, `{"vmid":42,"mode":"new"}`, 409},
		{"not a hypervisor", op, run, `{"vmid":1000000009,"mode":"new","agent_id":` + fmt.Sprint(pc) + `}`, 409},
		{"bad name", op, run, `{"vmid":1000000009,"mode":"new","name":"a/b"}`, 409},
		{"unknown agent", op, run, `{"vmid":1000000009,"mode":"new","agent_id":999999}`, 400},
		{"failed backup", op, failed, `{"vmid":1000000009,"mode":"new"}`, 409},
	}
	for _, x := range refused {
		if code, out := restore(x.token, x.run, x.req); code != x.code {
			t.Errorf("%s: %d %s", x.name, code, out)
		}
	}
	// A run that does not exist.
	if code, out := apiJSONCall(t, e, op, "POST", "/api/v1/runs/999999/vm-restore", `{"vmid":1,"mode":"new"}`); code != 404 {
		t.Errorf("missing run: %d %s", code, out)
	}
}
