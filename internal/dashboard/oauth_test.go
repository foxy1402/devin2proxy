package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/eventlog"
	"devin2proxy/internal/pb"
)

// The sign-in replaces the CLI entirely, so what it has to get right is the pair of
// secrets: the verifier stays in this process and the code is single-use. Both are
// what make a code copied off a browser page safe to paste into a dashboard.

// A started sign-in hands back a URL that carries a challenge, never the verifier,
// and the verifier it is holding really is the one behind that challenge.
func TestStartingASignInReturnsAURLWithoutTheVerifier(t *testing.T) {
	d := testDashboard(t)
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))

	rec := post(t, d, "/dashboard/api/accounts/oauth/start", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("starting a sign-in: status %d (body %s)", rec.Code, rec.Body)
	}
	var started struct {
		State string `json:"state"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if started.URL == "" || started.State == "" {
		t.Fatalf("no URL or state came back: %s", rec.Body)
	}
	for _, want := range []string{
		"https://app.devin.ai/auth/cli/continue",
		"code_challenge_method=S256",
		// Without the account picker the browser reuses whichever account is already
		// signed in, which is the whole reason a second account could not be added.
		"prompt=select_account",
		// What makes the page show a code to copy.
		"cli_pkce_marker=1",
		"state=" + started.State,
	} {
		if !strings.Contains(started.URL, want) {
			t.Errorf("the sign-in URL is missing %q: %s", want, started.URL)
		}
	}
	// No redirect_uri: the site checks that parameter against an allowlist of loopback
	// callbacks and answers anything else with "Invalid redirect URI", which is what
	// made the first version of this flow a dead link once the browser was signed in.
	if strings.Contains(started.URL, "redirect_uri") {
		t.Errorf("the sign-in URL names a redirect URI, which the site refuses: %s", started.URL)
	}
	// The state is in the URL; the verifier is not, and must not be.
	// Pending sign-ins are counted on the map itself: the flow struct has no
	// counter of its own for the tests to lean on.
	if len(d.logins.flows) != 1 {
		t.Fatalf("pending sign-ins = %d, want 1", len(d.logins.flows))
	}
	for _, flow := range d.logins.flows {
		if flow.verifier == "" {
			t.Fatal("the pending sign-in has no verifier to exchange with")
		}
		if strings.Contains(started.URL, flow.verifier) {
			t.Error("the sign-in URL contains the verifier")
		}
	}
}

// An expired or already-used code has to be refused with something that says what to
// do, because the fix is always the same: start again.
func TestFinishingWithoutAValidSignInIsRefused(t *testing.T) {
	d := testDashboard(t)
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))

	// No sign-in started at all.
	rec := post(t, d, "/dashboard/api/accounts/oauth/finish", map[string]string{"code": "abc"}, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("finishing with nothing started: status %d, want 400 (body %s)", rec.Code, rec.Body)
	}

	// An unknown state is a stale page, and is told to start again.
	rec = post(t, d, "/dashboard/api/accounts/oauth/finish", map[string]string{"state": "nope", "code": "abc"}, cookie)
	if rec.Code != http.StatusGone {
		t.Errorf("finishing an unknown sign-in: status %d, want 410 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "start") {
		t.Errorf("the refusal does not say what to do: %s", rec.Body)
	}

	// An empty code is refused before anything is sent anywhere.
	rec = post(t, d, "/dashboard/api/accounts/oauth/finish", map[string]string{"state": "nope", "code": "  "}, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("finishing with no code: status %d, want 400 (body %s)", rec.Code, rec.Body)
	}
}

// A sign-in is consumed by the first attempt, whatever the outcome. If it survived a
// failed exchange, a retry loop could keep presenting codes against one verifier, and
// the state would be a handle for as long as the process lives.
func TestASignInIsConsumedByItsFirstAttempt(t *testing.T) {
	// The exchange is pointed at a stub that refuses everything, so the attempt fails
	// the way a stale code does. What is being checked is that the flow is gone
	// afterwards either way.
	stub := newBackendStub(t)
	dir := t.TempDir()
	d := New(Options{
		Pool:         devin.NewPool(nil, stub.server.URL),
		Store:        lockedStore(t, dir+"/dashboard.json"),
		Events:       eventlog.New(64),
		APIServerURL: stub.server.URL,
	})
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))
	rec := post(t, d, "/dashboard/api/accounts/oauth/start", nil, cookie)
	var started struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	first := post(t, d, "/dashboard/api/accounts/oauth/finish",
		map[string]string{"state": started.State, "code": "not-a-real-code"}, cookie)
	if first.Code == http.StatusOK {
		t.Fatalf("a made-up code was accepted: %s", first.Body)
	}
	if len(d.logins.flows) != 0 {
		t.Errorf("pending sign-ins = %d after an attempt, want 0", len(d.logins.flows))
	}
	second := post(t, d, "/dashboard/api/accounts/oauth/finish",
		map[string]string{"state": started.State, "code": "not-a-real-code"}, cookie)
	if second.Code != http.StatusGone {
		t.Errorf("reusing a sign-in: status %d, want 410 (body %s)", second.Code, second.Body)
	}
}

// The sign-in is entirely on the Devin site now: the page states its code and the
// operator pastes it here. This proxy has no callback to land on, which is why the
// path a first version registered is gone rather than merely unused — a route that
// serves a page nobody can reach is a place for the next change to hide.
func TestThisProxyServesNoSignInCallback(t *testing.T) {
	d := testDashboard(t)
	// The old path now falls through to the dashboard page, which knows nothing about
	// a code. What matters is that nothing echoes one, and that no route claims it.
	stale := httptest.NewRequest(http.MethodGet, "/dashboard/oauth/callback?code=abc123&state=st", nil)
	stale.RemoteAddr = "127.0.0.1:54321"
	out := httptest.NewRecorder()
	d.ServeHTTP(out, stale)
	if strings.Contains(out.Body.String(), "abc123") {
		t.Errorf("a code from the query string reached the page: %s", out.Body)
	}
	// The root path is outside this handler's tree, and stays unrouted.
	root := httptest.NewRequest(http.MethodGet, "/callback?code=abc123&state=st", nil)
	root.RemoteAddr = "127.0.0.1:54321"
	rootOut := httptest.NewRecorder()
	d.ServeHTTP(rootOut, root)
	if rootOut.Code != http.StatusNotFound {
		t.Errorf("/callback: status %d, want 404 — nothing should serve a root callback", rootOut.Code)
	}
}

// The exchange is what the CLI used to do, and it is now done in-process. This is
// the whole flow end to end against a stub: start, exchange, verify the credential,
// and the account is in the pool under the token the exchange returned.
func TestSigningInAddsTheAccountItExchangesFor(t *testing.T) {
	stub := newBackendStub(t)
	token := devin.SessionTokenPrefix + "eyJhbGciOiJIUzI1NiJ9.eyJzZXNzaW9uX2lkIjoicyJ9.c2ln"
	stub.exchangeToken = token

	dir := t.TempDir()
	d := New(Options{
		Pool:       devin.NewPool(nil, stub.server.URL),
		Store:      lockedStore(t, dir+"/dashboard.json"),
		Events:     eventlog.New(64),
		WebappHost: "https://app.devin.ai",
		// Named explicitly: without it the exchange falls back to the CLI's stored
		// credential and the test would call the live backend.
		APIServerURL: stub.server.URL,
	})
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))

	rec := post(t, d, "/dashboard/api/accounts/oauth/start", nil, cookie)
	var started struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("unmarshal start: %v (%s)", err, rec.Body)
	}

	done := post(t, d, "/dashboard/api/accounts/oauth/finish",
		map[string]string{"state": started.State, "code": "the-code"}, cookie)
	if done.Code != http.StatusOK {
		t.Fatalf("finishing the sign-in: status %d (body %s)", done.Code, done.Body)
	}
	if d.cfg.Pool.Len() != 1 || d.cfg.Pool.Tokens()[0] != token {
		t.Fatalf("pool = %v, want the token the exchange returned", d.cfg.Pool.Tokens())
	}
	// The code and the verifier both went to the exchange, and nothing went with them
	// that could identify this proxy's own credentials.
	if stub.sawCode != "the-code" {
		t.Errorf("the exchange was sent code %q, want %q", stub.sawCode, "the-code")
	}
	if stub.sawVerifier == "" {
		t.Error("the exchange was sent no verifier")
	}
	if stub.sawAuth != "" {
		t.Errorf("the exchange carried an Authorization header (%q); a sign-in has no credential yet", stub.sawAuth)
	}
	// The pool's status call ran against the new credential, which is what proves the
	// token the exchange returned is one the backend authenticates.
	if !stub.sawStatus {
		t.Error("the new credential was never checked against the backend before being kept")
	}
	// The response names the account by tail, never by token.
	if strings.Contains(done.Body.String(), token) {
		t.Error("the sign-in response carried the token itself")
	}
}

// A token already in the pool must not be added twice: the same account twice would
// look like load sharing while spending one account's quota.
func TestSigningInTwiceAsTheSameAccountIsRefused(t *testing.T) {
	stub := newBackendStub(t)
	token := devin.SessionTokenPrefix + "eyJhbGciOiJIUzI1NiJ9.eyJzZXNzaW9uX2lkIjoicyJ9.c2ln"
	stub.exchangeToken = token

	dir := t.TempDir()
	d := New(Options{
		Pool:         devin.NewPool([]string{token}, stub.server.URL),
		Store:        lockedStore(t, dir+"/dashboard.json"),
		Events:       eventlog.New(64),
		APIServerURL: stub.server.URL,
	})
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))
	rec := post(t, d, "/dashboard/api/accounts/oauth/start", nil, cookie)
	var started struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)

	again := post(t, d, "/dashboard/api/accounts/oauth/finish",
		map[string]string{"state": started.State, "code": "the-code"}, cookie)
	if again.Code != http.StatusConflict {
		t.Errorf("signing in as an account already pooled: status %d, want 409 (body %s)", again.Code, again.Body)
	}
	if d.cfg.Pool.Len() != 1 {
		t.Errorf("pool size = %d, want 1", d.cfg.Pool.Len())
	}
}

// A credential the backend will not authenticate is refused rather than pooled: it
// would otherwise join the rotation and fail every request handed to it.
func TestACredentialTheBackendRefusesIsNotPooled(t *testing.T) {
	stub := newBackendStub(t)
	stub.exchangeToken = devin.SessionTokenPrefix + "eyJhbGciOiJIUzI1NiJ9.eyJzZXNzaW9uX2lkIjoicyJ9.c2ln"
	stub.statusStatus = http.StatusUnauthorized
	stub.statusBody = []byte(`{"code":"unauthenticated","message":"invalid api key"}`)

	dir := t.TempDir()
	d := New(Options{
		Pool:         devin.NewPool(nil, stub.server.URL),
		Store:        lockedStore(t, dir+"/dashboard.json"),
		Events:       eventlog.New(64),
		APIServerURL: stub.server.URL,
	})
	cookie := sessionCookieFrom(t, post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"}))
	rec := post(t, d, "/dashboard/api/accounts/oauth/start", nil, cookie)
	var started struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)

	done := post(t, d, "/dashboard/api/accounts/oauth/finish",
		map[string]string{"state": started.State, "code": "the-code"}, cookie)
	if done.Code != http.StatusBadGateway {
		t.Fatalf("a refused credential: status %d, want 502 (body %s)", done.Code, done.Body)
	}
	if d.cfg.Pool.Len() != 0 {
		t.Errorf("pool size = %d after a refused credential, want 0", d.cfg.Pool.Len())
	}
	if stored := d.cfg.Store.Accounts(); len(stored) != 0 {
		t.Errorf("a refused credential was stored: %v", stored)
	}
	if !strings.Contains(done.Body.String(), "refused") {
		t.Errorf("the refusal does not explain itself: %s", done.Body)
	}
}

// backendStub stands in for the seat-management service: the exchange and the status
// call share one host, so one stub covers the whole sign-in.
type backendStub struct {
	server        *httptest.Server
	exchangeToken string
	statusStatus  int
	statusBody    []byte

	sawCode     string
	sawVerifier string
	sawAuth     string
	sawStatus   bool
}

func newBackendStub(t *testing.T) *backendStub {
	t.Helper()
	s := &backendStub{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(r)
		switch {
		case strings.HasSuffix(r.URL.Path, "/ExchangeDevinCLIPKCECode"):
			s.sawCode, s.sawVerifier = decodeExchangeRequest(body)
			// Recorded here and not for every request: the status call that follows is
			// *supposed* to carry the new credential, so a shared field would say the
			// exchange was authenticated too.
			s.sawAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/proto")
			if s.exchangeToken == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"Invalid or expired code."}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(encodeStringField(1, s.exchangeToken))
		case strings.HasSuffix(r.URL.Path, "/GetUserStatus"):
			s.sawStatus = true
			w.Header().Set("Content-Type", "application/proto")
			status := s.statusStatus
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			if len(s.statusBody) > 0 {
				_, _ = w.Write(s.statusBody)
				return
			}
			_, _ = w.Write(accountStatusBody("pooled@example.com"))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func readBody(r *http.Request) []byte {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 128)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf
		}
	}
}

// decodeExchangeRequest pulls fields 1 and 2 out of the exchange body, which is how
// the stub can assert what was actually sent.
func decodeExchangeRequest(b []byte) (code, verifier string) {
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			return code, verifier
		}
		if wire != 2 {
			r.Skip(wire)
			continue
		}
		switch field {
		case 1:
			code = string(r.Bytes())
		case 2:
			verifier = string(r.Bytes())
		default:
			r.Skip(wire)
		}
	}
}

func encodeStringField(field int, s string) []byte {
	w := pb.NewWriter()
	w.String(field, s)
	return w.Bytes()
}

// accountStatusBody is the smallest response the status decoder accepts: the account
// block at field 1 with an email in it.
func accountStatusBody(email string) []byte {
	inner := pb.NewWriter()
	inner.String(2, email)
	w := pb.NewWriter()
	w.RawBytes(1, inner.Bytes())
	return w.Bytes()
}
