package epay

import (
	"errors"
	"strings"
	"testing"

	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
)

func TestValidateAmount(t *testing.T) {
	valid := []struct {
		text  string
		cents int64
	}{
		{"10", 1000},
		{"10.5", 1050}, // 5 是 5 角，不是 5 分
		{"10.50", 1050},
		{"12.50", 1250},
		{"0", 0},
		{"0.00", 0},
		{"0.01", 1},
		{"0.1", 10},
		{"1", 100},
		{"9999999999.99", 999999999999}, // 正则上限：10 位整数 + 2 位小数
	}
	for _, testCase := range valid {
		cents, err := ValidateAmount(testCase.text)
		if err != nil {
			t.Fatalf("ValidateAmount(%q) 返回错误: %v", testCase.text, err)
		}
		if cents != testCase.cents {
			t.Fatalf("ValidateAmount(%q) = %d，期望 %d", testCase.text, cents, testCase.cents)
		}
	}

	invalid := []string{
		"10.555",      // 三位小数
		"+10",         // 正号
		"-1",          // 负号
		" 10",         // 前导空格
		"10 ",         // 尾随空格
		"1e3",         // 科学计数
		"",            // 空串
		".5",          // 缺整数部分
		"10.",         // 缺小数部分
		"1,000",       // 千分位
		"10.5.5",      // 多个小数点
		"0x10",        // 十六进制
		"10000000000", // 11 位整数，超上限
		"１０",          // 全角数字
		"10.٥",        // 非 ASCII 数字
	}
	for _, text := range invalid {
		if _, err := ValidateAmount(text); !errors.Is(err, ErrInvalidAmount) {
			t.Fatalf("ValidateAmount(%q) 应返回 ErrInvalidAmount，实际 %v", text, err)
		}
	}
}

func TestNormalizeAmount(t *testing.T) {
	cases := []struct {
		cents int64
		text  string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{10, "0.10"},
		{50, "0.50"},
		{100, "1.00"},
		{1050, "10.50"},
		{1250, "12.50"},
		{999999999999, "9999999999.99"},
		{-1250, "-12.50"},
	}
	for _, testCase := range cases {
		if got := NormalizeAmount(testCase.cents); got != testCase.text {
			t.Fatalf("NormalizeAmount(%d) = %q，期望 %q", testCase.cents, got, testCase.text)
		}
	}

	// 与 ValidateAmount 往返一致：商户传 "10.5" 解析后格式化回来是 "10.50"，
	// 两者是不同的待签串，所以 NormalizeAmount 只能用于我方生成回调，
	// 不能拿来改写商户原文（见函数注释）。
	cents, err := ValidateAmount("10.5")
	if err != nil {
		t.Fatalf("ValidateAmount 返回错误: %v", err)
	}
	if NormalizeAmount(cents) != "10.50" {
		t.Fatalf("往返应为 10.50，实际 %q", NormalizeAmount(cents))
	}
	if NormalizeAmount(cents) == "10.5" {
		t.Fatal("NormalizeAmount 不应保留一位小数写法")
	}
}

func TestExcludeFromSign(t *testing.T) {
	if !ExcludeFromSign("sign") || !ExcludeFromSign("sign_type") {
		t.Fatal("sign 与 sign_type 必须不参与签名")
	}
	// 精确小写匹配：变体不剔除，交给 handler 拒绝。
	for _, name := range []string{"Sign", "SIGN", "sign_Type", "signType", "signs", "", "pid"} {
		if ExcludeFromSign(name) {
			t.Fatalf("%q 不应被当作签名参数剔除", name)
		}
	}
}

