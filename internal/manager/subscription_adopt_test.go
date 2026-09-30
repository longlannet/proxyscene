package manager

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func adoptionFixture(t *testing.T) (*App, *Store) {
	t.Helper()
	a, st := subscriptionUpdateFixture(t, reconcileSourceA, reconcileSourceB)
	st.Nodes = append(st.Nodes,
		reconcileTestNode(t, "legacy-one", false),
		reconcileTestNode(t, "legacy-two", false, reconcileSourceB),
		reconcileTestNode(t, "managed", true, reconcileSourceA),
	)
	st.SceneNodes[SceneTelegram] = "legacy-one"
	st.SpeedResults["legacy-one"] = SpeedResult{NodeID: "legacy-one", Success: true, LatencyMS: 8}
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	return a, st
}

func requireAdoptionTestRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("adoption commit requires root")
	}
}

func TestSubscriptionAdoptionPreservesMetadataSelectionsAndSources(t *testing.T) {
	requireAdoptionTestRoot(t)
	a, before := adoptionFixture(t)
	if err := a.adoptSubscriptionNodes("1", []string{"selected", "legacy-one", "legacy-two"}); err != nil {
		t.Fatal(err)
	}
	want := cloneStore(before)
	want.Generation++
	for i := 0; i < 3; i++ {
		want.Nodes[i].SubscriptionManaged = true
		want.Nodes[i].SubscriptionIDs = append(want.Nodes[i].SubscriptionIDs, subscriptionID(reconcileSourceA))
		slices.Sort(want.Nodes[i].SubscriptionIDs)
	}
	assertSubscriptionStoreUnchanged(t, a, want)

	// Existing source membership can be adopted without adding a duplicate.
	a2, st2 := adoptionFixture(t)
	if err := a2.adoptSubscriptionNodes(subscriptionID(reconcileSourceB)[:16], []string{"legacy-two"}); err != nil {
		t.Fatal(err)
	}
	st2.Generation++
	st2.Nodes[2].SubscriptionManaged = true
	assertSubscriptionStoreUnchanged(t, a2, st2)
}

func TestSubscriptionAdoptionRepairsLegacyAccumulationAfterRefresh(t *testing.T) {
	requireAdoptionTestRoot(t)
	a, st := subscriptionUpdateFixture(t, reconcileSourceA)
	var oldIDs []string
	for i := range 10 {
		id := fmt.Sprintf("legacy-%d", i)
		oldIDs = append(oldIDs, id)
		st.Nodes = append(st.Nodes, reconcileTestNode(t, id, false))
	}
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	var fresh []string
	for i := range 15 {
		fresh = append(fresh, fmt.Sprintf("trojan://secret@fresh-%d.example:443", i))
	}
	refresh := func(raw string) (preparedSubscription, error) {
		return prepareUpdateBody(t, raw, strings.Join(fresh, "\n")), nil
	}
	if err := a.updateSubscriptionsWithDownloader("1", refresh); err != nil {
		t.Fatal(err)
	}
	accumulated, err := a.loadStore()
	if err != nil || len(accumulated.Nodes) != 26 {
		t.Fatalf("legacy accumulation fixture not reproduced: count=%d err=%v", len(accumulated.Nodes), err)
	}
	if err := a.adoptSubscriptionNodes("1", oldIDs); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		fresh[0] = fmt.Sprintf("trojan://secret@changed-%d.example:443", i)
		if err := a.updateSubscriptionsWithDownloader("1", refresh); err != nil {
			t.Fatal(err)
		}
		got, err := a.loadStore()
		if err != nil || len(got.Nodes) != 16 || got.DefaultNodeID != "selected" || got.findNode("selected").SubscriptionManaged {
			t.Fatalf("adopted update did not converge while preserving unrelated manual node: count=%d err=%v", len(got.Nodes), err)
		}
		for _, id := range oldIDs {
			if got.findNode(id) != nil {
				t.Fatal("adopted withdrawn standby node remains")
			}
		}
	}
}

