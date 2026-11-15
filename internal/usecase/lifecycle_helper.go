package usecase

import (
	"context"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

const batchExpireSize = 100

// loadMerchantCloseWindows 读出「商户自配的关单窗口」：merchant_id → close_minutes。
//
// 只返回 CloseMinutes>0 的商户：0 表示跟随全局 settings.close，不必建条目。
// 整个批次只查一次 merchants.List，**不随订单数增长**（无 N+1）：
// 单次关单最多三批查询，商户数量级是「几十」，整表读入的成本远低于逐单查库。
// 读不到商户时不返回空表冒充「全部跟随全局」——调用方必须让这一轮关单失败，
// 否则长窗口商户会被按 5 分钟关掉（金额锁提前释放，10–30 分钟到账只能落无主转账）。
func loadMerchantCloseWindows(ctx context.Context, merchants port.MerchantRepository) (map[int64]int, error) {
	if merchants == nil {
		// 未装配商户仓储（只跑原生的部署）：全部跟随全局，保持旧行为。
		return nil, nil
	}
	list, err := merchants.List(ctx)
	if err != nil {
		return nil, err
	}
	windows := make(map[int64]int, len(list))
	for _, value := range list {
		if value.CloseMinutes > 0 {
			windows[value.ID] = value.CloseMinutes
		}
	}
	return windows, nil
}

// closeMinutesFor 返回该订单的有效关单窗口（分钟）。
//
// 取值规则（设计 §5.4 / 商户表 close_minutes）：
//   - protocol=0（原生订单）：恒用全局 settings.close，**行为逐字节不变**；
//   - protocol!=0（EPay 订单）：商户 close_minutes>0 时用它，0（或缺条目）跟随全局。
func closeMinutesFor(value order.Order, globalMinutes int, windows map[int64]int) int {
	if value.Protocol == merchant.ProtocolNative {
		return globalMinutes
	}
	if minutes, ok := windows[value.MerchantID]; ok && minutes > 0 {
		return minutes
	}
	return globalMinutes
}

// closeExpiredBatch 在已开启的事务内关闭一批过期 pending 订单。
// 它只承担状态机推进与价格锁释放，cutoff/now 与全局窗口由调用方决定，
// 使 OrderService.ExpireOrders 与 LifecycleService.closeExpiredOrders 共享同一套超时状态转换规则。
//
// 三遍查询的设计（每遍都受 batchExpireSize 限制，不随订单数放大）：
//  1. cutoff=now-global（与旧实现同名同参同批大小）取一批，再按每单的有效窗口过滤：
//     有效窗口 <= 全局的照旧立即关单，更长的（商户自配 30 分钟）留到第 2 遍。
//     protocol=0 的有效窗口恒为全局值，因此这一遍对原生订单与旧实现完全一致。
//  2. 只有存在比全局更长的商户窗口时才追加：cutoff=now-longest。
//     该查询取回的行 createdAt <= now-longest，对**任何**商户都必然已过期，
//     所以不需要过滤，也就不会出现「整批被跳过、长窗口订单永远关不掉」的饥饿。
//  3. 只有存在比全局更短的商户窗口时才追加：cutoff=now-shortest，按有效窗口过滤
//     （短窗口订单还没被第 1 遍的 cutoff 覆盖到）。
//
// 三遍互不重复：订单被关掉后 state 不再等于 pending，后续查询自然看不到它。
//
// 为什么必须分遍而不是「一次宽查询 + 过滤」：宽 cutoff 会让未到期的长窗口订单填满
// 整批（created_at ASC 取前 100），短窗口订单在同一轮里被挤掉，形成饥饿。
func closeExpiredBatch(
	txCtx context.Context,
	orders port.OrderRepository,
	priceLocks port.PriceLockRepository,
	cutoff time.Time,
	now time.Time,
	globalMinutes int,
	windows map[int64]int,
) (int, error) {
	keepExpired := func(value order.Order) bool {
		deadline := value.CreatedAt.Add(time.Duration(closeMinutesFor(value, globalMinutes, windows)) * time.Minute)
		return !deadline.After(now)
	}

	closed, err := closeExpiredByCutoff(txCtx, orders, priceLocks, cutoff, now, keepExpired)
	if err != nil {
		return closed, err
	}

	longest, shortest := globalMinutes, globalMinutes
	for _, minutes := range windows {
		if minutes > longest {
			longest = minutes
		}
		if minutes < shortest {
			shortest = minutes
		}
	}

	if longest > globalMinutes {
		count, wideErr := closeExpiredByCutoff(txCtx, orders, priceLocks,
			now.Add(-time.Duration(longest)*time.Minute), now, nil)
		closed += count
		if wideErr != nil {
			return closed, wideErr
		}
	}
	if shortest < globalMinutes {
		count, narrowErr := closeExpiredByCutoff(txCtx, orders, priceLocks,
			now.Add(-time.Duration(shortest)*time.Minute), now, keepExpired)
		closed += count
		if narrowErr != nil {
			return closed, narrowErr
		}
	}
	return closed, nil
}

// closeExpiredByCutoff 关掉一批 created_at < cutoff 的 pending 订单。
// keep 为 nil 表示这批全都该关；否则逐单过滤（同一 cutoff 下存在不同窗口时用）。
func closeExpiredByCutoff(
	txCtx context.Context,
	orders port.OrderRepository,
	priceLocks port.PriceLockRepository,
	cutoff time.Time,
	now time.Time,
	keep func(order.Order) bool,
) (int, error) {
	var closed int
	expired, err := orders.FindExpiredPendingForUpdate(txCtx, cutoff, batchExpireSize)
	if err != nil {
		return 0, wrap(CodeDependency, "查询过期订单失败", err)
	}
	for _, value := range expired {
		if keep != nil && !keep(value) {
			continue
		}
		changed, transitionErr := orders.Transition(txCtx, value.ID, order.StatusPending, order.StatusClosed, now)
		if transitionErr != nil {
			return closed, wrap(CodeDependency, "关闭过期订单失败", transitionErr)
		}
		if !changed {
			return closed, fail(CodeConflict, "过期订单状态已变化，请重试")
		}
		if err := priceLocks.ReleaseByOrderID(txCtx, value.OrderID); err != nil {
			return closed, wrap(CodeDependency, "释放过期订单金额失败", err)
		}
		closed++
	}
	return closed, nil
}
