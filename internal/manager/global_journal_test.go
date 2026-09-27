package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func withGlobalProxyTestPaths(t *testing.T, a *App) (string, string) {
	t.Helper()
	root := t.TempDir()
	profile := filepath.Join(root, "profile.d", "proxyscene-global-proxy.sh")
	apt := filepath.Join(root, "apt.conf.d", "99proxyscene-global-proxy")
	oldProfile := globalProxyProfilePath
	oldAPT := globalProxyAPTPath
	oldRead := globalProxyReadArtifact
	oldWrite := globalProxyWriteArtifact
	oldRemove := globalProxyRemoveArtifact
	oldSync := globalProxySyncArtifactDir
	oldJournalWrite := globalProxyWriteJournal
	oldAfterQuarantine := globalProxyCASAfterQuarantine
	globalProxyProfilePath = func() string { return profile }
	globalProxyAPTPath = func() string { return apt }
	globalProxyReadArtifact = readGlobalProxyArtifactStateNoFollow
	globalProxyWriteArtifact = writeGlobalProxyArtifactCAS
	globalProxyRemoveArtifact = removeGlobalProxyArtifactCAS
	globalProxySyncArtifactDir = func(string) error { return nil }
	globalProxyWriteJournal = writeFileAtomic
	globalProxyCASAfterQuarantine = func(string) {}
	t.Cleanup(func() {
		globalProxyProfilePath = oldProfile
		globalProxyAPTPath = oldAPT
		globalProxyReadArtifact = oldRead
		globalProxyWriteArtifact = oldWrite
		globalProxyRemoveArtifact = oldRemove
		globalProxySyncArtifactDir = oldSync
		globalProxyWriteJournal = oldJournalWrite
		globalProxyCASAfterQuarantine = oldAfterQuarantine
	})
	if err := os.MkdirAll(filepath.Dir(profile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(apt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(a.cfg.CoreDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return profile, apt
}

func assertGlobalProxyJournalCopiesAbsent(t *testing.T, a *App) {
	t.Helper()
	for _, path := range []string{a.globalProxyJournalPath(), a.globalProxyJournalBackupPath()} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("global journal copy remained after restore: %s err=%v", path, err)
		}
	}
}

func mustReadGlobalProxyTestState(t *testing.T, path string) globalProxyArtifactState {
	t.Helper()
	state, err := readGlobalProxyArtifactStateNoFollow(path, maxGlobalProxyArtifactBytes)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func globalProxyTestQuarantinePath(path string) string {
	return filepath.Join(filepath.Dir(path), globalProxyQuarantineName(filepath.Base(path)))
}

func assertGlobalProxyTestFile(t *testing.T, path string, want []byte, mode os.FileMode) {
	t.Helper()
	state := mustReadGlobalProxyTestState(t, path)
	if !state.present || !bytes.Equal(state.content, want) || state.mode.Perm() != mode.Perm() {
		t.Fatalf("state for %s = %+v, want content=%q mode=%#o", path, state, want, mode.Perm())
	}
}

func assertGlobalProxyTestQuarantineAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(globalProxyTestQuarantinePath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("global proxy quarantine remained for %s: %v", path, err)
	}
}

func TestGlobalProxyJournalWritesAndRecoversDualCopies(t *testing.T) {
	a := testApp(t)
	withGlobalProxyTestPaths(t, a)
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	mainJournal, err := readGlobalProxyJournalFile(a.globalProxyJournalPath())
	if err != nil {
		t.Fatal(err)
	}
	backupJournal, err := readGlobalProxyJournalFile(a.globalProxyJournalBackupPath())
	if err != nil {
		t.Fatal(err)
	}
	if mainJournal.Generation == 0 || !reflect.DeepEqual(mainJournal, backupJournal) {
		t.Fatalf("journal copies differ: main=%+v backup=%+v", mainJournal, backupJournal)
	}

	if err := os.WriteFile(a.globalProxyJournalPath(), []byte("{\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := a.loadGlobalProxyJournal()
	if err != nil || !reflect.DeepEqual(recovered, backupJournal) {
		t.Fatalf("did not recover corrupt main from backup: got=%+v err=%v", recovered, err)
	}
	if err := a.saveGlobalProxyJournal(recovered); err != nil {
		t.Fatalf("failed to heal journal copies: %v", err)
	}
	if err := os.WriteFile(a.globalProxyJournalBackupPath(), []byte("{\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err = a.loadGlobalProxyJournal()
	if err != nil || recovered.Generation != backupJournal.Generation+1 {
		t.Fatalf("did not recover corrupt backup from main: got=%+v err=%v", recovered, err)
	}
}

func TestGlobalProxyJournalRejectsEqualGenerationSplitBrain(t *testing.T) {
	a := testApp(t)
	withGlobalProxyTestPaths(t, a)
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	backup, err := readGlobalProxyJournalFile(a.globalProxyJournalBackupPath())
	if err != nil {
		t.Fatal(err)
	}
	backup.Artifacts[0].ManagedGID ^= 1
	raw, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(a.globalProxyJournalBackupPath(), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.loadGlobalProxyJournal(); err == nil || !strings.Contains(err.Error(), "同一 generation 内容不一致") {
		t.Fatalf("equal-generation split brain was accepted: %v", err)
	}
}

func TestGlobalProxyJournalMainWriteFailureLeavesRecoverableBackup(t *testing.T) {
	a := testApp(t)
	withGlobalProxyTestPaths(t, a)
	journal, err := a.newGlobalProxyJournal(globalProxyDesiredArtifacts(a.cfg))
	if err != nil {
		t.Fatal(err)
	}
	mainPath := a.globalProxyJournalPath()
	globalProxyWriteJournal = func(path string, data []byte, mode os.FileMode) error {
		if path == mainPath {
			return errors.New("injected main journal write failure")
		}
		return writeFileAtomic(path, data, mode)
	}
	if err := a.saveGlobalProxyJournal(journal); err == nil || !strings.Contains(err.Error(), "injected main journal write failure") {
		t.Fatalf("main journal write failure was not returned: %v", err)
	}
	loaded, err := a.loadGlobalProxyJournal()
	if err != nil || loaded.Generation != 1 || loaded.Phase != globalProxyPhasePrepared {
		t.Fatalf("newer backup was not recoverable: journal=%+v err=%v", loaded, err)
	}
	globalProxyWriteJournal = writeFileAtomic
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatalf("backup recovery did not converge: %v", err)
	}
	assertGlobalProxyJournalCopiesAbsent(t, a)
}

func TestGlobalProxyJournalRestoresOriginalFilesAndModes(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	profileOriginal := []byte("export operator_profile=1\n")
	aptOriginal := []byte("Acquire::Retries \"3\";\n")
	if err := os.WriteFile(profile, profileOriginal, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(apt, aptOriginal, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	journal, err := a.loadGlobalProxyJournal()
	if err != nil || journal.Phase != globalProxyPhaseActive {
		t.Fatalf("active journal missing: journal=%+v err=%v", journal, err)
	}
	desired := globalProxyDesiredArtifacts(a.cfg)
	for _, path := range []string{profile, apt} {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, desired[path]) {
			t.Fatalf("managed content mismatch for %s: %q err=%v", path, got, readErr)
		}
	}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{profile: profileOriginal, apt: aptOriginal} {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("original content mismatch for %s: %q err=%v", path, got, readErr)
		}
	}
	for path, want := range map[string]os.FileMode{profile: 0o640, apt: 0o600} {
		info, statErr := os.Stat(path)
		if statErr != nil || info.Mode().Perm() != want {
			t.Fatalf("original mode mismatch for %s: mode=%v err=%v", path, info.Mode().Perm(), statErr)
		}
	}
	if _, err := os.Lstat(a.globalProxyJournalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remained after restore: %v", err)
	}
}

func TestGlobalProxyJournalRemovesOnlyOwnedAbsentFiles(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{profile, apt} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned file was not removed: %s err=%v", path, err)
		}
	}
}

func TestGlobalProxyRestoreRetriesDirectoryBarrierBeforeDroppingJournal(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	oldRemove := globalProxyRemoveArtifact
	oldSync := globalProxySyncArtifactDir
	t.Cleanup(func() {
		globalProxyRemoveArtifact = oldRemove
		globalProxySyncArtifactDir = oldSync
	})
	failedBarrier := false
	globalProxyRemoveArtifact = func(path string, expected []globalProxyArtifactState) (bool, error) {
		removed, err := oldRemove(path, expected)
		if err == nil && removed && !failedBarrier {
			failedBarrier = true
			return true, errors.New("injected parent fsync failure after unlink")
		}
		return removed, err
	}
	barriers := 0
	globalProxySyncArtifactDir = func(string) error {
		barriers++
		return nil
	}
	if err := a.restoreGlobalWithJournal(); err == nil || !strings.Contains(err.Error(), "injected parent fsync failure") {
		t.Fatalf("post-unlink fsync failure was not returned: %v", err)
	}
	if _, err := os.Stat(a.globalProxyJournalPath()); err != nil {
		t.Fatalf("journal was lost after uncertain unlink durability: %v", err)
	}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatalf("restore retry failed: %v", err)
	}
	if barriers != 2 {
		t.Fatalf("retry did not establish both target-directory barriers: %d", barriers)
	}
	for _, path := range []string{profile, apt} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned target remained after retry: %s err=%v", path, err)
		}
	}
	if _, err := os.Lstat(a.globalProxyJournalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remained after durable retry: %v", err)
	}
}

func TestGlobalProxyCleanupWithoutJournalPreservesFiles(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	for _, path := range []string{profile, apt} {
		if err := os.WriteFile(path, []byte("operator-owned\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{profile, apt} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "operator-owned\n" {
			t.Fatalf("cleanup touched unowned file %s: %q err=%v", path, got, err)
		}
	}
}

func TestGlobalProxyCleanupPreservesModifiedManagedFile(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	modified := []byte("operator changed this while active\n")
	if err := os.WriteFile(profile, modified, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatalf("modified managed target should transfer ownership: %v", err)
	}
	got, readErr := os.ReadFile(profile)
	if readErr != nil || !bytes.Equal(got, modified) {
		t.Fatalf("modified target was overwritten: %q err=%v", got, readErr)
	}
	if _, statErr := os.Lstat(a.globalProxyJournalPath()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("ownership journal remained after transfer: %v", statErr)
	}
	if _, statErr := os.Lstat(apt); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unchanged owned peer was not restored: %v", statErr)
	}
}

func TestGlobalProxyCleanupPreservesMetadataOnlyAdministratorChange(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	desired := append([]byte(nil), globalProxyDesiredArtifacts(a.cfg)[profile]...)
	if err := os.Chmod(profile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatalf("metadata-only administrator change should transfer ownership: %v", err)
	}
	got, err := os.ReadFile(profile)
	if err != nil || !bytes.Equal(got, desired) {
		t.Fatalf("administrator-owned content changed: got=%q err=%v", got, err)
	}
	info, err := os.Stat(profile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("administrator mode was not preserved: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(apt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unchanged owned peer was not restored: %v", err)
	}
	assertGlobalProxyJournalCopiesAbsent(t, a)
}

func TestGlobalProxyApplyReplaceCASPreservesConcurrentFinalName(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	for _, path := range []string{profile, apt} {
		if err := os.WriteFile(path, []byte("operator original\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	concurrent := []byte("operator replaced profile during apply\n")
	var hookErr error
	fired := false
	globalProxyCASAfterQuarantine = func(path string) {
		if path != profile || fired {
			return
		}
		fired = true
		hookErr = os.WriteFile(profile, concurrent, 0o600)
	}
	err := a.applyGlobalWithJournal()
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !fired || !errors.Is(err, errGlobalProxyArtifactChanged) {
		t.Fatalf("apply replace race was not rejected: fired=%v err=%v", fired, err)
	}
	assertGlobalProxyTestFile(t, profile, concurrent, 0o600)
	assertGlobalProxyTestQuarantineAbsent(t, profile)
	assertGlobalProxyJournalCopiesAbsent(t, a)
}

func TestGlobalProxyRestoreReplaceCASPreservesConcurrentFinalName(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	for _, path := range []string{profile, apt} {
		if err := os.WriteFile(path, []byte("operator original\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("operator replaced profile during restore\n")
	var hookErr error
	fired := false
	globalProxyCASAfterQuarantine = func(path string) {
		if path != profile || fired {
			return
		}
		fired = true
		hookErr = os.WriteFile(profile, concurrent, 0o600)
	}
	err := a.restoreGlobalWithJournal()
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !fired || !errors.Is(err, errGlobalProxyArtifactChanged) {
		t.Fatalf("restore replace race was not rejected: fired=%v err=%v", fired, err)
	}
	assertGlobalProxyTestFile(t, profile, concurrent, 0o600)
	assertGlobalProxyTestQuarantineAbsent(t, profile)
	globalProxyCASAfterQuarantine = func(string) {}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatalf("restore retry did not converge: %v", err)
	}
	assertGlobalProxyTestFile(t, profile, concurrent, 0o600)
	assertGlobalProxyJournalCopiesAbsent(t, a)
}

func TestGlobalProxyRestoreRemoveCASPreservesConcurrentFinalName(t *testing.T) {
	a := testApp(t)
	profile, _ := withGlobalProxyTestPaths(t, a)
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	concurrent := []byte("operator created profile during remove\n")
	var hookErr error
	fired := false
	globalProxyCASAfterQuarantine = func(path string) {
		if path != profile || fired {
			return
		}
		fired = true
		hookErr = os.WriteFile(profile, concurrent, 0o600)
	}
	err := a.restoreGlobalWithJournal()
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if !fired || !errors.Is(err, errGlobalProxyArtifactChanged) {
		t.Fatalf("restore remove race was not rejected: fired=%v err=%v", fired, err)
	}
	assertGlobalProxyTestFile(t, profile, concurrent, 0o600)
	assertGlobalProxyTestQuarantineAbsent(t, profile)
	globalProxyCASAfterQuarantine = func(string) {}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatalf("remove retry did not converge: %v", err)
	}
	assertGlobalProxyTestFile(t, profile, concurrent, 0o600)
	assertGlobalProxyJournalCopiesAbsent(t, a)
}

func TestGlobalProxyApplyCreateCASPreservesConcurrentFinalName(t *testing.T) {
	a := testApp(t)
	profile, _ := withGlobalProxyTestPaths(t, a)
	concurrent := []byte("operator created profile before no-replace commit\n")
	oldWrite := globalProxyWriteArtifact
	fired := false
	var injectErr error
	globalProxyWriteArtifact = func(path string, expected []globalProxyArtifactState, desired globalProxyArtifactState) error {
		if path == profile && !fired {
			fired = true
			injectErr = os.WriteFile(profile, concurrent, 0o600)
		}
		return oldWrite(path, expected, desired)
	}
	err := a.applyGlobalWithJournal()
	if injectErr != nil {
		t.Fatal(injectErr)
	}
	if !fired || !errors.Is(err, errGlobalProxyArtifactChanged) {
		t.Fatalf("create race was not rejected: fired=%v err=%v", fired, err)
	}
	assertGlobalProxyTestFile(t, profile, concurrent, 0o600)
	assertGlobalProxyTestQuarantineAbsent(t, profile)
	assertGlobalProxyJournalCopiesAbsent(t, a)
}

func TestGlobalProxyCASRejectsClaimedMetadataDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(string) error
		check  func(globalProxyArtifactState) bool
	}{
		{
			name: "mode",
			mutate: func(path string) error {
				return os.Chmod(path, 0o600)
			},
			check: func(state globalProxyArtifactState) bool { return state.mode.Perm() == 0o600 },
		},
	}
	if os.Geteuid() == 0 {
		tests = append(tests,
			struct {
				name   string
				mutate func(string) error
				check  func(globalProxyArtifactState) bool
			}{
				name: "uid",
				mutate: func(path string) error {
					return os.Chown(path, 65534, -1)
				},
				check: func(state globalProxyArtifactState) bool { return state.uid == 65534 },
			},
			struct {
				name   string
				mutate func(string) error
				check  func(globalProxyArtifactState) bool
			}{
				name: "gid",
				mutate: func(path string) error {
					return os.Chown(path, -1, 65534)
				},
				check: func(state globalProxyArtifactState) bool { return state.gid == 65534 },
			},
		)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "profile.d")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "managed")
			expectedContent := []byte("same bytes\n")
			if err := os.WriteFile(path, expectedContent, 0o640); err != nil {
				t.Fatal(err)
			}
			expected := mustReadGlobalProxyTestState(t, path)
			desired := expected
			desired.content = []byte("replacement\n")
			var hookErr error
			oldHook := globalProxyCASAfterQuarantine
			globalProxyCASAfterQuarantine = func(claimedPath string) {
				hookErr = tc.mutate(globalProxyTestQuarantinePath(claimedPath))
			}
			t.Cleanup(func() { globalProxyCASAfterQuarantine = oldHook })
			err := writeGlobalProxyArtifactCAS(path, []globalProxyArtifactState{expected}, desired)
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			if !errors.Is(err, errGlobalProxyArtifactChanged) {
				t.Fatalf("claimed metadata drift was accepted: %v", err)
			}
			got := mustReadGlobalProxyTestState(t, path)
			if !bytes.Equal(got.content, expectedContent) || !tc.check(got) {
				t.Fatalf("claimed inode was not restored with administrator metadata: %+v", got)
			}
			assertGlobalProxyTestQuarantineAbsent(t, path)
		})
	}
}

