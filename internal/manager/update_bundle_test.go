package manager

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type updateArchiveEntry struct {
	header tar.Header
	data   []byte
}

func updateArchiveEntries() []updateArchiveEntry {
	const root = "proxyscene_bundle_linux_amd64/"
	entries := []updateArchiveEntry{{header: tar.Header{Name: root, Typeflag: tar.TypeDir, Mode: 0o777}}}
	var manifest strings.Builder
	for _, name := range updateBundleFiles {
		data := []byte("test payload for " + name + "\n")
		entries = append(entries, updateArchiveEntry{tar.Header{Name: root + name, Typeflag: tar.TypeReg, Mode: 0o777, Size: int64(len(data))}, data})
		fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256(data), name)
	}
	data := []byte(manifest.String())
	return append(entries, updateArchiveEntry{tar.Header{Name: root + "bundle-manifest.sha256", Typeflag: tar.TypeReg, Mode: 0o777, Size: int64(len(data))}, data})
}

func updateTarBytes(t *testing.T, entries []updateArchiveEntry) []byte {
	t.Helper()
	var raw bytes.Buffer
	writer := tar.NewWriter(&raw)
	for _, entry := range entries {
		if err := writer.WriteHeader(&entry.header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func updateGzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func writeUpdateTestArchive(t *testing.T, compressed []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(path, compressed, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractUpdateBundleChecksExactLayoutManifestAndModes(t *testing.T) {
	for _, format := range []tar.Format{tar.FormatUSTAR, tar.FormatGNU} {
		t.Run(format.String(), func(t *testing.T) {
			entries := updateArchiveEntries()
			for i := range entries {
				entries[i].header.Format = format
			}
			raw := append(updateTarBytes(t, entries), make([]byte, 4096)...)
			archive := writeUpdateTestArchive(t, updateGzipBytes(t, raw))
			dest := filepath.Join(t.TempDir(), "unpacked")
			bundle, err := extractUpdateBundle(archive, dest, "amd64")
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{dest, bundle} {
				info, err := os.Stat(path)
				if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
					t.Fatalf("directory permissions: %s, %v, %v", path, info, err)
				}
			}
			files, err := os.ReadDir(bundle)
			if err != nil || len(files) != 11 {
				t.Fatalf("wrong output files: %v, %v", files, err)
			}
			for _, file := range files {
				info, err := file.Info()
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
					t.Fatalf("file permissions: %s, %v, %v", file.Name(), info, err)
				}
			}
		})
	}
}

func TestExtractUpdateBundleRejectsUnsafeEntriesAndCleansOnlyOwnDirectory(t *testing.T) {
	cases := map[string]func([]updateArchiveEntry) []updateArchiveEntry{
		"traversal": func(e []updateArchiveEntry) []updateArchiveEntry {
			e[1].header.Name = "proxyscene_bundle_linux_amd64/../escape"
			return e
		},
		"absolute": func(e []updateArchiveEntry) []updateArchiveEntry { e[1].header.Name = "/tmp/escape"; return e },
		"normalized alias": func(e []updateArchiveEntry) []updateArchiveEntry {
			e[1].header.Name = "proxyscene_bundle_linux_amd64/./LICENSE"
			return e
		},
		"different architecture": func(e []updateArchiveEntry) []updateArchiveEntry {
			e[0].header.Name = "proxyscene_bundle_linux_arm64/"
			return e
		},
		"duplicate file": func(e []updateArchiveEntry) []updateArchiveEntry { return append(e, e[1]) },
		"duplicate root": func(e []updateArchiveEntry) []updateArchiveEntry { return append(e, e[0]) },
		"missing root":   func(e []updateArchiveEntry) []updateArchiveEntry { return e[1:] },
		"missing file":   func(e []updateArchiveEntry) []updateArchiveEntry { return append(e[:1], e[2:]...) },
		"extra file":     func(e []updateArchiveEntry) []updateArchiveEntry { e[1].header.Name += "-extra"; return e },
		"extra directory": func(e []updateArchiveEntry) []updateArchiveEntry {
			e[1].header.Typeflag = tar.TypeDir
			e[1].header.Size = 0
			e[1].data = nil
			return e
		},
		"symlink": func(e []updateArchiveEntry) []updateArchiveEntry {
			e[1].header.Typeflag = tar.TypeSymlink
			e[1].header.Linkname = "/tmp/escape"
			e[1].header.Size = 0
			e[1].data = nil
			return e
		},
		"hardlink": func(e []updateArchiveEntry) []updateArchiveEntry {
			e[1].header.Typeflag = tar.TypeLink
			e[1].header.Linkname = "/tmp/escape"
			e[1].header.Size = 0
			e[1].data = nil
			return e
		},
		"fifo": func(e []updateArchiveEntry) []updateArchiveEntry {
			e[1].header.Typeflag = tar.TypeFifo
			e[1].header.Size = 0
			e[1].data = nil
			return e
		},
		"device": func(e []updateArchiveEntry) []updateArchiveEntry {
			e[1].header.Typeflag = tar.TypeChar
			e[1].header.Size = 0
			e[1].data = nil
			return e
		},
		"pax metadata": func(e []updateArchiveEntry) []updateArchiveEntry {
			e[1].header.PAXRecords = map[string]string{"comment": "extension"}
			return e
		},
		"gnu long-name metadata": func(e []updateArchiveEntry) []updateArchiveEntry {
			e[1].header.Format = tar.FormatGNU
			e[1].header.Name = strings.Repeat("a", 200)
			return e
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			sentinel := filepath.Join(parent, "keep")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			archive := writeUpdateTestArchive(t, updateGzipBytes(t, updateTarBytes(t, mutate(updateArchiveEntries()))))
			dest := filepath.Join(parent, "unpacked")
			if _, err := extractUpdateBundle(archive, dest, "amd64"); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatalf("failed extraction was not cleaned: %v", err)
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "untouched" {
				t.Fatalf("neighbor touched: %s, %v", data, err)
			}
		})
	}
}

func TestExtractUpdateBundleRefusesExistingDestination(t *testing.T) {
	archive := writeUpdateTestArchive(t, updateGzipBytes(t, updateTarBytes(t, updateArchiveEntries())))
	dest := t.TempDir()
	sentinel := filepath.Join(dest, "keep")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractUpdateBundle(archive, dest, "amd64"); err == nil {
		t.Fatal("existing destination accepted")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("existing directory changed: %s, %v", data, err)
	}
}

func TestExtractUpdateBundleRejectsArchiveTails(t *testing.T) {
	raw := updateTarBytes(t, updateArchiveEntries())
	cases := map[string][]byte{
		"tar after terminator": updateGzipBytes(t, append(append([]byte(nil), raw...), raw...)),
		"missing terminator":   updateGzipBytes(t, raw[:len(raw)-1024]),
		"too much padding":     updateGzipBytes(t, append(append([]byte(nil), raw...), make([]byte, 10241)...)),
		"gzip concatenation":   append(updateGzipBytes(t, raw), updateGzipBytes(t, raw)...),
		"gzip trailing data":   append(updateGzipBytes(t, raw), byte(1)),
	}
	corrupted := updateGzipBytes(t, raw)
	corrupted[len(corrupted)-8] ^= 1
	cases["gzip crc mismatch"] = corrupted
	truncated := updateGzipBytes(t, raw)
	cases["gzip truncated trailer"] = truncated[:len(truncated)-4]
	for name, compressed := range cases {
		t.Run(name, func(t *testing.T) {
			archive := writeUpdateTestArchive(t, compressed)
			if _, err := extractUpdateBundle(archive, filepath.Join(t.TempDir(), "unpacked"), "amd64"); err == nil {
				t.Fatal("invalid archive tail accepted")
			}
		})
	}
}

func TestExtractUpdateBundleRejectsOversizedHeaderBeforePayload(t *testing.T) {
	var raw bytes.Buffer
	writer := tar.NewWriter(&raw)
	entries := updateArchiveEntries()
	if err := writer.WriteHeader(&entries[0].header); err != nil {
		t.Fatal(err)
	}
	entries[1].header.Size = updateBundleMaxFile + 1
	if err := writer.WriteHeader(&entries[1].header); err != nil {
		t.Fatal(err)
	}
	archive := writeUpdateTestArchive(t, updateGzipBytes(t, raw.Bytes()))
	if _, err := extractUpdateBundle(archive, filepath.Join(t.TempDir(), "unpacked"), "amd64"); err == nil || !strings.Contains(err.Error(), "大小限制") {
		t.Fatalf("oversized header did not fail before reading payload: %v", err)
	}
}

func TestExtractUpdateBundleRejectsInvalidManifest(t *testing.T) {
	cases := map[string]func(string) string{
		"wrong checksum": func(s string) string { return strings.Repeat("0", 64) + s[64:] },
		"duplicate": func(s string) string {
			lines := strings.Split(s, "\n")
			lines[1] = lines[0]
			return strings.Join(lines, "\n")
		},
		"traversal":        func(s string) string { return strings.Replace(s, "  LICENSE\n", "  ../LICENSE\n", 1) },
		"wrong case":       func(s string) string { return strings.ToUpper(s[:64]) + s[64:] },
		"missing entry":    func(s string) string { return s[strings.IndexByte(s, '\n')+1:] },
		"extra entry":      func(s string) string { return s + s[:strings.IndexByte(s, '\n')+1] },
		"no final newline": func(s string) string { return strings.TrimSuffix(s, "\n") },
		"too large":        func(s string) string { return s + strings.Repeat("x", 8193) },
		"bad separator":    func(s string) string { return s[:64] + " *" + s[66:] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			entries := updateArchiveEntries()
			last := &entries[len(entries)-1]
			last.data = []byte(mutate(string(last.data)))
			last.header.Size = int64(len(last.data))
			archive := writeUpdateTestArchive(t, updateGzipBytes(t, updateTarBytes(t, entries)))
			if _, err := extractUpdateBundle(archive, filepath.Join(t.TempDir(), "unpacked"), "amd64"); err == nil {
				t.Fatal("bad manifest accepted")
			}
		})
	}
}

func buildUpdateIdentityFixture(t *testing.T) (string, string) {
	t.Helper()
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	if _, _, err := updateELFArchitecture(arch); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":                      "module proxyscene\n\ngo 1.23\n",
		"internal/manager/manager.go": "package manager\nvar Version = \"dev\"\nvar Commit = \"\"\n",
		"cmd/proxyscene/main.go":      "package main\nimport \"proxyscene/internal/manager\"\nfunc main() { panic(manager.Version + manager.Commit) }\n",
	}
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "proxyscene")
	flags := "-s -w -buildid= -X proxyscene/internal/manager.Version=0.10.0 -X proxyscene/internal/manager.Commit=0123456789012345678901234567890123456789"
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goBinary, "build", "-trimpath", "-buildvcs=false", "-ldflags="+flags, "-o", out, "./cmd/proxyscene")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOENV=off", "GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local", "GOOS=linux", "GOARCH="+runtime.GOARCH, "GOARM=7", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building harmless identity fixture: %v\n%s", err, output)
	}
	return out, arch
}

