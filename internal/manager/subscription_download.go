package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const subscriptionUserAgent = "proxyscene/1 subscription-client"

var errSubscriptionPolicy = errors.New("订阅访问被安全策略拒绝")

// Only transport/status/read failures may try another route. Never retry policy,
// oversized/incomplete responses, or node parsing failures through a proxy.
type subscriptionDownloadError struct{ message string }

func (e *subscriptionDownloadError) Error() string { return e.message }

type subscriptionDownloader struct {
	direct, proxy *http.Client
	allowHTTP     bool
}

func (a *App) subscriptionDownloaderForStore(st *Store) (*subscriptionDownloader, error) {
	allowHTTP := envBool("PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION", false)
	allowPrivate := envBool("PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION", false)
	d := &subscriptionDownloader{direct: subscriptionHTTPClient(allowHTTP, allowPrivate), allowHTTP: allowHTTP}
	if st != nil && st.SceneEnabled[SceneGlobal] {
		active, err := a.appForStoreRuntime(st)
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("读取当前全局代理配置失败：%w", err)
		}
		proxyAddr, err := subscriptionGlobalProxyAddress(active.cfg)
		if err != nil {
			d.Close()
			return nil, err
		}
		d.proxy = subscriptionProxyHTTPClient(allowHTTP, allowPrivate, proxyAddr)
	}
	return d, nil
}

func (d *subscriptionDownloader) Close() {
	d.direct.CloseIdleConnections()
	if d.proxy != nil {
		d.proxy.CloseIdleConnections()
	}
}

func (d *subscriptionDownloader) Prepare(ctx context.Context, sub string) (preparedSubscription, error) {
	prepared, directErr := downloadAndPrepareSubscriptionWithContext(ctx, sub, d.allowHTTP, d.direct)
	var downloadErr *subscriptionDownloadError
	if directErr == nil || !errors.As(directErr, &downloadErr) || ctx.Err() != nil {
		return prepared, directErr
	}
	if d.proxy == nil {
		return preparedSubscription{}, fmt.Errorf("%w；全局代理未开启，未尝试代理下载", directErr)
	}
	fmt.Println("直连下载失败，正在通过当前全局代理重试订阅下载…")
	prepared, proxyErr := downloadAndPrepareSubscriptionWithContext(ctx, sub, d.allowHTTP, d.proxy)
	if proxyErr != nil {
		return preparedSubscription{}, fmt.Errorf("直连：%v；全局代理：%w", directErr, proxyErr)
	}
	return prepared, nil
}

func (a *App) downloadAndPrepareSubscriptionForStore(sub string, snapshot *Store) (preparedSubscription, error) {
	ctx, cancel := context.WithTimeout(context.Background(), maxSubscriptionUpdateTime)
	defer cancel()
	d, err := a.subscriptionDownloaderForStore(snapshot)
	if err != nil {
		return preparedSubscription{}, err
	}
	defer d.Close()
	return d.Prepare(ctx, sub)
}

func downloadAndPrepareSubscriptionWithClient(sub string, allowHTTP bool, client *http.Client) (preparedSubscription, error) {
	return downloadAndPrepareSubscriptionWithContext(context.Background(), sub, allowHTTP, client)
}

