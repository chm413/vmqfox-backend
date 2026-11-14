package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/usecase"
)

// 装配层传进来的 *usecase.MerchantService 必须满足 handler 的接口，在编译期钉住以免装配时才报错。
var _ MerchantManager = (*usecase.MerchantService)(nil)

// merchantHandlerTestService 记录入参并回放固定结果，用于断言 transport 层的白名单字段。
type merchantHandlerTestService struct {
	createInput    usecase.CreateMerchantInput
	createResult   usecase.CreateMerchantResult
	createErr      error
	listResult     []usecase.MerchantView
	listErr        error
	updateID       int64
	updateInput    usecase.UpdateMerchantInput
	updateResult   usecase.MerchantView
	updateErr      error
	rotateID       int64
	rotateProtocol merchant.Protocol
	rotateResult   usecase.RotateResult
	rotateErr      error
	publicKeyID    int64
	publicKey      string
	publicKeyErr   error
}

func (s *merchantHandlerTestService) Create(_ context.Context, input usecase.CreateMerchantInput) (usecase.CreateMerchantResult, error) {
	s.createInput = input
	return s.createResult, s.createErr
}

func (s *merchantHandlerTestService) List(context.Context) ([]usecase.MerchantView, error) {
	return s.listResult, s.listErr
}

func (s *merchantHandlerTestService) Update(_ context.Context, id int64, input usecase.UpdateMerchantInput) (usecase.MerchantView, error) {
	s.updateID, s.updateInput = id, input
	return s.updateResult, s.updateErr
}

func (s *merchantHandlerTestService) RotateKey(_ context.Context, id int64, protocol merchant.Protocol) (usecase.RotateResult, error) {
	s.rotateID, s.rotateProtocol = id, protocol
	return s.rotateResult, s.rotateErr
}

func (s *merchantHandlerTestService) PlatformPublicKey(_ context.Context, id int64) (string, error) {
	s.publicKeyID = id
	return s.publicKey, s.publicKeyErr
}

func merchantTestEngine(service MerchantManager) *gin.Engine {
	gin.SetMode(gin.TestMode)
	handlers := newMerchantHandlers(service)
	engine := gin.New()
	engine.GET("/api/merchant/list", handlers.ListAPI)
	engine.POST("/api/merchant/create", handlers.CreateAPI)
	engine.POST("/api/merchant/update/:id", handlers.UpdateAPI)
	engine.POST("/api/merchant/key/rotate/:id", handlers.RotateKeyAPI)
	engine.GET("/api/merchant/key/public/:id", handlers.PublicKeyAPI)
	return engine
}

type merchantTestEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func doMerchantRequest(t *testing.T, engine *gin.Engine, method, path, body string) (merchantTestEnvelope, map[string]any) {
	t.Helper()
	envelope, data, status := doMerchantRequestWithStatus(t, engine, method, path, body)
	if status != http.StatusOK {
		t.Fatalf("%s %s 期望 HTTP 200，实际 %d", method, path, status)
	}
	return envelope, data
}

// doMerchantRequestWithStatus 允许断言「服务不可用」这类非 200 的传输层状态。
func doMerchantRequestWithStatus(t *testing.T, engine *gin.Engine, method, path, body string) (merchantTestEnvelope, map[string]any, int) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	var envelope merchantTestEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析响应失败: %v（原文 %s）", err, recorder.Body.String())
	}
	data := map[string]any{}
	if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, &data); err != nil {
			t.Fatalf("解析 data 失败: %v（原文 %s）", err, string(envelope.Data))
		}
	}
	return envelope, data, recorder.Code
}

// TestCreateMerchantHandlerReturnsSecretOnce 断言明文只出现在创建响应里，且参数被正确透传。
func TestCreateMerchantHandlerReturnsSecretOnce(t *testing.T) {
	const secret = "abcdefghjkmnpqrstuvwxyz23456789"
	service := &merchantHandlerTestService{
		createResult: usecase.CreateMerchantResult{
			Merchant: merchant.Merchant{
				ID:           7,
				No:           "M7K3QPX9Z2AB4C",
				Name:         "自营站",
				Enabled:      true,
				CloseMinutes: 30,
				Note:         "联系人",
				CreatedAt:    time.Unix(1_700_000_000, 0).UTC(),
				UpdatedAt:    time.Unix(1_700_000_000, 0).UTC(),
			},
			SecretPlain: secret,
			SecretHint:  "6789",
		},
	}
	engine := merchantTestEngine(service)

	envelope, data := doMerchantRequest(t, engine, http.MethodPost, "/api/merchant/create",
		`{"name":"自营站","closeMinutes":30,"note":"联系人"}`)
	if envelope.Code != 200 {
		t.Fatalf("创建应成功，实际 code=%d msg=%q", envelope.Code, envelope.Msg)
	}
	if service.createInput.Name != "自营站" || service.createInput.CloseMinutes != 30 || service.createInput.Note != "联系人" {
		t.Fatalf("入参透传不正确: %+v", service.createInput)
	}
	if data["secret"] != secret {
		t.Fatalf("创建响应必须包含明文 secret，实际 %v", data["secret"])
	}
	if data["hint"] != "6789" {
		t.Fatalf("创建响应 hint 不正确: %v", data["hint"])
	}
	if data["merchantNo"] != "M7K3QPX9Z2AB4C" {
		t.Fatalf("创建响应应回显对外商户号: %v", data["merchantNo"])
	}
	if _, exists := data["publicKey"]; exists {
		t.Fatal("V2 默认不开通，创建响应不应出现 publicKey")
	}
	for _, forbidden := range []string{"secret_cipher", "secretCipher", "secret_nonce", "nonce", "keyVersion"} {
		if _, exists := data[forbidden]; exists {
			t.Fatalf("创建响应不应包含 %q: %v", forbidden, data)
		}
	}
}

