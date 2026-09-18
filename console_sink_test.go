package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/observability"
)

// 本文件覆盖 console_sink.go：默认出口的行版式、错误可见性，以及框架关闭时对出口的 flush 调用。
// 边界：出口怎么选、AsyncSink 的组合语义属上游 observability，这里只测框架侧怎么用它们。

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
	want := "PULSE | 2026/09/14 - 08:30:00 | 200 |   585.1µs | 192.0.2.1:1234  | GET     /users/42" +
		" | route=/users/{id} | size=29 | host=pulse-web | trace=8f2e1a3b4c5d6e7f8a9b0c1d2e3f4a5b\n"
	if got != want {
		t.Fatalf("版式不符：\n got=%q\nwant=%q", got, want)
	}
}

// TestConsoleSinkPaddingUsesDisplayWidth 钉住列补齐按**显示宽度**算，不是 rune 数。
//
// 全角字符占 2 列：`客户端-甲:1234` 是 10 rune / 14 显示列，正确补齐后该列占满
// 15 显示列（补 1 个空格）。按 rune 数补会补 5 个空格、把后续列整体推右 4 格——
// 旧实现就是那样（`utf8.RuneCount`），迁移时换成了上游 `DisplayWidth`。
func TestConsoleSinkPaddingUsesDisplayWidth(t *testing.T) {
	rec := httpRecord("200", 585100*time.Nanosecond)
	observability.Set(&rec.Attrs, attrClientAddr, "客户端-甲:1234")

	got := renderLine(t, rec)
	want := "PULSE | 2026/09/14 - 08:30:00 | 200 |   585.1µs | 客户端-甲:1234  | GET     /users/42" +
		" | route=/users/{id} | size=29 | host=pulse-web | trace=8f2e1a3b4c5d6e7f8a9b0c1d2e3f4a5b\n"
	if got != want {
		t.Fatalf("客户端列应按显示宽度占满 %d 列（全角算 2 列）：\n got=%q\nwant=%q", colClient, got, want)
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
		if !strings.Contains(got, "| GET     /nope |") {
			t.Fatalf("路径列应给具体路径且不留空列：%q", got)
		}
		if strings.Contains(got, "route=") {
			t.Fatalf("没有路由模板时不该出现 route=：%q", got)
		}
	})

	t.Run("方法/客户端缺值补 -", func(t *testing.T) {
		rec := httpRecord("200", 0)
		rec.Attrs = observability.Attrs{}
		// fields[0] 是行首标识 PULSE，[1] 才是时间列。
		fields := consoleFields(renderLine(t, rec))
		if len(fields) < 6 {
			t.Fatalf("列数不足：%q", fields)
		}
		if fields[2] != "200" {
			t.Errorf("状态列 = %q，want 200", fields[2])
		}
		if !strings.HasSuffix(fields[3], "0ns") {
			t.Errorf("耗时列应带单位：%q", fields[3])
		}
		if fields[4] != "-" {
			t.Errorf("客户端缺值应为 -，实得 %q", fields[4])
		}
		if got := strings.Fields(fields[5]); len(got) != 2 || got[0] != "-" || got[1] != "-" {
			t.Errorf("方法/路径缺值应各补 -，实得 %q", fields[5])
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
	want := "PULSE | 2026/09/14 - 08:30:00 | pulse.kernel.fiber_state host=svc fiber=db state=Starting→Running\n"
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

// TestConsoleSinkColumnKeyWithWrongType 钉住一条不变式：列键存成非预期类型时，
// 该属性必须落进兜底组，不能在「固定列」与「兜底」之间被两头跳过。
//
// 判定得用「这一列**真的渲染了值**」（consumed），不能用「这个名字属于某一列」——
// 后者在类型不符时仍为真，于是固定列没渲染它、兜底组又把它当已渲染跳过，
// 整条属性静默消失。
func TestConsoleSinkColumnKeyWithWrongType(t *testing.T) {
	rec := httpRecord("200", time.Microsecond)
	observability.Set(&rec.Attrs, attrHTTPBodySize, "29") // 期望 int64，这里存成 string

	got := renderLine(t, rec)
	if !strings.Contains(got, attrHTTPBodySize+"=29") {
		t.Fatalf("类型不符的列键被静默丢弃了：%q", got)
	}
	if strings.Contains(got, consoleSep+"size=") {
		t.Fatalf("类型不符时不该渲染成固定列：%q", got)
	}
}

// TestConsoleSinkMethodColumnAligned：7 字符方法（OPTIONS / CONNECT）不能把路径列
// 顶偏一格——列式版式的价值就在对齐。
func TestConsoleSinkMethodColumnAligned(t *testing.T) {
	pathStart := func(method string) int {
		rec := httpRecord("200", time.Microsecond)
		observability.Set(&rec.Attrs, attrHTTPMethod, method)
		return strings.Index(renderLine(t, rec), "/users/42")
	}
	if got, want := pathStart("OPTIONS"), pathStart("GET"); got != want {
		t.Fatalf("方法列未对齐：OPTIONS 的路径起点在第 %d 字节，GET 在第 %d 字节", got, want)
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

// failingWriter 每次写都失败（stdout 管道被关掉 / journald socket 满 / 磁盘满），
// 并记录被调用次数——用来验证「失败之后仍然继续尝试写」。
type failingWriter struct {
	err   error
	calls int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.calls++
	return 0, w.err
}

// TestConsoleSinkErrSurfacesWriteFailure 是写失败那条路的行为回归。
//
// 设计文档写着「写错误不抛，但可查」，而 `Err()` / `Flush()` 现在是**内嵌上游出口
// 白送**的——只有「方法是从上游嵌进来的」这层推理撑着，没有任何用例跑过写失败。
// 这里把三件事钉住：首错为准（后续错误不覆盖）、失败后继续尝试写、`Flush()` 把首错
// 交出来（关闭时序第 ⑤ 步读的就是它，见 `sinkFlusher` 与 `TestShutdownFlushesBufferingSink`）。
func TestConsoleSinkErrSurfacesWriteFailure(t *testing.T) {
	w := &failingWriter{err: errors.New("broken pipe")}
	s := NewConsoleSink(w, WithColor(false))

	if err := s.Err(); err != nil {
		t.Fatalf("还没写过就 Err()：%v", err)
	}

	s.Write(httpRecord("200", time.Microsecond))
	if s.Err() == nil {
		t.Fatal("写失败之后 Err() 必须非 nil——「日志早就不写了却没人知道」正是要避免的形态")
	}
	if got := s.Err().Error(); !strings.Contains(got, "broken pipe") {
		t.Fatalf("Err() 应原样返回写错误，实得 %q", got)
	}

	// 失败之后仍然继续尝试写：不静默退出、不改缓冲策略。
	before := w.calls
	s.Write(httpRecord("500", time.Microsecond))
	if w.calls == before {
		t.Fatal("写失败后不再尝试写：出口静默退化了")
	}

	// 首错为准：后续换成别的错误也不覆盖它。
	w.err = errors.New("second failure")
	s.Write(httpRecord("200", time.Microsecond))
	if got := s.Err().Error(); !strings.Contains(got, "broken pipe") {
		t.Fatalf("Err() 应保持首次错误，实得 %q", got)
	}

	// Flush 把首错交出来（关闭时序第 ⑤ 步据此记 slog.Warn）。
	if err := s.Flush(); err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("Flush() 应返回首错，实得 %v", err)
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
		if !strings.HasPrefix(ln, "PULSE | 2026/09/14 - 08:30:00 | 200 |") {
			t.Fatalf("第 %d 行不完整：%q", i+1, ln)
		}
	}
}

// TestConsoleSinkZeroAlloc 守住「渲染一条 0 分配」这条红线：它在写实现时被踩过
// 一次（兜底组的闭包捕获渲染缓冲，把缓冲顶到堆上，每条 4 次分配），而 0.19µs 的
// 渲染成本里分配是大头。这条用例就是那次事故的回归钉子。
func TestConsoleSinkZeroAlloc(t *testing.T) {
	rec := httpRecord("200", 585*time.Microsecond)
	s := NewConsoleSink(io.Discard, WithColor(false))
	s.Write(rec) // 预热：先把池里的缓冲建起来

	if n := testing.AllocsPerRun(1000, func() { s.Write(rec) }); n != 0 {
		t.Fatalf("渲染一条应 0 分配，实测 %v allocs/op", n)
	}

	event := observability.Record{
		Time: consoleTestTime, HostID: "svc", Event: "pulse.kernel.fiber_state",
		FiberName: "db", From: "A", To: "B",
	}
	s.Write(event)
	if n := testing.AllocsPerRun(1000, func() { s.Write(event) }); n != 0 {
		t.Fatalf("事件行应 0 分配，实测 %v allocs/op", n)
	}

	// 固定列之外还有属性（中间件 / 业务往访问记录里加的字段）——这一格以前没有
	// 用例覆盖：旧实现在这条路上走 `Attrs.Range` 兜底，实测每条 6 次分配；
	// 换成上游 `AppendAttrsExcept`（无闭包）之后是 0。
	withExtras := httpRecord("200", 585*time.Microsecond)
	observability.Set(&withExtras.Attrs, "llm.model", "MiniMax-M3")
	observability.Set(&withExtras.Attrs, "llm.temp", 0.75)
	s.Write(withExtras)
	if n := testing.AllocsPerRun(1000, func() { s.Write(withExtras) }); n != 0 {
		t.Fatalf("含未知属性的行应 0 分配，实测 %v allocs/op", n)
	}
}

// TestConsoleSinkErrReportsFirstFailure：写失败不 panic，`Err()` 给出**首次**错误，
// 不被后续失败或后续成功覆盖——语义与上游 `observability.LineSink.Err()` 一致。
func TestConsoleSinkErrReportsFirstFailure(t *testing.T) {
	rec := httpRecord("200", time.Microsecond)

	var buf bytes.Buffer
	ok := NewConsoleSink(&buf, WithColor(false))
	if err := ok.Err(); err != nil {
		t.Fatalf("没有写失败时 Err() 应为 nil，实得 %v", err)
	}
	ok.Write(rec)
	if err := ok.Err(); err != nil {
		t.Fatalf("写成功后 Err() 仍应为 nil，实得 %v", err)
	}

	first := errors.New("write: broken pipe")
	fw := &failingWriter{err: first}
	s := NewConsoleSink(fw, WithColor(false))
	s.Write(rec)
	if !errors.Is(s.Err(), first) {
		t.Fatalf("Err() 应报出首次写失败 %v，实得 %v", first, s.Err())
	}

	fw.err = errors.New("write: no space left on device")
	s.Write(rec)
	if !errors.Is(s.Err(), first) {
		t.Fatalf("首次错误被后来的失败覆盖：%v", s.Err())
	}
	if fw.calls != 2 {
		t.Fatalf("写失败后应继续尝试写（不静默退出），实得 %d 次调用", fw.calls)
	}
}

// TestConsoleSinkErrConcurrent：`Err()` 与并发写共用一把锁——-race 下必须干净。
func TestConsoleSinkErrConcurrent(t *testing.T) {
	fw := &failingWriter{err: errors.New("write: broken pipe")}
	s := NewConsoleSink(fw, WithColor(false))

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 32; j++ {
				s.Write(httpRecord("200", time.Microsecond))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 256; j++ {
			_ = s.Err()
		}
	}()
	wg.Wait()

	if s.Err() == nil {
		t.Fatal("整程都在写失败，Err() 不该是 nil")
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

// TestConsoleSinkDurationColumn：耗时列的单位与精度，口径来自上游
// `observability.AppendDuration`——它与另一个出口的 `duration_ms` 是同一条事实的
// 人读面与机器面。
//
// 这条同时钉住**迁移到 LineSink 的五处变化里的一处**：耗时从「浮点四舍五入」
// 改成上游口径的「整数截断」。旧实现走 `strconv.AppendFloat('f')`，585199ns 会
// 渲染成 `585.2µs`、7629999ns 成 `7.63ms`；上游口径是 `585.1µs` / `7.62ms`。
// 另四处是行首标识（`PULSE`）、列补齐按显示宽度、键名按需加引号、短状态列的
// ANSI 位置，理由写在 Issue #27：口径必须是各出口共用的一致性资产，本地不再留
// 第二份实现。
// 谁把它改回四舍五入（或改小数位），先红在这条上。
func TestConsoleSinkDurationColumn(t *testing.T) {
	durCol := func(d time.Duration) string {
		// fields[0] 是行首标识、[1] 是时间列，耗时列在 [3]。
		return consoleFields(renderLine(t, httpRecord("200", d)))[3]
	}
	cases := []struct {
		d    time.Duration
		want string
	}{
		{820 * time.Nanosecond, "820ns"},
		{585100 * time.Nanosecond, "585.1µs"},
		{585199 * time.Nanosecond, "585.1µs"}, // 截断，不是 585.2
		{7623 * time.Microsecond, "7.62ms"},
		{7629999 * time.Nanosecond, "7.62ms"}, // 截断，不是 7.63
		{1234 * time.Millisecond, "1.23s"},
	}
	for _, c := range cases {
		if got := durCol(c.d); got != c.want {
			t.Errorf("耗时列(%v) = %q，want %q", c.d, got, c.want)
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
	if cs.LineSink == nil {
		t.Fatal("默认出口必须是一层挂在 LineSink 上的薄壳")
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
		case strings.Contains(ln, "| 200 |") && strings.Contains(ln, "GET     /users/42"):
			ok200 = true
			if !strings.Contains(ln, "| route=/users/{id} |") {
				t.Fatalf("匹配到路由时应带 route= 尾段：%q", ln)
			}
			if !strings.Contains(ln, "host=svc") {
				t.Fatalf("配置了 HostID 应出现在行里：%q", ln)
			}
		case strings.Contains(ln, "| 500 |") && strings.Contains(ln, "GET     /boom"):
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

// 关闭期出口 flush 的形态覆盖。
//
// 上游有两种出口形态：带 ctx 的（`observability.AsyncSink`）与不带的
// （`observability.LineSink`）。只认其中一种会静默跳过另一种——LineSink
// 形态下「关闭前未达阈值的最后一批」会随进程消失，不报错、不告警。

// noFlushSink 连 Flush 都没有，用来确认探测不会误判。
type noFlushSink struct{}

func (noFlushSink) Write(observability.Record) {}

// ctxFlushRecorder / noCtxFlushRecorder 分别实现两种 flush 形态。
type ctxFlushRecorder struct{ called bool }

func (f *ctxFlushRecorder) Write(observability.Record) {}

func (f *ctxFlushRecorder) Flush(context.Context) error { f.called = true; return nil }

type noCtxFlushRecorder struct{ called bool }

func (f *noCtxFlushRecorder) Write(observability.Record) {}

func (f *noCtxFlushRecorder) Flush() error { f.called = true; return nil }

func TestSinkFlusherCoversBothShapes(t *testing.T) {
	ctxSink := &ctxFlushRecorder{}
	flush, ok := sinkFlusher(ctxSink)
	if !ok {
		t.Fatal("带 ctx 的形态（observability.AsyncSink）未被识别")
	}
	if err := flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ctxSink.called {
		t.Fatal("带 ctx 的 Flush 未被调用")
	}

	noCtx := &noCtxFlushRecorder{}
	flush, ok = sinkFlusher(noCtx)
	if !ok {
		t.Fatal("不带 ctx 的形态（observability.LineSink）未被识别——这正是会静默丢记录的那一种")
	}
	if err := flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !noCtx.called {
		t.Fatal("不带 ctx 的 Flush 未被调用")
	}

	if _, ok := sinkFlusher(noFlushSink{}); ok {
		t.Fatal("无 Flush 的出口不应被判定为可 flush")
	}
	if _, ok := sinkFlusher(nil); ok {
		t.Fatal("nil 出口不应被判定为可 flush")
	}

	// 真实出口：把上游的两个实现钉在探测面上。
	var buf bytes.Buffer
	if _, ok := sinkFlusher(observability.NewLineSink(&buf)); !ok {
		t.Fatal("observability.LineSink 必须被识别")
	}
	// 默认出口也是这一形态（内嵌 LineSink）——它是 New() 实际装配的那一个，
	// 漏了它，「关闭前最后一批随进程消失」会落回默认路径。
	var consoleBuf bytes.Buffer
	if _, ok := sinkFlusher(NewConsoleSink(&consoleBuf, WithColor(false))); !ok {
		t.Fatal("默认出口 ConsoleSink 必须被识别")
	}
	async := observability.NewAsyncSink(noFlushSink{})
	defer func() { _ = async.Close(context.Background()) }()
	if _, ok := sinkFlusher(async); !ok {
		t.Fatal("observability.AsyncSink 必须被识别")
	}
}

// TestShutdownFlushesBufferingSink 是缺陷回归：LineSink 缓冲未达阈值时不落盘，
// 关闭时序第 ⑤ 步必须把它 flush 出去。
//
// 复现缺陷的做法（依赖 v0.2.0 的老版本时不能照字面跑本用例——`NewLineSink`
// 在 v0.2.0 不存在，会直接编译失败）：**保持依赖不变、只回退
// `engine.go` 的 `sinkFlusher`**，用一个只实现 `Flush() error` 的等价出口，
// 即可在旧代码上看到 `out.Len() == 0`。本用例断言的就是那个出口。
func TestShutdownFlushesBufferingSink(t *testing.T) {
	var out bytes.Buffer
	e, _ := newTestEngine(t, WithSink(observability.NewLineSink(&out)))
	e.GET("/x", func(c *Ctx) error {
		c.Observe("probe.event", nil)
		return c.Text(http.StatusOK, "ok")
	})

	if rec := doReq(e, "GET", "/x", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if out.Len() != 0 {
		t.Fatalf("LineSink 未达阈值时不应落盘，实际已写 %d 字节", out.Len())
	}

	if err := e.shutdown(&http.Server{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("关闭后出口缓冲仍为空：flush 探测没覆盖 LineSink 形态")
	}
	if !strings.Contains(out.String(), "probe.event") {
		t.Fatalf("关闭后落盘内容不含业务记录: %q", out.String())
	}
}
