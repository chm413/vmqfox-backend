package mysql

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
	"github.com/hulisang/vmqfox-backend/internal/port"
	"gorm.io/gorm"
)

// 本文件用真实 MySQL 验证「pay_id 的唯一性已收敛到商户内」以及 merchant_id 的往返一致性。
//
// 为什么必须连库跑：这两条都不是纯逻辑问题，而是表结构 + 索引 + 映射三者叠加的结果。
// 只改 OrderRow 的 tag、漏改 orderToRow、或漏换索引，单测全绿但线上一定出事故
// （表现为跨商户串单、或两个商户撞单号被拒）。未设置 VMQ_TEST_MYSQL_DSN 时跳过。

// ordersSchemaDDL 直接取自嵌入的 schema.sql，保证测试验证的是「新装库」的那份结构，
// 而不是测试里另抄一份可能过时的建表语句。
func ordersSchemaDDL(t *testing.T) string {
	t.Helper()
	for _, statement := range splitSQLStatements(schemaSQL) {
		if strings.HasPrefix(strings.TrimSpace(statement), "CREATE TABLE IF NOT EXISTS orders") {
			return statement
		}
	}
	t.Fatal("schema.sql 里找不到 orders 建表语句")
	return ""
}

// resetOrdersTable 让每个用例从「新装库」的 orders 结构开始。
func resetOrdersTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec("DROP TABLE IF EXISTS orders").Error; err != nil {
		t.Fatalf("清理 orders 表失败: %v", err)
	}
	if err := db.Exec(ordersSchemaDDL(t)).Error; err != nil {
		t.Fatalf("按 schema.sql 重建 orders 表失败: %v", err)
	}
}

func newMerchantOrder(merchantID int64, payID, token string) order.Order {
	return order.Order{
		MerchantID:        merchantID,
		Protocol:          merchant.ProtocolNative,
		OrderID:           "OID-" + token[:8],
		PublicToken:       token,
		PayID:             payID,
		Type:              payment.Alipay,
		EpayType:          "alipay",
		MerchantOrderName: "测试商品",
		PriceCents:        100,
		ReallyPriceCents:  100,
		PriceText:         "1.00",
		ReallyPriceText:   "1.00",
		State:             order.StatusPending,
		CreatedAt:         time.Now().UTC().Truncate(time.Microsecond),
	}
}

// TestOrdersMigrationStatements 不连库，只钉住两条 DDL 的形态：
// 加列必须是一条 ALTER（拆开会重建 N 次表），索引切换必须是一条 ALTER
// （拆开会出现「无唯一约束」的窗口）。CI 里没有 MySQL 时，这条是 002/003 唯一的可见覆盖。
func TestOrdersMigrationStatements(t *testing.T) {
	addColumns := addColumnsStatement(ordersMerchantColumns)
	wantAdd := "ALTER TABLE orders " +
		"ADD COLUMN merchant_id BIGINT NOT NULL DEFAULT 0 AFTER id, " +
		"ADD COLUMN protocol TINYINT NOT NULL DEFAULT 0 AFTER merchant_id, " +
		"ADD COLUMN epay_type VARCHAR(16) NOT NULL DEFAULT '' AFTER protocol, " +
		"ADD COLUMN merchant_order_name VARCHAR(128) NOT NULL DEFAULT '' AFTER epay_type"
	if addColumns != wantAdd {
		t.Fatalf("002 加列语句不符合预期:\n got: %s\nwant: %s", addColumns, wantAdd)
	}

	switchIndex := switchIndexStatement(true)
	wantSwitch := "ALTER TABLE orders DROP INDEX uq_orders_pay_id, " +
		"ADD UNIQUE KEY uq_orders_merchant_pay_id (merchant_id, pay_id), " +
		"ADD KEY idx_orders_merchant_state (merchant_id, state, created_at)"
	if switchIndex != wantSwitch {
		t.Fatalf("003 索引切换语句不符合预期:\n got: %s\nwant: %s", switchIndex, wantSwitch)
	}
}

