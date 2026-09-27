package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
)

// The outer transition record persists this typed before-image before any
// resource step starts. Paths are always derived from its validated locators
// and identities; a recovery record cannot nominate an arbitrary file.
type runtimeResourceSnapshot struct {
	BeforeConfig    Config                    `json:"before_config"`
	CandidateConfig Config                    `json:"candidate_config"`
	Scenes          []Scene                   `json:"scenes"`
	Global          *runtimeGlobalResource    `json:"global,omitempty"`
	Dev             *runtimeDevResource       `json:"dev,omitempty"`
	Telegram        *runtimeTelegramResources `json:"telegram,omitempty"`
}

type runtimeArtifactState struct {
	Present bool   `json:"present"`
	Content []byte `json:"content,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
	UID     uint32 `json:"uid,omitempty"`
	GID     uint32 `json:"gid,omitempty"`
}

type runtimeGlobalResource struct {
	Before             *globalProxyJournal      `json:"before,omitempty"`
	Files              []runtimeArtifactState   `json:"files"`
	AllowedAfter       [][]runtimeArtifactState `json:"allowed_after"`
	RecoveryGeneration uint64                   `json:"recovery_generation"`
}

type runtimeDevResource struct {
	Before *devProxyBackup          `json:"before,omitempty"`
	Users  []runtimeDevUserResource `json:"users"`
}

type runtimeDevUserResource struct {
	Values       *devProxyBackup `json:"values"`
	AllowedProxy string          `json:"allowed_proxy"`
}

type runtimeTelegramResources struct {
	HermesGeneration   uint64                    `json:"hermes_generation"`
	OpenClawGeneration uint64                    `json:"openclaw_generation"`
	Hermes             []runtimeHermesResource   `json:"hermes"`
	OpenClaw           []runtimeOpenClawResource `json:"openclaw"`
	Legacy             []runtimeLegacyResource   `json:"legacy,omitempty"`
}

type runtimeHermesResource struct {
	SkipRestart    bool                       `json:"skip_restart,omitempty"`
	Target         string                     `json:"target"`
	Identity       *persistedUserIdentity     `json:"identity,omitempty"`
	Before         *telegramProxyJournalEntry `json:"before,omitempty"`
	Present        bool                       `json:"present"`
	Content        []byte                     `json:"content,omitempty"`
	AllowedContent []byte                     `json:"allowed_content,omitempty"`
	AllowAbsent    bool                       `json:"allow_absent"`
}

type runtimeOpenClawResource struct {
	SkipRestartTargets []string                   `json:"skip_restart_targets,omitempty"`
	User               string                     `json:"user"`
	Identity           *persistedUserIdentity     `json:"identity"`
	Targets            []string                   `json:"targets"`
	Before             *openClawProxyJournalEntry `json:"before,omitempty"`
	Value              json.RawMessage            `json:"value,omitempty"`
	Lexeme             string                     `json:"lexeme,omitempty"`
	Present            bool                       `json:"present"`
	ChannelsPresent    bool                       `json:"channels_present"`
	TelegramPresent    bool                       `json:"telegram_present"`
	AllowedProxy       string                     `json:"allowed_proxy,omitempty"`
}

type runtimeLegacyResource struct {
	RecoveryPending bool   `json:"recovery_pending,omitempty"`
	Target          string `json:"target"`
	Present         bool   `json:"present"`
	Content         []byte `json:"content,omitempty"`
}

func cloneRuntimeValue[T any](value T) (T, error) {
	var out T
	raw, err := json.Marshal(value)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

func runtimeRecoveryGeneration(before uint64, writes uint64) (uint64, error) {
	if writes == ^uint64(0) || before > ^uint64(0)-writes-1 {
		return 0, errors.New("运行事务 journal generation 即将耗尽")
	}
	return before + writes, nil
}

func captureRuntimeResources(beforeApp, candidateApp *App, before, candidate *Store, scenes []Scene, plan *telegramPlan) (*runtimeResourceSnapshot, error) {
	snapshot := &runtimeResourceSnapshot{BeforeConfig: beforeApp.cfg, CandidateConfig: candidateApp.cfg, Scenes: slices.Clone(scenes)}
	for _, scene := range scenes {
		var err error
		switch scene {
		case SceneGlobal:
			snapshot.Global, err = captureRuntimeGlobal(beforeApp, candidateApp, candidate.SceneEnabled[SceneGlobal])
		case SceneDev:
			snapshot.Dev, err = captureRuntimeDev(beforeApp, candidateApp, candidate.SceneEnabled[SceneDev])
		case SceneTelegram:
			snapshot.Telegram, err = captureRuntimeTelegram(beforeApp, candidateApp, before, candidate, plan)
		default:
			return nil, errors.New("运行事务资源场景无效")
		}
		if err != nil {
			return nil, fmt.Errorf("规划%s资源失败：%w", sceneName(scene), err)
		}
	}
	return snapshot, validateRuntimeResources(snapshot)
}

func runtimeArtifactFromGlobal(state globalProxyArtifactState) runtimeArtifactState {
	return runtimeArtifactState{Present: state.present, Content: bytes.Clone(state.content), Mode: uint32(state.mode.Perm()), UID: state.uid, GID: state.gid}
}
func (state runtimeArtifactState) globalState() globalProxyArtifactState {
	return globalProxyArtifactState{present: state.Present, content: state.Content, mode: os.FileMode(state.Mode), uid: state.UID, gid: state.GID}
}

func captureRuntimeGlobal(beforeApp, candidateApp *App, enabled bool) (*runtimeGlobalResource, error) {
	result := &runtimeGlobalResource{}
	journal, err := beforeApp.loadGlobalProxyJournal()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if journal != nil {
		if journal.Phase != globalProxyPhaseActive {
			return nil, errors.New("全局代理已有待完成 journal，请先恢复")
		}
		result.Before, err = cloneRuntimeValue(journal)
		if err != nil {
			return nil, err
		}
	}
	var generation uint64
	if journal != nil {
		generation = journal.Generation
	}
	// One fixed Global step writes prepare once, commit at most once, and
	// local restore at most once. Initial pending phases are rejected. Reserve
	// all three possible writes even when a failed write committed only backup.
	result.RecoveryGeneration, err = runtimeRecoveryGeneration(generation, 3)
	if err != nil {
		return nil, err
	}
	desired := globalProxyDesiredArtifacts(candidateApp.cfg)
	mode, uid, gid, err := currentGlobalProxyManagedMetadata()
	if err != nil {
		return nil, err
	}
	for _, path := range globalProxyArtifactPaths() {
		current, err := readGlobalProxyArtifact(path)
		if err != nil {
			return nil, err
		}
		if journal != nil {
			artifact := runtimeGlobalArtifact(journal, path)
			if artifact.Path != path || !globalProxyStateMatchesManaged(current, artifact.ManagedContent, artifact.ManagedMode, artifact.ManagedUID, artifact.ManagedGID) {
				return nil, errors.New("全局代理实际文件与 ownership 不一致")
			}
		}
		state := runtimeArtifactFromGlobal(current)
		result.Files = append(result.Files, state)
		allowed := []runtimeArtifactState{state}
		if enabled {
			allowed = append(allowed, runtimeArtifactState{Present: true, Content: desired[path], Mode: mode, UID: uid, GID: gid})
		}
		if journal != nil {
			allowed = append(allowed, runtimeArtifactFromGlobal(globalProxyOriginalArtifactState(runtimeGlobalArtifact(journal, path))))
		}
		result.AllowedAfter = append(result.AllowedAfter, allowed)
	}
	return result, nil
}

func compensateRuntimeResources(snapshot *runtimeResourceSnapshot, startedScenes []Scene, checkpoint ...func() error) error {
	if err := validateRuntimeResources(snapshot); err != nil {
		return err
	}
	var errs []error
	seen := map[Scene]bool{}
	for i := len(startedScenes) - 1; i >= 0; i-- {
		scene := startedScenes[i]
		if seen[scene] {
			continue
		}
		seen[scene] = true
		if !slices.Contains(snapshot.Scenes, scene) {
			errs = append(errs, errors.New("补偿场景不在持久资源计划中"))
			continue
		}
		var err error
		switch scene {
		case SceneGlobal:
			err = compensateRuntimeGlobal(NewApp(snapshot.BeforeConfig), snapshot.Global)
		case SceneDev:
			err = compensateRuntimeDev(NewApp(snapshot.BeforeConfig), snapshot.Dev)
		case SceneTelegram:
			err = compensateRuntimeTelegram(NewApp(snapshot.BeforeConfig), snapshot.Telegram, checkpoint...)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("补偿%s资源失败：%w", sceneName(scene), err))
		}
	}
	return errors.Join(errs...)
}

// The durable outer receipt, rather than a live restoring journal, retains
// before-images across forward ownership release. No deleted live journal is
// resurrected on successful commit; only the coordinator removes its receipt.
func finalizeRuntimeResources(snapshot *runtimeResourceSnapshot) error {
	return validateRuntimeResources(snapshot)
}

func runtimeGlobalArtifact(journal *globalProxyJournal, path string) *globalProxyJournalArtifact {
	if journal != nil {
		for _, artifact := range journal.Artifacts {
			if artifact.Path == path {
				return artifact
			}
		}
	}
	return nil
}

func compensateRuntimeGlobal(app *App, snapshot *runtimeGlobalResource) error {
	if snapshot == nil {
		return errors.New("缺少 Global 恢复计划")
	}
	currentJournal, err := app.loadGlobalProxyJournal()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	generation := snapshot.RecoveryGeneration
	if currentJournal != nil && currentJournal.Generation > generation {
		generation = currentJournal.Generation
	}
	// Validate the ownership lineage and complete pair before the first mutation.
	expected := make([][]globalProxyArtifactState, len(snapshot.Files))
	current := make([]globalProxyArtifactState, len(snapshot.Files))
	quarantined := make([]bool, len(snapshot.Files))
	for i, path := range globalProxyArtifactPaths() {
		for _, state := range snapshot.AllowedAfter[i] {
			expected[i] = append(expected[i], state.globalState())
		}
		if currentJournal != nil {
			artifact := runtimeGlobalArtifact(currentJournal, path)
			original := snapshot.Files[i].globalState()
			if snapshot.Before != nil {
				original = globalProxyOriginalArtifactState(runtimeGlobalArtifact(snapshot.Before, path))
			}
			if artifact == nil || !globalProxyArtifactStatesEqual(globalProxyOriginalArtifactState(artifact), original) {
				return errors.New("全局代理 ownership 原值与事务 receipt 不符")
			}
			for _, state := range globalProxyRestoreExpectedStates(artifact) {
				if !globalProxyStateMatchesAny(state, expected[i]) {
					return errors.New("全局代理 ownership 值超出事务 receipt")
				}
			}
		}
		var readErr error
		current[i], quarantined[i], readErr = readGlobalProxyArtifactLogical(path, expected[i])
		if readErr != nil {
			return readErr
		}
		if !globalProxyStateMatchesAny(current[i], expected[i]) {
			return fmt.Errorf("全局代理资源已在事务外修改，保留恢复记录：%s", path)
		}
	}
	changed := false
	for i, path := range globalProxyArtifactPaths() {
		if globalProxyArtifactStatesEqual(current[i], snapshot.Files[i].globalState()) && !quarantined[i] {
			continue
		}
		if snapshot.Files[i].Present {
			err = globalProxyWriteArtifact(path, expected[i], snapshot.Files[i].globalState())
		} else {
			_, err = globalProxyRemoveArtifact(path, expected[i])
		}
		if err != nil {
			return err
		}
		changed = true
	}
	if snapshot.Before == nil {
		if currentJournal == nil {
			return nil
		}
		return app.removeGlobalProxyJournal()
	}
	desired, err := cloneRuntimeValue(snapshot.Before)
	if err != nil {
		return err
	}
	if currentJournal != nil {
		same := *currentJournal
		same.Generation = desired.Generation
		if !changed && reflect.DeepEqual(&same, desired) {
			return nil
		}
	}
	desired.Generation = generation
	return app.saveGlobalProxyJournal(desired)
}

func validateRuntimeResources(snapshot *runtimeResourceSnapshot) error {
	if snapshot == nil || len(snapshot.Scenes) > 3 {
		return errors.New("运行资源快照无效")
	}
	if err := runtimeValidateConfig(snapshot.BeforeConfig); err != nil {
		return err
	}
	if err := runtimeValidateConfig(snapshot.CandidateConfig); err != nil {
		return err
	}
	if snapshot.BeforeConfig.CoreDir != snapshot.CandidateConfig.CoreDir || snapshot.BeforeConfig.InstallBin != snapshot.CandidateConfig.InstallBin || snapshot.BeforeConfig.SystemdService != snapshot.CandidateConfig.SystemdService || snapshot.BeforeConfig.RestoreService != snapshot.CandidateConfig.RestoreService {
		return errors.New("运行事务不能更换安装定位配置")
	}
	seen := map[Scene]bool{}
	for _, scene := range snapshot.Scenes {
		if seen[scene] || !knownScene(scene) {
			return errors.New("运行资源场景重复或无效")
		}
		seen[scene] = true
	}
	if (snapshot.Global != nil) != seen[SceneGlobal] || (snapshot.Dev != nil) != seen[SceneDev] || (snapshot.Telegram != nil) != seen[SceneTelegram] {
		return errors.New("运行资源场景与快照不一致")
	}
	if s := snapshot.Global; s != nil {
		if len(s.Files) != 2 || len(s.AllowedAfter) != 2 {
			return errors.New("全局代理恢复快照无效")
		}
		var initial uint64
		if s.Before != nil {
			if err := validateGlobalProxyJournal(s.Before); err != nil {
				return err
			}
			if s.Before.Phase != globalProxyPhaseActive {
				return errors.New("全局代理before phase无效")
			}
			initial = s.Before.Generation
		}
		floor, err := runtimeRecoveryGeneration(initial, 3)
		if err != nil || s.RecoveryGeneration != floor {
			return errors.New("全局代理恢复generation无效")
		}
		desired := globalProxyDesiredArtifacts(snapshot.CandidateConfig)
		mode, uid, gid, err := currentGlobalProxyManagedMetadata()
		if err != nil {
			return err
		}
		for i, path := range globalProxyArtifactPaths() {
			state := s.Files[i]
			if err := validateRuntimeArtifact(state); err != nil {
				return err
			}
			if len(s.AllowedAfter[i]) < 1 || len(s.AllowedAfter[i]) > 3 {
				return errors.New("全局代理允许状态数量无效")
			}
			expected := []globalProxyArtifactState{state.globalState(), {present: true, content: desired[path], mode: os.FileMode(mode), uid: uid, gid: gid}}
			if s.Before != nil {
				artifact := runtimeGlobalArtifact(s.Before, path)
				if !globalProxyStateMatchesManaged(state.globalState(), artifact.ManagedContent, artifact.ManagedMode, artifact.ManagedUID, artifact.ManagedGID) {
					return errors.New("全局代理before文件与ownership不符")
				}
				expected = append(expected, globalProxyOriginalArtifactState(artifact))
			}
			if !globalProxyArtifactStatesEqual(s.AllowedAfter[i][0].globalState(), state.globalState()) {
				return errors.New("全局代理允许状态缺少before")
			}
			for _, allowed := range s.AllowedAfter[i] {
				if err := validateRuntimeArtifact(allowed); err != nil {
					return err
				}
				if !globalProxyStateMatchesAny(allowed.globalState(), expected) {
					return errors.New("全局代理恢复快照含非派生候选值")
				}
			}
		}
	}
	if snapshot.Dev != nil {
		for _, item := range snapshot.Dev.Users {
			if item.AllowedProxy != snapshot.CandidateConfig.HTTPAddr(SceneDev) {
				return errors.New("开发代理候选代理值与配置不符")
			}
		}
	}
	if snapshot.Telegram != nil {
		desired := []byte("[Service]\n" + telegramProxySystemdEnvironmentLines(snapshot.CandidateConfig))
		for _, item := range snapshot.Telegram.Hermes {
			if len(item.AllowedContent) > 0 && !bytes.Equal(item.AllowedContent, desired) {
				return errors.New("网关Hermes候选代理内容与配置不符")
			}
		}
		for _, item := range snapshot.Telegram.OpenClaw {
			if item.AllowedProxy != "" && item.AllowedProxy != snapshot.CandidateConfig.HTTPAddr(SceneTelegram) {
				return errors.New("用户OpenClaw候选代理值与配置不符")
			}
		}
	}

	if err := validateRuntimeDev(snapshot.Dev); err != nil {
		return err
	}
	return validateRuntimeTelegram(snapshot.Telegram)
}

func validateRuntimeArtifact(state runtimeArtifactState) error {
	if len(state.Content) > maxGlobalProxyArtifactBytes || state.Mode > 0777 || state.UID > maxGlobalProxyOwnershipID || state.GID > maxGlobalProxyOwnershipID {
		return errors.New("运行资源文件元数据或大小无效")
	}
	if !state.Present && (len(state.Content) != 0 || state.Mode != 0 || state.UID != 0 || state.GID != 0) {
		return errors.New("缺失运行资源文件携带内容或元数据")
	}
	return nil
}

func captureRuntimeDev(beforeApp, candidateApp *App, enabled bool) (*runtimeDevResource, error) {
	result := &runtimeDevResource{}
	before, err := beforeApp.loadDevBackup()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if before != nil {
		if before.ApplyRollback != nil || devRestoreStarted(before) {
			return nil, errors.New("开发代理已有待完成恢复计划")
		}
		result.Before, err = cloneRuntimeValue(before)
		if err != nil {
			return nil, err
		}
	}
	var users []string
	if before != nil {
		users = append(users, before.User)
	}
	candidateUser := ""
	if enabled {
		candidateUser, err = candidateApp.devTargetUser()
		if err != nil {
			return nil, err
		}
		users = appendUniqueString(users, candidateUser)
	}
	for _, user := range users {
		var ownership *devProxyBackup
		if before != nil && before.User == user {
			ownership, err = cloneRuntimeValue(before)
		} else {
			identity, idErr := capturePersistedUserIdentity(user, devLookupUserIdentity)
			if idErr != nil {
				return nil, idErr
			}
			ownership = &devProxyBackup{Version: devProxyBackupVersion, User: user, Identity: identity, ToolsRecorded: true}
		}
		if err != nil {
			return nil, err
		}
		git, npm := ownership.GitManaged, ownership.NPMManaged
		if user == candidateUser {
			git = git || devCommandExists("git")
			npm = npm || devCommandExists("npm")
		}
		if git && !ownership.GitManaged {
			ownership.GitConfigLocation, err = devResolveGitTopology(user, ownership.Identity)
			if err != nil {
				return nil, err
			}
			ownership.GitManaged = true
		}
		values, err := snapshotDevProxyConfig(user, ownership, git, npm)
		if err != nil {
			return nil, err
		}
		result.Users = append(result.Users, runtimeDevUserResource{Values: values, AllowedProxy: candidateApp.cfg.HTTPAddr(SceneDev)})
	}
	return result, validateRuntimeDev(result)
}

func validateRuntimeDev(snapshot *runtimeDevResource) error {
	if snapshot == nil {
		return nil
	}
	if len(snapshot.Users) > 2 {
		return errors.New("开发代理资源用户超出固定范围")
	}
	if snapshot.Before != nil {
		if err := validateDevProxyBackup(snapshot.Before); err != nil {
			return err
		}
		if snapshot.Before.ApplyRollback != nil || devRestoreStarted(snapshot.Before) {
			return errors.New("开发代理 before-image含待完成计划")
		}
	}
	seen := map[string]bool{}
	for _, item := range snapshot.Users {
		if err := validateDevProxyBackup(item.Values); err != nil {
			return err
		}
		if seen[item.Values.User] || item.Values.ApplyRollback != nil || devRestoreStarted(item.Values) || item.AllowedProxy == "" {
			return errors.New("开发代理 before-image重复或含待完成计划")
		}
		seen[item.Values.User] = true
		if err := validateDevProxyValue("candidate proxy", &item.AllowedProxy); err != nil {
			return err
		}
		if snapshot.Before != nil && snapshot.Before.User == item.Values.User && !samePersistedUserIdentity(snapshot.Before.Identity, item.Values.Identity) {
			return errors.New("开发代理 before ownership身份不匹配")
		}
	}
	if snapshot.Before != nil && !seen[snapshot.Before.User] {
		return errors.New("开发代理 before ownership不在固定用户范围")
	}
	return nil
}

func runtimeDevOriginalFor(snapshot *runtimeDevResource, item runtimeDevUserResource) *devProxyBackup {
	if snapshot.Before != nil && snapshot.Before.User == item.Values.User {
		return snapshot.Before
	}
	return item.Values
}

func runtimeDevGitAllowed(current, before, original []string, proxy string) bool {
	return slices.Equal(current, before) || slices.Equal(current, original) || slices.Equal(current, []string{proxy})
}
func runtimeDevNPMAllowed(current, before, original *string, proxy string) bool {
	return optionalStringsEqual(current, before) || optionalStringsEqual(current, original) || optionalStringsEqual(current, &proxy)
}

// Ownership identity and initial originals are immutable across a transaction.
// Progress plans belong to the current journal and are never copied backwards.
func runtimeDevOwnershipMatches(current *devProxyBackup, item runtimeDevUserResource, original *devProxyBackup) bool {
	if current.User != item.Values.User || !samePersistedUserIdentity(current.Identity, item.Values.Identity) || current.GitManaged && !item.Values.GitManaged || current.NPMManaged && !item.Values.NPMManaged || current.GitManaged && current.GitConfigLocation != item.Values.GitConfigLocation {
		return false
	}
	allowedManaged := append(managedDevProxyValues(original, true, ""), managedDevProxyValues(original, false, "")...)
	allowedManaged = appendUniqueString(allowedManaged, item.AllowedProxy)
	for _, proxy := range append(managedDevProxyValues(current, true, ""), managedDevProxyValues(current, false, "")...) {
		if !containsString(allowedManaged, proxy) {
			return false
		}
	}
	gitHTTP, gitHTTPS := original.GitHTTPProxy, original.GitHTTPSProxy
	npmHTTP, npmHTTPS := original.NPMProxy, original.NPMHTTPSProxy
	if !original.GitManaged {
		gitHTTP, gitHTTPS = item.Values.GitHTTPProxy, item.Values.GitHTTPSProxy
	}
	if !original.NPMManaged {
		npmHTTP, npmHTTPS = item.Values.NPMProxy, item.Values.NPMHTTPSProxy
	}
	if !current.GitManaged {
		gitHTTP = nil
		gitHTTPS = nil
	}
	if !current.NPMManaged {
		npmHTTP = nil
		npmHTTPS = nil
	}
	return slices.Equal(current.GitHTTPProxy, gitHTTP) && slices.Equal(current.GitHTTPSProxy, gitHTTPS) && optionalStringsEqual(current.NPMProxy, npmHTTP) && optionalStringsEqual(current.NPMHTTPSProxy, npmHTTPS)
}

func runtimeDevRollbackMatches(rollback *devApplyRollback, values *devProxyBackup) bool {
	return rollback != nil && rollback.GitManaged == values.GitManaged && rollback.NPMManaged == values.NPMManaged && slices.Equal(rollback.GitHTTPProxy, values.GitHTTPProxy) && slices.Equal(rollback.GitHTTPSProxy, values.GitHTTPSProxy) && optionalStringsEqual(rollback.NPMProxy, values.NPMProxy) && optionalStringsEqual(rollback.NPMHTTPSProxy, values.NPMHTTPSProxy)
}

func runtimeDevNPMRestoreDesired(before, original *string, managed []string) *string {
	if before != nil && containsManagedNPMProxy(managed, *before) {
		return original
	}
	return before
}

func runtimeDevGitPlan(before, desired []string) *devGitRestorePlan {
	result := &devGitRestorePlan{Phase: devRestorePhasePrepared, Before: slices.Clone(before), Desired: slices.Clone(desired)}
	if slices.Equal(before, desired) {
		result.Phase = devRestorePhaseDone
		result.Next = len(desired) + 1
	}
	return result
}
func runtimeDevNPMPlan(before, desired *string) *devNPMRestorePlan {
	result := &devNPMRestorePlan{Phase: devRestorePhasePrepared, Before: cloneStringPointer(before), Desired: cloneStringPointer(desired)}
	if optionalStringsEqual(before, desired) {
		result.Phase = devRestorePhaseDone
	}
	return result
}

func compensateRuntimeDev(app *App, snapshot *runtimeDevResource) error {
	if snapshot == nil {
		return errors.New("缺少 Dev 恢复计划")
	}
	for i := len(snapshot.Users) - 1; i >= 0; i-- {
		item := snapshot.Users[i]
		values := item.Values
		original := runtimeDevOriginalFor(snapshot, item)
		if err := verifyDevUserIdentity(values.User, values.Identity); err != nil {
			return err
		}
		live, err := app.loadDevBackup()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// A different fixed user's ownership is only displaced after its own before
		// image has been restored. Reverse order undoes a candidate-user takeover.
		otherLive := live != nil && live.User != values.User
		if otherLive {
			found := false
			for j, other := range snapshot.Users {
				if other.Values.User == live.User {
					if j > i {
						return errors.New("开发代理后续用户ownership尚未释放")
					}
					found = true
				}
			}
			if !found {
				return errors.New("开发代理当前ownership不属于事务固定用户")
			}
			live = nil
		}
		if live != nil {
			if !runtimeDevOwnershipMatches(live, item, original) {
				return errors.New("开发代理当前ownership与事务原值或身份不符")
			}
			if live.ApplyRollback != nil {
				if !runtimeDevRollbackMatches(live.ApplyRollback, values) {
					return errors.New("开发代理当前apply rollback不是本事务before-image")
				}
				if err := runtimeValidateDevInverse(live.ApplyRollback, values, original, item.AllowedProxy); err != nil {
					return err
				}
				if err := app.resumeDevApplyRollback(live); err != nil {
					return err
				}
			}
			if devRestoreStarted(live) {
				// The forward release already persisted these exact per-key CAS plans.
				// Complete them without resetting Phase/Next; then create a new inverse.
				for _, pair := range []struct {
					plan             *devGitRestorePlan
					before, original []string
				}{{live.GitHTTPRestore, values.GitHTTPProxy, original.GitHTTPProxy}, {live.GitHTTPSRestore, values.GitHTTPSProxy, original.GitHTTPSProxy}} {
					if pair.plan != nil && (!runtimeDevGitAllowed(pair.plan.Before, pair.before, pair.original, item.AllowedProxy) || !slices.Equal(pair.plan.Desired, desiredGitProxyValues(pair.original, pair.plan.Before, append(managedDevProxyValues(live, true, item.AllowedProxy), managedDevProxyValues(live, false, item.AllowedProxy)...)))) {
						return errors.New("开发代理最终恢复计划超出事务范围")
					}
				}
				for _, pair := range []struct {
					plan             *devNPMRestorePlan
					before, original *string
				}{{live.NPMProxyRestore, values.NPMProxy, original.NPMProxy}, {live.NPMHTTPSRestore, values.NPMHTTPSProxy, original.NPMHTTPSProxy}} {
					if pair.plan != nil && (!runtimeDevNPMAllowed(pair.plan.Before, pair.before, pair.original, item.AllowedProxy) || !optionalStringsEqual(pair.plan.Desired, runtimeDevNPMRestoreDesired(pair.plan.Before, pair.original, append(managedDevProxyValues(live, true, item.AllowedProxy), managedDevProxyValues(live, false, item.AllowedProxy)...)))) {
						return errors.New("开发代理最终恢复计划超出事务范围")
					}
				}
				managedHTTP := managedDevProxyValues(live, true, item.AllowedProxy)
				managedHTTPS := managedDevProxyValues(live, false, item.AllowedProxy)
				if live.GitManaged {
					if err := app.restoreDevGitProxy(live, "http.proxy", live.GitHTTPProxy, managedHTTP, &live.GitHTTPRestore); err != nil {
						return err
					}
					if err := app.restoreDevGitProxy(live, "https.proxy", live.GitHTTPSProxy, managedHTTPS, &live.GitHTTPSRestore); err != nil {
						return err
					}
				}
				if live.NPMManaged {
					if err := app.restoreDevNPMProxy(live, "proxy", live.NPMProxy, managedHTTP, &live.NPMProxyRestore); err != nil {
						return err
					}
					if err := app.restoreDevNPMProxy(live, "https-proxy", live.NPMHTTPSProxy, managedHTTPS, &live.NPMHTTPSRestore); err != nil {
						return err
					}
				}
				// These plans are done, rather than rewound; the outer receipt now owns
				// the separate inverse operation if the process stops before its save.
				live.GitHTTPRestore = nil
				live.GitHTTPSRestore = nil
				live.NPMProxyRestore = nil
				live.NPMHTTPSRestore = nil
			}
		}
		current, err := snapshotDevProxyConfig(values.User, values, values.GitManaged, values.NPMManaged)
		if err != nil {
			return err
		}
		if !runtimeDevGitAllowed(current.GitHTTPProxy, values.GitHTTPProxy, original.GitHTTPProxy, item.AllowedProxy) || !runtimeDevGitAllowed(current.GitHTTPSProxy, values.GitHTTPSProxy, original.GitHTTPSProxy, item.AllowedProxy) || !runtimeDevNPMAllowed(current.NPMProxy, values.NPMProxy, original.NPMProxy, item.AllowedProxy) || !runtimeDevNPMAllowed(current.NPMHTTPSProxy, values.NPMHTTPSProxy, original.NPMHTTPSProxy, item.AllowedProxy) {
			return errors.New("开发代理实际代理值已在事务外修改，保留恢复记录")
		}
		changed := !slices.Equal(current.GitHTTPProxy, values.GitHTTPProxy) || !slices.Equal(current.GitHTTPSProxy, values.GitHTTPSProxy) || !optionalStringsEqual(current.NPMProxy, values.NPMProxy) || !optionalStringsEqual(current.NPMHTTPSProxy, values.NPMHTTPSProxy)
		if changed {
			if otherLive {
				return errors.New("开发代理候选用户已有变化但旧用户ownership尚未释放")
			}
			if live == nil {
				live, err = cloneRuntimeValue(original)
				if err != nil {
					return err
				}
				live.GitManaged = values.GitManaged
				live.NPMManaged = values.NPMManaged
				live.GitConfigLocation = values.GitConfigLocation
			}
			mergeManagedDevProxyValues(live, item.AllowedProxy)
			rollback := &devApplyRollback{GitManaged: values.GitManaged, NPMManaged: values.NPMManaged, ManagedProxy: item.AllowedProxy, GitHTTPProxy: slices.Clone(values.GitHTTPProxy), GitHTTPSProxy: slices.Clone(values.GitHTTPSProxy), NPMProxy: cloneStringPointer(values.NPMProxy), NPMHTTPSProxy: cloneStringPointer(values.NPMHTTPSProxy)}
			if values.GitManaged {
				rollback.GitHTTPRestore = runtimeDevGitPlan(current.GitHTTPProxy, values.GitHTTPProxy)
				rollback.GitHTTPSRestore = runtimeDevGitPlan(current.GitHTTPSProxy, values.GitHTTPSProxy)
			}
			if values.NPMManaged {
				rollback.NPMProxyRestore = runtimeDevNPMPlan(current.NPMProxy, values.NPMProxy)
				rollback.NPMHTTPSRestore = runtimeDevNPMPlan(current.NPMHTTPSProxy, values.NPMHTTPSProxy)
			}
			live.ApplyRollback = rollback
			if err := app.writeDevBackup(live); err != nil {
				return err
			}
			if err := app.resumeDevApplyRollback(live); err != nil {
				return err
			}
		}
		if snapshot.Before != nil && snapshot.Before.User == values.User {
			// Restoring semantic ownership is safe only after all current plans finish.
			desired, err := cloneRuntimeValue(snapshot.Before)
			if err != nil {
				return err
			}
			if live == nil || !reflect.DeepEqual(live, desired) {
				if err := app.writeDevBackup(desired); err != nil {
					return err
				}
			}
		} else if live != nil {
			if err := devRemoveBackup(app.cfg.DevBackupPath()); err != nil {
				return err
			}
		}
	}
	return nil
}

func runtimeValidateDevInverse(rollback *devApplyRollback, values, original *devProxyBackup, proxy string) error {
	for _, pair := range []struct {
		plan             *devGitRestorePlan
		before, original []string
	}{{rollback.GitHTTPRestore, values.GitHTTPProxy, original.GitHTTPProxy}, {rollback.GitHTTPSRestore, values.GitHTTPSProxy, original.GitHTTPSProxy}} {
		if pair.plan != nil && (!runtimeDevGitAllowed(pair.plan.Before, pair.before, pair.original, proxy) || !slices.Equal(pair.plan.Desired, pair.before)) {
			return errors.New("开发代理反向Git恢复计划超出事务范围")
		}
	}
	for _, pair := range []struct {
		plan             *devNPMRestorePlan
		before, original *string
	}{{rollback.NPMProxyRestore, values.NPMProxy, original.NPMProxy}, {rollback.NPMHTTPSRestore, values.NPMHTTPSProxy, original.NPMHTTPSProxy}} {
		if pair.plan != nil && (!runtimeDevNPMAllowed(pair.plan.Before, pair.before, pair.original, proxy) || !optionalStringsEqual(pair.plan.Desired, pair.before)) {
			return errors.New("开发代理反向npm恢复计划超出事务范围")
		}
	}
	return nil
}
