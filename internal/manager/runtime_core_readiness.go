package manager

import (
	"fmt"
	"time"
)

const coreRestartReadyTimeout = 2 * time.Second
const coreRestartPollInterval = 20 * time.Millisecond

var coreRestartNow = time.Now
var coreRestartSleep = time.Sleep

// Type=simple reports a successful start before the service child has exec'd.
// Only callers that just completed an explicit restart may wait for that child.
// Keep its PID and invocation fixed, reject unit/file changes immediately, and
// require the unchanged strict process proof before acknowledging readiness.
func (a *App) waitCoreRestartExecution(unit, config coreFileState, identity localUserIdentity, digest string) error {
	expected, err := coreReadService(a)
	if err != nil {
		return err
	}
	if expected.Load != "loaded" || expected.Active != "active" || expected.Sub != "running" || expected.NeedReload != "no" || expected.PID <= 0 || expected.Invocation == "" {
		return fmt.Errorf("核心重启后没有可绑定的活动进程")
	}
	deadline := coreRestartNow().Add(coreRestartReadyTimeout)
	for {
		state, err := coreReadService(a)
		if err != nil {
			return err
		}
		if state != expected {
			return fmt.Errorf("核心重启就绪等待期间运行代际变化，拒绝接受另一进程")
		}
		for path, want := range map[string]coreFileState{coreUnitPath(a.cfg): unit, a.cfg.XrayConfig(): config} {
			current, err := readCoreFile(path)
			if err != nil {
				return err
			}
			if !coreFilesEqual(current, want) {
				return fmt.Errorf("核心重启就绪等待期间文件变化：%s", path)
			}
		}
		if err := coreVerifyExecution(a, state, unit.Content, identity, digest, false); err != nil {
			return err
		}
		err = coreVerifyExecution(a, state, unit.Content, identity, digest, true)
		if err == nil {
			return nil
		}
		remaining := deadline.Sub(coreRestartNow())
		if remaining <= 0 {
			return fmt.Errorf("核心重启后在 %s 内未通过精确进程验证，保留失败状态：%w", coreRestartReadyTimeout, err)
		}
		coreRestartSleep(min(coreRestartPollInterval, remaining))
	}
}