func TestSubscriptionAdoptionRejectsInvalidBatchWithoutPartialWrite(t *testing.T) {
	requireAdoptionTestRoot(t)
	for name, tc := range map[string]struct {
		source string
		ids    []string
	}{
		"empty":           {"1", nil},
		"blank ID":        {"1", []string{"legacy-one", ""}},
		"duplicate":       {"1", []string{"legacy-one", "legacy-one"}},
		"missing":         {"1", []string{"legacy-one", "missing"}},
		"already managed": {"1", []string{"legacy-one", "managed"}},
		"node index":      {"1", []string{"2"}},
		"short ID":        {"1", []string{"legacy-o"}},
		"all sources":     {"--all", []string{"legacy-one"}},
		"unknown source":  {"3", []string{"legacy-one"}},
		"source URL":      {reconcileSourceA, []string{"legacy-one"}},
		"too many":        {"1", make([]string, maxTotalNodes+1)},
	} {
		t.Run(name, func(t *testing.T) {
			a, before := adoptionFixture(t)
			if err := a.adoptSubscriptionNodes(tc.source, tc.ids); err == nil {
				t.Fatal("invalid adoption succeeded")
			}
			assertSubscriptionStoreUnchanged(t, a, before)
		})
	}
}

func TestSubscriptionAdoptionSaveFailureRollsBack(t *testing.T) {
	requireAdoptionTestRoot(t)
	a, before := adoptionFixture(t)
	oldWrite := runtimeWriteStoreCAS
	t.Cleanup(func() { runtimeWriteStoreCAS = oldWrite })
	failed := false
	runtimeWriteStoreCAS = func(path string, expected runtimeStoreEvidence, data []byte) error {
		if path == a.cfg.StorePath() && !failed {
			failed = true
			return errors.New("injected adoption main write failure")
		}
		return oldWrite(path, expected, data)
	}
	if err := a.adoptSubscriptionNodes("1", []string{"legacy-one"}); err == nil || !failed {
		t.Fatalf("save failure not exercised: %v", err)
	}
	before.Generation += 2
	assertSubscriptionStoreUnchanged(t, a, before)
}

func TestSubscriptionAdoptionMenuCancellationAndInvalidSelections(t *testing.T) {
	for _, input := range []string{
		"", "q\n", "\n", "1\n", "1\nq\n", "1\n\n", "1\n2\n", "1\n2\ny", "1\n2\nq\n", "1\n2\nn\n", "1\n2\n\n",
		"--all\n", "1\n2,2\ny\n", "1\nmanaged\ny\n", "1\nlegacy\ny\n", "1\n,,\ny\n",
	} {
		t.Run(input, func(t *testing.T) {
			a, before := adoptionFixture(t)
			subscriptionMenuInputForTest(t, input)
			if err := a.adoptSubscriptionNodesMenu(); err == nil {
				t.Fatal("incomplete, declined or invalid adoption succeeded")
			}
			assertSubscriptionStoreUnchanged(t, a, before)
		})
	}
}

func TestSubscriptionAdoptionMenuResolvesOnlyDisplayedCandidates(t *testing.T) {
	requireAdoptionTestRoot(t)
	a, before := adoptionFixture(t)
	// A managed node before candidates must not shift their displayed indices.
	before.Nodes[0], before.Nodes[3] = before.Nodes[3], before.Nodes[0]
	before.Subscriptions[0] += "?token=SUBSCRIPTION_SECRET"
	before.Nodes[0].SubscriptionIDs = []string{subscriptionID(before.Subscriptions[0])}
	if err := a.saveStore(before); err != nil {
		t.Fatal(err)
	}
	subscriptionMenuInputForTest(t, "1\n1, legacy-t selected\ny\n")
	out, err := captureMainMenuTestOutput(t, a.adoptSubscriptionNodesMenu)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SUBSCRIPTION_SECRET") || strings.Contains(out, "trojan://") || strings.Contains(out, "secret@") || !strings.Contains(out, "当前选用") || !strings.Contains(out, "所有来源均已移除") {
		t.Fatalf("menu leaked credentials or omitted adoption consequences: %s", out)
	}
	got, err := a.loadStore()
	if err != nil || got.Generation != before.Generation+1 || got.DefaultNodeID != before.DefaultNodeID || !reflect.DeepEqual(got.SceneNodes, before.SceneNodes) {
		t.Fatalf("menu adoption changed selection or did not commit once: %v", err)
	}
	for _, id := range []string{"legacy-one", "legacy-two", "selected"} {
		if !got.findNode(id).SubscriptionManaged || !slices.Contains(got.findNode(id).SubscriptionIDs, subscriptionID(before.Subscriptions[0])) {
			t.Fatalf("displayed candidate %s not adopted", id)
		}
	}
	if !reflect.DeepEqual(*got.findNode("managed"), *before.findNode("managed")) {
		t.Fatal("menu changed a node excluded from the candidate list")
	}
}

