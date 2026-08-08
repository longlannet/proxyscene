package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	globalProxyJournalVersion         = 1
	maxGlobalProxyJournalBytes  int64 = 4 << 20
	maxGlobalProxyArtifactBytes       = 1 << 20

	globalProxyPhasePrepared    = "prepared"
	globalProxyPhaseActive      = "active"
	globalProxyPhaseRestoring   = "restoring"
	globalProxyQuarantineSuffix = ".proxyscene-quarantine"
	maxGlobalProxyCASAttempts   = 8

	// syscall.Fchown accepts int on every supported target. Keep journal IDs in
	// the common signed 32-bit range so a crafted uint32 cannot become -1 on 386.
	maxGlobalProxyOwnershipID = uint32(1<<31 - 1)
)

var (
	globalProxyProfilePath = func() string {
		return "/etc/profile.d/proxyscene-global-proxy.sh"
	}
	globalProxyAPTPath = func() string {
		return "/etc/apt/apt.conf.d/99proxyscene-global-proxy"
	}
	globalProxyReadArtifact       = readGlobalProxyArtifactStateNoFollow
	globalProxyWriteArtifact      = writeGlobalProxyArtifactCAS
	globalProxyRemoveArtifact     = removeGlobalProxyArtifactCAS
	globalProxySyncArtifactDir    = syncGlobalProxyArtifactDir
	globalProxyWriteJournal       = writeFileAtomic
	globalProxyCASAfterQuarantine = func(string) {}

	errGlobalProxyArtifactChanged = errors.New("全局代理目标在读取后已被修改")
)

type globalProxyJournal struct {
	Version    int                           `json:"version"`
	Generation uint64                        `json:"generation"`
	Phase      string                        `json:"phase"`
	Artifacts  []*globalProxyJournalArtifact `json:"artifacts"`
}

type globalProxyJournalArtifact struct {
	Path                  string `json:"path"`
	OriginalPresent       bool   `json:"original_present"`
	OriginalContent       []byte `json:"original_content,omitempty"`
	OriginalMode          uint32 `json:"original_mode,omitempty"`
	OriginalUID           uint32 `json:"original_uid,omitempty"`
	OriginalGID           uint32 `json:"original_gid,omitempty"`
	ManagedContent        []byte `json:"managed_content,omitempty"`
	ManagedMode           uint32 `json:"managed_mode,omitempty"`
	ManagedUID            uint32 `json:"managed_uid,omitempty"`
	ManagedGID            uint32 `json:"managed_gid,omitempty"`
	PendingManagedContent []byte `json:"pending_managed_content,omitempty"`
	PendingManagedMode    uint32 `json:"pending_managed_mode,omitempty"`
	PendingManagedUID     uint32 `json:"pending_managed_uid,omitempty"`
	PendingManagedGID     uint32 `json:"pending_managed_gid,omitempty"`
}

type globalProxyArtifactState struct {
	present bool
	content []byte
	mode    os.FileMode
	uid     uint32
	gid     uint32
}

func (a *App) globalProxyJournalPath() string {
	return filepath.Join(a.cfg.CoreDir, "global-proxy-journal.json")
}

func (a *App) globalProxyJournalBackupPath() string {
	return a.globalProxyJournalPath() + ".bak"
}

func globalProxyArtifactPaths() []string {
	return []string{globalProxyProfilePath(), globalProxyAPTPath()}
}

func globalProxyDesiredArtifacts(cfg Config) map[string][]byte {
	profile, apt := globalProxyArtifactContents(cfg.HTTPAddr(SceneGlobal), cfg.GlobalSocksAddr())
	return map[string][]byte{
		globalProxyProfilePath(): profile,
		globalProxyAPTPath():     apt,
	}
}

func globalProxyArtifactContents(httpProxy, socksProxy string) ([]byte, []byte) {
	profile := fmt.Sprintf("# 由 proxyscene 管理\nexport http_proxy=%q\nexport https_proxy=%q\nexport all_proxy=%q\nexport HTTP_PROXY=%q\nexport HTTPS_PROXY=%q\nexport ALL_PROXY=%q\n", httpProxy, httpProxy, socksProxy, httpProxy, httpProxy, socksProxy)
	apt := fmt.Sprintf("// 由 proxyscene 管理\nAcquire::http::Proxy %q;\nAcquire::https::Proxy %q;\n", httpProxy, httpProxy)
	return []byte(profile), []byte(apt)
}

func parseCanonicalQuotedAssignment(line, prefix, suffix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, suffix) {
		return "", false
	}
	quoted := strings.TrimSuffix(strings.TrimPrefix(line, prefix), suffix)
	value, err := strconv.Unquote(quoted)
	return value, err == nil && strconv.Quote(value) == quoted
}

func parseLegacyGlobalProxyURL(raw, scheme string) (string, int, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != scheme || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", 0, false
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil || net.ParseIP(host) == nil {
		return "", 0, false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, false
	}
	return host, port, true
}

func parseLegacyGlobalProxyArtifacts(profileRaw, aptRaw []byte) bool {
	profileLines := strings.Split(string(profileRaw), "\n")
	if len(profileLines) != 8 || profileLines[0] != "# 由 proxyscene 管理" || profileLines[7] != "" {
		return false
	}
	prefixes := []string{
		"export http_proxy=", "export https_proxy=", "export all_proxy=",
		"export HTTP_PROXY=", "export HTTPS_PROXY=", "export ALL_PROXY=",
	}
	values := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		value, ok := parseCanonicalQuotedAssignment(profileLines[i+1], prefix, "")
		if !ok {
			return false
		}
		values[i] = value
	}
	httpProxy, socksProxy := values[0], values[2]
	if values[1] != httpProxy || values[3] != httpProxy || values[4] != httpProxy || values[5] != socksProxy {
		return false
	}
	httpHost, _, httpOK := parseLegacyGlobalProxyURL(httpProxy, "http")
	socksHost, _, socksOK := parseLegacyGlobalProxyURL(socksProxy, "socks5h")
	if !httpOK || !socksOK || httpHost != socksHost {
		return false
	}

	aptLines := strings.Split(string(aptRaw), "\n")
	if len(aptLines) != 4 || aptLines[0] != "// 由 proxyscene 管理" || aptLines[3] != "" {
		return false
	}
	aptHTTP, okHTTP := parseCanonicalQuotedAssignment(aptLines[1], "Acquire::http::Proxy ", ";")
	aptHTTPS, okHTTPS := parseCanonicalQuotedAssignment(aptLines[2], "Acquire::https::Proxy ", ";")
	if !okHTTP || !okHTTPS || aptHTTP != httpProxy || aptHTTPS != httpProxy {
		return false
	}
	wantProfile, wantAPT := globalProxyArtifactContents(httpProxy, socksProxy)
	return bytes.Equal(profileRaw, wantProfile) && bytes.Equal(aptRaw, wantAPT)
}

