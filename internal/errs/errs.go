// Package errs classifies failures so agents can branch on a stable code
// (JSON "code" field and process exit status) instead of parsing messages.
package errs

import (
	"errors"
	"fmt"
)

// Code is a stable failure class.
type Code string

const (
	Internal     Code = "error"         // anything unclassified
	Usage        Code = "usage"         // bad arguments
	NotFound     Code = "not_found"     // unknown model, instance or engine
	Timeout      Code = "timeout"       // not ready in time
	EngineFailed Code = "engine_failed" // the engine process exited or failed to start
	Conflict     Code = "conflict"      // already running, port taken, entry exists
	Unsupported  Code = "unsupported"   // no engine can serve it, or flag/API not supported
	Capacity     Code = "capacity"      // not enough GPU memory, or a queue or quota is full
	Forbidden    Code = "forbidden"     // the API key lacks the role or model access
)

// Exit maps a code to the process exit status.
var Exit = map[Code]int{
	Internal: 1, Usage: 2, NotFound: 3, Timeout: 4, EngineFailed: 5, Conflict: 6, Unsupported: 7, Capacity: 8, Forbidden: 9,
}

// Error carries a Code.
type Error struct {
	Code Code
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// New returns a classified error.
func New(code Code, format string, a ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, a...)}
}

// Wrap classifies err (nil stays nil).
func Wrap(code Code, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Err: err}
}

// CodeOf returns err's code, Internal when unclassified.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Internal
}
