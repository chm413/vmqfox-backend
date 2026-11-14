package usecase

import (
	"context"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/epay"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// 本文件锁定 EPay 同步回跳的三分支（设计 §6.4）：签名密钥与字段集必须按协议切换，
// 否则 EPay 订单会出现「钱付了、跳回商户站却显示支付失败」。

// returnURLOrders 只服务公开令牌查询，其余方法留给基座。
type returnURLOrders struct {
	stubOrderRepository
	value order.Order
}

func (r returnURLOrders) FindByPublicToken(_ context.Context, token string) (order.Order, error) {
	if token != r.value.PublicToken {
		return order.Order{}, port.ErrNotFound
	}
	return r.value, nil
}

var _ port.OrderRepository = returnURLOrders{}

func newReturnURLService(t *testing.T, value order.Order, settings port.SettingRepository,
	merchants port.MerchantRepository, keys port.MerchantKeyRepository, box *secret.Box) *OrderService {
	t.Helper()
	service, err := NewOrderService(OrderServiceDeps{
		Transactions: &passthroughTransactions{},
		Orders:       returnURLOrders{value: value},
		PriceLocks:   grantingPriceLocks{},
		QRCodes:      emptyQRCodes{},
		Settings:     settings,
		Outbox:       &recordingOutbox{},
		Clock:        fakeClock{now: returnURLTestNow},
		OrderIDs:     fakeOrderIDs{value: "20261113000001"},
		PublicTokens: &scriptedTokens{tokens: []string{testToken(41)}},
		Merchants:    merchants,
		MerchantKeys: keys,
		MasterKeys:   box,
	})
	if err != nil {
		t.Fatalf("构造订单用例失败: %v", err)
	}
	return service
}

var returnURLTestNow = time.Unix(1_700_000_000, 0)

func returnURLTestOrder(protocol merchant.Protocol) order.Order {
	return order.Order{
		ID: 1, MerchantID: testMerchantID, Protocol: protocol,
		OrderID: "20261113000001", PublicToken: testToken(41), PayID: "M20261113001",
		Type: payment.Alipay, EpayType: "alipay",
		PriceText: "10.00", PriceCents: 1000,
		ReallyPriceText: "10.01", ReallyPriceCents: 1001,
		State:             order.StatusPaid,
		MerchantOrderName: "测试商品",
		Param:             "",
		ReturnURL:         "https://shop.test/return?from=epay",
	}
}

// epayFieldsOf 只挑出 EPay 签名域里的字段参与验签。
// return_url 自带的查询参数（如 from=epay）不属于回调协议，不参与验签。
func epayFieldsOf(query url.Values, withTimestamp bool) map[string]string {
	names := []string{"pid", "trade_no", "out_trade_no", "type", "name", "money", "trade_status", "param"}
	if withTimestamp {
		names = append(names, "timestamp")
	}
	params := make(map[string]string, len(names))
	for _, name := range names {
		if value, ok := query[name]; ok {
			params[name] = value[0]
		}
	}
	return params
}

func parseReturnURLEscape(t *testing.T, raw string) (url.Values, *url.URL) {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("回跳地址无法解析: %v", err)
	}
	return parsed.Query(), parsed
}

// TestReturnURLNativeKeepsExistingSignature 锁定原生回跳行为不变（settings.key + HMAC）。
func TestReturnURLNativeKeepsExistingSignature(t *testing.T) {
	service := newReturnURLService(t, returnURLTestOrder(merchant.ProtocolNative),
		newMapSettings(map[string]string{setting.MerchantKey: testMerchantKey}), nil, nil, nil)

	result, err := service.ReturnURL(context.Background(), testToken(41))
	if err != nil {
		t.Fatalf("原生回跳不应失败: %v", err)
	}
	query, parsed := parseReturnURLEscape(t, result.ReturnURL)
	if parsed.Host != "shop.test" || query.Get("from") != "epay" {
		t.Fatalf("原有查询参数必须保留，实际 %s", result.ReturnURL)
	}
	want := payment.CallbackSignV2("M20261113001", "", strconv.Itoa(int(payment.Alipay)), "10.00", "10.01", testMerchantKey)
	if query.Get("sign") != want {
		t.Fatalf("原生回跳签名应与改造前一致，实际 %q", query.Get("sign"))
	}
	if query.Get("payId") != "M20261113001" || query.Get("reallyPrice") != "10.01" {
		t.Fatalf("原生回跳字段集应保持不变，实际 %v", query)
	}
}