func readGlobalProxyArtifact(path string) (globalProxyArtifactState, error) {
	return globalProxyReadArtifact(path, maxGlobalProxyArtifactBytes)
}

func openGlobalProxyArtifactDir(path string, create bool) (string, int, error) {
	cleanPath := filepath.Clean(path)
	dir := filepath.Dir(cleanPath)
	var err error
	if create {
		err = ensurePublicDir(dir)
	} else {
		err = validateNoSymlinkComponents(dir, false)
	}
	if err != nil {
		return "", -1, err
	}
	dirFD, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return cleanPath, -1, os.ErrNotExist
		}
		return "", -1, err
	}
	if err := validatePrivilegedDirFD(dir, dirFD); err != nil {
		_ = syscall.Close(dirFD)
		return "", -1, err
	}
	return cleanPath, dirFD, nil
}

func readGlobalProxyArtifactAt(dirFD int, name, path string, max int64) (globalProxyArtifactState, error) {
	fd, err := syscall.Openat(dirFD, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return globalProxyArtifactState{}, nil
		}
		return globalProxyArtifactState{}, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return globalProxyArtifactState{}, err
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return globalProxyArtifactState{}, fmt.Errorf("全局代理目标不是普通文件：%s", path)
	}
	if stat.Uid > maxGlobalProxyOwnershipID || stat.Gid > maxGlobalProxyOwnershipID {
		return globalProxyArtifactState{}, fmt.Errorf("全局代理目标属主无法安全记录：%s", path)
	}
	if stat.Size < 0 || stat.Size > max {
		return globalProxyArtifactState{}, fmt.Errorf("全局代理目标超过大小限制 %d 字节：%s", max, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return globalProxyArtifactState{}, err
	}
	if int64(len(data)) > max {
		return globalProxyArtifactState{}, fmt.Errorf("全局代理目标超过大小限制 %d 字节：%s", max, path)
	}
	return globalProxyArtifactState{
		present: true,
		content: data,
		mode:    os.FileMode(stat.Mode).Perm(),
		uid:     stat.Uid,
		gid:     stat.Gid,
	}, nil
}

func readGlobalProxyArtifactStateNoFollow(path string, max int64) (globalProxyArtifactState, error) {
	cleanPath, dirFD, err := openGlobalProxyArtifactDir(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return globalProxyArtifactState{}, nil
	}
	if err != nil {
		return globalProxyArtifactState{}, err
	}
	defer syscall.Close(dirFD)
	return readGlobalProxyArtifactAt(dirFD, filepath.Base(cleanPath), cleanPath, max)
}

func globalProxyArtifactStatesEqual(left, right globalProxyArtifactState) bool {
	if left.present != right.present {
		return false
	}
	return !left.present || (bytes.Equal(left.content, right.content) &&
		left.mode.Perm() == right.mode.Perm() && left.uid == right.uid && left.gid == right.gid)
}

func globalProxyStateMatchesAny(current globalProxyArtifactState, expected []globalProxyArtifactState) bool {
	for _, candidate := range expected {
		if globalProxyArtifactStatesEqual(current, candidate) {
			return true
		}
	}
	return false
}

func globalProxyOriginalArtifactState(artifact *globalProxyJournalArtifact) globalProxyArtifactState {
	if !artifact.OriginalPresent {
		return globalProxyArtifactState{}
	}
	return globalProxyArtifactState{
		present: true,
		content: artifact.OriginalContent,
		mode:    os.FileMode(artifact.OriginalMode),
		uid:     artifact.OriginalUID,
		gid:     artifact.OriginalGID,
	}
}

func globalProxyManagedArtifactState(content []byte, mode, uid, gid uint32) (globalProxyArtifactState, bool) {
	if len(content) == 0 {
		return globalProxyArtifactState{}, false
	}
	return globalProxyArtifactState{present: true, content: content, mode: os.FileMode(mode), uid: uid, gid: gid}, true
}

func globalProxyApplyExpectedStates(artifact *globalProxyJournalArtifact) []globalProxyArtifactState {
	if managed, ok := globalProxyManagedArtifactState(artifact.ManagedContent, artifact.ManagedMode, artifact.ManagedUID, artifact.ManagedGID); ok {
		return []globalProxyArtifactState{managed}
	}
	return []globalProxyArtifactState{globalProxyOriginalArtifactState(artifact)}
}

func globalProxyRestoreExpectedStates(artifact *globalProxyJournalArtifact) []globalProxyArtifactState {
	// A prepared first apply can crash after claiming the original inode but
	// before installing PendingManagedContent. Restore must recognize that
	// quarantine as journal-owned as well as managed and pending states.
	expected := []globalProxyArtifactState{globalProxyOriginalArtifactState(artifact)}
	if managed, ok := globalProxyManagedArtifactState(artifact.ManagedContent, artifact.ManagedMode, artifact.ManagedUID, artifact.ManagedGID); ok {
		expected = append(expected, managed)
	}
	if pending, ok := globalProxyManagedArtifactState(artifact.PendingManagedContent, artifact.PendingManagedMode, artifact.PendingManagedUID, artifact.PendingManagedGID); ok {
		expected = append(expected, pending)
	}
	return expected
}

