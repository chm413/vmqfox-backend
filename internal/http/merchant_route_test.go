package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/hulisang/vmqfox-backend/internal/auth"
	"github.com/hulisang/vmqfox-backend/internal/http/handler"
	"github.com/hulisang/vmqfox-backend/internal/http/middleware"
)

// merchantRouteTestParser 用固定脚本模拟令牌校验，不引入真实 JWT 依赖。
type merchantRouteTestParser struct{ valid bool }

func (p merchantRouteTestParser) ParseAuthorization(string) (auth.Identity, error) {
	if p.valid {
		return auth.Identity{Subject: "admin"}, nil
	}
	return auth.Identity{}, errors.New("令牌无效")
}

// TestMerchantRoutesRequireAuth 保证五个管理端商户路由都挂在 JWT 组内：
// 未鉴权必须 401，而不是落到处理器的 503/200。
func TestMerchantRoutesRequireAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := NewRouter(RouterDeps{
		Handlers:    handler.New(handler.Dependencies{}),
		TokenParser: merchantRouteTestParser{},
		RuntimeMode: middleware.RuntimeWriter,
	})

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/merchant/list"},
		{http.MethodPost, "/api/merchant/create"},
		{http.MethodPost, "/api/merchant/update/1"},
		{http.MethodPost, "/api/merchant/key/rotate/1"},
		{http.MethodGet, "/api/merchant/key/public/1"},
	}
	for _, route := range routes {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(route.method, route.path, nil)
		engine.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 未鉴权时应返回 401，实际 %d（%s）",
				route.method, route.path, recorder.Code, recorder.Body.String())
		}
	}
}

// TestMerchantRoutesRegisteredBehindAuth 用「已鉴权但未装配服务」的 503 反证路由确实登记在管理端组内，
// 避免中间件一改就把路由变成 404 而没人发现。
func TestMerchantRoutesRegisteredBehindAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := NewRouter(RouterDeps{
		Handlers:    handler.New(handler.Dependencies{}),
		TokenParser: merchantRouteTestParser{valid: true},
		RuntimeMode: middleware.RuntimeWriter,
	})

	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/merchant/list"},
		{http.MethodGet, "/api/merchant/key/public/1"},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(route.method, route.path, nil)
		request.Header.Set("Authorization", "Bearer test-token")
		engine.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusNotFound {
			t.Errorf("%s %s 未登记到路由表", route.method, route.path)
			continue
		}
		// 未装配商户服务时应是明确的 503，不能是伪造成功。
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s 期望 503（服务未装配），实际 %d（%s）",
				route.method, route.path, recorder.Code, recorder.Body.String())
		}
	}
}
