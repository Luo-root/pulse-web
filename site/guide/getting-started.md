# 快速开始

## 安装

```bash
go get github.com/Luo-root/pulse-web
```

需要 **Go 1.27+**（`go.mod` 写死 `go 1.27.0`；工具链缺失时会自动下载）。

核心模块只依赖标准库与两个 `pulse` 包。判据是**真正编译进去的东西**，不是 `go.mod` 里列了什么：

```bash
go list -deps . | grep -E '^[^/]+\.[^/]+/'
# github.com/Luo-root/pulse/kernel
# github.com/Luo-root/pulse/observability
# github.com/Luo-root/pulse-web
```

## 起一个服务

```go
package main

import (
	"net/http"

	"github.com/Luo-root/pulse-web"
)

func main() {
	app := web.New()

	app.GET("/users/{id}", func(c *web.Ctx) error {
		return c.JSON(http.StatusOK, web.H{"id": c.Path("id")})
	})

	app.Run(":8080")
}
```

```bash
go run .
curl -s localhost:8080/users/42
# {"id":"42"}
```

::: tip 路径参数没有第二份副本
`c.Path("id")` 取的就是 `net/http` 写进请求里的那一份——`{id}` 是标准库 ServeMux 的模式，框架不另存一份。
:::

## 你什么都没配，但观测已经在跑了

上面那次 `curl` 会在 stdout 留下这样一行：

```text
PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378
```

默认出口是 `web.ConsoleSink`（写 stdout）：一行一条请求，列宽固定，给人读。同一个 `trace=` 也拿得到——在 handler 里调 `c.TraceID()`：

```go
app.GET("/users/{id}", func(c *web.Ctx) error {
	log.Printf("trace=%s", c.TraceID()) // 与上面那行的 trace= 是同一个
	return c.JSON(http.StatusOK, web.H{"id": c.Path("id")})
})
```

行首的 `PULSE` 是上游缺省标识（`observability.DefaultLinePrefix`）：pulse 与 pulse-web 同根同源，同一个进程树里两个出口的行首一致，`grep PULSE` 一把捞出全部行。

## 接下来

- [观测怎么用](/guide/observability)——默认出口、四种出口的取舍、`AsyncSink` 的代价
- [设计文档](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md)——运行时契约、关闭时序、性能实测
- [README](https://github.com/Luo-root/pulse-web#readme)——一页纸的结论与入口
