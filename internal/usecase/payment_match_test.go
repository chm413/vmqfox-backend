package usecase

import (
	"context"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// matchOrders 记录匹配条件并模拟「创建时间过滤」与「关闭回补」两条查询路径。
type matchOrders struct {
	stubOrderRepository
	pending    *order.Order
	reopenable *order.Order

	createdBeforeSeen time.Time
	closedAfterSeen   time.Time
	transitions       []transitionCall
	created           []order.Order
}

type transitionCall struct {
	from order.Status
	to   order.Status
}

func (r *matchOrders) FindPendingForUpdate(_ context.Context, _ payment.Type, _ int64, createdBefore time.Time) (order.Order, error) {
	r.createdBeforeSeen = createdBefore
	if r.pending != nil && !r.pending.CreatedAt.After(createdBefore) {
		return *r.pending, nil
	}
	return order.Order{}, port.ErrNotFound
}

func (r *matchOrders) FindReopenableForUpdate(_ context.Context, _ payment.Type, _ int64, createdBefore, closedAfter time.Time) (order.Order, error) {
	r.createdBeforeSeen = createdBefore
	r.closedAfterSeen = closedAfter
	if r.reopenable != nil && !r.reopenable.CreatedAt.After(createdBefore) {
		return *r.reopenable, nil
	}
	return order.Order{}, port.ErrNotFound
}

func (r *matchOrders) Transition(_ context.Context, _ int64, from, to order.Status, _ time.Time) (bool, error) {
	r.transitions = append(r.transitions, transitionCall{from: from, to: to})
	return true, nil
}

func (r *matchOrders) Create(_ context.Context, value order.Order) (order.Order, error) {
	r.created = append(r.created, value)
	return value, nil
}

var _ port.OrderRepository = (*matchOrders)(nil)

// duplicateEvents 模拟同一事件的重复推送。
type duplicateEvents struct{}

func (duplicateEvents) RecordIfNew(context.Context, port.PaymentEvent) (bool, error) {
	return false, nil
}

func (duplicateEvents) PruneBefore(context.Context, time.Time) (int64, error) { return 0, nil }

func newMatchService(t *testing.T, orders *matchOrders, events port.PaymentEventRepository, now time.Time, tokens *scriptedTokens) (*MonitorService, *recordingOutbox) {
	t.Helper()
	outbox := &recordingOutbox{}
	service, err := NewMonitorService(MonitorServiceDeps{
		Transactions: &passthroughTransactions{},
		Orders:       orders,
		PriceLocks:   grantingPriceLocks{},
		Settings:     newMapSettings(map[string]string{setting.MerchantKey: testMerchantKey}),
		Events:       events,
		Outbox:       outbox,
		Clock:        fakeClock{now: now},
		PublicTokens: tokens,
		SignTTL:      5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("构造监控用例失败: %v", err)
	}
	return service, outbox
}

// TestPushOnlyMatchesOrdersCreatedBeforePayment 锁定「付款之后创建的订单不可能被这笔付款结算」。
func TestPushOnlyMatchesOrdersCreatedBeforePayment(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	later := order.Order{
		ID: 2, OrderID: "later", PayID: "later", Type: payment.Alipay,
		ReallyPriceCents: 321, State: order.StatusPending,
		NotifyURL: "https://merchant.test/notify",
		CreatedAt: now.Add(2 * time.Minute), // 付款之后才创建
	}
	orders := &matchOrders{pending: &later}
	service, _ := newMatchService(t, orders, newEventsRepository{}, now, &scriptedTokens{tokens: []string{testToken(11)}})

	result, err := service.Push(context.Background(), pushInput(now, testMerchantKey))
	if err != nil {
		t.Fatalf("推送应成功，实际 err=%v", err)
	}
	if result.Matched {
		t.Fatalf("付款之后创建的订单不应被这笔付款结算")
	}
	if len(orders.transitions) != 0 {
		t.Fatalf("不应发生任何状态转换，实际 %+v", orders.transitions)
	}
	if len(orders.created) != 1 || orders.created[0].State != order.StatusPaid {
		t.Fatalf("应退化为一条已支付的无订单转账，实际 %+v", orders.created)
	}

	earlier := later
	earlier.CreatedAt = now.Add(-time.Minute)
	orders = &matchOrders{pending: &earlier}
	service, outbox := newMatchService(t, orders, newEventsRepository{}, now, &scriptedTokens{tokens: []string{testToken(12)}})

	result, err = service.Push(context.Background(), pushInput(now, testMerchantKey))
	if err != nil {
		t.Fatalf("推送应成功，实际 err=%v", err)
	}
	if !result.Matched {
		t.Fatalf("付款之前创建的待支付订单应被结算")
	}
	if len(orders.transitions) != 1 || orders.transitions[0].from != order.StatusPending || orders.transitions[0].to != order.StatusPaid {
		t.Fatalf("状态转换应为 Pending→Paid，实际 %+v", orders.transitions)
	}
	if len(outbox.messages) != 1 {
		t.Fatalf("结算后应入队一条商户通知，实际 %d 条", len(outbox.messages))
	}
}

// TestPushReopensOrderClosedByExpiryRace 锁定关闭/到账竞态：刚被关闭的订单应回补为已支付，而不是记成无订单转账。
func TestPushReopensOrderClosedByExpiryRace(t *testing.T) {
	now := time.Unix(1_700_000_500, 0)
	closed := order.Order{
		ID: 3, OrderID: "closed", PayID: "closed", Type: payment.Alipay,
		ReallyPriceCents: 321, State: order.StatusClosed,
		NotifyURL: "https://merchant.test/notify",
		CreatedAt: now.Add(-5 * time.Minute),
	}
	orders := &matchOrders{reopenable: &closed}
	service, outbox := newMatchService(t, orders, newEventsRepository{}, now, &scriptedTokens{tokens: []string{testToken(13)}})

	result, err := service.Push(context.Background(), pushInput(now, testMerchantKey))
	if err != nil {
		t.Fatalf("推送应成功，实际 err=%v", err)
	}
	if !result.Matched || result.Order.OrderID != "closed" {
		t.Fatalf("应回补已关闭订单，实际 %+v", result)
	}
	if len(orders.transitions) != 1 || orders.transitions[0].from != order.StatusClosed || orders.transitions[0].to != order.StatusPaid {
		t.Fatalf("回补时应执行 Closed→Paid，实际 %+v", orders.transitions)
	}
	if len(outbox.messages) != 1 {
		t.Fatalf("回补后应入队商户通知，实际 %d 条", len(outbox.messages))
	}
	if len(orders.created) != 0 {
		t.Fatalf("回补路径不应创建无订单转账，实际 %+v", orders.created)
	}
}

// TestPushIgnoresRepeatedEvent 锁定重复事件只更新最后到账时间，不再参与订单匹配。
func TestPushIgnoresRepeatedEvent(t *testing.T) {
	now := time.Unix(1_700_000_900, 0)
	pending := order.Order{
		ID: 4, OrderID: "pending", PayID: "pending", Type: payment.Alipay,
		ReallyPriceCents: 321, State: order.StatusPending,
		CreatedAt: now.Add(-time.Minute),
	}
	orders := &matchOrders{pending: &pending}
	service, _ := newMatchService(t, orders, duplicateEvents{}, now, &scriptedTokens{tokens: []string{testToken(14)}})

	result, err := service.Push(context.Background(), pushInput(now, testMerchantKey))
	if err != nil {
		t.Fatalf("重复推送不应报错，实际 err=%v", err)
	}
	if !result.Duplicate || result.Matched {
		t.Fatalf("重复事件应只标记 Duplicate，实际 %+v", result)
	}
	if len(orders.transitions) != 0 || len(orders.created) != 0 {
		t.Fatalf("重复事件不应产生状态变更或新订单")
	}
}
