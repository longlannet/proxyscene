package manager

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func hermesMultiplexToken(value string) string {
	// Python strips Unicode whitespace, but not the ECMAScript BOM. Unknown
	// config strings (including an empty string) enable multiplex upstream.
	return strings.ToLower(strings.TrimFunc(value, func(r rune) bool { return r != '\ufeff' && dotEnvSpace(r) }))
}

func hermesMultiplexFlagDisabled(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case bool:
		return !value
	case string:
		switch hermesMultiplexToken(value) {
		case "0", "false", "no", "off":
			return true
		}
	case int:
		return value == 0
	case float64:
		return value == 0
	}
	return false
}

func validateHermesMultiplexEnvironment(environment map[string]string) error {
	if value, present := environment["GATEWAY_MULTIPLEX_PROFILES"]; present && hermesMultiplexToken(value) != "" && !hermesMultiplexFlagDisabled(value) {
		return fmt.Errorf("GATEWAY_MULTIPLEX_PROFILES 启用或无法排除 Hermes multiplex；进程级 TELEGRAM_PROXY 不能控制各 profile 的代理")
	}
	return nil
}

func rejectHermesMultiplexConfig(document map[string]any) error {
	if value, present := document["multiplex_profiles"]; present && !hermesMultiplexFlagDisabled(value) {
		return fmt.Errorf("multiplex_profiles 启用或无法排除 Hermes multiplex；不支持逐 profile 代理接管")
	}
	if raw, present := document["gateway"]; present && raw != nil {
		gateway, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("gateway 必须是字符串键映射，无法安全排除 Hermes multiplex")
		}
		if value, present := gateway["multiplex_profiles"]; present && !hermesMultiplexFlagDisabled(value) {
			return fmt.Errorf("gateway.multiplex_profiles 启用或无法排除 Hermes multiplex；不支持逐 profile 代理接管")
		}
	}
	return nil
}

// Hermes 0.21.5 may multiplex automatically even when the setting is absent or
// explicitly false (false is retired). Its single-profile guard is stable only
// while no named profile exists. Do not depend on transient migration blockers,
// parked/standalone flags, or the current process's reported runtime mode.
// This conservative single-profile boundary is also safe for Hermes 0.19.
func validateHermesSingleProfile(user string, expected *persistedUserIdentity, hermesRoot, activeHome string) error {
	if activeHome != hermesRoot {
		return fmt.Errorf("电报代理：Hermes 使用命名 profile；默认根和命名 profile 可能被新版 gateway 自动 multiplex，拒绝部分接管")
	}
	identity, err := verifyPersistedUserIdentity(user, expected, telegramLookupUserIdentity)
	if err != nil {
		return err
	}
	profilesDir := filepath.Join(hermesRoot, "profiles")
	_, fd, err := openUserFileDirForIdentity(user, identity, filepath.Join(profilesDir, ".proxyscene-profile-scan"), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("无法安全排除 Hermes multiplex profile 目录 %s：%w", profilesDir, err)
	}
	directory := os.NewFile(uintptr(fd), profilesDir)
	defer directory.Close()
	seen := 0
	for {
		entries, readErr := directory.ReadDir(64)
		for _, entry := range entries {
			seen++
			if seen > maxTelegramTargets {
				return fmt.Errorf("电报代理：Hermes profiles 目录条目超过安全检查上限 %d，无法排除 multiplex", maxTelegramTargets)
			}
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			var stat unix.Stat_t
			if err := unix.Fstatat(fd, entry.Name(), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return fmt.Errorf("电报代理：Hermes profile 目录在检查时变化，无法排除 multiplex：%w", err)
			}
			// Reject even markerless/parked directories and symlinks: their
			// exclusion rules differ by release and may change before restart.
			if stat.Mode&unix.S_IFMT == unix.S_IFDIR || stat.Mode&unix.S_IFMT == unix.S_IFLNK {
				return fmt.Errorf("电报代理：Hermes profiles 中存在 %q，无法排除自动 multiplex；只支持没有命名 profile 的单 profile gateway", entry.Name())
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("读取 Hermes profiles 目录失败：%w", readErr)
		}
	}
}
