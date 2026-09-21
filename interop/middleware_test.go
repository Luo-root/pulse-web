package interop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/cors"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse/observability"
)

// 生态中间件对照矩阵（#92）。
//
// # 判据
//
// **同一个中间件、同一条路由，经 `web.Adapt`（洋葱内）与包在 `app.Handler()`
// 外面（外包）逐项比对**。一条请求比对三个观测面：
//
//	响应      状态码 + 关心的响应头 + body
//	中间件    它自己的旁观测——日志行、指标标签（`mount.side`）
//	框架      框架侧记下的那条访问记录（状态码 / 路由模板 / 体积 / 错误类别）
//
// 第三个面单独列出来，是因为站点那张表里有一格专讲它：「中间件短路写响应」这一行，
// 外包是「框架完全不知情」而 Adapt 是「记回采集层」——只比响应比不出这个差别。
//
// # 两个方向都要红
//
//   - 出现没申报的差异 → 红（适配面坏了）
//   - 申报了差异但两边其实一致 → 也红（声明过期：要么边界消失，要么断言写错）
//
// # 为什么不用「跑通了」当判据
//
// naive 适配器（只用公开 API 把 writer 包一层）会让抓状态码的中间件读到 0、让改写
// body 的中间件发出「Content-Encoding: gzip + 明文 body」这种损坏响应——状态码都是
// 200，光看「没报错」看不出来。
//
// # 另外三条纪律
//
//   - **有状态的中间件两个挂载点各造一份实例**（`mountFactory` 就是为此存在）：共用
//     一个实例时先跑的那一侧会把状态吃掉，测出来的是「状态串台」而不是适配面。
//   - **每行都要有非空断言**（`matrixRun.need`）：矩阵只比两侧，比不出「两边都没
//     生效」——「都加了同一个头」与「都没加」在比对里长得一样。
//   - **申报了差异，还要钉住差异的内容**：`known` 只说「这里会不一样」，至于不一样
//     的是不是我们说的那件事，靠 `need` / `outerAbsent` / `needStatus` 正面断言。
//     要注意**申报过的行不再逐项比对**，所以那几行客户端收到的状态码得用 `needStatus`
//     单独钉住——否则「中间件记错了」与「响应也错了」在这张矩阵里长得一样。
//
// 未匹配路由（真 404）与尾斜杠**不在** `defaultReqs()` 里：它们压根到不了洋葱内的
// 中间件，那个边界由 TestMatrixUnmatchedRoutesBypassOnion 单独断言。

// ---- 骨架 ----

// mount 描述一个挂载点：中间件本体 + 它自己的旁观测 + 它需要额外注册的路由。
//
// 工厂会被**调用两次**（洋葱内 / 外包各一次）。
type mount struct {
	// mw 是 stdlib 形状的中间件。
	mw func(http.Handler) http.Handler
	// probeRoutes 可空：把中间件注入的值读出来、写进 body 的路由（路径 → handler，
	// 注册成 GET）。中间件放进 request context（或换过的 request）里的东西，只有被
	// handler 读出来才进得了比对面——观测面只认响应里看得见的事实。
	// 注册发生在 `Use` 之后，所以这些路由照样在洋葱里。
	probeRoutes map[string]web.Handler
	// allowPreflight 给骨架里的每条 GET 路由**同时注册一条 OPTIONS**——预检那条绕法。
	// 路由匹配先于中间件（见 TestMatrixUnmatchedRoutesBypassOnion），不注册 OPTIONS
	// 时预检请求根本到不了洋葱内。这条路由只负责把请求送进洋葱，响应仍由中间件写。
	allowPreflight bool
	// side 可空：中间件自己的旁观测，返回**自上次调用以来**的新增。
	side func() string
}

type mountFactory func() mount

// once 把无状态中间件包成工厂。
func once(mw func(http.Handler) http.Handler) mountFactory {
	return func() mount { return mount{mw: mw} }
}

// reqSpec 是一条对照请求。
type reqSpec struct {
	name    string
	method  string
	path    string
	headers map[string]string
}

// defaultReqs 是矩阵固定跑的请求集：正常、handler 返回 error、panic、路径参数。
// 全部带 `Accept-Encoding: gzip`，让改写 body 的中间件（Compress）每条都真的走上压缩
// 路径——只在一条请求上开 gzip 会让另外几条看起来「一致」，其实什么都没验。
func defaultReqs() []reqSpec {
	gz := func(extra map[string]string) map[string]string {
		h := map[string]string{"Accept-Encoding": "gzip"}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}
	return []reqSpec{
		{"GET /ok", http.MethodGet, "/ok", gz(nil)},
		{"GET /err", http.MethodGet, "/err", gz(nil)},
		{"GET /panic", http.MethodGet, "/panic", gz(nil)},
		{"GET /users/42", http.MethodGet, "/users/42", gz(nil)},
	}
}

// watchHeaders 是矩阵比对的响应头。**值逐次不同的头只比「在不在」**：请求 id / trace
// id / cookie 每条请求都换，比字面值等于自找假阴性。
//
// `X-Interop-Saw` 是边界用例自己塞的标记（未匹配路由那条，见文件末尾）——放进同一张
// 清单，它才进得了比对面；`Location` 只有「路径需要归一化」那条会带。
var watchHeaders = []string{
	"Access-Control-Allow-Credentials", "Access-Control-Allow-Methods",
	"Access-Control-Allow-Origin", "Access-Control-Max-Age", "Allow",
	"Content-Encoding", "Content-Type", "Location", "Retry-After", "Set-Cookie",
	"Vary", "WWW-Authenticate", "X-Content-Type-Options", "X-Interop-Saw",
	"X-Real-Ip", "X-Request-Id", "X-Trace-Id",
}

var volatileHeader = map[string]bool{
	"Set-Cookie": true, "X-Request-Id": true, "X-Trace-Id": true,
}

// obs 是一条请求的观测快照，可直接比较。
type obs struct {
	status   int
	headers  string
	body     string
	rec      string // 框架侧记录 + 中间件旁观测
	panicked string // 非空 = panic 逃到了调用方（两边都 panic 也算「一致」）
}

