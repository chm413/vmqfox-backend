package mysql

// 资金级不变量的门禁。这两条 SQL 条件在 2026-11-15 审查后守卫性收敛：
//
//	DeleteExpiredBefore    只删 closed   —— pending 单仍在支付窗口内（EPay 商户可 30 分钟）
//	ReleaseByOrdersBefore  只放已离开 pending 的锁 —— 提前放锁 = 同一金额卖两次
//
// 收敛当日全库没有一条用例触达这两段 SQL（变异 A/B 全绿），它们只有注释在守。
// 这两条用例补上 DSN 门控的真库覆盖：CI（mysql:5.7）必跑，本地按 SKIP。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/port"
	"gorm.io/gorm"
)

// TestExpiredCleanupKeepsPendingAndDropsClosed 覆盖 DeleteExpiredBefore：
// 超过全局窗口的 pending 单必须保留（EPay 商户窗口可能 30 分钟，全局 cutoff 只是取批阈值），
// 同龄的 closed 单必须被清理。
func TestExpiredCleanupKeepsPendingAndDropsClosed(t *testing.T) {
	db := openTestDatabase(t)
	resetOrdersTable(t, db)
	repository := NewOrderRepository(db)
	ctx := context.Background()

	pending := newMerchantOrder(11, "EPAY-CLEANUP-PENDING", hexToken(6001))
	pending.CreatedAt = time.Now().UTC().Add(-10 * time.Minute)
	if _, err := repository.Create(ctx, pending); err != nil {
		t.Fatalf("pending 夹具应建单成功，实际 err=%v", err)
	}
	closed := newMerchantOrder(11, "EPAY-CLEANUP-CLOSED", hexToken(6002))
	closed.CreatedAt = time.Now().UTC().Add(-10 * time.Minute)
	closed.State = order.StatusClosed
	if _, err := repository.Create(ctx, closed); err != nil {
		t.Fatalf("closed 夹具应建单成功，实际 err=%v", err)
	}

	deleted, err := repository.DeleteExpiredBefore(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("清理应成功，实际 err=%v", err)
	}
	if deleted != 1 {
		t.Fatalf("只应删除 1 行（closed），实际 %d —— 若把 pending 也删了即资金级缺陷", deleted)
	}

	if _, err := repository.FindByPayID(ctx, 11, "EPAY-CLEANUP-PENDING"); err != nil {
		t.Fatalf("pending 单即使超过全局窗口也必须保留（商户窗口可能 30 分钟），实际 err=%v", err)
	}
	if _, err := repository.FindByPayID(ctx, 11, "EPAY-CLEANUP-CLOSED"); !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("已关闭的历史单应被清理，实际仍可查到: %v", err)
	}
}

// resetPriceLocksTable 重建 price_locks（其余用例不碰这张表，vmq_test 里默认不存在）。
// DDL 与 database/schema.sql 保持一致；主键 (payment_type, amount_cent) 就是金额锁本身。
func resetPriceLocksTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec("DROP TABLE IF EXISTS price_locks").Error; err != nil {
		t.Fatalf("清理 price_locks 表失败: %v", err)
	}
	if err := db.Exec("CREATE TABLE price_locks (" +
		"payment_type TINYINT NOT NULL," +
		"amount_cent BIGINT NOT NULL," +
		"order_id VARBINARY(128) NOT NULL," +
		"created_at DATETIME(6) NOT NULL," +
		"PRIMARY KEY (payment_type, amount_cent)," +
		"UNIQUE KEY uq_price_locks_order_id (order_id)" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci").Error; err != nil {
		t.Fatalf("重建 price_locks 表失败: %v", err)
	}
}

// TestExpiredLockReleaseSkipsPendingOrders 覆盖 ReleaseByOrdersBefore：
// 超过全局窗口的 pending 单的金额锁必须保留（提前放锁 = 同一金额卖两次），
// 同龄 closed 单的锁必须被释放（不再被该金额占死）。
func TestExpiredLockReleaseSkipsPendingOrders(t *testing.T) {
	db := openTestDatabase(t)
	resetOrdersTable(t, db)
	resetPriceLocksTable(t, db)
	orderRepo := NewOrderRepository(db)
	lockRepo := NewPriceLockRepository(db)
	ctx := context.Background()

	// 两笔用不同金额：金额锁的唯一键是 (payment_type, amount_cents)。
	pending := newMerchantOrder(12, "EPAY-LOCK-PENDING", hexToken(6101))
	pending.CreatedAt = time.Now().UTC().Add(-10 * time.Minute)
	if _, err := orderRepo.Create(ctx, pending); err != nil {
		t.Fatalf("pending 夹具应建单成功，实际 err=%v", err)
	}
	closed := newMerchantOrder(12, "EPAY-LOCK-CLOSED", hexToken(6102))
	closed.CreatedAt = time.Now().UTC().Add(-10 * time.Minute)
	closed.State = order.StatusClosed
	closed.PriceCents = 200
	closed.ReallyPriceCents = 200
	closed.PriceText = "2.00"
	closed.ReallyPriceText = "2.00"
	if _, err := orderRepo.Create(ctx, closed); err != nil {
		t.Fatalf("closed 夹具应建单成功，实际 err=%v", err)
	}

	for _, candidate := range []order.Order{pending, closed} {
		ok, err := lockRepo.TryAcquire(ctx, candidate.OrderID, candidate.Type, candidate.PriceCents)
		if err != nil {
			t.Fatalf("占锁应成功（%s）: %v", candidate.OrderID, err)
		}
		if !ok {
			t.Fatalf("占锁应成功（首占，%s）", candidate.OrderID)
		}
	}

	if err := lockRepo.ReleaseByOrdersBefore(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("批量释放应成功，实际 err=%v", err)
	}

	got, err := lockRepo.TryAcquire(ctx, pending.OrderID, pending.Type, pending.PriceCents)
	if err != nil {
		t.Fatalf("pending 的锁重占应只产生唯一键冲突，实际 err=%v", err)
	}
	if got {
		t.Fatal("pending 单的锁必须保留——提前放锁等于同一金额可以被二次占用")
	}

	got, err = lockRepo.TryAcquire(ctx, closed.OrderID, closed.Type, closed.PriceCents)
	if err != nil {
		t.Fatalf("closed 的锁释放后重占应成功，实际 err=%v", err)
	}
	if !got {
		t.Fatal("closed 单的锁应已被批量释放，该金额不应被永久占死")
	}
}
