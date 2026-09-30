package manager

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// externalCmdTimeout 限制读取型外部命令（git/npm 配置读取、getent 用户解析）的执行时间。
// 这些命令在持有状态锁期间运行，若卡死（NSS 后端慢、npm 经异常代理联网）会连带阻塞
// 所有并发的 proxyscene 命令，因此给一个宽松但有界的上限。
const externalCmdTimeout = 30 * time.Second

var (
	userCommandEffectiveUID = os.Geteuid
	userCommandEffectiveGID = os.Getegid
)

func envString(key, fallback string) string {
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	// 无法识别的值（例如拼错的 "ture"）不能静默当 false/true：打警告并用默认值，
	// 避免操作员以为某个开关生效了而实际没有。
	fmt.Printf("警告：环境变量 %s 的值无法识别（%q），使用默认值 %v\n", key, os.Getenv(key), fallback)
	return fallback
}

func splitFields(s string) []string {
	return strings.Fields(strings.ReplaceAll(s, ",", " "))
}

func itoa(n int) string { return strconv.Itoa(n) }

func requireRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("请用 root 运行")
	}
	return nil
}

func ensureDir(path string, perm os.FileMode) error {
	existed := true
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		existed = false
	} else {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("目录不能是符号链接：%s", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("路径不是目录：%s", path)
		}
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("目录不能是符号链接：%s", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("路径不是目录：%s", path)
	}
	if err := validatePrivilegedDirInfo(path, info); err != nil {
		return err
	}
	if !existed {
		return os.Chmod(path, perm)
	}
	return nil
}

func ensurePublicDir(path string) error {
	if err := validateNoSymlinkComponents(path, false); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if err := validateNoSymlinkComponents(path, false); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("路径不是目录：%s", path)
	}
	return validatePrivilegedDirInfo(path, info)
}

// validateNoSymlinkComponents walks every existing component of path. It is used for
// privileged paths before they are pinned by a directory fd; a missing suffix is fine
// when callers are about to create it.
func validateNoSymlinkComponents(path string, requireRootOwned bool) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("特权路径必须是绝对路径：%s", path)
	}
	current := string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(clean, string(os.PathSeparator)), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("特权路径不能经过符号链接：%s", current)
		}
		if !info.IsDir() && current != clean {
			return fmt.Errorf("特权路径祖先不是目录：%s", current)
		}
		if requireRootOwned && info.IsDir() {
			if err := validatePrivilegedDirInfo(current, info); err != nil {
				return err
			}
		}
	}
	return nil
}

func validatePrivilegedDirInfo(path string, info os.FileInfo) error {
	if os.Geteuid() != 0 {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("无法校验特权目录属主：%s", path)
	}
	if st.Uid != 0 {
		return fmt.Errorf("特权目录必须属于 root：%s（uid=%d）", path, st.Uid)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("特权目录不能允许组或其他用户写入：%s（权限=%#o）", path, info.Mode().Perm())
	}
	return nil
}

func validatePrivilegedDirFD(path string, fd int) error {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return fmt.Errorf("路径不是目录：%s", path)
	}
	if os.Geteuid() == 0 {
		if st.Uid != 0 {
			return fmt.Errorf("特权目录必须属于 root：%s（uid=%d）", path, st.Uid)
		}
		if os.FileMode(st.Mode).Perm()&0o022 != 0 {
			return fmt.Errorf("特权目录不能允许组或其他用户写入：%s（权限=%#o）", path, os.FileMode(st.Mode).Perm())
		}
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomicOwned(path, data, perm, -1, -1)
}

// writeFileAtomicOwned is writeFileAtomic with an explicit inode owner. The
// ownership and mode are applied to the temporary inode before its fsync and
// rename, so readers never observe a partially committed metadata state.
func writeFileAtomicOwned(path string, data []byte, perm os.FileMode, uid, gid int) error {
	if uid < -1 || gid < -1 {
		return fmt.Errorf("文件属主参数无效：uid=%d gid=%d", uid, gid)
	}
	dir := filepath.Dir(path)
	if err := ensurePublicDir(dir); err != nil {
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
	base := filepath.Base(path)
	tmpName, tmpFD, err := createTempFileAt(dirFD, base, perm)
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
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if uid >= 0 || gid >= 0 {
		if err := syscall.Fchown(int(f.Fd()), uid, gid); err != nil {
			_ = f.Close()
			return err
		}
	}
	// 先 fsync 文件数据再 rename，rename 后再 fsync 父目录：os.Rename 对并发读者是
	// 原子的但并不保证持久化，崩溃/掉电后可能出现 0 字节或残缺文件。state.json 等的
	// 崩溃恢复依赖此持久性保证。
	// 属主和权限也是待提交 inode 的一部分，必须在 fsync 前设置。fchown 可能清除
	// setuid/setgid 位，因此固定在 fchown 后重新 fchmod。
	if err := syscall.Fchmod(int(f.Fd()), uint32(perm.Perm())); err != nil {
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
	if err := syscall.Renameat(dirFD, tmpName, dirFD, base); err != nil {
		return err
	}
	if err := syscall.Fsync(dirFD); err != nil {
		return err
	}
	committed = true
	return nil
}

// fsyncDir 对目录执行 fsync，使其中文件的创建/改名在崩溃后可见（rename 的持久化屏障）。
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func writeUserFileAtomicAtNoReplace(dirFD int, base string, data []byte, perm os.FileMode, uid, gid int) error {
	return writeUserFileAtomicAtMode(dirFD, base, data, perm, uid, gid, true)
}

func writeUserFileAtomicAtMode(dirFD int, base string, data []byte, perm os.FileMode, uid, gid int, noReplace bool) error {
	return writeUserFileAtomicAtModeChecked(dirFD, base, data, perm, uid, gid, noReplace, nil)
}

func writeUserFileAtomicAtModeChecked(dirFD int, base string, data []byte, perm os.FileMode, uid, gid int, noReplace bool, beforePublish func() error) error {
	// Never expose credentials under the creator's group before Fchown, even
	// when the destination is group-readable. Remove any inherited access ACL
	// before writing so the requested POSIX permissions describe all readers.
	tmpName, tmpFD, err := createTempFileAt(dirFD, base, 0o600)
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
	if err := clearInheritedUserFileACL(int(f.Fd())); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := syscall.Fchown(int(f.Fd()), uid, gid); err != nil {
		_ = f.Close()
		return err
	}
	// Revalidate before making any credentials group-readable. A concurrent
	// chmod/chgrp/ACL change must fail while the candidate is still private.
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := syscall.Fchmod(int(f.Fd()), uint32(perm.Perm())); err != nil {
		_ = f.Close()
		return err
	}
	// 与 writeFileAtomic 一致：fsync 文件数据后再 renameat，并 fsync 目录 fd，保证持久性。
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			return err
		}
	}
	if noReplace {
		err = unix.Renameat2(dirFD, tmpName, dirFD, base, unix.RENAME_NOREPLACE)
	} else {
		err = syscall.Renameat(dirFD, tmpName, dirFD, base)
	}
	if err != nil {
		return err
	}
	if err := syscall.Fsync(dirFD); err != nil {
		return fmt.Errorf("同步用户级配置目录失败：%w", err)
	}
	committed = true
	return nil
}

var userTmpCounter uint64

