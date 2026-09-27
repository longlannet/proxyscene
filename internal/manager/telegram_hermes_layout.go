package manager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const hermesSystemProjectRoot = "/usr/local/lib/hermes-agent"

// Configuration/profile selection remains independent of the installation root.
// Keep the supported layouts explicit; no environment variable widens this list.
func hermesProjectUsesSystemLayout(hermesRoot, projectRoot string) (bool, error) {
	if projectRoot == hermesSystemProjectRoot {
		return true, nil
	}
	if projectRoot == filepath.Join(hermesRoot, "hermes-agent") {
		return false, nil
	}
	return false, fmt.Errorf("检测到 Hermes PROJECT_ROOT=%s 不属于支持的安装布局（%s 或 %s）", projectRoot, filepath.Join(hermesRoot, "hermes-agent"), hermesSystemProjectRoot)
}

func validateHermesSystemProject(content, projectRoot string) error {
	return validateHermesSystemProjectWithChecks(content, projectRoot, validateHermesSystemDirectory, validateHermesSystemExecutable)
}

// The checkers are explicit so isolated tests can anchor a fixture under /tmp.
// Production always validates every ancestor from the filesystem root.
func validateHermesSystemProjectWithChecks(content, projectRoot string, checkDirectory, checkExecutable func(string) error) error {
	for _, path := range []string{projectRoot, filepath.Join(projectRoot, "venv"), filepath.Join(projectRoot, "venv", "bin")} {
		if err := checkDirectory(path); err != nil {
			return fmt.Errorf("检测到 Hermes 系统安装目录 %s 不存在或不受信任：%w", path, err)
		}
	}
	execStarts, err := effectiveServiceExecStarts(content)
	if err != nil {
		return err
	}
	if len(execStarts) != 1 {
		return fmt.Errorf("系统安装必须有且只有一个直接 Hermes ExecStart")
	}
	boundRoot, err := hermesProjectRootFromArgv(execStarts[0])
	if err != nil || boundRoot != projectRoot {
		return fmt.Errorf("检测到 Hermes 系统安装与 ExecStart 不一致")
	}
	command, _ := systemdDirectExecCommand(execStarts[0][0])
	executables := []string{command}
	for _, directive := range []string{"ExecStop", "ExecStopPost"} {
		commands, err := effectiveServiceCommands(content, directive)
		if err != nil {
			return err
		}
		if len(commands) != 0 {
			// validateHermesEffectiveUnit already binds both official hooks to
			// this interpreter. Check it even when ExecStart is a console script.
			executables = append(executables, filepath.Join(projectRoot, "venv", "bin", "python"))
			break
		}
	}
	for _, executable := range executables {
		if err := checkExecutable(executable); err != nil {
			return fmt.Errorf("检测到 Hermes 系统安装可执行文件 %s 不受信任：%w", executable, err)
		}
	}
	return nil
}

func validateHermesSystemDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("系统安装目录必须是规范绝对路径")
	}
	for current := path; ; current = filepath.Dir(current) {
		if err := validateHermesSystemNode(current, true); err != nil {
			return err
		}
		if current == string(os.PathSeparator) {
			return nil
		}
	}
}

func validateHermesUserProjectDirectory(user string, expected *persistedUserIdentity, projectRoot string) error {
	identity, err := verifyPersistedUserIdentity(user, expected, telegramLookupUserIdentity)
	if err != nil {
		return err
	}
	_, dirFD, err := openUserFileDirForIdentity(user, identity, filepath.Join(projectRoot, ".env"), false)
	if err != nil {
		return fmt.Errorf("检测到 Hermes 用户安装目录 %s 不存在或不受信任：%w", projectRoot, err)
	}
	return syscall.Close(dirFD)
}

