package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hulisang/vmqfox-backend/internal/domain/epay"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/domain/order"
	"github.com/hulisang/vmqfox-backend/internal/domain/payment"
	"github.com/hulisang/vmqfox-backend/internal/domain/qrcode"
	"github.com/hulisang/vmqfox-backend/internal/domain/secret"
	"github.com/hulisang/vmqfox-backend/internal/domain/setting"
	"github.com/hulisang/vmqfox-backend/internal/port"
)

const (
	defaultCloseMinutes       = 5
	priceLockAttempts         = 10
	publicTokenCreateRetryMax = 8
)

type CreateOrderInput struct {
	PayID     string
	Param     string
	Type      string
	Price     string
	Sign      string
	NotifyURL string
	ReturnURL string
}

type CreateOrderResult struct {
	Order          order.Order
	Price          string
	ReallyPrice    string
	TimeoutMinutes int
	RedirectURL    string
}

type OrderView struct {
	Order            order.Order
	StateText        string
	TimeoutMinutes   int
	RemainingSeconds int64
	// AlipayScheme 是支付宝转账确认单深度链接；仅当支付方式为支付宝且后台
	// 配置了收款账号时非空，其余情况为空串，收银台据此退回二维码扫码。
	AlipayScheme string
}

type ListOrdersInput struct {
	State *order.Status
	Page  int
	Limit int
}

type OrderPageView struct {
	Items []order.Order
	Total int64
}

type OrderStatisticsView struct {
	TodayOrders       int64
	TodayPaidOrders   int64
	TodayClosedOrders int64
	TodayPaidCents    int64
	TotalOrders       int64
	TotalPaidCents    int64
}

type CheckOrderResult struct {
	State            order.Status
	RemainingSeconds int64
	Message          string
}

type ReturnURLResult struct {
	ReturnURL string
}

type OrderServiceDeps struct {
	Transactions port.TransactionManager
	Orders       port.OrderRepository
	PriceLocks   port.PriceLockRepository
	QRCodes      port.QRCodeRepository
	Settings     port.SettingRepository
	Outbox       port.OutboxRepository
	Clock        port.Clock
	OrderIDs     port.OrderIDGenerator
	PublicTokens port.PublicTokenGenerator
	// Merchants / MerchantKeys / MasterKeys 只服务 EPay 订单的同步回跳签名
	// （protocol=1 用商户 V1 密钥，protocol=2 用商户平台私钥）。原生回跳不碰它们。
	Merchants    port.MerchantRepository
	MerchantKeys port.MerchantKeyRepository
	MasterKeys   *secret.Box
	FrontendURL  string
}

type OrderService struct {
	transactions port.TransactionManager
	orders       port.OrderRepository
	priceLocks   port.PriceLockRepository
	qrcodes      port.QRCodeRepository
	settings     port.SettingRepository
	outbox       port.OutboxRepository
	clock        port.Clock
	orderIDs     port.OrderIDGenerator
	publicTokens port.PublicTokenGenerator
	merchants    port.MerchantRepository
	merchantKeys port.MerchantKeyRepository
	masterKeys   *secret.Box
	frontendURL  string
}

func NewOrderService(deps OrderServiceDeps) (*OrderService, error) {
	if deps.Transactions == nil || deps.Orders == nil || deps.PriceLocks == nil || deps.QRCodes == nil ||
		deps.Settings == nil || deps.Outbox == nil || deps.Clock == nil || deps.OrderIDs == nil || deps.PublicTokens == nil {
		return nil, errors.New("订单用例依赖不完整")
	}
	return &OrderService{
		transactions: deps.Transactions,
		orders:       deps.Orders,
		priceLocks:   deps.PriceLocks,
		qrcodes:      deps.QRCodes,
		settings:     deps.Settings,
		outbox:       deps.Outbox,
		clock:        deps.Clock,
		orderIDs:     deps.OrderIDs,
		publicTokens: deps.PublicTokens,
		merchants:    deps.Merchants,
		merchantKeys: deps.MerchantKeys,
		masterKeys:   deps.MasterKeys,
		frontendURL:  strings.TrimRight(deps.FrontendURL, "/"),
	}, nil
}

