// Package errs defines stuntdb's error kinds. Every error that reaches the
// command line carries a Kind, which cmd/stuntdb turns into an exit code.
package errs

import "errors"

// Kind classifies an error by what the user should do about it.
type Kind uint8

const (
	Internal     Kind = iota // a bug in stuntdb; the default for errors without a kind
	Usage                    // bad flags, arguments or configuration
	CheckFailed              // a check found drift, unreviewed columns or unsafe settings
	Connect                  // cannot connect or authenticate, or privileges are missing
	Integrity                // verification found broken references or wrong row counts
	Leak                     // the leak scan found likely personal data in the output
	Output                   // the output cannot be written
	Incompatible             // a strict cross-engine restore would lose information
)

var names = [...]string{
	Internal:     "ERROR",
	Usage:        "USAGE",
	CheckFailed:  "CHECK_FAILED",
	Connect:      "CONNECT",
	Integrity:    "INTEGRITY",
	Leak:         "LEAK",
	Output:       "OUTPUT",
	Incompatible: "INCOMPATIBLE",
}

// String returns the stable name used in JSON output, e.g. "CHECK_FAILED".
func (k Kind) String() string {
	if int(k) < len(names) {
		return names[k]
	}
	return names[Internal]
}

// Error is an error with a Kind.
type Error struct {
	Kind Kind
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

// E attaches kind to err. It returns nil if err is nil.
func E(kind Kind, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Err: err}
}

// KindOf returns the Kind of the first *Error in err's chain, or Internal if there is none.
func KindOf(err error) Kind {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Kind
	}
	return Internal
}
