package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const maxDevConfigBytes int64 = 8 << 20

// A failed durability barrier or post-write verification can occur after the
// final name already changed. The caller must journal and attempt rollback even
// when this was its first configuration mutation.
var errDevConfigMutationUncertain = errors.New("开发配置修改可能已提交")

var (
	devReadNPMConfig   = readDevNPMConfig
	devMutateNPMConfig = mutateDevNPMConfig
	devMutateGitConfig = mutateDevGitConfig
)

// readDevConfig pins every directory below the recorded home and never follows
// the final name. In particular, a root npm invocation must not read a .npmrc
// symlink or an unrelated owner's file and subsequently copy its contents.
func readDevConfig(user string, identity *persistedUserIdentity, path string) ([]byte, bool, *userFileMetadata, error) {
	current, err := verifyPersistedUserIdentity(user, identity, devLookupUserIdentity)
	if err != nil {
		return nil, false, nil, err
	}
	clean, dirFD, err := openUserFileDirForIdentity(user, current, path, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil, nil
	}
	if err != nil {
		return nil, false, nil, err
	}
	defer syscall.Close(dirFD)
	return readDevConfigAt(dirFD, filepath.Base(clean), identity.UID)
}

func readDevConfigAt(dirFD int, name string, uid int) ([]byte, bool, *userFileMetadata, error) {
	fd, err := syscall.Openat(dirFD, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		if err := ensureDevConfigNoQuarantine(dirFD); err != nil {
			return nil, false, nil, err
		}
		return nil, false, nil, nil
	}
	if err != nil {
		return nil, false, nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	metadata, err := readUserFileMetadata(fd)
	if err != nil {
		return nil, false, nil, err
	}
	if metadata.UID != uid {
		return nil, false, nil, fmt.Errorf("用户配置必须是记录用户拥有的非符号链接普通文件：%s", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDevConfigBytes+1))
	if err != nil {
		return nil, false, nil, err
	}
	if int64(len(data)) > maxDevConfigBytes {
		return nil, false, nil, fmt.Errorf("用户配置超过大小限制：%s", name)
	}
	if err := metadata.verifyFD(fd, true); err != nil {
		return nil, false, nil, err
	}
	return data, true, metadata, nil
}

func commitDevConfig(user string, identity *persistedUserIdentity, path string, before, after []byte, exists bool, metadata *userFileMetadata) error {
	if bytes.Equal(before, after) {
		return nil
	}
	if int64(len(after)) > maxDevConfigBytes {
		return fmt.Errorf("用户配置超过大小限制")
	}
	var err error
	if exists {
		err = writeUserFileAtomicCASMetadataPersisted(user, identity, devLookupUserIdentity, path, before, after, metadata)
	} else {
		err = writeDevConfigCreatePersisted(user, identity, path, after)
	}
	if err != nil {
		return errors.Join(errDevConfigMutationUncertain, err)
	}
	return nil
}

type npmConfigLine struct {
	start, end int
	key        string
}
type npmProxyConfig struct {
	values map[string]*string
	lines  []npmConfigLine
}

func npmINITrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r != '\u0085' && unicode.IsSpace(r) || r == '\ufeff' })
}

// npm's ini parser only unescapes backslash, semicolon and hash in unquoted
// scalars. Quoted scalars use JSON string syntax (single quotes wrap the input
// before the JSON attempt). Keeping this small parser limited to the two proxy
// keys avoids executing npm with user-controlled cache/logs-dir/userconfig etc.
func npmINIScalar(s string) (string, error) {
	s = npmINITrim(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		if s[0] == '\'' {
			s = s[1 : len(s)-1]
		}
		var v any
		if json.Unmarshal([]byte(s), &v) == nil {
			value, ok := v.(string)
			if !ok {
				return "", fmt.Errorf("npm proxy 仅支持字符串或未设置值")
			}
			return value, nil
		}
		return s, nil
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ';' || c == '#' {
			break
		}
		if c == '\\' && i+1 < len(s) {
			next := s[i+1]
			if next == '\\' || next == ';' || next == '#' {
				out.WriteByte(next)
				i++
				continue
			}
		}
		out.WriteByte(c)
	}
	return npmINITrim(out.String()), nil
}

