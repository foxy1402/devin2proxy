package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/eventlog"
	"devin2proxy/internal/pb"
)

// The login ban is the feature the user asked for by name, and it is the one most
// likely to be got subtly wrong: a ban that does not escalate, that a restart
// clears, or that can be reset by a single success between failures would all look
// right in a demo. These tests pin each of those.

func testDashboard(t *testing.T) *Dashboard {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "dashboard.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.SetPasswordText("correct-horse-battery"); err != nil {
		t.Fatalf("SetPasswordText: %v", err)
	}
	return New(Options{
		Pool:   devin.NewPool([]string{"devin-session-token$aaaaaa", "devin-session-token$bbbbbb"}, ""),
		Store:  store,
		Events: eventlog.New(64),
	})
}

// post sends a JSON body from the loopback address the dashboard trusts.
func post(t *testing.T, d *Dashboard, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var payload string
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		payload = string(b)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, d *Dashboard, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return rec
}

func sessionCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no session cookie in response (headers %v)", rec.Header())
	return nil
}

func TestLoginSucceedsWithTheRightPassword(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body)
	}
	cookie := sessionCookieFrom(t, rec)
	if !d.validSession(cookie.Value, time.Now()) {
		t.Fatal("the cookie issued at login is not a valid session")
	}
	if rec := get(t, d, "/dashboard/api/accounts", cookie); rec.Code != http.StatusOK {
		t.Fatalf("accounts with a session: status %d, body %s", rec.Code, rec.Body)
	}
	// The cookie must not be readable by script: it is the only thing standing
	// between a page on another origin and the credential pool.
	if !cookie.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("the session cookie is SameSite=%v, want Strict", cookie.SameSite)
	}
	if cookie.Path != dashboardPrefix {
		t.Errorf("the session cookie path is %q, want %q", cookie.Path, dashboardPrefix)
	}
}

func TestThreeFailuresBanAndTheBanEscalates(t *testing.T) {
	d := testDashboard(t)
	wrong := map[string]string{"password": "not-the-password"}

	// The first two failures are ordinary mistyping: they warn, they do not ban.
	for i := 1; i <= 2; i++ {
		rec := post(t, d, "/dashboard/api/login", wrong)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401 (body %s)", i, rec.Code, rec.Body)
		}
	}

	third := post(t, d, "/dashboard/api/login", wrong)
	if third.Code != http.StatusTooManyRequests {
		t.Fatalf("third failure: status %d, want 429 (body %s)", third.Code, third.Body)
	}

	// Banned means banned: even the right password is refused, because the ban is
	// checked before the password is.
	right := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	if right.Code != http.StatusTooManyRequests {
		t.Fatalf("login during a ban: status %d, want 429", right.Code)
	}

	firstBanUntil := d.cfg.Store.data.Bans["127.0.0.1"].Until

	// Clear the ban by moving the clock past it, then fail again: the next ban must
	// be longer, because that is what makes continuing to guess pointless.
	d.cfg.Store.mu.Lock()
	d.cfg.Store.data.Bans["127.0.0.1"].Until = time.Now().Add(-time.Second)
	d.cfg.Store.mu.Unlock()

	fourth := post(t, d, "/dashboard/api/login", wrong)
	if fourth.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth failure: status %d, want 429", fourth.Code)
	}
	secondBanUntil := d.cfg.Store.data.Bans["127.0.0.1"].Until
	if !secondBanUntil.After(firstBanUntil.Add(30 * time.Second)) {
		t.Errorf("the second ban ends at %s, not meaningfully after the first at %s; the ban is not escalating",
			secondBanUntil, firstBanUntil)
	}
}

func TestBanDurationDoublesAndIsCapped(t *testing.T) {
	cases := []struct {
		strikes int
		want    time.Duration
	}{
		{0, 0},
		{2, 0},                          // still within the free attempts
		{banAfterFailures, time.Minute}, // the first ban
		{banAfterFailures + 1, 2 * time.Minute},
		{banAfterFailures + 2, 4 * time.Minute},
		{banAfterFailures + 3, 8 * time.Minute},
		// Far past the cap: a mistyped password must not lock the owner out for
		// days, so the ladder stops at maxBan.
		{banAfterFailures + 30, maxBan},
	}
	for _, c := range cases {
		if got := banDuration(c.strikes); got != c.want {
			t.Errorf("banDuration(%d) = %s, want %s", c.strikes, got, c.want)
		}
	}
}

func TestSuccessfulLoginClearsTheFailureHistory(t *testing.T) {
	d := testDashboard(t)
	for i := 0; i < 2; i++ {
		post(t, d, "/dashboard/api/login", map[string]string{"password": "nope-not-it"})
	}
	if rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}); rec.Code != http.StatusOK {
		t.Fatalf("login after two failures: status %d", rec.Code)
	}
	state := d.loginStateFor("127.0.0.1")
	if state.Strikes != 0 {
		t.Errorf("strikes after a successful login = %d, want 0", state.Strikes)
	}
	if state.Remaining != banAfterFailures {
		t.Errorf("remaining attempts after a successful login = %d, want %d", state.Remaining, banAfterFailures)
	}
}

func TestBansSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dashboard.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.SetPasswordText("correct-horse-battery"); err != nil {
		t.Fatalf("SetPasswordText: %v", err)
	}
	d := New(Options{Store: store, Pool: devin.NewPool(nil, "")})
	for i := 0; i < banAfterFailures; i++ {
		post(t, d, "/dashboard/api/login", map[string]string{"password": "wrong"})
	}

	// A punishment that a restart clears is not a punishment.
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	restarted := New(Options{Store: reopened, Pool: devin.NewPool(nil, "")})
	if !restarted.loginStateFor("127.0.0.1").Banned {
		t.Fatal("the ban did not survive a restart")
	}
	if rec := post(t, restarted, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("login after a restart with an active ban: status %d, want 429", rec.Code)
	}
}

func TestSessionEpochInvalidatesEverySession(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)
	if !d.validSession(cookie.Value, time.Now()) {
		t.Fatal("a fresh session should be valid")
	}
	if _, err := d.cfg.Store.BumpSessionEpoch(); err != nil {
		t.Fatalf("BumpSessionEpoch: %v", err)
	}
	if d.validSession(cookie.Value, time.Now()) {
		t.Fatal("a session issued before the epoch bump is still valid")
	}
}

func TestSessionRejectsATamperedCookie(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	// Flip one character of the payload. The signature covers it, so this must fail
	// closed rather than decoding into something plausible.
	tampered := *cookie
	payload, sig, ok := strings.Cut(cookie.Value, ".")
	if !ok {
		t.Fatalf("cookie %q is not in payload.signature form", cookie.Value)
	}
	flipped := byte('A')
	if payload[0] == 'A' {
		flipped = 'B'
	}
	tampered.Value = string(flipped) + payload[1:] + "." + sig
	if d.validSession(tampered.Value, time.Now()) {
		t.Fatal("a tampered session cookie was accepted")
	}

	// An entirely invented cookie must fail too.
	if d.validSession("not-a-session", time.Now()) {
		t.Fatal("an invented session cookie was accepted")
	}
}

func TestExpiredSessionIsRefused(t *testing.T) {
	d := testDashboard(t)
	value, err := d.newSession(time.Now().Add(-2 * sessionTTL))
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	if d.validSession(value, time.Now()) {
		t.Fatal("a session issued beyond its lifetime was accepted")
	}
}

func TestUnauthenticatedRequestsAreRefused(t *testing.T) {
	d := testDashboard(t)
	for _, path := range []string{
		"/dashboard/api/accounts",
		"/dashboard/api/proxies",
		"/dashboard/api/overview",
		"/dashboard/api/logs/recent",
	} {
		rec := get(t, d, path)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session: status %d, want 401", path, rec.Code)
		}
	}
	// A mutating endpoint must refuse too, not just the read-only ones.
	if rec := post(t, d, "/dashboard/api/accounts/delete", map[string]int{"index": 0}); rec.Code != http.StatusUnauthorized {
		t.Errorf("delete without a session: status %d, want 401", rec.Code)
	}
	// And the pool must be untouched by the refusal.
	if d.cfg.Pool.Len() != 2 {
		t.Errorf("pool length after a refused delete = %d, want 2", d.cfg.Pool.Len())
	}
}

