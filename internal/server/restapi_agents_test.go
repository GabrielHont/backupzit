package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/server"
)

func enrollAPITestAgent(t *testing.T, e *env, hostname string) (server.Agent, api.EnrollResponse) {
	t.Helper()
	token, _, err := e.store.CreateEnrollmentToken(e.ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: token, Hostname: hostname, OS: "Windows 11", Arch: "amd64", Version: "0.47.3"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.store.AuthenticateAgent(e.ctx, resp.AgentUUID, resp.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return a, resp
}

func TestAPIAgentPackages(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	if !c.login("admin", "admin-pass-123") {
		t.Fatal("login")
	}
	token := apiToken(t, c, "viewer")
	e.srv.DistDir = t.TempDir()
	name, body := "backupzit-agent-0.47.3-x64.msi", "MSI fixture"
	if err := os.WriteFile(filepath.Join(e.srv.DistDir, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.srv.DistDir, "backupzit-server_0.47.3_amd64.deb"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, b := apiJSONCall(t, e, "", "GET", "/api/v1/agent-packages", ""); code != 401 {
		t.Fatalf("unauthenticated catalog: %d %s", code, b)
	}
	code, b := apiJSONCall(t, e, token, "GET", "/api/v1/agent-packages", "")
	var packages []struct {
		File, OS, Arch, Kind, Version, SHA256 string
		Size                                  int64
	}
	sum := sha256.Sum256([]byte(body))
	if code != 200 || json.Unmarshal([]byte(b), &packages) != nil || len(packages) != 1 || packages[0].File != name || packages[0].SHA256 != hex.EncodeToString(sum[:]) || packages[0].Size != int64(len(body)) {
		t.Fatalf("catalog: %d %s", code, b)
	}
	// A new computer needs neither the API token nor a console session.
	resp, err := e.ts.Client().Get(e.ts.URL + "/agent/" + packages[0].File)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 || string(got) != body || resp.Header.Get("X-Content-SHA256") != packages[0].SHA256 {
		t.Fatalf("public download: %d %q %v", resp.StatusCode, got, err)
	}
	anon := newClient(t, e)
	if code, loc, _ := anon.do("GET", "/downloads/"+name, nil); code != 303 || !strings.HasPrefix(loc, "/login") {
		t.Fatalf("legacy UI download no longer needs a session: %d %s", code, loc)
	}
	if code, _, b := c.do("GET", "/downloads/"+name, nil); code != 200 || b != body {
		t.Fatalf("legacy UI download changed: %d %q", code, b)
	}
}

func TestAPIAgentRenameAndRemove(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	op, view, restore := apiToken(t, c, "operator"), apiToken(t, c, "viewer"), apiToken(t, c, "restore")
	a, creds := enrollAPITestAgent(t, e, "PC-17")
	path := fmt.Sprintf("/api/v1/agents/%d", a.ID)
	for _, token := range []string{view, restore} {
		for _, method := range []string{"PATCH", "DELETE"} {
			if code, b := apiJSONCall(t, e, token, method, path, `{"name":"Office"}`); code != 403 {
				t.Fatalf("unauthorized %s: %d %s", method, code, b)
			}
		}
	}
	if code, b := apiJSONCall(t, e, op, "PATCH", path, `{"name":"  Birou Ștefan  "}`); code != 200 || !strings.Contains(b, `"name":"Birou Ștefan"`) || !strings.Contains(b, `"hostname":"PC-17"`) {
		t.Fatalf("rename: %d %s", code, b)
	}
	if err := e.store.TouchAgent(e.ctx, a.ID, api.PollRequest{Hostname: "PC-18", OS: "Windows 11", Arch: "amd64", Version: "0.47.3"}, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	updated, err := e.store.GetAgent(e.ctx, a.ID)
	if err != nil || updated.Name() != "Birou Ștefan" || updated.Hostname != "PC-18" || updated.RepoDir != a.RepoDir || updated.UUID != a.UUID {
		t.Fatalf("poll changed display name or identity: %+v %v", updated, err)
	}
	if code, b := apiJSONCall(t, e, view, "GET", path, ""); code != 200 || !strings.Contains(b, `"name":"Birou Ștefan"`) || !strings.Contains(b, `"hostname":"PC-18"`) {
		t.Fatalf("GET after poll: %d %s", code, b)
	}
	if code, b := apiJSONCall(t, e, view, "GET", "/api/v1/agents", ""); code != 200 || !strings.Contains(b, `"name":"Birou Ștefan"`) {
		t.Fatalf("list after rename: %d %s", code, b)
	}
	if _, _, page := c.do("GET", "/agents", nil); !strings.Contains(page, "Birou Ștefan") {
		t.Fatal("console does not show display name")
	}
	for _, body := range []string{`{}`, `{"name":null}`, `{"name":""}`, `{"name":"new","repo_dir":"new"}`} {
		if code, b := apiJSONCall(t, e, op, "PATCH", path, body); code != 400 {
			t.Fatalf("invalid rename %s: %d %s", body, code, b)
		}
	}
	for _, path := range []string{"/api/v1/agents/0", "/api/v1/agents/nope"} {
		for _, method := range []string{"PATCH", "DELETE"} {
			if code, b := apiJSONCall(t, e, op, method, path, `{"name":"Office"}`); code != 400 {
				t.Fatalf("invalid ID: %d %s", code, b)
			}
		}
	}
	if code, b := apiJSONCall(t, e, op, "DELETE", path, `{"backups":"all"}`); code != 400 {
		t.Fatalf("accepted DELETE options: %d %s", code, b)
	}
	// No repository-backed runs: the UI used to hard-delete this agent and
	// cascade even its log history. Keeping backups must keep that history.
	var runID, queuedID, runningID, stoppingID int64
	for status, id := range map[string]*int64{"success": &runID, "queued": &queuedID, "running": &runningID, "stopping": &stoppingID} {
		if err := e.pool.QueryRow(e.ctx, `INSERT INTO runs(agent_id,kind,status,repo_url) VALUES($1,'agent-log',$2,'') RETURNING id`, a.ID, status).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	if code, b := apiJSONCall(t, e, op, "DELETE", path, ""); code != 200 || !strings.Contains(b, `"removed":true`) || !strings.Contains(b, `"backups":"kept"`) || !strings.Contains(b, `"history":"kept"`) || !strings.Contains(b, `"jobs":"deleted"`) || !strings.Contains(b, `"uninstalled":false`) {
		t.Fatalf("remove: %d %s", code, b)
	}
	if _, err := e.store.AuthenticateAgent(e.ctx, creds.AgentUUID, creds.Secret); err == nil {
		t.Fatal("removed agent still authenticates")
	}
	if code, b := apiJSONCall(t, e, view, "GET", "/api/v1/agents", ""); code != 200 || b != "[]\n" {
		t.Fatalf("removed agent in active list: %d %s", code, b)
	}
	for _, id := range []int64{runID, runningID, stoppingID} {
		code, b := apiJSONCall(t, e, view, "GET", fmt.Sprintf("/api/v1/runs/%d", id), "")
		if code != 200 || !strings.Contains(b, fmt.Sprintf(`"agent_id":%d`, a.ID)) || !strings.Contains(b, `"job_id":null`) {
			t.Fatalf("history lost: %d %s", code, b)
		}
	}
	for _, id := range []int64{runningID, stoppingID} {
		run, err := e.store.GetRun(e.ctx, id)
		if err != nil || run.Status != "failed" || run.FinishedAt == nil {
			t.Fatalf("active run left after removal: %+v %v", run, err)
		}
	}
	if _, err := e.store.GetRun(e.ctx, queuedID); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("queued run not dropped like UI: %v", err)
	}
	for _, method := range []string{"GET", "DELETE", "PATCH"} {
		body := ""
		if method == "PATCH" {
			body = `{"name":"Again"}`
		}
		if code, b := apiJSONCall(t, e, op, method, path, body); code != 404 {
			t.Fatalf("removed agent %s: %d %s", method, code, b)
		}
	}
	var kept bool
	if err := e.pool.QueryRow(e.ctx, `SELECT removed_at IS NOT NULL AND repo_dir=$2 FROM agents WHERE id=$1`, a.ID, a.RepoDir).Scan(&kept); err != nil || !kept {
		t.Fatalf("tombstone missing: %v %v", kept, err)
	}
	audit, err := e.store.ListAudit(e.ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, entry := range audit {
		if (entry.Action == "agent.rename" || entry.Action == "agent.delete") && strings.Contains(entry.Username, `(token "`) {
			found[entry.Action] = true
		}
	}
	if !found["agent.rename"] || !found["agent.delete"] {
		t.Fatalf("API audit identity missing: %+v", audit)
	}
}

func TestAPIAgentRemoveKeepsBackupsAndHistory(t *testing.T) {
	x := newRemoveEnv(t, "test")
	op := apiToken(t, x.admin, "operator")
	jobs, err := x.e.store.ListJobs(x.e.ctx)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs: %+v %v", jobs, err)
	}
	runs, err := x.e.store.ListRuns(x.e.ctx, server.RunFilter{AgentID: x.agentID})
	if err != nil || len(runs) != 3 {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	if code, b := apiJSONCall(t, x.e, op, "DELETE", fmt.Sprintf("/api/v1/agents/%d", x.agentID), ""); code != 200 {
		t.Fatalf("remove: %d %s", code, b)
	}
	if n := x.snapshots(t); n != 3 {
		t.Fatalf("backup data removed: %d", n)
	}
	if _, err := x.e.store.GetJob(x.e.ctx, jobs[0].ID); !errors.Is(err, server.ErrNotFound) {
		t.Fatalf("job not removed: %v", err)
	}
	for _, before := range runs {
		after, err := x.e.store.GetRun(x.e.ctx, before.ID)
		if err != nil || after.AgentID != x.agentID || after.JobID != nil || after.SnapshotID != before.SnapshotID || after.RepoURL != before.RepoURL {
			t.Fatalf("history changed: %+v %v", after, err)
		}
		if code, b := apiJSONCall(t, x.e, op, "GET", fmt.Sprintf("/api/v1/runs/%d", before.ID), ""); code != 200 || !strings.Contains(b, `"job_id":null`) {
			t.Fatalf("API history lost: %d %s", code, b)
		}
	}
	if code, b := apiJSONCall(t, x.e, op, "GET", fmt.Sprintf("/api/v1/runs/%d/files?path=/", runs[0].ID), ""); code != 200 {
		t.Fatalf("kept backup not browsable: %d %s", code, b)
	}
	replacement, _ := enrollAPITestAgent(t, x.e, "Restore-PC")
	body := fmt.Sprintf(`{"paths":["/"],"target":{"directory":"C:\\Restore"},"agent_id":%d}`, replacement.ID)
	code, b := apiJSONCall(t, x.e, op, "POST", fmt.Sprintf("/api/v1/runs/%d/restore", runs[0].ID), body)
	var restore struct {
		ID      int64  `json:"id"`
		AgentID int64  `json:"agent_id"`
		JobID   *int64 `json:"job_id"`
	}
	if code != 202 || json.Unmarshal([]byte(b), &restore) != nil || restore.ID == 0 || restore.AgentID != replacement.ID || restore.JobID != nil {
		t.Fatalf("restore after removal: %d %s", code, b)
	}
	queued, err := x.e.store.GetRun(x.e.ctx, restore.ID)
	if err != nil || queued.RepoURL != runs[0].RepoURL || queued.SnapshotID != runs[0].SnapshotID {
		t.Fatalf("restore lost source snapshot: %+v %v", queued, err)
	}
}

func TestAPIAgentRemovalApproval(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	if _, err := e.store.CreateUser(e.ctx, server.User{Username: "admin2", Role: "admin"}, "correct-horse-9"); err != nil {
		t.Fatal(err)
	}
	if _, loc, _ := c.do("POST", "/settings/four-eyes", url.Values{"four_eyes": {"on"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("enable approval: %s", loc)
	}
	a, creds := enrollAPITestAgent(t, e, "Approval-PC")
	op := apiToken(t, c, "operator")
	path := fmt.Sprintf("/api/v1/agents/%d", a.ID)
	if code, b := apiJSONCall(t, e, op, "DELETE", path, ""); code != 409 || !strings.Contains(b, "Waiting for approval") {
		t.Fatalf("approval bypass: %d %s", code, b)
	}
	if _, err := e.store.AuthenticateAgent(e.ctx, creds.AgentUUID, creds.Secret); err != nil {
		t.Fatalf("agent removed before approval: %v", err)
	}
	var approvalID int64
	var who string
	var payload []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT id,requested_by,payload FROM approvals WHERE action='agent.delete' AND object_id=$1 AND status='pending'`, a.ID).Scan(&approvalID, &who, &payload); err != nil || who != "admin" || !strings.Contains(string(payload), `"backups": "keep"`) && !strings.Contains(string(payload), `"backups":"keep"`) {
		t.Fatalf("approval identity/options: %s %s %v", who, payload, err)
	}
	if _, loc, _ := c.do("POST", fmt.Sprintf("/approvals/%d/approve", approvalID), url.Values{}); !strings.Contains(loc, "err=") {
		t.Fatal("token owner approved own request")
	}
	if code, _ := apiJSONCall(t, e, op, "DELETE", path, ""); code != 409 {
		t.Fatal("duplicate request not rejected")
	}
	c2 := newClient(t, e)
	c2.login("admin2", "correct-horse-9")
	if _, loc, _ := c2.do("POST", fmt.Sprintf("/approvals/%d/approve", approvalID), url.Values{}); !strings.Contains(loc, "msg=") {
		t.Fatalf("approve: %s", loc)
	}
	if _, err := e.store.AuthenticateAgent(e.ctx, creds.AgentUUID, creds.Secret); err == nil {
		t.Fatal("agent authenticates after approval")
	}
}

func TestAPIRunStableIDsAfterJobDeletion(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	op := apiToken(t, c, "operator")
	a, _ := enrollAPITestAgent(t, e, "Stable-PC")
	targetID, err := e.store.CreateTarget(e.ctx, server.Target{Name: "stable", Kind: "local", URL: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := e.store.CreateJob(e.ctx, server.Job{AgentID: a.ID, TargetID: targetID, Name: "Stable docs", Paths: []string{`C:\Data`}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	runID, err := e.store.QueueBackup(e.ctx, jobID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := e.store.ClaimRun(e.ctx, a.ID)
	if err != nil || claimed == nil || claimed.ID != runID {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if err := e.store.FinishRun(e.ctx, a.ID, runID, api.RunResult{Status: "success", SnapshotID: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	before, err := e.store.GetRun(e.ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if code, b := apiJSONCall(t, e, op, "DELETE", fmt.Sprintf("/api/v1/jobs/%d", jobID), ""); code != 204 {
		t.Fatalf("delete job: %d %s", code, b)
	}
	code, b := apiJSONCall(t, e, op, "GET", fmt.Sprintf("/api/v1/runs/%d", runID), "")
	var run struct {
		ID      int64  `json:"id"`
		JobID   *int64 `json:"job_id"`
		AgentID int64  `json:"agent_id"`
	}
	if code != 200 || json.Unmarshal([]byte(b), &run) != nil || run.ID != runID || run.AgentID != a.ID || run.JobID != nil {
		t.Fatalf("GET after job deletion: %d %s", code, b)
	}
	after, err := e.store.GetRun(e.ctx, runID)
	if err != nil || after.AgentID != a.ID || after.JobID != nil || after.SnapshotID != before.SnapshotID || after.RepoURL != before.RepoURL || !after.QueuedAt.Equal(before.QueuedAt) {
		t.Fatalf("history changed: %+v %v", after, err)
	}
	for _, table := range []string{"agents", "runs"} {
		var typ, def string
		if err := e.pool.QueryRow(e.ctx, `SELECT data_type,column_default FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name='id'`, table).Scan(&typ, &def); err != nil || typ != "bigint" || !strings.HasPrefix(def, "nextval(") {
			t.Fatalf("%s ID is not BIGSERIAL: %s %s %v", table, typ, def, err)
		}
		var cycles bool
		if err := e.pool.QueryRow(e.ctx, `SELECT seqcycle FROM pg_sequence WHERE seqrelid=pg_get_serial_sequence($1,'id')::regclass`, table).Scan(&cycles); err != nil || cycles {
			t.Fatalf("%s sequence can reuse IDs: %v %v", table, cycles, err)
		}
	}
	// Deleting the highest IDs does not make ordinary inserts reuse them.
	var deletedRun, nextRun int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO runs(agent_id,kind,repo_url) VALUES($1,'agent-log','') RETURNING id`, a.ID).Scan(&deletedRun); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `DELETE FROM runs WHERE id=$1`, deletedRun); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO runs(agent_id,kind,repo_url) VALUES($1,'agent-log','') RETURNING id`, a.ID).Scan(&nextRun); err != nil || nextRun <= deletedRun || deletedRun <= runID {
		t.Fatalf("run ID reused: %d -> %d %v", deletedRun, nextRun, err)
	}
	deletedAgent, _ := enrollAPITestAgent(t, e, "Disposable")
	if err := e.store.DeleteAgent(e.ctx, deletedAgent.ID); err != nil {
		t.Fatal(err)
	}
	nextAgent, _ := enrollAPITestAgent(t, e, "New")
	if nextAgent.ID <= deletedAgent.ID || deletedAgent.ID <= a.ID {
		t.Fatalf("agent ID reused: %d -> %d", deletedAgent.ID, nextAgent.ID)
	}
}
