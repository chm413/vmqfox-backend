package merchant

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
)

// alphabet 去掉 0/O/1/I/L：商户密钥要在各家发卡站面板里手工复制粘贴，
// 易混淆字符是「密钥抄错」类工单的主要来源。31 个字符 × 32 位 ≈ 158 bit，
// 远超 MD5 共享密钥的暴力破解需求。
const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"

const (
	// merchantNoPrefix 固定前缀，让运营者在日志里一眼认出这是对外商户号而不是订单号。
	merchantNoPrefix = "M"
	// merchantNoLength 对外 pid 总长度（"M" + 13 位随机字符）。
	// 自增 id 可枚举且会泄露商户规模，所以对外标识必须是随机串。
	merchantNoLength = 14
	// V1SecretLength 与文档 §4.2 一致，解密后必须按这个长度断言。
	V1SecretLength = 32
	// rsaBits 2048 位是 EPay 生态通行值；4096 在部分老 PHP 环境验签会超时。
	rsaBits = 2048
	// hintPublicKeyLength 公钥指纹取 sha256 前 16 位十六进制，够辨认又不会塞满 VARCHAR(16)。
	hintPublicKeyLength = 16
)

// NewMerchantNo 生成对外商户号（pid），格式 "M" + 13 位随机字符。
func NewMerchantNo() (string, error) {
	suffix, err := randomFromAlphabet(merchantNoLength - len(merchantNoPrefix))
	if err != nil {
		return "", err
	}
	return merchantNoPrefix + suffix, nil
}

// NewV1Secret 生成 EPay V1 的 MD5 共享密钥：32 位小写 base32 字符。
func NewV1Secret() (string, error) {
	return randomFromAlphabet(V1SecretLength)
}

// IsValidV1Secret 校验解密出来的 V1 密钥。解密点必须 fail-fast：
// 任何编码/截断 bug 如果在签名阶段才暴露，日志里只会显示「签名错误」，排查方向全错。
func IsValidV1Secret(secret string) bool {
	if len(secret) != V1SecretLength {
		return false
	}
	for i := 0; i < len(secret); i++ {
		if strings.IndexByte(alphabet, secret[i]) < 0 {
			return false
		}
	}
	return true
}

// NewRSAKeyPair 生成 V2 平台密钥对，返回 PKCS#8 私钥 PEM 与 PKIX 公钥 PEM。
// 平台私钥由本网关自己加密入库，公钥导出给商户验签；每商户一把，不共享。
func NewRSAKeyPair() (privatePKCS8PEM, publicPKIXPEM []byte, err error) {
	// 用 rsa.GenerateKey 而不是自己拼素数：它的随机源固定是 crypto/rand。
	key, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, nil, fmt.Errorf("生成 RSA 密钥对失败: %w", err)
	}
	if err := key.Validate(); err != nil {
		return nil, nil, fmt.Errorf("RSA 密钥对自检失败: %w", err)
	}

	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("编码 PKCS#8 私钥失败: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("编码 PKIX 公钥失败: %w", err)
	}

	privatePKCS8PEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	publicPKIXPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})

	return privatePKCS8PEM, publicPKIXPEM, nil
}

// HintV1 取密钥末 4 位，供管理端辨认是哪一把而无需解密。
func HintV1(secret string) string {
	if len(secret) <= 4 {
		return secret
	}
	return secret[len(secret)-4:]
}

// HintPublicKey 取公钥 PEM 字节的 sha256 前 16 位小写十六进制。
// 公钥本身会随导出而公开，指纹只用来比对「商户贴的是不是这一把」。
func HintPublicKey(publicPEM []byte) string {
	sum := sha256.Sum256(publicPEM)
	return hex.EncodeToString(sum[:])[:hintPublicKeyLength]
}

// randomFromAlphabet 用 crypto/rand 逐字符取料。
// 不用 math/rand：它的种子在容器里可能来自同一时刻，重启后生成重复商户号与密钥。
// 也不用「随机字节取模」：256 不能被 31 整除，取模会让前半段字符出现得更频繁，
// rand.Int 走的是无偏拒绝采样。
func randomFromAlphabet(length int) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("随机串长度 %d 非法", length)
	}

	limit := big.NewInt(int64(len(alphabet)))
	out := make([]byte, length)
	for i := range out {
		index, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("读取随机数失败: %w", err)
		}
		out[i] = alphabet[index.Int64()]
	}

	return string(out), nil
}
