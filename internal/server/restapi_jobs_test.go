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

func TestAPITargetsAndJobs(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	admin, op, view := apiToken(t, c, "admin"), apiToken(t, c, "operator"), apiToken(t, c, "viewer")
	targetBody := `{"name":"portal S3","endpoint":"s3.example.test:9000","bucket":"backups","prefix":"firm","region":"eu-test-1","accessKey":"AK-test","secretKey":"SK-test","tls":true,"lockDays":30}`
	if code, _ := apiJSONCall(t, e, op, "POST", "/api/v1/targets", targetBody); code != 403 {
		t.Fatalf("operator created target: %d", code)
	}
	code, body := apiJSONCall(t, e, admin, "POST", "/api/v1/targets", targetBody)
	var target struct {
		ID          int64
		RecoveryKey string
	}
	if json.Unmarshal([]byte(body), &target) != nil || code != 201 || target.ID == 0 || target.RecoveryKey == "" {
		t.Fatalf("target: %d %s", code, body)
	}
	var storedKey, storedSecret string
	if err := e.pool.QueryRow(e.ctx, `SELECT repo_password,s3_secret_key FROM storage_targets WHERE id=$1`, target.ID).Scan(&storedKey, &storedSecret); err != nil {
		t.Fatal(err)
	}
	if storedKey == target.RecoveryKey || storedSecret == "SK-test" {
		t.Fatal("target secrets stored in plaintext")
	}
	if code, body := apiJSONCall(t, e, view, "GET", "/api/v1/targets", ""); code != 200 || strings.Contains(body, target.RecoveryKey) || strings.Contains(body, "SK-test") || strings.Contains(body, "AK-test") {
		t.Fatalf("target leak: %d %s", code, body)
	}
	token, _, _ := e.store.CreateEnrollmentToken(e.ctx, time.Hour)
	resp, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: token, Hostname: "linux", OS: "Linux", Version: "test", Capabilities: []string{api.CapabilityJobOptions}})
	if err != nil {
		t.Fatal(err)
	}
	ag, err := e.store.AuthenticateAgent(e.ctx, resp.AgentUUID, resp.Secret)
	if err != nil {
		t.Fatal(err)
	}
	jobBody := fmt.Sprintf(`{"name":"Files","agent_id":%d,"target_id":%d,"paths":["/data"],"excludes":["*.tmp"],"schedule":"0 2 * * *","retention":{"keep_last":3,"keep_daily":7,"keep_weekly":4,"keep_monthly":12,"keep_yearly":5}}`, ag.ID, target.ID)
	if code, _ := apiJSONCall(t, e, view, "POST", "/api/v1/jobs", jobBody); code != 403 {
		t.Fatal("viewer created job")
	}
	linuxImage := fmt.Sprintf(`{"kind":"image","name":"Invalid Linux image","agent_id":%d,"target_id":%d,"image_disk":0}`, ag.ID, target.ID)
	if code, b := apiJSONCall(t, e, op, "POST", "/api/v1/jobs", linuxImage); code != 400 {
		t.Fatalf("API accepted Linux image: %d %s", code, b)
	}
	code, body = apiJSONCall(t, e, op, "POST", "/api/v1/jobs", jobBody)
	var job struct{ ID int64 }
	if json.Unmarshal([]byte(body), &job) != nil || code != 201 || job.ID == 0 {
		t.Fatalf("job: %d %s", code, body)
	}
	path := fmt.Sprintf("/api/v1/jobs/%d", job.ID)
	if code, b := apiJSONCall(t, e, view, "GET", path, ""); code != 200 || b != body {
		t.Fatalf("create/get mismatch: %d %s != %s", code, b, body)
	}
	if code, b := apiJSONCall(t, e, op, "PATCH", path, `{"options":{"pre_command":"echo forbidden"}}`); code != 403 {
		t.Fatalf("operator commands: %d %s", code, b)
	}
	if code, b := apiJSONCall(t, e, op, "PATCH", path, `{"name":"Renamed","retention":{"keep_yearly":6}}`); code != 200 || !strings.Contains(b, `"keep_yearly":6`) {
		t.Fatalf("patch: %d %s", code, b)
	}
	if code, b := apiJSONCall(t, e, admin, "DELETE", fmt.Sprintf("/api/v1/targets/%d", target.ID), ""); code != 409 {
		t.Fatalf("deleted used target: %d %s", code, b)
	}
	if err := e.store.SetSetting(e.ctx, "four_eyes", true); err != nil {
		t.Fatal(err)
	}
	if code, b := apiJSONCall(t, e, op, "PATCH", path, `{"name":"Safe changes saved","retention":{"keep_yearly":1}}`); code != 409 {
		t.Fatalf("short retention: %d %s", code, b)
	}
	j, _ := e.store.GetJob(e.ctx, job.ID)
	if j.Name != "Safe changes saved" || j.Retention.KeepYearly != 6 {
		t.Fatalf("approval did not hold retention: %+v", j)
	}
	pending, _, err := e.store.ListApprovals(e.ctx)
	if err != nil || len(pending) != 1 || pending[0].RequestedBy != "admin" {
		t.Fatalf("approval identity: %+v %v", pending, err)
	}
	if code, b := apiJSONCall(t, e, op, "DELETE", path, ""); code != 409 {
		t.Fatalf("delete without approval: %d %s", code, b)
	}
	if _, err := e.store.GetJob(e.ctx, job.ID); err != nil {
		t.Fatal("pending delete already applied")
	}
	if err := e.store.SetSetting(e.ctx, "four_eyes", false); err != nil {
		t.Fatal(err)
	}
	if code, b := apiJSONCall(t, e, op, "DELETE", path, ""); code != 204 {
		t.Fatalf("delete: %d %s", code, b)
	}
	if code, b := apiJSONCall(t, e, admin, "DELETE", fmt.Sprintf("/api/v1/targets/%d", target.ID), ""); code != 204 {
		t.Fatalf("target delete: %d %s", code, b)
	}
	// An image can select all disks (-1), or an explicit whole-disk list.
	tid, err := e.store.CreateTarget(e.ctx, server.Target{Name: "local image", Kind: "local", URL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	wr, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: token, Hostname: "windows", OS: "Windows 11", Version: "0.47.3", Capabilities: []string{api.CapabilityJobOptions}})
	if err != nil {
		t.Fatal(err)
	}
	win, err := e.store.AuthenticateAgent(e.ctx, wr.AgentUUID, wr.Secret)
	if err != nil {
		t.Fatal(err)
	}
	for i, selection := range []string{`"image_disk":-1`, `"options":{"image_disks":[0,2],"vss":false}`} {
		b := fmt.Sprintf(`{"kind":"image","agent_id":%d,"target_id":%d,"name":"Image %d",%s}`, win.ID, tid, i, selection)
		code, b := apiJSONCall(t, e, op, "POST", "/api/v1/jobs", b)
		if code != 201 {
			t.Fatalf("image: %d %s", code, b)
		}
		var created struct{ ID int64 }
		if err := json.Unmarshal([]byte(b), &created); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.QueueBackup(e.ctx, created.ID, "api"); err != nil {
			t.Fatal(err)
		}
		code, b = apiJSONCall(t, e, wr.AgentUUID+":"+wr.Secret, "POST", "/api/agent/poll", `{"hostname":"windows","os":"Windows 11","arch":"amd64","version":"0.47.3","capabilities":["job-options-v1"],"instance":"image-api-test"}`)
		var poll api.PollResponse
		if code != 200 || json.Unmarshal([]byte(b), &poll) != nil || poll.Run == nil {
			t.Fatalf("image dispatch: %d %s", code, b)
		}
		if i == 1 && (len(poll.Run.ImageDisks) != 2 || poll.Run.VSS == nil || *poll.Run.VSS || len(poll.Run.ImagePartitions) != 0) {
			t.Fatalf("lost disk list or VSS override: %+v", poll.Run)
		}
		if err := e.store.FinishRun(e.ctx, win.ID, poll.Run.ID, api.RunResult{Status: api.StatusFailed, Message: "dispatch tested"}); err != nil {
			t.Fatal(err)
		}
	}
	// A downgraded/legacy agent must clear stale support on its next poll.
	if err := e.store.TouchAgent(e.ctx, win.ID, api.PollRequest{OS: "Windows 11", Version: "0.47.3"}, ""); err != nil {
		t.Fatal(err)
	}
	legacy, err := e.store.GetAgent(e.ctx, win.ID)
	if err != nil || len(legacy.Capabilities) != 0 {
		t.Fatalf("stale capability: %+v %v", legacy, err)
	}
	unsupported := fmt.Sprintf(`{"kind":"image","agent_id":%d,"target_id":%d,"name":"Unsupported list","options":{"image_disks":[0,2]}}`, win.ID, tid)
	if code, b := apiJSONCall(t, e, op, "POST", "/api/v1/jobs", unsupported); code != 400 {
		t.Fatalf("legacy agent accepted extended options: %d %s", code, b)
	}
}
