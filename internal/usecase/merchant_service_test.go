package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

// 本文件的替身刻意不放在 fakes_test.go：商户用例要断言「密钥只回一次」这类性质，
// 依赖共享替身会让别的测试改动间接打断这里的断言。

type merchantServiceTestClock struct{ now time.Time }

func (c merchantServiceTestClock) Now() time.Time { return c.now }

// merchantServiceTestRepo 是 port.MerchantRepository 的内存实现。
type merchantServiceTestRepo struct {
	items     map[int64]merchant.Merchant
	nextID    int64
	createErr error
}

func newMerchantServiceTestRepo() *merchantServiceTestRepo {
	return &merchantServiceTestRepo{items: make(map[int64]merchant.Merchant)}
}

func (r *merchantServiceTestRepo) Create(_ context.Context, value merchant.Merchant) (merchant.Merchant, error) {
	if r.createErr != nil {
		return merchant.Merchant{}, r.createErr
	}
	r.nextID++
	value.ID = r.nextID
	r.items[value.ID] = value
	return value, nil
}

func (r *merchantServiceTestRepo) FindByID(_ context.Context, id int64) (merchant.Merchant, error) {
	value, ok := r.items[id]
	if !ok {
		return merchant.Merchant{}, port.ErrNotFound
	}
	return value, nil
}

func (r *merchantServiceTestRepo) FindByNo(_ context.Context, no string) (merchant.Merchant, error) {
	for _, value := range r.items {
		if value.No == no {
			return value, nil
		}
	}
	return merchant.Merchant{}, port.ErrNotFound
}

func (r *merchantServiceTestRepo) List(context.Context) ([]merchant.Merchant, error) {
	items := make([]merchant.Merchant, 0, len(r.items))
	for id := int64(1); id <= r.nextID; id++ {
		if value, ok := r.items[id]; ok {
			items = append(items, value)
		}
	}
	return items, nil
}

func (r *merchantServiceTestRepo) Update(_ context.Context, value merchant.Merchant) error {
	if _, ok := r.items[value.ID]; !ok {
		return port.ErrNotFound
	}
	r.items[value.ID] = value
	return nil
}

func (r *merchantServiceTestRepo) Delete(_ context.Context, id int64) error {
	if _, ok := r.items[id]; !ok {
		return port.ErrNotFound
	}
	delete(r.items, id)
	return nil
}

func (r *merchantServiceTestRepo) Count(context.Context) (int64, error) {
	return int64(len(r.items)), nil
}

// merchantServiceTestKeys 复刻真实仓储的两代制语义，轮换后能查回上一代。
type merchantServiceTestKeys struct {
	rows      map[string]merchant.Key
	upsertErr error
	rotateErr error
	rotated   []merchant.Key
}

func newMerchantServiceTestKeys() *merchantServiceTestKeys {
	return &merchantServiceTestKeys{rows: make(map[string]merchant.Key)}
}

func merchantServiceTestKeyID(merchantID int64, protocol merchant.Protocol, generation int8) string {
	return protocol.String() + "|" + strconv.FormatInt(merchantID, 10) + "|" + strconv.Itoa(int(generation))
}

func (k *merchantServiceTestKeys) FindActive(_ context.Context, merchantID int64, protocol merchant.Protocol) (merchant.Key, error) {
	value, ok := k.rows[merchantServiceTestKeyID(merchantID, protocol, merchant.GenerationCurrent)]
	if !ok {
		return merchant.Key{}, port.ErrNotFound
	}
	return value, nil
}

func (k *merchantServiceTestKeys) FindAllForVerify(_ context.Context, merchantID int64, protocol merchant.Protocol) ([]merchant.Key, error) {
	items := make([]merchant.Key, 0, 2)
	for _, generation := range []int8{merchant.GenerationCurrent, merchant.GenerationPrevious} {
		if value, ok := k.rows[merchantServiceTestKeyID(merchantID, protocol, generation)]; ok {
			items = append(items, value)
		}
	}
	return items, nil
}

func (k *merchantServiceTestKeys) Upsert(_ context.Context, value merchant.Key) error {
	if k.upsertErr != nil {
		return k.upsertErr
	}
	k.rows[merchantServiceTestKeyID(value.MerchantID, value.Protocol, value.Generation)] = value
	return nil
}

