package manager

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
)

const runtimeTransitionVersion = 1
const maxRuntimeTransitionBytes = int64(3*maxStoreBytes + 32<<20)

var runtimeValidateConfig = func(cfg Config) error { return cfg.Validate() }
var runtimeWriteTransition = writeFileAtomic
var runtimePlanCore = func(a *App, st *Store) (*runtimeCorePlan, error) { return a.planCoreRuntime(st) }
var runtimeApplyCore = func(a *App, p *runtimeCorePlan) error { return a.applyCorePlan(p) }
var runtimeCompensateCore = func(a *App, p *runtimeCorePlan, checkpoint func() error) error {
	return a.compensateCorePlan(p, checkpoint)
}
var runtimePersistStore = func(a *App, st *Store) error { return a.saveStore(st) }

type runtimeStoreEvidence struct {
	Present bool   `json:"present"`
	Digest  string `json:"digest"`
}
type runtimeTransitionStep struct {
	Name     string `json:"name"`
	Started  bool   `json:"started"`
	Restored bool   `json:"restored"`
}
type runtimeTransitionRecord struct {
	Version         int                      `json:"version"`
	Phase           string                   `json:"phase"`
	Installation    installationOwnership    `json:"installation"`
	Before          *Store                   `json:"before"`
	Candidate       *Store                   `json:"candidate"`
	BeforeMain      runtimeStoreEvidence     `json:"before_main"`
	BeforeBackup    runtimeStoreEvidence     `json:"before_backup"`
	CandidateDigest string                   `json:"candidate_digest"`
	Compensation    *Store                   `json:"compensation,omitempty"`
	Core            *runtimeCorePlan         `json:"core,omitempty"`
	Resources       *runtimeResourceSnapshot `json:"resources"`
	Steps           []runtimeTransitionStep  `json:"steps"`
}

type runtimeMutationPlan struct {
	record       *runtimeTransitionRecord
	beforeApp    *App
	candidateApp *App
	telegram     *telegramPlan
	execution    *Store
}

