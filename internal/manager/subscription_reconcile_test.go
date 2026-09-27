package manager

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

const (
	reconcileSourceA = "https://a.example/subscription"
	reconcileSourceB = "https://b.example/subscription"
)

func reconcileTestNode(t *testing.T, id string, managed bool, sources ...string) Node {
	t.Helper()
	prepared, err := prepareNode("trojan://secret@" + id + ".example:443")
	if err != nil {
		t.Fatal(err)
	}
	node := Node{
		ID: id, Name: id, Protocol: prepared.Parsed.Protocol, RawURL: prepared.RawURL,
		CreatedAt: time.Unix(100, 0), UpdatedAt: time.Unix(200, 0), SubscriptionManaged: managed,
	}
	for _, source := range sources {
		node.SubscriptionIDs = append(node.SubscriptionIDs, subscriptionID(source))
	}
	slices.Sort(node.SubscriptionIDs)
	return node
}

func reconcileTestRefresh(t *testing.T, source string, nodes ...Node) preparedSubscription {
	t.Helper()
	sub := preparedSubscription{URL: source}
	for _, node := range nodes {
		prepared, err := prepareNode(node.RawURL)
		if err != nil {
			t.Fatal(err)
		}
		sub.Nodes = append(sub.Nodes, prepared)
	}
	return sub
}

func reconcileTestStore(nodes ...Node) *Store {
	st := newStore()
	st.Subscriptions = []string{reconcileSourceA, reconcileSourceB}
	st.Nodes = nodes
	if len(nodes) > 0 {
		st.DefaultNodeID = nodes[0].ID
	}
	return st
}

func TestReconcileSubscriptionsSharedNodeSurvivesUntilAllSourcesDisappear(t *testing.T) {
	anchor := reconcileTestNode(t, "anchor", false)
	shared := reconcileTestNode(t, "shared", true, reconcileSourceA, reconcileSourceB)
	shared.Name = "my custom shared node"
	st := reconcileTestStore(anchor, shared)
	sharedSpeed := SpeedResult{NodeID: shared.ID, LatencyMS: 81, Success: true, TestedAt: time.Unix(300, 0)}
	anchorSpeed := SpeedResult{NodeID: anchor.ID, LatencyMS: 42, Success: true}
	st.SpeedResults[shared.ID] = sharedSpeed
	st.SpeedResults[anchor.ID] = anchorSpeed
	newA := reconcileTestNode(t, "new-a", false)
	changes, err := reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceA, newA)})
	if err != nil {
		t.Fatal(err)
	}
	got := st.findNode(shared.ID)
	if got == nil || !reflect.DeepEqual(got.SubscriptionIDs, []string{subscriptionID(reconcileSourceB)}) || got.Name != shared.Name {
		t.Fatalf("updating source A lost source B's shared node or metadata: %+v", got)
	}
	if changes.Removed != 0 || changes.Added != 1 || !reflect.DeepEqual(st.SpeedResults[shared.ID], sharedSpeed) {
		t.Fatalf("first source removal changed shared-node identity or speed: changes=%+v speed=%+v", changes, st.SpeedResults)
	}

	newB := reconcileTestNode(t, "new-b", false)
	changes, err = reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceB, newB)})
	if err != nil {
		t.Fatal(err)
	}
	if st.findNode(shared.ID) != nil || changes.Removed != 1 {
		t.Fatalf("unselected managed node survived after its last source disappeared: nodes=%+v changes=%+v", st.Nodes, changes)
	}
	if _, found := st.SpeedResults[shared.ID]; found {
		t.Fatal("removed node's speed result remains")
	}
	if !reflect.DeepEqual(st.SpeedResults[anchor.ID], anchorSpeed) || st.findNodeByURL(newA.RawURL) == nil {
		t.Fatal("updating source B modified unrelated speed or source A nodes")
	}
}

