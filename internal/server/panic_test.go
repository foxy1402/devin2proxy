package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/eventlog"
)

func panicServer(t *testing.T) (*Server, *eventlog.Hub) {
	t.Helper()
	hub := eventlog.New(64)
	srv := New(Config{
		APIKey: "sk-devin-test",
		Models: []string{"swe-1.6"},
		Creds:  devin.NewPool([]string{"devin-session-token$stub"}, "http://127.0.0.1:1"),
		Events: hub,
	}, devin.NewClient(devin.Options{MaxConcurrent: 1}))
	return srv, hub
}

// TestAPanickingHandlerAnswers500AndRecordsIt pins the recovery behaviour: when a
// /v1 handler panics before any bytes are written, the client gets a 500 JSON error
// instead of a bare connection reset, and the request event records the 500. The
// panic's own value is written to the process log (which the dashboard's event
// stream tees), not into the request event: the event's Error is deliberately the
// message the client was actually sent.
func TestAPanickingHandlerAnswers500AndRecordsIt(t *testing.T) {
	srv, hub := panicServer(t)
	h := srv.observe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal error") {
		t.Errorf("body = %s, want a JSON internal_error", rec.Body)
	}
	events := hub.Recent()
	if len(events) == 0 {
		t.Fatal("no event was recorded for the panic")
	}
	ev := events[len(events)-1]
	if ev.Status != http.StatusInternalServerError {
		t.Errorf("event status = %d, want 500", ev.Status)
	}
	if ev.Error != "the proxy hit an internal error handling this request" {
		t.Errorf("event error = %q, want the client-facing message", ev.Error)
	}
}

// TestAStringPanicKeepsItsDetail pins the string branch of panicError.
func TestAStringPanicKeepsItsDetail(t *testing.T) {
	if got := (&panicError{value: "kaboom"}).Error(); got != "handler panic: kaboom" {
		t.Errorf("panicError = %q, want it to carry the string value", got)
	}
}

// TestANonStringPanicKeepsItsDetail pins the panicError formatting fix: a panic
// value that is not a string (a runtime error, a typed sentinel) used to be
// reported as a bare "handler panic", losing the one detail worth having.
func TestANonStringPanicKeepsItsDetail(t *testing.T) {
	e := &panicError{value: http.ErrAbortHandler}
	got := e.Error()
	if !strings.Contains(got, http.ErrAbortHandler.Error()) {
		t.Errorf("panicError = %q, want it to include the non-string value", got)
	}
	if got == "handler panic" {
		t.Errorf("panicError dropped the non-string detail")
	}
}

// TestAPanicAfterBytesRePanics pins the one case that must still tear the
// connection down: once the response is committed, writing a 500 would be a second
// status on the same response, and a clean close would look like a complete
// (empty) answer — so the panic is re-raised for net/http to abort the connection.
func TestAPanicAfterBytesRePanics(t *testing.T) {
	srv, _ := panicServer(t)
	h := srv.observe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
		panic("late kaboom")
	}))
	defer func() {
		if p := recover(); p == nil {
			t.Error("a panic after the response was committed did not re-panic")
		}
	}()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
}
