package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CORSOption 配置 app.CORS 的行为，见 CORSAllow* / CORSExposeHeaders / CORSMaxAge。
type CORSOption func(*corsConfig)

// corsConfig 是装配期就规范化好的配置：来源 / 方法 / 请求头都在这里转成待查的集合，
// 请求路径上只做查表与拼串。
type corsConfig struct {
	origins        map[string]struct{} // 全部小写、去掉尾斜杠
	originsAll     bool                // CORSAllowOrigins("*")
	methods        map[string]struct{} // 全部大写
	methodsAll     bool
	requestHdrs    map[string]struct{} // 全部小写
	requestHdrsAll bool
	exposeHdrs     string // 已拼好的头列表，空 = 不写这个响应头
	credentials    bool
	maxAge         int // 秒；0 = 不写这个响应头
}

// CORSAllowOrigins 设置允许的来源，可给多个，写法与浏览器 Origin 头一致：
// `scheme://host[:port]`，**不带路径**（尾斜杠会被去掉）。给 `"*"` 表示任意来源。
//
// 必须给至少一个来源，否则 app.CORS 在装配期 panic——"没配"多半是漏了，而不是
// 有意拒绝全部跨源请求。`"*"` 与 CORSAllowCredentials 不能同时用（见该函数的说明）。
func CORSAllowOrigins(origins ...string) CORSOption {
	return func(c *corsConfig) {
		for _, o := range origins {
			o = strings.ToLower(strings.TrimRight(strings.TrimSpace(o), "/"))
			if o == "" {
				continue
			}
			if o == "*" {
				c.originsAll = true
				continue
			}
			c.origins[o] = struct{}{}
		}
	}
}

// CORSAllowMethods 设置实际请求允许用的方法，默认 `GET` / `POST` / `HEAD`（Fetch 的
// simple methods）。给 `"*"` 表示任意方法。
func CORSAllowMethods(methods ...string) CORSOption {
	return func(c *corsConfig) {
		c.methods = map[string]struct{}{}
		for _, m := range methods {
			m = strings.ToUpper(strings.TrimSpace(m))
			if m == "" {
				continue
			}
			if m == "*" {
				c.methodsAll = true
				continue
			}
			c.methods[m] = struct{}{}
		}
	}
}

// CORSAllowHeaders 设置预检里允许客户端携带的请求头（`Access-Control-Request-Headers`
// 那一串），默认空——即只允许 CORS 安全列表内的头。给 `"*"` 表示任意请求头。
//
// 名字按 HTTP 语义**大小写不敏感**（两边都规范化成小写再比）。
func CORSAllowHeaders(headers ...string) CORSOption {
	return func(c *corsConfig) {
		for _, h := range headers {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" {
				continue
			}
			if h == "*" {
				c.requestHdrsAll = true
				continue
			}
			c.requestHdrs[h] = struct{}{}
		}
	}
}

// CORSExposeHeaders 设置浏览器脚本读得到的那几个响应头（`Access-Control-Expose-Headers`）。
// 不配的话脚本只读得到 CORS 安全列表内的响应头。
func CORSExposeHeaders(headers ...string) CORSOption {
	return func(c *corsConfig) {
		out := make([]string, 0, len(headers))
		for _, h := range headers {
			if h = strings.TrimSpace(h); h != "" {
				out = append(out, h)
			}
		}
		c.exposeHdrs = strings.Join(out, ", ")
	}
}

// CORSAllowCredentials 允许跨源请求携带凭据（Cookie / TLS 客户端证书 / Authorization）。
//
// 它要求响应里**回显具体来源**而不是 `*`，所以与 CORSAllowOrigins("*") 是矛盾的一对：
// 那个组合等于「任何网站都能带凭据访问本服务」，是漏洞而不是配置，本框架在装配期
// panic 拦下。要开放给多个站点就逐个列出来。
func CORSAllowCredentials() CORSOption { return func(c *corsConfig) { c.credentials = true } }

// CORSMaxAge 设置预检结果的缓存时长（`Access-Control-Max-Age`），<= 0 不写这个响应头
// （由浏览器自己定一个保守值）。注意各浏览器对上限的钳制不同（Chrome 2 小时、Firefox
// 24 小时），写大了也会被截。
func CORSMaxAge(d time.Duration) CORSOption {
	return func(c *corsConfig) {
		c.maxAge = int(d / time.Second)
		if c.maxAge < 0 {
			c.maxAge = 0
		}
	}
}

