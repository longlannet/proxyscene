package manager

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxTelegramUnitReadBytes int64 = 64 << 10

type localUserAccount struct {
	Name string
	Home string
}

func (a *App) telegramTargets(st *Store, includeStored bool) ([]systemdTargetName, error) {
	names := a.telegramTargetNames(st, includeStored)
	seen := map[string]bool{}
	targets := make([]systemdTargetName, 0, len(names))
	for _, name := range names {
		var err error
		targets, err = appendTelegramTargetName(targets, seen, name)
		if err != nil {
			return nil, err
		}
	}
	return targets, nil
}

func (a *App) telegramTargetsBestEffort(st *Store, includeStored bool) ([]systemdTargetName, []error) {
	names := a.telegramTargetNames(st, includeStored)
	seen := map[string]bool{}
	targets := make([]systemdTargetName, 0, len(names))
	errs := []error{}
	for _, name := range names {
		var err error
		targets, err = appendTelegramTargetName(targets, seen, name)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return targets, errs
}

func (a *App) telegramTargetNames(st *Store, includeStored bool) []string {
	names := []string{}
	names = append(names, a.cfg.TGTargetServices...)
	names = append(names, discoverTelegramTargetNames()...)
	if includeStored && st != nil {
		names = append(names, st.TelegramTargets...)
	}
	return names
}

func appendTelegramTargetName(targets []systemdTargetName, seen map[string]bool, name string) ([]systemdTargetName, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return targets, nil
	}
	target, err := parseSystemdTargetName(name)
	if err != nil {
		return targets, err
	}
	key := canonicalTelegramTargetName(target)
	if seen[key] {
		return targets, nil
	}
	seen[key] = true
	return append(targets, target), nil
}

func canonicalTelegramTargetName(target systemdTargetName) string {
	if target.UserMode {
		return "user:" + target.User + ":" + target.Service
	}
	return target.Service
}

func canonicalTelegramTargetNames(targets []systemdTargetName) []string {
	values := make([]string, 0, len(targets))
	for _, target := range targets {
		values = appendUniqueString(values, canonicalTelegramTargetName(target))
	}
	return values
}

func discoverTelegramTargetNames() []string {
	values := []string{}
	values = append(values, discoverSystemTelegramTargetNames()...)
	values = append(values, discoverUserTelegramTargetNames()...)
	return values
}

func discoverSystemTelegramTargetNames() []string {
	roots := []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"}
	values := []string{}
	seen := map[string]bool{}
	for _, root := range roots {
		walkTelegramUnitFiles(root, func(path, service string) {
			if !isTelegramRelatedUnit(path, service) || seen[service] {
				return
			}
			seen[service] = true
			values = append(values, service)
		})
	}
	sort.Strings(values)
	return values
}

func discoverUserTelegramTargetNames() []string {
	values := []string{}
	seen := map[string]bool{}
	for _, account := range listLocalUserAccounts() {
		if err := ensureUserHomeUsable(account.Home, account.Name); err != nil {
			continue
		}
		root := filepath.Join(account.Home, ".config/systemd/user")
		walkTelegramUnitFiles(root, func(path, service string) {
			if !isTelegramRelatedUnit(path, service) {
				return
			}
			name := "user:" + account.Name + ":" + service
			if seen[name] {
				return
			}
			seen[name] = true
			values = append(values, name)
		})
	}
	sort.Strings(values)
	return values
}

func walkTelegramUnitFiles(root string, visit func(path, service string)) {
	if root == "" {
		return
	}
	if _, err := os.Stat(root); err != nil {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".service") {
			return nil
		}
		if err := safeSystemdServiceName(name); err != nil {
			return nil
		}
		visit(path, name)
		return nil
	})
}

func isTelegramRelatedUnit(path, service string) bool {
	if containsTelegramServiceKeyword(service) {
		return true
	}
	content, err := readTelegramUnitContent(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "description", "documentation", "execstart", "environment", "environmentfile", "partof", "wants", "requires", "after", "before":
			if containsTelegramServiceKeyword(value) {
				return true
			}
		}
	}
	return false
}

func readTelegramUnitContent(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxTelegramUnitReadBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > maxTelegramUnitReadBytes {
		b = b[:maxTelegramUnitReadBytes]
	}
	return string(b), nil
}

func containsTelegramServiceKeyword(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "openclaw") || strings.Contains(s, "hermes")
}

func listLocalUserAccounts() []localUserAccount {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return nil
	}
	accounts := []localUserAccount{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 6 {
			continue
		}
		name := fields[0]
		home := fields[5]
		if validateUserName(name) != nil || home == "" || !filepath.IsAbs(home) {
			continue
		}
		accounts = append(accounts, localUserAccount{Name: name, Home: home})
	}
	return accounts
}
