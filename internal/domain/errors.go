// Package domain holds entities, value objects and invariants. It must not
// import any I/O package: everything here is pure and unit-testable.
package domain

import "errors"

// Code classifies a failure so transports can map it without string matching.
type Code string

const (
	CodeInvalid     Code = "invalid_input"
	CodeNotFound    Code = "not_found"
	CodeConflict    Code = "conflict"
	CodeUnavailable Code = "unavailable"
	CodeInternal    Code = "internal"
)

// Error is the single error type crossing layer boundaries. It keeps its text
// as a translatable Message instead of an already rendered string.
type Error struct {
	Code Code
	Msg  Message
	Err  error
}

// Error renders only the Message. Every constructor call formats the cause into
// it with %v already, so appending Err here would print the cause twice; Err
// exists for errors.Is and Unwrap, not for the text.
func (e *Error) Error() string { return e.Msg.String() }

func (e *Error) Unwrap() error { return e.Err }

func newError(code Code, format string, args ...any) *Error {
	var wrapped error
	for _, v := range args {
		if err, ok := v.(error); ok {
			wrapped = err
		}
	}
	return &Error{Code: code, Msg: Msg(format, args...), Err: wrapped}
}

func Invalid(format string, args ...any) *Error  { return newError(CodeInvalid, format, args...) }
func NotFound(format string, args ...any) *Error { return newError(CodeNotFound, format, args...) }
func Conflict(format string, args ...any) *Error { return newError(CodeConflict, format, args...) }
func Unavailable(format string, args ...any) *Error {
	return newError(CodeUnavailable, format, args...)
}
func Internal(format string, args ...any) *Error { return newError(CodeInternal, format, args...) }

// CodeOf extracts the classification, defaulting to internal.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// MessageOf extracts the translatable message, falling back to the raw text of
// errors that did not originate here.
func MessageOf(err error) Message {
	if err == nil {
		return Message{}
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Msg
	}
	return Msg(err.Error())
}