func (o obs) String() string {
	var b strings.Builder
	if o.panicked != "" {
		fmt.Fprintf(&b, "PANIC(%s) ", o.panicked)
	}
	fmt.Fprintf(&b, "%d | %s | %s | %s", o.status, o.headers, o.body, o.rec)
	return b.String()
}

// routes 造矩阵用的引擎：`sink` 是挂载点专属的采集口（框架侧记录进比对面），
// `probes` / `allowPreflight` 来自挂载点自己的字段（见 mount）。
func routes(mw web.Middleware, sink observability.Sink, probes map[string]web.Handler, allowPreflight bool) *web.Engine {
	app := web.New(web.WithSink(sink))
	if mw != nil {
		app.Use(mw)
	}
	// 骨架路由统一从这里走：`allowPreflight` 打开时给同一条路径补一条 OPTIONS。
	route := func(path string, h web.Handler) {
		app.GET(path, h)
		if allowPreflight {
			app.OPTIONS(path, func(c *web.Ctx) error { return c.NoContent(http.StatusNoContent) })
		}
	}
	route("/ok", func(c *web.Ctx) error {
		return c.JSON(http.StatusOK, map[string]string{"a": "b"})
	})
	route("/err", func(c *web.Ctx) error { return web.NotFound("nope", nil) })
	route("/panic", func(c *web.Ctx) error { panic("boom") })
	route("/users/{id}", func(c *web.Ctx) error { return c.Text(http.StatusOK, "user:"+c.Path("id")) })
	route("/slow", func(c *web.Ctx) error {
		time.Sleep(150 * time.Millisecond)
		return c.Text(http.StatusOK, "slow")
	})
	for path, h := range probes {
		app.GET(path, h)
	}
	return app
}

// observe 跑一次请求并把结果压成可比较的快照。
func observe(h http.Handler, spec reqSpec, sink *recordSink) obs {
	req := httptest.NewRequest(spec.method, spec.path, nil)
	for k, v := range spec.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()

	var panicked string
	func() {
		defer func() {
			if p := recover(); p != nil {
				panicked = fmt.Sprint(p)
			}
		}()
		h.ServeHTTP(rec, req)
	}()

	parts := make([]string, 0, len(watchHeaders))
	for _, name := range watchHeaders {
		v := rec.Header().Get(name)
		if v == "" {
			continue
		}
		if volatileHeader[name] {
			parts = append(parts, name+"=<present>")
			continue
		}
		parts = append(parts, name+"="+v)
	}
	sort.Strings(parts)

	body := rec.Body.String()
	if len(body) > 60 {
		body = body[:60] + "…"
	}
	return obs{
		status:   rec.Code,
		headers:  strings.Join(parts, " "),
		body:     strings.ReplaceAll(body, "\n", "\\n"),
		rec:      sink.drain(),
		panicked: panicked,
	}
}

// matrixRun 留住每条的观测，供用例在比对之外做非空断言。
type matrixRun struct {
	adapt map[string]obs
	outer map[string]obs
}

// need 断言**经 Adapt 那一侧真的有效果**：观测里必须出现 want 这段文本。
//
// 没有它，一条「中间件两边都没生效」的接线也会以「两边一致」通过——矩阵只比两侧，
// 比不出「什么都没有」。
func (r matrixRun) need(t *testing.T, req, want string) {
	t.Helper()
	if got := r.adapt[req]; !strings.Contains(got.String(), want) {
		t.Errorf("非空断言失败：经 Adapt 的 %s 观测里没有 %q——这一行可能只是「两边都没生效」\n  实际: %s",
			req, want, got)
	}
}

// outerAbsent 断言外包那一侧的观测里**没有** want。
//
// 用于「外包框架完全不知情」这类**负面事实**：`known` 只说两侧会不一样，至于不一样
// 的是不是「外包那侧少了这个」，得正面钉住。
func (r matrixRun) outerAbsent(t *testing.T, req, want string) {
	t.Helper()
	if got := r.outer[req]; strings.Contains(got.String(), want) {
		t.Errorf("外包的 %s 观测里不该有 %q（申报的差异正是「外包这侧没有它」）\n  实际: %s", req, want, got)
	}
}

// needStatus 断言经 Adapt 那一侧的**响应状态码**（客户端真正收到的那个）。
//
// 为什么不能只靠 `need(status="…")`：那个 `status=` 是**框架记录**里的字段，不是响应
// 本身。而两侧响应的逐项比对在 `known` 申报过的行上会被跳过——短路、错误、panic 那几
// 行恰恰全是申报过的，于是「响应状态码」这一个维度没有任何正面判据：把中间件写的状态
// 码吞掉（响应只剩 body 带来的缺省 200）也能全绿。
func (r matrixRun) needStatus(t *testing.T, req string, want int) {
	t.Helper()
	if got := r.adapt[req]; got.status != want {
		t.Errorf("经 Adapt 的 %s 响应状态码 = %d，want %d\n  实际: %s", req, got.status, want, got)
	}
}

