package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/eventlog"
)

// The log view's value rests entirely on what an event carries. A status code on
// its own is not a report of what happened — Devin reports a refused credential as
// HTTP 200 with the refusal inside the stream — so these tests pin the fields that
// make a row readable: the upstream status, the gateway's code, and the message the
// client was actually sent.

func observingServer(t *testing.T) (*Server, *eventlog.Hub) {
	t.Helper()
	hub := eventlog.New(64)
	srv := New(Config{
		APIKey: "sk-devin-test",
		Models: []string{"swe-1.6"},
		// A stub backend rather than no credential at all. With an empty pool the
		// handler would fall back to reading the CLI's real credentials file, and a
		// test that reaches the live backend spends the account's allowance — which
		// is exactly what happened once while writing these.
		Creds:  devin.NewPool([]string{"devin-session-token$stub"}, benchedBackend(t)),
		Events: hub,
	}, devin.NewClient(devin.Options{MaxConcurrent: 1}))
	return srv, hub
}

// benchedBackend is a backend that refuses everything, so a test can exercise the
// upstream paths without a network call.
func benchedBackend(t *testing.T) string {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(backend.Close)
	return backend.URL
}

func do(srv *Server, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestAnUnauthenticatedRequestIsRecordedWithItsError(t *testing.T) {
	srv, hub := observingServer(t)

	rec := do(srv, http.MethodPost, "/v1/chat/completions", "wrong-key",
		`{"model":"swe-1.6","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}

	events := hub.Recent()
	if len(events) != 1 {
		t.Fatalf("event count = %d, want exactly 1", len(events))
	}
	e := events[0]
	if e.Kind != "request" {
		t.Errorf("kind = %q, want request", e.Kind)
	}
	if e.Status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", e.Status)
	}
	if e.Path != "/v1/chat/completions" || e.Method != http.MethodPost {
		t.Errorf("recorded %s %s, want POST /v1/chat/completions", e.Method, e.Path)
	}
	// The message the client was told is the whole point: without it the row says
	// only "401", which is already visible in the response.
	if !strings.Contains(e.Error, "invalid API key") {
		t.Errorf("error = %q, want the message the client was sent", e.Error)
	}
	if e.Code != "invalid_api_key" {
		t.Errorf("code = %q, want invalid_api_key", e.Code)
	}
	if e.Level != eventlog.LevelWarn {
		t.Errorf("level = %q, want warn", e.Level)
	}
}

func TestARateLimitIsRecordedAsAQuotaRefusalWithItsCode(t *testing.T) {
	// A stub backend, so this exercises the real error path without ever spending
	// the account's allowance. A test that reached the live backend would be both
	// slow and expensive, and it would fail for anyone without a credential.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"you have used all your quota for today"}}`))
	}))
	defer backend.Close()

	hub := eventlog.New(64)
	srv := New(Config{
		APIKey: "sk-devin-test",
		Models: []string{"swe-1.6"},
		Creds:  devin.NewPool([]string{"devin-session-token$stub"}, backend.URL),
		Events: hub,
	}, devin.NewClient(devin.Options{MaxConcurrent: 1}))

	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 (body %s)", rec.Code, rec.Body)
	}

	events := hub.Recent()
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
	e := events[0]
	if e.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", e.Status)
	}
	if e.Code != "rate_limit_exceeded" {
		t.Errorf("code = %q, want rate_limit_exceeded", e.Code)
	}
	// The account is named, because "which account ran out" is the question the log
	// view exists to answer.
	if e.Account == "" {
		t.Error("the event does not name the account")
	}
	if strings.Contains(e.Account, "devin-session-token$") {
		t.Errorf("the event names the account as %q, which is the token itself", e.Account)
	}
	// And the message the client was given carries the backend's own text.
	if !strings.Contains(e.Error, "used all your quota") {
		t.Errorf("error = %q, want the backend's message", e.Error)
	}
	if e.DurationMS < 0 {
		t.Errorf("duration = %dms", e.DurationMS)
	}
}

func TestAMalformedRequestIsRecorded(t *testing.T) {
	srv, hub := observingServer(t)
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test", `{"model":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	events := hub.Recent()
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
	if events[0].Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", events[0].Status)
	}
	if !strings.Contains(events[0].Error, "invalid JSON body") {
		t.Errorf("error = %q, want the parse failure", events[0].Error)
	}
}

func TestTheDashboardIsNotRepresentedAsARequest(t *testing.T) {
	srv, hub := observingServer(t)
	do(srv, http.MethodGet, "/dashboard/api/session", "", "")
	do(srv, http.MethodGet, "/healthz", "", "")
	// The dashboard's own polling would otherwise be most of what the log view
	// shows, burying the requests the view exists for.
	if events := hub.Recent(); len(events) != 0 {
		t.Fatalf("recorded %d event(s) for non-/v1 requests: %+v", len(events), events)
	}
}

