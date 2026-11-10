package server_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

// Unirea portal: a Windows agent's disks are listed in the API, so the portal
// can offer "the disk with Windows" or "every internal disk" as an image job.
func TestAPIAgentDisks(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	op, view := apiToken(t, c, "operator"), apiToken(t, c, "viewer")
	target, err := e.store.CreateTarget(e.ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(e.ctx, time.Hour)
	resp, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: token, Hostname: "pc", OS: "windows", Version: "test", Capabilities: []string{api.CapabilityJobOptions}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.store.AuthenticateAgent(e.ctx, resp.AgentUUID, resp.Secret)
	if err != nil {
		t.Fatal(err)
	}
	disks := `[
	 {"number":0,"model":"Samsung SSD","size":512110190592,"sector_size":512,"style":"gpt","system":true,"partitions":[
	  {"number":1,"offset":1048576,"length":104857600,"gpt_type":"C12A7328-F81F-11D2-BA4B-00A0C93EC93B","file_system":"FAT32"},
	  {"number":2,"offset":105906176,"length":16777216,"gpt_type":"E3C9E316-0B5C-4DB8-817D-F92DF00215AE"},
	  {"number":3,"offset":122683392,"length":510000000000,"gpt_type":"EBD0A0A2-B9E5-4433-87C0-68B6B72699C7","mount_points":["C:\\"],"file_system":"NTFS","label":"Windows"},
	  {"number":4,"offset":510122683392,"length":700000000,"gpt_type":"DE94BBA4-06D1-4D40-A16A-BFD50179D6AC","file_system":"NTFS"}]},
	 {"number":1,"model":"WD Blue","size":1000204886016,"sector_size":512,"style":"gpt","partitions":[
	  {"number":1,"offset":1048576,"length":1000000000000,"gpt_type":"EBD0A0A2-B9E5-4433-87C0-68B6B72699C7","mount_points":["D:\\"],"file_system":"NTFS","label":"Date"}]},
	 {"number":2,"model":"USB stick","size":32000000000,"sector_size":512,"style":"mbr","bus":"usb","partitions":[
	  {"number":1,"offset":1048576,"length":31000000000,"mbr_type":12,"bootable":true,"mount_points":["E:\\"],"file_system":"FAT32"}]}]`
	if err := e.store.TouchAgent(e.ctx, a.ID, api.PollRequest{Hostname: "pc", OS: "windows", Version: "test", Disks: json.RawMessage(disks)}, ""); err != nil {
		t.Fatal(err)
	}

	code, body := apiJSONCall(t, e, view, "GET", fmt.Sprintf("/api/v1/agents/%d", a.ID), "")
	var agent struct {
		Disks []struct {
			Number     int
			Size       uint64
			System     bool
			Bus        string
			InAll      bool `json:"in_all_disks"`
			Partitions []struct {
				Number      int
				Size        uint64
				Role        string
				MountPoints []string `json:"mount_points"`
				Label       string
				Bootable    bool
			}
		}
	}
	if json.Unmarshal([]byte(body), &agent) != nil || code != 200 || len(agent.Disks) != 3 {
		t.Fatalf("agent disks: %d %s", code, body)
	}
	sys, data, usb := agent.Disks[0], agent.Disks[1], agent.Disks[2]
	roles := ""
	for _, p := range sys.Partitions {
		roles += p.Role + " "
	}
	if !sys.System || !sys.InAll || roles != "efi msr data recovery " || sys.Partitions[2].MountPoints[0] != `C:\` ||
		sys.Partitions[2].Label != "Windows" || sys.Partitions[2].Size != 510000000000 {
		t.Fatalf("system disk: %+v", sys)
	}
	if data.System || !data.InAll || data.Partitions[0].MountPoints[0] != `D:\` {
		t.Fatalf("data disk: %+v", data)
	}
	if usb.InAll || usb.Bus != "usb" || usb.Partitions[0].Role != "data" || !usb.Partitions[0].Bootable || sys.Partitions[0].Bootable {
		t.Fatalf("usb disk: %+v", usb)
	}

	// Both choices of the portal are accepted as image jobs.
	job := func(sel string) (int, string) {
		return apiJSONCall(t, e, op, "POST", "/api/v1/jobs", fmt.Sprintf(`{"kind":"image","name":"PC","agent_id":%d,"target_id":%d,"schedule":"0 1 * * *",%s}`, a.ID, target, sel))
	}
	if code, body := job(`"image_disk":0`); code != 201 {
		t.Fatalf("windows disk job: %d %s", code, body)
	}
	if code, body := job(`"image_disk":-1`); code != 201 {
		t.Fatalf("all disks job: %d %s", code, body)
	}
	if code, body := job(`"image_disk":7`); code != 400 {
		t.Fatalf("unknown disk accepted: %d %s", code, body)
	}

}
