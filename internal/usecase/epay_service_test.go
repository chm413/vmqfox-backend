package usecase

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
	"github.com/hulisang/vmqfox-backend/internal/domain/qrcode"
	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// EPay 下单用例的测试替身与用例。
//
// 复用的基础替身来自 fakes_test.go（mapSettings / fakeClock / fakeOrderIDs /
// scriptedTokens / passthroughTransactions / stubOrderRepository），
// 只在这里补 EPay 特有的「可控制金额锁与精确码」的部分。

var epayTestNow = time.Unix(1_700_000_000, 0)

// epayV1Merchants 按 pid 查商户；其余方法不会被 EPay 下单路径调用。
type epayV1Merchants struct{ byNo map[string]merchant.Merchant }

func (m epayV1Merchants) Create(_ context.Context, value merchant.Merchant) (merchant.Merchant, error) {
	return value, nil
}

func (m epayV1Merchants) FindByID(_ context.Context, id int64) (merchant.Merchant, error) {
	for _, value := range m.byNo {
		if value.ID == id {
			return value, nil
		}
	}
	return merchant.Merchant{}, port.ErrNotFound
}

func (m epayV1Merchants) FindByNo(_ context.Context, no string) (merchant.Merchant, error) {
	value, ok := m.byNo[no]
	if !ok {
		return merchant.Merchant{}, port.ErrNotFound
	}
	return value, nil
}

func (m epayV1Merchants) List(context.Context) ([]merchant.Merchant, error) { return nil, nil }

func (m epayV1Merchants) Update(context.Context, merchant.Merchant) error { return nil }

func (m epayV1Merchants) Delete(context.Context, int64) error { return nil }

func (m epayV1Merchants) Count(context.Context) (int64, error) { return int64(len(m.byNo)), nil }

// epayV1Keys 只实现 FindActive：下单路径只会取当前代密钥。
type epayV1Keys struct{ active map[string]merchant.Key }

func epayV1KeySlot(merchantID int64, protocol merchant.Protocol) string {
	return strconv.FormatInt(merchantID, 10) + "/" + strconv.Itoa(int(protocol))
}

func (k epayV1Keys) FindActive(_ context.Context, merchantID int64, protocol merchant.Protocol) (merchant.Key, error) {
	key, ok := k.active[epayV1KeySlot(merchantID, protocol)]
	if !ok {
		return merchant.Key{}, port.ErrNotFound
	}
	return key, nil
}

func (k epayV1Keys) FindAllForVerify(context.Context, int64, merchant.Protocol) ([]merchant.Key, error) {
	return nil, nil
}

func (k epayV1Keys) Upsert(context.Context, merchant.Key) error { return nil }

func (k epayV1Keys) Rotate(context.Context, int64, merchant.Protocol, merchant.Key) error { return nil }

func (k epayV1Keys) DeleteByMerchant(context.Context, int64) error { return nil }

// epayV1QRCodes 只提供「指定金额是否有精确码」，其余查询按不存在处理。
type epayV1QRCodes struct {
	emptyQRCodes
	exact   map[int64]string
	queried []int64
}

func (q *epayV1QRCodes) FindEnabledByAmount(_ context.Context, typ payment.Type, amountCents int64) (qrcode.QRCode, error) {
	q.queried = append(q.queried, amountCents)
	if payURL, ok := q.exact[amountCents]; ok {
		return qrcode.QRCode{PayURL: payURL, PriceCents: amountCents, Type: typ, State: qrcode.StateEnabled}, nil
	}
	return qrcode.QRCode{}, port.ErrNotFound
}

// epayV1PriceLocks 记录每次占锁尝试，可按金额模拟「已被占用」。
type epayV1PriceLocks struct {
	taken    map[int64]bool
	attempts []int64
}

func (p *epayV1PriceLocks) TryAcquire(_ context.Context, _ string, _ payment.Type, amountCents int64) (bool, error) {
	p.attempts = append(p.attempts, amountCents)
	if p.taken[amountCents] {
		return false, nil
	}
	return true, nil
}

func (p *epayV1PriceLocks) ReleaseByOrderID(context.Context, string) error { return nil }

