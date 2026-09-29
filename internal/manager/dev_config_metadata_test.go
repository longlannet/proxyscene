package manager

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func metadataTestAssert(t *testing.T, path string, uid, gid int, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != uid || int(stat.Gid) != gid || info.Mode().Perm() != mode {
		t.Fatalf("metadata uid=%d gid=%d mode=%04o, want %d:%d %04o", stat.Uid, stat.Gid, info.Mode().Perm(), uid, gid, mode)
	}
}

func metadataTestACL(t *testing.T, path, name string) []byte {
	t.Helper()
	// Linux POSIX ACL xattr v2, with a named reader distinct from the owner.
	entries := [][3]uint32{{0x01, 6, ^uint32(0)}, {0x02, 4, uint32(os.Geteuid() + 1)}, {0x04, 4, ^uint32(0)}, {0x10, 4, ^uint32(0)}, {0x20, 0, ^uint32(0)}}
	acl := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(acl, 2)
	for i, entry := range entries {
		binary.LittleEndian.PutUint16(acl[4+i*8:], uint16(entry[0]))
		binary.LittleEndian.PutUint16(acl[6+i*8:], uint16(entry[1]))
		binary.LittleEndian.PutUint32(acl[8+i*8:], entry[2])
	}
	if err := unix.Setxattr(path, name, acl, 0); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) {
			t.Skip("test filesystem does not support POSIX ACLs")
		}
		t.Fatal(err)
	}
	return acl
}

func TestDevConfigMetadataPreservesGitAndNPMPrivateGroup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to test a private group different from the account primary group")
	}
	for _, tool := range []string{"git", "npm"} {
		t.Run(tool, func(t *testing.T) {
			user, identity := realDevConfigFixture(t)
			identity.UID, identity.GID = 65534, 65534
			if err := os.Chmod(filepath.Dir(identity.Home), 0o711); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(identity.Home, identity.UID, identity.GID); err != nil {
				t.Fatal(err)
			}
			devLookupUserIdentity = func(string) (localUserIdentity, error) {
				return localUserIdentity{Name: user, UID: identity.UID, GID: identity.GID, UIDText: strconv.Itoa(identity.UID), GIDText: strconv.Itoa(identity.GID), Home: identity.Home}, nil
			}
			name, original := ".npmrc", "token=private-value\n"
			if tool == "git" {
				requireDevGit(t)
				name, original = ".gitconfig", "[private]\n\ttoken = private-value\n"
			}
			path := filepath.Join(identity.Home, name)
			devIOWrite(t, path, original)
			if err := os.Chown(path, identity.UID, 65533); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
			proxy := "http://127.0.0.1:7891"
			if tool == "git" {
				if err := mutateDevGitConfig(user, identity, devGitConfigHome, nil, "--replace-all", "--", "http.proxy", proxy); err != nil {
					t.Fatal(err)
				}
			} else if err := mutateDevNPMConfig(user, identity, "proxy", nil, &proxy); err != nil {
				t.Fatal(err)
			}
			metadataTestAssert(t, path, identity.UID, 65533, 0o640)
			if tool == "git" {
				if err := mutateDevGitConfig(user, identity, devGitConfigHome, []string{proxy}, "--unset-all", "--", "http.proxy"); err != nil {
					t.Fatal(err)
				}
			} else if err := mutateDevNPMConfig(user, identity, "proxy", &proxy, nil); err != nil {
				t.Fatal(err)
			}
			metadataTestAssert(t, path, identity.UID, 65533, 0o640)
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(data, []byte("private-value")) {
				t.Fatalf("unrelated secret lost: %v", err)
			}
		})
	}
}