// CORS 把跨源资源共享挂进本引擎：一是把 CORS 中间件压进洋葱（实际请求带 CORS 头），
// 二是**为之后注册的每条路由补一条同名 `OPTIONS` 路由**——预检因此进得了洋葱。
//
// 第二条是本框架特有的一半：路由匹配先于中间件（见「中间件与 stdlib 互操作」里的
// 「不搬动路由」），只注册了 `GET /api` 时 `OPTIONS /api` 会被 ServeMux 直接 405 掉，
// 预检根本到不了中间件。补出来的那条 OPTIONS 路由带着**同一条链**（全局 + 分组中间件），
// 于是预检照常进访问日志与 Trace——外包 `Handler()` 那条老路做不到这一点。
//
//	app.CORS(
//	    web.CORSAllowOrigins("https://app.example.com"),
//	    web.CORSAllowCredentials(),
//	    web.CORSMaxAge(10*time.Minute),
//	)
//
// 装配期调用，且要在注册业务路由**之前**（与 Use 同一规矩）。分组同样可用：
//
//	api := app.Group("/api")
//	api.CORS(web.CORSAllowOrigins("https://app.example.com"))
//	api.GET("/users", listUsers)   // 同时得到 OPTIONS /api/users
//
// 补出来的那条 OPTIONS 只做两件事：真预检（带 `Access-Control-Request-Method`）由中间件
// 当场答 `204`；**裸 OPTIONS**（没有那个头）则由它自己答 `405` + `Allow`，与 ServeMux
// 今天给的答案一致——差别是这条请求进了洋葱（有访问记录与 Trace），响应体走框架的统一
// 错误体。要别的语义，就在注册该路径的 `GET`/`POST` **之前**自己注册 `OPTIONS /具体路径`
// （见下）。
//
// 三条边界写在这里，免得踩：
//
//   - **不注册兜底 `OPTIONS /{path...}`**。那种写法看着更省事，但 `{path...}` 匹配任意
//     路径，ServeMux 会把「路径匹配、方法不匹配」判成 405——**全站未匹配的请求都会从
//     404 变成 405**（实测：注册兜底之后 `GET /nope` 返回 405）。所以按路径逐条补。
//   - **预检被拒是 403，不是静默少几个头**。来源 / 方法 / 请求头任一不在白名单，返回
//     `403` + 框架统一错误体（`cors_origin_not_allowed` 等），于是**拒绝对浏览器与监控
//     都看得见**（`error.type` 会进访问记录），而不是像生态件那样静默中止。
//   - **自己注册 `OPTIONS` 要趁早**：该路径已经补过自动 OPTIONS 之后再注册同路径的
//     `OPTIONS`，会因重复注册 panic（消息里写了怎么办）；写在**注册该路径方法路由之前**
//     就不会补，那条路径的 OPTIONS 语义归你——但**要让它也在 `app.CORS` 之后注册**，
//     否则 CORS 中间件不在它的链上，预检不会被子框架处理。
//
// 预检按路径无关处理（不检查目标路由是否存在）——这是 CORS 中间件的通行语义；真请求
// 到不到得了那条路由是后话。没匹配到任何路由的请求（真 404）不进洋葱，那条响应不会带
// CORS 头，这是框架「不搬动路由」的既有边界（`TestCORSUnmatchedRouteHasNoCORSHeaders`）。
//
// 补 `OPTIONS` 只发生在**带方法**的路由上（`observe` 里 `method == ""` 那条早退）。
// `Static` 挂的 `"<prefix>/"` 不带方法，本来就吃得住 `OPTIONS`，预检照常由中间件在洋葱
// 内答掉、照常进访问记录——那条路径不需要、也不会被补一条同名 `OPTIONS`（补了会遮蔽
// FileServer 自己的 404/405 语义）。见 `TestCORSPreflightOnStaticPrefix`。
func (e *Engine) CORS(opts ...CORSOption) {
	if e.cors != nil {
		panic("web: 这个引擎已经挂过 CORS 了")
	}
	cfg := &corsConfig{
		origins:     map[string]struct{}{},
		methods:     map[string]struct{}{http.MethodGet: {}, http.MethodPost: {}, http.MethodHead: {}},
		requestHdrs: map[string]struct{}{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	if !cfg.originsAll && len(cfg.origins) == 0 {
		panic("web: CORS 至少要一个来源——CORSAllowOrigins(\"https://app.example.com\")")
	}
	if cfg.originsAll && cfg.credentials {
		panic("web: CORSAllowOrigins(\"*\") 与 CORSAllowCredentials() 不能同时用——" +
			"那个组合等于允许任何网站带凭据访问；要开放给多个站点请逐个列出")
	}

	e.cors = &corsRoutes{paths: map[string]*corsPath{}}
	e.Use(corsMiddleware(cfg))
}

// corsRoutes 是挂了 CORS 之后、引擎为了「预检进得了洋葱」记的一张路径表。
//
// 为什么按路径逐条补 OPTIONS、而不是注册一条兜底 `OPTIONS /{path...}`：后者会把全站
// 未匹配的请求从 404 变成 405（见 CORS 的 godoc）。
type corsRoutes struct {
	paths map[string]*corsPath
}

// corsPath 是一条已注册路径上的状态。
type corsPath struct {
	methods     map[string]struct{} // 注册过的方法（GET 会带出 HEAD，用于 Allow）
	autoDone    bool                // 自动 OPTIONS 已经补过
	userOptions bool                // 用户自己注册了 OPTIONS：不再补
}

// observe 在每次注册路由时被调用（仅当引擎挂了 CORS）：记下方法，必要时补一条 OPTIONS。
func (rs *corsRoutes) observe(e *Engine, pattern string, mw []Middleware) {
	method, path := splitPattern(pattern)
	if path == "" {
		return
	}
	if method == http.MethodOptions {
		p := rs.get(path)
		if p.autoDone {
			panic("web: " + path + " 上的 OPTIONS 已经被 app.CORS 自动补过（它把预检送进洋葱用）。" +
				"要自定义这条路径的 OPTIONS 语义，请把 app.OPTIONS(\"" + path + "\", ...) " +
				"写在注册该路径的 GET/POST **之前**")
		}
		p.userOptions = true
		return
	}
	if method == "" {
		// 不带方法的模式（Static 注册的 "<prefix>/"）：不动它，今天的 OPTIONS 行为原样保留。
		return
	}

	p := rs.get(path)
	p.methods[method] = struct{}{}
	if method == http.MethodGet {
		p.methods[http.MethodHead] = struct{}{} // GET 模式天然吃 HEAD，Allow 里要如实列出
	}
	if p.autoDone || p.userOptions {
		return
	}
	p.autoDone = true
	// 链与触发它的那条路由相同：全局与分组中间件照常参与（CORS 中间件就在里面），
	// 预检因此进得了洋葱、进得了访问记录。
	e.handle(http.MethodOptions+" "+path, compose(e.chainMW(mw), p.bareOptions()))
}

func (rs *corsRoutes) get(path string) *corsPath {
	p := rs.paths[path]
	if p == nil {
		p = &corsPath{methods: map[string]struct{}{}}
		rs.paths[path] = p
	}
	return p
}

// bareOptions 是裸 OPTIONS（不是预检）落到自动补的那条路由上时的回答：与 ServeMux
// 今天给的答案一致——405 + Allow，方法列表按**当下**注册过的方法现算（后面再给这条
// 路径加方法，这里跟着变）。
func (p *corsPath) bareOptions() Handler {
	return func(c *Ctx) error {
		if allow := p.allow(); allow != "" {
			c.SetHeader("Allow", allow)
		}
		return &HTTPError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed"}
	}
}

func (p *corsPath) allow() string {
	out := make([]string, 0, len(p.methods))
	for m := range p.methods {
		out = append(out, m)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// splitPattern 把 "GET /api/users" 拆成方法与前缀已拼好的路径；不带方法的模式
// （Static 那种）方法为空。
func splitPattern(pattern string) (method, path string) {
	i := strings.IndexByte(pattern, ' ')
	if i < 0 {
		return "", pattern
	}
	return pattern[:i], pattern[i+1:]
}

// corsMiddleware 是 CORS 本体。
//
// 三种走法，判据都在**请求头**上（不看路由，也不看目标资源存不存在）：
//
//	没有 Origin        → 不是跨源请求，原样放行，一个头不加
//	真预检（OPTIONS + Access-Control-Request-Method）→ 校验来源/方法/请求头，当场 204
//	其余带 Origin      → 校验来源，加响应头后放行（拒绝时不加头，交给浏览器自己挡）
func corsMiddleware(cfg *corsConfig) Middleware {
	return func(c *Ctx, next Handler) error {
		req := c.Request()
		origin := req.Header.Get("Origin")
		if origin == "" {
			return next(c)
		}
		preflight := req.Method == http.MethodOptions && req.Header.Get("Access-Control-Request-Method") != ""

		allowed := cfg.originAllowed(origin)
		if preflight {
			// 预检的响应与来源绑死，缓存必须按来源分桶。
			h := c.Writer().Header()
			h.Add("Vary", "Origin")
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			if !allowed {
				return Forbidden("cors_origin_not_allowed", nil)
			}
			method := req.Header.Get("Access-Control-Request-Method")
			if !cfg.methodAllowed(method) {
				return Forbidden("cors_method_not_allowed", errUnlisted("method", method))
			}
			requested := parseHeaderList(req.Header.Values("Access-Control-Request-Headers"))
			if !cfg.headersAllowed(requested) {
				return Forbidden("cors_header_not_allowed", errUnlisted("header", strings.Join(requested, ", ")))
			}
			if cfg.credentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			// 回显**本次请求**要的那一个方法与那几个头：Fetch 标准允许只回答被问到的，
			// 这样响应体最小，也不会把没被问到的能力暴露出去。
			h.Set("Access-Control-Allow-Methods", strings.ToUpper(method))
			if len(requested) > 0 {
				h.Set("Access-Control-Allow-Headers", strings.Join(requested, ", "))
			}
			if cfg.maxAge > 0 {
				h.Set("Access-Control-Max-Age", strconv.Itoa(cfg.maxAge))
			}
			h.Set("Access-Control-Allow-Origin", cfg.allowOriginValue(origin))
			return c.NoContent(http.StatusNoContent)
		}

		// 实际请求：无论允许与否都要 Vary: Origin——响应内容随来源而变，缓存不能串桶。
		h := c.Writer().Header()
		h.Add("Vary", "Origin")
		if !allowed {
			// 不加 CORS 头就交给下游：浏览器自己会挡住响应，服务端不必替它做决定。
			return next(c)
		}
		h.Set("Access-Control-Allow-Origin", cfg.allowOriginValue(origin))
		if cfg.credentials {
			h.Set("Access-Control-Allow-Credentials", "true")
		}
		if cfg.exposeHdrs != "" {
			h.Set("Access-Control-Expose-Headers", cfg.exposeHdrs)
		}
		return next(c)
	}
}

func (cfg *corsConfig) originAllowed(origin string) bool {
	if cfg.originsAll {
		return true
	}
	_, ok := cfg.origins[strings.ToLower(strings.TrimRight(origin, "/"))]
	return ok
}

func (cfg *corsConfig) methodAllowed(method string) bool {
	if cfg.methodsAll {
		return true
	}
	if method == "" {
		return false
	}
	_, ok := cfg.methods[strings.ToUpper(method)]
	return ok
}

func (cfg *corsConfig) headersAllowed(requested []string) bool {
	if cfg.requestHdrsAll || len(requested) == 0 {
		return true
	}
	for _, h := range requested {
		if _, ok := cfg.requestHdrs[h]; !ok {
			return false
		}
	}
	return true
}

// allowOriginValue 决定回显什么：配了通配且不携凭据时可以是 `*`（可缓存、覆盖面最大），
// 其余情况一律回显请求里的那个来源——携凭据时规范明确禁止 `*`。
func (cfg *corsConfig) allowOriginValue(origin string) string {
	if cfg.originsAll && !cfg.credentials {
		return "*"
	}
	return origin
}

// parseHeaderList 把 `Access-Control-Request-Headers` 拆成一串规范化的小写头名：
// 允许值分在多行、逗号两侧带空白（Fetch 标准要求已排序且小写，但手写客户端经常不是，
// 这里宽容处理），重复的去掉，顺序保持原样。
func parseHeaderList(values []string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}

// errUnlisted 造一个「哪个值不在白名单里」的 cause。cause 只进观测记录、绝不进响应体
// （见 HTTPError 的说明）：响应里的 code 保持稳定、低基数，具体值留给排查时翻日志。
func errUnlisted(kind, value string) error {
	return fmt.Errorf("cors: %s %q is not in the allow list", kind, value)
}
