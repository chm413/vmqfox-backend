package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/hulisang/vmqfox-backend/internal/domain/merchant"
	"github.com/hulisang/vmqfox-backend/internal/http/handler"
	"github.com/hulisang/vmqfox-backend/internal/http/middleware"
	"github.com/hulisang/vmqfox-backend/internal/usecase"
)

// epayRouteStub 满足 handler.EPayManager，唯一作用是让装配层认为「EPay 用例已就绪」。
// 本文件断言的是路由表形状，不执行任何用例逻辑，因此三个方法一律返回零值。
type epayRouteStub struct{}

func (epayRouteStub) Authenticate(context.Context, map[string]string) (merchant.Merchant, merchant.Protocol, error) {
	return merchant.Merchant{}, 0, nil
}

func (epayRouteStub) SignEPayResponse(context.Context, int64, map[string]string) (string, string, error) {
	return "", "", nil
}

func (epayRouteStub) Create(context.Context, usecase.EPayCreateInput) (usecase.EPayCreateResult, error) {
	return usecase.EPayCreateResult{}, nil
}

func (epayRouteStub) Query(context.Context, int64, string) (usecase.EPayQueryResult, error) {
	return usecase.EPayQueryResult{}, nil
}

// ginAnyMethods 是 gin 的 Any 会注册的全部方法集合。
var ginAnyMethods = []string{
	http.MethodGet,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodHead,
	http.MethodOptions,
	http.MethodDelete,
	http.MethodConnect,
	http.MethodTrace,
}

// newEPayRouteEngine 用最小依赖构造真实路由表：除 EPay 外全部传零值，
// NewRouter 会用默认限流配置补齐，因此这里断言的就是线上装配出来的形状。
func newEPayRouteEngine(t *testing.T, epay handler.EPayManager) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return NewRouter(RouterDeps{
		Handlers:    handler.New(handler.Dependencies{EPay: epay}),
		RuntimeMode: middleware.RuntimeWriter,
	})
}

// collectRoutes 把 engine.Routes() 折叠成 path -> method 集合，便于做存在性与方法集合断言。
func collectRoutes(engine *gin.Engine) map[string]map[string]bool {
	routes := make(map[string]map[string]bool)
	for _, route := range engine.Routes() {
		if routes[route.Path] == nil {
			routes[route.Path] = make(map[string]bool, 1)
		}
		routes[route.Path][route.Method] = true
	}
	return routes
}

// TestEPayRoutesMountedAtRootLevel 钉住 EPay V1 三个入口挂载在根级。
//
// 为什么必须断言路由表而不是 handler：EPay 插件只让商户填一个「网关地址」，
// 插件自己拼出 <网关>/submit.php。一旦这三条被挪到 /api 前缀下（比如为了统一
// 风格把它们并进 registerAPI），所有商户已配置的网关地址立刻 404——
// 而 handler 单测直接调方法，完全不会红。
func TestEPayRoutesMountedAtRootLevel(t *testing.T) {
	routes := collectRoutes(newEPayRouteEngine(t, epayRouteStub{}))

	// submit.php 与 api.php 用 Any 注册：插件既有表单 POST，也有 GET 跳转。
	for _, path := range []string{"/submit.php", "/api.php"} {
		methods, ok := routes[path]
		if !ok {
			t.Fatalf("%s 未挂到根级路由表；EPay 插件拼出的 <网关>%s 会直接 404", path, path)
		}
		for _, method := range ginAnyMethods {
			if !methods[method] {
				t.Errorf("%s 缺少 %s 方法（现有 %v），插件用该方法提交时会 405", path, method, methodNames(methods))
			}
		}
		if len(methods) != len(ginAnyMethods) {
			t.Errorf("%s 的方法集合应恰好等于 Any 的 9 个方法，实际 %v", path, methodNames(methods))
		}
	}

	// mapi.php 只接受 POST：放宽方法等于多开一条无需页面跳转的入口。
	mapiMethods, ok := routes["/mapi.php"]
	if !ok {
		t.Fatalf("/mapi.php 未挂到根级路由表；商户 API 模式下单会直接 404")
	}
	if !mapiMethods[http.MethodPost] {
		t.Errorf("/mapi.php 缺少 POST（现有 %v）", methodNames(mapiMethods))
	}
	if len(mapiMethods) != 1 {
		t.Errorf("/mapi.php 应只注册 POST，实际 %v", methodNames(mapiMethods))
	}

	// 反证：不能同时出现在 /api 前缀下。若有人把路由挪进 registerAPI 又忘了删根级的，
	// 根级断言依然会过，只有这条能发现「多出一条影子入口」。
	for _, path := range []string{"/api/submit.php", "/api/mapi.php", "/api/api.php"} {
		if methods, ok := routes[path]; ok {
			t.Errorf("%s 不该存在（实际注册了 %v），EPay 入口必须只在根级", path, methodNames(methods))
		}
	}
}

// TestEPayRootRoutesReachable 用真实请求反证三条入口能被路由命中。
// 路由表里有、但被前置中间件拦成 404 的情况，只有发请求才能发现。
func TestEPayRootRoutesReachable(t *testing.T) {
	engine := newEPayRouteEngine(t, epayRouteStub{})

	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/submit.php"},
		{http.MethodPost, "/mapi.php"},
		{http.MethodPost, "/api.php"},
		{http.MethodGet, "/submit.php"},
	} {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(route.method, route.path, nil))
		if recorder.Code == http.StatusNotFound || recorder.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s 未命中 EPay 入口，实际 %d —— 插件会认为网关地址填错",
				route.method, route.path, recorder.Code)
		}
	}
}

// TestEPayRoutesAbsentWhenServiceNotWired 断言 EPay 用例为 nil 时三条根级路由完全不注册。
// 半装配（路由在、用例为 nil）会让每个请求都对空指针解引用，
// 只能靠 Recovery 转成 500，误报溯源极难。
func TestEPayRoutesAbsentWhenServiceNotWired(t *testing.T) {
	routes := collectRoutes(newEPayRouteEngine(t, nil))

	for _, path := range []string{"/submit.php", "/mapi.php", "/api.php"} {
		if methods, ok := routes[path]; ok {
			t.Errorf("EPay 未装配时不应注册 %s，实际注册了 %v —— 调用时会命中 nil 用例", path, methodNames(methods))
		}
	}

	engine := newEPayRouteEngine(t, nil)
	for _, path := range []string{"/submit.php", "/mapi.php", "/api.php"} {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("EPay 未装配时 POST %s 应为 404，实际 %d（%s）", path, recorder.Code, recorder.Body.String())
		}
	}
}

// methodNames 让失败信息可读：map 直接打印会是乱序的布尔表。
func methodNames(methods map[string]bool) []string {
	names := make([]string, 0, len(methods))
	for _, method := range ginAnyMethods {
		if methods[method] {
			names = append(names, method)
		}
	}
	// 兜底：Any 之外被显式注册的方法也要显示出来。
	for method := range methods {
		known := false
		for _, candidate := range ginAnyMethods {
			if candidate == method {
				known = true
				break
			}
		}
		if !known {
			names = append(names, method)
		}
	}
	return names
}
