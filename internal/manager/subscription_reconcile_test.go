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

func TestReconcileSubscriptionsReplacesExplicitAndImplicitSelections(t *testing.T) {
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
			st.SpeedResults[selected.ID] = SpeedResult{NodeID: selected.ID, Success: true}
			beforeSelected := make(map[Scene]string)
			for _, scene := range []Scene{SceneGlobal, SceneDev, SceneTelegram} {
				beforeSelected[scene] = st.selectedNodeID(scene)
			}
			replacement := reconcileTestNode(t, "replacement", false)
			changes, err := reconcileSubscriptions(st, []preparedSubscription{
				reconcileTestRefresh(t, reconcileSourceA, replacement),
			})
			if err != nil {
				t.Fatal(err)
			}
			fresh := st.findNodeByURL(replacement.RawURL)
			if st.findNode(selected.ID) != nil || fresh == nil || len(st.Nodes) != 2 || changes.Reselected != 1 || changes.Removed != 1 {
				t.Fatalf("selected stale node was not replaced: nodes=%d changes=%+v", len(st.Nodes), changes)
			}
			for scene, before := range beforeSelected {
				want := before
				if before == selected.ID {
					want = fresh.ID
				}
				if got := st.selectedNodeID(scene); got != want {
					t.Fatalf("%s selection = %s, want %s", scene, got, want)
				}
			}
			if _, found := st.SpeedResults[selected.ID]; found {
				t.Fatal("withdrawn selected node left stale speed data")
			}
			if _, err := validateStoreSemantics(st); err != nil {
				t.Fatalf("replacement left invalid bindings: %v", err)
			}
		})
	}
}

func TestReconcileSubscriptionsCapacityUsesFinalNodeSet(t *testing.T) {
	for _, selectedStale := range []bool{false, true} {
		t.Run(fmt.Sprintf("selected=%t", selectedStale), func(t *testing.T) {
			st := reconcileTestStore()
			for i := range maxTotalNodes {
				st.Nodes = append(st.Nodes, reconcileTestNode(t, fmt.Sprintf("existing-%d", i), false))
			}
			st.DefaultNodeID = st.Nodes[0].ID
			staleID := st.Nodes[1].ID
			st.Nodes[1].SubscriptionManaged = true
			st.Nodes[1].SubscriptionIDs = []string{subscriptionID(reconcileSourceA)}
			st.SpeedResults[staleID] = SpeedResult{NodeID: staleID, LatencyMS: 12, Success: true}
			if selectedStale {
				st.SceneNodes[SceneTelegram] = staleID
			}
			replacement := reconcileTestNode(t, "replacement", false)
			changes, err := reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceA, replacement)})
			if err != nil {
				t.Fatal(err)
			}
			fresh := st.findNodeByURL(replacement.RawURL)
			if len(st.Nodes) != maxTotalNodes || st.findNode(staleID) != nil || fresh == nil || changes.Added != 1 || changes.Removed != 1 {
				t.Fatalf("full store failed to replace a stale node: count=%d changes=%+v", len(st.Nodes), changes)
			}
			if selectedStale && (st.SceneNodes[SceneTelegram] != fresh.ID || changes.Reselected != 1) {
				t.Fatal("full store did not migrate its withdrawn selected node")
			}
			if _, found := st.SpeedResults[staleID]; found {
				t.Fatal("capacity replacement left removed node speed data")
			}
			before := cloneStore(st)
			_, err = reconcileSubscriptions(st, []preparedSubscription{
				reconcileTestRefresh(t, reconcileSourceA, replacement, reconcileTestNode(t, "overflow", false)),
			})
			if err == nil {
				t.Fatal("actual final-set capacity overflow unexpectedly accepted")
			}
			if !reflect.DeepEqual(cloneStore(st), before) {
				t.Fatal("capacity failure mutated nodes, bindings, memberships, or speed results")
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

func TestReconcileSubscriptionsReplacementUsesSavedSourceOrder(t *testing.T) {
	for _, reversePayload := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse-payload=%t", reversePayload), func(t *testing.T) {
			selected := reconcileTestNode(t, "shared-selected", true, reconcileSourceA, reconcileSourceB)
			st := reconcileTestStore(selected)
			st.Subscriptions = []string{reconcileSourceB, reconcileSourceA}
			firstA := reconcileTestNode(t, "first-a", false)
			firstB := reconcileTestNode(t, "first-b", false)
			prepared := []preparedSubscription{
				reconcileTestRefresh(t, reconcileSourceA, firstA),
				reconcileTestRefresh(t, reconcileSourceB, firstB, reconcileTestNode(t, "second-b", false)),
			}
			if reversePayload {
				slices.Reverse(prepared)
			}
			changes, err := reconcileSubscriptions(st, prepared)
			if err != nil {
				t.Fatal(err)
			}
			want := st.findNodeByURL(firstB.RawURL)
			if want == nil || st.DefaultNodeID != want.ID || st.findNode(selected.ID) != nil || changes.Reselected != 1 {
				t.Fatalf("replacement did not follow saved source order: selected=%s changes=%+v", st.DefaultNodeID, changes)
			}
		})
	}
}

