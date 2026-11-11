package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/repo"
)

// REST API for scripts, RMM and monitoring tools:
//
//	Authorization: Bearer bzt_...
//
// Tokens belong to a user and carry a role no higher than the user's.

// APIToken is a user's API token (the secret is shown only at creation).
type APIToken struct {
	ID        int64
	UserID    int64
	Username  string
	Name      string
	Role      string
	CreatedAt time.Time
	LastUsed  *time.Time
	ExpiresAt *time.Time
}

func (t APIToken) RoleName() string { return roleTitle(t.Role) }

func (t APIToken) Expired() bool { return t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt) }

var roleRank = map[string]int{"viewer": 1, "restore": 2, "operator": 3, "admin": 4}

func (s *Store) CreateAPIToken(ctx context.Context, owner User, name, role string, days int) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", errors.New("give the token a name (e.g. the tool that uses it)")
	}
	if _, ok := roleByKey(role); !ok {
		return "", errors.New("choose a role")
	}
	if roleRank[role] > roleRank[owner.Role] {
		return "", errors.New("a token cannot have more rights than your own role")
	}
	if days < 0 || days > 3650 {
		return "", errors.New("expiry must be 0 (never) to 3650 days")
	}
	var exp *time.Time
	if days > 0 {
		t := time.Now().AddDate(0, 0, days)
		exp = &t
	}
	tok := "bzt_" + randomToken(30)
	_, err := s.db.Exec(ctx, `INSERT INTO api_tokens(user_id, name, token_hash, role, expires_at) VALUES($1,$2,$3,$4,$5)`,
		owner.ID, name, hashToken(tok), role, exp)
	return tok, err
}

