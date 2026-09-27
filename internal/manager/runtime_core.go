package manager

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
)

// These hooks keep privileged integration tests inside temporary roots.
var coreUnitPath = func(cfg Config) string { return filepath.Join("/etc/systemd/system", cfg.SystemdService) }
var coreCheckConfig = func(a *App, path string) error { return a.checkXrayConfigAt(path) }
var coreLookupIdentity = lookupLocalUserIdentity
var coreReadService = readCoreServiceState

type coreFileState struct {
	Present bool   `json:"present"`
	Content []byte `json:"content,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
	UID     uint32 `json:"uid,omitempty"`
	GID     uint32 `json:"gid,omitempty"`
}

func coreFileFrom(s globalProxyArtifactState) coreFileState {
	return coreFileState{s.present, s.content, uint32(s.mode.Perm()), s.uid, s.gid}
}
func (s coreFileState) artifact() globalProxyArtifactState {
	return globalProxyArtifactState{present: s.Present, content: s.Content, mode: os.FileMode(s.Mode), uid: s.UID, gid: s.GID}
}
func readCoreFile(path string) (coreFileState, error) {
	s, err := readGlobalProxyArtifactStateNoFollow(path, maxGlobalProxyArtifactBytes)
	return coreFileFrom(s), err
}
func coreFilesEqual(a, b coreFileState) bool {
	return globalProxyArtifactStatesEqual(a.artifact(), b.artifact())
}
func setCoreFile(path string, before, after coreFileState) error {
	if coreFilesEqual(before, after) {
		return nil
	}
	if after.Present {
		return writeGlobalProxyArtifactCAS(path, []globalProxyArtifactState{before.artifact()}, after.artifact())
	}
	_, err := removeGlobalProxyArtifactCAS(path, []globalProxyArtifactState{before.artifact()})
	return err
}

type coreMetadata struct {
	Mode   uint32 `json:"mode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func readCoreMetadata(path string) (coreMetadata, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return coreMetadata{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return coreMetadata{}, fmt.Errorf("核心路径不能是符号链接：%s", path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return coreMetadata{}, fmt.Errorf("无法读取核心文件身份")
	}
	return coreMetadata{uint32(info.Mode().Perm()), st.Uid, st.Gid, uint64(st.Dev), st.Ino}, nil
}
func setCoreMetadata(path string, before, after coreMetadata) error {
	if before == after {
		return nil
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG && st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return fmt.Errorf("核心元数据目标不是文件或目录")
	}
	current := coreMetadata{st.Mode & 0777, st.Uid, st.Gid, uint64(st.Dev), st.Ino}
	if current == after {
		return nil
	}
	if current != before {
		if current.Device != before.Device || current.Inode != before.Inode ||
			(current.Mode != before.Mode && current.Mode != after.Mode) ||
			(current.UID != before.UID && current.UID != after.UID) ||
			(current.GID != before.GID && current.GID != after.GID) {
			return fmt.Errorf("核心文件身份或权限已变化，保留现状：%s", path)
		}
	}
	if err := syscall.Fchown(fd, int(after.UID), int(after.GID)); err != nil {
		return err
	}
	if err := syscall.Fchmod(fd, after.Mode); err != nil {
		return err
	}
	return syscall.Fsync(fd)
}

type coreServiceState struct {
	Load       string `json:"load"`
	Active     string `json:"active"`
	Sub        string `json:"sub"`
	Enabled    string `json:"enabled"`
	Invocation string `json:"invocation"`
	NeedReload string `json:"need_reload"`
	PID        int    `json:"pid"`
}

