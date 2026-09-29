package manager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestCompareUpdateVersions(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v0.9.2", "v0.9.2", 0},
		{"v0.9.2", "v0.10.0", -1},
		{"v1.0.0", "v0.99.99", 1},
		{"v0.0.0", "v0.0.1", -1},
		{"v999999999999999999999.0.0", "v1000000000000000000000.0.0", -1},
	} {
		t.Run(tc.a+"_"+tc.b, func(t *testing.T) {
			got, err := compareUpdateVersions(tc.a, tc.b)
			if err != nil || got != tc.want {
				t.Fatalf("compare = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "dev", "0.9.2", "v01.2.3", "v1.02.3", "v1.2.03", "v1.2", "v1.2.3.4", "v1.2.3-rc.1", "v1.2.3+build", "v1.2.3\n", "v-1.2.3", "v1/2/3", "v" + strings.Repeat("9", 125) + ".0.0"} {
		if _, err := compareUpdateVersions(bad, "v0.9.2"); err == nil {
			t.Errorf("accepted invalid current version %q", bad)
		}
		if _, err := compareUpdateVersions("v0.9.2", bad); err == nil {
			t.Errorf("accepted invalid target version %q", bad)
		}
	}
}

type updateReleaseFixture struct {
	meta      map[string]any
	reference map[string]any
	release   updateRelease
	payloads  map[string][]byte
}

func newUpdateReleaseFixture(t *testing.T) updateReleaseFixture {
	t.Helper()
	const tag = "v0.9.2"
	const commit = "7823daede06af7647a51ce199b35ddb57d25dfbc"
	r := updateRelease{Tag: tag, Commit: commit, ID: 42, Published: "2026-01-01T00:00:00Z", Source: "github", Notes: "Fixed Hermes compatibility.", Assets: map[string]updateAsset{}}
	payloads := map[string][]byte{}
	names := []string{"install.sh", "xray_source_v26.9.9.tar.gz"}
	for _, arch := range []string{"amd64", "arm64", "386", "armv7"} {
		names = append(names, "proxyscene_linux_"+arch+".tar.gz", "proxyscene_bundle_linux_"+arch+".tar.gz")
	}
	sort.Strings(names)
	var manifest strings.Builder
	for _, name := range names {
		payloads[name] = []byte("test file: " + name + "\n")
		hash := sha256.Sum256(payloads[name])
		r.Assets[name] = updateAsset{Name: name, Size: int64(len(payloads[name])), SHA256: hex.EncodeToString(hash[:]), URL: updateDownloadBase + "/" + tag + "/" + name}
		fmt.Fprintf(&manifest, "%x  %s\n", hash, name)
	}
	payloads["checksums.txt"] = []byte(manifest.String())
	hash := sha256.Sum256(payloads["checksums.txt"])
	r.Assets["checksums.txt"] = updateAsset{Name: "checksums.txt", Size: int64(len(payloads["checksums.txt"])), SHA256: hex.EncodeToString(hash[:]), URL: updateDownloadBase + "/" + tag + "/checksums.txt"}
	names = append(names, "checksums.txt")
	assets := make([]any, 0, len(names))
	for i, name := range names {
		a := r.Assets[name]
		assets = append(assets, map[string]any{
			"id": int64(i + 1), "name": a.Name, "size": a.Size,
			"digest": "sha256:" + a.SHA256, "browser_download_url": a.URL,
			"url": updateAPIBase + "/releases/assets/" + strconv.Itoa(i+1), "state": "uploaded",
		})
	}
	return updateReleaseFixture{
		release: r, payloads: payloads,
		meta: map[string]any{
			"id": r.ID, "tag_name": r.Tag, "body": r.Notes, "immutable": true, "draft": false, "prerelease": false,
			"published_at": "2026-01-01T00:00:00Z", "url": updateAPIBase + "/releases/42",
			"html_url": "https://github.com/" + updateRepository + "/releases/tag/" + tag, "assets": assets,
		},
		reference: map[string]any{
			"ref": "refs/tags/" + tag, "url": updateAPIBase + "/git/refs/tags/" + tag,
			"object": map[string]any{"type": "commit", "sha": commit, "url": updateAPIBase + "/git/commits/" + commit},
		},
	}
}

func updateTestResponse(status int, data []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data))}
}

