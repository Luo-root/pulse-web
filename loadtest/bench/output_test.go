package bench

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse-web/loadtest/pulseapp"
	"github.com/gin-gonic/gin"
)

// TestOpenBoxOutput 量的是「什么都不写，两边各自往控制台吐什么」——
// 这是「日志信息够不够」这类问题唯一能对得上的口径。
//
// gin 侧用**默认 debug 模式**（不 SetMode(ReleaseMode)）：那才是它的开箱形态，
// 启动横幅、路由表都在这里。
func TestOpenBoxOutput(t *testing.T) {
	req := func() *http.Request {
		return httptest.NewRequest(http.MethodGet, "/users/42", nil)
	}
	w := func() *nopWriter { return &nopWriter{h: map[string][]string{}} }

	// ---- gin：debug 模式开箱 ----
	var ginOut bytes.Buffer
	gin.DefaultWriter = &ginOut
	gin.DefaultErrorWriter = &ginOut
	gin.SetMode(gin.DebugMode)
	defer gin.SetMode(gin.ReleaseMode)

	engine := gin.Default()
	engine.GET("/users/:id", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "name": "user-" + c.Param("id")})
	})
	for range 2 {
		engine.ServeHTTP(w(), req())
	}
	t.Logf("gin（debug 开箱）输出 %d 行：\n%s", countLines(ginOut.String()), ginOut.String())

	// ---- pulse-web：默认装配 + 默认出口 ----
	var webOut bytes.Buffer
	app := pulseapp.NewWithSink(web.NewConsoleSink(&webOut))
	for range 2 {
		app.ServeHTTP(w(), req())
	}
	t.Logf("pulse-web（默认装配 + 默认出口 ConsoleSink）输出 %d 行：\n%s", countLines(webOut.String()), webOut.String())
}

func countLines(s string) int {
	n := 0
	for _, c := range s {
		if c == '\n' {
			n++
		}
	}
	return n
}

// TestFailureOutput 量失败路径：200 之外那两种（业务 500 / panic）两边各吐什么。
//
// 这是「gin 是不是会打 200/500 那种日志」这个问题的正面回答——会，两边都会，
// 差别在**同一行里还带了什么**：pulse-web 带错误**分类**（`http_5xx` / `panic`，
// 低基数、可直接聚合），gin 只有状态码本身。
func TestFailureOutput(t *testing.T) {
	w := func() *nopWriter { return &nopWriter{h: map[string][]string{}} }
	req := func(path string) *http.Request { return httptest.NewRequest(http.MethodGet, path, nil) }

	// ---- gin：Logger + Recovery（默认组合）----
	var ginOut bytes.Buffer
	gin.DefaultWriter = &ginOut
	gin.DefaultErrorWriter = &ginOut
	gin.SetMode(gin.DebugMode) // 开箱形态
	defer gin.SetMode(gin.ReleaseMode)

	engine := gin.New()
	engine.Use(gin.Logger(), gin.Recovery())
	engine.GET("/boom", func(c *gin.Context) { c.JSON(http.StatusInternalServerError, gin.H{"error": "boom"}) })
	engine.GET("/panic", func(c *gin.Context) { panic("kaboom") })

	engine.ServeHTTP(w(), req("/boom"))
	engine.ServeHTTP(w(), req("/panic"))

	if got := strings.Count(ginOut.String(), "| 500 |"); got != 2 {
		t.Errorf("gin: 输出里带 500 的行有 %d 条，期望 2（业务 500 + panic 兜底）", got)
	}
	stacked := strings.Count(ginOut.String(), ".go:")
	t.Logf("gin 请求行：\n%s", filterLines(ginOut.String(), "[GIN] "))
	t.Logf("gin panic 还会往 DefaultErrorWriter 打一份 [Recovery] 转储（含 %d 个栈帧）：%v",
		stacked, strings.Contains(ginOut.String(), "[Recovery]"))

	// ---- pulse-web：默认装配 + 默认出口 ----
	var webOut bytes.Buffer
	app := web.New(web.WithSink(web.NewConsoleSink(&webOut)))
	app.GET("/boom", func(c *web.Ctx) error { return web.Internal("boom", nil) })
	app.GET("/panic", func(c *web.Ctx) error { panic("kaboom") })

	app.ServeHTTP(w(), req("/boom"))
	app.ServeHTTP(w(), req("/panic"))

	// 默认出口是列式的：状态在**固定列**里（不是 `status=500` 那种 k=v），错误分类与
	// 消息合成尾部的错误格——`http_5xx "boom: Internal Server Error"` / `panic "panic: kaboom"`。
	if got := strings.Count(webOut.String(), "| 500 |"); got != 2 {
		t.Errorf("pulse-web: 状态列是 500 的行有 %d 条，期望 2（业务 500 + panic 兜底）", got)
	}
	for _, want := range []string{`http_5xx "boom: Internal Server Error"`, `panic "panic: kaboom"`} {
		if !strings.Contains(webOut.String(), want) {
			t.Errorf("pulse-web: 输出里没有 %s", want)
		}
	}
	t.Logf("pulse-web 请求行：\n%s", filterLines(webOut.String(), "| 500 |"))
	t.Logf("pulse-web panic 打 [Recovery] 转储：%v——栈进的是 `PanicError.Stack`，出口只渲染 error 的字符串",
		strings.Contains(webOut.String(), "[Recovery]"))
}

// filterLines 只留含 substr 的行，给日志用。
func filterLines(s, substr string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