func (k *merchantServiceTestKeys) Rotate(_ context.Context, merchantID int64, protocol merchant.Protocol, next merchant.Key) error {
	if k.rotateErr != nil {
		return k.rotateErr
	}
	k.rotated = append(k.rotated, next)

	delete(k.rows, merchantServiceTestKeyID(merchantID, protocol, merchant.GenerationPrevious))
	if current, ok := k.rows[merchantServiceTestKeyID(merchantID, protocol, merchant.GenerationCurrent)]; ok {
		current.Generation = merchant.GenerationPrevious
		k.rows[merchantServiceTestKeyID(merchantID, protocol, merchant.GenerationPrevious)] = current
	}
	next.MerchantID = merchantID
	next.Protocol = protocol
	next.Generation = merchant.GenerationCurrent
	k.rows[merchantServiceTestKeyID(merchantID, protocol, merchant.GenerationCurrent)] = next
	return nil
}

func (k *merchantServiceTestKeys) DeleteByMerchant(_ context.Context, merchantID int64) error {
	for key, value := range k.rows {
		if value.MerchantID == merchantID {
			delete(k.rows, key)
		}
	}
	return nil
}

var (
	_ port.MerchantRepository    = (*merchantServiceTestRepo)(nil)
	_ port.MerchantKeyRepository = (*merchantServiceTestKeys)(nil)
)

func newMerchantServiceTestBox(t *testing.T) *secret.Box {
	t.Helper()
	box, err := secret.New(map[int][]byte{1: []byte("0123456789abcdef0123456789abcdef")}, 1)
	if err != nil {
		t.Fatalf("构造主密钥失败: %v", err)
	}
	return box
}

func newMerchantServiceUnderTest(t *testing.T, box *secret.Box) (*MerchantService, *merchantServiceTestRepo, *merchantServiceTestKeys) {
	t.Helper()
	repo := newMerchantServiceTestRepo()
	keys := newMerchantServiceTestKeys()
	clock := merchantServiceTestClock{now: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}
	service, err := NewMerchantService(MerchantServiceDeps{
		Merchants: repo,
		Keys:      keys,
		Secrets:   box,
		Now:       clock.Now,
	})
	if err != nil {
		t.Fatalf("构造商户用例失败: %v", err)
	}
	return service, repo, keys
}

// TestMerchantServiceCreateReturnsSecretOnce 覆盖「创建时下发 V1 密钥、之后只能看到 hint」。
func TestMerchantServiceCreateReturnsSecretOnce(t *testing.T) {
	box := newMerchantServiceTestBox(t)
	service, _, keys := newMerchantServiceUnderTest(t, box)

	result, err := service.Create(context.Background(), CreateMerchantInput{
		Name:         " 自营站 ",
		CloseMinutes: 30,
		Note:         "联系人 13800000000",
	})
	if err != nil {
		t.Fatalf("创建商户失败: %v", err)
	}

	if !merchant.IsValidV1Secret(result.SecretPlain) {
		t.Fatalf("下发的 V1 密钥不符合规范: %q", result.SecretPlain)
	}
	if result.SecretHint != merchant.HintV1(result.SecretPlain) || len(result.SecretHint) != 4 {
		t.Fatalf("hint 应为密钥末 4 位，实际 %q", result.SecretHint)
	}
	if !strings.HasPrefix(result.Merchant.No, "M") || len(result.Merchant.No) != 14 {
		t.Fatalf("商户号应为 14 位且以 M 开头，实际 %q", result.Merchant.No)
	}
	if result.Merchant.Name != "自营站" {
		t.Fatalf("商户名称应被去掉首尾空白，实际 %q", result.Merchant.Name)
	}
	if !result.Merchant.Enabled {
		t.Fatal("新建商户应默认启用，停用会让运营以为创建失败")
	}
	if result.PublicKeyPEM != "" {
		t.Fatalf("V2 默认不开通，创建时不应下发平台公钥，实际 %q", result.PublicKeyPEM)
	}

	// 明文必须能用 AAD 解回来：这是「解密点 fail-fast」的前提。
	key, err := keys.FindActive(context.Background(), result.Merchant.ID, merchant.ProtocolEPayV1)
	if err != nil {
		t.Fatalf("未写入 V1 密钥: %v", err)
	}
	if key.Generation != merchant.GenerationCurrent || key.Hint != result.SecretHint {
		t.Fatalf("写入的密钥代或 hint 不正确: %+v", key)
	}
	if key.PeerPublicKey != "" {
		t.Fatalf("V1 行不应写入 peer_public_key，实际 %q", key.PeerPublicKey)
	}
	plain, err := box.Open(key.SecretCipher, key.SecretNonce, key.AAD(), key.KeyVersion)
	if err != nil {
		t.Fatalf("解密 V1 密钥失败: %v", err)
	}
	if string(plain) != result.SecretPlain {
		t.Fatalf("解回的密钥与下发的不一致")
	}

	views, err := service.List(context.Background())
	if err != nil {
		t.Fatalf("查询商户列表失败: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("列表应只有 1 个商户，实际 %d", len(views))
	}
	view := views[0]
	if !view.V1Enabled || view.V2Enabled {
		t.Fatalf("协议开通状态应为 V1 开、V2 关，实际 %+v", view)
	}
	if view.V1Hint != result.SecretHint || view.V2Hint != "" {
		t.Fatalf("列表 hint 不正确: %+v", view)
	}

	// 列表响应任何字段都不得携带明文或密文，用整体序列化做兜底断言。
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("序列化商户视图失败: %v", err)
	}
	if strings.Contains(string(raw), result.SecretPlain) {
		t.Fatalf("商户视图泄露了密钥明文: %s", raw)
	}
	for _, forbidden := range []string{"secret", "cipher", "nonce"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("商户视图出现了不该有的字段 %q: %s", forbidden, raw)
		}
	}
}