func TestRejectAmbiguous(t *testing.T) {
	valid := []map[string]string{
		{"pid": goldenMerchantNo},
		{"return_url": "https://m.example.com/return"},
		{"param": "a=b"},  // base64 常见，允许
		{"name": "测试商品"},  // 中文
		{"money": "0.00"}, // 零值
		{},
	}
	for i, params := range valid {
		if err := RejectAmbiguous(params); err != nil {
			t.Fatalf("第 %d 组合法参数被误判: %v", i, err)
		}
	}

	invalid := []map[string]string{
		{"param": "a&b"},                  // 值含 &
		{"a=b": "1"},                      // 名含 =
		{"a&b": "1"},                      // 名含 &
		{"param": "a\x00b"},               // 值含 NUL
		{"pid": "M1", "name": "x&pid=M2"}, // 值里塞第二条参数
	}
	for i, params := range invalid {
		if err := RejectAmbiguous(params); !errors.Is(err, ErrAmbiguousParam) {
			t.Fatalf("第 %d 组应返回 ErrAmbiguousParam，实际 %v", i, err)
		}
	}

	// 已知取舍（设计 §1）：URL 的 query 里带 & 时，即便商户按 %26 编码，
	// 解码后的值仍含 &，待签串无法唯一还原参数边界，因此一律拒绝。
	// 这确实会挡住「回跳地址带多个查询参数」的商户，需要 handler 给出明确文案，
	// 而不是静默拼出一个可能产生二义性的待签串。
	if err := RejectAmbiguous(map[string]string{"return_url": "https://m.example.com/r?a=1&b=2"}); !errors.Is(err, ErrAmbiguousParam) {
		t.Fatalf("值含 & 的 URL 应被拒绝，实际 %v", err)
	}

	// 参数走私的具体形态：签名域是解码后的值，而商户侧 PHP 用 $_GET 解析回调串。
	// 若放开值里的 &，这段在回调串里会被对端拆成顶层 money=1000，而 MD5 仍然对得上。
	smuggled := map[string]string{
		"return_url": "https://a.com/r?x=1&money=1000",
	}
	if err := RejectAmbiguous(smuggled); !errors.Is(err, ErrAmbiguousParam) {
		t.Fatalf("参数走私形态必须被拒绝，实际 %v", err)
	}

	// 控制字符（0x00–0x1F、0x7F）一律拒绝：CR/LF 是日志注入的基本盘，
	// name/return_url 会进日志，将来还可能进 302 的 Location。
	controlCases := []struct {
		name   string
		params map[string]string
	}{
		// team-lead 指定的两条：name 含 LF、return_url 含 CRLF（响应头注入形态）。
		{"name 值含 LF", map[string]string{"name": "a\nb"}},
		{"return_url 值含 CRLF", map[string]string{"return_url": "https://m.example.com/return\r\nX-Injected: 1"}},
		{"值含 NUL", map[string]string{"param": "a\x00b"}},
		{"值含 CR", map[string]string{"param": "a\rb"}},
		{"值含 LF", map[string]string{"param": "a\nb"}},
		{"值含 CRLF", map[string]string{"param": "a\r\nsign=deadbeef"}},
		{"值含 TAB", map[string]string{"param": "a\tb"}},
		{"值含 DEL", map[string]string{"param": "a\x7fb"}},
		{"值含 0x1F", map[string]string{"param": "a\x1fb"}},
		{"名含 LF", map[string]string{"a\nb": "1"}},
	}
	for _, testCase := range controlCases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := RejectAmbiguous(testCase.params); !errors.Is(err, ErrAmbiguousParam) {
				t.Fatalf("应返回 ErrAmbiguousParam，实际 %v", err)
			}
		})
	}

	// 错误文案必须能说明是「控制字符」，否则运维看到「歧义分隔符」会去查 & 和 =，
	// 而实际问题是一个不可见字符。文案里的关键词在此钉死。
	for _, keyword := range []string{"控制字符", "歧义"} {
		if !strings.Contains(ErrAmbiguousParam.Error(), keyword) {
			t.Fatalf("ErrAmbiguousParam 文案应包含 %q，实际 %q", keyword, ErrAmbiguousParam.Error())
		}
	}

	// 0x80 以上不属于控制字符：中文按 UTF-8 多字节传输，必须放行。
	if err := RejectAmbiguous(map[string]string{"name": "测试商品", "param": "中文参数"}); err != nil {
		t.Fatalf("中文不应被拒绝，实际 %v", err)
	}
	// 按字节扫 vs 按 rune 判 unicode.IsControl 的分界线，用正例钉死。
	// 结构性依据：合法 UTF-8 的续字节是 0x80–0xBF、多字节首字节 ≥ 0xC2，
	// C0 与 DEL 不可能出现在任何合法多字节序列里，因此下面这些都必须放行。
	//
	// 各条守的是不同的坑（unicode.IsControl 实测覆盖 Cc = 0x00–0x1F 与 0x7F–0x9F）：
	//   - U+0085（UTF-8 C2 85）：IsControl 判 true → 改成按 rune 判就会被误拒，本条挡这个；
	//   - U+2028/U+2029（E2 80 A8/A9）：IsControl 判 false，它们不属 Cc；
	//     仍要放行，是为了挡「strings.ContainsAny(v, "\r\n\u2028\u2029")」这类
	//     手写过严实现 —— 与 PHP 对端按字节处理保持一致，比"更严格"重要；
	//   - 😀（F0 9F 98 80）：4 字节首字节，验证「≥ 0x80 一律放行」的上界。
	validUTF8 := []string{
		"测试商品",     // 中文（3 字节）
		"a\u0085b", // U+0085 NEL：unicode.IsControl 判 true
		"a\u2028b", // U+2028 行分隔符：IsControl 判 false，但手写实现容易误拒
		"a\u2029b", // U+2029 段分隔符：同上
		"😀",        // 4 字节 emoji，首字节 0xF0
		"テスト商品",    // 日文
	}
	for _, value := range validUTF8 {
		if err := RejectAmbiguous(map[string]string{"name": value}); err != nil {
			t.Fatalf("合法 UTF-8 值 %q 不应被拒绝，实际 %v", value, err)
		}
	}
	// 空格（0x20）不在 C0 范围内，不能误伤：商户商品名带空格是常态。
	if err := RejectAmbiguous(map[string]string{"name": "测试 商品", "return_url": "https://m.example.com/return"}); err != nil {
		t.Fatalf("含空格的正常值不应被拒绝，实际 %v", err)
	}
	// 空格与可打印符号也不该被误杀。
	if err := RejectAmbiguous(map[string]string{"name": "测试 商品!", "param": "a-b_c.d~e"}); err != nil {
		t.Fatalf("可打印字符不应被拒绝，实际 %v", err)
	}
	// 边界：0x20（空格）放行、0x1F 拒绝 —— 一个字节之差，别让范围写错。
	if err := RejectAmbiguous(map[string]string{"param": " "}); err != nil {
		t.Fatalf("纯空格值不应被拒绝，实际 %v", err)
	}
	if err := RejectAmbiguous(map[string]string{"param": "\x1f"}); !errors.Is(err, ErrAmbiguousParam) {
		t.Fatalf("0x1F 应被拒绝，实际 %v", err)
	}
}

