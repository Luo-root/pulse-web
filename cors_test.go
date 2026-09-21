package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/observability"
)

// 本文件覆盖一方 CORS 的面（cors.go）：实际请求 / 预检 / 三种拒绝 / 裸 OPTIONS /
// 通配来源 / 装配期校验 / 分组 / 兜底路由与显式路由的优先级 / 观测（预检进访问记录、
// 拒绝带 error.type）。边界：`Adapt` 与生态中间件的对照在 interop/middleware_test.go。

const corsOrigin = "https://app.example.com"

// corsEngine 造一个已经挂上 CORS 的引擎。**必须在注册业务路由之前**调用 CORS
// （它内部就是 Use + 挂一张观察表，之后的每次注册都会被它看一眼），所以这个 helper
// 只挂 CORS，路由留给用例自己加。
func corsEngine(t *testing.T, opts ...CORSOption) (*Engine, *observability.MemorySink) {
	t.Helper()
	e, sink := newTestEngine(t)
	e.CORS(opts...)
	return e, sink
}

// corsPreflight 造一条预检请求：`acrh` 为空的段不写。
func corsPreflight(e *Engine, path, origin, method, acrh string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodOptions, path, nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", method)
	if acrh != "" {
		req.Header.Set("Access-Control-Request-Headers", acrh)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// corsProbe 是预检用例里那个「真路由」：它记下自己被调用过没有——预检应当由中间件
// 当场答完，**不该走到这里**。
type corsProbe struct{ ran bool }

func (p *corsProbe) handler() Handler {
	return func(c *Ctx) error {
		p.ran = true
		return c.Text(http.StatusOK, "ok")
	}
}

func TestCORSPreflightAllowed(t *testing.T) {
	e, sink := corsEngine(t,
		CORSAllowOrigins(corsOrigin),
		CORSAllowMethods(http.MethodGet, http.MethodPost),
		CORSAllowHeaders("content-type", "x-csrf-token"),
		CORSAllowCredentials(),
		CORSMaxAge(10*time.Minute),
	)
	probe := &corsProbe{}
	e.POST("/api/users", probe.handler())

	rec := corsPreflight(e, "/api/users", corsOrigin, "POST", "content-type, x-csrf-token")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("预检状态码 = %d，want 204（body=%q）", rec.Code, rec.Body.String())
	}
	if probe.ran {
		t.Error("预检不该走到业务 handler——中间件应当当场答完")
	}
	for name, want := range map[string]string{
		"Access-Control-Allow-Origin":      corsOrigin,
		"Access-Control-Allow-Methods":     "POST", // 只回显被问到的那一个
		"Access-Control-Allow-Headers":     "content-type, x-csrf-token",
		"Access-Control-Allow-Credentials": "true",
		"Access-Control-Max-Age":           "600",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q，want %q", name, got, want)
		}
	}
	// 预检的响应随来源 / 方法 / 请求头而变，缓存必须按这三样分桶。
	for _, v := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
		if !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), v) {
			t.Errorf("Vary 里缺 %q（实际 %q）", v, rec.Header().Values("Vary"))
		}
	}
	if rec.Body.Len() != 0 {
		t.Errorf("预检不该有 body，实际 %q", rec.Body.String())
	}

	// 观测面：预检进了框架自己的记录——这是「按路径补 OPTIONS」换来的一半价值。
	// 路由模板是**真路由**：预检落在 `OPTIONS /api/users` 这条补出来的路由上。
	rec2, ok := findRecord(sink, eventHTTPReq)
	if !ok {
		t.Fatal("访问记录里没有 http.request——预检没进采集层？")
	}
	if rec2.Status != "204" {
		t.Errorf("记录里的状态码 = %q，want 204", rec2.Status)
	}
	attrs := attrsOf(rec2)
	if got := attrs["http.route"]; got != "/api/users" {
		t.Errorf("记录里的路由模板 = %v，want /api/users（按路径补的那条 OPTIONS）", got)
	}
	if got := attrs["http.request.method"]; got != http.MethodOptions {
		t.Errorf("记录里的方法 = %v，want OPTIONS", got)
	}
}

