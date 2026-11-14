package mysql

import (
	"context"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/port"
	"gorm.io/gorm"
)

// MerchantRow 是 merchants 表的行映射。
// 行结构放在仓储文件里而不是 models.go，是为了让本次新增的商户能力自包含，
// 不牵动既有订单/二维码模型文件的改动面。
type MerchantRow struct {
	ID           int64     `gorm:"column:id;primaryKey;autoIncrement"`
	MerchantNo   string    `gorm:"column:merchant_no;not null;uniqueIndex:uq_merchants_no"`
	Name         string    `gorm:"column:name;not null"`
	Enabled      bool      `gorm:"column:enabled;not null"`
	CloseMinutes int       `gorm:"column:close_minutes;not null"`
	Note         string    `gorm:"column:note;not null"`
	CreatedAt    time.Time `gorm:"column:created_at;not null"`
	UpdatedAt    time.Time `gorm:"column:updated_at;not null"`
}

func (MerchantRow) TableName() string { return "merchants" }

func (m MerchantRow) ToDomain() merchant.Merchant {
	return merchant.Merchant{
		ID:           m.ID,
		No:           m.MerchantNo,
		Name:         m.Name,
		Enabled:      m.Enabled,
		CloseMinutes: m.CloseMinutes,
		Note:         m.Note,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
	}
}

func merchantToRow(value merchant.Merchant) MerchantRow {
	return MerchantRow{
		ID:           value.ID,
		MerchantNo:   value.No,
		Name:         value.Name,
		Enabled:      value.Enabled,
		CloseMinutes: value.CloseMinutes,
		Note:         value.Note,
		CreatedAt:    value.CreatedAt,
		UpdatedAt:    value.UpdatedAt,
	}
}

// MerchantRepository 是商户主体的持久化实现。
// 密钥不在这里：merchant_keys 由 MerchantKeyRepository 独立承担，便于单独审查加解密相关代码。
type MerchantRepository struct {
	db *gorm.DB
}

func NewMerchantRepository(db *gorm.DB) *MerchantRepository {
	return &MerchantRepository{db: db}
}

func (r *MerchantRepository) Create(ctx context.Context, value merchant.Merchant) (merchant.Merchant, error) {
	row := merchantToRow(value)
	// 时间兜底与 MerchantKeyRepository.Upsert 保持一致：
	// 调用方漏设时间时，宁可在这里落当前时间，也不要让 MySQL 的 NO_ZERO_DATE
	// 抛出 "Incorrect datetime value" —— 那种报错会把人引向列类型，而不是漏设字段。
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = row.CreatedAt
	}
	result := databaseFromContext(ctx, r.db).Create(&row)
	// merchant_no 有唯一约束，撞号说明随机源重复或人工指定了已存在的号，属于业务冲突。
	if duplicateKey(result.Error) {
		return merchant.Merchant{}, port.ErrConflict
	}
	if result.Error != nil {
		return merchant.Merchant{}, result.Error
	}
	return row.ToDomain(), nil
}

func (r *MerchantRepository) FindByID(ctx context.Context, id int64) (merchant.Merchant, error) {
	var row MerchantRow
	err := databaseFromContext(ctx, r.db).Where("id = ?", id).Take(&row).Error
	return row.ToDomain(), mapDatabaseError(err)
}

// FindByNo 按对外商户号查询；EPay 请求只带 pid，必须先换到内部 id。
func (r *MerchantRepository) FindByNo(ctx context.Context, no string) (merchant.Merchant, error) {
	var row MerchantRow
	err := databaseFromContext(ctx, r.db).Where("merchant_no = ?", no).Take(&row).Error
	return row.ToDomain(), mapDatabaseError(err)
}

// List 按 id 升序返回全部商户；商户数量有限，不做分页。
func (r *MerchantRepository) List(ctx context.Context) ([]merchant.Merchant, error) {
	var rows []MerchantRow
	if err := databaseFromContext(ctx, r.db).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]merchant.Merchant, 0, len(rows))
	for _, row := range rows {
		items = append(items, row.ToDomain())
	}
	return items, nil
}

// Update 只更新可变字段，created_at 与 merchant_no 一经写入不再改写。
// 显式用 map 而不是 Save，避免调用方漏填字段时把整行覆盖成零值。
func (r *MerchantRepository) Update(ctx context.Context, value merchant.Merchant) error {
	if value.ID <= 0 {
		return port.ErrNotFound
	}
	updatedAt := value.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	result := databaseFromContext(ctx, r.db).
		Model(&MerchantRow{}).
		Where("id = ?", value.ID).
		Updates(map[string]any{
			"name":          value.Name,
			"enabled":       value.Enabled,
			"close_minutes": value.CloseMinutes,
			"note":          value.Note,
			"updated_at":    updatedAt,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		// 注意：MySQL 对「值未变化」的 UPDATE 也会返回 0，
		// 因此这里只在确实查不到该行时才算未命中。
		var count int64
		if err := databaseFromContext(ctx, r.db).Model(&MerchantRow{}).Where("id = ?", value.ID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return port.ErrNotFound
		}
	}
	return nil
}

// Delete 只删商户主体；merchant_keys 由调用方在同一事务里通过 DeleteByMerchant 级联清理，
// 因为「该商户是否还有待支付订单」的判定必须在事务内完成。
func (r *MerchantRepository) Delete(ctx context.Context, id int64) error {
	result := databaseFromContext(ctx, r.db).Where("id = ?", id).Delete(&MerchantRow{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return port.ErrNotFound
	}
	return nil
}

func (r *MerchantRepository) Count(ctx context.Context) (int64, error) {
	var total int64
	if err := databaseFromContext(ctx, r.db).Model(&MerchantRow{}).Count(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

var _ port.MerchantRepository = (*MerchantRepository)(nil)