func TestGlobalProxyWriteCASReplaysQuarantineStates(t *testing.T) {
	tests := []struct {
		name        string
		final       []byte
		finalMode   os.FileMode
		want        []byte
		wantMode    os.FileMode
		wantChanged bool
	}{
		{name: "claim persisted before replacement", want: []byte("desired\n"), wantMode: 0o644},
		{name: "replacement persisted before cleanup", final: []byte("desired\n"), finalMode: 0o644, want: []byte("desired\n"), wantMode: 0o644},
		{name: "concurrent final name wins", final: []byte("operator new\n"), finalMode: 0o600, want: []byte("operator new\n"), wantMode: 0o600, wantChanged: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "profile.d")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "managed")
			quarantine := globalProxyTestQuarantinePath(path)
			if err := os.WriteFile(quarantine, []byte("expected\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			if tc.final != nil {
				if err := os.WriteFile(path, tc.final, tc.finalMode); err != nil {
					t.Fatal(err)
				}
			}
			expected := mustReadGlobalProxyTestState(t, quarantine)
			desired := globalProxyArtifactState{
				present: true,
				content: []byte("desired\n"),
				mode:    0o644,
				uid:     uint32(os.Geteuid()),
				gid:     uint32(os.Getegid()),
			}
			err := writeGlobalProxyArtifactCAS(path, []globalProxyArtifactState{expected}, desired)
			if tc.wantChanged {
				if !errors.Is(err, errGlobalProxyArtifactChanged) {
					t.Fatalf("concurrent replay was accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			assertGlobalProxyTestFile(t, path, tc.want, tc.wantMode)
			assertGlobalProxyTestQuarantineAbsent(t, path)
		})
	}
}

func TestGlobalProxyJournalReplaysFirstApplyClaimQuarantine(t *testing.T) {
	t.Run("apply resumes claimed original", func(t *testing.T) {
		a := testApp(t)
		profile, apt := withGlobalProxyTestPaths(t, a)
		for _, path := range []string{profile, apt} {
			if err := os.WriteFile(path, []byte("operator original\n"), 0o640); err != nil {
				t.Fatal(err)
			}
		}
		journal, err := a.newGlobalProxyJournal(globalProxyDesiredArtifacts(a.cfg))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.saveGlobalProxyJournal(journal); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(profile, globalProxyTestQuarantinePath(profile)); err != nil {
			t.Fatal(err)
		}
		if err := fsyncDir(filepath.Dir(profile)); err != nil {
			t.Fatal(err)
		}
		if err := a.applyGlobalWithJournal(); err != nil {
			t.Fatalf("apply did not replay claimed original: %v", err)
		}
		assertGlobalProxyTestFile(t, profile, globalProxyDesiredArtifacts(a.cfg)[profile], 0o644)
		assertGlobalProxyTestQuarantineAbsent(t, profile)
		if err := a.restoreGlobalWithJournal(); err != nil {
			t.Fatal(err)
		}
		assertGlobalProxyTestFile(t, profile, []byte("operator original\n"), 0o640)
	})

	t.Run("restore rolls back claimed original", func(t *testing.T) {
		a := testApp(t)
		profile, apt := withGlobalProxyTestPaths(t, a)
		for _, path := range []string{profile, apt} {
			if err := os.WriteFile(path, []byte("operator original\n"), 0o640); err != nil {
				t.Fatal(err)
			}
		}
		journal, err := a.newGlobalProxyJournal(globalProxyDesiredArtifacts(a.cfg))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.saveGlobalProxyJournal(journal); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(profile, globalProxyTestQuarantinePath(profile)); err != nil {
			t.Fatal(err)
		}
		if err := fsyncDir(filepath.Dir(profile)); err != nil {
			t.Fatal(err)
		}
		if err := a.restoreGlobalWithJournal(); err != nil {
			t.Fatalf("restore did not replay claimed original: %v", err)
		}
		assertGlobalProxyTestFile(t, profile, []byte("operator original\n"), 0o640)
		assertGlobalProxyTestQuarantineAbsent(t, profile)
		assertGlobalProxyJournalCopiesAbsent(t, a)
	})

	t.Run("restore rolls back replacement persisted before cleanup", func(t *testing.T) {
		a := testApp(t)
		profile, apt := withGlobalProxyTestPaths(t, a)
		for _, path := range []string{profile, apt} {
			if err := os.WriteFile(path, []byte("operator original\n"), 0o640); err != nil {
				t.Fatal(err)
			}
		}
		desired := globalProxyDesiredArtifacts(a.cfg)
		journal, err := a.newGlobalProxyJournal(desired)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.saveGlobalProxyJournal(journal); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(profile, globalProxyTestQuarantinePath(profile)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(profile, desired[profile], 0o644); err != nil {
			t.Fatal(err)
		}
		if err := fsyncDir(filepath.Dir(profile)); err != nil {
			t.Fatal(err)
		}
		if err := a.restoreGlobalWithJournal(); err != nil {
			t.Fatalf("restore did not replay committed replacement: %v", err)
		}
		assertGlobalProxyTestFile(t, profile, []byte("operator original\n"), 0o640)
		assertGlobalProxyTestQuarantineAbsent(t, profile)
		assertGlobalProxyJournalCopiesAbsent(t, a)
	})
}

func TestGlobalProxyRemoveCASReplaysQuarantineStates(t *testing.T) {
	tests := []struct {
		name        string
		final       []byte
		finalMode   os.FileMode
		wantChanged bool
	}{
		{name: "claim persisted before unlink"},
		{name: "concurrent different final wins", final: []byte("operator new\n"), finalMode: 0o600, wantChanged: true},
		{name: "concurrent equal final still wins", final: []byte("expected\n"), finalMode: 0o640, wantChanged: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "profile.d")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "managed")
			quarantine := globalProxyTestQuarantinePath(path)
			if err := os.WriteFile(quarantine, []byte("expected\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			if tc.final != nil {
				if err := os.WriteFile(path, tc.final, tc.finalMode); err != nil {
					t.Fatal(err)
				}
			}
			expected := mustReadGlobalProxyTestState(t, quarantine)
			removed, err := removeGlobalProxyArtifactCAS(path, []globalProxyArtifactState{expected})
			if tc.wantChanged {
				if removed || !errors.Is(err, errGlobalProxyArtifactChanged) {
					t.Fatalf("concurrent remove replay was accepted: removed=%v err=%v", removed, err)
				}
				assertGlobalProxyTestFile(t, path, tc.final, tc.finalMode)
			} else {
				if !removed || err != nil {
					t.Fatalf("remove replay did not complete: removed=%v err=%v", removed, err)
				}
				if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("removed final name reappeared: %v", statErr)
				}
			}
			assertGlobalProxyTestQuarantineAbsent(t, path)
		})
	}
}

