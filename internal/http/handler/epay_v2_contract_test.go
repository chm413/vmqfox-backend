package handler

// V2 报文契约：这些用例锁的是「V2 商户到底会收到什么形状的响应」。
//
// 为什么单独一个文件：V2 的字段集、签名覆盖范围、sign_type 取值是接入方实现验签的
// 唯一依据，任何一处漂移都会让商户侧「响应不可信」，而现象只是「对接不上」。
// V1 的报文必须逐字节不变——存量插件按字段存在与否做判断，多一个 timestamp 就可能出问题。

import (
	"net/http"
	"testing"

	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/usecase"
)

// TestEPayV2MAPIResponseContract 锁定 V2 的 mapi 成功响应：
// 必须带 timestamp / sign / sign_type=RSA，且签名字段集恰好是业务四字段。
//
// 签名域漏一个字段或混进 sign_type，商户侧重建的待签串就对不上——
// 这是联调阶段最常见、也最难自查的一类失败。
func TestEPayV2MAPIResponseContract(t *testing.T) {
	manager := &stubEPayManager{
		protocol:      merchant.ProtocolEPayV2,
		signTimestamp: "1794900000",
		signValue:     "V2-SIGN-BASE64",
		createResult: usecase.EPayCreateResult{
			TradeNo: "20261117000001",
			PayURL:  "https://pay.example.test/#/payment/token",
			Qrcode:  "https://qr.alipay.com/abc",
		},
	}
	handler := NewEPayV1Handler(manager)

	c, recorder := epayV1TestContext(t, http.MethodPost, "http://gw.test/mapi.php",
		"pid=M1&type=alipay&money=0.01&out_trade_no=N2&timestamp=1794900000&sign=deadbeef&sign_type=RSA")
	handler.MAPI(c)

	payload := decodeEPayJSON(t, recorder)
	if code := epayCodeOf(t, payload); code != 1 {
		t.Fatalf("V2 下单应成功，实际 code=%d（%v）", code, payload)
	}
	if got, _ := payload["timestamp"].(string); got != "1794900000" {
		t.Fatalf("V2 响应必须带 timestamp，实际 %q", got)
	}
	if got, _ := payload["sign"].(string); got != "V2-SIGN-BASE64" {
		t.Fatalf("V2 响应必须带 sign，实际 %q", got)
	}
	if got, _ := payload["sign_type"].(string); got != "RSA" {
		t.Fatalf("V2 响应的 sign_type 必须是 RSA，实际 %q", got)
	}

	// 签名域：恰好这四个业务字段，多一个少一个都会让商户验不过。
	if manager.signCalls != 1 {
		t.Fatalf("V2 响应必须调用一次签名，实际 %d 次", manager.signCalls)
	}
	want := map[string]string{
		"code":     "1",
		"trade_no": "20261117000001",
		"payurl":   "https://pay.example.test/#/payment/token",
		"qrcode":   "https://qr.alipay.com/abc",
	}
	if len(manager.signFields) != len(want) {
		t.Fatalf("签名域字段数应为 %d，实际 %d：%v", len(want), len(manager.signFields), manager.signFields)
	}
	for name, value := range want {
		if got := manager.signFields[name]; got != value {
			t.Fatalf("签名域 %s 应为 %q，实际 %q", name, value, got)
		}
	}
}

// TestEPayV1MAPIResponseHasNoV2Fields 锁定 V1 报文不被 V2 污染：
// 存量插件按字段存在与否做判断，多出 timestamp/sign/sign_type 可能直接解析失败。
func TestEPayV1MAPIResponseHasNoV2Fields(t *testing.T) {
	manager := &stubEPayManager{createResult: usecase.EPayCreateResult{
		TradeNo: "20261117000002",
		PayURL:  "https://pay.example.test/#/payment/token2",
		Qrcode:  "wxp://f2f0",
	}}
	handler := NewEPayV1Handler(manager)

	c, recorder := epayV1TestContext(t, http.MethodPost, "http://gw.test/mapi.php",
		"pid=M1&type=wxpay&money=12.50&out_trade_no=N3&sign=deadbeef&sign_type=MD5")
	handler.MAPI(c)

	payload := decodeEPayJSON(t, recorder)
	if code := epayCodeOf(t, payload); code != 1 {
		t.Fatalf("V1 下单应成功，实际 code=%d（%v）", code, payload)
	}
	for _, field := range []string{"timestamp", "sign", "sign_type"} {
		if _, exists := payload[field]; exists {
			t.Fatalf("V1 响应不得出现 %s 字段（会污染存量插件解析），实际 %v", field, payload)
		}
	}
	if manager.signCalls != 0 {
		t.Fatalf("V1 响应不应触发签名，实际 %d 次", manager.signCalls)
	}
}