// createTempFileAt 在 dirFD 目录下创建一个唯一的临时文件，使用 O_EXCL|O_NOFOLLOW
// 保证不会跟随符号链接、也不会覆盖已存在的文件。
func createTempFileAt(dirFD int, base string, perm os.FileMode) (string, int, error) {
	flags := syscall.O_CREAT | syscall.O_EXCL | syscall.O_WRONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	for i := 0; i < 100; i++ {
		seq := atomic.AddUint64(&userTmpCounter, 1)
		name := fmt.Sprintf(".%s.tmp.%d.%d", base, os.Getpid(), seq)
		fd, err := syscall.Openat(dirFD, name, flags, uint32(perm.Perm()))
		if err == nil {
			return name, fd, nil
		}
		if err != syscall.EEXIST {
			return "", -1, fmt.Errorf("创建用户级临时文件失败：%w", err)
		}
	}
	return "", -1, fmt.Errorf("创建用户级临时文件失败：重试次数过多")
}

// openUserDirChain 从家目录开始，沿 rel 逐级 openat 打开（必要时创建）目录，
// 全程使用 O_NOFOLLOW 拒绝符号链接，并确保每级目录属于目标用户。返回最终目录 fd。
func openUserDirChain(home, rel string, uid, gid int) (int, error) {
	homeFD, err := syscall.Open(home, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("打开用户家目录失败：%s：%w", home, err)
	}
	if err := validateUserHomeFD(home, homeFD, uid, gid); err != nil {
		_ = syscall.Close(homeFD)
		return -1, err
	}
	if rel == "." {
		return homeFD, nil
	}
	current := homeFD
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		next, err := openOwnedDirAt(current, part, uid, gid)
		_ = syscall.Close(current)
		if err != nil {
			return -1, err
		}
		current = next
	}
	return current, nil
}

func openOwnedDirAt(dirFD int, name string, uid, gid int) (int, error) {
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_DIRECTORY | syscall.O_CLOEXEC
	created := false
	fd, err := syscall.Openat(dirFD, name, flags, 0)
	if err != nil {
		if err != syscall.ENOENT {
			return -1, fmt.Errorf("打开用户级配置目录 %s 失败（拒绝符号链接）：%w", name, err)
		}
		mkErr := syscall.Mkdirat(dirFD, name, 0o755)
		if mkErr != nil && mkErr != syscall.EEXIST {
			return -1, fmt.Errorf("创建用户级配置目录 %s 失败：%w", name, mkErr)
		}
		created = mkErr == nil
		fd, err = syscall.Openat(dirFD, name, flags, 0)
		if err != nil {
			return -1, fmt.Errorf("打开用户级配置目录 %s 失败（创建后，拒绝符号链接）：%w", name, err)
		}
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		_ = syscall.Close(fd)
		return -1, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		_ = syscall.Close(fd)
		return -1, fmt.Errorf("用户级配置路径不是目录：%s", name)
	}
	// 仅对本函数新建的目录设置属主，避免强行改写用户已存在目录（如 ~/.config）的属主。
	if created && (int(st.Uid) != uid || int(st.Gid) != gid) {
		if err := syscall.Fchown(fd, uid, gid); err != nil {
			_ = syscall.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}

func ensureUserHomeUsable(home, userName string) error {
	if strings.TrimSpace(home) == "" || !filepath.IsAbs(home) || filepath.Clean(home) == string(os.PathSeparator) {
		return fmt.Errorf("用户 %s 的家目录无效：%s", userName, home)
	}
	info, err := os.Lstat(home)
	if err != nil {
		return fmt.Errorf("用户 %s 的家目录不可用：%w", userName, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("用户 %s 的家目录不能是符号链接：%s", userName, home)
	}
	if !info.IsDir() {
		return fmt.Errorf("用户 %s 的家目录不是目录：%s", userName, home)
	}
	return nil
}

// readUserFileNoFollow 以 root 身份读取目标用户家目录内的文件，沿家目录→目标文件全程使用
// O_NOFOLLOW：任何一级目录或最终文件是符号链接都拒绝。这与 writeUserFileAtomic 的防护一致，
// 防止非 root 用户把自己的配置软链到 root 才可读的文件、诱导 root 把其内容读出并写进用户可读
// 的文件（本地越权/信息泄露）。缺失（ENOENT）映射为 os.ErrNotExist，便于调用方将"配置不存在"
// 当作跳过处理。
func readUserFileNoFollow(userName, path string, max int64) ([]byte, error) {
	identity, err := lookupLocalUserIdentity(userName)
	if err != nil {
		return nil, err
	}
	if err := ensureUserHomeUsable(identity.Home, userName); err != nil {
		return nil, err
	}
	cleanHome := filepath.Clean(identity.Home)
	cleanPath := filepath.Clean(path)
	if cleanPath == cleanHome || !strings.HasPrefix(cleanPath, cleanHome+string(os.PathSeparator)) {
		return nil, fmt.Errorf("用户级配置路径必须位于用户 %s 的家目录内：%s", userName, path)
	}
	dir := filepath.Dir(cleanPath)
	rel, err := filepath.Rel(cleanHome, dir)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, fmt.Errorf("用户级配置目录必须位于用户家目录内：%s", dir)
	}
	dirFD, err := openExistingUserDirChain(cleanHome, rel, identity.UID, identity.GID)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(dirFD)
	base := filepath.Base(cleanPath)
	b, err := readRegularFileAtNoFollow(dirFD, base, max)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("打开用户级配置失败（仅允许普通文件且拒绝符号链接）：%s：%w", path, err)
	}
	return b, nil
}

func readRegularFileAtNoFollow(dirFD int, base string, max int64) ([]byte, error) {
	flags := syscall.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	fd, err := syscall.Openat(dirFD, base, flags, 0)
	if err != nil {
		if err == syscall.ENOENT {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), base)
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return nil, fmt.Errorf("不是普通文件")
	}
	if max >= 0 && st.Size > max {
		return nil, fmt.Errorf("文件超过大小限制 %d 字节", max)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("文件超过大小限制 %d 字节", max)
	}
	return b, nil
}

