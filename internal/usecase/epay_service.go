package usecase

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/epay"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

const (
	// epayRequireExactCodeKey 是「严格等额」开关的设置键（设计 §3.2 / §5.4）。
	//
	// 刻意不写进 internal/domain/setting 的 allKeys：那是一份「管理端可写」白名单，
	// 而本开关属于资金口径，改错会让「微调到没有码的价位」直接掉单。
	// 因此读取语义是「缺省即开启」——库里没有这行时按 true 处理。
	epayRequireExactCodeKey = "epayRequireExactCode"

	// epayAdjustIncrease 是传给 acquirePrice 的方向原文值："1" 对应 order.Increase。
	// EPay 订单永远只增不减（设计 §3.2），所以写死，不去读 settings.payQf——
	// 后台的金额调整开关不能改变商户已下单的金额。
	epayAdjustIncrease = "1"

	// epayOutTradeNoMaxLen / epayParamMaxLen 对齐列宽（pay_id VARBINARY(255)、param VARCHAR(255)）。
	// 在用例层提前拒绝，避免 MySQL 严格模式下变成 500 依赖错误。
	epayOutTradeNoMaxLen = 255
	epayParamMaxLen      = 255
)

// NotifyURLValidator 只暴露「这个回调地址能不能用」一个判断。
// 由用例层定义、由 adapter/httpclient 的 NotifyURLPolicy 实现：
// usecase 不 import adapter，出站策略仍只有一份实现。
type NotifyURLValidator interface {
	ValidateNotifyURL(raw string) error
}

type EPayCreateInput struct {
	MerchantID int64
	// Protocol 固定 merchant.ProtocolEPayV1；留成字段是为了让回调/查单复用同一结构时不混淆。
	Protocol   merchant.Protocol
	OutTradeNo string
	Type       string
	Name       string
	// Money 是商户传来的原始文本（"10" 与 "10.00" 必须原样回显）。
	// 只校验不重写：改写会直接改变商户侧签名域。
	Money     string
	NotifyURL string
	ReturnURL string
	Param     string
}

type EPayCreateResult struct {
	// TradeNo 是本网关订单号（orders.order_id），EPay 侧叫 trade_no。
	TradeNo string
	// PayURL 是收银台链接（携带公开令牌），供 submit.php 302 与 mapi.php 的 payurl。
	PayURL string
	// Qrcode 是收款码内容本身（orders.pay_url），供商户前端自行渲染二维码。
	Qrcode string
	IsAuto bool
}

type EPayQueryResult struct {
	TradeNo     string
	OutTradeNo  string
	Type        string
	Name        string
	Money       string
	TradeStatus string
	AddTime     time.Time
	EndTime     time.Time
	Param       string
}

// EPayServiceDeps 只额外要求商户侧依赖，资金/收款码/事务全部复用 OrderService。
//
// 这样做的理由不是图省事：EPay 与原生必须是**同一套** price_locks、qrcodes、settings 实例。
// 各自 New 一份会让两边的事务边界与连接池使用不可控，也让「同一金额被两个入口同时占用」
// 这种竞态变得无法推理。
type EPayServiceDeps struct {
	Orders       *OrderService
	Merchants    port.MerchantRepository
	MerchantKeys port.MerchantKeyRepository
	// MasterKeys 允许为 nil：未配主密钥时服务仍要能启动（设计 §4.1），
	// 但 EPay 下单会在验签阶段明确失败，绝不退化为「跳过验签」。
	MasterKeys *secret.Box
	NotifyURLs NotifyURLValidator
}

