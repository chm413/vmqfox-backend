package httpclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hulisang/vmqfox-backend/internal/config"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// notifyURLResolve 描述一次被注入的 DNS 解析结果。
// ips 为空且 err 为 nil 时模拟「域名存在但没有 A/AAAA 记录」。
type notifyURLResolve struct {
	ips []string
	err error
}

// notifyURLCase 是 ValidateNotifyURL 的一条表驱动用例。
//
// 每条用例都必须填 attack：它写成「删掉这条断言会打开哪个口子」。
// 没有这一栏的用例迟早会被人当成噪声删掉，而删掉的那一天不会有人发现闸门空了。
type notifyURLCase struct {
	name   string
	attack string

	rawURL       string
	allowHTTP    bool
	allowedCIDRs []string
	resolved     notifyURLResolve

	// wantLookup 为 true 断言必须走到 DNS 解析，为 false 断言在解析前就被拒。
	// IP 字面量若走到解析，说明「字面量一律拒绝」已被削弱成「解析后再判断」——
	// 那是把 URL 层的零成本拦截换成了依赖 DNS 的运行时拦截，属于降级。
	wantLookup bool

	wantErr       bool
	wantFragments []string
}

// mustCIDRs 把网段字符串转成白名单，忽略解析失败（测试自身错误直接 fail）。
func mustCIDRs(t *testing.T, blocks []string) []*net.IPNet {
	t.Helper()
	if len(blocks) == 0 {
		return nil
	}
	parsed := make([]*net.IPNet, 0, len(blocks))
	for _, block := range blocks {
		_, ipNet, err := net.ParseCIDR(block)
		if err != nil {
			t.Fatalf("用例配置了非法网段 %q: %v", block, err)
		}
		parsed = append(parsed, ipNet)
	}
	return parsed
}

// installNotifyURLResolver 把 lookupIP 换成单条用例的固定脚本，并在子用例结束时还原。
// 不联网是硬要求：真实 DNS 既不可控，也无法构造「公网域名解析到私网」这种 rebinding 首跳。
func installNotifyURLResolver(t *testing.T, resolved notifyURLResolve) *int {
	t.Helper()
	original := lookupIP
	t.Cleanup(func() { lookupIP = original })

	calls := new(int)
	lookupIP = func(_ context.Context, host string) ([]net.IPAddr, error) {
		*calls++
		if resolved.err != nil {
			return nil, resolved.err
		}
		addresses := make([]net.IPAddr, 0, len(resolved.ips))
		for _, raw := range resolved.ips {
			ip := net.ParseIP(raw)
			if ip == nil {
				t.Fatalf("用例配置了非法 IP %q", raw)
			}
			addresses = append(addresses, net.IPAddr{IP: ip})
		}
		return addresses, nil
	}
	return calls
}

// assertReadableError 断言错误信息可读：既要说清被拒原因，也要带上被拒的地址。
// 只断言 err != nil 是不够的——商户拿到「validate notify_url failed」这种文案
// 只能来提工单，运维也无从判断是配置错了还是被策略拦了。
func assertReadableError(t *testing.T, err error, wantFragments []string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝，实际通过了")
	}
	message := err.Error()
	if strings.TrimSpace(message) == "" {
		t.Fatalf("错误信息为空，调用方无法判断拒绝原因")
	}
	for _, fragment := range wantFragments {
		if !strings.Contains(message, fragment) {
			t.Errorf("错误信息 %q 未包含 %q（被拒原因不可读）", message, fragment)
		}
	}
	if len(wantFragments) == 0 {
		t.Errorf("用例没有声明期望的错误片段，等于只断言了 err != nil")
	}
}

