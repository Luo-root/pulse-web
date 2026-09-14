package web

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/observability"
)

var consoleTestTime = time.Date(2026, 9, 14, 8, 30, 0, 0, time.UTC)

// httpRecord 造一条与 Engine.writeAccessLog 同形的访问记录（字段集与生产一致）。
func httpRecord(status string, d time.Duration) observability.Record {
	rec := observability.Record{
		Time:     consoleTestTime,
		HostID:   "pulse-web",
		TraceID:  "8f2e1a3b4c5d6e7f8a9b0c1d2e3f4a5b",
		Source:   observability.Source(sourceHTTP),
		Event:    eventHTTPReq,
		Status:   status,
		Duration: d,
	}
	observability.Set(&rec.Attrs, attrHTTPMethod, "GET")
	observability.Set(&rec.Attrs, attrHTTPRoute, "/users/{id}")
	observability.Set(&rec.Attrs, attrURLPath, "/users/42")
	observability.Set(&rec.Attrs, attrHTTPBodySize, int64(29))
	observability.Set(&rec.Attrs, attrClientAddr, "192.0.2.1:1234")
	return rec
}

// renderLine 用给定记录渲染一行（颜色关，非终端）。
func renderLine(t *testing.T, rec observability.Record) string {
	t.Helper()
	var buf bytes.Buffer
	NewConsoleSink(&buf, WithColor(false)).Write(rec)
	return buf.String()
}

// TestConsoleSinkHTTPLine 钉住 http 行的完整版式：列宽、列序、尾段顺序。
//
// 这条断言是**故意写死整行**的：版式是这个出口的产出物，改版式必须在这里显式改
// 断言（而不是被一个 Contains 悄悄放过）。
func TestConsoleSinkHTTPLine(t *testing.T) {
	got := renderLine(t, httpRecord("200", 585100*time.Nanosecond))
	want := "2026/09/14 - 08:30:00 | 200 |   585.1µs | 192.0.2.1:1234  | GET    /users/42" +
		" | route=/users/{id} | size=29 | host=pulse-web | trace=8f2e1a3b4c5d6e7f8a9b0c1d2e3f4a5b\n"
	if got != want {
		t.Fatalf("版式不符：\n got=%q\nwant=%q", got, want)
	}
}

// consoleFields 按 ` | ` 切列并去掉两侧填充（断言列内容时不必逐个数空格）。
func consoleFields(line string) []string {
	raw := strings.Split(strings.TrimSuffix(line, "\n"), consoleSep)
	fields := make([]string, len(raw))
	for i, f := range raw {
		fields[i] = strings.TrimSpace(f)
	}
	return fields
}

// TestConsoleSinkHTTPLineFallbacks 覆盖列缺值时的退化行为：不能渲染出空列。
func TestConsoleSinkHTTPLineFallbacks(t *testing.T) {
	t.Run("无路由模板时列里给具体路径", func(t *testing.T) {
		rec := httpRecord("404", time.Millisecond)
		rec.Attrs = observability.Attrs{} // 清空后只补最小字段集
		observability.Set(&rec.Attrs, attrHTTPMethod, "GET")
		observability.Set(&rec.Attrs, attrURLPath, "/nope")
		got := renderLine(t, rec)
		if !strings.Contains(got, "| GET    /nope |") {
			t.Fatalf("路径列应给具体路径且不留空列：%q", got)
		}
		if strings.Contains(got, "route=") {
			t.Fatalf("没有路由模板时不该出现 route=：%q", got)
		}
	})

	t.Run("方法/客户端缺值补 -", func(t *testing.T) {
		rec := httpRecord("200", 0)
		rec.Attrs = observability.Attrs{}
		fields := consoleFields(renderLine(t, rec))
		if len(fields) < 5 {
			t.Fatalf("列数不足：%q", fields)
		}
		if fields[1] != "200" {
			t.Errorf("状态列 = %q，want 200", fields[1])
		}
		if !strings.HasSuffix(fields[2], "0ns") {
			t.Errorf("耗时列应带单位：%q", fields[2])
		}
		if fields[3] != "-" {
			t.Errorf("客户端缺值应为 -，实得 %q", fields[3])
		}
		if got := strings.Fields(fields[4]); len(got) != 2 || got[0] != "-" || got[1] != "-" {
			t.Errorf("方法/路径缺值应各补 -，实得 %q", fields[4])
		}
	})

	t.Run("静态路由（模板=路径）不重复打 route=", func(t *testing.T) {
		rec := httpRecord("200", time.Microsecond)
		observability.Set(&rec.Attrs, attrHTTPRoute, "/users/42") // 与 url.path 相同
		got := renderLine(t, rec)
		if strings.Contains(got, "route=") {
			t.Fatalf("模板与路径相同时不该出现 route=：%q", got)
		}
	})

	t.Run("只有模板没有路径时不重复", func(t *testing.T) {
		rec := httpRecord("200", time.Microsecond)
		observability.Set(&rec.Attrs, attrURLPath, "")
		got := renderLine(t, rec)
		if n := strings.Count(got, "/users/{id}"); n != 1 {
			t.Fatalf("模板只该出现一次（列里），实得 %d 次：%q", n, got)
		}
	})
}