func TestSessionEndpointReportsBanStateWithoutASession(t *testing.T) {
	d := testDashboard(t)
	rec := get(t, d, "/dashboard/api/session")
	if rec.Code != http.StatusOK {
		t.Fatalf("session endpoint: status %d", rec.Code)
	}
	var body struct {
		Configured    bool `json:"configured"`
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !body.Configured || body.Authenticated {
		t.Errorf("fresh session endpoint = %+v, want configured and not authenticated", body)
	}
	// It must not leak the account pool: that is the whole point of the guard.
	if strings.Contains(rec.Body.String(), "aaaaaa") || strings.Contains(rec.Body.String(), "bbbbbb") {
		t.Error("the unauthenticated session endpoint leaked an account token tail")
	}
}

func TestRemoteCallersAreRefusedUnlessAllowed(t *testing.T) {
	d := testDashboard(t)
	req := httptest.NewRequest(http.MethodPost, "/dashboard/api/login",
		strings.NewReader(`{"password":"correct-horse-battery"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.7:4444"
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a remote login attempt: status %d, want 403", rec.Code)
	}

	// The same request is served when remote access was asked for.
	d.cfg.AllowRemote = true
	rec = httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a remote login attempt with remote access allowed: status %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

func TestCrossOriginRequestsAreRefused(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	req := httptest.NewRequest(http.MethodPost, "/dashboard/api/accounts/delete",
		strings.NewReader(`{"index":0}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	req.RemoteAddr = "127.0.0.1:54321"
	req.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	d.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("a cross-origin delete: status %d, want 403", rec2.Code)
	}
	if d.cfg.Pool.Len() != 2 {
		t.Error("a cross-origin delete removed an account")
	}

	// The same request from the dashboard's own origin is allowed.
	req.Header.Set("Origin", "http://"+req.Host)
	rec3 := httptest.NewRecorder()
	d.ServeHTTP(rec3, req)
	if rec3.Code != http.StatusOK {
		t.Fatalf("a same-origin delete: status %d, want 200 (body %s)", rec3.Code, rec3.Body)
	}
	if d.cfg.Pool.Len() != 1 {
		t.Errorf("pool length after a same-origin delete = %d, want 1", d.cfg.Pool.Len())
	}
}

func TestAccountsAddAndDeleteGoThroughTheStore(t *testing.T) {
	d := testDashboard(t)
	d.cfg.Pool = devin.NewPool(nil, "")
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	// A token that is not a session token is refused before it reaches the pool: the
	// alternative is a pool full of things that only fail later.
	if got := post(t, d, "/dashboard/api/accounts/add", map[string]string{"token": "hello"}, cookie); got.Code != http.StatusBadRequest {
		t.Fatalf("adding a bogus token: status %d, want 400 (body %s)", got.Code, got.Body)
	}
	if d.cfg.Pool.Len() != 0 {
		t.Fatal("a rejected token reached the pool")
	}

	good := post(t, d, "/dashboard/api/accounts/add",
		map[string]string{"token": "devin-session-token$onetwothree"}, cookie)
	if good.Code != http.StatusOK {
		t.Fatalf("adding a token: status %d, want 200 (body %s)", good.Code, good.Body)
	}
	if d.cfg.Pool.Len() != 1 {
		t.Fatalf("pool length = %d, want 1", d.cfg.Pool.Len())
	}
	// The store is what a restart reads, so it has to agree with the pool.
	if stored := d.cfg.Store.Accounts(); len(stored) != 1 || stored[0] != "devin-session-token$onetwothree" {
		t.Fatalf("stored accounts = %v, want the token just added", stored)
	}
	// Adding the same token twice is a conflict, not a second slot.
	if dup := post(t, d, "/dashboard/api/accounts/add",
		map[string]string{"token": "devin-session-token$onetwothree"}, cookie); dup.Code != http.StatusConflict {
		t.Fatalf("adding a duplicate: status %d, want 409 (body %s)", dup.Code, dup.Body)
	}

	if del := post(t, d, "/dashboard/api/accounts/delete", map[string]int{"index": 0}, cookie); del.Code != http.StatusOK {
		t.Fatalf("delete: status %d, want 200 (body %s)", del.Code, del.Body)
	}
	if d.cfg.Pool.Len() != 0 {
		t.Errorf("pool length after delete = %d, want 0", d.cfg.Pool.Len())
	}
	if stored := d.cfg.Store.Accounts(); len(stored) != 0 {
		t.Errorf("stored accounts after delete = %v, want none", stored)
	}
}

func TestAccountsAreNotEditableWhenThePoolComesFromConfig(t *testing.T) {
	d := testDashboard(t)
	d.cfg.AccountsExternal = "config.json (tokens or tokens_file)"
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	if got := post(t, d, "/dashboard/api/accounts/add",
		map[string]string{"token": "devin-session-token$zzz"}, cookie); got.Code != http.StatusConflict {
		t.Fatalf("adding to a config-supplied pool: status %d, want 409", got.Code)
	}
	if got := post(t, d, "/dashboard/api/accounts/delete", map[string]int{"index": 0}, cookie); got.Code != http.StatusConflict {
		t.Fatalf("deleting from a config-supplied pool: status %d, want 409", got.Code)
	}
	if d.cfg.Pool.Len() != 2 {
		t.Errorf("the pool changed despite being supplied by config: %d entries", d.cfg.Pool.Len())
	}
}

func TestProxiesSaveValidatesBeforeSwapping(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	// A bad spec must leave the running configuration alone. Installing first and
	// discovering the problem afterwards would take a working proxy down.
	bad := post(t, d, "/dashboard/api/proxies", map[string]any{"text": "socks5://\n"}, cookie)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("saving a malformed route: status %d, want 400 (body %s)", bad.Code, bad.Body)
	}
	if n := d.cfg.Client.Egress().Len(); n != 0 {
		t.Fatalf("a rejected route list was installed: %d routes", n)
	}

	good := post(t, d, "/dashboard/api/proxies", map[string]any{
		"text": "socks5://user:secret@127.0.0.1:1080\n# a comment\nhttp://127.0.0.1:8080\n",
	}, cookie)
	if good.Code != http.StatusOK {
		t.Fatalf("saving routes: status %d, want 200 (body %s)", good.Code, good.Body)
	}
	pool := d.cfg.Client.Egress()
	if pool.Len() != 2 {
		t.Fatalf("route count = %d, want 2 (comments and blanks must be dropped)", pool.Len())
	}
	// The stored list is the cleaned one, so a restart rebuilds exactly this.
	stored := d.cfg.Store.Proxies()
	if len(stored) != 2 {
		t.Fatalf("stored routes = %v, want 2", stored)
	}
	// And the response must not carry the credential back out.
	if strings.Contains(good.Body.String(), "secret") {
		t.Error("the saved route list echoed the proxy password back to the page")
	}

	// An empty list is valid: it means the machine's own connection.
	if cleared := post(t, d, "/dashboard/api/proxies", map[string]any{"text": ""}, cookie); cleared.Code != http.StatusOK {
		t.Fatalf("clearing the routes: status %d (body %s)", cleared.Code, cleared.Body)
	}
	if n := d.cfg.Client.Egress().Len(); n != 0 {
		t.Errorf("route count after clearing = %d, want 0", n)
	}
}

func TestProxySpecsNeverCarryCredentialsIntoTheListing(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)
	post(t, d, "/dashboard/api/proxies", map[string]any{
		"text": "socks5://alice:hunter2@127.0.0.1:1080",
	}, cookie)

	listed := get(t, d, "/dashboard/api/proxies", cookie)
	if strings.Contains(listed.Body.String(), "hunter2") {
		t.Fatalf("the route listing carried the proxy password: %s", listed.Body)
	}
	// The masked form still has to identify the route, or the listing is useless.
	if !strings.Contains(listed.Body.String(), "127.0.0.1:1080") {
		t.Errorf("the route listing lost the host: %s", listed.Body)
	}
}

func TestTooManyRoutesIsRefused(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	specs := make([]string, maxProxySpecs+1)
	for i := range specs {
		specs[i] = "socks5://127.0.0.1:1080"
	}
	got := post(t, d, "/dashboard/api/proxies", map[string]any{"specs": specs}, cookie)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("saving %d routes: status %d, want 400", len(specs), got.Code)
	}
}