func TestGlobalProxyRestoresOriginalUIDAndGID(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown restoration requires root")
	}
	a := testApp(t)
	profile, _ := withGlobalProxyTestPaths(t, a)
	original := []byte("operator-owned profile\n")
	if err := os.WriteFile(profile, original, 0o640); err != nil {
		t.Fatal(err)
	}
	const originalID = 65534
	if err := os.Chown(profile, originalID, originalID); err != nil {
		t.Fatal(err)
	}
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(profile)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != originalID || stat.Gid != originalID || info.Mode().Perm() != 0o640 {
		t.Fatalf("original metadata not restored: uid=%d gid=%d mode=%#o", stat.Uid, stat.Gid, info.Mode().Perm())
	}
	got, err := os.ReadFile(profile)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("original content not restored: got=%q err=%v", got, err)
	}
}

func TestGlobalProxyProductionRemoverAndBarrier(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "managed")
	if err := os.WriteFile(path, []byte("managed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := removeGlobalProxyArtifactDurable(path)
	if err != nil || !removed {
		t.Fatalf("production remover failed: removed=%v err=%v", removed, err)
	}
	removed, err = removeGlobalProxyArtifactDurable(path)
	if err != nil || removed {
		t.Fatalf("production remover did not converge: removed=%v err=%v", removed, err)
	}
	if err := syncGlobalProxyArtifactDir(path); err != nil {
		t.Fatalf("production directory barrier failed: %v", err)
	}
}

func TestGlobalProxyProductionPathsRejectIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real", "profile.d")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(filepath.Join(root, "real"), linkedParent); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(linkedParent, "profile.d", "managed")
	if err := os.WriteFile(filepath.Join(realDir, "managed"), []byte("managed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readGlobalProxyArtifact(path); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("read accepted an intermediate symlink: %v", err)
	}
	if _, err := removeGlobalProxyArtifactDurable(path); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("remove accepted an intermediate symlink: %v", err)
	}
	if err := syncGlobalProxyArtifactDir(path); err == nil || !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("barrier accepted an intermediate symlink: %v", err)
	}
}

