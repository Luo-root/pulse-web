package bench

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Luo-root/pulse-web/loadtest/pulseapp"
	"github.com/Luo-root/pulse/observability"
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

	// ---- pulse-web：默认装配 + 默认出口形态 ----
	var webOut bytes.Buffer
	sink := observability.SlogSink{Logger: slog.New(slog.NewTextHandler(&webOut, nil))}
	app := pulseapp.NewWithSink(sink)
	for range 2 {
		app.ServeHTTP(w(), req())
	}
	t.Logf("pulse-web（默认装配 + 默认 SlogSink）输出 %d 行：\n%s", countLines(webOut.String()), webOut.String())
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
