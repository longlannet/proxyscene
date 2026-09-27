package manager

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func subscriptionUpdateFixture(t *testing.T, urls ...string) (*App, *Store) {
	t.Helper()
	a := testApp(t)
	oldOwnership, oldHost, oldInstall := hostOwnershipPath, hostLockPath, installLockPath
	root := t.TempDir()
	hostOwnershipPath = filepath.Join(root, "host.json")
	hostLockPath = filepath.Join(root, "host.lock")
	installLockPath = filepath.Join(root, "install.lock")
	oldRun, oldOutput := systemctlRun, systemctlOutput
	t.Cleanup(func() {
		hostOwnershipPath, hostLockPath, installLockPath = oldOwnership, oldHost, oldInstall
		systemctlRun, systemctlOutput = oldRun, oldOutput
	})
	systemctlRun = func(string, ...string) error { t.Fatal("subscription update invoked systemctl"); return nil }
	systemctlOutput = func(string, ...string) (string, error) {
		t.Fatal("subscription update queried systemctl")
		return "", nil
	}
	st := newStore()
	st.RuntimeConfig = a.cfg.runtimeConfig()
	st.Subscriptions = urls
	st.Nodes = []Node{{ID: "selected", Name: "manual", Protocol: "trojan", RawURL: "trojan://secret@selected.example:443"}}
	st.DefaultNodeID = "selected"
	st.SceneEnabled[SceneGlobal] = true
	if err := a.saveStore(st); err != nil {
		t.Fatal(err)
	}
	return a, st
}

func prepareUpdateBody(t *testing.T, raw, body string) preparedSubscription {
	t.Helper()
	prepared, err := prepareSubscriptionBody(raw, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func assertSubscriptionStoreUnchanged(t *testing.T, a *App, before *Store) {
	t.Helper()
	got, err := a.loadStore()
	if err != nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("state changed: got=%+v err=%v", got, err)
	}
	backup, err := a.loadStoreBackup()
	if err != nil || !reflect.DeepEqual(backup, before) {
		t.Fatalf("backup changed: got=%+v err=%v", backup, err)
	}
}

func TestSubscriptionUpdateAllCommitsOnceWithoutRestart(t *testing.T) {
	urls := []string{"https://one.example/sub?token=one", "https://two.example/sub?token=two"}
	a, before := subscriptionUpdateFixture(t, urls...)
	calls := 0
	err := a.updateSubscriptionsWithDownloader("--all", func(raw string) (preparedSubscription, error) {
		calls++
		assertSubscriptionStoreUnchanged(t, a, before)
		return prepareUpdateBody(t, raw, "trojan://secret@"+subscriptionLabel(raw)+":443"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.loadStore()
	if err != nil || calls != 2 || len(got.Nodes) != 3 || got.Generation != before.Generation+1 || got.DefaultNodeID != "selected" {
		t.Fatalf("unexpected update: %+v calls=%d err=%v", got, calls, err)
	}
	for _, node := range got.Nodes[1:] {
		if !node.SubscriptionManaged || len(node.SubscriptionIDs) != 1 {
			t.Fatalf("new node lacks ownership: %+v", node)
		}
	}
}

func TestSubscriptionUpdateFailureLeavesEntireBatchUntouched(t *testing.T) {
	valid := "trojan://secret@new.example:443"
	for _, failure := range []string{"network", "empty", "bad URI", "unknown scheme", "HTML", "partial status", "partial header"} {
		t.Run(failure, func(t *testing.T) {
			urls := []string{"https://one.example/sub", "https://two.example/sub"}
			a, before := subscriptionUpdateFixture(t, urls...)
			before.Nodes = append(before.Nodes, Node{ID: "obsolete", Name: "obsolete", Protocol: "trojan", RawURL: "trojan://secret@old.example:443", SubscriptionManaged: true, SubscriptionIDs: []string{subscriptionID(urls[1])}})
			if err := a.saveStore(before); err != nil {
				t.Fatal(err)
			}
			err := a.updateSubscriptionsWithDownloader("--all", func(raw string) (preparedSubscription, error) {
				if raw == urls[0] {
					return prepareUpdateBody(t, raw, valid), nil
				}
				switch failure {
				case "network":
					return preparedSubscription{}, errors.New("download failed")
				case "empty":
					return prepareSubscriptionBody(raw, nil)
				case "bad URI":
					return prepareSubscriptionBody(raw, []byte(valid+"\ntrojan://invalid"))
				case "unknown scheme":
					return prepareSubscriptionBody(raw, []byte(valid+"\nhy2://secret@missing.example:443"))
				case "HTML":
					return prepareSubscriptionBody(raw, []byte("<html>\n"+valid+"\n</html>"))
				default:
					client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
						status, header := http.StatusOK, make(http.Header)
						if failure == "partial status" {
							status = http.StatusPartialContent
						} else {
							header.Set("Content-Range", "bytes 0-31/1000")
						}
						return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(valid))}, nil
					})}
					return downloadAndPrepareSubscriptionWithClient(raw, false, client)
				}
			})
			if err == nil {
				t.Fatal("incomplete update accepted")
			}
			assertSubscriptionStoreUnchanged(t, a, before)
		})
	}
}