type EPayService struct {
	// orders 提供同包私有方法 acquirePrice / resolvePayURL，避免复制资金语义。
	orders *OrderService
	// orderRepo / transactions / ... 是从 orders 上取到的同一批实例，
	// 单独持有只是为了让下面的代码读起来不必写 s.orders.orders。
	orderRepo    port.OrderRepository
	transactions port.TransactionManager
	priceLocks   port.PriceLockRepository
	qrcodes      port.QRCodeRepository
	settings     port.SettingRepository
	clock        port.Clock
	orderIDs     port.OrderIDGenerator
	publicTokens port.PublicTokenGenerator
	frontendURL  string

	merchants    port.MerchantRepository
	merchantKeys port.MerchantKeyRepository
	masterKeys   *secret.Box
	notifyURLs   NotifyURLValidator

	// keyChainWarned 记录「哪些商户的密钥链路故障已经打过日志」。
	// 只用于日志限流，不参与任何业务判断：Authenticate 在未鉴权入口上，
	// 逐请求打印等于给探测者一个刷日志的放大器。
	keyChainWarned sync.Map
}

func NewEPayService(deps EPayServiceDeps) (*EPayService, error) {
	if deps.Orders == nil || deps.Merchants == nil || deps.MerchantKeys == nil || deps.NotifyURLs == nil {
		return nil, errors.New("EPay 下单用例依赖不完整")
	}
	core := deps.Orders
	return &EPayService{
		orders:       core,
		orderRepo:    core.orders,
		transactions: core.transactions,
		priceLocks:   core.priceLocks,
		qrcodes:      core.qrcodes,
		settings:     core.settings,
		clock:        core.clock,
		orderIDs:     core.orderIDs,
		publicTokens: core.publicTokens,
		frontendURL:  core.frontendURL,
		merchants:    deps.Merchants,
		merchantKeys: deps.MerchantKeys,
		masterKeys:   deps.MasterKeys,
		notifyURLs:   deps.NotifyURLs,
	}, nil
}

// Authenticate 完成 V1 入口的三件事：pid → 商户、取当前代密钥、验签。
//
// 放在用例层而不是 handler，是因为验签要解密商户密钥：
// 把 masterKeys 交给 HTTP 层等于让每个入口都能拿到明文密钥，一次日志误打印就是全商户沦陷。
//
// 文案遵守设计 §5.4：商户不存在与停用返回同一句话（避免 pid 变成存在性探针）；
// 参数歧义与验签失败对外完全一致（避免「参数有问题」被误诊成「密钥配错」）。
func (s *EPayService) Authenticate(ctx context.Context, params map[string]string) (merchant.Merchant, error) {
	pid := strings.TrimSpace(params["pid"])
	if pid == "" {
		return merchant.Merchant{}, fail(CodeInvalidArgument, "商户不存在")
	}

	value, err := s.merchants.FindByNo(ctx, pid)
	if errors.Is(err, port.ErrNotFound) {
		return merchant.Merchant{}, fail(CodeInvalidArgument, "商户不存在")
	}
	if err != nil {
		return merchant.Merchant{}, wrap(CodeDependency, "查询商户失败", err)
	}
	if !value.Enabled {
		return merchant.Merchant{}, fail(CodeInvalidArgument, "商户不存在")
	}

	if err := epay.RejectAmbiguous(params); err != nil {
		return merchant.Merchant{}, fail(CodeInvalidSignature, "签名错误")
	}
	secrets, err := s.v1SecretsForVerify(ctx, value)
	if err != nil {
		return merchant.Merchant{}, err
	}
	// 轮换期商户仍在用上一代密钥签名，必须两代都试。
	// 只试当前代会让轮换后所有在途请求失败，而商户侧只看到「签名错误」，
	// 排查方向会被引向「密钥配错」，实际只是没来得及换 key。
	for _, secretText := range secrets {
		if verifyErr := epay.VerifyV1(params, secretText, params["sign"]); verifyErr == nil {
			return value, nil
		}
	}
	return merchant.Merchant{}, fail(CodeInvalidSignature, "签名错误")
}

