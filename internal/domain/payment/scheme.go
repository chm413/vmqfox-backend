package payment

import (
	"net/url"
	"strings"
)

// alipayTransferAppID 是支付宝「转账」应用在 scheme 中的 appId。
// 打开后进入转账确认单界面，收款账号与金额均已预填，用户只需确认。
const alipayTransferAppID = "20000116"

// AlipayTransferScheme 生成支付宝转账确认单的深度链接。
//
// account 为收款方支付宝账号（手机号或邮箱），amount 为两位小数元金额。
// 任一为空则返回空串，调用方应退回二维码扫码，不要生成半成品链接。
//
// 说明：scheme 的参数集合不是支付宝公开契约，随客户端版本可能变化。
// 这里只使用长期稳定的 appId + actionType + account + amount 四个必要参数，
// 不附加 goBack 等可选参数，减少因版本差异导致落地页空白的面。
// 若后续要支持「备注」，在 amount 之外追加 memo 参数即可，无需改动调用方。
func AlipayTransferScheme(account, amount string) string {
	account = strings.TrimSpace(account)
	amount = strings.TrimSpace(amount)
	if account == "" || amount == "" {
		return ""
	}
	query := url.Values{}
	query.Set("appId", alipayTransferAppID)
	query.Set("actionType", "toAccount")
	query.Set("account", account)
	query.Set("amount", amount)
	return "alipays://platformapi/startapp?" + query.Encode()
}
