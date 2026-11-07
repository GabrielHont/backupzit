package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func packageSum(b string) string {
	h := sha256.Sum256([]byte(b))
	return hex.EncodeToString(h[:])
}

func TestAgentPackageNames(t *testing.T) {
	for _, tc := range []struct{ file, os, arch, kind, version string }{
		{"backupzit-agent-0.47.3-x64.msi", "windows", "amd64", "msi", "0.47.3"},
		{"backupzit-agent-0.47.3-x64-legacy.msi", "windows", "amd64", "msi", "0.47.3-legacy"},
		{"backupzit-agent_0.47.3_amd64.deb", "linux", "amd64", "deb", "0.47.3"},
		{"backupzit-agent_0.47.3-1_arm64.deb", "linux", "arm64", "deb", "0.47.3-1"},
		{"backupzit-agent_0.47.3_i386.deb", "linux", "386", "deb", "0.47.3"},
		{"backupzit-agent_0.47.3_armhf.deb", "linux", "arm", "deb", "0.47.3"},
		{"backupzit-agent-0.47.3-1.x86_64.rpm", "linux", "amd64", "rpm", "0.47.3-1"},
		{"backupzit-agent-0.47.3.aarch64.rpm", "linux", "arm64", "rpm", "0.47.3"},
		{"backupzit-agent-0.47.3.i686.rpm", "linux", "386", "rpm", "0.47.3"},
		{"backupzit-agent-0.47.3.armv7hl.rpm", "linux", "arm", "rpm", "0.47.3"},
	} {
		p, ct, ok := parseAgentPackage(tc.file)
		if !ok || ct == "" || p.File != tc.file || p.OS != tc.os || p.Arch != tc.arch || p.Kind != tc.kind || p.Version != tc.version {
			t.Errorf("%s: %+v %q %v", tc.file, p, ct, ok)
		}
	}
	for _, name := range []string{"", "backupzit-agent.exe", "anything-legacy.msi", "backupzit-agent.msi", "backupzit-server_0.47.3_amd64.deb", "backupzit-repo-0.47.3-1.x86_64.rpm", "recovery.iso", "backupzit-agent-0.47.3-x64.msi.bak", "backupzit-agent_0.47.3_unknown.deb", "backupzit-agent_0.47.3_amd64.deb\n", "../backupzit-agent-0.47.3-x64.msi", `..\backupzit-agent-0.47.3-x64.msi`, "backupzit-agent-0.47.3-x64.msi/secrets", "backupzit-agent-0.47.3-x64.msi%00", "backupzit-agent-0.47.3-x64.msi\x00"} {
		if _, _, ok := parseAgentPackage(name); ok {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestAgentPackageCatalog(t *testing.T) {
	s := &Server{DistDir: t.TempDir()}
	files := map[string]string{
		"backupzit-agent-0.47.2-x64.msi":        "older installer",
		"backupzit-agent-0.47.3-x64.msi":        "windows installer",
		"backupzit-agent-0.47.3-x64-legacy.msi": "legacy installer",
		"backupzit-agent_0.47.3_amd64.deb":      "debian installer",
		"backupzit-agent-0.47.3-1.x86_64.rpm":   "rpm installer",
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(s.DistDir, name), []byte(b), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"backupzit-server_0.47.3_amd64.deb", "backupzit-agent.exe", "backupzit-repo_0.47.3_amd64.deb", "recovery.iso", "anything-legacy.msi"} {
		if err := os.WriteFile(filepath.Join(s.DistDir, name), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(s.DistDir, "backupzit-agent_0.47.3_arm64.deb"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.DistDir, "backupzit-agent_0.47.3_i386.deb")); err != nil {
		t.Fatal(err)
	}
	// A link to a regular file *inside* DistDir is also excluded.
	if err := os.Symlink("backupzit-agent-0.47.3-x64.msi", filepath.Join(s.DistDir, "backupzit-agent_0.47.3_armhf.deb")); err != nil {
		t.Fatal(err)
	}
	packages, err := s.agentPackages()
	if err != nil || len(packages) != len(files) {
		t.Fatalf("catalog: %+v %v", packages, err)
	}
	for i, p := range packages {
		b, ok := files[p.File]
		if !ok || p.Size != int64(len(b)) || p.SHA256 != packageSum(b) || (i > 0 && packages[i-1].File >= p.File) {
			t.Errorf("wrong catalog entry: %+v", p)
		}
	}
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "missing")} {
		s.DistDir = dir
		packages, err = s.agentPackages()
		if err != nil || packages == nil || len(packages) != 0 {
			t.Errorf("empty catalog: %+v %v", packages, err)
		}
	}
	s.DistDir = outside // Misconfiguration must not be disguised as an empty catalog.
	if _, err := s.agentPackages(); err == nil {
		t.Error("file accepted as installer directory")
	}
}

func TestPublicAgentPackagesWithoutListeners(t *testing.T) {
	s, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.DistDir = t.TempDir()
	name, body := "backupzit-agent-0.47.3-x64.msi", "installer bytes"
	if err := os.WriteFile(filepath.Join(s.DistDir, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	// The public console can hide its UI behind a network allowlist. New
	// computers still need installers from the agent-facing address.
	s.access.at, s.access.v = time.Now(), WebAccess{Allow: []string{"10.0.0.0/8"}}
	h := s.MainHandler()
	request := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "198.51.100.7:1234"
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("GET", "/agent/"+name, nil)
	if w.Code != 200 || w.Body.String() != body {
		t.Fatalf("public GET: %d %q", w.Code, w.Body.String())
	}
	for k, v := range map[string]string{"Content-Type": "application/x-msi", "Content-Length": "15", "X-Content-SHA256": packageSum(body), "Cache-Control": "public, max-age=3600, must-revalidate", "ETag": `"` + packageSum(body) + `"`, "X-Content-Type-Options": "nosniff"} {
		if w.Header().Get(k) != v {
			t.Errorf("%s = %q, want %q", k, w.Header().Get(k), v)
		}
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), name) {
		t.Error("missing attachment name")
	}
	if w = request("HEAD", "/agent/"+name, nil); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "15" {
		t.Fatalf("HEAD: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if w = request("GET", "/agent/"+name, map[string]string{"Range": "bytes=1-3"}); w.Code != 206 || w.Body.String() != body[1:4] || w.Header().Get("X-Content-SHA256") != packageSum(body) {
		t.Fatalf("Range: %d %q", w.Code, w.Body.String())
	}
	if w = request("GET", "/agent/"+name, map[string]string{"If-None-Match": `"` + packageSum(body) + `"`}); w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("conditional GET: %d %q", w.Code, w.Body.String())
	}
	for _, path := range []string{"/agent/backupzit-server_0.47.3_amd64.deb", "/agent/backupzit-agent.exe", "/agent/backupzit-agent_0.47.3_amd64.deb", "/agent/..%2F" + name, "/agent/..%5C" + name, "/agent/" + name + "%00", "/agent/" + name + "%2Fextra", "/login"} {
		if w = request("GET", path, nil); w.Code != 404 || strings.Contains(w.Body.String(), body) {
			t.Errorf("%s: %d %q", path, w.Code, w.Body.String())
		}
	}
	// All supported package types use their actual media types.
	for _, name := range []string{"backupzit-agent_0.47.3_amd64.deb", "backupzit-agent-0.47.3-1.x86_64.rpm"} {
		if err := os.WriteFile(filepath.Join(s.DistDir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		_, contentType, _ := parseAgentPackage(name)
		if w = request("GET", "/agent/"+name, nil); w.Code != 200 || w.Header().Get("Content-Type") != contentType || w.Body.String() != body {
			t.Errorf("%s: %d %v", name, w.Code, w.Header())
		}
	}
	if w = request("POST", "/agent/"+name, nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
	if err := os.Remove(filepath.Join(s.DistDir, name)); err != nil {
		t.Fatal(err)
	}
	if w = request("GET", "/agent/"+name, nil); w.Code != 404 {
		t.Errorf("removed package served from checksum cache: %d", w.Code)
	}
}

func TestInstallerChecksumCache(t *testing.T) {
	s := &Server{DistDir: t.TempDir()}
	path := filepath.Join(s.DistDir, "backupzit-agent-0.47.3-x64.msi")
	now := time.Now().Add(-time.Hour).Truncate(time.Second)
	write := func(b string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(b), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want string) {
		t.Helper()
		packages, err := s.agentPackages()
		if err != nil || len(packages) != 1 || packages[0].SHA256 != packageSum(want) || packages[0].Size != int64(len(want)) {
			t.Fatalf("catalog checksum: %+v %v", packages, err)
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/agent/"+filepath.Base(path), nil)
		r.SetPathValue("file", filepath.Base(path))
		s.handlePublicAgentPackage(w, r)
		if w.Code != 200 || w.Body.String() != want || w.Header().Get("X-Content-SHA256") != packageSum(want) {
			t.Fatalf("download checksum: %d %q %v", w.Code, w.Body.String(), w.Header())
		}
	}
	write("aaa", now)
	check("aaa")
	check("aaa")                       // cached read
	write("bbb", now.Add(time.Second)) // same size, new mtime
	check("bbb")
	write("longer", now.Add(time.Second)) // same mtime, new size
	check("longer")
	other := path + ".new"
	if err := os.WriteFile(other, []byte("rename"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, path); err != nil {
		t.Fatal(err)
	}
	check("rename") // same size/mtime, different file identity
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 4 {
				if sum, err := s.fileSHA256(path); err != nil || sum != packageSum("rename") {
					t.Errorf("concurrent hash: %q %v", sum, err)
				}
			}
		})
	}
	wg.Wait()
	count := 0
	s.shaCache.Range(func(_, _ any) bool { count++; return true })
	if count != 1 {
		t.Fatalf("old cache entries retained: %d", count)
	}
	for _, badPath := range []string{s.DistDir, path + ".missing"} {
		if _, err := s.fileSHA256(badPath); err == nil {
			t.Errorf("hashed %s", badPath)
		}
	}
}

func TestPublicAgentPackageRejectsLinksAndPaths(t *testing.T) {
	s := &Server{DistDir: t.TempDir()}
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("private content"), 0600); err != nil {
		t.Fatal(err)
	}
	name := "backupzit-agent-0.47.3-x64.msi"
	if err := os.Symlink(outside, filepath.Join(s.DistDir, name)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{name, "../private", `..\private`, outside, "", ".", "..", "backupzit-agent.exe"} {
		r := httptest.NewRequest("GET", "/agent/invalid", nil)
		r.SetPathValue("file", name)
		w := httptest.NewRecorder()
		s.handlePublicAgentPackage(w, r)
		if w.Code != 404 || strings.Contains(w.Body.String(), "private content") {
			t.Errorf("unsafe path %q: %d %q", name, w.Code, w.Body.String())
		}
	}
}

func TestAgentNameValidation(t *testing.T) {
	for _, body := range []string{`{}`, `{"name":""}`, `{"name":"  "}`, `{"name":null}`, `{"name":1}`, `{"name":"a\u0000b"}`, `{"name":"a\nb"}`, `{"name":"ok","repo_dir":"new"}`, `{"hostname":"new"}`, `{} {}`, `null`, `{"name":"` + strings.Repeat("a", 256) + `"}`} {
		r := httptest.NewRequest("PATCH", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if _, err := readAgentName(r); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	r := httptest.NewRequest("PATCH", "/", strings.NewReader(`{"name":"  Contabilitate Ștefan 🖥  "}`))
	r.Header.Set("Content-Type", "application/json")
	if name, err := readAgentName(r); err != nil || name != "Contabilitate Ștefan 🖥" {
		t.Fatalf("name = %q, %v", name, err)
	}
	a := Agent{Hostname: "PC-1", RepoDir: "PC-1-fixed"}
	if toAPIAgent(a).Name != a.Hostname {
		t.Fatal("missing hostname fallback")
	}
	a.DisplayName = "Contabilitate"
	if out := toAPIAgent(a); out.Name != a.DisplayName || out.Hostname != "PC-1" || a.RepoDir != "PC-1-fixed" {
		t.Fatalf("name changed identity: %+v", out)
	}
}
