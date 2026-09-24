// Package eventlog carries the proxy's runtime events to whoever is watching:
// a bounded in-memory history for a page that has just loaded, live subscribers
// for one that is already open, and an io.Writer so the standard logger feeds it
// without every log call having to change.
//
// It deliberately knows nothing about the proxy. Everything in it is fed by the
// caller, which is what keeps the dashboard out of the request path: an event is
// a value, not a callback into the UI.
package eventlog

import (
	"strings"
	"sync"
	"time"
)

// Event is one thing that happened. The request fields are empty for a log line
// and the log fields are empty for a request, so one type covers both without a
// second stream to subscribe to.
type Event struct {
	Seq  int64     `json:"seq"`
	Time time.Time `json:"time"`
	// Kind is "request", "log" or "oauth". The UI groups and filters on it.
	Kind  string `json:"kind"`
	Level string `json:"level"`
	// Message is the human-readable line: an upstream error, a gateway error, or
	// whatever the standard logger was given.
	Message string `json:"message,omitempty"`

	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	Model  string `json:"model,omitempty"`
	// Account names the account as the logs do — the last few characters of the
	// token, never the token. It is filled from the identity in the status call
	// when one is known, since that is what an operator recognises.
	Account string `json:"account,omitempty"`
	// Route is the outbound proxy label, without credentials.
	Route  string `json:"route,omitempty"`
	Status int    `json:"status,omitempty"`
	// Code is the gateway's own short code ("rate_limit_exceeded"), and Error the
	// message from the backend or the gateway, verbatim.
	Code       string `json:"code,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Stream     bool   `json:"stream,omitempty"`
	TokensIn   int    `json:"tokens_in,omitempty"`
	TokensOut  int    `json:"tokens_out,omitempty"`
	// Finish is the OpenAI finish_reason when a completion ran to the end.
	Finish string `json:"finish,omitempty"`
}

// Levels. A dashboard filter and a colour are the only things that read them.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// DefaultCapacity is how many events are kept for a page that has just loaded.
// Large enough to cover a debugging session's worth of scrolling, small enough
// that the memory is uninteresting.
const DefaultCapacity = 1000

// subscriberBuffer is how many events may be queued for one live subscriber. A
// subscriber that cannot keep up loses events rather than stalling the proxy:
// nothing here may ever block a request, so the send is non-blocking and the
// loss is counted and reported.
const subscriberBuffer = 256

// Hub fans events out to the history and to every live subscriber.
type Hub struct {
	mu       sync.Mutex
	ring     []Event
	start    int // index of the oldest event, once the ring has wrapped
	count    int
	seq      int64
	subs     map[int]chan Event
	nextSub  int
	dropped  uint64
	capacity int
}

// New builds a hub holding at most capacity events. A capacity below 1 takes the
// default.
func New(capacity int) *Hub {
	if capacity < 1 {
		capacity = DefaultCapacity
	}
	return &Hub{
		ring:     make([]Event, capacity),
		subs:     map[int]chan Event{},
		capacity: capacity,
	}
}

// Emit records an event and hands it to every live subscriber. It never blocks:
// a subscriber whose buffer is full loses the event, and the count of losses is
// reported by Dropped so the UI can say so instead of quietly showing a gap.
func (h *Hub) Emit(e Event) {
	if h == nil {
		return
	}
	now := time.Now()
	h.mu.Lock()
	h.seq++
	e.Seq = h.seq
	if e.Time.IsZero() {
		e.Time = now
	}
	if e.Level == "" {
		e.Level = LevelInfo
	}
	switch e.Kind {
	case "request", "log", "oauth":
	default:
		e.Kind = "log"
	}

	if h.count < h.capacity {
		h.ring[(h.start+h.count)%h.capacity] = e
		h.count++
	} else {
		h.ring[h.start] = e
		h.start = (h.start + 1) % h.capacity
	}
	for _, ch := range h.subs {
		select {
		case ch <- e:
		default:
			h.dropped++
		}
	}
	h.mu.Unlock()
}

// Recent returns the retained history, oldest first.
func (h *Hub) Recent() []Event {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Event, 0, h.count)
	for i := 0; i < h.count; i++ {
		out = append(out, h.ring[(h.start+i)%h.capacity])
	}
	return out
}

// Dropped reports how many events a subscriber was too slow to receive.
func (h *Hub) Dropped() uint64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropped
}

// Subscribe returns a channel of events and a function that stops the
// subscription. Both are safe to call more than once on the cancel function, so
// a handler can defer it without tracking whether it already fired.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	if h == nil {
		return nil, func() {}
	}
	ch := make(chan Event, subscriberBuffer)
	h.mu.Lock()
	id := h.nextSub
	h.nextSub++
	h.subs[id] = ch
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, id)
			h.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// Write makes the hub usable as a log output: every line the standard logger
// emits is recorded as an event. It always reports success, because a logger that
// can fail is a logger that hides the failure somewhere worse.
//
// The proxy's log lines are written to be safe to display — account and route
// labels are truncated tails, never tokens — so this needs no redaction of its
// own.
func (h *Hub) Write(p []byte) (int, error) {
	if h == nil {
		return len(p), nil
	}
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		at, message := splitLogTime(line)
		h.Emit(Event{Kind: "log", Level: classify(message), Message: message, Time: at})
	}
	return len(p), nil
}

// splitLogTime removes the standard logger's timestamp prefix, since the event
// carries its own time and the dashboard would otherwise show it twice.
//
// The timestamp is parsed in local time because that is what the standard logger
// writes. Parsing it as UTC would put every log line hours away from the request
// lines beside it, which is the one thing that makes a mixed log view useless.
func splitLogTime(line string) (time.Time, string) {
	const layout = "2006/01/02 15:04:05"
	if len(line) > len(layout) && line[len(layout)] == ' ' {
		if at, err := time.ParseInLocation(layout, line[:len(layout)], time.Local); err == nil {
			return at, line[len(layout)+1:]
		}
	}
	return time.Time{}, line
}

// classify guesses a level from a log line, which is all that is available
// without every call site adopting a logging API. It is a display hint: the
// message itself is never altered, so a wrong guess costs a colour and nothing
// else.
func classify(message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "warning") ||
		strings.Contains(lower, "unavailable") ||
		strings.Contains(lower, "no usable"):
		return LevelWarn
	case strings.Contains(lower, "error") ||
		strings.Contains(lower, "failed") ||
		strings.Contains(lower, "refused") ||
		strings.Contains(lower, "rejected"):
		return LevelError
	default:
		return LevelInfo
	}
}
