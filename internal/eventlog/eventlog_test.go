package eventlog

import (
	"strings"
	"testing"
	"time"
)

// The hub is the proxy's log memory: Recent is what the dashboard's log view
// polls, so the ring has to keep the newest events in order, wrap without losing
// or reordering anything, and fill in the fields an event should not leave blank.
// There is no live-subscriber path any more — the view polls /logs/recent — so
// these tests pin exactly the behaviour that remains.

func TestRecentReturnsTheNewestEventsOldestFirst(t *testing.T) {
	h := New(4)
	for i := 0; i < 6; i++ {
		h.Emit(Event{Kind: "log", Message: strings.Repeat("x", i+1)})
	}
	got := h.Recent()
	if len(got) != 4 {
		t.Fatalf("Recent returned %d events, want the 4 the ring holds", len(got))
	}
	// Six events into a ring of four: the two oldest fell off and the rest are
	// here in order, so the newest is last.
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("events are not oldest-first: seq %d then %d", got[i-1].Seq, got[i].Seq)
		}
	}
	if got[0].Seq != 3 || got[3].Seq != 6 {
		t.Errorf("window runs seq %d..%d, want 3..6", got[0].Seq, got[3].Seq)
	}
}

func TestEmitFillsInTheFieldsAnEventShouldNotLeaveBlank(t *testing.T) {
	h := New(8)
	h.Emit(Event{Message: "just a line"})
	got := h.Recent()
	if len(got) != 1 {
		t.Fatalf("Recent returned %d events, want 1", len(got))
	}
	e := got[0]
	if e.Seq != 1 {
		t.Errorf("seq = %d, want 1", e.Seq)
	}
	if e.Time.IsZero() {
		t.Error("the event carries no time of its own")
	}
	if e.Level != LevelInfo {
		t.Errorf("level = %q, want the default %q", e.Level, LevelInfo)
	}
	if e.Kind != "log" {
		t.Errorf("kind = %q, want the default \"log\"", e.Kind)
	}
	// A kind the hub knows is kept as it arrived.
	h.Emit(Event{Kind: "request"})
	if got := h.Recent()[1]; got.Kind != "request" {
		t.Errorf("kind = %q, want it kept as it arrived", got.Kind)
	}
	// So is an explicit time and level.
	at := time.Now().Add(-time.Minute)
	h.Emit(Event{Kind: "oauth", Level: LevelWarn, Time: at})
	e = h.Recent()[2]
	if !e.Time.Equal(at) {
		t.Errorf("time = %s, want the one the event arrived with", e.Time)
	}
	if e.Level != LevelWarn {
		t.Errorf("level = %q, want %q", e.Level, LevelWarn)
	}
}

func TestWriteTurnsLogLinesIntoEvents(t *testing.T) {
	h := New(8)
	n, err := h.Write([]byte("2006/01/02 15:04:05 first line\n\n2006/01/02 15:04:06 second line\r\n"))
	if err != nil || n != len("2006/01/02 15:04:05 first line\n\n2006/01/02 15:04:06 second line\r\n") {
		t.Fatalf("Write = (%d, %v), want the whole input accepted", n, err)
	}
	got := h.Recent()
	if len(got) != 2 {
		t.Fatalf("Write recorded %d events, want one per non-blank line", len(got))
	}
	if got[0].Message != "first line" || got[1].Message != "second line" {
		t.Errorf("messages = %q, %q; the timestamp prefix and the line breaks must be gone",
			got[0].Message, got[1].Message)
	}
	if got[0].Time.IsZero() {
		t.Error("the parsed timestamp was discarded")
	}
	if got[1].Level != LevelInfo {
		t.Errorf("level = %q, want %q for an ordinary line", got[1].Level, LevelInfo)
	}
	// classify is a display hint, never an alteration of the message.
	if _, err := h.Write([]byte("dashboard: could not read the model catalogue: refused")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := h.Recent()[2]; got.Level != LevelError || got.Message != "dashboard: could not read the model catalogue: refused" {
		t.Errorf("classified line = (%q, %q), want the message verbatim at level error", got.Message, got.Level)
	}
}

func TestANilHubIsHarmless(t *testing.T) {
	var h *Hub
	h.Emit(Event{Kind: "log", Message: "nothing"})
	if got := h.Recent(); got != nil {
		t.Errorf("a nil hub returned %v", got)
	}
	if _, err := h.Write([]byte("nothing")); err != nil {
		t.Errorf("writing to a nil hub: %v", err)
	}
}