func TestSubscriptionAdoptionMenuRejectsConcurrentChange(t *testing.T) {
	requireAdoptionTestRoot(t)
	for _, mutation := range []string{"nodes reordered", "node identity changed", "subscriptions reordered", "selection changed"} {
		t.Run(mutation, func(t *testing.T) {
			a, before := adoptionFixture(t)
			var concurrent *Store
			old := stdinReader
			t.Cleanup(func() { stdinReader = old })
			stdinReader = bufio.NewReader(io.MultiReader(strings.NewReader("1\n2\n"), &nodeMenuBeforeRead{
				reader: strings.NewReader("y\n"),
				run: func() {
					concurrent = cloneStore(before)
					switch mutation {
					case "nodes reordered":
						concurrent.Nodes[0], concurrent.Nodes[1] = concurrent.Nodes[1], concurrent.Nodes[0]
					case "node identity changed":
						concurrent.Nodes[1].RawURL = "trojan://different@changed.example:443"
					case "subscriptions reordered":
						concurrent.Subscriptions[0], concurrent.Subscriptions[1] = concurrent.Subscriptions[1], concurrent.Subscriptions[0]
					case "selection changed":
						concurrent.DefaultNodeID = "legacy-two"
					}
					if err := a.saveStore(concurrent); err != nil {
						t.Fatal(err)
					}
				},
			}))
			if err := a.adoptSubscriptionNodesMenu(); err == nil || !strings.Contains(err.Error(), "发生变化") {
				t.Fatalf("stale confirmation accepted: %v", err)
			}
			assertSubscriptionStoreUnchanged(t, a, concurrent)
		})
	}
}

func TestSubscriptionAdoptionReplacesSelectedAndPreservesOtherSourceNodes(t *testing.T) {
	requireAdoptionTestRoot(t)
	a, _ := adoptionFixture(t)
	if err := a.adoptSubscriptionNodes("1", []string{"legacy-one", "legacy-two"}); err != nil {
		t.Fatal(err)
	}
	refresh := func(raw string) (preparedSubscription, error) {
		return prepareUpdateBody(t, raw, "trojan://secret@fresh.example:443"), nil
	}
	if err := a.updateSubscriptionsWithDownloader("1", refresh); err != nil {
		t.Fatal(err)
	}
	got, err := a.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	fresh := got.findNodeByURL("trojan://secret@fresh.example:443")
	shared := got.findNode("legacy-two")
	if got.findNode("legacy-one") != nil || fresh == nil || got.SceneNodes[SceneTelegram] != fresh.ID {
		t.Fatal("adopted withdrawn selected node was not replaced during refresh")
	}
	if _, exists := got.SpeedResults["legacy-one"]; exists {
		t.Fatal("withdrawn adopted node retained stale speed data")
	}
	if shared == nil || !slices.Equal(shared.SubscriptionIDs, []string{subscriptionID(reconcileSourceB)}) {
		t.Fatal("adopted node was removed while another source still owned it")
	}
	if err := a.updateSubscriptionsWithDownloader("--all", refresh); err != nil {
		t.Fatal(err)
	}
	got, err = a.loadStore()
	if err != nil || got.findNode("legacy-one") != nil || got.findNode("legacy-two") != nil {
		t.Fatalf("withdrawn adopted nodes survived after refreshing their last source: %v", err)
	}
	if _, exists := got.SpeedResults["legacy-one"]; exists {
		t.Fatal("deleted adopted node's speed result remains")
	}
}