func TestValidateUpdateBundleIdentityDoesNotExecutePrograms(t *testing.T) {
	path, arch := buildUpdateIdentityFixture(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(bundle, "xray"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateUpdateBundleIdentity(bundle, arch); err != nil {
		t.Fatal(err)
	}
	otherArch := "arm64"
	if arch == otherArch {
		otherArch = "amd64"
	}
	if err := validateUpdateManagerIdentity(path, otherArch); err == nil {
		t.Fatal("wrong architecture accepted")
	}
	badPath := filepath.Join(bundle, "bad-module")
	badModule := bytes.ReplaceAll(data, []byte("proxyscene/cmd/proxyscene"), []byte("alienpath/cmd/alienpaths"))
	if bytes.Equal(data, badModule) {
		t.Fatal("fixture has no module path")
	}
	if err := os.WriteFile(badPath, badModule, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateUpdateManagerIdentity(badPath, arch); err == nil {
		t.Fatal("wrong module identity accepted")
	}
	for name, invalid := range map[string][]byte{
		"wrong OS":            bytes.ReplaceAll(data, []byte("GOOS=linux"), []byte("GOOS=other")),
		"missing Go metadata": []byte("not a Go executable"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(badPath, invalid, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := validateUpdateManagerIdentity(badPath, arch); err == nil {
				t.Fatal("invalid platform or build metadata accepted")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(bundle, "xray"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateUpdateBundleIdentity(bundle, arch); err == nil {
		t.Fatal("non-ELF Xray accepted")
	}
}

func TestUpdateInstallerEnvironmentIsStrictAllowlist(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CoreDir = "/var/lib/proxyscene-update-test"
	cfg.InstallBin = "/usr/local/bin/proxyscene"
	cfg.SystemdService = "proxyscene-test.service"
	cfg.RestoreService = "proxyscene-test-restore.service"
	for _, name := range []string{"BASH_ENV", "LD_PRELOAD", "PROXYSCENE_SKIP_MANAGER_INIT", "PROXYSCENE_MANAGER_VERSION", "PROXYSCENE_TESTING", "PROXYSCENE_INHERITED_INSTALL_LOCK_FD", "GITHUB_TOKEN", "HTTPS_PROXY", "PROXYSCENE_EXPECTED_MANAGER_SHA256"} {
		t.Setenv(name, "must-not-leak")
	}
	want := []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8",
		"PROXYSCENE_MANAGER_DIR=" + cfg.CoreDir,
		"PROXYSCENE_SWITCH_BIN=" + cfg.InstallBin,
		"PROXYSCENE_SYSTEMD_SERVICE_NAME=" + cfg.SystemdService,
		"PROXYSCENE_BOOT_RESTORE_SERVICE_NAME=" + cfg.RestoreService,
	}
	if got := updateInstallerEnv(cfg); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected installer environment: %v", got)
	}
}

func TestRunUpdateInstallerUsesOfflineArgumentsAndIgnoresBashEnv(t *testing.T) {
	if err := validatePrivilegedExecutable("/usr/bin/bash", "test bash"); err != nil {
		t.Skip(err)
	}
	bundle := t.TempDir()
	marker := filepath.Join(bundle, "environment-injected")
	injected := filepath.Join(bundle, "bash-env")
	if err := os.WriteFile(injected, []byte("touch '"+marker+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", injected)
	t.Setenv("PROXYSCENE_SKIP_MANAGER_INIT", "1")
	t.Setenv("PROXYSCENE_TESTING", "1")
	t.Setenv("PROXYSCENE_INHERITED_INSTALL_LOCK_FD", "9")
	const script = `set -eu
[[ $# == 3 && "$1" == --offline && "$2" == --expected-manager-sha256 ]]
[[ "$3" =~ ^[0-9a-f]{64}$ ]]
[[ "$PATH" == /usr/sbin:/usr/bin:/sbin:/bin && "$LANG" == C.UTF-8 ]]
[[ -z "${BASH_ENV-}${PROXYSCENE_SKIP_MANAGER_INIT-}${PROXYSCENE_TESTING-}${PROXYSCENE_INHERITED_INSTALL_LOCK_FD-}" ]]
[[ "$PROXYSCENE_MANAGER_DIR" == /opt/proxyscene ]]
printf '%s\n' "$PWD" > installer-workdir
`
	if err := os.WriteFile(filepath.Join(bundle, "install.sh"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{CoreDir: "/opt/proxyscene", InstallBin: "/usr/local/bin/proxyscene", SystemdService: "proxyscene.service", RestoreService: "proxyscene-restore.service"}
	expected := sha256.Sum256([]byte("previous manager"))
	if err := runUpdateInstaller(bundle, cfg, hex.EncodeToString(expected[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("BASH_ENV executed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(bundle, "installer-workdir")); err != nil || string(data) != bundle+"\n" {
		t.Fatalf("wrong installer cwd: %q, %v", data, err)
	}
	if err := runUpdateInstaller(bundle, cfg, "not-a-hash"); err == nil {
		t.Fatal("bad old-manager digest accepted")
	}
	if err := os.WriteFile(filepath.Join(bundle, "install.sh"), []byte("exit 23\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runUpdateInstaller(bundle, cfg, hex.EncodeToString(expected[:])); err == nil || !strings.Contains(err.Error(), "exit status 23") {
		t.Fatalf("installer failure lost: %v", err)
	}
}
