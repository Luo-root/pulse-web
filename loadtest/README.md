# loadtest — pulse-web 与 gin 的真实负载对比

设计验收标准『真实负载与 gin 同级』（不以 micro-benchmark 胜负作承诺）的落地工程。**口径先行，结论其次**：
这类对比最容易变成「谁的数字好看谁赢」，所以这里把口径写进代码，数字只是副产品。

它**住在仓库里**（而不是某条侧分支上）。这不是洁癖：早前它待在 `bench/gin-compare` 分支上，
默认出口从 `SlogSink` 换成 `ConsoleSink` 之后，这一档**继续按「默认出口」的名义出数字、
量的却是旧默认**——不报错、不告警，直到要重跑才发现。跟着主干走、进 CI，它就没机会静默过期。

## 什么时候必须重跑（谁负责）

**默认出口、默认中间件、请求路径上的任何东西一变，改它的那个人就在同一个 PR 里重跑**：

```console
cd loadtest && go run ./cmd/compare -probe -passes 4 -d 8s -warmup 2s
```

然后把设计文档表 C 与站点性能页（中英两版）一起更新——同一批数字的两个读者面，只改一处比不改更糟。
当场出不了数（机器不在、跑不动）就在 PR 描述里**写明这一轮的负载数字不代表现状**，别让旧数字继续
挂在新默认旁边。

这条规则就是这个工程进仓库的理由：它上一次失效时（`SlogSink` → `ConsoleSink`）没人被提醒，
数字安静地错了一轮（#68 / #73）。

## 跑

```console
cd loadtest
go run ./cmd/compare -probe -passes 4 -d 8s -warmup 2s   # 记录进文档的那次
go run ./cmd/compare                                     # 只跑对拍档
go run ./cmd/compare -quick                              # 自检用缩水口径，数字不要进文档
go test -bench . -benchmem ./bench/                      # 成本分解（不是胜负承诺）
go test -run TestRecordsPerRequest -v ./bench/           # 每请求写几条（条数口径的钉子）
```

`compare` 会自己 build 被测服务、顺序起停、预热后计时，最后打印可直接粘进设计文档的
markdown 表。常用开关：`-c 64,256`、`-d 8s`、`-warmup 2s`、`-passes 4`、`-path /users/42`、
`-probe`、`-out result.md`。

只想对着一个已经在跑的服务打：

```console
go run ./cmd/server -fw gin -mode obs -addr 127.0.0.1:18080   # 另开一个终端
go run ./cmd/loadgen -url http://127.0.0.1:18080/users/42 -c 64 -d 15s
```

## 档位

| 档位 | 侧 | gin 侧 | pulse-web 侧 |
|---|---|---|---|
| `bare` | 对拍 | `gin.New()` | `web.New(web.Minimal())` |
| `obs` | 对拍 | `gin.Default()`（Logger + Recovery） | `web.New()`（默认装配 + **默认出口** `ConsoleSink`） |
| `obs-async` | 诊断 | — | 默认出口**外面再套一层** `NewAsyncSink` |
| `obs-nolog` | 诊断 | — | 默认装配但关掉访问日志 |

诊断档只跑 pulse-web 一侧：它们回答「**钱花在哪**」，不回答「谁快」。

### 出口选项只留今天真实存在的那几个

那两格是「**默认**」与「**默认外面套一层 `AsyncSink`**」——实际会被人选的两种。历史上有过
两档不该再出现的：

- `obs-line`（换 `LineSink`）：它的手法已经被默认出口吸收——`ConsoleSink` 就是行式缓冲出口 +
  列式版式的合体，再单列一格量的已经不是「换成什么更好」，而是「过去长什么样」；
- `obs-fast`（`loadtest/fastsink` 原型出口）：一个**不会发布**的实现。拿它当选项比较，等于
  拿原型当基准——它当初要证明的事已经由 `ConsoleSink` 兑现了。

同类的还有 `bench/` 里那个 `columnSink` 复刻（列式格式的一份手写副本，连它的演示用例一起删了）：
现在换成真的 `web.NewConsoleSink`。规则就一句：**基准必须是会被发布的那个东西**。

`AsyncSink` 的内层用 `ConsoleSink` 而不是缓冲出口：`AsyncSink` 只排空它自己的队列、**不级联**
内层的 `Flush`，内层要是还缓冲着，进程退出时那截就丢了。

## 条数与内容（不是「同一条日志」）

`TestRecordsPerRequest` 钉住这个口径（连出口那层的**行数**一起钉）：

- **条数一样**：pulse-web 每请求 **1 条**结构化 `Record`（Engine 收尾里的 AccessLog；
  请求路径上没有第二处写 Sink 的地方，`c.Observe` 要业务自己调才会多写）；gin 每请求 **1 行**文本。
  pulse-web 另有**装配期 3 条**（Bootstrap 的 `observability.host_ready` 等），不是每请求。