func TestReconcileSubscriptionsUpdateAllSourceHandoverPreservesMetadata(t *testing.T) {
	for _, reverseOrder := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverseOrder), func(t *testing.T) {
			anchor := reconcileTestNode(t, "anchor", false)
			moving := reconcileTestNode(t, "moving", true, reconcileSourceA)
			moving.Name = "operator's custom name"
			st := reconcileTestStore(anchor, moving)
			speed := SpeedResult{NodeID: moving.ID, Target: "probe", LatencyMS: 73, Success: true}
			st.SpeedResults[moving.ID] = speed
			prepared := []preparedSubscription{
				reconcileTestRefresh(t, reconcileSourceA, reconcileTestNode(t, "replacement", false)),
				reconcileTestRefresh(t, reconcileSourceB, moving),
			}
			if reverseOrder {
				slices.Reverse(prepared)
			}
			changes, err := reconcileSubscriptions(st, prepared)
			if err != nil {
				t.Fatal(err)
			}
			want := moving
			want.SubscriptionIDs = []string{subscriptionID(reconcileSourceB)}
			if got := st.findNode(moving.ID); got == nil || !reflect.DeepEqual(*got, want) {
				t.Fatalf("source handover replaced node identity, custom name, or timestamps: got=%+v want=%+v", got, want)
			}
			if changes.Added != 1 || changes.Existing != 1 || changes.Removed != 0 || !reflect.DeepEqual(st.SpeedResults[moving.ID], speed) {
				t.Fatalf("source handover changed speed or reported a replacement: %+v", changes)
			}
		})
	}
}

func TestReconcileSubscriptionsPreservesLegacyAndManualNodes(t *testing.T) {
	anchor := reconcileTestNode(t, "anchor", false)
	legacy := reconcileTestNode(t, "legacy", false)
	manual := reconcileTestNode(t, "manual", false, reconcileSourceA)
	st := reconcileTestStore(anchor, legacy, manual)
	if _, err := reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceA, legacy)}); err != nil {
		t.Fatal(err)
	}
	if got := st.findNode(legacy.ID); got == nil || got.SubscriptionManaged || !reflect.DeepEqual(got.SubscriptionIDs, []string{subscriptionID(reconcileSourceA)}) {
		t.Fatalf("matching a legacy node changed its unmanaged status: %+v", got)
	}
	changes, err := reconcileSubscriptions(st, []preparedSubscription{
		reconcileTestRefresh(t, reconcileSourceA, reconcileTestNode(t, "replacement", false)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{legacy.ID, manual.ID} {
		got := st.findNode(id)
		if got == nil || got.SubscriptionManaged || len(got.SubscriptionIDs) != 0 {
			t.Fatalf("source disappearance removed or reclassified unmanaged node %s: %+v", id, got)
		}
	}
	if changes.Removed != 0 {
		t.Fatalf("legacy/manual nodes were counted as removed: %+v", changes)
	}
}

func TestReconcileSubscriptionsManualAddProtectsManagedNode(t *testing.T) {
	anchor := reconcileTestNode(t, "anchor", false)
	managed := reconcileTestNode(t, "managed", true, reconcileSourceA)
	st := reconcileTestStore(anchor, managed)
	app := &App{}
	id, err := app.addNode(st, managed.RawURL, "manually retained", "")
	if err != nil {
		t.Fatal(err)
	}
	if id != managed.ID || len(st.Nodes) != 2 || st.findNode(id).SubscriptionManaged {
		t.Fatalf("manual add did not claim the existing managed node: id=%s nodes=%+v", id, st.Nodes)
	}
	if _, err := reconcileSubscriptions(st, []preparedSubscription{
		reconcileTestRefresh(t, reconcileSourceA, reconcileTestNode(t, "replacement", false)),
	}); err != nil {
		t.Fatal(err)
	}
	if got := st.findNode(id); got == nil || got.Name != "manually retained" || len(got.SubscriptionIDs) != 0 || got.SubscriptionManaged {
		t.Fatalf("manual node was not protected after source removal: %+v", got)
	}
}

func TestReconcileSubscriptionsProtectsExplicitAndImplicitSelections(t *testing.T) {
	cases := map[string]func(*Store, string){
		"default": func(st *Store, id string) { st.DefaultNodeID = id },
		"disabled scene": func(st *Store, id string) {
			st.SceneNodes[SceneTelegram] = id
			st.SceneEnabled[SceneTelegram] = false
		},
		"implicit first": func(st *Store, id string) {
			st.DefaultNodeID = ""
			slices.Reverse(st.Nodes)
		},
	}
	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			anchor := reconcileTestNode(t, "anchor", false)
			selected := reconcileTestNode(t, "selected", true, reconcileSourceA)
			st := reconcileTestStore(anchor, selected)
			configure(st, selected.ID)
			beforeDefault := st.DefaultNodeID
			beforeSelected := make(map[Scene]string)
			for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
				beforeSelected[scene] = st.selectedNodeID(scene)
			}
			changes, err := reconcileSubscriptions(st, []preparedSubscription{
				reconcileTestRefresh(t, reconcileSourceA, reconcileTestNode(t, "replacement", false)),
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := st.findNode(selected.ID); got == nil || len(got.SubscriptionIDs) != 0 || !got.SubscriptionManaged {
				t.Fatalf("selected stale node was removed or lost retained status: %+v", got)
			}
			if changes.Retained != 1 || changes.Removed != 0 || st.DefaultNodeID != beforeDefault {
				t.Fatalf("selected stale node not retained without changing selection: changes=%+v default=%s", changes, st.DefaultNodeID)
			}
			for scene, before := range beforeSelected {
				if got := st.selectedNodeID(scene); got != before {
					t.Fatalf("subscription addition changed %s fallback selection from %s to %s", scene, before, got)
				}
			}
		})
	}
}