func TestLogsRecentIsBoundedAndOrdered(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	for i := 0; i < 5; i++ {
		d.cfg.Events.Emit(eventlog.Event{Kind: "log", Message: "line", Level: eventlog.LevelInfo})
	}
	got := get(t, d, "/dashboard/api/logs/recent?limit=3", cookie)
	if got.Code != http.StatusOK {
		t.Fatalf("recent logs: status %d", got.Code)
	}
	var body struct {
		Events []eventlog.Event `json:"events"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(body.Events) != 3 {
		t.Fatalf("event count = %d, want the requested 3", len(body.Events))
	}
	// Newest first: the latest line is what a page opened now draws on top.
	if body.Events[0].Seq <= body.Events[2].Seq {
		t.Errorf("events are not newest-first: %d then %d", body.Events[0].Seq, body.Events[2].Seq)
	}
	if body.Events[0].Seq != 5 {
		t.Errorf("the first event is seq %d, want 5", body.Events[0].Seq)
	}
}

// The Logs view keeps the newest 200 events and drops older ones as new ones
// arrive, so a page left open does not grow without bound. The limit is enforced
// on the response — the page can ask for at most the same window it keeps —
// which is what makes one request per poll a fixed, predictable cost.
func TestLogsRecentNeverExceedsTheViewWindow(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	for i := 0; i < 250; i++ {
		d.cfg.Events.Emit(eventlog.Event{Kind: "log", Message: "line", Level: eventlog.LevelInfo})
	}
	// The test hub holds 64, which is smaller than the view window — the endpoint
	// answers with what it has, and the window is a ceiling, not a promise.
	for _, target := range []string{
		"/dashboard/api/logs/recent",
		"/dashboard/api/logs/recent?limit=5000",
		"/dashboard/api/logs/recent?limit=abc",
	} {
		got := get(t, d, target, cookie)
		if got.Code != http.StatusOK {
			t.Fatalf("%s: status %d", target, got.Code)
		}
		var body struct {
			Events []eventlog.Event `json:"events"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: parse: %v", target, err)
		}
		if len(body.Events) != 64 {
			t.Fatalf("%s: event count = %d, want the 64 the hub holds", target, len(body.Events))
		}
		// The hub holds the newest 64, and the endpoint hands them over newest-first.
		if body.Events[0].Seq != 250 || body.Events[63].Seq != 187 {
			t.Fatalf("%s: window runs %d..%d, want 250..187", target, body.Events[0].Seq, body.Events[63].Seq)
		}
	}
}

