package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/eventlog"
)

// Request observation.
//
// Every request through the OpenAI face is turned into one event for the
// dashboard: what was asked for, which account and route carried it, how long it
// took, and — the part that is otherwise invisible — the upstream status and the
// backend's own error text. A refusal from Devin arrives as HTTP 200 with the
// refusal inside the stream, so the status code alone is not a report of what
// happened; the event carries the message too, which is what makes the log view
// worth reading.
//
// The mechanism is deliberately the one that needs nothing from the handlers: a
// wrapper records the response status and parses the error body the existing
// error writer already produces. The handlers stay as they are, and a future
// error path cannot forget to report itself.

// traceKey is the context key for the per-request trace.
type traceKey struct{}

// reqTrace accumulates what is known about one request. It is written by the
// handlers (through the context) and read by the observer, so every field is
// guarded rather than relying on the two never overlapping.
type reqTrace struct {
	mu     sync.Mutex
	model  string
	stream bool
	// finish is the OpenAI finish_reason once the completion ended.
	finish    string
	account   string
	route     string
	errMsg    string
	tokensIn  int
	tokensOut int
}

func (t *reqTrace) setModel(model string, stream bool) {
	t.mu.Lock()
	t.model, t.stream = model, stream
	t.mu.Unlock()
}

func (t *reqTrace) setAccount(creds *devin.Credentials) {
	if creds == nil {
		return
	}
	t.mu.Lock()
	t.account = creds.Label()
	t.mu.Unlock()
}

func (t *reqTrace) setRoute(route string) {
	t.mu.Lock()
	t.route = route
	t.mu.Unlock()
}

func (t *reqTrace) setFinish(finish string) {
	t.mu.Lock()
	t.finish = finish
	t.mu.Unlock()
}

func (t *reqTrace) setTokens(in, out int) {
	t.mu.Lock()
	t.tokensIn, t.tokensOut = in, out
	t.mu.Unlock()
}

// fail records an upstream failure. It is called at the points where the error is
// known, which for a stream is mid-response, long after the status was sent.
func (t *reqTrace) fail(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	if t.errMsg == "" {
		t.errMsg = err.Error()
	}
	t.mu.Unlock()
}

// snapshot renders the trace into an event, leaving the request-shaped fields for
// the caller.
func (t *reqTrace) snapshot() eventlog.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return eventlog.Event{
		Model:     t.model,
		Stream:    t.stream,
		Finish:    t.finish,
		Account:   t.account,
		Route:     t.route,
		Error:     t.errMsg,
		TokensIn:  t.tokensIn,
		TokensOut: t.tokensOut,
	}
}

// traceFrom returns the trace for a request's context, or a detached one so a
// caller never has to nil-check. A handler invoked outside the middleware (a unit
// test, say) writes into a trace nobody reads, which is harmless.
func traceFrom(ctx context.Context) *reqTrace {
	if t, ok := ctx.Value(traceKey{}).(*reqTrace); ok && t != nil {
		return t
	}
	return &reqTrace{}
}

// recordingWriter remembers the response status and, for a JSON error response,
// the message and code already written into it. It forwards Flush and Unwrap so
// streaming and http.ResponseController keep working through it.
type recordingWriter struct {
	http.ResponseWriter
	status int
	// errMsg and code are lifted from a JSON error body, so the existing error
	// writer needs no change.
	errMsg   string
	code     string
	captured bool
}

func (w *recordingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	// Only an error body is parsed, and only once: a successful completion body is
	// the client's answer and is not inspected.
	if !w.captured && w.status >= 400 {
		w.captured = true
		var body struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(p, &body); err == nil && body.Error.Message != "" {
			w.errMsg = body.Error.Message
			w.code = body.Error.Code
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *recordingWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the original writer, which is how a
// handler sets a write deadline or hijacks the connection.
func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// observe wraps the mux so every request is measured and reported to the hub.
//
// Only the OpenAI face is observed. The dashboard's own polling — a session check
// every load, a log stream held open — would otherwise be most of what the log
// view shows, burying the requests the view exists for.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Events == nil || !strings.HasPrefix(r.URL.Path, "/v1/") || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		tr := &reqTrace{}
		rec := &recordingWriter{ResponseWriter: w}
		start := time.Now()

		defer func() {
			// A panic still produces a log line: a request that crashed the handler is
			// exactly the one worth seeing an entry for.
			if p := recover(); p != nil {
				tr.fail(&panicError{p})
				s.emitRequest(r, rec, tr, start)
				panic(p)
			}
			s.emitRequest(r, rec, tr, start)
		}()

		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), traceKey{}, tr)))
	})
}

type panicError struct{ value any }

func (e *panicError) Error() string {
	if s, ok := e.value.(string); ok {
		return "handler panic: " + s
	}
	return "handler panic"
}

// emitRequest turns the trace and the recorded response into one event.
func (s *Server) emitRequest(r *http.Request, rec *recordingWriter, tr *reqTrace, start time.Time) {
	e := tr.snapshot()
	e.Kind = "request"
	e.Method = r.Method
	e.Path = r.URL.Path
	e.DurationMS = time.Since(start).Milliseconds()
	if rec.status != 0 {
		e.Status = rec.status
	}
	// The error body's own message wins over anything recorded earlier: it is what
	// the client was actually told, and for a refusal it is the backend's text.
	if rec.errMsg != "" {
		e.Error = rec.errMsg
	}
	// The code only ever comes from the recorded body: the trace carries no code
	// of its own, so there is nothing to prefer over the client-facing one.
	e.Code = rec.code

	e.Level = eventlog.LevelInfo
	switch {
	case e.Status >= 500 || errorish(e.Code):
		e.Level = eventlog.LevelError
	case e.Status >= 400 || e.Error != "":
		e.Level = eventlog.LevelWarn
	}
	// A disconnect needs no special case here: the relay paths return quietly
	// when the client goes away, so the event carries the 200 (or no status at
	// all) and lands on info on its own.
	s.cfg.Events.Emit(e)
}

// errorish reports whether a code names a failure the operator has to act on,
// which is what separates a red row from a yellow one.
func errorish(code string) bool {
	switch strings.ToLower(code) {
	case "upstream_error", "model_unavailable", "egress_unavailable",
		"credential_rejected", "no_credential", "configuration_error":
		return true
	}
	return false
}
