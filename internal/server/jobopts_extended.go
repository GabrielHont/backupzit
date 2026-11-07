package server

import (
	"errors"
	"fmt"
	"strings"
)

// These wire fields require the corresponding fork agent. Older agents
// ignore unknown JSON fields, which could otherwise produce partial backups.
const minExtendedOptionsVersion = "0.47.4"

func checkExtendedJobOptions(a Agent, j Job) error {
	if (j.Options.VSS != nil || len(j.Options.ImageDisks) > 0 || j.Retention.KeepYearly > 0) && agentOlder(a, minExtendedOptionsVersion) {
		return fmt.Errorf("VSS overrides, disk lists and yearly retention require an updated fork agent (%s or newer)", minExtendedOptionsVersion)
	}
	windows := strings.HasPrefix(strings.ToLower(a.OS), "windows")
	if (j.Kind == JobImage || j.Options.VSS != nil) && !windows {
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
