package manager

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

var coreVerifyExecution = verifyCoreExecution
var coreCaptureBeforeProcess = captureCoreBeforeProcess

type coreProcessIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Digest string `json:"digest"`
	Start  string `json:"start"`
}

func captureCoreBeforeProcess(a *App, state coreServiceState, unit []byte, identity localUserIdentity) (*coreProcessIdentity, error) {
	if err := coreVerifyExecution(a, state, unit, identity, "", false); err != nil {
		return nil, err
	}
	if state.Active != "active" {
		return nil, nil
	}
	values, err := readCoreExecutionProperties(a)
	if err != nil {
		return nil, err
	}
	proof, err := readCoreProcessIdentity(a, state, identity, values["ControlGroup"])
	if err != nil {
		return nil, err
	}
	if err := coreVerifyExecution(a, state, unit, identity, "", false); err != nil {
		return nil, err
	}
	return proof, nil
}

var coreUnitSearchRoots = systemUnitSearchRoots
var coreProcRoot = "/proc"

// A managed core has one direct command and no drop-ins. Refusing overlays
// keeps the file and loaded-unit proof small and auditable, including overlays
// added on disk but not yet loaded by the manager.
func verifyCoreUnitLayout(a *App, unit []byte, identity localUserIdentity) error {
	roots := []unitSearchRoot{}
	for _, root := range coreUnitSearchRoots() {
		roots = append(roots, unitSearchRoot{Path: root, Manage: true})
	}
	path := coreUnitPath(a.cfg)
	if len(unit) > 0 {
		actual, err := readCoreFile(path)
		if err != nil {
			return err
		}
		if !actual.Present || !bytes.Equal(actual.Content, unit) {
			return fmt.Errorf("核心主unit在验证期间变化")
		}
		starts, err := effectiveServiceExecStarts(string(unit))
		if err != nil {
			return err
		}
		expected := []string{a.cfg.XrayBin(), "run", "-config", a.cfg.XrayConfig()}
		if len(starts) != 1 || !slices.Equal(starts[0], expected) {
			return fmt.Errorf("核心unit不是固定Xray配置的直接调用")
		}
		user, err := coreUnitServiceUser(unit)
		if err != nil {
			return err
		}
		if user != identity.Name {
			return fmt.Errorf("核心unit用户与持久身份不符")
		}
		for _, key := range []string{"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "Environment", "EnvironmentFile", "PassEnvironment", "Group", "SupplementaryGroups", "PAMName", "RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories", "NetworkNamespacePath"} {
			words, err := effectiveServiceWordList(string(unit), key)
			if err != nil {
				return err
			}
			if len(words) > 0 {
				return fmt.Errorf("核心unit含不支持的执行覆盖：%s", key)
			}
		}
		for _, key := range []string{"DynamicUser", "PrivateNetwork"} {
			word, err := effectiveServiceSingleWord(string(unit), key)
			if err != nil {
				return err
			}
			if word != "" && word != "no" && word != "false" {
				return fmt.Errorf("核心unit含不支持的%s", key)
			}
		}
		kind, err := effectiveServiceSingleWord(string(unit), "Type")
		if err != nil {
			return err
		}
		if kind != "" && kind != "simple" && kind != "exec" {
			return fmt.Errorf("核心unit Type不是单一前台进程")
		}
	}
	// Inspect only this unit's canonical/prefix/type-level directories. Loaded
	// aliases are rejected through Names; unrelated unit symlinks are never read.
	for _, dir := range effectiveDropInSearchDirs([]string{a.cfg.SystemdService}, roots) {
		info, err := os.Lstat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("核心drop-in路径不是普通目录")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".conf") {
				return fmt.Errorf("核心服务含不支持的systemd drop-in，拒绝修改或声明已加载")
			}
		}
	}
	return nil
}

var coreExecutionProperties = []string{"Id", "Names", "FragmentPath", "DropInPaths", "ExecStart", "User", "Group", "Type", "DynamicUser", "RootDirectory", "RootImage", "MainPID", "InvocationID", "ControlGroup", "NeedDaemonReload"}

func readCoreExecutionProperties(a *App) (map[string]string, error) {
	args := []string{"show"}
	for _, name := range coreExecutionProperties {
		args = append(args, "--property="+name)
	}
	args = append(args, "--", a.cfg.SystemdService)
	raw, err := systemctlOutput("验证核心有效unit与进程绑定", args...)
	if err != nil {
		return nil, err
	}
	if len(raw) > 64<<10 {
		return nil, fmt.Errorf("核心有效unit字段过大")
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || !containsString(coreExecutionProperties, key) {
			return nil, fmt.Errorf("核心有效unit字段无法解析")
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("核心有效unit字段重复：%s", key)
		}
		values[key] = value
	}
	for _, key := range coreExecutionProperties {
		if _, ok := values[key]; !ok {
			return nil, fmt.Errorf("核心有效unit字段缺失：%s", key)
		}
	}
	return values, nil
}