// newPublicToken 统一校验随机令牌生成器，确保所有创建路径都不会写入空或格式错误的公开凭据。
func newPublicToken(generator port.PublicTokenGenerator) (string, error) {
	token, err := generator.NewPublicToken()
	if err != nil {
		return "", err
	}
	if !order.IsValidPublicToken(token) {
		return "", errors.New("生成的公开订单令牌格式无效")
	}
	return token, nil
}

func (s *OrderService) Create(ctx context.Context, input CreateOrderInput) (CreateOrderResult, error) {
	if input.PayID == "" || input.Type == "" || input.Price == "" || input.Sign == "" {
		return CreateOrderResult{}, fail(CodeInvalidArgument, "参数不完整")
	}
	// 落库字段有长度上限（pay_id VARBINARY(255)、param VARCHAR(255)），
	// 在用例层提前拒绝，避免严格模式下变成 500 依赖错误。
	if len(input.PayID) > 255 || len(input.Param) > 255 || len(input.Price) > 64 {
		return CreateOrderResult{}, fail(CodeInvalidArgument, "参数长度超出限制")
	}

	typeNumber, err := strconv.Atoi(input.Type)
	if err != nil || !payment.Type(typeNumber).Valid() {
		return CreateOrderResult{}, fail(CodeInvalidArgument, "支付类型错误")
	}
	paymentType := payment.Type(typeNumber)
	priceCents, err := order.ParseAmountCents(input.Price)
	if err != nil || priceCents <= 0 {
		return CreateOrderResult{}, fail(CodeInvalidArgument, "价格错误")
	}

	settings, err := s.settings.GetMany(ctx, []string{
		setting.MerchantKey,
		setting.MonitorStateKey,
		setting.PriceAdjustKey,
		setting.NotifyURLKey,
		setting.ReturnURLKey,
		setting.WechatPayURLKey,
		setting.AlipayPayURLKey,
		setting.CloseMinutesKey,
	})
	if err != nil {
		return CreateOrderResult{}, wrap(CodeDependency, "读取系统设置失败", err)
	}
	merchantKey := settings[setting.MerchantKey]
	if merchantKey == "" {
		return CreateOrderResult{}, fail(CodeConfiguration, "系统未配置密钥")
	}
	if !validCallbackURL(input.NotifyURL) || !validCallbackURL(input.ReturnURL) {
		return CreateOrderResult{}, fail(CodeInvalidArgument, "notifyUrl 与 returnUrl 只允许 http/https 地址")
	}
	if !validCreateSign(input, merchantKey) {
		if usesLegacyCreateSign(input, merchantKey) {
			return CreateOrderResult{}, fail(CodeDeprecatedSignAlgo, "检测到已废弃的 v1 签名，请升级商户插件至 v2（HMAC-SHA-256，签名域包含 notifyUrl 与 returnUrl）")
		}
		return CreateOrderResult{}, fail(CodeInvalidSignature, "签名错误")
	}
	if settings[setting.MonitorStateKey] != "1" {
		return CreateOrderResult{}, fail(CodeMonitorOffline, "监控端状态异常，请检查")
	}

	now := s.clock.Now()
	orderID, err := s.orderIDs.NewOrderID(now)
	if err != nil {
		return CreateOrderResult{}, wrap(CodeDependency, "生成订单号失败", err)
	}
	var created order.Order
	for attempt := 0; attempt < publicTokenCreateRetryMax; attempt++ {
		publicToken, tokenErr := newPublicToken(s.publicTokens)
		if tokenErr != nil {
			return CreateOrderResult{}, wrap(CodeDependency, "生成公开订单令牌失败", tokenErr)
		}

		err = s.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
			// 原生接口固定是系统商户（merchant_id=0），重复单号检查也只在这个维度内成立。
			if _, findErr := s.orders.FindByPayIDForUpdate(txCtx, 0, input.PayID); findErr == nil {
				return fail(CodeDuplicateOrder, "商户订单号已存在，请勿重复提交")
			} else if !errors.Is(findErr, port.ErrNotFound) {
				return wrap(CodeDependency, "检查商户订单号失败", findErr)
			}

			reallyPriceCents, lockErr := s.acquirePrice(txCtx, orderID, paymentType, priceCents, settings[setting.PriceAdjustKey])
			if lockErr != nil {
				return lockErr
			}

			payURL, isAuto, payURLErr := s.resolvePayURL(txCtx, paymentType, reallyPriceCents, settings)
			if payURLErr != nil {
				return payURLErr
			}

			notifyURL := input.NotifyURL
			if notifyURL == "" {
				notifyURL = settings[setting.NotifyURLKey]
			}
			returnURL := input.ReturnURL
			if returnURL == "" {
				returnURL = settings[setting.ReturnURLKey]
			}
			if !validCallbackURL(notifyURL) || !validCallbackURL(returnURL) {
				return fail(CodeConfiguration, "系统配置的 notifyUrl 或 returnUrl 无效")
			}

			value := order.Order{
				OrderID:          orderID,
				PublicToken:      publicToken,
				PayID:            input.PayID,
				Type:             paymentType,
				PriceCents:       priceCents,
				ReallyPriceCents: reallyPriceCents,
				PriceText:        input.Price,
				ReallyPriceText:  order.FormatCents(reallyPriceCents),
				State:            order.StatusPending,
				Param:            input.Param,
				PayURL:           payURL,
				IsAuto:           isAuto,
				NotifyURL:        notifyURL,
				ReturnURL:        returnURL,
				CreatedAt:        now,
			}
			created, err = s.orders.Create(txCtx, value)
			if errors.Is(err, port.ErrPublicTokenConflict) {
				return port.ErrPublicTokenConflict
			}
			if errors.Is(err, port.ErrConflict) {
				return fail(CodeConflict, "创建订单冲突")
			}
			if err != nil {
				return wrap(CodeDependency, "创建订单失败", err)
			}
			return nil
		})
		if errors.Is(err, port.ErrPublicTokenConflict) {
			continue
		}
		if err != nil {
			return CreateOrderResult{}, err
		}
		break
	}
	if errors.Is(err, port.ErrPublicTokenConflict) {
		return CreateOrderResult{}, fail(CodeDependency, "生成唯一公开订单令牌失败")
	}

	redirectURL := ""
	if s.frontendURL != "" {
		// 公开收银台链接只携带随机令牌，不再把可预测的内部订单号当作凭据。
		redirectURL = s.frontendURL + "/#/payment/" + created.PublicToken
	}
	return CreateOrderResult{
		Order:          created,
		Price:          input.Price,
		ReallyPrice:    order.FormatCents(created.ReallyPriceCents),
		TimeoutMinutes: closeMinutes(settings[setting.CloseMinutesKey]),
		RedirectURL:    redirectURL,
	}, nil
}