// The view window is a ceiling the endpoint enforces: backed by a hub bigger
// than the window, a request for more than the window still gets the window —
// newest events, newest first. That ceiling is what makes one request per poll
// a fixed, predictable cost.
func TestLogsRecentIsCappedAtTheViewWindow(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "dashboard.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.SetPasswordText("correct-horse-battery"); err != nil {
		t.Fatalf("SetPasswordText: %v", err)
	}
	d := New(Options{
		Pool:   devin.NewPool([]string{"devin-session-token$aaaaaa", "devin-session-token$bbbbbb"}, ""),
		Store:  store,
		Events: eventlog.New(logsPageSize + 50),
	})
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	for i := 0; i < logsPageSize+50; i++ {
		d.cfg.Events.Emit(eventlog.Event{Kind: "log", Message: "line", Level: eventlog.LevelInfo})
	}
	got := get(t, d, "/dashboard/api/logs/recent?limit=5000", cookie)
	if got.Code != http.StatusOK {
		t.Fatalf("recent logs: status %d", got.Code)
	}
	var body struct {
		Events []eventlog.Event `json:"events"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(body.Events) != logsPageSize {
		t.Fatalf("event count = %d, want the capped %d", len(body.Events), logsPageSize)
	}
	if body.Events[0].Seq != logsPageSize+50 || body.Events[logsPageSize-1].Seq != 51 {
		t.Fatalf("window runs %d..%d, want %d..51", body.Events[0].Seq, body.Events[logsPageSize-1].Seq, logsPageSize+50)
	}
}
func TestTheOldLogStreamPointsAtThePollingEndpoint(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	got := get(t, d, "/dashboard/api/logs", cookie)
	if got.Code != http.StatusGone {
		t.Fatalf("old stream endpoint: status %d, want 410", got.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(body.Error, "/dashboard/api/logs/recent") {
		t.Errorf("the 410 does not name the replacement: %q", body.Error)
	}
}

// The page's Logs view draws what the endpoint hands it: a poll every three
// seconds, merged newest-first, capped at the same 200-event window.
func TestTheLogsViewPollsNewestFirstWithinACap(t *testing.T) {
	d := testDashboard(t)
	page := get(t, d, "/dashboard/").Body.String()
	for _, want := range []string{
		"function pollLogs(",
		"/logs/recent?limit=200",
		"logPollMS = 3000",
		"var logCap = 200",
		"setInterval(pollLogs, logPollMS)",
		"clearInterval(S.logTimer)",
		"S.logs.unshift(",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not mention %q", want)
		}
	}
	if strings.Contains(page, "new EventSource") {
		t.Error("the page still opens a streaming log connection")
	}
	// The overview poller has to stop with the session: a logged-out page that
	// keeps asking /overview re-enters boot() on every 401 and redraws the login
	// form out from under the operator.
	if !strings.Contains(page, "clearInterval(S.overviewTimer)") {
		t.Error("stopStream does not stop the overview poller")
	}
	for _, want := range []string{"newest first", "every 3 seconds", "newest 200"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not tell the operator %q", want)
		}
	}
}

func TestAccountsListingCarriesNoToken(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)
	body := get(t, d, "/dashboard/api/accounts", cookie).Body.String()
	if strings.Contains(body, "devin-session-token$") {
		t.Fatalf("the account listing carried a full token: %s", body)
	}
	if !strings.Contains(body, "aaaaaa") {
		t.Errorf("the account listing does not identify the accounts: %s", body)
	}
}

// The page offers the sign-in command for the operator to run in a terminal, so the
// command has to be the one this server actually uses. A generic "devin auth login"
// offered on a machine whose CLI lives somewhere else is a dead end the operator cannot
// diagnose from the page. The empty case matters too: with no CLI configured the page
// falls back to a name that at least reads correctly, and says so.
func TestTheSignInCommandIsTheOneThisMachineUses(t *testing.T) {
	dir := t.TempDir()
	store := mustStore(t, filepath.Join(dir, "dashboard.json"))
	if err := store.SetPasswordText("correct-horse-battery"); err != nil {
		t.Fatalf("SetPasswordText: %v", err)
	}
	d := New(Options{
		Pool:       devin.NewPool(nil, ""),
		Store:      store,
		Events:     eventlog.New(64),
		DevinCLI:   `C:\Program Files\devin\devin.exe`,
		LoginArgs:  []string{"auth", "login", "--force-manual-token-flow"},
		LogoutArgs: []string{"auth", "logout"},
	})
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))

	var listed struct {
		SignInCommand string `json:"sign_in_command"`
		CLIReady      bool   `json:"cli_ready"`
	}
	if err := json.Unmarshal(get(t, d, "/dashboard/api/accounts", cookie).Body.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal accounts: %v", err)
	}
	want := `"C:\Program Files\devin\devin.exe" auth login --force-manual-token-flow`
	if listed.SignInCommand != want {
		t.Errorf("sign_in_command = %q, want %q", listed.SignInCommand, want)
	}
	if !listed.CLIReady {
		t.Error("cli_ready is false with a CLI and login arguments configured")
	}

	// The fallback: no CLI configured, so the page must not claim one is ready.
	bare := New(Options{Pool: devin.NewPool(nil, ""), Store: store, Events: eventlog.New(64)})
	if got := bare.signInCommand(); !strings.HasPrefix(got, "devin ") {
		t.Errorf("sign-in command with no CLI configured = %q, want it to fall back to the documented name", got)
	}
}

func TestStoreWithNoPathStaysInMemory(t *testing.T) {
	// A dashboard with no writable directory must still work: the file is the
	// durable part, not a requirement.
	store, err := OpenStore("")
	if err != nil {
		t.Fatalf("OpenStore(\"\"): %v", err)
	}
	if err := store.SetPasswordText("correct-horse-battery"); err != nil {
		t.Fatalf("SetPasswordText: %v", err)
	}
	if store.Password() == nil {
		t.Fatal("an in-memory store lost the password")
	}
	if err := store.SetAccounts([]string{"devin-session-token$x"}); err != nil {
		t.Fatalf("SetAccounts: %v", err)
	}
	if got := store.Accounts(); len(got) != 1 {
		t.Fatalf("in-memory accounts = %v", got)
	}
}

func TestGeneratePasswordIsUsableAndStored(t *testing.T) {
	store, err := OpenStore("")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	password, err := store.GeneratePassword()
	if err != nil {
		t.Fatalf("GeneratePassword: %v", err)
	}
	if len(password) < MinPasswordLength {
		t.Errorf("generated password %q is shorter than the minimum", password)
	}
	rec := store.Password()
	if rec == nil {
		t.Fatal("GeneratePassword stored nothing")
	}
	if !verifyPassword(rec, password) {
		t.Error("the generated password does not verify against the record that was stored")
	}
	// The plaintext must not be in the record: only the hash is persisted.
	if strings.Contains(rec.Hash, password) || rec.Hash == password {
		t.Error("the password record contains the plaintext password")
	}
}

func TestAVeryShortPasswordIsRefused(t *testing.T) {
	store, err := OpenStore("")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.SetPasswordText("short"); err != ErrPasswordTooShort {
		t.Fatalf("SetPasswordText(\"short\") = %v, want ErrPasswordTooShort", err)
	}
	if store.Password() != nil {
		t.Error("a rejected password was still stored")
	}
}

func TestTheUIRouteServesThePageWithoutASession(t *testing.T) {
	d := testDashboard(t)
	rec := get(t, d, "/dashboard/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/: status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	// The page has to be reachable before anyone is signed in, so it must contain no
	// data of its own — only the code that fetches it, plus a format hint that names
	// the token prefix without being one.
	for _, token := range d.cfg.Pool.Tokens() {
		if strings.Contains(rec.Body.String(), token) {
			t.Error("the page contains a pool token")
		}
	}
	// /dashboard without the trailing slash redirects rather than 404s, which is
	// what someone typing the URL will do.
	redir := get(t, d, "/dashboard")
	if redir.Code != http.StatusFound {
		t.Errorf("GET /dashboard: status %d, want 302", redir.Code)
	}
}

// This is a lint on the page source, not a behavioural test, and it is here because
// the page's faults are otherwise invisible: it draws itself in JavaScript, so a
// mistake that concatenates a built element into a string produces no Go error and no
// browser error. It just prints the node's own string form where a styled chip was
// meant — the mask in the proxy help text read "[object HTMLElement]" on the page.
//
// Checking for that marker in the source would prove nothing, since the marker only
// exists once the browser runs the expression. What can be seen in the source is the
// concatenation itself, which is the actual mistake, so that is what this looks for:
// a "+" immediately before a built element, or immediately after one.
func TestThePageNeverConcatenatesABuiltElementIntoAString(t *testing.T) {
	d := testDashboard(t)
	page := get(t, d, "/dashboard/").Body.String()
	concatenated := regexp.MustCompile(`\+\s*el\(|el\([^()]*\)\s*\+`)
	if where := concatenated.FindString(page); where != "" {
		t.Errorf("the page concatenates a built element into a string (%q); it has to be appended as its own argument, or the page prints the node's string form instead of the element", where)
	}
}

// The add-account dialog has to offer the code box from the moment it opens.
// It used to reveal the box only after Start sign-in was pressed, and that reads as
// "there is nowhere to put the code": the sign-in happens in another tab, so
// returning to a page that reloaded in the meantime gave a dialog whose only visible
// input was "Paste a token" — which is for session tokens, and refuses a code. An
// operator with a valid, still-unexpired code in hand had no way to use it, and that
// is exactly what happened.
//
// Like the lint above, this reads the page source, because the fault is a shape the
// browser is perfectly happy with: nothing errors, the box is simply not there.
func TestTheAddAccountDialogAlwaysOffersTheCodeBox(t *testing.T) {
	d := testDashboard(t)
	page := get(t, d, "/dashboard/").Body.String()
	start := strings.Index(page, "function openAddAccount(")
	if start < 0 {
		t.Fatal("the page has no openAddAccount function")
	}
	rest := page[start:]
	// Cut at the next function so this is only this dialog's body.
	if end := strings.Index(rest[1:], "\n  function "); end >= 0 {
		rest = rest[:end+1]
	}
	if !strings.Contains(rest, "paste the code from the sign-in page") {
		t.Error("the dialog does not create the box the code is pasted into")
	}
	if !strings.Contains(rest, `text: "Add this account"`) {
		t.Error("the dialog does not create the button that exchanges the code")
	}
	if strings.Contains(rest, `text: "Add account"`) {
		t.Error("the dialog's button is named like the button on the panel behind it; one of them has to change")
	}
	if strings.Contains(rest, "display:none") {
		t.Error("the dialog hides part of itself: the code box has to be visible as soon as it opens, " +
			"or a code pasted after a page reload has nowhere to go")
	}
}

// The Models tab reads the same listing the account rows do: a model list is part
// of the status the pool remembers, so no second endpoint was needed for it. This
// pins the JSON the page looks for, because renaming any of these fields would
// silently empty that tab rather than fail anywhere.
func TestAccountsListingCarriesTheModelList(t *testing.T) {
	d := testDashboard(t)
	d.cfg.Pool.Remember(d.cfg.Pool.CredentialAt(0), &devin.AccountStatus{
		Email:  "someone@example.com",
		Plan:   "Free",
		Models: 2,
		ModelList: []devin.ModelInfo{
			{
				Name: "GPT-4.1", UID: "MODEL_CHAT_GPT_4_1_2025_04_14", Provider: "OpenAI",
				Context: 1047576, MaxOutput: 32768,
				Rates: []devin.ModelRate{{Label: "Input", Unit: "1M tokens"}},
			},
			{Name: "xAI Grok-3", UID: "MODEL_XAI_GROK_3", Provider: "xAI", Context: 131072, MaxOutput: 8192},
		},
	})
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))

	var listed struct {
		Accounts []struct {
			Status *devin.AccountStatus `json:"status"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(get(t, d, "/dashboard/api/accounts", cookie).Body.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal accounts: %v", err)
	}
	if len(listed.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(listed.Accounts))
	}
	st := listed.Accounts[0].Status
	if st == nil {
		t.Fatal("the account with a status came back without one")
	}
	if st.Models != 2 || len(st.ModelList) != 2 {
		t.Fatalf("models = %d, model_list = %+v", st.Models, st.ModelList)
	}
	first := st.ModelList[0]
	if first.Name != "GPT-4.1" || first.UID != "MODEL_CHAT_GPT_4_1_2025_04_14" || first.Provider != "OpenAI" {
		t.Fatalf("model = %+v", first)
	}
	if first.Context != 1047576 || first.MaxOutput != 32768 {
		t.Fatalf("limits = %d/%d", first.Context, first.MaxOutput)
	}
	if len(first.Rates) != 1 || first.Rates[0].Label != "Input" || first.Rates[0].Unit != "1M tokens" {
		t.Fatalf("rates = %+v", first.Rates)
	}
}

