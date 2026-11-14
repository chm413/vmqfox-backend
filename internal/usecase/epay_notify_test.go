package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hulisang/vmqfox-backend/internal/domain/epay"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// 本文件覆盖 EPay 多协议回调链路：载荷版本兼容、topic 分派、GET/POST 语义、
// V2 投递时重签，以及「签名失败不得回滚支付事实」（设计 §6.3/§6.6）。

const (
	testMerchantNo = "M7K3QPX9Z2AB4C"
	// testV1Secret 必须是 32 位且落在 merchant 包允许的字符集内，否则解密点会 fail-fast。
	testV1Secret    = "abcdefghjkmnpqrstuvwxyz234567892"
	testMasterKeyV1 = 1
	testMerchantID  = int64(7)
)

// epayMerchants 只提供 pid 查询，其余方法按「未实现」返回。
type epayMerchants struct{ values map[int64]merchant.Merchant }

func newEPayMerchants(ids ...int64) epayMerchants {
	values := make(map[int64]merchant.Merchant, len(ids))
	for index, id := range ids {
		values[id] = merchant.Merchant{ID: id, No: testMerchantNo + strconv.Itoa(index), Enabled: true}
	}
	// 第一个商户固定使用文档里的示例 pid，方便断言。
	if len(ids) > 0 {
		values[ids[0]] = merchant.Merchant{ID: ids[0], No: testMerchantNo, Enabled: true}
	}
	return epayMerchants{values: values}
}

func (m epayMerchants) Create(_ context.Context, value merchant.Merchant) (merchant.Merchant, error) {
	return value, nil
}

func (m epayMerchants) FindByID(_ context.Context, id int64) (merchant.Merchant, error) {
	value, ok := m.values[id]
	if !ok {
		return merchant.Merchant{}, port.ErrNotFound
	}
	return value, nil
}

func (m epayMerchants) FindByNo(context.Context, string) (merchant.Merchant, error) {
	return merchant.Merchant{}, port.ErrNotFound
}

func (m epayMerchants) List(context.Context) ([]merchant.Merchant, error) {
	items := make([]merchant.Merchant, 0, len(m.values))
	for _, value := range m.values {
		items = append(items, value)
	}
	return items, nil
}

func (m epayMerchants) Update(context.Context, merchant.Merchant) error { return nil }

func (m epayMerchants) Delete(context.Context, int64) error { return nil }

func (m epayMerchants) Count(context.Context) (int64, error) { return int64(len(m.values)), nil }

// epayMerchantKeys 用内存映射模拟「每商户每协议当前代」密钥行。
type epayMerchantKeys struct{ values map[string]merchant.Key }

func epayKeySlot(merchantID int64, protocol merchant.Protocol) string {
	return fmt.Sprintf("%d/%d", merchantID, int8(protocol))
}

func (k epayMerchantKeys) FindActive(_ context.Context, merchantID int64, protocol merchant.Protocol) (merchant.Key, error) {
	key, ok := k.values[epayKeySlot(merchantID, protocol)]
	if !ok {
		return merchant.Key{}, port.ErrNotFound
	}
	return key, nil
}

func (k epayMerchantKeys) FindAllForVerify(context.Context, int64, merchant.Protocol) ([]merchant.Key, error) {
	return nil, nil
}

func (k epayMerchantKeys) Upsert(context.Context, merchant.Key) error { return nil }

func (k epayMerchantKeys) Rotate(context.Context, int64, merchant.Protocol, merchant.Key) error {
	return nil
}

func (k epayMerchantKeys) DeleteByMerchant(context.Context, int64) error { return nil }

var (
	_ port.MerchantRepository    = epayMerchants{}
	_ port.MerchantKeyRepository = epayMerchantKeys{}
)

func newTestSecretBox(t *testing.T) *secret.Box {
	t.Helper()
	box, err := secret.New(map[int][]byte{testMasterKeyV1: bytes.Repeat([]byte{0x42}, 32)}, testMasterKeyV1)
	if err != nil {
		t.Fatalf("构造主密钥 Box 失败: %v", err)
	}
	return box
}

// sealTestKey 按生产同款 AAD 把明文加密成一行密钥，解密侧的断言才有意义。
func sealTestKey(t *testing.T, box *secret.Box, merchantID int64, protocol merchant.Protocol, plain []byte) merchant.Key {
	t.Helper()
	key := merchant.Key{
		MerchantID: merchantID,
		Protocol:   protocol,
		Generation: merchant.GenerationCurrent,
		Hint:       "test",
	}
	cipher, nonce, version, err := box.Seal(plain, key.AAD())
	if err != nil {
		t.Fatalf("加密测试密钥失败: %v", err)
	}
	key.SecretCipher = cipher
	key.SecretNonce = nonce
	key.KeyVersion = version
	return key
}

// capturingNotifier 记录出站请求，用于断言方法、地址、body 与请求头。
type capturingNotifier struct {
	requests []port.Notification
	result   port.NotificationResult
	err      error
}