func readCoreServiceState(a *App) (coreServiceState, error) {
	raw, err := systemctlOutput("读取核心运行身份", "show", "--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=UnitFileState", "--property=InvocationID", "--property=NeedDaemonReload", "--property=MainPID", "--", a.cfg.SystemdService)
	if err != nil {
		return coreServiceState{}, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if _, exists := values[k]; exists {
			return coreServiceState{}, fmt.Errorf("核心状态字段重复：%s", k)
		}
		values[k] = v
	}
	s := coreServiceState{Load: values["LoadState"], Active: values["ActiveState"], Sub: values["SubState"], Enabled: values["UnitFileState"], Invocation: values["InvocationID"], NeedReload: values["NeedDaemonReload"]}
	for _, key := range []string{"LoadState", "ActiveState", "SubState", "UnitFileState", "InvocationID", "NeedDaemonReload", "MainPID"} {
		if _, ok := values[key]; !ok {
			return s, fmt.Errorf("核心状态字段缺失：%s", key)
		}
	}
	var pidErr error
	s.PID, pidErr = strconv.Atoi(values["MainPID"])
	if pidErr != nil {
		return s, fmt.Errorf("核心MainPID无法解析")
	}
	if err := validateCoreServiceState(s); err != nil {
		return s, err
	}

	return s, nil
}

func validateCoreServiceState(s coreServiceState) error {
	if s.Load != "loaded" && s.Load != "not-found" {
		return fmt.Errorf("核心 unit 未正常加载：%s", s.Load)
	}
	if s.Active != "active" && s.Active != "inactive" && s.Active != "failed" {
		return fmt.Errorf("核心服务状态无法确认：%s", s.Active)
	}
	if s.Active == "active" && (s.Sub != "running" || s.PID <= 0 || len(s.Invocation) != 32 || strings.Trim(s.Invocation, "0123456789abcdef") != "") {
		return fmt.Errorf("核心运行身份无法确认")
	}
	switch s.Enabled {
	case "enabled", "disabled", "":
	default:
		return fmt.Errorf("核心启用状态无法安全协调：%s", s.Enabled)
	}
	if s.Load == "loaded" && (s.Enabled == "" || (s.NeedReload != "yes" && s.NeedReload != "no")) {
		return fmt.Errorf("核心 unit 状态字段不完整")
	}
	if s.PID < 0 || s.Active != "active" && s.PID != 0 {
		return fmt.Errorf("核心MainPID与服务状态不符")
	}
	if s.Invocation != "" && (len(s.Invocation) != 32 || strings.Trim(s.Invocation, "0123456789abcdef") != "" || s.Invocation == strings.Repeat("0", 32)) {
		return fmt.Errorf("核心InvocationID无效")
	}
	if s.Active == "inactive" && s.Sub != "dead" || s.Active == "failed" && s.Sub != "failed" {
		return fmt.Errorf("核心停止状态无法安全恢复")
	}
	return nil
}

func validateCoreIdentity(name string, identity localUserIdentity) error {
	if validateUserName(name) != nil || identity.Name != name || identity.UID < 0 || identity.GID < 0 || uint64(identity.UID) > uint64(maxGlobalProxyOwnershipID) || uint64(identity.GID) > uint64(maxGlobalProxyOwnershipID) || identity.UIDText != strconv.Itoa(identity.UID) || identity.GIDText != strconv.Itoa(identity.GID) {
		return fmt.Errorf("核心服务账号记录无效")
	}
	if name != "root" && (identity.UID == 0 || identity.GID == 0) {
		return fmt.Errorf("非root核心服务账号不能使用root身份")
	}
	return validatePersistedUserIdentity(name, &persistedUserIdentity{UID: identity.UID, GID: identity.GID, Home: identity.Home})
}

type coreLoadedReceipt struct {
	Version    int    `json:"version"`
	Config     string `json:"config"`
	Unit       string `json:"unit"`
	Binary     string `json:"binary"`
	Invocation string `json:"invocation"`
}

func contentDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func coreBinaryDigest(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 512<<20 {
		return "", fmt.Errorf("核心可执行文件类型或大小无效")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (512<<20)+1))
	if err != nil {
		return "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	if n > 512<<20 || n != info.Size() || after.Size() != info.Size() || after.ModTime() != info.ModTime() {
		return "", fmt.Errorf("核心可执行文件在读取期间变化或过大")
	}
	beforeStat := info.Sys().(*syscall.Stat_t)
	afterStat := after.Sys().(*syscall.Stat_t)
	if beforeStat.Ctim != afterStat.Ctim {
		return "", fmt.Errorf("核心可执行文件在读取期间变化")
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
func (a *App) coreReceiptPath() string { return filepath.Join(a.cfg.CoreDir, "core-loaded.json") }
func (a *App) coreLoadedMatches(p *runtimeCorePlan, s coreServiceState) bool {
	if s.Load != "loaded" || s.Active != "active" || s.Sub != "running" || s.Enabled != "enabled" || s.NeedReload != "no" {
		return false
	}
	data, err := readRegularFileNoFollow(a.coreReceiptPath(), 64<<10)
	if err != nil {
		return false
	}
	var r coreLoadedReceipt
	if json.Unmarshal(data, &r) != nil {
		return false
	}
	return coreVerifyExecution(a, s, p.AfterUnit.Content, p.Identity, p.BinaryDigest, true) == nil && r.Version == 1 && r.Config == contentDigest(p.AfterConfig.Content) && r.Unit == contentDigest(p.AfterUnit.Content) && r.Binary == p.BinaryDigest && r.Invocation == s.Invocation
}
func (a *App) saveCoreLoaded(p *runtimeCorePlan) error {
	s, err := coreReadService(a)
	if err != nil {
		return err
	}
	if s.Load != "loaded" || s.Active != "active" || s.Sub != "running" || s.NeedReload != "no" || s.Enabled != "enabled" {
		return fmt.Errorf("核心新运行配置尚未确认")
	}
	for path, want := range map[string]coreFileState{a.cfg.XrayConfig(): p.AfterConfig, coreUnitPath(a.cfg): p.AfterUnit} {
		got, err := readCoreFile(path)
		if err != nil {
			return err
		}
		if !coreFilesEqual(got, want) {
			return fmt.Errorf("核心文件在启动期间变化：%s", path)
		}
	}
	digest, err := coreBinaryDigest(a.cfg.XrayBin())
	if err != nil {
		return err
	}
	if digest != p.BinaryDigest {
		return fmt.Errorf("核心二进制在启动期间变化")
	}
	if err := coreVerifyExecution(a, s, p.AfterUnit.Content, p.Identity, p.BinaryDigest, true); err != nil {
		return err
	}
	r := coreLoadedReceipt{1, contentDigest(p.AfterConfig.Content), contentDigest(p.AfterUnit.Content), digest, s.Invocation}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeFileAtomic(a.coreReceiptPath(), append(data, '\n'), 0600)
}

type runtimeCorePlan struct {
	BeforeConfig          coreFileState        `json:"before_config"`
	BeforeUnit            coreFileState        `json:"before_unit"`
	AfterConfig           coreFileState        `json:"after_config"`
	AfterUnit             coreFileState        `json:"after_unit"`
	BeforeDir             coreMetadata         `json:"before_dir"`
	BeforeBinary          coreMetadata         `json:"before_binary"`
	AfterDir              coreMetadata         `json:"after_dir"`
	AfterBinary           coreMetadata         `json:"after_binary"`
	BeforeProcess         *coreProcessIdentity `json:"before_process,omitempty"`
	BeforeService         coreServiceState     `json:"before_service"`
	Identity              localUserIdentity    `json:"identity"`
	BinaryDigest          string               `json:"binary_digest"`
	Enabled               bool                 `json:"enabled"`
	Noop                  bool                 `json:"noop"`
	RestoreServicePending bool                 `json:"restore_service_pending,omitempty"`
	BeforeServiceUser     string               `json:"before_service_user,omitempty"`
	BeforeServiceIdentity *localUserIdentity   `json:"before_service_identity,omitempty"`
}

func (a *App) planCoreRuntime(st *Store) (*runtimeCorePlan, error) {
	if err := a.ensureXrayInstalled(); err != nil {
		return nil, err
	}
	p := &runtimeCorePlan{Enabled: hasEnabledScene(st)}
	var err error
	if p.Identity, err = coreLookupIdentity(a.cfg.XrayServiceUser); err != nil {
		return nil, fmt.Errorf("核心服务身份不可用，请先完成服务账号安装：%w", err)
	}
	if err := validateCoreIdentity(a.cfg.XrayServiceUser, p.Identity); err != nil {
		return nil, err
	}
	if a.cfg.XrayServiceUser != "root" && p.Identity.GID == 0 {
		return nil, fmt.Errorf("核心服务不能使用 root 主组")
	}
	if p.BeforeConfig, err = readCoreFile(a.cfg.XrayConfig()); err != nil {
		return nil, err
	}
	if p.BeforeUnit, err = readCoreFile(coreUnitPath(a.cfg)); err != nil {
		return nil, err
	}
	legacyExec := "ExecStart=" + systemdQuote(a.cfg.XrayBin()) + " run -config " + systemdQuote(a.cfg.XrayConfig())
	if err := validateUnitReplacementOwnership(coreUnitPath(a.cfg), legacyExec); err != nil {
		return nil, err
	}
	if p.BeforeDir, err = readCoreMetadata(a.cfg.CoreDir); err != nil {
		return nil, err
	}
	if p.BeforeBinary, err = readCoreMetadata(a.cfg.XrayBin()); err != nil {
		return nil, err
	}
	if p.BinaryDigest, err = coreBinaryDigest(a.cfg.XrayBin()); err != nil {
		return nil, err
	}
	if p.BeforeService, err = coreReadService(a); err != nil {
		return nil, err
	}
	if p.BeforeService.Active == "active" {
		if !p.BeforeUnit.Present || !p.BeforeConfig.Present {
			return nil, fmt.Errorf("运行中的核心缺少可恢复的unit或配置，拒绝修改")
		}
		user, err := coreUnitServiceUser(p.BeforeUnit.Content)
		if err != nil {
			return nil, err
		}
		identity, err := coreLookupIdentity(user)
		if err != nil {
			return nil, err
		}
		p.BeforeServiceUser = user
		p.BeforeServiceIdentity = &identity
	}

	beforeIdentity, err := a.coreBeforeIdentity(p)
	if err != nil {
		return nil, err
	}
	if p.BeforeProcess, err = coreCaptureBeforeProcess(a, p.BeforeService, p.BeforeUnit.Content, beforeIdentity); err != nil {
		return nil, err
	}

	p.AfterDir, p.AfterBinary = p.BeforeDir, p.BeforeBinary
	if a.cfg.XrayServiceUser != "root" {
		p.AfterDir.Mode, p.AfterDir.UID, p.AfterDir.GID = 0750, 0, uint32(p.Identity.GID)
		p.AfterBinary.Mode, p.AfterBinary.UID, p.AfterBinary.GID = 0750, 0, uint32(p.Identity.GID)
	}
	p.AfterUnit = coreFileState{Present: true, Content: a.xrayUnitContent(), Mode: 0644}
	if p.Enabled {
		data, err := a.renderXrayConfig(st)
		if err != nil {
			return nil, err
		}
		if len(data) > maxGlobalProxyArtifactBytes {
			return nil, fmt.Errorf("候选核心配置超过大小限制")
		}
		p.AfterConfig = coreFileState{Present: true, Content: data, Mode: 0600}
		if a.cfg.XrayServiceUser != "root" {
			p.AfterConfig.Mode = 0640
			p.AfterConfig.GID = uint32(p.Identity.GID)
		}
		if err := a.checkCoreConfigSnapshot(data); err != nil {
			return nil, err
		}
	}
	if p.BeforeProcess != nil && (p.BeforeProcess.Device != p.BeforeBinary.Device || p.BeforeProcess.Inode != p.BeforeBinary.Inode || p.BeforeProcess.Digest != p.BinaryDigest) && (!p.Enabled || !bytes.Equal(p.BeforeConfig.Content, p.AfterConfig.Content)) {
		if err := a.checkCoreConfigSnapshot(p.BeforeConfig.Content); err != nil {
			return nil, fmt.Errorf("已安装Xray不能验证旧配置，拒绝切换仍在运行的旧进程：%w", err)
		}
	}
	filesSame := coreFilesEqual(p.BeforeConfig, p.AfterConfig) && coreFilesEqual(p.BeforeUnit, p.AfterUnit) && p.BeforeDir == p.AfterDir && p.BeforeBinary == p.AfterBinary
	p.Noop = filesSame && ((p.Enabled && a.coreLoadedMatches(p, p.BeforeService)) || (!p.Enabled && p.BeforeService.Active != "active" && p.BeforeService.Enabled == "disabled" && p.BeforeService.NeedReload == "no"))
	return p, nil
}
func (a *App) validateCorePlan(p *runtimeCorePlan) error {
	if p == nil {
		return nil
	}
	id, err := coreLookupIdentity(a.cfg.XrayServiceUser)
	if err != nil {
		return err
	}
	if id != p.Identity {
		return fmt.Errorf("核心服务用户身份在规划后变化")
	}
	if p.BeforeServiceIdentity != nil {
		identity, err := coreLookupIdentity(p.BeforeServiceUser)
		if err != nil {
			return err
		}
		if identity != *p.BeforeServiceIdentity {
			return fmt.Errorf("旧核心服务身份在规划后变化")
		}
	}
	for path, want := range map[string]coreFileState{a.cfg.XrayConfig(): p.BeforeConfig, coreUnitPath(a.cfg): p.BeforeUnit} {
		got, err := readCoreFile(path)
		if err != nil {
			return err
		}
		if !coreFilesEqual(got, want) {
			return fmt.Errorf("核心文件在规划后变化：%s", path)
		}
	}
	for path, want := range map[string]coreMetadata{a.cfg.CoreDir: p.BeforeDir, a.cfg.XrayBin(): p.BeforeBinary} {
		got, err := readCoreMetadata(path)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("核心路径身份在规划后变化：%s", path)
		}
	}
	digest, err := coreBinaryDigest(a.cfg.XrayBin())
	if err != nil {
		return err
	}
	if digest != p.BinaryDigest {
		return fmt.Errorf("核心二进制在规划后变化")
	}
	state, err := coreReadService(a)
	if err != nil {
		return err
	}
	if state != p.BeforeService {
		return fmt.Errorf("核心运行状态在规划后变化，请重试")
	}
	beforeIdentity, err := a.coreBeforeIdentity(p)
	if err != nil {
		return err
	}
	proof, err := coreCaptureBeforeProcess(a, state, p.BeforeUnit.Content, beforeIdentity)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(proof, p.BeforeProcess) {
		return fmt.Errorf("旧核心进程身份在规划后变化")
	}
	return nil
}
func (a *App) applyCorePlan(p *runtimeCorePlan) error {
	if p == nil || p.Noop {
		return nil
	}
	if err := a.validateCorePlan(p); err != nil {
		return err
	}
	if err := setCoreMetadata(a.cfg.CoreDir, p.BeforeDir, p.AfterDir); err != nil {
		return err
	}
	if err := setCoreMetadata(a.cfg.XrayBin(), p.BeforeBinary, p.AfterBinary); err != nil {
		return err
	}
	if err := setCoreFile(coreUnitPath(a.cfg), p.BeforeUnit, p.AfterUnit); err != nil {
		return err
	}
	if err := setCoreFile(a.cfg.XrayConfig(), p.BeforeConfig, p.AfterConfig); err != nil {
		return err
	}
	if !coreFilesEqual(p.BeforeUnit, p.AfterUnit) || p.BeforeService.NeedReload == "yes" {
		if err := systemctlRun("加载计划内核心 unit", "daemon-reload"); err != nil {
			return err
		}
	}
	loadedState, err := coreReadService(a)
	if err != nil {
		return err
	}
	if err := coreVerifyExecution(a, loadedState, p.AfterUnit.Content, p.Identity, p.BinaryDigest, false); err != nil {
		return err
	}
	if !p.Enabled {
		return a.stopXrayService()
	}
	if p.BeforeService.Enabled != "enabled" {
		if err := systemctlRun("启用计划内核心", "enable", "--", a.cfg.SystemdService); err != nil {
			return err
		}
	}
	if err := a.restartXrayService(); err != nil {
		return err
	}
	if err := a.waitCoreRestartExecution(p.AfterUnit, p.AfterConfig, p.Identity, p.BinaryDigest); err != nil {
		return err
	}
	return a.saveCoreLoaded(p)
}
func (a *App) compensateCorePlan(p *runtimeCorePlan, checkpoint func() error) error {
	if p == nil || p.Noop {
		return nil
	}
	// Never execute a replaced binary or coordinate a reused service identity.
	id, err := coreLookupIdentity(a.cfg.XrayServiceUser)
	if err != nil {
		return err
	}
	if id != p.Identity {
		return fmt.Errorf("补偿核心时用户身份变化")
	}
	if p.BeforeServiceIdentity != nil {
		identity, err := coreLookupIdentity(p.BeforeServiceUser)
		if err != nil {
			return err
		}
		if identity != *p.BeforeServiceIdentity {
			return fmt.Errorf("旧核心服务身份已变化，拒绝重新启动")
		}
	}
	digest, err := coreBinaryDigest(a.cfg.XrayBin())
	if err != nil {
		return err
	}
	if digest != p.BinaryDigest {
		return fmt.Errorf("补偿核心时二进制变化")
	}
	stateBeforeRestore, err := coreReadService(a)
	if err != nil {
		return err
	}
	needsService := stateBeforeRestore.NeedReload == "yes" || stateBeforeRestore.Active != p.BeforeService.Active || (stateBeforeRestore.Enabled == "enabled") != (p.BeforeService.Enabled == "enabled")
	for path, want := range map[string]coreFileState{a.cfg.XrayConfig(): p.BeforeConfig, coreUnitPath(a.cfg): p.BeforeUnit} {
		current, err := readCoreFile(path)
		if err != nil {
			return err
		}
		if !coreFilesEqual(current, want) {
			needsService = true
		}
	}
	if needsService && !p.RestoreServicePending {
		p.RestoreServicePending = true
		if checkpoint != nil {
			if err := checkpoint(); err != nil {
				p.RestoreServicePending = false
				return err
			}
		}
	}
	changed := false
	for _, file := range []struct {
		path          string
		before, after coreFileState
	}{{a.cfg.XrayConfig(), p.BeforeConfig, p.AfterConfig}, {coreUnitPath(a.cfg), p.BeforeUnit, p.AfterUnit}} {
		current, err := readCoreFile(file.path)
		if err != nil {
			return err
		}
		if coreFilesEqual(current, file.before) {
			continue
		}
		if !coreFilesEqual(current, file.after) {
			return fmt.Errorf("核心补偿检测到外部修改，保留配置：%s", file.path)
		}
		if err := setCoreFile(file.path, current, file.before); err != nil {
			return err
		}
		changed = true
	}
	if err := setCoreMetadata(a.cfg.XrayBin(), p.AfterBinary, p.BeforeBinary); err != nil {
		return err
	}
	if err := setCoreMetadata(a.cfg.CoreDir, p.AfterDir, p.BeforeDir); err != nil {
		return err
	}
	state, err := coreReadService(a)
	if err != nil {
		return err
	}
	if changed || p.RestoreServicePending || state.NeedReload == "yes" {
		if err := systemctlRun("加载已补偿的核心 unit", "daemon-reload"); err != nil {
			return err
		}
	}
	if p.BeforeService.Active == "active" {
		loadedState, err := coreReadService(a)
		if err != nil {
			return err
		}
		beforeIdentity, err := a.coreBeforeIdentity(p)
		if err != nil {
			return err
		}
		if err := coreVerifyExecution(a, loadedState, p.BeforeUnit.Content, beforeIdentity, p.BinaryDigest, false); err != nil {
			return err
		}
		if changed || p.RestoreServicePending || state.Active != "active" {
			if err := a.restartXrayService(); err != nil {
				return err
			}
			if err := a.waitCoreRestartExecution(p.BeforeUnit, p.BeforeConfig, beforeIdentity, p.BinaryDigest); err != nil {
				return err
			}
		}
	} else if state.Active == "active" {
		if err := systemctlRun("恢复核心停止状态", "stop", "--", a.cfg.SystemdService); err != nil {
			return err
		}
	}
	wantEnabled := p.BeforeService.Enabled == "enabled"
	if wantEnabled != (state.Enabled == "enabled") {
		action := "disable"
		if wantEnabled {
			action = "enable"
		}
		if err := systemctlRun("恢复核心启用状态", action, "--", a.cfg.SystemdService); err != nil {
			return err
		}
	}
	if p.BeforeService.Active == "active" {
		confirmed, err := coreReadService(a)
		if err != nil {
			return err
		}
		beforeIdentity, err := a.coreBeforeIdentity(p)
		if err != nil {
			return err
		}
		untouched := false
		if !changed && !p.RestoreServicePending && confirmed == p.BeforeService {
			proof, err := coreCaptureBeforeProcess(a, confirmed, p.BeforeUnit.Content, beforeIdentity)
			if err != nil {
				return err
			}
			untouched = reflect.DeepEqual(proof, p.BeforeProcess)
		}
		if !untouched {
			if err := coreVerifyExecution(a, confirmed, p.BeforeUnit.Content, beforeIdentity, p.BinaryDigest, true); err != nil {
				return err
			}
		}
	}
	if p.RestoreServicePending {
		confirmed, err := coreReadService(a)
		if err != nil {
			return err
		}
		wantActive := p.BeforeService.Active == "active"
		if (confirmed.Active == "active") != wantActive || (confirmed.Active != "active" && confirmed.Active != "inactive" && confirmed.Active != "failed") ||
			(confirmed.Enabled == "enabled") != (p.BeforeService.Enabled == "enabled") || confirmed.NeedReload == "yes" ||
			(wantActive && (confirmed.Load != "loaded" || confirmed.Sub != "running")) {
			return fmt.Errorf("核心补偿后的服务状态尚未确认，保留待恢复记录")
		}
		p.RestoreServicePending = false
		if checkpoint != nil {
			return checkpoint()
		}
	}
	return nil
}