func TestGlobalProxyDisabledStoreCleanupReconcilesJournal(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	st := newStore()
	st.RuntimeConfig = a.cfg.runtimeConfig()
	if st.SceneEnabled[SceneGlobal] {
		t.Fatal("test requires disabled Store state")
	}
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	stubStoreTransitionCore(t)
	if err := a.commitStoreMutation(st, func(*Store) error { return nil }, storeRuntimeSyncAll); err != nil {
		t.Fatalf("disabled Store did not reconcile global journal: %v", err)
	}
	for _, path := range []string{profile, apt} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("disabled Store left owned artifact: %s err=%v", path, err)
		}
	}
	assertGlobalProxyJournalCopiesAbsent(t, a)
}

func TestGlobalProxyWritesJournalBeforeArtifacts(t *testing.T) {
	a := testApp(t)
	profile, _ := withGlobalProxyTestPaths(t, a)
	oldWrite := globalProxyWriteArtifact
	t.Cleanup(func() { globalProxyWriteArtifact = oldWrite })
	checked := false
	globalProxyWriteArtifact = func(path string, expected []globalProxyArtifactState, desired globalProxyArtifactState) error {
		if path == profile && !checked {
			checked = true
			journal, err := a.loadGlobalProxyJournal()
			if err != nil || journal.Phase != globalProxyPhasePrepared {
				t.Fatalf("artifact write preceded durable journal: journal=%+v err=%v", journal, err)
			}
		}
		return oldWrite(path, expected, desired)
	}
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("artifact writer was not called")
	}
}

