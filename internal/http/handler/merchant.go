package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/hulisang/vmqfox-backend/internal/auth"
	"github.com/hulisang/vmqfox-backend/internal/compat/php"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/usecase"
)

// 密钥操作审计事件。创建与轮换是唯一会改变商户密钥的入口，
// 误操作（例如把 V2 商户降级回 V1）之后，这行日志是唯一的追溯手段。
const (
	auditEventKeyCreate = "merchant.key.create"
	auditEventKeyRotate = "merchant.key.rotate"

	// auditUnknown 用于身份取不到时占位：宁可记 unknown，也不要让字段消失，
	// 否则日志里分不清「没人操作」和「忘了记」。
	auditUnknown = "unknown"
)

// MerchantManager 只暴露管理端所需用例，仓储与加解密细节不进入 transport。
type MerchantManager interface {
	Create(context.Context, usecase.CreateMerchantInput) (usecase.CreateMerchantResult, error)
	List(context.Context) ([]usecase.MerchantView, error)
	Update(context.Context, int64, usecase.UpdateMerchantInput) (usecase.MerchantView, error)
	RotateKey(context.Context, int64, merchant.Protocol) (usecase.RotateResult, error)
	PlatformPublicKey(context.Context, int64) (string, error)
}

// MerchantHandlers 是管理端商户接口集合，全部挂在已有 JWT 鉴权组内。
type MerchantHandlers struct {
	ListAPI      gin.HandlerFunc
	CreateAPI    gin.HandlerFunc
	UpdateAPI    gin.HandlerFunc
	RotateKeyAPI gin.HandlerFunc
	PublicKeyAPI gin.HandlerFunc
}

func newMerchantHandlers(service MerchantManager) MerchantHandlers {
	return MerchantHandlers{
		ListAPI:      listMerchantsHandler(service),
		CreateAPI:    createMerchantHandler(service),
		UpdateAPI:    updateMerchantHandler(service),
		RotateKeyAPI: rotateMerchantKeyHandler(service),
		PublicKeyAPI: platformPublicKeyHandler(service),
	}
}

func listMerchantsHandler(service MerchantManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			writeMerchantUnavailable(c)
			return
		}
		items, err := service.List(c.Request.Context())
		if err != nil {
			writeMerchantError(c, err)
			return
		}
		views := make([]gin.H, 0, len(items))
		for _, item := range items {
			views = append(views, apiMerchantViewData(item))
		}
		c.JSON(http.StatusOK, php.NewEnvelope(200, "成功", gin.H{
			"total": len(views),
			"items": views,
		}))
	}
}

func createMerchantHandler(service MerchantManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			writeMerchantUnavailable(c)
			return
		}
		params, err := scalarRequestParams(c)
		if err != nil {
			c.JSON(http.StatusOK, php.NewEnvelope(400, "商户参数无效", nil))
			return
		}
		closeMinutes, err := optionalIntParam(params["closeMinutes"])
		if err != nil {
			c.JSON(http.StatusOK, php.NewEnvelope(400, "订单有效期参数无效", nil))
			return
		}

		result, err := service.Create(c.Request.Context(), usecase.CreateMerchantInput{
			Name:         params["name"],
			CloseMinutes: closeMinutes,
			Note:         params["note"],
		})
		if err != nil {
			// 创建失败不动密钥，但仍要留痕：连续失败是「主密钥配错」这类部署事故的早期信号。
			logMerchantKeyAudit(c, merchantKeyAuditErrorLine(auditEventKeyCreate, 0, "", err))
			writeMerchantError(c, err)
			return
		}
		// 创建成功即下发了一把 V1 共享密钥（V2 要等接入方提供公钥），此处记下归属与指纹。
		logMerchantKeyAudit(c, merchantKeyAuditLine(
			auditEventKeyCreate,
			result.Merchant.ID,
			result.Merchant.No,
			int(merchant.ProtocolEPayV1),
			result.SecretHint,
			merchantOperator(c),
		))

		data := apiMerchantViewData(merchantViewOf(result.Merchant, true, result.SecretHint))
		// 明文只出现在这一次响应里：写入库成功之后才下发，之后任何接口都不再返回。
		data["secret"] = result.SecretPlain
		data["hint"] = result.SecretHint
		if result.PublicKeyPEM != "" {
			data["publicKey"] = result.PublicKeyPEM
		}
		// 响应体里带明文密钥：绝不允许被浏览器、代理或中间缓存留在磁盘上重放。
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, php.NewEnvelope(200, "创建成功，请立即保存商户密钥，关闭后无法再次查看", data))
	}
}