func downloadAndPrepareSubscriptionWithContext(ctx context.Context, sub string, allowHTTP bool, client *http.Client) (preparedSubscription, error) {
	sub = strings.TrimSpace(sub)
	if sub == "" {
		return preparedSubscription{}, fmt.Errorf("订阅链接不能为空")
	}
	if len(sub) > maxSubscriptionURLBytes {
		return preparedSubscription{}, fmt.Errorf("订阅链接过长，最多 %d 字节", maxSubscriptionURLBytes)
	}
	subURL, err := url.Parse(sub)
	if err != nil || subURL.Host == "" || subURL.Hostname() == "" {
		return preparedSubscription{}, fmt.Errorf("订阅链接必须是有效的 https 地址")
	}
	subURL.Scheme = strings.ToLower(subURL.Scheme)
	switch subURL.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return preparedSubscription{}, fmt.Errorf("订阅链接必须使用 https；如确需导入明文 HTTP 订阅，请设置 PROXYSCENE_ALLOW_HTTP_SUBSCRIPTION=1")
		}
		fmt.Println("警告：正在导入明文 HTTP 订阅，内容可能被中间人篡改")
	default:
		return preparedSubscription{}, fmt.Errorf("订阅链接必须是 https 地址")
	}
	canonicalURL := subURL.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, canonicalURL, nil)
	if err != nil {
		return preparedSubscription{}, fmt.Errorf("订阅链接无效")
	}
	req.Header.Set("User-Agent", subscriptionUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		// Transport errors often include bearer URLs; never wrap or print them.
		if errors.Is(err, errSubscriptionPolicy) {
			return preparedSubscription{}, fmt.Errorf("订阅下载被安全策略拒绝（目标主机 %s）", safeSubscriptionHost(subURL))
		}
		return preparedSubscription{}, &subscriptionDownloadError{fmt.Sprintf("订阅下载失败（目标主机 %s）", safeSubscriptionHost(subURL))}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPartialContent {
		return preparedSubscription{}, fmt.Errorf("订阅响应不完整：HTTP 状态码 %d，拒绝导入或更新", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return preparedSubscription{}, &subscriptionDownloadError{fmt.Sprintf("订阅下载失败：HTTP 状态码 %d", resp.StatusCode)}
	}
	if resp.Header.Get("Content-Range") != "" {
		return preparedSubscription{}, fmt.Errorf("订阅响应不完整，拒绝导入或更新")
	}
	if resp.ContentLength > maxSubscriptionBytes {
		return preparedSubscription{}, fmt.Errorf("订阅内容过大，超过 %d 字节", maxSubscriptionBytes)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionBytes+1))
	if err != nil {
		return preparedSubscription{}, &subscriptionDownloadError{"读取订阅内容失败"}
	}
	if int64(len(b)) > maxSubscriptionBytes {
		return preparedSubscription{}, fmt.Errorf("订阅内容过大，超过 %d 字节", maxSubscriptionBytes)
	}
	return prepareSubscriptionBody(canonicalURL, b)
}

// subscriptionHTTPClient 构造抓取订阅用的 HTTP 客户端，带两层 SSRF 防护：
//  1. CheckRedirect 在每一跳重新校验协议，禁止 https 被重定向降级到非允许协议；
//  2. Dialer.Control 在 DNS 解析后、连接前校验目标 IP，默认拒绝环回/私网/链路本地/
//     CGNAT 等非公网地址（含云元数据 169.254.169.254），可防 DNS rebinding。
//     如确需抓取部署在内网的订阅，设置 PROXYSCENE_ALLOW_PRIVATE_SUBSCRIPTION=1。
func subscriptionHTTPClient(allowHTTP, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("无法解析订阅目标地址：%s", address)
			}
			if !isPublicIP(ip) {
				return fmt.Errorf("%w：目标不是公网地址", errSubscriptionPolicy)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Subscription paths and queries often contain credentials. Never send
			// their source URL as Referer, even for a same-origin redirect.
			req.Header.Del("Referer")
			if len(via) == 0 || !sameSubscriptionOrigin(req.URL, via[0].URL) {
				req.Header.Del("Authorization")
				req.Header.Del("Proxy-Authorization")
				req.Header.Del("Cookie")
			}
			if len(via) >= 10 {
				return fmt.Errorf("%w：订阅重定向次数过多", errSubscriptionPolicy)
			}
			scheme := strings.ToLower(req.URL.Scheme)
			if scheme == "https" || (allowHTTP && scheme == "http") {
				return nil
			}
			return fmt.Errorf("%w：订阅重定向协议不允许", errSubscriptionPolicy)
		},
	}
}

func sameSubscriptionOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectiveURLPort(a) == effectiveURLPort(b)
}

func effectiveURLPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

// isPublicIP 报告 ip 是否为可路由的公网地址。
func isPublicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicSubscriptionPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

var nonPublicSubscriptionPrefixes = []netip.Prefix{
	// IPv4 special-use, private, documentation, benchmarking and reserved space.
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// IPv6 local, transition, special protocol, documentation and reserved space.
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}