func (f updateReleaseFixture) client(t *testing.T, requests *[]string) *updateClient {
	t.Helper()
	return &updateClient{http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if requests != nil {
			*requests = append(*requests, req.URL.String())
		}
		if req.Method != http.MethodGet || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Fatalf("unexpected method or authentication: %s", req.Method)
		}
		var value any
		switch req.URL.String() {
		case updateAPIBase + "/releases/latest", updateAPIBase + "/releases/tags/" + f.release.Tag:
			value = f.meta
		case updateAPIBase + "/git/ref/tags/" + f.release.Tag:
			value = f.reference
		default:
			for name, data := range f.payloads {
				if req.URL.String() == updateDownloadBase+"/"+f.release.Tag+"/"+name || req.URL.String() == updateMirrorBase+"/"+f.release.Tag+"/"+name {
					return updateTestResponse(http.StatusOK, data), nil
				}
			}
			t.Fatalf("unexpected request %s", req.URL.String())
		}
		if req.Header.Get("Accept") != "application/vnd.github+json" || req.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Fatal("missing fixed API request headers")
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return updateTestResponse(http.StatusOK, data), nil
	})}}
}

func TestUpdateReleaseReadsCanonicalMetadata(t *testing.T) {
	for _, latest := range []bool{true, false} {
		f := newUpdateReleaseFixture(t)
		var requests []string
		client := f.client(t, &requests)
		var got updateRelease
		var err error
		if latest {
			got, err = client.latest(context.Background())
		} else {
			got, err = client.release(context.Background(), f.release.Tag)
		}
		if err != nil {
			t.Fatal(err)
		}
		if got.Tag != f.release.Tag || got.Commit != f.release.Commit || got.ID != f.release.ID || got.Notes != f.release.Notes || len(got.Assets) != 11 {
			t.Fatalf("unexpected release %#v", got)
		}
		if len(requests) != 2 {
			t.Fatalf("expected release and tag requests, got %v", requests)
		}
	}
}

func TestUpdateReleaseRejectsInvalidMetadata(t *testing.T) {
	asset := func(f *updateReleaseFixture, i int) map[string]any {
		return f.meta["assets"].([]any)[i].(map[string]any)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*updateReleaseFixture)
	}{
		{"mutable", func(f *updateReleaseFixture) { f.meta["immutable"] = false }},
		{"missing immutable", func(f *updateReleaseFixture) { delete(f.meta, "immutable") }},
		{"draft", func(f *updateReleaseFixture) { f.meta["draft"] = true }},
		{"missing draft", func(f *updateReleaseFixture) { delete(f.meta, "draft") }},
		{"prerelease", func(f *updateReleaseFixture) { f.meta["prerelease"] = true }},
		{"missing prerelease", func(f *updateReleaseFixture) { delete(f.meta, "prerelease") }},
		{"zero release id", func(f *updateReleaseFixture) { f.meta["id"] = 0 }},
		{"boolean release id", func(f *updateReleaseFixture) { f.meta["id"] = true }},
		{"release api foreign repo", func(f *updateReleaseFixture) {
			f.meta["url"] = "https://api.github.com/repos/evil/proxyscene/releases/42"
		}},
		{"release html foreign repo", func(f *updateReleaseFixture) {
			f.meta["html_url"] = "https://github.com/evil/proxyscene/releases/tag/v0.9.2"
		}},
		{"leading zero tag", func(f *updateReleaseFixture) { f.meta["tag_name"] = "v00.9.2" }},
		{"invalid published date", func(f *updateReleaseFixture) { f.meta["published_at"] = "2026-02-30T00:00:00Z" }},
		{"future published date", func(f *updateReleaseFixture) { f.meta["published_at"] = "9999-01-01T00:00:00Z" }},
		{"missing asset", func(f *updateReleaseFixture) { f.meta["assets"] = f.meta["assets"].([]any)[:10] }},
		{"duplicate asset name", func(f *updateReleaseFixture) { asset(f, 1)["name"] = asset(f, 0)["name"] }},
		{"duplicate asset id", func(f *updateReleaseFixture) { asset(f, 1)["id"] = asset(f, 0)["id"] }},
		{"zero asset id", func(f *updateReleaseFixture) { asset(f, 0)["id"] = 0 }},
		{"pending asset", func(f *updateReleaseFixture) { asset(f, 0)["state"] = "new" }},
		{"asset api foreign repo", func(f *updateReleaseFixture) {
			asset(f, 0)["url"] = "https://api.github.com/repos/evil/proxyscene/releases/assets/1"
		}},
		{"missing asset digest", func(f *updateReleaseFixture) { delete(asset(f, 0), "digest") }},
		{"wrong digest algorithm", func(f *updateReleaseFixture) { asset(f, 0)["digest"] = "sha512:" + strings.Repeat("1", 64) }},
		{"noncanonical digest", func(f *updateReleaseFixture) { asset(f, 0)["digest"] = "sha256:" + strings.Repeat("A", 64) }},
		{"empty asset", func(f *updateReleaseFixture) { asset(f, 0)["size"] = 0 }},
		{"negative asset size", func(f *updateReleaseFixture) { asset(f, 0)["size"] = -1 }},
		{"oversize installer", func(f *updateReleaseFixture) { asset(f, 0)["size"] = updateMaxMetadataBytes + 1 }},
		{"oversize bundle", func(f *updateReleaseFixture) { asset(f, 1)["size"] = updateMaxAssetBytes + 1 }},
		{"total oversize", func(f *updateReleaseFixture) {
			for i := 1; i < 4; i++ {
				asset(f, i)["size"] = updateMaxAssetBytes
			}
		}},
		{"asset url query", func(f *updateReleaseFixture) {
			asset(f, 0)["browser_download_url"] = asset(f, 0)["browser_download_url"].(string) + "?token=secret"
		}},
		{"asset foreign repo", func(f *updateReleaseFixture) {
			asset(f, 0)["browser_download_url"] = "https://github.com/evil/proxyscene/releases/download/v0.9.2/install.sh"
		}},
		{"unknown asset", func(f *updateReleaseFixture) { asset(f, 0)["name"] = "extra.sh" }},
		{"unversioned source", func(f *updateReleaseFixture) { asset(f, 9)["name"] = "xray_source.tar.gz" }},
		{"tag ref mismatch", func(f *updateReleaseFixture) { f.reference["ref"] = "refs/tags/v9.9.9" }},
		{"tag api mismatch", func(f *updateReleaseFixture) { f.reference["url"] = updateAPIBase + "/git/refs/tags/v9.9.9" }},
		{"annotated tag", func(f *updateReleaseFixture) { f.reference["object"].(map[string]any)["type"] = "tag" }},
		{"invalid commit", func(f *updateReleaseFixture) { f.reference["object"].(map[string]any)["sha"] = "xyz" }},
		{"commit api mismatch", func(f *updateReleaseFixture) {
			f.reference["object"].(map[string]any)["url"] = "https://api.github.com/repos/evil/proxyscene/git/commits/" + f.release.Commit
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateReleaseFixture(t)
			tc.mutate(&f)
			if _, err := f.client(t, nil).latest(context.Background()); err == nil {
				t.Fatal("accepted invalid release metadata")
			}
		})
	}
}

