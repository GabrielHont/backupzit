package server

import (
	"context"
	"errors"
)

// CreateProvisioningAPIToken mints a token for the existing local admin,
// using the same token validation/hashing and audit chain as the UI.
// The caller must have local access to the console database credentials.
func (s *Store) CreateProvisioningAPIToken(ctx context.Context, name, role string, days int) (string, error) {
	owner, err := scanUser(s.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE username='admin'`))
	if err != nil {
		return "", err
	}
	if owner.Disabled || owner.Role != "admin" || owner.Source != "local" {
		return "", errors.New("provisioning requires an enabled local administrator named admin")
	}
	token, err := s.CreateAPIToken(ctx, owner, name, role, days)
	if err != nil {
		return "", err
	}
	if _, err := s.AppendAudit(ctx, owner.Username, "api_token.create", "CLI: "+name+", role "+role, "local CLI"); err != nil {
		// Never leave a usable token whose creation could not be audited.
		_, _ = s.db.Exec(context.WithoutCancel(ctx), `DELETE FROM api_tokens WHERE token_hash=$1`, hashToken(token))
		return "", err
	}
	return token, nil
}
