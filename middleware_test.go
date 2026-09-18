package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件覆盖中间件的编排与隔离：跨层顺序、短路、分组前缀，以及与 Static 的交互。
// 边界：单个中间件自己的语义（例如闸门的边界值）归它自己的文件（bodylimit_test.go）。

// traceMW 是记录器中间件：进入记 name+"+"，回程记 name+"-"。
// 记的是「谁跑过 + 怎么走的」，所以既能断言顺序，也能断言某个中间件压根没跑。
func traceMW(log *[]string, name string) Middleware {
	return func(c *Ctx, next Handler) error {
		*log = append(*log, name+"+")
		err := next(c)
		*log = append(*log, name+"-")
		return err
	}
}

// traceHandler 是配对的终端处理器：记 "h" 并写 200。
func traceHandler(log *[]string) Handler {
	return func(c *Ctx) error {
		*log = append(*log, "h")
		return c.Text(http.StatusOK, "ok")
	}
}

func assertTrace(t *testing.T, label string, log []string, want string) {
	t.Helper()
	if got := strings.Join(log, " "); got != want {
		t.Fatalf("%s\n  got  = %s\n  want = %s", label, got, want)
	}
}

// doOK 发一个 GET 并要求 200——隔离用例里真正关心的是「谁跑了」，状态码只用来兜底。
func doOK(t *testing.T, e *Engine, path string) {
	t.Helper()
	if rec := doReq(e, "GET", path, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d，body = %s", path, rec.Code, rec.Body.String())
	}
}

// ---- 层叠顺序 ----

