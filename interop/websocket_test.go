package interop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse/observability"
	gorillaws "github.com/gorilla/websocket"
)

// 本文件钉住「协议升级接得进来」：生态里两家 websocket 库经框架真跑一次握手 +
// 一条消息往返，并断言访问日志里的事实与实际情况对得上。
//
// 两家拿连接的方式**不同**，这正是两条都要覆盖的理由：
//
//	gorilla/websocket   w.(http.Hijacker) 直接断言；先 hijack，再自己往裸连接写 101
//	coder/websocket     先断言、找不到再沿 Unwrap() 链找；先经 writer 写 101，再 hijack
//
// 前半段决定框架必须让包装器**自己**是 http.Hijacker（只提供 Unwrap 不够——gorilla
// 那条断言不会展开链）。后半段决定访问日志的状态码不能只靠「框架看见了什么」：
// gorilla 那条路上框架什么都看不见，得按 101 补记，否则日志里是 status=0，
// 读起来像「没写响应」。
//
// 协议升级**没有第二个挂载点**可对照（不像中间件可以「洋葱内 vs 外包」），所以判据
// 换成了三条：握手成功、消息往返一致、观测事实与实际情况一致。

const wsEcho = "ping-42"

// eventHTTPRequest 是框架访问日志的事件名（见设计文档「打点入口」）。
const eventHTTPRequest = "http.request"

func newApp() (*web.Engine, *observability.MemorySink) {
	sink := &observability.MemorySink{}
	return web.New(web.WithSink(sink), web.WithHostID("interop")), sink
}

// wsURL 把 httptest 的 http:// 换成 ws://。
func wsURL(srv *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + path
}

// assertUpgradeRecord 断言访问日志对一次升级请求给出的事实：
// 状态码 101、带 connection.hijacked 标记、**不**记响应体积（连接已交出，框架
// 再也观测不到它），路由模板照常是真实的那一条。
func assertUpgradeRecord(t *testing.T, sink *observability.MemorySink, route string) {
	t.Helper()
	rec, attrs := lastRecord(t, sink)

	if rec.Status != "101" {
		t.Fatalf("访问日志 status = %q，want 101（连接已交出：框架看不到状态码时按协议升级补记）", rec.Status)
	}
	if got, ok := attrs["connection.hijacked"]; !ok || got != true {
		t.Fatalf("connection.hijacked = %v（ok=%v），want true", got, ok)
	}
	if v, ok := attrs["http.response.body.size"]; ok {
		t.Fatalf("连接已交出，不该记响应体积，得到 %v（记 0 会被读成「响应是空的」）", v)
	}
	if attrs["http.route"] != route {
		t.Fatalf("http.route = %v，want %q", attrs["http.route"], route)
	}
}

// lastRecord 等到访问日志落盘再返回（收尾在 handler 返回之后，客户端拿到回包时
// 它可能还没写）。不取「最后一条」而是按事件名找：sink 里还躺着启动期记录。
func lastRecord(t *testing.T, sink *observability.MemorySink) (observability.Record, map[string]any) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var seen []string
	for time.Now().Before(deadline) {
		seen = seen[:0]
		for _, rec := range sink.Snapshot() {
			if rec.Event == eventHTTPRequest {
				attrs := map[string]any{}
				rec.Attrs.Range(func(k string, v any) { attrs[k] = v })
				return rec, attrs
			}
			seen = append(seen, rec.Event+"/"+rec.Status)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("没等到访问日志（%s）；sink 里的记录：%v", eventHTTPRequest, seen)
	return observability.Record{}, nil
}

// TestGorillaWebSocketUpgrade：断言型取连接的那家。它先 hijack，再自己往裸连接写
// 101 —— 所以框架侧看到的状态码是空的，日志必须补记。
func TestGorillaWebSocketUpgrade(t *testing.T) {
	app, sink := newApp()

	up := gorillaws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var upgradeErr error
	app.GET("/ws", func(c *web.Ctx) error {
		conn, err := up.Upgrade(c.Writer(), c.Request(), nil)
		if err != nil {
			// 拿不到连接时 gorilla 已经把 500 写出来了；留个凭证让断言好读。
			upgradeErr = err
			return nil
		}
		defer func() { _ = conn.Close() }()
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return nil
		}
		return conn.WriteMessage(mt, data)
	})

	srv := httptest.NewServer(app)
	defer srv.Close()

	conn, resp, err := gorillaws.DefaultDialer.Dial(wsURL(srv, "/ws"), nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("握手失败：status=%d err=%v（handler 侧 upgradeErr=%v）", status, err, upgradeErr)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.WriteMessage(gorillaws.TextMessage, []byte(wsEcho)); err != nil {
		t.Fatalf("写消息失败：%v", err)
	}
	_, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读消息失败：%v", err)
	}
	if string(got) != wsEcho {
		t.Fatalf("回显 = %q，want %q", got, wsEcho)
	}

	assertUpgradeRecord(t, sink, "/ws")
}