// TestOrdersSchemaDDLShape 守住「新装库」那一份结构（不连库）。
//
// 两份 schema.sql 必须同步改：internal/adapter/mysql/schema.sql 是 -init-db 真正执行的嵌入副本，
// database/schema.sql 是容器首次初始化的副本。只改一份的后果是新装库缺列，
// 而这类问题在编译期与单元测试里都不会出现。
func TestOrdersSchemaDDLShape(t *testing.T) {
	embedded := ordersSchemaDDL(t)
	reference, err := os.ReadFile("../../../database/schema.sql")
	if err != nil {
		t.Fatalf("读取 database/schema.sql 失败: %v", err)
	}

	expected := []string{
		"merchant_id BIGINT NOT NULL DEFAULT 0",
		"protocol TINYINT NOT NULL DEFAULT 0",
		"epay_type VARCHAR(16) NOT NULL DEFAULT ''",
		"merchant_order_name VARCHAR(128) NOT NULL DEFAULT ''",
		"UNIQUE KEY uq_orders_merchant_pay_id (merchant_id, pay_id)",
		"KEY idx_orders_merchant_state (merchant_id, state, created_at)",
	}
	cases := map[string]string{
		"internal/adapter/mysql/schema.sql": embedded,
		"database/schema.sql":               string(reference),
	}
	for name, content := range cases {
		for _, want := range expected {
			if !strings.Contains(content, want) {
				t.Errorf("%s 缺少 %q", name, want)
			}
		}
		if strings.Contains(content, "uq_orders_pay_id (pay_id)") {
			t.Errorf("%s 仍保留全局唯一索引 uq_orders_pay_id，多商户会被它挡住", name)
		}
	}
}

// TestOrderPayIDUniquePerMerchant 覆盖「不同商户可以撞同一个 out_trade_no」。
// 这是多商户的前提：EPay 的 out_trade_no 只保证商户内唯一。
func TestOrderPayIDUniquePerMerchant(t *testing.T) {
	db := openTestDatabase(t)
	resetOrdersTable(t, db)
	repository := NewOrderRepository(db)
	ctx := context.Background()

	const payID = "EPAY-OUT-TRADE-NO-1"
	for index, merchantID := range []int64{0, 1, 2} {
		created, err := repository.Create(ctx, newMerchantOrder(merchantID, payID, hexToken(1000+index)))
		if err != nil {
			t.Fatalf("商户 %d 使用 pay_id=%s 建单应成功，实际 err=%v", merchantID, payID, err)
		}
		if created.MerchantID != merchantID {
			t.Fatalf("建单返回的 MerchantID 应为 %d，实际 %d", merchantID, created.MerchantID)
		}
	}
}

// TestOrderPayIDDuplicateWithinMerchant 覆盖「同一商户重复单号仍然被唯一索引挡住」。
func TestOrderPayIDDuplicateWithinMerchant(t *testing.T) {
	db := openTestDatabase(t)
	resetOrdersTable(t, db)
	repository := NewOrderRepository(db)
	ctx := context.Background()

	const payID = "EPAY-OUT-TRADE-NO-2"
	if _, err := repository.Create(ctx, newMerchantOrder(1, payID, hexToken(2001))); err != nil {
		t.Fatalf("首单应成功，实际 err=%v", err)
	}

	// 公开令牌必须不同，否则会先命中 uq_orders_public_token，测不到 pay_id 的唯一性。
	_, err := repository.Create(ctx, newMerchantOrder(1, payID, hexToken(2002)))
	if !errors.Is(err, port.ErrConflict) {
		t.Fatalf("同商户重复 pay_id 应返回 ErrConflict，实际 err=%v", err)
	}
}

