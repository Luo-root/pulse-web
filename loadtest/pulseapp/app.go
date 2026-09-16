// Package pulseapp 是压测里的 pulse-web 服务端。
//
// 它与 ginapp 是**刻意写成的对拍**：同样的路由、同样的响应体、同样的分节顺序，
// 连函数顺序都对齐。看其中一个时请并排看另一个——两个文件之间的 diff 就是
// 「同一件事两边怎么写」的答案。
package pulseapp

import (
	"io"
	"net/http"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse-web/loadtest/fastsink"
	"github.com/Luo-root/pulse/observability"
)

// Mode 是档位。
//
// **只有 `bare` 与 `obs` 是对拍档**，其余是 pulse-web 侧的诊断档：它们回答
// 「钱花在哪」，不回答「谁快」。
type Mode string

const (
	// ModeBare 只留路由、上下文与响应写出，对拍 gin.New()。
	//
	// 已知不对称：Minimal() 仍保留 Engine 的 panic 兜底，gin.New() 没有
	// Recovery。这一档比的是「裸路由 + 上下文 + 写响应」这一层，不是
	// 「有没有兜底」——兜底是一次 defer，成本在噪声里。
	ModeBare Mode = "bare"

	// ModeObs 对拍 gin.Default()（Logger + Recovery）：默认装配
	// （Bootstrap + Trace + 访问日志）+ **默认出口形态**（`ConsoleSink`，
	// 给人读的列式单行；见 engine.go 的 newDefaultSink）。
	//
	// 出口目的地是空设备而不是 stdout：保留每请求的**格式化成本**，同时排除终端 /
	// 管道 I/O——写管道要付系统调用，父进程还得起读取协程，那笔账会把两侧都拖进对比。
	// 真实部署里它是 stdout（然后落到采集器），那时磁盘/采集成本两边都要付。
	ModeObs Mode = "obs"

	// ModeObsLine 诊断：把出口换成 observability.NewLineSink（行式缓冲出口）。
	// **它不是默认出口**——上一版对比误把当时的默认形态当成了唯一形态，这一档用来量出
	// 「换出口」本身值多少。
	ModeObsLine Mode = "obs-line"

	// ModeObsAsync 诊断：上游推荐的异步组合 AsyncSink(LineSink)——请求路径只做
	// Attrs 深拷 + 入队，格式化与写出挪到后台协程。
	ModeObsAsync Mode = "obs-async"

	// ModeObsFast 诊断：换成 `loadtest/fastsink` 的**原型出口**（池化缓冲 +
	// 不用 fmt + 列式版式）。用来量「同一批字段，把渲染做便宜能便宜多少」。
	//
	// 注意它是**原型，不是框架代码**——框架要不要自带这样一个出口另说（见 #20）。
	ModeObsFast Mode = "obs-fast"

	// ModeObsNoLog 诊断：默认装配但关掉访问日志（`WithoutAccessLog()`），
	// 用来把观测开销拆成「Trace + 记录框架」与「访问日志 + 出口」两段。
	ModeObsNoLog Mode = "obs-nolog"
)

// discardConsole 是「默认出口形态 + 空目的地」：框架默认出口是
// `NewConsoleSink(os.Stdout)`（engine.go 的 newDefaultSink），这里给一个写空设备的
// ConsoleSink——同一条渲染路径，只把目的地换掉。
//
// 跟着默认走、不要写死具体出口类型：本轮就是因为写死了当时的默认（`SlogSink`），
// 默认换成 ConsoleSink 之后这一档量到的已经不是「默认开箱配置」了（#68）。
func discardConsole() observability.Sink {
	return web.NewConsoleSink(io.Discard)
}

// New 按档位构造压测用 handler。
func New(mode Mode) http.Handler {
	var app *web.Engine
	switch mode {
	case ModeBare:
		app = web.New(web.Minimal())
	case ModeObs:
		app = web.New(web.WithSink(discardConsole()))
	case ModeObsLine:
		app = web.New(web.WithSink(observability.NewLineSink(io.Discard)))
	case ModeObsAsync:
		app = web.New(web.WithSink(observability.NewAsyncSink(observability.NewLineSink(io.Discard))))
	case ModeObsFast:
		app = web.New(web.WithSink(fastsink.New(io.Discard)))
	case ModeObsNoLog:
		app = web.New(web.WithSink(discardConsole()), web.WithoutAccessLog())
	default:
		panic("pulseapp: 未知档位 " + string(mode))
	}
	registerRoutes(app)
	return app
}

// NewWithSink 用指定出口构造「默认装配」应用：探针用（数每请求写几条、
// 看记录长什么样、或换一个自写的出口）。
func NewWithSink(sink observability.Sink) http.Handler {
	app := web.New(web.WithSink(sink))
	registerRoutes(app)
	return app
}

// registerRoutes 注册路由——必须与 ginapp.registerRoutes 逐条一致
// （只有路径参数的写法不同：stdlib 是 {id}，gin 是 :id）。
func registerRoutes(app *web.Engine) {
	app.GET("/healthz", healthz)
	app.GET("/users/{id}", getUser)
}

func healthz(c *web.Ctx) error {
	return c.Text(http.StatusOK, "ok")
}

// user 是响应体的形状，两侧共用同一份字段与 JSON 标签。
type user struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func getUser(c *web.Ctx) error {
	id := c.Path("id")
	// 刻意「不逃逸成服务端查询」：响应体只由路径参数拼出，
	// 两边都是纯计算 + 一次 JSON 编码，差别只在框架层。
	return c.JSON(http.StatusOK, user{ID: id, Name: "user-" + id})
}
