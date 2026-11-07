-- Legacy enrollment tokens remain reusable without a usage limit.
ALTER TABLE enrollment_tokens
    ADD COLUMN uses BIGINT NOT NULL DEFAULT 0 CHECK (uses >= 0),
    ADD COLUMN max_uses BIGINT CHECK (max_uses > 0),
    ADD COLUMN label TEXT NOT NULL DEFAULT '',
    ADD COLUMN revoked_at TIMESTAMPTZ;

ALTER TABLE agents ADD COLUMN enrollment_token_id BIGINT
    REFERENCES enrollment_tokens(id) ON DELETE SET NULL;

-- Enrollment and limit changes lock this singleton row in the same order.
CREATE TABLE console_limits (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    max_agents BIGINT CHECK (max_agents >= 0)
);
INSERT INTO console_limits(id) VALUES(TRUE);
