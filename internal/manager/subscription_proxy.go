package manager

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Connect only to the saved global inbound. Never inherit HTTP_PROXY or use a
// freshly overridden port which may not belong to the running Xray instance.
func subscriptionGlobalProxyAddress(cfg Config) (string, error) {
	ip := net.ParseIP(cfg.ProxyHost)
	if ip == nil || cfg.GlobalHTTPPort < 1 || cfg.GlobalHTTPPort > 65535 {
		return "", fmt.Errorf("当前全局代理监听地址无效")
	}
	if ip.IsUnspecified() {
		if ip.To4() != nil {
			ip = net.IPv4(127, 0, 0, 1)
		} else {
			ip = net.IPv6loopback
		}
	}
	return net.JoinHostPort(ip.String(), itoa(cfg.GlobalHTTPPort)), nil
}

func subscriptionProxyHTTPClient(allowHTTP, allowPrivate bool, proxyAddress string) *http.Client {
	client := subscriptionHTTPClient(allowHTTP, allowPrivate)
	dialer := &subscriptionProxyDialer{
		proxyAddress: proxyAddress,
		allowPrivate: allowPrivate,
		lookupIP:     net.DefaultResolver.LookupIPAddr,
	}
	client.Transport.(*http.Transport).DialContext = dialer.DialContext
	return client
}

type subscriptionProxyDialer struct {
	proxyAddress string
	allowPrivate bool
	lookupIP     func(context.Context, string) ([]net.IPAddr, error)
}

// Validate the final destination locally and pass its numeric IP to CONNECT.
// Passing a hostname to a conventional forward proxy would let its independent
// DNS resolution bypass the direct client's SSRF/DNS-rebinding protection.
// The HTTP transport retains the original Host and TLS ServerName, so TLS
// identity verification is unchanged. DNS failure stays a failure; it does not
// silently delegate unrestricted DNS to the proxy.
func (d *subscriptionProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("订阅目标地址无效")
	}
	var ips []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IPAddr{{IP: ip}}
	} else {
		ips, err = d.lookupIP(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("订阅域名解析失败")
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("订阅域名没有可用地址")
	}
	for _, ip := range ips {
		if ip.Zone != "" || ip.IP == nil || (!d.allowPrivate && !isPublicIP(ip.IP)) {
			return nil, fmt.Errorf("%w：代理下载目标不是允许的地址", errSubscriptionPolicy)
		}
	}
	for _, ip := range ips {
		conn, dialErr := d.connect(ctx, net.JoinHostPort(ip.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		err = dialErr
		if ctx.Err() != nil {
			break
		}
	}
	return nil, err
}

func (d *subscriptionProxyDialer) connect(ctx context.Context, target string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", d.proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("无法连接当前全局代理")
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	success := false
	defer func() {
		stopCancel()
		if !success {
			_ = conn.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: make(http.Header)}
	req.Header.Set("User-Agent", subscriptionUserAgent)
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("全局代理隧道请求失败")
	}
	// Bound proxy response headers as well as subscription bodies.
	limited := &io.LimitedReader{R: conn, N: (16 << 10) + 1}
	reader := bufio.NewReader(limited)
	resp, err := http.ReadResponse(reader, req)
	if err != nil || limited.N <= 0 {
		return nil, fmt.Errorf("全局代理隧道响应无效")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("全局代理隧道失败：HTTP 状态码 %d", resp.StatusCode)
	}
	if !stopCancel() || ctx.Err() != nil {
		return nil, fmt.Errorf("全局代理连接已取消")
	}
	limited.N = math.MaxInt64
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("全局代理连接不可用")
	}
	success = true
	return &subscriptionTunnelConn{Conn: conn, reader: reader}, nil
}

type subscriptionTunnelConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *subscriptionTunnelConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