func (s *Store) ListAPITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.db.Query(ctx, `SELECT t.id, t.user_id, u.username, t.name, t.role, t.created_at, t.last_used_at, t.expires_at
		FROM api_tokens t JOIN users u ON u.id=t.user_id WHERE ($1::bigint=0 OR t.user_id=$1) ORDER BY t.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (APIToken, error) {
		var t APIToken
		err := r.Scan(&t.ID, &t.UserID, &t.Username, &t.Name, &t.Role, &t.CreatedAt, &t.LastUsed, &t.ExpiresAt)
		return t, err
	})
}

func (s *Store) DeleteAPIToken(ctx context.Context, id, userID int64) error {
	ct, err := s.db.Exec(ctx, `DELETE FROM api_tokens WHERE id=$1 AND ($2::bigint=0 OR user_id=$2)`, id, userID)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// tokenUser authenticates an API token; the returned user carries the
// token's role.
func (s *Store) tokenUser(ctx context.Context, token string) (User, string, error) {
	if !strings.HasPrefix(token, "bzt_") {
		return User{}, "", ErrNotFound
	}
	var role, name string
	var id int64
	u, err := scanUserWith(s.db.QueryRow(ctx, `UPDATE api_tokens t SET last_used_at=now() FROM users u
		WHERE u.id=t.user_id AND t.token_hash=$1 AND NOT u.disabled AND (t.expires_at IS NULL OR t.expires_at > now())
		RETURNING `+prefixed("u.", userCols)+`, t.role, t.name, t.id`, hashToken(token)), &role, &name, &id)
	if err != nil {
		return User{}, "", err
	}
	// The token's role, but never more than the user currently has.
	if roleRank[role] < roleRank[u.Role] {
		u.Role = role
	}
	return u, fmt.Sprintf("%s (token %q)", u.Username, name), nil
}

func prefixed(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

func scanUserWith(r pgx.Row, extra ...any) (User, error) {
	var u User
	dest := append([]any{&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.Role, &u.Source, &u.Disabled, &u.LastLogin, &u.CreatedAt, &u.TwoFactor}, extra...)
	return u, notFound(r.Scan(dest...))
}

// ---- HTTP

type apiHandler func(w http.ResponseWriter, r *http.Request) (any, error)

type apiError struct {
	code int
	msg  string
}

type apiResponse struct {
	status int
	value  any
}

// apiStreamed tells the wrapper that the handler already wrote the response.
type apiStreamed struct{}

const apiActorKey ctxKey = 20

func (e apiError) Error() string { return e.msg }

func apiErr(code int, format string, a ...any) error {
	return apiError{code, fmt.Sprintf(format, a...)}
}

// api authenticates a token and checks perm; responses are JSON.
func (s *Server) api(perm Perm, h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if wait := s.guard.Blocked(r, ""); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed sign-ins from this address"})
			return
		}
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		u, who, err := s.store.tokenUser(r.Context(), strings.TrimSpace(token))
		if err != nil {
			s.guard.Fail(r, "", "API token sign-ins")
			time.Sleep(300 * time.Millisecond)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing, invalid or expired API token"})
			return
		}
		if !u.Can(string(perm)) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "the token's role does not allow this"})
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), userCtxKey, &u))
		// Keep the real username for two-person approval. The token label
		// belongs only in audit entries, never in identity comparisons.
		r = r.WithContext(context.WithValue(r.Context(), apiActorKey, who))
		v, err := h(w, r)
		if err != nil {
			var ae apiError
			switch {
			case errors.As(err, &ae):
				writeJSON(w, ae.code, map[string]string{"error": ae.msg})
			case errors.Is(err, ErrNotFound):
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			case errors.Is(err, ErrRunActive):
				writeJSON(w, http.StatusConflict, map[string]string{"error": "a run of this job is already queued or running"})
			default:
				s.log.Error("api", "path", r.URL.Path, "err", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			}
			return
		}
		code := http.StatusOK
		if r.Method == http.MethodPost {
			code = http.StatusAccepted
		}
		if _, ok := v.(apiStreamed); ok {
			return
		}
		if resp, ok := v.(apiResponse); ok {
			code, v = resp.status, resp.value
		}
		if code == http.StatusNoContent {
			w.WriteHeader(code)
			return
		}
		writeJSON(w, code, v)
	}
}

// JSON shapes (stable field names, independent of internal structs).

type apiAgent struct {
	ID                int64      `json:"id"`
	Name              string     `json:"name"`
	Hostname          string     `json:"hostname"`
	OS                string     `json:"os"`
	Version           string     `json:"version"`
	Online            bool       `json:"online"`
	LastSeen          *time.Time `json:"last_seen"`
	Recovery          bool       `json:"recovery"`
	LocalIPs          []string   `json:"local_ips"`
	SeenFrom          string     `json:"seen_from"`
	Enrolled          time.Time  `json:"enrolled"`
	MaxRuns           int        `json:"max_runs"`
	EnrollmentTokenID *int64     `json:"enrollment_token_id,omitempty"`
	Capabilities      []string   `json:"capabilities"`
	// Hypervisor is set for an agent on a Proxmox VE node or Hyper-V host:
	// the virtual machines (and containers) a vm job of this agent can back up.
	Hypervisor *apiHypervisor `json:"hypervisor,omitempty"`
	// Disks is the disk inventory a Windows agent reports, for choosing what
	// an image job copies: the disk with Windows, or every internal disk.
	Disks []apiDisk `json:"disks,omitempty"`
}

type apiDisk struct {
	Number int    `json:"number"`
	Model  string `json:"model,omitempty"`
	Size   uint64 `json:"size"`
	Style  string `json:"style"`
	// System: the disk holds the running Windows.
	System bool `json:"system,omitempty"`
	// Bus: "usb", "sd", "virtual-file", "removable", or "" for internal disks.
	Bus string `json:"bus,omitempty"`
	// InAllDisks: an image job of "all disks" (image_disk -1) copies it.
	InAllDisks bool           `json:"in_all_disks"`
	Partitions []apiPartition `json:"partitions"`
}

type apiPartition struct {
	Number int    `json:"number"`
	Size   uint64 `json:"size"`
	// Role: efi, msr, recovery, data or other.
	Role        string   `json:"role"`
	MountPoints []string `json:"mount_points,omitempty"`
	FileSystem  string   `json:"file_system,omitempty"`
	Label       string   `json:"label,omitempty"`
	// Bootable: the active partition of an MBR disk (where BIOS starts Windows).
	Bootable bool `json:"bootable,omitempty"`
}

func toAPIDisks(a Agent) []apiDisk {
	var out []apiDisk
	for _, d := range a.Disks() {
		x := apiDisk{Number: d.Number, Model: d.Model, Size: d.Size, Style: d.Style, System: d.System, Bus: d.Bus,
			InAllDisks: imaging.InAllDisks(d), Partitions: []apiPartition{}}
		for _, p := range d.Partitions {
			x.Partitions = append(x.Partitions, apiPartition{Number: p.Number, Size: p.Length, Role: partitionRole(p),
				MountPoints: p.MountPoints, FileSystem: p.FileSystem, Label: p.Label, Bootable: p.Bootable})
		}
		out = append(out, x)
	}
	return out
}

func partitionRole(p imaging.Partition) string {
	switch p.Kind() {
	case "EFI System":
		return "efi"
	case "Microsoft Reserved":
		return "msr"
	case "Recovery":
		return "recovery"
	case "Basic data", "NTFS/exFAT", "FAT32":
		return "data"
	}
	return "other"
}

// apiHypervisor is the inventory a hypervisor agent reports, limited to the
// guests on its own node.
type apiHypervisor struct {
	Platform string     `json:"platform"` // proxmox | hyperv
	Node     string     `json:"node"`
	Cluster  string     `json:"cluster,omitempty"` // Hyper-V failover cluster
	Version  string     `json:"version,omitempty"`
	Guests   []apiGuest `json:"guests"`
}

type apiGuest struct {
	// ID is the value a vm job names in paths (Proxmox VMID, or the number
	// BackupZit derives from a Hyper-V VM GUID).
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"` // qemu | lxc (Hyper-V VMs report qemu)
	Status      string `json:"status"`
	DiskBytes   uint64 `json:"disk_bytes"`
	MemoryBytes uint64 `json:"memory_bytes"`
	Template    bool   `json:"template,omitempty"`
	// Clustered: a role of a Hyper-V failover cluster, backed up through a
	// cluster job in the console rather than from this host.
	Clustered bool `json:"clustered,omitempty"`
}