// TestMerchantServiceCreateRejectsMissingMasterKey 保证主密钥缺失时不 panic 也不写库。
func TestMerchantServiceCreateRejectsMissingMasterKey(t *testing.T) {
	service, repo, _ := newMerchantServiceUnderTest(t, nil)

	_, err := service.Create(context.Background(), CreateMerchantInput{Name: "自营站", CloseMinutes: 30})
	if err == nil {
		t.Fatal("主密钥缺失时创建商户必须失败")
	}
	code, ok := ErrorCodeOf(err)
	if !ok || code != CodeConfiguration {
		t.Fatalf("应返回配置错误，实际 %v（code=%v）", err, code)
	}
	if !strings.Contains(err.Error(), "VMQ_MASTER_KEY") {
		t.Fatalf("错误文案必须指明 VMQ_MASTER_KEY，实际 %q", err.Error())
	}
	if len(repo.items) != 0 {
		t.Fatalf("主密钥缺失时不应写入商户行，实际 %d 行", len(repo.items))
	}
}

// TestMerchantServiceCreateRollsBackWhenKeyWriteFails 避免留下「没有密钥的商户」。
func TestMerchantServiceCreateRollsBackWhenKeyWriteFails(t *testing.T) {
	box := newMerchantServiceTestBox(t)
	service, repo, keys := newMerchantServiceUnderTest(t, box)
	keys.upsertErr = errors.New("写入被拒绝")

	if _, err := service.Create(context.Background(), CreateMerchantInput{Name: "自营站", CloseMinutes: 30}); err == nil {
		t.Fatal("密钥写入失败时创建必须失败")
	}
	if len(repo.items) != 0 {
		t.Fatalf("密钥写入失败后应补偿删除商户行，实际残留 %d 行", len(repo.items))
	}
}

