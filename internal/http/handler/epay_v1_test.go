package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/usecase"
)

// stubEPayManager 记录调用次数与入参，用来断言「被拒绝的请求到底有没有走到用例层」。
type stubEPayManager struct {
	identity  merchant.Merchant
	authErr   error
	authCalls int

	createResult usecase.EPayCreateResult
	createErr    error
	createCalls  int
	createInput  usecase.EPayCreateInput

	queryResult     usecase.EPayQueryResult
	queryErr        error
	queryCalls      int
	queryMerchant   int64
	queryOutTradeNo string
}

func (m *stubEPayManager) Authenticate(context.Context, map[string]string) (merchant.Merchant, error) {
	m.authCalls++
	if m.authErr != nil {
		return merchant.Merchant{}, m.authErr
	}
	if m.identity.ID == 0 {
		return merchant.Merchant{ID: 7, No: "M7K3QPX9Z2AB4C", Enabled: true}, nil
	}
	return m.identity, nil
}

func (m *stubEPayManager) Create(_ context.Context, input usecase.EPayCreateInput) (usecase.EPayCreateResult, error) {
	m.createCalls++
	m.createInput = input
	return m.createResult, m.createErr
}

func (m *stubEPayManager) Query(_ context.Context, merchantID int64, outTradeNo string) (usecase.EPayQueryResult, error) {
	m.queryCalls++
	m.queryMerchant = merchantID
	m.queryOutTradeNo = outTradeNo
	return m.queryResult, m.queryErr
}

func epayV1TestContext(t *testing.T, method, target, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	c.Request = httptest.NewRequest(method, target, reader)
	if body != "" {
		c.Request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c, recorder
}

func decodeEPayJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v，body=%s", err, recorder.Body.String())
	}
	return payload
}

func epayCodeOf(t *testing.T, payload map[string]any) int {
	t.Helper()
	raw, ok := payload["code"]
	if !ok {
		t.Fatalf("响应缺少 code 字段: %v", payload)
	}
	number, ok := raw.(float64)
	if !ok {
		t.Fatalf("code 不是数字: %#v", raw)
	}
	return int(number)
}