func toAPIHypervisor(a Agent) *apiHypervisor {
	inv := a.PVE()
	if inv == nil {
		return nil
	}
	out := &apiHypervisor{Platform: a.Platform(), Node: inv.Node, Cluster: a.HVCluster(), Version: inv.Version, Guests: []apiGuest{}}
	for _, g := range a.LocalGuests() {
		out.Guests = append(out.Guests, apiGuest{ID: strconv.Itoa(g.VMID), Name: g.Name, Type: g.Type, Status: g.Status,
			DiskBytes: g.MaxDisk, MemoryBytes: g.MaxMem, Template: g.Template, Clustered: g.Clustered})
	}
	return out
}

type apiJob struct {
	ID              int64                `json:"id"`
	Name            string               `json:"name"`
	Kind            string               `json:"kind"`
	AgentID         int64                `json:"agent_id"`
	Agent           string               `json:"agent"`
	TargetID        int64                `json:"target_id"`
	Target          string               `json:"target"`
	Enabled         bool                 `json:"enabled"`
	Schedule        string               `json:"schedule"`
	NextRun         *time.Time           `json:"next_run"`
	LastStatus      string               `json:"last_status"`
	LastFinished    *time.Time           `json:"last_finished"`
	Paths           []string             `json:"paths,omitempty"`
	SourceJobID     *int64               `json:"source_job_id,omitempty"`
	RawSchedule     string               `json:"raw_schedule"`
	Excludes        []string             `json:"excludes"`
	ImageDisk       *int                 `json:"image_disk,omitempty"`
	ImagePartitions []int                `json:"image_partitions,omitempty"`
	Retention       repo.RetentionPolicy `json:"retention"`
	Options         apiJobOptions        `json:"options"`
	// RPO: the objective in hours, whether it is met and the newest good
	// backup.
	RPOHours       int        `json:"rpo_hours,omitempty"`
	RPOMet         *bool      `json:"rpo_met,omitempty"`
	LastGoodBackup *time.Time `json:"last_good_backup,omitempty"`
}

type apiRun struct {
	ID         int64           `json:"id"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Trigger    string          `json:"trigger"`
	AgentID    int64           `json:"agent_id"`
	Agent      string          `json:"agent"`
	JobID      *int64          `json:"job_id"`
	Job        string          `json:"job"`
	QueuedAt   time.Time       `json:"queued_at"`
	StartedAt  *time.Time      `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	DurationS  int             `json:"duration_s"`
	Snapshot   string          `json:"snapshot,omitempty"`
	Message    string          `json:"message,omitempty"`
	Errors     []string        `json:"errors,omitempty"`
	Stats      json.RawMessage `json:"stats,omitempty"`
	Suspicious string          `json:"suspicious,omitempty"`
	Expired    bool            `json:"expired"`
}

func toAPIAgent(a Agent) apiAgent {
	caps := a.Capabilities
	if caps == nil {
		caps = []string{}
	}
	return apiAgent{a.ID, a.Name(), a.Hostname, a.OS + "/" + a.Arch, a.Version, a.Online(), a.LastSeen, a.Recovery, a.LocalIPs, a.RemoteAddr, a.EnrolledAt, max(a.MaxRuns, 1), a.EnrollmentTokenID, caps, toAPIHypervisor(a), toAPIDisks(a)}
}