// v1SecretsForVerify 取出该商户 V1 的全部可验签密钥（当前代在前、上一代在后）。
//
// 用途只有一个：验签（商户发起签名）。轮换期必须两代都认——商户换 key 需要时间，
// 只认当前代会让窗口内所有请求报「签名错误」，而真正原因只是密钥没换完。
//
// 对外文案收敛（安全审计 P3，设计 §5.4 错误码表之外不留第 4 个可区分状态）：
// 「密钥链路坏了」（未配主密钥、密文解不开、明文格式非法）对外一律与真正的验签失败
// 同码同文案 `invalid_signature / 签名错误`。否则未鉴权者只要拿一个存在的 pid 发一次
// 请求，就能区分「商户存在但密钥链路坏了」与「签名不对」，把错误码表变成探测面。
// 真实原因只落内部日志（带 merchant_id 与 cause），排障靠日志不靠响应。
// 「该商户未开通 V1 接口」保持不变：那是设计明确允许的对外状态，且与「密钥损坏」是
// 两件事——前者是商户还没开通，后者是我方数据/配置坏了。
func (s *EPayService) v1SecretsForVerify(ctx context.Context, value merchant.Merchant) ([]string, error) {
	keys, err := s.merchantKeys.FindAllForVerify(ctx, value.ID, merchant.ProtocolEPayV1)
	if err != nil {
		return nil, wrap(CodeDependency, "读取商户密钥失败", err)
	}
	if len(keys) == 0 {
		// 没有密钥行 == 该协议未开通（密钥本身就是开通凭据）。
		return nil, fail(CodeConfiguration, "该商户未开通 V1 接口")
	}
	if s.masterKeys == nil {
		// 未配主密钥时绝不退化为「跳过验签」：那等于任何人可伪造任意商户下单。
		s.warnKeyChainOnce(0, "未配置主密钥", nil)
		return nil, fail(CodeInvalidSignature, "签名错误")
	}

	secrets := make([]string, 0, len(keys))
	for _, key := range keys {
		plain, openErr := s.masterKeys.Open(key.SecretCipher, key.SecretNonce, key.AAD(), key.KeyVersion)
		if openErr != nil {
			// 两代的 master key 版本可能不同，单代解不开不应拖累另一代。
			s.warnKeyChainOnce(value.ID, "密钥解密失败 key_version="+strconv.Itoa(key.KeyVersion), openErr)
			continue
		}
		secretText := string(plain)
		if !merchant.IsValidV1Secret(secretText) {
			// 编码/截断 bug 必须在解密点暴露，否则只会在验签阶段表现为「签名错误」。
			s.warnKeyChainOnce(value.ID, "密钥格式无效 key_version="+strconv.Itoa(key.KeyVersion), nil)
			continue
		}
		secrets = append(secrets, secretText)
	}
	if len(secrets) == 0 {
		s.warnKeyChainOnce(value.ID, "无可用验签密钥 keys="+strconv.Itoa(len(keys)), nil)
		return nil, fail(CodeInvalidSignature, "签名错误")
	}
	return secrets, nil
}

// warnKeyChainOnce 按 (merchant_id, 原因类别) 只记一次真实原因。
//
// 为什么限流而不是每次都打：Authenticate 在未鉴权入口上，一个探测者循环发请求
// 就能把日志刷满；而密钥链路故障是**确定性**的（同一商户每次都失败），
// 记一次足够定位，重复行没有任何排障价值。
// merchant_id=0 表示与商户无关的全局配置故障（未配主密钥）。
func (s *EPayService) warnKeyChainOnce(merchantID int64, reason string, cause error) {
	key := strconv.FormatInt(merchantID, 10) + ":" + reason
	if _, loaded := s.keyChainWarned.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	// 商户号与原因必须同时出现在同一行：只看原因无法判断是哪个商户的密钥坏了。
	log.Printf("epay.v1 验签密钥链路不可用 merchant_id=%d reason=%s cause=%v", merchantID, reason, cause)
}

