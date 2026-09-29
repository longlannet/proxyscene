package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type updateMirrorFixture struct {
	updateReleaseFixture
	index    map[string]any
	metadata map[string]any
}

func newUpdateMirrorFixture(t *testing.T) updateMirrorFixture {
	t.Helper()
	base := newUpdateReleaseFixture(t)
	base.release.Source = "mirror"
	assets := map[string]any{}
	for name, asset := range base.release.Assets {
		asset.URL = ""
		base.release.Assets[name] = asset
		assets[name] = map[string]any{"sha256": asset.SHA256, "size": asset.Size}
	}
	return updateMirrorFixture{
		updateReleaseFixture: base,
		index: map[string]any{
			"version": "0.9.2", "tag": base.release.Tag, "base_url": updateMirrorBase + "/" + base.release.Tag,
			"commit": base.release.Commit, "release_id": base.release.ID, "published_at": base.release.Published,
		},
		metadata: map[string]any{
			"schema_version": 1, "version": "0.9.2", "tag": base.release.Tag, "commit": base.release.Commit,
			"release_id": base.release.ID, "published_at": base.release.Published, "notes": base.release.Notes, "assets": assets,
		},
	}
}

func (f updateMirrorFixture) client(t *testing.T, requests *[]string) *updateClient {
	t.Helper()
	return &updateClient{http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if requests != nil {
			*requests = append(*requests, req.URL.String())
		}
		if req.URL.Host != "dl.ll.cd" || req.Method != http.MethodGet || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Fatalf("mirror-only update reached a forbidden source or authentication: %s", req.URL.String())
		}
		var value any
		switch req.URL.String() {
		case updateMirrorBase + "/latest.json":
			value = f.index
		case updateMirrorBase + "/metadata/" + f.release.Tag + ".json":
			value = f.metadata
		default:
			for name, data := range f.payloads {
				if req.URL.String() == updateMirrorBase+"/"+f.release.Tag+"/"+name {
					return updateTestResponse(http.StatusOK, data), nil
				}
			}
			t.Fatalf("unexpected mirror request: %s", req.URL.String())
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return updateTestResponse(http.StatusOK, data), nil
	})}}
}