func TestCORSPreflightDenied(t *testing.T) {
	for _, c := range []struct {
		name   string
		origin string
		method string
		acrh   string
		code   string
	}{
		{"来源不在白名单", "https://evil.example.com", "POST", "", "cors_origin_not_allowed"},
		{"方法不在白名单", corsOrigin, "DELETE", "", "cors_method_not_allowed"},
		{"请求头不在白名单", corsOrigin, "POST", "x-evil", "cors_header_not_allowed"},
	} {
		e, sink := corsEngine(t,
			CORSAllowOrigins(corsOrigin),
			CORSAllowMethods(http.MethodGet, http.MethodPost),
			CORSAllowHeaders("content-type"),
		)
		probe := &corsProbe{}
		e.POST("/api/users", probe.handler())

		rec := corsPreflight(e, "/api/users", c.origin, c.method, c.acrh)

		// 拒绝是显式 403（不是静默少几个头）：浏览器两侧都是 CORS 失败，但这一侧
		// 对人与监控都看得见。
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s：状态码 = %d，want 403（body=%q）", c.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), c.code) {
			t.Errorf("%s：响应体里没有 %q（实际 %q）", c.name, c.code, rec.Body.String())
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s：被拒的预检不该带 Access-Control-Allow-Origin（实际 %q）", c.name, got)
		}
		if !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Origin") {
			t.Errorf("%s：被拒的响应也该 Vary: Origin", c.name)
		}
		if probe.ran {
			t.Errorf("%s：被拒的预检不该走到业务 handler", c.name)
		}

		rec2, ok := findRecord(sink, eventHTTPReq)
		if !ok {
			t.Fatalf("%s：访问记录里没有 http.request", c.name)
		}
		if rec2.Status != "403" {
			t.Errorf("%s：记录里的状态码 = %q，want 403", c.name, rec2.Status)
		}
		if rec2.Err == nil {
			t.Errorf("%s：记录里应当带错误对象（拒绝是框架接住的 error）", c.name)
		}
		if got := attrsOf(rec2)["error.type"]; got != "http_4xx" {
			t.Errorf("%s：记录里的 error.type = %v，want http_4xx", c.name, got)
		}
	}
}