// compareMatrix 是矩阵本体：两个挂载点各观测一遍，逐条比对。
//
// `known` 把「两边本就该不同」的条目写成 `请求名 -> 为什么`；申报了却一致同样报错。
func compareMatrix(t *testing.T, name string, factory mountFactory, reqs []reqSpec, known map[string]string) matrixRun {
	t.Helper()
	if reqs == nil {
		reqs = defaultReqs()
	}

	// 两个挂载点各造一份中间件实例与采集口。
	inMount, outMount := factory(), factory()
	inSink, outSink := &recordSink{}, &recordSink{}
	inHandler := routes(web.Adapt(inMount.mw), inSink, inMount.probeRoutes, inMount.allowPreflight).Handler()
	outHandler := outMount.mw(routes(nil, outSink, outMount.probeRoutes, outMount.allowPreflight).Handler())

	run := matrixRun{adapt: map[string]obs{}, outer: map[string]obs{}}
	for _, spec := range reqs {
		in := observe(inHandler, spec, inSink)
		if inMount.side != nil {
			in.rec += " mid{" + inMount.side() + "}"
		}
		out := observe(outHandler, spec, outSink)
		if outMount.side != nil {
			out.rec += " mid{" + outMount.side() + "}"
		}
		run.adapt[spec.name], run.outer[spec.name] = in, out

		why, declared := known[spec.name]
		switch {
		case in == out && declared:
			t.Errorf("%s / %s：用例申报了已知边界（%s），但两个挂载点其实**一致**——"+
				"要么边界真的消失了（好事，删掉这条申报），要么申报写错了", name, spec.name, why)
		case in == out:
			t.Logf("%s / %s：一致 → %s", name, spec.name, in)
		case declared:
			t.Logf("%s / %s：已知边界（%s）\n  经 Adapt: %s\n  外包    : %s", name, spec.name, why, in, out)
		default:
			t.Errorf("%s / %s：两个挂载点不一致\n  经 Adapt: %s\n  外包    : %s",
				name, spec.name, in, out)
		}
	}
	return run
}

// ---- 框架侧记录 ----

// eventHTTPReq 是访问记录的事件名（框架内部同名常量未导出，这里照抄一份——名字漂了
// 这条过滤就会把记录面整个滤空，但**不会静默**：记录面的断言全是正面断言
// （`need` 那一批），滤空时它们先红，见 TestRecordSinkOneRecordPerRequest）。
//
// 为什么要过滤：内核启动期的记录（fiber_state 等）也会流进 Sink——内核被第一条请求懒
// 加载时冒出来，**什么时候到是不确定的**，而框架的访问记录是同步写出的。两者用事件名
// 分开，语义上就不是一类东西。
const eventHTTPReq = "http.request"

// TestRecordSinkOneRecordPerRequest 钉住采集口的**时序边界**：访问记录是框架在
// `ServeHTTP` 收尾时**同步**写出来的，所以 drain 紧跟在返回之后就能拿到，既不需要等待、
// 也不会有「上一条的残留被算进下一条」。这条边界是整套矩阵的前提——它不是同步的，
// 逐请求比对就无意义（`context_test.go` 那条 `waitRecord` 存在的理由，是那边走真
// socket，客户端读完响应时服务端收尾可能还没跑完）。
func TestRecordSinkOneRecordPerRequest(t *testing.T) {
	sink := &recordSink{}
	app := web.New(web.WithSink(sink))
	app.GET("/ok", func(c *web.Ctx) error { return c.Text(http.StatusOK, "ok") })
	h := app.Handler()

	for i := 1; i <= 3; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ok", nil))
		got := sink.drain()
		if n := strings.Count(got, "; ") + 1; got == "" || n != 1 {
			t.Errorf("第 %d 条请求 drain 出 %d 行（%q）——访问记录要么没同步落盘、"+
				"要么混进了别的行（内核记录的事件名与 %q 相同？）", i, n, got, eventHTTPReq)
		}
	}
}

// recordSink 接住框架写出的访问记录。只挑与中间件语义有关的字段：trace id 与耗时
// 每条请求都不同，比它们等于自找假阴性。值一律加引号——**空值（未匹配路由的
// `http.route`）与缺席是两件事**，不加引号它们长得一样。
var recordKeys = []string{"http.request.method", "http.route", "http.response.body.size", "error.type"}

type recordSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *recordSink) Write(rec observability.Record) {
	if rec.Event != eventHTTPReq {
		return
	}
	vals := map[string]string{}
	rec.Attrs.Range(func(k string, v any) { vals[k] = fmt.Sprint(v) })

	parts := []string{`status="` + rec.Status + `"`}
	for _, k := range recordKeys {
		if v, ok := vals[k]; ok {
			// 体积缺席（hijack 那条口径）与 0 是两件事，所以缺席要说出来。
			parts = append(parts, k+"=\""+v+"\"")
			continue
		}
		if k == "http.response.body.size" {
			parts = append(parts, k+"=<absent>")
		}
	}
	if rec.Err != nil {
		parts = append(parts, "err=<present>")
	}

	s.mu.Lock()
	s.lines = append(s.lines, strings.Join(parts, " "))
	s.mu.Unlock()
}

// drain 返回自上次调用以来记下的行（一条请求一条）。
func (s *recordSink) drain() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := strings.Join(s.lines, "; ")
	s.lines = nil
	return out
}

// ---- chi 全家 ----

// TestMatrixChiRequestID：最普通的一条——把请求 id 放进 request context，handler 读。
//
// 两条路都压着 `Adapt` 的承诺 ③（换过的 request 传得下去），因为值不在 request 对象
// 上，在它换过的那层 context 里：
//
//   - 入站带了 id 时中间件沿用它 → 值确定，可以**逐字比**
//   - 没带时中间件自己生成（进程级前缀 + 原子计数，两个挂载点还会各递增一次）→ 值
//     逐请求不同，比的是「拿没拿到」
func TestMatrixChiRequestID(t *testing.T) {
	run := compareMatrix(t, "chi middleware.RequestID", func() mount {
		return mount{
			mw: middleware.RequestID,
			probeRoutes: map[string]web.Handler{"/rid": func(c *web.Ctx) error {
				id := middleware.GetReqID(c.Request().Context())
				switch {
				case id == "":
					return c.Text(http.StatusOK, "rid:absent")
				case strings.HasPrefix(id, "inbound-"):
					return c.Text(http.StatusOK, "rid:"+id)
				default:
					return c.Text(http.StatusOK, "rid:generated")
				}
			}},
		}
	}, []reqSpec{
		{"GET /rid (inbound id)", http.MethodGet, "/rid", map[string]string{"X-Request-Id": "inbound-123"}},
		{"GET /rid (generated id)", http.MethodGet, "/rid", nil},
		{"GET /ok", http.MethodGet, "/ok", nil},
	}, nil)

	run.need(t, "GET /rid (inbound id)", "rid:inbound-123")
	run.need(t, "GET /rid (generated id)", "rid:generated")
}

