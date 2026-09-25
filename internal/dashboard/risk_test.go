package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"devin2proxy/internal/devin"
)

// Regression tests for the risk batch on the dashboard side: the Test button's
// quota throttle, the masked-route restore that must not mix up two routes that
// differ only in credentials, and the page itself honouring the loopback rule.

func TestTwoTestsOfTheSameAccountInTheSameWindowRefuseTheSecond(t *testing.T) {
	// A probe is a real completion and spends real quota. An operator who clicks
	// Test twice — or a page that double-fires — must not drain the account
	// twice in a row without being told why.
	stub, auths := probeStub(t, "")
	d, cookie := probingDashboard(t, stub)

	first := post(t, d, "/dashboard/api/accounts/test", map[string]any{"index": 0, "model": "swe-1-6-slow"}, cookie)
	if first.Code != http.StatusOK {
		t.Fatalf("first test: status %d, body %s", first.Code, first.Body)
	}
	if len(*auths) != 1 {
		t.Fatalf("backend asked %d times, want 1", len(*auths))
	}

	second := post(t, d, "/dashboard/api/accounts/test", map[string]any{"index": 0, "model": "swe-1-6-slow"}, cookie)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second test: status %d, want 429 (body %s)", second.Code, second.Body)
	}
	if !strings.Contains(second.Body.String(), "quota") {
		t.Errorf("the refusal does not explain itself: %s", second.Body)
	}
	if len(*auths) != 1 {
		t.Fatalf("backend asked %d times, want the throttle to have stopped the second probe", len(*auths))
	}

	// A different account is not throttled by this one's probe.
	third := post(t, d, "/dashboard/api/accounts/test", map[string]any{"index": 1, "model": "swe-1-6-slow"}, cookie)
	if third.Code != http.StatusOK {
		t.Fatalf("other account: status %d, want 200 (body %s)", third.Code, third.Body)
	}
	if len(*auths) != 2 {
		t.Fatalf("backend asked %d times, want 2", len(*auths))
	}
}

func TestTheThrottleWindowExpires(t *testing.T) {
	// The throttle is a window, not a permanent mark: after it passes, the same
	// account can be tested again. Shrink the recorded time rather than waiting
	// for the real interval.
	stub, _ := probeStub(t, "")
	d, cookie := probingDashboard(t, stub)

	rec := post(t, d, "/dashboard/api/accounts/test", map[string]any{"index": 0, "model": "swe-1-6-slow"}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	d.probeMu.Lock()
	for creds := range d.lastProbe {
		d.lastProbe[creds] = time.Now().Add(-2 * probeMinInterval)
	}
	d.probeMu.Unlock()

	again := post(t, d, "/dashboard/api/accounts/test", map[string]any{"index": 0, "model": "swe-1-6-slow"}, cookie)
	if again.Code != http.StatusOK {
		t.Fatalf("after the window: status %d, want 200 (body %s)", again.Code, again.Body)
	}
}

func TestAMaskedRouteRestoresToItsOwnCredentials(t *testing.T) {
	// Two routes on the same host and port under different accounts mask to the
	// identical string. A save that round-trips the masked list must give each
	// line back its own position's credentials, not the first match's — the
	// failure a plain scan has.
	stored := []string{
		"socks5://alice:first-password@proxy.example:1080",
		"socks5://bob:second-password@proxy.example:1080",
	}
	masked := maskSpecs(stored)
	if masked[0] != masked[1] {
		t.Fatalf("the two routes do not mask alike: %q vs %q", masked[0], masked[1])
	}

	restored := restoreCredentials(masked, stored)
	if restored[0] != stored[0] || restored[1] != stored[1] {
		t.Fatalf("restore gave %q, want each route its own credentials", restored)
	}
	if !strings.Contains(restored[1], "second-password") {
		t.Errorf("route 2 came back as %q, not its own password", restored[1])
	}
}

func TestAMaskedRouteMovedInTheListStillRestores(t *testing.T) {
	// Position is the fast path, not the only one: a route reordered on the page
	// must still find its credentials by content, as long as the match is
	// unambiguous.
	stored := []string{
		"socks5://alice:pw-a@alpha.example:1080",
		"socks5://bob:pw-b@beta.example:1081",
	}
	// The page sends them back swapped.
	swapped := []string{devin.MaskSpec(stored[1]), devin.MaskSpec(stored[0])}
	restored := restoreCredentials(swapped, stored)
	if restored[0] != stored[1] || restored[1] != stored[0] {
		t.Fatalf("restore of a reordered list = %q", restored)
	}
}

func TestAMaskedRouteMatchingNothingStoredIsLeftAsIs(t *testing.T) {
	// A masked line for a route nobody has credentials for cannot be restored;
	// leaving it masked means the pool rejects it, which is the honest outcome.
	masked := devin.MaskSpec("socks5://someone:secret@new.example:1080")
	restored := restoreCredentials([]string{masked}, []string{"socks5://other:pw@other.example:1080"})
	if restored[0] != masked {
		t.Fatalf("restore = %q, want the masked line untouched", restored[0])
	}
}

func TestThePageItselfIsLoopbackOnly(t *testing.T) {
	// The guard covers the data endpoints and the session check; the page shell
	// has to follow the same rule, or a remote caller gets a login form that can
	// never accept its password.
	d := testDashboard(t)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/", nil)
	req.RemoteAddr = "192.168.1.10:1234"
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403 (body %s)", rec.Code, rec.Body)
	}

	// From loopback it still serves, of course.
	if rec := get(t, d, "/dashboard/"); rec.Code != http.StatusOK {
		t.Fatalf("loopback status %d, want 200", rec.Code)
	}
}