// TestMerchantServiceRotateKeepsPreviousGenerationAndPeerPublicKey 覆盖两代制与 peer_public_key 保留。
func TestMerchantServiceRotateKeepsPreviousGenerationAndPeerPublicKey(t *testing.T) {
	box := newMerchantServiceTestBox(t)
	service, _, keys := newMerchantServiceUnderTest(t, box)
	ctx := context.Background()

	created, err := service.Create(ctx, CreateMerchantInput{Name: "合作方站", CloseMinutes: 0})
	if err != nil {
		t.Fatalf("创建商户失败: %v", err)
	}
	// 模拟已开通 V2 并粘贴了商户公钥：轮换时必须原样带入新代。
	privatePEM, publicPEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成平台密钥对失败: %v", err)
	}
	const peerPublicKey = "-----BEGIN PUBLIC KEY-----\nPEER\n-----END PUBLIC KEY-----\n"
	v2Key := merchant.Key{
		MerchantID:    created.Merchant.ID,
		Protocol:      merchant.ProtocolEPayV2,
		Generation:    merchant.GenerationCurrent,
		PeerPublicKey: peerPublicKey,
		Hint:          merchant.HintPublicKey(publicPEM),
	}
	cipherText, nonce, version, err := box.Seal(privatePEM, v2Key.AAD())
	if err != nil {
		t.Fatalf("加密平台私钥失败: %v", err)
	}
	v2Key.SecretCipher, v2Key.SecretNonce, v2Key.KeyVersion = cipherText, nonce, version
	if err := keys.Upsert(ctx, v2Key); err != nil {
		t.Fatalf("写入 V2 密钥失败: %v", err)
	}

	// V1 轮换：新密钥可解，旧密钥仍在 generation=0。
	oldV1, err := keys.FindActive(ctx, created.Merchant.ID, merchant.ProtocolEPayV1)
	if err != nil {
		t.Fatalf("读取 V1 当前代失败: %v", err)
	}
	if oldV1.Hint != created.SecretHint {
		t.Fatalf("轮换前当前代 hint 应与创建时下发的一致: %+v", oldV1)
	}
	rotated, err := service.RotateKey(ctx, created.Merchant.ID, merchant.ProtocolEPayV1)
	if err != nil {
		t.Fatalf("轮换 V1 密钥失败: %v", err)
	}
	if !merchant.IsValidV1Secret(rotated.Secret) || rotated.Hint != merchant.HintV1(rotated.Secret) {
		t.Fatalf("轮换结果不合法: %+v", rotated)
	}
	if rotated.Secret == created.SecretPlain {
		t.Fatal("轮换必须生成新密钥")
	}
	current, err := keys.FindActive(ctx, created.Merchant.ID, merchant.ProtocolEPayV1)
	if err != nil {
		t.Fatalf("轮换后读取当前代失败: %v", err)
	}
	newPlain, err := box.Open(current.SecretCipher, current.SecretNonce, current.AAD(), current.KeyVersion)
	if err != nil {
		t.Fatalf("解密新密钥失败: %v", err)
	}
	if string(newPlain) != rotated.Secret {
		t.Fatal("新密钥解密结果与下发值不一致")
	}
	if current.Hint != rotated.Hint {
		t.Fatalf("新代 hint 与下发值不一致: %+v", current)
	}
	previous, ok := keys.rows[merchantServiceTestKeyID(created.Merchant.ID, merchant.ProtocolEPayV1, merchant.GenerationPrevious)]
	if !ok {
		t.Fatal("旧密钥应降级为 generation=0 继续用于验签")
	}
	// 关键回归：仓储轮换只改 generation、不重新加密密文。
	// 上一代的密文与 nonce 必须原样保留，且按降级后的行仍能解开——
	// 这同时约束服务层不要再「偷偷重加密上一代」，那种补偿会把义务散给每个调用方。
	if !bytes.Equal(previous.SecretCipher, oldV1.SecretCipher) || !bytes.Equal(previous.SecretNonce, oldV1.SecretNonce) {
		t.Fatal("轮换不应重写上一代密文：密文与 nonce 必须原样保留")
	}
	oldPlain, err := box.Open(previous.SecretCipher, previous.SecretNonce, previous.AAD(), previous.KeyVersion)
	if err != nil {
		t.Fatalf("降级为 generation=0 后旧密文必须仍可解密，实际: %v", err)
	}
	if string(oldPlain) != created.SecretPlain {
		t.Fatal("generation=0 应保留轮换前的旧密钥")
	}

	// V2 轮换：换的是我方平台私钥，商户公钥必须原样带入。
	v2Rotated, err := service.RotateKey(ctx, created.Merchant.ID, merchant.ProtocolEPayV2)
	if err != nil {
		t.Fatalf("轮换 V2 密钥失败: %v", err)
	}
	if v2Rotated.Secret != "" {
		t.Fatalf("V2 轮换不应返回共享密钥明文，实际 %q", v2Rotated.Secret)
	}
	block, _ := pem.Decode([]byte(v2Rotated.PublicKey))
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("V2 轮换应下发新的平台公钥 PEM，实际 %q", v2Rotated.PublicKey)
	}
	if v2Rotated.Hint != merchant.HintPublicKey([]byte(v2Rotated.PublicKey)) {
		t.Fatalf("V2 hint 应为新公钥指纹，实际 %q", v2Rotated.Hint)
	}
	for _, generation := range []int8{merchant.GenerationCurrent, merchant.GenerationPrevious} {
		row, exists := keys.rows[merchantServiceTestKeyID(created.Merchant.ID, merchant.ProtocolEPayV2, generation)]
		if !exists {
			t.Fatalf("V2 generation=%d 缺失", generation)
		}
		if row.PeerPublicKey != peerPublicKey {
			t.Fatalf("轮换后 generation=%d 的 peer_public_key 被改动: %q", generation, row.PeerPublicKey)
		}
	}
	// 上一代平台私钥必须仍可用旧公钥解出，否则在途 V2 验签会整段失效。
	previousV2 := keys.rows[merchantServiceTestKeyID(created.Merchant.ID, merchant.ProtocolEPayV2, merchant.GenerationPrevious)]
	if !bytes.Equal(previousV2.SecretCipher, v2Key.SecretCipher) {
		t.Fatal("V2 轮换同样不应重写上一代密文")
	}
	previousPrivate, err := box.Open(previousV2.SecretCipher, previousV2.SecretNonce, previousV2.AAD(), previousV2.KeyVersion)
	if err != nil {
		t.Fatalf("解密上一代平台私钥失败: %v", err)
	}
	if string(previousPrivate) != string(privatePEM) {
		t.Fatal("generation=0 应保留轮换前的平台私钥")
	}
}