func parseNPMProxyConfig(data []byte) (npmProxyConfig, error) {
	result := npmProxyConfig{values: map[string]*string{}}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 || bytes.ContainsAny(data, "\u2028\u2029") {
		return result, fmt.Errorf("npm 用户配置含无效 UTF-8、NUL 或不支持的 Unicode 行分隔符")
	}
	inSection := false
	for start := 0; start < len(data); {
		end := start
		for end < len(data) && data[end] != '\n' && data[end] != '\r' {
			end++
		}
		next := end
		if next < len(data) {
			next++
			if data[end] == '\r' && next < len(data) && data[next] == '\n' {
				next++
			}
		}
		line := string(data[start:end])
		trimmed := npmINITrim(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ";") || strings.HasPrefix(trimmed, "#") {
			start = next
			continue
		}
		// ini recognizes a section only at the start of a physical line.
		if strings.HasPrefix(line, "[") && strings.HasSuffix(npmINITrim(line), "]") {
			close := strings.IndexByte(line, ']')
			if close > 0 && npmINITrim(line[close+1:]) == "" {
				section, err := npmINIScalar(line[1:close])
				if err != nil {
					return result, err
				}
				if section == "proxy" || section == "https-proxy" || strings.HasPrefix(section, "proxy.") || strings.HasPrefix(section, "https-proxy.") || strings.Contains(section, "__proto__") {
					return result, fmt.Errorf("npm proxy 不能是配置节或对象")
				}
				inSection = true
				start = next
				continue
			}
		}
		if inSection {
			start = next
			continue
		}
		rawKey, rawValue, hasValue := strings.Cut(line, "=")
		key, err := npmINIScalar(rawKey)
		if err != nil {
			start = next
			continue
		}
		if key == "proxy[]" || key == "https-proxy[]" {
			return result, fmt.Errorf("npm proxy 不支持数组值")
		}
		if key != "proxy" && key != "https-proxy" {
			start = next
			continue
		}
		if !hasValue {
			return result, fmt.Errorf("npm proxy 必须有显式字符串值")
		}
		value, err := npmINIScalar(rawValue)
		if err != nil {
			return result, err
		}
		if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return result, fmt.Errorf("npm proxy 含控制字符")
		}
		if strings.Contains(value, "${") {
			return result, fmt.Errorf("npm proxy 含环境变量插值，无法证明精确恢复")
		}
		if value == "true" || value == "false" {
			return result, fmt.Errorf("npm proxy 不支持布尔值")
		}
		if value == "" || value == "null" || value == "undefined" {
			result.values[key] = nil
		} else {
			if err := validateDevProxyValue(key, &value); err != nil {
				return result, err
			}
			result.values[key] = &value
		}
		result.lines = append(result.lines, npmConfigLine{start: start, end: next, key: key})
		start = next
	}
	return result, nil
}

func readDevNPMConfig(user string, identity *persistedUserIdentity, key string) (*string, error) {
	if err := validatePersistedUserIdentity(user, identity); err != nil {
		return nil, err
	}
	if key != "proxy" && key != "https-proxy" {
		return nil, fmt.Errorf("不支持的 npm 配置键")
	}
	data, _, _, err := readDevConfig(user, identity, filepath.Join(identity.Home, ".npmrc"))
	if err != nil {
		return nil, err
	}
	parsed, err := parseNPMProxyConfig(data)
	if err != nil {
		return nil, err
	}
	return cloneStringPointer(parsed.values[key]), nil
}