func (n *capturingNotifier) Send(_ context.Context, notification port.Notification) (port.NotificationResult, error) {
	n.requests = append(n.requests, notification)
	return n.result, n.err
}

func okNotifier() *capturingNotifier {
	return &capturingNotifier{result: port.NotificationResult{StatusCode: http.StatusOK, Body: []byte("success")}}
}

// countingOrders 只统计「有没有人查过订单」，用于证明未知 topic 不会碰订单状态。
type countingOrders struct {
	stubOrderRepository
	lookups     int
	transitions int
}

func (r *countingOrders) FindByOrderIDForUpdate(context.Context, string) (order.Order, error) {
	r.lookups++
	return order.Order{ID: 1, OrderID: "order-1", State: order.StatusPaid}, nil
}

func (r *countingOrders) Transition(context.Context, int64, order.Status, order.Status, time.Time) (bool, error) {
	r.transitions++
	return true, nil
}

func newTestNotificationService(t *testing.T, outbox port.OutboxRepository, orders port.OrderRepository,
	notifier port.Notifier, keys port.MerchantKeyRepository, box *secret.Box, now time.Time) *NotificationService {
	t.Helper()
	service, err := NewNotificationService(NotificationServiceDeps{
		Transactions: &passthroughTransactions{},
		Orders:       orders,
		Outbox:       outbox,
		Notifier:     notifier,
		Clock:        fakeClock{now: now},
		MerchantKeys: keys,
		MasterKeys:   box,
		MaxAttempts:  3,
	})
	if err != nil {
		t.Fatalf("构造通知用例失败: %v", err)
	}
	return service
}

// TestLegacyNotifyPayloadStillPostsForm 锁定向后兼容：库里已存在的旧载荷
// （没有 v、没有 method，只有 orderId/notifyUrl/newForm）必须继续按原 POST 路径投递。
func TestLegacyNotifyPayloadStillPostsForm(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	notifier := okNotifier()
	outbox := &outboxRecorder{}
	service := newTestNotificationService(t, outbox, &paidOrderRepository{state: order.StatusPaid}, notifier, nil, nil, now)

	outbox.pendingDeliveries = []port.OutboxMessage{{
		ID:          "m1",
		Topic:       notificationTopic,
		AggregateID: "order-1",
		LeaseToken:  "lease",
		Payload:     []byte(`{"orderId":"order-1","notifyUrl":"https://merchant.test/notify","newForm":"payId=p1&sign=s1"}`),
	}}

	stats, err := service.DeliverNext(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("投递旧载荷不应报错: %v", err)
	}
	if stats.Delivered != 1 || outbox.markDelivered != 1 {
		t.Fatalf("旧载荷应被正常投递: stats=%+v delivered=%d", stats, outbox.markDelivered)
	}
	if len(notifier.requests) != 1 {
		t.Fatalf("应发出一次请求，实际 %d 次", len(notifier.requests))
	}
	request := notifier.requests[0]
	if request.Method != http.MethodPost || request.URL != "https://merchant.test/notify" {
		t.Fatalf("旧载荷必须 POST 到 notifyUrl，实际 %s %s", request.Method, request.URL)
	}
	if string(request.Body) != "payId=p1&sign=s1" {
		t.Fatalf("body 必须是原样 form，实际 %q", string(request.Body))
	}
	if request.Headers["Content-Type"] != "application/x-www-form-urlencoded" {
		t.Fatalf("POST form 必须带 form Content-Type，实际 %v", request.Headers)
	}
}

// TestEPayV1NotifyDeliversByGetWithoutBody 锁定 V1 走 GET：不设 Content-Type、body 为空。
func TestEPayV1NotifyDeliversByGetWithoutBody(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	notifier := okNotifier()
	outbox := &outboxRecorder{}
	service := newTestNotificationService(t, outbox, &paidOrderRepository{state: order.StatusPaid}, notifier, nil, nil, now)

	signedURL := "https://merchant.test/notify?pid=M1&trade_no=T1&sign=abc&sign_type=MD5"
	payload, err := json.Marshal(NotificationPayload{
		Version:   notifyPayloadVersionEPay,
		OrderID:   "T1",
		NotifyURL: "https://merchant.test/notify",
		Method:    http.MethodGet,
		URL:       signedURL,
	})
	if err != nil {
		t.Fatalf("编码载荷失败: %v", err)
	}
	outbox.pendingDeliveries = []port.OutboxMessage{{
		ID: "m1", Topic: epayV1NotifyTopic, AggregateID: "T1", LeaseToken: "lease", Payload: payload,
	}}

	if _, err := service.DeliverNext(context.Background(), time.Minute); err != nil {
		t.Fatalf("投递 V1 回调不应报错: %v", err)
	}
	if len(notifier.requests) != 1 {
		t.Fatalf("应发出一次请求，实际 %d 次", len(notifier.requests))
	}
	request := notifier.requests[0]
	if request.Method != http.MethodGet {
		t.Fatalf("V1 必须用 GET，实际 %s", request.Method)
	}
	if request.URL != signedURL {
		t.Fatalf("V1 应原样使用入队时固化的完整地址，实际 %s", request.URL)
	}
	if len(request.Body) != 0 {
		t.Fatalf("GET 不应带 body，实际 %q", string(request.Body))
	}
	if _, ok := request.Headers["Content-Type"]; ok {
		t.Fatalf("GET 不应设置 Content-Type，实际 %v", request.Headers)
	}
}

