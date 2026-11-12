package usecase

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// pruneInterval 控制历史清理频率：生命周期任务每 10 秒一轮，清理一小时一次足够。
const pruneInterval = time.Hour

type LifecycleServiceDeps struct {
	Transactions port.TransactionManager
	Orders       port.OrderRepository
	PriceLocks   port.PriceLockRepository
	Settings     port.SettingRepository
	Clock        port.Clock
	// 以下两项与 Retention 均为可选：未提供时跳过历史清理，保持旧行为。
	Events    port.PaymentEventRepository
	Outbox    port.OutboxRepository
	Retention time.Duration
}

type LifecycleStats struct {
	ClosedOrders  int
	MonitorState  string
	PrunedRecords int64
}

// LifecycleService 集中处理时间驱动的状态变化，避免查询请求产生写库副作用。
type LifecycleService struct {
	transactions port.TransactionManager
	orders       port.OrderRepository
	priceLocks   port.PriceLockRepository
	settings     port.SettingRepository
	clock        port.Clock
	events       port.PaymentEventRepository
	outbox       port.OutboxRepository
	retention    time.Duration

	pruneMu   sync.Mutex
	lastPrune time.Time
}

func NewLifecycleService(deps LifecycleServiceDeps) (*LifecycleService, error) {
	if deps.Transactions == nil || deps.Orders == nil || deps.PriceLocks == nil || deps.Settings == nil || deps.Clock == nil {
		return nil, errors.New("生命周期用例依赖不完整")
	}
	return &LifecycleService{
		transactions: deps.Transactions,
		orders:       deps.Orders,
		priceLocks:   deps.PriceLocks,
		settings:     deps.Settings,
		clock:        deps.Clock,
		events:       deps.Events,
		outbox:       deps.Outbox,
		retention:    deps.Retention,
	}, nil
}

func (s *LifecycleService) Maintain(ctx context.Context, heartbeatTimeout time.Duration, batchSize int) (LifecycleStats, error) {
	if heartbeatTimeout <= 0 || batchSize < 1 {
		return LifecycleStats{}, fail(CodeConfiguration, "生命周期任务配置无效")
	}

	now := s.clock.Now()
	monitorState, monitorErr := s.refreshMonitorState(ctx, now, heartbeatTimeout)
	closedOrders, expiryErr := s.closeExpiredOrders(ctx, now, batchSize)
	pruned, pruneErr := s.pruneHistory(ctx, now)
	return LifecycleStats{
		ClosedOrders:  closedOrders,
		MonitorState:  monitorState,
		PrunedRecords: pruned,
	}, errors.Join(monitorErr, expiryErr, pruneErr)
}

// pruneHistory 清理过期的到账事件与已完结的通知消息，防止幂等表与 outbox 无限增长。
// 清理失败不影响订单状态推进，只作为独立错误上报。
func (s *LifecycleService) pruneHistory(ctx context.Context, now time.Time) (int64, error) {
	if s.retention <= 0 || s.events == nil || s.outbox == nil {
		return 0, nil
	}
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()
	if !s.lastPrune.IsZero() && now.Sub(s.lastPrune) < pruneInterval {
		return 0, nil
	}

	cutoff := now.Add(-s.retention)
	events, err := s.events.PruneBefore(ctx, cutoff)
	if err != nil {
		return 0, wrap(CodeDependency, "清理历史到账事件失败", err)
	}
	messages, err := s.outbox.PruneDeliveredBefore(ctx, cutoff)
	if err != nil {
		return events, wrap(CodeDependency, "清理历史通知消息失败", err)
	}
	s.lastPrune = now
	return events + messages, nil
}

func (s *LifecycleService) refreshMonitorState(ctx context.Context, now time.Time, heartbeatTimeout time.Duration) (string, error) {
	state := ""
	err := s.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
		values, err := s.settings.GetManyForUpdate(txCtx, []string{
			setting.LastHeartKey,
			setting.MonitorStateKey,
		})
		if err != nil {
			return wrap(CodeDependency, "读取监控状态失败", err)
		}

		state = expectedMonitorState(values[setting.LastHeartKey], now, heartbeatTimeout)
		if values[setting.MonitorStateKey] == state {
			return nil
		}
		if err := s.settings.Set(txCtx, setting.MonitorStateKey, state); err != nil {
			return wrap(CodeDependency, "更新监控状态失败", err)
		}
		return nil
	})
	return state, err
}

func (s *LifecycleService) closeExpiredOrders(ctx context.Context, now time.Time, batchSize int) (int, error) {
	closed := 0
	err := s.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
		rawCloseMinutes, err := s.settings.Get(txCtx, setting.CloseMinutesKey)
		if err != nil && !errors.Is(err, port.ErrNotFound) {
			return wrap(CodeDependency, "读取订单超时设置失败", err)
		}
		cutoff := now.Add(-time.Duration(closeMinutes(rawCloseMinutes)) * time.Minute)
		c, err := closeExpiredBatch(txCtx, s.orders, s.priceLocks, cutoff, now)
		closed = c
		return err
	})
	return closed, err
}

func expectedMonitorState(rawLastHeart string, now time.Time, heartbeatTimeout time.Duration) string {
	lastHeart, err := strconv.ParseInt(strings.TrimSpace(rawLastHeart), 10, 64)
	if err != nil || lastHeart <= 0 {
		return "-1"
	}
	if time.Unix(lastHeart, 0).Add(heartbeatTimeout).After(now) {
		return "1"
	}
	return "0"
}
