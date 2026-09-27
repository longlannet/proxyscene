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
	"regexp"
	"strconv"
	"strings"
	"time"
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
	Tag    string
	Commit string
	ID     int64
	Notes  string
	Assets map[string]updateAsset
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
	published, err := time.Parse("2006-01-02T15:04:05Z", raw.Published)
	if err != nil || published.After(time.Now().Add(5*time.Minute)) || published.Format("2006-01-02T15:04:05Z") != raw.Published {
		return updateRelease{}, errors.New("GitHub 发行时间无效")
	}
	if len(raw.Assets) != 11 {
		return updateRelease{}, errors.New("GitHub 发行必须包含完整的 11 个文件")
	}
	r := updateRelease{Tag: raw.Tag, ID: raw.ID, Notes: raw.Notes, Assets: make(map[string]updateAsset, len(raw.Assets))}
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
	if !validUpdateVersion(r.Tag) || r.ID <= 0 || !updateCommitPattern.MatchString(r.Commit) || len(r.Assets) != 11 {
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
			a.URL != updateDownloadBase+"/"+r.Tag+"/"+name {
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
	resp, err := c.get(ctx, updateAPIBase+endpoint, "api")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.ContentLength > updateMaxMetadataBytes {
		return errors.New("GitHub 元数据超过大小限制")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, updateMaxMetadataBytes+1))
	if err != nil || int64(len(raw)) > updateMaxMetadataBytes {
		return errors.New("GitHub 元数据读取失败或超过大小限制")
	}
	if err := validateUpdateJSON(raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("GitHub 元数据格式无效")
	}
	return nil
}

// Reject ambiguous duplicate members (including case variants accepted by Go's
// struct decoder), excess nesting, and trailing JSON before typed decoding.
func validateUpdateJSON(raw []byte) error {
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
		return errors.New("GitHub 元数据包含重复字段或无效 JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("GitHub 元数据包含多余内容")
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
	if source != "github" && source != "mirror" {
		return errors.New("更新下载来源只能是 github 或 mirror")
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
	// The checksum manifest is always authenticated against canonical GitHub
	// metadata and downloaded from GitHub, even when the bundle uses the mirror.
	var manifest bytes.Buffer
	if err := c.downloadAsset(ctx, r.Assets["checksums.txt"], "github", r.Assets["checksums.txt"].URL, &manifest); err != nil {
		return err
	}
	if err := validateUpdateChecksums(r, manifest.Bytes()); err != nil {
		return err
	}
	address := asset.URL
	if source == "mirror" {
		address = updateMirrorBase + "/" + r.Tag + "/" + asset.Name
	}
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
		return errors.New("更新文件的响应大小与 GitHub 记录不符")
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(dest, hash), io.LimitReader(resp.Body, asset.Size+1))
	if err != nil || n != asset.Size {
		return errors.New("更新文件读取失败或大小与 GitHub 记录不符")
	}
	if hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
		return errors.New("更新文件的 SHA256 与 GitHub 记录不符")
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
			return errors.New("发行校验清单与 GitHub 文件记录不符")
		}
		seen[name] = true
	}
	if len(seen) != len(r.Assets)-1 {
		return errors.New("发行校验清单缺少文件")
	}
	return nil
}