// The Accounts pool row shows the same two numbers the CLI's main page prints —
// "41% remaining (reset in 3d 7h)" — so this pins both halves of that: the JSON the
// page reads them from, and the page's own rendering of them. The absence of a
// percentage is as important as its value: the backend leaves the field out when it
// has nothing to report, and a page that draws that as 0% would claim every account
// is out of quota.
func TestAccountsListingCarriesTheQuotaWindows(t *testing.T) {
	weekly, daily := 41, 58
	overage := int64(-36850)
	d := testDashboard(t)
	d.cfg.Pool.Remember(d.cfg.Pool.CredentialAt(0), &devin.AccountStatus{
		Email:                  "someone@example.com",
		Plan:                   "Free",
		DailyRemainingPercent:  &daily,
		WeeklyRemainingPercent: &weekly,
		OverageBalanceMicros:   &overage,
		DailyReset:             time.Now().Add(7 * time.Hour),
		WeeklyReset:            time.Now().Add(79 * time.Hour),
		PlanFields:             map[int]int64{8: 2500, 9: 500},
	})
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))

	var listed struct {
		Accounts []struct {
			Status *devin.AccountStatus `json:"status"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(get(t, d, "/dashboard/api/accounts", cookie).Body.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal accounts: %v", err)
	}
	st := listed.Accounts[0].Status
	if st == nil {
		t.Fatal("the account with a status came back without one")
	}
	if st.DailyRemainingPercent == nil || *st.DailyRemainingPercent != 58 {
		t.Fatalf("daily percentage = %v, want 58", st.DailyRemainingPercent)
	}
	if st.WeeklyRemainingPercent == nil || *st.WeeklyRemainingPercent != 41 {
		t.Fatalf("weekly percentage = %v, want 41", st.WeeklyRemainingPercent)
	}
	if st.OverageBalanceMicros == nil || *st.OverageBalanceMicros != -36850 {
		t.Fatalf("overage balance = %v, want -36850", st.OverageBalanceMicros)
	}
	// The account with no status must say nothing rather than zero.
	if other := listed.Accounts[1].Status; other != nil && other.WeeklyRemainingPercent != nil {
		t.Fatalf("the account with no status reports a percentage: %v", *other.WeeklyRemainingPercent)
	}

	// The second account has no status at all, so the page must draw its own
	// placeholder rather than a figure — that is checked on the source below.
	page := get(t, d, "/dashboard/").Body.String()
	for _, want := range []string{
		"weekly_quota_remaining_percent",
		"daily_quota_remaining_percent",
		"% remaining",
		"resets in ",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page never mentions %q, so the quota figures cannot reach it", want)
		}
	}
	if !strings.Contains(page, "percent === undefined || percent === null") {
		t.Error("the page draws a percentage without checking it was sent: an absent window would read as 0% left")
	}
}

// The Models tab reads the catalogue from the backend rather than from its own
// config, so this pins the shape the page depends on: which ids are served, what
// each resolves to, and the limits and prices behind them. The backend is a stub
// pointed at by the pool's api server URL, so nothing here reaches Devin.
func TestModelsEndpointReportsTheCatalogue(t *testing.T) {
	var asked int
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if r.URL.Path != devin.GetCliModelConfigsPath {
			t.Errorf("posted to %s, want %s", r.URL.Path, devin.GetCliModelConfigsPath)
		}
		w.Header().Set("Content-Type", "application/proto")
		w.Write(encodeTestCatalogueForDashboard())
	}))
	defer stub.Close()

	store, err := OpenStore(filepath.Join(t.TempDir(), "dashboard.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.SetPasswordText("correct-horse-battery"); err != nil {
		t.Fatalf("SetPasswordText: %v", err)
	}
	d := New(Options{
		Pool:      devin.NewPool([]string{"devin-session-token$aaaaaa"}, stub.URL),
		Store:     store,
		Events:    eventlog.New(64),
		Client:    devin.NewClient(devin.Options{}),
		Forwarded: []string{"swe-1-6-fast", "swe-1-6-slow"},
		Info:      Info{Model: "swe-1-6-slow", Models: []string{"swe-1-6-slow", "swe-1.6", "swe"}},
	})
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))

	var body struct {
		Served []struct {
			ID          string `json:"id"`
			UID         string `json:"uid"`
			Default     bool   `json:"default"`
			Alias       bool   `json:"alias"`
			Forwarded   bool   `json:"forwarded"`
			InCatalogue bool   `json:"in_catalogue"`
		} `json:"served"`
		Details []struct {
			UID           string `json:"uid"`
			ContextWindow uint64 `json:"context_window"`
			MaxOutput     uint64 `json:"max_output"`
			PlanGated     bool   `json:"plan_gated"`
			Prices        []struct {
				Label  string  `json:"label"`
				Amount float64 `json:"amount"`
			} `json:"prices"`
		} `json:"details"`
		Available   []struct{ UID string } `json:"available"`
		ProbeModels []struct {
			UID    string `json:"uid"`
			Name   string `json:"name"`
			Gated  bool   `json:"gated"`
			Served bool   `json:"served"`
		} `json:"probe_models"`
		ProbeDefault  string `json:"probe_default"`
		CatalogueSize int    `json:"catalogue_size"`
		Gated         int    `json:"gated"`
		Identity      string `json:"identity"`
		Fetched       string `json:"fetched"`
		Account       string `json:"account"`
		Error         string `json:"error"`
	}
	rec := get(t, d, "/dashboard/api/models", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	if body.Error != "" {
		t.Fatalf("the endpoint reported an error: %s", body.Error)
	}
	if body.CatalogueSize != 2 || body.Gated != 1 {
		t.Fatalf("catalogue_size = %d, gated = %d, want 2 and 1", body.CatalogueSize, body.Gated)
	}
	if body.Identity != devin.CLIIdentity {
		t.Errorf("identity = %q, want %q: the catalogue only comes back to the CLI's identity", body.Identity, devin.CLIIdentity)
	}
	if body.Fetched == "" || body.Account != "aaaaaa" {
		t.Errorf("fetched = %q, account = %q", body.Fetched, body.Account)
	}

	// Three advertised ids: the default itself, and two aliases that resolve to it.
	if len(body.Served) != 4 {
		t.Fatalf("served = %+v, want the three advertised ids plus the forwarded one", body.Served)
	}
	first := body.Served[0]
	if first.ID != "swe-1-6-slow" || first.UID != "swe-1-6-slow" || !first.Default || first.Alias {
		t.Errorf("the default row is wrong: %+v", first)
	}
	if alias := body.Served[1]; alias.ID != "swe-1.6" || alias.UID != "swe-1-6-slow" || !alias.Alias || alias.Default {
		t.Errorf("an alias row is wrong: %+v", alias)
	}
	if fwd := body.Served[3]; fwd.ID != "swe-1-6-fast" || !fwd.Forwarded || !fwd.InCatalogue {
		t.Errorf("the forwarded row is wrong: %+v", fwd)
	}

	// The details carry the numbers the panel exists to show, and the gated model is
	// one of them: that flag is how "swe-1-6-fast is refused on this plan" is visible
	// before a client hits it.
	if len(body.Details) != 2 {
		t.Fatalf("details = %+v", body.Details)
	}
	slow := body.Details[0]
	if slow.UID != "swe-1-6-slow" || slow.ContextWindow != 200000 || slow.MaxOutput != 128000 || slow.PlanGated {
		t.Fatalf("details for the served model = %+v", slow)
	}
	if len(slow.Prices) != 1 || slow.Prices[0].Label != "Input" || slow.Prices[0].Amount != 0.5 {
		t.Fatalf("prices = %+v", slow.Prices)
	}
	if fast := body.Details[1]; fast.UID != "swe-1-6-fast" || !fast.PlanGated {
		t.Fatalf("the gated model is not reported as gated: %+v", fast)
	}
	if len(body.Available) != 1 || body.Available[0].UID != "swe-1-6-slow" {
		t.Fatalf("available = %+v", body.Available)
	}

	// The Accounts pool's test control reads these: every catalogue uid, served
	// first and gated last, plus the id to default to. It has to be the whole
	// catalogue rather than just what this proxy forwards, because "does this
	// account serve *this* model" is a question the gated ids answer too.
	if len(body.ProbeModels) != 2 {
		t.Fatalf("probe_models = %+v, want every catalogue uid", body.ProbeModels)
	}
	probed := body.ProbeModels[0]
	if probed.UID != "swe-1-6-slow" || !probed.Served || probed.Gated {
		t.Errorf("the first probe entry is wrong: %+v", probed)
	}
	if gatedEntry := body.ProbeModels[1]; gatedEntry.UID != "swe-1-6-fast" || !gatedEntry.Gated {
		// swe-1-6-fast is forwarded by this proxy *and* gated on this plan, so it is
		// both; the page keeps each uid in one group and says there that the plan
		// gates it, rather than listing it twice.
		t.Errorf("the gated entry is wrong: %+v", gatedEntry)
	}
	if body.ProbeDefault != "swe-1-6-slow" {
		t.Errorf("probe_default = %q, want the model this proxy serves", body.ProbeDefault)
	}

	// A second read is served from the cache: reading a few hundred kilobytes of
	// catalogue on every page load would be a cost with nothing to show for it.
	if again := get(t, d, "/dashboard/api/models", cookie); again.Code != http.StatusOK {
		t.Fatalf("second read: status %d", again.Code)
	}
	if asked != 1 {
		t.Errorf("the backend was asked %d times for a catalogue that should be cached", asked)
	}
	// ...and asking for a refresh does go back to it.
	if refreshed := get(t, d, "/dashboard/api/models?refresh=1", cookie); refreshed.Code != http.StatusOK {
		t.Fatalf("refresh: status %d", refreshed.Code)
	}
	if asked != 2 {
		t.Errorf("the backend was asked %d times, want 2 after a refresh", asked)
	}
}

// A failed catalogue read is reported, and the panel is told so rather than being
// shown an empty catalogue that would read as "this account has no models".
func TestModelsEndpointReportsAFailedRead(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer stub.Close()

	store, err := OpenStore(filepath.Join(t.TempDir(), "dashboard.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.SetPasswordText("correct-horse-battery"); err != nil {
		t.Fatalf("SetPasswordText: %v", err)
	}
	d := New(Options{
		Pool:   devin.NewPool([]string{"devin-session-token$aaaaaa"}, stub.URL),
		Store:  store,
		Events: eventlog.New(64),
		Client: devin.NewClient(devin.Options{}),
		Info:   Info{Model: "swe-1-6-slow", Models: []string{"swe-1-6-slow"}},
	})
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))

	var body struct {
		Error   string `json:"error"`
		Fetched string `json:"fetched"`
		Served  []struct {
			ID string `json:"id"`
		} `json:"served"`
	}
	rec := get(t, d, "/dashboard/api/models", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error == "" {
		t.Fatal("a failed catalogue read was not reported")
	}
	if body.Fetched != "" {
		t.Error("a failed read reported a fetch time")
	}
	// The served list is still there to draw: it comes from configuration and needs
	// no backend at all.
	if len(body.Served) != 1 || body.Served[0].ID != "swe-1-6-slow" {
		t.Fatalf("served = %+v", body.Served)
	}
}

// The Models tab is drawn from the catalogue endpoint, and the chat-model
// entitlement list the status call reports is deliberately no longer shown on it:
// those models cannot be driven through this proxy.
func TestTheModelsTabIsWiredIntoThePage(t *testing.T) {
	d := testDashboard(t)
	page := get(t, d, "/dashboard/").Body.String()
	if !strings.Contains(page, `["models", "Models"]`) {
		t.Error("the page has no Models tab")
	}
	if !strings.Contains(page, `S.tab === "models") renderModels(body)`) {
		t.Error("nothing draws the Models tab")
	}
	for _, want := range []string{
		"function renderModels(", "function modelBlock(", "function loadModels(",
		"/models", "context_window", "max_output", "plan_gated", "S.info.models",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not mention %s", want)
		}
	}
	if strings.Contains(page, "Account entitlements") {
		t.Error("the Models tab still shows the account entitlements: those are Devin's chat models, " +
			"which this proxy cannot serve")
	}
}

// The Test button is the one control on the page that spends the account's own
// quota, so what it does with the answer matters as much as the answer itself: a
// served completion lifts the account's cooldown, and a refusal must not bench an
// account that is perfectly healthy — the operator can aim the probe at a model the
// plan does not include, and the backend answers that with a 200 carrying
// permission_denied inside the stream.

// probeStub answers the chat path the way the backend does: Connect frames under a
// 200, with the refusal inside the stream when the test asks for one. It records the
// credential it was asked with, so a test can check the operator's chosen account was
// the one probed.
func probeStub(t *testing.T, refuse string) (*httptest.Server, *[]string) {
	t.Helper()
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != devin.GetChatMessagePath {
			t.Errorf("the probe posted to %s, want %s", r.URL.Path, devin.GetChatMessagePath)
		}
		auths = append(auths, r.Header.Get("Authorization"))

		if refuse != "" {
			// The shape a refusal really takes: HTTP 200, the error in the last frame.
			w.Write(devin.EncodeFrame(devin.FrameEndStream,
				[]byte(`{"error":{"code":"`+refuse+`","message":"this model needs a higher plan"}}`)))
			return
		}
		frame := pb.NewWriter()
		frame.String(3, "ok") // GetChatMessageResponse.delta_text
		w.Write(devin.EncodeFrame(devin.FrameData, frame.Bytes()))
		w.Write(devin.EncodeFrame(devin.FrameEndStream, []byte("{}")))
	}))
	t.Cleanup(srv.Close)
	return srv, &auths
}

// probingDashboard builds a dashboard whose pool points at a stub backend, and signs
// in. The pool is given a status source that always answers, so a quota cooldown can
// be put on an account without any call leaving the process.
func probingDashboard(t *testing.T, srv *httptest.Server) (*Dashboard, *http.Cookie) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "dashboard.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.SetPasswordText("correct-horse-battery"); err != nil {
		t.Fatalf("SetPasswordText: %v", err)
	}
	pool := devin.NewPool([]string{"devin-session-token$aaaaaa", "devin-session-token$bbbbbb"}, srv.URL)
	d := New(Options{
		Pool:   pool,
		Store:  store,
		Events: eventlog.New(64),
		Info:   Info{Model: "swe-1-6-slow", Models: []string{"swe-1-6-slow"}},
	})
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))
	return d, cookie
}

// accountTestResult is the response shape the page reads.
type accountTestResult struct {
	Probe struct {
		OK          bool   `json:"ok"`
		Model       string `json:"model"`
		StatusCode  int    `json:"status_code"`
		ErrorCode   string `json:"error_code"`
		Error       string `json:"error"`
		Text        string `json:"text"`
		ModelServed string `json:"model_served"`
	} `json:"probe"`
	Cleared  []string `json:"cleared"`
	Accounts []struct {
		Index     int  `json:"index"`
		Available bool `json:"available"`
	} `json:"accounts"`
}

func TestTestingAnAccountLiftsItsCooldownWhenItServesTheCompletion(t *testing.T) {
	stub, auths := probeStub(t, "")
	d, cookie := probingDashboard(t, stub)

	// Put account 0 out of rotation the way the backend would have: a 429.
	d.cfg.Pool.Report(d.cfg.Pool.CredentialAt(0), http.StatusTooManyRequests, nil)
	if got := d.cfg.Pool.Available(); got != 1 {
		t.Fatalf("Available = %d before the test, want 1 while account 0 is cooling", got)
	}

	rec := post(t, d, "/dashboard/api/accounts/test", map[string]any{"index": 0, "model": "swe-1-6-slow"}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var got accountTestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Probe.OK || got.Probe.StatusCode != http.StatusOK {
		t.Fatalf("probe = %+v, want a served completion", got.Probe)
	}
	if got.Probe.Text != "ok" {
		t.Errorf("probe text = %q, want the model's own words", got.Probe.Text)
	}
	if got.Probe.Model != "swe-1-6-slow" {
		t.Errorf("probe model = %q, want the model that was asked for", got.Probe.Model)
	}
	if len(got.Cleared) != 1 || got.Cleared[0] != "rotation" {
		t.Fatalf("cleared = %v, want the rotation cooldown", got.Cleared)
	}
	// The listing comes back with the probe so the row redraws as available.
	if len(got.Accounts) != 2 || !got.Accounts[0].Available || !got.Accounts[1].Available {
		t.Fatalf("accounts = %+v, want both available once the cooldown is lifted", got.Accounts)
	}
	if got := d.cfg.Pool.Available(); got != 2 {
		t.Fatalf("Available = %d after the test, want 2", got)
	}

	// The account named by the index is the one probed, not whichever the rotation
	// would have handed out. The stub keeps the whole Basic header, which carries the
	// token twice, so the tail identifies the account.
	if len(*auths) != 1 {
		t.Fatalf("the backend was asked %d times, want 1", len(*auths))
	}
	if !strings.Contains((*auths)[0], "aaaaaa") {
		t.Errorf("the probe used %q, want account 0's credential", (*auths)[0])
	}
	if strings.Contains((*auths)[0], "bbbbbb") {
		t.Errorf("the probe used account 1's credential: %q", (*auths)[0])
	}
}

func TestTestingAnAccountWithAGatedModelDoesNotBenchIt(t *testing.T) {
	// Pointing a test at a model the plan does not cover is a reasonable thing to do;
	// it must not cost the account its place in rotation. This is the failure the
	// design comment in test.go is about.
	stub, _ := probeStub(t, "permission_denied")
	d, cookie := probingDashboard(t, stub)

	d.cfg.Pool.Report(d.cfg.Pool.CredentialAt(0), http.StatusTooManyRequests, nil)

	rec := post(t, d, "/dashboard/api/accounts/test", map[string]any{"index": 0, "model": "swe-1-6-fast"}, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	var got accountTestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Probe.OK {
		t.Fatalf("probe.ok = true although the backend refused inside the 200: %+v", got.Probe)
	}
	if got.Probe.ErrorCode != "permission_denied" {
		t.Errorf("error_code = %q, want the backend's own code", got.Probe.ErrorCode)
	}
	if len(got.Cleared) != 0 {
		t.Fatalf("cleared = %v, want nothing: a refusal is not evidence the account works", got.Cleared)
	}
	if got := d.cfg.Pool.Available(); got != 1 {
		t.Fatalf("Available = %d, want the account still cooling: a failed test must change nothing", got)
	}
	if got := d.cfg.Pool.CredentialAt(0); got == nil {
		t.Fatal("account 0 disappeared")
	}
}

func TestTestingAnAccountRefusesInputItCannotActOn(t *testing.T) {
	stub, auths := probeStub(t, "")
	d, cookie := probingDashboard(t, stub)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no such account", map[string]any{"index": 7, "model": "swe-1-6-slow"}, "no such account"},
		{"a negative index", map[string]any{"index": -1, "model": "swe-1-6-slow"}, "no such account"},
		// A body that names no account must not default to slot 0 and spend it.
		{"no account at all", map[string]any{"model": "swe-1-6-slow"}, "account"},
		{"no model", map[string]any{"index": 0, "model": ""}, "model"},
		{"a blank model", map[string]any{"index": 0, "model": "   "}, "model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, d, "/dashboard/api/accounts/test", tc.body, cookie)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (body %s)", rec.Code, rec.Body)
			}
			var errBody struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !strings.Contains(errBody.Error, tc.want) {
				t.Errorf("error = %q, want it to mention %q", errBody.Error, tc.want)
			}
		})
	}
	// A request that cannot be acted on must not spend anyone's quota.
	if len(*auths) != 0 {
		t.Fatalf("the backend was asked %d times for requests that should have been refused first", len(*auths))
	}
}

func TestTestingAnEmptyPoolIsRefusedRatherThanProbed(t *testing.T) {
	stub, _ := probeStub(t, "")
	d, cookie := probingDashboard(t, stub)
	for d.cfg.Pool.Len() > 0 {
		d.cfg.Pool.Remove(0)
	}
	rec := post(t, d, "/dashboard/api/accounts/test", map[string]any{"index": 0, "model": "swe-1-6-slow"}, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", rec.Code, rec.Body)
	}
}

func TestTheTestControlIsWiredIntoThePage(t *testing.T) {
	d := testDashboard(t)
	page := get(t, d, "/dashboard/").Body.String()
	for _, want := range []string{
		"function probeModelPicker(", "function probeModel(", "function testAccount(",
		"function probeNote(", "function ensureProbeModels(",
		`"/accounts/test"`, "probe_models", "probe_default",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not mention %s", want)
		}
	}
	// The dropdown has to offer the backend's own ids and default to the model this
	// proxy serves, and the picker's groups must be drawn from the catalogue flags.
	for _, want := range []string{"Served at /v1", "Available on this plan", "Gated behind a higher plan"} {
		if !strings.Contains(page, want) {
			t.Errorf("the picker has no %q group", want)
		}
	}
	// The cost of the button is stated where it is pressed: a test is a real request.
	if !strings.Contains(page, "spends a little") {
		t.Error("the page does not say that a test spends the account's quota")
	}
}

func TestOverviewReportsBothPools(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)
	got := get(t, d, "/dashboard/api/overview", cookie)
	if got.Code != http.StatusOK {
		t.Fatalf("overview: status %d (body %s)", got.Code, got.Body)
	}
	var body struct {
		Pool struct {
			Accounts int `json:"accounts"`
		} `json:"pool"`
		Routes struct {
			Count int `json:"count"`
		} `json:"routes"`
		Events struct {
			Enabled bool `json:"enabled"`
		} `json:"events"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Pool.Accounts != 2 {
		t.Errorf("pool.accounts = %d, want 2", body.Pool.Accounts)
	}
	if body.Routes.Count != 0 {
		t.Errorf("routes.count = %d, want 0", body.Routes.Count)
	}
	if !body.Events.Enabled {
		t.Error("events.enabled is false, but a hub was supplied")
	}
}