func updateMerchantHandler(service MerchantManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			writeMerchantUnavailable(c)
			return
		}
		id, err := merchantIDParam(c)
		if err != nil {
			c.JSON(http.StatusOK, php.NewEnvelope(400, "商户ID无效", nil))
			return
		}
		params, err := scalarRequestParams(c)
		if err != nil {
			c.JSON(http.StatusOK, php.NewEnvelope(400, "商户参数无效", nil))
			return
		}
		// 更新是全量替换语义，enabled 必须显式给出：漏传字段把商户静默停用，
		// 表现是「新单全失败」，但管理端看不出自己改了什么。
		enabled, err := requiredBoolParam(params["enabled"])
		if err != nil {
			c.JSON(http.StatusOK, php.NewEnvelope(400, "启用状态参数无效", nil))
			return
		}
		closeMinutes, err := optionalIntParam(params["closeMinutes"])
		if err != nil {
			c.JSON(http.StatusOK, php.NewEnvelope(400, "订单有效期参数无效", nil))
			return
		}

		view, err := service.Update(c.Request.Context(), id, usecase.UpdateMerchantInput{
			Name:         params["name"],
			Enabled:      enabled,
			CloseMinutes: closeMinutes,
			Note:         params["note"],
		})
		if err != nil {
			writeMerchantError(c, err)
			return
		}
		c.JSON(http.StatusOK, php.NewEnvelope(200, "保存成功", apiMerchantViewData(view)))
	}
}

func rotateMerchantKeyHandler(service MerchantManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			writeMerchantUnavailable(c)
			return
		}
		id, err := merchantIDParam(c)
		if err != nil {
			c.JSON(http.StatusOK, php.NewEnvelope(400, "商户ID无效", nil))
			return
		}
		params, err := scalarRequestParams(c)
		if err != nil {
			c.JSON(http.StatusOK, php.NewEnvelope(400, "轮换参数无效", nil))
			return
		}
		protocolValue, err := strconv.Atoi(strings.TrimSpace(params["protocol"]))
		protocol := merchant.Protocol(protocolValue)
		if err != nil || !protocol.Valid() {
			c.JSON(http.StatusOK, php.NewEnvelope(400, "协议版本无效，只支持 1 或 2", nil))
			return
		}

		result, err := service.RotateKey(c.Request.Context(), id, protocol)
		if err != nil {
			// 轮换失败不改动已下发的密钥，但失败本身要能对上「谁在何时试过」。
			logMerchantKeyAudit(c, merchantKeyAuditErrorLine(auditEventKeyRotate, id, "", err))
			writeMerchantError(c, err)
			return
		}
		// 轮换是这个系统里最危险的一次点击：新密钥一落库，商户端旧配置立刻失效。
		// 日志里必须有商户、协议与指纹，且只有指纹。
		logMerchantKeyAudit(c, merchantKeyAuditLine(
			auditEventKeyRotate,
			id,
			"",
			int(result.Protocol),
			result.Hint,
			merchantOperator(c),
		))

		data := gin.H{
			"protocol": int(result.Protocol),
			"hint":     result.Hint,
		}
		// V1 返回新共享密钥明文，V2 返回新平台公钥；两者都只在这一个响应里出现一次。
		if result.Secret != "" {
			data["secret"] = result.Secret
		}
		if result.PublicKey != "" {
			data["publicKey"] = result.PublicKey
		}
		// 同上：响应里有新明文密钥，禁止任何缓存留存。
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, php.NewEnvelope(200, "轮换成功，请立即把新密钥配置到商户端", data))
	}
}

func platformPublicKeyHandler(service MerchantManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			writeMerchantUnavailable(c)
			return
		}
		id, err := merchantIDParam(c)
		if err != nil {
			c.JSON(http.StatusOK, php.NewEnvelope(400, "商户ID无效", nil))
			return
		}
		publicKey, err := service.PlatformPublicKey(c.Request.Context(), id)
		if err != nil {
			writeMerchantError(c, err)
			return
		}
		c.JSON(http.StatusOK, php.NewEnvelope(200, "成功", gin.H{"publicKey": publicKey}))
	}
}

// merchantKeyAuditLine 拼出密钥操作审计行（单行 key=value，便于 grep 与日志采集）。
//
// 签名里刻意没有明文与密文的位置：调用方拿不到这两个值，也就写不进日志。
// hint 只是末 4 位指纹，够回答「换的是哪一把」，不足以还原密钥本身。
func merchantKeyAuditLine(event string, id int64, no string, protocol int, hint, operator string) string {
	return fmt.Sprintf(
		"event=%s merchant_id=%d merchant_no=%s protocol=%d hint=%s operator=%s",
		event, id, auditValueOr(no), protocol, auditValueOr(hint), auditValueOr(operator),
	)
}

// merchantKeyAuditErrorLine 失败分支只记错误分类，不记原始错误文本：
// usecase 的消息里可能带商户名/商户号，审计行必须保证零业务数据外泄。
func merchantKeyAuditErrorLine(event string, id int64, no string, err error) string {
	return fmt.Sprintf(
		"event=%s result=error merchant_id=%d merchant_no=%s reason=%s",
		event, id, auditValueOr(no), merchantErrorReason(err),
	)
}

// merchantErrorReason 把用例错误压成稳定的小写下划线标签，避免日志被自由文本污染。
func merchantErrorReason(err error) string {
	var appError *usecase.Error
	if !errors.As(err, &appError) {
		return "internal"
	}
	switch appError.Code {
	case usecase.CodeInvalidArgument:
		return "invalid_argument"
	case usecase.CodeInvalidState:
		return "invalid_state"
	case usecase.CodeNotFound:
		return "not_found"
	case usecase.CodeConflict:
		return "conflict"
	case usecase.CodeConfiguration:
		return "configuration"
	case usecase.CodeDependency:
		return "dependency"
	default:
		return "internal"
	}
}

