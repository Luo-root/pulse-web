# loadtest — pulse-web 与 gin 的真实负载对比

验收标准第 8 条（「真实负载下与 gin 同级」）的落地工程。**口径先行，结论其次**：
这类对比最容易变成「谁的数字好看谁赢」，所以这里把口径写进代码，数字只是副产品。

## 跑

```console
cd loadtest
go run ./cmd/compare -probe -passes 4 -d 8s -warmup 2s   # 记录进文档的那次
go run ./cmd/compare                                     # 只跑对拍档
go run ./cmd/compare -quick                              # 自检用缩水口径，数字不要进文档
go test -bench . -benchmem ./bench/                      # 成本分解（不是胜负承诺）
go test -run TestRecordsPerRequest -v ./bench/           # 每请求写几条（条数口径的钉子）
```

`compare` 会自己 build 被测服务、顺序起停、预热后计时，最后打印可直接粘进
设计文档的 markdown 表。常用开关：`-c 64,256`、`-d 8s`、`-warmup 2s`、
`-passes 4`、`-path /users/42`、`-probe`、`-out result.md`。

只想对着一个已经在跑的服务打：

```console
go run ./cmd/server -fw gin -mode obs -addr 127.0.0.1:18080   # 另开一个终端
go run ./cmd/loadgen -url http://127.0.0.1:18080/users/42 -c 64 -d 15s
```

## 档位

| 档位 | 侧 | gin 侧 | pulse-web 侧 |
|---|---|---|---|
| `bare` | 对拍 | `gin.New()` | `web.New(web.Minimal())` |
| `obs` | 对拍 | `gin.Default()`（Logger + Recovery） | `web.New()`（默认装配 + **默认出口形态**） |
| `obs-line` | 诊断 | — | 出口换成 `NewLineSink` |
| `obs-async` | 诊断 | — | 出口换成 `NewAsyncSink(NewLineSink(...))` |
| `obs-fast` | 诊断 | — | 出口换成 `loadtest/fastsink`（**原型**，池化列式出口） |
| `obs-nolog` | 诊断 | — | 默认装配但关掉访问日志 |

诊断档只跑 pulse-web 一侧：它们回答「**钱花在哪**」（换出口值多少、异步值多少、
关掉访问日志值多少），不回答「谁快」。

### sink 这件事上一版搞错过

为了让日志不落盘，上一版直接把 pulse-web 的出口换成 `NewLineSink(io.Discard)`
——**那不是默认出口**。默认是 `SlogSink`（不指定 Logger 时走 `slog.Default()` → stderr），
而 `SlogSink` 与 `LineSink` 的成本并不一致（一个走 slog 文本 handler，一个是行式缓冲出口）。

现在 `obs` 档用的是**默认出口形态**：

```go
observability.SlogSink{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
```

目的地仍然是空设备（保留每请求的格式化成本、排除磁盘 I/O），但走的是默认那条路径；
「换出口值多少」交给 `obs-line` 单独量。真实部署里它是 stderr（然后落到采集器），
那时磁盘/采集成本两侧都要付。

### 出口能有多便宜：`obs-fast` 与 `fastsink` 原型

`obs-line` / `obs-async` 量的是上游现成出口之间的差别（同一套渲染，只挪线程）。
**渲染本身**能便宜多少，由 `loadtest/fastsink` 这个原型回答——它的手法都是常识：
池化 `[]byte`、不用 `fmt`（`time.AppendFormat` + `strconv.Append*`）、一次锁一次
`Write`。同一个 `Record`、同样写 `io.Discard`：

| 出口 | 单线程 渲染 | 并发 32 路 渲染 | 每请求分配 |
|---|---|---|---|
| `SlogSink`（默认） | ~1531ns | ~1313ns | 19 |
| `LineSink` | ~292ns | ~325ns | 1 |
| `AsyncSink(LineSink)` | ~515ns | — | 1 |
| `fastsink`（原型） | **~122ns** | **~153ns** | **0** |

端到端（`BenchmarkFastSinkVSDefault`，同一条请求路径只换出口）：
`2763ns / 35 allocs` → `1181ns / 16 allocs`；gin 的 `Logger` 同口径约 `1140ns / 15 allocs`。

两点说明：