// systemctl prints ExecStart as one structured record. Only the fixed direct
// command is accepted; whitespace or escaping in installation paths is rejected
// here rather than interpreted differently from the manager's display format.
func coreExecStartMatches(value, bin, config string) bool {
	if strings.ContainsAny(bin+config, " \t\r\n\\;{}") {
		return false
	}
	prefix := "{ path=" + bin + " ; argv[]=" + bin + " run -config " + config + " ; ignore_errors=no ; "
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, " }") {
		return false
	}
	suffix := strings.TrimSuffix(strings.TrimPrefix(value, prefix), " }")
	if strings.ContainsAny(suffix, "{}\n\r") {
		return false
	}
	fields := strings.Split(suffix, " ; ")
	allowed := map[string]bool{"start_time": true, "stop_time": true, "pid": true, "code": true, "status": true}
	seen := map[string]bool{}
	for _, field := range fields {
		key, _, ok := strings.Cut(field, "=")
		if !ok || !allowed[key] || seen[key] {
			return false
		}
		seen[key] = true
	}
	return len(seen) == len(allowed)
}

func verifyCoreExecution(a *App, state coreServiceState, unit []byte, identity localUserIdentity, binaryDigest string, validateProcess bool) error {
	if err := validateCoreServiceState(state); err != nil {
		return err
	}
	if err := validateCoreIdentity(identity.Name, identity); err != nil {
		return err
	}
	if err := verifyCoreUnitLayout(a, unit, identity); err != nil {
		return err
	}
	if state.Load == "not-found" {
		if state.Active == "active" || len(unit) > 0 {
			return fmt.Errorf("核心unit磁盘与manager状态不符")
		}
		return nil
	}
	values, err := readCoreExecutionProperties(a)
	if err != nil {
		return err
	}
	user := values["User"]
	if user == "" {
		user = "root"
	}
	if values["Id"] != a.cfg.SystemdService || !slices.Equal(strings.Fields(values["Names"]), []string{a.cfg.SystemdService}) || values["FragmentPath"] != coreUnitPath(a.cfg) || values["DropInPaths"] != "" || user != identity.Name || values["Group"] != "" || values["DynamicUser"] != "no" || values["RootDirectory"] != "" || values["RootImage"] != "" || (values["Type"] != "simple" && values["Type"] != "exec") || !coreExecStartMatches(values["ExecStart"], a.cfg.XrayBin(), a.cfg.XrayConfig()) {
		return fmt.Errorf("核心有效ExecStart、User或文件视图与固定计划不符，拒绝运行")
	}
	if values["MainPID"] != strconv.Itoa(state.PID) || values["InvocationID"] != state.Invocation || values["NeedDaemonReload"] != state.NeedReload {
		return fmt.Errorf("核心运行代际在验证期间变化")
	}
	if validateProcess && state.Active == "active" {
		if err := verifyCoreProcess(a, state, identity, values["ControlGroup"], binaryDigest); err != nil {
			return err
		}
	}
	after, err := readCoreExecutionProperties(a)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(values, after) {
		return fmt.Errorf("核心有效unit在身份验证期间变化")
	}
	final, err := coreReadService(a)
	if err != nil {
		return err
	}
	if final != state {
		return fmt.Errorf("核心运行身份在验证期间变化")
	}
	return nil
}

func readCoreProcFile(dirFD int, name string, max int64) ([]byte, error) {
	fd, err := syscall.Openat(dirFD, name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("核心proc字段不是普通文件")
	}
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, fmt.Errorf("核心proc字段超过长度限制")
	}
	return raw, nil
}
func coreProcStart(raw []byte, pid int) (string, error) {
	close := bytes.LastIndexByte(raw, ')')
	open := bytes.IndexByte(raw, '(')
	if open < 1 || close < open || strings.TrimSpace(string(raw[:open])) != strconv.Itoa(pid) {
		return "", fmt.Errorf("核心proc stat无法解析")
	}
	fields := strings.Fields(string(raw[close+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("核心proc stat缺少启动身份")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return "", fmt.Errorf("核心proc启动身份无效")
	}
	return fields[19], nil
}
func coreProcCredentials(raw []byte, identity localUserIdentity) error {
	fields := map[string][]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key != "Uid" && key != "Gid" {
			continue
		}
		if fields[key] != nil {
			return fmt.Errorf("核心进程身份字段重复")
		}
		fields[key] = strings.Fields(value)
	}
	for key, want := range map[string]int{"Uid": identity.UID, "Gid": identity.GID} {
		values := fields[key]
		if len(values) != 4 {
			return fmt.Errorf("核心进程身份字段缺失")
		}
		for _, value := range values {
			if value != strconv.Itoa(want) {
				return fmt.Errorf("核心进程%s与服务账号不符", key)
			}
		}
	}
	return nil
}
func coreProcCgroupMatches(raw []byte, expected string) bool {
	if expected == "" || !filepath.IsAbs(expected) || filepath.Clean(expected) != expected || strings.ContainsAny(expected, "\r\n\x00") {
		return false
	}
	matched := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			return false
		}
		if fields[0] == "0" && fields[1] == "" || containsString(strings.Split(fields[1], ","), "name=systemd") {
			if fields[2] != expected {
				return false
			}
			matched = true
		}
	}
	return matched
}
func verifyCoreProcess(a *App, state coreServiceState, identity localUserIdentity, cgroup, binaryDigest string) error {
	proof, err := readCoreProcessIdentity(a, state, identity, cgroup)
	if err != nil {
		return err
	}
	current, err := readCoreMetadata(a.cfg.XrayBin())
	if err != nil {
		return err
	}
	if proof.Device != current.Device || proof.Inode != current.Inode || proof.Digest != binaryDigest {
		return fmt.Errorf("核心进程执行的二进制不是已安装候选版本")
	}
	return nil
}