// TestEPayV1NotifyRejectsPost 锁定「V1 只允许 GET」：写错方法要判失败并重试，而不是静默发出去。
func TestEPayV1NotifyRejectsPost(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	notifier := okNotifier()
	outbox := &outboxRecorder{}
	service := newTestNotificationService(t, outbox, &paidOrderRepository{state: order.StatusPaid}, notifier, nil, nil, now)

	payload := []byte(`{"v":2,"orderId":"T1","notifyUrl":"https://merchant.test/notify","method":"POST","url":"https://merchant.test/notify?sign=x"}`)
	outbox.pendingDeliveries = []port.OutboxMessage{{ID: "m1", Topic: epayV1NotifyTopic, AggregateID: "T1", LeaseToken: "lease", Payload: payload}}

	stats, err := service.DeliverNext(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("投递不应报错: %v", err)
	}
	if stats.Failed != 1 || outbox.markFailedCalls != 1 || len(notifier.requests) != 0 {
		t.Fatalf("V1 用 POST 应判失败且不发请求: stats=%+v failed=%d requests=%d",
			stats, outbox.markFailedCalls, len(notifier.requests))
	}
}

// TestUnknownTopicMarksDeadWithoutTouchingOrder 锁定未知 topic 只转终态：
// 绝不能走 settleFailed 把订单改成通知失败（那会让一条脏消息污染订单状态）。
func TestUnknownTopicMarksDeadWithoutTouchingOrder(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	orders := &countingOrders{}
	outbox := &outboxRecorder{}
	notifier := okNotifier()
	service := newTestNotificationService(t, outbox, orders, notifier, nil, nil, now)

	outbox.pendingDeliveries = []port.OutboxMessage{{
		ID: "m1", Topic: "order.refunded.notify", AggregateID: "order-1", LeaseToken: "lease",
		Payload: []byte(`{"orderId":"order-1","notifyUrl":"https://merchant.test/notify","newForm":"a=b"}`),
	}}

	stats, err := service.DeliverNext(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("未知 topic 不应作为错误上抛: %v", err)
	}
	if stats.Dead != 1 || outbox.markDeadCalls != 1 {
		t.Fatalf("未知 topic 应直接转终态: stats=%+v dead=%d", stats, outbox.markDeadCalls)
	}
	if outbox.markFailedCalls != 0 || outbox.markDelivered != 0 {
		t.Fatalf("未知 topic 不得走重试或完成路径: failed=%d delivered=%d", outbox.markFailedCalls, outbox.markDelivered)
	}
	if orders.lookups != 0 || orders.transitions != 0 {
		t.Fatalf("未知 topic 不得触碰订单: lookups=%d transitions=%d", orders.lookups, orders.transitions)
	}
	if len(notifier.requests) != 0 {
		t.Fatalf("未知 topic 不得发起请求，实际 %d 次", len(notifier.requests))
	}
}

// TestNotifyPayloadWithoutTargetFails 锁定静默路径被堵死：
// notifyUrl 非空但 url/newForm/body 全空时必须判失败，而不是标记已投递。
func TestNotifyPayloadWithoutTargetFails(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	outbox := &outboxRecorder{}
	notifier := okNotifier()
	service := newTestNotificationService(t, outbox, &paidOrderRepository{state: order.StatusPaid}, notifier, nil, nil, now)

	outbox.pendingDeliveries = []port.OutboxMessage{{
		ID: "m1", Topic: epayV1NotifyTopic, AggregateID: "order-1", LeaseToken: "lease",
		Payload: []byte(`{"v":2,"orderId":"order-1","notifyUrl":"https://merchant.test/notify","method":"GET"}`),
	}}

	stats, err := service.DeliverNext(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("投递不应报错: %v", err)
	}
	if stats.Failed != 1 || outbox.markFailedCalls != 1 || outbox.markDelivered != 0 {
		t.Fatalf("载荷无目标应判失败: stats=%+v failed=%d delivered=%d",
			stats, outbox.markFailedCalls, outbox.markDelivered)
	}
	if len(notifier.requests) != 0 {
		t.Fatalf("载荷无目标不得发起请求，实际 %d 次", len(notifier.requests))
	}
}

