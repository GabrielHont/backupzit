//go:build !windows

package agent

import (
	"context"
	"errors"

	"github.com/max-zit/backupzit/internal/api"
)

func recoveryPrepareSupported() bool { return false }

func prepareRecoveryStart(context.Context, api.RecoveryPlan, func(string)) ([]string, error) {
	return nil, errors.New("restoring a whole computer this way needs Windows")
}

func cleanupRecoveryStart(func(string)) {}

func restartForRecovery(func(string)) {}

func rebootNow(func(string)) {}

// CleanupRecoveryStart removes a leftover preparation (nothing on other systems).
func CleanupRecoveryStart(func(string)) {}
