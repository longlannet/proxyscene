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
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	updateRepository       = "longlannet/proxyscene"
	updateMirrorBase       = "https://dl.ll.cd/proxyscene"
	updateAPIBase          = "https://api.github.com/repos/" + updateRepository
	updateDownloadBase     = "https://github.com/" + updateRepository + "/releases/download"
	updateMaxMetadataBytes = int64(1024 * 1024)
	updateMaxAssetBytes    = int64(256 * 1024 * 1024)
	updateMaxReleaseBytes  = int64(512 * 1024 * 1024)
)

var (
	updateVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	updateSourcePattern  = regexp.MustCompile(`^xray_source_v[0-9]+(?:\.[0-9]+)+\.tar\.gz$`)
	updateSHA256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	updateCommitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	updateChecksumLine   = regexp.MustCompile(`^([0-9a-f]{64})  ([A-Za-z0-9_.-]+)$`)
)

type updateAsset struct {
	Name   string
	Size   int64
	SHA256 string
	URL    string
}

type updateRelease struct {
	Tag       string
	Commit    string
	ID        int64
	Published string
	Notes     string
	Source    string
	Assets    map[string]updateAsset
}

type updateClient struct {
	http *http.Client
}

func newUpdateClient() *updateClient {
	return &updateClient{http: &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
		ForceAttemptHTTP2:     true,
	}}}
}

// compareUpdateVersions accepts only canonical stable versions, including the
// leading v. Compare decimal strings so even bounded, very large components do
// not overflow machine integers.
func compareUpdateVersions(a, b string) (int, error) {
	if !validUpdateVersion(a) || !validUpdateVersion(b) {
		return 0, errors.New("版本必须是正式版本 vMAJOR.MINOR.PATCH")
	}
	as, bs := strings.Split(a[1:], "."), strings.Split(b[1:], ".")
	for i := range as {
		if len(as[i]) < len(bs[i]) {
			return -1, nil
		}
		if len(as[i]) > len(bs[i]) {
			return 1, nil
		}
		if n := strings.Compare(as[i], bs[i]); n != 0 {
			return n, nil
		}
	}
	return 0, nil
}

func validUpdateVersion(tag string) bool {
	return len(tag) <= 128 && updateVersionPattern.MatchString(tag)
}

// The mirror is a separately trusted HTTPS publication endpoint. Its fixed
// manifest is produced only after the publisher has verified GitHub's immutable
// release; clients do not need to contact GitHub when using this source.
type updateMirrorLatest struct {
	Version   string `json:"version"`
	Tag       string `json:"tag"`
	BaseURL   string `json:"base_url"`
	Commit    string `json:"commit"`
	ReleaseID int64  `json:"release_id"`
	Published string `json:"published_at"`
}

type updateMirrorMetadata struct {
	SchemaVersion int    `json:"schema_version"`
	Tag           string `json:"tag"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	ReleaseID     int64  `json:"release_id"`
	Published     string `json:"published_at"`
	Notes         string `json:"notes"`
	Assets        map[string]struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"assets"`
}

func (c *updateClient) latestForSource(ctx context.Context, source string) (updateRelease, error) {
	switch source {
	case "github":
		return c.latest(ctx)
	case "mirror":
		var index updateMirrorLatest
		if err := c.mirrorJSON(ctx, updateMirrorBase+"/latest.json", &index); err != nil {
			return updateRelease{}, err
		}
		if !validUpdateVersion(index.Tag) || index.Version != strings.TrimPrefix(index.Tag, "v") ||
			index.BaseURL != updateMirrorBase+"/"+index.Tag || !updateCommitPattern.MatchString(index.Commit) ||
			index.ReleaseID <= 0 || !validUpdatePublished(index.Published) {
			return updateRelease{}, errors.New("镜像最新版本索引身份无效")
		}
		release, err := c.mirrorRelease(ctx, index.Tag)
		if err != nil {
			return updateRelease{}, err
		}
		if release.Commit != index.Commit || release.ID != index.ReleaseID || release.Published != index.Published {
			return updateRelease{}, errors.New("镜像最新版本索引与版本元数据身份不一致")
		}
		return release, nil
	default:
		return updateRelease{}, errors.New("更新下载来源只能是 github 或 mirror")
	}
}

