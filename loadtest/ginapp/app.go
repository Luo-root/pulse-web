// Package ginapp 是压测里的 gin 服务端。
//
// 它与 pulseapp 是**刻意写成的对拍**：同样的路由、同样的响应体、同样的两档
// 中间件面，连函数顺序都对齐。看其中一个时请并排看另一个——两个文件之间的
// diff 就是「同一件事两边怎么写」的答案。
package ginapp

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Mode 是对拍档位。
type Mode string

const (
	// ModeBare 只留路由、上下文与响应写出，对拍 pulse-web 的 Minimal()。
	//
	// 已知不对称：gin.New() 不带 Recovery，而 pulse-web 的 Minimal() 仍保留
	// Engine 的 panic 兜底。这一档比的是「裸路由 + 上下文 + 写响应」这一层，
	// 不是「有没有兜底」——兜底是一次 defer，成本在噪声里。
	ModeBare Mode = "bare"

	// ModeObs 打开 gin **自带**的默认中间件（Logger + Recovery），
	// 对拍 pulse-web 的默认装配（Bootstrap + Trace + 访问日志）。
	//
	// Logger 的出口指向空设备：保留每请求的**格式化成本**，排除磁盘 I/O。
	ModeObs Mode = "obs"
)

// LogWriter 是 obs 档 Logger 的出口。默认空设备（保留格式化成本、排除磁盘 I/O）；
// 探针可以换成计数 writer 来数「每请求几行」。真实部署里它是 stdout。
var LogWriter io.Writer = io.Discard

// New 按档位构造压测用 handler。
func New(mode Mode) http.Handler {
	// 关掉 debug 模式的启动横幅与告警：它们会往 stdout 写字，干扰压测输出。
	gin.SetMode(gin.ReleaseMode)

	var engine *gin.Engine
	switch mode {
	case ModeBare:
		engine = gin.New()
	case ModeObs:
		// 必须在 gin.Default() **之前**改：Logger 是在创建时抓 DefaultWriter 的，
		// 建完再改只影响之后创建的中间件。
		gin.DefaultWriter = LogWriter
		engine = gin.Default()
	default:
		panic("ginapp: 未知档位 " + string(mode))
	}
	registerRoutes(engine)
	return engine
}

// registerRoutes 注册路由——必须与 pulseapp.registerRoutes 逐条一致
// （只有路径参数的写法不同：gin 是 :id，stdlib 是 {id}）。
func registerRoutes(engine *gin.Engine) {
	engine.GET("/healthz", healthz)
	engine.GET("/users/:id", getUser)
}

func healthz(c *gin.Context) {
	c.String(http.StatusOK, "ok")
}

// user 是响应体的形状，两侧共用同一份字段与 JSON 标签。
type user struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func getUser(c *gin.Context) {
	id := c.Param("id")
	// 刻意「不逃逸成服务端查询」：响应体只由路径参数拼出，
	// 两边都是纯计算 + 一次 JSON 编码，差别只在框架层。
	c.JSON(http.StatusOK, user{ID: id, Name: "user-" + id})
}