// TestMatrixChiClientIPFromXFF：同一族的另一条——中间件解析 `X-Forwarded-For` 把客户端
// IP 放进 request context，handler 用 `GetClientIP` 取。
//
// 用 `ClientIPFromXFF` 而不是 `middleware.RealIP`：后者在 chi v5.3.2 已标 Deprecated，
// 理由是它改写 `r.RemoteAddr`、可被伪造（GHSA-3fxj-6jh8-hvhx 等三条）。新写法把解析
// 结果放进 context，**这个形状更考适配面**——值不在 request 对象本身。
func TestMatrixChiClientIPFromXFF(t *testing.T) {
	cases := []reqSpec{
		{"GET /ip (client, trusted hop)", http.MethodGet, "/ip", map[string]string{"X-Forwarded-For": "203.0.113.7, 10.0.0.5"}},
		{"GET /ip (trusted hop only)", http.MethodGet, "/ip", map[string]string{"X-Forwarded-For": "10.0.0.5"}},
		{"GET /ip (no XFF)", http.MethodGet, "/ip", nil},
		{"GET /ok (no XFF)", http.MethodGet, "/ok", nil},
	}
	run := compareMatrix(t, "chi middleware.ClientIPFromXFF", func() mount {
		return mount{
			mw: middleware.ClientIPFromXFF("10.0.0.0/8"),
			probeRoutes: map[string]web.Handler{"/ip": func(c *web.Ctx) error {
				return c.Text(http.StatusOK, "ip:"+middleware.GetClientIP(c.Request().Context()))
			}},
		}
	}, cases, nil)

	// 客户端 IP 逐字进了 body（两侧都得是它）；只有可信跳的那条按 fail-closed 留空。
	run.need(t, "GET /ip (client, trusted hop)", "ip:203.0.113.7")
	run.need(t, "GET /ip (trusted hop only)", "ip: |")
}

// TestMatrixChiRecoverer：**已知边界**——panic 谁接、500 长什么样归中间件自己。
//
//   - 洋葱内：Recoverer 先接住（它在框架的 recover 里面），写它自己的 500（**空体**）
//   - 外包：框架自己的 recover 先接住，写统一错误体
//
// 两边都 500，但 body 不同——这正是站点那句「panic 兜底用框架自己的」，也是 `Adapt`
// godoc 里「不改中间件语义」的实例。
//
// 这一行还带出**框架侧的一个已知缺陷**（见 [#96](https://github.com/Luo-root/pulse-web/issues/96)）：
// 中间件在 `next` 外面写了响应时，框架侧那条访问记录写的是 200（框架自己的 writer 一个字节
// 都没写，收尾落了缺省值），而客户端拿到的是 500。下面那条断言**把缺陷的现状钉住**：
//
//   - 它是一份**可执行的**缺陷记录——#96 修好那天它会红，提醒改成 `status="500"` 并删掉
//     `known` 里那条申报，而不是让缺陷悄悄溜过去；
//   - 之所以不改成 `known` 里的申报：`known` 的粒度是**整条观测**（两侧不一样就申报），
//     它说不出「不一样的是哪一格」。这一行恰恰是「响应一样（都 500）、记录不一样」，
//     只有正面断言能把「记录写成 200」单独钉住。
func TestMatrixChiRecoverer(t *testing.T) {
	known := map[string]string{
		"GET /panic": "Recoverer 在洋葱内先接住 panic，写它自己的空体 500；外包时框架的 recover 先接住，写统一错误体",
	}
	run := compareMatrix(t, "chi middleware.Recoverer", once(middleware.Recoverer), nil, known)
	run.need(t, "GET /err", `status="404"`)
	// 响应面：客户端拿到的是 500（Recoverer 自己写的那条）。这条与下面 #96 的断言合成
	// 一对——**响应是对的，错的是记录**。
	run.needStatus(t, "GET /panic", http.StatusInternalServerError)
	// 已知缺陷 #96 的现状（修好后：断言 status="500"，并删掉上面 known 里那条申报）。
	if got := run.adapt["GET /panic"]; !strings.Contains(got.rec, `status="200"`) {
		t.Errorf("已知缺陷 #96 的现状变了：洋葱内 Recoverer 写的 500 本应被框架记成 200，实际记的是 %q"+
			"（#96 若已修好：这里改成断言 status=\"500\"，并删掉 known 里 GET /panic 那条申报）", got.rec)
	}
}

// logRecorder 是 chi Logger 的旁观测。用自定义 LogFormatter 而不是它自带的那个：默认
// 格式是给人的一行文（`GET /ok 200 6B 1ms`），解析字符串会把断言变脆；这里直接拿结构化
// 字段——状态码与字节数正是「中间件包 writer 有没有抓到真实值」的判据。
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) NewLogEntry(req *http.Request) middleware.LogEntry {
	return &logEntry{rec: r, method: req.Method, path: req.URL.Path}
}

func (r *logRecorder) add(s string) {
	r.mu.Lock()
	r.lines = append(r.lines, s)
	r.mu.Unlock()
}

func (r *logRecorder) drain() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := strings.Join(r.lines, "; ")
	r.lines = nil
	return out
}

type logEntry struct {
	rec          *logRecorder
	method, path string
}

func (e *logEntry) Write(status, bytes int, _ http.Header, _ time.Duration, _ any) {
	e.rec.add(fmt.Sprintf("%s %s -> status=%d bytes=%d", e.method, e.path, status, bytes))
}

func (e *logEntry) Panic(v any, _ []byte) {
	e.rec.add(fmt.Sprintf("%s %s -> panic(%v)", e.method, e.path, v))
}