func TestMiddlewareChainOrderAcrossLayers(t *testing.T) {
	var log []string
	e, _ := newTestEngine(t)
	e.Use(traceMW(&log, "u1"), traceMW(&log, "u2"))
	api := e.Group("/api", traceMW(&log, "g1"), traceMW(&log, "g2"))
	v2 := api.Group("/v2", traceMW(&log, "n1"))
	v2.GET("/x", traceHandler(&log), traceMW(&log, "r1"), traceMW(&log, "r2"))

	if rec := doReq(e, "GET", "/api/v2/x", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	assertTrace(t, "引擎 Use(2) + 分组(2) + 嵌套分组(1) + 路由(2)：去程按注册顺序、回程逆序", log,
		"u1+ u2+ g1+ g2+ n1+ r1+ r2+ h r2- r1- n1- g2- g1- u2- u1-")
}

func TestMiddlewareShortCircuitInMiddleLayer(t *testing.T) {
	var log []string
	e, _ := newTestEngine(t)
	e.Use(
		traceMW(&log, "u1"),
		func(c *Ctx, next Handler) error {
			log = append(log, "block+")
			return Unauthorized("blocked", nil) // 不调 next
		},
		traceMW(&log, "u3"),
	)
	e.GET("/s", traceHandler(&log), traceMW(&log, "r1"))

	if rec := doReq(e, "GET", "/s", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d，want 401", rec.Code)
	}
	// u3 / 路由级中间件 / handler 都不该跑；block 没调 next，所以也没有它的回程项。
	assertTrace(t, "中间层短路：后续中间件与 handler 都不执行", log, "u1+ block+ u1-")
}

func TestUseAppliesOnlyToLaterRoutes(t *testing.T) {
	var log []string
	e, _ := newTestEngine(t)
	e.GET("/before", traceHandler(&log))
	e.Use(traceMW(&log, "late"))
	e.GET("/after", traceHandler(&log))

	doOK(t, e, "/before")
	assertTrace(t, "Use 之前注册的路由不经过它", log, "h")

	log = log[:0]
	doOK(t, e, "/after")
	assertTrace(t, "Use 之后注册的路由必须经过它", log, "late+ h late-")
}

// ---- 隔离 ----

func TestGroupMiddlewareDoesNotLeakToRootRoute(t *testing.T) {
	var log []string
	e, _ := newTestEngine(t)
	e.GET("/open", traceHandler(&log))
	e.Group("/api", traceMW(&log, "g")).GET("/x", traceHandler(&log))

	doOK(t, e, "/api/x")
	assertTrace(t, "分组内的路由经过分组中间件", log, "g+ h g-")

	log = log[:0]
	doOK(t, e, "/open")
	assertTrace(t, "同一引擎的根路由不该被分组中间件命中", log, "h")
}

func TestGroupMiddlewareIsolatedBetweenSiblingGroups(t *testing.T) {
	var log []string
	e, _ := newTestEngine(t)
	e.Group("/a", traceMW(&log, "A")).GET("/x", traceHandler(&log))
	e.Group("/b", traceMW(&log, "B")).GET("/y", traceHandler(&log))

	doOK(t, e, "/a/x")
	assertTrace(t, "/a 只跑 A 的中间件", log, "A+ h A-")

	log = log[:0]
	doOK(t, e, "/b/y")
	assertTrace(t, "/b 只跑 B 的中间件", log, "B+ h B-")
}

func TestNestedGroupMiddlewareIsolation(t *testing.T) {
	var log []string
	e, _ := newTestEngine(t)
	parent := e.Group("/p", traceMW(&log, "P"))
	child := parent.Group("/c", traceMW(&log, "C"))
	parent.GET("/self", traceHandler(&log))
	child.GET("/leaf", traceHandler(&log))
	e.Group("/q", traceMW(&log, "Q")).GET("/x", traceHandler(&log))

	doOK(t, e, "/p/self")
	assertTrace(t, "父分组自身的路由只跑父的中间件", log, "P+ h P-")

	log = log[:0]
	doOK(t, e, "/p/c/leaf")
	assertTrace(t, "子分组的路由先跑父、再跑子", log, "P+ C+ h C- P-")

	log = log[:0]
	doOK(t, e, "/q/x")
	assertTrace(t, "兄弟分组不被父 / 子分组的中间件命中", log, "Q+ h Q-")
}

func TestRouteMiddlewareIsolatedWithinGroup(t *testing.T) {
	var log []string
	e, _ := newTestEngine(t)
	g := e.Group("/api", traceMW(&log, "g"))
	g.GET("/a", traceHandler(&log), traceMW(&log, "ra"))
	g.GET("/b", traceHandler(&log))

	doOK(t, e, "/api/a")
	assertTrace(t, "/api/a 跑分组与它自己的路由级中间件", log, "g+ ra+ h ra- g-")

	log = log[:0]
	doOK(t, e, "/api/b")
	assertTrace(t, "同组内路由级中间件互不影响", log, "g+ h g-")
}

func TestGroupPrefixIsNotSubstringMatch(t *testing.T) {
	var log []string
	e, _ := newTestEngine(t)
	e.Group("/api", traceMW(&log, "g")).GET("/users", traceHandler(&log))
	e.GET("/apiv2/users", traceHandler(&log))

	doOK(t, e, "/api/users")
	assertTrace(t, "/api/users 跑分组中间件", log, "g+ h g-")

	log = log[:0]
	doOK(t, e, "/apiv2/users")
	assertTrace(t, "/apiv2 与 /api 只是共同前缀，不属于该分组", log, "h")
}

// TestStaticRunsThroughMiddleware 回归：静态资源必须经过全局中间件链。
func TestStaticRunsThroughMiddleware(t *testing.T) {
	e, _ := newTestEngine(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	var hit bool
	e.Use(func(c *Ctx, next Handler) error {
		hit = true
		return next(c)
	})
	e.Static("/files", dir)

	rec := doReq(e, "GET", "/files/a.txt", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hi" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if !hit {
		t.Fatal("static resource bypassed the middleware chain")
	}
}

// TestGroupMiddlewareAppliesToStatic 分组中间件同样覆盖静态资源。
func TestGroupMiddlewareAppliesToStatic(t *testing.T) {
	e, _ := newTestEngine(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("yo"), 0o600); err != nil {
		t.Fatal(err)
	}

	var hit bool
	g := e.Group("/api", func(c *Ctx, next Handler) error { hit = true; return next(c) })
	g.Static("/assets", dir)

	if rec := doReq(e, "GET", "/api/assets/b.txt", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !hit {
		t.Fatal("group middleware skipped for static resource")
	}
}
