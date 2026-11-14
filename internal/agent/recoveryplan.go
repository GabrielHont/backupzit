package agent

import (
	"context"
	"fmt"

	"github.com/max-zit/backupzit/internal/api"
)

// Unirea fork: restoring a whole computer from the portal, without boot media.
// On the computer's own Windows the agent prepares a one-time start into a
// copy of the computer's recovery environment (Windows RE) holding itself in
// recovery mode and the plan's single-use code; the restart follows the
// reported result. Nothing on the disk is written here.

// CapabilityRecoveryPrepare must match the console's constant.
const CapabilityRecoveryPrepare = "recovery-prepare-v1"

func agentCapabilities() []string {
	c := []string{api.CapabilityJobOptions}
	if recoveryPrepareSupported() {
		c = append(c, CapabilityRecoveryPrepare)
	}
	return c
}

func (a *Agent) recoveryPrepare(ctx context.Context, run api.Run) (api.RunResult, func()) {
	if run.RecoveryPlan == nil {
		return failed(fmt.Errorf("the restore plan is missing")), nil
	}
	steps, err := prepareRecoveryStart(ctx, *run.RecoveryPlan, func(s string) { a.log.Info("recovery prepare", "run", run.ID, "step", s) })
	if err != nil {
		// Whatever was set up is removed again: the computer keeps starting
		// its own Windows.
		cleanupRecoveryStart(func(s string) { a.log.Info("recovery cleanup", "step", s) })
		return failed(err), nil
	}
	res := api.RunResult{Status: api.StatusSuccess, Message: "The computer restarts in 60 seconds into the recovery environment to restore the copy"}
	res.Errors = steps
	return res, func() { restartForRecovery(func(s string) { a.log.Info("recovery restart", "step", s) }) }
}

// rebootIntoWindows leaves the recovery environment (or restarts the
// computer) once a restore plan step is reported.
func (a *Agent) rebootIntoWindows() {
	rebootNow(func(s string) { a.log.Info("reboot", "step", s) })
}
