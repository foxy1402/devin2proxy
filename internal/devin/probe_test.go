package devin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"devin2proxy/internal/pb"
)

// A probe exists because a status code is not a health check on this backend: it
// answers 200 for a credential it is about to refuse, so the refusal has to be
// read out of the stream. These tests pin that, and pin the rest of what a probe
// can come back with, so "Test" can never report a dead account as healthy.

// probeBackend answers the chat path with a canned body, or with a status code
// when the body is nil.
func probeBackend(t *testing.T, status int, body func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != GetChatMessagePath {
			t.Errorf("probe hit %s, want %s", r.URL.Path, GetChatMessagePath)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			w.Write([]byte("refused"))
			return
		}
		body(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// probeTextFrame is one response frame carrying text, as the backend sends them.
func probeTextFrame(delta, model string, outputTokens uint64) []byte {
	w := pb.NewWriter()
	w.String(respDeltaText, delta)
	if outputTokens > 0 {
		w.Message(respUsage, func(u *pb.Writer) { u.Varint(musOutputTokens, outputTokens) })
	}
	if model != "" {
		w.String(respActualModelUID, model)
	}
	return EncodeFrame(FrameData, w.Bytes())
}

// endStream finishes a stream, with an error inside it when code is not empty.
func endStream(code, message string) []byte {
	if code == "" {
		return EncodeFrame(FrameEndStream, []byte("{}"))
	}
	body := `{"error":{"code":"` + code + `","message":"` + message + `"}}`
	return EncodeFrame(FrameEndStream, []byte(body))
}

// probeAgainst runs one probe against a stub that writes the given bytes.
func probeAgainst(t *testing.T, status int, body func(w http.ResponseWriter)) ProbeResult {
	t.Helper()
	srv := probeBackend(t, status, body)
	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 5 * time.Second, RateLimitRetries: 1})
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return client.ProbeChat(ctx, creds, "swe-1-6-slow")
}

func TestProbeReadsTheAnswerOutOfTheStream(t *testing.T) {
	res := probeAgainst(t, http.StatusOK, func(w http.ResponseWriter) {
		w.Write(probeTextFrame("hello", "", 0))
		w.Write(probeTextFrame(" world", "swe-1-6-slow", 7))
		w.Write(endStream("", ""))
	})

	if !res.OK {
		t.Fatalf("OK = false on a stream that produced text: %+v", res)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", res.StatusCode)
	}
	if res.Text != "hello world" {
		t.Fatalf("Text = %q, want %q", res.Text, "hello world")
	}
	if res.ModelServed != "swe-1-6-slow" {
		t.Fatalf("ModelServed = %q, want the model the backend named", res.ModelServed)
	}
	if res.OutputTokens != 7 {
		t.Fatalf("OutputTokens = %d, want 7", res.OutputTokens)
	}
	if res.Error != "" {
		t.Fatalf("Error = %q, want none", res.Error)
	}
}

func TestProbeDoesNotCallARefusalInA200Healthy(t *testing.T) {
	// The whole reason a probe reads frames: this is what a dead token looks like.
	res := probeAgainst(t, http.StatusOK, func(w http.ResponseWriter) {
		w.Write(endStream("unauthenticated", "invalid session token"))
	})

	if res.OK {
		t.Fatalf("OK = true on a stream that refused the credential: %+v", res)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200: the refusal really does arrive as a 200", res.StatusCode)
	}
	if res.ErrorCode != "unauthenticated" {
		t.Fatalf("ErrorCode = %q, want %q", res.ErrorCode, "unauthenticated")
	}
	if !strings.Contains(res.Error, "invalid session token") {
		t.Fatalf("Error = %q, want the backend's own message", res.Error)
	}
}

func TestProbeRefusalAfterSomeTextIsStillAFailure(t *testing.T) {
	// A gated model can start streaming and then fail. Text that arrived is not
	// evidence of an account that works, so the error has to win.
	res := probeAgainst(t, http.StatusOK, func(w http.ResponseWriter) {
		w.Write(probeTextFrame("San", "", 0))
		w.Write(endStream("permission_denied", "this model needs a higher plan"))
	})

	if res.OK {
		t.Fatalf("OK = true although the stream ended in an error: %+v", res)
	}
	if res.ErrorCode != "permission_denied" {
		t.Fatalf("ErrorCode = %q, want %q", res.ErrorCode, "permission_denied")
	}
}

func TestProbeAnEmptyStreamIsNotSuccess(t *testing.T) {
	// Ending cleanly without ever sending a frame is an empty answer. Counting it
	// as healthy would report an account as serving a model it never touched.
	res := probeAgainst(t, http.StatusOK, func(w http.ResponseWriter) {
		w.Write(endStream("", ""))
	})

	if res.OK {
		t.Fatalf("OK = true on a stream that sent nothing: %+v", res)
	}
	if res.Error == "" {
		t.Fatal("Error is empty; an empty stream has to say so")
	}
}

func TestProbeReportsANonOKStatus(t *testing.T) {
	res := probeAgainst(t, http.StatusTooManyRequests, nil)

	if res.OK {
		t.Fatalf("OK = true on a 429: %+v", res)
	}
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want 429", res.StatusCode)
	}
}