func readRegularFileNoFollow(path string, max int64) ([]byte, error) {
	dir := filepath.Dir(path)
	dirFD, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ENOENT {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	defer syscall.Close(dirFD)
	return readRegularFileAtNoFollow(dirFD, filepath.Base(path), max)
}

var errUserFileChanged = errors.New("用户配置在读取后已被修改")

const (
	userFileQuarantineName            = ".proxyscene-quarantine"
	maxRemoveUserFileCASClaimAttempts = 3
)

var userFileCASAfterQuarantine = func(string) {}

// writeUserFileAtomicCAS first moves the current name to a reserved quarantine
// with RENAME_NOREPLACE. Only the claimed inode is compared and discarded, and
// the replacement is installed with RENAME_NOREPLACE as well. A concurrent user
// writer therefore wins the final name instead of being overwritten.
func writeUserFileAtomicCAS(userName, path string, expected, data []byte, perm os.FileMode) error {
	identity, err := lookupLocalUserIdentity(userName)
	if err != nil {
		return err
	}
	return writeUserFileAtomicCASWithIdentity(userName, identity, path, expected, data, perm)
}

func writeUserFileAtomicCASPersisted(userName string, expectedIdentity *persistedUserIdentity, lookup localUserIdentityLookup, path string, expected, data []byte, perm os.FileMode) error {
	identity, err := verifyPersistedUserIdentity(userName, expectedIdentity, lookup)
	if err != nil {
		return err
	}
	return writeUserFileAtomicCASWithIdentity(userName, identity, path, expected, data, perm)
}

func writeUserFileAtomicCASWithIdentity(userName string, identity localUserIdentity, path string, expected, data []byte, perm os.FileMode) error {
	return writeUserFileAtomicCASWithMetadata(userName, identity, path, expected, data, perm, nil)
}

func writeUserFileAtomicCASWithMetadata(userName string, identity localUserIdentity, path string, expected, data []byte, perm os.FileMode, metadata *userFileMetadata) error {
	cleanPath, dirFD, err := openUserFileDirForIdentity(userName, identity, path, false)
	if err != nil {
		return err
	}
	defer syscall.Close(dirFD)
	base := filepath.Base(cleanPath)
	quarantinePath := filepath.Join(filepath.Dir(cleanPath), userFileQuarantineName)
	claimErr := unix.Renameat2(dirFD, base, dirFD, userFileQuarantineName, unix.RENAME_NOREPLACE)
	if claimErr != nil {
		switch claimErr {
		case unix.ENOENT, unix.EEXIST:
			found, resume, replayErr := replayUserFileWriteQuarantine(dirFD, base, quarantinePath, expected, data, metadata)
			if replayErr != nil {
				return replayErr
			}
			if found {
				if resume {
					break
				}
				return nil
			}
			if errors.Is(claimErr, unix.ENOENT) {
				return errUserFileChanged
			}
			return fmt.Errorf("用户配置隔离文件在重放前消失，拒绝自动覆盖：%s", quarantinePath)
		default:
			return fmt.Errorf("隔离用户配置失败：%s：%w", cleanPath, claimErr)
		}
	} else {
		userFileCASAfterQuarantine(cleanPath)
	}

	matched, err := readExpectedUserFileQuarantineWithMetadata(dirFD, quarantinePath, expected, metadata)
	if err != nil {
		return err
	}
	if !matched {
		return fmt.Errorf("用户配置隔离文件在提交前消失，拒绝自动覆盖：%s", quarantinePath)
	}
	uid, gid := identity.UID, identity.GID
	var beforePublish func() error
	if metadata != nil {
		uid, gid, perm = metadata.UID, metadata.GID, metadata.Mode
		beforePublish = func() error {
			found, err := readExpectedUserFileQuarantineWithMetadata(dirFD, quarantinePath, expected, metadata)
			if err == nil && !found {
				return errUserFileChanged
			}
			return err
		}
	}
	if err := writeUserFileAtomicAtModeChecked(dirFD, base, data, perm, uid, gid, true, beforePublish); err != nil {
		if errors.Is(err, unix.EEXIST) {
			if cleanupErr := removeUserFileQuarantine(dirFD, quarantinePath); cleanupErr != nil {
				return errors.Join(errUserFileChanged, cleanupErr)
			}
			return errUserFileChanged
		}
		return errors.Join(fmt.Errorf("提交用户配置替换失败：%w", err), restoreUserFileQuarantine(dirFD, cleanPath))
	}
	if err := removeUserFileQuarantine(dirFD, quarantinePath); err != nil {
		return err
	}
	return nil
}

// replayUserFileWriteQuarantine reconciles a fixed-name quarantine left by a
// previous process. The quarantine is usable only when its bytes still equal
// the caller's expected value. In that case an absent final name resumes the
// write, an already-desired final name completes cleanup, and any other final
// name wins as a concurrent user update.
func replayUserFileWriteQuarantine(dirFD int, base, quarantinePath string, expected, data []byte, metadata *userFileMetadata) (found, resume bool, err error) {
	found, err = readExpectedUserFileQuarantineWithMetadata(dirFD, quarantinePath, expected, metadata)
	if err != nil || !found {
		return found, false, err
	}
	current, readErr := readUserFileWithExpectedMetadata(dirFD, base, int64(len(data))+1, metadata, false)
	if errors.Is(readErr, os.ErrNotExist) {
		return true, true, nil
	}
	if readErr == nil && bytes.Equal(current, data) {
		return true, false, removeUserFileQuarantine(dirFD, quarantinePath)
	}
	if metadata != nil && readErr != nil {
		return true, false, errors.Join(errUserFileChanged, readErr)
	}
	cleanupErr := removeUserFileQuarantine(dirFD, quarantinePath)
	if readErr != nil {
		return true, false, errors.Join(errUserFileChanged, fmt.Errorf("并发用户配置无法按普通文件读取，已保留最终名称：%w", readErr), cleanupErr)
	}
	return true, false, errors.Join(errUserFileChanged, cleanupErr)
}

func readExpectedUserFileQuarantine(dirFD int, quarantinePath string, expected []byte) (bool, error) {
	return readExpectedUserFileQuarantineWithMetadata(dirFD, quarantinePath, expected, nil)
}

func readExpectedUserFileQuarantineWithMetadata(dirFD int, quarantinePath string, expected []byte, metadata *userFileMetadata) (bool, error) {
	current, err := readUserFileWithExpectedMetadata(dirFD, userFileQuarantineName, int64(len(expected))+1, metadata, true)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, errors.Join(errUserFileChanged, fmt.Errorf("无法安全读取用户配置隔离文件，已保留并拒绝自动操作：%s：%w", quarantinePath, err))
	}
	if !bytes.Equal(current, expected) {
		return true, errors.Join(errUserFileChanged, fmt.Errorf("用户配置隔离文件内容与预期不匹配，已保留并拒绝自动操作：%s", quarantinePath))
	}
	return true, nil
}

func writeUserFileAtomicCreatePersisted(userName string, expectedIdentity *persistedUserIdentity, lookup localUserIdentityLookup, path string, data []byte, perm os.FileMode) error {
	identity, err := verifyPersistedUserIdentity(userName, expectedIdentity, lookup)
	if err != nil {
		return err
	}
	return writeUserFileAtomicCreateWithIdentity(userName, identity, path, data, perm)
}

func writeUserFileAtomicCreateWithIdentity(userName string, identity localUserIdentity, path string, data []byte, perm os.FileMode) error {
	cleanPath, dirFD, err := openUserFileDirForIdentity(userName, identity, path, true)
	if err != nil {
		return err
	}
	defer syscall.Close(dirFD)
	if err := writeUserFileAtomicAtNoReplace(dirFD, filepath.Base(cleanPath), data, perm, identity.UID, identity.GID); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return errUserFileChanged
		}
		return err
	}
	return nil
}

func openUserFileDirForIdentity(userName string, identity localUserIdentity, path string, create bool) (string, int, error) {
	if err := ensureUserHomeUsable(identity.Home, userName); err != nil {
		return "", -1, err
	}
	cleanHome := filepath.Clean(identity.Home)
	cleanPath := filepath.Clean(path)
	if cleanPath == cleanHome || !strings.HasPrefix(cleanPath, cleanHome+string(os.PathSeparator)) {
		return "", -1, fmt.Errorf("用户级配置路径必须位于用户 %s 的家目录内：%s", userName, path)
	}
	dirRel, err := filepath.Rel(cleanHome, filepath.Dir(cleanPath))
	if err != nil || dirRel == ".." || filepath.IsAbs(dirRel) || strings.HasPrefix(dirRel, ".."+string(os.PathSeparator)) {
		return "", -1, fmt.Errorf("用户级配置目录必须位于用户家目录内：%s", filepath.Dir(cleanPath))
	}
	var dirFD int
	if create {
		dirFD, err = openUserDirChain(cleanHome, dirRel, identity.UID, identity.GID)
	} else {
		dirFD, err = openExistingUserDirChain(cleanHome, dirRel, identity.UID, identity.GID)
	}
	if err != nil {
		return "", -1, err
	}
	return cleanPath, dirFD, nil
}

