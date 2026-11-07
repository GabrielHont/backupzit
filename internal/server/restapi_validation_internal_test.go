package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicJSONValidation(t *testing.T) {
	for _, body := range []string{`{"unknown":1}`, `{"name":`, `{} {}`, `[]`, `null`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		var req struct {
			Name string `json:"name"`
		}
		if err := readAPIJSON(r, &req); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
	if err := readAPIJSON(r, new(struct{})); err == nil {
		t.Fatal("accepted missing content type")
	}
}
