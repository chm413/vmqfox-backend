package epay

import (
	"errors"
	"strings"
	"testing"
)

// 附录 A 的黄金用例。money 保留 "12.50" 而不是 "12.5" —— V1 用原始字符串签名。
//
// 口径（经 team-lead 2026-11-14 统一，与文档附录 A 同步）：
//   - V1 密钥 32 位（§4.2 要求 32 字符，原附录 A 的 31 位是笔误）；
//   - merchant_no / pid 14 位（`M` + 13 位，生成器同规则）。
const goldenKey = "abcdefghjkmnpqrstuvwxyz23456789k"

// goldenMerchantNo 是 14 位对外商户号，出现在 pid 字段。
const goldenMerchantNo = "M7K3QPX9Z2AB4C"

// goldenCanonical 是附录 A 给出的待签串（不含 key），逐字符硬编码。
// 它是对外的字节级契约：即使实现改了排序或编码，这条断言也必须打死，
// 而不是反过来「按实现的输出更新期望值」。
const goldenCanonical = "money=12.50&name=测试商品&notify_url=https://m.example.com/notify&out_trade_no=M20261113001&pid=M7K3QPX9Z2AB4C&return_url=https://m.example.com/return&type=alipay"

// goldenSign 用命令行独立算出，不来自本实现：
//
//	printf '%s' '<goldenCanonical><goldenKey>' | md5sum
//	=> 51978ac227dba94b887893ca5ba14917
//
// 另用 printf 的 \xNN 显式 UTF-8 字节形式与 openssl md5 复核，三者一致；
// 字节数也可交叉验证：166（待签串）+ 32（key）= 198。
const goldenSign = "51978ac227dba94b887893ca5ba14917"

// goldenZeroCanonical 是「param=0 且 money=0.00」的待签串，用于证明
// "0" 不会被当成空值剔除。同样由命令行 md5sum 独立算出签名（173 字节 + 32 位 key）。
const goldenZeroCanonical = "money=0.00&name=测试商品&notify_url=https://m.example.com/notify&out_trade_no=M20261113001&param=0&pid=M7K3QPX9Z2AB4C&return_url=https://m.example.com/return&type=alipay"

const goldenZeroSign = "da925ce317b29493d22c7342ec358291"

// 附录 A「回调签发用例」：已支付订单的回调参数。同为命令行独立算出：
//
//	printf '%s' '<goldenCallbackCanonical><goldenKey>' | openssl md5
//	=> 9bea14e3e7b6f68ac4e479b9c3c9b3c5
const goldenCallbackCanonical = "money=12.50&name=测试商品&out_trade_no=M20261113001&pid=M7K3QPX9Z2AB4C&trade_no=20261113000001&trade_status=TRADE_SUCCESS&type=alipay"

const goldenCallbackSign = "9bea14e3e7b6f68ac4e479b9c3c9b3c5"

// goldenParams 返回附录 A 的下单参数。每次返回新 map，避免测试之间互相污染。
func goldenParams() map[string]string {
	return map[string]string{
		"pid":          goldenMerchantNo,
		"type":         "alipay",
		"out_trade_no": "M20261113001",
		"notify_url":   "https://m.example.com/notify",
		"return_url":   "https://m.example.com/return",
		"name":         "测试商品",
		"money":        "12.50",
	}
}