1. **它放在对比工程里，不是框架代码**。框架要不要自带这样一个出口（以及默认出口
   要不要换、版式谁来定）是产品决定，挂在 **#20**（方案 A 上游改 `SlogSink` /
   B 框架自带 / C 只给示例），本票只出数字。
2. **它不是等价替换**：装配期记录（`observability.host_ready` 等）没有 status / route，
   走同一套列式版式会出现空列——真做要按 `event` 分版式。原型不处理这件事。

## 条数与内容（不是「同一条日志」）

`TestRecordsPerRequest` 钉住这个口径（连出口那层的**行数**一起钉）：

- **条数一样**：pulse-web 每请求 **1 条**结构化 `Record`（Engine 收尾里的 AccessLog；
  请求路径上没有第二处写 Sink 的地方，`c.Observe` 要业务自己调才会多写）；gin 每请求 **1 行**文本。
  pulse-web 另有**装配期 3 条**（Bootstrap 的 `observability.host_ready`），不是每请求。
- **「请求和响应」是同一条记录里的两半，不是两条记录**——请求侧 `http.request.method` /
  `http.route` / `url.path` / `client.address`，响应侧 `status` / `http.response.body.size` /
  `duration_ms`，外加 `trace_id`。LineSink 出去的实样：

  ```
  time=2026-09-13T22:48:37.9496692+08:00 host_id=pulse-web trace_id=fe7d39b7606f9b4b71630724f749e365 source=http event=http.request status=200 client.address=192.0.2.1:1234 http.request.method=GET http.response.body.size=29 http.route=/users/{id} url.path=/users/42
  ```

  （`duration_ms` 只在非零时出现；上面这条是进程内直调、耗时落在时钟精度以下所以没有。）
- **内容不一样**：

  | 侧 | 内容 |
  |---|---|
  | pulse-web | `Source=http Event=http.request Status=200 TraceID=<32hex>` + `http.request.method` / `http.route` / `url.path` / `http.response.body.size` / `client.address`（有错误再加 `error.type`） |
  | gin | `[GIN] 2026/09/13 - 22:18:08 \| 200 \| 0s \| 192.0.2.1 \| GET "/users/42"` |

  差异点：pulse-web 带 TraceID（gin 没有）、带路由**模板**（`/users/{id}`，gin 只有实际路径）、
  带错误分类；客户端地址两边都记。时间戳这边是**出口在写入时才补**的
  （上游 `stampTime`：记录不带就补 `time.Now()`），框架侧的 Record 不携带时间。
- **什么情况下会看到「不止一行」**：① 进程启动时 Bootstrap 的装配记录（一次性，实测 3 条）；
  ② 业务自己调 `c.Observe()`（每调一次一条）；③ **失败路径**——`ServeHTTP`/`finish` 里有
  两处 `slog.Warn`（attach collector 失败、错误映射器自身失败），它们**直写 slog、不经 Sink**；
  默认出口也是 slog，所以这两条会和访问日志落在同一个 stderr 流里，但走的不是同一条路径。

所以观测档量的是「**各家默认开箱配置**」，不是「等价功能的成本」——要给 gin 配上
等价物（TraceID + 路由模板 + 错误分类）得另装第三方中间件。

### 开箱输出（什么都不写，两边各自吐什么）

`go test -run TestOpenBoxOutput -v ./bench/` 实测（2 个请求）：

| | gin（debug 开箱） | pulse-web（默认装配 + 默认 SlogSink） |
|---|---|---|
| 启动 | 2 条 `[GIN-debug] [WARNING]`（Logger/Recovery 已挂、debug 模式提醒）+ **每个路由一条注册行**：`[GIN-debug] GET /users/:id --> ...` | **3 条装配记录**：`event=observability.host_ready` ×2 + `event=pulse.kernel.fiber_state` |
| 每请求 | 1 行 `[GIN] 2026/09/13 - 22:58:12 \| 200 \| 0s \| 192.0.2.1 \| GET "/users/42"` | 1 行结构化记录（字段见上表） |

两点观察：

1. gin 那行是 **5 个字段**（时间 / 状态 / 耗时 / 客户端 / 方法+路径），没有 trace、没有路由模板、
   没有错误分类；pulse-web 那行带 `trace_id` / `source` / `event` / `status` / `client.address` /
   `http.request.method` / `http.route` / `url.path` / `http.response.body.size`（有错再加 `error.type`，
   有时长就加 `duration_ms`）。
