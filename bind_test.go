package web

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// bindReq 发一个带 Content-Type 的请求；不走 doReq 是因为部分用例要改
// ContentLength（预检路径）。
func bindReq(e *Engine, method, target, contentType string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// errCode 从统一错误响应体里取 code。
func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是统一错误格式：%v（body=%q）", err, rec.Body.String())
	}
	return payload.Error.Code
}

type bindUser struct {
	Name string `json:"name" xml:"name"`
	Age  int    `json:"age" xml:"age"`
}

func TestBindJSON(t *testing.T) {
	e, _ := newTestEngine(t)
	e.POST("/u", func(c *Ctx) error {
		var in bindUser
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})

	rec := bindReq(e, "POST", "/u", "application/json", strings.NewReader(`{"name":"a","age":3}`))
	if rec.Code != 200 {
		t.Fatalf("status = %d，body = %s", rec.Code, rec.Body.String())
	}
	var got bindUser
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "a" || got.Age != 3 {
		t.Fatalf("解析结果 = %+v", got)
	}

	// vendor 后缀的 +json
	rec = bindReq(e, "POST", "/u", "application/vnd.api+json", strings.NewReader(`{"name":"b"}`))
	if rec.Code != 200 {
		t.Fatalf("+json 后缀未识别：status = %d", rec.Code)
	}

	// 坏 JSON
	rec = bindReq(e, "POST", "/u", "application/json", strings.NewReader(`{"name":`))
	if rec.Code != 400 || errCode(t, rec) != "invalid_body" {
		t.Fatalf("坏 JSON：status = %d，code = %s", rec.Code, errCode(t, rec))
	}

	// 类型不符
	rec = bindReq(e, "POST", "/u", "application/json", strings.NewReader(`{"age":"not-a-number"}`))
	if rec.Code != 400 {
		t.Fatalf("类型不符：status = %d", rec.Code)
	}

	// 空 body（声明了 JSON）
	rec = bindReq(e, "POST", "/u", "application/json", nil)
	if rec.Code != 400 || errCode(t, rec) != "invalid_body" {
		t.Fatalf("空 body：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}

func TestBindXML(t *testing.T) {
	e, _ := newTestEngine(t)
	e.POST("/u", func(c *Ctx) error {
		var in bindUser
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})

	rec := bindReq(e, "POST", "/u", "application/xml", strings.NewReader(`<bindUser><name>x</name><age>7</age></bindUser>`))
	if rec.Code != 200 {
		t.Fatalf("status = %d，body = %s", rec.Code, rec.Body.String())
	}
	var got bindUser
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "x" || got.Age != 7 {
		t.Fatalf("解析结果 = %+v", got)
	}

	rec = bindReq(e, "POST", "/u", "text/xml", strings.NewReader(`<bindUser><name>`))
	if rec.Code != 400 || errCode(t, rec) != "invalid_body" {
		t.Fatalf("坏 XML：status = %d，code = %s", rec.Code, errCode(t, rec))
	}

	// XML 的编码声明走 encoding/xml 原生支持；空 body 报 400
	rec = bindReq(e, "POST", "/u", "application/xml", nil)
	if rec.Code != 400 {
		t.Fatalf("空 XML body：status = %d", rec.Code)
	}
}

func TestBindForm(t *testing.T) {
	e, _ := newTestEngine(t)
	type formIn struct {
		Name  string  `form:"name"`
		Age   int     `form:"age"`
		OK    bool    `form:"ok"`
		Score float64 `form:"score"`
		IDs   []int   `form:"ids"`
		Note  *string `form:"note"`
		Skip  string  `form:"-"`
		Mixed string  // 无 tag：用字段名匹配（大小写不敏感）
	}
	e.POST("/f", func(c *Ctx) error {
		var in formIn
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})

	body := url.Values{
		"name":  {"a"},
		"age":   {"42"},
		"ok":    {"true"},
		"score": {"1.5"},
		"ids":   {"1", "2", "3"},
		"note":  {"hi"},
		"Skip":  {"nope"},
		"mixed": {"m"},
	}.Encode()
	rec := bindReq(e, "POST", "/f", "application/x-www-form-urlencoded", strings.NewReader(body))
	if rec.Code != 200 {
		t.Fatalf("status = %d，body = %s", rec.Code, rec.Body.String())
	}
	var got formIn
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "a" || got.Age != 42 || !got.OK || got.Score != 1.5 {
		t.Fatalf("标量字段 = %+v", got)
	}
	if len(got.IDs) != 3 || got.IDs[2] != 3 {
		t.Fatalf("多值 slice = %v", got.IDs)
	}
	if got.Note == nil || *got.Note != "hi" {
		t.Fatalf("指针字段 = %v", got.Note)
	}
	if got.Skip != "" {
		t.Fatalf("form:\"-\" 应跳过，实际 = %q", got.Skip)
	}
	if got.Mixed != "m" {
		t.Fatalf("无 tag 字段未按字段名匹配：%q", got.Mixed)
	}

	// 转换失败 → 400
	rec = bindReq(e, "POST", "/f", "application/x-www-form-urlencoded", strings.NewReader("age=abc"))
	if rec.Code != 400 || errCode(t, rec) != "invalid_body" {
		t.Fatalf("坏数值：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}

func TestBindFormQueryIsNotMerged(t *testing.T) {
	// Bind 的 form 来源只取 body（PostForm），不混入 URL query —— query 有
	// 独立入口 BindQuery，两个来源不隐式合并。
	e, _ := newTestEngine(t)
	e.POST("/f", func(c *Ctx) error {
		var in struct {
			Name string `form:"name"`
		}
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})
	rec := bindReq(e, "POST", "/f?name=from-query", "application/x-www-form-urlencoded", strings.NewReader(""))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "from-query") {
		t.Fatalf("query 不应混入 form 绑定：%s", rec.Body.String())
	}
}

func TestBindMultipart(t *testing.T) {
	e, _ := newTestEngine(t)
	type uploadIn struct {
		Name string                  `form:"name"`
		File *multipart.FileHeader   `form:"file"`
		Docs []*multipart.FileHeader `form:"docs"`
	}
	e.POST("/up", func(c *Ctx) error {
		var in uploadIn
		if err := c.Bind(&in); err != nil {
			return err
		}
		var names []string
		for _, d := range in.Docs {
			names = append(names, d.Filename)
		}
		return c.JSON(200, map[string]any{
			"name": in.Name,
			"file": in.File.Filename,
			"docs": names,
		})
	})

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("name", "a")
	fw, _ := w.CreateFormFile("file", "hello.txt")
	_, _ = fw.Write([]byte("data"))
	for _, n := range []string{"d1.txt", "d2.txt"} {
		dw, _ := w.CreateFormFile("docs", n)
		_, _ = dw.Write([]byte("x"))
	}
	_ = w.Close()

	req := httptest.NewRequest("POST", "/up", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d，body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Name string   `json:"name"`
		File string   `json:"file"`
		Docs []string `json:"docs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "a" || got.File != "hello.txt" || len(got.Docs) != 2 {
		t.Fatalf("multipart 解析 = %+v", got)
	}
}

func TestBindQuery(t *testing.T) {
	e, _ := newTestEngine(t)
	type queryIn struct {
		Limit int    `query:"limit"`
		Q     string `query:"q"`
	}
	e.GET("/q", func(c *Ctx) error {
		var in queryIn
		if err := c.Bind(&in); err != nil { // GET 无 body：Bind 落到 query
			return err
		}
		return c.JSON(200, in)
	})
	e.GET("/qq", func(c *Ctx) error {
		var in queryIn
		if err := c.BindQuery(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})

	for _, path := range []string{"/q?limit=5&q=abc", "/qq?limit=5&q=abc"} {
		rec := bindReq(e, "GET", path, "", nil)
		if rec.Code != 200 {
			t.Fatalf("%s：status = %d，body = %s", path, rec.Code, rec.Body.String())
		}
		var got queryIn
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Limit != 5 || got.Q != "abc" {
			t.Fatalf("%s 解析 = %+v", path, got)
		}
	}

	// 大小写不敏感匹配（tag 与字段名一致口径）
	rec := bindReq(e, "GET", "/qq?LIMIT=9", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"Limit":9`) {
		t.Fatalf("大小写不敏感匹配失败：status = %d，body = %s", rec.Code, rec.Body.String())
	}

	// 转换失败 → 400 invalid_query
	rec = bindReq(e, "GET", "/qq?limit=x", "", nil)
	if rec.Code != 400 || errCode(t, rec) != "invalid_query" {
		t.Fatalf("坏 query：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}

func TestBindUnsupportedMediaType(t *testing.T) {
	e, _ := newTestEngine(t)
	e.POST("/u", func(c *Ctx) error {
		var in bindUser
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})

	rec := bindReq(e, "POST", "/u", "application/octet-stream", strings.NewReader("zzz"))
	if rec.Code != http.StatusUnsupportedMediaType || errCode(t, rec) != "unsupported_media_type" {
		t.Fatalf("status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}

func TestBindNoContentTypeFallsBackToForm(t *testing.T) {
	e, _ := newTestEngine(t)
	e.POST("/f", func(c *Ctx) error {
		var in struct {
			Name string `form:"name"`
		}
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})

	// 无 Content-Type 但有 body：按 form 处理（stdlib ParseForm 对空 CT 不解析
	// body，这一步由框架手工读 body + ParseQuery 补齐）
	rec := bindReq(e, "POST", "/f", "", strings.NewReader("name=v"))
	if rec.Code != 200 {
		t.Fatalf("status = %d，body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "v") {
		t.Fatalf("body 未被解析：%s", rec.Body.String())
	}
}

func TestBindMaxBodyBytes(t *testing.T) {
	e, _ := newTestEngine(t, WithMaxBodyBytes(64))
	e.POST("/u", func(c *Ctx) error {
		var in bindUser
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})

	// ① 小 body 正常
	rec := bindReq(e, "POST", "/u", "application/json", strings.NewReader(`{"name":"a"}`))
	if rec.Code != 200 {
		t.Fatalf("小 body 应通过：status = %d，body = %s", rec.Code, rec.Body.String())
	}

	// ② 读取闸门：ContentLength 未知（-1）时由 MaxBytesReader 掐
	big := `{"name":"` + strings.Repeat("a", 200) + `"}`
	req := httptest.NewRequest("POST", "/u", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1 // 模拟 chunked：跳过预检
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != "body_too_large" {
		t.Fatalf("闸门路径：status = %d，code = %s，body = %s", rec.Code, errCode(t, rec), rec.Body.String())
	}

	// ③ Content-Length 快速失败：声明即超限，不读 body
	req = httptest.NewRequest("POST", "/u", strings.NewReader(`{"name":"a"}`))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 4096
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != "body_too_large" {
		t.Fatalf("预检路径：status = %d，code = %s", rec.Code, errCode(t, rec))
	}

	// ④ 非绑定路径同样受限：用户直接读 body 也撞闸门
	e.POST("/raw", func(c *Ctx) error {
		b, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		return c.Text(200, string(b))
	})
	req = httptest.NewRequest("POST", "/raw", strings.NewReader(big))
	req.ContentLength = -1
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != "body_too_large" {
		t.Fatalf("用户自读路径：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}

func TestBindDefaultNoLimit(t *testing.T) {
	// 不设选项时行为与既有语义一致：无上限
	e, _ := newTestEngine(t)
	e.POST("/u", func(c *Ctx) error {
		var in bindUser
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})
	big := `{"name":"` + strings.Repeat("a", 4096) + `"}`
	rec := bindReq(e, "POST", "/u", "application/json", strings.NewReader(big))
	if rec.Code != 200 {
		t.Fatalf("默认无上限应通过：status = %d", rec.Code)
	}
}

func TestBindUserWrappedMaxBytesReader(t *testing.T) {
	// 框架选项未开、用户自包 MaxBytesReader：Bind 同样把超限分类为 413
	e, _ := newTestEngine(t)
	e.POST("/u", func(c *Ctx) error {
		c.Request().Body = http.MaxBytesReader(c.Writer(), c.Request().Body, 16)
		var in bindUser
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})
	rec := bindReq(e, "POST", "/u", "application/json", strings.NewReader(`{"name":"`+strings.Repeat("a", 100)+`"}`))
	if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != "body_too_large" {
		t.Fatalf("status = %d，code = %s，body = %s", rec.Code, errCode(t, rec), rec.Body.String())
	}
}

func TestBindTargetErrors(t *testing.T) {
	e, _ := newTestEngine(t)
	var notPointer int
	e.POST("/t", func(c *Ctx) error {
		return c.Bind(notPointer) // 编程错误：非指针
	})
	e.POST("/t2", func(c *Ctx) error {
		return c.Bind(&notPointer) // 编程错误：指向非 struct
	})

	rec := bindReq(e, "POST", "/t", "application/json", strings.NewReader(`{}`))
	if rec.Code != http.StatusInternalServerError || errCode(t, rec) != "bind_target" {
		t.Fatalf("非指针：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
	rec = bindReq(e, "POST", "/t2", "application/x-www-form-urlencoded", strings.NewReader("a=1"))
	if rec.Code != http.StatusInternalServerError || errCode(t, rec) != "bind_target" {
		t.Fatalf("非 struct：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}

func TestBindContentTypeWithParameters(t *testing.T) {
	e, _ := newTestEngine(t)
	e.POST("/u", func(c *Ctx) error {
		var in bindUser
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})
	rec := bindReq(e, "POST", "/u", "application/json; charset=utf-8", strings.NewReader(`{"name":"a"}`))
	if rec.Code != 200 {
		t.Fatalf("带参数的 Content-Type 未识别：status = %d，body = %s", rec.Code, rec.Body.String())
	}
	// 解析失败的 Content-Type 走容错降级（截分号前）
	rec = bindReq(e, "POST", "/u", "application/json; broken", strings.NewReader(`{"name":"a"}`))
	if rec.Code != 200 {
		t.Fatalf("容错降级失败：status = %d", rec.Code)
	}
}

func TestBindMoreScalarKinds(t *testing.T) {
	e, _ := newTestEngine(t)
	type kindsIn struct {
		Count uint32   `form:"count"`
		Ratio float32  `form:"ratio"`
		Small int8     `form:"small"`
		IDs   []int    `form:"ids"`
		Inner struct { // 不支持的类型：静默跳过，不报错
			X string
		}
	}
	e.POST("/k", func(c *Ctx) error {
		var in kindsIn
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})

	rec := bindReq(e, "POST", "/k", "application/x-www-form-urlencoded",
		strings.NewReader("count=7&ratio=1.5&small=-3&ids=1&ids=2"))
	if rec.Code != 200 {
		t.Fatalf("status = %d，body = %s", rec.Code, rec.Body.String())
	}
	var got kindsIn
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 7 || got.Ratio != 1.5 || got.Small != -3 || len(got.IDs) != 2 {
		t.Fatalf("解析 = %+v", got)
	}

	// 多值里有一个坏值 → 400
	rec = bindReq(e, "POST", "/k", "application/x-www-form-urlencoded", strings.NewReader("ids=1,x"))
	if rec.Code != 400 || errCode(t, rec) != "invalid_body" {
		t.Fatalf("坏 slice 值：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}

func TestBindMalformedInputs(t *testing.T) {
	e, _ := newTestEngine(t)
	e.POST("/u", func(c *Ctx) error {
		var in bindUser
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})

	// urlencoded（带 CT）里坏 percent 编码：ParseForm 失败 → 400
	rec := bindReq(e, "POST", "/u", "application/x-www-form-urlencoded", strings.NewReader("a=%zz"))
	if rec.Code != 400 || errCode(t, rec) != "invalid_body" {
		t.Fatalf("坏 urlencoded：status = %d，code = %s", rec.Code, errCode(t, rec))
	}

	// 无 CT 的 body 里坏 percent 编码：手工路径 → 400
	rec = bindReq(e, "POST", "/u", "", strings.NewReader("a=%zz"))
	if rec.Code != 400 || errCode(t, rec) != "invalid_body" {
		t.Fatalf("坏裸 form：status = %d，code = %s", rec.Code, errCode(t, rec))
	}

	// multipart 缺 boundary：ParseMultipartForm 失败 → 400
	rec = bindReq(e, "POST", "/u", "multipart/form-data", strings.NewReader("not-a-multipart"))
	if rec.Code != 400 || errCode(t, rec) != "invalid_body" {
		t.Fatalf("坏 multipart：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}

func TestBindJSONSlice(t *testing.T) {
	// JSON / XML 走 stdlib Decoder，目标是 *[]T / *map 同样合法（不限于 struct）
	e, _ := newTestEngine(t)
	e.POST("/us", func(c *Ctx) error {
		var in []bindUser
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})
	rec := bindReq(e, "POST", "/us", "application/json", strings.NewReader(`[{"name":"a"},{"name":"b"}]`))
	if rec.Code != 200 {
		t.Fatalf("status = %d，body = %s", rec.Code, rec.Body.String())
	}
	var got []bindUser
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Name != "b" {
		t.Fatalf("解析 = %+v", got)
	}
}

func TestBindJSONTrailingData(t *testing.T) {
	// 第一个 JSON 值之后还有内容：Decoder 默认静默丢弃，这里判 400
	e, _ := newTestEngine(t)
	e.POST("/u", func(c *Ctx) error {
		var in bindUser
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})
	rec := bindReq(e, "POST", "/u", "application/json", strings.NewReader(`{"name":"a"}{"name":"b"}`))
	if rec.Code != 400 || errCode(t, rec) != "invalid_body" {
		t.Fatalf("拓尾：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}

func TestBindURLEncodedOverLimit(t *testing.T) {
	// 与 stdlib parsePostForm 同口径的 10 MiB 解析闸：有 / 无 Content-Type
	// 两条公开路径都要产出 413 + 可识别的 *http.MaxBytesError，
	// 且不依赖 WithMaxBodyBytes 选项。
	e, _ := newTestEngine(t)
	e.POST("/f", func(c *Ctx) error {
		var in struct {
			Name string `form:"name"`
		}
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})

	big := strings.Repeat("x", maxFormSize+1)
	for _, ct := range []string{"application/x-www-form-urlencoded", ""} {
		rec := bindReq(e, "POST", "/f", ct, strings.NewReader(big))
		if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != "body_too_large" {
			t.Fatalf("ct=%q：status = %d，code = %s", ct, rec.Code, errCode(t, rec))
		}
	}

	// 选项打开且更小时，内层读取闸门先掐——同样是 413
	e2, _ := newTestEngine(t, WithMaxBodyBytes(1<<10))
	e2.POST("/f", func(c *Ctx) error {
		var in struct {
			Name string `form:"name"`
		}
		if err := c.Bind(&in); err != nil {
			return err
		}
		return c.JSON(200, in)
	})
	rec := bindReq(e2, "POST", "/f", "application/x-www-form-urlencoded",
		strings.NewReader(strings.Repeat("x", 2<<10)))
	if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != "body_too_large" {
		t.Fatalf("选项路径：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}

func TestBindQueryTargetError(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/q", func(c *Ctx) error {
		return c.BindQuery("not-a-pointer")
	})
	rec := bindReq(e, "GET", "/q", "", nil)
	if rec.Code != http.StatusInternalServerError || errCode(t, rec) != "bind_target" {
		t.Fatalf("status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}
