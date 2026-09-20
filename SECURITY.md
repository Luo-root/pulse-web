# Security Policy

Pulse-Web is pre-1.0 (`v0.x`). This file covers how to report a vulnerability and what is in scope.
Both halves are equivalent: English first, 中文在后.

## Supported versions

**There is no tagged release yet.** Until the first `vX.Y.Z` tag exists, `main` is the only
supported version — the rows below apply from that first tag onward.

| Version | Supported |
| --- | --- |
| `main` | ✅ |
| newest `v0.x` release (see [Releases](https://github.com/Luo-root/pulse-web/releases)) | ✅ *(from the first tag onward)* |
| older `0.x` releases | ❌ |

Fixes land on `main` and in the newest release. There are no backports during the `0.x` line,
so the fix for an older release is always the newest one.

## Reporting a vulnerability

**Do not open a public issue.** A public issue is visible to everyone long before a fix exists.

Report privately by email to **3029295957@qq.com**, including:

- the affected version, tag or commit;
- what the problem is and why it matters — the impact, not just the symptom;
- a minimal reproduction: code, configuration, or exact steps;
- any fix or mitigation you already have in mind;
- how you want to be credited, or say that you would rather not be.

GitHub's private vulnerability reporting is **not enabled** on this repository, so email is the
private channel. If that changes, this section will be updated.

## What to expect

- An acknowledgement within a few days.
- An assessment — is it a vulnerability, and how severe — plus a decision on the fix.
- Credit in the release notes when the fix ships, if you want it.
- **No bug bounty.** This is an unfunded open-source project; there is nothing to pay out, and
  we would rather say so up front than leave it implied.

## In scope

Anything in this repository — the framework's own handling of a request, and its assembly:

- routing: route matching, path-parameter extraction, groups, static-file routes;
- the middleware chain: composition order and short-circuit semantics;
- request binding: Content-Type dispatch (JSON / XML / form / multipart), the body limit and
  its 413 path;
- the error model: explicit error → status-code mapping, panic containment;
- the response writer wrapper: first-write status capture, byte counting, `Flush`, `Hijack`;
- TraceID generation and the inbound header trust switch;
- HTML template rendering and static file serving;
- stdlib interop (`web.Wrap` for stdlib handlers in, `web.Adapt` for stdlib middleware inside
  the onion, `Engine.Handler()` out);
- assembly and lifecycle: `WithRoot` and its cascading dispose, the graceful-shutdown
  sequence;
- the default console output and the fields an observation record can hold.

## Out of scope

- **Third-party dependencies** — report those upstream (a heads-up here is still appreciated).
  The supply-chain surface of the core module is deliberately small: `stdlib` plus
  `pulse/kernel` and `pulse/observability`. Anything in the upstream framework belongs in
  [Luo-root/pulse](https://github.com/Luo-root/pulse); anything in the standard library
  belongs with the Go project.
- **Your handler's own bugs.** Pulse-Web routes a request to your code and reports what your
  code returned. Authentication, authorization, input validation, injection and access control
  inside a handler are the application's responsibility — a vulnerable handler is not a
  framework vulnerability. (A framework flaw that *forces* a handler into an unsafe pattern —
  for example, silently corrupting an error value so an auth failure reads as success — is in
  scope.)
- **Anything requiring an attacker who already controls** the process, the machine, the
  environment, or the credentials configured in it.
- **Trust boundaries you placed yourself.** `client.address` is the peer address and does not
  parse `X-Forwarded-For`; the scheme is not recorded; the framework assumes a reverse proxy
  terminates TLS and sets its own trust boundary. A deployment that trusts a header the
  framework explicitly does not is a configuration problem.
- **Secrets committed in a fork** — secret scanning runs on this repository, and `.env*` is
  gitignored.
- **Code from the docs and examples pasted into production.** Examples are minimal
  illustrations of an API, not hardened configurations.

## Known intentional defaults (not vulnerabilities)

These are documented, deliberate defaults. If you can turn one of them into impact **beyond**
what is described here, that part is worth reporting.

- **Inbound trace headers are trusted by default** (`WithTrustedTraceHeader(true)`): a
  `traceparent` or `X-B3-TraceId` header is adopted as the request's TraceID. That is what lets
  a trace stay connected behind a gateway. It also means a client can choose the trace-id that
  appears in your logs. If the service is exposed directly, set the option to `false` and the
  framework always generates its own. This is a deployment trade-off, stated in the option's
  documentation.
- **The request body has no size limit by default** (`WithMaxBodyBytes` is unset), matching the
  default shape of gin and echo: a limit is a business policy. Production deployments either set
  the option (global), attach `BodyLimit` to the routes and groups that need a different limit
  (it can only tighten the global one, never loosen it), or set a limit in the reverse proxy.
- **`http.request` records are written for every request** (unless `WithoutAccessLog()` is set),
  including failed ones. The record carries method, route template, path, status, duration,
  response size and peer address — see the next section for what it cannot carry.
- **The write timeout is off by default.** `ServerConfig` ships read-side protection
  (`ReadHeaderTimeout` 5s, `ReadTimeout` 30s, `IdleTimeout` 120s, `MaxHeaderBytes` 1 MiB) but
  `WriteTimeout` defaults to `0`: a non-zero value cuts off SSE and long-lived connections, so
  the default cannot be one. A handler that runs forever is not killed by a write deadline —
  use the request context, or set the timeouts you want via `WithServer`.

## What is already in place

- Secret scanning and push protection are enabled on this repository.
- `.env`, `.env.*`, `*.pem` and `*.secrets` are gitignored.
- **The framework does not package the request body.** `http.request.body.size` is deliberately
  not measured: doing so means wrapping `r.Body`, which breaks streaming semantics. Response
  size is recorded because the framework already wraps the response writer.
- **No headers, no bodies and no query strings go into observation records.** `Record.Attrs`
  only accepts scalars (`~string | ~int64 | ~float64 | ~bool`) and has no `map[string]any`
  escape hatch, so a token, a cookie, a request body or a prompt cannot enter a record by
  construction. `url.query` is not recorded either — both because of its cardinality and
  because it routinely carries sensitive parameters.
- **The response writer wrapper forwards an explicit short list**: `Flush`, `Hijack`,
  `SetWriteDeadline` and `EnableFullDuplex`. `Pusher`, `FlushError` and `Unwrap()` are not
  passed through — `Unwrap()` would hand the whole underlying writer over, the two above
  included.
- **Panics in a handler are contained**: the request is answered with 500 and the process keeps
  serving. The panic is attached to the observation record as a `*web.PanicError` carrying the
  value and the stack trace; the client receives only the status text. `PanicError.Stack` never
  enters a response body.
- **Provider-less**: the framework makes no outbound network calls of its own. Running the test
  suite requires no credentials.

---

## 中文

Pulse-Web 目前仍在 1.0 之前（`v0.x`）。本文件说明漏洞上报方式与适用范围。

### 支持范围

**目前还没有 tag 过的 release。** 在首个 `vX.Y.Z` 之前，唯一受支持的版本是 `main`；
下表自首个 tag 起生效。

| 版本 | 是否支持 |
| --- | --- |
| `main` | ✅ |
| 最新的 `v0.x` release（见 [Releases](https://github.com/Luo-root/pulse-web/releases)） | ✅（自首个 tag 起） |
| 更早的 `0.x` 版本 | ❌ |

修复落在 `main` 与最新一版 release 上。`0.x` 期间不做 backport，所以旧版本上的修复就是升级到最新版。

### 上报漏洞

**不要开公开 Issue。**公开 Issue 在修复出现之前就对所有人可见。

请发邮件到 **3029295957@qq.com** 私密上报，内容尽量包含：

- 受影响的版本、tag 或 commit；
- 问题是什么、为什么重要（说影响，不只是现象）；
- 最小复现：代码、配置或确切步骤；
- 你已经有思路的修复或缓解方式；
- 是否愿意被致谢、以什么名义。

本仓库**未启用** GitHub 私密漏洞报告，所以邮箱是当前的私密渠道。若之后启用了，本节会同步更新。

### 你会得到什么

- 几天内的确认。
- 一个评估（是不是漏洞、严重程度如何）以及是否修、怎么修的决定。
- 修复随版本发布时在 Release notes 中致谢（如果你愿意）。
- **没有漏洞奖金。**这是一个没有经费的开源项目；与其含糊，不如直接说明。

### 在范围内

本仓库里的一切——框架自身对请求的处理，以及它的装配：

- 路由：路由匹配、路径参数提取、分组、静态文件路由；
- 中间件链：组合顺序与短路语义；
- 请求绑定：Content-Type 分派（JSON / XML / form / multipart）、body 上限及其 413 路径；
- 错误模型：显式 error → 状态码映射、panic 兜底；
- 响应写出器包装：首刷状态码捕获、字节计数、`Flush`、`Hijack`；
- TraceID 生成与入站链路头的信任开关；
- HTML 模板渲染与静态文件服务；
- stdlib 互操作（`web.Wrap` 接 stdlib handler 入、`web.Adapt` 把 stdlib 中间件接入洋葱内、`Engine.Handler()` 出）；
- 装配与生命周期：`WithRoot` 的级联销毁、优雅关闭时序；
- 默认控制台输出，以及一条观测记录能装得下的字段面。

### 不在范围内

- **第三方依赖漏洞**——请上报给上游（顺手告知我们一声仍然欢迎）。核心模块的供应链面是刻意做小的：
  `stdlib` 加 `pulse/kernel`、`pulse/observability` 两个包。上游框架的问题属于
  [Luo-root/pulse](https://github.com/Luo-root/pulse)；标准库的问题属于 Go 项目。
- **你自己 handler 里的漏洞。** Pulse-Web 把请求路由到你的代码，并如实报告你的代码返回了什么。
  handler 内部的鉴权、授权、输入校验、注入与访问控制是应用的责任——一个有漏洞的 handler
  不是框架漏洞。（框架缺陷**逼着** handler 走向不安全模式才算在范围内——例如静默破坏错误值，
  让鉴权失败被读成成功。）
- **需要攻击者已经控制**进程、机器、运行环境或其中配置的凭据的场景。
- **你自己划的信任边界。** `client.address` 是对端地址，不解析 `X-Forwarded-For`；scheme 不记录；
  框架假定前置反代终止 TLS 并自行划定信任边界。去信任框架明确不信任的头，是部署配置问题。
- **fork 里提交的密钥**——本仓库已开启 secret scanning，且 `.env*` 已被忽略。
- **把文档与示例代码直接搬进生产。** 示例是 API 的最小说明，不是加固过的配置。

### 已知的有意默认值（不要当漏洞上报）

以下都是已文档化、有意的默认值。如果你能把其中某一条做成**超出**这里所述的影响，那部分值得报。

- **入站链路头默认被信任**（`WithTrustedTraceHeader(true)`）：`traceparent` 或 `X-B3-TraceId`
  会被采纳为本次请求的 TraceID。这是「网关之后的链路接得上」的代价，也意味着客户端可以选择
  出现在你日志里的 trace-id。若服务直接暴露，把它设成 `false`，框架就总是自生成。这是部署取舍，
  写在该选项的文档里。
- **请求体默认不设上限**（不传 `WithMaxBodyBytes`），与 gin、echo 的默认形态一致：上限值是业务
  策略。生产部署的三条出口：设这个选项（全局）、给需要不同上限的路由 / 分组挂 `BodyLimit`
  （只能收紧全局那道，不能放宽）、或者在反代设限。
- **每个请求都会写 `http.request` 记录**（设了 `WithoutAccessLog()` 时除外），包括失败的请求。
  记录里是 method、路由模板、路径、状态码、耗时、响应体积与对端地址——**装不下**什么见下一节。
- **写出侧超时默认关闭。** `ServerConfig` 自带读侧保护（`ReadHeaderTimeout` 5s、
  `ReadTimeout` 30s、`IdleTimeout` 120s、`MaxHeaderBytes` 1 MiB），但 `WriteTimeout` 默认是 `0`：
  非 0 会切断 SSE 与长连接，所以默认值不能是它。跑不完的 handler 不会被写超时杀掉——用请求
  context，或按需通过 `WithServer` 设成你想要的超时。

### 已经做了哪些加固

- 仓库已开启 secret scanning 与 push protection。
- `.env`、`.env.*`、`*.pem`、`*.secrets` 均在 .gitignore 中。
- **框架不包请求体。** `http.request.body.size` 是刻意不量的：量它就要包 `r.Body`，会破坏流式
  语义。响应体积能记是因为框架本来就包了响应写出器。
- **头、体、query 都不会进观测记录。** `Record.Attrs` 只接受标量
  （`~string | ~int64 | ~float64 | ~bool`）且没有 `map[string]any` 逃生舱，所以 token、cookie、
  请求体、prompt 在类型上就进不了记录。`url.query` 同样不记——既因为基数，也因为它常带敏感参数。
- **响应写出器包装只转发一份显式的短清单**：`Flush`、`Hijack`、`SetWriteDeadline`、
  `EnableFullDuplex`。`Pusher`、`FlushError` 与 `Unwrap()` 都不透出——`Unwrap()` 等于把底层
  writer 整个交出去，连上面两个一起。
- **handler 里的 panic 被兜住**：该请求以 500 作答，进程继续服务。panic 以 `*web.PanicError`
  （值 + 栈）挂到观测记录上；客户端只拿到状态文本，`PanicError.Stack` 绝不进响应体。
- **无 provider、无外呼**：框架自身不发任何出站请求，跑测试集不需要任何凭据。
