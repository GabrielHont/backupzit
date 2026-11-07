package server_test

import (
	"strings"
	"testing"
)

func TestProvisioningAPIToken(t *testing.T) {
	e := setup(t)
	token, err := e.store.CreateProvisioningAPIToken(e.ctx, "portal provisioner", "admin", 30)
	if err != nil || !strings.HasPrefix(token, "bzt_") {
		t.Fatalf("provision: %q %v", token, err)
	}
	if code, b := apiJSONCall(t, e, token, "GET", "/api/v1/status", ""); code != 200 {
		t.Fatalf("token unusable: %d %s", code, b)
	}
	var valid bool
	if err := e.pool.QueryRow(e.ctx, `SELECT expires_at>now() AND role='admin' FROM api_tokens WHERE name='portal provisioner'`).Scan(&valid); err != nil || !valid {
		t.Fatalf("expiry/role: %v %v", valid, err)
	}
	entries, err := e.store.ListAudit(e.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if strings.Contains(entry.Detail, token) {
			t.Fatal("token leaked into audit")
		}
		if entry.Action == "api_token.create" && strings.Contains(entry.Detail, "portal provisioner") {
			found = true
		}
	}
	if !found {
		t.Fatal("provisioning was not audited")
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE users SET disabled=true WHERE username='admin'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateProvisioningAPIToken(e.ctx, "blocked", "admin", 0); err == nil {
		t.Fatal("disabled admin minted token")
	}
}
