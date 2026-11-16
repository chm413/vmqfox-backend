package httpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/config"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

const maxNotificationResponseBytes = 1024 * 1024

type Notifier struct {
	client *http.Client
	// allowInsecureHTTP 决定是否放行 http 出站；默认 false，只接受 https。
	allowInsecureHTTP bool
}

// NewNotifier 创建具备 SSRF 防御能力的通知出站 HTTP 客户端
func NewNotifier(client *http.Client, outboundCfg ...config.OutboundConfig) *Notifier {
	var cfg config.OutboundConfig
	if len(outboundCfg) > 0 {
		cfg = outboundCfg[0]
	}

	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	// guardedDial 是三层 SSRF 防护里的第三层：拨号前再校验一次解析结果，
	// 因此即使 ValidateNotifyURL 被绕过、或 DNS 在校验后改指向内网（rebinding），
	// 这里仍会拒绝。它必须**无条件安装**——一旦允许「传入自定义 client 就跳过」，
	// 这个 API 的默认安全就会被一次注入悄悄取消，且没有任何信号。
	guardedDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, errors.New("无法解析目标主机 IP")
		}

		// 检查解析出的每个 IP 是否安全（防止 DNS Rebinding 逃逸）
		for _, ipAddr := range ips {
			if err := checkIPSecurity(ipAddr.IP, cfg); err != nil {
				// 拦截细节（含具体 IP）只进服务端日志；对外错误不带 IP——
				// 这个错误会原样落进通知 dead-letter 的 last_error，管理端可读。
				log.Printf("[notifier] SSRF 拦截 %s: %v", addr, err)
				return nil, errors.New("SSRF 拦截: 目标地址不可达")
			}
		}

		// 使用解析出的首个已校验 IP 建立连接。
		// TLS 握手仍以请求 URL 中的主机名做 SNI 与证书校验，因此不会削弱 HTTPS 验证。
		targetAddr := net.JoinHostPort(ips[0].IP.String(), port)
		return dialer.DialContext(ctx, network, targetAddr)
	}

	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{
				DialContext:           guardedDial,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   5 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
			Timeout: 10 * time.Second,
		}
	} else {
		clientCopy := *client
		// 调用方传入的 client 只沿用其超时/连接池等设置，拨号守卫一律重新安装。
		// 自定义 RoundTripper（非 *http.Transport）无法安全地保留，退回默认 transport：
		// 宁可丢配置，也不留一条「注入即失效」的旁路。
		if base, ok := clientCopy.Transport.(*http.Transport); ok && base != nil {
			cloned := base.Clone()
			cloned.DialContext = guardedDial
			// Clone 会保留原 Transport 的 DialTLS/DialTLSContext（net/http 对 https
			// 优先走 TLS 拨号，DialContext 根本不会被调用）与 Proxy（有代理时拨号目标是
			// 代理地址，guardedDial 校验的是代理 IP 而不是真实目标）。这两个字段必须清空：
			// TLS 交回标准握手（证书校验不受影响），直连目标由守卫校验。
			// 代价是丢掉调用方的 mTLS/代理配置——对通知出站这是正确取舍。
			cloned.DialTLS = nil
			cloned.DialTLSContext = nil
			cloned.Proxy = nil
			clientCopy.Transport = cloned
		} else {
			if clientCopy.Transport != nil {
				log.Printf("[notifier] 自定义 RoundTripper (%T) 无法安装 SSRF 拨号守卫，已替换为内置 transport", clientCopy.Transport)
			}
			clientCopy.Transport = &http.Transport{DialContext: guardedDial}
		}
		client = &clientCopy
	}

	// 重定向可能改变方法或导致未授权跳转，始终保留原始 3xx 响应
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Notifier{client: client, allowInsecureHTTP: cfg.AllowInsecureHTTP}
}

// reservedCIDRs 是默认拒绝的保留网段。
// Go 标准库的 IsLoopback/IsPrivate/IsLinkLocal* 已覆盖多数内网地址，
// 这里补齐它未覆盖但同样不应作为商户回调目标的范围。
var reservedCIDRs = func() []*net.IPNet {
	blocks := []string{
		"100.64.0.0/10",   // RFC 6598 运营商级 NAT
		"192.0.0.0/24",    // RFC 6890 IETF 协议分配
		"192.0.2.0/24",    // RFC 5737 文档示例
		"198.18.0.0/15",   // RFC 2544 基准测试
		"198.51.100.0/24", // RFC 5737 文档示例
		"203.0.113.0/24",  // RFC 5737 文档示例
		"240.0.0.0/4",     // 保留未分配
		"::/128",          // IPv6 未指定
		"64:ff9b::/96",    // NAT64，可用于折射到 IPv4 内网
		"100::/64",        // RFC 6666 丢弃前缀
		"2001:db8::/32",   // 文档示例
		"fe80::/10",       // IPv6 链路本地
		"fec0::/10",       // 已废弃的站点本地
		"fd00:ec2::/32",   // 部分云厂商的 IPv6 元数据段
	}
	parsed := make([]*net.IPNet, 0, len(blocks))
	for _, block := range blocks {
		if _, ipNet, err := net.ParseCIDR(block); err == nil {
			parsed = append(parsed, ipNet)
		}
	}
	return parsed
}()

