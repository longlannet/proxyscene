package manager

import (
	"bufio"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func nodeMenuFixture(t *testing.T, empty bool) (*App, *Store, *int) {
	t.Helper()
	a, st := subscriptionUpdateFixture(t)
	st.SceneEnabled = map[Scene]bool{}
	if empty {
		st.Nodes = nil
		st.DefaultNodeID = ""
	} else {
		st.Nodes = append(st.Nodes, Node{ID: "backup", Name: "备用节点", Protocol: "trojan", RawURL: "trojan://secret@backup.example:443"})
	}
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	st, err := a.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	// These UI tests verify when a runtime plan is requested. Core effects and
	// compensation are exercised with real temporary files in runtime_core_test.
	calls := 0
	oldPlan := runtimePlanCore
	t.Cleanup(func() { runtimePlanCore = oldPlan })
	runtimePlanCore = func(*App, *Store) (*runtimeCorePlan, error) { calls++; return nil, nil }
	return a, st, &calls
}

func nodeMenuInput(t *testing.T, input string) {
	t.Helper()
	old := stdinReader
	t.Cleanup(func() { stdinReader = old })
	stdinReader = bufio.NewReader(strings.NewReader(input))
}

func TestMenuNodeSelectorBindsDisplayedIDAndRejectsAmbiguity(t *testing.T) {
	st := newStore()
	st.Nodes = []Node{{ID: "abcdef001"}, {ID: "abcdef002"}, {ID: "fedcba"}}
	for selector, want := range map[string]string{"2": "abcdef002", "fed": "fedcba", "abcdef001": "abcdef001"} {
		got, err := menuResolveNode(st, selector)
		if err != nil || got != want {
			t.Fatalf("selector %q: got=%q err=%v", selector, got, err)
		}
	}
	for _, selector := range []string{"", "99", "abcdef", "missing"} {
		if _, err := menuResolveNode(st, selector); err == nil {
			t.Fatalf("invalid/ambiguous selector accepted: %q", selector)
		}
	}
	for _, n := range st.Nodes {
		if got, err := menuResolveNode(st, menuShortNodeID(st, n.ID)); err != nil || got != n.ID {
			t.Fatalf("displayed short ID does not uniquely resolve %q: %q %v", n.ID, got, err)
		}
	}
}

func TestMenuAddNodeCancellationNeverCommits(t *testing.T) {
	raw := "trojan://secret@new.example:443"
	for name, input := range map[string]string{
		"URL EOF": "", "URL unterminated": raw, "URL cancel": "q\n",
		"name EOF": raw + "\n", "name cancel": raw + "\nq\n",
		"default EOF": raw + "\nnew\n", "default cancel": raw + "\nnew\nq\n",
		"default unterminated yes": raw + "\nnew\ny",
	} {
		t.Run(name, func(t *testing.T) {
			a, before, calls := nodeMenuFixture(t, false)
			nodeMenuInput(t, input)
			err := a.menuAddNode(before)
			if !errors.Is(err, errMenuClosed) && !errors.Is(err, errMenuCancelled) {
				t.Fatalf("expected cancellation or closed input: %v", err)
			}
			assertSubscriptionStoreUnchanged(t, a, before)
			if *calls != 0 {
				t.Fatalf("cancelled addition invoked runtime %d times", *calls)
			}
		})
	}
}

func TestMenuAddNodeDefaultPolicy(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("menu commit requires root")
	}
	for _, tc := range []struct {
		name, answer                       string
		empty, makeDefault, missingDefault bool
	}{
		{"keep selected by default", "\n", false, false, false},
		{"keep implicit first-node fallback", "\n", false, false, true},
		{"explicit default", "y\n", false, true, false},
		{"explicit default replaces fallback", "y\n", false, true, true},
		{"first node", "y\n", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, before, calls := nodeMenuFixture(t, tc.empty)
			if tc.missingDefault {
				before.DefaultNodeID = ""
				if err := a.saveStore(before); err != nil {
					t.Fatal(err)
				}
			}
			nodeMenuInput(t, "trojan://secret@new.example:443\nnew\n"+tc.answer)
			output, err := captureMainMenuTestOutput(t, func() error { return a.menuAddNode(before) })
			if err != nil {
				t.Fatal(err)
			}
			got, err := a.loadStore()
			if err != nil || len(got.Nodes) != len(before.Nodes)+1 || got.Generation != before.Generation+1 {
				t.Fatalf("addition did not commit once: %+v %v", got, err)
			}
			newID := got.Nodes[len(got.Nodes)-1].ID
			if !strings.Contains(output, "保存节点：new ["+newID+"]") {
				t.Fatalf("saved node identity differs from confirmed preview: id=%s output=%s", newID, output)
			}
			if tc.makeDefault && got.DefaultNodeID != newID || !tc.makeDefault && got.DefaultNodeID != before.DefaultNodeID {
				t.Fatalf("unexpected default: %+v", got)
			}
			if !tc.makeDefault {
				if *calls != 0 {
					t.Fatalf("saving a backup node touched runtime %d times", *calls)
				}
				for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
					if got.selectedNodeID(scene) != before.selectedNodeID(scene) {
						t.Fatalf("saving a backup node changed %s selection", scene)
					}
				}
			}
			if tc.makeDefault && *calls == 0 {
				t.Fatal("changing the default skipped runtime planning")
			}
		})
	}
}

func TestMenuNodeMutationsCancelWithoutSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		run         func(*App, *Store) error
	}{
		{"selection cancel", "2\n1\n\n", (*App).menuUseNode},
		{"selection EOF", "2\n1\ny", (*App).menuUseNode},
		{"delete defaults no", "1\n\n", (*App).menuRemoveNode},
		{"delete EOF", "1\ny", (*App).menuRemoveNode},
		{"delete q", "1\nq\n", (*App).menuRemoveNode},
		{"delete all defaults no", "\n", (*App).menuRemoveAllNodes},
		{"delete all no", "n\n", (*App).menuRemoveAllNodes},
		{"delete all EOF", "", (*App).menuRemoveAllNodes},
		{"delete all unterminated yes", "y", (*App).menuRemoveAllNodes},
		{"delete all q", "q\n", (*App).menuRemoveAllNodes},
		{"rename EOF", "1\n", (*App).menuRenameNode},
		{"rename q", "1\nq\n", (*App).menuRenameNode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, before, calls := nodeMenuFixture(t, false)
			nodeMenuInput(t, tc.input)
			if err := tc.run(a, before); !errors.Is(err, errMenuClosed) && !errors.Is(err, errMenuCancelled) {
				t.Fatalf("expected cancellation/closed input: %v", err)
			}
			assertSubscriptionStoreUnchanged(t, a, before)
			if *calls != 0 {
				t.Fatalf("cancelled operation invoked runtime %d times", *calls)
			}
		})
	}
}

type nodeMenuBeforeRead struct {
	run    func()
	reader io.Reader
}

func (r *nodeMenuBeforeRead) Read(p []byte) (int, error) {
	if r.run != nil {
		run := r.run
		r.run = nil
		run()
	}
	return r.reader.Read(p)
}

func TestMenuNodeConfirmationRejectsConcurrentStateChange(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("menu commit requires root")
	}
	for _, tc := range []struct {
		name, prefix, suffix string
		run                  func(*App, *Store) error
	}{
		{"use", "2\n1\n", "y\n", (*App).menuUseNode},
		{"delete", "1\n", "y\n", (*App).menuRemoveNode},
		{"delete all", "", "y\n", (*App).menuRemoveAllNodes},
		{"rename", "1\n", "changed\n", (*App).menuRenameNode},
		{"add", "trojan://secret@new.example:443\nnew\n", "y\n", (*App).menuAddNode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, before, calls := nodeMenuFixture(t, false)
			var concurrent *Store
			old := stdinReader
			t.Cleanup(func() { stdinReader = old })
			stdinReader = bufio.NewReader(io.MultiReader(strings.NewReader(tc.prefix), &nodeMenuBeforeRead{
				reader: strings.NewReader(tc.suffix),
				run: func() {
					concurrent = cloneStore(before)
					concurrent.Nodes[0], concurrent.Nodes[1] = concurrent.Nodes[1], concurrent.Nodes[0]
					concurrent.DefaultNodeID = "backup"
					if tc.name == "delete all" {
						concurrent.Nodes = append(concurrent.Nodes, Node{ID: "new", Name: "new", Protocol: "trojan", RawURL: "trojan://secret@new.example:443"})
					}
					if err := a.saveStore(concurrent); err != nil {
						t.Fatal(err)
					}
				},
			}))
			if err := tc.run(a, before); err == nil || !strings.Contains(err.Error(), "发生变化") {
				t.Fatalf("concurrent change accepted: %v", err)
			}
			assertSubscriptionStoreUnchanged(t, a, concurrent)
			if *calls != 0 {
				t.Fatalf("stale confirmation invoked runtime %d times", *calls)
			}
		})
	}
}

func TestMenuUseAndDeleteUseConfirmedScopeAndFallback(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("menu commit requires root")
	}
	a, before, _ := nodeMenuFixture(t, false)
	nodeMenuInput(t, "2\n4\ny\n")
	if err := a.menuUseNode(before); err != nil {
		t.Fatal(err)
	}
	selected, err := a.loadStore()
	if err != nil || selected.DefaultNodeID != "selected" || selected.SceneNodes[SceneTelegram] != "backup" || len(selected.SceneNodes) != 1 {
		t.Fatalf("scope was not preserved: %+v %v", selected, err)
	}
	preview := cloneStore(selected)
	if err := removeNodeFromStore(preview, "backup"); err != nil {
		t.Fatal(err)
	}
	stdinReader = bufio.NewReader(strings.NewReader("2\ny\n"))
	if err := a.menuRemoveNode(selected); err != nil {
		t.Fatal(err)
	}
	removed, err := a.loadStore()
	preview.Generation++
	if err != nil || !reflect.DeepEqual(removed, preview) || removed.selectedNodeID(SceneTelegram) != "selected" {
		t.Fatalf("delete differed from confirmed preview: %+v %v", removed, err)
	}
}

func TestMenuLastNodeRemovalPreviewClosesEveryScene(t *testing.T) {
	st := newStore()
	st.Nodes = []Node{{ID: "only"}}
	st.DefaultNodeID = "only"
	for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
		st.SceneNodes[scene] = "only"
		st.SceneEnabled[scene] = true
	}
	if err := removeNodeFromStore(st, "only"); err != nil {
		t.Fatal(err)
	}
	if len(st.Nodes) != 0 || len(st.SceneNodes) != 0 || st.DefaultNodeID != "" || hasEnabledScene(st) {
		t.Fatalf("last-node removal leaves selected/enabled scene: %+v", st)
	}
}
