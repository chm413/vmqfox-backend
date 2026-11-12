package usecase

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// notifierStub 返回固定响应，用于覆盖通知成功判定与重试分支。
type notifierStub struct {
	result port.NotificationResult
	err    error
	calls  int
}

func (n *notifierStub) Send(context.Context, port.Notification) (port.NotificationResult, error) {
	n.calls++
	return n.result, n.err
}

// outboxRecorder 记录结算动作，便于断言「重试」与「转终态」的切换。
type outboxRecorder struct {
	markFailedCalls   int
	markDeadCalls     int
	markDelivered     int
	lastReason        string
	lastNextAttempt   time.Time
	pendingDeliveries []port.OutboxMessage
}

func (o *outboxRecorder) Enqueue(context.Context, port.OutboxMessage) error { return nil }

func (o *outboxRecorder) ClaimPending(context.Context, time.Time, int, time.Duration) ([]port.OutboxMessage, error) {
	if len(o.pendingDeliveries) == 0 {
		return nil, nil
	}
	message := o.pendingDeliveries[0]
	o.pendingDeliveries = o.pendingDeliveries[1:]
	return []port.OutboxMessage{message}, nil
}

func (o *outboxRecorder) MarkDelivered(context.Context, string, string, time.Time) error {
	o.markDelivered++
	return nil
}

func (o *outboxRecorder) MarkFailed(_ context.Context, _, _ string, _ int, nextAttemptAt time.Time, reason string) error {
	o.markFailedCalls++
	o.lastNextAttempt = nextAttemptAt
	o.lastReason = reason
	return nil
}

func (o *outboxRecorder) MarkDead(_ context.Context, _, _ string, _ int, reason string) error {
	o.markDeadCalls++
	o.lastReason = reason
	return nil
}

func (o *outboxRecorder) PruneDeliveredBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

var _ port.OutboxRepository = (*outboxRecorder)(nil)

// paidOrderRepository 让通知结算路径取到一笔已支付订单。
type paidOrderRepository struct {
	stubOrderRepository
	state order.Status
}

func (r *paidOrderRepository) FindByOrderIDForUpdate(context.Context, string) (order.Order, error) {
	return order.Order{ID: 1, OrderID: "order-1", State: r.state}, nil
}

func (r *paidOrderRepository) Transition(context.Context, int64, order.Status, order.Status, time.Time) (bool, error) {
	return true, nil
}

// TestNotificationSuccessBodyVariants 锁定回调成功判定对常见商户实现的兼容性。
func TestNotificationSuccessBodyVariants(t *testing.T) {
	cases := []struct {
		name   string
		result port.NotificationResult
		err    error
		want   bool
	}{
		{name: "精确 success", result: port.NotificationResult{StatusCode: http.StatusOK, Body: []byte("success")}, want: true},
		{name: "带换行", result: port.NotificationResult{StatusCode: http.StatusOK, Body: []byte("success\n")}, want: true},
		{name: "大小写差异", result: port.NotificationResult{StatusCode: http.StatusOK, Body: []byte("Success")}, want: true},
		{name: "空正文", result: port.NotificationResult{StatusCode: http.StatusNoContent}, want: true},
		{name: "非 success 正文", result: port.NotificationResult{StatusCode: http.StatusOK, Body: []byte("ok")}, want: false},
		{name: "500 状态码", result: port.NotificationResult{StatusCode: http.StatusInternalServerError, Body: []byte("success")}, want: false},
		{name: "网络错误", err: context.DeadlineExceeded, want: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := notificationSucceeded(testCase.result, testCase.err); got != testCase.want {
				t.Fatalf("notificationSucceeded 期望 %v 实际 %v", testCase.want, got)
			}
		})
	}
}

// TestNotificationStopsAtMaxAttempts 锁定超过重试上限后转入终态，不再无限重试。
func TestNotificationStopsAtMaxAttempts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	deadOutbox := &outboxRecorder{}
	deadService, err := NewNotificationService(NotificationServiceDeps{
		Transactions: &passthroughTransactions{},
		Orders:       &paidOrderRepository{state: order.StatusPaid},
		Outbox:       deadOutbox,
		Notifier:     &notifierStub{result: port.NotificationResult{StatusCode: http.StatusInternalServerError}},
		Clock:        fakeClock{now: now},
		MaxAttempts:  3,
	})
	if err != nil {
		t.Fatalf("构造通知用例失败: %v", err)
	}
	deadMessage := port.OutboxMessage{ID: "m1", Topic: notificationTopic, AggregateID: "order-1", Attempts: 2, LeaseToken: "lease"}
	deadOutbox.pendingDeliveries = []port.OutboxMessage{deadMessage}

	stats, err := deadService.DeliverNext(context.Background(), time.Minute)
	if err != nil {
		t.Fatalf("第三次失败不应返回错误，实际 err=%v", err)
	}
	if stats.Failed != 1 || deadOutbox.markDeadCalls != 1 || deadOutbox.markFailedCalls != 0 {
		t.Fatalf("超过上限应转终态：stats=%+v dead=%d failed=%d", stats, deadOutbox.markDeadCalls, deadOutbox.markFailedCalls)
	}

	retryOutbox := &outboxRecorder{}
	retryService, err := NewNotificationService(NotificationServiceDeps{
		Transactions: &passthroughTransactions{},
		Orders:       &paidOrderRepository{state: order.StatusPaid},
		Outbox:       retryOutbox,
		Notifier:     &notifierStub{result: port.NotificationResult{StatusCode: http.StatusInternalServerError}},
		Clock:        fakeClock{now: now},
		MaxAttempts:  3,
	})
	if err != nil {
		t.Fatalf("构造通知用例失败: %v", err)
	}
	retryMessage := port.OutboxMessage{ID: "m2", Topic: notificationTopic, AggregateID: "order-1", Attempts: 0, LeaseToken: "lease"}
	retryOutbox.pendingDeliveries = []port.OutboxMessage{retryMessage}

	if _, err := retryService.DeliverNext(context.Background(), time.Minute); err != nil {
		t.Fatalf("首次失败应安排重试，实际 err=%v", err)
	}
	if retryOutbox.markFailedCalls != 1 || retryOutbox.markDeadCalls != 0 {
		t.Fatalf("未达上限应安排重试：dead=%d failed=%d", retryOutbox.markDeadCalls, retryOutbox.markFailedCalls)
	}
	if !retryOutbox.lastNextAttempt.After(now) {
		t.Fatalf("重试时间应晚于当前时间，实际 %v", retryOutbox.lastNextAttempt)
	}
}
