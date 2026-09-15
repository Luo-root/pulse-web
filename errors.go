package web

import (
	"errors"
	"fmt"
	"net/http"
)

// HTTPError 是携带 HTTP 语义的错误：handler 返回它，框架据此映射状态码。
//
// cause 不导出——要携带原始错误请用构造器；cause 只进观测记录，绝不进响应体。
type HTTPError struct {
	Status  int    // HTTP 状态码
	Code    string // 机器可读业务码（会出现在响应体）
	Message string // 人读消息；为空时取状态码标准文案
	cause   error  // 原始错误（可经 errors.Unwrap 追溯）
}

// Error 实现 error。文本用于日志，不直接进响应体。
func (e *HTTPError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return e.Code + ": " + msg
	}
	return msg
}

// Unwrap 让 errors.Is / errors.As 能追溯到 cause。
func (e *HTTPError) Unwrap() error { return e.cause }

// StatusCode 实现 StatusCoder。
func (e *HTTPError) StatusCode() int { return e.Status }

// StatusCoder 让自定义错误类型参与状态码映射：非 *HTTPError 但实现了本接口的
// error 用其状态码，响应消息仍走默认文案（不泄露内部细节）。
type StatusCoder interface {
	error
	StatusCode() int
}

// PanicError 包装 handler panic（由 Engine 兜底 recover 产生）。
// Stack 只进观测记录，绝不进响应体。
type PanicError struct {
	Value any
	Stack []byte
}

// Error 实现 error。
func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// 内置构造器。cause 可传 nil；它只进观测记录。
func BadRequest(code string, cause error) *HTTPError {
	return &HTTPError{Status: http.StatusBadRequest, Code: code, cause: cause}
}
func Unauthorized(code string, cause error) *HTTPError {
	return &HTTPError{Status: http.StatusUnauthorized, Code: code, cause: cause}
}
func Forbidden(code string, cause error) *HTTPError {
	return &HTTPError{Status: http.StatusForbidden, Code: code, cause: cause}
}
func NotFound(code string, cause error) *HTTPError {
	return &HTTPError{Status: http.StatusNotFound, Code: code, cause: cause}
}
func Conflict(code string, cause error) *HTTPError {
	return &HTTPError{Status: http.StatusConflict, Code: code, cause: cause}
}
func TooLarge(code string, cause error) *HTTPError {
	return &HTTPError{Status: http.StatusRequestEntityTooLarge, Code: code, cause: cause}
}
func Internal(code string, cause error) *HTTPError {
	return &HTTPError{Status: http.StatusInternalServerError, Code: code, cause: cause}
}

// ErrorHandler 把 handler 链的错误映射为响应。
//
// 参数 err 是原始 error（只读，框架不改写）；返回值表示**映射器自身的失败**
// （如写响应出错），它不会覆盖原始 error——响应已写出时只进框架内部日志。
type ErrorHandler func(c *Ctx, err error) error

// errorPayload 是默认错误响应体：{"error":{"code":"...","message":"..."}}。
type errorPayload struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// defaultErrorHandler 是默认映射器：HTTPError / StatusCoder 用其状态码；
// 请求体超限（*http.MaxBytesError）归 413；panic 与其余普通 error 一律 500 +
// 通用文案（内部细节只进观测记录）。
func defaultErrorHandler(c *Ctx, err error) error {
	status := http.StatusInternalServerError
	code := "internal"
	message := http.StatusText(http.StatusInternalServerError)

	var perr *PanicError
	var herr *HTTPError
	var sc StatusCoder
	var maxErr *http.MaxBytesError

	switch {
	case errors.As(err, &perr):
		// panic 一律 500 + 通用文案（栈只进观测记录）。
		// 注意：panic(web.NotFound(...)) 不会返回 404 —— 要 4xx 请 return。
	case errors.As(err, &maxErr):
		// 请求体超限（http.MaxBytesReader）：无论超限发生在 Bind 还是用户
		// 直读 body 的路径上，读错误都是 *http.MaxBytesError，统一归 413。
		status = http.StatusRequestEntityTooLarge
		code = "body_too_large"
		message = http.StatusText(status)
	case errors.As(err, &herr):
		status = herr.Status
		code = herr.Code
		if code == "" {
			code = "error"
		}
		message = herr.Message
		if message == "" {
			message = http.StatusText(herr.Status)
		}
	case errors.As(err, &sc):
		status = sc.StatusCode()
		code = "error"
		message = http.StatusText(status)
	}

	return c.JSON(status, errorPayload{Error: errorDetail{Code: code, Message: message}})
}
