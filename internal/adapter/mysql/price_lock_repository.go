package mysql

import (
	"context"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
	"github.com/hulisang/vmqfox-backend/internal/port"
	"gorm.io/gorm"
)

type PriceLockRepository struct {
	db *gorm.DB
}

func NewPriceLockRepository(db *gorm.DB) *PriceLockRepository {
	return &PriceLockRepository{db: db}
}

func (r *PriceLockRepository) TryAcquire(ctx context.Context, orderID string, paymentType payment.Type, amountCents int64) (bool, error) {
	row := PriceLockRow{
		PaymentType: int(paymentType),
		AmountCent:  amountCents,
		OrderID:     orderID,
		// 与订单表保持同一时间基准（进程本地时区），避免 DATETIME 混写造成偏移。
		CreatedAt: time.Now(),
	}
	result := databaseFromContext(ctx, r.db).Create(&row)
	if result.Error == nil {
		return true, nil
	}
	if duplicateKey(result.Error) {
		return false, nil
	}
	return false, result.Error
}

func (r *PriceLockRepository) ReleaseByOrderID(ctx context.Context, orderID string) error {
	return databaseFromContext(ctx, r.db).Where("order_id = ?", orderID).Delete(&PriceLockRow{}).Error
}

// ReleaseByOrdersBefore 以订单为驱动条件批量释放价格锁。
// 旧实现按 price_locks.created_at 释放，与「按 orders.created_at 删除」是两套判定，
// 时间基准或窗口一旦不一致就会留下没有订单的孤儿锁，把金额永久占死。
func (r *PriceLockRepository) ReleaseByOrdersBefore(ctx context.Context, before time.Time) error {
	return databaseFromContext(ctx, r.db).Exec(
		"DELETE pl FROM price_locks pl JOIN orders o ON o.order_id = pl.order_id WHERE o.created_at < ?",
		before,
	).Error
}

// ReleaseOrphaned 清理已经找不到对应订单的残留价格锁，作为历史脏数据的兜底。
func (r *PriceLockRepository) ReleaseOrphaned(ctx context.Context) error {
	return databaseFromContext(ctx, r.db).Exec(
		"DELETE pl FROM price_locks pl LEFT JOIN orders o ON o.order_id = pl.order_id WHERE o.id IS NULL",
	).Error
}

var _ port.PriceLockRepository = (*PriceLockRepository)(nil)
