package bench

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse/observability"
)

// columnSink 是「默认出口格式不好读」这个问题的**原型**：同一份 Record，
// 渲染成 gin 那种列式一行。
//
// 它要证明的是：清爽与否是**出口**的事，不是框架给的数据不够——
// 需要的字段（状态、耗时、客户端、方法、路由模板、错误分类、trace）记录里全都有。
type columnSink struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *columnSink) Write(r observability.Record) {
	var method, route, client, etype string
	r.Attrs.Range(func(k string, v any) {
		switch k {
		case "http.request.method":
			method, _ = v.(string)
		case "http.route":
			route, _ = v.(string)
		case "client.address":
			client, _ = v.(string)
		case "error.type":
			etype, _ = v.(string)
		}
	})
	if r.Time.IsZero() {
		r.Time = time.Now()
	}

	line := fmt.Sprintf("[WEB] %s | %3s | %9s | %-21s | %-6s %-14s",
		r.Time.Format("2006/01/02 - 15:04:05"), r.Status, r.Duration.String(), client, method, route)
	if etype != "" {
		line += " | " + etype
	}
	if len(r.TraceID) >= 8 {
		line += " | trace=" + r.TraceID[:8]
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(s.w, line)
}

// TestLogFormatPrototype 把**同一批请求**分别经默认出口与列式出口渲染，并排看。
func TestLogFormatPrototype(t *testing.T) {
	w := func() *nopWriter { return &nopWriter{h: map[string][]string{}} }
	req := func(path string) *http.Request { return httptest.NewRequest(http.MethodGet, path, nil) }

	build := func(sink observability.Sink) http.Handler {
		app := web.New(web.WithSink(sink))
		app.GET("/users/{id}", func(c *web.Ctx) error {
			return c.JSON(http.StatusOK, map[string]string{"id": c.Path("id")})
		})
		app.GET("/boom", func(c *web.Ctx) error { return web.Internal("boom", nil) })
		return app
	}
	drive := func(h http.Handler) {
		h.ServeHTTP(w(), req("/users/42"))
		h.ServeHTTP(w(), req("/boom"))
	}

	var defaultOut bytes.Buffer
	drive(build(observability.SlogSink{Logger: slog.New(slog.NewTextHandler(&defaultOut, nil))}))

	// 列式原型也是**真实的 Sink**，装配期的几条记录同样会经过它——
	// 取基线再算增量（这个坑在 records_test.go 里已经踩过一次）。
	var columnOut bytes.Buffer
	columnApp := build(&columnSink{w: &columnOut})
	base := strings.Count(columnOut.String(), "\n")
	drive(columnApp)
	lines := strings.Split(strings.TrimRight(columnOut.String(), "\n"), "\n")

	t.Logf("默认出口（SlogSink）：\n%s", filterLines(defaultOut.String(), "event=http.request"))
	t.Logf("列式原型（同一条 Record 换个出口；装配期另有 %d 行没打）：\n%s", base, strings.Join(lines[base:], "\n"))

	for _, want := range []string{"status=200", "status=500", "error.type=http_5xx"} {
		if !strings.Contains(defaultOut.String(), want) {
			t.Errorf("默认出口输出里缺 %s", want)
		}
	}
	if got := len(lines) - base; got != 2 {
		t.Errorf("列式出口对 2 个请求写了 %d 行", got)
	}
}
