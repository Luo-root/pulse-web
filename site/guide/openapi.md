# OpenAPI 工具链

框架里**没有**任何 OpenAPI / Swagger 代码：不生成 spec、不托管文档页、不认识 schema。这一页讲的是**用现成的包怎么接**——主推的注释驱动路线给的是实测过的完整配方与四个坑，另两条只给判断依据，最后是「谁挂进来会削掉什么」。

## 为什么框架不做

三条硬事实同时成立，才叫「不做」：

- handler 是 `func(*web.Ctx) error`，**不带类型信息**——spec 要的 schema 从签名里推不出来；
- 路由直接交给标准库 `ServeMux`，引擎**不保留路由表**，没有可枚举的注册记录；
- 标准库 `ServeMux` 本身**没有枚举 API**：`Handle` / `HandleFunc` / `Handler(r)` / `ServeHTTP`，仅此。

要在框架里生成 spec，就得新增一整套声明词汇表（schema / 参数 / 响应 / 安全），而那套词汇表注定是 OpenAPI 的劣化副本，还会同时破坏三条约定：核心模块零第三方依赖、不分领域语义、不允许逃生舱。

生态现状也支持这个判断：gin、chi、fiber、beego、go-zero 的 README 里一处 OpenAPI 都没有；echo 与 hertz 只在「第三方中间件清单」里给个入口。**只有把 OpenAPI 当一等公民的框架**（huma、Fuego、goa）才自带——它们为此改掉了 handler 的形状。

## 主推：注释驱动（swaggo）

handler 不动，只在它上方写注解：

```go
// @Summary      Get one user
// @Tags         users
// @Param        id   path      int  true  "User ID"
// @Success      200  {object}  main.User
// @Router       /users/{id} [get]
func getUser(c *web.Ctx) error {
	return c.JSON(http.StatusOK, User{ID: 1, Name: c.Path("id")})
}
```

依赖分两半：UI 是 `github.com/swaggo/http-swagger/v2`（普通依赖）；生成器 `swag` 是 **CLI**，不进 `go.mod`：

```bash
go install github.com/swaggo/swag/cmd/swag@v1.16.4
swag init -g main.go -o docs      # 产出 docs/docs.go + swagger.json + swagger.yaml
```

挂载——它就是一个普通的 `http.Handler`：

```go
import (
	web "github.com/Luo-root/pulse-web"
	httpSwagger "github.com/swaggo/http-swagger/v2"

	_ "yourapp/docs" // swag init 的产物：init() 把 spec 注册进 swaggo 的 registry
)

app := web.New()
app.GET("/users/{id}", getUser)
app.Handle("/swagger/", web.Wrap(httpSwagger.WrapHandler))
```

`/swagger/index.html` 是 UI，`/swagger/doc.json` 是 spec 本体；两者都走框架的中间件链与观测——访问日志里它们带自己的 trace id。

### 四个坑

1. **`swag init` 是构建前置步骤**。产出的 `docs/` 要么提交进仓库，要么在 CI 里先生成：代码里有 `_ "yourapp/docs"` 这个导入，缺了编不过。
2. **CLI 与库版本必须对齐**。`go.mod` 里的 `github.com/swaggo/swag` 要钉成与 CLI 相同的版本——CLI v1.16.4 生成的 `docs.go` 用了 `LeftDelim` / `RightDelim`，而 `go mod tidy` 可能解析出很旧的 `v1.8.1`（没这两个字段），编译期才会报 `unknown field`。
3. **响应体必须是具名类型**。`c.JSON(200, map[string]any{...})` 推不出 schema：给响应定义 struct，字段配 `json` 与 `example` tag，注解里写 `@Success 200 {object} main.User`。
4. **稳定线只到 Swagger 2.0**。产出 spec 的第一行是 `"swagger": "2.0"`；要 3.x 见下一条。

### 想要 OpenAPI 3.0：转一下

`kin-openapi` 自带 2.0 → 3.0 的转换器。实测转出的 `openapi: 3.0.3` 保留全部 paths 与 schemas，并通过官方校验：

```go
var doc2 openapi2.T
json.Unmarshal(raw, &doc2)              // raw = swag 产出的 swagger.json
doc3, err := openapi2conv.ToV3(&doc2)
if err != nil {
	return err
}
if err := doc3.Validate(context.Background()); err != nil {
	return err
}
```

把它做成 CI 里的一步（构建后转一次，把 3.0 的 spec 当产物发布）。**不要手改转出来的文件**：下次 `swag init` 会覆盖源头。

## 另外两条路线

**spec 优先（`oapi-codegen` / `ogen`）**：先写 `openapi.yaml`，再由它生成类型与服务骨架。`oapi-codegen` 有面向标准库 `net/http` 的 `std-http-server` 模板（Go 1.22+），ogen 生成的 `Server` 本身就是 `http.Handler`——同样经 `web.Wrap` 挂进来。适合「契约先于实现」的团队；代价是先写 YAML，且生成代码的风格不 Go。

**代码优先叠坐（huma）**：huma 把 OpenAPI 当一等公民（产 3.1，自带 `/openapi.json` 与 `/docs`），并且**官方提供标准库 `ServeMux` 的适配器**：

```go
sub := http.NewServeMux()
api := humago.New(sub, huma.DefaultConfig("My API", "1.0.0"))
app.Handle("/api/", web.Wrap(sub))
```

它的 handler 形状是 `func(ctx, *In) (*Out, error)`，不是 `func(*web.Ctx) error`——那条子树里就没有 `c` 用了。

## 挂进来会削掉什么

上面两条路线（含 huma 那条）都是把**一整棵路由树**当成一个 `http.Handler` 挂进引擎。框架的 `ServeMux` 只认得「子树 pattern」，于是：

- 访问日志与 span 的 `http.route` 会变成挂载前缀（`/api/`），**不再逐条路由**：trace id、耗时、状态码都在，只有这一列粗了；
- 子树内部的中间件与参数解析归那套库自己管，框架的中间件只在外层生效。

注释驱动那条不受影响：handler 仍是框架的 handler，路由归属照旧（见[观测](/guide/observability)里的 `http.route` 口径）。

## 自己验证

```bash
go install github.com/swaggo/swag/cmd/swag@v1.16.4
swag init -g main.go -o docs
go run . && curl -s -o /dev/null -w '%{http_code} %{http_code}\n' localhost:8080/swagger/index.html localhost:8080/swagger/doc.json
```

两条都应是 `200`；再看启动日志里你自己的路由，`route=` 那一列没有变成 `/swagger/`。
