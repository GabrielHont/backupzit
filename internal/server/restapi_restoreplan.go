package server

import (
	"net/http"
)

// Unirea fork: restore plans through the API (see store_restoreplan.go).
func (s *Server) registerAPIRestorePlans(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/restore-plans", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req struct {
			RunID        int64 `json:"run_id"`
			Image        int   `json:"image"`
			AgentID      int64 `json:"agent_id"`
			ConfirmErase bool  `json:"confirm_erase"`
		}
		if err := readAPIJSON(r, &req); err != nil {
			return nil, err
		}
		if !req.ConfirmErase {
			return nil, apiErr(400, "confirm_erase must be true: everything now on the computer is replaced")
		}
		if req.RunID <= 0 || req.AgentID <= 0 {
			return nil, apiErr(400, "run_id and agent_id are required")
		}
		p, err := s.store.CreateRestorePlan(r.Context(), RestorePlanOptions{
			BackupRunID: req.RunID, Image: req.Image, TargetAgentID: req.AgentID,
			Server: s.publicURL(r), Fingerprint: s.CertFingerprint, CreatedBy: "api",
		})
		if err != nil {
			return nil, mutationAPIError(err, false)
		}
		s.audit(r, "restore.plan", "plan #%d: image #%d onto agent %d", p.ID, req.RunID, req.AgentID)
		return apiResponse{201, p}, nil
	}))
	mux.HandleFunc("GET /api/v1/restore-plans/{id}", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		return s.store.GetRestorePlan(r.Context(), id)
	}))
	mux.HandleFunc("POST /api/v1/restore-plans/{id}/cancel", s.api(PermRestore, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		if err := s.store.CancelRestorePlan(r.Context(), id); err != nil {
			return nil, mutationAPIError(err, true)
		}
		s.audit(r, "restore.plan_cancel", "plan #%d", id)
		return s.store.GetRestorePlan(r.Context(), id)
	}))
}