func globalProxyPendingArtifactState(artifact *globalProxyJournalArtifact) globalProxyArtifactState {
	return globalProxyArtifactState{
		present: true,
		content: artifact.PendingManagedContent,
		mode:    os.FileMode(artifact.PendingManagedMode),
		uid:     artifact.PendingManagedUID,
		gid:     artifact.PendingManagedGID,
	}
}

func globalProxyStateMatchesOriginal(current globalProxyArtifactState, artifact *globalProxyJournalArtifact) bool {
	if current.present != artifact.OriginalPresent {
		return false
	}
	return !current.present || (bytes.Equal(current.content, artifact.OriginalContent) &&
		current.mode.Perm() == os.FileMode(artifact.OriginalMode) &&
		current.uid == artifact.OriginalUID && current.gid == artifact.OriginalGID)
}

func globalProxyStateMatchesManaged(current globalProxyArtifactState, content []byte, mode, uid, gid uint32) bool {
	return current.present && len(content) > 0 && bytes.Equal(current.content, content) &&
		current.mode.Perm() == os.FileMode(mode) && current.uid == uid && current.gid == gid
}

func restoreGlobalProxyArtifact(artifact *globalProxyJournalArtifact, expected []globalProxyArtifactState) error {
	if !artifact.OriginalPresent {
		_, err := globalProxyRemoveArtifact(artifact.Path, expected)
		return err
	}
	return globalProxyWriteArtifact(artifact.Path, expected, globalProxyOriginalArtifactState(artifact))
}

func globalProxyQuarantineName(base string) string {
	return "." + base + globalProxyQuarantineSuffix
}

func writeGlobalProxyArtifactAtNoReplace(dirFD int, base string, desired globalProxyArtifactState) error {
	tmpName, tmpFD, err := createTempFileAt(dirFD, base, desired.mode)
	if err != nil {
		return err
	}
	committed := false
	f := os.NewFile(uintptr(tmpFD), tmpName)
	defer func() {
		if !committed {
			_ = syscall.Unlinkat(dirFD, tmpName)
		}
	}()
	n, writeErr := f.Write(desired.content)
	if writeErr == nil && n != len(desired.content) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		_ = f.Close()
		return writeErr
	}
	if err := syscall.Fchown(int(f.Fd()), int(desired.uid), int(desired.gid)); err != nil {
		_ = f.Close()
		return err
	}
	if err := syscall.Fchmod(int(f.Fd()), uint32(desired.mode.Perm())); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := unix.Renameat2(dirFD, tmpName, dirFD, base, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := syscall.Fsync(dirFD); err != nil {
		return fmt.Errorf("同步全局代理目标目录失败：%w", err)
	}
	committed = true
	return nil
}

func removeGlobalProxyQuarantine(dirFD int, quarantine, quarantinePath string) error {
	if err := syscall.Unlinkat(dirFD, quarantine); err != nil {
		return fmt.Errorf("删除已确认的全局代理隔离文件失败，文件已保留：%s：%w", quarantinePath, err)
	}
	if err := syscall.Fsync(dirFD); err != nil {
		return fmt.Errorf("同步全局代理隔离文件删除失败：%s：%w", quarantinePath, err)
	}
	return nil
}

func restoreGlobalProxyQuarantine(dirFD int, base, quarantine, quarantinePath string) error {
	err := unix.Renameat2(dirFD, quarantine, dirFD, base, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("全局代理目标已被并发创建，隔离文件已保留：%s", quarantinePath)
	}
	if err != nil {
		return fmt.Errorf("恢复全局代理隔离文件失败，文件已保留：%s：%w", quarantinePath, err)
	}
	if err := syscall.Fsync(dirFD); err != nil {
		return fmt.Errorf("同步全局代理隔离文件恢复失败：%s：%w", quarantinePath, err)
	}
	return nil
}

func globalProxyQuarantineMismatchError(dirFD int, base, quarantine, path string, final globalProxyArtifactState, readErr error) error {
	quarantinePath := filepath.Join(filepath.Dir(path), quarantine)
	detail := fmt.Errorf("全局代理隔离文件与 journal 预期内容或元数据不一致：%s", quarantinePath)
	if readErr != nil {
		detail = fmt.Errorf("无法安全读取全局代理隔离文件，已拒绝自动操作：%s：%w", quarantinePath, readErr)
	}
	if final.present || readErr != nil {
		return errors.Join(errGlobalProxyArtifactChanged, detail)
	}
	return errors.Join(errGlobalProxyArtifactChanged, detail, restoreGlobalProxyQuarantine(dirFD, base, quarantine, quarantinePath))
}

