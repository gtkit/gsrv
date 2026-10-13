// Package fault 定义公共治理错误，业务 sentinel 仍由业务 transport 翻译。
package fault

import (
	"context"
	"errors"
)

// Code 是协议无关的治理错误码。
type Code string

// 公共错误码由 HTTP/gRPC 适配器映射到各自状态。
const (
	OK                Code = "OK"
	NotFound          Code = "NOT_FOUND"
	PayloadTooLarge   Code = "PAYLOAD_TOO_LARGE"
	InvalidArgument   Code = "INVALID_ARGUMENT"
	Unauthenticated   Code = "UNAUTHENTICATED"
	PermissionDenied  Code = "PERMISSION_DENIED"
	ResourceExhausted Code = "RESOURCE_EXHAUSTED"
	Unavailable       Code = "UNAVAILABLE"
	DeadlineExceeded  Code = "DEADLINE_EXCEEDED"
	Canceled          Code = "CANCELED"
	Internal          Code = "INTERNAL"
)

// Error 携带允许对外展示的错误文案与可选底层原因。
type Error struct {
	code    Code
	message string
	cause   error
}

// New 构造安全错误，不自动回显底层原因。
func New(code Code, message string) error { return &Error{code: code, message: message} }

// Wrap 保留底层原因供 errors.Is/AsType 使用；message 是显式的安全文案。
func Wrap(code Code, message string, cause error) error {
	return &Error{code: code, message: message, cause: cause}
}

// Error 返回安全文案。
func (e *Error) Error() string { return e.message }

// Unwrap 返回底层原因。
func (e *Error) Unwrap() error { return e.cause }

// CodeOf 将未知错误归为 INTERNAL。
func CodeOf(err error) Code {
	if err == nil {
		return OK
	}
	if errors.Is(err, context.Canceled) {
		return Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return DeadlineExceeded
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e.code
	}
	return Internal
}

// Message 只返回显式的安全文案；未知错误隐藏底层信息。
func Message(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.message
	}
	switch CodeOf(err) {
	case Canceled:
		return "request canceled"
	case DeadlineExceeded:
		return "deadline exceeded"
	default:
		return "internal server error"
	}
}
