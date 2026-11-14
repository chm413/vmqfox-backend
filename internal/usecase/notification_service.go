package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hulisang/vmqfox-backend/internal/domain/epay"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// 通知主题白名单。三个 topic 各对应一条互不相同的投递路径，
// 且 (event_key, topic) 是 outbox 的唯一键，因此三者天然隔离、互不顶替。
const (
	// notificationTopic 是 VMQ 原生回调：POST form + HMAC-SHA-256，签名在入队时固化。
	notificationTopic = "order.paid.notify"
	// epayV1NotifyTopic 是 EPay V1 回调：GET + MD5，签名在入队时固化进完整 URL。
	epayV1NotifyTopic = "epay.v1.notify"
	// epayV2NotifyTopic 是 EPay V2 回调：POST form + RSA。timestamp 参与签名且商户侧
	// 按 ±300s 校验，而 outbox 的退避从第 4 次（约 7 分钟）起必然过期，
	// 所以 V2 只能在投递时重签（设计 §6.2/§6.3）。
	epayV2NotifyTopic = "epay.v2.notify"
)

// 通知载荷版本。缺省（0）一律按旧结构解析 —— 库里已有的消息必须能继续投递。
const (
	// notifyPayloadVersionLegacy 是旧结构：仅 orderId/notifyUrl/newForm，走 POST。
	notifyPayloadVersionLegacy = 1
	// notifyPayloadVersionEPay 是 EPay 结构：按 method/url/body 投递。
	notifyPayloadVersionEPay = 2
)

// epayNameMaxBytes 对齐设计 §5.2 的 name 截断规则。
const epayNameMaxBytes = 127

type NotificationServiceDeps struct {
	Transactions port.TransactionManager
	Orders       port.OrderRepository
	Outbox       port.OutboxRepository
	Notifier     port.Notifier
	Clock        port.Clock
	// MerchantKeys / MasterKeys 只服务 EPay V2 的「投递时重签」。
	// 原生与 V1 的签名在入队时已固化，投递侧与密钥表完全解耦。
	// 二者缺失不阻塞构造（原生通道照常工作），只让 V2 退回入队时的预签快照。
	MerchantKeys port.MerchantKeyRepository
	MasterKeys   *secret.Box
	// MaxAttempts 是单条通知的最大投递次数，超过后转入终态，避免无限重试。
	MaxAttempts int
}

type NotificationService struct {
	transactions port.TransactionManager
	orders       port.OrderRepository
	outbox       port.OutboxRepository
	notifier     port.Notifier
	clock        port.Clock
	merchantKeys port.MerchantKeyRepository
	masterKeys   *secret.Box
	maxAttempts  int
}

// defaultNotificationMaxAttempts 与指数退避配合，覆盖约 8 小时的商户不可用窗口。
const defaultNotificationMaxAttempts = 20

type DeliveryStats struct {
	Claimed   int
	Delivered int
	Failed    int
	// Dead 统计本次被直接转终态的消息（未知 topic、载荷生成失败的占位消息）。
	Dead int
}