func TestUpdateMirrorDefaultCheckNeverRequestsGitHub(t *testing.T) {
	f := newUpdateMirrorFixture(t)
	setUpdateTestVersion(t, "0.9.1", strings.Repeat("a", 40))
	options, err := parseUpdateOptions([]string{"--check"})
	if err != nil {
		t.Fatal(err)
	}
	var requests []string
	output, err := captureMainMenuTestOutput(t, func() error {
		attempted, err := (&App{}).runUpdateWithClient(options, f.client(t, &requests))
		if attempted {
			t.Fatal("check-only update attempted installation")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{updateMirrorBase + "/latest.json", updateMirrorBase + "/metadata/v0.9.2.json"}
	if !reflect.DeepEqual(requests, want) || !strings.Contains(output, f.release.Notes) || strings.Contains(output, "github.com") || !strings.Contains(output, "dl.ll.cd") {
		t.Fatalf("check did not use mirror metadata and notes: requests=%v output=%q", requests, output)
	}
}

func TestUpdateMirrorDownloadAndRevalidationUsePinnedVersion(t *testing.T) {
	f := newUpdateMirrorFixture(t)
	var requests []string
	client := f.client(t, &requests)
	release, err := client.latestForSource(context.Background(), "mirror")
	if err != nil || !reflect.DeepEqual(release, f.release) {
		t.Fatalf("mirror identity mismatch: got=%+v err=%v", release, err)
	}
	// Publication may advance Latest once this installation has selected a
	// complete release. Revalidation must use the selected version's metadata.
	f.index["tag"], f.index["version"] = "v99.0.0", "99.0.0"
	dest := filepath.Join(t.TempDir(), "bundle")
	if err := client.downloadBundle(context.Background(), release, "amd64", "mirror", dest); err != nil {
		t.Fatal(err)
	}
	if err := client.revalidate(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(data, f.payloads["proxyscene_bundle_linux_amd64.tar.gz"]) {
		t.Fatalf("downloaded bundle mismatch: %v", err)
	}
	want := []string{
		updateMirrorBase + "/latest.json", updateMirrorBase + "/metadata/v0.9.2.json",
		updateMirrorBase + "/v0.9.2/checksums.txt", updateMirrorBase + "/v0.9.2/proxyscene_bundle_linux_amd64.tar.gz",
		updateMirrorBase + "/metadata/v0.9.2.json",
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("update contacted an unexpected endpoint: %v", requests)
	}
}

func TestUpdateMirrorMetadataFailsClosed(t *testing.T) {
	asset := func(f *updateMirrorFixture, name string) map[string]any {
		return f.metadata["assets"].(map[string]any)[name].(map[string]any)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*updateMirrorFixture)
	}{
		{"index foreign base", func(f *updateMirrorFixture) { f.index["base_url"] = "https://evil.invalid/v0.9.2" }},
		{"index wrong version", func(f *updateMirrorFixture) { f.index["version"] = "0.9.3" }},
		{"index invalid tag", func(f *updateMirrorFixture) { f.index["tag"] = "../latest" }},
		{"index mismatch commit", func(f *updateMirrorFixture) { f.index["commit"] = strings.Repeat("a", 40) }},
		{"index mismatch release", func(f *updateMirrorFixture) { f.index["release_id"] = 123 }},
		{"index mismatch published", func(f *updateMirrorFixture) { f.index["published_at"] = "2026-01-02T00:00:00Z" }},
		{"index extra URL", func(f *updateMirrorFixture) { f.index["metadata_url"] = "https://evil.invalid/metadata" }},
		{"index missing field", func(f *updateMirrorFixture) { delete(f.index, "base_url") }},
		{"index null field", func(f *updateMirrorFixture) { f.index["commit"] = nil }},
		{"schema unsupported", func(f *updateMirrorFixture) { f.metadata["schema_version"] = 2 }},
		{"schema boolean", func(f *updateMirrorFixture) { f.metadata["schema_version"] = true }},
		{"metadata wrong tag", func(f *updateMirrorFixture) { f.metadata["tag"] = "v0.9.3" }},
		{"metadata wrong version", func(f *updateMirrorFixture) { f.metadata["version"] = "0.9.3" }},
		{"metadata invalid commit", func(f *updateMirrorFixture) { f.metadata["commit"] = strings.Repeat("A", 40) }},
		{"metadata negative release", func(f *updateMirrorFixture) { f.metadata["release_id"] = -1 }},
		{"metadata float release", func(f *updateMirrorFixture) { f.metadata["release_id"] = 1.5 }},
		{"metadata future publication", func(f *updateMirrorFixture) { f.metadata["published_at"] = "9999-01-01T00:00:00Z" }},
		{"metadata date invalid", func(f *updateMirrorFixture) { f.metadata["published_at"] = "2026-02-30T00:00:00Z" }},
		{"metadata missing notes", func(f *updateMirrorFixture) { delete(f.metadata, "notes") }},
		{"metadata null notes", func(f *updateMirrorFixture) { f.metadata["notes"] = nil }},
		{"metadata oversized notes", func(f *updateMirrorFixture) { f.metadata["notes"] = strings.Repeat("x", (64<<10)+1) }},
		{"metadata extra URL", func(f *updateMirrorFixture) { f.metadata["url"] = "https://evil.invalid" }},
		{"metadata noncanonical key", func(f *updateMirrorFixture) { f.metadata["Tag"] = f.metadata["tag"]; delete(f.metadata, "tag") }},
		{"asset missing", func(f *updateMirrorFixture) { delete(f.metadata["assets"].(map[string]any), "install.sh") }},
		{"asset extra", func(f *updateMirrorFixture) {
			f.metadata["assets"].(map[string]any)["extra.sh"] = asset(f, "install.sh")
		}},
		{"asset URL forbidden", func(f *updateMirrorFixture) { asset(f, "install.sh")["url"] = "https://evil.invalid" }},
		{"asset missing sha", func(f *updateMirrorFixture) { delete(asset(f, "install.sh"), "sha256") }},
		{"asset uppercase sha", func(f *updateMirrorFixture) { asset(f, "install.sh")["sha256"] = strings.Repeat("A", 64) }},
		{"asset key case", func(f *updateMirrorFixture) {
			a := asset(f, "install.sh")
			a["SHA256"] = a["sha256"]
			delete(a, "sha256")
		}},
		{"asset null sha", func(f *updateMirrorFixture) { asset(f, "install.sh")["sha256"] = nil }},
		{"asset boolean size", func(f *updateMirrorFixture) { asset(f, "install.sh")["size"] = true }},
		{"asset zero size", func(f *updateMirrorFixture) { asset(f, "install.sh")["size"] = 0 }},
		{"asset metadata oversized", func(f *updateMirrorFixture) { asset(f, "install.sh")["size"] = updateMaxMetadataBytes + 1 }},
		{"asset bundle oversized", func(f *updateMirrorFixture) {
			asset(f, "proxyscene_bundle_linux_amd64.tar.gz")["size"] = updateMaxAssetBytes + 1
		}},
		{"assets total oversized", func(f *updateMirrorFixture) {
			for _, arch := range []string{"amd64", "arm64", "386"} {
				asset(f, "proxyscene_bundle_linux_"+arch+".tar.gz")["size"] = updateMaxAssetBytes
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateMirrorFixture(t)
			tc.mutate(&f)
			if _, err := f.client(t, nil).latestForSource(context.Background(), "mirror"); err == nil {
				t.Fatal("accepted invalid mirror metadata")
			}
		})
	}
}

func TestUpdateMirrorRejectsMetadataAndNetworkFailureWithoutFallback(t *testing.T) {
	for _, endpoint := range []string{"/latest.json", "/metadata/v0.9.2.json"} {
		for _, tc := range []struct {
			name string
			raw  string
			code int
		}{
			{"missing", "", http.StatusNotFound},
			{"error", "", http.StatusInternalServerError},
			{"malformed", "{broken}", http.StatusOK},
			{"duplicate", `{"tag":"v0.9.2","TAG":"v0.9.3"}`, http.StatusOK},
			{"extra JSON", "{} {}", http.StatusOK},
			{"null", "null", http.StatusOK},
			{"oversized", strings.Repeat(" ", int(updateMaxMetadataBytes)+1), http.StatusOK},
			{"invalid UTF8", "{\"notes\":\"\xff\"}", http.StatusOK},
		} {
			t.Run(endpoint+tc.name, func(t *testing.T) {
				f := newUpdateMirrorFixture(t)
				client := f.client(t, nil)
				transport := client.http.Transport
				client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.String() == updateMirrorBase+endpoint {
						resp := updateTestResponse(tc.code, []byte(tc.raw))
						resp.ContentLength = -1
						return resp, nil
					}
					return transport.RoundTrip(req)
				})
				if _, err := client.latestForSource(context.Background(), "mirror"); err == nil {
					t.Fatal("accepted invalid or missing mirror metadata")
				}
			})
		}
	}
}

func TestUpdateMirrorRevalidationDetectsDrift(t *testing.T) {
	for _, field := range []string{"commit", "release_id", "notes", "assets"} {
		t.Run(field, func(t *testing.T) {
			f := newUpdateMirrorFixture(t)
			client := f.client(t, nil)
			release, err := client.latestForSource(context.Background(), "mirror")
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "commit":
				f.metadata[field] = strings.Repeat("a", 40)
			case "release_id":
				f.metadata[field] = 99
			case "notes":
				f.metadata[field] = "replacement notes"
			case "assets":
				f.metadata[field].(map[string]any)["install.sh"].(map[string]any)["sha256"] = strings.Repeat("a", 64)
			}
			if err := client.revalidate(context.Background(), release); err == nil {
				t.Fatal("accepted changed mirror release")
			}
		})
	}
}

func TestUpdateMirrorRejectsCorruptDownloadsAndMixedSources(t *testing.T) {
	for _, corrupt := range []string{"checksums.txt", "proxyscene_bundle_linux_amd64.tar.gz", "mixed source"} {
		t.Run(corrupt, func(t *testing.T) {
			f := newUpdateMirrorFixture(t)
			var requests []string
			client := f.client(t, &requests)
			release, err := client.latestForSource(context.Background(), "mirror")
			if err != nil {
				t.Fatal(err)
			}
			source := "mirror"
			if corrupt == "mixed source" {
				source = "github"
			} else {
				f.payloads[corrupt][0] ^= 1
			}
			dest := filepath.Join(t.TempDir(), "bundle")
			if err := client.downloadBundle(context.Background(), release, "amd64", source, dest); err == nil {
				t.Fatal("accepted corrupt or mixed-source update")
			}
			if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed update left temporary bundle")
			}
			if corrupt == "mixed source" && len(requests) != 2 {
				t.Fatalf("mixed source downloaded before rejection: %v", requests)
			}
		})
	}
}

