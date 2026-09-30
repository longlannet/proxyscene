package manager

import (
	"errors"
	"testing"
)

func TestTelegramAbsentReleaseRejectsPendingJob(t *testing.T) {
	target := systemdTargetName{Service: "hermes-absent.service"}
	for _, load := range []string{"loaded", "not-found", "masked"} {
		for _, active := range []string{"inactive", "failed"} {
			t.Run(load+"/"+active, func(t *testing.T) {
				stubTelegramServiceState(t, telegramServiceState{LoadState: load, ActiveState: active, Job: "17"})
				if err := validateTelegramAbsentRelease(target, nil); !errors.Is(err, errSystemdRestartUnsettled) {
					t.Fatalf("release accepted a pending service job: %v", err)
				}
				stubTelegramServiceState(t, telegramServiceState{LoadState: load, ActiveState: active})
				if err := validateTelegramAbsentRelease(target, nil); err != nil {
					t.Fatalf("settled absent release rejected: %v", err)
				}
			})
		}
	}
}