// TestMatrixChiLogger：中间件的收尾逻辑抓到的状态码与字节数。
//
// 正常路径两边一致（`200 / 10B`）；**错误路径不一致**——框架的错误映射发生在
// 中间件返回之后，那时中间件的 writer 已经被还原，所以它自己记的是 `0 / 0`，而外包
// 那侧记的是真实值。这是「收尾型中间件」那条边界在观测面上的表现，也是站点表格里
// 「中间件包 writer 抓状态码」那一格必须写清的部分。
func TestMatrixChiLogger(t *testing.T) {
	factory := func() mount {
		rec := &logRecorder{}
		return mount{mw: middleware.RequestLogger(rec), side: rec.drain}
	}
	known := map[string]string{
		"GET /err":   "框架的错误映射在中间件返回之后，洋葱内的 logger 记到 0 / 0；外包记到 404 / 48B",
		"GET /panic": "同一条时序：panic 从洋葱内逃出时 logger 记到 0 / 0；外包记到 500 / 64B",
	}
	run := compareMatrix(t, "chi middleware.Logger", factory, nil, known)
	run.need(t, "GET /ok", "status=200 bytes=10")
	run.need(t, "GET /err", "status=0 bytes=0")
	// 响应面：这两行都在 `known` 里（申报过就不逐项比对了），所以客户端看到的状态码
	// 得正面钉住——不然「中间件记错了」与「响应也错了」在这里长得一样。
	run.needStatus(t, "GET /err", http.StatusNotFound)
	run.needStatus(t, "GET /panic", http.StatusInternalServerError)
	// 记录面的另外三格：`error.type` 的**分类**（`http_4xx` / `panic` 两个不同取值，
	// 不是恒定值）与错误对象本身——只看状态码看不出错误类别记错了。
	run.need(t, "GET /err", `error.type="http_4xx"`)
	run.need(t, "GET /panic", `error.type="panic"`)
	run.need(t, "GET /err", "err=<present>")
	// 框架记录里的路由模板（`/users/{id}` 而不是 `/users/42`）——中间件看不到它，
	// 这正是「要路由模板就挂洋葱内」的理由，也是记录面的一条可失败判据。
	run.need(t, "GET /users/42", `http.route="/users/{id}"`)
	run.need(t, "GET /ok", `http.response.body.size="10"`)
}

// TestMatrixChiCompress：改写 body 的中间件。
//
// `/ok` 这类「handler 正常写出」的请求两边一致（真 gzip 流）；错误与 panic 路径上是
// 已知边界——压缩器在 `next` 返回（或 unwind）之后收尾，那时框架的错误映射刚把响应写进
// **别的** writer，于是洋葱内那侧退化成「没有 Content-Encoding 的明文 404/500」，
// 而外包那侧是合法 gzip。这就是站点那句「动 body 且会在 next 返回后无条件收尾的中间件
// → 外包 Handler()」。
func TestMatrixChiCompress(t *testing.T) {
	known := map[string]string{
		"GET /err":   "压缩器在 next 返回后收尾，框架的错误映射写在另一个 writer 上：洋葱内是**明文** 404（且没有 Content-Encoding），外包是合法 gzip",
		"GET /panic": "同一条时序：洋葱内是明文 500，外包是 gzip 过的 500",
	}
	run := compareMatrix(t, "chi middleware.Compress(5)", once(middleware.Compress(5)), nil, known)
	run.need(t, "GET /ok", "Content-Encoding=gzip")
	run.need(t, "GET /err", `status="404"`)
	// 响应面：申报过的行不比响应，正面钉住客户端看到的两个状态码。
	run.needStatus(t, "GET /err", http.StatusNotFound)
	run.needStatus(t, "GET /panic", http.StatusInternalServerError)
}

// TestMatrixChiTimeout：收尾型中间件里**两边一致**的那个——它在超时后补写 504，而响应
// 已经落定，两个挂载点都吃到「第二次 WriteHeader 不生效」。
func TestMatrixChiTimeout(t *testing.T) {
	reqs := append(defaultReqs(), reqSpec{"GET /slow", http.MethodGet, "/slow", nil})
	run := compareMatrix(t, "chi middleware.Timeout(30ms)", once(middleware.Timeout(30*time.Millisecond)), reqs, nil)
	run.need(t, "GET /slow", "slow")
}

// TestMatrixChiCleanPath：**已知边界**——它读 chi 自己的路由上下文，挂在 pulse-web 上
// 两边都没有那个上下文，所以两个挂载点都会 panic。差别在**谁接住**：
//
//   - 洋葱内：panic 落在 Engine 的 recover 里，兜成框架的统一 500，还记了一条访问
//     记录（`error.type="panic"`）
//   - 外包：panic 在 Engine 之外，直接逃到调用方——没有响应、没有记录
//
// 两边都不能用，适配层也没有把它变得更糟、更没有假装能用。要路径规范化用
// `http.ServeMux` 自己那一套——**未归一的路径它在洋葱之前就 307 掉**，
// 见 TestMatrixUncleanedPathRedirectsBeforeOnion。
func TestMatrixChiCleanPath(t *testing.T) {
	why := "chi.CleanPath 需要 chi 的路由上下文，两个挂载点都 panic；差别是谁接住：" +
		"洋葱内由 Engine 兜成 500 + 记录，外包那侧逃到调用方（无响应、无记录）"
	known := map[string]string{}
	for _, spec := range defaultReqs() {
		known[spec.name] = why
	}
	run := compareMatrix(t, "chi middleware.CleanPath", once(middleware.CleanPath), nil, known)

	// 申报了差异就要钉住差异的内容（见文件头的第三条纪律）。
	run.need(t, "GET /ok", `error.type="panic"`)
	run.needStatus(t, "GET /ok", http.StatusInternalServerError)
	run.outerAbsent(t, "GET /ok", `status="500"`)
	if run.outer["GET /ok"].panicked == "" {
		t.Errorf("外包那侧本该 panic 逃到调用方，实际观测：%s", run.outer["GET /ok"])
	}
}

// ---- 指标：promhttp ----

// counterSeries 把登记表里的指标摊成 `name{code=200,method=GET} += 1` 的行，只回报
// **相对上次调用有变化**的条目及其增量。增量而不是累计值：矩阵逐请求比对，累计值会把
// 上一条请求的结论带进这一条（`/err` 被算进 200 桶之后，后面每条 200 请求的累计值都差
// 1——那是同一件事的余波，不是新的不一致）。
type counterSeries struct {
	reg  *prometheus.Registry
	prev map[string]float64
}