func TestCanonicalV1_Golden(t *testing.T) {
	canonical, err := CanonicalV1(goldenParams())
	if err != nil {
		t.Fatalf("CanonicalV1 返回错误: %v", err)
	}
	if canonical != goldenCanonical {
		t.Fatalf("待签串与附录 A 不一致\n得到: %s\n期望: %s", canonical, goldenCanonical)
	}
	// 字节数也是契约的一部分：中文按 UTF-8 三字节算，"测试商品" 是 12 字节。
	if len(canonical) != 166 {
		t.Fatalf("待签串字节数应为 166，实际 %d", len(canonical))
	}
	// 末尾不得出现 key，也不得出现多余的 & 或 key= 前缀。
	if strings.HasSuffix(canonical, "&") || strings.Contains(canonical, "key=") {
		t.Fatalf("待签串末尾不干净: %q", canonical)
	}
	// 口径锁定：key 32 位（§4.2）、merchant_no 14 位（M + 13 位），与文档附录 A 一致。
	// 谁把这两处常量改回旧口径（31 位 key / 15 位 pid），这里必须立刻红。
	if len(goldenKey) != 32 {
		t.Fatalf("黄金 key 必须 32 位（§4.2 要求），实际 %d", len(goldenKey))
	}
	if len(goldenMerchantNo) != 14 {
		t.Fatalf("merchant_no 必须 14 位（M + 13 位），实际 %d", len(goldenMerchantNo))
	}
	if !strings.HasPrefix(goldenMerchantNo, "M") {
		t.Fatalf("merchant_no 必须以 M 开头，实际 %q", goldenMerchantNo)
	}
}

func TestCanonicalV1_CallbackGolden(t *testing.T) {
	// 回调走同一套 CanonicalV1，但参数集不同（带 trade_no / trade_status，
	// 且没有 notify_url / return_url）。这条用例把回调的字段顺序也钉住：
	// 若下游有人「顺手」给回调加了 notify_url 之类字段，这里就会红。
	params := map[string]string{
		"pid":          goldenMerchantNo,
		"trade_no":     "20261113000001",
		"out_trade_no": "M20261113001",
		"type":         "alipay",
		"name":         "测试商品",
		"money":        "12.50",
		"trade_status": "TRADE_SUCCESS",
		"param":        "", // 商户未传 param：空值剔除，回显时也不该出现
	}

	canonical, err := CanonicalV1(params)
	if err != nil {
		t.Fatalf("CanonicalV1 返回错误: %v", err)
	}
	if canonical != goldenCallbackCanonical {
		t.Fatalf("回调待签串与附录 A 不一致\n得到: %s\n期望: %s", canonical, goldenCallbackCanonical)
	}
	if len(canonical) != 137 {
		t.Fatalf("回调待签串字节数应为 137，实际 %d", len(canonical))
	}

	sign, err := SignV1(params, goldenKey)
	if err != nil {
		t.Fatalf("SignV1 返回错误: %v", err)
	}
	if sign != goldenCallbackSign {
		t.Fatalf("回调签名与命令行 openssl md5 结果不一致\n得到: %s\n期望: %s", sign, goldenCallbackSign)
	}
	// 我方发出的回调 sign 必须能被我们自己的严格校验接受（32 位小写十六进制）：
	// 对端做严格校验时，任何一个大写字符都会让整笔回调被判签名错误。
	if !isLowerMD5Hex(sign) {
		t.Fatalf("我方回调签名必须是 32 位小写十六进制，实际 %q", sign)
	}

	// 回调里 money 必须回下单金额原文；"12.5" 会得到另一个签名。
	mutated := map[string]string{}
	for name, value := range params {
		mutated[name] = value
	}
	mutated["money"] = "12.5"
	if err := VerifyV1(mutated, goldenKey, goldenCallbackSign); !errors.Is(err, ErrSignature) {
		t.Fatalf("money 改成 12.5 后应验签失败，实际 %v", err)
	}
}

func TestCanonicalV1_MultibyteOrder(t *testing.T) {
	// sort.Strings 是字节序，等价 PHP 的 ksort(SORT_STRING)。EPay 的参数名目前
	// 全是 ASCII，但「ASCII 一样所以多字节也一样」是错的假设，这里钉一条向量：
	// 期望顺序 type < 值 < 名，独立推导命令（GNU sort 的字节序）：
	//
	//	printf '名\n值\ntype\n' | LC_ALL=C sort   =>  type / 值 / 名
	//
	// 依据：UTF-8 下 值 = E5 80 BC，名 = E5 90 8D，第二字节 80 < 90，故 值 在前；
	// 而 't' = 0x74 < 0xE5，ASCII 名一律排在中日韩名之前。
	params := map[string]string{
		"名":    "b",
		"值":    "a",
		"type": "alipay",
	}
	canonical, err := CanonicalV1(params)
	if err != nil {
		t.Fatalf("CanonicalV1 返回错误: %v", err)
	}
	const want = "type=alipay&值=a&名=b"
	if canonical != want {
		t.Fatalf("多字节参数名排序不符\n得到: %s\n期望: %s", canonical, want)
	}
}