func (p *epayV1PriceLocks) ReleaseByOrdersBefore(context.Context, time.Time) error { return nil }

func (p *epayV1PriceLocks) ReleaseOrphaned(context.Context) error { return nil }

// epayV1Orders 是带 (merchant_id, pay_id) 唯一性的内存订单表，用来验证幂等回放。
type epayV1Orders struct {
	stubOrderRepository
	byKey   map[string]order.Order
	created []order.Order
}

func epayOrderKey(merchantID int64, payID string) string {
	return strconv.FormatInt(merchantID, 10) + "|" + payID
}

func (r *epayV1Orders) FindByPayID(_ context.Context, merchantID int64, payID string) (order.Order, error) {
	value, ok := r.byKey[epayOrderKey(merchantID, payID)]
	if !ok {
		return order.Order{}, port.ErrNotFound
	}
	return value, nil
}

func (r *epayV1Orders) FindByPayIDForUpdate(ctx context.Context, merchantID int64, payID string) (order.Order, error) {
	return r.FindByPayID(ctx, merchantID, payID)
}

func (r *epayV1Orders) Create(_ context.Context, value order.Order) (order.Order, error) {
	if r.byKey == nil {
		r.byKey = make(map[string]order.Order)
	}
	key := epayOrderKey(value.MerchantID, value.PayID)
	if _, exists := r.byKey[key]; exists {
		return order.Order{}, port.ErrConflict
	}
	value.ID = int64(len(r.created) + 1)
	r.byKey[key] = value
	r.created = append(r.created, value)
	return value, nil
}

// epayV1NotifyURLs 是可控的回调地址校验器。
type epayV1NotifyURLs struct{ reject bool }

func (v epayV1NotifyURLs) ValidateNotifyURL(string) error {
	if v.reject {
		return errors.New("notify_url 指向私网地址")
	}
	return nil
}

var (
	_ port.MerchantRepository    = epayV1Merchants{}
	_ port.MerchantKeyRepository = epayV1Keys{}
	_ port.QRCodeRepository      = (*epayV1QRCodes)(nil)
	_ port.PriceLockRepository   = (*epayV1PriceLocks)(nil)
)

// epayTestEnv 暴露各个替身，便于用例断言「到底有没有碰资金」。
type epayTestEnv struct {
	orders   *epayV1Orders
	qrcodes  *epayV1QRCodes
	locks    *epayV1PriceLocks
	settings *mapSettings
	box      *secret.Box
	keys     epayV1Keys
}

func epayV1TestBox(t *testing.T) *secret.Box {
	t.Helper()
	box, err := secret.New(map[int][]byte{1: bytes.Repeat([]byte{0x42}, 32)}, 1)
	if err != nil {
		t.Fatalf("构造主密钥 Box 失败: %v", err)
	}
	return box
}

// sealEPayV1Key 用生产同款 AAD 加密商户密钥，让解密路径真的被跑到。
func sealEPayV1Key(t *testing.T, box *secret.Box, merchantID int64, plain string) merchant.Key {
	t.Helper()
	key := merchant.Key{MerchantID: merchantID, Protocol: merchant.ProtocolEPayV1, Generation: merchant.GenerationCurrent}
	cipherText, nonce, version, err := box.Seal([]byte(plain), key.AAD())
	if err != nil {
		t.Fatalf("加密商户密钥失败: %v", err)
	}
	key.SecretCipher = cipherText
	key.SecretNonce = nonce
	key.KeyVersion = version
	return key
}

