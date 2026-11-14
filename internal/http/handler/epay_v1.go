package handler

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/usecase"
)

const (
	// epayMaxParams 对齐设计 §5.5 #10：参数 ≤32 个。
	// 签名与歧义校验都是 O(参数字节)，不设上限等于开放 CPU 放大。
	epayMaxParams = 32
	// epayMaxBodyBytes 对齐设计 §5.5 #10：请求体 ≤64KB。
	epayMaxBodyBytes = 64 << 10
	// epayTimeLayout 是 EPay 约定的时间格式（本地时间，不做时区转换）。
	epayTimeLayout = "2006-01-02 15:04:05"

	// epaySuccessCode / epayErrorCode 是 EPay 的成功与失败码。
	// 失败一律 -1、HTTP 状态固定 200：EPay 插件普遍只读 body，不看状态码。
	epaySuccessCode = 1
	epayErrorCode   = -1

	// epayMessageNotImplemented 用于 api.php 里未开通的 act。
	epayMessageNotImplemented = "接口未开通"
	// epayMessageAmbiguousParams 与验签失败同文案：设计 §5.5 #11 要求对外不可区分，
	// 否则攻击者能靠「参数重复」与「签名错误」的差异判断自己离撞库成功还有多远。
	epayMessageAmbiguousParams = "签名错误"
)

// EPayManager 是 EPay V1 三个入口依赖的窄接口，由 usecase.EPayService 实现。
type EPayManager interface {
	Authenticate(context.Context, map[string]string) (merchant.Merchant, error)
	Create(context.Context, usecase.EPayCreateInput) (usecase.EPayCreateResult, error)
	Query(context.Context, int64, string) (usecase.EPayQueryResult, error)
}

// EPayV1Handler 承载 EPay V1 的三个入口。
// 路由由装配层挂（/submit.php、/mapi.php、/api.php），本类型不注册任何路由，
// 也不依赖全局状态，便于直接构造测试。
type EPayV1Handler struct {
	epay EPayManager
}

func NewEPayV1Handler(epay EPayManager) *EPayV1Handler {
	return &EPayV1Handler{epay: epay}
}

var (
	errEPayTooManyParams = errors.New("参数过多")
	errEPayAmbiguous     = errors.New("参数重复或存在大小写变体")
)

// Submit 处理 submit.php（页面跳转支付）：验签 → 下单 → 302 到收银台。
//
// 失败必须渲染 HTML 而不是 JSON：这个入口是用户浏览器直接打开的，
// 回一段 JSON 等于让用户看到花括号。
func (h *EPayV1Handler) Submit(c *gin.Context) {
	ctx := c.Request.Context()

	params, err := epayV1Params(c)
	if err != nil {
		writePaymentErrorHTML(c, epayParamErrorMessage(err))
		return
	}
	identity, err := h.epay.Authenticate(ctx, params)
	if err != nil {
		writePaymentErrorHTML(c, epayErrorMessage(err))
		return
	}
	result, err := h.epay.Create(ctx, epayCreateInput(identity, params))
	if err != nil {
		writePaymentErrorHTML(c, epayErrorMessage(err))
		return
	}
	c.Redirect(http.StatusFound, result.PayURL)
}

// MAPI 处理 mapi.php（API 支付）：返回 JSON，字段与设计 §5.3 一致。
// qrcode 给的是收款码内容而不是图片地址，商户用自己前端渲染，省一次往返。
func (h *EPayV1Handler) MAPI(c *gin.Context) {
	ctx := c.Request.Context()

	params, err := epayV1Params(c)
	if err != nil {
		writeEPayError(c, epayParamErrorMessage(err))
		return
	}
	identity, err := h.epay.Authenticate(ctx, params)
	if err != nil {
		writeEPayError(c, epayErrorMessage(err))
		return
	}
	result, err := h.epay.Create(ctx, epayCreateInput(identity, params))
	if err != nil {
		writeEPayError(c, epayErrorMessage(err))
		return
	}

	c.JSON(http.StatusOK, epayMAPIResponse{
		Code:      epaySuccessCode,
		Msg:       "",
		TradeNo:   result.TradeNo,
		PayURL:    result.PayURL,
		Qrcode:    result.Qrcode,
		URLScheme: "",
	})
}

// API 处理 api.php：只实现 act=order（单笔查单）。
//
// 其余 act（query/refund/settle/orders）一律「接口未开通」，且不返回任何假数据：
// 本网关不经手资金，没有余额与结算实体；act=query 的原版实现还会回显商户密钥。
// act 判定放在验签之前——未开通接口不该先消耗一次密钥解密。
func (h *EPayV1Handler) API(c *gin.Context) {
	ctx := c.Request.Context()

	params, err := epayV1Params(c)
	if err != nil {
		writeEPayError(c, epayParamErrorMessage(err))
		return
	}
	if params["act"] != "order" {
		writeEPayError(c, epayMessageNotImplemented)
		return
	}

	identity, err := h.epay.Authenticate(ctx, params)
	if err != nil {
		writeEPayError(c, epayErrorMessage(err))
		return
	}
	result, err := h.epay.Query(ctx, identity.ID, params["out_trade_no"])
	if err != nil {
		writeEPayError(c, epayErrorMessage(err))
		return
	}

	c.JSON(http.StatusOK, epayQueryResponse{
		Code:        epaySuccessCode,
		Msg:         "",
		Pid:         identity.No,
		TradeNo:     result.TradeNo,
		OutTradeNo:  result.OutTradeNo,
		Type:        result.Type,
		Name:        result.Name,
		Money:       result.Money,
		TradeStatus: result.TradeStatus,
		AddTime:     epayTimeText(result.AddTime),
		EndTime:     epayTimeText(result.EndTime),
		Param:       result.Param,
	})
}