// TestEPayV2QueryResponseContract 锁定查单响应：
// 商户拿它做对账，字段集与签名同样必须稳定。
func TestEPayV2QueryResponseContract(t *testing.T) {
	manager := &stubEPayManager{
		protocol:      merchant.ProtocolEPayV2,
		signTimestamp: "1794900001",
		signValue:     "QUERY-SIGN",
		queryResult: usecase.EPayQueryResult{
			TradeNo:     "20261117000003",
			OutTradeNo:  "N4",
			Type:        "alipay",
			Name:        "对账商品",
			Money:       "0.01",
			TradeStatus: "TRADE_SUCCESS",
			Param:       "biz",
		},
	}
	handler := NewEPayV1Handler(manager)

	c, recorder := epayV1TestContext(t, http.MethodPost, "http://gw.test/api.php",
		"act=order&pid=M1&out_trade_no=N4&timestamp=1794900001&sign=deadbeef&sign_type=RSA")
	handler.API(c)

	payload := decodeEPayJSON(t, recorder)
	if code := epayCodeOf(t, payload); code != 1 {
		t.Fatalf("查单应成功，实际 code=%d（%v）", code, payload)
	}
	if got, _ := payload["sign_type"].(string); got != "RSA" {
		t.Fatalf("查单响应的 sign_type 必须是 RSA，实际 %q", got)
	}
	if manager.signCalls != 1 {
		t.Fatalf("查单响应必须签名一次，实际 %d 次", manager.signCalls)
	}
	// 对账字段一个都不能漏：商户按 out_trade_no 对账、按 money 核金额。
	for _, field := range []string{"code", "pid", "trade_no", "out_trade_no", "type", "name", "money", "trade_status", "param"} {
		if _, exists := manager.signFields[field]; !exists {
			t.Fatalf("查单签名域缺少 %s：%v", field, manager.signFields)
		}
	}
}

// TestEPayV2MAPIWithSignFailureFailsClosed 锁定失败方向：
// 平台私钥不可用（解密失败、密钥缺失）时，宁可不返回成功报文，也不能回一个
// code=1 却没有签名的响应——商户侧无法区分「真没签」和「被中间人剥掉签名」。
func TestEPayV2MAPIWithSignFailureFailsClosed(t *testing.T) {
	manager := &stubEPayManager{
		protocol: merchant.ProtocolEPayV2,
		signErr:  errStubSignFailure{},
		createResult: usecase.EPayCreateResult{
			TradeNo: "20261117000004",
			PayURL:  "https://pay.example.test/#/payment/token3",
			Qrcode:  "wxp://f2f0",
		},
	}
	handler := NewEPayV1Handler(manager)

	c, recorder := epayV1TestContext(t, http.MethodPost, "http://gw.test/mapi.php",
		"pid=M1&type=wxpay&money=0.01&out_trade_no=N5&timestamp=1794900000&sign=deadbeef&sign_type=RSA")
	handler.MAPI(c)

	payload := decodeEPayJSON(t, recorder)
	if code := epayCodeOf(t, payload); code == 1 {
		t.Fatalf("签名失败时绝不能返回 code=1，实际 %v", payload)
	}
	if msg, _ := payload["msg"].(string); msg != "服务异常" {
		t.Fatalf("签名失败应回「服务异常」，实际 %q", msg)
	}
	if _, exists := payload["sign"]; exists {
		t.Fatal("签名失败时不应带 sign 字段")
	}
}

type errStubSignFailure struct{}

func (errStubSignFailure) Error() string { return "stub: 平台私钥不可用" }