func epayV1TestService(t *testing.T, settings map[string]string) (*EPayService, *epayTestEnv) {
	t.Helper()

	box := epayV1TestBox(t)
	env := &epayTestEnv{
		orders:  &epayV1Orders{byKey: make(map[string]order.Order)},
		qrcodes: &epayV1QRCodes{exact: make(map[int64]string)},
		locks:   &epayV1PriceLocks{taken: make(map[int64]bool)},
		settings: newMapSettings(map[string]string{
			setting.MonitorStateKey: "1",
			setting.AlipayPayURLKey: "https://qr.test/alipay",
			setting.WechatPayURLKey: "https://qr.test/wechat",
			setting.PriceAdjustKey:  "0",
			setting.CloseMinutesKey: "30",
			epayRequireExactCodeKey: "1",
		}),
		box: box,
	}
	for key, value := range settings {
		env.settings.values[key] = value
	}

	core, err := NewOrderService(OrderServiceDeps{
		Transactions: &passthroughTransactions{},
		Orders:       env.orders,
		PriceLocks:   env.locks,
		QRCodes:      env.qrcodes,
		Settings:     env.settings,
		Outbox:       &recordingOutbox{},
		Clock:        fakeClock{now: epayTestNow},
		OrderIDs:     fakeOrderIDs{value: "20261113000001"},
		PublicTokens: &scriptedTokens{tokens: []string{testToken(1), testToken(2), testToken(3)}},
		FrontendURL:  "https://pay.example.test/",
	})
	if err != nil {
		t.Fatalf("构造订单用例失败: %v", err)
	}

	merchants := epayV1Merchants{byNo: map[string]merchant.Merchant{
		testMerchantNo: {ID: testMerchantID, No: testMerchantNo, Enabled: true},
	}}
	env.keys = epayV1Keys{active: map[string]merchant.Key{
		epayV1KeySlot(testMerchantID, merchant.ProtocolEPayV1): sealEPayV1Key(t, box, testMerchantID, testV1Secret),
	}}

	service, err := NewEPayService(EPayServiceDeps{
		Orders:       core,
		Merchants:    merchants,
		MerchantKeys: env.keys,
		MasterKeys:   box,
		NotifyURLs:   epayV1NotifyURLs{},
	})
	if err != nil {
		t.Fatalf("构造 EPay 下单用例失败: %v", err)
	}
	return service, env
}

func epayV1Input(outTradeNo, money string) EPayCreateInput {
	return EPayCreateInput{
		MerchantID: testMerchantID,
		Protocol:   merchant.ProtocolEPayV1,
		OutTradeNo: outTradeNo,
		Type:       "alipay",
		Name:       "测试商品",
		Money:      money,
		NotifyURL:  "https://m.example.com/notify",
		ReturnURL:  "https://m.example.com/return",
		Param:      "biz-1",
	}
}

// TestEPayCreateSkipsPricesWithoutExactCode 覆盖「严格等额」门槛的核心：
// 基础金额没有精确码时**不能占锁**，直接跳到下一个有码的候选。
// 若先占锁再查码，那个没有码的金额会被白白占死，且订单会指向一个不存在的收款码。
func TestEPayCreateSkipsPricesWithoutExactCode(t *testing.T) {
	service, env := epayV1TestService(t, nil)
	env.qrcodes.exact[1251] = "https://qr.test/1251"

	result, err := service.Create(context.Background(), epayV1Input("ORDER-1", "12.50"))
	if err != nil {
		t.Fatalf("下单应成功，实际 err=%v", err)
	}
	if len(env.orders.created) != 1 {
		t.Fatalf("应写入 1 笔订单，实际 %d", len(env.orders.created))
	}
	created := env.orders.created[0]
	if created.ReallyPriceCents != 1251 {
		t.Fatalf("应跳到有精确码的 1251，实际 %d", created.ReallyPriceCents)
	}
	if len(env.locks.attempts) != 1 || env.locks.attempts[0] != 1251 {
		t.Fatalf("只应尝试占用 1251，实际 %v", env.locks.attempts)
	}
	if created.PayURL != "https://qr.test/1251" {
		t.Fatalf("收款码应取精确码，实际 %q", created.PayURL)
	}
	if result.Qrcode != "https://qr.test/1251" {
		t.Fatalf("响应 qrcode 应是收款码内容，实际 %q", result.Qrcode)
	}
	if result.PayURL != "https://pay.example.test/#/payment/"+created.PublicToken {
		t.Fatalf("响应 payurl 应是收银台链接，实际 %q", result.PayURL)
	}
}

