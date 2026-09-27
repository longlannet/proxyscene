package manager

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

var runtimeWriteStoreCAS = writeRuntimeStoreCAS

func artifactStoreEvidence(state globalProxyArtifactState) runtimeStoreEvidence {
	if !state.present {
		return runtimeStoreEvidence{}
	}
	return runtimeStoreEvidence{true, contentDigest(state.content)}
}

func (a *App) validateRuntimeStoreUnchanged(r *runtimeTransitionRecord) error {
	main, err := storeEvidence(a.cfg.StorePath())
	if err != nil {
		return err
	}
	backup, err := storeEvidence(a.cfg.StoreBackupPath())
	if err != nil {
		return err
	}
	if main != r.BeforeMain || backup != r.BeforeBackup {
		return fmt.Errorf("Store 在事务外变化，拒绝覆盖")
	}
	return nil
}

// A committed main is authoritative, but finalization also requires its fixed
// backup. A divergent backup is retained with the receipt for explicit repair.
func (a *App) validateRuntimeStoreCommitted(r *runtimeTransitionRecord) error {
	expected := runtimeStoreEvidence{true, r.CandidateDigest}
	for _, path := range []string{a.cfg.StorePath(), a.cfg.StoreBackupPath()} {
		observed, err := storeEvidence(path)
		if err != nil {
			return err
		}
		if observed != expected {
			return fmt.Errorf("已提交 Store 的主文件或备份偏离固定候选，保留恢复记录：%s", path)
		}
	}
	return nil
}

// The expected digest is fixed before planning. The underlying CAS claims the
// current inode, checks its full bytes/metadata, and never replaces a file
// created concurrently after that claim.
func writeRuntimeStoreCAS(path string, expected runtimeStoreEvidence, data []byte) error {
	state, err := readGlobalProxyArtifactStateNoFollow(path, maxStoreBytes)
	if err != nil {
		return err
	}
	if artifactStoreEvidence(state) != expected {
		return fmt.Errorf("Store CAS 前置条件已变化：%s", path)
	}
	desired := globalProxyArtifactState{present: true, content: data, mode: 0600, uid: uint32(os.Geteuid()), gid: uint32(os.Getegid())}
	return writeBoundedArtifactCAS(path, []globalProxyArtifactState{state}, desired, maxStoreBytes)
}

func (a *App) savePlannedRuntimeStore(st *Store, r *runtimeTransitionRecord) error {
	candidate := cloneStore(st)
	if candidate.Generation == ^uint64(0) {
		return fmt.Errorf("Store generation 已耗尽")
	}
	candidate.Generation++
	data, err := encodedRuntimeStore(candidate)
	if err != nil {
		return err
	}
	if contentDigest(data) != r.CandidateDigest {
		return fmt.Errorf("Store 待提交内容偏离固定候选")
	}
	if err := a.validateRuntimeStoreUnchanged(r); err != nil {
		return err
	}
	if err := runtimeWriteStoreCAS(a.cfg.StoreBackupPath(), r.BeforeBackup, data); err != nil {
		return err
	}
	// Main is the commit point. A change to either file stops the commit.
	backup, err := storeEvidence(a.cfg.StoreBackupPath())
	if err != nil {
		return err
	}
	if backup != (runtimeStoreEvidence{true, r.CandidateDigest}) {
		return fmt.Errorf("Store 候选备份在提交前变化")
	}
	if err := runtimeWriteStoreCAS(a.cfg.StorePath(), r.BeforeMain, data); err != nil {
		return err
	}
	restoreStore(st, candidate)
	return nil
}

// A crash can leave the previous inode quarantined by CAS. Only identities
// named in the durable receipt may be put back or retired; an external file
// always stops recovery. This does not apply scenes or run service commands.
func (a *App) settleRuntimeStoreQuarantines(r *runtimeTransitionRecord) error {
	candidate := runtimeStoreEvidence{true, r.CandidateDigest}
	var compensation runtimeStoreEvidence
	if r.Compensation != nil {
		data, err := encodedRuntimeStore(r.Compensation)
		if err != nil {
			return err
		}
		compensation = runtimeStoreEvidence{true, contentDigest(data)}
	}
	for _, item := range []struct {
		path   string
		before runtimeStoreEvidence
	}{{a.cfg.StorePath(), r.BeforeMain}, {a.cfg.StoreBackupPath(), r.BeforeBackup}} {
		allowed := []runtimeStoreEvidence{item.before, candidate}
		if compensation.Present {
			allowed = append(allowed, compensation)
		}
		if err := settleRuntimeStoreQuarantine(item.path, allowed); err != nil {
			return err
		}
	}
	return nil
}

