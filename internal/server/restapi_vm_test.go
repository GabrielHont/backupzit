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
