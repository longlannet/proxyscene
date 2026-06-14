package manager

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"
)

func (a *App) withStoreLock(fn func() error) error {
	if err := ensureDir(a.cfg.CoreDir, 0o700); err != nil {
		return err
	}
	return withFileLock(a.cfg.StoreLockPath(), fn)
}

func (a *App) loadStore() (*Store, error) {
	if err := ensureDir(a.cfg.CoreDir, 0o700); err != nil {
		return nil, err
	}
	path := a.cfg.StorePath()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newStore(), nil
	}
	if err != nil {
		return nil, err
	}
	st := newStore()
	if len(b) > 0 {
		if err := json.Unmarshal(b, st); err != nil {
			return nil, err
		}
	}
	normalizeStore(st)
	return st, nil
}

func normalizeStore(st *Store) {
	if st.SceneNodes == nil {
		st.SceneNodes = map[Scene]string{}
	}
	if st.SceneEnabled == nil {
		st.SceneEnabled = map[Scene]bool{}
	}
	if st.SpeedResults == nil {
		st.SpeedResults = map[string]SpeedResult{}
	}
}

func (a *App) saveStore(st *Store) error {
	normalizeStore(st)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(a.cfg.StorePath(), append(b, '\n'), 0o600)
}

func (st *Store) findNode(id string) *Node {
	for i := range st.Nodes {
		if st.Nodes[i].ID == id {
			return &st.Nodes[i]
		}
	}
	return nil
}

func (st *Store) findNodeByURL(raw string) *Node {
	for i := range st.Nodes {
		if st.Nodes[i].RawURL == raw {
			return &st.Nodes[i]
		}
	}
	return nil
}

func (st *Store) firstNodeID() string {
	if len(st.Nodes) == 0 {
		return ""
	}
	return st.Nodes[0].ID
}

func (st *Store) selectedNodeID(scene Scene) string {
	if id := st.SceneNodes[scene]; id != "" && st.findNode(id) != nil {
		return id
	}
	if st.DefaultNodeID != "" && st.findNode(st.DefaultNodeID) != nil {
		return st.DefaultNodeID
	}
	return st.firstNodeID()
}

func newNodeID() string {
	return "node-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}