func restoreUserFileQuarantine(dirFD int, cleanPath string) error {
	quarantinePath := filepath.Join(filepath.Dir(cleanPath), userFileQuarantineName)
	err := unix.Renameat2(dirFD, userFileQuarantineName, dirFD, filepath.Base(cleanPath), unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("用户配置已并发创建，无法无覆盖恢复；隔离文件已保留：%s", quarantinePath)
	}
	if err != nil {
		return fmt.Errorf("恢复隔离用户配置失败，隔离文件已保留：%s：%w", quarantinePath, err)
	}
	if err := syscall.Fsync(dirFD); err != nil {
		return fmt.Errorf("同步隔离用户配置恢复失败：%s：%w", filepath.Dir(cleanPath), err)
	}
	return nil
}

func removeUserFileQuarantine(dirFD int, quarantinePath string) error {
	if err := syscall.Unlinkat(dirFD, userFileQuarantineName); err != nil {
		return fmt.Errorf("删除已确认的用户配置隔离文件失败，文件已保留：%s：%w", quarantinePath, err)
	}
	if err := syscall.Fsync(dirFD); err != nil {
		return fmt.Errorf("同步用户配置隔离文件删除失败：%s：%w", filepath.Dir(quarantinePath), err)
	}
	return nil
}

// openExistingUserDirChain 从 home 起沿 rel 逐级 openat 打开已存在的目录（不创建），
// 全程 O_NOFOLLOW|O_DIRECTORY 拒绝符号链接；任一级缺失返回 os.ErrNotExist。
func openExistingUserDirChain(home, rel string, uid, gid int) (int, error) {
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_DIRECTORY | syscall.O_CLOEXEC
	homeFD, err := syscall.Open(home, flags, 0)
	if err != nil {
		return -1, fmt.Errorf("打开用户家目录失败：%s：%w", home, err)
	}
	if err := validateUserHomeFD(home, homeFD, uid, gid); err != nil {
		_ = syscall.Close(homeFD)
		return -1, err
	}
	if rel == "." {
		return homeFD, nil
	}
	current := homeFD
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		next, err := syscall.Openat(current, part, flags, 0)
		_ = syscall.Close(current)
		if err != nil {
			if err == syscall.ENOENT {
				return -1, os.ErrNotExist
			}
			return -1, fmt.Errorf("打开用户级配置目录 %s 失败（拒绝符号链接）：%w", part, err)
		}
		current = next
	}
	return current, nil
}

func validateUserHomeFD(home string, fd, uid, gid int) error {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fmt.Errorf("校验用户家目录失败：%s：%w", home, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return fmt.Errorf("用户家目录不是目录：%s", home)
	}
	if int(st.Uid) != uid || int(st.Gid) != gid {
		return fmt.Errorf(
			"用户家目录打开后属主与记录身份不一致，拒绝操作：%s（记录 uid=%d gid=%d；目录 uid=%d gid=%d）",
			home,
			uid,
			gid,
			st.Uid,
			st.Gid,
		)
	}
	return nil
}

// externalCommandTimeout 给一般外部命令（systemctl、git/npm 写、useradd 等）一个宽松上限。
// 取 5 分钟：远高于 systemd 默认的 stop+start 超时（各 90s），只用于兜住真正卡死的调用，
// 避免它们在持有状态锁时无限阻塞所有并发的 proxyscene 命令。
const externalCommandTimeout = 5 * time.Minute

func runQuiet(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), externalCommandTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run()
}

func unlinkatWithFlags(dirFD int, name string, flags int) error {
	return unix.Unlinkat(dirFD, name, flags)
}