// Unlike optional .env files, a missing installation directory or executable is
// an error. Do not execute the interpreter or import any installed Python code.
func validateHermesSystemNode(path string, directory bool) error {
	flags := syscall.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if directory {
		flags |= syscall.O_DIRECTORY
	}
	fd, err := syscall.Open(path, flags, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return err
	}
	return validateHermesSystemMode(stat.Mode, stat.Uid, directory)
}

func validateHermesSystemMode(mode, uid uint32, directory bool) error {
	wantType := uint32(syscall.S_IFREG)
	if directory {
		wantType = syscall.S_IFDIR
	}
	if mode&syscall.S_IFMT != wantType || uid != 0 || mode&0o022 != 0 {
		return fmt.Errorf("路径类型、root 所有权或写权限不符合系统安装要求")
	}
	if !directory && mode&0o111 == 0 {
		return fmt.Errorf("不是可执行文件")
	}
	return nil
}

// uv may link both venv/bin/python and a directory in its target path. Follow
// only root-owned links beneath trusted directories, validating every lexical
// and resolved component. Project/configuration directory checks remain strict.
func validateHermesSystemExecutable(path string) error {
	return validateHermesSystemExecutableAt(string(os.PathSeparator), path)
}

// root is / in production; tests use a private fixture root. O_PATH avoids
// opening a device or running/reading installed code. O_NOFOLLOW pins each link
// itself, so fstat and readlinkat inspect the same object without a name race.
func validateHermesSystemExecutableAt(root, path string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("解释器路径必须是规范绝对路径")
	}
	relative := func(path string) (string, error) {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return "", errors.New("解释器路径超出可信根目录")
		}
		return rel, nil
	}
	rel, err := relative(path)
	if err != nil {
		return err
	}
	flags := unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
	rootFD, err := unix.Open(root, flags|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil {
		return err
	}
	if err := validateHermesSystemMode(rootStat.Mode, rootStat.Uid, true); err != nil {
		return err
	}
	dirFD, err := unix.Openat(rootFD, ".", flags|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dirFD) }()
	current := root
	pending := strings.Split(rel, string(os.PathSeparator))
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "." {
			continue
		}
		if part == ".." && current == root {
			return errors.New("解释器链接目标超出可信根目录")
		}
		nextFD, err := unix.Openat(dirFD, part, flags, 0)
		if err != nil {
			return fmt.Errorf("检查解释器路径组件 %s：%w", filepath.Join(current, part), err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(nextFD, &stat); err != nil {
			_ = unix.Close(nextFD)
			return err
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
			links++
			if stat.Uid != 0 || links > 16 {
				_ = unix.Close(nextFD)
				return errors.New("解释器符号链接必须属于 root，且链接链不能超过 16 层或形成循环")
			}
			buffer := make([]byte, 4096)
			n, err := unix.Readlinkat(nextFD, "", buffer)
			_ = unix.Close(nextFD)
			if err != nil {
				return err
			}
			target := string(buffer[:n])
			// Never clean away an unchecked component such as link/..; Linux
			// resolves that link before applying .., possibly somewhere else.
			if n == 0 || n == len(buffer) || filepath.Clean(target) != target {
				return errors.New("解释器符号链接目标必须使用有界规范路径")
			}
			if filepath.IsAbs(target) {
				target, err = relative(target)
				if err != nil {
					return err
				}
				resetFD, err := unix.Openat(rootFD, ".", flags|unix.O_DIRECTORY, 0)
				if err != nil {
					return err
				}
				_ = unix.Close(dirFD)
				dirFD, current = resetFD, root
			}
			pending = append(strings.Split(target, string(os.PathSeparator)), pending...)
			continue
		}
		err = validateHermesSystemMode(stat.Mode, stat.Uid, len(pending) > 0)
		if err != nil || len(pending) == 0 {
			_ = unix.Close(nextFD)
			return err
		}
		_ = unix.Close(dirFD)
		dirFD = nextFD
		current = filepath.Join(current, part)
	}
	return errors.New("解释器路径未指向普通可执行文件")
}