// TestEPayV2NotifyResignsAtDelivery 锁定 V2 的 timestamp 只能由投递侧重签产生：
// 入队快照里不含 timestamp，投递时必须用 generation=1 私钥重签并补当前时间。
func TestEPayV2NotifyResignsAtDelivery(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	box := newTestSecretBox(t)
	privatePEM, publicPEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成测试 RSA 密钥对失败: %v", err)
	}
	keys := epayMerchantKeys{values: map[string]merchant.Key{
		epayKeySlot(testMerchantID, merchant.ProtocolEPayV2): sealTestKey(t, box, testMerchantID, merchant.ProtocolEPayV2, privatePEM),
	}}

	fields := map[string]string{
		"pid":          testMerchantNo,
		"trade_no":     "T1",
		"out_trade_no": "M1",
		"type":         "alipay",
		"name":         "测试商品",
		"money":        "10.00",
		"trade_status": "TRADE_SUCCESS",
		"param":        "",
	}
	snapshot, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("编码字段快照失败: %v", err)
	}
	payload, err := json.Marshal(NotificationPayload{
		Version:    notifyPayloadVersionEPay,
		OrderID:    "T1",
		NotifyURL:  "https://merchant.test/notify",
		Method:     http.MethodPost,
		Body:       string(snapshot),
		MerchantID: testMerchantID,
		// 入队时的预签 form 已经过期，正确实现必须忽略它并重签。
		NewForm: "timestamp=1&sign=stale&sign_type=RSA",
	})
	if err != nil {
		t.Fatalf("编码载荷失败: %v", err)
	}

	notifier := okNotifier()
	outbox := &outboxRecorder{}
	service := newTestNotificationService(t, outbox, &paidOrderRepository{state: order.StatusPaid}, notifier, keys, box, now)
	outbox.pendingDeliveries = []port.OutboxMessage{{ID: "m1", Topic: epayV2NotifyTopic, AggregateID: "T1", LeaseToken: "lease", Payload: payload}}

	if _, err := service.DeliverNext(context.Background(), time.Minute); err != nil {
		t.Fatalf("投递 V2 回调不应报错: %v", err)
	}
	if len(notifier.requests) != 1 {
		t.Fatalf("应发出一次请求，实际 %d 次", len(notifier.requests))
	}
	request := notifier.requests[0]
	if request.Method != http.MethodPost || request.URL != "https://merchant.test/notify" {
		t.Fatalf("V2 必须 POST 到 notifyUrl，实际 %s %s", request.Method, request.URL)
	}

	values, err := url.ParseQuery(string(request.Body))
	if err != nil {
		t.Fatalf("解析回调 form 失败: %v", err)
	}
	if values.Get("timestamp") != strconv.FormatInt(now.Unix(), 10) {
		t.Fatalf("timestamp 必须是投递时刻，实际 %q", values.Get("timestamp"))
	}
	if values.Get("sign_type") != "RSA" {
		t.Fatalf("sign_type 必须是 RSA，实际 %q", values.Get("sign_type"))
	}
	sign := values.Get("sign")
	params := make(map[string]string, len(values))
	for name := range values {
		params[name] = values.Get(name)
	}
	if err := epay.VerifyV2(params, publicPEM, sign); err != nil {
		t.Fatalf("重签结果应能被平台公钥验签通过: %v", err)
	}
	if values.Get("sign") == "stale" {
		t.Fatal("必须重签，不能直接使用入队时的过期 form")
	}
}

// TestEPayV2NotifyFallsBackToPreSignedForm 锁定取舍：重签依赖缺失时退回入队时的预签 form，
// 至少让首次投递（timestamp 还在窗口内）有机会成功。
func TestEPayV2NotifyFallsBackToPreSignedForm(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	notifier := okNotifier()
	outbox := &outboxRecorder{}
	// 故意不装配 MerchantKeys / MasterKeys：模拟装配缺失或主密钥未配置。
	service := newTestNotificationService(t, outbox, &paidOrderRepository{state: order.StatusPaid}, notifier, nil, nil, now)

	payload := []byte(`{"v":2,"orderId":"T1","notifyUrl":"https://merchant.test/notify","method":"POST","body":"{\"pid\":\"M1\"}","newForm":"pid=M1&sign=pre&sign_type=RSA"}`)
	outbox.pendingDeliveries = []port.OutboxMessage{{ID: "m1", Topic: epayV2NotifyTopic, AggregateID: "T1", LeaseToken: "lease", Payload: payload}}

	if _, err := service.DeliverNext(context.Background(), time.Minute); err != nil {
		t.Fatalf("投递不应报错: %v", err)
	}
	if len(notifier.requests) != 1 {
		t.Fatalf("应退回预签 form 并发出请求，实际 %d 次", len(notifier.requests))
	}
	if string(notifier.requests[0].Body) != "pid=M1&sign=pre&sign_type=RSA" {
		t.Fatalf("应使用入队时的预签 form，实际 %q", string(notifier.requests[0].Body))
	}
}