func (s *OrderService) Get(ctx context.Context, publicToken string) (OrderView, error) {
	value, err := s.findPublicOrder(ctx, publicToken)
	if err != nil {
		return OrderView{}, err
	}
	minutes, err := s.readCloseMinutes(ctx)
	if err != nil {
		return OrderView{}, err
	}
	view := orderView(value, minutes, s.clock.Now())
	view.AlipayScheme = s.alipayTransferScheme(ctx, value)
	return view, nil
}

// alipayTransferScheme 生成支付宝转账确认单链接，金额取实付金额（reallyPrice）——
// 与到账匹配使用的金额保持一致，避免预填金额与实际应付款不符。
// 任何一步缺失都返回空串，让收银台安静地退回扫码，不向用户暴露配置缺失。
func (s *OrderService) alipayTransferScheme(ctx context.Context, value order.Order) string {
	if value.Type != payment.Alipay {
		return ""
	}
	account, err := s.settings.Get(ctx, setting.AlipayAccountKey)
	if err != nil {
		return ""
	}
	return payment.AlipayTransferScheme(account, order.FormatCents(value.ReallyPriceCents))
}

// findPublicOrder 统一公开订单令牌校验与查询；格式错误和不存在都返回相同结果，避免形成订单存在性探针。
func (s *OrderService) findPublicOrder(ctx context.Context, publicToken string) (order.Order, error) {
	publicToken = strings.TrimSpace(publicToken)
	if !order.IsValidPublicToken(publicToken) {
		return order.Order{}, fail(CodeNotFound, "订单不存在")
	}
	value, err := s.orders.FindByPublicToken(ctx, publicToken)
	if errors.Is(err, port.ErrNotFound) {
		return order.Order{}, fail(CodeNotFound, "订单不存在")
	}
	if err != nil {
		return order.Order{}, wrap(CodeDependency, "查询订单失败", err)
	}
	return value, nil
}