func validateCorePlanRecord(p *runtimeCorePlan) error {
	if p == nil {
		return nil
	}
	if err := validateCoreIdentity(p.Identity.Name, p.Identity); err != nil {
		return err
	}
	if err := validateCoreServiceState(p.BeforeService); err != nil {
		return err
	}
	if p.BeforeService.Active == "active" {
		if !p.BeforeUnit.Present || !p.BeforeConfig.Present || p.BeforeServiceIdentity == nil || p.BeforeProcess == nil {
			return fmt.Errorf("活动核心恢复记录缺少旧配置或身份")
		}
		if err := validateCoreIdentity(p.BeforeServiceUser, *p.BeforeServiceIdentity); err != nil {
			return err
		}
		user, err := coreUnitServiceUser(p.BeforeUnit.Content)
		if err != nil || user != p.BeforeServiceUser {
			return fmt.Errorf("旧核心unit与持久服务身份不符")
		}
	} else if p.BeforeServiceUser != "" || p.BeforeServiceIdentity != nil || p.BeforeProcess != nil {
		return fmt.Errorf("非活动核心恢复记录携带无依据的旧服务身份")
	}
	if proof := p.BeforeProcess; proof != nil {
		start, err := strconv.ParseUint(proof.Start, 10, 64)
		if err != nil || start == 0 || strconv.FormatUint(start, 10) != proof.Start || proof.Inode == 0 || len(proof.Digest) != 64 {
			return fmt.Errorf("旧核心进程恢复身份无效")
		}
		if _, err := hex.DecodeString(proof.Digest); err != nil {
			return fmt.Errorf("旧核心进程摘要无效")
		}
	}
	if p.Noop && p.RestoreServicePending {
		return fmt.Errorf("核心noop记录不能含服务恢复进度")
	}
	if len(p.BinaryDigest) != 64 {
		return fmt.Errorf("核心恢复记录摘要无效")
	}
	if _, err := hex.DecodeString(p.BinaryDigest); err != nil {
		return err
	}
	for _, s := range []coreFileState{p.BeforeConfig, p.BeforeUnit, p.AfterConfig, p.AfterUnit} {
		if len(s.Content) > maxGlobalProxyArtifactBytes || s.Mode > 0777 || s.UID > maxGlobalProxyOwnershipID || s.GID > maxGlobalProxyOwnershipID || (!s.Present && (len(s.Content) != 0 || s.Mode != 0 || s.UID != 0 || s.GID != 0)) {
			return fmt.Errorf("核心恢复记录文件状态无效")
		}
	}
	if !p.AfterUnit.Present || !bytes.HasPrefix(p.AfterUnit.Content, []byte(managedSystemdUnitHeader)) {
		return fmt.Errorf("核心恢复记录 unit 无效")
	}
	user, err := coreUnitServiceUser(p.AfterUnit.Content)
	if err != nil || user != p.Identity.Name {
		return fmt.Errorf("候选核心unit与服务身份不符")
	}
	for _, metadata := range []coreMetadata{p.BeforeDir, p.AfterDir, p.BeforeBinary, p.AfterBinary} {
		if metadata.Mode > 0777 || metadata.UID > maxGlobalProxyOwnershipID || metadata.GID > maxGlobalProxyOwnershipID || metadata.Inode == 0 {
			return fmt.Errorf("核心恢复记录文件身份无效")
		}
	}
	if p.BeforeDir.Device != p.AfterDir.Device || p.BeforeDir.Inode != p.AfterDir.Inode || p.BeforeBinary.Device != p.AfterBinary.Device || p.BeforeBinary.Inode != p.AfterBinary.Inode {
		return fmt.Errorf("核心恢复记录不能更换文件身份")
	}
	if p.AfterConfig.Present != p.Enabled {
		return fmt.Errorf("核心恢复记录开关不一致")
	}
	return nil
}

