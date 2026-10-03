// Package apperr defines transport-agnostic error kinds returned by the service layer.
package apperr

import "fmt"

type Kind int

const (
	KindInvalid Kind = iota + 1
	KindUnauthorized
	KindForbidden
	KindNotFound
	KindConflict
	KindUnavailable
)

type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

func Invalid(format string, args ...any) error {
	return &Error{Kind: KindInvalid, Message: fmt.Sprintf(format, args...)}
}

func Unauthorized(format string, args ...any) error {
	return &Error{Kind: KindUnauthorized, Message: fmt.Sprintf(format, args...)}
}

func Forbidden(format string, args ...any) error {
	return &Error{Kind: KindForbidden, Message: fmt.Sprintf(format, args...)}
}

func NotFound(format string, args ...any) error {
	return &Error{Kind: KindNotFound, Message: fmt.Sprintf(format, args...)}
}

func Conflict(format string, args ...any) error {
	return &Error{Kind: KindConflict, Message: fmt.Sprintf(format, args...)}
}

func Unavailable(format string, args ...any) error {
	return &Error{Kind: KindUnavailable, Message: fmt.Sprintf(format, args...)}
}