func (s *OrderService) List(ctx context.Context, input ListOrdersInput) (OrderPageView, error) {
	if input.State != nil && !input.State.Valid() {
		return OrderPageView{}, fail(CodeInvalidArgument, "订单状态无效")
	}
	page, err := s.orders.List(ctx, port.OrderFilter{
		State: input.State,
		Page:  input.Page,
		Limit: input.Limit,
	})
	if err != nil {
		return OrderPageView{}, wrap(CodeDependency, "查询订单列表失败", err)
	}
	return OrderPageView{Items: page.Items, Total: page.Total}, nil
}

func (s *OrderService) Statistics(ctx context.Context) (OrderStatisticsView, error) {
	now := s.clock.Now()
	year, month, day := now.Date()
	dayStart := time.Date(year, month, day, 0, 0, 0, 0, now.Location())
	statistics, err := s.orders.Statistics(ctx, dayStart, dayStart.AddDate(0, 0, 1))
	if err != nil {
		return OrderStatisticsView{}, wrap(CodeDependency, "查询订单统计失败", err)
	}
	return OrderStatisticsView{
		TodayOrders:       statistics.TodayOrders,
		TodayPaidOrders:   statistics.TodayPaidOrders,
		TodayClosedOrders: statistics.TodayClosedOrders,
		TodayPaidCents:    statistics.TodayPaidCents,
		TotalOrders:       statistics.TotalOrders,
		TotalPaidCents:    statistics.TotalPaidCents,
	}, nil
}

func (s *OrderService) Detail(ctx context.Context, id int64) (order.Order, error) {
	if id <= 0 {
		return order.Order{}, fail(CodeInvalidArgument, "订单ID无效")
	}
	value, err := s.orders.FindByID(ctx, id)
	if errors.Is(err, port.ErrNotFound) {
		return order.Order{}, fail(CodeNotFound, "订单不存在")
	}
	if err != nil {
		return order.Order{}, wrap(CodeDependency, "查询订单失败", err)
	}
	return value, nil
}

// Check 只计算对外可见状态，持久化过期状态由生命周期任务统一处理。
func (s *OrderService) Check(ctx context.Context, publicToken string) (CheckOrderResult, error) {
	value, err := s.findPublicOrder(ctx, publicToken)
	if err != nil {
		return CheckOrderResult{}, err
	}
	minutes, err := s.readCloseMinutes(ctx)
	if err != nil {
		return CheckOrderResult{}, err
	}
	return checkResult(value, minutes, s.clock.Now()), nil
}

func (s *OrderService) ReturnURL(ctx context.Context, publicToken string) (ReturnURLResult, error) {
	value, err := s.findPublicOrder(ctx, publicToken)
	if err != nil {
		return ReturnURLResult{}, err
	}
	if value.ReturnURL == "" {
		return ReturnURLResult{}, fail(CodeConfiguration, "订单没有配置返回URL")
	}
	if !validCallbackURL(value.ReturnURL) {
		return ReturnURLResult{}, fail(CodeConfiguration, "订单返回URL配置无效")
	}
	// 安全加固：仅当订单已处于已支付或通知失败状态时，才允许生成带签名的回跳地址，防止未支付订单骗取签名
	if value.State != order.StatusPaid && value.State != order.StatusNotifyFailed {
		return ReturnURLResult{}, fail(CodeInvalidState, "订单尚未支付，无法生成回跳签名")
	}

	switch value.Protocol {
	case merchant.ProtocolEPayV1, merchant.ProtocolEPayV2:
		// EPay 订单必须按商户协议签名：拿 settings.key 签原生字段，商户验签必然失败，
		// 用户付了钱却被回跳到「支付失败」页（设计 §6.4）。
		signed, signErr := s.epayReturnURL(ctx, value)
		if signErr != nil {
			return ReturnURLResult{}, signErr
		}
		return ReturnURLResult{ReturnURL: signed}, nil
	}

	merchantKey, err := s.settings.Get(ctx, setting.MerchantKey)
	if errors.Is(err, port.ErrNotFound) || merchantKey == "" {
		return ReturnURLResult{}, fail(CodeConfiguration, "系统未配置密钥")
	}
	if err != nil {
		return ReturnURLResult{}, wrap(CodeDependency, "读取系统密钥失败", err)
	}

	typeText := strconv.Itoa(int(value.Type))
	price := amountText(value.PriceText, value.PriceCents)
	reallyPrice := amountText(value.ReallyPriceText, value.ReallyPriceCents)
	signNew := payment.CallbackSignV2(value.PayID, value.Param, typeText, price, reallyPrice, merchantKey)
	baseQuery := "payId=" + url.QueryEscape(value.PayID) +
		"&param=" + url.QueryEscape(value.Param) +
		"&type=" + url.QueryEscape(typeText) +
		"&price=" + url.QueryEscape(price) +
		"&reallyPrice=" + url.QueryEscape(reallyPrice)
	newURL := appendReturnQuery(value.ReturnURL, baseQuery+"&sign="+url.QueryEscape(signNew))

	// 回跳签名已嵌入 URL 查询参数，匿名接口不再重复暴露独立签名字段。
	return ReturnURLResult{ReturnURL: newURL}, nil
}