func validUpdatePublished(value string) bool {
	published, err := time.Parse("2006-01-02T15:04:05Z", value)
	return err == nil && !published.After(time.Now().Add(5*time.Minute)) && published.Format("2006-01-02T15:04:05Z") == value
}

func (c *updateClient) mirrorRelease(ctx context.Context, tag string) (updateRelease, error) {
	if !validUpdateVersion(tag) {
		return updateRelease{}, errors.New("发行版本格式无效")
	}
	var metadata updateMirrorMetadata
	if err := c.mirrorJSON(ctx, updateMirrorBase+"/metadata/"+tag+".json", &metadata); err != nil {
		return updateRelease{}, err
	}
	if metadata.SchemaVersion != 1 || metadata.Tag != tag || metadata.Version != strings.TrimPrefix(tag, "v") || !validUpdatePublished(metadata.Published) || len(metadata.Notes) > 64<<10 {
		return updateRelease{}, errors.New("镜像版本元数据格式或身份无效")
	}
	release := updateRelease{Tag: tag, Commit: metadata.Commit, ID: metadata.ReleaseID, Published: metadata.Published,
		Notes: metadata.Notes, Source: "mirror", Assets: make(map[string]updateAsset, len(metadata.Assets))}
	for name, asset := range metadata.Assets {
		release.Assets[name] = updateAsset{Name: name, Size: asset.Size, SHA256: asset.SHA256}
	}
	if err := validateUpdateRelease(release); err != nil {
		return updateRelease{}, err
	}
	return release, nil
}

func (c *updateClient) revalidate(ctx context.Context, selected updateRelease) error {
	var current updateRelease
	var err error
	if selected.Source == "mirror" {
		// Keep the version selected by the one Latest read, even if another
		// release finishes publishing during the download. Recheck its fixed
		// metadata instead of switching versions midway through an upgrade.
		current, err = c.mirrorRelease(ctx, selected.Tag)
	} else if selected.Source == "github" {
		current, err = c.latest(ctx)
	} else {
		return errors.New("更新下载来源无效")
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, selected) {
		return errors.New("下载期间发行身份发生变化，请重新检查更新")
	}
	return nil
}

func (c *updateClient) latest(ctx context.Context) (updateRelease, error) {
	return c.readRelease(ctx, "latest")
}

func (c *updateClient) release(ctx context.Context, tag string) (updateRelease, error) {
	if !validUpdateVersion(tag) {
		return updateRelease{}, errors.New("发行版本格式无效")
	}
	return c.readRelease(ctx, tag)
}

