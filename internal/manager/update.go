package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type updateOptions struct {
	Check  bool
	Yes    bool
	Source string
}

func parseUpdateOptions(args []string) (updateOptions, error) {
	options := updateOptions{Source: "mirror"}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		if seen[args[i]] {
			return options, fmt.Errorf("更新参数不能重复")
		}
		seen[args[i]] = true
		switch args[i] {
		case "--check":
			options.Check = true
		case "--yes":
			options.Yes = true
		case "--source":
			i++
			if i == len(args) || (args[i] != "github" && args[i] != "mirror") {
				return options, fmt.Errorf("--source 仅接受 github 或 mirror")
			}
			options.Source = args[i]
		default:
			return options, fmt.Errorf("用法：proxyscene update [--check | --yes] [--source github|mirror]")
		}
	}
	if options.Check && options.Yes {
		return options, fmt.Errorf("--check 不能与 --yes 同时使用")
	}
	return options, nil
}

func currentUpdateTag() (string, error) {
	tag := "v" + strings.TrimPrefix(Version, "v")
	if _, err := compareUpdateVersions(tag, tag); err != nil {
		return "", fmt.Errorf("当前不是可识别的正式版本，请使用经过验证的 Release bundle 手动安装")
	}
	return tag, nil
}

func updateArchitecture() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64", "386":
		return runtime.GOARCH, nil
	case "arm":
		return "armv7", nil
	default:
		return "", fmt.Errorf("当前架构不支持自动升级")
	}
}

func validUpdateCommit(commit string) bool {
	decoded, err := hex.DecodeString(commit)
	return err == nil && len(decoded) == 20 && strings.ToLower(commit) == commit
}

func (a *App) updateCommand(args []string) error {
	options, err := parseUpdateOptions(args)
	if err != nil {
		return err
	}
	_, err = a.runUpdate(options)
	return err
}

func (a *App) runUpdate(options updateOptions) (bool, error) {
	client := newUpdateClient()
	defer client.http.CloseIdleConnections()
	return a.checkAndUpdate(options, client.latest, func(release updateRelease) (bool, error) {
		return a.applyUpdate(options, release, client)
	})
}

