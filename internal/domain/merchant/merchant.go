// Package merchant 定义商户与商户密钥的领域模型。
//
// 与 order/payment 一样是纯数据 + 纯规则：不 import gorm、不 import gin、不碰 context，
// 加解密细节留给 secret 包，持久化留给 adapter。
package merchant

import (
	"fmt"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
)

// Protocol 是接口协议版本，落库为 orders.protocol / merchant_keys.protocol。
//
// 注意与 payment.SignVersion2（VMQ 自己的签名协议 v2）区分，两者毫无关系。
type Protocol int8

const (
	// ProtocolNative 是 VMQ 原生接口，merchant_id=0 的系统商户，不需要商户密钥。
	ProtocolNative Protocol = 0
	// ProtocolEPayV1 易支付旧版协议：MD5 共享密钥。
	ProtocolEPayV1 Protocol = 1
	// ProtocolEPayV2 易支付新版协议：SHA256WithRSA 双向签名。
	ProtocolEPayV2 Protocol = 2
)

// Valid 只认 V1/V2：原生协议不参与商户密钥体系，任何“给原生商户配密钥”的调用都是 bug。
func (p Protocol) Valid() bool {
	return p == ProtocolEPayV1 || p == ProtocolEPayV2
}

func (p Protocol) String() string {
	switch p {
	case ProtocolNative:
		return "native"
	case ProtocolEPayV1:
		return "epay.v1"
	case ProtocolEPayV2:
		return "epay.v2"
	default:
		// 带上原值：协议字段错位导致的「查不到配置」全靠日志里这个数字定位。
		return fmt.Sprintf("unknown(%d)", int8(p))
	}
}

// Purpose 返回该协议密文的用途标签，参与 secret.AAD。
// 值一旦改动，所有存量密文都会解不开，等同不可回滚的迁移。
func (p Protocol) Purpose() string {
	switch p {
	case ProtocolEPayV1:
		return "epay.v1.key"
	case ProtocolEPayV2:
		return "epay.v2.private"
	default:
		// 原生协议不落 merchant_keys，取不到密文；空串只是兜底，不应被真正用上。
		return ""
	}
}

// 代（generation）只有两档：主键 (merchant_id, protocol, generation) 天然限制每商户每协议最多两行。
const (
	// GenerationCurrent 当前代，用于签发与回调签名。
	GenerationCurrent int8 = 1
	// GenerationPrevious 上一代，仅用于验签，轮换后保留到下一次轮换被清掉。
	GenerationPrevious int8 = 0
)

// CloseMinutesDefault 是公告的默认订单有效期，与主流发卡站的支付窗口对齐。
const CloseMinutesDefault = 30

type Merchant struct {
	ID   int64
	No   string
	Name string
	// Enabled=false 只拦新下单；在途订单的回调照常投递，否则等于把已收的钱变成不发货。
	Enabled bool
	// CloseMinutes 为 0 表示跟随全局 settings.close；原生订单不看这个字段。
	CloseMinutes int
	Note         string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Key 对应 merchant_keys 的一行。
type Key struct {
	MerchantID int64
	Protocol   Protocol
	Generation int8
	// KeyVersion 记录加密时使用的主密钥版本，解密时按它选键，而不是“试哪把能解开”。
	KeyVersion int
	// SecretCipher/SecretNonce V1 存共享密钥、V2 存平台私钥，均为 AES-256-GCM 密文。
	SecretCipher []byte
	SecretNonce  []byte
	// PeerPublicKey 是商户自己的公钥（V2 专用），不敏感所以明文存。
	// 它属于「商户身份」而不是「我方密钥」，轮换平台密钥时必须原样带入新代。
	PeerPublicKey string
	// Hint V1 存密钥末 4 位，V2 存公钥指纹前 16 位，供管理端辨认而无需解密。
	Hint       string
	LastUsedAt time.Time
	CreatedAt  time.Time
}

// AAD 返回该行密文的附加认证数据，保证密文不能被跨行复用。
// 放在这里而不是让调用方手拼，是为了让「protocol/purpose 传错」只有一个写法。
func (k Key) AAD() []byte {
	return secret.AAD(k.MerchantID, int8(k.Protocol), k.Generation, k.Protocol.Purpose())
}
