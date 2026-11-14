package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/imaging"
)

// Unirea fork: restoring a whole computer from the portal, without boot media.
//
//	preparing  the computer's Windows sets up a one-time start into a recovery environment
//	waiting    it restarts; the recovery environment should join with the plan's code
//	connected  the recovery environment joined; the console looks for the recorded disk
//	restoring  the disk is being written
//	rebooting  written; the restored Windows should start and its agent check in
//	done       the restored computer's agent checked in
//	failed     with message; "writing" tells whether the disk was touched
//	cancelled  before anything was written
//
// Before "restoring" nothing on the computer has been written: every failure
// sends the recovery environment back into the computer's current Windows.

// CapabilityRecoveryPrepare: the agent can set up the one-time start.
const CapabilityRecoveryPrepare = "recovery-prepare-v1"

// recoveryWait is how long the computer may stay away after preparing; if its
// own Windows checks in later while the plan still waits, the recovery
// environment did not join (no network there) and nothing was written.
const recoveryWait = 12 * time.Minute

type RestorePlan struct {
	ID              int64            `json:"id"`
	BackupRunID     int64            `json:"backup_run_id"`
	Image           int              `json:"image"`
	TargetAgentID   int64            `json:"agent_id"`
	State           string           `json:"state"`
	Writing         bool             `json:"writing"`
	Message         string           `json:"message,omitempty"`
	RecoveryAgentID *int64           `json:"recovery_agent_id,omitempty"`
	PrepareRunID    *int64           `json:"prepare_run_id,omitempty"`
	RestoreRunID    *int64           `json:"restore_run_id,omitempty"`
	Disk            api.DiskIdentity `json:"disk"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

const planCols = `id, backup_run_id, image_index, target_agent_id, state, writing, message, recovery_agent_id, prepare_run_id, restore_run_id, expect_disk, created_at, updated_at`

func scanPlan(row pgx.Row) (RestorePlan, error) {
	var p RestorePlan
	var disk []byte
	err := row.Scan(&p.ID, &p.BackupRunID, &p.Image, &p.TargetAgentID, &p.State, &p.Writing, &p.Message, &p.RecoveryAgentID, &p.PrepareRunID, &p.RestoreRunID, &disk, &p.CreatedAt, &p.UpdatedAt)
	if err == nil {
		_ = json.Unmarshal(disk, &p.Disk)
	}
	return p, err
}

// reconcilePlans ends plans whose step can no longer finish by itself: a
// recovery environment that left without an inventory (nothing written), or a
// restore run the console marked failed because its agent vanished.
func (s *Store) reconcilePlans(ctx context.Context) {
	rows, err := s.db.Query(ctx, `SELECT id FROM restore_plans WHERE state='connected' AND updated_at < now() - interval '40 minutes'`)
	if err == nil {
		ids, _ := pgx.CollectRows(rows, pgx.RowTo[int64])
		for _, id := range ids {
			s.failPlan(ctx, id, "The recovery environment stopped answering before the restore started; nothing was written.", false)
		}
	}
	rows, err = s.db.Query(ctx, `SELECT p.id, r.message FROM restore_plans p JOIN runs r ON r.id=p.restore_run_id
		WHERE p.state='restoring' AND r.status IN ('failed','cancelled')`)
	if err == nil {
		type stuck struct {
			ID  int64
			Msg string
		}
		list, _ := pgx.CollectRows(rows, pgx.RowToStructByPos[stuck])
		for _, x := range list {
			s.failPlan(ctx, x.ID, "The restore stopped and the computer no longer answers: "+x.Msg, true)
		}
	}
	rows, err = s.db.Query(ctx, `SELECT p.id FROM restore_plans p JOIN runs r ON r.id=p.prepare_run_id
		WHERE p.state='preparing' AND r.status IN ('failed','cancelled')`)
	if err == nil {
		ids, _ := pgx.CollectRows(rows, pgx.RowTo[int64])
		for _, id := range ids {
			s.failPlan(ctx, id, "The computer could not prepare the restart. Nothing was changed.", false)
		}
	}
}

// ListRestorePlans returns the newest plans first.
func (s *Store) ListRestorePlans(ctx context.Context, limit int) ([]RestorePlan, error) {
	s.reconcilePlans(ctx)
	rows, err := s.db.Query(ctx, `SELECT `+planCols+` FROM restore_plans ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RestorePlan, error) { return scanPlan(r) })
}

