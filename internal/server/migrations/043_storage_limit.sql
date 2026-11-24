-- Unirea: a firm's hard limit on backup space. The portal measures the
-- firm's storage (every object in its bucket) and sends the limit with the
-- measurement; while the space is full, the console hands out no new backups.
ALTER TABLE console_limits
    ADD COLUMN storage_limit_bytes BIGINT CHECK (storage_limit_bytes > 0),
    ADD COLUMN storage_used_bytes BIGINT CHECK (storage_used_bytes >= 0),
    ADD COLUMN storage_measured_at TIMESTAMPTZ;
