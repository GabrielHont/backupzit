package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
	"github.com/max-zit/backupzit/internal/testutil"
)

// Unirea: while the firm's backup space is full, backups end at once as
// failed and other runs still go out; a newer measurement under the limit
// (or a larger limit) lets backups run again.
func TestStorageLimitPausesBackups(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	s3, err := testutil.StartS3Server("backups")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s3.Close)
	targetID, err := e.store.CreateTarget(ctx, server.Target{Name: "s3", Kind: "s3", URL: "s3://" + s3.Host + "/backups/files?tls=false", S3AccessKey: "AK", S3SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.47.3")
	if err != nil {
		t.Fatal(err)
	}
	poll := func() *api.Run {
		t.Helper()
		b, _ := json.Marshal(api.PollRequest{Hostname: "PC-1", OS: "Windows 11", Arch: "amd64", Version: "0.47.3"})
		req, _ := http.NewRequest("POST", e.ts.URL+api.PathPoll, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var pr api.PollResponse
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&pr) != nil {
			t.Fatalf("poll: %d", resp.StatusCode)
		}
		return pr.Run
	}
	poll()
	agents, _ := e.store.ListAgents(ctx)
	a := agents[0]
	jobID, err := e.store.CreateJob(ctx, server.Job{AgentID: a.ID, TargetID: targetID, Name: "Docs", Paths: []string{t.TempDir()}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	admin, viewer := apiToken(t, c, "admin"), apiToken(t, c, "viewer")
	put := func(body string) (int, string) { return apiJSONCall(t, e, admin, "PUT", "/api/v1/limits", body) }
	at := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339Nano) }

	for _, body := range []string{
		`{"storage":{"limitBytes":10,"usedBytes":1,"measuredAt":"` + at(0) + `"}}`,
		`{"maxAgents":3,"storage":{"limitBytes":0,"usedBytes":1,"measuredAt":"` + at(0) + `"}}`,
		`{"maxAgents":3,"storage":{"limitBytes":10,"usedBytes":-1,"measuredAt":"` + at(0) + `"}}`,
		`{"maxAgents":3,"storage":{"limitBytes":10,"measuredAt":"` + at(0) + `"}}`,
		`{"maxAgents":3,"storage":{"limitBytes":10,"usedBytes":1}}`,
		`{"maxAgents":3,"storage":{"usedBytes":1,"measuredAt":"` + at(0) + `"}}`,
		`{"maxAgents":3,"storage":{"limitBytes":10,"usedBytes":1,"measuredAt":"` + at(time.Hour) + `"}}`,
		`{"maxAgents":3,"storage":{"limitBytes":10,"usedBytes":1,"measuredAt":"` + at(0) + `","x":1}}`,
		`{"maxAgents":3,"other":1}`,
	} {
		if code, b := put(body); code != 400 {
			t.Fatalf("invalid %s: %d %s", body, code, b)
		}
	}
	// Never measured: backups run as before.
	if code, b := apiJSONCall(t, e, viewer, "GET", "/api/v1/limits", ""); code != 200 || strings.Contains(b, "storage") {
		t.Fatalf("limits before a measurement: %d %s", code, b)
	}

	// Full: 12 GB of 10 GB.
	if code, b := put(`{"maxAgents":3,"storage":{"limitBytes":10000000000,"usedBytes":12000000000,"measuredAt":"` + at(-time.Minute) + `"}}`); code != 200 || !strings.Contains(b, `"full":true`) {
		t.Fatalf("set full: %d %s", code, b)
	}
	backupID, err := e.store.QueueBackup(ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	logID, err := e.store.QueueAgentLog(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := poll()
	if run == nil || run.ID != logID {
		t.Fatalf("other runs must still go out in the same poll, got %+v", run)
	}
	r, err := e.store.GetRun(ctx, backupID)
	if err != nil || r.Status != "failed" || !strings.HasPrefix(r.Message, "backup storage is full: ") {
		t.Fatalf("refused backup: %+v %v", r, err)
	}
	e.store.FinishRun(ctx, a.ID, logID, api.RunResult{Status: api.StatusSuccess})

	// A measurement sent late (older) does not replace the newer one.
	if code, b := put(`{"maxAgents":3,"storage":{"limitBytes":10000000000,"usedBytes":1,"measuredAt":"` + at(-time.Hour) + `"}}`); code != 200 || !strings.Contains(b, `"full":true`) {
		t.Fatalf("an older measurement replaced the newer one: %d %s", code, b)
	}
	// Only maxAgents (an older portal, or the device limit alone): storage stays.
	if code, b := put(`{"maxAgents":4}`); code != 200 || !strings.Contains(b, `"full":true`) || !strings.Contains(b, `"maxAgents":4`) {
		t.Fatalf("maxAgents alone: %d %s", code, b)
	}
	if _, err := e.store.QueueBackup(ctx, jobID, "manual"); err != nil {
		t.Fatal(err)
	}
	if run := poll(); run != nil {
		t.Fatalf("a backup went out while full: %+v", run)
	}

	// A larger plan (same measurement): backups run again.
	if code, b := put(`{"maxAgents":4,"storage":{"limitBytes":20000000000,"usedBytes":12000000000,"measuredAt":"` + at(-time.Minute) + `"}}`); code != 200 || !strings.Contains(b, `"full":false`) {
		t.Fatalf("raise limit: %d %s", code, b)
	}
	id, err := e.store.QueueBackup(ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if run := poll(); run == nil || run.ID != id || run.Kind != api.KindBackup {
		t.Fatalf("backup after the limit was raised: %+v", run)
	}
	// Unlimited (null) never pauses.
	if code, b := put(`{"maxAgents":4,"storage":{"limitBytes":null,"usedBytes":99000000000,"measuredAt":"` + at(0) + `"}}`); code != 200 || !strings.Contains(b, `"full":false`) {
		t.Fatalf("unlimited: %d %s", code, b)
	}
}
