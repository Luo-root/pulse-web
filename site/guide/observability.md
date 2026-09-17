# 观测

这套观测的重点不是「能接 Sink」，而是**默认就接好了**：`web.New()` 起服务之后，每请求一行访问日志、每请求一个 32hex TraceID、请求作用域按时回收——一行配置都不用写。

## 默认出口：给人读的控制台列式

`web.New()` 的默认出口是 `web.NewConsoleSink(os.Stdout)`：把 `Record` 渲染成一行列式文本，列宽固定，不缓冲（写完即落）。

```text
PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378
```

尾段的 `route=` / `size=` / `host=` / 错误 / `trace=` 是各自独立的字段，有才出现。状态列在目的地是终端时按区间上色，重定向到文件或管道自动关（`NewConsoleSink(w, WithColor(true))` 可强制）。

## 换出口：按「谁来读这份输出」选

`WithSink` 只换出口，**装配里的其它东西一个都不变**。

```go
web.WithSink(web.NewConsoleSink(os.Stdout))                     // 给人读——默认：不缓冲，写完即落
web.WithSink(observability.SlogSink{Logger: slog.Default()})    // 给机器读——接宿主 logger / JSON / 采集器（默认写 stderr）
web.WithSink(observability.NewAsyncSink(inner))                 // 要吞吐——把写出口移出请求路径
web.WithSink(observability.MultiSink{a, b})                     // 同时送多个目的地
```

| 出口 | 什么时候用 | 你要知道的事 |
|---|---|---|
| `web.ConsoleSink`（默认） | 想在终端 / `kubectl logs` 里直接看懂 | 不缓冲；写失败不抛，但 `Err()` 报出首次失败 |
| `observability.SlogSink` | 已经有一套日志管道、要 JSON、喂采集器 | 默认写 **stderr**；走宿主 logger 的格式与级别 |
| `observability.LineSink` | 要上游默认版式的行式输出 + 32 KiB 缓冲 | 引擎在优雅关闭时会替你 `Flush` |
| `observability.NewAsyncSink(inner)` | 出口是瓶颈（文件 / 网络导出器） | 队列有界、生命周期归你——见下 |
| `observability.MultiSink` | 要同时给人看又送采集器 | 扇出，nil 成员跳过 |

::: warning 别把 `SlogSink` 当成默认
它是**给机器读**的那个出口。默认出口是 `ConsoleSink`——`0.585` 与 `585.1µs` 是同一条耗时，开机第一眼要的是后者。
:::

## 出口是吞吐所在：什么时候包 `AsyncSink`

观测的钱几乎全花在「把记录送出门」这一步，而 kernel 的事件派发是**全同步**的（`Emit` / `EmitLocal` / `Waterfall`，`Parallel` 也等完成）——**出口有多慢，请求路径就有多慢**。

`NewAsyncSink(inner)` 把慢出口从调用方 goroutine 上摘掉：`Write` 只做 `Attrs` 深拷 + 入队就返回，单后台协程按 FIFO 调 `inner.Write`。

```go
app := web.New(
	web.WithHostID("orders-api"),
	web.WithSink(observability.NewAsyncSink(
		observability.SlogSink{Logger: slog.Default()},
	)),
)
```

代价是**所有权归你**，两条都要处理：

1. **队列有界**（默认 1024）。满时默认**回压**——不丢记录，把背压留给生产者；`observability.DropOnFull()` 改为丢**新**记录并计入 `Dropped()`。
2. **生命周期归创建者**。异步出口改变了「树销毁后 Sink 零残留」的达成方式：关闭前必须 `Flush(ctx)` 或 `Close(ctx)`，否则队列里的记录随进程一起消失。框架的关闭时序会替你 `Flush` 一次已产生的记录，但 `Close` 不代做——把出口的所有权交给框架，`Detach` 出来的后台任务就会被静默截断。

::: tip 数字在哪
「换出口差多少」的实测（真实负载对比、微基准、分配计数）见[性能](/performance)——三张表的公开版、复现命令与口径说明都在那一页；口径与推导的完整版在设计文档的「实测数据」一节。
:::

## 每个请求都有什么

无论用哪个出口，`New()` 的装配下每个请求都会：

- 生成或沿用 32hex 的 **TraceID**（`c.TraceID()`；可采纳入站的 W3C `traceparent` / B3，见 `WithTrustedTraceHeader`）
- 写一条 **`http.request` 记录**：状态码、耗时、路由模式、响应体积、host、trace
- 开一个**请求作用域**：handler 返回时立即 LIFO 回收——早于引擎做错误映射与任何后续写出
- 兜 panic：handler 里的 panic 变成 500，panic 值与栈留在进程内（挂在记录上，绝不发给客户端）

入站身份优先按 W3C `traceparent` 解析（字段级严格校验，任何一处不合法就整条忽略、起新 trace）。B3 的 `X-B3-TraceId` 是**历史兼容路径**：它只有 trace-id、没有 span-id，所以走 B3 的请求在链路里是 **root span**（没有 parent）——它不是 W3C 的等价物。

## 接进追踪体系：span 出口

框架自己**不引任何追踪 SDK**（主模块零第三方依赖是红线），它产出结构化数据，把数据变成真 span 的是宿主——官方适配在独立 module 里：