// TestConsoleSinkErrorLine 错误行：错误分类 + 带空格的错误文本要加引号
// （否则一行里的字段边界就没了）。
func TestConsoleSinkErrorLine(t *testing.T) {
	rec := httpRecord("500", 7623*time.Microsecond)
	rec.Err = errors.New("boom: Internal Server Error")
	observability.Set(&rec.Attrs, attrErrorType, "http_5xx")

	got := renderLine(t, rec)
	if !strings.Contains(got, `| http_5xx "boom: Internal Server Error"`) {
		t.Fatalf("错误尾段不符（分类 + 引号后的文本）：%q", got)
	}
	if !strings.Contains(got, "| 500 |") {
		t.Fatalf("状态列不符：%q", got)
	}
}

// TestConsoleSinkEventLine：非 http 记录退回 `时间 | event | k=v …`，
// 不硬套列式（装配期记录没有 status / route，套了就是一堆空列）。
func TestConsoleSinkEventLine(t *testing.T) {
	rec := observability.Record{
		Time:      consoleTestTime,
		HostID:    "svc",
		Source:    observability.Source("kernel"),
		Event:     "pulse.kernel.fiber_state",
		FiberName: "db",
		From:      "Starting",
		To:        "Running",
	}
	got := renderLine(t, rec)
	want := "2026/09/14 - 08:30:00 | pulse.kernel.fiber_state host=svc fiber=db state=Starting→Running\n"
	if got != want {
		t.Fatalf("事件行版式不符：\n got=%q\nwant=%q", got, want)
	}
	if strings.Contains(got, "|  |") {
		t.Fatalf("事件行不该出现空列：%q", got)
	}
}

// TestConsoleSinkKeepsUnknownAttrs：固定列盖不住的属性不能丢——中间件与业务
// 往访问记录里加的字段（llm.model 之类）要照样出现在行尾。
func TestConsoleSinkKeepsUnknownAttrs(t *testing.T) {
	rec := httpRecord("200", time.Microsecond)
	observability.Set(&rec.Attrs, "llm.model", "MiniMax-M3")
	observability.Set(&rec.Attrs, "llm.tokens", int64(1234))
	observability.Set(&rec.Attrs, "llm.cached", true)
	observability.Set(&rec.Attrs, "llm.temp", 0.75)

	got := renderLine(t, rec)
	for _, want := range []string{"llm.model=MiniMax-M3", "llm.tokens=1234", "llm.cached=true", "llm.temp=0.75"} {
		if !strings.Contains(got, want) {
			t.Fatalf("未知属性 %q 丢了：%q", want, got)
		}
	}
	// 未知属性聚成一组（一个 ` | ` + 空格分隔），不逐条占一个分隔符。
	group := " | llm.model=MiniMax-M3 llm.tokens=1234 llm.cached=true llm.temp=0.75"
	if !strings.Contains(got, group) {
		t.Fatalf("未知属性应聚成一组：%q", got)
	}
	if n := strings.Count(got, "| llm."); n != 1 {
		t.Fatalf("未知属性组应只占一个分隔符，实得 %d 个：%q", n, got)
	}
}

// TestConsoleSinkColor：颜色只在要求时出现（缺省非终端不上色，否则日志里会混
// 转义序列），并且只染状态列。
func TestConsoleSinkColor(t *testing.T) {
	var plain bytes.Buffer
	NewConsoleSink(&plain, WithColor(false)).Write(httpRecord("500", time.Microsecond))
	if strings.Contains(plain.String(), "\x1b[") {
		t.Fatalf("关色时不该出现转义序列：%q", plain.String())
	}

	var colored bytes.Buffer
	NewConsoleSink(&colored, WithColor(true)).Write(httpRecord("500", time.Microsecond))
	if !strings.Contains(colored.String(), ansiRed+"500"+ansiReset) {
		t.Fatalf("5xx 状态列应为红色：%q", colored.String())
	}

	colored.Reset()
	NewConsoleSink(&colored, WithColor(true)).Write(httpRecord("200", time.Microsecond))
	if !strings.Contains(colored.String(), ansiGreen+"200"+ansiReset) {
		t.Fatalf("2xx 状态列应为绿色：%q", colored.String())
	}
	if strings.Contains(colored.String(), ansiRed) {
		t.Fatalf("2xx 不该出现红色：%q", colored.String())
	}
}

