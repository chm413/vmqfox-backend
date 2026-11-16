package usecase

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// 本文件覆盖「商户自配关单窗口（merchants.close_minutes）必须真正生效」。
//
// 背景（资金级缺陷）：关单阈值原先只读全局 settings.close（默认 5 分钟），
// EPay 商户设成 30 分钟也不生效 —— 5 分钟就关单并释放金额锁，而到账回补窗口
// 只有 10 分钟，第 10–30 分钟到账的付款会落成「无订单转账」，订单永远关闭。

// windowOrderRepo 是「只返回 pending 且 created_at < cutoff」的内存订单表，
// 语义与 mysql 的 FindExpiredPendingForUpdate 对齐（state=pending、按 created_at 升序、limit）。
type windowOrderRepo struct {
	stubOrderRepository
	rows    []order.Order
	queries []time.Time
	closed  []int64
}

func (r *windowOrderRepo) FindExpiredPendingForUpdate(_ context.Context, before time.Time, limit int) ([]order.Order, error) {
	r.queries = append(r.queries, before)
	matched := make([]order.Order, 0, len(r.rows))
	for _, value := range r.rows {
		if value.State != order.StatusPending || !value.CreatedAt.Before(before) {
			continue
		}
		matched = append(matched, value)
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].CreatedAt.Before(matched[j].CreatedAt) })
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

func (r *windowOrderRepo) Transition(_ context.Context, id int64, from, to order.Status, _ time.Time) (bool, error) {
	for index, value := range r.rows {
		if value.ID != id {
			continue
		}
		if value.State != from {
			return false, nil
		}
		r.rows[index].State = to
		r.closed = append(r.closed, id)
		return true, nil
	}
	return false, nil
}

// windowMerchants 只提供 List：关单路径必须一次查出全部商户（禁止逐单查询）。
type windowMerchants struct {
	values []merchant.Merchant
	lists  int
}

func (m *windowMerchants) Create(_ context.Context, value merchant.Merchant) (merchant.Merchant, error) {
	return value, nil
}

func (m *windowMerchants) FindByID(_ context.Context, id int64) (merchant.Merchant, error) {
	for _, value := range m.values {
		if value.ID == id {
			return value, nil
		}
	}
	return merchant.Merchant{}, port.ErrNotFound
}

func (m *windowMerchants) FindByNo(context.Context, string) (merchant.Merchant, error) {
	return merchant.Merchant{}, port.ErrNotFound
}

func (m *windowMerchants) List(context.Context) ([]merchant.Merchant, error) {
	m.lists++
	return m.values, nil
}

func (m *windowMerchants) Update(context.Context, merchant.Merchant) error { return nil }
func (m *windowMerchants) Delete(context.Context, int64) error             { return nil }
func (m *windowMerchants) Count(context.Context) (int64, error)            { return int64(len(m.values)), nil }

var _ port.MerchantRepository = (*windowMerchants)(nil)

var windowNow = time.Unix(1_700_000_000, 0)

func windowService(t *testing.T, repo *windowOrderRepo, merchants *windowMerchants, globalCloseMinutes string) *LifecycleService {
	t.Helper()
	service, err := NewLifecycleService(LifecycleServiceDeps{
		Transactions: &passthroughTransactions{},
		Orders:       repo,
		PriceLocks:   &countingPriceLocks{},
		Settings:     newMapSettings(map[string]string{setting.CloseMinutesKey: globalCloseMinutes}),
		Clock:        fakeClock{now: windowNow},
		Merchants:    merchants,
	})
	if err != nil {
		t.Fatalf("构造生命周期用例失败: %v", err)
	}
	return service
}

func windowOrder(id int64, merchantID int64, protocol merchant.Protocol, age time.Duration) order.Order {
	return order.Order{
		ID:         id,
		MerchantID: merchantID,
		Protocol:   protocol,
		OrderID:    "2026111300000" + string(rune('0'+id)),
		PayID:      "M-ORDER-" + string(rune('0'+id)),
		State:      order.StatusPending,
		CreatedAt:  windowNow.Add(-age),
	}
}