// epayReturnURL 生成 EPay 订单的同步回跳地址。
// 字段构造与回调共用 epayNotifyFields，避免「回调验签通过、回跳验签失败」两套逻辑漂移。
func (s *OrderService) epayReturnURL(ctx context.Context, value order.Order) (string, error) {
	merchantNo, err := merchantNoFor(ctx, s.merchants, value.MerchantID)
	if err != nil {
		return "", wrap(CodeConfiguration, "生成 EPay 回跳地址失败", err)
	}
	fields, fieldErr := epayNotifyFields(value, merchantNo)
	if fieldErr != nil {
		return "", wrap(CodeConfiguration, "生成 EPay 回跳地址失败", fieldErr)
	}

	if value.Protocol == merchant.ProtocolEPayV2 {
		privatePEM, keyErr := merchantV2PrivateKey(ctx, s.merchantKeys, s.masterKeys, value.MerchantID)
		if keyErr != nil {
			return "", wrap(CodeConfiguration, "生成 EPay V2 回跳签名失败", keyErr)
		}
		signed := cloneEPayFields(fields)
		epay.AppendTimestamp(signed, s.clock.Now())
		sign, signErr := epay.SignV2(signed, privatePEM)
		if signErr != nil {
			return "", wrap(CodeConfiguration, "生成 EPay V2 回跳签名失败", signErr)
		}
		return appendReturnQuery(value.ReturnURL, encodeEPayForm(signed, sign, "RSA")), nil
	}

	secretText, keyErr := merchantV1Secret(ctx, s.merchantKeys, s.masterKeys, value.MerchantID)
	if keyErr != nil {
		return "", wrap(CodeConfiguration, "生成 EPay V1 回跳签名失败", keyErr)
	}
	sign, signErr := epay.SignV1(fields, secretText)
	if signErr != nil {
		return "", wrap(CodeConfiguration, "生成 EPay V1 回跳签名失败", signErr)
	}
	return appendReturnQuery(value.ReturnURL, encodeEPayForm(fields, sign, "MD5")), nil
}

func appendReturnQuery(baseURL, query string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return baseURL + "?" + query
	}
	// 直接拼接 RawQuery 而不是字符串尾部追加，保证签名参数落在 fragment 之前。
	if parsed.RawQuery == "" {
		parsed.RawQuery = query
	} else {
		parsed.RawQuery = parsed.RawQuery + "&" + query
	}
	return parsed.String()
}

func (s *OrderService) CloseByID(ctx context.Context, id int64) error {
	if id <= 0 {
		return fail(CodeInvalidArgument, "订单ID无效")
	}
	return s.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
		value, err := s.orders.FindByIDForUpdate(txCtx, id)
		if errors.Is(err, port.ErrNotFound) {
			return fail(CodeNotFound, "订单不存在")
		}
		if err != nil {
			return wrap(CodeDependency, "查询订单失败", err)
		}
		if value.State != order.StatusPending {
			return fail(CodeInvalidState, "只能关闭未支付的订单")
		}
		changed, err := s.orders.Transition(txCtx, value.ID, order.StatusPending, order.StatusClosed, s.clock.Now())
		if err != nil {
			return wrap(CodeDependency, "关闭订单失败", err)
		}
		if !changed {
			return fail(CodeConflict, "订单状态已变化，请重试")
		}
		if err := s.priceLocks.ReleaseByOrderID(txCtx, value.OrderID); err != nil {
			return wrap(CodeDependency, "释放订单金额失败", err)
		}
		return nil
	})
}