// merchantOperator 取当前请求的操作者标识：身份由 TokenAuth 写进 request context。
func merchantOperator(c *gin.Context) string {
	identity, ok := auth.IdentityFromContext(c.Request.Context())
	if !ok {
		return auditUnknown
	}
	return auditValueOr(identity.Subject)
}

// merchantRequestID 复用 RequestMetadata 注入的请求标识（键名与 middleware.RequestIDKey 一致）；
// 中间件未接入时返回空串，整段省略，而不是写一个假的 request_id。取出后必须先转义：
// 该值直接来自 X-Request-ID 请求头。
func merchantRequestID(c *gin.Context) string {
	value, exists := c.Get("request_id")
	if !exists {
		return ""
	}
	requestID, _ := value.(string)
	return logValue(requestID)
}

// logMerchantKeyAudit 落一行审计日志。
// 用标准库 logger：handler 层没有注入 logger，而这类「谁动了密钥」的记录不能被可选依赖吞掉。
func logMerchantKeyAudit(c *gin.Context, line string) {
	if requestID := merchantRequestID(c); requestID != "" {
		line += " request_id=" + requestID
	}
	log.Printf("%s", line)
}

// auditValueOr 让空值显式呈现为 unknown，保持字段个数恒定。
func auditValueOr(value string) string {
	if sanitized := logValue(value); sanitized != "" {
		return sanitized
	}
	return auditUnknown
}

// logValue 把外部可控文本压成单行安全取值。
// X-Request-ID 直接来自请求头，塞一个换行就能伪造整条日志（日志注入）：
// 控制字符与空白一律降级为下划线，并截断到 64 个字符。
func logValue(value string) string {
	var builder strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if r <= ' ' || r == 0x7f {
			builder.WriteRune('_')
			continue
		}
		builder.WriteRune(r)
	}
	runes := []rune(builder.String())
	if len(runes) > 64 {
		runes = runes[:64]
	}
	return string(runes)
}

// apiMerchantViewData 是管理端商户的白名单视图：不含明文，也不含密文与 nonce。
func apiMerchantViewData(view usecase.MerchantView) gin.H {
	return gin.H{
		"id":           view.ID,
		"merchantNo":   view.MerchantNo,
		"name":         view.Name,
		"enabled":      view.Enabled,
		"closeMinutes": view.CloseMinutes,
		"note":         view.Note,
		"v1Enabled":    view.V1Enabled,
		"v2Enabled":    view.V2Enabled,
		"v1Hint":       view.V1Hint,
		"v2Hint":       view.V2Hint,
		"createdAt":    view.CreatedAt,
		"updatedAt":    view.UpdatedAt,
	}
}

// merchantViewOf 把刚创建出来的商户转成视图，避免为了拿 hint 再查一次库。
func merchantViewOf(value merchant.Merchant, v1Enabled bool, v1Hint string) usecase.MerchantView {
	return usecase.MerchantView{
		ID:           value.ID,
		MerchantNo:   value.No,
		Name:         value.Name,
		Enabled:      value.Enabled,
		CloseMinutes: value.CloseMinutes,
		Note:         value.Note,
		V1Enabled:    v1Enabled,
		V1Hint:       v1Hint,
		CreatedAt:    value.CreatedAt,
		UpdatedAt:    value.UpdatedAt,
	}
}

func merchantIDParam(c *gin.Context) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("商户ID无效")
	}
	return id, nil
}

// optionalIntParam 允许字段缺省（返回 0），但显式传了非数字时一定要报错：
// 静默当成 0 会把「有效期 30 分钟」变成「跟随全局」，属于改错配置却看不出来。
func optionalIntParam(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	return strconv.Atoi(value)
}

// requiredBoolParam 只接受明确的真/假写法，其余一律报错。
func requiredBoolParam(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "on", "yes":
		return true, nil
	case "0", "false", "off", "no":
		return false, nil
	default:
		return false, errors.New("启用状态参数无效")
	}
}

func writeMerchantUnavailable(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, php.NewEnvelope(503, "商户服务不可用", nil))
}

func writeMerchantError(c *gin.Context, err error) {
	var appError *usecase.Error
	if !errors.As(err, &appError) {
		c.JSON(http.StatusOK, php.NewEnvelope(500, "商户服务异常", nil))
		return
	}
	code := 400
	switch appError.Code {
	case usecase.CodeInvalidArgument, usecase.CodeInvalidState:
		code = 400
	case usecase.CodeNotFound:
		code = 404
	case usecase.CodeConflict:
		code = 409
	case usecase.CodeConfiguration:
		// 主密钥缺失/配错属于部署问题，不能用 400 让人去查参数。
		code = 503
	case usecase.CodeDependency:
		code = 500
	}
	c.JSON(http.StatusOK, php.NewEnvelope(code, appError.Message, nil))
}