func TestABodyOverTheDashboardCapIsRefused(t *testing.T) {
	// decodeBody bounds the dashboard's reads at 1 MiB; a larger paste must
	// come back as a refusal, not as a truncated document the handler then
	// acts on.
	d := testDashboard(t)
	login := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, login)

	filler := strings.Repeat("a", (1<<20)+1024)
	body := `{"password":"` + filler + `"}`
	req := httptest.NewRequest(http.MethodPost, "/dashboard/api/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %.200s)", rec.Code, rec.Body)
	}
}

func TestRestoreCredentialsLeavesAnEmptyListAlone(t *testing.T) {
	// The save path treats an empty list as "clear the routes"; restore must not
	// turn it into something else.
	got := restoreCredentials(nil, []string{"socks5://a:b@c.example:1080"})
	if len(got) != 0 {
		t.Fatalf("restore of an empty list = %q", got)
	}
}

// A stored line that is itself the mask — what pool.Specs() hands back, or a line
// a past bug wrote into the store — carries no credentials, so it must never be
// used as a restoration source. Restoring against it would write "***:***" into
// the pool as if the mask were the password.
func TestAMaskedStoredLineIsNeverARestorationSource(t *testing.T) {
	stored := []string{
		"socks5://***:***@proxy.example:1080",             // a credential-stripped copy
		"socks5://alice:real-password@proxy.example:1080", // the real route
	}
	in := []string{devin.MaskSpec(stored[1])}
	if in[0] != stored[0] {
		t.Fatalf("the stripped copy does not mask alike: %q vs %q", in[0], stored[0])
	}
	restored := restoreCredentials(in, stored)
	if restored[0] != stored[1] {
		t.Fatalf("restore = %q, want the line that actually carries credentials", restored[0])
	}
}

// The page surfaces a refused test — including the throttle's 429 — by showing
// the server's own error text, so the operator sees why the button did nothing
// rather than concluding the account is broken.
func TestThePageSurfacesARefusedTest(t *testing.T) {
	d := testDashboard(t)
	page := get(t, d, "/dashboard/").Body.String()
	if !strings.Contains(page, "function testAccount(") {
		t.Fatal("the page has no testAccount")
	}
	// api() lifts the {error: ...} body into the thrown Error's message...
	if !strings.Contains(page, "data.error") {
		t.Error("api() does not lift the server's error text")
	}
	// ...and testAccount records it, which probeNote renders as "failed — ...".
	if !strings.Contains(page, "S.probeResults[index] = { ok: false, error: err.message }") {
		t.Error("testAccount swallows the refusal instead of recording its message")
	}
	if !strings.Contains(page, "failed — ") {
		t.Error("probeNote does not render a failure line")
	}
}