// TestCoderWebSocketUpgrade：沿 Unwrap() 链找连接的那家。它先经 writer 写 101 再
// hijack —— 框架**看得见**状态码，日志不该靠补记。
func TestCoderWebSocketUpgrade(t *testing.T) {
	app, sink := newApp()

	var acceptErr error
	app.GET("/ws", func(c *web.Ctx) error {
		conn, err := websocket.Accept(c.Writer(), c.Request(), &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			acceptErr = err
			return nil
		}
		defer conn.CloseNow()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return nil
		}
		return conn.Write(ctx, typ, data)
	})

	srv := httptest.NewServer(app)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL(srv, "/ws"), nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("握手失败：status=%d err=%v（handler 侧 acceptErr=%v）", status, err, acceptErr)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageText, []byte(wsEcho)); err != nil {
		t.Fatalf("写消息失败：%v", err)
	}
	typ, got, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读消息失败：%v", err)
	}
	if typ != websocket.MessageText || string(got) != wsEcho {
		t.Fatalf("回显 = (%v, %q)，want (1, %q)", typ, got, wsEcho)
	}

	assertUpgradeRecord(t, sink, "/ws")
}

// TestUpgradeThroughAdaptMiddleware：升级路由挂在 Adapt 中间件之下。
//
// 这条是回归守卫：Adapt 交给中间件的 writer 是框架采集层外面那一个（adaptProxy），
// 而洋葱内的 handler 手上的 writer 链要穿过它——proxy 不转发 Hijack 时，凡是
// `Use(Adapt(...))` 之后的升级路由都会**成片失效**（gorilla 500 / coder 501）。
// 顺带钉住第二件事：这条路上框架仍然知道连接被交出去了（标记打在同一个 Ctx
// writer 上），日志不会退化成「200 正常响应」。
func TestUpgradeThroughAdaptMiddleware(t *testing.T) {
	app, sink := newApp()

	app.Use(web.Adapt(func(next http.Handler) http.Handler {
		// 纯透传：writer 原样往下传，中间件本身不碰连接。
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		})
	}))

	up := gorillaws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	app.GET("/ws", func(c *web.Ctx) error {
		conn, err := up.Upgrade(c.Writer(), c.Request(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return nil
		}
		return conn.WriteMessage(mt, data)
	})

	srv := httptest.NewServer(app)
	defer srv.Close()

	conn, resp, err := gorillaws.DefaultDialer.Dial(wsURL(srv, "/ws"), nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("Adapt 之下的升级路由握手失败：status=%d err=%v", status, err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.WriteMessage(gorillaws.TextMessage, []byte(wsEcho)); err != nil {
		t.Fatalf("写消息失败：%v", err)
	}
	_, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读消息失败：%v", err)
	}
	if string(got) != wsEcho {
		t.Fatalf("回显 = %q，want %q", got, wsEcho)
	}

	assertUpgradeRecord(t, sink, "/ws")
}