// commitTrackingTransactions 在回调返回错误时回滚到账事件，模拟真实事务语义。
// 用它才能证明「签名失败不会连带回滚 payment_events 去重」。
type commitTrackingTransactions struct {
	events     *stagedEvents
	rolledBack int
	commits    int
}

func (t *commitTrackingTransactions) WithinTransaction(ctx context.Context, fn func(context.Context) error) error {
	snapshot := t.events.snapshot()
	err := fn(ctx)
	if err != nil {
		t.events.restore(snapshot)
		t.rolledBack++
		return err
	}
	t.commits++
	return nil
}

type stagedEvents struct {
	seen   map[string]bool
	writes int
}

func newStagedEvents() *stagedEvents {
	return &stagedEvents{seen: map[string]bool{}}
}

func (e *stagedEvents) RecordIfNew(_ context.Context, event port.PaymentEvent) (bool, error) {
	e.writes++
	if e.seen[event.EventKey] {
		return false, nil
	}
	e.seen[event.EventKey] = true
	return true, nil
}

func (e *stagedEvents) PruneBefore(context.Context, time.Time) (int64, error) { return 0, nil }

func (e *stagedEvents) snapshot() map[string]bool {
	copied := make(map[string]bool, len(e.seen))
	for key, value := range e.seen {
		copied[key] = value
	}
	return copied
}

func (e *stagedEvents) restore(snapshot map[string]bool) { e.seen = snapshot }

var _ port.PaymentEventRepository = (*stagedEvents)(nil)

