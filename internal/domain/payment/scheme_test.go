package payment

import (
	"net/url"
	"strings"
	"testing"
)

// TestAlipayTransferSchemeBuildsDeepLink 锁定转账确认单链接的必需参数与预填金额。
func TestAlipayTransferSchemeBuildsDeepLink(t *testing.T) {
	got := AlipayTransferScheme("13800138000", "12.34")
	if got == "" {
		t.Fatal("配置齐全时应生成链接")
	}
	if !strings.HasPrefix(got, "alipays://platformapi/startapp?") {
		t.Fatalf("scheme 前缀不符: %q", got)
	}
	query, err := url.Parse(got)
	if err != nil {
		t.Fatalf("链接无法解析: %v", err)
	}
	values := query.Query()
	cases := map[string]string{
		"appId":      "20000116",
		"actionType": "toAccount",
		"account":    "13800138000",
		"amount":     "12.34",
	}
	for key, want := range cases {
		if got := values.Get(key); got != want {
			t.Fatalf("参数 %s 期望 %q，实际 %q", key, want, got)
		}
	}
}

// TestAlipayTransferSchemeTrimsInput 锁定首尾空格被清除，避免生成带空白的账号。
func TestAlipayTransferSchemeTrimsInput(t *testing.T) {
	got := AlipayTransferScheme("  13800138000  ", " 0.01 ")
	query, err := url.Parse(got)
	if err != nil {
		t.Fatalf("链接无法解析: %v", err)
	}
	values := query.Query()
	if values.Get("account") != "13800138000" || values.Get("amount") != "0.01" {
		t.Fatalf("空格未清除: account=%q amount=%q", values.Get("account"), values.Get("amount"))
	}
}

// TestAlipayTransferSchemeEncodesAccount 锁定邮箱账号中的 @ 被正确转义。
func TestAlipayTransferSchemeEncodesAccount(t *testing.T) {
	got := AlipayTransferScheme("pay@example.com", "1.00")
	if strings.Contains(got, "pay@example.com") {
		t.Fatalf("@ 应被百分号编码，实际 %q", got)
	}
	query, err := url.Parse(got)
	if err != nil {
		t.Fatalf("链接无法解析: %v", err)
	}
	if account := query.Query().Get("account"); account != "pay@example.com" {
		t.Fatalf("解析后账号应为原文，实际 %q", account)
	}
}

// TestAlipayTransferSchemeEmptyInputs 锁定缺配置时返回空串，调用方据此退回扫码。
func TestAlipayTransferSchemeEmptyInputs(t *testing.T) {
	for _, tc := range []struct{ name, account, amount string }{
		{"账号为空", "", "12.34"},
		{"金额为空", "13800138000", ""},
		{"账号为空白", "   ", "12.34"},
		{"两者皆空", "", ""},
	} {
		if got := AlipayTransferScheme(tc.account, tc.amount); got != "" {
			t.Fatalf("%s：应为空串，实际 %q", tc.name, got)
		}
	}
}
