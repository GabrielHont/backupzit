package server

import (
	"io"
	"net/http"
	"strings"
	"unicode"
)

func readAgentName(r *http.Request) (string, error) {
	var q struct {
		Name string `json:"name"`
	}
	if err := readAPIJSON(r, &q); err != nil {
		return "", err
	}
	name := strings.TrimSpace(q.Name)
	if name == "" || len(name) > 255 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", apiErr(http.StatusBadRequest, "name must contain 1 to 255 bytes and no control characters")
	}
	return name, nil
}

func (s *Server) renameAgent(r *http.Request, id int64, name string) (Agent, error) {
	a, err := s.store.GetAgent(r.Context(), id)
	if err != nil {
		return Agent{}, err
	}
	// The poll updates hostname, never display_name or repo_dir.
	updated, err := s.store.setAgentDisplayName(r.Context(), id, name)
	if err != nil {
		return Agent{}, err
	}
	s.audit(r, "agent.rename", "agent %d (%s): %q to %q", a.ID, a.Hostname, a.Name(), updated.Name())
	return updated, nil
}

type apiAgentRemoval struct {
	ID          int64  `json:"id"`
	Removed     bool   `json:"removed"`
	Backups     string `json:"backups"`
	History     string `json:"history"`
	Jobs        string `json:"jobs"`
	Uninstalled bool   `json:"uninstalled"`
	Message     string `json:"message"`
}

func (s *Server) registerAPIAgentMutations(mux *http.ServeMux) {
	mux.HandleFunc("PATCH /api/v1/agents/{id}", s.api(PermAgents, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		name, err := readAgentName(r)
		if err != nil {
			return nil, err
		}
		a, err := s.renameAgent(r, id, name)
		return toAPIAgent(a), err
	}))
	mux.HandleFunc("DELETE /api/v1/agents/{id}", s.api(PermAgents, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		// This operation only removes the console enrollment and keeps data.
		// Reject a body so destructive UI options cannot be silently ignored.
		var b [1]byte
		if n, err := io.ReadFull(r.Body, b[:]); n != 0 || err != io.EOF {
			return nil, apiErr(400, "DELETE agent takes no body; all backups are kept")
		}
		a, err := s.store.GetAgent(r.Context(), id)
		if err != nil {
			return nil, err
		}
		msg, err := s.removeAgent(r, a, removalOpts{Backups: removeKeepBackups})
		if err != nil {
			return nil, mutationAPIError(err, true)
		}
		return apiAgentRemoval{ID: id, Removed: true, Backups: "kept", History: "kept", Jobs: "deleted", Message: msg}, nil
	}))
}