func writeGlobalProxyArtifactCAS(path string, expected []globalProxyArtifactState, desired globalProxyArtifactState) error {
	if !desired.present {
		return fmt.Errorf("全局代理 CAS 写入目标不能为空：%s", path)
	}
	cleanPath, dirFD, err := openGlobalProxyArtifactDir(path, true)
	if err != nil {
		return err
	}
	defer syscall.Close(dirFD)
	base := filepath.Base(cleanPath)
	quarantine := globalProxyQuarantineName(base)
	quarantinePath := filepath.Join(filepath.Dir(cleanPath), quarantine)
	claimedInCall := false

	for attempt := 0; attempt < maxGlobalProxyCASAttempts; attempt++ {
		claimed, readErr := readGlobalProxyArtifactAt(dirFD, quarantine, quarantinePath, maxGlobalProxyArtifactBytes)
		if readErr != nil {
			final, finalErr := readGlobalProxyArtifactAt(dirFD, base, cleanPath, maxGlobalProxyArtifactBytes)
			return errors.Join(globalProxyQuarantineMismatchError(dirFD, base, quarantine, cleanPath, final, readErr), finalErr)
		}
		if claimed.present {
			final, finalErr := readGlobalProxyArtifactAt(dirFD, base, cleanPath, maxGlobalProxyArtifactBytes)
			if finalErr != nil {
				return errors.Join(errGlobalProxyArtifactChanged, fmt.Errorf("无法安全读取并发全局代理目标，隔离文件已保留：%s：%w", cleanPath, finalErr))
			}
			if !globalProxyStateMatchesAny(claimed, expected) {
				return globalProxyQuarantineMismatchError(dirFD, base, quarantine, cleanPath, final, nil)
			}
			if final.present {
				if !claimedInCall && globalProxyArtifactStatesEqual(final, desired) {
					return removeGlobalProxyQuarantine(dirFD, quarantine, quarantinePath)
				}
				if !claimedInCall && globalProxyStateMatchesAny(final, expected) {
					if err := removeGlobalProxyQuarantine(dirFD, quarantine, quarantinePath); err != nil {
						return err
					}
					continue
				}
				return errors.Join(
					errGlobalProxyArtifactChanged,
					fmt.Errorf("并发全局代理目标已保留：%s", cleanPath),
					removeGlobalProxyQuarantine(dirFD, quarantine, quarantinePath),
				)
			}
			if err := writeGlobalProxyArtifactAtNoReplace(dirFD, base, desired); err != nil {
				if errors.Is(err, unix.EEXIST) {
					return errors.Join(
						errGlobalProxyArtifactChanged,
						fmt.Errorf("并发全局代理目标已保留：%s", cleanPath),
						removeGlobalProxyQuarantine(dirFD, quarantine, quarantinePath),
					)
				}
				return errors.Join(fmt.Errorf("提交全局代理目标替换失败：%s：%w", cleanPath, err), restoreGlobalProxyQuarantine(dirFD, base, quarantine, quarantinePath))
			}
			return removeGlobalProxyQuarantine(dirFD, quarantine, quarantinePath)
		}

		final, readErr := readGlobalProxyArtifactAt(dirFD, base, cleanPath, maxGlobalProxyArtifactBytes)
		if readErr != nil {
			return readErr
		}
		if globalProxyArtifactStatesEqual(final, desired) {
			return nil
		}
		if !globalProxyStateMatchesAny(final, expected) {
			return errGlobalProxyArtifactChanged
		}
		if !final.present {
			if err := writeGlobalProxyArtifactAtNoReplace(dirFD, base, desired); err != nil {
				if errors.Is(err, unix.EEXIST) {
					continue
				}
				return fmt.Errorf("创建全局代理目标失败：%s：%w", cleanPath, err)
			}
			return nil
		}
		claimErr := unix.Renameat2(dirFD, base, dirFD, quarantine, unix.RENAME_NOREPLACE)
		if errors.Is(claimErr, unix.ENOENT) || errors.Is(claimErr, unix.EEXIST) {
			continue
		}
		if claimErr != nil {
			return fmt.Errorf("隔离全局代理目标失败：%s：%w", cleanPath, claimErr)
		}
		if err := syscall.Fsync(dirFD); err != nil {
			return fmt.Errorf("同步全局代理目标隔离失败：%s：%w", quarantinePath, err)
		}
		claimedInCall = true
		globalProxyCASAfterQuarantine(cleanPath)
	}
	return errors.Join(errGlobalProxyArtifactChanged, fmt.Errorf("全局代理 CAS 重试超过上限：%s", cleanPath))
}

