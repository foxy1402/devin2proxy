package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/eventlog"
)

// hangingServer points the proxy at an upstream that accepts the request and then
// never sends a frame, so the only thing that can end the request is the proxy's
// own RequestTimeout. Do not call t.Cleanup(Close) on it while a request is in
// flight: the handler is parked until the test ends.
//
// The optional firstFrames are written before the upstream parks, which is how a
// test gets a stream that has already opened when the deadline fires.
func hangingServer(t *testing.T, timeout time.Duration, firstFrames ...[]byte) *Server {
	t.Helper()
	held := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, f := range firstFrames {
			w.Write(f)
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-held // never answers further; released by the test's Cleanup
	}))
	t.Cleanup(func() { close(held); backend.Close() })
	return New(Config{
		APIKey:         "sk-devin-test",
		Models:         []string{"swe-1.6"},
		Creds:          devin.NewPool([]string{"devin-session-token$stub"}, backend.URL),
		Events:         eventlog.New(64),
		RequestTimeout: timeout,
	}, devin.NewClient(devin.Options{MaxConcurrent: 1}))
}

// TestRequestTimeoutIsNotSilent pins the distinction between the proxy's own
// deadline and a client disconnect. A client that hung up gets silence — there is
// nobody to read an error. The proxy's RequestTimeout fires with the client still
// connected, so the client must still be told: a 504 for a non-streaming request,
// rather than a 200 with an empty body.
func TestRequestTimeoutIsNotSilent(t *testing.T) {
	srv := hangingServer(t, 150*time.Millisecond)
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status %d (body %s), want 504", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "timeout") {
		t.Errorf("the 504 body does not name the timeout: %s", rec.Body)
	}
}

// TestStreamingRequestTimeoutEndsWithADone pins that a stream that hits the
// proxy's deadline does not just stop: the client gets an in-band error and the
// [DONE] terminator, so a streaming client can tell a timeout from a clean end.
func TestStreamingRequestTimeoutEndsWithADone(t *testing.T) {
	// One text frame opens the stream, then the upstream parks: the deadline now
	// fires with the response already committed, so the timeout has to be reported
	// in-band.
	srv := hangingServer(t, 150*time.Millisecond, textFrame("partial", devin.StopPartial, false))
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (the stream opened)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "timeout") {
		t.Errorf("the stream carried no in-band timeout error: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("the stream ended without a [DONE] terminator: %s", body)
	}
}