func (c *counterSeries) drain() string {
	fams, err := c.reg.Gather()
	if err != nil {
		return "ERR(" + err.Error() + ")"
	}
	now := map[string]float64{}
	for _, fam := range fams {
		for _, m := range fam.GetMetric() {
			labels := make([]string, 0, 4)
			for _, lp := range m.GetLabel() {
				labels = append(labels, lp.GetName()+"="+lp.GetValue())
			}
			sort.Strings(labels)
			var v float64
			switch {
			case m.GetCounter() != nil:
				v = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				v = m.GetGauge().GetValue()
			}
			now[fam.GetName()+"{"+strings.Join(labels, ",")+"}"] = v
		}
	}

	out := make([]string, 0, len(now))
	for k, v := range now {
		if d := v - c.prev[k]; d != 0 {
			out = append(out, fmt.Sprintf("%s += %g", k, d))
		}
	}
	sort.Strings(out)
	c.prev = now
	return strings.Join(out, "; ")
}

// TestMatrixPromhttpInstrumentHandlerCounter：除了矩阵比对，还比它记下的**标签**。
//
// 正常路径两边一致（`code=200`）；错误路径是已知边界——计数器在 `next` 返回后才自增，
// 而框架的错误映射还在更外面，于是 `d.Status()` 是 0、`sanitizeCode(0)` 记成
// **`code=200`**（把 404 记成 200，不是记成 0——这一点实测才知道）；panic 那条更彻底：
// 中间件的收尾整段被跳过了，一条都没记。
func TestMatrixPromhttpInstrumentHandlerCounter(t *testing.T) {
	factory := func() mount {
		reg := prometheus.NewRegistry()
		counter := prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "interop_requests_total",
			Help: "对照矩阵用：按 code / method 计请求数。",
		}, []string{"code", "method"})
		reg.MustRegister(counter)
		series := &counterSeries{reg: reg, prev: map[string]float64{}}
		return mount{
			mw:   func(next http.Handler) http.Handler { return promhttp.InstrumentHandlerCounter(counter, next) },
			side: series.drain,
		}
	}
	known := map[string]string{
		"GET /err":   "计数器在 next 返回后自增，读到的是 0 → sanitizeCode(0) 把 404 记成 code=200",
		"GET /panic": "panic 打断了中间件的收尾，这条一条都没记；外包那侧记到 code=500",
	}
	run := compareMatrix(t, "promhttp.InstrumentHandlerCounter", factory, nil, known)
	run.need(t, "GET /ok", "code=200,method=get} += 1")
	run.need(t, "GET /err", "code=200,method=get} += 1")
}

// ---- CORS：rs/cors ----

func corsMW() func(http.Handler) http.Handler {
	return cors.New(cors.Options{
		AllowedOrigins:   []string{"https://example.com"},
		AllowedMethods:   []string{http.MethodGet, http.MethodPost},
		AllowCredentials: true,
	}).Handler
}

// TestMatrixRsCors：普通请求（含白名单外、无 Origin、返回 error 的路由）两个挂载点
// 逐项一致。
func TestMatrixRsCors(t *testing.T) {
	reqs := []reqSpec{
		{"GET /ok (allowed origin)", http.MethodGet, "/ok", map[string]string{"Origin": "https://example.com"}},
		{"GET /ok (evil origin)", http.MethodGet, "/ok", map[string]string{"Origin": "https://evil.com"}},
		{"GET /ok (no origin)", http.MethodGet, "/ok", nil},
		{"GET /err (allowed origin)", http.MethodGet, "/err", map[string]string{"Origin": "https://example.com"}},
	}
	run := compareMatrix(t, "rs/cors", once(corsMW()), reqs, nil)
	run.need(t, "GET /ok (allowed origin)", "Access-Control-Allow-Origin=https://example.com")
	run.need(t, "GET /ok (evil origin)", "Vary=Origin")
}

// TestMatrixRsCorsPreflight：**已知边界**——预检到不了洋葱内。
//
// `Use` 挂在路由注册里，而路由匹配先于中间件：只注册了 `GET /ok` 时 `OPTIONS /ok`
// 根本走不到中间件，ServeMux 直接 405（`Allow: GET, HEAD`）。这与纯 stdlib 下的表现
// 完全同形——不是适配层弄坏的，是「中间件在路由器之后」这件事本身。
func TestMatrixRsCorsPreflight(t *testing.T) {
	spec := reqSpec{"OPTIONS /ok (preflight)", http.MethodOptions, "/ok", map[string]string{
		"Origin":                        "https://example.com",
		"Access-Control-Request-Method": http.MethodGet,
	}}
	known := map[string]string{
		spec.name: "路由匹配先于中间件：洋葱内看不到未注册的 OPTIONS，ServeMux 直接 405",
	}
	run := compareMatrix(t, "rs/cors 预检", once(corsMW()), []reqSpec{spec}, known)
	run.need(t, spec.name, "405")
	run.needStatus(t, spec.name, http.StatusMethodNotAllowed)
	run.outerAbsent(t, spec.name, "405")
}

