package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/max-zit/backupzit/internal/repo"
)

// This allowlist excludes NAS/M365 secrets and settings for other job types.
type apiJobOptions struct {
	VSS            *bool  `json:"vss,omitempty"`
	ImageDisks     []int  `json:"image_disks,omitempty"`
	LimitMBps      int    `json:"limit_mbps,omitempty"`
	LimitFrom      int    `json:"limit_from,omitempty"`
	LimitTo        int    `json:"limit_to,omitempty"`
	PreCommand     string `json:"pre_command,omitempty"`
	PostCommand    string `json:"post_command,omitempty"`
	CommandMinutes int    `json:"command_minutes,omitempty"`
	Compression    string `json:"compression,omitempty"`
	MaxHours       int    `json:"max_hours,omitempty"`
	RPOHours       int    `json:"rpo_hours,omitempty"`
	Window         bool   `json:"window,omitempty"`
	WindowFrom     int    `json:"window_from,omitempty"`
	WindowTo       int    `json:"window_to,omitempty"`
}

func publicJobOptions(o JobOptions) apiJobOptions {
	b, _ := json.Marshal(o)
	var out apiJobOptions
	_ = json.Unmarshal(b, &out)
	return out
}

type apiJobRequest struct {
	Kind            string               `json:"kind"`
	AgentID         int64                `json:"agent_id"`
	TargetID        int64                `json:"target_id"`
	Name            string               `json:"name"`
	Paths           []string             `json:"paths"`
	Excludes        []string             `json:"excludes"`
	Schedule        string               `json:"schedule"`
	Enabled         bool                 `json:"enabled"`
	ImageDisk       *int                 `json:"image_disk"`
	ImagePartitions []int                `json:"image_partitions"`
	Retention       repo.RetentionPolicy `json:"retention"`
	Options         apiJobOptions        `json:"options"`
}

func readJobMutation(r *http.Request, old Job, create bool) (Job, error) {
	q := apiJobRequest{Kind: old.Kind, AgentID: old.AgentID, TargetID: old.TargetID, Name: old.Name, Paths: old.Paths, Excludes: old.Excludes,
		Schedule: old.Schedule, Enabled: old.Enabled, ImageDisk: old.ImageDisk, ImagePartitions: old.ImagePartitions, Retention: old.Retention, Options: publicJobOptions(old.Options)}
	if create {
		q.Kind = JobFiles
		q.Enabled = true
		q.Retention = repo.RetentionPolicy{KeepLast: 3, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}
	}
	if old.ImageDisk != nil {
		d := *old.ImageDisk
		q.ImageDisk = &d
	}
	if old.ImagePartitions != nil {
		q.ImagePartitions = append([]int{}, old.ImagePartitions...)
	}
	// Decode through RawMessage first to reject null for scalar/object
	// settings; null remains useful for slices and the image_disk selector.
	var fields map[string]json.RawMessage
	if err := readAPIJSON(r, &fields); err != nil {
		return Job{}, err
	}
	if create && fields["retention"] != nil {
		q.Retention = repo.RetentionPolicy{}
	}
	for _, key := range []string{"kind", "agent_id", "target_id", "name", "schedule", "enabled", "retention", "options"} {
		if v, ok := fields[key]; ok && string(v) == "null" {
			return Job{}, apiErr(400, "%s cannot be null", key)
		}
	}
	for _, key := range []string{"retention", "options"} {
		if raw := fields[key]; raw != nil {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(raw, &nested); err != nil {
				return Job{}, apiErr(400, "%s must be an object", key)
			}
			for field, value := range nested {
				if string(value) == "null" && field != "vss" && field != "image_disks" {
					return Job{}, apiErr(400, "%s.%s cannot be null", key, field)
				}
			}
		}
	}
	b, _ := json.Marshal(fields)
	// Reuse the strict decoder, including nested option allowlists.
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(bytes.NewReader(b))
	if err := readAPIJSON(clone, &q); err != nil {
		return Job{}, err
	}
	if q.Kind != JobFiles && q.Kind != JobImage {
		return Job{}, apiErr(400, "only files and image jobs can be managed through this endpoint")
	}
	if !create {
		if q.Kind != old.Kind || q.AgentID != old.AgentID || q.TargetID != old.TargetID {
			return Job{}, apiErr(400, "kind, agent_id and target_id cannot be changed; create a new job")
		}
		if q.Enabled != old.Enabled {
			return Job{}, apiErr(400, "use the enable or disable endpoint to change enabled")
		}
		if !reflect.DeepEqual(q.ImageDisk, old.ImageDisk) || !reflect.DeepEqual(q.ImagePartitions, old.ImagePartitions) {
			return Job{}, apiErr(400, "image_disk and image_partitions cannot be changed; use options.image_disks for whole disk selections")
		}
	}
	j := old
	j.Kind, j.AgentID, j.TargetID, j.Name, j.Paths, j.Excludes, j.Schedule, j.Enabled = q.Kind, q.AgentID, q.TargetID, q.Name, q.Paths, q.Excludes, q.Schedule, q.Enabled
	j.ImageDisk, j.ImagePartitions, j.Retention = q.ImageDisk, q.ImagePartitions, q.Retention
	// Marshal omitempty would lose explicit zero/false/null PATCH values.
	// Start from a clean allowlisted options value for these two job kinds.
	j.Options = JobOptions{}
	b, _ = json.Marshal(q.Options)
	_ = json.Unmarshal(b, &j.Options)
	return j, nil
}

func mutationAPIError(err error, conflict bool) error {
	if err == nil {
		return nil
	}
	var ae apiError
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &ae), errors.Is(err, ErrNotFound), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, errCommandPermission):
		return apiErr(403, "%s", err)
	case errors.Is(err, errApprovalPending), errors.Is(err, errGroupMember), errors.Is(err, errAlreadyRequested):
		return apiErr(409, "%s", err)
	case errors.As(err, &pe):
		return err
	case conflict:
		return apiErr(409, "%s", err)
	default:
		return apiErr(400, "%s", err)
	}
}

func (s *Server) apiJobByID(r *http.Request, id int64) (any, error) {
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		return nil, err
	}
	return withRPO(toAPIJob(j), j, s.coverageMap(r.Context())[j.ID]), nil
}

func (s *Server) registerAPIJobs(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/jobs", s.api(PermJobs, func(w http.ResponseWriter, r *http.Request) (any, error) {
		j, err := readJobMutation(r, Job{}, true)
		if err != nil {
			return nil, err
		}
		id, _, err := s.createBackupJob(r, j)
		if err != nil {
			return nil, mutationAPIError(err, false)
		}
		out, err := s.apiJobByID(r, id)
		return apiResponse{201, out}, err
	}))
	mux.HandleFunc("PATCH /api/v1/jobs/{id}", s.api(PermJobs, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		old, err := s.store.GetJob(r.Context(), id)
		if err != nil {
			return nil, err
		}
		j, err := readJobMutation(r, old, false)
		if err != nil {
			return nil, err
		}
		if _, err := s.updateBackupJob(r, old, j); err != nil {
			return nil, mutationAPIError(err, false)
		}
		return s.apiJobByID(r, id)
	}))
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", s.api(PermJobs, func(w http.ResponseWriter, r *http.Request) (any, error) {
		id, err := pathInt(r, "id")
		if err != nil {
			return nil, err
		}
		if err := s.deleteBackupJob(r, id); err != nil {
			return nil, mutationAPIError(err, true)
		}
		return apiResponse{204, nil}, nil
	}))
}