func coreUnitServiceUser(content []byte) (string, error) {
	user := "root"
	section := ""
	for _, line := range systemdLogicalLines(string(content)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section != "[Service]" {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok && strings.TrimSpace(key) == "User" {
			user = strings.TrimSpace(value)
			if user == "" {
				user = "root"
			}
			if err := validateUserName(user); err != nil {
				return "", fmt.Errorf("旧核心unit的服务身份不能安全恢复：%w", err)
			}
		}
	}
	return user, nil
}

func (a *App) coreBeforeIdentity(p *runtimeCorePlan) (localUserIdentity, error) {
	if p.BeforeServiceIdentity != nil {
		return *p.BeforeServiceIdentity, nil
	}
	if !p.BeforeUnit.Present {
		return p.Identity, nil
	}
	user, err := coreUnitServiceUser(p.BeforeUnit.Content)
	if err != nil {
		return localUserIdentity{}, err
	}
	identity, err := coreLookupIdentity(user)
	if err != nil {
		return localUserIdentity{}, err
	}
	return identity, validateCoreIdentity(user, identity)
}

func (a *App) checkCoreConfigSnapshot(data []byte) error {
	dir, err := os.MkdirTemp("", "proxyscene-config-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "config.json")
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return err
	}
	return coreCheckConfig(a, path)
}