// TestTypeMap 覆盖 type_map.go：EPay type 字符串 ↔ payment.Type 双向映射。
func TestTypeMap(t *testing.T) {
	// 内部枚举值是与数据库存量数据绑定的契约，钉死它，防止改常量后静默错位。
	if payment.Alipay != 2 || payment.Wechat != 1 {
		t.Fatalf("payment 常量被改动: Alipay=%d Wechat=%d", payment.Alipay, payment.Wechat)
	}

	fromEPay := []struct {
		value string
		want  payment.Type
	}{
		{"alipay", payment.Alipay},
		{"wxpay", payment.Wechat},
	}
	for _, testCase := range fromEPay {
		got, ok := TypeFromEPay(testCase.value)
		if !ok || got != testCase.want {
			t.Fatalf("TypeFromEPay(%q) = (%d, %v)，期望 (%d, true)", testCase.value, got, ok, testCase.want)
		}
	}

	// 未开通渠道必须明确失败，不允许回落到微信/支付宝。
	unsupported := []string{"unionpay", "bank", "qqpay", "wechat", "weixin", "Alipay", "WXPAY", " alipay", "alipay ", ""}
	for _, value := range unsupported {
		if got, ok := TypeFromEPay(value); ok {
			t.Fatalf("TypeFromEPay(%q) 应失败，实际返回 %d", value, got)
		}
	}

	// 反向映射用于回调原样回显。
	toEPay := []struct {
		value payment.Type
		want  string
	}{
		{payment.Alipay, "alipay"},
		{payment.Wechat, "wxpay"},
	}
	for _, testCase := range toEPay {
		got, ok := EPayTypeFrom(testCase.value)
		if !ok || got != testCase.want {
			t.Fatalf("EPayTypeFrom(%d) = (%q, %v)，期望 (%q, true)", testCase.value, got, ok, testCase.want)
		}
	}
	for _, value := range []payment.Type{0, 3, -1} {
		if got, ok := EPayTypeFrom(value); ok {
			t.Fatalf("EPayTypeFrom(%d) 应失败，实际返回 %q", value, got)
		}
	}

	// 双向往返必须闭合。
	for _, value := range []payment.Type{payment.Alipay, payment.Wechat} {
		text, ok := EPayTypeFrom(value)
		if !ok {
			t.Fatalf("EPayTypeFrom(%d) 失败", value)
		}
		back, ok := TypeFromEPay(text)
		if !ok || back != value {
			t.Fatalf("往返不闭合: %d -> %q -> %d", value, text, back)
		}
	}
}
