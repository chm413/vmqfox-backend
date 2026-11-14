package epay

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"
)

// testV2Keys 现场生一对 2048 位 RSA 密钥，转成 PEM。
// 不把密钥硬编码进仓库：测试要能证明「换一对密钥就验不过」，
// 硬编码会让人误以为这对密钥有特殊含义。
func testV2Keys(t *testing.T) (privatePEM, publicPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 密钥失败: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("导出 PKCS#8 私钥失败: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("导出 PKIX 公钥失败: %v", err)
	}
	privatePEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	publicPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	return privatePEM, publicPEM
}

func TestV2_RSARoundTrip(t *testing.T) {
	privatePEM, publicPEM := testV2Keys(t)

	// 附录 C：V2 的请求、响应、回调都带 timestamp 且参与签名。
	now := time.Unix(1697452800, 0)
	params := goldenParams()
	AppendTimestamp(params, now)

	sign, err := SignV2(params, privatePEM)
	if err != nil {
		t.Fatalf("SignV2 返回错误: %v", err)
	}
	if sign == "" {
		t.Fatal("SignV2 返回空签名")
	}

	if err := VerifyV2(params, publicPEM, sign); err != nil {
		t.Fatalf("自签自验应通过，实际 %v", err)
	}

	// 改一个参数后必须失败。
	mutations := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"改金额", func(copied map[string]string) { copied["money"] = "12.51" }},
		{"改订单号", func(copied map[string]string) { copied["out_trade_no"] = "M20261113002" }},
		{"改 timestamp", func(copied map[string]string) { copied["timestamp"] = "1697452801" }},
		{"删 timestamp", func(copied map[string]string) { delete(copied, "timestamp") }},
		{"加一个参数", func(copied map[string]string) { copied["param"] = "1" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			copied := copyParams(params)
			mutation.mutate(copied)
			if err := VerifyV2(copied, publicPEM, sign); !errors.Is(err, ErrSignature) {
				t.Fatalf("篡改后应验签失败，实际 %v", err)
			}
		})
	}

	// sign / sign_type / 空值不进签名域：塞进去不影响验签。
	t.Run("sign 与空值不参与签名", func(t *testing.T) {
		copied := copyParams(params)
		copied["sign"] = sign
		copied["sign_type"] = "RSA"
		copied["param"] = ""
		if err := VerifyV2(copied, publicPEM, sign); err != nil {
			t.Fatalf("sign/sign_type/空值应被剔除，实际 %v", err)
		}
	})

	// 换一对密钥必须验不过（证明验签真的用了公钥）。
	t.Run("换密钥对", func(t *testing.T) {
		_, otherPublicPEM := testV2Keys(t)
		if err := VerifyV2(params, otherPublicPEM, sign); !errors.Is(err, ErrSignature) {
			t.Fatalf("换公钥后应验签失败，实际 %v", err)
		}
	})

	// 签名被截断 / 不是 Base64 一律 ErrSignature。
	t.Run("签名损坏", func(t *testing.T) {
		if err := VerifyV2(params, publicPEM, sign[:len(sign)-4]); !errors.Is(err, ErrSignature) {
			t.Fatalf("截断签名应返回 ErrSignature，实际 %v", err)
		}
		if err := VerifyV2(params, publicPEM, "not base64!!!"); !errors.Is(err, ErrSignature) {
			t.Fatalf("非法 Base64 应返回 ErrSignature，实际 %v", err)
		}
		if err := VerifyV2(params, publicPEM, ""); !errors.Is(err, ErrSignature) {
			t.Fatalf("空签名应返回 ErrSignature，实际 %v", err)
		}
	})

	// 配置类错误（PEM 坏了）必须与「报文被篡改」区分：返回具体错误而不是 ErrSignature。
	t.Run("坏公钥", func(t *testing.T) {
		err := VerifyV2(params, []byte("not a pem"), sign)
		if err == nil {
			t.Fatal("坏公钥应返回错误")
		}
		if errors.Is(err, ErrSignature) {
			t.Fatalf("坏公钥不应归为 ErrSignature，实际 %v", err)
		}
	})
	t.Run("坏私钥", func(t *testing.T) {
		if _, err := SignV2(params, []byte("not a pem")); err == nil {
			t.Fatal("坏私钥应返回错误")
		}
	})
}

