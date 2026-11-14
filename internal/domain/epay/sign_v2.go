package epay

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SignV2 用私钥（PKCS#8 PEM）对 CanonicalV1(params) 做 SHA256WithRSA 签名，返回 Base64。
//
// V2（彩虹/ezfp 系）的待签串构造与 V1 完全一致：剔除 sign/sign_type、剔除空值、
// 参数名 ASCII 升序、值不做 URL 编码。差异只有算法、Base64 编码，以及
// timestamp 参与签名（见设计附录 C）。所以这里复用 CanonicalV1 ——
// 不要在 V2 里另写一套排序逻辑，否则两代签名域迟早漂移。
//
// 签名用 PKCS#1 v1.5（rsa.SignPKCS1v15），不是 PSS：EPay 生态的商户端实现
// 基本都是 `openssl dgst -sha256 -sign` 那一套默认填充。
func SignV2(params map[string]string, privatePKCS8PEM []byte) (string, error) {
	canonical, err := CanonicalV1(params)
	if err != nil {
		return "", err
	}
	key, err := parsePKCS8PrivateKey(privatePKCS8PEM)
	if err != nil {
		return "", err
	}

	digest := sha256.Sum256([]byte(canonical))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("V2 签名失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

// VerifyV2 用公钥（PKIX PEM）验签。
//
// 公钥本身配错（PEM 坏了、不是 RSA）返回具体错误而不是 ErrSignature：
// 这类错误是我方配置问题，必须能从日志里和「商户报文被篡改」区分开。
// 只有「签名值不匹配」与「sign 不是合法 Base64」才归为 ErrSignature。
func VerifyV2(params map[string]string, publicPKIXPEM []byte, signBase64 string) error {
	canonical, err := CanonicalV1(params)
	if err != nil {
		return err
	}
	key, err := parsePKIXPublicKey(publicPKIXPEM)
	if err != nil {
		return err
	}

	signature, ok := decodeBase64Loose(signBase64)
	if !ok {
		return ErrSignature
	}
	digest := sha256.Sum256([]byte(canonical))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return ErrSignature
	}
	return nil
}

// TimestampValid 校验 10 位秒级时间戳与 now 的偏差是否在 skew 内。
//
// 非 10 位纯数字一律 false：8 位（毫秒被截断）、13 位（毫秒）、ISO8601 都直接拒绝，
// 不做猜测式解析 —— 猜错方向就等于 ±300s 的重放防护失效。
// 若设计附录 C「联调首日必须核对的三项」第 1 项最终确认为 ISO8601，
// 在这个函数里加分支，不要在上游 handler 里各自解析。
func TimestampValid(timestamp string, now time.Time, skew time.Duration) bool {
	if skew < 0 {
		// 负窗口等价于「永不通过」，而不是「全部放行」：
		// 调用方把配置写错时宁可拒单，也不能让重放防护静默失效。
		return false
	}
	if len(timestamp) != 10 {
		return false
	}
	for i := 0; i < len(timestamp); i++ {
		if timestamp[i] < '0' || timestamp[i] > '9' {
			return false
		}
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}

	diff := now.Unix() - seconds
	if diff < 0 {
		diff = -diff
	}
	// 亚秒级 skew 会被截断成 0，此时只接受同一秒；窗口语义按整秒计。
	return diff <= int64(skew/time.Second)
}

// AppendTimestamp 给参数集写入 timestamp=now.Unix() 的十进制字符串（就地修改 map）。
//
// 只用于我方生成响应与回调。处理外部请求时不能先 Append 再验签，
// 那等于用我方时间覆盖对方时间，重放防护形同失效。
func AppendTimestamp(params map[string]string, now time.Time) {
	params["timestamp"] = strconv.FormatInt(now.Unix(), 10)
}

func parsePKCS8PrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("V2 私钥不是合法的 PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("V2 私钥不是合法的 PKCS#8: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("V2 私钥不是 RSA 私钥")
	}
	return key, nil
}

func parsePKIXPublicKey(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("V2 公钥不是合法的 PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("V2 公钥不是合法的 PKIX: %w", err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("V2 公钥不是 RSA 公钥")
	}
	return key, nil
}

// decodeBase64Loose 兼容标准 Base64 的几种常见变体。
//
// 存在的理由：标准 Base64 里的 '+' 一旦经过 query 未编码传输会被改写成空格，
// '/' 也会遇到 URL-safe 变体；这些都与签名值无关，只是传输层走了样。
// 容错不影响安全性 —— 解出来的字节仍要过 RSA 验签。
func decodeBase64Loose(text string) ([]byte, bool) {
	if text == "" {
		return nil, false
	}
	// 标准 Base64 自身不含空格，把空格还原成 '+'：标准 Base64 的 '+' 一旦经过
	// query 未编码传输就会被改写成空格，这是传输层副作用，与签名值无关。
	// 容错不影响安全性 —— 解出来的字节仍要过 RSA 验签，签名对不上照样拒绝。
	candidate := strings.ReplaceAll(text, " ", "+")
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(candidate); err == nil {
			return decoded, true
		}
	}
	return nil, false
}
