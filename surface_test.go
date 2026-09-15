package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/kernel"
)

// 公开 API 面的薄包装：路由方法注册器、Ctx 的两条读取入口、服务查找的 miss
// 分支、内置错误构造器与两种 Error 文本。它们此前没有任何测试调用过。

// TestHTTPMethodRegistrars 覆盖除 GET 外的六个路由方法注册器与底层 Handle。
func TestHTTPMethodRegistrars(t *testing.T) {
	e, _ := newTestEngine(t)
	h := func(c *Ctx) error { return c.Text(http.StatusOK, c.Request().Method) }
	e.POST("/m", h)
	e.PUT("/m", h)
	e.PATCH("/m", h)
	e.DELETE("/m", h)
	e.HEAD("/m", h)
	e.OPTIONS("/m", h)
	e.Handle("GET /raw", func(c *Ctx) error { return c.Text(http.StatusOK, "raw") })

	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		rec := doReq(e, m, "/m", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s /m status = %d，want 200", m, rec.Code)
		}
		// HEAD 的 body 语义由 net/http 承担，这里只断状态码
		if m != "HEAD" && rec.Body.String() != m {
			t.Fatalf("%s /m body = %q，want %q", m, rec.Body.String(), m)
		}
	}

	if rec := doReq(e, "GET", "/raw", nil); rec.Code != http.StatusOK || rec.Body.String() != "raw" {
		t.Fatalf("Handle(\"GET /raw\") = %d %q", rec.Code, rec.Body.String())
	}
}

// TestCtxQueryAndSetHeader：查询参数与响应头两条薄包装。
func TestCtxQueryAndSetHeader(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/q", func(c *Ctx) error {
		c.SetHeader("X-Custom", "v1")
		if got := c.Query("name"); got != "jiangnan" {
			return BadRequest("bad_name", nil)
		}
		if got := c.Query("absent"); got != "" {
			return BadRequest("absent_should_be_empty", nil)
		}
		return c.Text(http.StatusOK, "ok")
	})

	rec := doReq(e, "GET", "/q?name=jiangnan", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Custom"); got != "v1" {
		t.Fatalf("X-Custom = %q，want v1", got)
	}
}

// TestServiceLookupMissReturnsFalse：`Ctx.Service` / `Detached.Service` 在未提供
// 时返回 (zero, false) 而不是 panic —— panic 是 MustService 的语义。
func TestServiceLookupMissReturnsFalse(t *testing.T) {
	e, _ := newTestEngine(t)
	missing := kernel.NewServiceKey[string]("test.missing")

	e.GET("/ctx", func(c *Ctx) error {
		if v, ok := c.Service(missing); ok || v != "" {
			return Internal("unexpected_hit", nil)
		}
		return c.Text(http.StatusOK, "absent")
	})
	e.GET("/detached", func(c *Ctx) error {
		if v, ok := c.Detach().Service(missing); ok || v != "" {
			return Internal("unexpected_hit", nil)
		}
		return c.Text(http.StatusOK, "absent")
	})

	for _, p := range []string{"/ctx", "/detached"} {
		if rec := doReq(e, "GET", p, nil); rec.Code != http.StatusOK || rec.Body.String() != "absent" {
			t.Fatalf("%s = %d %q", p, rec.Code, rec.Body.String())
		}
	}
}

// TestErrorConstructorsAndText：六个内置构造器的状态码与 code 落位，以及
// 两种 Error 文本——文本用于日志（code + 状态标准文案），**不含 cause**。
func TestErrorConstructorsAndText(t *testing.T) {
	cases := []struct {
		name string
		err  *HTTPError
		want int
	}{
		{"BadRequest", BadRequest("bad", nil), http.StatusBadRequest},
		{"Unauthorized", Unauthorized("nope", nil), http.StatusUnauthorized},
		{"Forbidden", Forbidden("denied", nil), http.StatusForbidden},
		{"NotFound", NotFound("gone", nil), http.StatusNotFound},
		{"Conflict", Conflict("dup", nil), http.StatusConflict},
		{"Internal", Internal("boom", nil), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.err.Status != c.want {
				t.Fatalf("Status = %d，want %d", c.err.Status, c.want)
			}
			if got := c.err.StatusCode(); got != c.want {
				t.Fatalf("StatusCode() = %d，want %d", got, c.want)
			}
			if c.err.Code == "" {
				t.Fatal("构造器应写入 code")
			}
			if c.err.Message != "" {
				t.Fatal("构造器不设 Message：默认文案取状态码标准文案")
			}
			if got, want := c.err.Error(), c.err.Code+": "+http.StatusText(c.want); got != want {
				t.Fatalf("Error() = %q，want %q", got, want)
			}
		})
	}

	// 没有 Code / Message 时退回状态码标准文案
	if got, want := (&HTTPError{Status: http.StatusTeapot}).Error(), http.StatusText(http.StatusTeapot); got != want {
		t.Fatalf("Error() = %q，want %q", got, want)
	}

	// cause 只进观测记录，但必须能经 errors.Is 追溯
	cause := errors.New("db is down")
	withCause := Internal("db", cause)
	if !errors.Is(withCause, cause) {
		t.Fatal("errors.Is 追不到 cause")
	}
	if strings.Contains(withCause.Error(), "db is down") {
		t.Fatal("Error() 文本不应包含 cause（它只进观测记录）")
	}

	if got := (&PanicError{Value: "kaboom"}).Error(); got != "panic: kaboom" {
		t.Fatalf("PanicError.Error() = %q", got)
	}
}
