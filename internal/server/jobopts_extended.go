package server

import (
	"errors"
	"fmt"
	"github.com/max-zit/backupzit/internal/api"
	"slices"
	"strings"
)

// Older agents ignore unknown JSON fields. Require explicit support to
// prevent silently incomplete disk backups or ignored retention settings.

func checkExtendedJobOptions(a Agent, j Job) error {
	if (j.Options.VSS != nil || len(j.Options.ImageDisks) > 0 || j.Retention.KeepYearly > 0) && !slices.Contains(a.Capabilities, api.CapabilityJobOptions) {
		return fmt.Errorf("VSS overrides, disk lists and yearly retention require an updated agent advertising job-options-v1; upgrade the fork agent and wait for its next poll")
	}
	windows := strings.HasPrefix(strings.ToLower(a.OS), "windows")
	if (len(j.Options.ImageDisks) > 0 || j.Options.VSS != nil) && !windows {
		return errors.New("disk images and VSS options require a Windows agent")
	}
	if len(j.Options.ImageDisks) > 0 {
		if j.Kind != JobImage {
			return errors.New("image_disks is only available for disk image jobs")
		}
		if len(j.Options.ImageDisks) > 128 {
			return errors.New("choose at most 128 disks")
		}
		seen := map[int]bool{}
		for _, d := range j.Options.ImageDisks {
			if d < 0 || seen[d] {
				return errors.New("image_disks must contain unique non-negative disk numbers")
			}
			seen[d] = true
			if err := checkImageSelection(a, d, nil); err != nil {
				return err
			}
		}
	}
	if j.Kind == JobImage && j.ImageDisk != nil && *j.ImageDisk < -1 {
		return errors.New("image_disk must be -1 (all disks) or a non-negative disk number")
	}
	return nil
}

func agentCapabilities(reported []string) []string {
	out := []string{}
	// Only capabilities the console knows are kept (Unirea fork: plus the
	// one-time start into a recovery environment for restore plans).
	for _, c := range []string{api.CapabilityJobOptions, CapabilityRecoveryPrepare} {
		if slices.Contains(reported, c) {
			out = append(out, c)
		}
	}
	return out
}