// TestFindByPayIDScopedToMerchant 覆盖查询维度：带错商户必须返回 NotFound，
// 而不是把别的商户的订单捞出来（串单是资金级事故）。
func TestFindByPayIDScopedToMerchant(t *testing.T) {
	db := openTestDatabase(t)
	resetOrdersTable(t, db)
	repository := NewOrderRepository(db)
	ctx := context.Background()

	const payID = "EPAY-OUT-TRADE-NO-3"
	if _, err := repository.Create(ctx, newMerchantOrder(1, payID, hexToken(3001))); err != nil {
		t.Fatalf("建单应成功，实际 err=%v", err)
	}

	found, err := repository.FindByPayID(ctx, 1, payID)
	if err != nil {
		t.Fatalf("同一商户应查到订单，实际 err=%v", err)
	}
	if found.MerchantID != 1 || found.PayID != payID {
		t.Fatalf("查到的订单维度不对: merchant_id=%d pay_id=%s", found.MerchantID, found.PayID)
	}

	for _, wrongMerchantID := range []int64{0, 2} {
		if _, err := repository.FindByPayIDForUpdate(ctx, wrongMerchantID, payID); !errors.Is(err, port.ErrNotFound) {
			t.Fatalf("商户 %d 查 pay_id=%s 应返回 ErrNotFound，实际 err=%v", wrongMerchantID, payID, err)
		}
	}
}

// TestOrderMerchantFieldsRoundTrip 覆盖「4 个新列真的写进库、真的读回来」。
// 漏改 orderToRow 时编译与其它用例都不会报错，这条是唯一的兜底。
func TestOrderMerchantFieldsRoundTrip(t *testing.T) {
	db := openTestDatabase(t)
	resetOrdersTable(t, db)
	repository := NewOrderRepository(db)
	ctx := context.Background()

	want := newMerchantOrder(7, "EPAY-OUT-TRADE-NO-4", hexToken(4001))
	want.Protocol = merchant.ProtocolEPayV2
	want.EpayType = "wxpay"
	want.MerchantOrderName = "季度会员"

	created, err := repository.Create(ctx, want)
	if err != nil {
		t.Fatalf("建单应成功，实际 err=%v", err)
	}

	byID, err := repository.FindByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("按 id 查询应成功，实际 err=%v", err)
	}
	byPayID, err := repository.FindByPayID(ctx, want.MerchantID, want.PayID)
	if err != nil {
		t.Fatalf("按商户+pay_id 查询应成功，实际 err=%v", err)
	}

	for _, got := range []order.Order{created, byID, byPayID} {
		if got.MerchantID != want.MerchantID ||
			got.Protocol != want.Protocol ||
			got.EpayType != want.EpayType ||
			got.MerchantOrderName != want.MerchantOrderName {
			t.Fatalf("商户维度字段往返不一致: got=%+v want merchant_id=%d protocol=%d epay_type=%s name=%s",
				got, want.MerchantID, want.Protocol, want.EpayType, want.MerchantOrderName)
		}
	}

	// 再直查一次原始列：结构体映射可能被 gorm 的零值规则掩盖，库里的值才是事实。
	var raw struct {
		MerchantID        int64  `gorm:"column:merchant_id"`
		Protocol          int8   `gorm:"column:protocol"`
		EpayType          string `gorm:"column:epay_type"`
		MerchantOrderName string `gorm:"column:merchant_order_name"`
	}
	if err := db.Raw("SELECT merchant_id, protocol, epay_type, merchant_order_name FROM orders WHERE id = ?", created.ID).
		Scan(&raw).Error; err != nil {
		t.Fatalf("直查 orders 新列失败: %v", err)
	}
	if raw.MerchantID != 7 || raw.Protocol != int8(merchant.ProtocolEPayV2) ||
		raw.EpayType != "wxpay" || raw.MerchantOrderName != "季度会员" {
		t.Fatalf("orders 表里的新列与预期不符: %+v", raw)
	}
}