func TestValidateNotifyURLRejectsSSRFTargets(t *testing.T) {
	cases := []notifyURLCase{
		// --- 在解析之前就该拒掉的形态 ---
		{
			name:          "空字符串",
			attack:        "空 notify_url 会让出站层失去目标判断，直接被当成「无需校验」",
			rawURL:        "",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"notify_url 不能为空"},
		},
		{
			name:          "纯空白字符串",
			attack:        "只靠 strings.TrimSpace 之外的路径判断，会把空白当合法 URL",
			rawURL:        "   \n\t  ",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"notify_url 不能为空"},
		},
		{
			name:          "无法解析的 URL",
			attack:        "畸形 URL 若被放行，出站层会用自己的解析器得出不同结论（解析器差分）",
			rawURL:        "://notify.example.com/epay",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"notify_url 不是合法 URL"},
		},
		{
			name:          "file 协议",
			attack:        "file:/// 读取本机文件，属于协议层的任意文件读取",
			rawURL:        "file:///etc/passwd",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"只允许 http/https"},
		},
		{
			name:          "协议相对 URL（无 scheme）",
			attack:        "//host/path 没有 scheme，出站层可能继承默认协议并绕过 scheme 策略",
			rawURL:        "//notify.example.com/epay",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"只允许 http/https"},
		},
		{
			name:          "AllowHTTP=false 时拒绝明文 http",
			attack:        "明文回调可被链路中间人篡改，把「支付失败」改写为「支付成功」",
			rawURL:        "http://notify.example.com/epay",
			allowHTTP:     false,
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"必须使用 https"},
		},
		{
			name:          "携带 userinfo 的 URL",
			attack:        "https://user:pass@host/ 在不同解析器里主机边界不同，且会把凭据写进日志",
			rawURL:        "https://user:pass@notify.example.com/epay",
			allowHTTP:     false,
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"不允许携带用户名或密码"},
		},
		{
			name:          "空主机名（https://）",
			attack:        "空主机名让出站层回落到默认或本地地址",
			rawURL:        "https://",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"缺少主机名"},
		},
		{
			name:          "空主机名（三斜杠形式）",
			attack:        "https:///path 把路径当主机，是最常见的 SSRF 写法之一",
			rawURL:        "https:///epay/notify",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"缺少主机名"},
		},

		// --- IP 字面量：没有域名就没有稳定身份，也绕开了对域名的策略 ---
		{
			name:          "IPv4 回环字面量",
			attack:        "直接打本机上的内部服务（管理端、监控、debug 端口）",
			rawURL:        "https://127.0.0.1/epay",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"不允许使用 IP 地址"},
		},
		{
			name:          "10/8 私网字面量",
			attack:        "直达内网主机，绕过边界防火墙",
			rawURL:        "https://10.0.0.5/epay",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"不允许使用 IP 地址"},
		},
		{
			name:          "172.16/12 私网字面量",
			attack:        "直达内网主机，绕过边界防火墙",
			rawURL:        "https://172.16.31.9/epay",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"不允许使用 IP 地址"},
		},
		{
			name:          "192.168/16 私网字面量",
			attack:        "直达内网主机，绕过边界防火墙",
			rawURL:        "https://192.168.1.10/epay",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"不允许使用 IP 地址"},
		},
		{
			name:          "云元数据地址字面量",
			attack:        "169.254.169.254 可读取实例临时凭据，SSRF 最高危目标",
			rawURL:        "https://169.254.169.254/latest/meta-data/iam/security-credentials/",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"不允许使用 IP 地址"},
		},
		{
			name:          "CGNAT 100.64/10 字面量",
			attack:        "运营商级 NAT 段常与内网互通，标准库 IsPrivate 不覆盖",
			rawURL:        "https://100.64.7.7/epay",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"不允许使用 IP 地址"},
		},
		{
			name:          "IPv6 回环字面量",
			attack:        "IPv6 回环同样能打到本机服务，只查 IPv4 的回环判断会漏",
			rawURL:        "https://[::1]/epay",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"不允许使用 IP 地址"},
		},
		{
			name:          "IPv4-mapped IPv6 字面量",
			attack:        "::ffff:127.0.0.1 用 IPv6 写法表达 IPv4 回环，绕过只匹配 IPv4 字面量的规则",
			rawURL:        "https://[::ffff:127.0.0.1]/epay",
			wantLookup:    false,
			wantErr:       true,
			wantFragments: []string{"不允许使用 IP 地址"},
		},

		// --- 域名解析到内网：DNS rebinding 的第一跳 ---
		{
			name:          "域名解析到回环",
			attack:        "攻击者控制的域名 A 记录指向 127.0.0.1，用域名身份穿过字面量检查",
			rawURL:        "https://rebind-loopback.example.com/epay",
			resolved:      notifyURLResolve{ips: []string{"127.0.0.1"}},
			wantLookup:    true,
			wantErr:       true,
			wantFragments: []string{"指向的地址不可作为回调目标", "私网或回环", "127.0.0.1"},
		},
		{
			name:          "域名解析到 10/8",
			attack:        "DNS rebinding 首跳：公网域名解析到内网地址后直达内网服务",
			rawURL:        "https://rebind-ten.example.com/epay",
			resolved:      notifyURLResolve{ips: []string{"10.1.2.3"}},
			wantLookup:    true,
			wantErr:       true,
			wantFragments: []string{"指向的地址不可作为回调目标", "私网或回环", "10.1.2.3"},
		},
		{
			name:          "域名解析到 172.16/12",
			attack:        "DNS rebinding 首跳：公网域名解析到内网地址后直达内网服务",
			rawURL:        "https://rebind-172.example.com/epay",
			resolved:      notifyURLResolve{ips: []string{"172.16.31.9"}},
			wantLookup:    true,
			wantErr:       true,
			wantFragments: []string{"指向的地址不可作为回调目标", "私网或回环", "172.16.31.9"},
		},
		{
			name:          "域名解析到 192.168/16",
			attack:        "DNS rebinding 首跳：公网域名解析到内网地址后直达内网服务",
			rawURL:        "https://rebind-192.example.com/epay",
			resolved:      notifyURLResolve{ips: []string{"192.168.1.10"}},
			wantLookup:    true,
			wantErr:       true,
			wantFragments: []string{"指向的地址不可作为回调目标", "私网或回环", "192.168.1.10"},
		},
		{
			name:          "域名解析到云元数据地址",
			attack:        "用域名包装 169.254.169.254 窃取实例凭据，必须在预校验就拦下",
			rawURL:        "https://rebind-metadata.example.com/latest/meta-data/",
			resolved:      notifyURLResolve{ips: []string{"169.254.169.254"}},
			wantLookup:    true,
			wantErr:       true,
			wantFragments: []string{"指向的地址不可作为回调目标", "云元数据服务地址", "169.254.169.254"},
		},
		{
			name:          "域名解析到 CGNAT 网段",
			attack:        "100.64/10 与内网常互通，且不在标准库私网判定内",
			rawURL:        "https://rebind-cgnat.example.com/epay",
			resolved:      notifyURLResolve{ips: []string{"100.64.7.7"}},
			wantLookup:    true,
			wantErr:       true,
			wantFragments: []string{"指向的地址不可作为回调目标", "保留网段", "100.64.7.7"},
		},
		{
			name:          "域名解析到 IPv6 回环",
			attack:        "AAAA 记录指向 ::1，绕过只检查 A 记录的实现",
			rawURL:        "https://rebind-loopback6.example.com/epay",
			resolved:      notifyURLResolve{ips: []string{"::1"}},
			wantLookup:    true,
			wantErr:       true,
			wantFragments: []string{"指向的地址不可作为回调目标", "私网或回环", "::1"},
		},
		{
			name:          "域名解析到 IPv4-mapped 回环",
			attack:        "AAAA 记录返回 ::ffff:127.0.0.1，必须经 To4 归一后命中回环判定",
			rawURL:        "https://rebind-mapped.example.com/epay",
			resolved:      notifyURLResolve{ips: []string{"::ffff:127.0.0.1"}},
			wantLookup:    true,
			wantErr:       true,
			wantFragments: []string{"指向的地址不可作为回调目标", "私网或回环", "127.0.0.1"},
		},
		{
			name:          "公网与私网混合解析结果",
			attack:        "同时返回一个合法公网地址和一个私网地址，若只看首个地址就会被绕过",
			rawURL:        "https://rebind-mixed.example.com/epay",
			resolved:      notifyURLResolve{ips: []string{"93.184.216.34", "10.0.0.5"}},
			wantLookup:    true,
			wantErr:       true,
			wantFragments: []string{"指向的地址不可作为回调目标", "10.0.0.5"},
		},
		{
			name:          "域名解析失败",
			attack:        "解析失败若被当成「无地址可拒」放行，等于用坏 DNS 关闭整条策略",
			rawURL:        "https://nxdomain.example.com/epay",
			resolved:      notifyURLResolve{err: errors.New("no such host")},
			wantLookup:    true,
			wantErr:       true,
			wantFragments: []string{"域名无法解析"},
		},
		{
			name:          "域名没有可用地址",
			attack:        "空解析结果绕过逐 IP 检查，等于一次都不校验就放行",
			rawURL:        "https://no-record.example.com/epay",
			resolved:      notifyURLResolve{ips: nil},
			wantLookup:    true,
			wantErr:       true,
			wantFragments: []string{"域名没有可用地址"},
		},

		// --- 必须放行的形态：策略过严会拒掉真实商户，比漏放更难排查 ---
		{
			name:       "公网域名 https 通过",
			attack:     "（对照）正常商户回调不能被误杀，否则下单全线失败",
			rawURL:     "https://notify.example.com/epay/callback",
			allowHTTP:  false,
			resolved:   notifyURLResolve{ips: []string{"93.184.216.34"}},
			wantLookup: true,
			wantErr:    false,
		},
		{
			name:       "AllowHTTP=true 时明文 http 通过",
			attack:     "（对照）运维显式放开 http 后必须真的放行，否则该开关形同虚设",
			rawURL:     "http://notify.example.com/epay/callback",
			allowHTTP:  true,
			resolved:   notifyURLResolve{ips: []string{"93.184.216.34"}},
			wantLookup: true,
			wantErr:    false,
		},
		{
			name:       "带端口的回调地址通过",
			attack:     "（对照）本函数不限制端口；若误加端口白名单会拒掉实际可投递的地址",
			rawURL:     "https://notify.example.com:8443/epay/callback",
			allowHTTP:  false,
			resolved:   notifyURLResolve{ips: []string{"93.184.216.34"}},
			wantLookup: true,
			wantErr:    false,
		},
		{
			name:       "无路径的裸域名通过",
			attack:     "（对照）根路径回调是常见配置，不应因为缺少 path 被拒",
			rawURL:     "https://notify.example.com",
			allowHTTP:  false,
			resolved:   notifyURLResolve{ips: []string{"93.184.216.34"}},
			wantLookup: true,
			wantErr:    false,
		},
		{
			name:       "公网 IPv6 回调地址通过",
			attack:     "（对照）只放行 IPv4 会让纯 IPv6 商户无法接入",
			rawURL:     "https://notify-v6.example.com/epay/callback",
			allowHTTP:  false,
			resolved:   notifyURLResolve{ips: []string{"2606:4700:4700::1111"}},
			wantLookup: true,
			wantErr:    false,
		},
		{
			name:         "白名单网段放行解析出的回环地址",
			attack:       "（对照）自建内网回调必须能用 AllowedCIDRs 显式放开，否则该逃生门是死代码",
			rawURL:       "https://sandbox.example.com/epay/callback",
			allowHTTP:    false,
			allowedCIDRs: []string{"127.0.0.1/32"},
			resolved:     notifyURLResolve{ips: []string{"127.0.0.1"}},
			wantLookup:   true,
			wantErr:      false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			lookupCalls := installNotifyURLResolver(t, testCase.resolved)
			allowedCIDRs := mustCIDRs(t, testCase.allowedCIDRs)

			err := ValidateNotifyURL(testCase.rawURL, testCase.allowHTTP, allowedCIDRs)

			if testCase.wantLookup && *lookupCalls == 0 {
				t.Errorf("用例期望走到 DNS 预校验，但 lookupIP 未被调用（策略可能被短路）")
			}
			if !testCase.wantLookup && *lookupCalls != 0 {
				t.Errorf("用例应在 DNS 解析之前就被拒绝，实际调用了 %d 次解析：%s",
					*lookupCalls, testCase.attack)
			}

			if !testCase.wantErr {
				if err != nil {
					t.Fatalf("期望通过，实际被拒: %v（该用例防的是：%s）", err, testCase.attack)
				}
				return
			}
			assertReadableError(t, err, testCase.wantFragments)
		})
	}
}