// removeUserFileAndEmptyParentNoFollow removes a managed file below a user's
// home without ever resolving a user-controlled absolute path as root. Both the
// file unlink and optional empty parent removal are relative to pinned fds.
func removeUserFileAndEmptyParentNoFollow(userName, path string) (bool, error) {
	identity, err := lookupLocalUserIdentity(userName)
	if err != nil {
		return false, err
	}
	if err := ensureUserHomeUsable(identity.Home, userName); err != nil {
		return false, err
	}
	cleanHome := filepath.Clean(identity.Home)
	cleanPath := filepath.Clean(path)
	if cleanPath == cleanHome || !strings.HasPrefix(cleanPath, cleanHome+string(os.PathSeparator)) {
		return false, fmt.Errorf("用户级配置路径必须位于用户 %s 的家目录内：%s", userName, path)
	}
	dirRel, err := filepath.Rel(cleanHome, filepath.Dir(cleanPath))
	if err != nil || dirRel == ".." || filepath.IsAbs(dirRel) || strings.HasPrefix(dirRel, ".."+string(os.PathSeparator)) {
		return false, fmt.Errorf("用户级配置目录必须位于用户家目录内：%s", filepath.Dir(cleanPath))
	}
	parentRel := filepath.Dir(dirRel)
	dirName := filepath.Base(dirRel)
	if dirRel == "." {
		parentRel = "."
		dirName = "."
	}
	parentFD, err := openExistingUserDirChain(cleanHome, parentRel, identity.UID, identity.GID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer syscall.Close(parentFD)
	dirFD := parentFD
	if dirName != "." {
		dirFD, err = syscall.Openat(parentFD, dirName, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
		if err == syscall.ENOENT {
			if syncErr := syscall.Fsync(parentFD); syncErr != nil {
				return false, fmt.Errorf("同步已删除的用户级配置目录父目录失败：%s：%w", filepath.Dir(filepath.Dir(path)), syncErr)
			}
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("打开用户级配置目录失败（拒绝符号链接）：%s：%w", dirName, err)
		}
		defer syscall.Close(dirFD)
	}
	removed := false
	if err := syscall.Unlinkat(dirFD, filepath.Base(cleanPath)); err != nil {
		if err != syscall.ENOENT {
			return false, fmt.Errorf("删除用户级配置失败：%s：%w", path, err)
		}
	} else {
		removed = true
	}
	// The barrier also runs on an ENOENT retry: the previous process may have
	// unlinked the file but failed before proving that deletion durable.
	if err := syscall.Fsync(dirFD); err != nil {
		return removed, fmt.Errorf("同步用户级配置删除失败：%s：%w", filepath.Dir(path), err)
	}
	if dirName != "." {
		if err := unlinkatWithFlags(parentFD, dirName, unix.AT_REMOVEDIR); err != nil {
			switch err {
			case syscall.ENOTEMPTY, syscall.EEXIST:
				return removed, nil
			case syscall.ENOENT:
				// A replayed/concurrent rmdir still requires the parent barrier below.
			default:
				return removed, fmt.Errorf("删除空用户级配置目录失败：%s：%w", filepath.Dir(path), err)
			}
		}
		if err := syscall.Fsync(parentFD); err != nil {
			return removed, fmt.Errorf("同步用户级配置目录删除失败：%s：%w", filepath.Dir(filepath.Dir(path)), err)
		}
	}
	return removed, nil
}

// removeUserFileAndEmptyParentCAS removes only the inode whose bytes match
// expected. The final name is first claimed with RENAME_NOREPLACE, so a file
// created concurrently at that name is preserved.
func removeUserFileAndEmptyParentCAS(userName, path string, expected []byte) (bool, error) {
	identity, err := lookupLocalUserIdentity(userName)
	if err != nil {
		return false, err
	}
	return removeUserFileCASWithIdentity(userName, identity, path, expected)
}

func removeUserFileCASPersisted(userName string, expectedIdentity *persistedUserIdentity, lookup localUserIdentityLookup, path string, expected []byte) (bool, error) {
	identity, err := verifyPersistedUserIdentity(userName, expectedIdentity, lookup)
	if err != nil {
		return false, err
	}
	return removeUserFileCASWithIdentity(userName, identity, path, expected)
}

func removeUserFileCASWithIdentity(userName string, identity localUserIdentity, path string, expected []byte) (bool, error) {
	cleanPath, dirFD, err := openUserFileDirForIdentity(userName, identity, path, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer syscall.Close(dirFD)
	base := filepath.Base(cleanPath)
	quarantinePath := filepath.Join(filepath.Dir(cleanPath), userFileQuarantineName)
	for attempt := 0; attempt < maxRemoveUserFileCASClaimAttempts; attempt++ {
		claimErr := unix.Renameat2(dirFD, base, dirFD, userFileQuarantineName, unix.RENAME_NOREPLACE)
		if claimErr == nil {
			userFileCASAfterQuarantine(cleanPath)
			matched, err := readExpectedUserFileQuarantine(dirFD, quarantinePath, expected)
			if err != nil {
				return false, err
			}
			if !matched {
				return false, fmt.Errorf("用户配置隔离文件在删除前消失，拒绝自动提交：%s", quarantinePath)
			}
			if err := removeUserFileQuarantine(dirFD, quarantinePath); err != nil {
				return false, err
			}
			return true, nil
		}

		switch claimErr {
		case unix.ENOENT:
			found, replayErr := readExpectedUserFileQuarantine(dirFD, quarantinePath, expected)
			if replayErr != nil {
				return false, replayErr
			}
			if found {
				if cleanupErr := removeUserFileQuarantine(dirFD, quarantinePath); cleanupErr != nil {
					return false, cleanupErr
				}
				return true, nil
			}
			if syncErr := syscall.Fsync(dirFD); syncErr != nil {
				return false, fmt.Errorf("同步已删除的用户配置失败：%s：%w", filepath.Dir(cleanPath), syncErr)
			}
			return false, nil
		case unix.EEXIST:
			found, replayErr := readExpectedUserFileQuarantine(dirFD, quarantinePath, expected)
			if replayErr != nil {
				return false, replayErr
			}
			if !found {
				return false, fmt.Errorf("用户配置隔离文件在重放前消失，拒绝自动删除：%s", quarantinePath)
			}

			current, readErr := readRegularFileAtNoFollow(dirFD, base, int64(len(expected))+1)
			if errors.Is(readErr, os.ErrNotExist) {
				if cleanupErr := removeUserFileQuarantine(dirFD, quarantinePath); cleanupErr != nil {
					return false, cleanupErr
				}
				return true, nil
			}
			if readErr != nil {
				return false, errors.Join(errUserFileChanged, fmt.Errorf("并发用户配置无法按普通文件安全读取，隔离文件已保留：%s：%w", quarantinePath, readErr))
			}
			if !bytes.Equal(current, expected) {
				return false, errors.Join(errUserFileChanged, fmt.Errorf("并发用户配置内容与预期不匹配，隔离文件已保留：%s", quarantinePath))
			}
			if attempt+1 >= maxRemoveUserFileCASClaimAttempts {
				return false, errors.Join(errUserFileChanged, fmt.Errorf("用户配置删除重放超过重试上限，隔离文件已保留：%s", quarantinePath))
			}
			if cleanupErr := removeUserFileQuarantine(dirFD, quarantinePath); cleanupErr != nil {
				return false, cleanupErr
			}
			// The final name contains another expected inode. Reclaim it through
			// the same no-replace path instead of treating the stale quarantine
			// as proof that deletion already completed.
			continue
		default:
			return false, fmt.Errorf("隔离待删除用户配置失败：%s：%w", cleanPath, claimErr)
		}
	}
	return false, errors.Join(errUserFileChanged, fmt.Errorf("用户配置删除重放超过重试上限，隔离文件已保留：%s", quarantinePath))
}

func confirmUserFileAbsentPersisted(userName string, expectedIdentity *persistedUserIdentity, lookup localUserIdentityLookup, path string) error {
	identity, err := verifyPersistedUserIdentity(userName, expectedIdentity, lookup)
	if err != nil {
		return err
	}
	return confirmUserFileAbsentWithIdentity(userName, identity, path)
}

func confirmUserFileAbsentWithIdentity(userName string, identity localUserIdentity, path string) error {
	cleanPath, dirFD, err := openUserFileDirForIdentity(userName, identity, path, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer syscall.Close(dirFD)
	var st unix.Stat_t
	err = unix.Fstatat(dirFD, filepath.Base(cleanPath), &st, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		return errUserFileChanged
	}
	if !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err := syscall.Fsync(dirFD); err != nil {
		return fmt.Errorf("同步已删除的用户配置失败：%s：%w", filepath.Dir(cleanPath), err)
	}
	return nil
}

func runQuietLabel(label, name string, args ...string) error {
	return runQuietEnvLabel(label, nil, name, args...)
}

func runQuietEnvLabel(label string, env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), externalCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s超时（%s）", label, externalCommandTimeout)
		}
		return commandFailed(label, err)
	}
	return nil
}

func outputQuietLabel(label, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), externalCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	b, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return string(b), fmt.Errorf("%s超时（%s）", label, externalCommandTimeout)
		}
		return string(b), commandFailed(label, err)
	}
	return string(b), nil
}

func commandFailed(label string, err error) error {
	if label == "" {
		label = "执行外部命令"
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%s失败：找不到命令", label)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("%s失败，退出码：%d", label, exitErr.ExitCode())
	}
	return fmt.Errorf("%s失败：%v", label, err)
}

func outputAsUser(user, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), externalCmdTimeout)
	defer cancel()
	cmd, _, err := commandAsUser(ctx, user, name, args...)
	if err != nil {
		return "", err
	}
	b, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return string(b), fmt.Errorf("以用户 %s 执行命令 %s 超时（%s）", user, name, externalCmdTimeout)
	}
	return string(b), err
}

func commandAsUser(ctx context.Context, user, name string, args ...string) (*exec.Cmd, string, error) {
	if user == "" {
		user = "root"
	}
	identity, err := lookupLocalUserIdentity(user)
	if err != nil {
		return nil, "", err
	}
	return commandForUserIdentity(ctx, user, identity, name, args...)
}