func TestSignV1_Golden(t *testing.T) {
	sign, err := SignV1(goldenParams(), goldenKey)
	if err != nil {
		t.Fatalf("SignV1 返回错误: %v", err)
	}
	if sign != goldenSign {
		t.Fatalf("签名与命令行 md5sum 结果不一致\n得到: %s\n期望: %s", sign, goldenSign)
	}
	if sign != strings.ToLower(sign) {
		t.Fatalf("签名必须是小写十六进制: %s", sign)
	}
	if len(sign) != 32 {
		t.Fatalf("签名长度应为 32，实际 %d", len(sign))
	}

	// 同一份参数再来一次，纯函数不应有随机性。
	again, err := SignV1(goldenParams(), goldenKey)
	if err != nil {
		t.Fatalf("SignV1 第二次调用返回错误: %v", err)
	}
	if again != sign {
		t.Fatalf("SignV1 不是纯函数: %s != %s", again, sign)
	}
}

func TestSignV1_EmptyAndZero(t *testing.T) {
	// 空串值必须剔除：不影响待签串。
	withEmpty := goldenParams()
	withEmpty["param"] = ""
	withEmpty["device"] = ""
	canonical, err := CanonicalV1(withEmpty)
	if err != nil {
		t.Fatalf("CanonicalV1 返回错误: %v", err)
	}
	if canonical != goldenCanonical {
		t.Fatalf("空值未被剔除\n得到: %s\n期望: %s", canonical, goldenCanonical)
	}

	// "0" 与 "0.00" 必须保留：空串 ≠ "0"。
	// 若把零值当空值剔除，带 param=0 的订单与不带 param 的订单会得到同一个签名，
	// 参数是否出现就无法被签名保护了。
	withZero := goldenParams()
	withZero["param"] = "0"
	withZero["money"] = "0.00"
	zeroCanonical, err := CanonicalV1(withZero)
	if err != nil {
		t.Fatalf("CanonicalV1 返回错误: %v", err)
	}
	// 173 = 166（基准）- 1（"12.50"→"0.00" 少一位）+ 8（"&param=0"）。
	if len(zeroCanonical) != 173 {
		t.Fatalf("零值用例待签串字节数应为 173，实际 %d", len(zeroCanonical))
	}
	if zeroCanonical != goldenZeroCanonical {
		t.Fatalf("零值被误剔除\n得到: %s\n期望: %s", zeroCanonical, goldenZeroCanonical)
	}

	zeroSign, err := SignV1(withZero, goldenKey)
	if err != nil {
		t.Fatalf("SignV1 返回错误: %v", err)
	}
	if zeroSign != goldenZeroSign {
		t.Fatalf("零值用例签名不一致\n得到: %s\n期望: %s", zeroSign, goldenZeroSign)
	}
	if zeroSign == goldenSign {
		t.Fatalf("param=0 与 money=0.00 未进入签名域，签名与基准用例相同")
	}
}