func TestReconcileSubscriptionsCapacityUsesFinalNodeSet(t *testing.T) {
	for _, protectStale := range []bool{false, true} {
		t.Run(fmt.Sprintf("protected=%t", protectStale), func(t *testing.T) {
			st := reconcileTestStore()
			for i := range maxTotalNodes {
				st.Nodes = append(st.Nodes, reconcileTestNode(t, fmt.Sprintf("existing-%d", i), false))
			}
			st.DefaultNodeID = st.Nodes[0].ID
			staleID := st.Nodes[1].ID
			st.Nodes[1].SubscriptionManaged = true
			st.Nodes[1].SubscriptionIDs = []string{subscriptionID(reconcileSourceA)}
			st.SpeedResults[staleID] = SpeedResult{NodeID: staleID, LatencyMS: 12, Success: true}
			if protectStale {
				st.SceneNodes[SceneTelegram] = staleID
			}
			before := cloneStore(st)
			replacement := reconcileTestNode(t, "replacement", false)
			changes, err := reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceA, replacement)})
			if protectStale {
				if err == nil {
					t.Fatal("capacity overflow unexpectedly accepted")
				}
				if !reflect.DeepEqual(st, before) {
					t.Fatal("capacity failure mutated the original store, memberships, or speed results")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(st.Nodes) != maxTotalNodes || st.findNode(staleID) != nil || st.findNodeByURL(replacement.RawURL) == nil || changes.Added != 1 || changes.Removed != 1 {
				t.Fatalf("full store failed to replace the removable stale node: count=%d changes=%+v", len(st.Nodes), changes)
			}
			if _, found := st.SpeedResults[staleID]; found {
				t.Fatal("capacity replacement left removed node speed data")
			}
		})
	}
}

func TestReconcileSubscriptionsRepeatedImportKeepsSingleURLAndNode(t *testing.T) {
	st := newStore()
	node := reconcileTestNode(t, "imported", false)
	prepared := reconcileTestRefresh(t, reconcileSourceA, node, node)
	changes, err := reconcileSubscriptions(st, []preparedSubscription{prepared})
	if err != nil {
		t.Fatal(err)
	}
	if changes.Added != 1 || len(st.Nodes) != 1 || !st.Nodes[0].SubscriptionManaged || st.DefaultNodeID != st.Nodes[0].ID {
		t.Fatalf("initial import did not deduplicate or select its first node: changes=%+v nodes=%+v", changes, st.Nodes)
	}
	id := st.Nodes[0].ID
	changes, err = reconcileSubscriptions(st, []preparedSubscription{prepared})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Subscriptions) != 1 || st.Subscriptions[0] != reconcileSourceA || len(st.Nodes) != 1 || st.Nodes[0].ID != id || len(st.Nodes[0].SubscriptionIDs) != 1 || changes.Added != 0 || changes.Existing != 1 {
		t.Fatalf("repeated import duplicated subscription or node identity: changes=%+v nodes=%+v subscriptions=%+v", changes, st.Nodes, st.Subscriptions)
	}
}