func TestSubscriptionUpdateRejectsConcurrentMutation(t *testing.T) {
	a, before := subscriptionUpdateFixture(t, "https://one.example/sub")
	err := a.updateSubscriptionsWithDownloader("1", func(raw string) (preparedSubscription, error) {
		before.Nodes[0].Name = "concurrent rename"
		if err := a.saveStore(before); err != nil {
			t.Fatal(err)
		}
		return prepareUpdateBody(t, raw, "trojan://secret@new.example:443"), nil
	})
	if err == nil || !strings.Contains(err.Error(), "状态发生变化") {
		t.Fatalf("concurrent update not rejected: %v", err)
	}
	assertSubscriptionStoreUnchanged(t, a, before)
}

func TestSubscriptionUpdateSaveFailureRestoresSources(t *testing.T) {
	urls := []string{"https://one.example/sub", "https://two.example/sub"}
	a, before := subscriptionUpdateFixture(t, urls...)
	before.Nodes[0].SubscriptionIDs = []string{subscriptionID(urls[0]), subscriptionID(urls[1])}
	if err := a.saveStore(before); err != nil {
		t.Fatal(err)
	}
	oldWrite := runtimeWriteStoreCAS
	t.Cleanup(func() { runtimeWriteStoreCAS = oldWrite })
	failed := false
	runtimeWriteStoreCAS = func(path string, expected runtimeStoreEvidence, data []byte) error {
		if path == a.cfg.StorePath() && !failed {
			failed = true
			return errors.New("injected main write failure")
		}
		return oldWrite(path, expected, data)
	}
	err := a.updateSubscriptionsWithDownloader("1", func(raw string) (preparedSubscription, error) {
		return prepareUpdateBody(t, raw, "trojan://secret@new.example:443"), nil
	})
	if err == nil || !failed {
		t.Fatalf("save failure not exercised: %v", err)
	}
	before.Generation += 2 // the failed candidate and its compensation use distinct generations
	assertSubscriptionStoreUnchanged(t, a, before)
}

func TestSubscriptionUpdatePreservesSavedURLIdentity(t *testing.T) {
	original := "HTTPS://one.example/sub"
	a, _ := subscriptionUpdateFixture(t, original)
	err := a.updateSubscriptionsWithDownloader("1", func(raw string) (preparedSubscription, error) {
		return prepareUpdateBody(t, strings.ToLower(raw), "trojan://secret@new.example:443"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.loadStore()
	if err != nil || len(got.Subscriptions) != 1 || got.Subscriptions[0] != original || got.Nodes[1].SubscriptionIDs[0] != subscriptionID(original) {
		t.Fatalf("saved identity changed: %+v err=%v", got, err)
	}
}

func TestSubscriptionUpdateBoundsAggregateEntries(t *testing.T) {
	urls := []string{"https://one.example/sub", "https://two.example/sub", "https://three.example/sub"}
	a, before := subscriptionUpdateFixture(t, urls...)
	err := a.updateSubscriptionsWithDownloader("--all", func(raw string) (preparedSubscription, error) {
		return prepareUpdateBody(t, raw, strings.Repeat("trojan://secret@new.example:443\n", maxSubscriptionNodes)), nil
	})
	if err == nil || !strings.Contains(err.Error(), "条目总数") {
		t.Fatalf("aggregate limit not enforced: %v", err)
	}
	assertSubscriptionStoreUnchanged(t, a, before)
}

func TestSubscriptionRefreshRequiresCompletePlainOrBase64List(t *testing.T) {
	plain := "trojan://secret@one.example:443#one\r\n\ttrojan://secret@two.example:443#two\n"
	for _, body := range []string{plain, base64.StdEncoding.EncodeToString([]byte(plain)), base64.RawURLEncoding.EncodeToString([]byte(plain))} {
		prepared := prepareUpdateBody(t, "https://one.example/sub", body)
		if err := validateSubscriptionRefresh(prepared); err != nil || len(prepared.Nodes) != 2 {
			t.Fatalf("complete list rejected: %v", err)
		}
	}
	for _, body := range []string{"\"trojan://secret@one.example:443\"", plain + "truncated", plain + "\ntrojan://invalid", "prefix " + plain} {
		prepared := prepareUpdateBody(t, "https://one.example/sub", body)
		if err := validateSubscriptionRefresh(prepared); err == nil {
			t.Fatal("incomplete list accepted for refresh")
		}
	}
}

func TestSubscriptionDownloadContextDeadline(t *testing.T) {
	const secretURL = "https://user:password@one.example/private?token=SECRET"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	_, err := downloadAndPrepareSubscriptionWithContext(ctx, secretURL, false, client)
	if err == nil || ctx.Err() == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "password") {
		t.Fatalf("deadline/redaction failed: %v", err)
	}
}

func TestSubscriptionListAndSelectorHideCredentials(t *testing.T) {
	const secretURL = "https://user:password@one.example/private-path?token=SECRET#fragment"
	st := newStore()
	st.Subscriptions = []string{secretURL, "https://two.example/sub"}
	st.Nodes = []Node{{SubscriptionIDs: []string{subscriptionID(secretURL)}}}
	var out bytes.Buffer
	writeSubscriptionList(&out, st)
	for _, secret := range []string{"user", "password", "private-path", "SECRET", "fragment"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("list leaked %s", secret)
		}
	}
	if !strings.Contains(out.String(), "one.example") || !strings.Contains(out.String(), "关联节点：1") {
		t.Fatalf("list lacks useful identity/count: %s", &out)
	}
	for _, selector := range []string{"1", subscriptionID(secretURL), subscriptionID(secretURL)[:16]} {
		got, err := selectSubscriptionURLs(st, selector)
		if err != nil || !reflect.DeepEqual(got, []string{secretURL}) {
			t.Fatalf("selector %q failed: %v", selector, err)
		}
	}
	for _, selector := range []string{secretURL, "0", "3", "all", "sub-short", "sub-gggggggggggg", "sub-000000000000"} {
		_, err := selectSubscriptionURLs(st, selector)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("bad selector accepted or leaked: %v", err)
		}
	}
}