func TestAnUnsupportedRouteIsRecordedWithItsCode(t *testing.T) {
	srv, hub := observingServer(t)
	rec := do(srv, http.MethodPost, "/v1/embeddings", "sk-devin-test", `{}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", rec.Code)
	}
	events := hub.Recent()
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
	if events[0].Code != "not_implemented" {
		t.Errorf("code = %q, want not_implemented", events[0].Code)
	}
}

func TestObservingIsOffWhenThereIsNoHub(t *testing.T) {
	srv := New(Config{
		APIKey: "sk-devin-test",
		Models: []string{"swe-1.6"},
		Creds:  devin.NewPool([]string{"devin-session-token$stub"}, benchedBackend(t)),
	}, devin.NewClient(devin.Options{MaxConcurrent: 1}))
	// The point is only that nothing panics and the response is still correct: the
	// observation path must be strictly additive.
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "wrong",
		`{"model":"swe-1.6","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}

func TestMaskedRouteSpecsAreRecognisable(t *testing.T) {
	const spec = "socks5://alice:hunter2@127.0.0.1:1080"
	masked := devin.MaskSpec(spec)
	if strings.Contains(masked, "hunter2") || strings.Contains(masked, "alice") {
		t.Fatalf("MaskSpec(%q) = %q, which still carries the credentials", spec, masked)
	}
	if !devin.HasMaskedCredentials(masked) {
		t.Errorf("MaskSpec(%q) = %q, which is not recognisable as masked", spec, masked)
	}
	if !strings.Contains(masked, "127.0.0.1:1080") {
		t.Errorf("MaskSpec(%q) = %q, which lost the host", spec, masked)
	}
	// An @ inside a password must not be mistaken for the end of the userinfo.
	tricky := devin.MaskSpec("socks5://user:p@ss@127.0.0.1:1080")
	if !strings.Contains(tricky, "127.0.0.1:1080") || strings.Contains(tricky, "p@ss") {
		t.Errorf("MaskSpec of a password containing @ = %q", tricky)
	}
	// A route with no credentials is left exactly as it is.
	if got := devin.MaskSpec("http://127.0.0.1:8080"); got != "http://127.0.0.1:8080" {
		t.Errorf("MaskSpec of a credential-free route = %q", got)
	}
}

func TestAClientDisconnectOnTheNonStreamingRelaysIsQuiet(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"swe-1.6","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/completions", `{"model":"swe-1.6","prompt":"hi"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			// The backend sends one frame — enough for the proxy's connect phase
			// to succeed — and then hangs, so the client's hang-up lands inside
			// the relay loop the way an IDE's cancel does.
			release := make(chan struct{})
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write(textFrame("half an answer", 0, false))
				<-release
			}))
			t.Cleanup(func() { close(release); backend.Close() })

			hub := eventlog.New(64)
			srv := New(Config{
				APIKey: "sk-devin-test",
				Models: []string{"swe-1.6"},
				Creds:  devin.NewPool([]string{"devin-session-token$stub"}, backend.URL),
				Events: hub,
			}, devin.NewClient(devin.Options{MaxConcurrent: 1}))
			api := httptest.NewServer(srv)
			t.Cleanup(api.Close)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer sk-devin-test")
			req.Header.Set("Content-Type", "application/json")

			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = http.DefaultClient.Do(req)
			}()
			// Give the relay time to get past the connect phase — the stub
			// answers its first frame immediately — then hang up.
			time.Sleep(250 * time.Millisecond)
			cancel()
			<-done

			// The event is emitted when the handler returns; wait for it.
			var e eventlog.Event
			for deadline := time.Now().Add(5 * time.Second); ; {
				if evs := hub.Recent(); len(evs) == 1 {
					e = evs[0]
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the request was never recorded")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// A hang-up is routine and has no reader for a 502: it must not be
			// recorded as an upstream failure carrying an error body's code.
			if e.Level == eventlog.LevelError {
				t.Errorf("a disconnect was recorded at error level: %+v", e)
			}
			if e.Code != "" {
				t.Errorf("a disconnect was recorded with code %q; no error body was written", e.Code)
			}
		})
	}
}

func TestMethodNotAllowedNamesTheAllowedMethod(t *testing.T) {
	srv, _ := observingServer(t)
	for _, tc := range []struct{ method, path, wantAllow string }{
		{http.MethodGet, "/v1/chat/completions", http.MethodPost},
		{http.MethodPut, "/v1/completions", http.MethodPost},
		{http.MethodPost, "/v1/models", "GET, HEAD"},
		{http.MethodPost, "/healthz", "GET, HEAD"},
	} {
		rec := do(srv, tc.method, tc.path, "sk-devin-test", "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, rec.Code)
			continue
		}
		if allow := rec.Header().Get("Allow"); allow != tc.wantAllow {
			t.Errorf("%s %s Allow = %q, want %q", tc.method, tc.path, allow, tc.wantAllow)
		}
	}
	// The read-only routes answer GET and HEAD as before.
	if rec := do(srv, http.MethodGet, "/v1/models", "sk-devin-test", ""); rec.Code != http.StatusOK {
		t.Errorf("GET /v1/models = %d, want 200", rec.Code)
	}
	if rec := do(srv, http.MethodHead, "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Errorf("HEAD /healthz = %d, want 200", rec.Code)
	}
}

func TestAMalformedStopIsA400BeforeAnythingUpstream(t *testing.T) {
	// observingServer's stub backend refuses everything with a 503, so a 400
	// here also proves the request never spent an account's attempt: the stop
	// is validated before the upstream call, like the rest of the request
	// validation.
	srv, hub := observingServer(t)
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","stop":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Errorf("body = %s, want the invalid_request_error type", rec.Body)
	}
	if events := hub.Recent(); len(events) != 1 || events[0].Status != http.StatusBadRequest {
		t.Errorf("recorded %+v, want one 400 event", events)
	}
}
