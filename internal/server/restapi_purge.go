package server

// Unirea: deleting every backup of a console. Unirea Cloud runs one console per firm; when a firm's data
// is deleted (it stopped paying, 14 + 45 days later), the portal empties its console through this route:
// every agent, current or removed, is deleted, and then every repository the console ever wrote to.
// The software on the firm's computers is not touched (no uninstall): its enrollment simply stops working.
//
// The list of repositories is written to the settings before anything is deleted, so a restart of the
// console resumes the deletion (StartPurgeResume) instead of forgetting what was left.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/max-zit/backupzit/internal/backend"
)

const purgeSetting = "unirea.purge.v2"

// purgeConfirm must be sent verbatim, so the route cannot be called by mistake.
const purgeConfirm = "delete every backup in this console"

// purgeSecondPass is how long the deletion waits before going over every repository again, when backups
// were running at the start: a backup already started keeps writing until its agent gives up.
var purgeSecondPass = 15 * time.Minute

// purgeLastPass is the last pass, for long backups that were running.
var purgeLastPass = 3 * time.Hour

type purgeRepo struct {
	TargetID int64  `json:"target_id"`
	Target   string `json:"target"`
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	Agents   string `json:"agents"`
	Result   string `json:"result,omitempty"`
	OK       bool   `json:"ok"`
	Done     bool   `json:"done"`
}

