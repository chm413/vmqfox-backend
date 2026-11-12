package usecase

import (
	"context"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// countingPriceLocks 统计批量释放路径的调用次数，用于锁定「不再按单条订单循环释锁」。
type countingPriceLocks struct {
	grantingPriceLocks
	releaseByOrdersBefore int
	releaseOrphaned       int
	releasedByOrderIDs    []string
}

func (c *countingPriceLocks) ReleaseByOrdersBefore(context.Context, time.Time) error {
	c.releaseByOrdersBefore++
	return nil
}

func (c *countingPriceLocks) ReleaseOrphaned(context.Context) error {
	c.releaseOrphaned++
	return nil
}

func (c *countingPriceLocks) ReleaseByOrderID(_ context.Context, orderID string) error {
	c.releasedByOrderIDs = append(c.releasedByOrderIDs, orderID)
	return nil
}

var _ port.PriceLockRepository = (*countingPriceLocks)(nil)

// bulkExpiredOrders 模拟一次清理命中大量过期订单。
type bulkExpiredOrders struct {
	stubOrderRepository
	deleted int64
}

func (r *bulkExpiredOrders) DeleteExpiredBefore(context.Context, time.Time) (int64, error) {
	return r.deleted, nil
}

// TestDeleteExpiredReleasesLocksForEveryExpiredOrder 锁定过期订单清理不再受批量上限影响：
// 旧实现只给前 500 笔订单释锁，却删除全部过期订单，超出部分会留下永久占死的金额锁。
func TestDeleteExpiredReleasesLocksForEveryExpiredOrder(t *testing.T) {
	locks := &countingPriceLocks{}
	orders := &bulkExpiredOrders{deleted: 1200}
	service, err := NewOrderService(OrderServiceDeps{
		Transactions: &passthroughTransactions{},
		Orders:       orders,
		PriceLocks:   locks,
		QRCodes:      emptyQRCodes{},
		Settings:     newMapSettings(map[string]string{setting.CloseMinutesKey: "5"}),
		Outbox:       &recordingOutbox{},
		Clock:        fakeClock{now: time.Unix(1_700_000_000, 0)},
		OrderIDs:     fakeOrderIDs{value: "2026082500000000001"},
		PublicTokens: &scriptedTokens{tokens: []string{testToken(21)}},
		FrontendURL:  "https://pay.example.test",
	})
	if err != nil {
		t.Fatalf("构造订单用例失败: %v", err)
	}

	count, err := service.DeleteExpired(context.Background())
	if err != nil {
		t.Fatalf("清理过期订单应成功，实际 err=%v", err)
	}
	if count != 1200 {
		t.Fatalf("应返回实际删除数量 1200，实际 %d", count)
	}
	if locks.releaseByOrdersBefore != 1 {
		t.Fatalf("释放应以订单为条件批量执行一次，实际 %d 次", locks.releaseByOrdersBefore)
	}
	if len(locks.releasedByOrderIDs) != 0 {
		t.Fatalf("不应再逐条释放价格锁，实际释放 %d 条", len(locks.releasedByOrderIDs))
	}
	if locks.releaseOrphaned != 1 {
		t.Fatalf("删除后应执行一次孤儿锁清理，实际 %d 次", locks.releaseOrphaned)
	}
}