// TestEPaySubmitRedirectsToCashier 锁定 submit.php 的成功行为：302 且 Location 就是 payurl。
func TestEPaySubmitRedirectsToCashier(t *testing.T) {
	manager := &stubEPayManager{createResult: usecase.EPayCreateResult{
		TradeNo: "20261113000001",
		PayURL:  "https://pay.example.test/#/payment/abc",
		Qrcode:  "https://qr.test/1250",
	}}
	handler := NewEPayV1Handler(manager)

	c, recorder := epayV1TestContext(t, http.MethodGet,
		"http://gw.test/submit.php?pid=M7K3QPX9Z2AB4C&money=12.50&out_trade_no=N1&sign=deadbeef", "")
	handler.Submit(c)

	if recorder.Code != http.StatusFound {
		t.Fatalf("submit.php 成功应 302，实际 %d", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != manager.createResult.PayURL {
		t.Fatalf("Location 应等于 payurl，实际 %q", location)
	}
	if manager.createInput.OutTradeNo != "N1" || manager.createInput.Money != "12.50" {
		t.Fatalf("参数映射不对: %+v", manager.createInput)
	}
	if manager.createInput.Protocol != merchant.ProtocolEPayV1 {
		t.Fatalf("EPay 入口的 protocol 必须是 V1，实际 %v", manager.createInput.Protocol)
	}
}

// TestEPaySubmitRejectsDuplicateParams 覆盖设计 §5.5 #1：
// 同名参数出现两次时必须拒绝，且不能走到用例层（否则「取第一个还是最后一个」的差异会被利用）。
func TestEPaySubmitRejectsDuplicateParams(t *testing.T) {
	manager := &stubEPayManager{}
	handler := NewEPayV1Handler(manager)

	c, recorder := epayV1TestContext(t, http.MethodGet,
		"http://gw.test/submit.php?pid=M1&money=12.50&money=1.00", "")
	handler.Submit(c)

	if manager.authCalls != 0 {
		t.Fatalf("参数重复必须在入口拒绝，实际进入验签 %d 次", manager.authCalls)
	}
	if body := recorder.Body.String(); !strings.Contains(body, epayMessageAmbiguousParams) {
		t.Fatalf("响应应包含 %q，实际 %s", epayMessageAmbiguousParams, body)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("submit.php 的失败页应是 HTML，实际 %q", contentType)
	}
}

// TestEPaySubmitRejectsSignCaseVariant 覆盖设计 §5.5 #2：sign/sign_type 的大小写变体一律拒绝。
func TestEPaySubmitRejectsSignCaseVariant(t *testing.T) {
	for _, query := range []string{"Sign=deadbeef", "SIGN=deadbeef", "sign_type=MD5&Sign_Type=MD5"} {
		manager := &stubEPayManager{}
		handler := NewEPayV1Handler(manager)

		c, recorder := epayV1TestContext(t, http.MethodGet, "http://gw.test/submit.php?pid=M1&"+query, "")
		handler.Submit(c)

		if manager.authCalls != 0 {
			t.Fatalf("%s 必须被入口拒绝，实际进入验签 %d 次", query, manager.authCalls)
		}
		if body := recorder.Body.String(); !strings.Contains(body, epayMessageAmbiguousParams) {
			t.Fatalf("%s 的响应应包含 %q，实际 %s", query, epayMessageAmbiguousParams, body)
		}
	}
}

// TestEPaySubmitEscapesErrorMessage：错误文案可能带商户可控内容，必须转义后再进 HTML。
func TestEPaySubmitEscapesErrorMessage(t *testing.T) {
	manager := &stubEPayManager{createErr: &usecase.Error{
		Code:    usecase.CodeConfiguration,
		Message: `暂无可用支付二维码<script>alert(1)</script>`,
	}}
	handler := NewEPayV1Handler(manager)

	c, recorder := epayV1TestContext(t, http.MethodGet,
		"http://gw.test/submit.php?pid=M1&money=12.50", "")
	handler.Submit(c)

	body := recorder.Body.String()
	if !strings.Contains(body, "暂无可用支付二维码") {
		t.Fatalf("错误页应展示用例文案，实际 %s", body)
	}
	if strings.Contains(body, "<script>") {
		t.Fatalf("错误文案必须转义，实际 %s", body)
	}
}

// TestEPayMAPIResponseEnvelope 锁定 mapi.php 的 JSON 契约：code=1、msg 空串、字段齐全。
func TestEPayMAPIResponseEnvelope(t *testing.T) {
	manager := &stubEPayManager{createResult: usecase.EPayCreateResult{
		TradeNo: "20261113000001",
		PayURL:  "https://pay.example.test/#/payment/abc",
		Qrcode:  "wxp://f2f0",
		IsAuto:  false,
	}}
	handler := NewEPayV1Handler(manager)

	c, recorder := epayV1TestContext(t, http.MethodPost, "http://gw.test/mapi.php",
		"pid=M1&type=wxpay&money=12.50&out_trade_no=N2&sign=deadbeef")
	handler.MAPI(c)

	payload := decodeEPayJSON(t, recorder)
	if code := epayCodeOf(t, payload); code != 1 {
		t.Fatalf("成功应 code=1，实际 %d（%v）", code, payload)
	}
	if msg, _ := payload["msg"].(string); msg != "" {
		t.Fatalf("成功时 msg 必须为空串（设计 §5.5 #12），实际 %q", msg)
	}
	for field, want := range map[string]string{
		"trade_no":  "20261113000001",
		"payurl":    "https://pay.example.test/#/payment/abc",
		"qrcode":    "wxp://f2f0",
		"urlscheme": "",
	} {
		if got, _ := payload[field].(string); got != want {
			t.Fatalf("%s 应为 %q，实际 %q", field, want, got)
		}
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("HTTP 状态必须固定 200（EPay 插件只读 body），实际 %d", recorder.Code)
	}
}

// TestEPayMAPIRejectsDuplicateParams：mapi 走 JSON 错误体，不是 HTML。
func TestEPayMAPIRejectsDuplicateParams(t *testing.T) {
	manager := &stubEPayManager{}
	handler := NewEPayV1Handler(manager)

	c, recorder := epayV1TestContext(t, http.MethodPost, "http://gw.test/mapi.php",
		"pid=M1&money=12.50&money=13.50")
	handler.MAPI(c)

	payload := decodeEPayJSON(t, recorder)
	if code := epayCodeOf(t, payload); code != -1 {
		t.Fatalf("失败应 code=-1，实际 %d", code)
	}
	if manager.authCalls != 0 {
		t.Fatalf("参数重复必须在入口拒绝，实际进入验签 %d 次", manager.authCalls)
	}
}

// TestEPayAPIRejectsUnimplementedActs：除 act=order 外一律「接口未开通」，且不消耗验签。
func TestEPayAPIRejectsUnimplementedActs(t *testing.T) {
	for _, act := range []string{"", "query", "refund", "settle", "orders"} {
		manager := &stubEPayManager{}
		handler := NewEPayV1Handler(manager)

		c, recorder := epayV1TestContext(t, http.MethodGet, "http://gw.test/api.php?act="+act+"&pid=M1", "")
		handler.API(c)

		payload := decodeEPayJSON(t, recorder)
		if code := epayCodeOf(t, payload); code != -1 {
			t.Fatalf("act=%q 应 code=-1，实际 %d", act, code)
		}
		if msg, _ := payload["msg"].(string); msg != epayMessageNotImplemented {
			t.Fatalf("act=%q 的 msg 应为「接口未开通」，实际 %q", act, msg)
		}
		if manager.authCalls != 0 || manager.queryCalls != 0 {
			t.Fatalf("未开通接口不得进入验签或查单：auth=%d query=%d", manager.authCalls, manager.queryCalls)
		}
	}
}

// TestEPayAPIOrderReturnsDocumentedFields 锁定 §5.3 的查单字段表与时间格式。
func TestEPayAPIOrderReturnsDocumentedFields(t *testing.T) {
	addTime := time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local)
	endTime := time.Date(2026, 11, 13, 12, 1, 3, 0, time.Local)
	manager := &stubEPayManager{queryResult: usecase.EPayQueryResult{
		TradeNo:     "20261113000001",
		OutTradeNo:  "M20261113001",
		Type:        "alipay",
		Name:        "测试商品",
		Money:       "12.50",
		TradeStatus: "TRADE_SUCCESS",
		AddTime:     addTime,
		EndTime:     endTime,
		Param:       "",
	}}
	handler := NewEPayV1Handler(manager)

	c, recorder := epayV1TestContext(t, http.MethodGet,
		"http://gw.test/api.php?act=order&pid=M7K3QPX9Z2AB4C&out_trade_no=M20261113001&sign=deadbeef", "")
	handler.API(c)

	payload := decodeEPayJSON(t, recorder)
	if code := epayCodeOf(t, payload); code != 1 {
		t.Fatalf("查单成功应 code=1，实际 %d（%v）", code, payload)
	}
	want := map[string]string{
		"pid":          "M7K3QPX9Z2AB4C",
		"trade_no":     "20261113000001",
		"out_trade_no": "M20261113001",
		"type":         "alipay",
		"name":         "测试商品",
		"money":        "12.50",
		"trade_status": "TRADE_SUCCESS",
		"addtime":      "2026-11-13 12:00:00",
		"endtime":      "2026-11-13 12:01:03",
		"param":        "",
	}
	for field, expected := range want {
		got, exists := payload[field]
		if !exists {
			t.Fatalf("查单响应缺少字段 %s（%v）", field, payload)
		}
		if text, _ := got.(string); text != expected {
			t.Fatalf("%s 应为 %q，实际 %#v", field, expected, got)
		}
	}
	if manager.queryMerchant != 7 || manager.queryOutTradeNo != "M20261113001" {
		t.Fatalf("查单必须带商户维度：merchant=%d out_trade_no=%q", manager.queryMerchant, manager.queryOutTradeNo)
	}
}

// TestEPayAPILimitsParamCount 覆盖设计 §5.5 #10 的参数上限。
func TestEPayAPILimitsParamCount(t *testing.T) {
	manager := &stubEPayManager{}
	handler := NewEPayV1Handler(manager)

	values := url.Values{"act": {"order"}}
	for index := 0; index < epayMaxParams+4; index++ {
		values.Set("p"+strconv.Itoa(index), "1")
	}
	c, recorder := epayV1TestContext(t, http.MethodGet, "http://gw.test/api.php?"+values.Encode(), "")
	handler.API(c)

	payload := decodeEPayJSON(t, recorder)
	if code := epayCodeOf(t, payload); code != -1 {
		t.Fatalf("参数过多应 code=-1，实际 %d", code)
	}
	if msg, _ := payload["msg"].(string); msg != "参数过多" {
		t.Fatalf("文案应为「参数过多」，实际 %q", msg)
	}
	if manager.authCalls != 0 {
		t.Fatalf("参数过多必须在入口拒绝，实际进入验签 %d 次", manager.authCalls)
	}
}

// TestEPayErrorMessageFoldsInternalErrors：非用例错误不得把内部细节回给商户。
func TestEPayErrorMessageFoldsInternalErrors(t *testing.T) {
	if got := epayErrorMessage(io.ErrUnexpectedEOF); got != "系统错误，请稍后重试" {
		t.Fatalf("内部错误应折叠为通用提示，实际 %q", got)
	}
	if got := epayErrorMessage(&usecase.Error{Code: usecase.CodeInvalidSignature, Message: "签名错误"}); got != "签名错误" {
		t.Fatalf("用例错误应原样透出文案，实际 %q", got)
	}
}