// TestPushKeepsPaymentWhenNotifySigningFails 锁定解耦的核心不变量（设计 §6.6）：
// EPay 载荷生成失败（缺密钥 → 解密/签名失败）时，
// 订单仍为已支付/通知失败、金额锁已释放、到账事件未被回滚，且写一条待重签的终态记录。
func TestPushKeepsPaymentWhenNotifySigningFails(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	events := newStagedEvents()
	transactions := &commitTrackingTransactions{events: events}
	outbox := &recordingOutbox{}
	box := newTestSecretBox(t)

	// 商户已开通 V1 但密钥表里没有当前代密钥（轮换后被清 / 装配错误），载荷必然生成失败。
	pending := order.Order{
		ID: 9, MerchantID: testMerchantID, Protocol: merchant.ProtocolEPayV1,
		OrderID: "T9", PayID: "M9", Type: payment.Alipay, EpayType: "alipay",
		ReallyPriceCents: 321, PriceText: "3.21", State: order.StatusPending,
		NotifyURL: "https://merchant.test/notify", CreatedAt: now.Add(-time.Minute),
	}
	orders := &matchOrders{pending: &pending}

	service, err := NewMonitorService(MonitorServiceDeps{
		Transactions: transactions,
		Orders:       orders,
		PriceLocks:   grantingPriceLocks{},
		Settings:     newMapSettings(map[string]string{setting.MerchantKey: testMerchantKey}),
		Events:       events,
		Outbox:       outbox,
		Clock:        fakeClock{now: now},
		PublicTokens: &scriptedTokens{tokens: []string{testToken(31)}},
		Merchants:    newEPayMerchants(testMerchantID),
		MerchantKeys: epayMerchantKeys{},
		MasterKeys:   box,
		SignTTL:      5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("构造监控用例失败: %v", err)
	}

	result, pushErr := service.Push(context.Background(), pushInput(now, testMerchantKey))
	if pushErr != nil {
		t.Fatalf("载荷生成失败不得让到账推送整体失败，实际 err=%v", pushErr)
	}
	if !result.Matched || result.Order.State != order.StatusNotifyFailed {
		t.Fatalf("订单应已支付但通知失败，实际 %+v", result)
	}
	if transactions.rolledBack != 0 {
		t.Fatalf("载荷生成失败不得回滚支付事务，实际回滚 %d 次", transactions.rolledBack)
	}

	transitions := orders.transitions
	wantTransitions := []transitionCall{
		{from: order.StatusPending, to: order.StatusPaid},
		{from: order.StatusPaid, to: order.StatusNotifyFailed},
	}
	if len(transitions) != len(wantTransitions) {
		t.Fatalf("状态转换应为 %+v，实际 %+v", wantTransitions, transitions)
	}
	for index, want := range wantTransitions {
		if transitions[index] != want {
			t.Fatalf("第 %d 次状态转换应为 %+v，实际 %+v", index, want, transitions[index])
		}
	}

	if len(outbox.messages) != 1 {
		t.Fatalf("应写一条待重签的终态记录，实际 %d 条", len(outbox.messages))
	}
	dead := outbox.messages[0]
	if dead.Topic != epayV1NotifyTopic {
		t.Fatalf("终态记录应落 EPay V1 topic，实际 %s", dead.Topic)
	}
	var deadPayload NotificationPayload
	if err := json.Unmarshal(dead.Payload, &deadPayload); err != nil {
		t.Fatalf("解析终态记录载荷失败: %v", err)
	}
	if !deadPayload.Dead || deadPayload.Reason == "" {
		t.Fatalf("终态记录必须带 Dead 标记与原因，实际 %+v", deadPayload)
	}
	if deadPayload.NotifyURL != pending.NotifyURL {
		t.Fatalf("终态记录必须保留 notifyUrl，实际 %q", deadPayload.NotifyURL)
	}

	// 到账事件未被回滚：同一个事件再推一次会被判为重复，而不是重新走一遍失败。
	if !events.seen[paymentEventKey(pushInput(now, testMerchantKey).Type, pushInput(now, testMerchantKey).Price, pushInput(now, testMerchantKey).Timestamp)] {
		t.Fatal("到账事件应已提交，实际被回滚")
	}
	again, err := service.Push(context.Background(), pushInput(now, testMerchantKey))
	if err != nil {
		t.Fatalf("重复推送不应报错: %v", err)
	}
	if !again.Duplicate || again.Matched {
		t.Fatalf("同一事件重复推送应被去重，实际 %+v", again)
	}
}

// TestEPayNotifyFieldsCarryDocumentedLayout 锁定回调字段集与文档一致：
// money 回下单金额、param 原样回显、name 按 127 字节截断。
func TestEPayNotifyFieldsCarryDocumentedLayout(t *testing.T) {
	value := order.Order{
		OrderID: "20261113000001", PayID: "M20261113001",
		Type: payment.Alipay, EpayType: "alipay",
		PriceText: "12.50", PriceCents: 1250, ReallyPriceText: "12.51", ReallyPriceCents: 1251,
		MerchantOrderName: "测试商品", Param: "uid=1",
	}
	fields, err := epayNotifyFields(value, testMerchantNo)
	if err != nil {
		t.Fatalf("构造回调字段失败: %v", err)
	}
	want := map[string]string{
		"pid":          testMerchantNo,
		"trade_no":     "20261113000001",
		"out_trade_no": "M20261113001",
		"type":         "alipay",
		"name":         "测试商品",
		"money":        "12.50",
		"trade_status": "TRADE_SUCCESS",
		"param":        "uid=1",
	}
	for name, expect := range want {
		if fields[name] != expect {
			t.Errorf("字段 %s 应为 %q，实际 %q", name, expect, fields[name])
		}
	}

	long := make([]byte, 0, 200)
	for len(long) < 200 {
		long = append(long, "测"...)
	}
	truncated := truncateEPayName(string(long))
	if len(truncated) > epayNameMaxBytes {
		t.Fatalf("name 必须截断到 %d 字节，实际 %d", epayNameMaxBytes, len(truncated))
	}
	if !utf8.ValidString(truncated) {
		t.Fatal("截断后的 name 必须是合法 UTF-8")
	}
}

// reissueOrders 模拟管理端补单路径：FindByIDForUpdate 返回答一单，Transition 记录顺序。
type reissueOrders struct {
	stubOrderRepository
	value       order.Order
	transitions []transitionCall
}

func (r *reissueOrders) FindByIDForUpdate(_ context.Context, id int64) (order.Order, error) {
	if id != r.value.ID {
		return order.Order{}, port.ErrNotFound
	}
	return r.value, nil
}

func (r *reissueOrders) Transition(_ context.Context, _ int64, from, to order.Status, _ time.Time) (bool, error) {
	r.transitions = append(r.transitions, transitionCall{from: from, to: to})
	return true, nil
}

var reissueTestNow = time.Unix(1_700_000_100, 0)

func newReissueService(t *testing.T, orders port.OrderRepository, settings port.SettingRepository,
	merchants port.MerchantRepository, keys port.MerchantKeyRepository, box *secret.Box,
	outbox port.OutboxRepository, transactions port.TransactionManager) *OrderService {
	t.Helper()
	service, err := NewOrderService(OrderServiceDeps{
		Transactions: transactions,
		Orders:       orders,
		PriceLocks:   grantingPriceLocks{},
		QRCodes:      emptyQRCodes{},
		Settings:     settings,
		Outbox:       outbox,
		Clock:        fakeClock{now: reissueTestNow},
		OrderIDs:     fakeOrderIDs{value: "20261113000001"},
		PublicTokens: &scriptedTokens{tokens: []string{testToken(51)}},
		Merchants:    merchants,
		MerchantKeys: keys,
		MasterKeys:   box,
	})
	if err != nil {
		t.Fatalf("构造订单用例失败: %v", err)
	}
	return service
}

func reissueTestOrder(protocol merchant.Protocol) order.Order {
	return order.Order{
		ID: 11, MerchantID: testMerchantID, Protocol: protocol,
		OrderID: "20261113000077", PublicToken: testToken(51), PayID: "M20261113077",
		Type: payment.Alipay, EpayType: "alipay",
		PriceText: "10.00", PriceCents: 1000,
		ReallyPriceText: "10.01", ReallyPriceCents: 1001,
		State:             order.StatusPending,
		MerchantOrderName: "测试商品",
		NotifyURL:         "https://merchant.test/notify",
		CreatedAt:         reissueTestNow.Add(-time.Minute),
	}
}

// TestReissueByProtocol 锁定管理端重签按 protocol 选签名器与 topic：
// 原生 order.paid.notify / V1 epay.v1.notify(GET+MD5) / V2 epay.v2.notify(POST+RSA+timestamp)。
func TestReissueByProtocol(t *testing.T) {
	box := newTestSecretBox(t)
	privatePEM, publicPEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成测试 RSA 密钥对失败: %v", err)
	}
	keys := epayMerchantKeys{values: map[string]merchant.Key{
		epayKeySlot(testMerchantID, merchant.ProtocolEPayV1): sealTestKey(t, box, testMerchantID, merchant.ProtocolEPayV1, []byte(testV1Secret)),
		epayKeySlot(testMerchantID, merchant.ProtocolEPayV2): sealTestKey(t, box, testMerchantID, merchant.ProtocolEPayV2, privatePEM),
	}}

	// protocol=0：原生 HMAC + order.paid.notify
	nativeOutbox := &recordingOutbox{}
	nativeOrders := &reissueOrders{value: reissueTestOrder(merchant.ProtocolNative)}
	nativeService := newReissueService(t, nativeOrders,
		newMapSettings(map[string]string{setting.MerchantKey: testMerchantKey}),
		newEPayMerchants(testMerchantID), keys, box, nativeOutbox, &passthroughTransactions{})
	if err := nativeService.ReissueByID(context.Background(), 11); err != nil {
		t.Fatalf("原生重签不应失败: %v", err)
	}
	nativeMessage := onlyOutboxMessage(t, nativeOutbox)
	if nativeMessage.Topic != notificationTopic {
		t.Fatalf("原生重签 topic 应为 %s，实际 %s", notificationTopic, nativeMessage.Topic)
	}
	var nativePayload NotificationPayload
	if err := json.Unmarshal(nativeMessage.Payload, &nativePayload); err != nil {
		t.Fatalf("解析原生重签载荷失败: %v", err)
	}
	nativeForm, err := url.ParseQuery(nativePayload.NewForm)
	if err != nil {
		t.Fatalf("解析原生重签 form 失败: %v", err)
	}
	wantSign := payment.CallbackSignV2("M20261113077", "", strconv.Itoa(int(payment.Alipay)), "10.00", "10.01", testMerchantKey)
	if nativeForm.Get("sign") != wantSign {
		t.Fatalf("原生重签签名应与核销路径一致，实际 %q", nativeForm.Get("sign"))
	}

	// protocol=1：商户 V1 密钥 + GET 完整地址
	v1Outbox := &recordingOutbox{}
	v1Orders := &reissueOrders{value: reissueTestOrder(merchant.ProtocolEPayV1)}
	v1Service := newReissueService(t, v1Orders,
		newMapSettings(map[string]string{setting.MerchantKey: testMerchantKey}),
		newEPayMerchants(testMerchantID), keys, box, v1Outbox, &passthroughTransactions{})
	if err := v1Service.ReissueByID(context.Background(), 11); err != nil {
		t.Fatalf("EPay V1 重签不应失败: %v", err)
	}
	v1Message := onlyOutboxMessage(t, v1Outbox)
	if v1Message.Topic != epayV1NotifyTopic {
		t.Fatalf("EPay V1 重签 topic 应为 %s，实际 %s", epayV1NotifyTopic, v1Message.Topic)
	}
	var v1Payload NotificationPayload
	if err := json.Unmarshal(v1Message.Payload, &v1Payload); err != nil {
		t.Fatalf("解析 EPay V1 重签载荷失败: %v", err)
	}
	if v1Payload.Method != http.MethodGet || v1Payload.URL == "" {
		t.Fatalf("EPay V1 重签必须是 GET + 完整地址，实际 %+v", v1Payload)
	}
	v1Query, parseErr := url.ParseQuery(strings.TrimPrefix(v1Payload.URL, "https://merchant.test/notify?"))
	if parseErr != nil {
		t.Fatalf("解析 EPay V1 重签地址失败: %v", parseErr)
	}
	if err := epay.VerifyV1(epayFieldsOf(v1Query, false), testV1Secret, v1Query.Get("sign")); err != nil {
		t.Fatalf("EPay V1 重签应能用商户密钥验签通过: %v", err)
	}

	// protocol=2：平台私钥 + POST form + timestamp
	v2Outbox := &recordingOutbox{}
	v2Orders := &reissueOrders{value: reissueTestOrder(merchant.ProtocolEPayV2)}
	v2Service := newReissueService(t, v2Orders,
		newMapSettings(map[string]string{setting.MerchantKey: testMerchantKey}),
		newEPayMerchants(testMerchantID), keys, box, v2Outbox, &passthroughTransactions{})
	if err := v2Service.ReissueByID(context.Background(), 11); err != nil {
		t.Fatalf("EPay V2 重签不应失败: %v", err)
	}
	v2Message := onlyOutboxMessage(t, v2Outbox)
	if v2Message.Topic != epayV2NotifyTopic {
		t.Fatalf("EPay V2 重签 topic 应为 %s，实际 %s", epayV2NotifyTopic, v2Message.Topic)
	}
	var v2Payload NotificationPayload
	if err := json.Unmarshal(v2Message.Payload, &v2Payload); err != nil {
		t.Fatalf("解析 EPay V2 重签载荷失败: %v", err)
	}
	if v2Payload.Method != http.MethodPost || v2Payload.Body == "" {
		t.Fatalf("EPay V2 重签必须是 POST + 字段快照，实际 %+v", v2Payload)
	}
	v2Form, parseErr := url.ParseQuery(v2Payload.NewForm)
	if parseErr != nil {
		t.Fatalf("解析 EPay V2 重签 form 失败: %v", parseErr)
	}
	if v2Form.Get("timestamp") != strconv.FormatInt(reissueTestNow.Unix(), 10) {
		t.Fatalf("EPay V2 重签必须带当前 timestamp，实际 %q", v2Form.Get("timestamp"))
	}
	if err := epay.VerifyV2(epayFieldsOf(v2Form, true), publicPEM, v2Form.Get("sign")); err != nil {
		t.Fatalf("EPay V2 重签应能用平台公钥验签通过: %v", err)
	}
}

