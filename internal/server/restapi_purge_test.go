package server_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Unirea: when a firm's data is deleted, the portal empties its console: every agent and every backup.
func TestAPIPurgeDeletesEveryBackup(t *testing.T) {
	x := newRemoveEnv(t, "test")
	viewer := apiToken(t, x.admin, "viewer")
	admin := apiToken(t, x.admin, "admin")
	if code, b := apiJSONCall(t, x.e, admin, "GET", "/api/v1/purge", ""); code != 200 || !strings.Contains(b, `"started":false`) {
		t.Fatalf("no purge yet: %d %s", code, b)
	}
	// A viewer cannot, and the confirmation must be exact.
	if code, _ := apiJSONCall(t, x.e, viewer, "POST", "/api/v1/purge", `{"confirm":"delete every backup in this console"}`); code != 403 {
		t.Fatalf("viewer purge: %d", code)
	}
	if code, _ := apiJSONCall(t, x.e, admin, "POST", "/api/v1/purge", `{"confirm":"yes"}`); code != 400 {
		t.Fatalf("wrong confirmation accepted: %d", code)
	}
	if n := x.snapshots(t); n != 3 {
		t.Fatalf("backups touched before the purge: %d", n)
	}
	code, b := apiJSONCall(t, x.e, admin, "POST", "/api/v1/purge", `{"confirm":"delete every backup in this console"}`)
	if code != 202 || !strings.Contains(b, `"agents":1`) || !strings.Contains(b, `"repositories":1`) {
		t.Fatalf("purge: %d %s", code, b)
	}
	if x.agentExists() {
		t.Fatal("agent still there")
	}
	var st struct {
		Started bool `json:"started"`
		State   struct {
			FinishedAt *time.Time `json:"finished_at"`
			OK         bool       `json:"ok"`
			Repos      []struct {
				OK     bool   `json:"ok"`
				Done   bool   `json:"done"`
				Result string `json:"result"`
			} `json:"repositories"`
		} `json:"state"`
	}
	for i := 0; i < 100; i++ {
		_, b = apiJSONCall(t, x.e, admin, "GET", "/api/v1/purge", "")
		if json.Unmarshal([]byte(b), &st) == nil && st.State.FinishedAt != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !st.State.OK || len(st.State.Repos) != 1 || !st.State.Repos[0].OK || !st.State.Repos[0].Done {
		t.Fatalf("purge outcome: %s", b)
	}
	x.waitSnapshots(t, -1)
	// The software on the computer is not told to uninstall; its enrollment just no longer works.
	if code, b := apiJSONCall(t, x.e, admin, "GET", "/api/v1/enrollment-tokens", ""); code != 200 || strings.TrimSpace(b) != "[]" {
		t.Fatalf("enrollment tokens left: %d %s", code, b)
	}
	// Running it again on an empty console is harmless: nothing left to delete, and it says so.
	if code, b := apiJSONCall(t, x.e, admin, "POST", "/api/v1/purge", `{"confirm":"delete every backup in this console"}`); code != 202 || !strings.Contains(b, `"agents":0`) || !strings.Contains(b, `"repositories":0`) {
		t.Fatalf("second purge: %d %s", code, b)
	}
}