func (s *OrderService) acquirePrice(ctx context.Context, orderID string, paymentType payment.Type, baseCents int64, rawAdjustment string) (int64, error) {
	adjustment := order.NoAdjustment
	switch rawAdjustment {
	case "1":
		adjustment = order.Increase
	case "2":
		adjustment = order.Decrease
	}
	for _, candidate := range order.CandidateAmounts(baseCents, adjustment, priceLockAttempts) {
		acquired, err := s.priceLocks.TryAcquire(ctx, orderID, paymentType, candidate)
		if err != nil {
			return 0, wrap(CodeDependency, "占用订单金额失败", err)
		}
		if acquired {
			return candidate, nil
		}
	}
	return 0, fail(CodeOverloaded, "订单超出负荷，请稍后重试")
}

// resolvePayURL 按「精确金额固定码 → 码库通用码 → 系统设置兜底码」的顺序选择收款地址。
func (s *OrderService) resolvePayURL(ctx context.Context, paymentType payment.Type, amountCents int64, settings map[string]string) (string, bool, error) {
	fixed, err := s.qrcodes.FindEnabledByAmount(ctx, paymentType, amountCents)
	if err == nil && fixed.PayURL != "" {
		return fixed.PayURL, false, nil
	}
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		return "", false, wrap(CodeDependency, "查询支付二维码失败", err)
	}

	// 通用码不含金额，付款人需手动输入，因此与系统设置兜底码一样标记为 isAuto。
	generic, err := s.qrcodes.FindEnabledByAmount(ctx, paymentType, qrcode.AnyAmountCents)
	if err == nil && generic.PayURL != "" {
		return generic.PayURL, true, nil
	}
	if err != nil && !errors.Is(err, port.ErrNotFound) {
		return "", false, wrap(CodeDependency, "查询通用支付二维码失败", err)
	}

	key := setting.AlipayPayURLKey
	method := "支付宝"
	if paymentType == payment.Wechat {
		key = setting.WechatPayURLKey
		method = "微信"
	}
	if settings[key] == "" {
		return "", false, fail(CodeConfiguration, "暂无可用支付二维码，请在后台【系统设置】或【"+method+"二维码】中配置")
	}
	return settings[key], true, nil
}

func (s *OrderService) readCloseMinutes(ctx context.Context) (int, error) {
	value, err := s.settings.Get(ctx, setting.CloseMinutesKey)
	if errors.Is(err, port.ErrNotFound) {
		return defaultCloseMinutes, nil
	}
	if err != nil {
		return 0, wrap(CodeDependency, "读取订单超时设置失败", err)
	}
	return closeMinutes(value), nil
}

func validCreateSign(input CreateOrderInput, key string) bool {
	expected := payment.CreateSignV2(
		input.PayID,
		input.Param,
		input.Type,
		input.Price,
		input.NotifyURL,
		input.ReturnURL,
		key,
	)
	return payment.SignEqual(input.Sign, expected)
}

// usesLegacyCreateSign 用于把「旧 SDK 未升级」与「密钥配错」区分开，只影响报错文案，不构成放行路径。
func usesLegacyCreateSign(input CreateOrderInput, key string) bool {
	legacy := payment.LegacyCreateSign(input.PayID, input.Param, input.Type, input.Price, key)
	return payment.SignEqual(input.Sign, legacy)
}