func toAPIJob(j Job) apiJob {
	x := apiJob{ID: j.ID, Name: j.Name, Kind: j.Kind, AgentID: j.AgentID, Agent: j.Hostname, TargetID: j.TargetID, Target: j.TargetName,
		Enabled: j.Enabled, Schedule: DescribeSchedule(j.Schedule), LastStatus: deref(j.LastStatus), LastFinished: j.LastFinished,
		Paths: j.Paths, SourceJobID: j.SourceJobID, RawSchedule: j.Schedule, Excludes: j.Excludes, ImageDisk: j.ImageDisk, ImagePartitions: j.ImagePartitions, Retention: j.Retention, Options: publicJobOptions(j.Options)}
	if j.Enabled {
		if n := NextRun(j.Schedule, time.Now()); !n.IsZero() {
			x.NextRun = &n
		}
	}
	return x
}

func toAPIRunJSON(r Run) apiRun {
	return apiRun{ID: r.ID, Kind: r.Kind, Status: r.Status, Trigger: r.Trigger, AgentID: r.AgentID, Agent: r.Hostname,
		JobID: r.JobID, Job: deref(r.JobName), QueuedAt: r.QueuedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		DurationS: int(r.Duration().Seconds()), Snapshot: r.SnapshotID, Message: r.Message, Errors: r.Errors,
		Stats: r.Stats, Suspicious: r.Anomaly, Expired: r.Expired}
}

func pathInt(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, apiErr(http.StatusBadRequest, "invalid %s", name)
	}
	return id, nil
}

func (s *Server) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/status", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		sum, err := s.store.Summary(r.Context())
		return map[string]any{"version": s.Version, "source_url": s.sourceURL(), "agents": sum.Agents, "agents_online": sum.AgentsOnline, "jobs": sum.Jobs,
			"storage_targets": sum.Targets, "runs_24h": sum.Runs24h, "failed_24h": sum.Failed24h, "warnings_24h": sum.Warning24h,
			"active_runs": sum.Running, "suspicious_unreviewed": sum.Suspicious,
			"ok": sum.Failed24h == 0 && sum.Suspicious == 0 && sum.AgentsOnline == sum.Agents}, err
	}))
	mux.HandleFunc("GET /api/v1/agents", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		list, err := s.store.ListAgents(r.Context())
		out := []apiAgent{}
		for _, a := range list {
			out = append(out, toAPIAgent(a))
		}
		return out, err
	}))
	mux.HandleFunc("GET /api/v1/jobs", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		list, err := s.store.ListJobs(r.Context())
		cov := s.coverageMap(r.Context())
		out := []apiJob{}
		for _, j := range list {
			out = append(out, withRPO(toAPIJob(j), j, cov[j.ID]))
		}
		return out, err
	}))
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		j, err := s.store.GetJob(r.Context(), id)
		if err != nil {
			return nil, err
		}
		return withRPO(toAPIJob(j), j, s.coverageMap(r.Context())[j.ID]), nil
	}))
	mux.HandleFunc("POST /api/v1/jobs/{id}/run", s.api(PermRun, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		if _, err := s.store.GetJob(r.Context(), id); err != nil {
			return nil, err
		}
		rid, err := s.store.QueueBackup(r.Context(), id, "api")
		if err != nil {
			return nil, err
		}
		s.audit(r, "api.job_run", "job %d, run %d", id, rid)
		return map[string]int64{"run_id": rid}, nil
	}))
	mux.HandleFunc("POST /api/v1/jobs/{id}/test", s.api(PermRun, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		rid, err := s.store.QueueRestoreTest(r.Context(), id, "api")
		if err != nil && !errors.Is(err, ErrRunActive) && !errors.Is(err, ErrNotFound) {
			return nil, apiErr(http.StatusConflict, "%s", err.Error())
		}
		if err != nil {
			return nil, err
		}
		s.audit(r, "api.job_test", "job %d, run %d", id, rid)
		return map[string]int64{"run_id": rid}, nil
	}))
	mux.HandleFunc("GET /api/v1/runs", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		q := r.URL.Query()
		job, _ := strconv.ParseInt(q.Get("job"), 10, 64)
		agent, _ := strconv.ParseInt(q.Get("agent"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		var before int64
		if raw := q.Get("before"); raw != "" {
			var err error
			before, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || before <= 0 {
				return nil, apiErr(400, "before must be a positive run ID")
			}
		}
		list, err := s.store.ListRuns(r.Context(), RunFilter{JobID: job, AgentID: agent, Limit: limit, BeforeID: before, Status: q.Get("status"), OrderByID: true})
		out := []apiRun{}
		for _, x := range list {
			out = append(out, toAPIRunJSON(x))
		}
		return out, err
	}))
	mux.HandleFunc("GET /api/v1/runs/{id}", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		x, err := s.store.GetRun(r.Context(), id)
		if err != nil {
			return nil, err
		}
		return toAPIRunJSON(x), nil
	}))
	mux.HandleFunc("POST /api/v1/runs/{id}/cancel", s.api(PermRun, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		queued, err := s.cancelRun(r, id)
		if errors.Is(err, errNotCancellable) {
			return nil, apiErr(http.StatusConflict, "%s", err.Error())
		} else if err != nil {
			return nil, err
		}
		state := "stopping"
		if queued {
			state = "cancelled"
		}
		return map[string]any{"run_id": id, "state": state}, nil
	}))
	mux.HandleFunc("GET /api/v1/report", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		rep, err := s.reportFromRequest(r)
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, "%s", err.Error())
		}
		jobs := []map[string]any{}
		for _, j := range rep.Jobs {
			jobs = append(jobs, map[string]any{"job_id": j.JobID, "job": j.Name, "agent": j.Hostname, "runs": j.Runs,
				"success": j.Success, "warning": j.Warning, "failed": j.Failed, "success_rate": j.SuccessRate,
				"last_status": j.LastStatus, "last_good_backup": j.LastSuccess, "uploaded_bytes": j.BytesStored,
				"restore_test": j.LastTest, "restore_test_at": j.LastTestAt})
		}
		return map[string]any{"from": rep.Period.From, "to": rep.Period.To, "label": rep.Period.Label, "scope": rep.Scope(),
			"backups": rep.Backups, "success": rep.Success, "warning": rep.Warning, "failed": rep.Failed,
			"success_rate": rep.SuccessRate, "read_bytes": rep.BytesRead, "uploaded_bytes": rep.BytesStored,
			"restores": rep.Restores, "restore_tests": rep.Tests, "restore_tests_failed": rep.TestsFailed,
			"suspicious": rep.Suspicious, "jobs": jobs}, nil
	}))
	mux.HandleFunc("GET /api/v1/targets", s.api(PermView, func(w http.ResponseWriter, r *http.Request) (any, error) {
		list, err := s.store.ListTargets(r.Context())
		out := []map[string]any{}
		for _, t := range list { // no credentials
			out = append(out, map[string]any{"id": t.ID, "name": t.Name, "kind": t.Kind, "location": t.URL,
				"encrypted": t.Encrypted, "immutable_days": t.S3LockDays})
		}
		return out, err
	}))
	s.registerAPIMore(mux)
	s.registerAPIProvision(mux)
	s.registerAPIJobs(mux)
	s.registerAPITargets(mux)
	s.registerAPIRecovery(mux)
	s.registerAPIImage(mux)
	s.registerAPIDocs(mux)
	s.registerAPIAgentPackages(mux)
	s.registerAPIAgentMutations(mux)
}

