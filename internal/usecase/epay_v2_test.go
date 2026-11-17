package usecase

// V2（RSA 双向签名）接线的门禁用例。
//
// 这组用例存在的理由：V2 在此之前的真实状态是「算法实现完整、但没有开通入口，
// 也没有入站验签」，即一条永远走不通的死路。只测「能创建商户」「能验 V1 签名」
// 无法发现这种问题——必须端到端地从「录入商户公钥」一路验到「用商户私钥签的请求
// 能通过」，才算接线完成。

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/epay"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
)

// sealEPayV2Key 把平台私钥加密成 V2 密钥行，并带上商户公钥。
// 与真实开户路径一致：私钥是我方生成的、商户公钥是对方给的。
func sealEPayV2Key(t *testing.T, box *secret.Box, merchantID int64, peerPublicPEM []byte, platformPrivatePEM []byte) merchant.Key {
	t.Helper()
	key := merchant.Key{
		MerchantID:    merchantID,
		Protocol:      merchant.ProtocolEPayV2,
		Generation:    merchant.GenerationCurrent,
		PeerPublicKey: string(peerPublicPEM),
	}
	cipherText, nonce, version, err := box.Seal(platformPrivatePEM, key.AAD())
	if err != nil {
		t.Fatalf("加密平台私钥失败: %v", err)
	}
	key.SecretCipher = cipherText
	key.SecretNonce = nonce
	key.KeyVersion = version
	return key
}

// TestMerchantServiceCreateV2RequiresValidPeerKey 覆盖开通凭据校验：
// 没有公钥、公钥是垃圾、公钥不是 RSA，都必须在写库之前被拒。
// 若放行，商户第一次下单才会暴露，而对外只能回「签名错误」，排障方向会完全跑偏。
func TestMerchantServiceCreateV2RequiresValidPeerKey(t *testing.T) {
	box := newMerchantServiceTestBox(t)

	cases := []struct {
		name string
		peer string
	}{
		{name: "空公钥", peer: ""},
		{name: "不是 PEM", peer: "not-a-pem"},
		{name: "PEM 里不是公钥", peer: "-----BEGIN PUBLIC KEY-----\nZm9v\n-----END PUBLIC KEY-----"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, repo, _ := newMerchantServiceUnderTest(t, box)
			_, err := service.Create(context.Background(), CreateMerchantInput{
				Name:          "V2 商户",
				Protocol:      merchant.ProtocolEPayV2,
				PeerPublicKey: tc.peer,
			})
			if err == nil {
				t.Fatal("公钥无效时必须拒绝创建")
			}
			if code, _ := ErrorCodeOf(err); code != CodeInvalidArgument {
				t.Fatalf("应为 CodeInvalidArgument，实际 code=%v err=%v", code, err)
			}
			// 关键：校验失败不能留下半条商户行。
			if len(repo.items) != 0 {
				t.Fatalf("公钥校验失败后不应写商户行，实际 %d 条", len(repo.items))
			}
		})
	}
}

