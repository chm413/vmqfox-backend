package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/config"
)

// notifyURLResolveTimeout 限制预校验里的 DNS 查询时长。
// 下单是同步路径，解析卡住等于把建单请求一起拖死；2 秒足够正常域名，
// 超时按「不可用」拒绝，比让商户等到网关超时更好排查。
const notifyURLResolveTimeout = 2 * time.Second

// lookupIP 抽成变量是为了让用例可以在不联网的情况下覆盖 DNS 结果
// （否则「域名解析到私网」这类分支只能靠真实网络碰运气）。
var lookupIP = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// ValidateNotifyURL 在下单期对 notify_url 做只读预校验，不发起任何 HTTP 请求。
//
// 为什么必须在下单期做：出站 notifier 会拒绝回环/私网/保留网段与云元数据地址，
// 若下单时不拦，就会变成「下单成功、付款成功、回调 100% 失败」——
// 而失败要等 20 次退避（约 8 小时）才进死信箱，用户的钱已经收了，商户却永远收不到通知。
//
// 判定与 checkIPSecurity 共用同一份策略，避免「预校验通过、实际投递被拦」的两套口径漂移：
//   - scheme 由 allowHTTP 决定（生产默认只允许 https）；
//   - 拒绝 userinfo（https://user:pass@host/ 会被日志与代理分别解释）；
//   - 拒绝 IP 字面量（没有域名就没有稳定身份，也绕开了对域名的策略）；
//   - 域名解析出的**每个** IP 都要过 checkIPSecurity，任一不合法即拒绝。
//
// 注意：本函数不做端口限制。出站策略本身不限端口，预校验若更严会拒掉
// 实际能回调成功的地址（假阴性），让商户在集成期遇到无法解释的「下单失败」。
func ValidateNotifyURL(raw string, allowHTTP bool, allowedCIDRs []*net.IPNet) error {
	text := strings.TrimSpace(raw)
	if text == "" {
		return errors.New("notify_url 不能为空")
	}

	parsed, err := url.Parse(text)
	if err != nil || parsed == nil {
		return errors.New("notify_url 不是合法 URL")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return errors.New("notify_url 必须使用 https")
		}
	default:
		return errors.New("notify_url 只允许 http/https")
	}
	if parsed.User != nil {
		return errors.New("notify_url 不允许携带用户名或密码")
	}

	host := parsed.Hostname()
	if host == "" {
		return errors.New("notify_url 缺少主机名")
	}
	if net.ParseIP(host) != nil {
		return errors.New("notify_url 不允许使用 IP 地址")
	}

	// 解析放在带超时的独立 context 里：本函数的签名不接收调用方 ctx（纯只读校验），
	// 不能因此让一次坏 DNS 拖住整个建单事务。
	ctx, cancel := context.WithTimeout(context.Background(), notifyURLResolveTimeout)
	defer cancel()
	addresses, err := lookupIP(ctx, host)
	if err != nil {
		return fmt.Errorf("notify_url 域名无法解析: %w", err)
	}
	if len(addresses) == 0 {
		return errors.New("notify_url 域名没有可用地址")
	}

	cfg := config.OutboundConfig{AllowInsecureHTTP: allowHTTP, AllowedCIDRs: allowedCIDRs}
	for _, address := range addresses {
		if err := checkIPSecurity(address.IP, cfg); err != nil {
			return fmt.Errorf("notify_url 指向的地址不可作为回调目标: %w", err)
		}
	}
	return nil
}

// NotifyURLPolicy 把预校验所需的配置固化成可传递的值。
//
// 用例层只依赖 `ValidateNotifyURL(raw string) error` 这一个方法（消费方定义的窄接口），
// 因此 usecase 不需要 import 本包，也不需要知道 CIDR/IP 策略的细节。
type NotifyURLPolicy struct {
	AllowHTTP    bool
	AllowedCIDRs []*net.IPNet
}

func (p NotifyURLPolicy) ValidateNotifyURL(raw string) error {
	return ValidateNotifyURL(raw, p.AllowHTTP, p.AllowedCIDRs)
}