// TestMatrixRsCorsPreflightExplicitRoute：站点给的绕法——**给路由显式注册 OPTIONS**——
// 真的能让预检进洋葱：两个挂载点都拿到同一组 CORS 响应头。
//
// 差异落在框架那一面：外包的 CORS 在引擎之前就短路了，这条请求框架**不知情**（没有
// trace 头、没有访问记录）；洋葱内的那侧是一条普通请求。这正是站点表格里
// 「外包 ⚠️ 框架完全不知情」那一格的实测。
//
// 这条同时是「**部分短路**」那一格：CORS 对预检是**只写状态码、不写 body** 的短路
// （204、零字节，注册的那条路由只负责把请求送进洋葱，handler 根本没跑）。所以下面
// 除了状态码还钉住体积 `0`——短路回填要同时把状态码与字节数记回去，只补一个也算坏。
func TestMatrixRsCorsPreflightExplicitRoute(t *testing.T) {
	spec := reqSpec{"OPTIONS /ok (preflight, explicit route)", http.MethodOptions, "/ok", map[string]string{
		"Origin":                        "https://example.com",
		"Access-Control-Request-Method": http.MethodGet,
	}}
	known := map[string]string{
		spec.name: "外包的 CORS 在引擎之前短路，框架不知情（无 trace 头、无访问记录）；洋葱内是一条普通请求",
	}
	run := compareMatrix(t, "rs/cors 预检 + 显式 OPTIONS 路由", func() mount {
		return mount{mw: corsMW(), allowPreflight: true}
	}, []reqSpec{spec}, known)

	run.need(t, spec.name, "Access-Control-Allow-Origin=https://example.com")
	run.need(t, spec.name, `http.request.method="OPTIONS"`)
	run.need(t, spec.name, `status="204"`)
	run.need(t, spec.name, `http.response.body.size="0"`)
	run.needStatus(t, spec.name, http.StatusNoContent)
	run.outerAbsent(t, spec.name, "X-Trace-Id")
}

// ---- 鉴权：golang-jwt/jwt/v5 ----

type subjectKey struct{}

// jwtMW 是应用里最常见的那种写法：校验 Bearer token，成功就把 subject 塞进
// **request context**，交给下游 handler 读。
func jwtMW(secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			tok, err := jwt.Parse(raw, func(*jwt.Token) (any, error) { return secret, nil },
				jwt.WithValidMethods([]string{"HS256"}))
			if err != nil || !tok.Valid {
				w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			sub, _ := tok.Claims.GetSubject()
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), subjectKey{}, sub)))
		})
	}
}

// TestMatrixJWT 压三条：**短路**（无 token → 401，且要记回框架的采集层）、**换
// request**（有 token → handler 读得到 subject）、以及方法不匹配。
//
// 短路那两条的差异全在框架那一面：外包的 401 写在引擎外面，框架不知情（没有 trace 头、
// 没有访问记录）；洋葱内那侧记回采集层。站点表格里「中间件短路写响应」那一行就是它。
func TestMatrixJWT(t *testing.T) {
	secret := []byte("interop-test-secret")
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{Subject: "u-42"})
	signed, err := token.SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}

	shortCircuit := "短路：401 由中间件写出，外包时框架完全不知情（无 trace 头、无访问记录）；洋葱内记回采集层"
	run := compareMatrix(t, "golang-jwt/jwt/v5", func() mount {
		return mount{
			mw: jwtMW(secret),
			probeRoutes: map[string]web.Handler{"/me": func(c *web.Ctx) error {
				sub, _ := c.Request().Context().Value(subjectKey{}).(string)
				return c.Text(http.StatusOK, "sub:"+sub)
			}},
		}
	}, []reqSpec{
		{"GET /me (valid token)", http.MethodGet, "/me", map[string]string{"Authorization": "Bearer " + signed}},
		{"GET /me (no token)", http.MethodGet, "/me", nil},
		{"GET /me (garbage token)", http.MethodGet, "/me", map[string]string{"Authorization": "Bearer nope.nope.nope"}},
		{"POST /me (valid token)", http.MethodPost, "/me", map[string]string{"Authorization": "Bearer " + signed}},
	}, map[string]string{
		"GET /me (no token)":      shortCircuit,
		"GET /me (garbage token)": shortCircuit,
	})

	// 换过的 request 真的传到底了（subject 进了 body），两侧都得有。
	run.need(t, "GET /me (valid token)", "sub:u-42")
	// 短路那条：401 是中间件写的，而且它**进了框架的记录**——外包那侧没有。
	run.need(t, "GET /me (no token)", `status="401"`)
	run.needStatus(t, "GET /me (no token)", http.StatusUnauthorized)
	run.needStatus(t, "GET /me (garbage token)", http.StatusUnauthorized)
	run.outerAbsent(t, "GET /me (no token)", "X-Trace-Id")
	// 记录里的方法必须是**这一条请求的**方法（这一行是唯一的非 GET 请求）——方法恒定
	// 记成 GET 的写法在别的行上看不出来，因为两侧一样错。
	run.need(t, "POST /me (valid token)", `http.request.method="POST"`)
}

// ---- 限流：httprate ----

// TestMatrixHttprate：限流器是**有状态**的，两个挂载点必须各用一份实例，否则先跑的一侧
// 会把额度吃掉、矩阵测出来的是「状态串台」而不是适配面。
//
// 判据：前两次 200，第三次 429（含 `Retry-After`），额度耗尽后连 `/err` 也是 429。
// 用 `LimitBy(..., Key("*"))` 而不是 `Limit(...)`——后者在 v0.16.0 已标 Deprecated
// （限流键从 option 变成了显式参数）。
func TestMatrixHttprate(t *testing.T) {
	exhausted := "限流器短路：429 由中间件写出，外包时框架完全不知情；洋葱内记回采集层"
	run := compareMatrix(t, "httprate.LimitBy(2, 1s)", func() mount {
		return mount{mw: httprate.LimitBy(2, time.Second, httprate.Key("*"))}
	}, []reqSpec{
		{"GET /ok #1", http.MethodGet, "/ok", nil},
		{"GET /ok #2", http.MethodGet, "/ok", nil},
		{"GET /ok #3", http.MethodGet, "/ok", nil},
		{"GET /err #1 (exhausted)", http.MethodGet, "/err", nil},
	}, map[string]string{
		"GET /ok #3":              exhausted,
		"GET /err #1 (exhausted)": exhausted,
	})

	run.need(t, "GET /ok #3", "429")
	run.need(t, "GET /ok #3", "Retry-After=")
	run.needStatus(t, "GET /ok #3", http.StatusTooManyRequests)
	run.need(t, "GET /err #1 (exhausted)", `status="429"`)
}

// ---- 边界：到不了洋葱的请求 ----