func TestCORSActualRequest(t *testing.T) {
	for _, c := range []struct {
		name       string
		origin     string
		wantOrigin string
	}{
		{"允许的来源", corsOrigin, corsOrigin},
		{"不在白名单", "https://evil.example.com", ""},
		{"没带 Origin", "", ""},
	} {
		e, _ := corsEngine(t,
			CORSAllowOrigins(corsOrigin),
			CORSAllowCredentials(),
			CORSExposeHeaders("X-Total-Count"),
		)
		probe := &corsProbe{}
		e.GET("/api/users", probe.handler())

		hdr := []string{}
		if c.origin != "" {
			hdr = append(hdr, "Origin", c.origin)
		}
		rec := doReq(e, http.MethodGet, "/api/users", nil, hdr...)

		// 实际请求无论允许与否都放行给业务 handler——拒绝只是「不加 CORS 头」，
		// 由浏览器去挡，服务端不替它做决定。
		if rec.Code != http.StatusOK || !probe.ran {
			t.Errorf("%s：实际请求应当照常走到 handler（status=%d ran=%v）", c.name, rec.Code, probe.ran)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != c.wantOrigin {
			t.Errorf("%s：Access-Control-Allow-Origin = %q，want %q", c.name, got, c.wantOrigin)
		}
		vary := strings.Join(rec.Header().Values("Vary"), ",")
		if c.origin != "" && !strings.Contains(vary, "Origin") {
			t.Errorf("%s：带 Origin 的响应必须 Vary: Origin（实际 %q）", c.name, vary)
		}
		if c.origin == "" && vary != "" {
			t.Errorf("%s：没有 Origin 就不是跨源请求，不该加任何 CORS 相关的头（Vary=%q）", c.name, vary)
		}
		// 凭据与暴露头只在「允许」那一格出现。
		wantCred := ""
		wantExpose := ""
		if c.wantOrigin != "" {
			wantCred, wantExpose = "true", "X-Total-Count"
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != wantCred {
			t.Errorf("%s：Access-Control-Allow-Credentials = %q，want %q", c.name, got, wantCred)
		}
		if got := rec.Header().Get("Access-Control-Expose-Headers"); got != wantExpose {
			t.Errorf("%s：Access-Control-Expose-Headers = %q，want %q", c.name, got, wantExpose)
		}
	}
}

// TestCORSWildcardOrigin：配了 `*` 且不携凭据时直接回 `*`（能让缓存复用）；携凭据时
// 那个组合在装配期就被拦下（另见 TestCORSAssemblyPanics）。
func TestCORSWildcardOrigin(t *testing.T) {
	e, _ := corsEngine(t, CORSAllowOrigins("*"))
	probe := &corsProbe{}
	e.GET("/api/users", probe.handler())

	rec := doReq(e, http.MethodGet, "/api/users", nil, "Origin", "https://anywhere.example.com")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q，want *", got)
	}

	pf := corsPreflight(e, "/api/users", "https://anywhere.example.com", http.MethodGet, "")
	if pf.Code != http.StatusNoContent {
		t.Errorf("预检状态码 = %d，want 204", pf.Code)
	}
	if got := pf.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("预检的 Access-Control-Allow-Origin = %q，want *", got)
	}
}