```go
import (
	"go.opentelemetry.io/otel/sdk/trace"
	otelweb "github.com/Luo-root/pulse-web/otel"
)

tp := trace.NewTracerProvider(trace.WithBatcher(exporter))
app := web.New(web.WithSpanHook(otelweb.New(tp)))
```

一次请求的两头由 `SpanHook` 的两个方法接住：

| 时刻 | 框架给什么 | 适配件做什么 |
|---|---|---|
| `Begin`（路由前） | `SpanInfo`：入站解析结果（`TraceID` / `ParentID` / `TraceState` / `Sampled` / `Random`）加方法与实际路径 | 建 server span 并注入请求 context（下游 `otelhttp` / `otelgrpc` / `otelsql` 自动接上），再用 `SpanRef` 把身份回给框架 |
| handler 期间 | —— | span 正在记录：`c.SpanID()` 拿得到 id，往下传的 `context.Context` 带着它 |
| `End`（响应写完后） | `Span`：路由模板、**映射后**的状态码、与访问日志同一份属性、起止时间、原始错误 | `trace.SpanFromContext(ctx)` 取回同一个 span，补名称 / 属性 / 状态后结束 |

口径按 semconv：span 名用 `{method} {http.route}`（路由拿不到时退化为 `{method}`，**不**退回 URI 路径）；未知方法在名字里退化为 `HTTP`、属性侧写 `_OTHER` 并附 `http.request.method_original`；5xx → `Error`、4xx/2xx → 保持 unset；`http.response.status_code` 用映射后的状态码。

访问日志里那个方法**不归一**（`purge` 就写 `purge`）——日志给人读，span 给 APM，两边属性名相同、取值口径这一处不同是有意的。

**有意不记的三个 Recommended 属性**：`url.scheme` / `server.address` / `network.protocol.version` 框架不产出——它们记的是「本服务的监听信息」而不是请求事实（反代后面 `url.scheme` 拿到的是内网值，照记等于写错），要它们请在**边缘那一层**记。这是一条**声明过的**对 semconv 的偏差，不是漏记。

### 两个响应头，各管各的

装了 span 出口后，同一个响应上会多出一个链路相关的头——它和 `X-Trace-Id` **不是一回事**：

| 头 | 内容 | 什么时候有 |
|---|---|---|
| `X-Trace-Id` | 32hex trace-id，**不含 span** | 一直有（`Minimal()` 关掉 trace 时没有） |
| `Server-Timing: trace;desc=…` | `00-<trace-id>-<本请求 span-id>-<flags>`，含 span | 只有拿到 span 身份才有 |

响应侧**不写 `traceparent`**：W3C 没有给响应定义这个头。出站请求侧的 `traceparent` 由宿主下游客户端的插桩注入，parent-id 就是本请求的 span-id。

**在多层代理后面时，`Server-Timing` 可能被中间设备改写或剥掉**——WAF、CDN、老网关对不认得的响应头并不都原样透传；`X-Trace-Id` 是自带头，被剥掉的概率低得多。排查「链路信息怎么没了」时按这个顺序看：访问日志里的 `trace=` 与 `span.id` → `X-Trace-Id` → 最后才怀疑 `Server-Timing`。

### 后台任务：link，不是父子

`c.Detach()` 出来的任务活过请求，把它做成请求 span 的子节点会让父 span 的时长语义失真。推荐做法是**显式 link**：把 `c.TraceID()` / `c.SpanID()` 带进 `Detached` 值（或自己的任务表），在追踪体系里建一条 link。框架不替后台任务建 span。

## 打自己的点：`c.Observe`

```go
app.POST("/orders", func(c *web.Ctx) error {
	var in Order
	if err := c.Bind(&in); err != nil {
		return err
	}
	c.Observe("order.paid", func(a *observability.Attrs) {
		a.Set("order_id", in.ID)
		a.Set("amount_cents", in.AmountCents)
	})
	return c.JSON(http.StatusCreated, in)
})
```

你自己的事件与框架的进**同一个出口**，所以换出口时它们一起走。

## 开关与诊断

```go
app := web.New(
	web.WithHostID("orders-api"),   // 访问日志里的 host= 字段
	web.WithoutAccessLog(),         // 已经另有日志系统时：只关访问日志
	web.WithCollector(),            // 需要请求级 fiber 可见性时，把 kernel collector 挂到请求作用域
)

app.Debug("/debug/assembly")        // 装配状态的 JSON 视图：快照、fiber、插件
```

`WithoutAccessLog()` 只关访问日志——Trace 与 panic 兜底（500 响应）不受影响。注意**关掉之后 panic 不再写观测记录**：访问级记录是它唯一的出口。

::: warning `WithRoot` 的生命周期
`web.WithRoot(root)` 接入既有的 kernel 树；`Run` / `Serve` 返回时会**级联销毁**它，之后不要再使用。
:::

## 记录的边界

`Record` 的值域只有标量（`~string | ~int64 | ~float64 | ~bool`），**没有 `map[string]any` 逃生舱**——所以请求体、请求头、query 在类型上就进不了日志，不靠「记得别写」。

`Record` 里的字段一个不少、缺的只是渲染层：要机器可读就换 `SlogSink`，不需要为了拿字段换一套埋点。

## 持久化不归框架

默认出口写 stdout，它**不是持久化层**。落盘、轮转、保留策略属于你的平台——容器运行时、systemd / journald，或你自己 `WithSink` 给的 writer。部署侧的配置与「丢多少由哪一层决定」见[落地与运行时契约](/guide/ops-contracts)。