// TestEPayCreateIgnoresPriceAdjustSettingAndOnlyIncreases 锁定两件事：
// 1) 金额只会往上调（设计 §3.2），后台 payQf=2（向下）对 EPay 无效；
// 2) 同金额被占时推进到下一个候选。
func TestEPayCreateIgnoresPriceAdjustSettingAndOnlyIncreases(t *testing.T) {
	service, env := epayV1TestService(t, map[string]string{setting.PriceAdjustKey: "2"})
	env.qrcodes.exact[1250] = "https://qr.test/1250"
	env.qrcodes.exact[1251] = "https://qr.test/1251"
	env.qrcodes.exact[1249] = "https://qr.test/1249"
	env.locks.taken[1250] = true

	if _, err := service.Create(context.Background(), epayV1Input("ORDER-2", "12.50")); err != nil {
		t.Fatalf("下单应成功，实际 err=%v", err)
	}
	created := env.orders.created[0]
	if created.ReallyPriceCents != 1251 {
		t.Fatalf("应推进到 1251，实际 %d（若为 1249 说明读了下调设置）", created.ReallyPriceCents)
	}
	if len(env.locks.attempts) != 2 || env.locks.attempts[0] != 1250 || env.locks.attempts[1] != 1251 {
		t.Fatalf("占锁顺序应为 [1250 1251]，实际 %v", env.locks.attempts)
	}
}

// TestEPayCreateOverloadedWhenNoExactCodeExists：所有候选都没有精确码时不能建单。
func TestEPayCreateOverloadedWhenNoExactCodeExists(t *testing.T) {
	service, env := epayV1TestService(t, nil)

	_, err := service.Create(context.Background(), epayV1Input("ORDER-3", "12.50"))
	if code, _ := ErrorCodeOf(err); code != CodeOverloaded {
		t.Fatalf("应返回 CodeOverloaded，实际 code=%v err=%v", code, err)
	}
	if len(env.locks.attempts) != 0 {
		t.Fatalf("没有精确码时不得占用任何金额，实际 %v", env.locks.attempts)
	}
	if len(env.orders.created) != 0 {
		t.Fatalf("没有精确码时不得建单，实际 %d 笔", len(env.orders.created))
	}
}

// TestEPayCreateLegacyPathWhenExactCodeDisabled：开关显式关闭时退回原生行为（不查精确码）。
func TestEPayCreateLegacyPathWhenExactCodeDisabled(t *testing.T) {
	service, env := epayV1TestService(t, map[string]string{
		epayRequireExactCodeKey: "0",
		setting.PriceAdjustKey:  "2",
	})

	if _, err := service.Create(context.Background(), epayV1Input("ORDER-4", "12.50")); err != nil {
		t.Fatalf("下单应成功，实际 err=%v", err)
	}
	created := env.orders.created[0]
	if created.ReallyPriceCents != 1250 {
		t.Fatalf("关闭严格等额后应按原价下单，实际 %d", created.ReallyPriceCents)
	}
	// 全局配置兜底码属于「需要用户手动输入金额」的通用码，必须标记 isAuto。
	if !created.IsAuto || created.PayURL != "https://qr.test/alipay" {
		t.Fatalf("应回落到系统设置里的支付宝码并标记 isAuto，实际 payURL=%q isAuto=%v", created.PayURL, created.IsAuto)
	}
}

// TestEPayCreateReplaysExistingOrderWithoutAcquiringPrice 是设计 §5.4 的硬约束：
// 幂等命中时不得重新 acquirePrice，否则 price_locks 撞自己的锁会让订单金额漂移。
func TestEPayCreateReplaysExistingOrderWithoutAcquiringPrice(t *testing.T) {
	service, env := epayV1TestService(t, nil)
	existing := order.Order{
		ID:          99,
		MerchantID:  testMerchantID,
		Protocol:    merchant.ProtocolEPayV1,
		OrderID:     "20261112999999",
		PublicToken: testToken(7),
		PayID:       "ORDER-5",
		Type:        payment.Alipay,
		EpayType:    "alipay",
		PriceCents:  1250,
		PriceText:   "12.50",
		State:       order.StatusPending,
		PayURL:      "https://qr.test/1250",
		NotifyURL:   "https://m.example.com/notify",
		CreatedAt:   epayTestNow,
	}
	env.orders.byKey[epayOrderKey(testMerchantID, existing.PayID)] = existing

	result, err := service.Create(context.Background(), epayV1Input("ORDER-5", "12.50"))
	if err != nil {
		t.Fatalf("幂等重放应成功，实际 err=%v", err)
	}
	if result.TradeNo != existing.OrderID {
		t.Fatalf("应返回原订单号 %q，实际 %q", existing.OrderID, result.TradeNo)
	}
	if result.PayURL != "https://pay.example.test/#/payment/"+existing.PublicToken {
		t.Fatalf("应返回原收银台链接，实际 %q", result.PayURL)
	}
	if len(env.locks.attempts) != 0 {
		t.Fatalf("幂等命中不得重新占锁，实际尝试 %v", env.locks.attempts)
	}
	if len(env.orders.created) != 0 {
		t.Fatalf("幂等命中不得新建订单，实际 %d 笔", len(env.orders.created))
	}
}

