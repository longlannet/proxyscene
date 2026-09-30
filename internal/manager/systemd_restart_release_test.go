package manager

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
)

func TestTelegramReleaseUnsettledRestartDefersInnerArtifactRollback(t *testing.T) {
	for _, kind := range []string{"Hermes", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			h := newTelegramJournalTestHarness(t)
			target := h.target()
			key := canonicalTelegramTargetName(target)
			st := newStore()
			st.RuntimeConfig = h.app.cfg.runtimeConfig()
			artifact := h.systemPath
			if kind == "Hermes" {
				if _, err := h.app.applyTelegram(st, []systemdTargetName{target}); err != nil {
					t.Fatal(err)
				}
			} else {
				artifact = h.legacyPath
				st.TelegramTargets = []string{key}
				if err := os.WriteFile(artifact, []byte("[Service]\nEnvironmentFile=-/etc/openclaw-hermes-tg-proxy.env\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(h.envPath, []byte(telegramProxyEnvContent(h.app.cfg)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			beforeArtifact, err := os.ReadFile(artifact)
			if err != nil {
				t.Fatal(err)
			}
			beforeTargets := h.journal(t).Targets
			plan, err := h.app.planTelegramSelection(st, nil, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := captureRuntimeTelegram(h.app, h.app, st, cloneStore(st), plan)
			if err != nil {
				t.Fatal(err)
			}

			pending := false
			telegramReadServiceState = func(current systemdTargetName, _ *persistedUserIdentity) (telegramServiceState, error) {
				if current != target {
					t.Fatal("service check escaped the fixed target")
				}
				state := telegramServiceState{LoadState: "loaded", ActiveState: "active"}
				if pending {
					state.Job = "94 /org/freedesktop/systemd1/job/94"
				}
				return state, nil
			}
			restarts, reloads := 0, 0
			systemctlRun = func(_ string, args ...string) error {
				switch args[0] {
				case "daemon-reload":
					reloads++
					if pending {
						t.Fatal("reloaded manager before the restart settled")
					}
				case "try-restart":
					if args[len(args)-1] != target.Service {
						t.Fatal("restart escaped the fixed target")
					}
					restarts++
					if restarts == 1 {
						pending = true
						return fmt.Errorf("fixture client deadline: %w", errSystemdRestartUnsettled)
					}
				default:
					t.Fatalf("unexpected service operation: %v", args)
				}
				return nil
			}
			if err := h.app.restoreTelegramPlan(st, plan); !errors.Is(err, errSystemdRestartUnsettled) {
				t.Fatalf("release lost uncertain restart error: %v", err)
			}
			if _, err := os.Stat(artifact); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("release restored the artifact under an unfinished restart: %v", err)
			}
			if restarts != 1 || reloads != 1 {
				t.Fatalf("release retried service operation: restarts=%d reloads=%d", restarts, reloads)
			}
			if kind == "Hermes" {
				entry := h.journal(t).Targets[key]
				if entry == nil || entry.Phase != telegramPhaseRestoring {
					t.Fatal("release lost restoring ownership")
				}
			} else if !containsString(st.TelegramTargets, key) {
				t.Fatal("release lost legacy ownership")
			}
			pendingTargets := h.journal(t).Targets
			if err := compensateRuntimeTelegram(h.app, snapshot); !errors.Is(err, errSystemdRestartUnsettled) {
				t.Fatalf("recovery accepted pending restart: %v", err)
			}
			if _, err := os.Stat(artifact); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovery restored artifact while restart remained pending")
			}
			if restarts != 1 || reloads != 1 || !reflect.DeepEqual(h.journal(t).Targets, pendingTargets) {
				t.Fatal("pending recovery changed service or ownership")
			}

			pending = false
			if err := compensateRuntimeTelegram(h.app, snapshot); err != nil {
				t.Fatal(err)
			}
			afterArtifact, err := os.ReadFile(artifact)
			if err != nil || !bytes.Equal(beforeArtifact, afterArtifact) {
				t.Fatalf("settled recovery did not restore artifact: %v", err)
			}
			if restarts != 2 || reloads != 2 {
				t.Fatalf("settled recovery operations: restarts=%d reloads=%d", restarts, reloads)
			}
			if !reflect.DeepEqual(h.journal(t).Targets, beforeTargets) {
				t.Fatal("settled recovery did not restore ownership")
			}
		})
	}
}
