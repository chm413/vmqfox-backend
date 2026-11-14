package mysql

import (
	"context"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/port"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MerchantKeyRow 是 merchant_keys 表的行映射。
// 密文列只做透传：加解密与 AAD 绑定在 secret 包与领域层完成，仓储不碰明文。
type MerchantKeyRow struct {
	MerchantID    int64      `gorm:"column:merchant_id;primaryKey"`
	Protocol      int8       `gorm:"column:protocol;primaryKey"`
	Generation    int8       `gorm:"column:generation;primaryKey"`
	KeyVersion    int        `gorm:"column:key_version;not null"`
	SecretCipher  []byte     `gorm:"column:secret_cipher;not null;type:blob"`
	SecretNonce   []byte     `gorm:"column:secret_nonce;not null;type:blob"`
	PeerPublicKey string     `gorm:"column:peer_public_key;not null;type:varchar(1024)"`
	Hint          string     `gorm:"column:hint;not null"`
	LastUsedAt    *time.Time `gorm:"column:last_used_at"`
	CreatedAt     time.Time  `gorm:"column:created_at;not null"`
}

func (MerchantKeyRow) TableName() string { return "merchant_keys" }

func (m MerchantKeyRow) ToDomain() merchant.Key {
	return merchant.Key{
		MerchantID:    m.MerchantID,
		Protocol:      merchant.Protocol(m.Protocol),
		Generation:    m.Generation,
		KeyVersion:    m.KeyVersion,
		SecretCipher:  m.SecretCipher,
		SecretNonce:   m.SecretNonce,
		PeerPublicKey: m.PeerPublicKey,
		Hint:          m.Hint,
		LastUsedAt:    timeValue(m.LastUsedAt),
		CreatedAt:     m.CreatedAt,
	}
}

func merchantKeyToRow(value merchant.Key) MerchantKeyRow {
	return MerchantKeyRow{
		MerchantID:    value.MerchantID,
		Protocol:      int8(value.Protocol),
		Generation:    value.Generation,
		KeyVersion:    value.KeyVersion,
		SecretCipher:  value.SecretCipher,
		SecretNonce:   value.SecretNonce,
		PeerPublicKey: value.PeerPublicKey,
		Hint:          value.Hint,
		LastUsedAt:    timePointer(value.LastUsedAt),
		CreatedAt:     value.CreatedAt,
	}
}

// MerchantKeyRepository 是商户密钥（两代制）的持久化实现。
type MerchantKeyRepository struct {
	db *gorm.DB
}

func NewMerchantKeyRepository(db *gorm.DB) *MerchantKeyRepository {
	return &MerchantKeyRepository{db: db}
}

// FindActive 只取当前代（generation=1）：签发与回调签名只允许用当前代。
// 未命中即「该商户未开通此协议」，直接返回 port.ErrNotFound。
func (r *MerchantKeyRepository) FindActive(ctx context.Context, merchantID int64, protocol merchant.Protocol) (merchant.Key, error) {
	var row MerchantKeyRow
	err := databaseFromContext(ctx, r.db).
		Where("merchant_id = ? AND protocol = ? AND generation = ?", merchantID, int8(protocol), merchant.GenerationCurrent).
		Take(&row).Error
	return row.ToDomain(), mapDatabaseError(err)
}

// FindAllForVerify 返回当前代与上一代（当前代在前），供验签逐一尝试。
// 与 List 一致的语义：查不到时返回空切片 + nil 错误，由调用方决定是「未开通」还是「验签失败」，
// 避免把「没有密钥」误报成「签名错误」。
func (r *MerchantKeyRepository) FindAllForVerify(ctx context.Context, merchantID int64, protocol merchant.Protocol) ([]merchant.Key, error) {
	var rows []MerchantKeyRow
	err := databaseFromContext(ctx, r.db).
		Where("merchant_id = ? AND protocol = ?", merchantID, int8(protocol)).
		Order("generation DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	items := make([]merchant.Key, 0, len(rows))
	for _, row := range rows {
		items = append(items, row.ToDomain())
	}
	return items, nil
}

// Upsert 按主键 (merchant_id, protocol, generation) 覆盖写入。
// 不更新 created_at：首次写入的时间就是这代密钥的诞生时间。
func (r *MerchantKeyRepository) Upsert(ctx context.Context, value merchant.Key) error {
	row := merchantKeyToRow(value)
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	return databaseFromContext(ctx, r.db).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "merchant_id"},
				{Name: "protocol"},
				{Name: "generation"},
			},
			DoUpdates: clause.AssignmentColumns([]string{
				"key_version",
				"secret_cipher",
				"secret_nonce",
				"peer_public_key",
				"hint",
				"last_used_at",
			}),
		}).
		Create(&row).Error
}

// Rotate 在一个事务里完成三步：删上一代 → 当前代降级为上一代 → 写入新当前代。
//
// 事务开头先 SELECT ... FROM merchants WHERE id = ? FOR UPDATE，
// 用商户行做串行化令牌，防止两个管理员并发轮换导致出现两行 generation=1。
func (r *MerchantKeyRepository) Rotate(ctx context.Context, merchantID int64, protocol merchant.Protocol, next merchant.Key) error {
	if !protocol.Valid() {
		return port.ErrNotFound
	}
	return databaseFromContext(ctx, r.db).Transaction(func(tx *gorm.DB) error {
		var merchantRow MerchantRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", merchantID).
			Take(&merchantRow).Error
		if err != nil {
			// 商户不存在时不允许写入孤立密钥行（否则会留下谁也查不到的密文）。
			return mapDatabaseError(err)
		}

		protocolValue := int8(protocol)
		if err := tx.Where("merchant_id = ? AND protocol = ? AND generation = ?",
			merchantID, protocolValue, merchant.GenerationPrevious).
			Delete(&MerchantKeyRow{}).Error; err != nil {
			return err
		}

		if err := tx.Model(&MerchantKeyRow{}).
			Where("merchant_id = ? AND protocol = ? AND generation = ?",
				merchantID, protocolValue, merchant.GenerationCurrent).
			Update("generation", merchant.GenerationPrevious).Error; err != nil {
			return err
		}

		row := merchantKeyToRow(next)
		// 代与归属由调用参数决定，不允许传进来的 next 覆盖，否则会写出第三行或错商户的行。
		row.MerchantID = merchantID
		row.Protocol = protocolValue
		row.Generation = merchant.GenerationCurrent
		if row.CreatedAt.IsZero() {
			row.CreatedAt = time.Now().UTC()
		}
		return tx.Create(&row).Error
	})
}

// DeleteByMerchant 物理删除该商户全部密钥。
// 用于「删除商户」级联清理；删除后历史订单不再能重签回调，属于不可逆操作。
func (r *MerchantKeyRepository) DeleteByMerchant(ctx context.Context, merchantID int64) error {
	return databaseFromContext(ctx, r.db).
		Where("merchant_id = ?", merchantID).
		Delete(&MerchantKeyRow{}).Error
}

var _ port.MerchantKeyRepository = (*MerchantKeyRepository)(nil)
