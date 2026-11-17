import React from 'react'
import { Card, CardHeader, CardTitle, CardContent } from '@/components/ui/card'
import { BookOpen, Code2, ShieldAlert, Webhook, ArrowLeftRight, Store } from 'lucide-react'

/**
 * 商户对接文档。
 * 内容必须与 Go 侧实现保持一致：
 *  - 建单签名域见 internal/domain/payment.CreateSignV2
 *  - 回调与回跳签名域见 internal/domain/payment.CallbackSignV2
 *  - 公开收银台路由见 internal/http/router.go
 */
export const ApiDocView: React.FC = () => {
  return (
    <div className="space-y-6">
      <Card className="p-6">
        <CardHeader className="p-0 pb-6">
          <div className="flex items-center gap-2">
            <div className="p-2 rounded-2xl bg-primary/10 text-primary">
              <BookOpen className="size-4" />
            </div>
            <div>
              <CardTitle className="text-base">VMQ API 协议对接文档</CardTitle>
              <p className="text-xs text-muted-foreground mt-0.5">商户系统发起支付与接收异步回调指南</p>
            </div>
          </div>
        </CardHeader>

        <CardContent className="p-0 space-y-6 text-sm">
          {/* 1. 创建订单 */}
          <div className="space-y-3">
            <h3 className="font-bold text-foreground flex items-center gap-1.5">
              <Code2 className="size-4 text-primary" /> 1. 创建订单接口
            </h3>
            <div className="p-3 bg-muted/40 rounded-2xl border border-border/70 font-mono text-xs">
              <span className="text-emerald-600 dark:text-emerald-400 font-bold">POST</span> /api/order/create
            </div>
            <div className="text-xs text-muted-foreground leading-relaxed space-y-1.5">
              <div>
                请求参数：<code>payId</code>（商户唯一订单号）、<code>type</code>（1 微信 / 2 支付宝）、
                <code>price</code>（金额，两位小数字符串）、<code>param</code>（自定义透传参数）、
                <code>notifyUrl</code>、<code>returnUrl</code>、<code>sign</code>。
              </div>
              <div>
                <code>notifyUrl</code> 与 <code>returnUrl</code> 留空时使用后台系统设置中的默认值，
                但签名必须按<b>实际提交的值</b>计算：留空即以空字符串参与签名。
              </div>
              <div>
                响应 <code>data</code> 包含 <code>publicToken</code> 与 <code>redirectUrl</code>；
                请引导买家访问 <code>redirectUrl</code>，不要自行拼接内部订单号。
                传入 <code>isHtml=1</code> 时后端直接返回跳转页面。
              </div>
            </div>
          </div>

          {/* 2. 签名算法 */}
          <div className="space-y-3">
            <h3 className="font-bold text-foreground flex items-center gap-1.5">
              <ShieldAlert className="size-4 text-amber-500" /> 2. V2 签名算法 (HMAC-SHA-256)
            </h3>
            <div className="p-4 bg-muted/50 rounded-2xl border border-border/70 space-y-2 font-mono text-xs leading-relaxed">
              <div className="text-muted-foreground">// 建单签名域，字段顺序固定，密钥不拼入明文</div>
              <div className="text-primary font-semibold break-all">
                canonical = "payId=" + payId + "&amp;param=" + param + "&amp;type=" + type
                + "&amp;price=" + price + "&amp;notifyUrl=" + notifyUrl + "&amp;returnUrl=" + returnUrl
              </div>
              <div>sign = HMAC_SHA256(canonical, key) 的小写十六进制</div>
            </div>
            <div className="p-3 rounded-2xl bg-amber-500/10 border border-amber-500/20 text-xs text-amber-700 dark:text-amber-400 leading-relaxed">
              注意区分两套「v1」：这里指 <b>VMQ 原生协议</b>的旧版 MD5 签名
              （<code>payId + param + type + price + key</code>），已停止受理，服务端会返回明确的升级提示；
              下文第 5 节的 <b>EPay V1</b> 是另一套协议（独角数卡兼容，MD5 规则不同），两者互不影响。
            </div>
          </div>

          {/* 3. 异步回调 */}
          <div className="space-y-3">
            <h3 className="font-bold text-foreground flex items-center gap-1.5">
              <Webhook className="size-4 text-primary" /> 3. 异步通知回调 (Webhook)
            </h3>
            <div className="text-xs text-muted-foreground leading-relaxed space-y-1.5">
              <div>
                买家付款后，系统向 <code>notifyUrl</code> 推送
                <code>payId</code>、<code>param</code>、<code>type</code>、<code>price</code>、
                <code>reallyPrice</code> 与 <code>sign</code>。
                商户处理成功后必须返回纯文本 <code>success</code>，其它响应视为失败并进入重试。
              </div>
              <div>
                回调 <code>payId</code> 是<b>商户订单号</b>，请以它为对账主键。
              </div>
              <div>
                出站通知默认只允许 <code>https</code> 且拒绝内网地址，请确保回调地址为公网可达的 HTTPS。
              </div>
            </div>
            <div className="p-4 bg-muted/50 rounded-2xl border border-border/70 space-y-2 font-mono text-xs leading-relaxed">
              <div className="text-muted-foreground">// 回调与同步回跳共用同一签名域</div>
              <div className="text-primary font-semibold break-all">
                canonical = "payId=" + payId + "&amp;param=" + param + "&amp;type=" + type
                + "&amp;price=" + price + "&amp;reallyPrice=" + reallyPrice
              </div>
              <div>sign = HMAC_SHA256(canonical, key) 的小写十六进制</div>
            </div>
          </div>

          {/* 4. 收银台与回跳 */}
          <div className="space-y-3">
            <h3 className="font-bold text-foreground flex items-center gap-1.5">
              <ArrowLeftRight className="size-4 text-primary" /> 4. 收银台查询与同步回跳
            </h3>
            <div className="p-3 bg-muted/40 rounded-2xl border border-border/70 font-mono text-xs space-y-1">
              <div>GET /api/order/get/&#123;publicToken&#125;</div>
              <div>GET /api/order/check/&#123;publicToken&#125;</div>
              <div>GET /api/order/return-url/&#123;publicToken&#125;</div>
            </div>
            <div className="text-xs text-muted-foreground leading-relaxed space-y-1.5">
              <div>
                <code>publicToken</code> 是高熵随机凭据，只能放在路径中，不要写入日志或第三方统计。
              </div>
              <div>
                回跳地址只能通过 <code>return-url</code> 接口获取，且服务端仅在订单已支付时才签发；
                公开订单响应本身不包含回跳地址、透传参数与通知地址。
              </div>
            </div>
          </div>

          {/* 5. EPay 商户接入 */}
          <div className="space-y-3">
            <h3 className="font-bold text-foreground flex items-center gap-1.5">
              <Store className="size-4 text-primary" /> 5. EPay 商户接入（V1 / V2）
            </h3>
            <div className="text-xs text-muted-foreground leading-relaxed space-y-1.5">
              <div>
                在「商户管理」创建商户后，你会拿到 <code>商户号(pid)</code> 与一次性的
                <code>商户密钥</code>。密钥只显示一次，丢失只能轮换。
              </div>
              <div>
                我们的 EPay V1 与独角数卡等主流 EPay 插件兼容，三个入口都挂在根路径：
              </div>
            </div>
            <div className="p-3 bg-muted/40 rounded-2xl border border-border/70 font-mono text-xs space-y-1">
              <div><span className="text-emerald-600 dark:text-emerald-400 font-bold">GET</span> /submit.php —— 页面跳转下单</div>
              <div><span className="text-emerald-600 dark:text-emerald-400 font-bold">POST</span> /mapi.php —— API 下单（返回 JSON 含 payurl/qrcode）</div>
              <div><span className="text-emerald-600 dark:text-emerald-400 font-bold">GET</span> /api.php?act=order —— 查询单笔订单</div>
            </div>
            <div className="p-4 bg-muted/50 rounded-2xl border border-border/70 space-y-2 font-mono text-xs leading-relaxed">
              <div className="text-muted-foreground">// V1 签名：参数名 ASCII 升序 + 原值拼接 + 末尾拼密钥，MD5 小写</div>
              <div className="text-primary font-semibold break-all">
                canonical = ksort(params) 后 "k1=v1&amp;k2=v2&amp;..." + key
              </div>
              <div>sign = md5(canonical)，回传 sign 与 sign_type=MD5</div>
            </div>
            <div className="p-3 rounded-2xl bg-amber-500/10 border border-amber-500/20 text-xs text-amber-700 dark:text-amber-400 leading-relaxed space-y-1.5">
              <div>
                <b>参数值里不允许出现 &amp; 与控制字符</b>，否则请求会被判为「签名错误」。
                业务数据请放进 <code>param</code> 并自行 URL 编码；<code>notify_url</code> 请只用路径，
                不要带多个查询参数。
              </div>
              <div>
                异步回调是 <b>GET</b>：我们会带 <code>pid/trade_no/out_trade_no/type/name/money/trade_status/param/sign/sign_type</code>
                请求你的 <code>notify_url</code>，你返回纯文本 <code>success</code> 即视为送达。
              </div>
            </div>
          </div>

          {/* 5.1 EPay V2 */}
          <div className="space-y-3">
            <h3 className="font-bold text-foreground flex items-center gap-1.5">
              <Store className="size-4 text-primary" /> 5.1 EPay V2（RSA 双向签名）
            </h3>
            <div className="text-xs text-muted-foreground leading-relaxed space-y-1.5">
              <div>
                开通时选 V2 并录入<b>商户自己的公钥</b>（PKIX PEM，2048 或 4096 位）；创建成功后会拿到
                <b>平台公钥</b>，请配置到商户面板，用于验证我们的响应与回调。
              </div>
              <div>
                下单与验签参数跟 V1 完全一样，额外带 <code>timestamp</code>（10 位秒级）参与签名；
                签名用<b>商户私钥</b>做 SHA256withRSA，<code>sign</code> 为 Base64，<code>sign_type=RSA</code>。
              </div>
            </div>
            <div className="p-4 bg-muted/50 rounded-2xl border border-border/70 space-y-2 font-mono text-xs leading-relaxed">
              <div className="text-muted-foreground">// V2 待签串与 V1 同构：剔 sign/sign_type、剔空值、ASCII 升序、值不 urlencode</div>
              <div className="text-primary font-semibold break-all">
                canonical = ksort(params 含 timestamp) 后 "k1=v1&amp;k2=v2&amp;..."
              </div>
              <div className="text-primary font-semibold break-all">
                sign = base64( RSA_SHA256( canonical ) )
              </div>
              <div className="text-amber-600 dark:text-amber-400">
                // timestamp 是普通参数，参与 ASCII 排序；不要在末尾追加它
              </div>
            </div>
            <div className="p-3 rounded-2xl bg-amber-500/10 border border-amber-500/20 text-xs text-amber-700 dark:text-amber-400 leading-relaxed space-y-1.5">
              <div>
                <b>时间戳窗口 ±300 秒</b>：超窗的请求即使签名正确也按「签名错误」拒绝，
                商户服务器必须做 NTP 对时，否则极易误判成密钥配错。
              </div>
              <div>
                <b>成功响应</b>带 <code>timestamp</code> 和平台私钥签名：<code>mapi.php</code> 签
                <code>code/trade_no/payurl/qrcode</code>，<code>api.php</code> 签查单字段集，回调签
                <code>pid/trade_no/out_trade_no/type/name/money/trade_status/param</code>；空值不参与签名。
                <b>错误响应不签名</b>：先看 <code>code</code>，<code>code=1</code> 才验签。
              </div>
              <div>
                V2 回调是 <b>POST + application/x-www-form-urlencoded</b>（V1 是 GET）。
                回调端点必须接受 POST form，否则会 404/405，表现为「收不到回调」。
              </div>
              <div>
                <b>接入前请先与商户联调确认调用地址</b>：本网关 V2 复用 V1 的
                <code>submit.php</code>/<code>mapi.php</code>/<code>api.php</code>，
                暂未实现部分彩虹/ezfp 文档中的 <code>/api/pay/*</code> 路径。
              </div>
            </div>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