// TestReturnURLUsesMerchantV1Secret 锁定 protocol=1 用商户 V1 密钥 + EPay 字段集。
func TestReturnURLUsesMerchantV1Secret(t *testing.T) {
	box := newTestSecretBox(t)
	keys := epayMerchantKeys{values: map[string]merchant.Key{
		epayKeySlot(testMerchantID, merchant.ProtocolEPayV1): sealTestKey(t, box, testMerchantID, merchant.ProtocolEPayV1, []byte(testV1Secret)),
	}}
	service := newReturnURLService(t, returnURLTestOrder(merchant.ProtocolEPayV1),
		newMapSettings(map[string]string{setting.MerchantKey: testMerchantKey}),
		newEPayMerchants(testMerchantID), keys, box)

	result, err := service.ReturnURL(context.Background(), testToken(41))
	if err != nil {
		t.Fatalf("EPay V1 回跳不应失败: %v", err)
	}
	query, parsed := parseReturnURLEscape(t, result.ReturnURL)
	if parsed.Host != "shop.test" || query.Get("from") != "epay" {
		t.Fatalf("原有查询参数必须保留，实际 %s", result.ReturnURL)
	}
	if query.Get("sign_type") != "MD5" {
		t.Fatalf("V1 回跳 sign_type 必须是 MD5，实际 %q", query.Get("sign_type"))
	}
	if query.Get("money") != "10.00" {
		t.Fatalf("money 必须回下单金额，实际 %q", query.Get("money"))
	}
	if err := epay.VerifyV1(epayFieldsOf(query, false), testV1Secret, query.Get("sign")); err != nil {
		t.Fatalf("回跳签名应能用商户 V1 密钥验签通过: %v", err)
	}
	// 原生密钥签出来的字段不应该出现在 EPay 回跳里。
	if query.Get("payId") != "" || query.Get("reallyPrice") != "" {
		t.Fatalf("EPay 回跳不得混入原生字段，实际 %v", query)
	}
}

// TestReturnURLUsesMerchantV2PrivateKey 锁定 protocol=2 用商户平台私钥 + timestamp。
func TestReturnURLUsesMerchantV2PrivateKey(t *testing.T) {
	box := newTestSecretBox(t)
	privatePEM, publicPEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成测试 RSA 密钥对失败: %v", err)
	}
	keys := epayMerchantKeys{values: map[string]merchant.Key{
		epayKeySlot(testMerchantID, merchant.ProtocolEPayV2): sealTestKey(t, box, testMerchantID, merchant.ProtocolEPayV2, privatePEM),
	}}
	service := newReturnURLService(t, returnURLTestOrder(merchant.ProtocolEPayV2),
		newMapSettings(map[string]string{setting.MerchantKey: testMerchantKey}),
		newEPayMerchants(testMerchantID), keys, box)

	result, err := service.ReturnURL(context.Background(), testToken(41))
	if err != nil {
		t.Fatalf("EPay V2 回跳不应失败: %v", err)
	}
	query, _ := parseReturnURLEscape(t, result.ReturnURL)
	if query.Get("sign_type") != "RSA" {
		t.Fatalf("V2 回跳 sign_type 必须是 RSA，实际 %q", query.Get("sign_type"))
	}
	if query.Get("timestamp") != strconv.FormatInt(returnURLTestNow.Unix(), 10) {
		t.Fatalf("V2 回跳必须带当前时间戳，实际 %q", query.Get("timestamp"))
	}
	if err := epay.VerifyV2(epayFieldsOf(query, true), publicPEM, query.Get("sign")); err != nil {
		t.Fatalf("回跳签名应能用平台公钥验签通过: %v", err)
	}
}

// TestReturnURLRejectsUnpaidOrderBeforeSigning 锁定未支付订单不签发回跳（协议分支不得越过状态校验）。
func TestReturnURLRejectsUnpaidOrderBeforeSigning(t *testing.T) {
	unpaid := returnURLTestOrder(merchant.ProtocolEPayV1)
	unpaid.State = order.StatusPending
	box := newTestSecretBox(t)
	keys := epayMerchantKeys{values: map[string]merchant.Key{
		epayKeySlot(testMerchantID, merchant.ProtocolEPayV1): sealTestKey(t, box, testMerchantID, merchant.ProtocolEPayV1, []byte(testV1Secret)),
	}}
	service := newReturnURLService(t, unpaid,
		newMapSettings(map[string]string{setting.MerchantKey: testMerchantKey}),
		newEPayMerchants(testMerchantID), keys, box)

	_, err := service.ReturnURL(context.Background(), testToken(41))
	if err == nil {
		t.Fatal("未支付订单不应生成回跳签名")
	}
	if code, ok := ErrorCodeOf(err); !ok || code != CodeInvalidState {
		t.Fatalf("应返回 %s，实际 %v", CodeInvalidState, err)
	}
}

// TestReturnURLFailsWithoutMerchantKeyMaterial 锁定密钥缺失时不退化成原生签名：
// 宁可报配置错误，也不能给商户一个必然验签失败的地址。
func TestReturnURLFailsWithoutMerchantKeyMaterial(t *testing.T) {
	service := newReturnURLService(t, returnURLTestOrder(merchant.ProtocolEPayV1),
		newMapSettings(map[string]string{setting.MerchantKey: testMerchantKey}),
		newEPayMerchants(testMerchantID), epayMerchantKeys{}, newTestSecretBox(t))

	if _, err := service.ReturnURL(context.Background(), testToken(41)); err == nil {
		t.Fatal("缺少商户密钥时必须报错")
	} else if code, ok := ErrorCodeOf(err); !ok || code != CodeConfiguration {
		t.Fatalf("应返回 %s，实际 %v", CodeConfiguration, err)
	}
}
