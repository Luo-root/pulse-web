package web

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

// multipartMemoryLimit 是 multipart 解析的内存阈值：阈值内保留在内存，
// 超出部分落临时文件（net/http 在请求结束时回收）。
// 请求体**总量**上限由 WithMaxBodyBytes 的读取闸门承担，与本阈值分层无关。
const multipartMemoryLimit = 32 << 20

// maxFormSize 是 urlencoded body 的解析上限，与 net/http.parsePostForm 同值。
// stdlib 对 form 有这道隐式默认闸；本包用显式实现对齐它——既保证两条
// form 路径（有 / 无 Content-Type）闸门一致，又产出可识别的
// *http.MaxBytesError（stdlib 超限时给的是无法分类的明文错误）。
const maxFormSize = 10 << 20

// Bind 把请求体解析进 v（必须是非 nil 指针），按 Content-Type 分派：
//
//	application/json（含 +json 后缀）      encoding/json
//	application/xml / text/xml（含 +xml）  encoding/xml
//	application/x-www-form-urlencoded      表单映射（规则见下）
//	multipart/form-data                    表单 + 文件字段映射
//	无 Content-Type 且 body 非空           按 form-urlencoded 处理
//	无 body（GET / HEAD 等）               落到 query 绑定（等价 BindQuery）
//	其他类型                               415 unsupported_media_type
//
// 目标形状：JSON / XML 走 stdlib Decoder，可解到任意合法目标（struct、
// *[]T、*map）；form / multipart / query 走字段映射，必须指向 struct
// （否则 500 bind_target——编程错误，与客户端输入错误区分）。
//
// 映射规则（form / multipart / query 共用）：
//
//   - tag：form:"name"（urlencoded / multipart）、query:"name"；无 tag 时用
//     字段名匹配（精确优先，其次大小写不敏感）
//   - 类型：string、bool、int / uint 全系、float32 / 64，以及它们的 slice（多值，
//     如 ?ids=1&ids=2 → []int）与指针字段（自动分配）；multipart 额外支持
//     *multipart.FileHeader 及其 slice
//   - 不做嵌套结构、time.Time、值校验（范围说明见 #32）
//
// 解析闸门：urlencoded body 与 stdlib 同口径限 10 MiB（超限 413）；其余
// 大小由 WithMaxBodyBytes 决定（默认不限）。
//
// 失败返回 *HTTPError：400 invalid_body（query 来源为 invalid_query）/
// 413 body_too_large（超过上限）/ 415 unsupported_media_type。
// 因此直接 `return c.Bind(&in)` 即可获得统一错误响应：
//
//	var in CreateUser
//	if err := c.Bind(&in); err != nil {
//		return err
//	}
func (c *Ctx) Bind(v any) error {
	if err := bindTarget(v); err != nil {
		return err
	}

	mt := mediaType(c.r.Header.Get("Content-Type"))

	// 无 body（典型 GET / HEAD）：落到 query
	if mt == "" && c.r.ContentLength == 0 {
		return mapValues(v, c.r.URL.Query(), "query", "invalid_query")
	}

	switch {
	case isJSONType(mt):
		return bindDecode(v, json.NewDecoder(c.r.Body), "invalid_body")
	case isXMLType(mt):
		return bindDecode(v, xml.NewDecoder(c.r.Body), "invalid_body")
	case mt == "application/x-www-form-urlencoded", mt == "":
		// 无 Content-Type 但有 body 也按 form 处理（对齐 gin 的缺省口径）
		return c.bindURLEncoded(v)
	case mt == "multipart/form-data":
		return c.bindMultipart(v)
	default:
		return &HTTPError{Status: http.StatusUnsupportedMediaType, Code: "unsupported_media_type"}
	}
}

// BindQuery 把 query 参数解析进 v（必须是非 nil 指针，指向 struct）。
// GET / 过滤场景用；与 Bind 的 query 落点共用同一实现。
func (c *Ctx) BindQuery(v any) error {
	if err := bindTarget(v); err != nil {
		return err
	}
	return mapValues(v, c.r.URL.Query(), "query", "invalid_query")
}

// ---- 各来源的解析 ----

type bodyDecoder interface{ Decode(v any) error }