// TestValidateNotifyURLWhitelistStillRejectsLiteralIP 钉住一个容易改错的边界：
// AllowedCIDRs 放开的是「域名解析出的地址」，IP 字面量在任何情况下都不接受。
// 否则运维为了让内网回调可用，会顺带把「用 IP 直连」也放出去，域名身份校验彻底失效。
func TestValidateNotifyURLWhitelistStillRejectsLiteralIP(t *testing.T) {
	allowedCIDRs := mustCIDRs(t, []string{"127.0.0.1/32", "10.0.0.0/8"})

	for _, rawURL := range []string{
		"https://127.0.0.1/epay",
		"https://10.0.0.5/epay",
		"https://[::1]/epay",
	} {
		lookupCalls := installNotifyURLResolver(t, notifyURLResolve{ips: []string{"93.184.216.34"}})
		err := ValidateNotifyURL(rawURL, true, allowedCIDRs)
		assertReadableError(t, err, []string{"不允许使用 IP 地址"})
		if *lookupCalls != 0 {
			t.Errorf("%s 应在 DNS 解析前被拒，实际调用了 %d 次解析", rawURL, *lookupCalls)
		}
	}
}

// TestNotifyURLPolicyMatchesFreeFunction 防止 NotifyURLPolicy 与 ValidateNotifyURL 口径漂移：
// 用例层只依赖 NotifyURLPolicy，两条路径若分叉，预校验就会被绕过。
func TestNotifyURLPolicyMatchesFreeFunction(t *testing.T) {
	installNotifyURLResolver(t, notifyURLResolve{ips: []string{"10.1.2.3"}})
	policy := NotifyURLPolicy{AllowHTTP: false, AllowedCIDRs: nil}

	err := policy.ValidateNotifyURL("https://rebind.example.com/epay")
	assertReadableError(t, err, []string{"指向的地址不可作为回调目标", "私网或回环"})

	installNotifyURLResolver(t, notifyURLResolve{ips: []string{"93.184.216.34"}})
	if err := policy.ValidateNotifyURL("https://notify.example.com/epay"); err != nil {
		t.Fatalf("公网 https 地址经 NotifyURLPolicy 应通过，实际被拒: %v", err)
	}
	// AllowHTTP 必须真正传递到自由函数，而不是被 Policy 默认为 false 覆盖。
	allowPolicy := NotifyURLPolicy{AllowHTTP: true}
	if err := allowPolicy.ValidateNotifyURL("http://notify.example.com/epay"); err != nil {
		t.Fatalf("AllowHTTP=true 的 Policy 应放行 http，实际被拒: %v", err)
	}
}

