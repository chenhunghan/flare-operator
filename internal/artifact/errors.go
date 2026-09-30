package artifact

import (
	"errors"
	"fmt"
)

// Kind classifies a load failure, so a controller can pick its condition reason and whether to
// retry.
type Kind int

const (
	// KindDependency: a referenced ConfigMap or Secret is missing, not labelled
	// cloudflare.flare.dev/artifact=true, or of the wrong type. It can be fixed without changing
	// the object; retry when the ConfigMap or Secret changes.
	KindDependency Kind = iota + 1
	// KindInvalid: the source in the spec cannot be used (a malformed image reference, URL or
	// path). Only a spec change fixes it.
	KindInvalid
	// KindRejected: the content breaks a limit or a safety rule (size, file count, compression
	// ratio, path traversal, a link escaping the root, a device file, a SHA-256 mismatch).
	KindRejected
	// KindFetch: the registry or server failed, refused the credentials or could not be
	// reached (including a refused address). Usually transient; retry with backoff.
	KindFetch
)

func (k Kind) String() string {
	switch k {
	case KindDependency:
		return "Dependency"
	case KindInvalid:
		return "Invalid"
	case KindRejected:
		return "Rejected"
	case KindFetch:
		return "Fetch"
	}
	return "Unknown"
}

// Error is a classified load failure. Its message is safe to show in an object's status: it
// never includes credentials or file content.
type Error struct {
	Kind Kind
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the Kind of err, or 0 when err is not an *Error (e.g. a Kubernetes API error
// or a cancelled context, which the caller handles as usual).
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

func newErr(k Kind, err error, format string, args ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, args...), Err: err}
}

func dependency(format string, args ...any) *Error {
	return newErr(KindDependency, nil, format, args...)
}
func invalid(format string, args ...any) *Error { return newErr(KindInvalid, nil, format, args...) }
func rejected(format string, args ...any) *Error {
	return newErr(KindRejected, nil, format, args...)
}
func fetchErr(err error, format string, args ...any) *Error {
	return newErr(KindFetch, err, format, args...)
}
