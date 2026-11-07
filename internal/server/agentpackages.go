package server

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// Only installer names produced by our packaging scripts are public. Do not
// use classify/fileKind here: those also accept unversioned or unrelated files.
var installerNames = []struct {
	re          *regexp.Regexp
	os, kind    string
	contentType string
}{
	{regexp.MustCompile(`^backupzit-agent-([0-9]+\.[0-9]+\.[0-9]+)-x64(-legacy)?\.msi$`), "windows", "msi", "application/x-msi"},
	{regexp.MustCompile(`^backupzit-agent_([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9]+)?)_(amd64|arm64|i386|armhf)\.deb$`), "linux", "deb", "application/vnd.debian.binary-package"},
	{regexp.MustCompile(`^backupzit-agent-([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9]+)?)\.(x86_64|aarch64|i386|i686|armv7hl)\.rpm$`), "linux", "rpm", "application/x-rpm"},
}

type apiAgentPackage struct {
	File    string `json:"file"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Kind    string `json:"kind"`
	Version string `json:"version"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

func parseAgentPackage(name string) (apiAgentPackage, string, bool) {
	for _, spec := range installerNames {
		m := spec.re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		p := apiAgentPackage{File: name, OS: spec.os, Kind: spec.kind, Version: m[1], Arch: "amd64"}
		if spec.kind == "msi" {
			p.Version += m[2]
		} else {
			p.Arch = map[string]string{"amd64": "amd64", "x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64", "i386": "386", "i686": "386", "armhf": "arm", "armv7hl": "arm"}[m[2]]
		}
		return p, spec.contentType, true
	}
	return apiAgentPackage{}, "", false
}

// openAgentPackage is the shared allowlist for the catalog and public route.
// Reject links and non-regular files, and keep resolution inside DistDir even
// if a directory entry is replaced between the check and open.
func (s *Server) openAgentPackage(name string) (*os.File, os.FileInfo, error) {
	if _, _, ok := parseAgentPackage(name); !ok || s.DistDir == "" {
		return nil, nil, os.ErrNotExist
	}
	root, err := os.OpenRoot(s.DistDir)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, os.ErrNotExist
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(before, fi) {
		f.Close()
		return nil, nil, fmt.Errorf("installer changed while opening: %s", name)
	}
	return f, fi, nil
}

func (s *Server) agentPackages() ([]apiAgentPackage, error) {
	out := []apiAgentPackage{}
	if s.DistDir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(s.DistDir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries { // ReadDir sorts by filename.
		p, _, ok := parseAgentPackage(entry.Name())
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		f, fi, err := s.openAgentPackage(p.File)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		p.Size = fi.Size()
		p.SHA256, err = s.openFileSHA256(filepath.Join(s.DistDir, p.File), f, fi)
		f.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Server) handlePublicAgentPackage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	f, fi, err := s.openAgentPackage(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	sum, err := s.openFileSHA256(filepath.Join(s.DistDir, name), f, fi)
	if err != nil {
		http.Error(w, "installer unavailable", http.StatusInternalServerError)
		return
	}
	_, contentType, _ := parseAgentPackage(name)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set("X-Content-SHA256", sum)
	w.Header().Set("ETag", `"`+sum+`"`)
	w.Header().Set("Cache-Control", "public, max-age=3600, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

func (s *Server) registerAPIAgentPackages(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agent-packages", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.agentPackages()
	}))
	mux.HandleFunc("GET /agent/{file}", s.handlePublicAgentPackage)
}
