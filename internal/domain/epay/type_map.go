package epay

import "github.com/hulisang/vmqfox-backend/internal/domain/payment"

// TypeFromEPay 把 EPay 的 type 字符串映射到内部支付方式。
//
// 未开通的渠道（unionpay、bank、qqpay 等）一律返回 false，由 handler 回
// 「支付方式未开通」。不允许静默回落到微信/支付宝：挂机端只能监听微信与
// 支付宝通知栏，回落会让用户付了钱但类型不匹配，到账无法核销（设计 §5.2）。
// 精确匹配且区分大小写 —— EPay 生态传的就是这两个小写单词，把 "Alipay"
// 也放行只会让「同一份订单两种 type 写法」在日志与回调里各说各话。
func TypeFromEPay(value string) (payment.Type, bool) {
	switch value {
	case "alipay":
		return payment.Alipay, true
	case "wxpay":
		return payment.Wechat, true
	default:
		return 0, false
	}
}

// EPayTypeFrom 反向映射，用于回调与查单响应原样回显 type。
func EPayTypeFrom(value payment.Type) (string, bool) {
	switch value {
	case payment.Alipay:
		return "alipay", true
	case payment.Wechat:
		return "wxpay", true
	default:
		return "", false
	}
}
