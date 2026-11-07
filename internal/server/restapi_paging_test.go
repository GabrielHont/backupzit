package server_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

func TestAPIRunCursor(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	token := apiToken(t, c, "viewer")
	et, _, err := e.store.CreateEnrollmentToken(e.ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: et, Hostname: "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.store.AuthenticateAgent(e.ctx, resp.AgentUUID, resp.Secret)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(status string) int64 {
		var id int64
		// Deliberately equal timestamps: cursor order must use immutable IDs.
		if err := e.pool.QueryRow(e.ctx, `INSERT INTO runs(agent_id,kind,status,repo_url,queued_at) VALUES($1,'backup',$2,'test','2026-01-01') RETURNING id`, a.ID, status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := insert("success")
	insert("failed")
	third := insert("success")
	fourth := insert("success")
	get := func(q string) []struct{ ID int64 } {
		code, b := apiJSONCall(t, e, token, "GET", "/api/v1/runs?"+q, "")
		var out []struct{ ID int64 }
		if code != 200 || json.Unmarshal([]byte(b), &out) != nil {
			t.Fatalf("runs: %d %s", code, b)
		}
		return out
	}
	page := get(fmt.Sprintf("agent=%d&status=success&limit=2", a.ID))
	if len(page) != 2 || page[0].ID != fourth || page[1].ID != third {
		t.Fatalf("first page: %+v", page)
	}
	insert("success")
	page = get(fmt.Sprintf("agent=%d&status=success&limit=2&before=%d", a.ID, third))
	if len(page) != 1 || page[0].ID != first {
		t.Fatalf("unstable cursor or filter after limit: %+v", page)
	}
	for _, cursor := range []string{"-1", "0", "nope"} {
		if code, b := apiJSONCall(t, e, token, "GET", "/api/v1/runs?before="+cursor, ""); code != 400 {
			t.Fatalf("bad cursor: %d %s", code, b)
		}
	}
}