func removeGlobalProxyArtifactCAS(path string, expected []globalProxyArtifactState) (bool, error) {
	cleanPath, dirFD, err := openGlobalProxyArtifactDir(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer syscall.Close(dirFD)
	base := filepath.Base(cleanPath)
	quarantine := globalProxyQuarantineName(base)
	quarantinePath := filepath.Join(filepath.Dir(cleanPath), quarantine)

	for attempt := 0; attempt < maxGlobalProxyCASAttempts; attempt++ {
		claimed, readErr := readGlobalProxyArtifactAt(dirFD, quarantine, quarantinePath, maxGlobalProxyArtifactBytes)
		if readErr != nil {
			final, finalErr := readGlobalProxyArtifactAt(dirFD, base, cleanPath, maxGlobalProxyArtifactBytes)
			return false, errors.Join(globalProxyQuarantineMismatchError(dirFD, base, quarantine, cleanPath, final, readErr), finalErr)
		}
		if claimed.present {
			final, finalErr := readGlobalProxyArtifactAt(dirFD, base, cleanPath, maxGlobalProxyArtifactBytes)
			if finalErr != nil {
				return false, errors.Join(errGlobalProxyArtifactChanged, fmt.Errorf("无法安全读取并发全局代理目标，隔离文件已保留：%s：%w", cleanPath, finalErr))
			}
			if !globalProxyStateMatchesAny(claimed, expected) {
				return false, globalProxyQuarantineMismatchError(dirFD, base, quarantine, cleanPath, final, nil)
			}
			if final.present {
				return false, errors.Join(
					errGlobalProxyArtifactChanged,
					fmt.Errorf("并发全局代理目标已保留：%s", cleanPath),
					removeGlobalProxyQuarantine(dirFD, quarantine, quarantinePath),
				)
			}
			if err := removeGlobalProxyQuarantine(dirFD, quarantine, quarantinePath); err != nil {
				return false, err
			}
			return true, nil
		}

		final, readErr := readGlobalProxyArtifactAt(dirFD, base, cleanPath, maxGlobalProxyArtifactBytes)
		if readErr != nil {
			return false, readErr
		}
		if !final.present {
			return false, nil
		}
		if !globalProxyStateMatchesAny(final, expected) {
			return false, errGlobalProxyArtifactChanged
		}
		claimErr := unix.Renameat2(dirFD, base, dirFD, quarantine, unix.RENAME_NOREPLACE)
		if errors.Is(claimErr, unix.ENOENT) || errors.Is(claimErr, unix.EEXIST) {
			continue
		}
		if claimErr != nil {
			return false, fmt.Errorf("隔离待删除全局代理目标失败：%s：%w", cleanPath, claimErr)
		}
		if err := syscall.Fsync(dirFD); err != nil {
			return false, fmt.Errorf("同步待删除全局代理目标隔离失败：%s：%w", quarantinePath, err)
		}
		globalProxyCASAfterQuarantine(cleanPath)
	}
	return false, errors.Join(errGlobalProxyArtifactChanged, fmt.Errorf("全局代理删除 CAS 重试超过上限：%s", cleanPath))
}

func readGlobalProxyArtifactLogical(path string, quarantineExpected []globalProxyArtifactState) (globalProxyArtifactState, bool, error) {
	cleanPath, dirFD, err := openGlobalProxyArtifactDir(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return globalProxyArtifactState{}, false, nil
	}
	if err != nil {
		return globalProxyArtifactState{}, false, err
	}
	defer syscall.Close(dirFD)
	base := filepath.Base(cleanPath)
	quarantine := globalProxyQuarantineName(base)
	quarantinePath := filepath.Join(filepath.Dir(cleanPath), quarantine)
	final, err := readGlobalProxyArtifactAt(dirFD, base, cleanPath, maxGlobalProxyArtifactBytes)
	if err != nil {
		return globalProxyArtifactState{}, false, err
	}
	claimed, err := readGlobalProxyArtifactAt(dirFD, quarantine, quarantinePath, maxGlobalProxyArtifactBytes)
	if err != nil {
		return globalProxyArtifactState{}, true, err
	}
	if !claimed.present {
		return final, false, nil
	}
	if !globalProxyStateMatchesAny(claimed, quarantineExpected) {
		return globalProxyArtifactState{}, true, errors.Join(errGlobalProxyArtifactChanged, fmt.Errorf("全局代理隔离文件与 journal 预期内容或元数据不一致：%s", quarantinePath))
	}
	if !final.present {
		return claimed, true, nil
	}
	return final, true, nil
}

func removeGlobalProxyArtifactDurable(path string) (bool, error) {
	dir := filepath.Dir(path)
	if err := validateNoSymlinkComponents(dir, false); err != nil {
		return false, err
	}
	dirFD, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer syscall.Close(dirFD)
	if err := validatePrivilegedDirFD(dir, dirFD); err != nil {
		return false, err
	}
	base := filepath.Base(path)
	if err := syscall.Unlinkat(dirFD, base); err != nil {
		if err == syscall.ENOENT {
			return false, nil
		}
		return false, err
	}
	if err := syscall.Fsync(dirFD); err != nil {
		return true, fmt.Errorf("持久化全局代理目标删除失败：%w", err)
	}
	return true, nil
}

func syncGlobalProxyArtifactDir(path string) error {
	dir := filepath.Dir(path)
	if err := validateNoSymlinkComponents(dir, false); err != nil {
		return err
	}
	dirFD, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(dirFD)
	if err := validatePrivilegedDirFD(dir, dirFD); err != nil {
		return err
	}
	return syscall.Fsync(dirFD)
}

func decodeGlobalProxyJournal(raw []byte) (*globalProxyJournal, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var journal *globalProxyJournal
	if err := decoder.Decode(&journal); err != nil {
		return nil, err
	}
	if journal == nil {
		return nil, fmt.Errorf("全局代理 ownership journal 不能是 null")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("全局代理 ownership journal 含多个 JSON 值")
		}
		return nil, err
	}
	if err := validateGlobalProxyJournal(journal); err != nil {
		return nil, err
	}
	return journal, nil
}

func validateGlobalProxyJournal(journal *globalProxyJournal) error {
	if journal.Version != globalProxyJournalVersion {
		return fmt.Errorf("不支持的全局代理 ownership journal 版本：%d", journal.Version)
	}
	switch journal.Phase {
	case globalProxyPhasePrepared, globalProxyPhaseActive, globalProxyPhaseRestoring:
	default:
		return fmt.Errorf("全局代理 ownership journal phase 无效：%q", journal.Phase)
	}
	expected := globalProxyArtifactPaths()
	if len(journal.Artifacts) != len(expected) {
		return fmt.Errorf("全局代理 ownership journal 必须包含 %d 个目标", len(expected))
	}
	seen := make(map[string]bool, len(expected))
	byPath := make(map[string]*globalProxyJournalArtifact, len(expected))
	for _, artifact := range journal.Artifacts {
		if artifact == nil || seen[artifact.Path] {
			return fmt.Errorf("全局代理 ownership journal 目标为空或重复")
		}
		if artifact.Path != expected[0] && artifact.Path != expected[1] {
			return fmt.Errorf("全局代理 ownership journal 含未知目标：%s", artifact.Path)
		}
		seen[artifact.Path] = true
		byPath[artifact.Path] = artifact
		if len(artifact.OriginalContent) > maxGlobalProxyArtifactBytes || len(artifact.ManagedContent) > maxGlobalProxyArtifactBytes || len(artifact.PendingManagedContent) > maxGlobalProxyArtifactBytes {
			return fmt.Errorf("全局代理 ownership journal 目标内容过长：%s", artifact.Path)
		}
		if artifact.OriginalMode > 0o777 || artifact.OriginalUID > maxGlobalProxyOwnershipID || artifact.OriginalGID > maxGlobalProxyOwnershipID {
			return fmt.Errorf("全局代理 ownership journal 原元数据无效：%s", artifact.Path)
		}
		if !artifact.OriginalPresent && (len(artifact.OriginalContent) != 0 || artifact.OriginalMode != 0 || artifact.OriginalUID != 0 || artifact.OriginalGID != 0) {
			return fmt.Errorf("全局代理 ownership journal absent 原值携带内容或元数据：%s", artifact.Path)
		}
		if err := validateGlobalManagedMetadata(artifact.Path, "managed", artifact.ManagedContent, artifact.ManagedMode, artifact.ManagedUID, artifact.ManagedGID); err != nil {
			return err
		}
		if err := validateGlobalManagedMetadata(artifact.Path, "pending", artifact.PendingManagedContent, artifact.PendingManagedMode, artifact.PendingManagedUID, artifact.PendingManagedGID); err != nil {
			return err
		}
		if journal.Phase == globalProxyPhaseActive && (len(artifact.ManagedContent) == 0 || len(artifact.PendingManagedContent) != 0) {
			return fmt.Errorf("全局代理 active ownership journal 内容无效：%s", artifact.Path)
		}
		if journal.Phase == globalProxyPhasePrepared && len(artifact.PendingManagedContent) == 0 {
			return fmt.Errorf("全局代理 prepared ownership journal 缺少 pending 内容：%s", artifact.Path)
		}
	}
	for _, path := range expected {
		if !seen[path] {
			return fmt.Errorf("全局代理 ownership journal 缺少目标：%s", path)
		}
	}
	profile := byPath[expected[0]]
	apt := byPath[expected[1]]
	if err := validateGlobalManagedPair(profile.ManagedContent, apt.ManagedContent, true); err != nil {
		return fmt.Errorf("全局代理 ownership journal managed pair 无效：%w", err)
	}
	if err := validateGlobalManagedPair(profile.PendingManagedContent, apt.PendingManagedContent, true); err != nil {
		return fmt.Errorf("全局代理 ownership journal pending pair 无效：%w", err)
	}
	return nil
}

