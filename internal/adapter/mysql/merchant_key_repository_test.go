package mysql

// merchant_keys 仓储的集成用例。
//
// 为什么必须打真库：`UpdatePeerPublicKey` 的整个安全价值在于 SQL 里**没有** generation 条件。
// 这条语义用内存 fake 是测不出来的——fake 由测试自己实现，想让它「覆盖全部代」它就覆盖，
// 把 SQL 改回只更新当前代，fake 依然全绿（QA 变异实验已证明这一点）。
// 只有真库才能证明「一条 UPDATE 真的改到了两行」。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// TestUpdatePeerPublicKeyCoversAllGenerations 锁定安全修复的真库语义：
// 换商户公钥必须一次改掉该商户该协议的全部代（含上一代），否则旧私钥仍能签单。
func TestUpdatePeerPublicKeyCoversAllGenerations(t *testing.T) {
	db := openTestDatabase(t)
	if err := InitSchema(db); err != nil {
		t.Fatalf("初始化测试表失败: %v", err)
	}
	if err := db.Exec("DELETE FROM merchant_keys").Error; err != nil {
		t.Fatalf("清理密钥表失败: %v", err)
	}
	if err := db.Exec("DELETE FROM merchants").Error; err != nil {
		t.Fatalf("清理商户表失败: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	const merchantNo = "MTESTV2KEYS0001"
	if err := db.Exec(
		"INSERT INTO merchants (merchant_no, name, enabled, close_minutes, note, created_at, updated_at) VALUES (?,?,?,?,?,?,?)",
		merchantNo, "真库用例商户", 1, 30, "", now, now,
	).Error; err != nil {
		t.Fatalf("插入商户失败: %v", err)
	}
	var merchantID int64
	if err := db.Raw("SELECT id FROM merchants WHERE merchant_no = ?", merchantNo).Scan(&merchantID).Error; err != nil {
		t.Fatalf("查询商户 id 失败: %v", err)
	}
	if merchantID == 0 {
		t.Fatal("未取到商户 id")
	}

	repo := NewMerchantKeyRepository(db)
	ctx := context.Background()
	base := merchant.Key{
		MerchantID:   merchantID,
		Protocol:     merchant.ProtocolEPayV2,
		KeyVersion:   1,
		SecretCipher: []byte("cipher"),
		SecretNonce:  []byte("nonce"),
		Hint:         "hint",
		CreatedAt:    now,
	}
	current := base
	current.Generation = merchant.GenerationCurrent
	current.PeerPublicKey = "-----BEGIN PUBLIC KEY-----\nCURRENT\n-----END PUBLIC KEY-----"
	previous := base
	previous.Generation = merchant.GenerationPrevious
	previous.PeerPublicKey = "-----BEGIN PUBLIC KEY-----\nPREVIOUS\n-----END PUBLIC KEY-----"
	if err := repo.Upsert(ctx, current); err != nil {
		t.Fatalf("写入当前代失败: %v", err)
	}
	if err := repo.Upsert(ctx, previous); err != nil {
		t.Fatalf("写入上一代失败: %v", err)
	}

	const replaced = "-----BEGIN PUBLIC KEY-----\nREPLACED\n-----END PUBLIC KEY-----"
	if err := repo.UpdatePeerPublicKey(ctx, merchantID, merchant.ProtocolEPayV2, replaced); err != nil {
		t.Fatalf("更新商户公钥失败: %v", err)
	}

	keys, err := repo.FindAllForVerify(ctx, merchantID, merchant.ProtocolEPayV2)
	if err != nil {
		t.Fatalf("读取密钥失败: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("应有当前代与上一代两行，实际 %d 行", len(keys))
	}
	for _, key := range keys {
		if key.PeerPublicKey != replaced {
			t.Fatalf("generation=%d 的商户公钥未更新（旧私钥仍可签单），实际 %q",
				key.Generation, key.PeerPublicKey)
		}
	}

	// 键行不存在时必须返回 ErrNotFound，而不是「影响 0 行也算成功」。
	if err := repo.UpdatePeerPublicKey(ctx, merchantID+9999, merchant.ProtocolEPayV2, replaced); !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("不存在的商户应返回 ErrNotFound，实际 %v", err)
	}

	// 重复写入同一个值是幂等的：不能被 RowsAffected=0 误报成「未开通」。
	if err := repo.UpdatePeerPublicKey(ctx, merchantID, merchant.ProtocolEPayV2, replaced); err != nil {
		t.Fatalf("重复写入同一把公钥应幂等成功，实际 %v", err)
	}
}