// validCallbackURL 限制回调与回跳地址只能是 http(s)，阻断 javascript: 等伪协议经支付页跳转执行。
func validCallbackURL(raw string) bool {
	if raw == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func closeMinutes(value string) int {
	minutes, err := strconv.Atoi(value)
	if err != nil || minutes <= 0 {
		return defaultCloseMinutes
	}
	return minutes
}

func orderView(value order.Order, minutes int, now time.Time) OrderView {
	return OrderView{
		Order:            value,
		StateText:        stateText(value.State),
		TimeoutMinutes:   minutes,
		RemainingSeconds: remainingSeconds(value.CreatedAt, minutes, now),
	}
}

func checkResult(value order.Order, minutes int, now time.Time) CheckOrderResult {
	result := CheckOrderResult{State: value.State}
	switch value.State {
	case order.StatusPaid, order.StatusNotifyFailed:
		result.Message = "支付成功"
	case order.StatusClosed:
		result.RemainingSeconds = 0
		result.Message = "订单已过期"
	default:
		result.RemainingSeconds = remainingSeconds(value.CreatedAt, minutes, now)
		if order.IsExpired(value.State, value.CreatedAt, time.Duration(minutes)*time.Minute, now) {
			result.State = order.StatusClosed
			result.RemainingSeconds = 0
			result.Message = "订单已过期"
		} else {
			result.Message = "订单未支付"
		}
	}
	return result
}

func remainingSeconds(createdAt time.Time, minutes int, now time.Time) int64 {
	remaining := createdAt.Add(time.Duration(minutes)*time.Minute).Unix() - now.Unix()
	if remaining < 0 {
		return 0
	}
	return remaining
}

func stateText(status order.Status) string {
	switch status {
	case order.StatusClosed:
		return "已关闭"
	case order.StatusPending:
		return "未支付"
	case order.StatusPaid:
		return "已支付"
	case order.StatusNotifyFailed:
		return "通知失败"
	default:
		return "未知状态"
	}
}

func (s *OrderService) DeleteByID(ctx context.Context, id int64) error {
	if id <= 0 {
		return fail(CodeInvalidArgument, "订单ID无效")
	}
	return s.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
		value, err := s.orders.FindByIDForUpdate(txCtx, id)
		if errors.Is(err, port.ErrNotFound) {
			return fail(CodeNotFound, "订单不存在")
		}
		if err != nil {
			return wrap(CodeDependency, "查询订单失败", err)
		}

		if value.State == order.StatusPending {
			if err := s.priceLocks.ReleaseByOrderID(txCtx, value.OrderID); err != nil {
				return wrap(CodeDependency, "释放订单价格锁失败", err)
			}
		}

		_, err = s.orders.Delete(txCtx, id)
		if err != nil {
			return wrap(CodeDependency, "删除订单失败", err)
		}
		return nil
	})
}

func (s *OrderService) DeleteLast(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, fail(CodeInvalidArgument, "清理时间范围无效")
	}
	now := s.clock.Now()
	cutoff := now.Add(-olderThan)

	var count int64
	err := s.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
		// 先按订单维度释放价格锁，再删除订单：两者使用同一个 cutoff，不会留下孤儿锁。
		if err := s.priceLocks.ReleaseByOrdersBefore(txCtx, cutoff); err != nil {
			return wrap(CodeDependency, "批量清理价格锁失败", err)
		}

		c, err := s.orders.DeleteBefore(txCtx, cutoff)
		if err != nil {
			return wrap(CodeDependency, "批量删除订单失败", err)
		}
		count = c

		if err := s.priceLocks.ReleaseOrphaned(txCtx); err != nil {
			return wrap(CodeDependency, "清理残留价格锁失败", err)
		}
		return nil
	})
	return count, err
}

func (s *OrderService) DeleteExpired(ctx context.Context) (int64, error) {
	now := s.clock.Now()
	var count int64
	err := s.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
		rawCloseMinutes, err := s.settings.Get(txCtx, setting.CloseMinutesKey)
		if err != nil && !errors.Is(err, port.ErrNotFound) {
			return wrap(CodeDependency, "读取订单超时设置失败", err)
		}
		cutoff := now.Add(-time.Duration(closeMinutes(rawCloseMinutes)) * time.Minute)

		// 释放条件直接挂在 orders 上，且不设批量上限：
		// 旧实现只给前 500 笔过期订单释放锁，却删除全部过期订单，超出部分会留下永久孤儿锁。
		if err := s.priceLocks.ReleaseByOrdersBefore(txCtx, cutoff); err != nil {
			return wrap(CodeDependency, "级联释放过期订单锁失败", err)
		}

		c, err := s.orders.DeleteExpiredBefore(txCtx, cutoff)
		if err != nil {
			return wrap(CodeDependency, "删除过期订单失败", err)
		}
		count = c

		if err := s.priceLocks.ReleaseOrphaned(txCtx); err != nil {
			return wrap(CodeDependency, "清理残留价格锁失败", err)
		}
		return nil
	})
	return count, err
}

