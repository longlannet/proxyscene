package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// systemctl exiting (or being killed) does not cancel the job queued in systemd.
// Callers must retain their recovery journal rather than compensate while a
// restart may still be applying the configuration.
var errSystemdRestartUnsettled = errors.New("systemd 重启尚未确认完成")

type systemdCommandBuilder func(context.Context, ...string) (*exec.Cmd, error)

const (
	systemdRestartMinimumWait = 5 * time.Minute
	systemdRestartMaximumWait = 15 * time.Minute
	// Allow the service's complete stop/start sequence plus systemd overhead.
	systemdRestartGrace         = 30 * time.Second
	systemdRestartProgressEvery = 20 * time.Second
	systemdRestartStatusTimeout = 5 * time.Second
	systemdRestartCommandWait   = 250 * time.Millisecond
)

type systemdRestartState struct {
	load, active, job, stop, start string
}

func runSystemdRestart(label, service string, command systemdCommandBuilder) error {
	state, err := readSystemdRestartState(service, command)
	if err != nil {
		return fmt.Errorf("%s：未提交重启，%w", label, err)
	}
	if err := state.ready(); err != nil {
		return fmt.Errorf("%s：未提交新的重启，%w", label, err)
	}
	budget, err := systemdRestartBudget(state.stop, state.start)
	if err != nil {
		return fmt.Errorf("%s：未提交重启，%w", label, err)
	}
	return waitSystemdRestart(label, service, budget, systemdRestartProgressEvery, os.Stdout, command)
}

func readSystemdRestartState(service string, command systemdCommandBuilder) (systemdRestartState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), systemdRestartStatusTimeout)
	defer cancel()
	cmd, err := command(ctx, "show", "--property=LoadState", "--property=ActiveState", "--property=Job", "--property=TimeoutStopUSec", "--property=TimeoutStartUSec", "--", service)
	if err != nil {
		return systemdRestartState{}, err
	}
	if cmd == nil {
		return systemdRestartState{}, errors.New("无法构造 systemd 状态查询")
	}
	var output telegramStatusBuffer
	cmd.Stdout, cmd.Stderr = &output, nil
	cmd.WaitDelay = systemdRestartCommandWait
	if err := cmd.Run(); err != nil {
		return systemdRestartState{}, errors.New("无法在限定时间和输出范围内查询 systemd 重启状态")
	}
	return parseSystemdRestartState(output.String())
}

func parseSystemdRestartState(output string) (systemdRestartState, error) {
	fields := make(map[string]string, 5)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			return systemdRestartState{}, errors.New("systemd 重启状态格式无效")
		}
		switch key {
		case "LoadState", "ActiveState", "Job", "TimeoutStopUSec", "TimeoutStartUSec":
		default:
			return systemdRestartState{}, errors.New("systemd 重启状态包含未知字段")
		}
		if _, exists := fields[key]; exists {
			return systemdRestartState{}, errors.New("systemd 重启状态字段重复")
		}
		fields[key] = strings.TrimSpace(value)
	}
	if len(fields) != 5 || fields["LoadState"] == "" || fields["ActiveState"] == "" {
		return systemdRestartState{}, errors.New("systemd 重启状态字段不完整")
	}
	return systemdRestartState{fields["LoadState"], fields["ActiveState"], fields["Job"], fields["TimeoutStopUSec"], fields["TimeoutStartUSec"]}, nil
}

func (s systemdRestartState) ready() error {
	if s.job != "" {
		return unsettledSystemdRestart("服务已有待完成的 systemd 任务")
	}
	switch s.active {
	case "active", "inactive", "failed":
	default:
		return unsettledSystemdRestart("服务正在切换或状态无法确认")
	}
	if s.load != "loaded" {
		return errors.New("目标服务未正常加载")
	}
	return nil
}

func unsettledSystemdRestart(reason string) error {
	return fmt.Errorf("%w：%s；systemd 任务可能仍在运行，请等待服务稳定后再执行 proxyscene recover", errSystemdRestartUnsettled, reason)
}

func systemdRestartBudget(stop, start string) (time.Duration, error) {
	stopTime, err := parseSystemdRestartDuration(stop)
	if err != nil {
		return 0, errors.New("无法确认有限的 TimeoutStopUSec，拒绝重启")
	}
	startTime, err := parseSystemdRestartDuration(start)
	if err != nil {
		return 0, errors.New("无法确认有限的 TimeoutStartUSec，拒绝重启")
	}
	// Compare before adding so even individually valid, very large durations
	// cannot overflow into a short wait budget.
	available := systemdRestartMaximumWait - systemdRestartGrace
	if stopTime > available || startTime > available-stopTime {
		return 0, fmt.Errorf("服务停止和启动预算超过等待上限 %s，拒绝重启", systemdRestartMaximumWait)
	}
	budget := stopTime + startTime + systemdRestartGrace
	if budget < systemdRestartMinimumWait {
		budget = systemdRestartMinimumWait
	}
	return budget, nil
}

func parseSystemdRestartDuration(value string) (time.Duration, error) {
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return 0, errors.New("缺少服务超时")
	}
	var total time.Duration
	for _, part := range parts {
		// systemctl formats minutes as "min" and separates duration terms
		// with spaces; Go also accepts compact values and fractional units.
		if strings.ContainsAny(part, "+-") {
			return 0, errors.New("服务超时不能带符号")
		}
		duration, err := time.ParseDuration(strings.ReplaceAll(part, "min", "m"))
		if err != nil || duration < 0 || duration > time.Duration(1<<63-1)-total {
			return 0, errors.New("服务超时无效或溢出")
		}
		total += duration
	}
	// Zero disables timeouts in systemd unit settings; never infer a safe
	// restart budget from zero or an explicitly unlimited value.
	if total <= 0 {
		return 0, errors.New("服务超时必须有限且大于零")
	}
	return total, nil
}

// The small wait helper accepts the budget, interval and writer so tests can
// exercise timeout/progress boundaries using isolated child processes.
func waitSystemdRestart(label, service string, budget, progressEvery time.Duration, output io.Writer, command systemdCommandBuilder) error {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd, err := command(ctx, "try-restart", "--", service)
	if err != nil {
		return err
	}
	if cmd == nil {
		return errors.New("无法构造 systemd 重启命令")
	}
	cmd.Stdout, cmd.Stderr = nil, nil
	cmd.WaitDelay = systemdRestartCommandWait
	if err := cmd.Start(); err != nil {
		return commandFailed(label, err)
	}
	started := time.Now()
	fmt.Fprintf(output, "%s：等待服务重启，最长 %s（停止、启动及缓冲预算）。\n", label, budget)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(progressEvery)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				return unsettledSystemdRestart(fmt.Sprintf("等待已达 %s 上限；终止 systemctl 等待不会取消服务任务", budget))
			}
			if err == nil {
				return nil
			}
			// A normal command failure permits compensation only after another
			// bounded, read-only query proves no service job is still pending.
			state, stateErr := readSystemdRestartState(service, command)
			if stateErr != nil || state.ready() != nil {
				return unsettledSystemdRestart("重启命令失败，无法确认服务任务已经结束")
			}
			return commandFailed(label, err)
		case <-ticker.C:
			fmt.Fprintf(output, "%s：仍在等待服务重启，已等待 %s / 最长 %s。\n", label, time.Since(started).Truncate(time.Second), budget)
		}
	}
}
