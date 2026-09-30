package manager

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestNodeRemoveAllPreviewClearsEverySelectionAndKeepsSubscriptions(t *testing.T) {
	st := newStore()
	st.Nodes = []Node{{ID: "manual"}, {ID: "subscription", SubscriptionManaged: true, SubscriptionIDs: []string{"source"}}}
	st.DefaultNodeID = "manual"
	st.Subscriptions = []string{"https://example.invalid/sub"}
	st.RuntimeConfig = DefaultConfig().runtimeConfig()
	st.TelegramTargets = []string{"hermes.service"}
	st.SpeedResults["manual"] = SpeedResult{NodeID: "manual", Success: true}
	st.SpeedResults["subscription"] = SpeedResult{NodeID: "subscription", Success: true}
	for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
		st.SceneNodes[scene] = "subscription"
		st.SceneEnabled[scene] = true
	}
	preview := cloneStore(st)
	removeAllNodesFromStore(preview)
	if len(preview.Nodes) != 0 || preview.DefaultNodeID != "" || len(preview.SceneNodes) != 0 || len(preview.SpeedResults) != 0 || hasEnabledScene(preview) {
		t.Fatalf("bulk removal leaves node state or enabled scenes: %+v", preview)
	}
	if !reflect.DeepEqual(preview.Subscriptions, st.Subscriptions) || !reflect.DeepEqual(preview.RuntimeConfig, st.RuntimeConfig) || !reflect.DeepEqual(preview.TelegramTargets, st.TelegramTargets) {
		t.Fatal("preview discarded subscriptions, settings or runtime cleanup evidence")
	}
	if len(st.Nodes) != 2 || len(st.SceneNodes) != 3 || len(st.SpeedResults) != 2 || !hasEnabledScene(st) {
		t.Fatal("preview mutated original state")
	}
}

func TestNodeRemoveAllMenuAndCommandCommitOnce(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("node mutation requires root")
	}
	for _, route := range []string{"menu", "remove", "delete"} {
		t.Run(route, func(t *testing.T) {
			a, before, calls := nodeMenuFixture(t, false)
			before.Subscriptions = []string{"https://example.invalid/sub?token=private"}
			before.Nodes[1].SubscriptionManaged = true
			before.Nodes[1].SubscriptionIDs = []string{subscriptionID(before.Subscriptions[0])}
			before.SceneNodes[SceneTelegram] = "backup"
			before.SpeedResults["selected"] = SpeedResult{NodeID: "selected", Success: true}
			if err := a.saveStore(before); err != nil {
				t.Fatal(err)
			}
			output, err := captureMainMenuTestOutput(t, func() error {
				if route == "menu" {
					nodeMenuInput(t, "8\ny\n0\n")
					return a.nodeMenu()
				}
				return a.nodeCommand([]string{route, "--all"})
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := a.loadStore()
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Nodes) != 0 || got.DefaultNodeID != "" || len(got.SceneNodes) != 0 || len(got.SpeedResults) != 0 || hasEnabledScene(got) {
				t.Fatalf("node state remains after deleting all: %+v", got)
			}
			if got.Generation != before.Generation+1 || *calls != 1 {
				t.Fatalf("bulk deletion did not commit once: generation %d -> %d, runtime calls %d", before.Generation, got.Generation, *calls)
			}
			if !reflect.DeepEqual(got.Subscriptions, before.Subscriptions) || !reflect.DeepEqual(got.RuntimeConfig, before.RuntimeConfig) {
				t.Fatal("bulk deletion changed subscriptions or runtime settings")
			}
			assertSubscriptionStoreUnchanged(t, a, got)
			if strings.Contains(output, "token=private") {
				t.Fatal("deletion preview exposed subscription credentials")
			}
			if route == "menu" {
				for _, want := range []string{"8. 删除全部节点", "即将删除全部 2 个节点", "所有代理场景均关闭", "订阅地址保留"} {
					if !strings.Contains(output, want) {
						t.Fatalf("missing deletion preview %q: %s", want, output)
					}
				}
			}
		})
	}
}

func TestNodeRemoveAllEmptyListHasNoSideEffects(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("node command requires root")
	}
	for _, route := range []string{"menu", "remove", "delete"} {
		t.Run(route, func(t *testing.T) {
			a, before, calls := nodeMenuFixture(t, true)
			output, err := captureMainMenuTestOutput(t, func() error {
				if route == "menu" {
					nodeMenuInput(t, "8\n0\n")
					return a.nodeMenu()
				}
				return a.nodeCommand([]string{route, "--all"})
			})
			if err != nil || !strings.Contains(output, "节点列表为空，无需删除") {
				t.Fatalf("empty-list result: %s %v", output, err)
			}
			assertSubscriptionStoreUnchanged(t, a, before)
			if *calls != 0 {
				t.Fatal("empty-list deletion invoked runtime")
			}
		})
	}
}

func TestNodeRemoveAllCommandRejectsExtraOrMissingArguments(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("node command requires root")
	}
	for _, command := range []string{"remove", "delete"} {
		for _, args := range [][]string{{command}, {command, "--all", "selected"}, {command, "selected", "--all"}, {command, "selected", "backup"}} {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				a, before, calls := nodeMenuFixture(t, false)
				if err := a.nodeCommand(args); err == nil || !strings.Contains(err.Error(), "用法") {
					t.Fatalf("invalid deletion arguments accepted: %v", err)
				}
				assertSubscriptionStoreUnchanged(t, a, before)
				if *calls != 0 {
					t.Fatal("invalid deletion invoked runtime")
				}
			})
		}
	}
}