func (a *App) runtimeTransitionPath() string {
	return filepath.Join(a.cfg.CoreDir, "runtime-transition.json")
}
func storeEvidence(path string) (runtimeStoreEvidence, error) {
	b, err := readRegularFileNoFollow(path, maxStoreBytes)
	if errors.Is(err, os.ErrNotExist) {
		return runtimeStoreEvidence{}, nil
	}
	if err != nil {
		return runtimeStoreEvidence{}, err
	}
	return runtimeStoreEvidence{true, contentDigest(b)}, nil
}
func encodedRuntimeStore(st *Store) ([]byte, error) {
	clone := cloneStore(st)
	normalizeStore(clone)
	if _, err := validateStoreSemantics(clone); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(clone, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if err := validateStoreDataSize(b, maxStoreBytes); err != nil {
		return nil, err
	}
	return b, nil
}
func (a *App) highestStoreGeneration() (uint64, error) {
	var generation uint64
	for _, path := range []string{a.cfg.StorePath(), a.cfg.StoreBackupPath()} {
		b, err := readRegularFileNoFollow(path, maxStoreBytes)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		st, _, err := decodeStore(b)
		if err != nil {
			continue
		}
		if st.Generation > generation {
			generation = st.Generation
		}
	}
	return generation, nil
}
func (a *App) hasRuntimeTransition() (bool, error) {
	_, err := readRegularFileNoFollow(a.runtimeTransitionPath(), maxRuntimeTransitionBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
func (a *App) writeRuntimeTransition(r *runtimeTransitionRecord) error {
	if err := a.validateRuntimeTransition(r); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if int64(len(b)) > maxRuntimeTransitionBytes {
		return fmt.Errorf("运行事务恢复记录过大")
	}
	return runtimeWriteTransition(a.runtimeTransitionPath(), append(b, '\n'), 0600)
}
func (a *App) readRuntimeTransition() (*runtimeTransitionRecord, error) {
	b, err := readRegularFileNoFollow(a.runtimeTransitionPath(), maxRuntimeTransitionBytes)
	if err != nil {
		return nil, err
	}
	var r runtimeTransitionRecord
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("运行事务恢复记录损坏，已保留：%w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("运行事务记录含尾随内容")
	}
	if err := a.validateRuntimeTransition(&r); err != nil {
		return nil, err
	}
	return &r, nil
}
func (a *App) validateRuntimeTransition(r *runtimeTransitionRecord) error {
	if r == nil || r.Version != runtimeTransitionVersion || r.Before == nil || r.Candidate == nil || len(r.Steps) > 4 {
		return fmt.Errorf("运行事务记录无效")
	}
	if r.Installation != a.expectedInstallationOwnership() {
		return fmt.Errorf("运行事务记录不属于当前安装")
	}
	switch r.Phase {
	case "applying", "committing", "compensating", "committed":
	default:
		return fmt.Errorf("运行事务阶段无效")
	}
	for _, st := range []*Store{r.Before, r.Candidate} {
		if _, err := encodedRuntimeStore(st); err != nil {
			return err
		}
	}
	raw, err := encodedRuntimeStore(r.Candidate)
	if err != nil {
		return err
	}
	if contentDigest(raw) != r.CandidateDigest {
		return fmt.Errorf("运行事务候选摘要不匹配")
	}
	if r.Candidate.Generation <= r.Before.Generation {
		return fmt.Errorf("运行事务候选 generation 未前进")
	}
	for _, evidence := range []runtimeStoreEvidence{r.BeforeMain, r.BeforeBackup} {
		if _, err := hex.DecodeString(evidence.Digest); err != nil {
			return fmt.Errorf("运行事务 Store 证据摘要无效")
		}
		if (!evidence.Present && evidence.Digest != "") || (evidence.Present && len(evidence.Digest) != 64) {
			return fmt.Errorf("运行事务 Store 证据摘要无效")
		}
	}
	if r.Compensation != nil {
		if _, err := encodedRuntimeStore(r.Compensation); err != nil {
			return err
		}
		prior := cloneStore(r.Before)
		prior.Generation = r.Compensation.Generation
		if r.Phase != "compensating" || r.Compensation.Generation <= r.Candidate.Generation || !reflect.DeepEqual(prior, cloneStore(r.Compensation)) {
			return fmt.Errorf("运行事务补偿内容或 generation 无效")
		}
	}
	if r.Resources == nil {
		if r.Core != nil || len(r.Steps) != 0 || !reflect.DeepEqual(r.Before.RuntimeConfig, r.Candidate.RuntimeConfig) {
			return fmt.Errorf("纯状态事务不能包含运行态变更")
		}
		return nil
	}
	if err := validateCorePlanRecord(r.Core); err != nil {
		return err
	}
	if err := validateRuntimeResources(r.Resources); err != nil {
		return err
	}
	if NewApp(r.Resources.CandidateConfig).expectedInstallationOwnership() != r.Installation || NewApp(r.Resources.BeforeConfig).expectedInstallationOwnership() != r.Installation {
		return fmt.Errorf("资源恢复记录的安装定位不一致")
	}
	if r.Candidate.RuntimeConfig == nil || !runtimeConfigsEqual(r.Candidate.RuntimeConfig, r.Resources.CandidateConfig.runtimeConfig()) {
		return fmt.Errorf("资源恢复记录的候选运行配置不一致")
	}
	seen := map[string]bool{}
	for _, step := range r.Steps {
		if seen[step.Name] || (!step.Started && step.Restored) {
			return fmt.Errorf("运行事务步骤重复或进度无效")
		}
		seen[step.Name] = true
		if step.Name == "core" {
			if r.Core == nil || r.Core.Noop {
				return fmt.Errorf("核心事务步骤无计划")
			}
		} else if !slices.Contains(r.Resources.Scenes, Scene(step.Name)) {
			return fmt.Errorf("运行事务步骤不在资源计划中")
		}
	}
	return nil
}
func (a *App) removeRuntimeTransition() error {
	if err := os.Remove(a.runtimeTransitionPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsyncDir(a.cfg.CoreDir)
}

func (a *App) runtimeOwnershipEvidence() (map[Scene]bool, error) {
	result := map[Scene]bool{}
	paths := map[Scene][]string{
		SceneGlobal:   {a.globalProxyJournalPath(), a.globalProxyJournalBackupPath()},
		SceneDev:      {a.cfg.DevBackupPath()},
		SceneTelegram: {a.telegramProxyJournalPath(), a.telegramProxyJournalBackupPath(), a.openClawJournalPath(), a.openClawJournalBackupPath()},
	}
	for scene, names := range paths {
		for _, path := range names {
			_, err := readRegularFileNoFollow(path, maxRuntimeTransitionBytes)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			result[scene] = true
		}
	}
	return result, nil
}
func runtimeSceneChanged(before, candidate Config, scene Scene) bool {
	switch scene {
	case SceneGlobal:
		return before.HTTPAddr(scene) != candidate.HTTPAddr(scene) || before.GlobalSocksAddr() != candidate.GlobalSocksAddr()
	case SceneDev:
		return before.HTTPAddr(scene) != candidate.HTTPAddr(scene) || before.DevTargetUser != candidate.DevTargetUser
	case SceneTelegram:
		return before.HTTPAddr(scene) != candidate.HTTPAddr(scene) || before.TGSocksPort != candidate.TGSocksPort || before.ManageOpenClawConfig != candidate.ManageOpenClawConfig || !slices.Equal(before.TGTargetServices, candidate.TGTargetServices)
	}
	return false
}
func scopedRuntimeScene(mode storeRuntimeSyncMode) Scene {
	switch mode {
	case storeRuntimeSyncGlobal:
		return SceneGlobal
	case storeRuntimeSyncDev:
		return SceneDev
	case storeRuntimeSyncTelegram:
		return SceneTelegram
	}
	return ""
}
func (a *App) planRuntimeMutation(before, candidate *Store, mode storeRuntimeSyncMode) (*runtimeMutationPlan, error) {
	main, err := storeEvidence(a.cfg.StorePath())
	if err != nil {
		return nil, err
	}
	backup, err := storeEvidence(a.cfg.StoreBackupPath())
	if err != nil {
		return nil, err
	}
	if err := a.validateRuntimeStoreBefore(before, main, backup); err != nil {
		return nil, err
	}

	owned, err := a.runtimeOwnershipEvidence()
	if err != nil {
		return nil, err
	}
	if before.RuntimeConfig == nil && (hasEnabledScene(before) || len(before.TelegramTargets) > 0 || owned[SceneGlobal] || owned[SceneDev] || owned[SceneTelegram]) {
		return nil, fmt.Errorf("旧运行状态缺少 RuntimeConfig，拒绝猜测历史配置；已保留配置和 ownership，请先核对迁移证据")
	}
	next := NewApp(a.cfg)
	if candidate.SceneEnabled[SceneDev] && next.cfg.DevTargetUser == "" {
		u, err := next.devTargetUser()
		if err != nil {
			return nil, err
		}
		next.cfg.DevTargetUser = u
	}
	if err := runtimeValidateConfig(next.cfg); err != nil {
		return nil, err
	}
	old := NewApp(next.cfg)
	if before.RuntimeConfig != nil {
		old.cfg = before.RuntimeConfig.applyTo(old.cfg)
	}
	candidate.RuntimeConfig = next.cfg.runtimeConfig()
	generation, err := a.highestStoreGeneration()
	if err != nil {
		return nil, err
	}
	if generation < before.Generation {
		generation = before.Generation
	}
	if generation >= ^uint64(0)-1 {
		return nil, fmt.Errorf("状态 generation 无法为失败补偿保留新代号")
	}
	candidate.Generation = generation + 1
	data, err := encodedRuntimeStore(candidate)
	if err != nil {
		return nil, err
	}
	scenes := []Scene{}
	for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
		changed := before.SceneEnabled[scene] != candidate.SceneEnabled[scene]
		requested := scopedRuntimeScene(mode) == scene
		reconcile := mode == storeRuntimeSyncAll && (candidate.SceneEnabled[scene] || owned[scene] || (scene == SceneTelegram && len(before.TelegramTargets) > 0))
		configChanged := runtimeSceneChanged(old.cfg, next.cfg, scene) && (candidate.SceneEnabled[scene] || owned[scene])
		if changed || requested || reconcile || configChanged {
			scenes = append(scenes, scene)
		}
	}
	plan := &runtimeMutationPlan{beforeApp: old, candidateApp: next}
	if slices.Contains(scenes, SceneTelegram) {
		plan.telegram, err = next.planTelegramTransition(before, candidate)
		if err != nil {
			return nil, err
		}
	}
	resources, err := captureRuntimeResources(old, next, before, candidate, scenes, plan.telegram)
	if err != nil {
		return nil, err
	}
	core, err := runtimePlanCore(next, candidate)
	if err != nil {
		return nil, err
	}
	if before.RuntimeConfig == nil && core != nil && (core.BeforeConfig.Present || core.BeforeService.Active == "active") {
		return nil, fmt.Errorf("核心仍有历史运行状态但缺少 RuntimeConfig，拒绝自动迁移")
	}

	r := &runtimeTransitionRecord{Version: runtimeTransitionVersion, Phase: "applying", Installation: a.expectedInstallationOwnership(), Before: cloneStore(before), Candidate: cloneStore(candidate), CandidateDigest: contentDigest(data), BeforeMain: main, BeforeBackup: backup, Core: core, Resources: resources}
	// Disabled external routes retire before stopping the core. Enabled routes
	// are attached after the core has loaded the candidate configuration.
	for _, scene := range scenes {
		if !candidate.SceneEnabled[scene] {
			r.Steps = append(r.Steps, runtimeTransitionStep{Name: string(scene)})
		}
	}
	if core != nil && !core.Noop {
		r.Steps = append(r.Steps, runtimeTransitionStep{Name: "core"})
	}
	for _, scene := range scenes {
		if candidate.SceneEnabled[scene] {
			r.Steps = append(r.Steps, runtimeTransitionStep{Name: string(scene)})
		}
	}
	plan.record = r
	plan.execution = cloneStore(r.Candidate)
	if err := a.validateRuntimeTransition(r); err != nil {
		return nil, err
	}
	if err := a.validateRuntimeStoreUnchanged(r); err != nil {
		return nil, err
	}
	return plan, nil
}
func (a *App) validateRuntimeMutation(p *runtimeMutationPlan) error {
	r := p.record
	if err := a.validateRuntimeStoreUnchanged(r); err != nil {
		return err
	}
	if r.Resources == nil {
		return nil
	}
	if p.telegram != nil {
		if err := p.candidateApp.validateTelegramPlan(r.Candidate, p.telegram); err != nil {
			return err
		}
	}
	// A second read-only capture checks all file/journal/value preconditions.
	observed, err := captureRuntimeResources(p.beforeApp, p.candidateApp, r.Before, r.Candidate, r.Resources.Scenes, p.telegram)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(observed, r.Resources) {
		return fmt.Errorf("场景资源在规划后变化，请重试")
	}
	if err := p.candidateApp.validateCorePlan(r.Core); err != nil {
		return err
	}
	return a.validateRuntimeStoreUnchanged(r)
}
func (a *App) executeRuntimeStep(p *runtimeMutationPlan, name string) error {
	if name == "core" {
		return runtimeApplyCore(p.candidateApp, p.record.Core)
	}
	scene := Scene(name)
	if scene == SceneTelegram {
		if p.record.Candidate.SceneEnabled[scene] {
			_, err := p.candidateApp.applyTelegramPlan(p.execution, p.telegram)
			return err
		}
		return p.candidateApp.restoreTelegramPlan(p.execution, p.telegram)
	}
	if p.record.Candidate.SceneEnabled[scene] {
		return p.candidateApp.applyScene(p.execution, scene)
	}
	return p.candidateApp.restoreScene(p.execution, scene)
}

func (a *App) commitPlannedStoreMutation(st *Store, mutate func(*Store) error, mode storeRuntimeSyncMode) error {
	pending, err := a.hasRuntimeTransition()
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("存在未完成运行事务；请先执行 proxyscene recover，恢复前拒绝新的修改")
	}
	before := cloneStore(st)
	candidate := cloneStore(st)
	if err := mutate(candidate); err != nil {
		return err
	}
	var p *runtimeMutationPlan
	if mode == storeRuntimeSyncNone {
		p, err = a.planMetadataMutation(before, candidate)
	} else {
		p, err = a.planRuntimeMutation(before, candidate, mode)
	}
	if err != nil {
		return err
	}
	if err := a.validateRuntimeMutation(p); err != nil {
		return err
	}
	r := p.record
	if err := a.writeRuntimeTransition(r); err != nil {
		return err
	}
	for i := range r.Steps {
		if err := a.validateRuntimeStoreUnchanged(r); err != nil {
			return errors.Join(err, a.compensateRuntimeTransition(r))
		}
		r.Steps[i].Started = true
		if err := a.writeRuntimeTransition(r); err != nil {
			return errors.Join(err, a.compensateRuntimeTransition(r))
		}
		if err := a.executeRuntimeStep(p, r.Steps[i].Name); err != nil {
			// A timed-out systemctl client does not cancel the systemd job.
			// Keep the fixed plan until recovery can prove the service is stable.
			if errors.Is(err, errSystemdRestartUnsettled) {
				return fmt.Errorf("服务重启结果尚未确定，已保留运行事务；等待服务稳定后执行 proxyscene recover：%w", err)
			}
			return errors.Join(err, a.compensateRuntimeTransition(r))
		}
	}
	// Execution may retire legacy evidence. Commit exactly that finished state.
	r.Candidate = cloneStore(p.execution)
	encoded, err := encodedRuntimeStore(r.Candidate)
	if err != nil {
		return errors.Join(err, a.compensateRuntimeTransition(r))
	}
	r.CandidateDigest = contentDigest(encoded)
	r.Phase = "committing"
	if err := a.writeRuntimeTransition(r); err != nil {
		return errors.Join(err, a.compensateRuntimeTransition(r))
	}
	if err := a.validateRuntimeStoreUnchanged(r); err != nil {
		return fmt.Errorf("Store 在执行期间变化，已保留运行事务：%w", err)
	}
	p.candidateApp.runtimeStoreCommit = r
	defer func() { p.candidateApp.runtimeStoreCommit = nil }()
	committing := cloneStore(r.Candidate)
	committing.Generation--
	if err := runtimePersistStore(p.candidateApp, committing); err != nil {
		main, readErr := storeEvidence(a.cfg.StorePath())
		if readErr != nil || main == (runtimeStoreEvidence{true, r.CandidateDigest}) {
			return errors.Join(err, readErr, fmt.Errorf("状态提交结果需恢复确认；已保留运行事务，请执行 proxyscene recover"))
		}
		return errors.Join(err, a.compensateRuntimeTransition(r))
	}
	if err := a.validateRuntimeStoreCommitted(r); err != nil {
		return err
	}
	if committing.Generation != r.Candidate.Generation {
		return fmt.Errorf("Store 提交身份不匹配，已保留运行事务")
	}
	restoreStore(st, committing)
	a.cfg = p.candidateApp.cfg
	r.Phase = "committed"
	if err := a.writeRuntimeTransition(r); err != nil {
		return fmt.Errorf("运行状态已提交，事务收尾失败；请执行 proxyscene recover：%w", err)
	}
	if err := a.validateRuntimeStoreCommitted(r); err != nil {
		return err
	}
	if err := finalizeRuntimeTransitionResources(r); err != nil {
		return fmt.Errorf("运行状态已提交，资源收尾失败；请执行 proxyscene recover：%w", err)
	}
	if err := a.removeRuntimeTransition(); err != nil {
		return fmt.Errorf("运行状态已提交，清理恢复记录失败；请执行 proxyscene recover 确认：%w", err)
	}
	return nil
}

func (a *App) compensateRuntimeTransition(r *runtimeTransitionRecord) error {
	if err := a.settleRuntimeStoreQuarantines(r); err != nil {
		return err
	}
	if err := a.validateTransitionStoreBeforeCompensation(r); err != nil {
		return err
	}
	r.Phase = "compensating"
	if err := a.writeRuntimeTransition(r); err != nil {
		return fmt.Errorf("无法记录补偿进度，保留事务：%w", err)
	}
	app := NewApp(a.cfg)
	if r.Resources != nil {
		app.cfg = r.Resources.CandidateConfig
	}
	for i := len(r.Steps) - 1; i >= 0; i-- {
		step := &r.Steps[i]
		if !step.Started || step.Restored {
			continue
		}
		var err error
		if step.Name == "core" {
			err = runtimeCompensateCore(app, r.Core, func() error { return a.writeRuntimeTransition(r) })
		} else {
			err = compensateRuntimeResources(r.Resources, []Scene{Scene(step.Name)}, func() error { return a.writeRuntimeTransition(r) })
		}
		if err != nil {
			return fmt.Errorf("%s 补偿未完成，已保留固定恢复计划：%w", step.Name, err)
		}
		step.Restored = true
		if err := a.writeRuntimeTransition(r); err != nil {
			return err
		}
	}
	return a.finishRuntimeCompensationStore(r)
}

func (a *App) validateTransitionStoreBeforeCompensation(r *runtimeTransitionRecord) error {
	main, err := storeEvidence(a.cfg.StorePath())
	if err != nil {
		return err
	}
	backup, err := storeEvidence(a.cfg.StoreBackupPath())
	if err != nil {
		return err
	}
	candidate := runtimeStoreEvidence{true, r.CandidateDigest}
	var compensation runtimeStoreEvidence
	if r.Compensation != nil {
		b, err := encodedRuntimeStore(r.Compensation)
		if err != nil {
			return err
		}
		compensation = runtimeStoreEvidence{true, contentDigest(b)}
	}
	if main != r.BeforeMain && (!compensation.Present || main != compensation) {
		return fmt.Errorf("Store 主提交点已不在本事务允许的补偿状态，保留全部运行态和恢复记录")
	}
	if backup != r.BeforeBackup && backup != candidate && (!compensation.Present || backup != compensation) {
		return fmt.Errorf("Store 备份在事务外变化，保留全部运行态和恢复记录")
	}
	return nil
}
func (a *App) finishRuntimeCompensationStore(r *runtimeTransitionRecord) error {
	if err := a.validateTransitionStoreBeforeCompensation(r); err != nil {
		return err
	}
	main, err := storeEvidence(a.cfg.StorePath())
	if err != nil {
		return err
	}
	backup, err := storeEvidence(a.cfg.StoreBackupPath())
	if err != nil {
		return err
	}
	if main == r.BeforeMain && backup == r.BeforeBackup {
		return a.removeRuntimeTransition()
	}
	if r.Compensation == nil {
		highest, err := a.highestStoreGeneration()
		if err != nil {
			return err
		}
		if highest == ^uint64(0) {
			return fmt.Errorf("恢复状态 generation 已耗尽")
		}
		r.Compensation = cloneStore(r.Before)
		r.Compensation.Generation = highest + 1
		if err := a.writeRuntimeTransition(r); err != nil {
			return err
		}
	}
	data, err := encodedRuntimeStore(r.Compensation)
	if err != nil {
		return err
	}
	expected := runtimeStoreEvidence{true, contentDigest(data)}
	// Replaying these exact bytes is idempotent, including a crash after only
	// backup committed. The reserved generation is never reused for other data.
	if backup != expected {
		if err := runtimeWriteStoreCAS(a.cfg.StoreBackupPath(), backup, data); err != nil {
			return err
		}
	}
	if err := a.validateTransitionStoreBeforeCompensation(r); err != nil {
		return err
	}
	if main != expected {
		if err := runtimeWriteStoreCAS(a.cfg.StorePath(), main, data); err != nil {
			return err
		}
	}
	for _, path := range []string{a.cfg.StorePath(), a.cfg.StoreBackupPath()} {
		observed, err := storeEvidence(path)
		if err != nil {
			return err
		}
		if observed != expected {
			return fmt.Errorf("补偿后的 Store 主文件或备份偏离固定补偿，保留恢复记录：%s", path)
		}
	}
	return a.removeRuntimeTransition()
}

func (a *App) recoverRuntimeTransition() (bool, error) {
	r, err := a.readRuntimeTransition()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if err := a.settleRuntimeStoreQuarantines(r); err != nil {
		return true, err
	}
	main, err := storeEvidence(a.cfg.StorePath())
	if err != nil {
		return true, err
	}
	if main == (runtimeStoreEvidence{true, r.CandidateDigest}) {
		if r.Phase != "committing" && r.Phase != "committed" {
			return true, fmt.Errorf("运行事务阶段与 Store 提交点矛盾，保留证据")
		}
		if err := a.validateRuntimeStoreCommitted(r); err != nil {
			return true, err
		}
		if err := finalizeRuntimeTransitionResources(r); err != nil {
			return true, err
		}
		return true, a.removeRuntimeTransition()
	}
	if r.Phase == "committed" {
		return true, fmt.Errorf("已提交事务的 Store 被修改，保留恢复记录")
	}
	return true, a.compensateRuntimeTransition(r)
}
func (a *App) recoverRuntimeCommand() error {
	if err := requireRoot(); err != nil {
		return err
	}
	return a.withStoreLock(func() error {
		recovered, err := a.recoverRuntimeTransition()
		if err != nil {
			return err
		}
		if recovered {
			fmt.Println("已按固定计划完成运行事务恢复；未重新发现或接管其他服务")
		} else {
			fmt.Println("没有待恢复的运行事务")
		}
		return nil
	})
}

// Install's bootstrap units/account are created only after the same read-only
// scene checks used by ordinary mutations. Fresh, stopped installations have
// no service identity yet; they validate the candidate before bootstrapping it.
func (a *App) preflightInstallation(before *Store, raw string) error {
	pending, err := a.hasRuntimeTransition()
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("存在未完成运行事务，请先执行 proxyscene recover")
	}
	if err := a.ensureXrayInstalled(); err != nil {
		return err
	}
	candidate := cloneStore(before)
	if raw != "" {
		if _, err := a.addNode(candidate, raw, "", "default"); err != nil {
			return err
		}
	}
	if err := runtimeValidateConfig(a.cfg); err != nil {
		return err
	}
	if _, err := encodedRuntimeStore(candidate); err != nil {
		return err
	}
	owned, err := a.runtimeOwnershipEvidence()
	if err != nil {
		return err
	}
	if before.RuntimeConfig == nil && !hasEnabledScene(before) && len(before.TelegramTargets) == 0 && !owned[SceneGlobal] && !owned[SceneDev] && !owned[SceneTelegram] {
		config, err := readCoreFile(a.cfg.XrayConfig())
		if err != nil {
			return err
		}
		service, err := coreReadService(a)
		if err != nil {
			return err
		}
		if config.Present || service.Active == "active" {
			return fmt.Errorf("缺少历史 RuntimeConfig 且核心仍有运行状态，拒绝初始化覆盖")
		}
		return nil
	}
	plan, err := a.planRuntimeMutation(before, candidate, storeRuntimeSyncAll)
	if err != nil {
		return err
	}
	return a.validateRuntimeMutation(plan)
}
