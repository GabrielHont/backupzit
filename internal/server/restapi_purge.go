package server

// Unirea: deleting every backup of a console. Unirea Cloud runs one console per firm; when a firm's data
// is deleted (it stopped paying, 14 + 45 days later), the portal empties its console through this route:
// every agent, current or removed, is deleted together with its repositories on the console's storage.
// The software on the firm's computers is not touched (no uninstall): its enrollment simply stops working.

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

const purgeSetting = "unirea.purge"

// purgeConfirm must be sent verbatim, so the route cannot be called by mistake.
const purgeConfirm = "delete every backup in this console"

type purgeRepo struct {
	Agent  string `json:"agent"`
	Target string `json:"target"`
	URL    string `json:"url"`
	Result string `json:"result"`
	OK     bool   `json:"ok"`
}

// purgeState is kept in settings, so the portal can read the outcome after a restart.
type purgeState struct {
	StartedAt    time.Time   `json:"started_at"`
	FinishedAt   *time.Time  `json:"finished_at,omitempty"`
	By           string      `json:"by"`
	Agents       int         `json:"agents"`
	Repositories int         `json:"repositories"`
	Results      []purgeRepo `json:"results"`
	// Done and every repository deleted.
	OK bool `json:"ok"`
}

// allAgents lists every agent, also the removed ones kept as history of their backups.
func (s *Store) allAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.Query(ctx, `SELECT `+agentCols+` FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Agent, error) { return scanAgent(r) })
}

func (s *Server) registerAPIPurge(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/purge", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var st purgeState
		if err := s.store.GetSetting(r.Context(), purgeSetting, &st); err != nil {
			return nil, err
		}
		if st.StartedAt.IsZero() {
			return map[string]any{"started": false}, nil
		}
		return map[string]any{"started": true, "state": st}, nil
	}))
	mux.HandleFunc("POST /api/v1/purge", s.api(PermAgents, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var q struct {
			Confirm string `json:"confirm"`
		}
		if err := readAPIJSON(r, &q); err != nil {
			return nil, err
		}
		if q.Confirm != purgeConfirm {
			return nil, apiErr(http.StatusBadRequest, "confirm must be %q", purgeConfirm)
		}
		if held, err := s.approvalForChange(r, "console.purge", 0, "all backups", q); err != nil {
			return nil, mutationAPIError(err, true)
		} else if held {
			return nil, mutationAPIError(errApprovalPending, true)
		}
		ctx := r.Context()
		var prev purgeState
		if err := s.store.GetSetting(ctx, purgeSetting, &prev); err != nil {
			return nil, err
		}
		if !prev.StartedAt.IsZero() && prev.FinishedAt == nil && time.Since(prev.StartedAt) < 13*time.Hour {
			return nil, apiErr(http.StatusConflict, "a purge is already running since %s", prev.StartedAt.Format(time.RFC3339))
		}
		by := currentUser(r).Username
		if actor, ok := ctx.Value(apiActorKey).(string); ok {
			by = actor
		}
		agents, err := s.store.allAgents(ctx)
		if err != nil {
			return nil, err
		}
		type work struct {
			agent Agent
			repos []agentRepo
		}
		var todo []work
		repos := 0
		for _, a := range agents {
			x, err := s.store.agentRepos(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			todo = append(todo, work{a, x})
			repos += len(x)
		}
		// The agents go first, in the request: none can sign in or start a backup again while the
		// repositories are deleted. Their jobs, runs and pending removals go with them (ON DELETE CASCADE).
		for _, w := range todo {
			if err := s.store.DeleteAgent(ctx, w.agent.ID); err != nil {
				return nil, err
			}
		}
		_, _ = s.store.db.Exec(ctx, `DELETE FROM enrollment_tokens`)
		st := purgeState{StartedAt: time.Now().UTC(), By: by, Agents: len(todo), Repositories: repos, Results: []purgeRepo{}}
		if err := s.store.SetSetting(ctx, purgeSetting, st); err != nil {
			return nil, err
		}
		s.audit(r, "console.purge", "%d agents, %d repositories: every backup is being deleted", len(todo), repos)
		remote := s.guard.ClientIP(r)
		s.cleanups.Add(1)
		go func() { // #nosec G118 -- background work that outlives the request on purpose
			defer s.cleanups.Done()
			cctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
			defer cancel()
			ok := true
			for _, w := range todo {
				for _, x := range w.repos {
					detail, _ := s.cleanupRepo(cctx, x, removeAllBackups)
					done := len(detail) >= len("repository deleted") && detail[:len("repository deleted")] == "repository deleted"
					ok = ok && done
					st.Results = append(st.Results, purgeRepo{Agent: w.agent.Hostname, Target: x.TargetName, URL: x.URL, Result: detail, OK: done})
					_ = s.store.SetSetting(cctx, purgeSetting, st)
					_, _ = s.store.AppendAudit(cctx, by, "agent.backups", fmt.Sprintf("%s, %s on %s: %s", w.agent.Hostname, x.URL, x.TargetName, detail), remote)
				}
			}
			now := time.Now().UTC()
			st.FinishedAt, st.OK = &now, ok
			_ = s.store.SetSetting(cctx, purgeSetting, st)
			s.log.Info("console purge finished", "agents", len(todo), "repositories", repos, "ok", ok)
		}()
		return map[string]any{"started": true, "agents": len(todo), "repositories": repos}, nil
	}))
}