func TestSubscriptionCommandsImportThenRefreshThroughHTTP(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("mutating CLI requires root; store and update transaction tests run without root")
	}
	t.Setenv("PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION", "1")
	t.Setenv("PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION", "1")
	a, _ := subscriptionUpdateFixture(t)
	var body atomic.Value
	body.Store("trojan://secret@kept.example:443#kept\ntrojan://secret@removed.example:443#removed")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "private-token" {
			t.Error("subscription credential not preserved in request")
		}
		fmt.Fprint(w, body.Load().(string))
	}))
	defer server.Close()
	raw := server.URL + "/subscription?token=private-token"
	oldInput := stdinReader
	t.Cleanup(func() { stdinReader = oldInput })
	stdinReader = bufio.NewReader(strings.NewReader(raw + "\n"))
	if err := a.nodeCommand([]string{"import", "--stdin"}); err != nil {
		t.Fatal(err)
	}
	imported, err := a.loadStore()
	if err != nil || len(imported.Nodes) != 3 || len(imported.Subscriptions) != 1 {
		t.Fatalf("CLI import failed: %+v %v", imported, err)
	}
	keptID := imported.Nodes[1].ID
	if err := a.subscriptionCommand([]string{"list"}); err != nil {
		t.Fatal(err)
	}
	body.Store("trojan://secret@kept.example:443#kept\ntrojan://secret@added.example:443#added")
	if err := a.subscriptionCommand([]string{"update", "1"}); err != nil {
		t.Fatal(err)
	}
	got, err := a.loadStore()
	if err != nil || len(got.Nodes) != 3 || got.findNode(keptID) == nil || got.findNode(imported.Nodes[2].ID) != nil {
		t.Fatalf("CLI refresh failed: %+v %v", got, err)
	}
	if err := a.subscriptionCommand([]string{"update", "--all"}); err != nil {
		t.Fatal(err)
	}
	stdinReader = bufio.NewReader(strings.NewReader("0\n"))
	if err := a.subscriptionCommand(nil); err != nil {
		t.Fatal(err)
	}
	stdinReader = bufio.NewReader(strings.NewReader("0\n"))
	if err := a.nodeCommand(nil); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionCommandRejectsUnsupportedArguments(t *testing.T) {
	a, before := subscriptionUpdateFixture(t, "https://one.example/sub")
	for _, args := range [][]string{{"list", "extra"}, {"update"}, {"update", "1", "extra"}, {"SECRET"}} {
		if err := a.subscriptionCommand(args); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("invalid command accepted or leaked: %v", err)
		}
	}
	assertSubscriptionStoreUnchanged(t, a, before)
}