func (c *updateClient) readRelease(ctx context.Context, requested string) (updateRelease, error) {
	endpoint := "/releases/latest"
	if requested != "latest" {
		endpoint = "/releases/tags/" + requested
	}
	var raw struct {
		Tag        string `json:"tag_name"`
		ID         int64  `json:"id"`
		URL        string `json:"url"`
		HTMLURL    string `json:"html_url"`
		Notes      string `json:"body"`
		Immutable  *bool  `json:"immutable"`
		Draft      *bool  `json:"draft"`
		Prerelease *bool  `json:"prerelease"`
		Published  string `json:"published_at"`
		Assets     []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			Digest string `json:"digest"`
			URL    string `json:"browser_download_url"`
			APIURL string `json:"url"`
			State  string `json:"state"`
		} `json:"assets"`
	}
	if err := c.apiJSON(ctx, endpoint, &raw); err != nil {
		return updateRelease{}, err
	}
	if !validUpdateVersion(raw.Tag) || (requested != "latest" && raw.Tag != requested) {
		return updateRelease{}, errors.New("GitHub 返回了无效或不匹配的发行版本")
	}
	if raw.Immutable == nil || !*raw.Immutable || raw.Draft == nil || *raw.Draft || raw.Prerelease == nil || *raw.Prerelease {
		return updateRelease{}, errors.New("只能更新到 GitHub 已锁定的正式发行版")
	}
	if raw.ID <= 0 || raw.URL != updateAPIBase+"/releases/"+strconv.FormatInt(raw.ID, 10) ||
		raw.HTMLURL != "https://github.com/"+updateRepository+"/releases/tag/"+raw.Tag {
		return updateRelease{}, errors.New("GitHub 发行身份与官方仓库不符")
	}
	if !validUpdatePublished(raw.Published) {
		return updateRelease{}, errors.New("GitHub 发行时间无效")
	}
	if len(raw.Assets) != 11 {
		return updateRelease{}, errors.New("GitHub 发行必须包含完整的 11 个文件")
	}
	r := updateRelease{Tag: raw.Tag, ID: raw.ID, Published: raw.Published, Notes: raw.Notes, Source: "github", Assets: make(map[string]updateAsset, len(raw.Assets))}
	ids := make(map[int64]bool, len(raw.Assets))
	for _, a := range raw.Assets {
		if a.ID <= 0 || ids[a.ID] || a.State != "uploaded" || a.APIURL != updateAPIBase+"/releases/assets/"+strconv.FormatInt(a.ID, 10) {
			return updateRelease{}, errors.New("GitHub 发行文件身份或上传状态无效")
		}
		ids[a.ID] = true
		if _, exists := r.Assets[a.Name]; exists {
			return updateRelease{}, errors.New("GitHub 发行包含重复文件")
		}
		if !strings.HasPrefix(a.Digest, "sha256:") {
			return updateRelease{}, errors.New("GitHub 发行文件缺少 SHA256 校验值")
		}
		r.Assets[a.Name] = updateAsset{Name: a.Name, Size: a.Size, SHA256: strings.TrimPrefix(a.Digest, "sha256:"), URL: a.URL}
	}
	var reference struct {
		Ref    string `json:"ref"`
		URL    string `json:"url"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
			URL  string `json:"url"`
		} `json:"object"`
	}
	if err := c.apiJSON(ctx, "/git/ref/tags/"+r.Tag, &reference); err != nil {
		return updateRelease{}, err
	}
	if reference.Ref != "refs/tags/"+r.Tag || reference.URL != updateAPIBase+"/git/refs/tags/"+r.Tag ||
		reference.Object.Type != "commit" || !updateCommitPattern.MatchString(reference.Object.SHA) ||
		reference.Object.URL != updateAPIBase+"/git/commits/"+reference.Object.SHA {
		return updateRelease{}, errors.New("发行 tag 必须直接指向官方仓库的 commit")
	}
	r.Commit = reference.Object.SHA
	if err := validateUpdateRelease(r); err != nil {
		return updateRelease{}, err
	}
	return r, nil
}

func validateUpdateRelease(r updateRelease) error {
	if !validUpdateVersion(r.Tag) || r.ID <= 0 || !updateCommitPattern.MatchString(r.Commit) || len(r.Assets) != 11 ||
		(r.Source != "mirror" && r.Source != "github") {
		return errors.New("发行身份或文件列表无效")
	}
	expected := map[string]bool{"install.sh": true, "checksums.txt": true}
	for _, arch := range []string{"amd64", "arm64", "386", "armv7"} {
		expected["proxyscene_linux_"+arch+".tar.gz"] = true
		expected["proxyscene_bundle_linux_"+arch+".tar.gz"] = true
	}
	var total int64
	sources := 0
	for name, a := range r.Assets {
		if !expected[name] {
			if len(name) > 128 || !updateSourcePattern.MatchString(name) {
				return errors.New("发行包含不支持的文件名")
			}
			sources++
		}
		limit := updateMaxAssetBytes
		if name == "install.sh" || name == "checksums.txt" {
			limit = updateMaxMetadataBytes
		}
		if a.Name != name || a.Size <= 0 || a.Size > limit || !updateSHA256Pattern.MatchString(a.SHA256) ||
			(r.Source == "github" && a.URL != updateDownloadBase+"/"+r.Tag+"/"+name) || (r.Source == "mirror" && a.URL != "") {
			return errors.New("发行文件的大小、校验值或官方地址无效")
		}
		total += a.Size
	}
	if sources != 1 || total > updateMaxReleaseBytes {
		return errors.New("发行文件集合不完整或超过大小限制")
	}
	for name := range expected {
		if _, ok := r.Assets[name]; !ok {
			return errors.New("发行缺少必要文件")
		}
	}
	return nil
}

func (c *updateClient) apiJSON(ctx context.Context, endpoint string, out any) error {
	return c.metadataJSON(ctx, updateAPIBase+endpoint, "api", out)
}

func (c *updateClient) mirrorJSON(ctx context.Context, address string, out any) error {
	return c.metadataJSON(ctx, address, "mirror", out)
}

func (c *updateClient) metadataJSON(ctx context.Context, address, source string, out any) error {
	resp, err := c.get(ctx, address, source)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.ContentLength > updateMaxMetadataBytes {
		return errors.New("发行元数据超过大小限制")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, updateMaxMetadataBytes+1))
	if err != nil || int64(len(raw)) > updateMaxMetadataBytes {
		return errors.New("发行元数据读取失败或超过大小限制")
	}
	if err := validateUpdateJSON(raw); err != nil {
		return err
	}
	if source == "mirror" {
		// Unlike GitHub's extensible API, this protocol has a fixed, versioned
		// schema. Require every canonical field, including an empty notes field.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return errors.New("镜像元数据格式无效")
		}
		var required []string
		switch out.(type) {
		case *updateMirrorLatest:
			required = []string{"version", "tag", "base_url", "commit", "release_id", "published_at"}
		case *updateMirrorMetadata:
			required = []string{"schema_version", "version", "tag", "commit", "release_id", "published_at", "notes", "assets"}
		default:
			return errors.New("镜像元数据类型无效")
		}
		if len(fields) != len(required) {
			return errors.New("镜像元数据字段不完整或不受支持")
		}
		for _, key := range required {
			if value, ok := fields[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return errors.New("镜像元数据缺少必要字段")
			}
		}
		if assetsJSON, ok := fields["assets"]; ok {
			var assets map[string]map[string]json.RawMessage
			if err := json.Unmarshal(assetsJSON, &assets); err != nil {
				return errors.New("镜像文件元数据格式无效")
			}
			for _, asset := range assets {
				if len(asset) != 2 || asset["sha256"] == nil || asset["size"] == nil {
					return errors.New("镜像文件元数据字段不完整或不受支持")
				}
			}
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(out); err != nil {
			return errors.New("镜像元数据字段格式无效")
		}
	} else if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("GitHub 元数据格式无效")
	}
	return nil
}

// Reject ambiguous duplicate members (including case variants accepted by Go's
// struct decoder), excess nesting, and trailing JSON before typed decoding.
func validateUpdateJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("发行元数据不是有效的 UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("excess JSON depth")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[strings.ToLower(name)] {
					return errors.New("duplicate JSON member")
				}
				seen[strings.ToLower(name)] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return errors.New("发行元数据包含重复字段或无效 JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("发行元数据包含多余内容")
	}
	// encoding/json replaces unpaired UTF-16 escapes with U+FFFD. Reject
	// these ambiguous strings instead of silently changing published notes
	// or metadata identifiers while decoding the publisher's document.
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if i+1 >= len(raw) || raw[i+1] != 'u' {
			i++
			continue
		}
		if i+6 > len(raw) {
			return errors.New("发行元数据包含无效 Unicode 转义")
		}
		value, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
		if err != nil || value >= 0xdc00 && value <= 0xdfff {
			return errors.New("发行元数据包含无效 Unicode 转义")
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+12 > len(raw) || raw[i+6] != '\\' || raw[i+7] != 'u' {
				return errors.New("发行元数据包含无效 Unicode 转义")
			}
			low, err := strconv.ParseUint(string(raw[i+8:i+12]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return errors.New("发行元数据包含无效 Unicode 转义")
			}
			i += 6
		}
		i += 5
	}
	return nil
}

// get creates a request-local client so injected test transports and concurrent
// checks cannot weaken redirect policy or mutate a shared client's settings.
func (c *updateClient) get(ctx context.Context, address, source string) (*http.Response, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawQuery != "" || u.ForceQuery {
		return nil, errors.New("更新下载地址不符合 HTTPS 来源规则")
	}
	switch source {
	case "api":
		if u.Host != "api.github.com" || !strings.HasPrefix(address, updateAPIBase+"/") {
			return nil, errors.New("更新 API 地址不是官方仓库")
		}
	case "github":
		if u.Host != "github.com" || !strings.HasPrefix(address, updateDownloadBase+"/") {
			return nil, errors.New("更新下载地址不是官方 GitHub 发行")
		}
	case "mirror":
		if u.Host != "dl.ll.cd" || !strings.HasPrefix(address, updateMirrorBase+"/") {
			return nil, errors.New("更新下载地址不是官方镜像")
		}
	default:
		return nil, errors.New("更新下载来源无效")
	}
	client := *c.http
	client.Jar = nil
	limit := 180 * time.Second
	if source == "api" {
		limit = 40 * time.Second
	}
	if client.Timeout <= 0 || client.Timeout > limit {
		client.Timeout = limit
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if source != "github" || len(via) > 5 || !trustedUpdateAssetRedirect(req.URL) {
			return errors.New("更新下载重定向被拒绝")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("无法建立更新请求")
	}
	req.Header.Set("User-Agent", "proxyscene-updater")
	req.Header.Set("Accept-Encoding", "identity")
	if source == "api" {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("更新请求取消或超时: %w", ctx.Err())
		}
		// net/http errors can include signed redirect query strings or proxy
		// credentials. Do not expose transport error text in terminal output.
		return nil, errors.New("更新 HTTPS 请求失败；请检查网络与官方服务状态")
	}
	if resp.StatusCode != http.StatusOK || len(resp.Header.Values("Content-Range")) != 0 ||
		len(resp.Header.Values("Content-Encoding")) > 1 ||
		(resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity") {
		resp.Body.Close()
		return nil, fmt.Errorf("更新下载返回无效响应 (HTTP %d)", resp.StatusCode)
	}
	return resp, nil
}

func trustedUpdateAssetRedirect(u *url.URL) bool {
	return u.Scheme == "https" && u.User == nil && u.Opaque == "" && u.Fragment == "" &&
		(u.Host == "release-assets.githubusercontent.com" || u.Host == "objects.githubusercontent.com")
}

func (c *updateClient) downloadBundle(ctx context.Context, r updateRelease, arch, source, dest string) error {
	if err := validateUpdateRelease(r); err != nil {
		return err
	}
	if source != r.Source {
		return errors.New("更新文件来源与发行元数据来源不一致")
	}
	asset, ok := r.Assets["proxyscene_bundle_linux_"+arch+".tar.gz"]
	if !ok {
		return errors.New("当前 CPU 架构没有受支持的升级包")
	}
	file, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("无法独占创建升级包临时文件")
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			_ = os.Remove(dest)
		}
	}()
	// Both the manifest and the archive use the selected source. Neither
	// transport failure nor a digest mismatch permits a fallback source.
	base := updateDownloadBase
	if source == "mirror" {
		base = updateMirrorBase
	}
	var manifest bytes.Buffer
	if err := c.downloadAsset(ctx, r.Assets["checksums.txt"], source, base+"/"+r.Tag+"/checksums.txt", &manifest); err != nil {
		return err
	}
	if err := validateUpdateChecksums(r, manifest.Bytes()); err != nil {
		return err
	}
	address := base + "/" + r.Tag + "/" + asset.Name
	if err := c.downloadAsset(ctx, asset, source, address, file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return errors.New("无法同步升级包临时文件")
	}
	if err := file.Close(); err != nil {
		return errors.New("无法关闭升级包临时文件")
	}
	complete = true
	return nil
}

func (c *updateClient) downloadAsset(ctx context.Context, asset updateAsset, source, address string, dest io.Writer) error {
	resp, err := c.get(ctx, address, source)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.ContentLength >= 0 && resp.ContentLength != asset.Size {
		return errors.New("更新文件的响应大小与发行元数据记录不符")
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(dest, hash), io.LimitReader(resp.Body, asset.Size+1))
	if err != nil || n != asset.Size {
		return errors.New("更新文件读取失败或大小与发行元数据记录不符")
	}
	if hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
		return errors.New("更新文件的 SHA256 与发行元数据记录不符")
	}
	return nil
}

func validateUpdateChecksums(r updateRelease, raw []byte) error {
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return errors.New("发行校验清单格式无效")
	}
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(raw[:len(raw)-1]), "\n") {
		match := updateChecksumLine.FindStringSubmatch(line)
		if match == nil || seen[match[2]] || match[2] == "checksums.txt" {
			return errors.New("发行校验清单包含重复或无效条目")
		}
		name := match[2]
		a, ok := r.Assets[name]
		if !ok || a.SHA256 != match[1] {
			return errors.New("发行校验清单与发行元数据文件记录不符")
		}
		seen[name] = true
	}
	if len(seen) != len(r.Assets)-1 {
		return errors.New("发行校验清单缺少文件")
	}
	return nil
}