// TestEPayCreateRejectsReplayWithDifferentMoney：绝不静默按旧金额返回（等于替商户改价）。
func TestEPayCreateRejectsReplayWithDifferentMoney(t *testing.T) {
	service, env := epayV1TestService(t, nil)
	env.orders.byKey[epayOrderKey(testMerchantID, "ORDER-6")] = order.Order{
		MerchantID: testMerchantID,
		OrderID:    "20261112000001",
		PayID:      "ORDER-6",
		Type:       payment.Alipay,
		PriceCents: 1250,
		State:      order.StatusPending,
	}

	_, err := service.Create(context.Background(), epayV1Input("ORDER-6", "13.00"))
	if code, _ := ErrorCodeOf(err); code != CodeDuplicateOrder {
		t.Fatalf("金额不一致应返回 CodeDuplicateOrder，实际 code=%v err=%v", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "金额不一致") {
		t.Fatalf("文案应说明金额不一致，实际 %v", err)
	}
}

// TestEPayCreateRejectsReplayOnClosedOrder：已关闭订单不能再付。
func TestEPayCreateRejectsReplayOnClosedOrder(t *testing.T) {
	service, env := epayV1TestService(t, nil)
	env.orders.byKey[epayOrderKey(testMerchantID, "ORDER-7")] = order.Order{
		MerchantID: testMerchantID,
		OrderID:    "20261112000002",
		PayID:      "ORDER-7",
		Type:       payment.Alipay,
		PriceCents: 1250,
		State:      order.StatusClosed,
	}

	_, err := service.Create(context.Background(), epayV1Input("ORDER-7", "12.50"))
	if code, _ := ErrorCodeOf(err); code != CodeInvalidState {
		t.Fatalf("已关闭订单应返回 CodeInvalidState，实际 code=%v err=%v", code, err)
	}
}

// TestEPayCreateRejectsNotifyURLBeforeTouchingFunds：预校验失败必须不写单、不占锁。
func TestEPayCreateRejectsNotifyURLBeforeTouchingFunds(t *testing.T) {
	service, env := epayV1TestService(t, nil)
	service.notifyURLs = epayV1NotifyURLs{reject: true}

	_, err := service.Create(context.Background(), epayV1Input("ORDER-8", "12.50"))
	if code, _ := ErrorCodeOf(err); code != CodeInvalidArgument {
		t.Fatalf("回调地址不合规应返回 CodeInvalidArgument，实际 code=%v err=%v", code, err)
	}
	if len(env.locks.attempts) != 0 || len(env.orders.created) != 0 {
		t.Fatalf("预校验失败不得动资金或落库：attempts=%v created=%d", env.locks.attempts, len(env.orders.created))
	}
}

// TestEPayCreateWritesMerchantDimension 锁定落库字段：少写 merchant_id 会让回调签名全错，
// 而这类漏写在编译期与其它单测里都不会报错。
func TestEPayCreateWritesMerchantDimension(t *testing.T) {
	service, env := epayV1TestService(t, nil)
	env.qrcodes.exact[1250] = "https://qr.test/1250"

	if _, err := service.Create(context.Background(), epayV1Input("ORDER-9", "12.50")); err != nil {
		t.Fatalf("下单应成功，实际 err=%v", err)
	}
	created := env.orders.created[0]
	if created.MerchantID != testMerchantID {
		t.Fatalf("merchant_id 应为 %d，实际 %d", testMerchantID, created.MerchantID)
	}
	if created.Protocol != merchant.ProtocolEPayV1 {
		t.Fatalf("protocol 应为 V1，实际 %v", created.Protocol)
	}
	if created.EpayType != "alipay" || created.MerchantOrderName != "测试商品" {
		t.Fatalf("epay_type/merchant_order_name 落库不对: %q / %q", created.EpayType, created.MerchantOrderName)
	}
	if created.PriceText != "12.50" {
		t.Fatalf("price_text 必须保存商户原文，实际 %q", created.PriceText)
	}
	if created.NotifyURL != "https://m.example.com/notify" {
		t.Fatalf("notify_url 必须是商户传入值且不回落全局设置，实际 %q", created.NotifyURL)
	}
	if created.PayID != "ORDER-9" {
		t.Fatalf("out_trade_no 应映射到 pay_id，实际 %q", created.PayID)
	}
}

// TestEPayAuthenticateGoldenVector 用设计附录 A 的黄金值验证「pid→密钥→验签」整条链路，
// 并确认篡改金额后立刻变成「签名错误」。
func TestEPayAuthenticateGoldenVector(t *testing.T) {
	const goldenKey = "abcdefghjkmnpqrstuvwxyz23456789k"
	const goldenSign = "51978ac227dba94b887893ca5ba14917"

	service, env := epayV1TestService(t, nil)
	env.keys.active[epayV1KeySlot(testMerchantID, merchant.ProtocolEPayV1)] = sealEPayV1Key(t, env.box, testMerchantID, goldenKey)

	params := map[string]string{
		"pid":          testMerchantNo,
		"type":         "alipay",
		"out_trade_no": "M20261113001",
		"notify_url":   "https://m.example.com/notify",
		"return_url":   "https://m.example.com/return",
		"name":         "测试商品",
		"money":        "12.50",
		"sign":         goldenSign,
		"sign_type":    "MD5",
	}

	identity, err := service.Authenticate(context.Background(), params)
	if err != nil {
		t.Fatalf("黄金用例应验签通过，实际 err=%v", err)
	}
	if identity.ID != testMerchantID || identity.No != testMerchantNo {
		t.Fatalf("应解析出商户 %d/%s，实际 %d/%s", testMerchantID, testMerchantNo, identity.ID, identity.No)
	}

	tampered := make(map[string]string, len(params))
	for name, value := range params {
		tampered[name] = value
	}
	tampered["money"] = "13.50"
	if _, err := service.Authenticate(context.Background(), tampered); err == nil {
		t.Fatal("篡改金额后必须验签失败")
	} else if code, _ := ErrorCodeOf(err); code != CodeInvalidSignature {
		t.Fatalf("应返回 CodeInvalidSignature，实际 code=%v err=%v", code, err)
	}
}

// TestEPayAuthenticateHidesMerchantState 覆盖「商户不存在与停用同一句话」：
// 两者可区分的话，pid 就成了商户存在性探针。
func TestEPayAuthenticateHidesMerchantState(t *testing.T) {
	service, _ := epayV1TestService(t, nil)
	params := map[string]string{"pid": "MNOTEXIST00000", "sign": "00000000000000000000000000000000"}

	_, missingErr := service.Authenticate(context.Background(), params)
	if missingErr == nil {
		t.Fatal("不存在的商户必须拒绝")
	}
	if _, err := service.Authenticate(context.Background(), map[string]string{"pid": testMerchantNo}); err == nil {
		t.Fatal("缺 sign 也必须拒绝")
	}
	if code, _ := ErrorCodeOf(missingErr); code != CodeInvalidArgument {
		t.Fatalf("应返回 CodeInvalidArgument，实际 code=%v", code)
	}
	if !strings.Contains(missingErr.Error(), "商户不存在") {
		t.Fatalf("文案应为「商户不存在」，实际 %v", missingErr)
	}
}

// TestEPayQueryScopesByMerchantAndMapsStatus 覆盖查单的商户维度与两个对外状态。
func TestEPayQueryScopesByMerchantAndMapsStatus(t *testing.T) {
	service, env := epayV1TestService(t, nil)
	env.orders.byKey[epayOrderKey(testMerchantID, "ORDER-10")] = order.Order{
		MerchantID:        testMerchantID,
		OrderID:           "20261113000001",
		PayID:             "ORDER-10",
		Type:              payment.Alipay,
		EpayType:          "alipay",
		MerchantOrderName: "测试商品",
		PriceCents:        1250,
		PriceText:         "12.50",
		Param:             "biz-1",
		State:             order.StatusPending,
		CreatedAt:         epayTestNow,
	}

	result, err := service.Query(context.Background(), testMerchantID, "ORDER-10")
	if err != nil {
		t.Fatalf("查单应成功，实际 err=%v", err)
	}
	if result.TradeStatus != "WAIT_BUYER_PAY" {
		t.Fatalf("待支付应为 WAIT_BUYER_PAY，实际 %q", result.TradeStatus)
	}
	if result.Money != "12.50" || result.Type != "alipay" || result.Param != "biz-1" {
		t.Fatalf("查单字段不对: %+v", result)
	}
	if !result.EndTime.IsZero() {
		t.Fatalf("未支付不应有 endtime，实际 %v", result.EndTime)
	}

	// 跨商户必须查不到，且表现与「不存在」一致（不能透露订单是否存在）。
	if _, err := service.Query(context.Background(), testMerchantID+1, "ORDER-10"); err == nil {
		t.Fatal("跨商户查单必须返回不存在")
	} else if code, _ := ErrorCodeOf(err); code != CodeNotFound {
		t.Fatalf("应返回 CodeNotFound，实际 code=%v err=%v", code, err)
	}

	paid := env.orders.byKey[epayOrderKey(testMerchantID, "ORDER-10")]
	paid.State = order.StatusNotifyFailed
	paid.PaidAt = epayTestNow.Add(time.Minute)
	env.orders.byKey[epayOrderKey(testMerchantID, "ORDER-10")] = paid

	result, err = service.Query(context.Background(), testMerchantID, "ORDER-10")
	if err != nil {
		t.Fatalf("查单应成功，实际 err=%v", err)
	}
	if result.TradeStatus != "TRADE_SUCCESS" {
		t.Fatalf("通知失败也算已支付，实际 %q", result.TradeStatus)
	}
	if result.EndTime.IsZero() {
		t.Fatal("已支付应有 endtime")
	}
}

// TestEPayCreateRejectsUnsupportedType 锁定「未开通渠道明确拒绝、不回落」。
func TestEPayCreateRejectsUnsupportedType(t *testing.T) {
	service, env := epayV1TestService(t, nil)
	input := epayV1Input("ORDER-11", "12.50")
	input.Type = "qqpay"

	_, err := service.Create(context.Background(), input)
	if code, _ := ErrorCodeOf(err); code != CodeInvalidArgument {
		t.Fatalf("未开通渠道应返回 CodeInvalidArgument，实际 code=%v err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "支付方式未开通") {
		t.Fatalf("文案应为「支付方式未开通」，实际 %v", err)
	}
	if len(env.orders.created) != 0 {
		t.Fatalf("拒绝的请求不得落库，实际 %d 笔", len(env.orders.created))
	}
}

// TestEPayCreateRejectsMalformedMoney 锁定严格金额解析（宽松化会改变对方签名域）。
func TestEPayCreateRejectsMalformedMoney(t *testing.T) {
	service, _ := epayV1TestService(t, nil)
	for _, money := range []string{"", "1e2", " 1.00", "1.001", "-1.00", "0"} {
		if _, err := service.Create(context.Background(), epayV1Input("ORDER-12", money)); err == nil {
			t.Fatalf("金额 %q 应被拒绝", money)
		} else if code, _ := ErrorCodeOf(err); code != CodeInvalidArgument {
			t.Fatalf("金额 %q 应返回 CodeInvalidArgument，实际 code=%v", money, code)
		}
	}
}