// TestMerchantServiceCreateV2StoresPlatformKeyAndPeerKey 覆盖开通成功路径：
// 返回的平台公钥必须能解析成 RSA 公钥，商户公钥必须落库（验签要用），
// hint 必须是平台公钥指纹（管理端据此辨认）。
func TestMerchantServiceCreateV2StoresPlatformKeyAndPeerKey(t *testing.T) {
	box := newMerchantServiceTestBox(t)
	service, _, keys := newMerchantServiceUnderTest(t, box)

	// 模拟接入方提供的公钥。
	_, peerPublicPEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成商户密钥对失败: %v", err)
	}

	result, err := service.Create(context.Background(), CreateMerchantInput{
		Name:          "V2 商户",
		Protocol:      merchant.ProtocolEPayV2,
		PeerPublicKey: string(peerPublicPEM),
	})
	if err != nil {
		t.Fatalf("V2 开通应成功，实际 err=%v", err)
	}
	if result.PublicKeyPEM == "" {
		t.Fatal("V2 开通必须返回平台公钥")
	}
	if result.SecretPlain != "" {
		t.Fatal("V2 不应下发 V1 明文共享密钥")
	}

	// 平台公钥必须可用（商户端要拿它验我方签名）。
	normalized, err := merchant.NormalizeRSAPublicKeyPEM(result.PublicKeyPEM)
	if err != nil {
		t.Fatalf("返回的平台公钥不可用: %v", err)
	}
	if !strings.Contains(normalized, "BEGIN PUBLIC KEY") {
		t.Fatalf("平台公钥应为 PKIX PEM，实际 %q", normalized)
	}

	stored, ok := keys.rows[merchantServiceTestKeyID(result.Merchant.ID, merchant.ProtocolEPayV2, merchant.GenerationCurrent)]
	if !ok {
		t.Fatal("V2 当前代密钥行未落库")
	}
	if stored.PeerPublicKey == "" {
		t.Fatal("商户公钥必须落库，否则入站请求无法验签")
	}
	if stored.Hint != merchant.HintPublicKey([]byte(result.PublicKeyPEM)) {
		t.Fatalf("hint 应为平台公钥指纹，实际 %q", stored.Hint)
	}
	if len(stored.SecretCipher) == 0 || len(stored.SecretNonce) == 0 {
		t.Fatal("平台私钥必须加密落库")
	}
}

// TestEPayAuthenticateV2 是最关键的一条：用商户私钥签的请求必须验通，且回传协议 V2。
// 回传协议决定 orders.protocol，进而决定回调用哪把密钥签名——错了商户收到的是
// 验不过的回调，而且现象是「回调签名错误」，指向完全错误的方向。
func TestEPayAuthenticateV2(t *testing.T) {
	service, env := epayV1TestService(t, nil)

	merchantPrivatePEM, merchantPublicPEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成商户密钥对失败: %v", err)
	}
	_, platformPublicPEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成平台密钥对失败: %v", err)
	}
	// 平台私钥随便给一把（这条用例只验入站，不验出站签名）。
	_, platformPrivatePEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成平台私钥失败: %v", err)
	}
	_ = platformPublicPEM

	env.keys.active[epayV1KeySlot(testMerchantID, merchant.ProtocolEPayV2)] = sealEPayV2Key(
		t, env.box, testMerchantID, merchantPublicPEM, platformPrivatePEM)

	params := map[string]string{
		"pid":          testMerchantNo,
		"type":         "alipay",
		"out_trade_no": "M20261117001",
		"notify_url":   "https://m.example.com/notify",
		"return_url":   "https://m.example.com/return",
		"name":         "V2 测试商品",
		"money":        "12.50",
		// 10 位秒级：与真实 EPay V2 报文一致。
		"timestamp": strconv.FormatInt(epayTestNow.Unix(), 10),
	}
	sign, err := epay.SignV2(params, merchantPrivatePEM)
	if err != nil {
		t.Fatalf("用商户私钥签名失败: %v", err)
	}
	params["sign"] = sign
	params["sign_type"] = "RSA"

	identity, protocol, err := service.Authenticate(context.Background(), params)
	if err != nil {
		t.Fatalf("V2 验签应通过，实际 err=%v", err)
	}
	if identity.ID != testMerchantID {
		t.Fatalf("应解析出商户 %d，实际 %d", testMerchantID, identity.ID)
	}
	if protocol != merchant.ProtocolEPayV2 {
		t.Fatalf("必须回传 EPayV2 协议（决定订单落库与回调签名），实际 %v", protocol)
	}

	// 篡改金额必须失败——证明验的是真签名，不是「有 sign 就放行」。
	tampered := make(map[string]string, len(params))
	for name, value := range params {
		tampered[name] = value
	}
	tampered["money"] = "13.50"
	if _, _, err := service.Authenticate(context.Background(), tampered); err == nil {
		t.Fatal("篡改金额后必须验签失败")
	}
}

