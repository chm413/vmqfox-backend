// Package secret 负责商户密钥的对称加密封装。
//
// 选 AES-256-GCM 而不是 AES-CBC：密文必须自带完整性校验。没有校验时，
// 被篡改或被别的行误取来的密文会被当成「合法但内容奇怪」的明文继续往下用，
// 故障点就从「密钥不可解密」漂移到日志里那句毫无信息量的「签名错误」。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

var (
	// ErrUnknownKeyVersion 表示密文由当前进程没有的主密钥版本加密。
	// 必须与 ErrDecrypt 区分：它意味着「换错环境/回滚了库」，不是「商户配错了密钥」。
	ErrUnknownKeyVersion = errors.New("密文由未知版本的主密钥加密")
	// ErrDecrypt 表示密钥不对、AAD 不匹配或密文被篡改，三者对外统一，避免调用方据此猜原因。
	ErrDecrypt = errors.New("密文解密失败")
	// ErrNoMasterKey 表示主密钥未配置（VMQ_MASTER_KEY 缺省时 Box 为 nil）。
	ErrNoMasterKey = errors.New("主密钥未配置")
)

const (
	// masterKeySize AES-256 要求 32 字节。长度不符时 aes.NewCipher 会静默降级到 AES-128，
	// 等于加密强度打折，所以在构造期就拒绝。
	masterKeySize = 32
	// nonceSize GCM 推荐值，也是 cipher.NewGCM 的默认 nonce 长度。
	nonceSize = 12
	// fingerprintMessage 是主密钥自检指纹的固定前缀，改动等于让所有存量库的指纹失配。
	fingerprintMessage = "vmqfox/master-key-check/v1"
	// fingerprintHexLen 指纹取 HMAC 前 8 字节（16 位十六进制），足够识别换键又短到能贴进日志。
	fingerprintHexLen = 16
)

// Box 持有「版本 -> 主密钥」的映射与写入版本，可并发使用。
type Box struct {
	keys         map[int][]byte
	writeVersion int
}

// New 用版本化主密钥构造 Box。writeVersion 决定新密文写入哪个版本，
// 其余版本仅用于解密，这是主密钥轮换期间「双读」的基础。
func New(keys map[int][]byte, writeVersion int) (*Box, error) {
	if len(keys) == 0 {
		return nil, errors.New("主密钥集合为空")
	}

	copied := make(map[int][]byte, len(keys))
	for version, key := range keys {
		if len(key) != masterKeySize {
			return nil, fmt.Errorf("主密钥版本 %d 长度为 %d 字节，要求 %d 字节，可用 openssl rand -base64 32 生成",
				version, len(key), masterKeySize)
		}
		// 复制一份，避免调用方后续复用/清零同一个切片导致密钥在运行期被改掉。
		copied[version] = append([]byte(nil), key...)
	}
	if _, ok := copied[writeVersion]; !ok {
		return nil, fmt.Errorf("写入版本 %d 不在主密钥集合中", writeVersion)
	}

	return &Box{keys: copied, writeVersion: writeVersion}, nil
}

// Seal 加密明文并返回密文、nonce 与写入版本，三者必须一起落库。
// nonce 每次都由 crypto/rand 生成：确定性派生（如「用 merchantID 当 nonce」）在 GCM 下
// 会让同一商户两次轮换的密文共用 nonce，直接泄露明文异或值，是硬性禁止项。
func (b *Box) Seal(plain, aad []byte) (cipherText, nonce []byte, keyVersion int, err error) {
	if b == nil {
		return nil, nil, 0, ErrNoMasterKey
	}

	aead, err := b.aead(b.writeVersion)
	if err != nil {
		return nil, nil, 0, err
	}

	nonce = make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		// 绝不允许退化为全零 nonce：那会让所有密文的密钥流相同。
		return nil, nil, 0, fmt.Errorf("生成 nonce 失败: %w", err)
	}

	return aead.Seal(nil, nonce, plain, aad), nonce, b.writeVersion, nil
}

// Open 按密文自带的版本选主密钥解密。版本不存在时返回 ErrUnknownKeyVersion 而不是 ErrDecrypt，
// 好让上层文案落到「商户密钥无法解密，请联系管理员重新下发」，而不是把运维引向商户配置。
func (b *Box) Open(cipherText, nonce, aad []byte, keyVersion int) ([]byte, error) {
	aead, err := b.aead(keyVersion)
	if err != nil {
		return nil, err
	}
	// gcm.Open 对非法的 nonce 长度是 panic 而非返回错误，必须先自己挡掉。
	if len(nonce) != nonceSize {
		return nil, fmt.Errorf("%w: nonce 长度为 %d 字节，要求 %d 字节", ErrDecrypt, len(nonce), nonceSize)
	}

	plain, err := aead.Open(nil, nonce, cipherText, aad)
	if err != nil {
		// GCM 的返回错误不区分「密钥错」「AAD 不匹配」「密文被改」，这里也保持不可区分。
		return nil, fmt.Errorf("%w: 认证数据校验失败", ErrDecrypt)
	}

	return plain, nil
}

func (b *Box) aead(version int) (cipher.AEAD, error) {
	if b == nil {
		return nil, ErrNoMasterKey
	}

	key, ok := b.keys[version]
	if !ok {
		return nil, fmt.Errorf("%w: 版本 %d", ErrUnknownKeyVersion, version)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化 AES 失败: %w", err)
	}

	return cipher.NewGCM(block)
}

// AAD 拼出附加认证数据，把密文钉死在「哪一行」上：
// merchantID ‖ 0x00 ‖ protocol ‖ 0x00 ‖ generation ‖ 0x00 ‖ purpose。
//
// 没有 AAD 时，仓库查询漏掉 protocol 条件会把 V2 平台私钥密文当 V1 共享密钥解开
// （GCM 本身不会报错），然后拿一段 PEM 当 MD5 key 用，签名永远对不上，
// 而日志只显示「签名错误」——这类排查成本极高的故障必须在密码学层就变成解密失败。
//
// protocol/generation 按单字节写入（int8 负值取补码）；purpose 为 ASCII，不可含 0x00。
func AAD(merchantID int64, protocol int8, generation int8, purpose string) []byte {
	buf := make([]byte, 0, 8+1+1+1+1+1+len(purpose))
	buf = binary.BigEndian.AppendUint64(buf, uint64(merchantID))
	buf = append(buf, 0x00, byte(protocol), 0x00, byte(generation), 0x00)
	return append(buf, purpose...)
}

// Fingerprint 按文档 §4.1 计算主密钥自检指纹：HMAC-SHA256(key, 固定串) 前 8 字节的十六进制。
// 主密钥配错时服务照样能启动、decrypt 全失败而界面一切正常，所以必须在启动期就拿它
// 和 settings.masterKeyFingerprint 比对，不一致直接拒绝启动。
func Fingerprint(masterKey []byte) (string, error) {
	if len(masterKey) != masterKeySize {
		return "", fmt.Errorf("主密钥长度为 %d 字节，要求 %d 字节", len(masterKey), masterKeySize)
	}

	mac := hmac.New(sha256.New, masterKey)
	mac.Write([]byte(fingerprintMessage))

	return hex.EncodeToString(mac.Sum(nil)[:fingerprintHexLen/2]), nil
}
