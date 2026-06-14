package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type devProxyBackup struct {
	User                string   `json:"user"`
	GitHTTPProxy        []string `json:"git_http_proxy"`
	GitHTTPSProxy       []string `json:"git_https_proxy"`
	NPMProxy            *string  `json:"npm_proxy,omitempty"`
	NPMHTTPSProxy       *string  `json:"npm_https_proxy,omitempty"`
	ManagedHTTPProxy    string   `json:"managed_http_proxy,omitempty"`
	ManagedHTTPSProxy   string   `json:"managed_https_proxy,omitempty"`
	ManagedHTTPProxies  []string `json:"managed_http_proxies,omitempty"`
	ManagedHTTPSProxies []string `json:"managed_https_proxies,omitempty"`
}

func (a *App) devTargetUser() (string, error) {
	user := a.cfg.DevTargetUser
	if user == "" {
		if sudoUser := strings.TrimSpace(os.Getenv("SUDO_USER")); sudoUser != "" && sudoUser != "root" {
			user = sudoUser
		}
	}
	if user == "" {
		user = strings.TrimSpace(os.Getenv("USER"))
	}
	if user == "" {
		user = "root"
	}
	if err := validateUserName(user); err != nil {
		return "", err
	}
	if err := exec.Command("id", "-u", user).Run(); err != nil {
		return "", fmt.Errorf("开发代理目标用户不存在：%s", user)
	}
	return user, nil
}

func (a *App) backupDevConfig(user string) error {
	proxy := a.cfg.HTTPAddr(SceneDev)
	if backup, err := a.loadDevBackup(); err == nil {
		if backup.User != "" && backup.User != user {
			return fmt.Errorf("已有开发代理备份属于用户 %s，请先关闭开发代理或删除备份文件：%s", backup.User, a.cfg.DevBackupPath())
		}
		changed := mergeManagedDevProxyValues(backup, proxy)
		if changed {
			b, err := json.MarshalIndent(backup, "", "  ")
			if err != nil {
				return err
			}
			return writeFileAtomic(a.cfg.DevBackupPath(), append(b, '\n'), 0o600)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	backup := devProxyBackup{User: user, ManagedHTTPProxy: proxy, ManagedHTTPSProxy: proxy, ManagedHTTPProxies: []string{proxy}, ManagedHTTPSProxies: []string{proxy}}
	backup.GitHTTPProxy = getGitConfigAll(user, "http.proxy")
	backup.GitHTTPSProxy = getGitConfigAll(user, "https.proxy")
	backup.NPMProxy = getNPMConfig(user, "proxy")
	backup.NPMHTTPSProxy = getNPMConfig(user, "https-proxy")
	b, err := json.MarshalIndent(backup, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.cfg.DevBackupPath(), append(b, '\n'), 0o600)
}

func getGitConfigAll(user, key string) []string {
	out, err := outputAsUser(user, "git", "config", "--global", "--get-all", key)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	values := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			values = append(values, line)
		}
	}
	return values
}

func getNPMConfig(user, key string) *string {
	out, err := outputAsUser(user, "npm", "config", "get", key)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	v := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			v = s
			break
		}
	}
	if v == "" || v == "undefined" || v == "null" {
		return nil
	}
	return &v
}

func (a *App) restoreDev() error {
	backup, err := a.loadDevBackup()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			user, targetErr := a.devTargetUser()
			if targetErr != nil {
				return targetErr
			}
			fmt.Println("未找到开发代理备份，仅清理本程序当前代理值")
			return a.cleanupManagedDevConfig(user, []string{a.cfg.HTTPAddr(SceneDev)})
		}
		return err
	}
	user := backup.User
	if user == "" {
		user = "root"
	}
	if err := validateUserName(user); err != nil {
		return err
	}
	managedHTTP := managedDevProxyValues(backup, true, a.cfg.HTTPAddr(SceneDev))
	managedHTTPS := managedDevProxyValues(backup, false, a.cfg.HTTPAddr(SceneDev))
	if err := restoreGitProxyValues(user, "http.proxy", backup.GitHTTPProxy, managedHTTP); err != nil {
		return err
	}
	if err := restoreGitProxyValues(user, "https.proxy", backup.GitHTTPSProxy, managedHTTPS); err != nil {
		return err
	}
	if err := restoreNPMProxyValue(user, "proxy", backup.NPMProxy, managedHTTP); err != nil {
		return err
	}
	if err := restoreNPMProxyValue(user, "https-proxy", backup.NPMHTTPSProxy, managedHTTPS); err != nil {
		return err
	}
	return os.Remove(a.cfg.DevBackupPath())
}