// TestMerchantServiceRotateRejectsUnopenedProtocol 保证「未开通」不会退化成「写入一把新密钥」。
func TestMerchantServiceRotateRejectsUnopenedProtocol(t *testing.T) {
	box := newMerchantServiceTestBox(t)
	service, _, _ := newMerchantServiceUnderTest(t, box)
	ctx := context.Background()

	created, err := service.Create(ctx, CreateMerchantInput{Name: "自营站", CloseMinutes: 30})
	if err != nil {
		t.Fatalf("创建商户失败: %v", err)
	}
	_, err = service.RotateKey(ctx, created.Merchant.ID, merchant.ProtocolEPayV2)
	code, ok := ErrorCodeOf(err)
	if !ok || code != CodeInvalidState {
		t.Fatalf("未开通 V2 时轮换应返回状态错误，实际 %v（code=%v）", err, code)
	}
	if !strings.Contains(err.Error(), "未开通 V2") {
		t.Fatalf("错误文案应指明协议未开通，实际 %q", err.Error())
	}

	if _, err := service.RotateKey(ctx, created.Merchant.ID, merchant.ProtocolNative); err == nil {
		t.Fatal("原生协议不应参与商户密钥轮换")
	}
	if _, err := service.RotateKey(ctx, 999, merchant.ProtocolEPayV1); err == nil {
		t.Fatal("商户不存在时轮换必须失败")
	}
}

