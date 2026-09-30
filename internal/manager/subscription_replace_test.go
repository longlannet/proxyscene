package manager

import (
	"fmt"
	"slices"
	"testing"
)

func TestSubscriptionReplaceTenWithFifteenWithoutAccumulating(t *testing.T) {
	a, st := subscriptionUpdateFixture(t, reconcileSourceA, reconcileSourceB)
	manual := st.Nodes[0]
	other := reconcileTestNode(t, "other-source", true, reconcileSourceB)
	st.Nodes = append(st.Nodes, other)
	for i := range 10 {
		node := reconcileTestNode(t, fmt.Sprintf("old-%d", i), true, reconcileSourceA)
		st.Nodes = append(st.Nodes, node)
		st.SpeedResults[node.ID] = SpeedResult{NodeID: node.ID, Success: true, LatencyMS: 10}
	}
	st.DefaultNodeID = "old-0"
	st.SceneNodes[SceneTelegram] = "old-9"
	st.SceneEnabled[SceneGlobal] = false
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	for round := range 3 {
		var fresh []Node
		var wantURLs []string
		for i := range 15 {
			node := reconcileTestNode(t, fmt.Sprintf("round-%d-fresh-%d", round, i), true, reconcileSourceA)
			fresh = append(fresh, node)
			wantURLs = append(wantURLs, node.RawURL)
		}
		if err := a.updateSubscriptionsWithDownloader("1", func(raw string) (preparedSubscription, error) {
			return reconcileTestRefresh(t, raw, fresh...), nil
		}); err != nil {
			t.Fatal(err)
		}
		got, err := a.loadStore()
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Nodes) != 17 || got.findNode(manual.ID) == nil || got.findNode(other.ID) == nil {
			t.Fatalf("round %d: replacement accumulated nodes or removed unrelated nodes: count=%d", round, len(got.Nodes))
		}
		var gotURLs []string
		for _, node := range got.Nodes {
			if slices.Contains(node.SubscriptionIDs, subscriptionID(reconcileSourceA)) {
				gotURLs = append(gotURLs, node.RawURL)
			}
		}
		slices.Sort(gotURLs)
		slices.Sort(wantURLs)
		if !slices.Equal(gotURLs, wantURLs) {
			t.Fatalf("round %d: subscription must equal the complete new list", round)
		}
		first := got.findNodeByURL(fresh[0].RawURL)
		if first == nil || got.DefaultNodeID != first.ID || got.SceneNodes[SceneTelegram] != first.ID {
			t.Fatalf("round %d: removed selections did not migrate to the source's first current node", round)
		}
		if len(got.SpeedResults) != 0 {
			t.Fatalf("round %d: withdrawn nodes left stale speed results", round)
		}
		assertSubscriptionStoreUnchanged(t, a, got)
	}
}

func TestSubscriptionReplaceReusesIncomingURLAlreadyOwnedBySurvivingNode(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprintf("manual=%t", manual), func(t *testing.T) {
			a, st := subscriptionUpdateFixture(t, reconcileSourceA, reconcileSourceB)
			old := reconcileTestNode(t, "old-selected", true, reconcileSourceA)
			old.RawURL += "#old"
			survivor := old
			survivor.ID = "survivor"
			survivor.RawURL = "trojan://secret@old-selected.example:443#fresh"
			survivor.SubscriptionManaged = !manual
			survivor.SubscriptionIDs = []string{subscriptionID(reconcileSourceB)}
			st.Nodes = []Node{old, survivor}
			st.DefaultNodeID = old.ID
			// An equivalent connection needs only a metadata transaction even
			// when the selected ID moves to a manually retained/source B copy.
			if err := a.saveStore(st); err != nil {
				t.Fatal(err)
			}
			if err := a.updateSubscriptionsWithDownloader("1", func(raw string) (preparedSubscription, error) {
				return reconcileTestRefresh(t, raw, survivor), nil
			}); err != nil {
				t.Fatal(err)
			}
			got, err := a.loadStore()
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Nodes) != 1 || got.DefaultNodeID != survivor.ID || got.Nodes[0].RawURL != survivor.RawURL || got.Nodes[0].SubscriptionManaged != survivor.SubscriptionManaged {
				t.Fatalf("incoming URL duplicated or reclassified its existing owner: %+v", got.Nodes)
			}
			for _, source := range []string{reconcileSourceA, reconcileSourceB} {
				if !slices.Contains(got.Nodes[0].SubscriptionIDs, subscriptionID(source)) {
					t.Fatal("surviving node lost source ownership")
				}
			}
		})
	}
}