func (a *App) cleanupManagedDevConfig(user string, managed []string) error {
	if err := restoreGitProxyValues(user, "http.proxy", nil, managed); err != nil {
		return err
	}
	if err := restoreGitProxyValues(user, "https.proxy", nil, managed); err != nil {
		return err
	}
	if current := getNPMConfig(user, "proxy"); current != nil && containsString(managed, *current) {
		if err := runAsUser(user, "npm", "config", "delete", "proxy"); err != nil {
			return err
		}
	}
	if current := getNPMConfig(user, "https-proxy"); current != nil && containsString(managed, *current) {
		if err := runAsUser(user, "npm", "config", "delete", "https-proxy"); err != nil {
			return err
		}
	}
	return nil
}

func restoreGitProxyValues(user, key string, original []string, managed []string) error {
	current := getGitConfigAll(user, key)
	_ = runAsUser(user, "git", "config", "--global", "--unset-all", key)
	values := []string{}
	for _, v := range original {
		values = appendUniqueString(values, strings.TrimSpace(v))
	}
	for _, v := range current {
		v = strings.TrimSpace(v)
		if v != "" && !containsString(managed, v) {
			values = appendUniqueString(values, v)
		}
	}
	for _, v := range values {
		if err := runAsUser(user, "git", "config", "--global", "--add", key, v); err != nil {
			return err
		}
	}
	return nil
}

func restoreNPMProxyValue(user, key string, original *string, managed []string) error {
	current := getNPMConfig(user, key)
	if original != nil {
		return runAsUser(user, "npm", "config", "set", key, *original)
	}
	if current != nil && !containsString(managed, *current) {
		return nil
	}
	return runAsUser(user, "npm", "config", "delete", key)
}

func mergeManagedDevProxyValues(backup *devProxyBackup, proxy string) bool {
	changed := false
	beforeHTTP := len(backup.ManagedHTTPProxies)
	beforeHTTPS := len(backup.ManagedHTTPSProxies)
	backup.ManagedHTTPProxies = appendUniqueString(backup.ManagedHTTPProxies, backup.ManagedHTTPProxy)
	backup.ManagedHTTPProxies = appendUniqueString(backup.ManagedHTTPProxies, proxy)
	backup.ManagedHTTPSProxies = appendUniqueString(backup.ManagedHTTPSProxies, backup.ManagedHTTPSProxy)
	backup.ManagedHTTPSProxies = appendUniqueString(backup.ManagedHTTPSProxies, proxy)
	if backup.ManagedHTTPProxy == "" {
		backup.ManagedHTTPProxy = proxy
		changed = true
	}
	if backup.ManagedHTTPSProxy == "" {
		backup.ManagedHTTPSProxy = proxy
		changed = true
	}
	return changed || len(backup.ManagedHTTPProxies) != beforeHTTP || len(backup.ManagedHTTPSProxies) != beforeHTTPS
}

func managedDevProxyValues(backup *devProxyBackup, httpProxy bool, fallback string) []string {
	values := []string{}
	if httpProxy {
		values = appendUniqueString(values, backup.ManagedHTTPProxy)
		for _, v := range backup.ManagedHTTPProxies {
			values = appendUniqueString(values, v)
		}
	} else {
		values = appendUniqueString(values, backup.ManagedHTTPSProxy)
		for _, v := range backup.ManagedHTTPSProxies {
			values = appendUniqueString(values, v)
		}
	}
	values = appendUniqueString(values, fallback)
	return values
}

func appendUniqueString(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}

func (a *App) loadDevBackup() (*devProxyBackup, error) {
	b, err := os.ReadFile(a.cfg.DevBackupPath())
	if err != nil {
		return nil, err
	}
	var backup devProxyBackup
	if err := json.Unmarshal(b, &backup); err != nil {
		return nil, err
	}
	return &backup, nil
}
