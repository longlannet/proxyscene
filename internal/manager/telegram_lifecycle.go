package manager

import "fmt"

var (
	telegramValidateHermesRestartPolicy        = validateHermesTelegramRestartPolicy
	telegramValidateHermesReleaseRestartPolicy = validateHermesTelegramReleaseRestartPolicy
)

func telegramTargetRunning(target systemdTargetName, identity *persistedUserIdentity) (bool, error) {
	state, err := telegramReadServiceState(target, identity)
	if err != nil {
		return false, fmt.Errorf("无法确认目标 %s 的运行状态，未执行重启", canonicalTelegramTargetName(target))
	}
	if state.LoadState != "loaded" {
		return false, fmt.Errorf("目标 %s 未正常加载，未执行重启", canonicalTelegramTargetName(target))
	}
	switch state.ActiveState {
	case "inactive", "failed":
		return false, nil
	case "active":
		return true, nil
	default:
		return false, fmt.Errorf("目标 %s 正在切换状态，请稍后重试", canonicalTelegramTargetName(target))
	}
}

func validateHermesTelegramRestartSafety(target systemdTargetName, identity *persistedUserIdentity) error {
	running, err := telegramTargetRunning(target, identity)
	if err != nil || !running {
		return err
	}
	return telegramValidateHermesRestartPolicy(target, identity)
}

func confirmTelegramTargetRunning(target systemdTargetName, identity *persistedUserIdentity) error {
	running, err := telegramTargetRunning(target, identity)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("目标 %s 重启后未运行，保留待完成记录；尚未验证 Telegram 连接", canonicalTelegramTargetName(target))
	}
	return nil
}

// A successful reload confirms the new runtime generation without restarting
// unrelated gateway channels. Unsupported/older gateways use service restart.
func (a *App) reconcileOpenClawTelegramTarget(target systemdTargetName, plan *openClawTelegramReloadPlan) error {
	handled, err := a.finishOpenClawTelegramReload(target, plan)
	if err != nil {
		return err
	}
	key := canonicalTelegramTargetName(target)
	if handled {
		if plan.inactive {
			fmt.Printf("目标 %s 未运行，仅保存配置；尚未验证 Telegram 连接\n", key)
		} else {
			fmt.Printf("目标 %s 新配置及 Telegram 账号运行状态已确认，网关进程未重启；未额外探测 Telegram API\n", key)
		}
		return nil
	}
	fmt.Printf("目标 %s 未取得频道重载确认，回退到服务重启；尚未验证 Telegram 连接\n", key)
	return a.restartTelegramTarget(target)
}