// purgeState is kept in settings, so the outcome survives a restart and the portal can read it.
type purgeState struct {
	StartedAt  time.Time   `json:"started_at"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
	By         string      `json:"by"`
	Agents     int         `json:"agents"`
	Running    int         `json:"running_at_start"`
	Repos      []purgeRepo `json:"repositories"`
	// Done, and every repository confirmed deleted.
	OK bool `json:"ok"`
}

// purger makes sure one deletion runs at a time in this console.
type purger struct {
	mu      sync.Mutex
	running bool
}

var consolePurger purger

func (p *purger) start() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return false
	}
	p.running = true
	return true
}

func (p *purger) stop() {
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
}

// purgeInventory lists every repository the console wrote to: backups and copies of every agent, also
// removed ones, also on storage targets deleted since (those cannot be reached any more and are reported).
func (s *Store) purgeInventory(ctx context.Context) ([]purgeRepo, error) {
	rows, err := s.db.Query(ctx, `SELECT COALESCE(r.target_id,0), COALESCE(t.name,''), COALESCE(t.kind,''), r.repo_url,
			string_agg(DISTINCT COALESCE(a.hostname,'?'), ', ')
		FROM runs r LEFT JOIN storage_targets t ON t.id=r.target_id LEFT JOIN agents a ON a.id=r.agent_id
		WHERE r.repo_url<>''
		GROUP BY 1,2,3,4 ORDER BY 2,4`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (purgeRepo, error) {
		var x purgeRepo
		err := r.Scan(&x.TargetID, &x.Target, &x.Kind, &x.URL, &x.Agents)
		return x, err
	})
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
		ctx := r.Context()
		// Four-eyes approval cannot carry this out later; with it on, an administrator deletes in the console.
		if s.store.fourEyes(ctx) {
			return nil, apiErr(http.StatusConflict, "four-eyes approval is on: remove the agents with their backups in the console")
		}
		if !consolePurger.start() {
			return nil, apiErr(http.StatusConflict, "a deletion of every backup is already running")
		}
		started := false
		defer func() {
			if !started {
				consolePurger.stop()
			}
		}()
		var prev purgeState
		if err := s.store.GetSetting(ctx, purgeSetting, &prev); err != nil {
			return nil, err
		}
		by := currentUser(r).Username
		if actor, ok := ctx.Value(apiActorKey).(string); ok {
			by = actor
		}
		st, err := s.preparePurge(ctx, prev, by)
		if err != nil {
			return nil, err
		}
		s.audit(r, "console.purge", "%d agents, %d repositories: every backup is being deleted", st.Agents, len(st.Repos))
		started = true
		s.runPurge(st, s.guard.ClientIP(r))
		return map[string]any{"started": true, "agents": st.Agents, "repositories": len(st.Repos)}, nil
	}))
}

// preparePurge writes down every repository, then deletes the agents (none can sign in, start a backup or
// be enrolled again) and keeps what an unfinished earlier deletion had still to do.
func (s *Server) preparePurge(ctx context.Context, prev purgeState, by string) (purgeState, error) {
	repos, err := s.store.purgeInventory(ctx)
	if err != nil {
		return purgeState{}, err
	}
	known := map[string]bool{}
	for _, x := range repos {
		known[x.URL] = true
	}
	// An earlier deletion that did not finish (the console restarted): its repositories are done again;
	// their runs are gone, so only the manifest still knows them.
	for _, x := range prev.Repos {
		if !known[x.URL] && !(x.Done && x.OK) {
			x.Done, x.OK, x.Result = false, false, ""
			repos = append(repos, x)
			known[x.URL] = true
		}
	}
	var running int
	if err := s.store.db.QueryRow(ctx, `SELECT count(*) FROM runs WHERE status IN ('running','stopping')`).Scan(&running); err != nil {
		return purgeState{}, err
	}
	st := purgeState{StartedAt: time.Now().UTC(), By: by, Running: running, Repos: repos}
	if err := s.store.SetSetting(ctx, purgeSetting, st); err != nil {
		return purgeState{}, err
	}
	n, err := s.deleteAllAgents(ctx)
	if err != nil {
		return purgeState{}, err
	}
	st.Agents = n
	return st, s.store.SetSetting(ctx, purgeSetting, st)
}

// deleteAllAgents removes every agent and enrollment code; agents enrolled meanwhile are caught by the
// next round. Also run when a deletion resumes after a restart, in case the console stopped half-way.
func (s *Server) deleteAllAgents(ctx context.Context) (int, error) {
	n := 0
	for round := 0; round < 3; round++ {
		if _, err := s.store.db.Exec(ctx, `DELETE FROM enrollment_tokens`); err != nil {
			return n, err
		}
		agents, err := s.store.allAgents(ctx)
		if err != nil {
			return n, err
		}
		if len(agents) == 0 {
			break
		}
		for _, a := range agents {
			if err := s.store.DeleteAgent(ctx, a.ID); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// runPurge deletes the repositories in the background and records each outcome.
func (s *Server) runPurge(st purgeState, remote string) {
	s.cleanups.Add(1)
	go func() { // #nosec G118 -- background work that outlives the request on purpose
		defer s.cleanups.Done()
		defer consolePurger.stop()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
		defer cancel()
		pass := func() bool {
			ok := true
			for i := range st.Repos {
				x := &st.Repos[i]
				if x.Done && x.OK {
					continue
				}
				x.Result, x.OK = s.purgeRepo(ctx, *x)
				x.Done = true
				ok = ok && x.OK
				_ = s.store.SetSetting(ctx, purgeSetting, st)
				_, _ = s.store.AppendAudit(ctx, st.By, "agent.backups", fmt.Sprintf("%s, %s on %s: %s", x.Agents, x.URL, x.Target, x.Result), remote)
			}
			return ok
		}
		ok := pass()
		if st.Running > 0 {
			// A backup that was running may still write until its agent gives up: go over everything again
			// after 15 minutes and after 3 hours.
			for _, wait := range []time.Duration{purgeSecondPass, purgeLastPass} {
				select {
				case <-time.After(wait):
				case <-ctx.Done():
				}
				for i := range st.Repos {
					st.Repos[i].Done = false
					st.Repos[i].OK = false
				}
				ok = pass()
			}
		}
		// Nothing may be left: a repository that has files again is reported, not counted as deleted.
		for i := range st.Repos {
			x := &st.Repos[i]
			if !x.OK {
				continue
			}
			if left := s.purgeLeft(ctx, *x); left != "" {
				x.OK, x.Result = false, left
				ok = false
			}
		}
		now := time.Now().UTC()
		st.FinishedAt, st.OK = &now, ok
		_ = s.store.SetSetting(ctx, purgeSetting, st)
		s.log.Info("console purge finished", "agents", st.Agents, "repositories", len(st.Repos), "ok", ok)
	}()
}

// purgeRepo deletes one repository. It counts as deleted only when nothing is left and the storage cannot
// keep hidden versions of it (Object Lock, hardened storage): those are reported, not hidden.
func (s *Server) purgeRepo(ctx context.Context, x purgeRepo) (string, bool) {
	if x.TargetID == 0 || x.Target == "" {
		return "not deleted: the storage target was removed from the console", false
	}
	t, err := s.store.GetTarget(ctx, x.TargetID)
	if err != nil {
		return "not deleted: the storage target was removed from the console", false
	}
	detail, _ := s.cleanupRepo(ctx, agentRepo{TargetID: x.TargetID, TargetName: x.Target, TargetKind: x.Kind, URL: x.URL}, removeAllBackups)
	if !strings.HasPrefix(detail, "repository deleted") {
		return detail, false
	}
	if t.S3LockDays > 0 || t.HardenedKey != "" || t.Kind == "hardened" {
		return detail + "; the storage keeps locked or hidden versions until they expire", false
	}
	return detail, true
}

// purgeLeft lists a deleted repository again; it describes what is still there ("" when empty).
func (s *Server) purgeLeft(ctx context.Context, x purgeRepo) string {
	t, err := s.store.GetTarget(ctx, x.TargetID)
	if err != nil {
		return "not confirmed: the storage target was removed from the console"
	}
	be, err := backend.Open(ctx, x.URL, backend.Options{
		SFTPPassword: t.SFTPPassword, SFTPHostKey: t.SFTPHostKey,
		S3AccessKey: t.S3AccessKey, S3SecretKey: t.S3SecretKey, S3Region: t.S3Region, S3LockDays: t.S3LockDays,
		SMBPassword: t.SMBPassword, SMBDomain: t.SMBDomain,
		HardenedKey: t.HardenedKey, HardenedFingerprint: t.HardenedFingerprint,
		AzureKey: t.AzureKey, AzureSAS: t.AzureSAS,
	})
	if err != nil {
		return "not confirmed: " + err.Error()
	}
	defer be.Close()
	names, err := be.List(ctx, "")
	if err != nil {
		return "not confirmed: " + err.Error()
	}
	if len(names) > 0 {
		return fmt.Sprintf("not deleted: %d files are there again (a backup kept writing)", len(names))
	}
	return ""
}

// StartPurgeResume finishes, after a restart, a deletion of every backup that the console had begun.
func (s *Server) StartPurgeResume(ctx context.Context) {
	var st purgeState
	if err := s.store.GetSetting(ctx, purgeSetting, &st); err != nil || st.StartedAt.IsZero() || st.FinishedAt != nil {
		return
	}
	if !consolePurger.start() {
		return
	}
	s.log.Info("resuming the deletion of every backup", "repositories", len(st.Repos))
	// The console may have stopped before every agent was deleted.
	if n, err := s.deleteAllAgents(ctx); err != nil {
		s.log.Error("resume purge: agents", "err", err)
	} else {
		st.Agents += n
	}
	s.runPurge(st, "")
}