- **「请求和响应」是同一条记录里的两半，不是两条记录**——请求侧 `http.request.method` /
  `http.route` / `url.path` / `client.address`，响应侧 `status` / `http.response.body.size` /
  `duration`，外加 `trace_id`。默认出口出去的实样：

  ```
  PULSE | 2026/09/16 - 16:29:06 | 200 |       0ns | 192.0.2.1:1234  | GET     /users/42 | route=/users/{id} | size=29 | host=pulse-web | trace=628a97798d29a9f654a762ca0dd8f71b
  ```

  （时长只在非零时出现；上面这条是进程内直调、耗时落在本机时钟精度以下所以是 `0ns`——
   这台机器的 `time.Now()` 推进量子约 0.3–0.6ms，亚毫秒区间读不出真值。）
- **内容不一样**：

  | 侧 | 内容 |
  |---|---|
  | pulse-web | 状态 / 时长 / 客户端 / 方法+路径 / 路由**模板** / 响应字节 / host / `trace`，有错误再加**错误格**：`http_5xx "boom: Internal Server Error"` |
  | gin | `[GIN] 2026/09/16 - 16:29:06 \| 200 \| 0s \| 192.0.2.1 \| GET "/users/42"` |

  差异点：pulse-web 带 TraceID（gin 没有）、带路由**模板**（`/users/{id}`，gin 只有实际路径）、
  带错误**分类**（`http_5xx` / `panic`，低基数、可直接聚合）；客户端地址两边都记。
- **什么情况下会看到「不止一行」**：① 进程启动时 Bootstrap 的装配记录（一次性，实测 3 条）；
  ② 业务自己调 `c.Observe()`（每调一次一条）；③ **失败路径**——`ServeHTTP`/`finish` 里有
  两处 `slog.Warn`（attach collector 失败、错误映射器自身失败），它们**直写 slog、不经 Sink**。

所以观测档量的是「**各家默认开箱配置**」，不是「等价功能的成本」——要给 gin 配上等价物
（TraceID + 路由模板 + 错误分类）得另装第三方中间件。

## 开箱输出（什么都不写，两边各自吐什么）

`go test -run TestOpenBoxOutput -v ./bench/` 实测（2 个请求）。gin 侧走**默认 debug 模式**
（那才是它的开箱形态）：

| | gin（debug 开箱） | pulse-web（默认装配 + 默认出口 `ConsoleSink`） |
|---|---|---|
| 启动 | 2 条 `[GIN-debug] [WARNING]` + **每个路由一条注册行**：`[GIN-debug] GET /users/:id --> ...` | **3 条装配记录**：`event=observability.host_ready` ×2 + `event=pulse.kernel.fiber_state` |
| 每请求 | 1 行 `[GIN] 2026/09/16 - 16:29:06 \| 200 \| 0s \| 192.0.2.1 \| GET "/users/42"` | 1 行列式记录（字段见上） |

失败路径（`TestFailureOutput`：`/boom` 返回 500、`/panic` 直接 panic）：

| | gin（Logger + Recovery） | pulse-web（默认装配） |
|---|---|---|
| 每请求行 | `[GIN] … \| 500 \| 585.1µs \| 192.0.2.1 \| GET "/boom"` | `… \| 500 \| 531.9µs \| 192.0.2.1:1234 \| GET /boom \| size=60 \| host=pulse-web \| http_5xx "boom: Internal Server Error" \| trace=…` |
| panic 那行 | `[GIN] … \| 500 \| 2.14ms \| …`（Recovery 接了并回写 500） | `… \| 500 \| 0ns \| … \| panic "panic: kaboom" \| trace=…` |
| panic 的额外输出 | 往 `DefaultErrorWriter` 打一份 `[Recovery] … panic recovered:` **带栈帧** | 无 |

> 最后一行是值得单独讨论的点：设计文档写的是「栈只进记录」，而默认出口只渲染错误**消息串**
> （`panic "panic: kaboom"`），于是**默认配置下 panic 的栈到不了任何地方**——要诊断得自写出口
> 或直接从 `PanicError` 取。这里只记录，不改框架行为。

## 两处已知的不对称，如实写在下面而不是抹平

1. `pulse-web` 的 `Minimal()` 仍保留 Engine 的 panic 兜底，`gin.New()` 没有 Recovery。
   这一档比的是请求路径，不是兜底；兜底是一次 defer，成本在噪声里。
2. 观测档两边的日志出口都指向空设备（`io.Discard`）：保留每请求的**格式化成本**，
   排除磁盘 I/O——否则这一档比的是磁盘带宽，而不是框架开销。出口写成默认那条路径
   （`NewConsoleSink(io.Discard)`），不写 `web.New()` 走 stdout：那会反向引入管道写，
   把父进程的读取协程拖进对比。

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

## 这个 module 是独立的，但在仓库里

核心模块（pulse-web 本身）**零第三方依赖**，不为「跟 gin 比一次」破例。所以对比工程自带
`go.mod`（`replace github.com/Luo-root/pulse-web => ../`，比对的永远是**当前工作副本**而不是
已发布的版本），gin 只出现在这里；主模块的 `./...` 不过 module 边界，依赖判据不受影响。

CI 单独跑它的 `go build` / `go vet` / `go test`（防腐烂：根 module 的 `./...` 盖不到它），
但**不跑压测 / 基准**——CI 上的计时是噪声，跑出来的数字只会误导。