func validateGlobalManagedMetadata(path, label string, content []byte, mode, uid, gid uint32) error {
	if len(content) == 0 {
		if mode != 0 || uid != 0 || gid != 0 {
			return fmt.Errorf("全局代理 ownership journal %s 内容为空但携带元数据：%s", label, path)
		}
		return nil
	}
	if mode != 0o644 || uid > maxGlobalProxyOwnershipID || gid > maxGlobalProxyOwnershipID {
		return fmt.Errorf("全局代理 ownership journal %s 元数据无效：%s", label, path)
	}
	return nil
}

func currentGlobalProxyManagedMetadata() (mode, uid, gid uint32, err error) {
	euid, egid := os.Geteuid(), os.Getegid()
	if euid < 0 || egid < 0 || uint64(euid) > uint64(maxGlobalProxyOwnershipID) || uint64(egid) > uint64(maxGlobalProxyOwnershipID) {
		return 0, 0, 0, fmt.Errorf("当前进程 uid/gid 无法安全记录：uid=%d gid=%d", euid, egid)
	}
	return 0o644, uint32(euid), uint32(egid), nil
}

func validateGlobalManagedPair(profile, apt []byte, allowEmpty bool) error {
	if len(profile) == 0 && len(apt) == 0 && allowEmpty {
		return nil
	}
	if len(profile) == 0 || len(apt) == 0 || !parseLegacyGlobalProxyArtifacts(profile, apt) {
		return fmt.Errorf("两份内容不是互相一致的 proxyscene 全局代理模板")
	}
	return nil
}

func readGlobalProxyJournalFile(path string) (*globalProxyJournal, error) {
	if err := validateNoSymlinkComponents(path, false); err != nil {
		return nil, err
	}
	raw, err := readRegularFileNoFollow(path, maxGlobalProxyJournalBytes)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("全局代理 ownership journal 必须是 root-only 普通文件：%s", path)
	}
	if os.Geteuid() == 0 {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return nil, fmt.Errorf("全局代理 ownership journal 必须属于 root：%s", path)
		}
	}
	journal, err := decodeGlobalProxyJournal(raw)
	if err != nil {
		return nil, fmt.Errorf("全局代理 ownership journal 损坏：%w", err)
	}
	return journal, nil
}

func (a *App) loadGlobalProxyJournal() (*globalProxyJournal, error) {
	mainJournal, mainErr := readGlobalProxyJournalFile(a.globalProxyJournalPath())
	backupJournal, backupErr := readGlobalProxyJournalFile(a.globalProxyJournalBackupPath())
	if mainErr == nil && backupErr == nil {
		switch {
		case mainJournal.Generation > backupJournal.Generation:
			fmt.Println("警告：全局代理 ownership journal 主文件比备份新，使用主文件恢复")
			return mainJournal, nil
		case backupJournal.Generation > mainJournal.Generation:
			fmt.Println("警告：全局代理 ownership journal 备份比主文件新，使用备份恢复")
			return backupJournal, nil
		case !reflect.DeepEqual(mainJournal, backupJournal):
			return nil, fmt.Errorf("全局代理 ownership journal 主备在同一 generation 内容不一致")
		default:
			return mainJournal, nil
		}
	}
	if mainErr == nil {
		fmt.Printf("警告：全局代理 ownership journal 备份不可用，使用主文件：%v\n", backupErr)
		return mainJournal, nil
	}
	if backupErr == nil {
		fmt.Printf("警告：全局代理 ownership journal 主文件不可用，使用备份：%v\n", mainErr)
		return backupJournal, nil
	}
	if errors.Is(mainErr, os.ErrNotExist) && errors.Is(backupErr, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	return nil, fmt.Errorf("全局代理 ownership journal 与备份均不可用：主=%v，备份=%v", mainErr, backupErr)
}

func (a *App) saveGlobalProxyJournal(journal *globalProxyJournal) error {
	if journal.Generation == ^uint64(0) {
		return fmt.Errorf("全局代理 ownership journal generation 已耗尽")
	}
	journal.Version = globalProxyJournalVersion
	if err := validateGlobalProxyJournal(journal); err != nil {
		return err
	}
	journal.Generation++
	raw, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// Backup first: if the main write fails, load selects the newer valid backup
	// and preserves the already-durable transaction phase.
	if err := globalProxyWriteJournal(a.globalProxyJournalBackupPath(), raw, 0o600); err != nil {
		return fmt.Errorf("写入全局代理 ownership journal 备份失败：%w", err)
	}
	if err := globalProxyWriteJournal(a.globalProxyJournalPath(), raw, 0o600); err != nil {
		return fmt.Errorf("写入全局代理 ownership journal 主文件失败：%w", err)
	}
	return nil
}

func (a *App) removeGlobalProxyJournal() error {
	var errs []error
	for _, path := range []string{a.globalProxyJournalPath(), a.globalProxyJournalBackupPath()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("删除 %s 失败：%w", path, err))
		}
	}
	// Persist every successful unlink even when its peer failed, leaving at least
	// one recoverable copy rather than an uncertain directory state.
	errs = append(errs, fsyncDir(a.cfg.CoreDir))
	return errors.Join(errs...)
}