func TestUpdateReleaseRejectsDifferentRequestedTag(t *testing.T) {
	f := newUpdateReleaseFixture(t)
	f.meta["tag_name"] = "v0.9.3"
	if _, err := f.client(t, nil).release(context.Background(), "v0.9.2"); err == nil {
		t.Fatal("accepted different returned tag")
	}
	var requests []string
	if _, err := f.client(t, &requests).release(context.Background(), "../latest?token=secret"); err == nil || len(requests) != 0 {
		t.Fatal("invalid tag reached network")
	}
}

func TestUpdateReleaseRejectsMalformedJSON(t *testing.T) {
	for _, raw := range []string{
		`{"immutable":false,"immutable":true}`, `{"immutable":false,"IMMUTABLE":true}`,
		`{"object":{"sha":"first","sha":"second"}}`, `{} {}`, `{} null`, `null`, `[]`,
		`{"x":` + strings.Repeat("[", 66) + `0` + strings.Repeat("]", 66) + `}`,
		`{broken}`, strings.Repeat(" ", int(updateMaxMetadataBytes)+1),
	} {
		client := &updateClient{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			resp := updateTestResponse(http.StatusOK, []byte(raw))
			resp.ContentLength = -1 // Exercise the streaming bound, not just the header check.
			return resp, nil
		})}}
		if _, err := client.latest(context.Background()); err == nil {
			t.Fatalf("accepted malformed JSON %.80q", raw)
		}
	}
}