func TestGlobalProxyPreparedApplyConflictDoesNotPartiallyWritePair(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	desired := globalProxyDesiredArtifacts(a.cfg)
	journal, err := a.newGlobalProxyJournal(desired)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.saveGlobalProxyJournal(journal); err != nil {
		t.Fatal(err)
	}
	operatorAPT := []byte("operator claimed apt after journal prepare\n")
	if err := os.WriteFile(apt, operatorAPT, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.applyGlobalWithJournal(); err == nil || !strings.Contains(err.Error(), "发生变化") {
		t.Fatalf("second-target conflict was not returned: %v", err)
	}
	if _, err := os.Lstat(profile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile was partially written before apt conflict: %v", err)
	}
	got, err := os.ReadFile(apt)
	if err != nil || !bytes.Equal(got, operatorAPT) {
		t.Fatalf("operator apt content changed: got=%q err=%v", got, err)
	}
}

func TestGlobalProxyPreparedUpdateConflictKeepsOldManagedPair(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	oldDesired := globalProxyDesiredArtifacts(a.cfg)
	a.cfg.GlobalHTTPPort++
	a.cfg.GlobalSocksPort++
	newDesired := globalProxyDesiredArtifacts(a.cfg)
	if _, err := a.prepareGlobalProxyJournal(newDesired); err != nil {
		t.Fatal(err)
	}
	operatorAPT := []byte("operator claimed apt during update\n")
	if err := os.WriteFile(apt, operatorAPT, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.applyGlobalWithJournal(); err == nil || !strings.Contains(err.Error(), "发生变化") {
		t.Fatalf("prepared update conflict was not returned: %v", err)
	}
	gotProfile, err := os.ReadFile(profile)
	if err != nil || !bytes.Equal(gotProfile, oldDesired[profile]) {
		t.Fatalf("profile partially changed to pending value: got=%q pending=%q err=%v", gotProfile, newDesired[profile], err)
	}
	gotAPT, err := os.ReadFile(apt)
	if err != nil || !bytes.Equal(gotAPT, operatorAPT) {
		t.Fatalf("operator apt content changed: got=%q err=%v", gotAPT, err)
	}
}

func TestGlobalProxyApplyFailureRollsBackAndDropsJournal(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	profileOriginal := []byte("original profile\n")
	aptOriginal := []byte("original apt\n")
	if err := os.WriteFile(profile, profileOriginal, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(apt, aptOriginal, 0o644); err != nil {
		t.Fatal(err)
	}
	oldWrite := globalProxyWriteArtifact
	t.Cleanup(func() { globalProxyWriteArtifact = oldWrite })
	failed := false
	globalProxyWriteArtifact = func(path string, expected []globalProxyArtifactState, desired globalProxyArtifactState) error {
		if path == apt && !bytes.Equal(desired.content, aptOriginal) && !failed {
			failed = true
			return errors.New("injected write failure")
		}
		return oldWrite(path, expected, desired)
	}
	if err := a.applyGlobalWithJournal(); err == nil || !strings.Contains(err.Error(), "injected write failure") {
		t.Fatalf("apply failure not returned: %v", err)
	}
	for path, want := range map[string][]byte{profile: profileOriginal, apt: aptOriginal} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("rollback mismatch for %s: %q err=%v", path, got, err)
		}
	}
	if _, err := os.Lstat(a.globalProxyJournalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remained after successful rollback: %v", err)
	}
}

func TestGlobalProxyReapplyChangesManagedValueWithoutLosingOriginal(t *testing.T) {
	a := testApp(t)
	profile, _ := withGlobalProxyTestPaths(t, a)
	original := []byte("original profile\n")
	if err := os.WriteFile(profile, original, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	a.cfg.GlobalHTTPPort++
	a.cfg.GlobalSocksPort++
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(profile)
	if err != nil || !bytes.Equal(got, globalProxyDesiredArtifacts(a.cfg)[profile]) {
		t.Fatalf("updated managed value missing: %q err=%v", got, err)
	}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(profile)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("original lost across reapply: %q err=%v", got, err)
	}
}

func TestGlobalProxyExactPreexistingFilesRemainOriginalWithoutLegacyState(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	desired := globalProxyDesiredArtifacts(a.cfg)
	for _, path := range []string{profile, apt} {
		if err := os.WriteFile(path, desired[path], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.applyGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{profile, apt} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, desired[path]) {
			t.Fatalf("coincidentally matching original was not preserved: %s got=%q err=%v", path, got, err)
		}
	}
}

func TestGlobalProxyLegacyMigrationRequiresStateEvidenceAndExactPair(t *testing.T) {
	a := testApp(t)
	profile, apt := withGlobalProxyTestPaths(t, a)
	legacyCfg := a.cfg
	legacyCfg.GlobalHTTPPort = 18090
	legacyCfg.GlobalSocksPort = 18094
	desired := globalProxyDesiredArtifacts(legacyCfg)
	for _, path := range []string{profile, apt} {
		if err := os.WriteFile(path, desired[path], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.migrateLegacyGlobalProxyOwnership(); err != nil {
		t.Fatal(err)
	}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{profile, apt} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy managed artifact was not cleaned: %s err=%v", path, err)
		}
	}

	if err := os.WriteFile(profile, desired[profile], 0o644); err != nil {
		t.Fatal(err)
	}
	operatorAPT := []byte("operator apt content\n")
	if err := os.WriteFile(apt, operatorAPT, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateLegacyGlobalProxyOwnership(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(a.globalProxyJournalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial legacy match unexpectedly gained ownership: %v", err)
	}
	if err := a.restoreGlobalWithJournal(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{profile: desired[profile], apt: operatorAPT} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("unowned partial pair changed: %s got=%q err=%v", path, got, err)
		}
	}
}

func TestLegacyGlobalProxyTemplateParserRejectsInconsistentOrUnsafeContent(t *testing.T) {
	profile, apt := globalProxyArtifactContents("http://127.0.0.1:18090", "socks5h://127.0.0.1:18094")
	if !parseLegacyGlobalProxyArtifacts(profile, apt) {
		t.Fatal("valid historical custom-port template was rejected")
	}
	cases := []struct {
		name    string
		profile []byte
		apt     []byte
	}{
		{name: "cross-file mismatch", profile: profile, apt: bytes.Replace(apt, []byte(":18090"), []byte(":28090"), 1)},
		{name: "mixed hosts", profile: bytes.Replace(profile, []byte("socks5h://127.0.0.1"), []byte("socks5h://127.0.0.2"), -1), apt: apt},
		{name: "userinfo", profile: bytes.Replace(profile, []byte("http://127.0.0.1"), []byte("http://user@127.0.0.1"), -1), apt: bytes.Replace(apt, []byte("http://127.0.0.1"), []byte("http://user@127.0.0.1"), -1)},
		{name: "extra directive", profile: append(append([]byte(nil), profile...), []byte("export BAD=1\n")...), apt: apt},
		{name: "noncanonical quote", profile: bytes.Replace(profile, []byte(`"http://127.0.0.1:18090"`), []byte(`"http:\u002f\u002f127.0.0.1:18090"`), -1), apt: apt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if parseLegacyGlobalProxyArtifacts(tc.profile, tc.apt) {
				t.Fatal("unsafe or inconsistent historical template was accepted")
			}
		})
	}
}
