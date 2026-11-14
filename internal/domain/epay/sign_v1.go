package epay

import (
	"crypto/md5"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
)

// CanonicalV1 生成 EPay V1 的待签串（不含末尾 key）。
//
// 逐字节复刻 EPay 的实现，任何「顺手规范化」都会让第三方验签失败：
//  1. 剔除 sign、sign_type（参数名精确小写匹配）；
//  2. 剔除值为空串的参数 —— 空串 ≠ "0"，"0" 与 "0.00" 必须保留；
//  3. 按参数名 ASCII 升序（sort.Strings 即字节序，等价 PHP 的 ksort(SORT_STRING)）；
//  4. 用 a=b&c=d 拼接，值不做 URL 编码、不 trim（签名域永远用解码后的原始值，
//     组装发送串时才 urlencode，见设计 §1「签名与发送的编码关系」）；
//  5. 末尾不追加 key，由 SignV1 直接拼上去。
func CanonicalV1(params map[string]string) (string, error) {
	if err := RejectAmbiguous(params); err != nil {
		return "", err
	}

	names := make([]string, 0, len(params))
	for name, value := range params {
		if ExcludeFromSign(name) {
			continue
		}
		if value == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var builder strings.Builder
	for i, name := range names {
		if i > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(params[name])
	}
	return builder.String(), nil
}

// SignV1 = md5(CanonicalV1(params) + key)，小写十六进制。
//
// key 是「紧接」在待签串末尾的：没有 &，也没有 key= 前缀 ——
// 这是 V1 实现里最常见的错误点（见设计附录 A）。
func SignV1(params map[string]string, key string) (string, error) {
	canonical, err := CanonicalV1(params)
	if err != nil {
		return "", err
	}
	return md5LowerHex(canonical + key), nil
}

// VerifyV1 以常量时间比较校验商户签名。
//
// sign 必须恰好 32 位小写十六进制，否则直接判 ErrSignature ——
// 不做「转小写再比」的宽容处理，避免同一个签名出现多种可接受写法。
// 参数含歧义时如实上抛 ErrAmbiguousParam 而不是 ErrSignature：
// 否则会被误判成「商户密钥配错」，这类问题在生产上极难定位。
func VerifyV1(params map[string]string, key, sign string) error {
	if !isLowerMD5Hex(sign) {
		return ErrSignature
	}
	expected, err := SignV1(params, key)
	if err != nil {
		return err
	}
	if !payment.SignEqual(sign, expected) {
		return ErrSignature
	}
	return nil
}

// isLowerMD5Hex 判断是否为 32 位小写十六进制。
func isLowerMD5Hex(text string) bool {
	if len(text) != 32 {
		return false
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func md5LowerHex(text string) string {
	sum := md5.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}