// TestListMerchantHandlerHidesSecrets 断言列表响应不含明文与密文（JSON 层白名单）。
func TestListMerchantHandlerHidesSecrets(t *testing.T) {
	service := &merchantHandlerTestService{
		listResult: []usecase.MerchantView{{
			ID:           7,
			MerchantNo:   "M7K3QPX9Z2AB4C",
			Name:         "自营站",
			Enabled:      true,
			CloseMinutes: 30,
			Note:         "联系人",
			V1Enabled:    true,
			V2Enabled:    false,
			V1Hint:       "6789",
			CreatedAt:    time.Unix(1_700_000_000, 0).UTC(),
			UpdatedAt:    time.Unix(1_700_000_000, 0).UTC(),
		}},
	}
	engine := merchantTestEngine(service)

	envelope, data := doMerchantRequest(t, engine, http.MethodGet, "/api/merchant/list", "")
	if envelope.Code != 200 {
		t.Fatalf("列表应成功，实际 code=%d msg=%q", envelope.Code, envelope.Msg)
	}
	items, ok := data["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("列表应返回 1 个商户，实际 %v", data["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("商户项格式不正确: %v", items[0])
	}
	for _, forbidden := range []string{"secret", "secretPlain", "secret_cipher", "secretCipher", "secret_nonce", "nonce", "keyVersion"} {
		if _, exists := item[forbidden]; exists {
			t.Fatalf("列表响应不应包含 %q: %v", forbidden, item)
		}
	}
	// hint 只允许是末 4 位指纹，不能是完整密钥。
	if item["v1Hint"] != "6789" {
		t.Fatalf("列表 hint 不正确: %v", item["v1Hint"])
	}
	if item["v1Enabled"] != true || item["v2Enabled"] != false {
		t.Fatalf("协议开通状态不正确: %v", item)
	}
}

// TestRotateMerchantKeyHandler 断言 V1 回明文、V2 回公钥，且 protocol 被正确透传。
func TestRotateMerchantKeyHandler(t *testing.T) {
	service := &merchantHandlerTestService{
		rotateResult: usecase.RotateResult{
			Protocol: merchant.ProtocolEPayV1,
			Secret:   "abcdefghjkmnpqrstuvwxyz23456789",
			Hint:     "6789",
		},
	}
	engine := merchantTestEngine(service)

	envelope, data := doMerchantRequest(t, engine, http.MethodPost, "/api/merchant/key/rotate/7", `{"protocol":1}`)
	if envelope.Code != 200 {
		t.Fatalf("轮换应成功，实际 code=%d msg=%q", envelope.Code, envelope.Msg)
	}
	if service.rotateID != 7 || service.rotateProtocol != merchant.ProtocolEPayV1 {
		t.Fatalf("轮换入参透传不正确: id=%d protocol=%v", service.rotateID, service.rotateProtocol)
	}
	if data["secret"] != "abcdefghjkmnpqrstuvwxyz23456789" || data["hint"] != "6789" {
		t.Fatalf("轮换响应应含新明文与 hint: %v", data)
	}
	if _, exists := data["publicKey"]; exists {
		t.Fatal("V1 轮换不应返回平台公钥")
	}

	// V2 轮换：只回平台公钥，不应回共享密钥明文。
	service.rotateResult = usecase.RotateResult{
		Protocol:  merchant.ProtocolEPayV2,
		PublicKey: "-----BEGIN PUBLIC KEY-----\nAAA\n-----END PUBLIC KEY-----\n",
		Hint:      "0123456789abcdef",
	}
	envelope, data = doMerchantRequest(t, engine, http.MethodPost, "/api/merchant/key/rotate/7", `{"protocol":2}`)
	if envelope.Code != 200 {
		t.Fatalf("V2 轮换应成功，实际 code=%d msg=%q", envelope.Code, envelope.Msg)
	}
	if data["publicKey"] == nil || data["publicKey"] == "" {
		t.Fatalf("V2 轮换应返回平台公钥: %v", data)
	}
	if _, exists := data["secret"]; exists {
		t.Fatal("V2 轮换不应返回共享密钥明文")
	}
}

