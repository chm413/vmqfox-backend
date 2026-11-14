package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/hulisang/vmqfox-backend/internal/compat/php"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/usecase"
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
			writeMerchantError(c, err)
			return
		}

		data := apiMerchantViewData(merchantViewOf(result.Merchant, true, result.SecretHint))
		// 明文只出现在这一次响应里：写入库成功之后才下发，之后任何接口都不再返回。
		data["secret"] = result.SecretPlain
		data["hint"] = result.SecretHint
		if result.PublicKeyPEM != "" {
			data["publicKey"] = result.PublicKeyPEM
		}
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
			writeMerchantError(c, err)
			return
		}
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