func bindDecode(v any, d bodyDecoder, code string) error {
	if err := d.Decode(v); err != nil {
		return bindError(err, code)
	}
	// JSON 拓尾（第一个值之后还有内容）判失败：json.Decoder 默认只消费第一个
	// 值、静默丢弃其余，而 json.Unmarshal 对多余 token 报错——这里对齐严格
	// 口径，拼错的客户端（重复序列化）立即暴露，而不是 200 半绑。
	if jd, ok := d.(*json.Decoder); ok && jd.More() {
		return &HTTPError{
			Status: http.StatusBadRequest,
			Code:   code,
			cause:  errors.New("web: trailing data after first JSON value"),
		}
	}
	return nil
}

// bindURLEncoded 解析 urlencoded body（有 / 无 Content-Type 两条公开路径共用）。
//
// 不用 stdlib 的 ParseForm：它对空 Content-Type 不解析 body（当成
// octet-stream），且对 >10 MiB 的 body 产出明文错误（errors.New("http: POST
// too large")）无法分类。这里与 parsePostForm 对齐同一道 10 MiB 闸，超限
// 统一产出 *http.MaxBytesError（bindError 与自定义 mapper 都认）。
// 解析结果写回 r.PostForm，与 ParseForm 的缓存语义对齐（重复调用幂等、
// 后续自读 PostForm 的代码也能拿到值）。
func (c *Ctx) bindURLEncoded(v any) error {
	if c.r.PostForm == nil {
		b, err := io.ReadAll(io.LimitReader(c.r.Body, maxFormSize+1))
		if err != nil {
			return bindError(err, "invalid_body")
		}
		if int64(len(b)) > maxFormSize {
			return bindError(&http.MaxBytesError{Limit: maxFormSize}, "invalid_body")
		}
		vs, err := url.ParseQuery(string(b))
		if err != nil {
			return bindError(err, "invalid_body")
		}
		c.r.PostForm = vs
	}
	return mapValues(v, c.r.PostForm, "form", "invalid_body")
}

func (c *Ctx) bindMultipart(v any) error {
	if err := c.r.ParseMultipartForm(multipartMemoryLimit); err != nil {
		return bindError(err, "invalid_body")
	}
	mf := c.r.MultipartForm
	if err := mapValues(v, mf.Value, "form", "invalid_body"); err != nil {
		return err
	}
	return mapFiles(v, mf.File, "invalid_body")
}

// ---- 错误分类 ----

// bindError 把解析失败分类：超限优先（MaxBytesReader 的读错误会出现在任意
// 解析路径上），其余归为客户端输入错误。
func bindError(err error, code string) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return &HTTPError{Status: http.StatusRequestEntityTooLarge, Code: "body_too_large", cause: err}
	}
	return &HTTPError{Status: http.StatusBadRequest, Code: code, cause: err}
}

// bindTarget 校验绑定目标：必须是非 nil 指针——这是编程错误（会 100% 在开发期
// 暴露），用 500 与客户端输入错误区分开。
func bindTarget(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return Internal("bind_target", errors.New("bind target must be a non-nil pointer"))
	}
	return nil
}

// ---- 映射器（form / multipart / query 共用）----

// structTarget 取 *struct 的元素值；其他形状是编程错误（500）。
func structTarget(v any) (reflect.Value, error) {
	elem := reflect.ValueOf(v).Elem()
	if elem.Kind() != reflect.Struct {
		return reflect.Value{}, Internal("bind_target", errors.New("form / query binding requires a pointer to struct"))
	}
	return elem, nil
}

// mapValues 把 url.Values 映射进 struct 字段（标量 / slice / 指针，规则见 Bind godoc）。
func mapValues(v any, values url.Values, tagKey, code string) error {
	target, err := structTarget(v)
	if err != nil {
		return err
	}
	t := target.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		rv := target.Field(i)
		if field.PkgPath != "" || !rv.CanSet() {
			continue // 未导出
		}
		name, ok := fieldName(field, tagKey)
		if !ok {
			continue
		}
		vs := lookupValues(values, name)
		if len(vs) == 0 {
			continue
		}
		if !supportedField(rv.Type()) {
			continue // 不支持的字段类型：跳过（不报错）
		}
		if err := setField(rv, vs); err != nil {
			return bindError(err, code)
		}
	}
	return nil
}

