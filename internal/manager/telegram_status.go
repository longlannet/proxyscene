package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Service state is deliberately separate from ownership and Telegram health.
// In particular, try-restart succeeds without starting an inactive service.
type telegramServiceState struct {
	LoadState   string
	ActiveState string
}

var telegramReadServiceState = readTelegramServiceState

func readTelegramServiceState(target systemdTargetName, identity *persistedUserIdentity) (telegramServiceState, error) {
	if _, err := parseSystemdTargetName(canonicalTelegramTargetName(target)); err != nil {
		return telegramServiceState{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := []string{"show", "--property=LoadState", "--property=ActiveState", "--", target.Service}
	var cmd *exec.Cmd
	if target.UserMode {
		var err error
		cmd, _, err = commandUserSystemctlPersisted(ctx, target.User, identity, telegramLookupUserIdentity, args...)
		if err != nil {
			return telegramServiceState{}, err
		}
	} else {
		cmd = exec.CommandContext(ctx, "systemctl", args...)
	}
	var output telegramStatusBuffer
	cmd.Stdout = &output
	cmd.WaitDelay = 250 * time.Millisecond
	if err := cmd.Run(); err != nil {
		return telegramServiceState{}, fmt.Errorf("无法查询 Telegram 服务状态")
	}
	load, active, err := parseSystemdUnitState(output.String())
	if err != nil {
		return telegramServiceState{}, err
	}
	return telegramServiceState{LoadState: load, ActiveState: active}, nil
}

// Do not let an unexpected command response accumulate unbounded output.
type telegramStatusBuffer struct{ buffer bytes.Buffer }

func (b *telegramStatusBuffer) Len() int       { return b.buffer.Len() }
func (b *telegramStatusBuffer) String() string { return b.buffer.String() }

func (b *telegramStatusBuffer) Write(p []byte) (int, error) {
	if len(p) > 4096-b.Len() {
		return 0, errors.New("服务状态输出超过上限")
	}
	return b.buffer.Write(p)
}

func (s telegramServiceState) description() string {
	if s.LoadState == "not-found" {
		return "未安装"
	}
	if s.LoadState != "loaded" {
		return "未正常加载"
	}
	switch s.ActiveState {
	case "active":
		return "运行中"
	case "inactive":
		return "未运行"
	case "failed":
		return "启动失败"
	case "activating", "deactivating", "reloading":
		return "切换中"
	default:
		return "未知"
	}
}

type telegramTargetStatus struct {
	Target  string
	Config  string
	Service string
}

func telegramPhaseDescription(phase string) string {
	switch phase {
	case telegramPhasePrepared:
		return "待完成应用"
	case telegramPhaseRestoring:
		return "待完成恢复"
	case telegramPhaseActive:
		return "已记录，待核对"
	default:
		return "未知"
	}
}

func telegramStatusService(target systemdTargetName, identity *persistedUserIdentity) string {
	state, err := telegramReadServiceState(target, identity)
	if err != nil {
		return "无法查询"
	}
	return state.description()
}

func (a *App) telegramTargetStatuses() ([]telegramTargetStatus, error) {
	hermes, hermesErr := a.loadTelegramProxyJournal()
	openClaw, openClawErr := a.loadOpenClawProxyJournal()
	var statuses []telegramTargetStatus
	if hermesErr == nil {
		for key, entry := range hermes.Targets {
			target, err := parseSystemdTargetName(key)
			row := telegramTargetStatus{Target: key, Config: telegramPhaseDescription(entry.Phase), Service: "未查询"}
			if err != nil || verifyTelegramUserIdentity(target, entry.Identity) != nil {
				row.Config = "身份校验失败"
				statuses = append(statuses, row)
				continue
			}
			row.Service = telegramStatusService(target, entry.Identity)
			if entry.Phase == telegramPhaseActive {
				proxy, proxyErr := telegramProxyFromManagedContent(entry.ManagedContent)
				path, pathErr := a.telegramManagedArtifactPath(target, entry.Identity)
				var raw []byte
				if pathErr == nil {
					raw, pathErr = readTelegramManagedArtifact(target, entry.Identity, path, maxTelegramManagedContentBytes)
				}
				if proxyErr == nil && pathErr == nil && bytes.Equal(raw, []byte(entry.ManagedContent)) &&
					telegramValidateHermesTarget(target, entry.Identity, proxy) == nil {
					row.Config = "已写入并核对"
				} else {
					row.Config = "已变化或无法验证"
				}
			}
			statuses = append(statuses, row)
		}
	}
	if openClawErr == nil {
		for _, entry := range openClaw.Users {
			for _, key := range entry.Targets {
				target, err := parseSystemdTargetName(key)
				row := telegramTargetStatus{Target: key, Config: telegramPhaseDescription(entry.Phase), Service: "未查询"}
				if err != nil || verifyOpenClawUserIdentity(entry.User, entry.Identity) != nil {
					row.Config = "身份校验失败"
					statuses = append(statuses, row)
					continue
				}
				row.Service = telegramStatusService(target, entry.Identity)
				if entry.Phase == openClawPhaseActive {
					if a.openClawStatusConfigMatches(target, entry) {
						row.Config = "已写入并核对"
					} else {
						row.Config = "已变化或无法验证"
					}
				}
				statuses = append(statuses, row)
			}
		}
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Target < statuses[j].Target })
	return statuses, errors.Join(hermesErr, openClawErr)
}

func (a *App) openClawStatusConfigMatches(target systemdTargetName, entry *openClawProxyJournalEntry) bool {
	if openClawValidateTargetRuntime(target, entry.Identity) != nil {
		return false
	}
	path, err := a.openClawConfigPath(entry.User, entry.Identity)
	if err != nil {
		return false
	}
	raw, err := readOpenClawUserConfig(entry.User, entry.Identity, path)
	if err != nil || rejectOpenClawRuntimeConfigSelectors(raw) != nil || rejectOpenClawAccountProxyOverrides(raw) != nil {
		return false
	}
	value, present, err := openClawTelegramProxyRawValue(raw)
	return err == nil && proxyStateMatchesString(value, present, entry.ManagedValue)
}

func writeTelegramTargetStatuses(w io.Writer, rows []telegramTargetStatus, readErr error) {
	fmt.Fprintln(w, "Telegram 接管状态（读取时）：")
	for _, row := range rows {
		fmt.Fprintf(w, "  %s：配置=%s；服务=%s；频道/代理连通性=未探测\n", row.Target, row.Config, row.Service)
	}
	if readErr != nil {
		// Configuration errors can contain proxy credentials. Status reports the
		// failed check without echoing raw configuration or subprocess output.
		fmt.Fprintln(w, "  部分接管记录无法安全读取，状态未知")
	} else if len(rows) == 0 {
		fmt.Fprintln(w, "  没有接管记录")
	}
	fmt.Fprintln(w, "  配置已写入、服务运行中均不代表 Telegram 已连通；此查询不发送消息。")
}

func (a *App) printTelegramTargetStatus() {
	rows, err := a.telegramTargetStatuses()
	var text strings.Builder
	writeTelegramTargetStatuses(&text, rows, err)
	fmt.Print(text.String())
}
