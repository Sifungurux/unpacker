package unpacker

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

// A pipeline branching on why a run failed must not have to parse stderr,
// which breaks the first time a message is reworded.
func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, ExitOK},
		{"unclassified", errors.New("something else"), ExitFailure},
		{"limit", limitErrorf("archive expands past the %d byte total limit (--max-total-bytes)", 10), ExitLimit},
		{"limit, wrapped by a caller", fmt.Errorf("unpack: %w", limitErrorf("nope (--max-entries)")), ExitLimit},
		{"verification", fmt.Errorf("%w: no signature", ErrVerification), ExitVerification},
		{"not found, oras", fmt.Errorf("fetch manifest: %w", errdef.ErrNotFound), ExitNotFound},
		{"not found, crane", &transport.Error{StatusCode: http.StatusNotFound}, ExitNotFound},
		{"not found, oras response", &errcode.ErrorResponse{StatusCode: http.StatusNotFound}, ExitNotFound},
		{"unauthorized, crane", &transport.Error{StatusCode: http.StatusUnauthorized}, ExitAuth},
		{"forbidden, crane", &transport.Error{StatusCode: http.StatusForbidden}, ExitAuth},
		{"unauthorized, oras response", &errcode.ErrorResponse{StatusCode: http.StatusUnauthorized}, ExitAuth},
		{"other status is not special", &transport.Error{StatusCode: http.StatusInternalServerError}, ExitFailure},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExitCode(c.err); got != c.want {
				t.Errorf("ExitCode = %d, want %d", got, c.want)
			}
		})
	}
}

// The sentinel travels beside the message rather than inside it, so wording a
// user or a test already reads stays exactly as it was.
func TestLimitErrorKeepsItsMessage(t *testing.T) {
	err := limitErrorf("file %s exceeds the %d byte limit for a single file (--max-file-bytes)", "big.bin", 10)
	const want = "file big.bin exceeds the 10 byte limit for a single file (--max-file-bytes)"
	if err.Error() != want {
		t.Errorf("message = %q, want %q", err, want)
	}
	if !errors.Is(err, ErrLimit) {
		t.Error("a limit error must be recognisable as one")
	}
}
