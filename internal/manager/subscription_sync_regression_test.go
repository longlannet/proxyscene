package manager

import (
	"fmt"
	"net/url"
	"os"
	"reflect"
	"slices"
	"testing"
)

func TestSubscriptionSyncRepairsLegacyAccumulationAfterExplicitAdoption(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("explicit adoption requires root")
	}
	a, st := subscriptionUpdateFixture(t, reconcileSourceA)
	var adoptIDs []string
	var current []Node
	// Reproduce an already affected store: one independent manual node,
	// ten untracked legacy nodes, and fifteen later managed imports.
	for i := range 10 {
		node := reconcileTestNode(t, fmt.Sprintf("legacy-%d", i), false)
		node.RawURL += "#old-name"
		st.Nodes = append(st.Nodes, node)
		adoptIDs = append(adoptIDs, node.ID)
		st.SpeedResults[node.ID] = SpeedResult{NodeID: node.ID, Success: true, LatencyMS: int64(i + 1)}
	}
	for i := range 15 {
		node := reconcileTestNode(t, fmt.Sprintf("fresh-%d", i), true, reconcileSourceA)
		if i < 6 {
			node.RawURL = fmt.Sprintf("trojan://secret@legacy-%d.example:443#new-name", i)
		}
		st.Nodes = append(st.Nodes, node)
		current = append(current, node)
		st.SpeedResults[node.ID] = SpeedResult{NodeID: node.ID, Success: true, LatencyMS: 20}
	}
	if len(st.Nodes) != 26 {
		t.Fatal("fixture must reproduce the reported 26-node state")
	}
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	manual := st.Nodes[0]
	if err := a.subscriptionCommand(append([]string{"adopt", "1"}, adoptIDs...)); err != nil {
		t.Fatal(err)
	}
	adopted, err := a.loadStore()
	if err != nil || len(adopted.Nodes) != 26 || adopted.DefaultNodeID != manual.ID {
		t.Fatalf("adoption must only record ownership: count/default changed or load failed: %v", err)
	}
	for round := range 3 {
		err := a.updateSubscriptionsWithDownloader("1", func(raw string) (preparedSubscription, error) {
			return reconcileTestRefresh(t, raw, current...), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.loadStore()
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Nodes) != 16 || got.DefaultNodeID != manual.ID || !reflect.DeepEqual(*got.findNode(manual.ID), manual) {
			t.Fatalf("round %d retained old copies or modified manual selection: count=%d default=%s", round, len(got.Nodes), got.DefaultNodeID)
		}
		for i := range 10 {
			id := fmt.Sprintf("legacy-%d", i)
			if i < 6 {
				if got.findNode(id) == nil || got.SpeedResults[id].LatencyMS != int64(i+1) {
					t.Fatalf("round %d lost matching legacy node identity or speed: %s", round, id)
				}
			} else {
				if got.findNode(id) != nil {
					t.Fatalf("round %d retained withdrawn legacy node %s", round, id)
				}
				if _, exists := got.SpeedResults[id]; exists {
					t.Fatalf("round %d retained withdrawn node speed %s", round, id)
				}
			}
		}
		assertSubscriptionStoreUnchanged(t, a, got)
	}
}

func TestSubscriptionSyncEquivalentKeeperPriority(t *testing.T) {
	for _, kind := range []string{"selected", "manual", "first"} {
		t.Run(kind, func(t *testing.T) {
			anchor := reconcileTestNode(t, "anchor", false)
			first := reconcileTestNode(t, "first", true, reconcileSourceA)
			second := first
			second.ID, second.RawURL = "second", first.RawURL+"#second"
			if kind == "manual" {
				second.SubscriptionManaged = false
			}
			st := reconcileTestStore(anchor, first, second)
			if kind == "selected" {
				st.SceneNodes[SceneTelegram] = second.ID
			}
			wantID := second.ID
			if kind == "first" {
				wantID = first.ID
			}
			incoming := first
			incoming.RawURL += "#incoming"
			for range 2 {
				changes, err := reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceA, incoming)})
				if err != nil {
					t.Fatal(err)
				}
				if len(st.Nodes) != 2 || st.findNode(wantID) == nil || changes.Added != 0 || changes.Existing != 1 {
					t.Fatalf("wrong equivalent keeper: nodes=%d keeper=%s changes=%+v", len(st.Nodes), wantID, changes)
				}
			}
		})
	}
}

func TestSubscriptionSyncDuplicateKeepsUnrefreshedOwnership(t *testing.T) {
	first := reconcileTestNode(t, "first", true, reconcileSourceA)
	other := first
	other.ID, other.RawURL = "other", first.RawURL+"#other"
	other.SubscriptionIDs = []string{subscriptionID(reconcileSourceA), subscriptionID(reconcileSourceB)}
	slices.Sort(other.SubscriptionIDs)
	st := reconcileTestStore(first, other)
	incoming := first
	incoming.RawURL += "#incoming"
	changes, err := reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceA, incoming)})
	if err != nil || changes.Added != 0 || changes.Removed != 0 || len(st.Nodes) != 2 || !slices.Equal(st.findNode(other.ID).SubscriptionIDs, []string{subscriptionID(reconcileSourceB)}) {
		t.Fatalf("equivalent cleanup touched unrefreshed source: changes=%+v err=%v", changes, err)
	}
	if !slices.Equal(st.findNode(first.ID).SubscriptionIDs, []string{subscriptionID(reconcileSourceA)}) {
		t.Fatal("unrefreshed source was silently transferred to the keeper")
	}
	changes, err = reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceB, incoming)})
	wantSources := []string{subscriptionID(reconcileSourceA), subscriptionID(reconcileSourceB)}
	slices.Sort(wantSources)
	if err != nil || changes.Removed != 1 || len(st.Nodes) != 1 || !slices.Equal(st.Nodes[0].SubscriptionIDs, wantSources) {
		t.Fatalf("complete refresh did not clean equivalent managed copy: changes=%+v err=%v", changes, err)
	}
}

func TestSubscriptionSyncLeavesLegacyIncompatibleNodesReadable(t *testing.T) {
	raw := compatibilityVLESSBase + "security=tls&type=xhttp&extra=" + url.QueryEscape(`{"downloadSettings":{}}`)
	prepared, err := prepareStoredNode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareNode(raw); err == nil {
		t.Fatal("fixture must remain stored-readable but ineligible for import")
	}
	legacy := Node{ID: "legacy", Name: "legacy", RawURL: raw, Protocol: prepared.Parsed.Protocol, SubscriptionIDs: []string{}}
	st := reconcileTestStore(legacy)
	current := reconcileTestNode(t, "current", true, reconcileSourceA)
	for range 2 {
		changes, err := reconcileSubscriptions(st, []preparedSubscription{reconcileTestRefresh(t, reconcileSourceA, current)})
		if err != nil || len(st.Nodes) != 2 || !reflect.DeepEqual(*st.findNode(legacy.ID), legacy) || changes.Removed != 0 {
			t.Fatalf("legacy fallback blocked refresh or modified old selection: changes=%+v err=%v", changes, err)
		}
	}
}