// NotificationPayload 是 outbox 里通知消息的持久化载荷。
//
// 版本化是为了让「库里已经存在的旧消息」继续按旧语义投递：旧载荷只有
// orderId/notifyUrl/newForm，Version 缺失时按 notifyPayloadVersionLegacy 处理。
type NotificationPayload struct {
	Version int `json:"v,omitempty"`
	// OrderID / NotifyURL 是消息与订单的关联键。生产端必须始终写 notifyUrl——
	// 旧版 worker 以「notifyUrl 为空」判定该订单无需通知。
	OrderID   string `json:"orderId"`
	NotifyURL string `json:"notifyUrl"`
	// NewForm 是 VMQ 原生与 EPay V2 的预签名 form（POST body）。
	NewForm string `json:"newForm,omitempty"`
	// Method 只允许 GET/POST；缺省按 POST 处理，兼容没有该字段的旧载荷。
	Method string `json:"method,omitempty"`
	// URL 是已带签名参数的完整地址，EPay V1 用它做 GET 投递。
	URL string `json:"url,omitempty"`
	// Body 是 EPay V2 的字段快照（JSON，不含 sign/timestamp），投递时据此重签。
	Body string `json:"body,omitempty"`
	// MerchantID 是 V2 重签时查密钥的依据；原生载荷不会写（omitempty 保持旧载荷字节不变）。
	MerchantID int64 `json:"merchantId,omitempty"`
	// Dead 标记「载荷生成失败」的占位消息：投递侧只转终态，不发起任何请求，
	// 供管理端按 protocol 重签（设计 §6.6）。Reason 写入 last_error 供排查。
	Dead   bool   `json:"dead,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// notifyRoute 是 topic 归一化后的投递路径。
type notifyRoute int

const (
	notifyRouteNative notifyRoute = iota
	notifyRouteEPayV1
	notifyRouteEPayV2
)

// notifyOutcome 把「这条消息投递成功了吗」与「这条消息还值得重试吗」分开。
// dead 表示无论重试多少次都不会成功（未知 topic、载荷生成失败的占位消息），
// 这种消息只能转终态，且绝不允许经由 settleFailed 去改订单状态。
type notifyOutcome struct {
	delivered bool
	dead      bool
	reason    string
}

func NewNotificationService(deps NotificationServiceDeps) (*NotificationService, error) {
	if deps.Transactions == nil || deps.Orders == nil || deps.Outbox == nil || deps.Notifier == nil || deps.Clock == nil {
		return nil, errors.New("通知用例依赖不完整")
	}
	maxAttempts := deps.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultNotificationMaxAttempts
	}
	return &NotificationService{
		transactions: deps.Transactions,
		orders:       deps.Orders,
		outbox:       deps.Outbox,
		notifier:     deps.Notifier,
		clock:        deps.Clock,
		merchantKeys: deps.MerchantKeys,
		masterKeys:   deps.MasterKeys,
		maxAttempts:  maxAttempts,
	}, nil
}

func (s *NotificationService) DeliverNext(ctx context.Context, leaseDuration time.Duration) (DeliveryStats, error) {
	if leaseDuration <= 0 {
		return DeliveryStats{}, fail(CodeConfiguration, "通知任务租约时长无效")
	}
	now := s.clock.Now()
	messages, err := s.outbox.ClaimPending(ctx, now, 1, leaseDuration)
	if err != nil {
		return DeliveryStats{}, wrap(CodeDependency, "领取通知任务失败", err)
	}

	stats := DeliveryStats{Claimed: len(messages)}
	for _, message := range messages {
		outcome := s.deliver(ctx, message)
		if outcome.delivered {
			if err := s.settleDelivered(ctx, message, s.clock.Now()); err != nil {
				return stats, err
			}
			stats.Delivered++
			continue
		}
		if outcome.dead {
			if err := s.settleDead(ctx, message, outcome.reason); err != nil {
				return stats, err
			}
			stats.Dead++
			continue
		}
		if err := s.settleFailed(ctx, message, outcome.reason, s.clock.Now()); err != nil {
			return stats, err
		}
		stats.Failed++
	}
	return stats, nil
}

func (s *NotificationService) deliver(ctx context.Context, message port.OutboxMessage) notifyOutcome {
	route, ok := notifyRouteFor(message.Topic)
	if !ok {
		return notifyOutcome{dead: true, reason: "不支持的通知主题: " + message.Topic}
	}
	var payload NotificationPayload
	if err := json.Unmarshal(message.Payload, &payload); err != nil {
		return notifyOutcome{reason: "通知载荷无效: " + err.Error()}
	}
	if payload.Dead {
		// 载荷生成阶段就失败的消息：订单侧已置通知失败，这里只把消息转为终态。
		reason := payload.Reason
		if reason == "" {
			reason = "通知载荷生成失败"
		}
		return notifyOutcome{dead: true, reason: reason}
	}
	if payload.NotifyURL == "" && payload.URL == "" {
		// 旧语义：该订单不需要通知，直接完成。
		// 但必须同时确认载荷里也没有待发内容，否则就是「回调没发出去、消息却标记已投递」的静默路径。
		if payload.NewForm == "" && payload.Body == "" {
			return notifyOutcome{delivered: true}
		}
		return notifyOutcome{reason: "通知载荷缺少回调地址"}
	}

	request, reason := s.buildRequest(ctx, route, payload)
	if reason != "" {
		return notifyOutcome{reason: reason}
	}
	result, err := s.notifier.Send(ctx, request)
	if notificationSucceeded(result, err) {
		return notifyOutcome{delivered: true}
	}
	return notifyOutcome{reason: notificationFailure(result, err)}
}

// buildRequest 按 topic 与载荷版本组装出站请求。空值校验在这里统一执行：
// notifyUrl 非空时，url/newForm/body 至少一个非空，否则判定失败。
func (s *NotificationService) buildRequest(ctx context.Context, route notifyRoute, payload NotificationPayload) (port.Notification, string) {
	method := strings.ToUpper(strings.TrimSpace(payload.Method))
	if method == "" {
		// 旧载荷没有 method 字段，历史行为是 POST form；V1 的协议语义固定是 GET。
		method = http.MethodPost
		if route == notifyRouteEPayV1 {
			method = http.MethodGet
		}
	}
	if method != http.MethodGet && method != http.MethodPost {
		return port.Notification{}, "通知载荷的请求方法不在白名单内: " + payload.Method
	}
	if payload.URL == "" && payload.NewForm == "" && payload.Body == "" {
		return port.Notification{}, "通知载荷缺少预签名请求参数"
	}

	switch route {
	case notifyRouteEPayV1:
		// EPay 生态的商户端多数只实现了 GET 接收，POST 会直接 404（设计 §6.1）。
		if method != http.MethodGet {
			return port.Notification{}, "EPay V1 通知必须使用 GET"
		}
		if payload.URL == "" {
			return port.Notification{}, "EPay V1 通知载荷缺少预签名地址"
		}
		// V1 无时效字段，入队时的签名原样发出：GET 不带 body，也不设 Content-Type。
		return port.Notification{
			OrderID: payload.OrderID,
			URL:     payload.URL,
			Method:  http.MethodGet,
		}, ""

	case notifyRouteEPayV2:
		if method != http.MethodPost {
			return port.Notification{}, "EPay V2 通知必须使用 POST"
		}
		form, reason := s.resignEPayV2(ctx, payload)
		if reason != "" {
			// 重签不可用（缺依赖/密钥/解密失败）时退回入队时预签的 form：
			// 它只在商户侧 ±300s 窗口内有效，属于尽力而为，不改变「重签优先」的次序。
			if payload.NewForm == "" {
				return port.Notification{}, reason
			}
			form = payload.NewForm
		}
		return postFormRequest(payload.OrderID, payload.NotifyURL, form), ""
	}

	// 原生 / 旧载荷：POST form 原样发，行为与改造前逐字节一致。
	if method == http.MethodGet {
		if payload.URL == "" {
			return port.Notification{}, "GET 通知缺少完整地址"
		}
		return port.Notification{OrderID: payload.OrderID, URL: payload.URL, Method: http.MethodGet}, ""
	}
	if payload.NewForm == "" {
		// 原生载荷的 body 只能是已签名的 form；Body 字段是 V2 的字段快照，不能当 form 发。
		return port.Notification{}, "原生通知载荷缺少预签名请求参数"
	}
	return postFormRequest(payload.OrderID, payload.NotifyURL, payload.NewForm), ""
}

func postFormRequest(orderID, target, form string) port.Notification {
	return port.Notification{
		OrderID: orderID,
		URL:     target,
		Method:  http.MethodPost,
		Body:    []byte(form),
		Headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	}
}

// resignEPayV2 用商户当前代（generation=1）的平台私钥为字段快照重签并补 timestamp。
// 返回非空 reason 表示无法重签，由调用方决定是否退回入队时的预签快照。
func (s *NotificationService) resignEPayV2(ctx context.Context, payload NotificationPayload) (string, string) {
	if payload.Body == "" {
		return "", "EPay V2 通知载荷缺少字段快照"
	}
	if s.merchantKeys == nil || s.masterKeys == nil {
		return "", "投递侧重签依赖未装配，无法重签 EPay V2 回调"
	}
	var fields map[string]string
	if err := json.Unmarshal([]byte(payload.Body), &fields); err != nil {
		return "", "EPay V2 字段快照无效: " + err.Error()
	}
	privatePEM, err := merchantV2PrivateKey(ctx, s.merchantKeys, s.masterKeys, payload.MerchantID)
	if err != nil {
		return "", err.Error()
	}

	signed := make(map[string]string, len(fields)+1)
	for name, value := range fields {
		if name == "sign" || name == "sign_type" {
			continue
		}
		signed[name] = value
	}
	epay.AppendTimestamp(signed, s.clock.Now())
	sign, err := epay.SignV2(signed, privatePEM)
	if err != nil {
		return "", "EPay V2 重签失败: " + err.Error()
	}
	return encodeEPayForm(signed, sign, "RSA"), ""
}

// settleDead 只把消息转终态。刻意不放进事务、不查订单：
// 未知 topic 或载荷已判死的消息与订单状态无关，任何「顺手改一下订单」都是错的方向。
func (s *NotificationService) settleDead(ctx context.Context, message port.OutboxMessage, reason string) error {
	attempts := message.Attempts + 1
	if attempts < 1 {
		attempts = 1
	}
	if reason == "" {
		reason = "通知消息无法投递"
	}
	if err := s.outbox.MarkDead(ctx, message.ID, message.LeaseToken, attempts, reason); err != nil {
		return wrap(CodeDependency, "标记通知终态失败", err)
	}
	return nil
}

func (s *NotificationService) settleDelivered(ctx context.Context, message port.OutboxMessage, now time.Time) error {
	return s.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
		value, err := s.orders.FindByOrderIDForUpdate(txCtx, message.AggregateID)
		if err != nil {
			if errors.Is(err, port.ErrNotFound) {
				// 关联订单已被删除，直接将孤立消息标记完成避免死循环
				return s.outbox.MarkDelivered(txCtx, message.ID, message.LeaseToken, now)
			}
			return wrap(CodeDependency, "查询通知订单失败", err)
		}
		if value.State == order.StatusNotifyFailed {
			changed, transitionErr := s.orders.Transition(txCtx, value.ID, order.StatusNotifyFailed, order.StatusPaid, now)
			if transitionErr != nil {
				return wrap(CodeDependency, "恢复订单通知状态失败", transitionErr)
			}
			if !changed {
				return fail(CodeConflict, "订单状态已变化，请重试")
			}
		} else if value.State != order.StatusPaid {
			return fail(CodeInvalidState, "订单不处于已支付状态")
		}
		if err := s.outbox.MarkDelivered(txCtx, message.ID, message.LeaseToken, now); err != nil {
			return wrap(CodeDependency, "确认通知任务失败", err)
		}
		return nil
	})
}

func (s *NotificationService) settleFailed(ctx context.Context, message port.OutboxMessage, reason string, now time.Time) error {
	attempts := message.Attempts + 1
	nextAttemptAt := now.Add(notificationBackoff(attempts))
	dead := attempts >= s.maxAttempts
	return s.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
		value, err := s.orders.FindByOrderIDForUpdate(txCtx, message.AggregateID)
		if err != nil {
			if errors.Is(err, port.ErrNotFound) {
				// 关联订单已被删除，将孤立消息标记为已结束，避免无限重试刷屏
				return s.outbox.MarkDelivered(txCtx, message.ID, message.LeaseToken, now)
			}
			return wrap(CodeDependency, "查询通知订单失败", err)
		}
		if value.State == order.StatusPaid {
			changed, transitionErr := s.orders.Transition(txCtx, value.ID, order.StatusPaid, order.StatusNotifyFailed, now)
			if transitionErr != nil {
				return wrap(CodeDependency, "标记订单通知失败异常", transitionErr)
			}
			if !changed {
				return fail(CodeConflict, "订单状态已变化，请重试")
			}
		} else if value.State != order.StatusNotifyFailed {
			return fail(CodeInvalidState, "订单不处于可通知状态")
		}
		// 付款事实已经确认，重试耗尽只终止通知投递，绝不回退订单的已支付状态。
		if dead {
			return s.outbox.MarkDead(txCtx, message.ID, message.LeaseToken, attempts, reason)
		}
		if err := s.outbox.MarkFailed(txCtx, message.ID, message.LeaseToken, attempts, nextAttemptAt, reason); err != nil {
			return wrap(CodeDependency, "记录通知失败结果异常", err)
		}
		return nil
	})
}

func notificationSucceeded(result port.NotificationResult, err error) bool {
	if err != nil || result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		return false
	}
	// 容忍商户端返回 "success\n"/"Success" 或 2xx 空正文；非空且非 success 的正文仍然判失败并重试。
	body := strings.TrimSpace(string(result.Body))
	return body == "" || strings.EqualFold(body, "success")
}

func notificationFailure(result port.NotificationResult, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("通知失败，HTTP状态码: %d，响应: %s", result.StatusCode, truncateBody(result.Body))
}

func notificationBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 6 {
		attempts = 6
	}
	return time.Duration(1<<(attempts-1)) * time.Minute
}

func truncateBody(body []byte) string {
	const limit = 200
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit])
}

// buildNotificationFor 是「按订单协议生成回调载荷」的唯一入口：
// 支付核销（MonitorService.markPaid）与管理端重签（OrderService.ReissueByID）必须共用它，
// 否则重签会把原生 HMAC 报文发给 EPay 商户，商户验签必然失败。
//
// 签名材料缺失、AES 解密失败、V1 MD5 / V2 RSA 签名失败一律以 error 返回，
// 由调用方按 §6.6 决定降级方式——绝不能让它回滚支付事实。
func buildNotificationFor(ctx context.Context, value order.Order, nativeKey string,
	merchants port.MerchantRepository, merchantKeys port.MerchantKeyRepository,
	masterKeys *secret.Box, now time.Time) (string, []byte, error) {
	switch value.Protocol {
	case merchant.ProtocolEPayV1, merchant.ProtocolEPayV2:
		return buildEPayNotification(ctx, value, merchants, merchantKeys, masterKeys, now)
	default:
		return nativeNotification(value, nativeKey)
	}
}

// nativeNotification 是 VMQ 原生回调：settings.key + HMAC-SHA-256，行为与改造前一致。
func nativeNotification(value order.Order, merchantKey string) (string, []byte, error) {
	if merchantKey == "" {
		// 绝不用空密钥签名：那会产出一条「看起来正常、商户却永远验不过」的报文。
		return "", nil, errors.New("系统密钥未设置")
	}
	payload := signedNotificationPayload(value, merchantKey)
	payload.Version = notifyPayloadVersionLegacy
	return encodeNotifyPayload(notificationTopic, payload)
}

// buildEPayNotification 生成 EPay 回调：V1 固化完整 GET 地址，V2 固化字段快照 + 预签 form。
func buildEPayNotification(ctx context.Context, value order.Order,
	merchants port.MerchantRepository, merchantKeys port.MerchantKeyRepository,
	masterKeys *secret.Box, now time.Time) (string, []byte, error) {
	merchantNo, err := merchantNoFor(ctx, merchants, value.MerchantID)
	if err != nil {
		return "", nil, fmt.Errorf("生成 EPay 回调失败: %w", err)
	}
	fields, err := epayNotifyFields(value, merchantNo)
	if err != nil {
		return "", nil, fmt.Errorf("生成 EPay 回调失败: %w", err)
	}

	payload := NotificationPayload{
		Version:    notifyPayloadVersionEPay,
		OrderID:    value.OrderID,
		NotifyURL:  value.NotifyURL,
		MerchantID: value.MerchantID,
	}

	if value.Protocol == merchant.ProtocolEPayV2 {
		privatePEM, keyErr := merchantV2PrivateKey(ctx, merchantKeys, masterKeys, value.MerchantID)
		if keyErr != nil {
			return "", nil, fmt.Errorf("生成 EPay V2 回调签名失败: %w", keyErr)
		}
		snapshot, marshalErr := json.Marshal(fields)
		if marshalErr != nil {
			return "", nil, fmt.Errorf("编码 EPay V2 回调字段失败: %w", marshalErr)
		}
		signed := cloneEPayFields(fields)
		epay.AppendTimestamp(signed, now)
		sign, signErr := epay.SignV2(signed, privatePEM)
		if signErr != nil {
			return "", nil, fmt.Errorf("生成 EPay V2 回调签名失败: %w", signErr)
		}
		payload.Method = http.MethodPost
		payload.Body = string(snapshot)
		// 预签 form 只在 timestamp 窗口内有效，仅作投递侧重签不可用时的兜底。
		payload.NewForm = encodeEPayForm(signed, sign, "RSA")
		return encodeNotifyPayload(notifyTopicFor(value.Protocol), payload)
	}

	secretText, keyErr := merchantV1Secret(ctx, merchantKeys, masterKeys, value.MerchantID)
	if keyErr != nil {
		return "", nil, fmt.Errorf("生成 EPay V1 回调签名失败: %w", keyErr)
	}
	sign, signErr := epay.SignV1(fields, secretText)
	if signErr != nil {
		return "", nil, fmt.Errorf("生成 EPay V1 回调签名失败: %w", signErr)
	}
	payload.Method = http.MethodGet
	payload.URL = appendReturnQuery(value.NotifyURL, encodeEPayForm(fields, sign, "MD5"))
	return encodeNotifyPayload(notifyTopicFor(value.Protocol), payload)
}

func encodeNotifyPayload(topic string, payload NotificationPayload) (string, []byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", nil, fmt.Errorf("编码通知事件失败: %w", err)
	}
	return topic, encoded, nil
}

// notifyFailureRecorder 把「支付/补单事实已提交、但回调载荷发不出去」落成两条记录：
// 订单转通知失败 + 一条带 Dead 标记的终态消息（last_error 记原因，供管理端按协议重签）。
//
// 支付核销与管理端重签共用同一份规则，避免两处降级行为漂移。
//
// 为什么写成带 Dead 标记的普通入队消息而不是直接写 status=dead 行：
// port.OutboxRepository 没有 EnqueueDead，而 MarkDead 要求 lease_token 非空（只有被领取的消息才有），
// 所以这里交由投递 worker 在下一轮把它转成终态，期间不会发起任何请求、也不会触碰订单状态。
type notifyFailureRecorder struct {
	transactions port.TransactionManager
	orders       port.OrderRepository
	outbox       port.OutboxRepository
}

func (r notifyFailureRecorder) record(ctx context.Context, value order.Order, eventKey string, cause error, now time.Time) error {
	reason := "回调载荷生成失败: " + cause.Error()
	encoded, marshalErr := json.Marshal(NotificationPayload{
		Version:    notifyPayloadVersionEPay,
		OrderID:    value.OrderID,
		NotifyURL:  value.NotifyURL,
		MerchantID: value.MerchantID,
		Dead:       true,
		Reason:     reason,
	})
	if marshalErr != nil {
		return wrap(CodeDependency, "编码通知失败事件失败", marshalErr)
	}
	return r.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
		changed, transitionErr := r.orders.Transition(txCtx, value.ID, order.StatusPaid, order.StatusNotifyFailed, now)
		if transitionErr != nil {
			return wrap(CodeDependency, "标记订单通知失败异常", transitionErr)
		}
		if !changed {
			return fail(CodeConflict, "订单状态已变化，请重试")
		}
		// event_key 单独取命名空间：正常回调消息已经用掉了 (event_key, topic) 这个唯一键，
		// 直接复用会被唯一约束判成「已处理」而写不进去。
		enqueueErr := r.outbox.Enqueue(txCtx, port.OutboxMessage{
			ID:            digest("notify-dead", eventKey, value.OrderID),
			Topic:         notifyTopicFor(value.Protocol),
			AggregateID:   value.OrderID,
			Payload:       encoded,
			EventKey:      digest("notify-dead", eventKey),
			CreatedAt:     now,
			NextAttemptAt: now,
		})
		if enqueueErr != nil && !errors.Is(enqueueErr, port.ErrAlreadyProcessed) {
			return wrap(CodeDependency, "写入通知失败事件失败", enqueueErr)
		}
		return nil
	})
}

// notifyRouteFor 把 topic 归一成投递路径。白名单之外一律 false —— 调用方据此转终态。
func notifyRouteFor(topic string) (notifyRoute, bool) {
	switch topic {
	case notificationTopic:
		return notifyRouteNative, true
	case epayV1NotifyTopic:
		return notifyRouteEPayV1, true
	case epayV2NotifyTopic:
		return notifyRouteEPayV2, true
	default:
		return 0, false
	}
}

// notifyTopicFor 由订单协议决定 topic，生产端（支付核销、重签）共用这一处映射。
func notifyTopicFor(protocol merchant.Protocol) string {
	switch protocol {
	case merchant.ProtocolEPayV1:
		return epayV1NotifyTopic
	case merchant.ProtocolEPayV2:
		return epayV2NotifyTopic
	default:
		return notificationTopic
	}
}

// epayNotifyFields 构造 EPay V1/V2 回调与同步回跳共用的字段集（不含 sign/sign_type/timestamp）。
// 两处必须共用同一份构造器，否则「回调验签通过、回跳验签失败」这类漂移只会在生产上暴露。
//
// money 回下单金额（orders.price_text），不是真实到账金额：金额微调是内部核销手段，
// 商户侧记账必须用自己下的金额（设计 §3.2、§6.1）。
func epayNotifyFields(value order.Order, merchantNo string) (map[string]string, error) {
	epayType, ok := orderEPayType(value)
	if !ok {
		return nil, fmt.Errorf("订单 %s 的支付方式无法映射到 EPay type", value.OrderID)
	}
	return map[string]string{
		"pid":          merchantNo,
		"trade_no":     value.OrderID,
		"out_trade_no": value.PayID,
		"type":         epayType,
		"name":         truncateEPayName(value.MerchantOrderName),
		"money":        amountText(value.PriceText, value.PriceCents),
		"trade_status": "TRADE_SUCCESS",
		"param":        value.Param,
	}, nil
}

// orderEPayType 优先回显下单时的原始 type 文本，缺失时按内部支付方式反推。
func orderEPayType(value order.Order) (string, bool) {
	if value.EpayType != "" {
		return value.EpayType, true
	}
	return epay.EPayTypeFrom(value.Type)
}

// truncateEPayName 按字节上限截断商品名，只在 UTF-8 边界上切，避免产生半个汉字。
func truncateEPayName(name string) string {
	if len(name) <= epayNameMaxBytes {
		return name
	}
	cut := epayNameMaxBytes
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut]
}

// cloneEPayFields 复制一份字段，供「加 timestamp 再签名」使用，
// 避免把 timestamp 写回共享的字段快照（快照必须不含 timestamp，否则重签会叠加）。
func cloneEPayFields(fields map[string]string) map[string]string {
	cloned := make(map[string]string, len(fields)+1)
	for name, value := range fields {
		cloned[name] = value
	}
	return cloned
}

// encodeEPayForm 把字段与签名拼成 a=b&c=d 形式的 form 串（键升序、值 urlencode）。
// 空值参数照原样发送：EPay 的签名域剔除空值，但发送串保留字段，两端语义不同。
func encodeEPayForm(fields map[string]string, sign, signType string) string {
	values := make(url.Values, len(fields)+2)
	for name, value := range fields {
		if name == "sign" || name == "sign_type" {
			continue
		}
		values.Set(name, value)
	}
	values.Set("sign", sign)
	values.Set("sign_type", signType)
	return values.Encode()
}

// merchantNoFor 取商户对外标识（pid）。商户被删除后历史订单不再能回调，属于不可逆操作。
func merchantNoFor(ctx context.Context, repo port.MerchantRepository, merchantID int64) (string, error) {
	if repo == nil {
		return "", errors.New("商户仓储未装配，无法取得商户号")
	}
	value, err := repo.FindByID(ctx, merchantID)
	if err != nil {
		return "", fmt.Errorf("查询商户失败: %w", err)
	}
	if value.No == "" {
		return "", fmt.Errorf("商户 %d 缺少对外商户号", merchantID)
	}
	return value.No, nil
}

// merchantV1Secret 取并解密商户当前代 V1 共享密钥（MD5 用）。
// 解密点 fail-fast 校验密钥形态：任何编码/截断 bug 若拖到签名阶段才暴露，日志里只会显示「签名错误」。
func merchantV1Secret(ctx context.Context, keys port.MerchantKeyRepository, box *secret.Box, merchantID int64) (string, error) {
	plain, err := merchantSecret(ctx, keys, box, merchantID, merchant.ProtocolEPayV1)
	if err != nil {
		return "", err
	}
	secretText := string(plain)
	if !merchant.IsValidV1Secret(secretText) {
		return "", fmt.Errorf("商户 %d 的 V1 密钥形态无效（长度或字符集不符）", merchantID)
	}
	return secretText, nil
}

// merchantV2PrivateKey 取并解密商户当前代 V2 平台私钥（PKCS#8 PEM）。
func merchantV2PrivateKey(ctx context.Context, keys port.MerchantKeyRepository, box *secret.Box, merchantID int64) ([]byte, error) {
	return merchantSecret(ctx, keys, box, merchantID, merchant.ProtocolEPayV2)
}

// merchantSecret 是「按商户+协议取当前代密钥并解密」的唯一入口，回调、回跳、重签共用。
func merchantSecret(ctx context.Context, keys port.MerchantKeyRepository, box *secret.Box, merchantID int64, protocol merchant.Protocol) ([]byte, error) {
	if keys == nil || box == nil {
		return nil, errors.New("商户密钥依赖未装配，无法读取密钥")
	}
	key, err := keys.FindActive(ctx, merchantID, protocol)
	if err != nil {
		if errors.Is(err, port.ErrNotFound) {
			return nil, fmt.Errorf("商户 %d 未开通 %s 协议或缺少当前代密钥", merchantID, protocol)
		}
		return nil, fmt.Errorf("读取商户 %s 密钥失败: %w", protocol, err)
	}
	plain, err := box.Open(key.SecretCipher, key.SecretNonce, key.AAD(), key.KeyVersion)
	if err != nil {
		// 不把「密钥解不开」伪装成「签名失败」：前者要运维重新下发密钥，后者要商户改配置。
		return nil, fmt.Errorf("商户 %d 的 %s 密钥解密失败: %w", merchantID, protocol, err)
	}
	return plain, nil
}
