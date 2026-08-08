package manager

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func removeFileForTest(path string) (bool, error) {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func newRootUserTestDir(t *testing.T, pattern string) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires the root account and its trusted home directory")
	}
	dir, err := os.MkdirTemp("/root", pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestReadRegularFileNoFollowRejectsFIFOAndDevice(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "unit.service")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularFileNoFollow(fifo, 1024); err == nil || !strings.Contains(err.Error(), "普通文件") {
		t.Fatalf("FIFO must be rejected without blocking: %v", err)
	}
	if _, err := readRegularFileNoFollow("/dev/null", 1024); err == nil || !strings.Contains(err.Error(), "普通文件") {
		t.Fatalf("device must be rejected: %v", err)
	}
}

func TestRemoveUserFileNoFollowCannotEscapeIntermediateSymlink(t *testing.T) {
	base := newRootUserTestDir(t, ".proxyscene-remove-test-")
	outside := newRootUserTestDir(t, ".proxyscene-remove-outside-")
	parent := filepath.Join(base, ".config/systemd/user")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "10-openclaw-hermes-telegram-proxy.conf")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "gateway.service.d")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	managedPath := filepath.Join(link, filepath.Base(outsideFile))
	removed, err := removeUserFileAndEmptyParentNoFollow("root", managedPath)
	if err == nil || removed {
		t.Fatalf("intermediate symlink must be rejected: removed=%v err=%v", removed, err)
	}
	if data, readErr := os.ReadFile(outsideFile); readErr != nil || string(data) != "keep" {
		t.Fatalf("outside file was affected: data=%q err=%v", data, readErr)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink rejection should not be reported as a missing target: %v", err)
	}
}