func TestARequestBodyThatIsTooLargeOrUnknownIsRefused(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	// An unknown field is refused rather than ignored, so a client that misspells a
	// parameter is told instead of silently having nothing done.
	req := httptest.NewRequest(http.MethodPost, "/dashboard/api/accounts/delete",
		strings.NewReader(`{"idx":0}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	req.AddCookie(cookie)
	out := httptest.NewRecorder()
	d.ServeHTTP(out, req)
	if out.Code != http.StatusBadRequest {
		t.Fatalf("a body with an unknown field: status %d, want 400 (body %s)", out.Code, out.Body)
	}
	if d.cfg.Pool.Len() != 2 {
		t.Error("a malformed body changed the pool")
	}
}

// The route listing is what the proxy pool panel draws from, and an empty list has to
// arrive as an empty list. It used to arrive as null, which the page then read as
// `.length` on nothing and threw on — so the whole panel failed to draw, and the
// textarea an operator needs to paste a proxy into was never created at all. The
// symptom was "the proxy pool is broken and shows me nothing", with no error
// anywhere, because the fault was a null where a list was promised.
func TestAnEmptyRouteListingIsAnEmptyListNotNull(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	listed := get(t, d, "/dashboard/api/proxies", cookie)
	if listed.Code != http.StatusOK {
		t.Fatalf("listing routes: status %d (body %s)", listed.Code, listed.Body)
	}
	assertListField(t, listed.Body.Bytes(), "routes")
	assertListField(t, listed.Body.Bytes(), "specs")

	// The save response is redrawn from too, so it carries the same contract.
	saved := post(t, d, "/dashboard/api/proxies", map[string]any{"text": ""}, cookie)
	if saved.Code != http.StatusOK {
		t.Fatalf("clearing routes: status %d (body %s)", saved.Code, saved.Body)
	}
	assertListField(t, saved.Body.Bytes(), "specs")
	assertListField(t, saved.Body.Bytes(), "routes")
}

// encodeTestCatalogueForDashboard builds a two-model catalogue in the live shape: one
// model with limits and a priced row, one gated behind a higher plan.
func encodeTestCatalogueForDashboard() []byte {
	w := pb.NewWriter()
	w.Message(1, func(m *pb.Writer) {
		m.String(1, "SWE-1.6 Slow")
		m.String(22, "swe-1-6-slow")
		m.Message(23, func(cfg *pb.Writer) {
			cfg.Varint(4, 200000)
			cfg.String(5, "LLAMA_WITH_SPECIAL")
			cfg.Varint(13, 128000)
		})
		m.Message(32, func(p *pb.Writer) {
			p.String(1, "Input")
			p.Float32(2, 0.5)
			p.String(3, "1M tokens")
		})
	})
	w.Message(1, func(m *pb.Writer) {
		m.String(1, "SWE-1.6 Fast")
		m.String(22, "swe-1-6-fast")
		m.Message(33, func(gate *pb.Writer) { gate.Varint(1, 1) })
	})
	return w.Bytes()
}

// assertListField checks one field of a response body is a JSON array, and specifically
// not null. It compares the raw bytes rather than decoding into a slice, because
// decoding turns null and [] into the same nil slice and would hide the bug entirely.
func assertListField(t *testing.T, body []byte, field string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	raw, ok := fields[field]
	if !ok {
		t.Fatalf("%q is missing from %s", field, body)
	}
	got := strings.TrimSpace(string(raw))
	if got != "[]" && !strings.HasPrefix(got, "[{") {
		t.Errorf("%q = %s, want a JSON array (null is not a list): %s", field, got, body)
	}
}

// Signing the CLI out deletes the only copy of the account it was holding, so the
// account has to reach the pool first. This is the invariant that keeps an operator who
// signs out and then fails to sign in as anybody else from being left with nothing.
func TestKeepingTheSignedInAccountSurvivesTheSignOut(t *testing.T) {
	dir := t.TempDir()
	kept := devinTokenPrefix + "kept-account"

	// The pool starts empty, which is the state this matters most in: the CLI's own
	// credential is the only one the machine has.
	d := New(Options{
		Pool:   devin.NewPool(nil, "http://127.0.0.1:1"),
		Store:  mustStore(t, filepath.Join(dir, "dashboard.json")),
		Events: eventlog.New(64),
	})

	d.keepCredential(context.Background(), kept)

	if d.cfg.Pool.Len() != 1 {
		t.Fatalf("pool size = %d after keeping the signed-in account, want 1", d.cfg.Pool.Len())
	}
	if got := d.cfg.Pool.Tokens()[0]; got != kept {
		t.Errorf("pool holds %q, want the account that was signed in", got)
	}
	// Persisted, so a sign-out followed by a crash still leaves the account recoverable.
	if stored := d.cfg.Store.Accounts(); len(stored) != 1 || stored[0] != kept {
		t.Errorf("stored accounts = %v, want the kept account", stored)
	}

	// Keeping the same account again is a no-op, not a duplicate.
	d.keepCredential(context.Background(), kept)
	if d.cfg.Pool.Len() != 1 {
		t.Errorf("pool size = %d after keeping the same account twice, want 1", d.cfg.Pool.Len())
	}

	// A value the backend cannot authenticate is not kept: it would join the rotation
	// and fail every request it was handed.
	d.keepCredential(context.Background(), "not-a-devin-token")
	if d.cfg.Pool.Len() != 1 {
		t.Errorf("pool size = %d after a token in the wrong format, want 1", d.cfg.Pool.Len())
	}
}

// The sign-out runs the CLI, so it must not be run with no arguments: the CLI with no
// subcommand prints its help text instead of signing anything out.
func TestSignOutRefusesWithoutAConfiguredCommand(t *testing.T) {
	d := testDashboard(t)
	if _, err := d.runCLILogout(context.Background()); err == nil {
		t.Error("runCLILogout ran the CLI with no arguments instead of refusing")
	}
}

// Taking the signed-in account is how an account added in a terminal reaches the pool.
// The credential comes from a file this test owns, so the real one is never read and
// the backend address is a closed port, which keeps every call off the network.
func TestCapturingTheSignedInAccount(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "credentials.toml")
	token := devinTokenPrefix + "captured-account"
	writeCreds := func(t *testing.T, content string) {
		t.Helper()
		if err := os.WriteFile(credsPath, []byte(content), 0o600); err != nil {
			t.Fatalf("write credentials: %v", err)
		}
	}
	writeCreds(t, "windsurf_api_key = \""+token+"\"\napi_server_url = \"http://127.0.0.1:1\"\n")
	t.Setenv("DEVIN_CREDENTIALS_PATH", credsPath)
	t.Setenv("WINDSURF_API_SERVER_URL", "http://127.0.0.1:1")

	d := New(Options{
		Pool:   devin.NewPool(nil, "http://127.0.0.1:1"),
		Store:  lockedStore(t, filepath.Join(dir, "dashboard.json")),
		Events: eventlog.New(64),
	})
	login := func(t *testing.T) *http.Cookie {
		t.Helper()
		rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
		return sessionCookieFrom(t, rec)
	}
	cookie := login(t)

	taken := post(t, d, "/dashboard/api/accounts/capture", nil, cookie)
	if taken.Code != http.StatusOK {
		t.Fatalf("capturing the signed-in account: status %d (body %s)", taken.Code, taken.Body)
	}
	if d.cfg.Pool.Len() != 1 || d.cfg.Pool.Tokens()[0] != token {
		t.Fatalf("pool = %v, want the captured account", d.cfg.Pool.Tokens())
	}
	if stored := d.cfg.Store.Accounts(); len(stored) != 1 || stored[0] != token {
		t.Errorf("stored accounts = %v, want the captured account", stored)
	}
	// The response must name the account by its tail, never by its token — the same
	// rule the listing follows. The tail is bare here, as it is in every account
	// listing; the leading ellipsis belongs to the log lines, not to the JSON.
	if strings.Contains(taken.Body.String(), token) {
		t.Error("the capture response carried the token itself")
	}
	if !strings.Contains(taken.Body.String(), `"tail":"`+tailOf(token)+`"`) {
		t.Errorf("the capture response does not name the account: %s", taken.Body)
	}

	// Capturing the same account twice is refused rather than silently duplicated.
	again := post(t, d, "/dashboard/api/accounts/capture", nil, cookie)
	if again.Code != http.StatusConflict {
		t.Errorf("capturing the same account twice: status %d, want 409 (body %s)", again.Code, again.Body)
	}
	if d.cfg.Pool.Len() != 1 {
		t.Errorf("pool size = %d after capturing twice, want 1", d.cfg.Pool.Len())
	}

	// A credential the backend cannot authenticate is refused, so it cannot join the
	// rotation and fail every request it is handed.
	writeCreds(t, "windsurf_api_key = \"some-other-token\"\napi_server_url = \"http://127.0.0.1:1\"\n")
	if bad := post(t, d, "/dashboard/api/accounts/capture", nil, cookie); bad.Code != http.StatusBadRequest {
		t.Errorf("capturing a credential in the wrong format: status %d, want 400 (body %s)", bad.Code, bad.Body)
	}
	if d.cfg.Pool.Len() != 1 {
		t.Errorf("pool size = %d after a refused capture, want 1", d.cfg.Pool.Len())
	}

	// With the CLI signed out there is nothing to take, and the answer has to say so
	// rather than reporting an account that was not added.
	if err := os.Remove(credsPath); err != nil {
		t.Fatalf("remove credentials: %v", err)
	}
	if gone := post(t, d, "/dashboard/api/accounts/capture", nil, cookie); gone.Code != http.StatusConflict {
		t.Errorf("capturing with the CLI signed out: status %d, want 409 (body %s)", gone.Code, gone.Body)
	}
	if d.cfg.Pool.Len() != 1 {
		t.Errorf("pool size = %d after capturing nothing, want 1", d.cfg.Pool.Len())
	}
}

// The sign-out endpoint is the one CLI step that needs no terminal, and it must keep the
// account it is signing out of. The CLI is the test binary re-run with no matching test,
// which starts, prints nothing and exits 0 on every platform.
func TestSigningOutKeepsTheAccountItRemoves(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "credentials.toml")
	token := devinTokenPrefix + "signing-out"
	if err := os.WriteFile(credsPath,
		[]byte("windsurf_api_key = \""+token+"\"\napi_server_url = \"http://127.0.0.1:1\"\n"), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	t.Setenv("DEVIN_CREDENTIALS_PATH", credsPath)
	t.Setenv("WINDSURF_API_SERVER_URL", "http://127.0.0.1:1")

	d := New(Options{
		Pool:       devin.NewPool(nil, "http://127.0.0.1:1"),
		Store:      lockedStore(t, filepath.Join(dir, "dashboard.json")),
		Events:     eventlog.New(64),
		DevinCLI:   os.Args[0],
		LogoutArgs: []string{"-test.run=^$"},
	})
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	cookie := sessionCookieFrom(t, rec)

	if got := post(t, d, "/dashboard/api/accounts/logout", nil, cookie); got.Code != http.StatusOK {
		t.Fatalf("signing the CLI out: status %d (body %s)", got.Code, got.Body)
	}
	// The whole point: the account the CLI was holding is still in the pool afterwards.
	if d.cfg.Pool.Len() != 1 || d.cfg.Pool.Tokens()[0] != token {
		t.Fatalf("pool = %v after signing out, want the account that was signed out", d.cfg.Pool.Tokens())
	}
	if stored := d.cfg.Store.Accounts(); len(stored) != 1 || stored[0] != token {
		t.Errorf("stored accounts = %v, want the account that was signed out", stored)
	}
}

func mustStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return store
}

// lockedStore is a store with the test password already set. The tests that build a
// dashboard by hand rather than through testDashboard need it: without a password every
// sign-in is refused, and a test that never gets a session fails on the cookie lookup
// rather than on the thing it means to check.
func lockedStore(t *testing.T, path string) *Store {
	t.Helper()
	store := mustStore(t, path)
	if err := store.SetPasswordText("correct-horse-battery"); err != nil {
		t.Fatalf("SetPasswordText: %v", err)
	}
	return store
}

// The CLI draws its prompt with terminal control sequences, so what it writes is not
// the text a person sees. Left in, the sign-in URL arrives wrapped in cursor and colour
// codes and the page shows "[?2004h[?2026h" instead of the link — which is exactly what
// the login modal did before this.
func TestTheCLIOutputIsStrippedOfTerminalControlSequences(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "bracketed paste and cursor sequences around a prompt",
			in:   "\x1b[?2004h\x1b[?2026hCode:",
			want: "Code:",
		},
		{
			name: "colour around a word",
			in:   "\x1b[1m\x1b[38;5;81m\x1b[48;5;234m? \x1b[0mPaste the code from the sign-in page\x1b[K",
			want: "? Paste the code from the sign-in page",
		},
		{
			name: "the sign-in url survives intact",
			in:   "\x1b[?2004hVisit https://app.devin.ai/auth/cli/continue?state=abc&prompt=select_account to sign in\x1b[?2026h",
			want: "Visit https://app.devin.ai/auth/cli/continue?state=abc&prompt=select_account to sign in",
		},
		{
			name: "plain text is untouched",
			in:   "Logged out successfully",
			want: "Logged out successfully",
		},
	}
	for _, tc := range cases {
		if got := stripANSI(tc.in); got != tc.want {
			t.Errorf("%s: stripANSI(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
