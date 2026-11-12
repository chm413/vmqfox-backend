package job

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/usecase"
)

type NotificationDelivery interface {
	DeliverNext(context.Context, time.Duration) (usecase.DeliveryStats, error)
}

type NotificationWorkerConfig struct {
	PollInterval   time.Duration
	AttemptTimeout time.Duration
	LeaseDuration  time.Duration
	BatchSize      int
	// Concurrency 是同一批任务内的并发投递数：单个商户回调变慢时不再阻塞其他订单。
	Concurrency int
}

type NotificationWorker struct {
	delivery NotificationDelivery
	config   NotificationWorkerConfig
	logger   *log.Logger
}

func NewNotificationWorker(delivery NotificationDelivery, config NotificationWorkerConfig, logger *log.Logger) (*NotificationWorker, error) {
	if delivery == nil {
		return nil, errors.New("通知 worker 用例不能为空")
	}
	if config.PollInterval <= 0 || config.AttemptTimeout <= 0 || config.LeaseDuration <= config.AttemptTimeout || config.BatchSize < 1 {
		return nil, errors.New("通知 worker 配置无效")
	}
	if config.Concurrency < 1 {
		config.Concurrency = 1
	}
	if config.Concurrency > config.BatchSize {
		config.Concurrency = config.BatchSize
	}
	if logger == nil {
		logger = log.Default()
	}
	return &NotificationWorker{delivery: delivery, config: config, logger: logger}, nil
}

// Run 按固定周期排空待投递任务。
func (w *NotificationWorker) Run(ctx context.Context) {
	w.runBatch(ctx)
	ticker := time.NewTicker(w.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runBatch(ctx)
		}
	}
}

// runBatch 用固定数量的协程共享一份批次预算，每个协程逐条领取并结算。
// 领取与结算都依赖 outbox 的行锁与租约令牌，因此并发消费不会重复投递同一条消息。
func (w *NotificationWorker) runBatch(ctx context.Context) {
	var (
		budgetMu sync.Mutex
		budget   = w.config.BatchSize
		wg       sync.WaitGroup
	)
	// take 返回 true 表示本协程还可以再处理一条。
	take := func() bool {
		budgetMu.Lock()
		defer budgetMu.Unlock()
		if budget <= 0 {
			return false
		}
		budget--
		return true
	}

	for index := 0; index < w.config.Concurrency; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil && take() {
				attemptCtx, cancel := context.WithTimeout(ctx, w.config.AttemptTimeout)
				stats, err := w.delivery.DeliverNext(attemptCtx, w.config.LeaseDuration)
				cancel()
				if err != nil {
					if ctx.Err() == nil {
						w.logger.Printf("通知任务处理失败: %v", err)
					}
					return
				}
				if stats.Claimed == 0 {
					return
				}
				if stats.Failed > 0 {
					w.logger.Printf("通知任务本次投递失败，已安排重试")
				}
			}
		}()
	}
	wg.Wait()
}