// mapFiles 映射 multipart 文件字段：*multipart.FileHeader 与 []*multipart.FileHeader。
func mapFiles(v any, files map[string][]*multipart.FileHeader, code string) error {
	if len(files) == 0 {
		return nil
	}
	target, err := structTarget(v)
	if err != nil {
		return err
	}
	t := target.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		rv := target.Field(i)
		if field.PkgPath != "" || !rv.CanSet() {
			continue
		}
		name, ok := fieldName(field, "form")
		if !ok {
			continue
		}
		fs := lookupFileHeaders(files, name)
		if len(fs) == 0 {
			continue
		}
		switch {
		case rv.Type() == fileHeaderPtrType:
			rv.Set(reflect.ValueOf(fs[0]))
		case rv.Type() == fileHeaderSliceType:
			rv.Set(reflect.ValueOf(fs))
		}
	}
	return nil
}

var (
	fileHeaderPtrType   = reflect.TypeOf((*multipart.FileHeader)(nil))
	fileHeaderSliceType = reflect.TypeOf([]*multipart.FileHeader(nil))
)

// fieldName 解析字段的绑定名：tag 优先（"-" 表示跳过），否则用字段名。
func fieldName(field reflect.StructField, tagKey string) (string, bool) {
	if tag, ok := field.Tag.Lookup(tagKey); ok {
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			return "", false
		}
		if name != "" {
			return name, true
		}
	}
	return field.Name, true
}

// lookupValues 先精确匹配、再大小写不敏感匹配。
func lookupValues(values url.Values, name string) []string {
	if vs, ok := values[name]; ok {
		return vs
	}
	for k, vs := range values {
		if strings.EqualFold(k, name) {
			return vs
		}
	}
	return nil
}

func lookupFileHeaders(files map[string][]*multipart.FileHeader, name string) []*multipart.FileHeader {
	if fs, ok := files[name]; ok {
		return fs
	}
	for k, fs := range files {
		if strings.EqualFold(k, name) {
			return fs
		}
	}
	return nil
}

// supportedField 报告该字段类型是否可以承载 form / query 值。
func supportedField(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Slice, reflect.Pointer:
		return supportedScalar(t.Elem().Kind())
	default:
		return supportedScalar(t.Kind())
	}
}

func supportedScalar(k reflect.Kind) bool {
	switch k {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// setField 把多值字符串写进字段（slice 取全部、其余取第一个）。
func setField(rv reflect.Value, vs []string) error {
	switch rv.Kind() {
	case reflect.Slice:
		slice := reflect.MakeSlice(rv.Type(), len(vs), len(vs))
		for i, raw := range vs {
			if err := setScalar(slice.Index(i), raw); err != nil {
				return err
			}
		}
		rv.Set(slice)
		return nil
	case reflect.Pointer:
		if rv.IsNil() {
			rv.Set(reflect.New(rv.Type().Elem()))
		}
		return setScalar(rv.Elem(), vs[0])
	default:
		return setScalar(rv, vs[0])
	}
}

// setScalar 把单个字符串按目标的 Kind 转换写入。
func setScalar(rv reflect.Value, raw string) error {
	switch rv.Kind() {
	case reflect.String:
		rv.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		rv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, rv.Type().Bits())
		if err != nil {
			return err
		}
		rv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, rv.Type().Bits())
		if err != nil {
			return err
		}
		rv.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, rv.Type().Bits())
		if err != nil {
			return err
		}
		rv.SetFloat(f)
	}
	return nil
}

// mediaType 取规范化的媒体类型（小写、去参数）；解析失败时做容错降级。
func mediaType(ct string) string {
	if ct == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = ct[:i]
		}
		mt = strings.TrimSpace(ct)
	}
	return strings.ToLower(mt)
}

func isJSONType(mt string) bool {
	return mt == "application/json" || mt == "text/json" || strings.HasSuffix(mt, "+json")
}

func isXMLType(mt string) bool {
	return mt == "application/xml" || mt == "text/xml" || strings.HasSuffix(mt, "+xml")
}
