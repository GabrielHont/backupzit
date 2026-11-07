package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type apiFileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Type    string    `json:"type"`
	Size    uint64    `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// Public paths use snapshot components, as returned by /files. Leading /
// is optional; parent traversal, NUL and Windows separators are rejected.
func publicSnapshotPath(p string) (string, error) {
	if len(p) > 32768 || strings.ContainsAny(p, "\x00\\") {
		return "", apiErr(400, "use snapshot paths with forward slashes, without NUL")
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return "", apiErr(400, "parent traversal is not allowed in snapshot paths")
		}
	}
	return strings.Join(snapshotComps(p), "/"), nil
}

func restoreDirectory(raw json.RawMessage) (string, error) {
	var original string
	if json.Unmarshal(raw, &original) == nil && original == "original" {
		return "", nil
	}
	var object struct {
		Directory string `json:"directory"`
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(&object); err != nil || strings.TrimSpace(object.Directory) == "" || len(object.Directory) > 32768 || strings.ContainsRune(object.Directory, 0) {
		return "", apiErr(400, "target must be original or an object with a non-empty directory")
	}
	return strings.TrimSpace(object.Directory), nil
}

func (s *Server) registerAPIRecovery(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/runs/{id}/files", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		dir, err := publicSnapshotPath(r.URL.Query().Get("path"))
		if err != nil {
			return nil, err
		}
		run, err := s.store.GetRun(r.Context(), id)
		if err != nil {
			return nil, err
		}
		repository, sn, err := s.fileSnapshot(r.Context(), run)
		if err != nil {
			return nil, apiErr(409, "%s", err)
		}
		list, err := listDir(r.Context(), repository, sn, dir)
		if err != nil {
			return nil, apiErr(404, "%s", err)
		}
		out := make([]apiFileEntry, 0, len(list))
		for _, entry := range list {
			out = append(out, apiFileEntry{Name: entry.Name, Path: "/" + entry.Path, Type: entry.Type, Size: entry.Size, ModTime: entry.ModTime})
		}
		return out, nil
	}))
	mux.HandleFunc("GET /api/v1/runs/{id}/download", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		if r.URL.Query().Get("path") == "" {
			return nil, apiErr(400, "path is required (use / for the whole backup)")
		}
		p, err := publicSnapshotPath(r.URL.Query().Get("path"))
		if err != nil {
			return nil, err
		}
		run, err := s.store.GetRun(r.Context(), id)
		if err != nil {
			return nil, err
		}
		started, err := s.downloadFiles(w, r, run, []string{p})
		if err != nil {
			if started {
				s.log.Error("api download", "run", id, "err", err)
				panic(http.ErrAbortHandler)
			}
			return nil, apiErr(409, "%s", err)
		}
		return apiStreamed{}, nil
	}))
	mux.HandleFunc("POST /api/v1/runs/{id}/restore", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		var req struct {
			Paths   []string        `json:"paths"`
			Target  json.RawMessage `json:"target"`
			AgentID int64           `json:"agent_id"`
			Verify  bool            `json:"verify"`
		}
		if err := readAPIJSON(r, &req); err != nil {
			return nil, err
		}
		if len(req.Paths) == 0 || len(req.Paths) > 10000 {
			return nil, apiErr(400, "choose between 1 and 10000 paths (use / for the whole backup)")
		}
		target, err := restoreDirectory(req.Target)
		if err != nil {
			return nil, err
		}
		backup, err := s.store.GetRun(r.Context(), id)
		if err != nil {
			return nil, err
		}
		if req.AgentID == 0 {
			req.AgentID = backup.AgentID
		}
		if req.AgentID < 0 {
			return nil, apiErr(400, "agent_id must be positive")
		}
		win := s.runIsWindows(r.Context(), backup)
		paths := make([]string, 0, len(req.Paths))
		all := false
		for _, p := range req.Paths {
			p, err = publicSnapshotPath(p)
			if err != nil {
				return nil, err
			}
			if p == "" {
				all = true
				continue
			}
			paths = append(paths, agentSyntaxPath(snapshotComps(p), win))
		}
		if all {
			paths = nil
		}
		rid, err := s.queueFileRestore(r, id, req.AgentID, target, paths, req.Verify)
		if err != nil {
			return nil, mutationAPIError(err, true)
		}
		run, err := s.store.GetRun(r.Context(), rid)
		if err != nil {
			return nil, err
		}
		return toAPIRunJSON(run), nil
	}))
}