// metadataIPs 是需要单独点名拒绝的云元数据地址。
// 这些地址一旦可达，即可读取实例凭据，属于 SSRF 最高危目标。
var metadataIPs = []string{"169.254.169.254", "fd00:ec2::254"}

func checkIPSecurity(ip net.IP, cfg config.OutboundConfig) error {
	if ip == nil {
		return errors.New("无效的目标 IP")
	}

	// 1. 优先判断是否显式命中了白名单（如果配置了 127.0.0.1 或指定私网网段则直接放行）
	for _, allowedIP := range cfg.AllowedIPs {
		if allowedIP.Equal(ip) {
			return nil
		}
	}
	for _, cidr := range cfg.AllowedCIDRs {
		if cidr.Contains(ip) {
			return nil
		}
	}

	// 2. 高危元数据地址与未指定地址（未显式白名单时绝对拒绝）
	if ip.IsUnspecified() {
		return fmt.Errorf("目标地址属于敏感或受限地址: %s", ip.String())
	}
	for _, metadata := range metadataIPs {
		if ip.Equal(net.ParseIP(metadata)) {
			return fmt.Errorf("目标地址属于云元数据服务地址: %s", ip.String())
		}
	}

	// 3. 默认阻止私网、回环、链路本地等内网地址。
	//    IPv4-mapped IPv6（如 ::ffff:127.0.0.1）经标准库的 To4 归一后同样命中这些判定。
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return fmt.Errorf("目标地址属于未白名单放行的私网或回环地址: %s", ip.String())
	}

	// 4. 阻止其余保留网段，避免通过 NAT64、CGNAT 等路径折射回内部资源。
	for _, cidr := range reservedCIDRs {
		if cidr.Contains(ip) {
			return fmt.Errorf("目标地址属于保留网段: %s", ip.String())
		}
	}

	return nil
}

// checkScheme 生产默认只允许 https 出站。
// 明文 http 会把签名后的回调内容暴露给链路中间人，必须由运维显式承担该风险后才放行。
func (n *Notifier) checkScheme(scheme string) error {
	switch scheme {
	case "https":
		return nil
	case "http":
		if n.allowInsecureHTTP {
			return nil
		}
		return errors.New("通知地址必须使用 https；如确需兼容明文 http，请显式设置 VMQ_NOTIFY_ALLOW_HTTP=true")
	default:
		return fmt.Errorf("不支持的通知协议 Scheme: %s", scheme)
	}
}

func (n *Notifier) Send(ctx context.Context, notification port.Notification) (port.NotificationResult, error) {
	if notification.URL == "" || notification.Method == "" {
		return port.NotificationResult{}, errors.New("通知 URL 或方法为空")
	}

	parsedURL, err := url.Parse(notification.URL)
	if err != nil {
		return port.NotificationResult{}, fmt.Errorf("通知 URL 无效: %w", err)
	}
	if err := n.checkScheme(parsedURL.Scheme); err != nil {
		return port.NotificationResult{}, err
	}
	if parsedURL.Host == "" {
		return port.NotificationResult{}, errors.New("通知 URL 缺少主机名")
	}

	request, err := http.NewRequestWithContext(
		ctx,
		notification.Method,
		notification.URL,
		bytes.NewReader(notification.Body),
	)
	if err != nil {
		return port.NotificationResult{}, err
	}
	for key, value := range notification.Headers {
		request.Header.Set(key, value)
	}

	response, err := n.client.Do(request)
	if err != nil {
		return port.NotificationResult{}, err
	}
	defer response.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxNotificationResponseBytes+1))
	if len(body) > maxNotificationResponseBytes {
		body = body[:maxNotificationResponseBytes]
		if readErr == nil {
			readErr = errors.New("通知响应正文超过限制")
		}
	}
	return port.NotificationResult{StatusCode: response.StatusCode, Body: body}, readErr
}

var _ port.Notifier = (*Notifier)(nil)