// Create 是 EPay V1 的下单入口。
//
// 与原生 Create 的关系：复用同一条资金与收款码路径（acquirePrice / resolvePayURL），
// 但不复用 OrderService.Create —— 那条路径强依赖 settings.key 的 VMQ 验签
// 与全局 notifyUrl 回落，两者都是 EPay 明确不允许的（每商户自带密钥与回调地址）。
func (s *EPayService) Create(ctx context.Context, input EPayCreateInput) (EPayCreateResult, error) {
	paymentType, priceCents, err := validateEPayCreateInput(input)
	if err != nil {
		return EPayCreateResult{}, err
	}
	if s.frontendURL == "" {
		// 没有前台地址就没有 payurl：submit.php 无处可跳，mapi.php 会返回空链接。
		// 这是部署配置问题，必须在下单前失败，而不是把空链接交给商户插件。
		return EPayCreateResult{}, fail(CodeConfiguration, "未配置前台地址，无法生成支付链接")
	}
	if err := s.notifyURLs.ValidateNotifyURL(input.NotifyURL); err != nil {
		// 校验失败必须**不写订单、不占金额锁**，否则会留下永远收不到回调的待支付单。
		return EPayCreateResult{}, fail(CodeInvalidArgument, "notify_url 必须是公网可访问地址")
	}

	settings, err := s.settings.GetMany(ctx, []string{
		setting.MonitorStateKey,
		setting.WechatPayURLKey,
		setting.AlipayPayURLKey,
		epayRequireExactCodeKey,
	})
	if err != nil {
		return EPayCreateResult{}, wrap(CodeDependency, "读取系统设置失败", err)
	}
	if settings[setting.MonitorStateKey] != "1" {
		return EPayCreateResult{}, fail(CodeMonitorOffline, "收款通道异常")
	}
	requireExactCode := exactCodeRequired(settings[epayRequireExactCodeKey])

	now := s.clock.Now()
	orderID, err := s.orderIDs.NewOrderID(now)
	if err != nil {
		return EPayCreateResult{}, wrap(CodeDependency, "生成订单号失败", err)
	}

	orderName := truncateEPayName(input.Name)
	for attempt := 0; attempt < publicTokenCreateRetryMax; attempt++ {
		publicToken, tokenErr := newPublicToken(s.publicTokens)
		if tokenErr != nil {
			return EPayCreateResult{}, wrap(CodeDependency, "生成公开订单令牌失败", tokenErr)
		}

		var created, replayed order.Order
		err = s.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
			existing, findErr := s.orderRepo.FindByPayIDForUpdate(txCtx, input.MerchantID, input.OutTradeNo)
			if findErr == nil {
				// 幂等命中：只校验与回放，**绝不重新 acquirePrice**。
				// price_locks 有 uq_price_locks_order_id，重占会撞自己的锁并推进到下一个候选金额，
				// 表现为「同一个订单号金额自己变了」（设计 §5.4 实现约束）。
				if matchErr := checkEPayReplay(existing, paymentType, priceCents); matchErr != nil {
					return matchErr
				}
				replayed = existing
				return nil
			}
			if !errors.Is(findErr, port.ErrNotFound) {
				return wrap(CodeDependency, "检查商户订单号失败", findErr)
			}

			reallyPriceCents, lockErr := s.acquirePriceForEPay(txCtx, orderID, paymentType, priceCents, requireExactCode)
			if lockErr != nil {
				return lockErr
			}

			payURL, isAuto, payURLErr := s.orders.resolvePayURL(txCtx, paymentType, reallyPriceCents, settings)
			if payURLErr != nil {
				return payURLErr
			}

			value := order.Order{
				MerchantID: input.MerchantID,
				// 落库 protocol 之后，回调与同步回跳才知道该用哪把密钥签名（设计 §6.4 / §6.6）。
				Protocol:          merchant.ProtocolEPayV1,
				OrderID:           orderID,
				PublicToken:       publicToken,
				PayID:             input.OutTradeNo,
				Type:              paymentType,
				EpayType:          input.Type,
				MerchantOrderName: orderName,
				PriceCents:        priceCents,
				ReallyPriceCents:  reallyPriceCents,
				// price_text 存商户原文：查单与回调都要原样回显，改写即改变对方签名域。
				PriceText: input.Money,
				// really_price_text 是我方实收，用格式化值，仅供内部与前端展示。
				ReallyPriceText: order.FormatCents(reallyPriceCents),
				State:           order.StatusPending,
				Param:           input.Param,
				PayURL:          payURL,
				IsAuto:          isAuto,
				// notify_url 必填且**不回落**全局设置：回落会把 A 商户的回调发到 B 商户。
				NotifyURL: input.NotifyURL,
				ReturnURL: input.ReturnURL,
				CreatedAt: now,
			}
			persisted, createErr := s.orderRepo.Create(txCtx, value)
			if errors.Is(createErr, port.ErrPublicTokenConflict) {
				return port.ErrPublicTokenConflict
			}
			if createErr != nil {
				// 文案不在这里定：撞 uq_orders_merchant_pay_id(1062) 或死锁(1213) 的常见成因
				// 是「另一个并发请求刚插入了同一个 (merchant_id, out_trade_no)」，
				// 必须出事务后重新查一次，命中就回放同一个 payurl（设计 §5.4）。
				return epayCreateConflict{cause: createErr}
			}
			created = persisted
			return nil
		})
		if errors.Is(err, port.ErrPublicTokenConflict) {
			continue
		}
		var conflict epayCreateConflict
		if errors.As(err, &conflict) {
			// 并发幂等：行不存在时两个事务的间隙锁互不冲突，都能穿过上面的
			// FindByPayIDForUpdate 检查，后到者只会在写库时撞唯一索引/死锁。
			// 此时在同一请求内重查一次，命中则回放，**不再占一次金额锁**。
			replayedOrder, found, replayErr := s.replayAfterCreateConflict(ctx, input, paymentType, priceCents)
			if replayErr != nil {
				return EPayCreateResult{}, replayErr
			}
			if found {
				return s.resultOf(replayedOrder), nil
			}
			// 查不到：真的不是同一笔订单，按原错误上抛（唯一索引 → 冲突，其余 → 依赖失败）。
			if errors.Is(conflict.cause, port.ErrConflict) {
				return EPayCreateResult{}, fail(CodeConflict, "创建订单冲突")
			}
			return EPayCreateResult{}, wrap(CodeDependency, "创建订单失败", conflict.cause)
		}
		if err != nil {
			return EPayCreateResult{}, err
		}
		if replayed.OrderID != "" {
			return s.resultOf(replayed), nil
		}
		return s.resultOf(created), nil
	}
	return EPayCreateResult{}, fail(CodeDependency, "生成唯一公开订单令牌失败")
}

