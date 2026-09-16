# pulse-web/otel

把 pulse-web 的请求级 span 数据接进 OpenTelemetry（otel-go）。

主模块 `github.com/Luo-root/pulse-web` **不依赖任何追踪 SDK**（零第三方依赖是红线）：它按 W3C Trace Context 与 OTel HTTP semantic conventions 产出结构化数据（`web.SpanInfo` / `web.SpanRef` / `web.Span`）。本 module 是官方适配——把那份数据翻成 SDK 类型，并负责 span 的生命周期。

## 用法

```go
import (
	"go.opentelemetry.io/otel/sdk/trace"
	otelweb "github.com/Luo-root/pulse-web/otel"
	web "github.com/Luo-root/pulse-web"
)

tp := trace.NewTracerProvider(trace.WithBatcher(exporter))
defer func() { _ = tp.Shutdown(ctx) }()

app := web.New(web.WithSpanHook(otelweb.New(tp)))
```

一次请求：

1. `Begin` 建 server span（父 = 入站 `traceparent` 解析出的 span，没有就是 root），并把它注入请求 context；
2. handler 期间 `c.SpanID()` 可读；往下传的 `context.Context` 让 `otelhttp` / `otelgrpc` / `otelsql` 自动接上这条链路；
3. `End` 补名称（`{method} {http.route}`）、属性与状态，结束 span。

## 口径（按 semconv，不按喜好）

| 项 | 取值 |
|---|---|
| span 名 | `{method} {http.route}`；无路由时退化为 `{method}`（**不得**用 URI 路径） |
| span kind | `Server` |
| 状态 | 5xx → `Error`（描述留空）；4xx / 3xx / 2xx → 保持 unset |
| `http.response.status_code` | 框架**映射后**的状态码（与访问日志同源） |
| `error.type` | 5xx 时：框架给了就用（`panic` / `http_5xx` / `internal`），没给则写状态码字符串；4xx 及以下**不带**（semconv：成功完成的请求不应设该属性） |
| 属性 | 与访问日志同一份来源（`http.request.method` / `http.route` / `url.path` / `http.response.body.size` / `client.address`） |
| 异常 | 不调 `span.RecordError`——错误原文留在进程内的观测记录里，不随 span 出到追踪后端 |

采样完全归宿主的 `TracerProvider`：入站 `sampled=0` 的请求在 `ParentBased` 采样器下不会导出 span，这是 OTel 的既定语义，不是缺陷。

## 开销（实测）

同一条请求路径（`-benchtime=20000x -count=3`，本机 i9-14900HX；口径见主仓 `bench/`）：

| 档 | ns/op | B/op | allocs/op |
|---|---|---|---|
| 不装 span 出口 | ~780 | 1222 | 14 |
| 装 span 出口（本 module + 真 SDK + 内存 exporter） | ~2450–2850 | 5255 | 38 |

差值是 SDK 的 span 机制主导：「框架属性 → OTel 属性」这一步单独量只有 **6 allocs / 159 ns**（6 属性记录），其余约 18 次分配来自 SDK 建 span 与导出。所以默认路径的零分配不受影响——不装 hook 时请求路径逐字节不变，主模块的分配预算门禁卡的就是那一档。

## 边界

- **不做** metrics / logs 的 OTel 化（本 module 只有 trace 面）。
- **不**替后台任务建 span：`c.Detach()` 出来的任务请用 `c.TraceID()` / `c.SpanID()` 在追踪体系里建一条显式 link。
- 响应侧不写 `traceparent`（W3C 没有这个响应头）；标准形态是框架写的 `Server-Timing: trace;desc=…`（与既有的 `X-Trace-Id` 各管各的，见主仓 README 的观测一节）。

## 测试

```bash
cd otel && go test ./...
```

用例覆盖：span 形状与属性、状态语义（5xx / 4xx / 2xx 与 `error.type` 的两个分支）、下游注入（注入出的 `traceparent` 的 parent-id 必须是本请求 span 的 id）、日志关联（访问记录里的 `span.id` 与导出 span 一致），以及**与官方 propagator 的差分对照**（同一批 `traceparent` 逐条比对「采纳与否」与 trace/parent id）。
