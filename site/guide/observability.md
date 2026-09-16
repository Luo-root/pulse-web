# 观测

这套观测的卖点不是「能接 Sink」，而是**默认就接好了**：`web.New()` 起服务之后，每请求一行访问日志、每请求一个 32hex TraceID、请求作用域按时回收——一行配置都不用写。

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
| `observability.LineSink` | 要上游缺省版式的行式输出 + 32 KiB 缓冲 | 引擎在优雅关闭时会替你 `Flush` |
| `observability.NewAsyncSink(inner)` | 出口是瓶颈（文件 / 网络导出器） | 队列有界、生命周期归你——见下 |
| `observability.MultiSink` | 要同时给人看又送采集器 | 扇出，nil 成员跳过 |

::: warning 别把 `SlogSink` 当成默认
它是**给机器读**的那个出口。默认出口是 `ConsoleSink`——`0.585` 与 `585.1µs` 是同一条耗时，开机第一眼要的是后者。要切回结构化出口用 `WithSink`，不需要改别处。
:::

## 出口是吞吐所在：什么时候包 `AsyncSink`

观测的钱几乎全花在「把记录送出门」这一步，而 kernel 的事件派发是**全同步**的（`Emit` / `EmitLocal` / `Waterfall`，`Parallel` 也等完成）——**出口有多慢，请求路径就有多慢**。

`NewAsyncSink(inner)` 把手慢的出口从调用方 goroutine 上摘掉：`Write` 只做 `Attrs` 深拷 + 入队就返回，单后台协程按 FIFO 调 `inner.Write`。

```go
app := web.New(
	web.WithHostID("orders-api"),
	web.WithSink(observability.NewAsyncSink(
		observability.SlogSink{Logger: slog.Default()},
	)),
)
```

代价是**所有权归你**，两条都要处理：

1. **队列有界**（缺省 1024）。满时缺省**回压**——不丢记录，把背压留给生产者；`observability.DropOnFull()` 改为丢**新**记录并计入 `Dropped()`。
2. **生命周期归创建者**。异步出口改变了「树销毁后 Sink 零残留」的达成方式：关闭前必须 `Flush(ctx)` 或 `Close(ctx)`，否则队列里的记录随进程一起消失。框架的关闭时序会替你 `Flush` 一次已产生的记录，但 `Close` 不代做——把出口的所有权交给框架，`Detach` 出来的后台任务就会被静默截断。

::: tip 数字在哪
「换出口差多少」的实测（真实负载对比、微基准、分配计数）在设计文档的「性能」一节——站点性能页随后落地，先看文档是准的。
:::

## 每个请求都有什么

无论用哪个出口，`New()` 的装配下每个请求都会：

- 生成或沿用 32hex 的 **TraceID**（`c.TraceID()`；可采纳入站的 W3C `traceparent` / B3，见 `WithTrustedTraceHeader`）
- 写一条 **`http.request` 记录**：状态码、耗时、路由模式、响应体积、host、trace
- 开一个**请求作用域**：handler 返回时立即 LIFO 回收——早于引擎做错误映射与任何后续写出
- 兜 panic：handler 里的 panic 变成 500，panic 值与栈留在进程内（挂在记录上，绝不发给客户端）

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

默认出口写 stdout，它**不是持久化层**。落盘、轮转、保留策略属于你的平台——容器运行时、systemd / journald，或你自己 `WithSink` 给的 writer。部署侧的配置与「丢多少由哪一层决定」随后在站点的落地页展开（那部分内容是从 README 移出来的，设计文档的「日志落地」一节已有全文）。
