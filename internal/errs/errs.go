// Package errs defines wam's typed errors. Each error carries a Kind, which
// fixes its exit code, a one-line message, and the command that fixes it.
package errs

import (
	"errors"
	"os"
	"syscall"
)

// Kind classifies an error and determines the process exit code.
type Kind int

// Kinds, in exit-code order.
const (
	Runtime     Kind = iota // 1: runtime or upstream failure
	Usage                   // 2: usage error, or missing input with no terminal
	Auth                    // 3: authentication failure
	NotFound                // 4: target does not exist
	Conflict                // 5: conflict or failed precondition
	Interrupted             // 130: SIGINT
	Terminated              // 143: SIGTERM
)

// ExitCode is the process exit code for k.
func (k Kind) ExitCode() int {
	switch k {
	case Usage:
		return 2
	case Auth:
		return 3
	case NotFound:
		return 4
	case Conflict:
		return 5
	case Interrupted:
		return 130
	case Terminated:
		return 143
	default:
		return 1
	}
}

// String is the default machine-readable code for k.
func (k Kind) String() string {
	switch k {
	case Usage:
		return "usage"
	case Auth:
		return "auth"
	case NotFound:
		return "not_found"
	case Conflict:
		return "conflict"
	case Interrupted, Terminated:
		return "interrupted"
	default:
		return "runtime"
	}
}

// Error is a typed wam error.
type Error struct {
	Kind Kind
	// Code is a stable machine-readable code. Empty means Kind.String().
	Code string
	// Message is one line in plain terms.
	Message string
	// Run is the command that fixes the problem.
	Run string
	// Retryable reports whether running the same command again may succeed.
	Retryable bool
	// Err is the underlying cause, if any.
	Err error
}

// New returns an error of kind k.
func New(k Kind, message, run string) *Error {
	return &Error{Kind: k, Message: message, Run: run}
}

// Wrap returns an error of kind k caused by err.
func Wrap(k Kind, err error, message, run string) *Error {
	return &Error{Kind: k, Message: message, Run: run, Err: err}
}

// Interrupt returns the error for a run stopped by sig.
func Interrupt(sig os.Signal) *Error {
	k := Interrupted
	if sig == syscall.SIGTERM {
		k = Terminated
	}
	return &Error{Kind: k, Message: "interrupted by " + sig.String(), Retryable: true}
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return e.Err }

// CodeOrDefault is e.Code, or the kind's default when unset.
func (e *Error) CodeOrDefault() string {
	if e.Code != "" {
		return e.Code
	}
	return e.Kind.String()
}

// As returns the first *Error in err's chain, if any.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