// resultOf 把订单转成 EPay 响应字段。
// 命名陷阱：结果的 PayURL 是收银台链接，Qrcode 才是 orders.pay_url 里的收款码内容。
func (s *EPayService) resultOf(value order.Order) EPayCreateResult {
	return EPayCreateResult{
		TradeNo: value.OrderID,
		PayURL:  s.frontendURL + "/#/payment/" + value.PublicToken,
		Qrcode:  value.PayURL,
		IsAuto:  value.IsAuto,
	}
}

// Query 按 (merchant_id, out_trade_no) 查单；跨商户查不到，表现与「不存在」一致。
func (s *EPayService) Query(ctx context.Context, merchantID int64, outTradeNo string) (EPayQueryResult, error) {
	if merchantID <= 0 || outTradeNo == "" {
		return EPayQueryResult{}, fail(CodeInvalidArgument, "参数不完整")
	}

	value, err := s.orderRepo.FindByPayID(ctx, merchantID, outTradeNo)
	if errors.Is(err, port.ErrNotFound) {
		return EPayQueryResult{}, fail(CodeNotFound, "订单不存在")
	}
	if err != nil {
		return EPayQueryResult{}, wrap(CodeDependency, "查询订单失败", err)
	}

	epayType, _ := orderEPayType(value)
	return EPayQueryResult{
		TradeNo:     value.OrderID,
		OutTradeNo:  value.PayID,
		Type:        epayType,
		Name:        truncateEPayName(value.MerchantOrderName),
		Money:       amountText(value.PriceText, value.PriceCents),
		TradeStatus: epayTradeStatus(value.State),
		AddTime:     value.CreatedAt,
		EndTime:     epayEndTime(value),
		Param:       value.Param,
	}, nil
}

