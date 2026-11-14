package usecase

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

const (
	// merchantNameMaxRunes 对齐 merchants.name VARCHAR(64)。MySQL 的 VARCHAR 按字符计数，
	// 用 rune 数校验才不会让 64 个汉字的名称在写库那一刻被拒。
	merchantNameMaxRunes = 64
	// merchantNoteMaxRunes 对齐 merchants.note VARCHAR(255)，理由同上。
	merchantNoteMaxRunes = 255
	// merchantCloseMinutesMax 上限 24 小时。更长的支付窗口没有业务意义，
	// 而 EPay 订单会一直占着全局共享的金额锁（price_locks 没有商户维度）。
	merchantCloseMinutesMax = 1440
	// merchantNoAttempts merchant_no 是 "M" + 13 位随机串，撞号概率极低；
	// 撞上唯一键时重试几次即可，不重试会把极低概率事件变成运营看得到的一次报错。
	merchantNoAttempts = 3
)

// errNoMasterKeyMessage 是「主密钥未配置」的对外文案。
// 它会原样展示给管理员，所以必须自带解决动作：只写「未配置 VMQ_MASTER_KEY」会让人去翻代码，
// 而不是去补环境变量。
const errNoMasterKeyMessage = "未配置 VMQ_MASTER_KEY，无法创建商户密钥，请用 openssl rand -base64 32 生成后重启服务"

type MerchantServiceDeps struct {
	Merchants port.MerchantRepository
	Keys      port.MerchantKeyRepository
	// Secrets 为 nil 表示未配置主密钥。所有需要加解密的入口都必须显式拒绝，
	// 绝不允许 panic，也不允许退化成明文入库。
	Secrets *secret.Box
	Now     func() time.Time
}

type MerchantService struct {
	merchants port.MerchantRepository
	keys      port.MerchantKeyRepository
	secrets   *secret.Box
	now       func() time.Time
}

type CreateMerchantInput struct {
	Name string
	// CloseMinutes 为 0 表示跟随全局 settings.close；原生订单不看这个字段。
	CloseMinutes int
	Note         string
}

type CreateMerchantResult struct {
	Merchant merchant.Merchant
	// SecretPlain 是 V1 共享密钥明文，只在这次响应里返回一次，之后任何接口都拿不到。
	SecretPlain string
	SecretHint  string
	// PublicKeyPEM 是 V2 平台公钥，可反复导出。
	// 但 V2 默认不开通（要等接入方提供商户公钥），所以创建时该字段恒为空；
	// 唯一导出入口是 PlatformPublicKey。
	PublicKeyPEM string
}

// UpdateMerchantInput 是全量替换语义：调用方必须显式给出 enabled，
// 避免「漏传字段 → 商户被静默停用」这种只能靠工单发现的故障。
type UpdateMerchantInput struct {
	Name         string
	Enabled      bool
	CloseMinutes int
	Note         string
}

