package setting

const (
	MerchantKey     = "key"
	NotifyURLKey    = "notifyUrl"
	ReturnURLKey    = "returnUrl"
	LastHeartKey    = "lastheart"
	LastPayKey      = "lastpay"
	MonitorStateKey = "jkstate"
	CloseMinutesKey = "close"
	PriceAdjustKey  = "payQf"
	WechatPayURLKey = "wxpay"
	AlipayPayURLKey = "zfbpay"
	// AlipayAccountKey 是收款方支付宝账号（手机号或邮箱），仅用于生成「转账确认单」
	// 深度链接；留空时收银台退回二维码扫码，行为与未配置前完全一致。
	AlipayAccountKey = "alipayAccount"
	// MasterKeyFingerprintKey 是主密钥自检指纹（HMAC 前 8 字节的十六进制）。
	// 它由系统在首次启用主密钥时写入，仅用于启动期比对「环境里的主密钥是否还是
	// 当初那一把」；本身不含密钥材料，也不参与任何加解密，因此归为非敏感项。
	// 管理端 Update 的白名单不含它，普通保存设置不会覆盖；若有人直接用 SQL 改错，
	// 服务会在下次启动时拒绝启动——这是刻意的：主密钥配错的后果是所有商户密钥
	// 静默解不开，比启动失败难排查得多。
	MasterKeyFingerprintKey = "masterKeyFingerprint"
)

var allKeys = []string{
	MerchantKey,
	NotifyURLKey,
	ReturnURLKey,
	LastHeartKey,
	LastPayKey,
	MonitorStateKey,
	CloseMinutesKey,
	PriceAdjustKey,
	WechatPayURLKey,
	AlipayPayURLKey,
	AlipayAccountKey,
	MasterKeyFingerprintKey,
}

func AllKeys() []string {
	return append([]string(nil), allKeys...)
}

func IsSensitiveKey(key string) bool {
	switch key {
	case MerchantKey, WechatPayURLKey, AlipayPayURLKey:
		return true
	default:
		return false
	}
}