func runAsPersistedUser(user string, expected *persistedUserIdentity, lookup localUserIdentityLookup, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), externalCommandTimeout)
	defer cancel()
	cmd, label, err := commandAsPersistedUser(ctx, user, expected, lookup, name, args...)
	if err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s超时（%s）", label, externalCommandTimeout)
		}
		return commandFailed(label, err)
	}
	return nil
}

func outputAsPersistedUser(user string, expected *persistedUserIdentity, lookup localUserIdentityLookup, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), externalCmdTimeout)
	defer cancel()
	cmd, _, err := commandAsPersistedUser(ctx, user, expected, lookup, name, args...)
	if err != nil {
		return "", err
	}
	b, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return string(b), fmt.Errorf("以记录身份执行用户 %s 的命令 %s 超时（%s）", user, name, externalCmdTimeout)
	}
	return string(b), err
}

func commandAsPersistedUser(ctx context.Context, user string, expected *persistedUserIdentity, lookup localUserIdentityLookup, name string, args ...string) (*exec.Cmd, string, error) {
	if _, err := verifyPersistedUserIdentity(user, expected, lookup); err != nil {
		return nil, "", err
	}
	// The command identity is built exclusively from the durable record. A same-name
	// account replacement after the verification above cannot redirect exec to the
	// replacement account because no later username lookup or runuser/sudo occurs.
	identity := localUserIdentity{
		Name:    user,
		UID:     expected.UID,
		GID:     expected.GID,
		UIDText: strconv.Itoa(expected.UID),
		GIDText: strconv.Itoa(expected.GID),
		Home:    expected.Home,
	}
	return commandForUserIdentity(ctx, user, identity, name, args...)
}

func commandForUserIdentity(ctx context.Context, user string, identity localUserIdentity, name string, args ...string) (*exec.Cmd, string, error) {
	if err := ensureUserHomeUsable(identity.Home, user); err != nil {
		return nil, "", err
	}
	if identity.UID < 0 || identity.GID < 0 || uint64(identity.UID) > uint64(^uint32(0)) || uint64(identity.GID) > uint64(^uint32(0)) {
		return nil, "", fmt.Errorf("用户 %s 的数值身份超出系统调用范围：uid=%d gid=%d", user, identity.UID, identity.GID)
	}
	env := userConfigCommandEnvironment(identity)
	label := "以数值身份执行用户 " + user + " 的命令 " + name
	cmd := exec.CommandContext(ctx, name, args...)
	effectiveUID := userCommandEffectiveUID()
	effectiveGID := userCommandEffectiveGID()
	if effectiveUID == 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid:    uint32(identity.UID),
				Gid:    uint32(identity.GID),
				Groups: []uint32{},
			},
		}
	} else if effectiveUID != identity.UID || effectiveGID != identity.GID {
		return nil, "", fmt.Errorf(
			"当前进程不是 root，且有效身份 uid=%d gid=%d 与目标用户 %s 的记录 uid=%d gid=%d 不一致，拒绝通过用户名切换身份",
			effectiveUID,
			effectiveGID,
			user,
			identity.UID,
			identity.GID,
		)
	}
	cmd.Env = env
	cmd.Dir = identity.Home
	return cmd, label, nil
}

func userConfigCommandEnvironment(identity localUserIdentity) []string {
	// These commands configure another account. A blacklist cannot prevent new
	// tool-specific settings or credentials from crossing that identity boundary.
	// Keep PATH for installed tools (including Node), and make all other inputs
	// explicit. Never inherit loader/runtime hooks or credentials.
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"LANG=C",
		"LC_ALL=C",
		"HOME=" + identity.Home,
		"USER=" + identity.Name,
		"LOGNAME=" + identity.Name,
		"XDG_RUNTIME_DIR=/run/user/" + strconv.Itoa(identity.UID),
	}
}

// stdinReader 是进程级共享的标准输入读取器。共享单个 bufio.Reader 可避免每次
// ask 都新建 Reader 时丢失上一次读取已缓冲（read-ahead）的字节。
var stdinReader = bufio.NewReader(os.Stdin)

// ask 读取一行输入。第二个返回值在遇到 EOF 且没有任何输入时为 false，
// 调用方据此退出交互菜单，避免在 stdin 关闭时陷入死循环。
func ask(prompt string) (string, bool) {
	fmt.Print(prompt)
	s, err := stdinReader.ReadString('\n')
	s = strings.TrimSpace(s)
	if err != nil && s == "" {
		return "", false
	}
	return s, true
}

// commandExists 报告命令是否在当前 PATH 中可用。
func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func withFileLock(path string, fn func() error) error {
	if err := ensurePublicDir(filepath.Dir(path)); err != nil {
		return err
	}
	// O_NOFOLLOW：与本仓库其余特权写入一致地拒绝符号链接锁文件，避免 flock 到链接目标。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("锁路径必须是普通文件：%s", path)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// withFileLockOrInherited lets install.sh hand its already-held lock across
// exec without opening an unlocked commit/init window. The inherited fd is
// accepted only when it is the exact root-only regular lock inode and a
// non-blocking flock confirms this open-file description owns the lock.
func withFileLockOrInherited(path, envName string, fn func() error) error {
	rawFD := strings.TrimSpace(os.Getenv(envName))
	if rawFD == "" {
		return withFileLock(path, fn)
	}
	fd64, err := strconv.ParseInt(rawFD, 10, 32)
	if err != nil || fd64 < 3 {
		return fmt.Errorf("继承锁文件描述符无效（%s=%q）", envName, rawFD)
	}
	fd := int(fd64)
	if err := validateNoSymlinkComponents(path, true); err != nil {
		return fmt.Errorf("继承锁路径不安全：%w", err)
	}
	var fdStat syscall.Stat_t
	if err := syscall.Fstat(fd, &fdStat); err != nil {
		return fmt.Errorf("继承锁文件描述符不可用（%s=%d）：%w", envName, fd, err)
	}
	var pathStat syscall.Stat_t
	if err := syscall.Lstat(path, &pathStat); err != nil {
		return fmt.Errorf("继承锁路径不可用：%s：%w", path, err)
	}
	if fdStat.Mode&syscall.S_IFMT != syscall.S_IFREG || pathStat.Mode&syscall.S_IFMT != syscall.S_IFREG ||
		fdStat.Dev != pathStat.Dev || fdStat.Ino != pathStat.Ino {
		return fmt.Errorf("继承锁文件描述符与锁路径不是同一普通文件：%s", path)
	}
	if os.Geteuid() == 0 {
		if fdStat.Uid != 0 || os.FileMode(fdStat.Mode).Perm() != 0o600 {
			return fmt.Errorf("继承锁必须属于 root 且权限为 0600：%s", path)
		}
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("继承锁未由安装器持有：%s：%w", path, err)
	}
	return fn()
}

func safePath(path, field string, mustAbs bool) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%s 不能为空", field)
	}
	if mustAbs && !filepath.IsAbs(path) {
		return fmt.Errorf("%s 必须是绝对路径：%s", field, path)
	}
	if strings.ContainsAny(path, "\x00\n\r\t ") {
		return fmt.Errorf("%s 包含非法或不兼容字符，请不要包含空白字符", field)
	}
	clean := filepath.Clean(path)
	if clean != path {
		return fmt.Errorf("%s 必须使用规范化路径：%s", field, path)
	}
	return nil
}