func (a *App) newGlobalProxyJournal(desired map[string][]byte) (*globalProxyJournal, error) {
	managedMode, managedUID, managedGID, err := currentGlobalProxyManagedMetadata()
	if err != nil {
		return nil, err
	}
	journal := &globalProxyJournal{Version: globalProxyJournalVersion, Phase: globalProxyPhasePrepared}
	for _, path := range globalProxyArtifactPaths() {
		current, err := readGlobalProxyArtifact(path)
		if err != nil {
			return nil, fmt.Errorf("读取全局代理目标失败：%s：%w", path, err)
		}
		artifact := &globalProxyJournalArtifact{
			Path:                  path,
			OriginalPresent:       current.present,
			OriginalContent:       append([]byte(nil), current.content...),
			OriginalMode:          uint32(current.mode.Perm()),
			OriginalUID:           current.uid,
			OriginalGID:           current.gid,
			PendingManagedContent: append([]byte(nil), desired[path]...),
			PendingManagedMode:    managedMode,
			PendingManagedUID:     managedUID,
			PendingManagedGID:     managedGID,
		}
		journal.Artifacts = append(journal.Artifacts, artifact)
	}
	return journal, nil
}

// migrateLegacyGlobalProxyOwnership is called only while transitioning a
// pre-RuntimeConfig Store whose Global scene was already enabled. That state
// evidence plus an exact match for both historical dedicated files is enough
// to adopt them as legacy managed artifacts; a partial match is preserved.
func (a *App) migrateLegacyGlobalProxyOwnership() error {
	if _, err := a.loadGlobalProxyJournal(); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	paths := globalProxyArtifactPaths()
	states := make(map[string]globalProxyArtifactState, len(paths))
	for _, path := range paths {
		current, err := readGlobalProxyArtifact(path)
		if err != nil {
			return fmt.Errorf("检查旧版全局代理目标失败：%s：%w", path, err)
		}
		states[path] = current
	}
	profile, apt := states[paths[0]], states[paths[1]]
	managedMode, managedUID, managedGID, err := currentGlobalProxyManagedMetadata()
	if err != nil {
		return err
	}
	if !parseLegacyGlobalProxyArtifacts(profile.content, apt.content) ||
		!globalProxyStateMatchesManaged(profile, profile.content, managedMode, managedUID, managedGID) ||
		!globalProxyStateMatchesManaged(apt, apt.content, managedMode, managedUID, managedGID) {
		fmt.Println("提示：旧版全局代理文件不完全匹配历史托管内容与元数据，已保留为管理员原文件")
		return nil
	}
	journal := &globalProxyJournal{Version: globalProxyJournalVersion, Phase: globalProxyPhaseActive}
	for _, path := range paths {
		journal.Artifacts = append(journal.Artifacts, &globalProxyJournalArtifact{
			Path:           path,
			ManagedContent: append([]byte(nil), states[path].content...),
			ManagedMode:    managedMode,
			ManagedUID:     managedUID,
			ManagedGID:     managedGID,
		})
	}
	if err := a.saveGlobalProxyJournal(journal); err != nil {
		return fmt.Errorf("迁移旧版全局代理 ownership 失败：%w", err)
	}
	return nil
}

func (a *App) prepareGlobalProxyJournal(desired map[string][]byte) (*globalProxyJournal, error) {
	journal, err := a.loadGlobalProxyJournal()
	if errors.Is(err, os.ErrNotExist) {
		journal, err = a.newGlobalProxyJournal(desired)
		if err != nil {
			return nil, err
		}
		if err := a.saveGlobalProxyJournal(journal); err != nil {
			return nil, fmt.Errorf("写入全局代理 ownership journal 失败：%w", err)
		}
		return journal, nil
	}
	if err != nil {
		return nil, err
	}
	if journal.Phase == globalProxyPhaseRestoring {
		if err := a.restoreGlobalWithJournal(); err != nil {
			return nil, fmt.Errorf("继续未完成的全局代理恢复失败：%w", err)
		}
		return a.prepareGlobalProxyJournal(desired)
	}
	if journal.Phase == globalProxyPhasePrepared {
		for _, artifact := range journal.Artifacts {
			if !bytes.Equal(artifact.PendingManagedContent, desired[artifact.Path]) {
				if err := a.restoreGlobalWithJournal(); err != nil {
					return nil, fmt.Errorf("回退未完成的旧全局代理应用失败：%w", err)
				}
				return a.prepareGlobalProxyJournal(desired)
			}
		}
		return journal, nil
	}
	for _, artifact := range journal.Artifacts {
		current, readErr := readGlobalProxyArtifact(artifact.Path)
		if readErr != nil {
			return nil, fmt.Errorf("校验受管全局代理目标失败：%s：%w", artifact.Path, readErr)
		}
		if !globalProxyStateMatchesManaged(current, artifact.ManagedContent, artifact.ManagedMode, artifact.ManagedUID, artifact.ManagedGID) {
			return nil, fmt.Errorf("受管全局代理目标已被修改，拒绝覆盖：%s", artifact.Path)
		}
		managedMode, managedUID, managedGID, metadataErr := currentGlobalProxyManagedMetadata()
		if metadataErr != nil {
			return nil, metadataErr
		}
		artifact.PendingManagedContent = append([]byte(nil), desired[artifact.Path]...)
		artifact.PendingManagedMode = managedMode
		artifact.PendingManagedUID = managedUID
		artifact.PendingManagedGID = managedGID
	}
	journal.Phase = globalProxyPhasePrepared
	if err := a.saveGlobalProxyJournal(journal); err != nil {
		return nil, fmt.Errorf("准备全局代理 ownership journal 失败：%w", err)
	}
	return journal, nil
}

