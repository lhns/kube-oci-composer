package reconciler

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

func TestIsGoneNeedsTheRegistrysOwnCode(t *testing.T) {
	coded := func(status int, codes ...transport.ErrorCode) error {
		e := &transport.Error{StatusCode: status}
		for _, c := range codes {
			e.Errors = append(e.Errors, transport.Diagnostic{Code: c})
		}
		return e
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"manifest unknown", coded(http.StatusNotFound, transport.ManifestUnknownErrorCode), true},
		{"name unknown", coded(http.StatusNotFound, transport.NameUnknownErrorCode), true},
		{"wrapped", fmt.Errorf("reading: %w", coded(http.StatusNotFound, transport.ManifestUnknownErrorCode)), true},
		{"a bare 404, as a proxy answers", coded(http.StatusNotFound), false},
		{"another code on a 404", coded(http.StatusNotFound, transport.UnauthorizedErrorCode), false},
		{"the code on another status", coded(http.StatusServiceUnavailable, transport.ManifestUnknownErrorCode), false},
		{"not a transport error", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsGone(tc.err); got != tc.want {
				t.Fatalf("IsGone = %v, want %v", got, tc.want)
			}
		})
	}
}
