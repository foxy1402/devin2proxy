package server

import (
	"errors"
	"net/http"
	"testing"

	"devin2proxy/internal/devin"
)

// Which failures are worth another account is the one decision that makes the pool
// useful, so it is pinned here rather than left to the handler's behaviour to
// imply. The case that matters most is the rate limit: a pool exists because the
// allowance is per account, so returning a 429 to the client while a healthy
// account sits idle would defeat it for the failure it was built for.
func TestIsAccountRefusal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"401 is the account's credential", &devin.HTTPError{Status: http.StatusUnauthorized}, true},
		{"403 is the account's credential", &devin.HTTPError{Status: http.StatusForbidden}, true},
		{"429 is this account being out of allowance", &devin.HTTPError{Status: http.StatusTooManyRequests}, true},
		{"unauthenticated has another account to try", &devin.ConnectError{Code: "unauthenticated"}, true},
		{"permission_denied has another account to try", &devin.ConnectError{Code: "permission_denied"}, true},
		{"resource_exhausted has another account to try", &devin.ConnectError{Code: "resource_exhausted"}, true},

		// Every account would answer these the same way, so walking the pool only
		// spends requests to collect the same error.
		{"a backend fault is not the account's", &devin.HTTPError{Status: http.StatusServiceUnavailable}, false},
		{"a backend fault in connect form is not the account's", &devin.ConnectError{Code: "unavailable"}, false},
		{"an internal fault is not the account's", &devin.ConnectError{Code: "internal"}, false},
		{"a malformed request is the client's", &devin.ConnectError{Code: "invalid_argument"}, false},
		{"a failed precondition is the client's", &devin.ConnectError{Code: "failed_precondition"}, false},
		{"a cancelled request is nobody's", errors.New("context canceled"), false},
		{"a route failure is not the account's", &devin.EgressError{Op: "dial", Broken: true, Err: errors.New("refused")}, false},
		{"no error at all is not a refusal", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAccountRefusal(tc.err); got != tc.want {
				t.Fatalf("isAccountRefusal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
