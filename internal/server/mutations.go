package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/max-zit/backupzit/internal/repo"
)

var errApprovalPending = errors.New("waiting for approval: another user must approve this change under Approvals in the console")
var errGroupMember = errors.New("this job belongs to a group; change the group in the console")
var errCommandPermission = errors.New("only administrators may change commands that run on an agent")

// These operations are shared by the HTML handlers and the public API.
// Keep audit and approval here, alongside the corresponding mutation.
func (s *Server) createStorageTarget(r *http.Request, t Target) (int64, string, error) {
	if t.S3LockDays < 0 || t.S3LockDays > 3650 {
		return 0, "", errors.New("immutability period must be 0 to 3650 days")
	}
	if t.Encrypted && t.RecoveryKey == "" {
		t.RecoveryKey = NewRecoveryKey()
	}
	id, err := s.store.CreateTarget(r.Context(), t)
	if err == nil {
		s.audit(r, "storage.create", "%s (%s) %s", t.Name, t.Kind, t.URL)
	}
	return id, t.RecoveryKey, err
}

func (s *Server) deleteStorageTarget(r *http.Request, id int64) error {
	t, err := s.store.GetTarget(r.Context(), id)
	if err != nil {
		return err
	}
	var used bool
	if err := s.store.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM jobs WHERE target_id=$1)`, id).Scan(&used); err != nil {
		return err
	}
	if used {
		return errors.New("target is used by jobs; delete them first")
	}
	if held, err := s.approvalForChange(r, "storage.delete", id, t.Name, nil); err != nil {
		return err
	} else if held {
		return errApprovalPending
	}
	if err := s.store.DeleteTarget(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "storage.delete", "target %d", id)
	return nil
}

func (s *Server) createBackupJob(r *http.Request, job Job) (int64, string, error) {
	if job.Options.HasCommands() && !canCommands(r) {
		return 0, "", errCommandPermission
	}
	note, err := s.m365Precheck(r.Context(), requestLanguage(r), job)
	if err != nil {
		return 0, "", err
	}
	id, err := s.store.CreateJob(r.Context(), job)
	if err == nil {
		s.audit(r, "job.create", "#%d %s%s", id, job.Name, commandsAudit(JobOptions{}, job.Options))
	}
	return id, note, err
}

// updateBackupJob applies safe changes immediately, holding shorter retention
// for approval exactly as the UI does. A caller must report a pending result.
func (s *Server) updateBackupJob(r *http.Request, old, job Job) (string, error) {
	if old.GroupID != nil {
		return "", errGroupMember
	}
	if !canCommands(r) && (old.Options.PreCommand != job.Options.PreCommand || old.Options.PostCommand != job.Options.PostCommand || old.Options.CommandMinutes != job.Options.CommandMinutes) {
		return "", errCommandPermission
	}
	if err := validateRetention(job.Retention); err != nil {
		return "", err
	}
	wanted := job.Retention
	held := s.store.fourEyes(r.Context()) && shorterRetention(old.Retention, wanted)
	if held {
		job.Retention = old.Retention
	}
	note, err := s.m365Precheck(r.Context(), requestLanguage(r), job)
	if err != nil {
		return "", err
	}
	if err := s.store.UpdateJob(r.Context(), job); err != nil {
		return "", err
	}
	s.audit(r, "job.update", "#%d %s%s", job.ID, job.Name, commandsAudit(old.Options, job.Options))
	if held {
		if _, err := s.approvalForChange(r, "job.retention", job.ID, job.Name, wanted); err != nil {
			return note, err
		}
		return note, errApprovalPending
	}
	return note, nil
}

func (s *Server) deleteBackupJob(r *http.Request, id int64) error {
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		return err
	}
	if j.GroupID != nil {
		return errGroupMember
	}
	if held, err := s.approvalForChange(r, "job.delete", id, j.Name, nil); err != nil {
		return err
	} else if held {
		return errApprovalPending
	}
	if err := s.store.DeleteJob(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "job.delete", "job %d", id)
	return nil
}

func validateRetention(p repo.RetentionPolicy) error {
	for _, n := range []int{p.KeepLast, p.KeepDaily, p.KeepWeekly, p.KeepMonthly, p.KeepYearly} {
		if n < 0 || n > 100000 {
			return fmt.Errorf("retention counts must be between 0 and 100000")
		}
	}
	return nil
}

func (s *Server) queueFileRestore(r *http.Request, backupID, agentID int64, target string, paths []string, verify bool) (int64, error) {
	id, err := s.store.QueueRestore(r.Context(), backupID, agentID, target, paths, verify)
	if err == nil {
		s.audit(r, "restore.files", "run #%d from backup #%d to %q: %d paths", id, backupID, target, len(paths))
	}
	return id, err
}
