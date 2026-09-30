package manager

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func subscriptionMenuInputForTest(t *testing.T, input string) {
	t.Helper()
	old := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader(input))
	t.Cleanup(func() { stdinReader = old })
}

func TestMenuSubscriptionCancellationLeavesStateUntouched(t *testing.T) {
	for _, input := range []string{"0\n", "q\n", "1\nq\n0\n", "2\nq\n0\n", "3\nn\n0\n", "3\n\n0\n", "3\nq\n0\n", "4\nq\n0\n", "4\n1\nq\n0\n", "4\n1\n1\nn\n0\n", "invalid\n0\n"} {
		t.Run(input, func(t *testing.T) {
			a, before := subscriptionUpdateFixture(t, "https://one.example/sub?token=SECRET")
			subscriptionMenuInputForTest(t, input)
			if err := a.subscriptionMenu(); err != nil {
				t.Fatal(err)
			}
			assertSubscriptionStoreUnchanged(t, a, before)
		})
	}
}

func TestMenuSubscriptionEOFStopsEveryPrompt(t *testing.T) {
	for _, input := range []string{"", "1", "1\n", "2\n", "3\n", "3\ny", "4\n", "4\n1\n", "4\n1\n1\n"} {
		t.Run(input, func(t *testing.T) {
			a, before := subscriptionUpdateFixture(t, "https://one.example/sub?token=SECRET")
			subscriptionMenuInputForTest(t, input)
			if err := a.subscriptionMenu(); !errors.Is(err, errMenuClosed) {
				t.Fatalf("EOF must stop menu: %v", err)
			}
			assertSubscriptionStoreUnchanged(t, a, before)
		})
	}
}

func TestMenuSubscriptionSelectionBindsDisplayedIdentity(t *testing.T) {
	one, two := "https://one.example/sub", "https://two.example/sub"
	a, displayed := subscriptionUpdateFixture(t, one, two)
	selected, err := subscriptionMenuID(displayed, "1")
	if err != nil || selected != subscriptionID(one) {
		t.Fatalf("displayed selection was not bound: %q %v", selected, err)
	}
	current, err := a.loadStore()
	if err != nil {
		t.Fatal(err)
	}
	current.Subscriptions = []string{two, one}
	if err := a.saveStore(current); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := a.updateSubscriptionsWithDownloader(selected, func(raw string) (preparedSubscription, error) {
		calls++
		if raw != one {
			t.Fatalf("reordered list selected wrong subscription: %s", raw)
		}
		return prepareUpdateBody(t, raw, "trojan://secret@fresh.example:443"), nil
	}); err != nil || calls != 1 {
		t.Fatalf("bound update failed: calls=%d err=%v", calls, err)
	}
	for _, selector := range []string{"--all", "0", "3", "https://one.example/sub"} {
		if _, err := subscriptionMenuID(displayed, selector); err == nil {
			t.Fatalf("invalid single selection accepted: %s", selector)
		}
	}
	removed := newStore()
	removed.Subscriptions = []string{two}
	if _, err := selectSubscriptionURLs(removed, selected); err == nil {
		t.Fatal("removed displayed subscription silently fell back to another source")
	}
}

func TestMenuSubscriptionImportAndRefreshUseLocalFixtureOnly(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("menu mutations require root; all cancellation tests run without root")
	}
	t.Setenv("PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION", "1")
	t.Setenv("PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION", "1")
	a, _ := subscriptionUpdateFixture(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("token") != "SECRET" {
			t.Error("credential missing from subscription request")
		}
		fmt.Fprintln(w, "trojan://secret@fresh.example:443#fresh")
	}))
	defer server.Close()
	url := server.URL + "/sub?token=SECRET"
	subscriptionMenuInputForTest(t, "1\n"+url+"\n2\n1\n3\ny\n0\n")
	if err := a.subscriptionMenu(); err != nil {
		t.Fatal(err)
	}
	st, err := a.loadStore()
	if err != nil || !reflect.DeepEqual(st.Subscriptions, []string{url}) || len(st.Nodes) != 2 || st.DefaultNodeID != "selected" || requests.Load() != 3 {
		t.Fatalf("import/single/all route failed: requests=%d state=%+v err=%v", requests.Load(), st, err)
	}
}