func settleRuntimeStoreQuarantine(path string, allowed []runtimeStoreEvidence) error {
	clean, dirFD, err := openGlobalProxyArtifactDir(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer syscall.Close(dirFD)
	base := filepath.Base(clean)
	quarantine := globalProxyQuarantineName(base)
	quarantinePath := filepath.Join(filepath.Dir(clean), quarantine)
	claimed, err := readGlobalProxyArtifactAt(dirFD, quarantine, quarantinePath, maxStoreBytes)
	if err != nil {
		return err
	}
	if !claimed.present {
		return nil
	}
	if !slices.Contains(allowed, artifactStoreEvidence(claimed)) {
		return fmt.Errorf("Store 隔离文件不是本事务允许内容，已保留：%s", quarantinePath)
	}
	final, err := readGlobalProxyArtifactAt(dirFD, base, clean, maxStoreBytes)
	if err != nil {
		return err
	}
	if !final.present {
		return restoreGlobalProxyQuarantine(dirFD, base, quarantine, quarantinePath)
	}
	if !slices.Contains(allowed, artifactStoreEvidence(final)) {
		return fmt.Errorf("Store 存在外部修改，隔离文件已保留：%s", quarantinePath)
	}
	return removeGlobalProxyQuarantine(dirFD, quarantine, quarantinePath)
}

func (a *App) planMetadataMutation(before, candidate *Store) (*runtimeMutationPlan, error) {
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
	highest, err := a.highestStoreGeneration()
	if err != nil {
		return nil, err
	}
	if highest < before.Generation {
		highest = before.Generation
	}
	if highest >= ^uint64(0)-1 {
		return nil, fmt.Errorf("Store generation 无法为补偿保留新代号")
	}
	candidate.RuntimeConfig = before.RuntimeConfig
	candidate.Generation = highest + 1
	data, err := encodedRuntimeStore(candidate)
	if err != nil {
		return nil, err
	}
	r := &runtimeTransitionRecord{Version: runtimeTransitionVersion, Phase: "applying", Installation: a.expectedInstallationOwnership(), Before: cloneStore(before), Candidate: cloneStore(candidate), BeforeMain: main, BeforeBackup: backup, CandidateDigest: contentDigest(data)}
	if err := a.validateRuntimeTransition(r); err != nil {
		return nil, err
	}
	return &runtimeMutationPlan{record: r, beforeApp: NewApp(a.cfg), candidateApp: NewApp(a.cfg), execution: cloneStore(candidate)}, nil
}

func finalizeRuntimeTransitionResources(r *runtimeTransitionRecord) error {
	if r.Resources == nil {
		return nil
	}
	return finalizeRuntimeResources(r.Resources)
}

// Bind the in-memory before-image to the main commit point (or the explicitly
// recoverable backup when main is absent/corrupt). Capturing only disk digests
// would otherwise accept an administrator edit made just before planning.
func (a *App) validateRuntimeStoreBefore(before *Store, main, backup runtimeStoreEvidence) error {
	observed := newStore()
	decoded := false
	for _, item := range []struct {
		path     string
		evidence runtimeStoreEvidence
	}{{a.cfg.StorePath(), main}, {a.cfg.StoreBackupPath(), backup}} {
		if !item.evidence.Present {
			continue
		}
		raw, err := readRegularFileNoFollow(item.path, maxStoreBytes)
		if err != nil {
			return err
		}
		if contentDigest(raw) != item.evidence.Digest {
			return fmt.Errorf("Store 在读取前态时变化")
		}
		parsed, _, err := decodeStore(raw)
		if err != nil {
			continue
		}
		observed = parsed
		decoded = true
		break
	}
	if (main.Present || backup.Present) && !decoded {
		return fmt.Errorf("Store 前态的主文件与备份均无法解码")
	}
	actual, err := encodedRuntimeStore(observed)
	if err != nil {
		return err
	}
	expected, err := encodedRuntimeStore(before)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("Store 已不同于本次读取的前态，拒绝覆盖")
	}
	return nil
}
