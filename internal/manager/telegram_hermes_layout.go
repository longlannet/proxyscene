package manager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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
	return validateHermesSystemProjectWithDirectoryCheck(content, projectRoot, validateHermesSystemDirectory)
}

// The directory checker is explicit so isolated tests can anchor a fixture under
// /tmp. Production always validates every ancestor from the filesystem root.
func validateHermesSystemProjectWithDirectoryCheck(content, projectRoot string, checkDirectory func(string) error) error {
	for _, path := range []string{projectRoot, filepath.Join(projectRoot, "venv"), filepath.Join(projectRoot, "venv", "bin")} {
		if err := checkDirectory(path); err != nil {
			return fmt.Errorf("Hermes 系统安装目录 %s 不存在或不受信任：%w", path, err)
		}
	}
	execStarts, err := effectiveServiceExecStarts(content)
	if err != nil {
		return err
	}
	if len(execStarts) != 1 {
		return fmt.Errorf("Hermes 系统安装必须有一个直接 ExecStart")
	}
	boundRoot, err := hermesProjectRootFromArgv(execStarts[0])
	if err != nil || boundRoot != projectRoot {
		return fmt.Errorf("Hermes 系统安装与 ExecStart 不一致")
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
		if err := validateHermesSystemExecutable(executable, checkDirectory); err != nil {
			return fmt.Errorf("Hermes 系统安装可执行文件 %s 不受信任：%w", executable, err)
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
		return fmt.Errorf("Hermes 用户安装目录 %s 不存在或不受信任：%w", projectRoot, err)
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
	wantType := uint32(syscall.S_IFREG)
	if directory {
		wantType = syscall.S_IFDIR
	}
	if stat.Mode&syscall.S_IFMT != wantType || stat.Uid != 0 || stat.Mode&0o022 != 0 {
		return fmt.Errorf("路径类型、root 所有权或写权限不符合系统安装要求")
	}
	if !directory && stat.Mode&0o111 == 0 {
		return fmt.Errorf("不是可执行文件")
	}
	return nil
}

// venv/bin/python is normally a symlink. Permit a bounded chain of leaf links,
// validating each target's parent directory before following the next link.
// Configuration paths and directory components still never follow symlinks.
func validateHermesSystemExecutable(path string, checkDirectory func(string) error) error {
	for links := 0; links < 16; links++ {
		if err := checkDirectory(filepath.Dir(path)); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return validateHermesSystemNode(path, false)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return fmt.Errorf("解释器符号链接必须属于 root")
		}
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		// Do not erase an unverified component such as link/..: the kernel
		// would resolve that link before applying .., possibly elsewhere.
		if filepath.Clean(target) != target {
			return fmt.Errorf("解释器符号链接目标必须使用规范路径")
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = filepath.Clean(target)
	}
	return errors.New("解释器符号链接链过长或形成循环")
}
