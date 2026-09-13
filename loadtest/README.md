# loadtest — pulse-web 与 gin 的真实负载对比

验收标准第 8 条（「真实负载下与 gin 同级」）的落地工程。**口径先行，结论其次**：
这类对比最容易变成「谁的数字好看谁赢」，所以这里把口径写进代码，数字只是副产品。

## 跑

```console
cd loadtest
go run ./cmd/compare -probe                        # 记录进文档的那次（口径见下）
go run ./cmd/compare                               # 不跑诊断档
go run ./cmd/compare -quick                        # 自检用缩水口径，数字不要进文档
go test -bench . -benchmem ./bench/                # 成本分解（不是胜负承诺）
```

`compare` 会自己 build 被测服务、顺序起停、预热后计时，最后打印可直接粘进
设计文档的 markdown 表。常用开关：`-c 64,256`、`-d 15s`、`-warmup 3s`、
`-passes 4`、`-path /users/42`、`-probe`、`-out result.md`。

只想对着一个已经在跑的服务打：

```console
go run ./cmd/server -fw gin -mode obs -addr 127.0.0.1:18080   # 另开一个终端
go run ./cmd/loadgen -url http://127.0.0.1:18080/users/42 -c 64 -d 15s
```

## 两个对拍档位

| 档位 | gin 侧 | pulse-web 侧 | 这一档比什么 |
|---|---|---|---|
| `bare` | `gin.New()` | `web.New(web.Minimal())` | 路由 + 上下文 + 写响应 |
| `obs` | `gin.Default()`（Logger + Recovery） | `web.New()`（Bootstrap + Trace + 访问日志） | 把各家**自带**的观测也算进去 |

还有一个 `obs-nolog` 是 **pulse-web 的诊断档，不是对拍档**（gin 侧没有对应档，
它的 `bare` 就是「不开日志」）：只开 Trace、关掉访问日志，用来把观测那截开销
拆成「Trace + 记录框架」与「访问日志 + 出口」两段。**它回答「钱花在哪」，
不回答「谁快」**。

两侧的服务端是刻意写成的对拍（`pulseapp/app.go` 与 `ginapp/app.go`），
同样的路由、同样的响应体、同样的分节顺序——两个文件之间的 diff 就是
「同一件事两边怎么写」。

**两处已知的不对称，如实写在下面而不是抹平：**

1. `pulse-web` 的 `Minimal()` 仍保留 Engine 的 panic 兜底，`gin.New()` 没有
   Recovery。这一档比的是请求路径，不是兜底；兜底是一次 defer，成本在噪声里。
   要严格对称可以把 `bare` 档的 gin 侧改成 `gin.New()+gin.Recovery()`。
2. 观测档两边的日志出口都指向空设备（`io.Discard`）：保留每请求的**格式化
   成本**，排除磁盘 I/O——否则这一档比的是磁盘带宽，而不是框架开销。
3. `obs` 档两侧做的**并不是同一件事**：pulse-web 每请求生成 TraceID 并组装
   结构化记录写 Sink，gin 只是把一行文本格式化后写出。这一档量的是「各家默认
   开箱配置」，不是「等价功能的成本」——要给 gin 配上等价的追踪得另装第三方中间件。

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

CI 只**编译**这个 module（防腐烂），**不跑压测**：CI 上的计时是噪声，
跑出来的数字只会误导。