func (s *Store) GetRestorePlan(ctx context.Context, id int64) (RestorePlan, error) {
	s.reconcilePlans(ctx)
	p, err := scanPlan(s.db.QueryRow(ctx, `SELECT `+planCols+` FROM restore_plans WHERE id=$1`, id))
	return p, notFound(err)
}

type RestorePlanOptions struct {
	BackupRunID   int64
	Image         int
	TargetAgentID int64
	// The console address and certificate the recovery environment joins.
	Server, Fingerprint string
	CreatedBy           string
}

func diskIdentity(d imaging.Disk) api.DiskIdentity {
	return api.DiskIdentity{Style: d.Style, GPTDiskID: d.GPTDiskID, MBRSignature: d.MBRSignature, Size: d.Size, Model: d.Model}
}

func identityMatches(id api.DiskIdentity, d imaging.Disk) bool {
	return imaging.DiskIdentity{Style: id.Style, GPTDiskID: id.GPTDiskID, MBRSignature: id.MBRSignature, Size: id.Size, Model: id.Model}.Matches(d)
}

// CreateRestorePlan checks that the computer can take the image, records its
// system disk, issues a single-use code and asks its agent to prepare.
func (s *Store) CreateRestorePlan(ctx context.Context, o RestorePlanOptions) (RestorePlan, error) {
	b, err := s.GetRun(ctx, o.BackupRunID)
	if err != nil {
		return RestorePlan{}, err
	}
	if b.Kind != api.KindImageBackup || b.SnapshotID == "" || (b.Status != api.StatusSuccess && b.Status != api.StatusWarning) || b.Expired {
		return RestorePlan{}, errors.New("choose a successful, kept disk image")
	}
	images := imageDetails(b)
	if o.Image < 0 || o.Image >= len(images) {
		return RestorePlan{}, errors.New("image must name a disk of the backup")
	}
	img := images[o.Image]
	a, err := s.GetAgent(ctx, o.TargetAgentID)
	if err != nil {
		return RestorePlan{}, errors.New("unknown computer")
	}
	if a.Recovery || !strings.Contains(strings.ToLower(a.OS), "windows") {
		return RestorePlan{}, errors.New("the computer must run Windows with the backup program installed")
	}
	if !a.Online() {
		return RestorePlan{}, errors.New("the computer is not connected now")
	}
	capable := false
	for _, c := range a.Capabilities {
		capable = capable || c == CapabilityRecoveryPrepare
	}
	if !capable {
		return RestorePlan{}, errors.New("the backup program on the computer is too old for this restore; update it first")
	}
	var system *imaging.Disk
	for _, d := range a.Disks() {
		if d.System {
			d := d
			system = &d
		}
	}
	if system == nil {
		return RestorePlan{}, errors.New("the computer has not reported its disk with Windows yet; try again in a few minutes")
	}
	var need uint64
	for _, p := range img.Partitions {
		need = max(need, p.Offset+p.Length)
	}
	need += 1 << 20
	if system.Size < need {
		return RestorePlan{}, fmt.Errorf("the computer's disk holds %d bytes; the copy needs at least %d", system.Size, need)
	}
	if system.SectorSize != img.SectorSize {
		return RestorePlan{}, fmt.Errorf("the computer's disk has %d byte sectors, the copy %d", system.SectorSize, img.SectorSize)
	}
	// The copy must hold a Windows that can start: its boot partition and an
	// NTFS volume backed up, on the same kind of disk (GPT starts through
	// UEFI, MBR through BIOS) as the computer's.
	if !strings.EqualFold(img.Style, system.Style) {
		return RestorePlan{}, fmt.Errorf("the copy is a %s disk and the computer's disk is %s; it would not start", img.Style, system.Style)
	}
	var bootPart, windowsPart bool
	for _, p := range img.Partitions {
		if !p.Included {
			continue
		}
		if strings.EqualFold(p.GPTType, imaging.GPTTypeEFI) || p.Bootable {
			bootPart = true
		}
		if p.FileSystem == "NTFS" && (strings.EqualFold(p.GPTType, imaging.GPTTypeBasicData) || p.MBRType == 0x07) {
			windowsPart = true
		}
	}
	if !bootPart || !windowsPart {
		return RestorePlan{}, errors.New("this copy does not hold a Windows that can start (its boot partition or Windows partition was not backed up)")
	}
	// The old computer must be off: two running copies of it would clash.
	if b.AgentID != a.ID {
		if src, err := s.GetAgent(ctx, b.AgentID); err == nil && src.Online() {
			return RestorePlan{}, errors.New("the old computer is still connected; turn it off before restoring it elsewhere")
		}
	}
	disk := diskIdentity(*system)
	diskJSON, _ := json.Marshal(disk)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return RestorePlan{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	secret := randomToken(24)
	var tokenID int64
	if err := tx.QueryRow(ctx, `INSERT INTO enrollment_tokens(token_hash,expires_at,max_uses,label) VALUES($1,$2,1,$3) RETURNING id`,
		hashToken(secret), time.Now().Add(3*time.Hour), "Restore of a whole computer").Scan(&tokenID); err != nil {
		return RestorePlan{}, err
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO restore_plans(backup_run_id, image_index, target_agent_id, expect_disk, token_id, code, server, fingerprint, created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		b.ID, o.Image, a.ID, diskJSON, tokenID, secret, o.Server, o.Fingerprint, o.CreatedBy).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "restore_plans_open") {
			return RestorePlan{}, errors.New("a restore is already in progress on this computer")
		}
		return RestorePlan{}, err
	}
	var runID int64
	if err := tx.QueryRow(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, restore_plan_id) VALUES($1,$2,'manual','',$3) RETURNING id`,
		a.ID, api.KindRecoveryPrepare, id).Scan(&runID); err != nil {
		return RestorePlan{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE restore_plans SET prepare_run_id=$2 WHERE id=$1`, id, runID); err != nil {
		return RestorePlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RestorePlan{}, err
	}
	return s.GetRestorePlan(ctx, id)
}

