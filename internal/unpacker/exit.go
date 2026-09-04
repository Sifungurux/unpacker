package unpacker

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// Exit codes. A scheduled monitor wants to treat "the tag does not exist" and
// "the signature was refused" differently, and parsing stderr to tell them
// apart breaks the first time a message is reworded.
//
// 0 and 1 keep their meanings, so anything that only checks for non-zero is
// unaffected.
const (
	ExitOK           = 0
	ExitFailure      = 1 // anything not classified below
	ExitNotFound     = 2 // no such repository, tag or digest
	ExitAuth         = 3 // credentials missing, wrong, or insufficient
	ExitVerification = 4 // signature verification was requested and refused
	ExitLimit        = 5 // a --max-* limit was exceeded
)

// Sentinels for the two failures unpacker raises itself. Registry failures are
// classified from the library errors instead, below.
var (
	ErrLimit        = errors.New("limit exceeded")
	ErrVerification = errors.New("verification failed")
)

// limitErrorf builds a limit error whose message is exactly what it always
// was. The sentinel travels alongside rather than in the text, so errors.Is
// works without rewording anything a user or a test already reads.
func limitErrorf(format string, args ...any) error {
	return limitError{msg: fmt.Sprintf(format, args...)}
}

type limitError struct{ msg string }

func (e limitError) Error() string { return e.msg }
func (e limitError) Unwrap() error { return ErrLimit }

// ExitCode maps an error to the process exit status.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrVerification):
		return ExitVerification
	case errors.Is(err, ErrLimit):
		return ExitLimit
	case isNotFound(err):
		return ExitNotFound
	case isAuth(err):
		return ExitAuth
	default:
		return ExitFailure
	}
}

// isNotFound and isAuth read both registry clients, because which one handled
// the pull depends on the manifest shape rather than on anything the caller
// chose. Each reports its own error type, so both are consulted.
func isNotFound(err error) bool {
	return errors.Is(err, errdef.ErrNotFound) || hasStatus(err, http.StatusNotFound)
}

func isAuth(err error) bool {
	return hasStatus(err, http.StatusUnauthorized, http.StatusForbidden)
}

func hasStatus(err error, codes ...int) bool {
	var status int
	var craneErr *transport.Error
	var orasErr *errcode.ErrorResponse
	switch {
	case errors.As(err, &craneErr):
		status = craneErr.StatusCode
	case errors.As(err, &orasErr):
		status = orasErr.StatusCode
	default:
		return false
	}
	for _, c := range codes {
		if status == c {
			return true
		}
	}
	return false
}