// A still-running old executable can survive an installer's atomic replacement.
// Bind that process separately; only the strict verifier above can declare the
// installed candidate loaded. The old executable is never re-executed here.
func readCoreProcessIdentity(a *App, state coreServiceState, identity localUserIdentity, cgroup string) (*coreProcessIdentity, error) {
	if state.PID <= 0 {
		return nil, fmt.Errorf("核心MainPID无效")
	}
	procDir := filepath.Join(coreProcRoot, strconv.Itoa(state.PID))
	dirFD, err := syscall.Open(procDir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(dirFD)
	stat, err := readCoreProcFile(dirFD, "stat", 64<<10)
	if err != nil {
		return nil, err
	}
	start, err := coreProcStart(stat, state.PID)
	if err != nil {
		return nil, err
	}
	argv, err := readCoreProcFile(dirFD, "cmdline", 64<<10)
	if err != nil {
		return nil, err
	}
	expected := []byte(strings.Join([]string{a.cfg.XrayBin(), "run", "-config", a.cfg.XrayConfig()}, "\x00") + "\x00")
	if !bytes.Equal(argv, expected) {
		return nil, fmt.Errorf("核心进程argv未绑定计划内Xray和配置")
	}
	credentials, err := readCoreProcFile(dirFD, "status", 64<<10)
	if err != nil {
		return nil, err
	}
	if err := coreProcCredentials(credentials, identity); err != nil {
		return nil, err
	}
	groups, err := readCoreProcFile(dirFD, "cgroup", 64<<10)
	if err != nil {
		return nil, err
	}
	if !coreProcCgroupMatches(groups, cgroup) {
		return nil, fmt.Errorf("核心进程不在已加载unit的cgroup")
	}
	// Following this single /proc magic link is intentional. The PID directory is
	// held open. Capture the trusted executed inode independently from the
	// installed path; the strict loaded verifier additionally requires equality.
	exeFD, err := syscall.Openat(dirFD, "exe", syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	exe := os.NewFile(uintptr(exeFD), "core process executable")
	defer exe.Close()
	info, err := exe.Stat()
	if err != nil {
		return nil, err
	}
	st := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 512<<20 || st.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0111 == 0 {
		return nil, fmt.Errorf("核心进程可执行文件不是可信root拥有的ELF")
	}
	target, err := os.Readlink(filepath.Join(procDir, "exe"))
	if err != nil {
		return nil, err
	}
	if target != a.cfg.XrayBin() && target != a.cfg.XrayBin()+" (deleted)" {
		return nil, fmt.Errorf("核心进程可执行文件不属于受管安装路径")
	}
	executable, err := elf.NewFile(exe)
	if err != nil {
		return nil, fmt.Errorf("核心旧进程不是有效ELF可执行文件")
	}
	defer executable.Close()
	if (executable.Type != elf.ET_EXEC && executable.Type != elf.ET_DYN) || !elfMachineMatchesRuntime(executable.Machine) {
		return nil, fmt.Errorf("核心旧进程ELF类型或架构不符")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(exe, (512<<20)+1))
	if err != nil {
		return nil, err
	}
	if n != info.Size() {
		return nil, fmt.Errorf("核心进程可执行文件大小在读取期间变化")
	}
	digest := hex.EncodeToString(h.Sum(nil))
	after, err := readCoreProcFile(dirFD, "stat", 64<<10)
	if err != nil {
		return nil, err
	}
	afterStart, err := coreProcStart(after, state.PID)
	if err != nil {
		return nil, err
	}
	afterInfo, err := exe.Stat()
	if err != nil {
		return nil, err
	}
	if afterStart != start || info.Size() != afterInfo.Size() || info.ModTime() != afterInfo.ModTime() || info.Sys().(*syscall.Stat_t).Ctim != afterInfo.Sys().(*syscall.Stat_t).Ctim {
		return nil, fmt.Errorf("核心进程在身份验证期间变化")
	}
	return &coreProcessIdentity{Device: uint64(st.Dev), Inode: st.Ino, Digest: digest, Start: start}, nil
}
