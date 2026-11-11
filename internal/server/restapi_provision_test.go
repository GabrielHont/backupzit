package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/api"
)

func apiJSONCall(t *testing.T, e *env, token, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestAPIEnrollmentLimits(t *testing.T) {
	e := setup(t)
	c := newClient(t, e)
	c.login("admin", "admin-pass-123")
	admin, viewer := apiToken(t, c, "admin"), apiToken(t, c, "viewer")
	if code, b := apiJSONCall(t, e, viewer, "GET", "/api/v1/openapi.json", ""); code != 200 || !strings.Contains(b, `"openapi":"3.1.0"`) {
		t.Fatalf("OpenAPI: %d %s", code, b)
	}
	if code, _ := apiJSONCall(t, e, viewer, "PUT", "/api/v1/limits", `{"maxAgents":1}`); code != 403 {
		t.Fatalf("viewer set limit: %d", code)
	}
	for _, body := range []string{`{}`, `{"maxAgents":-1}`, `{"maxAgents":1,"other":0}`, `{"maxAgents":1.5}`} {
		if code, b := apiJSONCall(t, e, admin, "PUT", "/api/v1/limits", body); code != 400 {
			t.Fatalf("invalid limit %s: %d %s", body, code, b)
		}
	}
	for _, body := range []string{`{"ttlHours":0}`, `{"ttlHours":8761}`, `{"maxUses":0}`, `{"ttlHours":24,"typo":1}`, `{} {}`} {
		if code, b := apiJSONCall(t, e, admin, "POST", "/api/v1/enrollment-tokens", body); code != 400 {
			t.Fatalf("invalid enrollment %s: %d %s", body, code, b)
		}
	}
	create := func(body string) (int64, string) {
		code, b := apiJSONCall(t, e, admin, "POST", "/api/v1/enrollment-tokens", body)
		var out struct {
			ID      int64
			Code    string
			MaxUses *int64
		}
		if err := json.Unmarshal([]byte(b), &out); err != nil || code != 201 || out.ID == 0 {
			t.Fatalf("create: %d %s %v", code, b, err)
		}
		_, secret, _, err := api.DecodeEnrollCode(out.Code)
		if err != nil {
			t.Fatal(err)
		}
		return out.ID, secret
	}
	id, secret := create(`{"maxUses":1,"ttlHours":24,"label":"portal"}`)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: secret, Hostname: "one"}); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("single-use token admitted %d agents", wins.Load())
	}
	agents, _ := e.store.ListAgents(e.ctx)
	if len(agents) != 1 || agents[0].EnrollmentTokenID == nil || *agents[0].EnrollmentTokenID != id {
		t.Fatalf("token provenance: %+v", agents)
	}
	code, b := apiJSONCall(t, e, admin, "GET", "/api/v1/enrollment-tokens", "")
	if code != 200 || !strings.Contains(b, `"uses":1`) || strings.Contains(b, secret) || strings.Contains(b, "BZ1-") {
		t.Fatalf("metadata: %d %s", code, b)
	}
	if code, b := apiJSONCall(t, e, admin, "PUT", "/api/v1/limits", `{"maxAgents":2}`); code != 200 {
		t.Fatalf("limit: %d %s", code, b)
	}
	// Different tokens must still serialize against one shared limit.
	_, first := create(`{"maxUses":16}`)
	_, second := create(`{"maxUses":16}`)
	wins.Store(0)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := first
			if i%2 == 1 {
				tok = second
			}
			if _, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: tok, Hostname: "limited"}); err == nil {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("limit admitted %d more agents", wins.Load())
	}
	var totalUses int
	if err := e.pool.QueryRow(e.ctx, `SELECT sum(uses) FROM enrollment_tokens`).Scan(&totalUses); err != nil || totalUses != 2 {
		t.Fatalf("failed enrollment consumed token: %d %v", totalUses, err)
	}
	// At the limit, a computer started from the recovery medium still joins and
	// is not counted; an ordinary one is still refused (Unirea fork).
	_, recoveryToken := create(`{"maxUses":1}`)
	if _, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: recoveryToken, Hostname: "MINWINPC", Recovery: true}); err != nil {
		t.Fatalf("recovery agent refused at the limit: %v", err)
	}
	_, plainToken := create(`{"maxUses":1}`)
	if _, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: plainToken, Hostname: "one-too-many"}); err == nil {
		t.Fatal("an ordinary agent joined above the limit next to a recovery agent")
	}
	if code, _ := apiJSONCall(t, e, admin, "PUT", "/api/v1/limits", `{"maxAgents":null}`); code != 200 {
		t.Fatal("remove limit")
	}
	revokedID, revoked := create(`{}`)
	if code, b := apiJSONCall(t, e, admin, "DELETE", fmt.Sprintf("/api/v1/enrollment-tokens/%d", revokedID), ""); code != 204 {
		t.Fatalf("revoke: %d %s", code, b)
	}
	if _, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: revoked, Hostname: "revoked"}); err == nil {
		t.Fatal("revoked token accepted")
	}
	legacy, _, err := e.store.CreateEnrollmentToken(e.ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: legacy, Hostname: "legacy"}); err != nil {
			t.Fatal(err)
		}
	}
	expiredID, expired := create(`{}`)
	if _, err := e.pool.Exec(e.ctx, `UPDATE enrollment_tokens SET expires_at=now()-interval '1 hour' WHERE id=$1`, expiredID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.EnrollAgent(e.ctx, api.EnrollRequest{Token: expired, Hostname: "expired"}); err == nil {
		t.Fatal("expired token accepted")
	}
}