// MerchantView 是管理端列表/更新后的视图：只有辨认信息与协议开通状态。
// 刻意不含明文、也不含密文与 nonce——列表接口是最容易顺手放大泄露面的地方。
type MerchantView struct {
	ID           int64
	MerchantNo   string
	Name         string
	Enabled      bool
	CloseMinutes int
	Note         string
	// V1Enabled/V2Enabled 由「有没有当前代密钥」推导，不存在独立的布尔开关，
	// 也就不会出现「开关开着但没密钥」的不一致状态。
	V1Enabled bool
	V2Enabled bool
	V1Hint    string
	V2Hint    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type RotateResult struct {
	Protocol merchant.Protocol
	// Secret 是 V1 轮换后的新明文，仅此一次；V2 轮换时为空。
	Secret string
	// PublicKey 是 V2 轮换后的新平台公钥（可反复导出）；V1 轮换时为空。
	PublicKey string
	Hint      string
}

func NewMerchantService(deps MerchantServiceDeps) (*MerchantService, error) {
	if deps.Merchants == nil || deps.Keys == nil {
		return nil, errors.New("商户用例依赖不完整")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &MerchantService{
		merchants: deps.Merchants,
		keys:      deps.Keys,
		secrets:   deps.Secrets,
		now:       now,
	}, nil
}

// Create 创建商户并同时开通 EPay V1（生成 32 位共享密钥、加密入库、只回一次明文）。
// V2 默认不开通：它需要接入方的商户公钥，创建时拿不到。
func (s *MerchantService) Create(ctx context.Context, input CreateMerchantInput) (CreateMerchantResult, error) {
	// 主密钥缺失必须在写库之前拒绝。若先插商户行再失败，库里会留下一条永远开不通 V1、
	// 且看不出原因的商户，比直接报错更难收拾。
	if s.secrets == nil {
		return CreateMerchantResult{}, fail(CodeConfiguration, errNoMasterKeyMessage)
	}
	name, note, err := validateMerchantFields(input.Name, input.CloseMinutes, input.Note)
	if err != nil {
		return CreateMerchantResult{}, err
	}

	now := s.now().UTC()
	created, err := s.createMerchantRow(ctx, name, note, input.CloseMinutes, now)
	if err != nil {
		return CreateMerchantResult{}, err
	}

	plain, err := s.sealNewV1Key(ctx, created.ID, now)
	if err != nil {
		// 补偿删除：留下「没有密钥的商户」比创建失败更糟——它既不能下单，也看不出为什么。
		if deleteErr := s.merchants.Delete(ctx, created.ID); deleteErr != nil {
			return CreateMerchantResult{}, wrap(CodeDependency,
				"商户密钥写入失败且回滚失败，请人工检查该商户",
				errors.Join(err, deleteErr))
		}
		return CreateMerchantResult{}, err
	}

	return CreateMerchantResult{
		Merchant:    created,
		SecretPlain: plain,
		SecretHint:  merchant.HintV1(plain),
	}, nil
}

func (s *MerchantService) createMerchantRow(ctx context.Context, name, note string, closeMinutes int, now time.Time) (merchant.Merchant, error) {
	var lastErr error
	for attempt := 0; attempt < merchantNoAttempts; attempt++ {
		no, err := merchant.NewMerchantNo()
		if err != nil {
			return merchant.Merchant{}, wrap(CodeDependency, "生成商户号失败", err)
		}
		created, err := s.merchants.Create(ctx, merchant.Merchant{
			No:           no,
			Name:         name,
			Enabled:      true,
			CloseMinutes: closeMinutes,
			Note:         note,
			CreatedAt:    now,
			UpdatedAt:    now,
		})
		if err == nil {
			return created, nil
		}
		if !errors.Is(err, port.ErrConflict) {
			return merchant.Merchant{}, wrap(CodeDependency, "创建商户失败", err)
		}
		// 只在唯一键冲突时重试；其它错误重试没有意义，还会把真实原因拖成一条超时。
		lastErr = err
	}
	return merchant.Merchant{}, wrap(CodeConflict, "商户号冲突，请重试", lastErr)
}

// sealNewV1Key 生成并入库当前代 V1 密钥，返回明文供本次响应使用。
func (s *MerchantService) sealNewV1Key(ctx context.Context, merchantID int64, now time.Time) (string, error) {
	plain, err := merchant.NewV1Secret()
	if err != nil {
		return "", wrap(CodeDependency, "生成商户密钥失败", err)
	}
	// 解密点会按同一规则断言；这里先校验一次，让编码类 bug 在诞生处就暴露，
	// 而不是等到某次验签失败、日志只写「签名错误」时才发作。
	if !merchant.IsValidV1Secret(plain) {
		return "", fail(CodeConfiguration, "生成的商户密钥不符合规范")
	}

	key := merchant.Key{
		MerchantID: merchantID,
		Protocol:   merchant.ProtocolEPayV1,
		Generation: merchant.GenerationCurrent,
		Hint:       merchant.HintV1(plain),
		CreatedAt:  now,
	}
	if err := s.sealKey(&key, []byte(plain)); err != nil {
		return "", err
	}
	if err := s.keys.Upsert(ctx, key); err != nil {
		return "", wrap(CodeDependency, "写入商户密钥失败", err)
	}
	return plain, nil
}

// sealKey 把明文加密进 key 的密文列。AAD 由 key 自身拼出（merchant_id/protocol/generation/purpose），
// 因此调用前必须先填好这三个字段，否则密文会被钉在错误的行上、之后谁也解不开。
func (s *MerchantService) sealKey(key *merchant.Key, plain []byte) error {
	if s.secrets == nil {
		return fail(CodeConfiguration, errNoMasterKeyMessage)
	}
	cipherText, nonce, version, err := s.secrets.Seal(plain, key.AAD())
	if err != nil {
		return wrap(CodeDependency, "加密商户密钥失败", err)
	}
	key.SecretCipher = cipherText
	key.SecretNonce = nonce
	key.KeyVersion = version
	return nil
}

func (s *MerchantService) List(ctx context.Context) ([]MerchantView, error) {
	items, err := s.merchants.List(ctx)
	if err != nil {
		return nil, wrap(CodeDependency, "查询商户列表失败", err)
	}
	views := make([]MerchantView, 0, len(items))
	for _, item := range items {
		view, err := s.viewOf(ctx, item)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *MerchantService) Update(ctx context.Context, id int64, input UpdateMerchantInput) (MerchantView, error) {
	if id <= 0 {
		return MerchantView{}, fail(CodeInvalidArgument, "商户ID无效")
	}
	name, note, err := validateMerchantFields(input.Name, input.CloseMinutes, input.Note)
	if err != nil {
		return MerchantView{}, err
	}
	current, err := s.find(ctx, id)
	if err != nil {
		return MerchantView{}, err
	}

	updated := current
	updated.Name = name
	updated.Note = note
	// 停用只拦新下单；在途订单的回调照常投递，否则等于把已收的钱变成不发货。
	updated.Enabled = input.Enabled
	updated.CloseMinutes = input.CloseMinutes
	updated.UpdatedAt = s.now().UTC()

	if err := s.merchants.Update(ctx, updated); err != nil {
		if errors.Is(err, port.ErrNotFound) {
			return MerchantView{}, fail(CodeNotFound, "商户不存在")
		}
		return MerchantView{}, wrap(CodeDependency, "更新商户失败", err)
	}
	return s.viewOf(ctx, updated)
}

// RotateKey 轮换指定协议的密钥：删上一代 → 当前代降级为仅验签 → 写入新当前代（由仓储在一个事务里完成）。
// 明文/新公钥只在这里返回一次。
func (s *MerchantService) RotateKey(ctx context.Context, id int64, protocol merchant.Protocol) (RotateResult, error) {
	if id <= 0 {
		return RotateResult{}, fail(CodeInvalidArgument, "商户ID无效")
	}
	if !protocol.Valid() {
		return RotateResult{}, fail(CodeInvalidArgument, "协议版本无效，只支持 1（EPayV1）或 2（EPayV2）")
	}
	if s.secrets == nil {
		return RotateResult{}, fail(CodeConfiguration, errNoMasterKeyMessage)
	}
	if _, err := s.find(ctx, id); err != nil {
		return RotateResult{}, err
	}

	// 轮换前必须拿到旧行的 peer_public_key 并原样带入新代：
	// 它属于「商户身份」而不是「我方密钥」，清空它会让该商户的 V2 入站请求全部验签失败。
	old, err := s.keys.FindActive(ctx, id, protocol)
	if errors.Is(err, port.ErrNotFound) {
		return RotateResult{}, fail(CodeInvalidState, "该商户未开通 "+protocolLabel(protocol)+" 接口")
	}
	if err != nil {
		return RotateResult{}, wrap(CodeDependency, "读取商户密钥失败", err)
	}

	next := merchant.Key{
		MerchantID:    id,
		Protocol:      protocol,
		Generation:    merchant.GenerationCurrent,
		PeerPublicKey: old.PeerPublicKey,
		CreatedAt:     s.now().UTC(),
	}
	result := RotateResult{Protocol: protocol}

	switch protocol {
	case merchant.ProtocolEPayV1:
		plain, err := merchant.NewV1Secret()
		if err != nil {
			return RotateResult{}, wrap(CodeDependency, "生成商户密钥失败", err)
		}
		if !merchant.IsValidV1Secret(plain) {
			return RotateResult{}, fail(CodeConfiguration, "生成的商户密钥不符合规范")
		}
		next.Hint = merchant.HintV1(plain)
		if err := s.sealKey(&next, []byte(plain)); err != nil {
			return RotateResult{}, err
		}
		result.Secret = plain
		result.Hint = next.Hint
	case merchant.ProtocolEPayV2:
		privatePEM, publicPEM, err := merchant.NewRSAKeyPair()
		if err != nil {
			return RotateResult{}, wrap(CodeDependency, "生成平台密钥对失败", err)
		}
		next.Hint = merchant.HintPublicKey(publicPEM)
		if err := s.sealKey(&next, privatePEM); err != nil {
			return RotateResult{}, err
		}
		result.PublicKey = string(publicPEM)
		result.Hint = next.Hint
	}

	if err := s.keys.Rotate(ctx, id, protocol, next); err != nil {
		if errors.Is(err, port.ErrNotFound) {
			return RotateResult{}, fail(CodeNotFound, "商户不存在")
		}
		// 并发轮换撞唯一键必须落到业务错误，不能是 500：管理员重试一次就好。
		if errors.Is(err, port.ErrConflict) {
			return RotateResult{}, fail(CodeConflict, "轮换冲突，请重试")
		}
		return RotateResult{}, wrap(CodeDependency, "轮换商户密钥失败", err)
	}

	// 明文只在上面的写入成功之后才随返回值送出：写入失败时它随调用栈一起丢弃，
	// 绝不出现「管理员拿到一把从未入库的密钥」。
	return result, nil
}

// openKey 按行自带的 key_version 与 AAD 解出明文，并把「主密钥问题」与「商户配置问题」分开报。
func (s *MerchantService) openKey(key merchant.Key) ([]byte, error) {
	plain, err := s.secrets.Open(key.SecretCipher, key.SecretNonce, key.AAD(), key.KeyVersion)
	if err == nil {
		return plain, nil
	}
	// 未知主密钥版本必须与「签名错误」区分开：前者是换错环境/回滚了库，
	// 后者会把运维引向商户配置，永远查不出来。
	if errors.Is(err, secret.ErrUnknownKeyVersion) {
		return nil, fail(CodeConfiguration, "商户密钥无法解密：密文由未知版本的主密钥加密，请恢复原主密钥或重置该商户密钥")
	}
	return nil, fail(CodeConfiguration, "商户密钥无法解密，请联系管理员重置该商户密钥")
}

// PlatformPublicKey 导出该商户的 V2 平台公钥（可反复调用，公钥不敏感）。
// 公钥不入库：它可以从平台私钥重算出来，重算比多存一列更不容易漂移，
// 也让 peer_public_key 继续只承载商户公钥，两者不混用。
func (s *MerchantService) PlatformPublicKey(ctx context.Context, id int64) (string, error) {
	if id <= 0 {
		return "", fail(CodeInvalidArgument, "商户ID无效")
	}
	if _, err := s.find(ctx, id); err != nil {
		return "", err
	}
	if s.secrets == nil {
		return "", fail(CodeConfiguration, errNoMasterKeyMessage)
	}

	key, err := s.keys.FindActive(ctx, id, merchant.ProtocolEPayV2)
	if errors.Is(err, port.ErrNotFound) {
		return "", fail(CodeNotFound, "该商户未开通 V2 接口")
	}
	if err != nil {
		return "", wrap(CodeDependency, "读取商户密钥失败", err)
	}

	plain, err := s.openKey(key)
	if err != nil {
		return "", err
	}

	publicPEM, err := publicKeyPEMFromPrivate(plain)
	if err != nil {
		return "", wrap(CodeDependency, "解析平台私钥失败", err)
	}
	return publicPEM, nil
}

// publicKeyPEMFromPrivate 从 PKCS#8 私钥 PEM 重算 PKIX 公钥 PEM，与 merchant.NewRSAKeyPair 严格对称。
func publicKeyPEMFromPrivate(privatePEM []byte) (string, error) {
	block, _ := pem.Decode(privatePEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		return "", errors.New("平台私钥不是 PKCS#8 PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	privateKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return "", errors.New("平台私钥不是 RSA 私钥")
	}
	der, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

func (s *MerchantService) viewOf(ctx context.Context, value merchant.Merchant) (MerchantView, error) {
	v1, err := s.keyState(ctx, value.ID, merchant.ProtocolEPayV1)
	if err != nil {
		return MerchantView{}, err
	}
	v2, err := s.keyState(ctx, value.ID, merchant.ProtocolEPayV2)
	if err != nil {
		return MerchantView{}, err
	}
	return MerchantView{
		ID:           value.ID,
		MerchantNo:   value.No,
		Name:         value.Name,
		Enabled:      value.Enabled,
		CloseMinutes: value.CloseMinutes,
		Note:         value.Note,
		V1Enabled:    v1.enabled,
		V2Enabled:    v2.enabled,
		V1Hint:       v1.hint,
		V2Hint:       v2.hint,
		CreatedAt:    value.CreatedAt,
		UpdatedAt:    value.UpdatedAt,
	}, nil
}

type keyState struct {
	enabled bool
	hint    string
}

func (s *MerchantService) keyState(ctx context.Context, merchantID int64, protocol merchant.Protocol) (keyState, error) {
	key, err := s.keys.FindActive(ctx, merchantID, protocol)
	if errors.Is(err, port.ErrNotFound) {
		// 未命中即「该协议未开通」：开通与否由有没有当前代密钥自然决定。
		return keyState{}, nil
	}
	if err != nil {
		return keyState{}, wrap(CodeDependency, "读取商户密钥状态失败", err)
	}
	return keyState{enabled: true, hint: key.Hint}, nil
}

func (s *MerchantService) find(ctx context.Context, id int64) (merchant.Merchant, error) {
	value, err := s.merchants.FindByID(ctx, id)
	if errors.Is(err, port.ErrNotFound) {
		return merchant.Merchant{}, fail(CodeNotFound, "商户不存在")
	}
	if err != nil {
		return merchant.Merchant{}, wrap(CodeDependency, "查询商户失败", err)
	}
	return value, nil
}

// validateMerchantFields 统一校验创建与更新的字段，返回规范化后的名称与备注。
func validateMerchantFields(name string, closeMinutes int, note string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", fail(CodeInvalidArgument, "商户名称不能为空")
	}
	if utf8.RuneCountInString(name) > merchantNameMaxRunes {
		return "", "", fail(CodeInvalidArgument,
			fmt.Sprintf("商户名称不能超过 %d 个字符", merchantNameMaxRunes))
	}
	if closeMinutes < 0 || closeMinutes > merchantCloseMinutesMax {
		return "", "", fail(CodeInvalidArgument,
			fmt.Sprintf("订单有效期必须在 0-%d 分钟之间（0 表示跟随全局设置）", merchantCloseMinutesMax))
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > merchantNoteMaxRunes {
		return "", "", fail(CodeInvalidArgument,
			fmt.Sprintf("备注不能超过 %d 个字符", merchantNoteMaxRunes))
	}
	return name, note, nil
}

// protocolLabel 只用于文案，不参与任何持久化。
func protocolLabel(value merchant.Protocol) string {
	if value == merchant.ProtocolEPayV2 {
		return "V2"
	}
	return "V1"
}
