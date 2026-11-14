package epay

import (
	"regexp"
	"strconv"
	"strings"
)

// amountPattern 严格匹配 EPay 的 money：元、最多十位整数、最多两位小数。
//
// 刻意收紧：前导正负号、前后空格、千分位、科学计数、全角数字一律不认。
// money 原文要进签名域，任何「顺手解析再格式化」都会改变待签串，导致商户侧
// 验签失败，而日志里只会显示「签名错误」（设计 §5.2）。
var amountPattern = regexp.MustCompile(`^[0-9]{1,10}(\.[0-9]{1,2})?$`)

// ValidateAmount 校验 EPay 的 money 文本并返回分值。
//
// 合法形态：`^[0-9]{1,10}(\.[0-9]{1,2})?$`；不合法返回 ErrInvalidAmount。
// 完全避开浮点：先按小数点切分再整数相加，否则 0.1+0.2 这类误差会污染金额。
func ValidateAmount(text string) (int64, error) {
	if !amountPattern.MatchString(text) {
		return 0, ErrInvalidAmount
	}

	yuanText := text
	centText := "00"
	if dot := strings.IndexByte(text, '.'); dot >= 0 {
		yuanText = text[:dot]
		centText = text[dot+1:]
	}
	// "10.5" 的 5 是 5 角 = 50 分，必须补零成 "50" 而不是当成 5 分。
	for len(centText) < 2 {
		centText += "0"
	}

	yuan, err := strconv.ParseInt(yuanText, 10, 64)
	if err != nil {
		// 正则已限定最多 10 位数字，理论上不会溢出；保留判断以防正则被放宽。
		return 0, ErrInvalidAmount
	}
	cent, err := strconv.ParseInt(centText, 10, 64)
	if err != nil {
		return 0, ErrInvalidAmount
	}
	return yuan*100 + cent, nil
}

// NormalizeAmount 把分值格式化成 "12.50"。
//
// 只用于我方生成回调与查单响应里的 money，绝不能拿来改写商户下单时传来的原文：
// "12.5" 与 "12.50" 是两个不同的待签串，改写必然导致商户侧验签失败。
func NormalizeAmount(cents int64) string {
	negative := cents < 0
	var magnitude uint64
	if negative {
		// 走无符号幅值，避免对 math.MinInt64 取反溢出。
		magnitude = uint64(-(cents + 1)) + 1
	} else {
		magnitude = uint64(cents)
	}

	// 角分固定两位：7 分 → "07"，50 分 → "50"。
	cent := magnitude % 100
	centText := string([]byte{'0' + byte(cent/10), '0' + byte(cent%10)})
	text := strconv.FormatUint(magnitude/100, 10) + "." + centText
	if negative {
		return "-" + text
	}
	return text
}

// ExcludeFromSign 判断参数名是否不参与签名：sign、sign_type，精确小写匹配。
//
// 大小写变体（Sign、SIGN_TYPE）在这里返回 false，即会被算进待签串 ——
// 这是有意的：剔不剔除只影响签名域，而「是否允许出现变体」属于入口校验，
// 由 handler 拒绝（EPay 规范里不存在变体，放过变体等于给下游留一个不参与
// 签名却能被读到的字段）。
func ExcludeFromSign(name string) bool {
	return name == "sign" || name == "sign_type"
}

// RejectAmbiguous 拒绝会让待签串产生二义性的参数。
//
//   - 参数名含 '='：`a=b` 拼值后无法与「参数名 a、值 b」区分；
//   - 参数名或参数值含 '&'：`a=1&b=2` 无法判断是一条参数还是两条。
//     这条必须保留，不是洁癖：签名域是解码后的值，而商户侧 PHP 用 $_GET 解析
//     回调串，若放开，`return_url=...?x=1%26money=1000` 解码后含 &，我们发出的
//     回调串里这段会被对端拆成顶层 money=1000，签名却完全对得上 —— 参数走私
//     直接改写商户记录的金额；
//   - 参数名或参数值含控制字符（0x00–0x1F、0x7F）：NUL 会在 MySQL 与日志处截断，
//     CR/LF 是日志注入的基本盘，详见 hasControlByte。
//
// 同名参数出现两次（Go 的 url.Values.Get 取第一个、PHP 的 $_GET 取最后一个）
// 无法用 map 表达，必须由 handler 在解析 query/body 时按出现次数拒绝。
// CanonicalV1 内部会调用本函数，避免调用方漏调。
func RejectAmbiguous(params map[string]string) error {
	for name, value := range params {
		if strings.ContainsAny(name, "=&") || hasControlByte(name) {
			return ErrAmbiguousParam
		}
		if strings.IndexByte(value, '&') >= 0 || hasControlByte(value) {
			return ErrAmbiguousParam
		}
	}
	return nil
}

// hasControlByte 判断是否含 C0 控制字符（0x00–0x1F）或 DEL（0x7F）。
//
// 为什么连 NUL 以外的控制字符一起拒：
//   - NUL 会在 MySQL 与日志里截断值，两侧看到的字符串不同；
//   - CR/LF 是日志注入的基本盘（name/return_url 会进日志，将来还可能进 302 的
//     Location；Go 的 http 包会拦 header 里的换行，日志层不会），伪造一行
//     "sign 错误" 之类的日志足以把排障带偏；
//   - 其余 C0 与 DEL 没有合法用途，只会让日志不可读。
//
// TAB 也在拒绝范围内：EPay 参数里没有合法用 TAB 的字段，放过它等于留一个
// 不可见的分隔符。
//
// 为什么按「字节」扫而不是按 rune 判 unicode.IsControl：
//
// 0x80 以上一律放行不是「大概没问题」，而是结构性保证 —— 合法 UTF-8 的续字节是
// 0x80–0xBF、多字节首字节 ≥ 0xC2，所以 C0 与 DEL 永远不可能出现在任何合法的
// 多字节序列里，这条规则对任何合法 UTF-8 输入都不会误伤。
//
// 反过来，unicode.IsControl 覆盖的是 Unicode 类别 Cc，即 0x00–0x1F **与
// 0x7F–0x9F**（实测：U+0085 → true，其 UTF-8 是 C2 85）。也就是说它会额外拒绝
// 整个 C1 段 —— 那些字符在 PHP 对端（按字节处理）是完全合法的文本，属于误伤。
// 另一个方向它又不够：U+2028/U+2029 属 Zl/Zp 而非 Cc，IsControl 判 false
// （实测），所以「日志里被当成换行」的这类字符它也拦不住。
// 结论：与对端对齐只能按字节扫，params_test.go 里 U+0085/U+2028 的正例断言
// 就是这条决定的门闩。
func hasControlByte(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] < 0x20 || text[i] == 0x7f {
			return true
		}
	}
	return false
}