// recoveryPlanFor is what the prepare run of a plan carries to the agent.
func (s *Store) recoveryPlanFor(ctx context.Context, planID int64) (*api.RecoveryPlan, error) {
	var p api.RecoveryPlan
	var disk []byte
	err := s.db.QueryRow(ctx, `SELECT id, server, code, fingerprint, expect_disk FROM restore_plans WHERE id=$1 AND state='preparing' AND code<>''`, planID).
		Scan(&p.PlanID, &p.Server, &p.Token, &p.Fingerprint, &disk)
	if err != nil {
		return nil, errors.New("the restore plan is no longer waiting for this computer")
	}
	_ = json.Unmarshal(disk, &p.Disk)
	return &p, nil
}

// failPlan ends a plan; before any write it also sends a joined recovery
// environment back into the computer's Windows.
func (s *Store) failPlan(ctx context.Context, id int64, msg string, writing bool) {
	var rec *int64
	err := s.db.QueryRow(ctx, `UPDATE restore_plans SET state='failed', message=$2, writing=writing OR $3, code='', updated_at=now()
		WHERE id=$1 AND state NOT IN ('done','failed','cancelled') RETURNING recovery_agent_id`, id, msg, writing).Scan(&rec)
	if err != nil {
		return
	}
	_, _ = s.db.Exec(ctx, `UPDATE enrollment_tokens SET revoked_at=now() WHERE id=(SELECT token_id FROM restore_plans WHERE id=$1) AND revoked_at IS NULL`, id)
	if rec != nil && !writing {
		s.queuePlanReboot(ctx, id, *rec)
	}
	// A failure while writing keeps the recovery agent for the team; else it
	// leaves once the way back has been handed over.
	if rec != nil && !writing {
		s.retireRecoveryAgentLater(ctx, *rec)
	}
}