func TestWriteUserFileAtomicCASPreservesConcurrentReplacement(t *testing.T) {
	dir := newRootUserTestDir(t, ".proxyscene-user-cas-write-")
	path := filepath.Join(dir, "managed.conf")
	if err := os.WriteFile(path, []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldHook := userFileCASAfterQuarantine
	t.Cleanup(func() { userFileCASAfterQuarantine = oldHook })
	userFileCASAfterQuarantine = func(got string) {
		if got != path {
			t.Fatalf("CAS hook path = %q, want %q", got, path)
		}
		if err := os.WriteFile(path, []byte("user-new"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	err := writeUserFileAtomicCAS("root", path, []byte("managed"), []byte("desired"), 0o600)
	if !errors.Is(err, errUserFileChanged) {
		t.Fatalf("concurrent replacement error = %v, want %v", err, errUserFileChanged)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != "user-new" {
		t.Fatalf("concurrent replacement was overwritten: got=%q err=%v", got, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, userFileQuarantineName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("confirmed old managed inode was not cleaned up: %v", statErr)
	}
}

func TestRemoveUserFileCASPreservesConcurrentReplacement(t *testing.T) {
	dir := newRootUserTestDir(t, ".proxyscene-user-cas-remove-")
	path := filepath.Join(dir, "managed.conf")
	if err := os.WriteFile(path, []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldHook := userFileCASAfterQuarantine
	t.Cleanup(func() { userFileCASAfterQuarantine = oldHook })
	userFileCASAfterQuarantine = func(string) {
		if err := os.WriteFile(path, []byte("user-new"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := removeUserFileAndEmptyParentCAS("root", path, []byte("managed"))
	if err != nil || !removed {
		t.Fatalf("conditional remove = (%v, %v), want (true, nil)", removed, err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != "user-new" {
		t.Fatalf("concurrent replacement was removed: got=%q err=%v", got, readErr)
	}
}

func TestUserFileCASRetainsQuarantineWhenRestoreWouldOverwrite(t *testing.T) {
	dir := newRootUserTestDir(t, ".proxyscene-user-cas-quarantine-")
	path := filepath.Join(dir, "managed.conf")
	if err := os.WriteFile(path, []byte("changed-before-claim"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldHook := userFileCASAfterQuarantine
	t.Cleanup(func() { userFileCASAfterQuarantine = oldHook })
	userFileCASAfterQuarantine = func(string) {
		if err := os.WriteFile(path, []byte("user-new"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	err := writeUserFileAtomicCAS("root", path, []byte("managed"), []byte("desired"), 0o600)
	quarantinePath := filepath.Join(dir, userFileQuarantineName)
	if !errors.Is(err, errUserFileChanged) || !strings.Contains(err.Error(), quarantinePath) {
		t.Fatalf("unsafe restore error = %v, want changed error containing %s", err, quarantinePath)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != "user-new" {
		t.Fatalf("new final file changed: got=%q err=%v", got, readErr)
	}
	quarantined, readErr := os.ReadFile(quarantinePath)
	if readErr != nil || string(quarantined) != "changed-before-claim" {
		t.Fatalf("conflicting inode was not retained: got=%q err=%v", quarantined, readErr)
	}
}

func newUserCASReplayPaths(t *testing.T) (dir, path, quarantinePath string) {
	t.Helper()
	dir = newRootUserTestDir(t, ".proxyscene-user-cas-replay-")
	return dir, filepath.Join(dir, "managed.conf"), filepath.Join(dir, userFileQuarantineName)
}

func TestWriteUserFileAtomicCASReplaysQuarantineStates(t *testing.T) {
	expected := []byte("managed")
	desired := []byte("desired")
	tests := []struct {
		name           string
		quarantine     []byte
		base           []byte
		wantErrChanged bool
		wantBase       []byte
		wantQuarantine bool
	}{
		{name: "claim persisted before replacement", quarantine: expected, wantBase: desired},
		{name: "replacement persisted before cleanup", quarantine: expected, base: desired, wantBase: desired},
		{name: "concurrent final name wins", quarantine: expected, base: []byte("user-new"), wantErrChanged: true, wantBase: []byte("user-new")},
		{name: "mismatched quarantine without final", quarantine: []byte("unknown"), wantErrChanged: true, wantQuarantine: true},
		{name: "mismatched quarantine with final", quarantine: []byte("unknown"), base: []byte("user-new"), wantErrChanged: true, wantBase: []byte("user-new"), wantQuarantine: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, path, quarantinePath := newUserCASReplayPaths(t)
			if err := os.WriteFile(quarantinePath, tc.quarantine, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.base != nil {
				if err := os.WriteFile(path, tc.base, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := writeUserFileAtomicCAS("root", path, expected, desired, 0o600)
			if tc.wantErrChanged != errors.Is(err, errUserFileChanged) {
				t.Fatalf("changed error=%v, want=%v (err=%v)", errors.Is(err, errUserFileChanged), tc.wantErrChanged, err)
			}
			if !tc.wantErrChanged && err != nil {
				t.Fatal(err)
			}
			base, baseErr := os.ReadFile(path)
			if tc.wantBase == nil {
				if !errors.Is(baseErr, os.ErrNotExist) {
					t.Fatalf("final name unexpectedly exists: data=%q err=%v", base, baseErr)
				}
			} else if baseErr != nil || string(base) != string(tc.wantBase) {
				t.Fatalf("final name=%q err=%v, want=%q", base, baseErr, tc.wantBase)
			}
			quarantine, quarantineErr := os.ReadFile(quarantinePath)
			if tc.wantQuarantine {
				if quarantineErr != nil || string(quarantine) != string(tc.quarantine) {
					t.Fatalf("quarantine=%q err=%v, want retained %q", quarantine, quarantineErr, tc.quarantine)
				}
			} else if !errors.Is(quarantineErr, os.ErrNotExist) {
				t.Fatalf("quarantine was not cleaned: %v", quarantineErr)
			}
		})
	}
}

func TestRemoveUserFileCASReplaysQuarantineStates(t *testing.T) {
	expected := []byte("managed")
	tests := []struct {
		name           string
		quarantine     []byte
		setupBase      []byte
		setupSymlink   bool
		wantRemoved    bool
		wantErrChanged bool
		wantBase       []byte
		wantSymlink    bool
		wantQuarantine bool
	}{
		{name: "claim persisted before unlink", quarantine: expected, wantRemoved: true},
		{name: "matching final name is reclaimed", quarantine: expected, setupBase: expected, wantRemoved: true},
		{name: "different final name retains ownership", quarantine: expected, setupBase: []byte("user-new"), wantErrChanged: true, wantBase: []byte("user-new"), wantQuarantine: true},
		{name: "oversized final name retains ownership", quarantine: expected, setupBase: []byte("attacker-long"), wantErrChanged: true, wantBase: []byte("attacker-long"), wantQuarantine: true},
		{name: "symlink final name retains ownership", quarantine: expected, setupSymlink: true, wantErrChanged: true, wantSymlink: true, wantQuarantine: true},
		{name: "mismatched quarantine retained", quarantine: []byte("unknown"), wantErrChanged: true, wantQuarantine: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, path, quarantinePath := newUserCASReplayPaths(t)
			if err := os.WriteFile(quarantinePath, tc.quarantine, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.setupBase != nil {
				if err := os.WriteFile(path, tc.setupBase, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if tc.setupSymlink {
				if err := os.Symlink("attacker-controlled", path); err != nil {
					t.Fatal(err)
				}
			}

			removed, err := removeUserFileAndEmptyParentCAS("root", path, expected)
			if removed != tc.wantRemoved || errors.Is(err, errUserFileChanged) != tc.wantErrChanged {
				t.Fatalf("remove=(%v,%v), want removed=%v changed=%v", removed, err, tc.wantRemoved, tc.wantErrChanged)
			}
			if !tc.wantErrChanged && err != nil {
				t.Fatal(err)
			}
			if tc.wantSymlink {
				info, statErr := os.Lstat(path)
				if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("concurrent symlink was not retained: info=%v err=%v", info, statErr)
				}
			} else if tc.wantBase != nil {
				base, baseErr := os.ReadFile(path)
				if baseErr != nil || string(base) != string(tc.wantBase) {
					t.Fatalf("concurrent final name=%q err=%v, want=%q", base, baseErr, tc.wantBase)
				}
			} else if _, baseErr := os.Lstat(path); !errors.Is(baseErr, os.ErrNotExist) {
				t.Fatalf("final name was not removed: %v", baseErr)
			}
			if tc.wantErrChanged && string(tc.quarantine) == string(expected) && !strings.Contains(err.Error(), "隔离文件已保留") {
				t.Fatalf("changed error does not report retained ownership: %v", err)
			}
			quarantine, quarantineErr := os.ReadFile(quarantinePath)
			if tc.wantQuarantine {
				if quarantineErr != nil || string(quarantine) != string(tc.quarantine) {
					t.Fatalf("quarantine=%q err=%v, want retained %q", quarantine, quarantineErr, tc.quarantine)
				}
			} else if !errors.Is(quarantineErr, os.ErrNotExist) {
				t.Fatalf("quarantine was not cleaned: %v", quarantineErr)
			}
		})
	}
}
