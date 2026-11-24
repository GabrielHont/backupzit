package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/max-zit/backupzit/internal/api"
)

// Unirea fork: a firm's hard limit on backup space.
//
// The portal measures the firm's storage (every object, every retained
// version, in the firm's own bucket) and sends the limit together with the
// measurement (PUT /limits {"storage": {...}}). While the measured space is
// at or above the limit, the console hands out no run that writes backups
// (backups of every kind and copies): each one ends at once as failed with
// storageFullMessage, which the portal shows in Romanian. Restores,
// browsing, checks and agent updates go on.
//
// This pauses new backups; it is not a byte-exact ceiling: agents upload
// straight to the storage, so backups already running finish and may pass
// the limit. A full state lasts until a newer measurement says otherwise.
// The console never deletes backups to make room (its own pruning could race
// an agent holding the repository's index, or a VM running from a backup, and
// retention keeps the newest periods that hold backups, so with backups
// paused nothing would expire). The way out is a larger plan or limit, or
// support removing backups.

// StorageLimit is the firm's backup space as the portal last measured it.
type StorageLimit struct {
	// LimitBytes is the firm's limit; null is unlimited.
	LimitBytes *int64 `json:"limitBytes"`
	// UsedBytes is what the portal measured on the storage.
	UsedBytes int64 `json:"usedBytes"`
	// MeasuredAt is when the portal measured it.
	MeasuredAt time.Time `json:"measuredAt"`
	// Full reports whether new backups are refused now (read only).
	Full bool `json:"full"`
}

// storageFullPrefix starts the message of a backup refused for space; the
// portal recognises it.
const storageFullPrefix = "backup storage is full"

func storageFullMessage(l StorageLimit) string {
	return fmt.Sprintf("%s: %s of %s used. New backups are paused until there is space again: the firm's plan is raised, or old backups or folders are removed.",
		storageFullPrefix, decimalGB(l.UsedBytes), decimalGB(*l.LimitBytes))
}

// decimalGB writes bytes as plans are sold: 250 GB is 250 000 000 000 bytes.
func decimalGB(b int64) string {
	return fmt.Sprintf("%.1f GB", float64(b)/1e9)
}

// full applies the rule: a limit and a measurement at or above it.
func (l StorageLimit) full() bool {
	return l.LimitBytes != nil && l.UsedBytes >= *l.LimitBytes
}

func (l StorageLimit) valid() error {
	if l.LimitBytes != nil && *l.LimitBytes <= 0 {
		return errors.New("storage.limitBytes must be positive or null (unlimited)")
	}
	if l.UsedBytes < 0 {
		return errors.New("storage.usedBytes must be non-negative")
	}
	if l.MeasuredAt.IsZero() {
		return errors.New("storage.measuredAt is required")
	}
	if l.MeasuredAt.After(time.Now().Add(10 * time.Minute)) {
		return errors.New("storage.measuredAt is in the future")
	}
	return nil
}

// storageLimit reads the firm's backup space; nil when the portal never sent it.
func (s *Store) storageLimit(ctx context.Context) (*StorageLimit, error) {
	var l StorageLimit
	var used *int64
	var at *time.Time
	err := s.db.QueryRow(ctx, `SELECT storage_limit_bytes, storage_used_bytes, storage_measured_at FROM console_limits WHERE id`).
		Scan(&l.LimitBytes, &used, &at)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (used == nil || at == nil)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l.UsedBytes, l.MeasuredAt = *used, *at
	l.Full = l.full()
	return &l, nil
}

// setStorageLimit keeps the newest measurement: one sent late (an older
// measuredAt) does not replace a newer one.
func (s *Store) setStorageLimit(ctx context.Context, l StorageLimit) error {
	if err := l.valid(); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO console_limits(id, storage_limit_bytes, storage_used_bytes, storage_measured_at)
		VALUES(TRUE, $1, $2, $3)
		ON CONFLICT(id) DO UPDATE SET storage_limit_bytes=excluded.storage_limit_bytes,
			storage_used_bytes=excluded.storage_used_bytes, storage_measured_at=excluded.storage_measured_at
		WHERE console_limits.storage_measured_at IS NULL OR console_limits.storage_measured_at <= excluded.storage_measured_at`,
		l.LimitBytes, l.UsedBytes, l.MeasuredAt)
	return err
}

// writesBackups reports whether a run adds data to backup storage.
func writesBackups(kind string) bool { return isBackupKind(kind) || kind == api.KindCopy }

// backupRefused says why a run must not go to its agent now, or "".
func (s *Server) backupRefused(ctx context.Context, run *Run) string {
	if !writesBackups(run.Kind) {
		return ""
	}
	l, err := s.store.storageLimit(ctx)
	if err != nil {
		s.log.Error("read storage limit", "err", err)
		return ""
	}
	if l == nil || !l.Full {
		return ""
	}
	return storageFullMessage(*l)
}

// requeueRun puts a claimed run that was not handed out back in the queue.
func (s *Store) requeueRun(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `UPDATE runs SET status='queued', started_at=NULL WHERE id=$1 AND status='running'`, id)
	return err
}
