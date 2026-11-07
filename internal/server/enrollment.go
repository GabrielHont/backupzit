package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnrollmentToken contains metadata only. Its secret is never stored.
type EnrollmentToken struct {
	ID        int64      `json:"id"`
	ExpiresAt time.Time  `json:"expiresAt"`
	MaxUses   *int64     `json:"maxUses"`
	Uses      int64      `json:"uses"`
	Label     string     `json:"label"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt"`
}

type ConsoleLimits struct {
	MaxAgents *int64 `json:"maxAgents"`
}

func (s *Store) createEnrollmentToken(ctx context.Context, ttl time.Duration, maxUses *int64, label string) (EnrollmentToken, string, error) {
	label = strings.TrimSpace(label)
	if ttl <= 0 || ttl > 365*24*time.Hour {
		return EnrollmentToken{}, "", errors.New("ttlHours must be between 1 and 8760")
	}
	if maxUses != nil && *maxUses < 1 {
		return EnrollmentToken{}, "", errors.New("maxUses must be positive or null (unlimited)")
	}
	if len(label) > 255 || strings.ContainsRune(label, 0) {
		return EnrollmentToken{}, "", errors.New("label must be at most 255 bytes and contain no NUL")
	}
	t := EnrollmentToken{ExpiresAt: time.Now().Add(ttl), MaxUses: maxUses, Label: label}
	secret := randomToken(24)
	err := s.db.QueryRow(ctx, `INSERT INTO enrollment_tokens(token_hash,expires_at,max_uses,label)
		VALUES($1,$2,$3,$4) RETURNING id,created_at`, hashToken(secret), t.ExpiresAt, maxUses, label).Scan(&t.ID, &t.CreatedAt)
	return t, secret, err
}

func (s *Store) listEnrollmentTokens(ctx context.Context) ([]EnrollmentToken, error) {
	rows, err := s.db.Query(ctx, `SELECT id,expires_at,max_uses,uses,label,created_at,revoked_at FROM enrollment_tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnrollmentToken, error) {
		var t EnrollmentToken
		err := r.Scan(&t.ID, &t.ExpiresAt, &t.MaxUses, &t.Uses, &t.Label, &t.CreatedAt, &t.RevokedAt)
		return t, err
	})
}

// Revocation keeps the token ID for the agents' enrollment provenance.
func (s *Store) revokeEnrollmentToken(ctx context.Context, id int64) error {
	ct, err := s.db.Exec(ctx, `UPDATE enrollment_tokens SET revoked_at=clock_timestamp() WHERE id=$1 AND revoked_at IS NULL`, id)
	if err == nil && ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) consoleLimits(ctx context.Context) (ConsoleLimits, error) {
	var l ConsoleLimits
	err := s.db.QueryRow(ctx, `SELECT max_agents FROM console_limits WHERE id`).Scan(&l.MaxAgents)
	return l, err
}

func (s *Store) setConsoleLimits(ctx context.Context, l ConsoleLimits) error {
	if l.MaxAgents != nil && *l.MaxAgents < 0 {
		return errors.New("maxAgents must be non-negative or null (unlimited)")
	}
	_, err := s.db.Exec(ctx, `UPDATE console_limits SET max_agents=$1 WHERE id`, l.MaxAgents)
	return err
}