func TestV2_Base64LooseDecode(t *testing.T) {
	// 直接钉住解码器的容错行为，不依赖随机签名里恰好出现 '+'。
	// []byte{0xfb,0xef,0xbe} 的标准 Base64 正好是 "++++"。
	want := []byte{0xfb, 0xef, 0xbe}
	cases := []struct {
		name string
		text string
	}{
		{"标准 Base64", "++++"},
		{"'+' 被 query 解码成空格", "    "},
		{"无 padding 的标准 Base64", base64.RawStdEncoding.EncodeToString([]byte{0xfb})},
		{"URL-safe 变体", base64.URLEncoding.EncodeToString([]byte{0xfb, 0xef, 0xbe})},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decoded, ok := decodeBase64Loose(testCase.text)
			if !ok {
				t.Fatalf("decodeBase64Loose(%q) 解码失败", testCase.text)
			}
			// "++++" 与 "    " 必须解出同一份字节。
			if testCase.name == "标准 Base64" || testCase.name == "'+' 被 query 解码成空格" {
				if string(decoded) != string(want) {
					t.Fatalf("decodeBase64Loose(%q) 解出的字节不符", testCase.text)
				}
			}
		})
	}
	if _, ok := decodeBase64Loose("not base64!!!"); ok {
		t.Fatal("非法 Base64 不应解码成功")
	}
	if _, ok := decodeBase64Loose(""); ok {
		t.Fatal("空串不应解码成功")
	}

	// 端到端：真实签名经过同样的两处传输损耗后仍要能验签。
	privatePEM, publicPEM := testV2Keys(t)
	params := goldenParams()
	AppendTimestamp(params, time.Unix(1697452800, 0))

	sign, err := SignV2(params, privatePEM)
	if err != nil {
		t.Fatalf("SignV2 返回错误: %v", err)
	}
	if !strings.Contains(sign, "+") && !strings.Contains(sign, "/") {
		t.Skip("随机签名恰好不含 '+'/'/'，端到端容错用例跳过")
	}
	// 两种传输损耗各自单独发生，不要混用字母表：只把 '+' 换成空格（query 解码）、
	// 或整体切成 URL-safe（'+'→'-'、'/'→'_'）才是现实中会出现的形态。
	if err := VerifyV2(params, publicPEM, strings.ReplaceAll(sign, "+", " ")); err != nil {
		t.Fatalf("'+' 被 query 解码成空格后仍应能验签，实际 %v", err)
	}
	urlSafe := strings.NewReplacer("+", "-", "/", "_").Replace(sign)
	if err := VerifyV2(params, publicPEM, urlSafe); err != nil {
		t.Fatalf("URL-safe Base64 也应能验签，实际 %v", err)
	}
	if err := VerifyV2(params, publicPEM, strings.TrimRight(sign, "=")); err != nil {
		t.Fatalf("无 padding 的 Base64 也应能验签，实际 %v", err)
	}
}

func TestAppendTimestamp(t *testing.T) {
	params := map[string]string{"pid": goldenMerchantNo}
	now := time.Unix(1697452800, 0)
	AppendTimestamp(params, now)
	if params["timestamp"] != "1697452800" {
		t.Fatalf("timestamp 应为 10 位秒级十进制，实际 %q", params["timestamp"])
	}
	if !TimestampValid(params["timestamp"], now, 300*time.Second) {
		t.Fatal("AppendTimestamp 写入的值必须能通过 TimestampValid")
	}
	// 亚秒部分必须被丢弃，不能出现 "1697452800.5" 这类值。
	AppendTimestamp(params, now.Add(999*time.Millisecond))
	if params["timestamp"] != "1697452800" {
		t.Fatalf("亚秒部分未被丢弃，实际 %q", params["timestamp"])
	}
}

func TestTimestampValid(t *testing.T) {
	// 基准：2023-10-16T16:00:00Z，随便挑的整秒。
	const nowText = "1697452800"
	now := time.Unix(1697452800, 0)
	skew := 300 * time.Second

	cases := []struct {
		name      string
		timestamp string
		want      bool
	}{
		{"当前时间", nowText, true},
		{"now-299s", "1697452501", true},
		{"now+299s", "1697453099", true},
		{"now-300s 边界放行", "1697452500", true},
		{"now+300s 边界放行", "1697453100", true},
		{"now-301s 超出窗口", "1697452499", false},
		{"now+301s 超出窗口", "1697453101", false},
		{"10 位历史值", "1697452800", true},
		{"9 位拒绝", "169745280", false},
		{"11 位拒绝", "16974528000", false},
		{"13 位毫秒拒绝", "1697452800000", false},
		{"ISO8601 拒绝", "2023-10-16T16:00:00Z", false},
		{"非数字拒绝", "abcdefghij", false},
		{"含符号拒绝", "-697452800", false},
		{"空串拒绝", "", false},
		{"全角数字拒绝", "１697452800", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := TimestampValid(testCase.timestamp, now, skew); got != testCase.want {
				t.Fatalf("TimestampValid(%q) = %v，期望 %v", testCase.timestamp, got, testCase.want)
			}
		})
	}

	// 负窗口必须「全拒」，而不是因为比较逻辑写反而变成「全放行」。
	if TimestampValid(nowText, now, -time.Second) {
		t.Fatal("负 skew 应全部拒绝")
	}
	// 零窗口只接受同一秒。
	if !TimestampValid(nowText, now, 0) {
		t.Fatal("skew=0 时同一秒应通过")
	}
	if TimestampValid("1697452801", now, 0) {
		t.Fatal("skew=0 时下一秒应拒绝")
	}
}

func copyParams(params map[string]string) map[string]string {
	copied := make(map[string]string, len(params))
	for name, value := range params {
		copied[name] = value
	}
	return copied
}
