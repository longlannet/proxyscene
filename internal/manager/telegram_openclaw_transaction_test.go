package manager

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestOpenClawReloadEvidenceRequiresPendingOwnership(t *testing.T) {
	h, target, plan, changed := setupOpenClawReloadFixture(t, `{"channels":{"telegram":{"botToken":"test"}}}`)
	*changed = true
	// The observer only checks the supplied expectation against the gateway.
	// Positive gateway evidence alone must not satisfy a transaction lacking
	// durable ownership, even though its config bytes and identity agree.
	if !observeOpenClawTelegramReload(target, plan, h.configPath, h.config(t)) {
		t.Fatal("matching gateway evidence rejected")
	}
	openClawReloadRPC = func(context.Context, systemdTargetName, *openClawTelegramReloadPlan, string, any) error {
		t.Fatal("gateway queried before pending ownership was verified")
		return nil
	}
	if handled, err := h.app.finishOpenClawTelegramReload(target, plan); handled || err == nil {
		t.Fatalf("unowned evidence accepted: handled=%v error=%v", handled, err)
	}
}

func TestOpenClawReloadTransactionRechecksAfterObservation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage string
		edit  string
	}{
		{"rpc failure with config edit", "rpc failure", "config"},
		{"rpc failure with released ownership", "rpc failure", "ownership"},
		{"ready channel with identity drift", "channel ready", "identity"},
		{"final state with config edit", "final state", "config"},
		{"final state with runtime drift", "final state", "runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, target, plan, changed := setupOpenClawReloadFixture(t, `{"channels":{"telegram":{"botToken":"test"}}}`)
			if _, _, err := h.app.applyOpenClawTelegramProxy(target, "http://127.0.0.1:7890"); err != nil {
				t.Fatal(err)
			}
			*changed = true
			edited := false
			edit := func() {
				if edited {
					return
				}
				edited = true
				switch tc.edit {
				case "config":
					if err := os.WriteFile(h.configPath, append(h.config(t), '\n'), 0o600); err != nil {
						t.Fatal(err)
					}
				case "ownership":
					journal := h.journal(t)
					delete(journal.Users, h.user)
					if err := h.app.saveOpenClawProxyJournal(journal); err != nil {
						t.Fatal(err)
					}
				case "identity":
					h.identity.UID++
				case "runtime":
					openClawValidateTargetRuntime = func(systemdTargetName, *persistedUserIdentity) error {
						return errors.New("runtime binding changed")
					}
				}
			}
			baseRPC, baseState := openClawReloadRPC, openClawReloadUnitState
			openClawReloadRPC = func(ctx context.Context, target systemdTargetName, plan *openClawTelegramReloadPlan, method string, result any) error {
				if tc.stage == "rpc failure" {
					edit()
					return errors.New("RPC unavailable")
				}
				err := baseRPC(ctx, target, plan, method, result)
				if tc.stage == "channel ready" && method == "channels.status" {
					edit()
				}
				return err
			}
			stateCalls := 0
			openClawReloadUnitState = func(target systemdTargetName, identity *persistedUserIdentity) (string, error) {
				stateCalls++
				if tc.stage == "final state" && stateCalls == 2 {
					edit()
				}
				return baseState(target, identity)
			}
			if handled, err := h.app.finishOpenClawTelegramReload(target, plan); handled || err == nil {
				t.Fatalf("observation conflict accepted or sent to restart fallback: handled=%v error=%v", handled, err)
			}
			if !edited {
				t.Fatal("concurrent edit was not exercised")
			}
			if tc.edit != "ownership" && h.journal(t).Users[h.user].Phase != openClawPhasePrepared {
				t.Fatal("observation committed a conflicted transaction")
			}
		})
	}
}