// sawHeader 是边界用例用的极简中间件：只留一个「我看过这条请求」的标记。它对所有请求
// 都生效，用来区分「中间件参与了」与「中间件没参与」。
func sawHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Interop-Saw", "yes")
		next.ServeHTTP(w, r)
	})
}

// TestMatrixUnmatchedRoutesBypassOnion：**已知边界**，与具体中间件无关——`Use` 挂在
// 路由注册里，所以**没匹配到路由的请求根本不经洋葱**。
//
//   - 外包：中间件看得到（404 上也加得到头）
//   - 洋葱内：看不到（ServeMux 自己写 404，中间件没跑）
//
// 站点表格里「拦预检 / 路由之前」那一格（外包 ✅ / Adapt ❌）就是这个机制，预检只是它
// 的一个表现。尾斜杠（`/ok/` 没注册为模式）同理。
//
// 这条**不在** `defaultReqs()` 里：把它塞进每条矩阵会让每个中间件都多一条「其实与本
// 中间件无关」的申报。
func TestMatrixUnmatchedRoutesBypassOnion(t *testing.T) {
	for _, spec := range []reqSpec{
		{"GET /nope (no route)", http.MethodGet, "/nope", nil},
		{"GET /ok/ (trailing slash, no route)", http.MethodGet, "/ok/", nil},
	} {
		inSink, outSink := &recordSink{}, &recordSink{}
		in := observe(routes(web.Adapt(sawHeader), inSink, nil, false).Handler(), spec, inSink)
		out := observe(sawHeader(routes(nil, outSink, nil, false).Handler()), spec, outSink)

		// 响应本身两边一样：都是 ServeMux 自己写的 404。
		if in.status != http.StatusNotFound || out.status != http.StatusNotFound {
			t.Errorf("%s：应当两边都是 ServeMux 的 404\n  经 Adapt: %s\n  外包    : %s", spec.name, in, out)
		}
		if in.body != out.body {
			t.Errorf("%s：两侧的 404 文本就不一样了\n  经 Adapt: %s\n  外包    : %s", spec.name, in, out)
		}
		// 差的是中间件有没有参与：外包看得到，洋葱内看不到。
		if !strings.Contains(out.headers, "X-Interop-Saw=yes") {
			t.Errorf("%s：外包的中间件没看见未匹配的请求（%s）——这条边界的前提不成立了", spec.name, out)
		}
		if strings.Contains(in.headers, "X-Interop-Saw=yes") {
			t.Errorf("%s：洋葱内的中间件竟然看见了未匹配的请求（%s）——路由先于中间件这条前提变了", spec.name, in)
		}
		// 框架侧：两条都进了引擎并记了访问记录，路由模板是**空值**（不是缺席）。
		for side, got := range map[string]obs{"经 Adapt": in, "外包": out} {
			if !strings.Contains(got.rec, `http.request.method="GET"`) {
				t.Errorf("%s / %s：框架该记下这条请求（%s）", spec.name, side, got.rec)
			}
			if !strings.Contains(got.rec, `http.route=""`) {
				t.Errorf("%s / %s：未匹配的路由模板该是**空值**（%s）", spec.name, side, got.rec)
			}
		}
	}
}

// TestMatrixUncleanedPathRedirectsBeforeOnion：路径需要归一化时（`/users//42`、`//users/42`、
// `/users/./42`），**ServeMux 在调用路由 handler 之前就自己 307 到干净路径**——它与「未匹
// 配路由」（404）是同一条边界（路由先于中间件）：洋葱内的中间件看不到这条请求，外包的看
// 得到。两处实测细节值得钉住：
//
//   - 状态码是 **307**（`Location` 指向干净路径），响应体是 ServeMux 自带的
//     `<a href="…">` 那一段；
//   - 记录里的 **`http.route` 非空**（`/users/{id}`）——ServeMux 在重定向前就把 pattern
//     写上了。所以「记录里 `http.route` 非空」**不等于**「这条请求真的被那个 handler 处
//     理过」，读记录的人别把它当处理过的证据。
func TestMatrixUncleanedPathRedirectsBeforeOnion(t *testing.T) {
	for _, spec := range []reqSpec{
		{"GET /users//42 (double slash)", http.MethodGet, "/users//42", nil},
		{"GET //users/42 (leading double slash)", http.MethodGet, "//users/42", nil},
		{"GET /users/./42 (dot segment)", http.MethodGet, "/users/./42", nil},
	} {
		inSink, outSink := &recordSink{}, &recordSink{}
		in := observe(routes(web.Adapt(sawHeader), inSink, nil, false).Handler(), spec, inSink)
		out := observe(sawHeader(routes(nil, outSink, nil, false).Handler()), spec, outSink)

		// 响应本身两边一样：ServeMux 自己写的 307 与 Location。
		for side, got := range map[string]obs{"经 Adapt": in, "外包": out} {
			if got.status != http.StatusTemporaryRedirect {
				t.Errorf("%s / %s：应当是 ServeMux 自己的 307（%s）", spec.name, side, got)
			}
			if !strings.Contains(got.headers, "Location=/users/42") {
				t.Errorf("%s / %s：该 307 到干净路径（%s）", spec.name, side, got)
			}
		}
		if in.body != out.body {
			t.Errorf("%s：两侧的重定向文本就不一样了\n  经 Adapt: %s\n  外包    : %s", spec.name, in, out)
		}
		// 差的是中间件有没有参与。
		if !strings.Contains(out.headers, "X-Interop-Saw=yes") {
			t.Errorf("%s：外包的中间件没看见这条请求（%s）——这条边界的前提不成立了", spec.name, out)
		}
		if strings.Contains(in.headers, "X-Interop-Saw=yes") {
			t.Errorf("%s：洋葱内的中间件竟然看见了——重定向不再是「在洋葱之前」了（%s）", spec.name, in)
		}
		// 记录面：非空的 pattern，但 handler 没跑过。
		if !strings.Contains(in.rec, `http.route="/users/{id}"`) {
			t.Errorf("%s：重定向前 ServeMux 已经把 pattern 写上了，记录里该是非空路由（%s）", spec.name, in.rec)
		}
	}
}