func TestSignV1_Ambiguous(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"值含 &", func(params map[string]string) { params["param"] = "a&b" }},
		{"参数名含 =", func(params map[string]string) { params["a=b"] = "1" }},
		{"参数名含 &", func(params map[string]string) { params["a&b"] = "1" }},
		{"值含 NUL", func(params map[string]string) { params["param"] = "a\x00b" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			params := goldenParams()
			testCase.mutate(params)

			if _, err := CanonicalV1(params); !errors.Is(err, ErrAmbiguousParam) {
				t.Fatalf("CanonicalV1 应返回 ErrAmbiguousParam，实际 %v", err)
			}
			if _, err := SignV1(params, goldenKey); !errors.Is(err, ErrAmbiguousParam) {
				t.Fatalf("SignV1 应返回 ErrAmbiguousParam，实际 %v", err)
			}
			// 验签也要如实上抛，不能被折叠成 ErrSignature。
			if err := VerifyV1(params, goldenKey, goldenSign); !errors.Is(err, ErrAmbiguousParam) {
				t.Fatalf("VerifyV1 应返回 ErrAmbiguousParam，实际 %v", err)
			}
		})
	}

	// 合法值不能被误杀：URL 与 base64 里都有 '='，值含 '=' 是允许的。
	allowed := goldenParams()
	allowed["param"] = "a=b"
	allowed["return_url"] = "https://m.example.com/return?a=1"
	if _, err := CanonicalV1(allowed); err != nil {
		t.Fatalf("值含 '=' 不应被拒绝，实际 %v", err)
	}

	// 同名参数出现两次无法用 map 表达（Go map 天然去重），因此这一项无法在本层测到：
	// 必须由 handler 在解析 query/body 时按出现次数拒绝。原因：Go 的
	// url.Values.Get 取第一个、PHP 的 $_GET 取最后一个，两侧取到不同值即可绕过
	// 金额校验（设计 §1 称之为 V1 最容易被忽略的洞）。
}

func TestVerifyV1_ConstantAndFormat(t *testing.T) {
	// 正常路径：带上 sign / sign_type 时也要通过（两者不参与签名）。
	withSignParams := goldenParams()
	withSignParams["sign"] = goldenSign
	withSignParams["sign_type"] = "MD5"
	if err := VerifyV1(withSignParams, goldenKey, goldenSign); err != nil {
		t.Fatalf("黄金用例验签应通过，实际 %v", err)
	}

	// 篡改任一参数都必须失败。
	tampered := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"改金额", func(params map[string]string) { params["money"] = "12.51" }},
		{"改金额格式", func(params map[string]string) { params["money"] = "12.5" }},
		{"改订单号", func(params map[string]string) { params["out_trade_no"] = "M20261113002" }},
		{"改回调地址", func(params map[string]string) { params["notify_url"] = "https://evil.example.com/notify" }},
		{"加一个参数", func(params map[string]string) { params["param"] = "1" }},
		{"删一个参数", func(params map[string]string) { delete(params, "return_url") }},
	}
	for _, testCase := range tampered {
		t.Run(testCase.name, func(t *testing.T) {
			params := goldenParams()
			testCase.mutate(params)
			if err := VerifyV1(params, goldenKey, goldenSign); !errors.Is(err, ErrSignature) {
				t.Fatalf("篡改后应验签失败，实际 %v", err)
			}
		})
	}

	// 密钥不对。
	if err := VerifyV1(goldenParams(), "00000000000000000000000000000000", goldenSign); !errors.Is(err, ErrSignature) {
		t.Fatalf("错误密钥应验签失败，实际 %v", err)
	}

	// sign 格式非法：大写、长度不对、非十六进制、空 —— 一律 ErrSignature。
	badSigns := map[string]string{
		"大写":    strings.ToUpper(goldenSign),
		"31 位":  goldenSign[:31],
		"33 位":  goldenSign + "a",
		"非十六进制": goldenSign[:31] + "g",
		"空串":    "",
		"带前缀":   "0x" + goldenSign[:30],
	}
	for name, badSign := range badSigns {
		t.Run("sign "+name, func(t *testing.T) {
			if err := VerifyV1(goldenParams(), goldenKey, badSign); !errors.Is(err, ErrSignature) {
				t.Fatalf("非法 sign 应返回 ErrSignature，实际 %v", err)
			}
		})
	}
}