func mutateDevNPMConfig(user string, identity *persistedUserIdentity, key string, expected, desired *string) error {
	if err := validatePersistedUserIdentity(user, identity); err != nil {
		return err
	}
	if key != "proxy" && key != "https-proxy" {
		return fmt.Errorf("不支持的 npm 配置键")
	}
	path := filepath.Join(identity.Home, ".npmrc")
	data, exists, metadata, err := readDevConfig(user, identity, path)
	if err != nil {
		return err
	}
	parsed, err := parseNPMProxyConfig(data)
	if err != nil {
		return err
	}
	if !optionalStringsEqual(parsed.values[key], expected) {
		return errUserFileChanged
	}
	if optionalStringsEqual(expected, desired) {
		return nil
	}
	var updated bytes.Buffer
	if desired != nil {
		if err := validateDevProxyValue(key, desired); err != nil {
			return err
		}
		encoded, _ := json.Marshal(*desired)
		fmt.Fprintf(&updated, "%s=%s\n", key, encoded)
	}
	cursor := 0
	for _, line := range parsed.lines {
		if line.key != key {
			continue
		}
		updated.Write(data[cursor:line.start])
		cursor = line.end
	}
	updated.Write(data[cursor:])
	check, err := parseNPMProxyConfig(updated.Bytes())
	if err != nil {
		return err
	}
	if !optionalStringsEqual(check.values[key], desired) {
		return fmt.Errorf("npm 用户配置写入值无法无损表达")
	}
	if err := commitDevConfig(user, identity, path, data, updated.Bytes(), exists, metadata); err != nil {
		return err
	}
	actual, err := readDevNPMConfig(user, identity, key)
	if err != nil {
		return errors.Join(errDevConfigMutationUncertain, err)
	}
	if !optionalStringsEqual(actual, desired) {
		return errors.Join(errDevConfigMutationUncertain, errUserFileChanged)
	}
	return nil
}

// Git's normal writers coordinate through <config>.lock. Publish a fully
// initialized marker inode with a held flock, so only our own abandoned lock
// can be recovered after a crash; an ordinary Git lock is never removed.
func withDevGitLock(user string, identity *persistedUserIdentity, path string, fn func() error) (retErr error) {
	current, err := verifyPersistedUserIdentity(user, identity, devLookupUserIdentity)
	if err != nil {
		return err
	}
	clean, dirFD, err := openUserFileDirForIdentity(user, current, path, true)
	if err != nil {
		return err
	}
	defer syscall.Close(dirFD)
	lockName := filepath.Base(clean) + ".lock"
	marker := []byte("proxyscene Git configuration lock v1\n" + clean + "\n")
	tmp, fd, err := createTempFileAt(dirFD, "proxyscene-git-lock", 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), tmp)
	defer file.Close()
	defer syscall.Unlinkat(dirFD, tmp)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	if _, err := file.Write(marker); err != nil {
		return err
	}
	if err := file.Chown(identity.UID, identity.GID); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	published := false
	for attempt := 0; attempt < 3; attempt++ {
		err = unix.Linkat(dirFD, tmp, dirFD, lockName, 0)
		if err == nil {
			published = true
			break
		}
		if !errors.Is(err, unix.EEXIST) {
			return err
		}
		if err := recoverDevGitLock(dirFD, lockName, marker, identity.UID); err != nil {
			return err
		}
	}
	if !published {
		return fmt.Errorf("开发代理：Git 配置锁正被并发创建")
	}
	defer func() {
		var held, named unix.Stat_t
		if err := unix.Fstat(fd, &held); err != nil {
			retErr = errors.Join(retErr, err)
			return
		}
		if err := unix.Fstatat(dirFD, lockName, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			retErr = errors.Join(retErr, err)
			return
		}
		if held.Ino != named.Ino || held.Dev != named.Dev {
			retErr = errors.Join(retErr, fmt.Errorf("开发代理：Git 配置锁在事务期间已被替换"))
			return
		}
		retErr = errors.Join(retErr, syscall.Unlinkat(dirFD, lockName), syscall.Fsync(dirFD))
	}()
	// Only the recognizable lock name needs to survive a process crash.
	if err := syscall.Unlinkat(dirFD, tmp); err != nil {
		return err
	}
	if err := syscall.Fsync(dirFD); err != nil {
		return err
	}
	return fn()
}