// acquirePriceForEPay 是 EPay 专用取金额路径。
//
// requireExact=false 时直接复用原生 acquirePrice（同一条资金逻辑），
// 唯一区别是方向写死为 Increase，不读 settings.payQf。
// requireExact=true 时在「拿锁之前」要求该金额存在精确码：
// 微调到没有对应收款码的价位，等于让用户扫一个金额不符的码，到账永远匹配不上（设计 §3.2）。
func (s *EPayService) acquirePriceForEPay(ctx context.Context, orderID string, paymentType payment.Type, baseCents int64, requireExact bool) (int64, error) {
	if !requireExact {
		return s.orders.acquirePrice(ctx, orderID, paymentType, baseCents, epayAdjustIncrease)
	}

	for _, candidate := range order.CandidateAmounts(baseCents, order.Increase, priceLockAttempts) {
		switch _, err := s.qrcodes.FindEnabledByAmount(ctx, paymentType, candidate); {
		case err == nil:
		case errors.Is(err, port.ErrNotFound):
			// 该价位没有精确码：跳过，不要占锁，否则这个金额会被白白占死。
			continue
		default:
			return 0, wrap(CodeDependency, "查询支付二维码失败", err)
		}

		acquired, err := s.priceLocks.TryAcquire(ctx, orderID, paymentType, candidate)
		if err != nil {
			return 0, wrap(CodeDependency, "占用订单金额失败", err)
		}
		if acquired {
			return candidate, nil
		}
	}
	// 全部候选都没有精确码或都被占用，对商户是同一件事：这个价位现在收不了钱。
	return 0, fail(CodeOverloaded, "该金额正在支付中，请稍后重试")
}

// validateEPayCreateInput 只做校验与映射，不产生任何副作用（不写库、不占锁）。
// 顺序是「先便宜后昂贵」：本地判完参数/金额/类型，最后才进入需要查库或解密的分支。
func validateEPayCreateInput(input EPayCreateInput) (payment.Type, int64, error) {
	if input.MerchantID <= 0 {
		return 0, 0, fail(CodeInvalidArgument, "商户不存在")
	}
	if input.Protocol != merchant.ProtocolEPayV1 {
		return 0, 0, fail(CodeConfiguration, "该商户未开通 V1 接口")
	}
	if input.OutTradeNo == "" || len(input.OutTradeNo) > epayOutTradeNoMaxLen {
		return 0, 0, fail(CodeInvalidArgument, "out_trade_no 缺失或超长")
	}
	if len(input.Param) > epayParamMaxLen {
		return 0, 0, fail(CodeInvalidArgument, "param 超出长度限制")
	}

	// 严格按 EPay 的 money 形态解析（^[0-9]{1,10}(\.[0-9]{1,2})?$）：
	// 宽松解析会放进 "1e2"、" 1.00" 这类原文，而它们进不了对方签名域。
	priceCents, err := epay.ValidateAmount(input.Money)
	if err != nil || priceCents <= 0 {
		return 0, 0, fail(CodeInvalidArgument, "金额格式错误")
	}

	paymentType, ok := epay.TypeFromEPay(input.Type)
	if !ok {
		// 未开通渠道必须明确拒绝，绝不回落到微信/支付宝：
		// 挂机端只监听这两个渠道，回落会让「付了钱但类型不匹配」变成无法核销的掉单。
		return 0, 0, fail(CodeInvalidArgument, "支付方式未开通")
	}

	if strings.TrimSpace(input.NotifyURL) == "" {
		return 0, 0, fail(CodeInvalidArgument, "notify_url 必须是公网可访问地址")
	}
	if input.ReturnURL != "" && !validCallbackURL(input.ReturnURL) {
		return 0, 0, fail(CodeInvalidArgument, "return_url 只允许 http/https 地址")
	}
	return paymentType, priceCents, nil
}

// epayCreateConflict 标记「写订单失败」这一具体来源，供出事务后做并发回放。
//
// 为什么不直接返回 port.ErrConflict：唯一索引冲突只是「可能是同一笔订单」的线索，
// 还要区分「撞的是 uq_orders_merchant_pay_id」与「撞的是 order_id/public_token 之外的
// 唯一键」，而这两类的最终文案不同（CodeConflict 与 CodeDependency）。
// 真正的判据不是错误码，而是「重查能否查到同一笔订单」。
type epayCreateConflict struct{ cause error }

