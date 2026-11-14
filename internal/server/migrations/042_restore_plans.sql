-- Unirea fork: a whole computer restored from the portal, without boot media.
-- The computer's own Windows prepares a one-time start into a recovery
-- environment; from there it joins with a single-use code, the console checks
-- the disk recorded beforehand and starts the restore by itself.

-- Image restores can name the disk that may be overwritten and always repair
-- the Windows start afterwards.
ALTER TABLE runs ADD COLUMN expect_disk JSONB;
ALTER TABLE runs ADD COLUMN repair_boot BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE runs ADD COLUMN restore_plan_id BIGINT;

CREATE TABLE restore_plans (
    id                BIGSERIAL PRIMARY KEY,
    backup_run_id     BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    image_index       INT NOT NULL DEFAULT 0,
    -- The computer that is restored (its current, fresh Windows).
    target_agent_id   BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    -- Its system disk, recorded while that Windows ran: the only disk the
    -- restore may overwrite.
    expect_disk       JSONB NOT NULL,
    token_id          BIGINT REFERENCES enrollment_tokens(id) ON DELETE SET NULL,
    -- The single-use code, only until the recovery environment has joined,
    -- with the console address and certificate it joins.
    code              TEXT NOT NULL DEFAULT '',
    server            TEXT NOT NULL DEFAULT '',
    fingerprint       TEXT NOT NULL DEFAULT '',
    recovery_agent_id BIGINT REFERENCES agents(id) ON DELETE SET NULL,
    prepare_run_id    BIGINT REFERENCES runs(id) ON DELETE SET NULL,
    restore_run_id    BIGINT REFERENCES runs(id) ON DELETE SET NULL,
    -- preparing, waiting, connected, restoring, rebooting, done, failed, cancelled
    state             TEXT NOT NULL DEFAULT 'preparing',
    -- Set once the first byte is written: before it, a failure leaves the
    -- computer's current Windows untouched.
    writing           BOOLEAN NOT NULL DEFAULT FALSE,
    message           TEXT NOT NULL DEFAULT '',
    created_by        TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX restore_plans_token ON restore_plans(token_id);
-- One open plan per computer.
CREATE UNIQUE INDEX restore_plans_open ON restore_plans(target_agent_id)
    WHERE state NOT IN ('done', 'failed', 'cancelled');