// TestCORSBareOptions：裸 OPTIONS（没有 Access-Control-Request-Method）不是预检，
// 落到补出来的那条 OPTIONS 上，答案与 ServeMux 今天给的一致——405 + Allow。
func TestCORSBareOptions(t *testing.T) {
	e, sink := corsEngine(t, CORSAllowOrigins(corsOrigin))
	probe := &corsProbe{}
	e.GET("/api/users", probe.handler())

	rec := doReq(e, http.MethodOptions, "/api/users", nil, "Origin", corsOrigin)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("裸 OPTIONS 状态码 = %d，want 405（与 ServeMux 今天的答案一致）", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q，want %q", got, "GET, HEAD")
	}
	if probe.ran {
		t.Error("裸 OPTIONS 不该落到业务 handler 上")
	}
	// 差别只在观测：这条请求现在进了洋葱，于是有记录、有路由模板。
	rec2, ok := findRecord(sink, eventHTTPReq)
	if !ok || rec2.Status != "405" {
		t.Fatalf("裸 OPTIONS 也该进访问记录（status=%v ok=%v）", rec2.Status, ok)
	}
	if got := attrsOf(rec2)["http.route"]; got != "/api/users" {
		t.Errorf("记录里的路由模板 = %v，want /api/users", got)
	}

	// 没带 Origin 的裸 OPTIONS 同理，一个 CORS 头都不加。
	rec = doReq(e, http.MethodOptions, "/api/users", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("无 Origin 的裸 OPTIONS 状态码 = %d，want 405", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("无 Origin 时不该回 Access-Control-Allow-Origin（实际 %q）", got)
	}
}

// TestCORSBareOptionsAllowListsEveryMethod：Allow 是**按当下注册过的方法现算**的——
// 同一路径后面再加方法，它跟着变。
func TestCORSBareOptionsAllowListsEveryMethod(t *testing.T) {
	e, _ := corsEngine(t, CORSAllowOrigins(corsOrigin))
	e.GET("/api/users", func(c *Ctx) error { return c.NoContent(http.StatusOK) })
	e.PUT("/api/users", func(c *Ctx) error { return c.NoContent(http.StatusOK) })
	e.DELETE("/api/users", func(c *Ctx) error { return c.NoContent(http.StatusOK) })

	rec := doReq(e, http.MethodOptions, "/api/users", nil)
	if got := rec.Header().Get("Allow"); got != "DELETE, GET, HEAD, PUT" {
		t.Errorf("Allow = %q，want %q（排序后全列出来）", got, "DELETE, GET, HEAD, PUT")
	}
}

// TestCORSPreflightOnWildcardRoute：带路径参数的路由同样补 OPTIONS，预检照常兑现。
func TestCORSPreflightOnWildcardRoute(t *testing.T) {
	e, _ := corsEngine(t, CORSAllowOrigins(corsOrigin))
	probe := &corsProbe{}
	e.GET("/users/{id}", probe.handler())

	rec := corsPreflight(e, "/users/42", corsOrigin, http.MethodGet, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("通配路径的预检状态码 = %d，want 204（body=%q）", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != corsOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q，want %q", got, corsOrigin)
	}
	if probe.ran {
		t.Error("预检不该走到业务 handler")
	}
}

// TestCORSSpecificOptionsRouteWins：想给某条路径自定义 OPTIONS 语义，就在**注册该路径的
// 方法路由之前**注册它——那时 CORS 不再补自动的那条，这条路径的 OPTIONS 归使用者。
func TestCORSSpecificOptionsRouteWins(t *testing.T) {
	e, _ := corsEngine(t, CORSAllowOrigins(corsOrigin))
	e.OPTIONS("/api/users", func(c *Ctx) error { return c.Text(http.StatusOK, "mine") })
	e.GET("/api/users", func(c *Ctx) error { return c.Text(http.StatusOK, "get") })

	rec := doReq(e, http.MethodOptions, "/api/users", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "mine" {
		t.Errorf("显式注册的 OPTIONS 没赢：status=%d body=%q", rec.Code, rec.Body.String())
	}

	// 没被显式注册的路径照常补自动 OPTIONS。
	e.GET("/api/other", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })
	if rec := doReq(e, http.MethodOptions, "/api/other", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("未显式注册路径的 OPTIONS 状态码 = %d，want 405（自动补的那条）", rec.Code)
	}
}

// TestCORSUserOptionsAfterRoutePanics：反过来的顺序是**装配期错误**——那条路径已经补过
// 自动 OPTIONS，再注册同路径的 OPTIONS 会撞车。panic 消息里写了怎么办，别让人去猜。
func TestCORSUserOptionsAfterRoutePanics(t *testing.T) {
	e, _ := corsEngine(t, CORSAllowOrigins(corsOrigin))
	e.GET("/api/users", func(c *Ctx) error { return c.NoContent(http.StatusOK) })

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("晚注册的 OPTIONS 应当 panic")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "GET/POST **之前**") {
			t.Errorf("panic 消息没写清怎么办：%v", msg)
		}
	}()
	e.OPTIONS("/api/users", func(c *Ctx) error { return c.NoContent(http.StatusOK) })
}

// TestCORSRegisteredTwicePanics：挂两次是装配期错误（中间件会叠两层，观察表也会重置）。
func TestCORSRegisteredTwicePanics(t *testing.T) {
	e, _ := newTestEngine(t)
	e.CORS(CORSAllowOrigins(corsOrigin))
	defer func() {
		if recover() == nil {
			t.Error("重复挂 CORS 应当 panic")
		}
	}()
	e.CORS(CORSAllowOrigins(corsOrigin))
}