// TestConsoleSinkConcurrentWrites：并发写不串台（-race 下同时验数据竞争），
// 且每个并发调用都不需要自己拿锁。
func TestConsoleSinkConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	s := NewConsoleSink(&buf, WithColor(false))

	const writers, perWriter = 8, 64
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				s.Write(httpRecord("200", 100*time.Microsecond))
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != writers*perWriter {
		t.Fatalf("行数不符：got %d want %d（写串台或被吞）", len(lines), writers*perWriter)
	}
	for i, ln := range lines {
		if !strings.HasPrefix(ln, "2026/09/14 - 08:30:00 | 200 |") {
			t.Fatalf("第 %d 行不完整：%q", i+1, ln)
		}
	}
}

func TestConsoleSinkNilWriterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil writer 应在构造期 panic")
		}
	}()
	NewConsoleSink(nil)
}

// TestAppendDurationUnits：单位与精度（亚毫秒不取整成 0——旧默认出口的毛病）。
func TestAppendDurationUnits(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{820 * time.Nanosecond, "820ns"},
		{585100 * time.Nanosecond, "585.1µs"},
		{7623 * time.Microsecond, "7.62ms"},
		{1234 * time.Millisecond, "1.23s"},
	}
	for _, c := range cases {
		if got := string(appendDuration(nil, c.d)); got != c.want {
			t.Errorf("appendDuration(%v) = %q，want %q", c.d, got, c.want)
		}
	}
}

// TestDefaultSinkIsConsoleSink：默认装配的出口就是这个可读出口（不是 SlogSink）。
// 这条断言防的是「默认值被悄悄改回去」——默认出口是开箱体验，属于显式决定（#20）。
func TestDefaultSinkIsConsoleSink(t *testing.T) {
	sink := newDefaultSink()
	cs, ok := sink.(*ConsoleSink)
	if !ok {
		t.Fatalf("默认出口应为 *ConsoleSink，实际 %T", sink)
	}
	if cs.w == nil {
		t.Fatal("默认出口必须绑定一个 writer")
	}
}

// TestConsoleSinkEndToEnd：真跑请求，看默认装配接上这个出口后写出什么。
// 同时覆盖「按 event 分版式」：装配期记录走事件行，访问记录走列式行。
func TestConsoleSinkEndToEnd(t *testing.T) {
	var buf bytes.Buffer
	e := New(WithSink(NewConsoleSink(&buf, WithColor(false))), WithHostID("svc"), WithTrustedTraceHeader(false))
	t.Cleanup(e.dispose)
	e.GET("/users/{id}", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })
	e.GET("/boom", func(c *Ctx) error { return errors.New("boom") })

	doReq(e, "GET", "/users/42", nil)
	doReq(e, "GET", "/boom", nil)

	out := buf.String()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")

	// 装配期记录：事件行（`pulse.*` / `observability.*` 出现在第一个 ` | ` 之后）。
	var assembly int
	for _, ln := range lines {
		if strings.Contains(ln, "observability.host_ready") || strings.Contains(ln, "pulse.kernel.fiber_state") {
			assembly++
			if !strings.Contains(ln, consoleSep) {
				t.Fatalf("装配期记录应是事件行：%q", ln)
			}
		}
	}
	if assembly == 0 {
		t.Fatalf("装配期记录没走事件行：\n%s", out)
	}

	// 访问记录：列式行（` | <status> | <耗时> | <客户端> | <方法 路径>`）。
	var ok200, err500 bool
	for _, ln := range lines {
		switch {
		case strings.Contains(ln, "| 200 |") && strings.Contains(ln, "GET    /users/42"):
			ok200 = true
			if !strings.Contains(ln, "| route=/users/{id} |") {
				t.Fatalf("匹配到路由时应带 route= 尾段：%q", ln)
			}
			if !strings.Contains(ln, "host=svc") {
				t.Fatalf("配置了 HostID 应出现在行里：%q", ln)
			}
		case strings.Contains(ln, "| 500 |") && strings.Contains(ln, "GET    /boom"):
			err500 = true
			if !strings.Contains(ln, "internal boom") {
				t.Fatalf("500 行应带错误分类 + 文本：%q", ln)
			}
		}
	}
	if !ok200 || !err500 {
		t.Fatalf("请求行缺失（200=%v 500=%v）：\n%s", ok200, err500, out)
	}
}
