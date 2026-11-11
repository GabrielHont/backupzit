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

// Unirea portal: files inside a Windows disk image are listed and restored
// through the API, onto any Windows computer of the console.
func TestAPIImageFiles(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	op, view := apiToken(t, c, "operator"), apiToken(t, c, "viewer")
	target, err := e.store.CreateTarget(e.ctx, server.Target{Name: "local", Kind: "local", URL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(host, osName string) int64 {
		token, _, _ := e.store.CreateEnrollmentToken(e.ctx, time.Hour)
		resp, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: token, Hostname: host, OS: osName, Version: "test"})
		if err != nil {
			t.Fatal(err)
		}
		a, err := e.store.AuthenticateAgent(e.ctx, resp.AgentUUID, resp.Secret)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.store.TouchAgent(e.ctx, a.ID, api.PollRequest{Hostname: host, OS: osName, Version: "test"}, ""); err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	old, fresh, linux := enroll("PC-VECHI", "windows"), enroll("PC-NOU", "windows"), enroll("srv", "linux")

	disk := func(n int) *int { return &n }
	jobID, err := e.store.CreateJob(e.ctx, server.Job{Kind: server.JobImage, AgentID: old, TargetID: target, Name: "PC", ImageDisk: disk(0)})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := e.store.QueueBackup(e.ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := e.store.ClaimRun(e.ctx, old); err != nil || claimed == nil || claimed.ID != runID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	details := `{"number":0,"model":"Msft Virtual Disk","size":146028888064,"sector_size":512,"style":"gpt","head":"` + strings.Repeat("ab", 32) + `",
	  "partitions":[{"number":1,"offset":1048576,"length":104857600,"file_system":"FAT32","included":true},
	                {"number":3,"offset":122683392,"length":100000000000,"mount_points":["C:\\"],"label":"Windows","file_system":"NTFS","included":true},
	                {"number":4,"offset":100122683392,"length":45000000000,"mount_points":["E:\\"],"file_system":"NTFS","included":true},
	                {"number":5,"offset":145122683392,"length":524288000,"file_system":"NTFS","included":false}]}`
	if err := e.store.FinishRun(e.ctx, old, runID, api.RunResult{Status: api.StatusSuccess, SnapshotID: strings.Repeat("cd", 32), Details: json.RawMessage(details)}); err != nil {
		t.Fatal(err)
	}

	// The layout: C: and E: can be browsed; the EFI (FAT32) and the excluded partition cannot.
	code, body := apiJSONCall(t, e, view, "GET", fmt.Sprintf("/api/v1/runs/%d/image", runID), "")
	var disks []struct {
		Index      int
		Number     int
		Partitions []struct {
			Ref         int
			MountPoints []string `json:"mount_points"`
			Label       string
			Browsable   bool
		}
	}
	if json.Unmarshal([]byte(body), &disks) != nil || code != 200 || len(disks) != 1 || len(disks[0].Partitions) != 4 {
		t.Fatalf("image: %d %s", code, body)
	}
	got := ""
	for _, p := range disks[0].Partitions {
		got += fmt.Sprintf("%d:%v:%v ", p.Ref, p.MountPoints, p.Browsable)
	}
	if got != "1:[]:false 3:[C:\\]:true 4:[E:\\]:true 5:[]:false " {
		t.Fatalf("partitions: %s", got)
	}
	// A run without an image is refused.
	if code, body := apiJSONCall(t, e, view, "GET", "/api/v1/runs/999999/image", ""); code != 404 {
		t.Fatalf("missing run: %d %s", code, body)
	}

	files := func(q string) (int, string) {
		return apiJSONCall(t, e, op, "GET", fmt.Sprintf("/api/v1/runs/%d/image/files?%s", runID, q), "")
	}
	// Listing files is a restore right: a viewer sees the layout, not the files.
	if code, _ := apiJSONCall(t, e, view, "GET", fmt.Sprintf("/api/v1/runs/%d/image/files?part=3", runID), ""); code != 403 {
		t.Fatalf("viewer files: %d", code)
	}
	for _, q := range []string{"", "part=1", "part=5", "part=3&path=/Users/../../Windows"} {
		if code, body := files(q); code != 400 {
			t.Fatalf("files %q: %d %s", q, code, body)
		}
	}

	restore := func(body string) (int, string) {
		return apiJSONCall(t, e, op, "POST", fmt.Sprintf("/api/v1/runs/%d/image/restore", runID), body)
	}
	refused := []string{
		fmt.Sprintf(`{"part":1,"paths":["/"],"agent_id":%d,"target":{"directory":"C:\\Recuperat"}}`, fresh),
		fmt.Sprintf(`{"part":3,"paths":[],"agent_id":%d,"target":{"directory":"C:\\Recuperat"}}`, fresh),
		fmt.Sprintf(`{"part":3,"paths":["/Users/../.."],"agent_id":%d,"target":{"directory":"C:\\Recuperat"}}`, fresh),
		fmt.Sprintf(`{"part":3,"paths":["/Users/ana"],"agent_id":%d,"target":{"directory":"/srv"}}`, linux),
		fmt.Sprintf(`{"part":3,"paths":["/Users/ana"],"agent_id":%d,"target":{"directory":""}}`, fresh),
	}
	for _, b := range refused {
		if code, body := restore(b); code != 400 {
			t.Fatalf("restore %s: %d %s", b, code, body)
		}
	}
	// A viewer cannot restore.
	if code, _ := apiJSONCall(t, e, view, "POST", fmt.Sprintf("/api/v1/runs/%d/image/restore", runID), `{"part":3,"paths":["/"]}`); code != 403 {
		t.Fatalf("viewer restore: %d", code)
	}

	// Onto the new computer, into a separate folder.
	code, body = restore(fmt.Sprintf(`{"part":4,"paths":["/Contabilitate","Users\\ana\\Desktop"],"agent_id":%d,"target":{"directory":"C:\\Recuperat\\PC-VECHI"}}`, fresh))
	var run struct {
		ID      int64
		Kind    string
		AgentID int64 `json:"agent_id"`
		Status  string
	}
	if json.Unmarshal([]byte(body), &run) != nil || code != 202 || run.Kind != api.KindImageFileRestore || run.AgentID != fresh || run.Status != "queued" {
		t.Fatalf("restore: %d %s", code, body)
	}
	queued, err := e.store.GetRun(e.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(queued.Paths, "|") != `\Contabilitate|\Users\ana\Desktop` || queued.RestoreTarget != `C:\Recuperat\PC-VECHI` ||
		len(queued.ImagePartitions) != 1 || queued.ImagePartitions[0] != 4 || queued.SnapshotID != strings.Repeat("cd", 32) {
		t.Fatalf("queued run: %+v", queued)
	}
	// "original" keeps the old paths; agent_id 0 means the computer the image came from.
	if code, body := restore(`{"part":3,"paths":["/"],"target":"original"}`); code != 202 || !strings.Contains(body, fmt.Sprintf(`"agent_id":%d`, old)) {
		t.Fatalf("original restore: %d %s", code, body)
	}
}