// TestCORSOnGroup：分组同样能用，兜底路由落在分组前缀下——组外的 OPTIONS 不受影响，
// 照旧由 ServeMux 答 405。
func TestCORSOnGroup(t *testing.T) {
	e, _ := newTestEngine(t)
	api := e.Group("/api")
	api.CORS(CORSAllowOrigins(corsOrigin))
	probe := &corsProbe{}
	api.POST("/users", probe.handler())

	rec := corsPreflight(e, "/api/users", corsOrigin, "POST", "")
	if rec.Code != http.StatusNoContent {
		t.Errorf("分组内的预检状态码 = %d，want 204（body=%q）", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != corsOrigin {
		t.Errorf("分组内的预检 Access-Control-Allow-Origin = %q，want %q", got, corsOrigin)
	}

	out := doReq(e, http.MethodOptions, "/outside", nil, "Origin", corsOrigin,
		"Access-Control-Request-Method", "POST")
	// 组外连路径都不匹配（不是「路径匹配但方法不对」那种 405），所以是 404——
	// 兜底路由在 /api 前缀下，管不到这里。
	if out.Code != http.StatusNotFound {
		t.Errorf("分组外的预检状态码 = %d，want 404（兜底路由在 /api 前缀下，管不到这里）", out.Code)
	}
}

// TestCORSHeaderListTolerance：`Access-Control-Request-Headers` 的手写形态很脏——
// 分多行、大小写混着、逗号两侧带空白、重复。按 HTTP 语义都该被接受，回显时规范化。
func TestCORSHeaderListTolerance(t *testing.T) {
	e, _ := corsEngine(t, CORSAllowOrigins(corsOrigin), CORSAllowHeaders("content-type", "x-csrf-token"))
	e.POST("/api/users", func(c *Ctx) error { return c.NoContent(http.StatusOK) })

	req := httptest.NewRequest(http.MethodOptions, "/api/users", nil)
	req.Header.Set("Origin", corsOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Add("Access-Control-Request-Headers", "  Content-Type ,X-CSRF-Token ")
	req.Header.Add("Access-Control-Request-Headers", "x-csrf-token")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("状态码 = %d，want 204（body=%q）", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "content-type, x-csrf-token" {
		t.Errorf("Access-Control-Allow-Headers = %q，want 规范化去重后的 %q", got, "content-type, x-csrf-token")
	}

	// 通配请求头：配了 `*` 就都放行，回显仍只回被问到的那几个。
	e2, _ := corsEngine(t, CORSAllowOrigins(corsOrigin), CORSAllowHeaders("*"))
	e2.POST("/api/users", func(c *Ctx) error { return c.NoContent(http.StatusOK) })
	pf := corsPreflight(e2, "/api/users", corsOrigin, http.MethodPost, "x-anything-at-all")
	if pf.Code != http.StatusNoContent {
		t.Errorf("CORSAllowHeaders(\"*\") 下预检状态码 = %d，want 204", pf.Code)
	}
}

// TestCORSPreflightOnStaticPrefix：`Static` 挂的前缀模式（`"<prefix>/"`）**不带方法**，
// 本来就吃得住 OPTIONS——预检不需要框架补路由，照常由中间件在洋葱内答掉。这条正对着
// `Static` godoc 里那句「auth / CORS / 限流不会有例外」。
//
// 反向也钉住：这条路径**不**该被补一条同名 `OPTIONS`（补了会遮蔽 FileServer 自己的
// 405/404 语义）——补只发生在带方法的模式上（见 observe 里 `method == ""` 那条早退）。
func TestCORSPreflightOnStaticPrefix(t *testing.T) {
	e, sink := corsEngine(t, CORSAllowOrigins(corsOrigin))
	e.Static("/assets", t.TempDir())

	rec := corsPreflight(e, "/assets/app.css", corsOrigin, http.MethodGet, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("静态路径的预检状态码 = %d，want 204（body=%q）", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != corsOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q，want %q", got, corsOrigin)
	}

	// 进了洋葱，路由模板是那条前缀模式（不是补出来的 OPTIONS）。
	rec2, ok := findRecord(sink, eventHTTPReq)
	if !ok {
		t.Fatalf("静态路径的预检没进访问记录")
	}
	if got := attrsOf(rec2)["http.route"]; got != "/assets/" {
		t.Errorf("记录里的路由模板 = %v，want /assets/（Static 注册的那条前缀模式）", got)
	}
}

// TestCORSPreflightOnUnregisteredPathIs404：预检打到**没有注册过任何方法**的路径时，
// 那条 OPTIONS 也没被补出来（补是跟着路由走的），于是由 ServeMux 答 404——诚实的答案：
// 这条请求对应的资源根本不存在。要连这种情况也覆盖，得让整条路由链搬到中间件之后，
// 那是另一个量级的改动（见 CORS 的 godoc）。
func TestCORSPreflightOnUnregisteredPathIs404(t *testing.T) {
	e, _ := corsEngine(t, CORSAllowOrigins(corsOrigin))
	e.GET("/api/users", func(c *Ctx) error { return c.NoContent(http.StatusOK) })

	rec := corsPreflight(e, "/api/nowhere", corsOrigin, http.MethodGet, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("未注册路径的预检状态码 = %d，want 404", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("未注册路径的预检不该带 CORS 头（实际 %q）", got)
	}
}

// TestCORSMethodDefault：不配方法时按 Fetch 的 simple methods——GET / POST / HEAD 放行，
// 其余拒绝。
func TestCORSMethodDefault(t *testing.T) {
	e, _ := corsEngine(t, CORSAllowOrigins(corsOrigin))
	e.GET("/api/users", func(c *Ctx) error { return c.NoContent(http.StatusOK) })

	for _, c := range []struct {
		method string
		want   int
	}{
		{http.MethodGet, http.StatusNoContent},
		{http.MethodPost, http.StatusNoContent},
		{http.MethodHead, http.StatusNoContent},
		{http.MethodPut, http.StatusForbidden},
		{http.MethodDelete, http.StatusForbidden},
	} {
		rec := corsPreflight(e, "/api/users", corsOrigin, c.method, "")
		if rec.Code != c.want {
			t.Errorf("默认方法白名单下 %s 的预检状态码 = %d，want %d", c.method, rec.Code, c.want)
		}
	}
}

// TestCORSOriginTrailingSlashAndCase：来源是按字符串比的，但大小写与尾斜杠这两种
// 「粘错了」的写法不该静默失配。
func TestCORSOriginTrailingSlashAndCase(t *testing.T) {
	e, _ := corsEngine(t, CORSAllowOrigins("https://App.Example.com/"))
	probe := &corsProbe{}
	e.GET("/api/users", probe.handler())

	rec := doReq(e, http.MethodGet, "/api/users", nil, "Origin", "https://app.example.com")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q，want 回显请求里的原始来源", got)
	}
}

// TestCORSUnmatchedRouteHasNoCORSHeaders：**已知边界**——没匹配到路由的请求不进洋葱
// （框架「不搬动路由」那条边界的同一处），于是它的响应一个 CORS 头都没有：跨源请求
// 打到一个不存在的路径时，浏览器看到的是 CORS 失败而不是那个 404。
//
// 兜底路由只管 OPTIONS，管不到这里；要覆盖它得让整条路由链搬动到中间件之后——那是
// 另一个量级的改动，不在本功能的范围内。修了这条边界时，这条断言会红。
func TestCORSUnmatchedRouteHasNoCORSHeaders(t *testing.T) {
	e, _ := corsEngine(t, CORSAllowOrigins(corsOrigin))

	rec := doReq(e, http.MethodGet, "/nope", nil, "Origin", corsOrigin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未匹配路径的状态码 = %d，want 404", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("未匹配路由的响应不该带 CORS 头（实际 %q）——这条边界被改动了？", got)
	}
}

func TestCORSAssemblyPanics(t *testing.T) {
	for _, c := range []struct {
		name string
		call func(*Engine)
	}{
		{"一个来源都没配", func(e *Engine) { e.CORS() }},
		{"通配来源 + 携凭据", func(e *Engine) { e.CORS(CORSAllowOrigins("*"), CORSAllowCredentials()) }},
	} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("%s：装配期应当 panic", c.name)
				}
			}()
			e, _ := newTestEngine(t)
			c.call(e)
		}()
	}
}
