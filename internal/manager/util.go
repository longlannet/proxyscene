package manager

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func envIntAny(keys []string, fallback int) int {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
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
	if !existed {
		return os.Chmod(path, perm)
	}
	return nil
}

func ensurePublicDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := ensurePublicDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func writeUserFileAtomic(userName, path string, data []byte, perm os.FileMode) error {
	identity, err := lookupLocalUserIdentity(userName)
	if err != nil {
		return err
	}
	if err := ensureUserHomeUsable(identity.Home, userName); err != nil {
		return err
	}
	cleanHome := filepath.Clean(identity.Home)
	cleanPath := filepath.Clean(path)
	if cleanPath == cleanHome || !strings.HasPrefix(cleanPath, cleanHome+string(os.PathSeparator)) {
		return fmt.Errorf("用户级配置路径必须位于用户 %s 的家目录内：%s", userName, path)
	}
	dir := filepath.Dir(cleanPath)
	if err := ensureUserOwnedDirChain(cleanHome, dir, identity.UID, identity.GID); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(cleanPath)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Chown(tmpName, identity.UID, identity.GID); err != nil {
		return err
	}
	if err := os.Rename(tmpName, cleanPath); err != nil {
		return err
	}
	if err := os.Chown(cleanPath, identity.UID, identity.GID); err != nil {
		return err
	}
	return os.Chmod(cleanPath, perm)
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

func ensureUserOwnedDirChain(home, dir string, uid, gid int) error {
	rel, err := filepath.Rel(home, dir)
	if err != nil || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("用户级配置目录必须位于用户家目录内：%s", dir)
	}
	current := home
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := ensureUserOwnedDir(current, uid, gid, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func ensureUserOwnedDir(path string, uid, gid int, perm os.FileMode) error {
	created := false
	if err := os.Mkdir(path, perm); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
	} else {
		created = true
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("用户级配置目录不能是符号链接：%s", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("用户级配置路径不是目录：%s", path)
	}
	if created {
		if err := os.Chmod(path, perm); err != nil {
			return err
		}
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) == uid && int(stat.Gid) == gid {
		return nil
	}
	return os.Chown(path, uid, gid)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}

func runQuietLabel(label, name string, args ...string) error {
	return runQuietEnvLabel(label, nil, name, args...)
}

func runQuietEnvLabel(label string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if err := cmd.Run(); err != nil {
		return commandFailed(label, err)
	}
	return nil
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

func runAsUser(user, name string, args ...string) error {
	if user == "" || user == "root" {
		return runQuietLabel("执行命令 "+name, name, args...)
	}
	if _, err := exec.LookPath("runuser"); err == nil {
		runArgs := append([]string{"-u", user, "--", name}, args...)
		return runQuietLabel("以用户 "+user+" 执行命令 "+name, "runuser", runArgs...)
	}
	if _, err := exec.LookPath("sudo"); err == nil {
		runArgs := append([]string{"-H", "-u", user, name}, args...)
		return runQuietLabel("以用户 "+user+" 执行命令 "+name, "sudo", runArgs...)
	}
	return fmt.Errorf("需要 runuser 或 sudo 才能以用户 %s 执行命令", user)
}

func outputAsUser(user, name string, args ...string) (string, error) {
	var cmd *exec.Cmd
	if user == "" || user == "root" {
		cmd = exec.Command(name, args...)
	} else if _, err := exec.LookPath("runuser"); err == nil {
		runArgs := append([]string{"-u", user, "--", name}, args...)
		cmd = exec.Command("runuser", runArgs...)
	} else if _, err := exec.LookPath("sudo"); err == nil {
		runArgs := append([]string{"-H", "-u", user, name}, args...)
		cmd = exec.Command("sudo", runArgs...)
	} else {
		return "", fmt.Errorf("需要 runuser 或 sudo 才能以用户 %s 执行命令", user)
	}
	b, err := cmd.Output()
	return string(b), err
}

func ask(prompt string) string {
	fmt.Print(prompt)
	r := bufio.NewReader(os.Stdin)
	s, _ := r.ReadString('\n')
	return strings.TrimSpace(s)
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
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
		service = normalizeSystemdServiceName(service)
		if err := safeSystemdServiceName(service); err != nil {
			return systemdTargetName{}, err
		}
		return systemdTargetName{UserMode: true, User: userName, Service: service}, nil
	}
	service := name
	if !strings.HasSuffix(service, ".service") {
		if !systemdPlainNameRE.MatchString(service) {
			return systemdTargetName{}, fmt.Errorf("目标服务名包含非法字符：%s", name)
		}
		service = normalizeSystemdServiceName(service)
	}
	if err := safeSystemdServiceName(service); err != nil {
		return systemdTargetName{}, err
	}
	return systemdTargetName{Service: service}, nil
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
		if fields[2] == "" || fields[3] == "" || fields[5] == "" {
			break
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
	return localUserIdentity{}, fmt.Errorf("未找到系统用户：%s", userName)
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

func runUserSystemctlQuiet(userName string, args ...string) error {
	uid, _, err := lookupLocalUser(userName)
	if err != nil {
		return err
	}
	runtimeDir := "/run/user/" + uid
	busPath := runtimeDir + "/bus"
	if _, err := os.Stat(busPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("用户 %s 的 systemd 用户总线未运行：%s", userName, busPath)
		}
		return err
	}
	env := []string{
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + busPath,
	}
	systemctlArgs := append([]string{"--user"}, args...)
	if userName == "root" || uid == "0" {
		return runQuietEnvLabel("执行用户级 systemd 命令", env, "systemctl", systemctlArgs...)
	}
	if _, err := exec.LookPath("runuser"); err == nil {
		runArgs := []string{"-u", userName, "--", "env"}
		runArgs = append(runArgs, env...)
		runArgs = append(runArgs, "systemctl")
		runArgs = append(runArgs, systemctlArgs...)
		return runQuietLabel("执行用户 "+userName+" 的 systemd 命令", "runuser", runArgs...)
	}
	if _, err := exec.LookPath("sudo"); err == nil {
		sudoArgs := []string{"-H", "-u", userName, "env"}
		sudoArgs = append(sudoArgs, env...)
		sudoArgs = append(sudoArgs, "systemctl")
		sudoArgs = append(sudoArgs, systemctlArgs...)
		return runQuietLabel("执行用户 "+userName+" 的 systemd 命令", "sudo", sudoArgs...)
	}
	return fmt.Errorf("需要 runuser 或 sudo 才能执行用户 %s 的 systemd 命令", userName)
}

func runUserSystemctlWarn(userName, label string, args ...string) {
	if label == "" {
		label = "执行用户级 systemd 命令"
	}
	if err := runUserSystemctlQuiet(userName, args...); err != nil {
		fmt.Printf("警告：%s 失败：%v\n", label, err)
	}
}

func validPort(port int, field string) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("%s 端口无效：%d", field, port)
	}
	return nil
}

func validateProxyHost(host string) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("代理监听地址不能为空")
	}
	if strings.ContainsAny(host, "\x00\n\r:/") {
		return fmt.Errorf("代理监听地址包含非法字符：%s", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}
	if !regexp.MustCompile(`^[A-Za-z0-9.-]+$`).MatchString(host) {
		return fmt.Errorf("代理监听地址格式无效：%s", host)
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
