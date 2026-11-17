package usecase

// 支付宝转账确认单（alipays:// 深度链接）的用例层契约。
//
// 这里要钉住的不是「字符串怎么拼」（那是 domain 的事），而是**取数口径**：
// 金额必须取实收金额（really_price），账号必须来自后台配置，
// 非支付宝订单或未配置账号时必须回空串让收银台退回扫码。

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// alipayAccountSettings 只对 alipayAccount 返回配置值，其余键沿用默认。
type alipayAccountSettings struct {
	account string
}

func (s alipayAccountSettings) Get(_ context.Context, key string) (string, error) {
	if key == setting.AlipayAccountKey {
		return s.account, nil
	}
	return "5", nil
}

func (alipayAccountSettings) GetMany(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (alipayAccountSettings) GetManyForUpdate(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (alipayAccountSettings) Set(context.Context, string, string) error { return nil }

func (alipayAccountSettings) SetMany(context.Context, map[string]string) error { return nil }

var _ port.SettingRepository = alipayAccountSettings{}

// TestAlipayTransferSchemeUsesReallyPrice 锁定金额口径：转账链接里的金额必须是**实收金额**。
//
// 扫码支付会给同价订单做金额微调来区分（1.00 → 1.01），转账必须转微调后的金额，
// 否则到账的钱对不上任何订单。若哪天有人把它"简化"成 PriceCents，
// 用户按 1.00 转账，系统等的是 1.01——钱到了但永远核销不上，且现象是"没收到钱"。
func TestAlipayTransferSchemeUsesReallyPrice(t *testing.T) {
	service := &OrderService{settings: alipayAccountSettings{account: "pay@example.com"}}

	scheme := service.alipayTransferScheme(context.Background(), order.Order{
		Type:             payment.Alipay,
		PriceCents:       100, // 下单金额 1.00
		ReallyPriceCents: 101, // 实收（微调后）1.01
		CreatedAt:        time.Unix(1_700_000_000, 0),
	})
	if scheme == "" {
		t.Fatal("配置齐全时应生成转账链接")
	}

	parsed, err := url.Parse(scheme)
	if err != nil {
		t.Fatalf("转账链接无法解析: %v", err)
	}
	values := parsed.Query()
	if got := values.Get("amount"); got != "1.01" {
		t.Fatalf("金额必须取实收金额 1.01，实际 %q（若为 1.00 说明用了下单金额，核销会失败）", got)
	}
	if got := values.Get("account"); got != "pay@example.com" {
		t.Fatalf("收款账号应来自后台配置，实际 %q", got)
	}
	if got := values.Get("actionType"); got != "toAccount" {
		t.Fatalf("actionType 必须是 toAccount，实际 %q", got)
	}
}

// TestAlipayTransferSchemeFallsBackToQR 锁定两个兜底：非支付宝订单、未配置账号。
// 这两种情况必须回空串——返回半成品链接会让收银台跳到一个空白页。
func TestAlipayTransferSchemeFallsBackToQR(t *testing.T) {
	service := &OrderService{settings: alipayAccountSettings{account: "pay@example.com"}}
	if got := service.alipayTransferScheme(context.Background(), order.Order{
		Type:             payment.Wechat,
		ReallyPriceCents: 101,
	}); got != "" {
		t.Fatalf("非支付宝订单不得生成转账链接，实际 %q", got)
	}

	noAccount := &OrderService{settings: alipayAccountSettings{account: ""}}
	if got := noAccount.alipayTransferScheme(context.Background(), order.Order{
		Type:             payment.Alipay,
		ReallyPriceCents: 101,
	}); got != "" {
		t.Fatalf("未配置收款账号时必须回空串（收银台退回扫码），实际 %q", got)
	}
}