func (a *App) applyGlobalWithJournal() error {
	desired := globalProxyDesiredArtifacts(a.cfg)
	journal, err := a.prepareGlobalProxyJournal(desired)
	if err != nil {
		return err
	}
	expectedStates := make(map[string][]globalProxyArtifactState, len(journal.Artifacts))
	for _, artifact := range journal.Artifacts {
		expected := globalProxyApplyExpectedStates(artifact)
		expectedStates[artifact.Path] = expected
		current, hadQuarantine, readErr := readGlobalProxyArtifactLogical(artifact.Path, expected)
		if readErr != nil {
			err = fmt.Errorf("读取全局代理目标失败：%s：%w", artifact.Path, readErr)
			break
		}
		if globalProxyStateMatchesManaged(current, artifact.PendingManagedContent, artifact.PendingManagedMode, artifact.PendingManagedUID, artifact.PendingManagedGID) {
			continue
		}
		if !globalProxyStateMatchesAny(current, expected) {
			if hadQuarantine {
				// The CAS state machine will preserve the concurrent final name and
				// retire only the journal-owned quarantine.
				continue
			}
			err = fmt.Errorf("全局代理目标在 journal 写入后发生变化，拒绝覆盖：%s", artifact.Path)
			break
		}
	}
	if err != nil {
		// No artifact has been written yet. Keep the prepared journal so the
		// surrounding Store transaction can either retry or explicitly restore it.
		return err
	}
	// Validate the complete pair before replacing either target. Both production
	// parent directories are root-owned and not writable by unprivileged users.
	for _, artifact := range journal.Artifacts {
		if writeErr := globalProxyWriteArtifact(artifact.Path, expectedStates[artifact.Path], globalProxyPendingArtifactState(artifact)); writeErr != nil {
			err = fmt.Errorf("写入全局代理目标失败：%s：%w", artifact.Path, writeErr)
			break
		}
	}
	if err != nil {
		return errors.Join(err, wrapRollbackError("恢复全局代理文件", a.restoreGlobalWithJournal()))
	}
	for _, artifact := range journal.Artifacts {
		artifact.ManagedContent = artifact.PendingManagedContent
		artifact.ManagedMode = artifact.PendingManagedMode
		artifact.ManagedUID = artifact.PendingManagedUID
		artifact.ManagedGID = artifact.PendingManagedGID
		artifact.PendingManagedContent = nil
		artifact.PendingManagedMode = 0
		artifact.PendingManagedUID = 0
		artifact.PendingManagedGID = 0
	}
	journal.Phase = globalProxyPhaseActive
	if err := a.saveGlobalProxyJournal(journal); err != nil {
		return errors.Join(
			fmt.Errorf("提交全局代理 ownership journal 失败：%w", err),
			wrapRollbackError("恢复全局代理文件", a.restoreGlobalWithJournal()),
		)
	}
	return nil
}

func (a *App) restoreGlobalWithJournal() error {
	journal, err := a.loadGlobalProxyJournal()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	states := make(map[string]globalProxyArtifactState, len(journal.Artifacts))
	expectedStates := make(map[string][]globalProxyArtifactState, len(journal.Artifacts))
	var errs []error
	for _, artifact := range journal.Artifacts {
		expected := globalProxyRestoreExpectedStates(artifact)
		expectedStates[artifact.Path] = expected
		current, _, readErr := readGlobalProxyArtifactLogical(artifact.Path, expected)
		if readErr != nil {
			errs = append(errs, fmt.Errorf("读取全局代理目标失败：%s：%w", artifact.Path, readErr))
			continue
		}
		states[artifact.Path] = current
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	journalChanged := journal.Phase != globalProxyPhaseRestoring
	for _, artifact := range journal.Artifacts {
		current := states[artifact.Path]
		if globalProxyStateMatchesOriginal(current, artifact) ||
			globalProxyStateMatchesManaged(current, artifact.ManagedContent, artifact.ManagedMode, artifact.ManagedUID, artifact.ManagedGID) ||
			globalProxyStateMatchesManaged(current, artifact.PendingManagedContent, artifact.PendingManagedMode, artifact.PendingManagedUID, artifact.PendingManagedGID) {
			continue
		}
		// The operator took ownership while the scene was active. Persist that
		// exact regular-file state as the new original before touching its peer.
		artifact.OriginalPresent = current.present
		artifact.OriginalContent = append([]byte(nil), current.content...)
		artifact.OriginalMode = uint32(current.mode.Perm())
		artifact.OriginalUID = current.uid
		artifact.OriginalGID = current.gid
		journalChanged = true
		fmt.Printf("警告：受管全局代理目标已被管理员修改，保留当前文件并释放 ownership：%s\n", artifact.Path)
	}
	journal.Phase = globalProxyPhaseRestoring
	if journalChanged {
		if err := a.saveGlobalProxyJournal(journal); err != nil {
			return fmt.Errorf("准备恢复全局代理 ownership journal 失败：%w", err)
		}
	}
	// The complete decision is durable before either target is restored.
	for _, artifact := range journal.Artifacts {
		if restoreErr := restoreGlobalProxyArtifact(artifact, expectedStates[artifact.Path]); restoreErr != nil {
			return fmt.Errorf("恢复全局代理目标失败：%s：%w", artifact.Path, restoreErr)
		}
	}
	// A prior attempt may have completed rename/unlink but reported its parent
	// fsync failure. Establish fresh barriers unconditionally before deleting the
	// durable ownership records.
	for _, artifact := range journal.Artifacts {
		if err := globalProxySyncArtifactDir(artifact.Path); err != nil {
			return fmt.Errorf("持久化全局代理目标目录失败：%s：%w", filepath.Dir(artifact.Path), err)
		}
	}
	if err := a.removeGlobalProxyJournal(); err != nil {
		return fmt.Errorf("删除全局代理 ownership journal 失败：%w", err)
	}
	return nil
}
