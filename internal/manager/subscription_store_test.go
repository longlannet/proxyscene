package manager

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func subscriptionStoreFixture() *Store {
	st := newStore()
	st.Subscriptions = []string{"https://first.example/subscription", "https://second.example/subscription"}
	st.Nodes = []Node{{
		ID: "node-one", Name: "one", Protocol: "trojan", RawURL: "trojan://secret@example.com:443",
		SubscriptionIDs:     []string{subscriptionID(st.Subscriptions[0]), subscriptionID(st.Subscriptions[1])},
		SubscriptionManaged: true,
	}}
	return st
}

func TestSubscriptionStoreLegacyJSONKeepsNodesUnmanaged(t *testing.T) {
	data := []byte(`{
		"nodes": [{"id":"node-legacy","name":"legacy","protocol":"trojan","raw_url":"trojan://secret@example.com:443"}],
		"subscriptions": ["https://first.example/subscription"]
	}`)
	st, _, err := decodeStore(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Nodes) != 1 || st.Nodes[0].SubscriptionManaged || len(st.Nodes[0].SubscriptionIDs) != 0 {
		t.Fatalf("legacy node was assigned subscription ownership: %+v", st.Nodes)
	}
	encoded, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "subscription_ids") || strings.Contains(string(encoded), "subscription_managed") {
		t.Fatalf("legacy ownership zero values were persisted: %s", encoded)
	}
}

func TestSubscriptionStoreRoundTripMultipleSources(t *testing.T) {
	for _, managed := range []bool{true, false} {
		t.Run(fmt.Sprintf("managed=%t", managed), func(t *testing.T) {
			st := subscriptionStoreFixture()
			st.Nodes[0].SubscriptionManaged = managed
			data, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			got, _, err := decodeStore(data)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, st) {
				t.Fatalf("subscription metadata changed in round trip:\ngot  %+v\nwant %+v", got.Nodes, st.Nodes)
			}
		})
	}
}

func TestSubscriptionStoreAcceptsRetainedManagedNodeWithoutSources(t *testing.T) {
	st := subscriptionStoreFixture()
	st.Nodes[0].SubscriptionIDs = nil
	if _, err := validateStoreSemantics(st); err != nil {
		t.Fatalf("retained node without an active subscription source rejected: %v", err)
	}
}

func TestSubscriptionStoreRejectsInvalidSources(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Store)
		want   string
	}{
		"unknown": {
			mutate: func(st *Store) {
				st.Nodes[0].SubscriptionIDs[0] = subscriptionID("https://unknown.example/subscription")
			},
			want: "未知或重复订阅来源",
		},
		"empty": {
			mutate: func(st *Store) { st.Nodes[0].SubscriptionIDs[0] = "" },
			want:   "未知或重复订阅来源",
		},
		"raw URL": {
			mutate: func(st *Store) { st.Nodes[0].SubscriptionIDs[0] = st.Subscriptions[0] },
			want:   "未知或重复订阅来源",
		},
		"duplicate": {
			mutate: func(st *Store) { st.Nodes[0].SubscriptionIDs[1] = st.Nodes[0].SubscriptionIDs[0] },
			want:   "未知或重复订阅来源",
		},
		"too many": {
			mutate: func(st *Store) { st.Nodes[0].SubscriptionIDs = make([]string, maxSubscriptions+1) },
			want:   "订阅来源数超过上限",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st := subscriptionStoreFixture()
			tc.mutate(st)
			data, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := decodeStore(data); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected source validation failure containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSubscriptionStoreAcceptsMaximumSources(t *testing.T) {
	st := subscriptionStoreFixture()
	st.Subscriptions = make([]string, maxSubscriptions)
	st.Nodes[0].SubscriptionIDs = make([]string, maxSubscriptions)
	for i := range st.Subscriptions {
		st.Subscriptions[i] = fmt.Sprintf("https://example.com/subscription/%d", i)
		st.Nodes[0].SubscriptionIDs[i] = subscriptionID(st.Subscriptions[i])
	}
	if _, err := validateStoreSemantics(st); err != nil {
		t.Fatalf("maximum permitted sources rejected: %v", err)
	}
}

func TestSubscriptionStoreCloneAndRestoreDoNotAliasSources(t *testing.T) {
	original := subscriptionStoreFixture()
	firstSource := original.Nodes[0].SubscriptionIDs[0]
	snapshot := cloneStore(original)
	original.Nodes[0].SubscriptionIDs[0] = "changed-original"
	original.Nodes[0].SubscriptionManaged = false
	if snapshot.Nodes[0].SubscriptionIDs[0] != firstSource || !snapshot.Nodes[0].SubscriptionManaged {
		t.Fatalf("snapshot shares mutable source data: %+v", snapshot.Nodes[0])
	}

	restored := newStore()
	restoreStore(restored, snapshot)
	snapshot.Nodes[0].SubscriptionIDs[0] = "changed-snapshot"
	if restored.Nodes[0].SubscriptionIDs[0] != firstSource {
		t.Fatalf("restored state shares source data with snapshot: %+v", restored.Nodes[0])
	}
	restored.Nodes[0].SubscriptionIDs[1] = "changed-restored"
	if snapshot.Nodes[0].SubscriptionIDs[1] != subscriptionID(snapshot.Subscriptions[1]) {
		t.Fatalf("restored source mutation leaked into snapshot: %+v", snapshot.Nodes[0])
	}
}