func TestUpdateDownloadBundle(t *testing.T) {
	for _, source := range []string{"github", "mirror"} {
		t.Run(source, func(t *testing.T) {
			f := newUpdateReleaseFixture(t)
			if source == "mirror" {
				f.release.Source = source
				for name, asset := range f.release.Assets {
					asset.URL = ""
					f.release.Assets[name] = asset
				}
			}
			var requests []string
			dest := filepath.Join(t.TempDir(), "bundle.tar.gz")
			if err := f.client(t, &requests).downloadBundle(context.Background(), f.release, "amd64", source, dest); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(dest)
			if err != nil || !bytes.Equal(got, f.payloads["proxyscene_bundle_linux_amd64.tar.gz"]) {
				t.Fatalf("wrong bundle: %v", err)
			}
			info, err := os.Stat(dest)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("bundle permissions: %v, %v", info, err)
			}
			base := updateDownloadBase
			if source == "mirror" {
				base = updateMirrorBase
			}
			wantBundle := base + "/v0.9.2/proxyscene_bundle_linux_amd64.tar.gz"
			if len(requests) != 2 || requests[0] != base+"/v0.9.2/checksums.txt" || requests[1] != wantBundle {
				t.Fatalf("incorrect trust/download sources: %v", requests)
			}
		})
	}
}

func TestUpdateDownloadPreservesExistingDestinations(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		f := newUpdateReleaseFixture(t)
		dir := t.TempDir()
		dest := filepath.Join(dir, "bundle")
		target := dest
		if symlink {
			target = filepath.Join(dir, "victim")
			if err := os.Symlink(target, dest); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		var requests []string
		if err := f.client(t, &requests).downloadBundle(context.Background(), f.release, "amd64", "github", dest); err == nil {
			t.Fatal("overwrote existing destination")
		}
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "keep" || len(requests) != 0 {
			t.Fatalf("existing file changed or network reached: %q, %v, %v", data, err, requests)
		}
	}
}

func TestUpdateDownloadRejectsInvalidChoicesBeforeNetwork(t *testing.T) {
	for _, tc := range []struct{ arch, source string }{{"amd64", "https://evil.invalid"}, {"../amd64", "github"}, {"mips", "mirror"}} {
		f := newUpdateReleaseFixture(t)
		var requests []string
		dest := filepath.Join(t.TempDir(), "bundle")
		if err := f.client(t, &requests).downloadBundle(context.Background(), f.release, tc.arch, tc.source, dest); err == nil || len(requests) != 0 {
			t.Fatalf("invalid choice accepted or reached network: %v", err)
		}
		if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid choice created a file")
		}
	}
}

func TestUpdateDownloadCleansFailedBundle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*updateReleaseFixture)
	}{
		{"wrong bundle hash", func(f *updateReleaseFixture) { f.payloads["proxyscene_bundle_linux_amd64.tar.gz"][0] ^= 1 }},
		{"short bundle", func(f *updateReleaseFixture) { f.payloads["proxyscene_bundle_linux_amd64.tar.gz"] = []byte("short") }},
		{"long bundle", func(f *updateReleaseFixture) {
			f.payloads["proxyscene_bundle_linux_amd64.tar.gz"] = append(f.payloads["proxyscene_bundle_linux_amd64.tar.gz"], 'x')
		}},
		{"wrong manifest hash", func(f *updateReleaseFixture) { f.payloads["checksums.txt"][0] ^= 1 }},
		{"manifest missing entry", func(f *updateReleaseFixture) {
			lines := strings.Split(string(f.payloads["checksums.txt"]), "\n")
			f.payloads["checksums.txt"] = []byte(strings.Join(lines[1:], "\n"))
			f.rehashManifest()
		}},
		{"manifest duplicate entry", func(f *updateReleaseFixture) {
			line := strings.Split(string(f.payloads["checksums.txt"]), "\n")[0]
			f.payloads["checksums.txt"] = append(f.payloads["checksums.txt"], []byte(line+"\n")...)
			f.rehashManifest()
		}},
		{"manifest wrong digest", func(f *updateReleaseFixture) {
			f.payloads["checksums.txt"][0] = 'a'
			f.payloads["checksums.txt"][1] = 'b'
			f.rehashManifest()
		}},
		{"manifest crlf", func(f *updateReleaseFixture) {
			f.payloads["checksums.txt"] = bytes.ReplaceAll(f.payloads["checksums.txt"], []byte("\n"), []byte("\r\n"))
			f.rehashManifest()
		}},
		{"manifest no final newline", func(f *updateReleaseFixture) {
			f.payloads["checksums.txt"] = bytes.TrimSuffix(f.payloads["checksums.txt"], []byte("\n"))
			f.rehashManifest()
		}},
		{"manifest path traversal", func(f *updateReleaseFixture) {
			f.payloads["checksums.txt"] = bytes.ReplaceAll(f.payloads["checksums.txt"], []byte("install.sh"), []byte("../install.sh"))
			f.rehashManifest()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateReleaseFixture(t)
			tc.mutate(&f)
			base := f.client(t, nil)
			transport := base.http.Transport
			base.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				resp, err := transport.RoundTrip(req)
				if resp != nil {
					resp.ContentLength = -1 // Detect short/long streams independently of Content-Length.
				}
				return resp, err
			})
			dest := filepath.Join(t.TempDir(), "bundle")
			if err := base.downloadBundle(context.Background(), f.release, "amd64", "github", dest); err == nil {
				t.Fatal("accepted invalid content")
			}
			if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed bundle not removed: %v", err)
			}
		})
	}
}

