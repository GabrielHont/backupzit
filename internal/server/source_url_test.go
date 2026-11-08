package server_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/server"
)

// AGPLv3 section 13: the console offers the source of the running build to
// everyone who uses it over the network, on its pages and in the API.
func TestSourceURLOffered(t *testing.T) {
	e := setup(t)
	token, err := e.store.CreateProvisioningAPIToken(e.ctx, "source check", "admin", 30)
	if err != nil {
		t.Fatal(err)
	}
	if _, b := apiJSONCall(t, e, token, "GET", "/api/v1/status", ""); !strings.Contains(b, `"source_url":"`+server.UpstreamSourceURL+`"`) {
		t.Fatalf("default source missing from status: %s", b)
	}
	const fork = "https://example.org/fork/backupzit"
	e.srv.SourceURL = fork
	if _, b := apiJSONCall(t, e, token, "GET", "/api/v1/status", ""); !strings.Contains(b, `"source_url":"`+fork+`"`) {
		t.Fatalf("fork source missing from status: %s", b)
	}
	client := e.ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Get(e.ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(page), `href="`+fork+`"`) {
		t.Fatalf("login page does not link the source (status %d)", res.StatusCode)
	}
}