func onlyOutboxMessage(t *testing.T, outbox *recordingOutbox) port.OutboxMessage {
	t.Helper()
	if len(outbox.messages) != 1 {
		t.Fatalf("应入队 1 条通知消息，实际 %d 条", len(outbox.messages))
	}
	return outbox.messages[0]
}

// TestReissueDegradesWhenSigningFails 锁定补单的解耦边界：
// 签名材料不可用时订单仍保持已支付（只降级为通知失败），返回 CodeConfiguration，
// 并且写入一条待重签的 dead 记录——绝不能把管理端补的单回滚掉。
func TestReissueDegradesWhenSigningFails(t *testing.T) {
	cases := []struct {
		name     string
		value    order.Order
		settings port.SettingRepository
		box      *secret.Box
	}{
		{
			name:     "EPay V1 密钥行缺失",
			value:    reissueTestOrder(merchant.ProtocolEPayV1),
			settings: newMapSettings(map[string]string{setting.MerchantKey: testMerchantKey}),
			box:      newTestSecretBox(t),
		},
		{
			name:     "原生系统密钥缺失",
			value:    reissueTestOrder(merchant.ProtocolNative),
			settings: newMapSettings(nil),
			box:      newTestSecretBox(t),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			events := newStagedEvents()
			transactions := &commitTrackingTransactions{events: events}
			outbox := &recordingOutbox{}
			orders := &reissueOrders{value: testCase.value}
			service := newReissueService(t, orders, testCase.settings,
				newEPayMerchants(testMerchantID), epayMerchantKeys{}, testCase.box, outbox, transactions)

			err := service.ReissueByID(context.Background(), testCase.value.ID)
			if err == nil {
				t.Fatal("签名材料缺失必须报错，不能静默成功")
			}
			if code, ok := ErrorCodeOf(err); !ok || code != CodeConfiguration {
				t.Fatalf("应返回 %s，实际 %v", CodeConfiguration, err)
			}
			if transactions.rolledBack != 0 {
				t.Fatalf("重签失败不得回滚补单事实，实际回滚 %d 次", transactions.rolledBack)
			}

			want := []transitionCall{
				{from: order.StatusPending, to: order.StatusPaid},
				{from: order.StatusPaid, to: order.StatusNotifyFailed},
			}
			if len(orders.transitions) != len(want) {
				t.Fatalf("状态转换应为 %+v，实际 %+v", want, orders.transitions)
			}
			for index := range want {
				if orders.transitions[index] != want[index] {
					t.Fatalf("第 %d 次状态转换应为 %+v，实际 %+v", index, want[index], orders.transitions[index])
				}
			}

			message := onlyOutboxMessage(t, outbox)
			if message.Topic != notifyTopicFor(testCase.value.Protocol) {
				t.Fatalf("dead 记录应落协议对应 topic，实际 %s", message.Topic)
			}
			var payload NotificationPayload
			if err := json.Unmarshal(message.Payload, &payload); err != nil {
				t.Fatalf("解析 dead 记录载荷失败: %v", err)
			}
			if !payload.Dead || payload.Reason == "" {
				t.Fatalf("dead 记录必须带 Dead 标记与原因，实际 %+v", payload)
			}
		})
	}
}