// attempted is true once the installer has been called, even when it fails:
// file commit can precede service initialization, so an old menu must exit.
func (a *App) checkAndUpdate(options updateOptions, latest func(context.Context) (updateRelease, error), apply func(updateRelease) (bool, error)) (attempted bool, err error) {
	if !options.Check {
		if err := requireRoot(); err != nil {
			return false, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	release, err := latest(ctx)
	if err != nil {
		return false, err
	}
	fmt.Printf("当前版本：%s\n最新正式版本：%s\n更新说明：https://github.com/%s/releases/tag/%s\n", sanitizeDisplayText(VersionString(), 200), release.Tag, updateRepository, release.Tag)
	if release.Notes != "" {
		notes := release.Notes
		if len(notes) > 16384 {
			notes = notes[:16384]
		}
		for _, line := range strings.Split(notes, "\n") {
			fmt.Println(sanitizeDisplayText(line, 4096))
		}
	}
	current, currentErr := currentUpdateTag()
	if currentErr != nil || !validUpdateCommit(Commit) {
		if options.Check {
			fmt.Println("当前构建缺少可验证的正式版本身份；自动升级不可用")
			return false, nil
		}
		return false, fmt.Errorf("当前构建缺少正式版本或 commit，请使用经过验证的 Release bundle 手动安装")
	}
	comparison, err := compareUpdateVersions(release.Tag, current)
	if err != nil {
		return false, err
	}
	if comparison == 0 && release.Commit != Commit {
		return false, fmt.Errorf("同一版本的 commit 与 GitHub 不一致，拒绝自动升级")
	}
	if comparison <= 0 {
		if comparison == 0 {
			fmt.Println("已是最新正式版本")
		} else {
			fmt.Println("当前版本高于最新正式版本，不执行降级")
		}
		return false, nil
	}
	if options.Check {
		fmt.Println("有新版本；执行 sudo proxyscene update 可下载并升级")
		return false, nil
	}
	return apply(release)
}

func (a *App) updateMenu() (bool, error) {
	return updateMenuWithRunner(a.runUpdate)
}

func updateMenuWithRunner(run func(updateOptions) (bool, error)) (bool, error) {
	for {
		fmt.Println("\n========== 程序更新 ==========")
		fmt.Println("1. 检查更新\n2. 升级程序（dl.ll.cd）\n3. 升级程序（GitHub）\n0. 返回")
		choice, err := menuInput("请输入选项 [0-3，q 返回]: ")
		if errors.Is(err, errMenuCancelled) || choice == "0" {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		options := updateOptions{Source: "mirror"}
		switch choice {
		case "1":
			options.Check = true
		case "2":
		case "3":
			options.Source = "github"
		default:
			fmt.Println("无效选项，请输入 0-3")
			continue
		}
		attempted, err := run(options)
		if attempted {
			return true, err
		}
		if err := reportMenuAction(err); err != nil {
			return false, err
		}
	}
}

// Only an installed running executable can initiate replacement. Linux's proc
// handle identifies the executing inode even after its original path is reused.
func (a *App) preflightSelfUpdate(arch string) (string, error) {
	if err := a.validateInstallationOwnership(); err != nil {
		return "", err
	}
	// Read the persisted configuration on a separate App: cancellation or a
	// download failure must leave this menu's explicit runtime overrides intact.
	inspection := NewApp(a.cfg)
	if _, err := inspection.loadStoreForBoot(); err != nil {
		return "", err
	}
	if err := validatePrivilegedExecutable(a.cfg.InstallBin, "当前管理程序"); err != nil {
		return "", err
	}
	running, err := os.Open("/proc/self/exe")
	if err != nil {
		return "", err
	}
	defer running.Close()
	runningInfo, err := running.Stat()
	if err != nil {
		return "", err
	}
	installedInfo, err := os.Lstat(a.cfg.InstallBin)
	if err != nil {
		return "", err
	}
	if !os.SameFile(runningInfo, installedInfo) {
		return "", fmt.Errorf("当前进程与安装路径不是同一程序，可能已被另一进程升级；请重新运行已安装的 proxyscene")
	}
	if err := validateUpdateManagerIdentity(a.cfg.InstallBin, arch); err != nil {
		return "", err
	}
	return hashUpdateFile(running)
}

func hashUpdateFile(file *os.File) (string, error) {
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > updateBundleMaxFile {
		return "", fmt.Errorf("升级程序必须是大小受限的普通文件")
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(file, info.Size()+1))
	if err != nil || n != info.Size() {
		return "", fmt.Errorf("读取升级程序摘要失败")
	}
	after, err := file.Stat()
	if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return "", fmt.Errorf("校验期间程序文件发生变化")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func hashUpdatePath(path string) (string, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return hashUpdateFile(file)
}

func (a *App) applyUpdate(options updateOptions, release updateRelease, client *updateClient) (attempted bool, err error) {
	arch, err := updateArchitecture()
	if err != nil {
		return false, err
	}
	expected, err := a.preflightSelfUpdate(arch)
	if err != nil {
		return false, err
	}
	if err := validateNoSymlinkComponents(a.cfg.CoreDir, true); err != nil {
		return false, err
	}
	stage, err := os.MkdirTemp(a.cfg.CoreDir, ".proxyscene-update-")
	if err != nil {
		return false, err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(stage); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("清理升级临时目录 %s 失败：%w", stage, cleanupErr))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	archive := filepath.Join(stage, "bundle.tar.gz")
	fmt.Printf("正在从 %s 下载并校验 %s…\n", options.Source, release.Tag)
	if err := client.downloadBundle(ctx, release, arch, options.Source, archive); err != nil {
		return false, err
	}
	bundleDir, err := extractUpdateBundle(archive, filepath.Join(stage, "extracted"), arch)
	if err != nil {
		return false, err
	}
	if err := validateUpdateBundleIdentity(bundleDir, arch); err != nil {
		return false, err
	}
	// A moving Latest must not silently install a now-superseded selection.
	latest, err := client.latest(ctx)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(latest, release) {
		return false, fmt.Errorf("下载期间 GitHub 最新版本身份发生变化，请重新检查更新")
	}
	return a.installPreparedUpdate(options, release, bundleDir, expected, arch, runUpdateInstaller)
}

func (a *App) installPreparedUpdate(options updateOptions, release updateRelease, bundleDir, expected, arch string, install func(string, Config, string) error) (bool, error) {
	fmt.Printf("%s 已通过校验。升级将更新程序及配套 Xray，并重新初始化管理服务，可能重启代理。\n", release.Tag)
	fmt.Println("若服务初始化失败，已提交的新版文件会保留，需根据错误修复后重试。")
	if !options.Yes {
		confirmed, err := menuConfirm("确认执行升级？")
		if err != nil {
			return false, err
		}
		if !confirmed {
			fmt.Println("已取消升级")
			return false, nil
		}
	}
	newHash, err := hashUpdatePath(filepath.Join(bundleDir, "proxyscene"))
	if err != nil {
		return false, err
	}
	if err := install(bundleDir, a.cfg, expected); err != nil {
		return true, fmt.Errorf("升级安装器未成功完成：%w；新版文件可能已提交，请重新运行 proxyscene version 确认，并按安装器提示修复", err)
	}
	installedHash, err := hashUpdatePath(a.cfg.InstallBin)
	if err != nil || installedHash != newHash {
		return true, fmt.Errorf("安装器退出后程序摘要未通过校验，请重新检查安装状态")
	}
	if err := validateUpdateManagerIdentity(a.cfg.InstallBin, arch); err != nil {
		return true, err
	}
	fmt.Printf("已升级到 %s；请重新运行 proxyscene。\n", release.Tag)
	return true, nil
}