// epayMAPIResponse 是 mapi.php 的成功响应。
// 不合并成一个大结构体：两种响应的字段集不同，合并会让 mapi 里冒出 addtime 之类的空字段，
// 而某些插件会按字段存在与否做判断。
type epayMAPIResponse struct {
	Code      int    `json:"code"`
	Msg       string `json:"msg"`
	TradeNo   string `json:"trade_no"`
	PayURL    string `json:"payurl"`
	Qrcode    string `json:"qrcode"`
	URLScheme string `json:"urlscheme"`
}

// epayQueryResponse 是 api.php?act=order 的成功响应（字段见设计 §5.3）。
// param 不加 omitempty：EPay 插件常见做法是直接读该字段，缺 key 与空串在部分实现里不等价。
type epayQueryResponse struct {
	Code        int    `json:"code"`
	Msg         string `json:"msg"`
	Pid         string `json:"pid"`
	TradeNo     string `json:"trade_no"`
	OutTradeNo  string `json:"out_trade_no"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Money       string `json:"money"`
	TradeStatus string `json:"trade_status"`
	AddTime     string `json:"addtime"`
	EndTime     string `json:"endtime"`
	Param       string `json:"param"`
}

type epayErrorResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func writeEPayError(c *gin.Context, message string) {
	c.JSON(http.StatusOK, epayErrorResponse{Code: epayErrorCode, Msg: message})
}

// epayV1Params 收集 query 与 form 参数，并在入口处消灭三类签名歧义（设计 §5.5 #1/#2/#10）。
//
// 为什么必须自己收而不是用 c.Request.FormValue：那个 API 会静默取第一个值，
// 而 Go 取第一个、PHP 取最后一个 —— 两侧取到不同的金额，验签通过但金额被换掉。
func epayV1Params(c *gin.Context) (map[string]string, error) {
	params := make(map[string]string, 16)
	if err := mergeEPayParams(params, c.Request.URL.Query()); err != nil {
		return nil, err
	}

	if c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, epayMaxBodyBytes)
	}
	if err := c.Request.ParseForm(); err != nil {
		// 超限的 body 会在这里报错；对外与歧义参数同一文案，避免暴露内部阈值。
		return nil, errEPayAmbiguous
	}
	if err := mergeEPayParams(params, c.Request.PostForm); err != nil {
		return nil, err
	}

	if len(params) > epayMaxParams {
		return nil, errEPayTooManyParams
	}
	return params, nil
}

// mergeEPayParams 把一批参数并入 params，任何歧义都拒绝。
// 三类歧义：同名出现 ≥2 次、query 与 form 出现同名、sign/sign_type 的大小写变体。
func mergeEPayParams(params map[string]string, values url.Values) error {
	for name, list := range values {
		if len(list) != 1 {
			return errEPayAmbiguous
		}
		if isEPayNameVariant(name) {
			return errEPayAmbiguous
		}
		if _, exists := params[name]; exists {
			return errEPayAmbiguous
		}
		params[name] = list[0]
	}
	return nil
}

// isEPayNameVariant 判断参数名是否是 sign/sign_type 的大小写变体。
// 我方签名域只剔除精确小写名，而 PHP 侧可能按不区分大小写读参数，
// 放任变体存在就等于留一个「不参与签名却能被下游读到」的字段。
func isEPayNameVariant(name string) bool {
	lower := strings.ToLower(name)
	if lower != "sign" && lower != "sign_type" {
		return false
	}
	return name != lower
}

// epayCreateInput 把已验签的参数映射成用例入参。
// money/name 等一律传原始文本：改写任意一个字符都会让商户侧验签失败。
func epayCreateInput(identity merchant.Merchant, params map[string]string) usecase.EPayCreateInput {
	return usecase.EPayCreateInput{
		MerchantID: identity.ID,
		Protocol:   merchant.ProtocolEPayV1,
		OutTradeNo: params["out_trade_no"],
		Type:       params["type"],
		Name:       params["name"],
		Money:      params["money"],
		NotifyURL:  params["notify_url"],
		ReturnURL:  params["return_url"],
		Param:      params["param"],
	}
}

// epayErrorMessage 取用例层已经写好的对外文案。
// 没带文案的内部错误一律折叠成一句通用提示：把 gorm/驱动的原文回给商户等于泄露内部结构。
func epayErrorMessage(err error) string {
	var appError *usecase.Error
	if errors.As(err, &appError) && appError != nil && appError.Message != "" {
		return appError.Message
	}
	return "系统错误，请稍后重试"
}

func epayParamErrorMessage(err error) string {
	if errors.Is(err, errEPayTooManyParams) {
		return "参数过多"
	}
	return epayMessageAmbiguousParams
}

// epayTimeText 输出 EPay 的时间格式；零值输出空串（未支付没有 endtime）。
func epayTimeText(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(epayTimeLayout)
}