func validatePrivilegedExecutable(path, field string) error {
	if err := safePath(path, field, true); err != nil {
		return err
	}
	if err := validateNoSymlinkComponents(path, true); err != nil {
		return fmt.Errorf("%s 不安全：%w", field, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s 不可用：%w", field, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s 必须是普通文件：%s", field, path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("无法校验 %s 属主：%s", field, path)
	}
	if st.Uid != 0 {
		return fmt.Errorf("%s 必须属于 root：%s（uid=%d）", field, path, st.Uid)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s 不能允许组或其他用户写入：%s（权限=%#o）", field, path, info.Mode().Perm())
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s 必须可执行：%s", field, path)
	}
	return nil
}

func safeCoreDir(path, field string) error {
	if err := safePath(path, field, true); err != nil {
		return err
	}
	clean := filepath.Clean(path)
	blockedExact := map[string]bool{
		"/": true, "/bin": true, "/boot": true, "/dev": true, "/etc": true, "/home": true,
		"/lib": true, "/lib64": true, "/media": true, "/mnt": true, "/opt": true, "/proc": true,
		"/root": true, "/run": true, "/sbin": true, "/srv": true, "/sys": true, "/tmp": true,
		"/usr": true, "/var": true, "/var/lib": true, "/var/opt": true, "/var/tmp": true,
	}
	if blockedExact[clean] {
		return fmt.Errorf("%s 不能使用系统目录本身：%s", field, path)
	}
	blockedPrefixes := []string{"/etc/", "/usr/", "/bin/", "/sbin/", "/lib/", "/lib64/", "/proc/", "/sys/", "/dev/", "/run/", "/home/", "/root/", "/tmp/", "/var/tmp/"}
	for _, prefix := range blockedPrefixes {
		if strings.HasPrefix(clean+string(os.PathSeparator), prefix) {
			return fmt.Errorf("%s 不能位于敏感系统目录下：%s", field, path)
		}
	}
	allowed := false
	for _, prefix := range []string{"/opt/", "/var/lib/", "/var/opt/"} {
		if strings.HasPrefix(clean+string(os.PathSeparator), prefix) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%s 必须位于 /opt、/var/lib 或 /var/opt 下的专用目录：%s", field, path)
	}
	if err := validateNoSymlinkComponents(clean, true); err != nil {
		return fmt.Errorf("%s 不安全：%w", field, err)
	}
	if info, err := os.Lstat(clean); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s 不能是符号链接：%s", field, path)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s 已存在但不是目录：%s", field, path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

var systemdServiceNameRE = regexp.MustCompile(`^[A-Za-z0-9_.@:-]+\.service$`)
var systemdPlainNameRE = regexp.MustCompile(`^[A-Za-z0-9_.@:-]+$`)

func safeSystemdServiceName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("systemd 服务名不能为空")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "..") || strings.ContainsAny(name, "\x00\n\r") {
		return fmt.Errorf("systemd 服务名不安全：%s", name)
	}
	if !systemdServiceNameRE.MatchString(name) {
		return fmt.Errorf("systemd 服务名必须以 .service 结尾且只包含安全字符：%s", name)
	}
	return nil
}

func safeProxysceneServiceName(name string) error {
	stem := strings.TrimSuffix(name, ".service")
	if stem == "proxyscene" || strings.HasPrefix(stem, "proxyscene-") || strings.HasPrefix(stem, "proxyscene@") {
		return nil
	}
	return fmt.Errorf("proxyscene systemd 服务名必须位于 proxyscene 命名空间：%s", name)
}

type systemdTargetName struct {
	UserMode bool
	User     string
	Service  string
}

func safeSystemdTargetName(name string) error {
	_, err := parseSystemdTargetName(name)
	return err
}

func parseSystemdTargetName(name string) (systemdTargetName, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return systemdTargetName{}, fmt.Errorf("目标服务名不能为空")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "..") || strings.ContainsAny(name, "\x00\n\r") {
		return systemdTargetName{}, fmt.Errorf("目标服务名不安全：%s", name)
	}
	if strings.HasPrefix(name, "user:") {
		rest := strings.TrimPrefix(name, "user:")
		if rest == "" {
			return systemdTargetName{}, fmt.Errorf("用户级服务名不能为空")
		}
		userName := "root"
		service := rest
		if parts := strings.SplitN(rest, ":", 2); len(parts) == 2 {
			userName = strings.TrimSpace(parts[0])
			service = strings.TrimSpace(parts[1])
			if userName == "" {
				return systemdTargetName{}, fmt.Errorf("用户级服务用户名不能为空")
			}
		}
		if strings.TrimSpace(service) == "" {
			return systemdTargetName{}, fmt.Errorf("用户级服务名不能为空")
		}
		if err := validateUserName(userName); err != nil {
			return systemdTargetName{}, err
		}
		normalized, err := normalizeTargetServiceName(service)
		if err != nil {
			return systemdTargetName{}, err
		}
		if err := safeSystemdServiceName(normalized); err != nil {
			return systemdTargetName{}, err
		}
		return systemdTargetName{UserMode: true, User: userName, Service: normalized}, nil
	}
	service, err := normalizeTargetServiceName(name)
	if err != nil {
		return systemdTargetName{}, err
	}
	if err := safeSystemdServiceName(service); err != nil {
		return systemdTargetName{}, err
	}
	return systemdTargetName{Service: service}, nil
}

// normalizeTargetServiceName 把简写服务名补全为 *.service。对于不含 .service 的简写，
// 拒绝包含 '@' 的形式：避免操作员误把 "foo@bar" 当普通服务，却被 systemd 解释为模板
// 实例单元。确需模板实例时请写完整的 name@instance.service。
func normalizeTargetServiceName(name string) (string, error) {
	if strings.HasSuffix(name, ".service") {
		return name, nil
	}
	if strings.Contains(name, "@") {
		return "", fmt.Errorf("模板实例服务名请使用完整形式 name@instance.service：%s", name)
	}
	if !systemdPlainNameRE.MatchString(name) {
		return "", fmt.Errorf("目标服务名包含非法字符：%s", name)
	}
	return normalizeSystemdServiceName(name), nil
}

func normalizeSystemdServiceName(name string) string {
	if strings.HasSuffix(name, ".service") {
		return name
	}
	return name + ".service"
}

type localUserIdentity struct {
	Name    string
	UID     int
	GID     int
	UIDText string
	GIDText string
	Home    string
}

func lookupLocalUserIdentity(userName string) (localUserIdentity, error) {
	if strings.TrimSpace(userName) == "" {
		return localUserIdentity{}, fmt.Errorf("用户名不能为空")
	}
	if err := validateUserName(userName); err != nil {
		return localUserIdentity{}, err
	}
	// 优先用 getent，以支持 NSS（LDAP/SSSD/systemd-userdb 等）用户。CGO_ENABLED=0 的
	// 静态二进制下 os/user 只读 /etc/passwd，无法解析 NSS，因此显式调用 getent；
	// getent 不可用或未命中时回退到直接解析 /etc/passwd，与 `id -u` 的判断保持一致。
	if line, ok := getentPasswd(userName); ok {
		if id, err := parsePasswdLine(line, userName); err == nil {
			return id, nil
		}
	}
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return localUserIdentity{}, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 || fields[0] != userName {
			continue
		}
		return parsePasswdLine(line, userName)
	}
	return localUserIdentity{}, fmt.Errorf("未找到系统用户：%s", userName)
}