// TestMerchantServicePlatformPublicKeyRecomputedFromPrivate 证明公钥不需要单独入库。
func TestMerchantServicePlatformPublicKeyRecomputedFromPrivate(t *testing.T) {
	box := newMerchantServiceTestBox(t)
	service, _, keys := newMerchantServiceUnderTest(t, box)
	ctx := context.Background()

	created, err := service.Create(ctx, CreateMerchantInput{Name: "合作方站", CloseMinutes: 30})
	if err != nil {
		t.Fatalf("创建商户失败: %v", err)
	}

	// 未开通 V2 时导出公钥必须明确失败，而不是返回空串让商户去乱猜。
	if _, err := service.PlatformPublicKey(ctx, created.Merchant.ID); err == nil {
		t.Fatal("未开通 V2 时导出平台公钥必须失败")
	} else if code, ok := ErrorCodeOf(err); !ok || code != CodeNotFound {
		t.Fatalf("应返回未找到错误，实际 %v（code=%v）", err, code)
	}

	privatePEM, publicPEM, err := merchant.NewRSAKeyPair()
	if err != nil {
		t.Fatalf("生成平台密钥对失败: %v", err)
	}
	v2Key := merchant.Key{
		MerchantID:    created.Merchant.ID,
		Protocol:      merchant.ProtocolEPayV2,
		Generation:    merchant.GenerationCurrent,
		PeerPublicKey: "商户公钥占位",
		Hint:          merchant.HintPublicKey(publicPEM),
	}
	cipherText, nonce, version, err := box.Seal(privatePEM, v2Key.AAD())
	if err != nil {
		t.Fatalf("加密平台私钥失败: %v", err)
	}
	v2Key.SecretCipher, v2Key.SecretNonce, v2Key.KeyVersion = cipherText, nonce, version
	if err := keys.Upsert(ctx, v2Key); err != nil {
		t.Fatalf("写入 V2 密钥失败: %v", err)
	}

	exported, err := service.PlatformPublicKey(ctx, created.Merchant.ID)
	if err != nil {
		t.Fatalf("导出平台公钥失败: %v", err)
	}
	if exported != string(publicPEM) {
		t.Fatal("导出的平台公钥应与私钥重算结果逐字节一致")
	}

	// 用主密钥以外的密钥解不出：证明密文确实受主密钥保护。
	otherBox, err := secret.New(map[int][]byte{1: []byte("ffffffffffffffffffffffffffffffff")}, 1)
	if err != nil {
		t.Fatalf("构造另一把主密钥失败: %v", err)
	}
	if _, err := otherBox.Open(v2Key.SecretCipher, v2Key.SecretNonce, v2Key.AAD(), v2Key.KeyVersion); err == nil {
		t.Fatal("换一把主密钥不应能解开平台私钥")
	}
}

// TestMerchantServiceUpdateAndValidation 覆盖字段校验与更新语义。
func TestMerchantServiceUpdateAndValidation(t *testing.T) {
	box := newMerchantServiceTestBox(t)
	service, _, _ := newMerchantServiceUnderTest(t, box)
	ctx := context.Background()

	created, err := service.Create(ctx, CreateMerchantInput{Name: "自营站", CloseMinutes: 30})
	if err != nil {
		t.Fatalf("创建商户失败: %v", err)
	}

	updated, err := service.Update(ctx, created.Merchant.ID, UpdateMerchantInput{
		Name:         "自营站-停用",
		Enabled:      false,
		CloseMinutes: 0,
		Note:         "临时停用",
	})
	if err != nil {
		t.Fatalf("更新商户失败: %v", err)
	}
	if updated.Enabled || updated.Name != "自营站-停用" || updated.CloseMinutes != 0 || updated.Note != "临时停用" {
		t.Fatalf("更新结果不符合预期: %+v", updated)
	}
	// 停用不改动密钥：仍要能验签、能回调在途订单。
	if !updated.V1Enabled || updated.V1Hint != created.SecretHint {
		t.Fatalf("更新不应影响已开通的密钥: %+v", updated)
	}

	cases := []struct {
		name  string
		input UpdateMerchantInput
	}{
		{"空名称", UpdateMerchantInput{Name: "  ", CloseMinutes: 30}},
		{"名称超长", UpdateMerchantInput{Name: strings.Repeat("商", 65), CloseMinutes: 30}},
		{"有效期越界", UpdateMerchantInput{Name: "自营站", CloseMinutes: 1441}},
		{"有效期负数", UpdateMerchantInput{Name: "自营站", CloseMinutes: -1}},
		{"备注超长", UpdateMerchantInput{Name: "自营站", CloseMinutes: 30, Note: strings.Repeat("备", 256)}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := service.Update(ctx, created.Merchant.ID, testCase.input)
			code, ok := ErrorCodeOf(err)
			if !ok || code != CodeInvalidArgument {
				t.Fatalf("应返回参数错误，实际 %v（code=%v）", err, code)
			}
		})
	}

	if _, err := service.Update(ctx, 999, UpdateMerchantInput{Name: "自营站", CloseMinutes: 30}); err == nil {
		t.Fatal("商户不存在时更新必须失败")
	} else if code, ok := ErrorCodeOf(err); !ok || code != CodeNotFound {
		t.Fatalf("应返回未找到错误，实际 %v（code=%v）", err, code)
	}
}

// TestNewMerchantServiceRequiresRepositories 保证装配漏项在构造期就失败。
func TestNewMerchantServiceRequiresRepositories(t *testing.T) {
	if _, err := NewMerchantService(MerchantServiceDeps{}); err == nil {
		t.Fatal("缺少仓储时构造商户用例必须失败")
	}
}