// TestCloseExpiredOrdersHonorsMerchantCloseMinutes 是本次缺陷的主门闩：
// close_minutes=30 的 EPay 订单在 10 分钟时**不得**被关单（否则 10–30 分钟到账的钱
// 会落成无订单转账），而超过 30 分钟的必须照常关闭。
func TestCloseExpiredOrdersHonorsMerchantCloseMinutes(t *testing.T) {
	const merchantID = int64(7)
	repo := &windowOrderRepo{rows: []order.Order{
		windowOrder(1, merchantID, merchant.ProtocolEPayV1, 10*time.Minute), // 30 分钟窗口内
		windowOrder(2, merchantID, merchant.ProtocolEPayV1, 31*time.Minute), // 已超过 30 分钟
	}}
	merchants := &windowMerchants{values: []merchant.Merchant{
		{ID: merchantID, No: "M0001", Enabled: true, CloseMinutes: 30},
	}}

	stats, err := windowService(t, repo, merchants, "5").Maintain(context.Background(), time.Minute, 100)
	if err != nil {
		t.Fatalf("生命周期任务应成功，实际 err=%v", err)
	}
	if stats.ClosedOrders != 1 {
		t.Fatalf("只应关闭超过 30 分钟的那一笔，实际关闭 %d 笔（closed=%v）", stats.ClosedOrders, repo.closed)
	}
	if len(repo.closed) != 1 || repo.closed[0] != 2 {
		t.Fatalf("应只关闭 id=2（31 分钟），实际 %v", repo.closed)
	}
	if repo.rows[0].State != order.StatusPending {
		t.Fatalf("30 分钟窗口内的订单必须保持 pending，实际 %v", repo.rows[0].State)
	}
	// 无 N+1：整轮只查一次商户列表，不随订单数增长。
	if merchants.lists != 1 {
		t.Fatalf("每轮只应查一次商户列表，实际 %d 次", merchants.lists)
	}
}

// TestCloseExpiredOrdersFallsBackToGlobalWindow 覆盖两种「仍按全局 5 分钟」的口径：
// close_minutes=0（跟随全局）与 protocol=0（原生订单不看该字段）。
// 后一条是硬约束：原生订单的行为必须逐字节不变。
func TestCloseExpiredOrdersFallsBackToGlobalWindow(t *testing.T) {
	const (
		epayMerchantID   = int64(8)
		nativeMerchantID = int64(7)
	)
	repo := &windowOrderRepo{rows: []order.Order{
		// close_minutes=0 → 跟随全局 5 分钟，6 分钟就该关。
		windowOrder(1, epayMerchantID, merchant.ProtocolEPayV1, 6*time.Minute),
		// 商户配了 30 分钟，但这笔是原生订单 → 仍按全局 5 分钟关。
		windowOrder(2, nativeMerchantID, merchant.ProtocolNative, 6*time.Minute),
	}}
	merchants := &windowMerchants{values: []merchant.Merchant{
		{ID: nativeMerchantID, No: "M0001", Enabled: true, CloseMinutes: 30},
		{ID: epayMerchantID, No: "M0002", Enabled: true, CloseMinutes: 0},
	}}

	stats, err := windowService(t, repo, merchants, "5").Maintain(context.Background(), time.Minute, 100)
	if err != nil {
		t.Fatalf("生命周期任务应成功，实际 err=%v", err)
	}
	if stats.ClosedOrders != 2 {
		t.Fatalf("两笔都应按全局 5 分钟关闭，实际关闭 %d 笔（closed=%v）", stats.ClosedOrders, repo.closed)
	}
	if len(repo.closed) != 2 || repo.closed[0] != 1 || repo.closed[1] != 2 {
		t.Fatalf("应按 id=1(EPay,close=0)、id=2(原生) 关闭，实际 %v", repo.closed)
	}
}

// TestCloseExpiredOrdersClosesShorterThanGlobalWindow 覆盖第三遍扫描（shortest<global）：
// 全局 5 分钟、商户 close_minutes=3 的 EPay 单在 4 分钟时（已过商户窗口、未到全局窗口）
// 必须被关闭。这是三遍扫描里唯一的短窗口分支——审查终验时全库没有任何
// CloseMinutes<全局 的用例，该分支此前属于「测试死代码」。
func TestCloseExpiredOrdersClosesShorterThanGlobalWindow(t *testing.T) {
	const merchantID = int64(15)
	repo := &windowOrderRepo{rows: []order.Order{
		windowOrder(1, merchantID, merchant.ProtocolEPayV1, 4*time.Minute), // 3<4<5：商户窗口已过、全局未到
	}}
	merchants := &windowMerchants{values: []merchant.Merchant{
		{ID: merchantID, No: "M0003", Enabled: true, CloseMinutes: 3},
	}}

	stats, err := windowService(t, repo, merchants, "5").Maintain(context.Background(), time.Minute, 100)
	if err != nil {
		t.Fatalf("生命周期任务应成功，实际 err=%v", err)
	}
	if stats.ClosedOrders != 1 {
		t.Fatalf("3 分钟窗口的单在 4 分钟时必须被关闭（旧实现会拖到全局 5 分钟），实际关闭 %d 笔（closed=%v）", stats.ClosedOrders, repo.closed)
	}
	if len(repo.closed) != 1 || repo.closed[0] != 1 {
		t.Fatalf("应只关闭 id=1，实际 %v", repo.closed)
	}
}