// retireRecoveryAgentLater removes a recovery environment's agent once its
// last run has been taken (its access to the storage ends with the plan).
func (s *Store) retireRecoveryAgentLater(ctx context.Context, agentID int64) {
	_, _ = s.db.Exec(ctx, `UPDATE agents SET removed_at=now() WHERE id=$1 AND recovery
		AND NOT EXISTS(SELECT 1 FROM runs WHERE agent_id=$1 AND status IN ('queued','running'))`, agentID)
}

func (s *Store) queuePlanReboot(ctx context.Context, planID, agentID int64) {
	if _, err := s.db.Exec(ctx, `INSERT INTO runs(agent_id, kind, trigger, repo_url, restore_plan_id) VALUES($1,$2,'manual','',$3)`,
		agentID, api.KindRecoveryReboot, planID); err != nil {
		slog.Error("restore plan: queue reboot", "plan", planID, "err", err)
	}
}

// CancelRestorePlan stops a plan that has not written anything yet.
func (s *Store) CancelRestorePlan(ctx context.Context, id int64) error {
	var rec *int64
	err := s.db.QueryRow(ctx, `UPDATE restore_plans SET state='cancelled', code='', updated_at=now()
		WHERE id=$1 AND state IN ('preparing','waiting','connected') RETURNING recovery_agent_id`, id).Scan(&rec)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("the restore has already started writing or has ended; it cannot be cancelled")
	} else if err != nil {
		return err
	}
	_, _ = s.db.Exec(ctx, `UPDATE enrollment_tokens SET revoked_at=now() WHERE id=(SELECT token_id FROM restore_plans WHERE id=$1) AND revoked_at IS NULL`, id)
	_, _ = s.db.Exec(ctx, `UPDATE runs SET status='failed', finished_at=now(), message='Restore cancelled' WHERE restore_plan_id=$1 AND status='queued'`, id)
	if rec != nil {
		s.queuePlanReboot(ctx, id, *rec)
	}
	return nil
}

// planOnPoll moves plans along when an agent checks in.
func (s *Store) planOnPoll(ctx context.Context, agentID int64) {
	a, err := s.GetAgent(ctx, agentID)
	if err != nil {
		return
	}
	if a.Recovery {
		var p RestorePlan
		p, err = scanPlan(s.db.QueryRow(ctx, `SELECT `+planCols+` FROM restore_plans WHERE recovery_agent_id=$1 AND state='connected' AND restore_run_id IS NULL`, agentID))
		if err != nil {
			return
		}
		disks := a.Disks()
		if len(disks) == 0 {
			return // the inventory comes with a later poll
		}
		var found []imaging.Disk
		for _, d := range disks {
			if identityMatches(p.Disk, d) {
				found = append(found, d)
			}
		}
		if len(found) != 1 {
			s.failPlan(ctx, p.ID, fmt.Sprintf("The computer's disk was not found with certainty in the recovery environment (%d matches); nothing was written.", len(found)), false)
			return
		}
		b, err := s.GetRun(ctx, p.BackupRunID)
		if err != nil {
			s.failPlan(ctx, p.ID, "The copy is no longer available; nothing was written.", false)
			return
		}
		// Taken atomically: a cancel that comes first wins, and one that comes
		// after finds the plan restoring and refuses.
		tag, err := s.db.Exec(ctx, `UPDATE restore_plans SET state='restoring', updated_at=now() WHERE id=$1 AND state='connected'`, p.ID)
		if err != nil || tag.RowsAffected() != 1 {
			return
		}
		planID := p.ID
		rid, err := s.QueueImageRestore(ctx, p.BackupRunID, agentID, ImageRestoreOptions{
			Image: p.Image, TargetDisk: found[0].Number, Expect: &p.Disk, RepairBoot: true,
			// Onto another computer than the one the image came from.
			NewHardware: p.TargetAgentID != b.AgentID, PlanID: &planID,
		})
		if err != nil {
			s.failPlan(ctx, p.ID, "The restore could not start: "+err.Error()+"; nothing was written.", false)
			return
		}
		_ = tag
		_, _ = s.db.Exec(ctx, `UPDATE restore_plans SET restore_run_id=$2, updated_at=now() WHERE id=$1`, p.ID, rid)
		return
	}
	// The restored computer's own agent: the image's agent checks in again.
	_, _ = s.db.Exec(ctx, `UPDATE agents SET removed_at=now() WHERE recovery AND removed_at IS NULL AND id IN (
		SELECT p.recovery_agent_id FROM restore_plans p JOIN runs b ON b.id=p.backup_run_id
		WHERE p.state='rebooting' AND b.agent_id=$1)`, agentID)
	_, _ = s.db.Exec(ctx, `UPDATE restore_plans p SET state='done', message='', updated_at=now() FROM runs b
		WHERE p.state='rebooting' AND b.id=p.backup_run_id AND b.agent_id=$1 AND p.updated_at < now()`, agentID)
	// The computer came back into its current Windows while the plan still
	// waited for the recovery environment: it never joined.
	var waiting int64
	if s.db.QueryRow(ctx, `SELECT id FROM restore_plans WHERE target_agent_id=$1 AND state='waiting' AND updated_at < now() - $2::interval`,
		agentID, fmt.Sprintf("%d seconds", int(recoveryWait.Seconds()))).Scan(&waiting) == nil {
		s.failPlan(ctx, waiting, "The computer could not connect from the recovery environment, so nothing was changed. Connect it with a network cable and try again.", false)
	}
}

