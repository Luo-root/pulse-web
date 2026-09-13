// Package pulseapp 是压测里的 pulse-web 服务端。
//
// 它与 ginapp 是**刻意写成的对拍**：同样的路由、同样的响应体、同样的两档
// 中间件面，连函数顺序都对齐。看其中一个时请并排看另一个——两个文件之间的
// diff 就是「同一件事两边怎么写」的答案。
package pulseapp

import (
	"io"
	"net/http"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse/observability"
)

// Mode 是对拍档位。
type Mode string

const (
	// ModeBare 只留路由、上下文与响应写出，对拍 gin.New()。
	//
	// 已知不对称：Minimal() 仍保留 Engine 的 panic 兜底，gin.New() 没有
	// Recovery。这一档比的是「裸路由 + 上下文 + 写响应」这一层，不是
	// 「有没有兜底」——兜底是一次 defer，成本在噪声里。
	ModeBare Mode = "bare"

	// ModeObs 打开框架**自带**的观测装配（Bootstrap + Trace + 访问日志），
	// 对拍 gin.Default()（Logger + Recovery）。
	//
	// 出口指向空设备：保留每请求的**格式化成本**，排除磁盘 I/O——否则这一档
	// 比的是磁盘带宽，不是框架开销。
	ModeObs Mode = "obs"

	// ModeObsNoLog 是**诊断档，不是对拍档**：观测装配只留 Trace，关掉访问日志。
	//
	// 用来把观测档那截开销拆成「Trace + 记录框架」与「访问日志 + 出口」两段。
	// gin 侧没有对应档（它的 bare 就是「不开日志」），所以这一档只有 pulse-web
	// 一侧——它回答的是「钱花在哪」，不是「谁快」。
	ModeObsNoLog Mode = "obs-nolog"
)

// New 按档位构造压测用 handler。
func New(mode Mode) http.Handler {
	var app *web.Engine
	switch mode {
	case ModeBare:
		app = web.New(web.Minimal())
	case ModeObs:
		app = web.New(web.WithSink(observability.NewLineSink(io.Discard)))
	case ModeObsNoLog:
		app = web.New(
			web.WithSink(observability.NewLineSink(io.Discard)),
			web.WithoutAccessLog(),
		)
	default:
		panic("pulseapp: 未知档位 " + string(mode))
	}
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