func TestDevConfigMetadataRejectsExistingACLWithoutMutation(t *testing.T) {
	for _, tool := range []string{"git", "npm"} {
		t.Run(tool, func(t *testing.T) {
			user, identity := realDevConfigFixture(t)
			name, original := ".npmrc", "token=private-value\n"
			if tool == "git" {
				requireDevGit(t)
				name, original = ".gitconfig", "[private]\n\ttoken = private-value\n"
			}
			path := filepath.Join(identity.Home, name)
			devIOWrite(t, path, original)
			acl := metadataTestACL(t, path, "system.posix_acl_access")
			proxy := "http://127.0.0.1:7891"
			var err error
			if tool == "git" {
				err = mutateDevGitConfig(user, identity, devGitConfigHome, nil, "--replace-all", "--", "http.proxy", proxy)
			} else {
				err = mutateDevNPMConfig(user, identity, "proxy", nil, &proxy)
			}
			if err == nil {
				t.Fatal("ACL-bearing configuration was replaced")
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || string(got) != original {
				t.Fatalf("refused configuration changed: %v", readErr)
			}
			gotACL := make([]byte, len(acl))
			n, err := unix.Getxattr(path, "system.posix_acl_access", gotACL)
			if err != nil || !bytes.Equal(gotACL[:n], acl) {
				t.Fatalf("refused ACL changed: %v", err)
			}
			entries, err := os.ReadDir(identity.Home)
			if err != nil || len(entries) != 1 || entries[0].Name() != name {
				t.Fatalf("refused update left temporary files: %v, %v", entries, err)
			}
		})
	}
}

func TestDevConfigMetadataInheritedACLIsNotActivated(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", exists), func(t *testing.T) {
			user, identity := realDevConfigFixture(t)
			path := filepath.Join(identity.Home, ".npmrc")
			wantMode := os.FileMode(0o600)
			if exists {
				devIOWrite(t, path, "token=private-value\n")
				wantMode = 0o640
				if err := os.Chmod(path, wantMode); err != nil {
					t.Fatal(err)
				}
			}
			metadataTestACL(t, identity.Home, "system.posix_acl_default")
			proxy := "http://127.0.0.1:7891"
			if err := mutateDevNPMConfig(user, identity, "proxy", nil, &proxy); err != nil {
				t.Fatal(err)
			}
			metadataTestAssert(t, path, identity.UID, identity.GID, wantMode)
			if _, err := unix.Getxattr(path, "system.posix_acl_access", nil); !errors.Is(err, unix.ENODATA) {
				t.Fatalf("inherited named grant survived chmod: %v", err)
			}
		})
	}
}