// TestEPayAuthenticateV2RejectsStaleTimestamp 覆盖重放防护：
// 过期的 timestamp 即使签名本身正确也必须拒绝，否则抓到一个报文就能无限重放。
func TestEPayAuthenticateV2RejectsStaleTimestamp(t *testing.T) {
	service, env := epayV1TestService(t, nil)

	merchantPrivatePEM, merchantPublicPEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成商户密钥对失败: %v", err)
	}
	_, platformPrivatePEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成平台密钥失败: %v", err)
	}
	env.keys.active[epayV1KeySlot(testMerchantID, merchant.ProtocolEPayV2)] = sealEPayV2Key(
		t, env.box, testMerchantID, merchantPublicPEM, platformPrivatePEM)

	// 超出 ±300s 窗口。
	stale := epayTestNow.Add(-10 * time.Minute).Unix()
	params := map[string]string{
		"pid":          testMerchantNo,
		"type":         "alipay",
		"out_trade_no": "M20261117002",
		"notify_url":   "https://m.example.com/notify",
		"name":         "过期报文",
		"money":        "12.50",
		"timestamp":    strconv.FormatInt(stale, 10),
	}
	sign, err := epay.SignV2(params, merchantPrivatePEM)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	params["sign"] = sign

	if _, _, err := service.Authenticate(context.Background(), params); err == nil {
		t.Fatal("过期时间戳必须拒绝")
	} else if code, _ := ErrorCodeOf(err); code != CodeInvalidSignature {
		t.Fatalf("对外应为 CodeInvalidSignature，实际 code=%v err=%v", code, err)
	}
}

// TestEPaySignResponseV2Verifiable 覆盖出站响应签名：
// SignEPayResponse 出的 timestamp + sign 必须能用平台公钥验过。
// 缺这一步，V2 商户会把所有响应判为「不可信」，表现为「下单成功但商户侧报错」。
func TestEPaySignResponseV2Verifiable(t *testing.T) {
	service, env := epayV1TestService(t, nil)

	platformPrivatePEM, platformPublicPEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成平台密钥对失败: %v", err)
	}
	// 商户公钥这条用例用不到，给一把合法的即可。
	_, merchantPublicPEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成商户密钥对失败: %v", err)
	}
	env.keys.active[epayV1KeySlot(testMerchantID, merchant.ProtocolEPayV2)] = sealEPayV2Key(
		t, env.box, testMerchantID, merchantPublicPEM, platformPrivatePEM)

	fields := map[string]string{
		"code":     "1",
		"trade_no": "20261117000001",
		"payurl":   "https://pay.example.com/#/payment/token",
		"qrcode":   "wxp://example",
	}
	timestamp, sign, err := service.SignEPayResponse(context.Background(), testMerchantID, fields)
	if err != nil {
		t.Fatalf("响应签名失败: %v", err)
	}
	if timestamp == "" || sign == "" {
		t.Fatal("V2 响应必须带 timestamp 与 sign")
	}

	// 商户侧会这样验：把收到的字段 + timestamp 放回 map，用平台公钥验 sign。
	received := make(map[string]string, len(fields)+1)
	for name, value := range fields {
		received[name] = value
	}
	received["timestamp"] = timestamp
	if err := epay.VerifyV2(received, platformPublicPEM, sign); err != nil {
		t.Fatalf("平台签名必须能用平台公钥验过，实际 %v", err)
	}

	// 字段被改动后必须验不过。
	received["payurl"] = "https://evil.example.com"
	if err := epay.VerifyV2(received, platformPublicPEM, sign); err == nil {
		t.Fatal("篡改响应字段后必须验签失败")
	}
}