// ---- token management pages (My account)

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request, user string) {
	me := currentUser(r)
	days, _ := strconv.Atoi(r.FormValue("days"))
	tok, err := s.store.CreateAPIToken(r.Context(), *me, r.FormValue("name"), r.FormValue("role"), days)
	if err != nil {
		redirectErr(w, r, "/account#tokens", err)
		return
	}
	s.audit(r, "api_token.create", "%q, role %s", r.FormValue("name"), r.FormValue("role"))
	s.renderAccount(w, r, user, tok)
}

func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request, _ string) {
	me := currentUser(r)
	id, _ := pathID(r)
	owner := me.ID
	if me.Can(string(PermUsers)) {
		owner = 0 // administrators may revoke any token
	}
	if err := s.store.DeleteAPIToken(r.Context(), id, owner); err != nil {
		redirectErr(w, r, "/account#tokens", errors.New("token not found"))
		return
	}
	s.audit(r, "api_token.delete", "token %d", id)
	back := "/account#tokens"
	if strings.HasPrefix(r.FormValue("back"), "/users") {
		back = "/users"
	}
	redirectMsg(w, r, back, "API token revoked.")
}

// withRPO adds the RPO standing of a job.
func withRPO(x apiJob, j Job, c *JobCoverage) apiJob {
	st := rpoState(j, c, time.Now())
	x.LastGoodBackup = st.LastGood
	if st.Hours > 0 {
		met := !st.Missed
		x.RPOHours, x.RPOMet = st.Hours, &met
	}
	return x
}
