package order

import "testing"

// TestCanTransitionAllowsClosedToPaid 锁定两条关键路径：
// 管理端对已关闭订单人工补单，以及关闭/到账竞态下的回补。
func TestCanTransitionAllowsClosedToPaid(t *testing.T) {
	if !CanTransition(StatusClosed, StatusPaid) {
		t.Fatal("已关闭订单必须允许被确认为已支付")
	}
	if CanTransition(StatusClosed, StatusPending) {
		t.Fatal("已关闭订单不允许退回待支付")
	}
	if CanTransition(StatusPaid, StatusClosed) {
		t.Fatal("已支付订单不允许被关闭")
	}
	if !CanTransition(StatusPending, StatusPaid) {
		t.Fatal("待支付订单必须允许变为已支付")
	}
}