func recoverDevGitLock(dirFD int, name string, marker []byte, uid int) error {
	fd, err := syscall.Openat(dirFD, name, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("开发代理：Git 配置锁不可用：%w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || int(stat.Uid) != uid || info.Size() != int64(len(marker)) {
		return fmt.Errorf("开发代理：Git 配置已锁定，保留其它写入者的锁")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("开发代理：Git 配置事务仍在运行：%w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(len(marker))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(data, marker) {
		return fmt.Errorf("开发代理：Git 配置已锁定，保留其它写入者的锁")
	}
	var named unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if named.Ino != stat.Ino || named.Dev != stat.Dev {
		return fmt.Errorf("开发代理：Git 配置锁在恢复期间变化")
	}
	if err := syscall.Unlinkat(dirFD, name); err != nil {
		return err
	}
	return syscall.Fsync(dirFD)
}

func mutateDevGitConfig(user string, identity *persistedUserIdentity, location devGitConfigLocation, expected []string, args ...string) error {
	if len(args) < 3 || args[1] != "--" {
		return fmt.Errorf("开发代理：Git 配置修改参数无效")
	}
	key := args[2]
	if key != "http.proxy" && key != "https.proxy" {
		return fmt.Errorf("不支持的 Git 配置键")
	}
	path, err := devGitConfigPath(identity, location)
	if err != nil {
		return err
	}
	committed := false
	err = withDevGitLock(user, identity, path, func() error {
		if err := devValidateGitTopology(user, identity, location); err != nil {
			return err
		}
		data, exists, metadata, err := readDevConfig(user, identity, path)
		if err != nil {
			return err
		}
		// The editing process only sees this isolated snapshot. Its own Git lock
		// is separate from the real config lock held for the whole transaction.
		dir, err := os.MkdirTemp("/tmp", "proxyscene-dev-git-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		// The invoking identity owns the outer directory. A different target
		// user can edit config, but cannot replace its parent directory.
		if err := os.Chmod(dir, 0o711); err != nil {
			return err
		}
		inner := filepath.Join(dir, "edit")
		if err := os.Mkdir(inner, 0o700); err != nil {
			return err
		}
		innerFD, err := syscall.Open(inner, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer syscall.Close(innerFD)
		tempPath := filepath.Join(inner, "config")
		if err := os.WriteFile(tempPath, data, 0o600); err != nil {
			return err
		}
		if err := os.Chown(tempPath, identity.UID, identity.GID); err != nil {
			return err
		}
		if err := syscall.Fchown(innerFD, identity.UID, identity.GID); err != nil {
			return err
		}
		out, err := devOutputAsUser(user, identity, "git", "config", "--file", tempPath, "--no-includes", "--null", "--get-all", "--", key)
		if err != nil && commandExitCode(err) != 1 {
			return err
		}
		values, err := parseGitNullOutput(out)
		if err != nil {
			return err
		}
		if !slices.Equal(values, expected) {
			return errUserFileChanged
		}
		commandArgs := append([]string{"config", "--file", tempPath, "--no-includes"}, args...)
		if err := devRunAsUser(user, identity, "git", commandArgs...); err != nil {
			return err
		}
		updated, present, _, err := readDevConfigAt(innerFD, "config", identity.UID)
		if err != nil {
			return err
		}
		if !present {
			return errUserFileChanged
		}
		if err := devValidateGitTopology(user, identity, location); err != nil {
			return err
		}
		if err := commitDevConfig(user, identity, path, data, updated, exists, metadata); err != nil {
			return err
		}
		committed = true
		return nil
	})
	if err != nil && committed {
		return errors.Join(errDevConfigMutationUncertain, err)
	}
	return err
}