func (e epayCreateConflict) Error() string { return "创建订单失败: " + e.cause.Error() }

func (e epayCreateConflict) Unwrap() error { return e.cause }

const (
	// epayConflictReplayAttempts/Delay 控制「撞冲突后重查」的重试：
	// 死锁(1213)时赢家事务可能尚未提交，立刻查会查不到，极短重试即可覆盖
	// （MySQL 的 1062 只会在对方提交后返回，因此 1062 路径一次就够）。
	epayConflictReplayAttempts = 3
	epayConflictReplayDelay    = 10 * time.Millisecond
)

// replayAfterCreateConflict 在写库撞唯一索引(1062)/死锁(1213)后，于同一请求内
// 重新按 (merchant_id, out_trade_no) 查一次：命中且金额/类型一致就回放原订单。
//
// 为什么不在事务里重查：事务已经回滚，锁也随回滚释放；在**新**的读里才能看到
// 赢家事务提交的行。整个过程不碰 price_locks —— 回放绝不能重新 acquirePrice
// （会撞 uq_price_locks_order_id 并推进到下一个候选金额，导致金额漂移）。
func (s *EPayService) replayAfterCreateConflict(
	ctx context.Context,
	input EPayCreateInput,
	paymentType payment.Type,
	priceCents int64,
) (order.Order, bool, error) {
	for attempt := 0; ; attempt++ {
		existing, err := s.orderRepo.FindByPayID(ctx, input.MerchantID, input.OutTradeNo)
		switch {
		case err == nil:
			// 金额或类型不一致时 checkEPayReplay 会明确拒绝：绝不静默按旧金额回放（等于替商户改价）。
			if matchErr := checkEPayReplay(existing, paymentType, priceCents); matchErr != nil {
				return order.Order{}, false, matchErr
			}
			return existing, true, nil
		case errors.Is(err, port.ErrNotFound):
			if attempt+1 >= epayConflictReplayAttempts {
				return order.Order{}, false, nil
			}
			time.Sleep(epayConflictReplayDelay)
		default:
			return order.Order{}, false, wrap(CodeDependency, "查询订单失败", err)
		}
	}
}

// checkEPayReplay 校验幂等命中的订单是否与本次请求一致。
// 金额或支付方式不一致必须明确拒绝：静默按旧金额返回等于替商户改价。
func checkEPayReplay(existing order.Order, paymentType payment.Type, priceCents int64) error {
	if existing.PriceCents != priceCents || existing.Type != paymentType {
		return fail(CodeDuplicateOrder, "商户订单号已存在且金额不一致")
	}
	if existing.State == order.StatusClosed {
		// 已关闭订单不能再付：令牌还在，但收款码可能已失效，让商户重新下单。
		return fail(CodeInvalidState, "订单已关闭，请重新下单")
	}
	return nil
}

// epayTradeStatus 只有两个对外状态（设计 §5.3）：
// 已支付与「通知失败」都算成功——钱已到账，通知失败是我方内部问题。
func epayTradeStatus(state order.Status) string {
	if state == order.StatusPaid || state == order.StatusNotifyFailed {
		return "TRADE_SUCCESS"
	}
	return "WAIT_BUYER_PAY"
}

// epayEndTime 取结束时刻：已支付取 paid_at，已关闭取 closed_at，其余留零值（响应里留空）。
func epayEndTime(value order.Order) time.Time {
	if !value.PaidAt.IsZero() {
		return value.PaidAt
	}
	if !value.ClosedAt.IsZero() {
		return value.ClosedAt
	}
	return time.Time{}
}

// exactCodeRequired 解析「严格等额」开关：缺省开启。
// 只有显式 0/false/off/no 才关闭——这个开关出错的方向是掉单，默认值必须偏保守。
func exactCodeRequired(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}