// TestNotifierGuardsInjectedClient 守住最容易失效的一处 API 形状：
// NewNotifier 允许传入自定义 *http.Client，早期实现是「传了就直接用」，
// 于是注入一个 client 就能整体跳过拨号期 SSRF 拦截，只剩预校验。
// 生产当前传 nil，所以线上安全；但没有用例守住时，这个旁路会在某次重构里悄悄回来。
func TestNotifierGuardsInjectedClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	notifier := NewNotifier(&http.Client{}, config.OutboundConfig{AllowInsecureHTTP: true})
	_, err := notifier.Send(context.Background(), port.Notification{
		OrderID: "oid",
		URL:     server.URL,
		Method:  http.MethodPost,
		Body:    []byte("hello"),
	})
	if err == nil {
		t.Fatal("注入自定义 client 后仍必须拦截回环地址，实际请求被放行")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("应返回 SSRF 拦截错误，实际 %v", err)
	}
}

// TestNotifierStripsDialTLSAndProxyFromInjectedTransport 钉住 Clone 保留字段的清理：
// net/http 对 https 优先走 DialTLS/DialTLSContext（不清空则守卫根本不会被调用），
// Proxy 则让守卫校验的是代理 IP 而非真实目标。这两条都是「传入即绕过」的潜伏路径。
func TestNotifierStripsDialTLSAndProxyFromInjectedTransport(t *testing.T) {
	injected := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nil, nil
		},
	}
	notifier := NewNotifier(&http.Client{Transport: injected}, config.OutboundConfig{AllowInsecureHTTP: true})

	cloned, ok := notifier.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport 应被替换为 *http.Transport，实际 %T", notifier.client.Transport)
	}
	if cloned.DialContext == nil {
		t.Fatal("守卫 DialContext 必须已安装")
	}
	if cloned.DialTLS != nil || cloned.DialTLSContext != nil {
		t.Fatal("自定义 TLS 拨号必须被清除，否则 https 请求绕过 SSRF 守卫")
	}
	if cloned.Proxy != nil {
		t.Fatal("Proxy 必须被清除，否则守卫校验的是代理 IP 而非真实目标")
	}
}
