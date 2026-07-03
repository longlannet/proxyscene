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
)

// externalCmdTimeout 限制读取型外部命令（git/npm 配置读取、getent 用户解析）的执行时间。
// 这些命令在持有状态锁期间运行，若卡死（NSS 后端慢、npm 经异常代理联网）会连带阻塞
// 所有并发的 proxyscene 命令，因此给一个宽松但有界的上限。
const externalCmdTimeout = 30 * time.Second

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

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		fmt.Printf("警告：环境变量 %s 的值无效（%q），使用默认值 %d\n", key, v, fallback)
		return fallback
	}
	return n
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
	// 先 fsync 文件数据再 rename，rename 后再 fsync 父目录：os.Rename 对并发读者是
	// 原子的但并不保证持久化，崩溃/掉电后可能出现 0 字节或残缺文件。state.json 等的
	// 崩溃恢复依赖此持久性保证。
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return fsyncDir(dir)
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

// writeUserFileAtomic 以 root 身份在目标用户家目录内原子写入一个属于该用户的文件。
//
// 由于 root 写入用户可控目录，存在符号链接 TOCTOU 风险（用户可能把中途某级目录
// 换成符号链接，把写入重定向到任意路径）。这里通过 openat + O_NOFOLLOW 逐级打开/
// 创建目录链，所有创建、改属主、临时文件写入和 rename 都相对已校验的目录 fd 完成，
// 任何一级是符号链接都会被拒绝，从根本上消除路径再次解析带来的竞争窗口。
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
	rel, err := filepath.Rel(cleanHome, dir)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("用户级配置目录必须位于用户家目录内：%s", dir)
	}

	dirFD, err := openUserDirChain(cleanHome, rel, identity.UID, identity.GID)
	if err != nil {
		return err
	}
	defer syscall.Close(dirFD)

	base := filepath.Base(cleanPath)
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
	if err := syscall.Fchown(int(f.Fd()), identity.UID, identity.GID); err != nil {
		_ = f.Close()
		return err
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
	if err := syscall.Renameat(dirFD, tmpName, dirFD, base); err != nil {
		return err
	}
	_ = syscall.Fsync(dirFD)
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
	dirFD, err := openExistingUserDirChain(cleanHome, rel)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(dirFD)
	base := filepath.Base(cleanPath)
	fd, err := syscall.Openat(dirFD, base, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ENOENT {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("打开用户级配置失败（拒绝符号链接）：%s：%w", path, err)
	}
	f := os.NewFile(uintptr(fd), base)
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

// openExistingUserDirChain 从 home 起沿 rel 逐级 openat 打开已存在的目录（不创建），
// 全程 O_NOFOLLOW|O_DIRECTORY 拒绝符号链接；任一级缺失返回 os.ErrNotExist。
func openExistingUserDirChain(home, rel string) (int, error) {
	flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_DIRECTORY | syscall.O_CLOEXEC
	homeFD, err := syscall.Open(home, flags, 0)
	if err != nil {
		return -1, fmt.Errorf("打开用户家目录失败：%s：%w", home, err)
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

// externalCommandTimeout 给一般外部命令（systemctl、git/npm 写、useradd 等）一个宽松上限。
// 取 5 分钟：远高于 systemd 默认的 stop+start 超时（各 90s），只用于兜住真正卡死的调用，
// 避免它们在持有状态锁时无限阻塞所有并发的 proxyscene 命令。
const externalCommandTimeout = 5 * time.Minute

func runQuiet(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), externalCommandTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run()
}

// writeFileAtomicIfChanged 仅在目标内容与 data 不同时才原子写入，返回是否实际写入。
// 用于避免在内容未变时无谓地改文件 mtime / 触发依赖该文件的服务重启。
func writeFileAtomicIfChanged(path string, data []byte, perm os.FileMode) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return false, nil
	}
	return true, writeFileAtomic(path, data, perm)
}

// writeUserFileAtomicIfChanged 是 writeFileAtomicIfChanged 的用户家目录变体（读用 O_NOFOLLOW）。
func writeUserFileAtomicIfChanged(userName, path string, data []byte, perm os.FileMode) (bool, error) {
	if existing, err := readUserFileNoFollow(userName, path, maxOpenClawConfigBytes); err == nil && bytes.Equal(existing, data) {
		return false, nil
	}
	return true, writeUserFileAtomic(userName, path, data, perm)
}

// removeFileReport 删除文件，返回该文件此前是否存在（用于判断是否需要重启依赖它的服务）。
func removeFileReport(path string) bool {
	return os.Remove(path) == nil
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
		// -n：非交互，需要密码时直接失败而不是挂起等待输入。
		runArgs := append([]string{"-n", "-H", "-u", user, name}, args...)
		return runQuietLabel("以用户 "+user+" 执行命令 "+name, "sudo", runArgs...)
	}
	return fmt.Errorf("需要 runuser 或 sudo 才能以用户 %s 执行命令", user)
}

func outputAsUser(user, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), externalCmdTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if user == "" || user == "root" {
		cmd = exec.CommandContext(ctx, name, args...)
	} else if _, err := exec.LookPath("runuser"); err == nil {
		runArgs := append([]string{"-u", user, "--", name}, args...)
		cmd = exec.CommandContext(ctx, "runuser", runArgs...)
	} else if _, err := exec.LookPath("sudo"); err == nil {
		runArgs := append([]string{"-n", "-H", "-u", user, name}, args...)
		cmd = exec.CommandContext(ctx, "sudo", runArgs...)
	} else {
		return "", fmt.Errorf("需要 runuser 或 sudo 才能以用户 %s 执行命令", user)
	}
	b, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return string(b), fmt.Errorf("以用户 %s 执行命令 %s 超时（%s）", user, name, externalCmdTimeout)
	}
	return string(b), err
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
		sudoArgs := []string{"-n", "-H", "-u", userName, "env"}
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
func validateProxyHost(host string) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("代理监听地址不能为空")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("代理监听地址必须是 IPv4/IPv6 字面量（xray 监听不支持主机名）：%s", host)
	}
	// 本地 HTTP/SOCKS 入站没有认证；绑定到非环回地址会把它暴露成开放代理。
	// 默认只允许环回，确需对外监听时须显式设置 PROXYSCENE_ALLOW_PUBLIC_BIND=1。
	if !ip.IsLoopback() && !envBool("PROXYSCENE_ALLOW_PUBLIC_BIND", false) {
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