// planOnFinish moves a plan along when one of its runs ends.
func (s *Store) planOnFinish(ctx context.Context, runID int64, res api.RunResult) {
	var planID *int64
	var kind string
	if s.db.QueryRow(ctx, `SELECT restore_plan_id, kind FROM runs WHERE id=$1`, runID).Scan(&planID, &kind) != nil || planID == nil {
		return
	}
	ok := res.Status == api.StatusSuccess || res.Status == api.StatusWarning
	switch kind {
	case api.KindRecoveryPrepare:
		if ok {
			_, _ = s.db.Exec(ctx, `UPDATE restore_plans SET state='waiting', updated_at=now() WHERE id=$1 AND state='preparing'`, *planID)
		} else {
			s.failPlan(ctx, *planID, "The computer could not prepare the restart: "+res.Message+". Nothing was changed.", false)
		}
	case api.KindImageRestore:
		// The agent says whether the disk was touched and whether the restored
		// Windows can start; older agents only say it in the message.
		var out struct {
			DiskWritten *bool `json:"disk_written"`
			BootReady   *bool `json:"boot_ready"`
		}
		_ = json.Unmarshal(res.Details, &out)
		written := out.DiskWritten == nil || *out.DiskWritten
		if out.DiskWritten == nil && strings.Contains(res.Message, "nothing was written") {
			written = false
		}
		switch {
		case ok && (out.BootReady == nil || *out.BootReady):
			_, _ = s.db.Exec(ctx, `UPDATE restore_plans SET state='rebooting', writing=true, updated_at=now() WHERE id=$1 AND state='restoring'`, *planID)
		case ok:
			s.failPlan(ctx, *planID, "The disk was restored but the Windows start could not be repaired: "+res.Message, true)
		case !written:
			s.failPlan(ctx, *planID, "The restore stopped before writing, nothing was written: "+res.Message, false)
		default:
			s.failPlan(ctx, *planID, "The restore stopped while writing the disk: "+res.Message, true)
		}
	case api.KindRecoveryReboot:
		var rec int64
		if s.db.QueryRow(ctx, `SELECT agent_id FROM runs WHERE id=$1`, runID).Scan(&rec) == nil {
			s.retireRecoveryAgentLater(ctx, rec)
		}
	}
}