// TestEPayCreateV2PersistsProtocol 覆盖 V2 下单：
// 这笔单必须落 protocol=2，否则回调会按 V1 用共享密钥签名，
// 商户拿平台公钥验不过——现象是「下单成功、回调永远验签失败」。
//
// 这条用例的直接来源是真机端到端测试：validateEPayCreateInput 里曾有一条
// 「协议必须等于 V1」的旧守卫，单测没覆盖，结果 V2 商户下单被拒「该商户未开通 V1 接口」。
func TestEPayCreateV2PersistsProtocol(t *testing.T) {
	service, env := epayV1TestService(t, nil)
	env.qrcodes.exact[1] = "https://qr.test/0.01"

	input := epayV1Input("V2-ORDER-1", "0.01")
	input.Protocol = merchant.ProtocolEPayV2

	if _, err := service.Create(context.Background(), input); err != nil {
		t.Fatalf("V2 下单应成功，实际 err=%v", err)
	}
	if len(env.orders.created) != 1 {
		t.Fatalf("应写入 1 笔订单，实际 %d", len(env.orders.created))
	}
	if got := env.orders.created[0].Protocol; got != merchant.ProtocolEPayV2 {
		t.Fatalf("订单协议必须是 V2（决定回调签名密钥），实际 %v", got)
	}
}

// TestMerchantServiceUpdateRejectsBadPeerKeyWithoutPartialWrite 锁定「整体失败」语义：
// 公钥非法时，商户名/启停/有效期都不能落库。否则运营看到报错以为没改，
// 实际已经改了一半，下次进页面才发现名字变了。
func TestMerchantServiceUpdateRejectsBadPeerKeyWithoutPartialWrite(t *testing.T) {
	box := newMerchantServiceTestBox(t)
	service, repo, _ := newMerchantServiceUnderTest(t, box)

	created, err := service.Create(context.Background(), CreateMerchantInput{Name: "原名", CloseMinutes: 10})
	if err != nil {
		t.Fatalf("创建商户失败: %v", err)
	}

	bad := "-----BEGIN PUBLIC KEY-----\nZm9v\n-----END PUBLIC KEY-----"
	_, err = service.Update(context.Background(), created.Merchant.ID, UpdateMerchantInput{
		Name:          "新名",
		Enabled:       false,
		CloseMinutes:  99,
		PeerPublicKey: &bad,
	})
	if err == nil {
		t.Fatal("非法公钥必须拒绝更新")
	}
	if code, _ := ErrorCodeOf(err); code != CodeInvalidArgument {
		t.Fatalf("应为 CodeInvalidArgument，实际 code=%v err=%v", code, err)
	}

	stored := repo.items[created.Merchant.ID]
	if stored.Name != "原名" || !stored.Enabled || stored.CloseMinutes != 10 {
		t.Fatalf("公钥校验失败时不得落库任何字段，实际 name=%q enabled=%v close=%d",
			stored.Name, stored.Enabled, stored.CloseMinutes)
	}
}

// TestNormalizeRSAPublicKeyPEMBounds 锁定公钥尺寸边界：2048/4096 通过，8192 拒绝。
// 上界不是洁癖：peer_public_key 是 varchar(1024)，8192 位 PEM 约 1516 字节，
// 写库要么 500、要么被非严格模式静默截断，后者会让该商户 V2 请求永远「签名错误」。
func TestNormalizeRSAPublicKeyPEMBounds(t *testing.T) {
	for _, bits := range []int{2048, 4096} {
		_, pub, err := merchant.NewRSAKeyPairWithBits(bits)
		if err != nil {
			t.Fatalf("生成 %d 位密钥失败: %v", bits, err)
		}
		if _, err := merchant.NormalizeRSAPublicKeyPEM(string(pub)); err != nil {
			t.Fatalf("%d 位公钥应通过校验，实际 %v", bits, err)
		}
	}

	_, big, err := merchant.NewRSAKeyPairWithBits(8192)
	if err != nil {
		t.Fatalf("生成 8192 位密钥失败: %v", err)
	}
	_, err = merchant.NormalizeRSAPublicKeyPEM(string(big))
	if err == nil {
		t.Fatal("8192 位公钥必须被拒绝（超出列宽与验签成本上限）")
	}
	// 断言走的是「位数」分支而不是「字节长度」分支：
	// 两道上限在 8192 位这个输入上是重叠的（PEM≈1516B > 1024B），只断言「报错」
	// 会让位数判断永远无法被单独证伪。错误消息里带位数，才能钉住这个分支。
	if !strings.Contains(err.Error(), "4096") {
		t.Fatalf("应命中位数上限并给出可读错误，实际 %v", err)
	}
}
