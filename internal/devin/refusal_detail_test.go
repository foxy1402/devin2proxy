package devin

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A deployment logged four accounts stepping out of rotation "after HTTP 403"
// with nothing else to go on. Four healthy keys and one blocked source address
// are indistinguishable in that line, and the operator cannot act on either.
// The upstream's own words travel in the error, so the log quotes them —
// collapsed, bounded, and never invented.

func TestRefusalDetailQuotesTheUpstream(t *testing.T) {
	html := "<html>\n  <head><title>403 Forbidden</title></head>\n  <body>\n  <center>Request blocked by WAF</center>\n  </body>\n</html>"
	detail := refusalDetail(&HTTPError{Status: 403, ContentType: "text/html", Body: []byte(html)})
	if detail == "" {
		t.Fatal("an HTTPError with a body produced no detail")
	}
	if strings.Contains(detail, "\n") {
		t.Errorf("the detail is not collapsed to one line: %q", detail)
	}
	if !strings.Contains(detail, "Request blocked by WAF") {
		t.Errorf("the detail lost the upstream's words: %q", detail)
	}
	if len(detail) > 163 { // 160-byte cap plus the 3-byte ellipsis
		t.Errorf("the detail is %d bytes, longer than the 160-byte cap", len(detail))
	}

	connect := &ConnectError{Code: "permission_denied", Message: "account not entitled to this model"}
	if got := refusalDetail(connect); !strings.Contains(got, "not entitled") {
		t.Errorf("a ConnectError produced %q, want its message", got)
	}

	// A wrapped error still carries the detail: the call sites pass the error
	// through fmt.Errorf chains.
	wrapped := fmt.Errorf("opening stream: %w", &HTTPError{Status: 403, Body: []byte("nope")})
	if got := refusalDetail(wrapped); got != "nope" {
		t.Errorf("a wrapped HTTPError produced %q, want %q", got, "nope")
	}

	// Nothing to quote stays nothing: a nil error, a plain transport error,
	// or an HTTPError with an empty body.
	if got := refusalDetail(nil); got != "" {
		t.Errorf("nil error produced %q", got)
	}
	if got := refusalDetail(errors.New("dial tcp: connection refused")); got != "" {
		t.Errorf("a transport error produced %q", got)
	}
	if got := refusalDetail(&HTTPError{Status: 403}); got != "" {
		t.Errorf("an empty body produced %q", got)
	}
}

func TestDetailSuffixIsSilentWhenEmpty(t *testing.T) {
	if got := detailSuffix(""); got != "" {
		t.Errorf("empty detail produced %q, want no suffix at all", got)
	}
	if got := detailSuffix("blocked"); got != " (blocked)" {
		t.Errorf("detail produced %q, want %q", got, " (blocked)")
	}
}
