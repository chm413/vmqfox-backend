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