// TestUpdateMerchantHandlerRequiresExplicitEnabled 防止漏传 enabled 把商户静默停用。
func TestUpdateMerchantHandlerRequiresExplicitEnabled(t *testing.T) {
	service := &merchantHandlerTestService{}
	engine := merchantTestEngine(service)

	envelope, _ := doMerchantRequest(t, engine, http.MethodPost, "/api/merchant/update/7", `{"name":"自营站","closeMinutes":30}`)
	if envelope.Code != 400 {
		t.Fatalf("缺少 enabled 时应返回 400，实际 code=%d", envelope.Code)
	}
	if service.updateID != 0 {
		t.Fatal("参数无效时不应调用用例")
	}

	service.updateResult = usecase.MerchantView{ID: 7, Name: "自营站-停用", Enabled: false, CloseMinutes: 0, V1Enabled: true, V1Hint: "6789"}
	envelope, data := doMerchantRequest(t, engine, http.MethodPost, "/api/merchant/update/7",
		`{"name":"自营站-停用","enabled":false,"closeMinutes":0,"note":""}`)
	if envelope.Code != 200 {
		t.Fatalf("更新应成功，实际 code=%d msg=%q", envelope.Code, envelope.Msg)
	}
	if service.updateInput.Enabled {
		t.Fatal("enabled=false 应被解析为停用")
	}
	if data["enabled"] != false {
		t.Fatalf("更新响应应回显停用状态: %v", data)
	}
	if _, exists := data["secret"]; exists {
		t.Fatal("更新响应不应包含明文")
	}
}

// TestMerchantHandlerRejectsInvalidParams 覆盖 ID、协议、有效期三类参数校验。
func TestMerchantHandlerRejectsInvalidParams(t *testing.T) {
	engine := merchantTestEngine(&merchantHandlerTestService{})

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"非法商户ID", http.MethodPost, "/api/merchant/update/abc", `{"name":"自营站","enabled":true,"closeMinutes":30}`},
		{"零商户ID", http.MethodPost, "/api/merchant/key/rotate/0", `{"protocol":1}`},
		{"非法协议", http.MethodPost, "/api/merchant/key/rotate/7", `{"protocol":0}`},
		{"协议越界", http.MethodPost, "/api/merchant/key/rotate/7", `{"protocol":3}`},
		{"有效期非数字", http.MethodPost, "/api/merchant/update/7", `{"name":"自营站","enabled":true,"closeMinutes":"abc"}`},
		{"公钥接口非法ID", http.MethodGet, "/api/merchant/key/public/0", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			envelope, _ := doMerchantRequest(t, engine, testCase.method, testCase.path, testCase.body)
			if envelope.Code != 400 {
				t.Fatalf("应返回 400，实际 code=%d msg=%q", envelope.Code, envelope.Msg)
			}
		})
	}
}

// TestMerchantHandlerMapsApplicationErrors 保证主密钥缺失这类部署问题不会被报成 400。
func TestMerchantHandlerMapsApplicationErrors(t *testing.T) {
	service := &merchantHandlerTestService{
		createErr: &usecase.Error{Code: usecase.CodeConfiguration, Message: "未配置 VMQ_MASTER_KEY"},
	}
	envelope, _ := doMerchantRequest(t, merchantTestEngine(service), http.MethodPost, "/api/merchant/create", `{"name":"自营站"}`)
	if envelope.Code != 503 {
		t.Fatalf("主密钥缺失应映射为 503，实际 code=%d", envelope.Code)
	}

	service.publicKeyErr = &usecase.Error{Code: usecase.CodeNotFound, Message: "该商户未开通 V2 接口"}
	envelope, _ = doMerchantRequest(t, merchantTestEngine(service), http.MethodGet, "/api/merchant/key/public/7", "")
	if envelope.Code != 404 {
		t.Fatalf("未开通 V2 应映射为 404，实际 code=%d", envelope.Code)
	}

	service.publicKeyErr = &usecase.Error{Code: usecase.CodeConflict, Message: "轮换冲突，请重试"}
	envelope, _ = doMerchantRequest(t, merchantTestEngine(service), http.MethodGet, "/api/merchant/key/public/7", "")
	if envelope.Code != 409 {
		t.Fatalf("冲突应映射为 409，实际 code=%d", envelope.Code)
	}
}

// TestNewMerchantHandlers 防止改造 DTO 时漏装某个路由处理器。
func TestNewMerchantHandlers(t *testing.T) {
	handlers := newMerchantHandlers(nil)
	for name, routeHandler := range map[string]gin.HandlerFunc{
		"list":      handlers.ListAPI,
		"create":    handlers.CreateAPI,
		"update":    handlers.UpdateAPI,
		"rotate":    handlers.RotateKeyAPI,
		"publicKey": handlers.PublicKeyAPI,
	} {
		if routeHandler == nil {
			t.Errorf("商户处理器 %s 未装配", name)
		}
	}

	// 服务为 nil 时必须回 503，而不是 panic 或伪造成功。
	envelope, _, status := doMerchantRequestWithStatus(t, merchantTestEngine(nil), http.MethodGet, "/api/merchant/list", "")
	if status != http.StatusServiceUnavailable || envelope.Code != 503 {
		t.Fatalf("服务不可用时应返回 503，实际 HTTP %d code=%d", status, envelope.Code)
	}
}