func TestReconcileSubscriptionsSelectionPrefersEquivalentKeeper(t *testing.T) {
	first := reconcileTestNode(t, "first", true, reconcileSourceA)
	second := first
	second.ID, second.RawURL = "second", first.RawURL+"#second"
	st := reconcileTestStore(first, second)
	st.SceneNodes[SceneTelegram] = second.ID
	st.SpeedResults[second.ID] = SpeedResult{NodeID: second.ID, Success: true}
	unrelated := reconcileTestNode(t, "unrelated-first-entry", false)
	changes, err := reconcileSubscriptions(st, []preparedSubscription{
		reconcileTestRefresh(t, reconcileSourceA, unrelated, second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Nodes) != 2 || st.findNode(second.ID) != nil || st.DefaultNodeID != first.ID || st.SceneNodes[SceneTelegram] != first.ID || changes.Reselected != 1 {
		t.Fatalf("duplicate selection did not migrate to equivalent keeper: changes=%+v bindings=%+v", changes, st.SceneNodes)
	}
	if _, exists := st.SpeedResults[second.ID]; exists {
		t.Fatal("removed equivalent duplicate left speed data")
	}
}

func TestReconcileSubscriptionsReplacesPreviouslyRetainedOrphan(t *testing.T) {
	manual := reconcileTestNode(t, "manual", false)
	manual.SubscriptionIDs = []string{}
	orphan := reconcileTestNode(t, "old-retained", true)
	st := reconcileTestStore(manual, orphan)
	st.DefaultNodeID = orphan.ID
	st.SceneNodes[SceneTelegram] = orphan.ID
	st.SpeedResults[orphan.ID] = SpeedResult{NodeID: orphan.ID, Success: true}
	fresh := reconcileTestNode(t, "fresh", false)
	changes, err := reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceB, fresh)})
	if err != nil {
		t.Fatal(err)
	}
	got := st.findNodeByURL(fresh.RawURL)
	if len(st.Nodes) != 2 || got == nil || st.findNode(orphan.ID) != nil || st.DefaultNodeID != got.ID || st.SceneNodes[SceneTelegram] != got.ID || changes.Reselected != 1 {
		t.Fatalf("old retained orphan survived complete refresh: changes=%+v nodes=%d", changes, len(st.Nodes))
	}
	if gotManual := st.findNode(manual.ID); gotManual == nil || !reflect.DeepEqual(*gotManual, manual) {
		t.Fatal("orphan cleanup modified unrelated manual node")
	}
	if _, exists := st.SpeedResults[orphan.ID]; exists {
		t.Fatal("orphan cleanup left speed data")
	}
}

func TestReconcileSubscriptionsChangedSnapshotsDoNotAccumulate(t *testing.T) {
	st := newStore()
	for round := range 4 {
		count := 15
		if round == 0 {
			count = 10
		}
		incoming := make([]Node, 0, count)
		for i := range count {
			incoming = append(incoming, reconcileTestNode(t, fmt.Sprintf("round-%d-node-%d", round, i), false))
		}
		beforeID := st.DefaultNodeID
		changes, err := reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceA, incoming...)})
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Nodes) != count || st.DefaultNodeID != st.findNodeByURL(incoming[0].RawURL).ID {
			t.Fatalf("round %d did not replace the snapshot: count=%d changes=%+v", round, len(st.Nodes), changes)
		}
		if round > 0 && (st.findNode(beforeID) != nil || changes.Reselected != 1) {
			t.Fatalf("round %d retained selected node from older snapshot: %+v", round, changes)
		}
	}
}

func TestReconcileSubscriptionsSelectionFindsEquivalentUnrefreshedSource(t *testing.T) {
	selected := reconcileTestNode(t, "selected-a", true, reconcileSourceA)
	survivor := selected
	survivor.ID, survivor.RawURL = "survivor-b", selected.RawURL+"#source-b"
	survivor.SubscriptionIDs = []string{subscriptionID(reconcileSourceB)}
	st := reconcileTestStore(selected, survivor)
	st.SceneNodes[SceneTelegram] = selected.ID
	speed := SpeedResult{NodeID: survivor.ID, Success: true, LatencyMS: 25}
	st.SpeedResults[survivor.ID] = speed
	st.SpeedResults[selected.ID] = SpeedResult{NodeID: selected.ID, Success: true}
	fresh := reconcileTestNode(t, "fresh-a", false)
	changes, err := reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceA, fresh)})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Nodes) != 2 || st.findNode(selected.ID) != nil || st.DefaultNodeID != survivor.ID || st.SceneNodes[SceneTelegram] != survivor.ID || changes.Reselected != 1 {
		t.Fatalf("selection ignored equivalent node from unrefreshed source: changes=%+v bindings=%+v", changes, st.SceneNodes)
	}
	if got := st.findNode(survivor.ID); got == nil || !reflect.DeepEqual(*got, survivor) || !reflect.DeepEqual(st.SpeedResults[survivor.ID], speed) {
		t.Fatal("refresh changed survivor from unrefreshed source")
	}
	if _, exists := st.SpeedResults[selected.ID]; exists {
		t.Fatal("withdrawn selection left duplicate speed data")
	}
}