2. pulse-web 默认 `SlogSink` 那行里 **`time=` 出现了两次**——slog 的 handler 自己打一个，上游
   `SlogSink` 又把 `r.Time` 当 attr 输出一个。这是**上游的小 wart**（不是本仓库引入的），
   要修得动 `pulse@observability/sink.go`。

失败路径（`TestFailureOutput`：`/boom` 返回 500、`/panic` 直接 panic）：

| | gin（Logger + Recovery） | pulse-web（默认装配） |
|---|---|---|
| 每请求行 | `[GIN] … \| 500 \| 585.1µs \| 192.0.2.1 \| GET "/boom"` | `status=500 error="boom: Internal Server Error" … error.type=http_5xx` |
| panic 那行 | `[GIN] … \| 500 \| 7.62ms \| …`（Recovery 接了并回写 500） | `status=500 error="panic: kaboom" … error.type=panic` |
| panic 的额外输出 | 往 `DefaultErrorWriter` 打一份 `[Recovery] … panic recovered:` **带栈帧** | 无——栈进 `PanicError.Stack`，而默认出口只输出 `error=` 的字符串 |

> 最后一行是个值得单独讨论的点：设计文档写的是「栈只进记录」，但默认出口（`SlogSink`）
> 只渲染 `Err.Error()`，于是**默认配置下 panic 的栈到不了任何地方**——要诊断得换自写出口
> 或直接从 `PanicError` 取。本票只记录，不改框架行为。

## 两处已知的不对称，如实写在下面而不是抹平

1. `pulse-web` 的 `Minimal()` 仍保留 Engine 的 panic 兜底，`gin.New()` 没有 Recovery。
   这一档比的是请求路径，不是兜底；兜底是一次 defer，成本在噪声里。
2. 观测档两边的日志出口都指向空设备（`io.Discard`）：保留每请求的**格式化成本**，
   排除磁盘 I/O——否则这一档比的是磁盘带宽，而不是框架开销。

## 口径细节

- **只让 handler 是变量**：两个服务都用 `http.ListenAndServe` 起，不用
  `gin.Run()` / `Engine.Run()`——那会把各自的默认 server 配置（超时、优雅
  关闭、错误日志）引进对比。
- **同一格内两侧相邻跑**，奇偶轮交换先后，且**只用偶数轮**：先跑位本身有系统性
  优势（实测同一格会差到 20%），奇数轮会让某一侧多占一次先跑位，`compare` 会就此告警。
- **每格取 RPS 中位那一轮**的整套分位，不取「最好的一轮」：噪声确实只会让 RPS
  变低，但轮次先后带来的偏差是双向的，取最好的一轮等于把「哪一侧恰好排在后面」
  当成结果。
- **比值取逐轮配对比值的中位**：同一轮内两侧相邻，机器状态接近，比值比绝对值稳得多。
  **本机的绝对值跨轮会漂 ±20%**（同一格实测出现过 49k / 62k / 70k），所以文档里
  只认比值，绝对值只做量级参考——与 `bench/` 那套「只信跨运行稳定的量」同一条纪律。
- **顺序跑，不并发**：同一时刻只有一个服务在跑，避免两边抢 CPU。
- **压测器自己写**（`internal/loadgen`）：固定并发、固定时长、连接全复用、
  计时窗口内**每个**请求都进分位（不采样）；显式 `Proxy: nil`，这台机器有
  代理环境变量，被套进去打的就是代理而不是被测服务。
- **每格换一个端口**：避免上一格残留 socket 影响下一格绑定。

## 这个 module 是独立的

核心模块（pulse-web 本身）**零第三方依赖**，不为「跟 gin 比一次」破例。
所以对比工程自带 `go.mod`（`replace github.com/Luo-root/pulse-web => ../`，
对比的永远是当前工作副本而不是已发布的版本），gin 只出现在这里。

CI 跑这个 module 的 `go build` / `go vet` / `go test`（防腐烂：根 module 的 `./...`
盖不到它），但**不跑压测/基准**：CI 上的计时是噪声，跑出来的数字只会误导。
