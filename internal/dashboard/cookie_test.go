package dashboard

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

var tlsConnState = tls.ConnectionState{}

// TestSecureCookieCanBeForcedWithoutTLS covers the reverse-proxy deployment: TLS
// terminates upstream, so r.TLS is nil here, but the cookie must still carry
// Secure or it would travel in cleartext on any plain-HTTP path to the same host.
func TestSecureCookieCanBeForcedWithoutTLS(t *testing.T) {
	d := testDashboard(t)
	d.cfg.SecureCookie = true

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil) // no TLS
	d.setSessionCookie(rec, req, "value")

	cookie := rec.Result().Cookies()
	if len(cookie) == 0 {
		t.Fatal("no cookie was set")
	}
	if !cookie[0].Secure {
		t.Errorf("cookie Secure = false with the knob on, want true")
	}
}

// TestCookieIsNotSecureOnPlainHTTPByDefault pins the inverse: without the knob and
// without TLS (the loopback default), Secure stays off, or the cookie would vanish
// on the setup this is normally run as.
func TestCookieIsNotSecureOnPlainHTTPByDefault(t *testing.T) {
	d := testDashboard(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	d.setSessionCookie(rec, req, "value")
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 {
		t.Fatal("no cookie was set")
	}
	if cookie[0].Secure {
		t.Errorf("cookie Secure = true on plain HTTP without the knob, want false")
	}
}

// TestSecureCookieOnTLS pins that TLS still implies Secure with the knob off.
func TestSecureCookieOnTLS(t *testing.T) {
	d := testDashboard(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://localhost/dashboard/", nil)
	req.TLS = &tlsConnState
	d.setSessionCookie(rec, req, "value")
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 {
		t.Fatal("no cookie was set")
	}
	if !cookie[0].Secure {
		t.Errorf("cookie Secure = false over TLS, want true")
	}
}

// TestClearedCookieAlsoHonoursTheKnob pins that the logout path agrees with the
// login path, so a browser does not keep a non-Secure clear over a Secure set.
func TestClearedCookieAlsoHonoursTheKnob(t *testing.T) {
	d := testDashboard(t)
	d.cfg.SecureCookie = true
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	d.clearSessionCookie(rec, req)
	cookie := rec.Result().Cookies()
	if len(cookie) == 0 {
		t.Fatal("no cookie was set")
	}
	if !cookie[0].Secure {
		t.Errorf("cleared cookie Secure = false with the knob on, want true")
	}
}