func (f *updateReleaseFixture) rehashManifest() {
	a := f.release.Assets["checksums.txt"]
	hash := sha256.Sum256(f.payloads["checksums.txt"])
	a.Size, a.SHA256 = int64(len(f.payloads["checksums.txt"])), hex.EncodeToString(hash[:])
	f.release.Assets["checksums.txt"] = a
}

func TestUpdateHTTPRedirectPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, source, target string
		allowed              bool
	}{
		{"release asset", "github", "https://release-assets.githubusercontent.com/file?signature=TOP-SECRET", true},
		{"objects asset", "github", "https://objects.githubusercontent.com/file?signature=TOP-SECRET", true},
		{"http", "github", "http://release-assets.githubusercontent.com/file", false},
		{"subdomain", "github", "https://release-assets.githubusercontent.com.evil.invalid/file", false},
		{"userinfo", "github", "https://secret@release-assets.githubusercontent.com/file", false},
		{"port", "github", "https://release-assets.githubusercontent.com:443/file", false},
		{"fragment", "github", "https://release-assets.githubusercontent.com/file#fragment", false},
		{"github other path", "github", "https://github.com/evil/file", false},
		{"loopback", "github", "https://127.0.0.1/file", false},
		{"mirror redirect", "mirror", "https://release-assets.githubusercontent.com/file", false},
		{"api redirect", "api", "https://api.github.com/repos/longlannet/proxyscene/releases/42", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &updateClient{http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					resp := updateTestResponse(http.StatusFound, nil)
					resp.Header.Set("Location", tc.target)
					return resp, nil
				}
				if req.URL.String() != tc.target || !tc.allowed {
					t.Fatal("forbidden redirect was sent")
				}
				return updateTestResponse(http.StatusOK, []byte("ok")), nil
			})}}
			address := updateDownloadBase + "/v0.9.2/checksums.txt"
			if tc.source == "api" {
				address = updateAPIBase + "/releases/latest"
			} else if tc.source == "mirror" {
				address = updateMirrorBase + "/v0.9.2/checksums.txt"
			}
			resp, err := client.get(context.Background(), address, tc.source)
			if resp != nil {
				resp.Body.Close()
			}
			if (err == nil) != tc.allowed {
				t.Fatalf("redirect result: %v; want allowed=%v", err, tc.allowed)
			}
			if err != nil && (strings.Contains(err.Error(), "TOP-SECRET") || strings.Contains(err.Error(), "secret@")) {
				t.Fatalf("redirect credentials leaked: %v", err)
			}
		})
	}
}

func TestUpdateHTTPRejectsPartialEncodedAndErrorResponses(t *testing.T) {
	for _, tc := range []struct {
		status       int
		contentRange string
		encoding     string
	}{
		{http.StatusPartialContent, "bytes 0-1/10", ""},
		{http.StatusOK, "bytes 0-1/10", ""},
		{http.StatusOK, "", "gzip"},
		{http.StatusUnauthorized, "", ""},
		{http.StatusTooManyRequests, "", ""},
	} {
		client := &updateClient{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			resp := updateTestResponse(tc.status, []byte("TOP-SECRET"))
			if tc.contentRange != "" {
				resp.Header.Set("Content-Range", tc.contentRange)
			}
			resp.Header.Set("Content-Encoding", tc.encoding)
			resp.Status = "TOP-SECRET"
			return resp, nil
		})}}
		_, err := client.get(context.Background(), updateAPIBase+"/releases/latest", "api")
		if err == nil || strings.Contains(err.Error(), "TOP-SECRET") {
			t.Fatalf("invalid status/encoding accepted or leaked: %v", err)
		}
	}
}

func TestUpdateHTTPErrorsRedactTransportDetails(t *testing.T) {
	client := &updateClient{http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("proxy password TOP-SECRET https://release-assets.githubusercontent.com/file?signature=SECRET")
	})}}
	_, err := client.latest(context.Background())
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "https://") {
		t.Fatalf("transport error was not redacted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.latest(ctx)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("context cancellation not preserved safely: %v", err)
	}
}