func (s *OrderService) ExpireOrders(ctx context.Context) (int, error) {
	now := s.clock.Now()
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

func (s *OrderService) ReissueByID(ctx context.Context, id int64) error {
	if id <= 0 {
		return fail(CodeInvalidArgument, "订单ID无效")
	}
	now := s.clock.Now()
	// 载荷生成失败不能作为事务错误返回：补单确认的是「这笔钱确实到账」，
	// 签名材料不可用只是「通知发不出去」，把它变成回滚等于让管理端补的单凭空消失（设计 §6.6）。
	var (
		reissued     order.Order
		buildFailure error
	)
	err := s.transactions.WithinTransaction(ctx, func(txCtx context.Context) error {
		value, err := s.orders.FindByIDForUpdate(txCtx, id)
		if errors.Is(err, port.ErrNotFound) {
			return fail(CodeNotFound, "订单不存在")
		}
		if err != nil {
			return wrap(CodeDependency, "查询订单失败", err)
		}

		if value.State == order.StatusPaid || value.State == order.StatusNotifyFailed {
			return fail(CodeInvalidState, "订单已付款，请勿重复补单")
		}

		originalState := value.State
		changed, transitionErr := s.orders.Transition(txCtx, value.ID, originalState, order.StatusPaid, now)
		if transitionErr != nil {
			return wrap(CodeDependency, "更新订单支付状态失败", transitionErr)
		}
		if !changed {
			return fail(CodeConflict, "订单状态已变化，请重试")
		}

		if originalState == order.StatusPending {
			if err := s.priceLocks.ReleaseByOrderID(txCtx, value.OrderID); err != nil {
				return wrap(CodeDependency, "释放订单价格锁失败", err)
			}
		}

		value.State = order.StatusPaid
		value.PaidAt = now

		if value.NotifyURL == "" {
			return nil
		}

		// 原生订单才需要全局密钥；EPay 订单用商户自己的密钥，缺失时由构造器判失败。
		nativeKey := ""
		if value.Protocol == merchant.ProtocolNative {
			nativeKey, err = s.settings.Get(txCtx, setting.MerchantKey)
			if err != nil && !errors.Is(err, port.ErrNotFound) {
				return wrap(CodeDependency, "读取系统密钥失败", err)
			}
		}

		// 重签必须按 protocol 选签名器，否则 EPay 订单会被原生 HMAC 报文投给商户。
		topic, payload, buildErr := buildNotificationFor(txCtx, value, nativeKey, s.merchants, s.merchantKeys, s.masterKeys, now)
		if buildErr != nil {
			reissued, buildFailure = value, wrap(CodeConfiguration, "生成回调签名失败", buildErr)
			return nil
		}

		eventKey := reissueEventKey(value.OrderID, now)
		messageID := reissueDigest("notify", eventKey, value.OrderID)
		if enqueueErr := s.outbox.Enqueue(txCtx, port.OutboxMessage{
			ID:            messageID,
			Topic:         topic,
			AggregateID:   value.OrderID,
			Payload:       payload,
			EventKey:      eventKey,
			CreatedAt:     now,
			NextAttemptAt: now,
		}); enqueueErr != nil && !errors.Is(enqueueErr, port.ErrAlreadyProcessed) {
			return wrap(CodeDependency, "写入通知事件失败", enqueueErr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if buildFailure != nil {
		// 补单事实已提交：只降级通知状态并留待重签记录，然后把配置错误如实报给管理端。
		recorder := notifyFailureRecorder{transactions: s.transactions, orders: s.orders, outbox: s.outbox}
		eventKey := reissueEventKey(reissued.OrderID, now)
		if degradeErr := recorder.record(ctx, reissued, eventKey, buildFailure, now); degradeErr != nil {
			return degradeErr
		}
		return buildFailure
	}
	return nil
}

func reissueEventKey(orderID string, now time.Time) string {
	return reissueDigest("reissue", orderID, now.String())
}

func reissueDigest(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