func TestProbeWithoutARouteReportsNoStatus(t *testing.T) {
	// A dead route never reaches the backend, so there is no status to report. The
	// distinction matters on the page: "no answer" and "refused" are different
	// problems, and only one of them is about the account.
	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 2 * time.Second, RateLimitRetries: 1})
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: "http://127.0.0.1:1"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := client.ProbeChat(ctx, creds, "swe-1-6-slow")

	if res.OK || res.StatusCode != 0 {
		t.Fatalf("probe of an unroutable host = %+v, want a failure with no status", res)
	}
	if res.Error == "" {
		t.Fatal("Error is empty; a probe that could not reach the backend has to say so")
	}
}

func TestProbeRefusesToRunWithoutACredentialOrAModel(t *testing.T) {
	// An empty model would be accepted by the backend and served by its default,
	// which would make the probe look like it tested something it did not.
	client := NewClient(Options{MaxConcurrent: 1})
	ctx := context.Background()

	if res := client.ProbeChat(ctx, nil, "swe-1-6-slow"); res.OK || res.Error == "" {
		t.Fatalf("probe with no credential = %+v, want a failure", res)
	}
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: "http://127.0.0.1:1"}
	if res := client.ProbeChat(ctx, creds, "   "); res.OK || res.Error == "" {
		t.Fatalf("probe with a blank model = %+v, want a failure", res)
	}
}

func TestProbeSendsWhatItSaysItSends(t *testing.T) {
	// The request is the smallest one the backend accepts, but it still has to be
	// a real one: the model asked for, a user turn, and the same token budget and
	// sampling fields a real request carries. Two past failures are pinned here:
	// a 32-token budget ended the stream with MAX_TOKENS before any answer text
	// existed, and sampling fields left at zero were refused with
	// invalid_argument on a perfectly healthy account.
	var seen *GetChatMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		frames, err := ParseFrames(body)
		if err != nil {
			t.Errorf("request body is not a Connect frame: %v", err)
		}
		for _, f := range frames {
			if f.Flag == FrameData {
				seen = DecodeGetChatMessageRequest(f.Data)
			}
		}
		w.Write(probeTextFrame("ok", "", 0))
		w.Write(endStream("", ""))
	}))
	t.Cleanup(srv.Close)

	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 5 * time.Second, RateLimitRetries: 1})
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client.ProbeChat(ctx, creds, "swe-1-6-slow")

	if seen == nil {
		t.Fatal("the probe sent no request")
	}
	if seen.ChatModelUID != "swe-1-6-slow" {
		t.Fatalf("ChatModelUID = %q, want the model that was asked for", seen.ChatModelUID)
	}
	if seen.Configuration == nil || seen.Configuration.MaxTokens != probeMaxTokens {
		t.Fatalf("MaxTokens = %+v, want the %d-token floor", seen.Configuration, probeMaxTokens)
	}
	for field, got := range map[string]float64{
		"Temperature": seen.Configuration.Temperature,
		"TopP":        seen.Configuration.TopP,
	} {
		if got <= 0 {
			t.Fatalf("%s = %v: the backend refuses a present configuration with it at zero", field, got)
		}
	}
	for field, got := range map[string]uint64{
		"TopK":        seen.Configuration.TopK,
		"MaxNewlines": seen.Configuration.MaxNewlines,
	} {
		if got == 0 {
			t.Fatalf("%s = 0: the backend refuses a present configuration with it at zero", field)
		}
	}
	if len(seen.ChatMessagePrompts) != 1 || seen.ChatMessagePrompts[0].Source != SourceUser {
		t.Fatalf("prompts = %+v, want exactly one user turn", seen.ChatMessagePrompts)
	}
	if seen.TrajectoryRef == nil {
		t.Fatal("no trajectory reference: the backend requires one to start a conversation")
	}
}

// The page renders duration_ms as milliseconds, and the field name says the same.
// A time.Duration tagged duration_ms would have marshalled nanoseconds into it —
// a 1.5s probe showing up as 1500000s — so the wire field is a real int64 of ms.
func TestProbeDurationMarshalsAsMilliseconds(t *testing.T) {
	res := ProbeResult{OK: true}
	res.Duration = 1500 * time.Millisecond
	res.DurationMS = res.Duration.Milliseconds()

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ms, ok := got["duration_ms"].(float64); !ok || int64(ms) != 1500 {
		t.Fatalf("duration_ms = %v, want 1500", got["duration_ms"])
	}
	if _, leaked := got["Duration"]; leaked {
		t.Errorf("the Go field leaked into the JSON: %s", b)
	}
}

func TestCapTextCutsOnARuneBoundary(t *testing.T) {
	// The page renders this straight into the DOM, so the cap must not leave a
	// partial rune behind: a byte cut that lands inside a character would put an
	// invalid byte on the page. Every rune here is two bytes, so an odd cap always
	// lands mid-character.
	got := capText(strings.Repeat("é", 100), 11)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("capText = %q, want an ellipsis on the end", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("capText left an incomplete character behind: %q", got)
	}
	body := strings.TrimSuffix(got, "…")
	if len(body) > 11 {
		t.Fatalf("capText kept %d bytes, want at most 11", len(body))
	}
	if runes := utf8.RuneCountInString(body); runes != 5 {
		t.Fatalf("capText kept %d runes, want the 5 whole ones that fit in 11 bytes", runes)
	}

	if short := capText("ok", TextLimit); short != "ok" {
		t.Fatalf("capText left a short string alone, got %q", short)
	}
}