func TestUpdateExplicitGitHubCheckPreservesSource(t *testing.T) {
	f := newUpdateReleaseFixture(t)
	setUpdateTestVersion(t, "0.9.1", strings.Repeat("a", 40))
	options, err := parseUpdateOptions([]string{"--check", "--source", "github"})
	if err != nil {
		t.Fatal(err)
	}
	var requests []string
	output, err := captureMainMenuTestOutput(t, func() error {
		_, err := (&App{}).runUpdateWithClient(options, f.client(t, &requests))
		return err
	})
	if err != nil || len(requests) != 2 || !strings.HasPrefix(requests[0], updateAPIBase+"/") ||
		!strings.Contains(output, "sudo proxyscene update --source github") || !strings.Contains(output, "https://github.com/"+updateRepository) {
		t.Fatalf("explicit GitHub update changed source: requests=%v output=%q err=%v", requests, output, err)
	}
}

func TestUpdateJSONRejectsInvalidUnicodeWithoutChangingValidNotes(t *testing.T) {
	for _, raw := range []string{`{"notes":"\ud800"}`, `{"notes":"\udc00"}`, `{"notes":"\ud800abc"}`, `{"notes":"\ud800\u1234"}`} {
		if err := validateUpdateJSON([]byte(raw)); err == nil {
			t.Fatalf("accepted unpaired surrogate: %s", raw)
		}
	}
	for _, raw := range []string{`{"notes":"说明"}`, `{"notes":"\ud83d\ude03"}`, `{"notes":"\\ud800"}`, `{"notes":"\ufffd"}`} {
		if err := validateUpdateJSON([]byte(raw)); err != nil {
			t.Fatalf("rejected valid notes: %s: %v", raw, err)
		}
	}
}