// parsePasswdLine 解析一行 passwd 记录（name:passwd:uid:gid:gecos:home:shell）。
func parsePasswdLine(line, userName string) (localUserIdentity, error) {
	fields := strings.Split(line, ":")
	if len(fields) < 7 || fields[0] != userName {
		return localUserIdentity{}, fmt.Errorf("未找到系统用户：%s", userName)
	}
	if fields[2] == "" || fields[3] == "" || fields[5] == "" {
		return localUserIdentity{}, fmt.Errorf("用户 %s 的 passwd 记录不完整", userName)
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil || uid < 0 {
		return localUserIdentity{}, fmt.Errorf("用户 %s 的 UID 无效", userName)
	}
	gid, err := strconv.Atoi(fields[3])
	if err != nil || gid < 0 {
		return localUserIdentity{}, fmt.Errorf("用户 %s 的 GID 无效", userName)
	}
	return localUserIdentity{Name: userName, UID: uid, GID: gid, UIDText: fields[2], GIDText: fields[3], Home: fields[5]}, nil
}

// getentPasswd 通过 getent 查询单个用户的 passwd 记录，返回首行。validateUserName 已
// 限制 userName 字符集，作为独立 argv 传入，无注入风险。
func getentPasswd(userName string) (string, bool) {
	if _, err := exec.LookPath("getent"); err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), externalCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "getent", "passwd", userName).Output()
	if err != nil {
		return "", false
	}
	line := strings.TrimRight(string(out), "\n")
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if line == "" {
		return "", false
	}
	return line, true
}

func lookupLocalUser(userName string) (uid string, home string, err error) {
	identity, err := lookupLocalUserIdentity(userName)
	if err != nil {
		return "", "", err
	}
	return identity.UIDText, identity.Home, nil
}

func userHomeDir(userName string) (string, error) {
	_, home, err := lookupLocalUser(userName)
	return home, err
}

var (
	userSystemctlStat       = os.Stat
	userSystemctlExecutable = "systemctl"
)

func runUserSystemctlQuietPersisted(userName string, identity *persistedUserIdentity, lookup localUserIdentityLookup, args ...string) error {
	if len(args) == 3 && args[0] == "try-restart" && args[1] == "--" {
		return runSystemdRestart("重启用户级服务 user:"+userName+":"+args[2], args[2], func(ctx context.Context, commandArgs ...string) (*exec.Cmd, error) {
			cmd, _, err := commandUserSystemctlPersisted(ctx, userName, identity, lookup, commandArgs...)
			return cmd, err
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), externalCommandTimeout)
	defer cancel()
	cmd, label, err := commandUserSystemctlPersisted(ctx, userName, identity, lookup, args...)
	if err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s超时（%s）", label, externalCommandTimeout)
		}
		return commandFailed(label, err)
	}
	return nil
}

func outputUserSystemctlQuietPersisted(userName string, identity *persistedUserIdentity, lookup localUserIdentityLookup, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), externalCommandTimeout)
	defer cancel()
	cmd, label, err := commandUserSystemctlPersisted(ctx, userName, identity, lookup, args...)
	if err != nil {
		return "", err
	}
	b, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return string(b), fmt.Errorf("%s超时（%s）", label, externalCommandTimeout)
		}
		return string(b), commandFailed(label, err)
	}
	return string(b), nil
}

func commandUserSystemctlPersisted(ctx context.Context, userName string, identity *persistedUserIdentity, lookup localUserIdentityLookup, args ...string) (*exec.Cmd, string, error) {
	if _, err := verifyPersistedUserIdentity(userName, identity, lookup); err != nil {
		return nil, "", err
	}
	runtimeDir := "/run/user/" + strconv.Itoa(identity.UID)
	busPath := runtimeDir + "/bus"
	if _, err := userSystemctlStat(busPath); err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("用户 %s 的 systemd 用户总线未运行：%s", userName, busPath)
		}
		return nil, "", err
	}
	env := []string{"DBUS_SESSION_BUS_ADDRESS=unix:path=" + busPath}
	systemctlArgs := append([]string{"--user"}, args...)
	cmd, _, err := commandAsPersistedUser(ctx, userName, identity, lookup, userSystemctlExecutable, systemctlArgs...)
	if err != nil {
		return nil, "", err
	}
	cmd.Env = append(cmd.Env, env...)
	label := "以记录身份执行用户 " + userName + " 的 systemd 命令"
	return cmd, label, nil
}

// userBusAvailable 报告目标用户的 systemd 用户总线（/run/user/<uid>/bus）是否在运行。
func userBusAvailable(userName string) bool {
	uid, _, err := lookupLocalUser(userName)
	if err != nil {
		return false
	}
	_, err = os.Stat("/run/user/" + uid + "/bus")
	return err == nil
}

// warnIfUserBusMissing 在目标用户没有可用 systemd 用户总线时打印明确指引：用户级注入此时
// 不会生效、开机也不会自动恢复，需 loginctl enable-linger。把"静默跳过"升级为可见前置提示。
func warnIfUserBusMissing(userName string) {
	if userBusAvailable(userName) {
		return
	}
	fmt.Printf("提示：用户 %s 的 systemd 用户总线未运行——其用户级 Telegram 代理注入暂不会生效，开机也不会自动恢复；请先执行：loginctl enable-linger %s\n", userName, userName)
}

func validPort(port int, field string) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("%s 端口无效：%d", field, port)
	}
	return nil
}

func validateTestURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("代理测试地址不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("代理测试地址必须是 http(s) URL：%s", raw)
	}
	return nil
}

// validateProxyHost 校验代理监听地址，只接受 IPv4/IPv6 字面量：xray 的 listen 字段不接受
// 主机名，主机名会拖到启用场景做配置检查时才失败，这里提前拦截并给出明确报错。
// net.ParseIP 不接受 zone（%eth0），因此合法值的字符集是十六进制、点、冒号——这同时保证
// applyGlobal 的 %q 引用安全（见 scenes.go 的耦合注释）。IPv6 由 HTTPAddr 等经
// net.JoinHostPort 自动加方括号。
func validateProxyHost(host string, allowPublic bool) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("代理监听地址不能为空")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("代理监听地址必须是 IPv4/IPv6 字面量（xray 监听不支持主机名）：%s", host)
	}
	// 本地 HTTP/SOCKS 入站没有认证；绑定到非环回地址会把它暴露成开放代理。
	// 默认只允许环回，确需对外监听时须显式设置 PROXYSCENE_ALLOW_PUBLIC_BIND=1。
	if !ip.IsLoopback() && !allowPublic {
		return fmt.Errorf("代理监听地址 %s 非环回地址：无认证入站对外监听会形成开放代理；如确需，请设置 PROXYSCENE_ALLOW_PUBLIC_BIND=1", host)
	}
	return nil
}

func validateUserName(user string) error {
	if user == "" {
		return nil
	}
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,31}$`).MatchString(user) {
		return fmt.Errorf("用户名不安全：%s", user)
	}
	return nil
}

func systemdQuote(s string) string {
	q := `"`
	for _, r := range s {
		switch r {
		case '\\', '"':
			q += `\` + string(r)
		case '%':
			q += `%%`
		default:
			q += string(r)
		}
	}
	q += `"`
	return q
}