func TestDevConfigMetadataConcurrentChangeIsRetained(t *testing.T) {
	for _, change := range []string{"mode", "group", "owner", "acl", "same-bytes-new-inode"} {
		t.Run(change, func(t *testing.T) {
			if (change == "group" || change == "owner") && os.Geteuid() != 0 {
				t.Skip("requires root to change numeric owner/group")
			}
			user, identity := realDevConfigFixture(t)
			path := filepath.Join(identity.Home, ".npmrc")
			original := []byte("token=private-value\n")
			devIOWrite(t, path, string(original))
			before, exists, metadata, err := readDevConfig(user, identity, path)
			if err != nil {
				t.Fatal(err)
			}
			quarantine := filepath.Join(identity.Home, userFileQuarantineName)
			old := userFileCASAfterQuarantine
			t.Cleanup(func() { userFileCASAfterQuarantine = old })
			userFileCASAfterQuarantine = func(string) {
				var err error
				switch change {
				case "mode":
					err = os.Chmod(quarantine, 0o640)
				case "group":
					err = os.Chown(quarantine, -1, 65533)
				case "owner":
					err = os.Chown(quarantine, 65534, -1)
				case "acl":
					metadataTestACL(t, quarantine, "system.posix_acl_access")
				case "same-bytes-new-inode":
					replacement := filepath.Join(identity.Home, "replacement")
					devIOWrite(t, replacement, string(original))
					err = os.Rename(replacement, quarantine)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			err = commitDevConfig(user, identity, path, before, []byte("proxy=http://127.0.0.1:7891\n"), exists, metadata)
			if !errors.Is(err, errUserFileChanged) {
				t.Fatalf("concurrent metadata change accepted: %v", err)
			}
			got, err := os.ReadFile(quarantine)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("concurrent file lost: %v", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("replacement published despite changed metadata: %v", err)
			}
			entries, err := os.ReadDir(identity.Home)
			if err != nil || len(entries) != 1 || entries[0].Name() != userFileQuarantineName {
				t.Fatalf("unexpected remaining secret copies: %v, %v", entries, err)
			}
		})
	}
}

func TestDevConfigMetadataQuarantineReplay(t *testing.T) {
	for _, state := range []string{"claimed", "committed", "changed-quarantine", "changed-quarantine-group", "changed-final-mode", "changed-final-group", "changed-final-acl"} {
		t.Run(state, func(t *testing.T) {
			if (state == "changed-quarantine-group" || state == "changed-final-group") && os.Geteuid() != 0 {
				t.Skip("requires root to change a numeric group")
			}
			user, identity := realDevConfigFixture(t)
			path := filepath.Join(identity.Home, ".npmrc")
			devIOWrite(t, path, "token=private-value\n")
			if os.Geteuid() == 0 {
				if err := os.Chown(path, identity.UID, 65533); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
			before, exists, metadata, err := readDevConfig(user, identity, path)
			if err != nil {
				t.Fatal(err)
			}
			after := []byte("token=private-value\nproxy=http://127.0.0.1:7891\n")
			quarantine := filepath.Join(identity.Home, userFileQuarantineName)
			if err := os.Rename(path, quarantine); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "committed", "changed-final-mode", "changed-final-group", "changed-final-acl":
				devIOWrite(t, path, string(after))
				if state != "changed-final-group" {
					if err := os.Chown(path, metadata.UID, metadata.GID); err != nil {
						t.Fatal(err)
					}
				}
				if state != "changed-final-mode" {
					if err := os.Chmod(path, metadata.Mode); err != nil {
						t.Fatal(err)
					}
				}
				if state == "changed-final-acl" {
					metadataTestACL(t, path, "system.posix_acl_access")
				}
			case "changed-quarantine-group":
				if err := os.Chown(quarantine, -1, identity.GID); err != nil {
					t.Fatal(err)
				}
			case "changed-quarantine":
				if err := os.Chmod(quarantine, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err = commitDevConfig(user, identity, path, before, after, exists, metadata)
			if state == "claimed" || state == "committed" {
				if err != nil {
					t.Fatal(err)
				}
				metadataTestAssert(t, path, metadata.UID, metadata.GID, 0o640)
				if _, err := os.Lstat(quarantine); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("successful replay retained quarantine: %v", err)
				}
			} else {
				if !errors.Is(err, errUserFileChanged) {
					t.Fatalf("metadata mismatch accepted on replay: %v", err)
				}
				got, err := os.ReadFile(quarantine)
				if err != nil || !bytes.Equal(got, before) {
					t.Fatalf("failed replay lost original inode: %v", err)
				}
			}
		})
	}
}

func TestDevConfigMetadataPrepublishFailureCleansPrivateTemporary(t *testing.T) {
	dir := t.TempDir()
	metadataTestACL(t, dir, "system.posix_acl_default")
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	failure := errors.New("concurrent metadata edit before publication")
	checked := false
	err = writeUserFileAtomicAtModeChecked(fd, ".npmrc", []byte("token=private-value\n"), 0o640, os.Geteuid(), os.Getegid(), true, func() error {
		checked = true
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("prepared transaction entries: %v, %v", entries, err)
		}
		tmp := filepath.Join(dir, entries[0].Name())
		metadataTestAssert(t, tmp, os.Geteuid(), os.Getegid(), 0o600)
		if _, err := unix.Getxattr(tmp, "system.posix_acl_access", nil); !errors.Is(err, unix.ENODATA) {
			t.Fatalf("prepared secret inherited a named ACL grant: %v", err)
		}
		return failure
	})
	if !checked || !errors.Is(err, failure) {
		t.Fatalf("did not fail at the requested publication boundary: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed transaction left secret copies: %v, %v", entries, err)
	}
}

func TestDevConfigMetadataInterruptedClaimCannotBecomeNewFile(t *testing.T) {
	for _, when := range []string{"before-read", "before-create"} {
		t.Run(when, func(t *testing.T) {
			user, identity := realDevConfigFixture(t)
			path := filepath.Join(identity.Home, ".npmrc")
			before, exists, metadata, err := readDevConfig(user, identity, path)
			if err != nil || exists {
				t.Fatalf("initial missing configuration: %v", err)
			}
			quarantine := filepath.Join(identity.Home, userFileQuarantineName)
			original := "token=private-value\n"
			devIOWrite(t, quarantine, original)
			if when == "before-read" {
				_, _, _, err = readDevConfig(user, identity, path)
			} else {
				err = commitDevConfig(user, identity, path, before, []byte("proxy=http://127.0.0.1:7891\n"), exists, metadata)
			}
			if !errors.Is(err, errUserFileChanged) {
				t.Fatalf("interrupted claim became a new file: %v", err)
			}
			got, err := os.ReadFile(quarantine)
			if err != nil || string(got) != original {
				t.Fatalf("interrupted configuration lost: %v", err)
			}
			entries, err := os.ReadDir(identity.Home)
			if err != nil || len(entries) != 1 || entries[0].Name() != userFileQuarantineName {
				t.Fatalf("refused creation left new files: %v, %v", entries, err)
			}
		})
	}
}
