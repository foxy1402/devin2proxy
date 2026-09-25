package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/eventlog"
	"devin2proxy/internal/pb"
)

// Regression tests for the risk batch: what an unauthenticated /healthz may and
// may not say, the request-body limits, the usage line in the log view for a
// non-streaming completion, and the finish_reason a tool call earns.

// frameServer builds a server whose single-account pool points at a stub that
// answers the chat path with the given frames, and whose events go to a hub.
func frameServer(t *testing.T, frames ...[]byte) (*Server, *eventlog.Hub) {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, f := range frames {
			w.Write(f)
		}
	}))
	t.Cleanup(backend.Close)
	hub := eventlog.New(64)
	srv := New(Config{
		APIKey: "sk-devin-test",
		Models: []string{"swe-1.6"},
		Creds:  devin.NewPool([]string{"devin-session-token$stub"}, backend.URL),
		Events: hub,
	}, devin.NewClient(devin.Options{MaxConcurrent: 1}))
	return srv, hub
}

func usageFrame(in, out uint64) []byte {
	w := pb.NewWriter()
	w.Message(respUsageField, func(u *pb.Writer) {
		u.Varint(respUsageInput, in)
		u.Varint(respUsageOutput, out)
	})
	return devin.EncodeFrame(devin.FrameData, w.Bytes())
}

func toolCallFrame(id, name, args string) []byte {
	w := pb.NewWriter()
	w.Message(respToolCallsField, func(tc *pb.Writer) {
		tc.String(1, id)
		tc.String(2, name)
		tc.String(3, args)
	})
	return devin.EncodeFrame(devin.FrameData, w.Bytes())
}

// The response field numbers, restated here so the test frames are readable
// without importing the unexported wire constants.
const (
	respUsageField     = 7
	respUsageInput     = 2
	respUsageOutput    = 3
	respToolCallsField = 6
)

func TestHealthzSaysNoMoreThanWhetherItIsUp(t *testing.T) {
	// An empty pool takes the file-credential branch, the one that used to print
	// the backend URL and the credential loader's error text — local paths and
	// infrastructure detail — to an endpoint that answers without authentication.
	srv := New(Config{
		APIKey: "sk-devin-test",
		Models: []string{"swe-1.6"},
		Creds:  devin.NewPool(nil, ""),
	}, devin.NewClient(devin.Options{MaxConcurrent: 1}))
	rec := do(srv, http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "backend") {
		t.Errorf("healthz discloses the backend URL: %s", body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cred, _ := got["credential"].(string)
	if cred != "loaded" && cred != "unavailable" {
		t.Errorf("credential = %q, want the bare state with no explanation", got["credential"])
	}
}

func TestABodyOverTheCapIsA413NotAParseError(t *testing.T) {
	srv, _ := frameServer(t, textFrame("ok", devin.StopNormal, true), endFrame())
	filler := strings.Repeat("a", (8<<20)+1024)
	body := `{"model":"swe-1.6","messages":[{"role":"user","content":"` + filler + `"}]}`
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413 (body %.200s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "body_too_large") {
		t.Errorf("body = %s, want the body_too_large code", rec.Body)
	}
}

func TestTrailingGarbageAfterTheJSONIsRefused(t *testing.T) {
	srv, _ := frameServer(t, textFrame("ok", devin.StopNormal, true), endFrame())
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","messages":[{"role":"user","content":"hi"}]} {"second":"document"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "after the top-level value") {
		t.Errorf("body = %s, want an explanation of the trailing data", rec.Body)
	}
}

func TestANonStreamingCompletionRecordsItsUsageInTheLogView(t *testing.T) {
	srv, hub := frameServer(t,
		textFrame("all good", devin.StopNormal, true),
		usageFrame(120, 34),
		endFrame())
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", rec.Code, rec.Body)
	}
	events := hub.Recent()
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
	if events[0].TokensIn != 120 || events[0].TokensOut != 34 {
		t.Fatalf("tokens = %d/%d, want 120/34: the log view exists to answer what a request cost",
			events[0].TokensIn, events[0].TokensOut)
	}
}

func TestAWrongKeyAnswersSlowlyEnoughToDiscourageGuessing(t *testing.T) {
	// On a public deployment the API key is the only gate in front of the
	// account's quota, and it has no ban ladder behind it like the dashboard
	// password does. The floor on a wrong key's answer is what makes online
	// guessing pointless.
	srv := New(Config{
		APIKey:           "sk-devin-test",
		Models:           []string{"swe-1.6"},
		Creds:            devin.NewPool([]string{"devin-session-token$stub"}, benchedBackend(t)),
		AuthFailureDelay: 60 * time.Millisecond,
	}, devin.NewClient(devin.Options{MaxConcurrent: 1}))
	start := time.Now()
	rec := do(srv, http.MethodGet, "/v1/models", "wrong-key", "")
	elapsed := time.Since(start)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("a wrong key answered in %v; the floor did not apply", elapsed)
	}
	// The correct key must not pay the floor — autocomplete latency is the
	// whole reason this proxy exists.
	start = time.Now()
	rec = do(srv, http.MethodGet, "/v1/models", "sk-devin-test", "")
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d with the right key", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Errorf("the right key took %v; the floor must only apply to failures", elapsed)
	}
}

func TestAToolCallThatStopsNormallyFinishesAsToolCalls(t *testing.T) {
	// Clients branch on finish_reason to decide whether to execute a tool call.
	// A backend that delivers the call and then signals a normal stop must not
	// have its answer labelled "stop", or the client skips the call.
	srv, _ := frameServer(t,
		toolCallFrame("call_1", "get_weather", `{"city":"Paris"}`),
		textFrame("", devin.StopNormal, true),
		endFrame())
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","messages":[{"role":"user","content":"weather in Paris"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", rec.Code, rec.Body)
	}
	var body struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if len(body.Choices) != 1 || body.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", body.Choices[0].FinishReason)
	}
	if len(body.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v, want the call the backend delivered", body.Choices[0].Message.ToolCalls)
	}
}

// sseChunk is the loose shape both streaming endpoints end with: one frame with
// no choices, carrying the usage line.
type sseChunk struct {
	Choices []json.RawMessage `json:"choices"`
	Usage   *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// lastSSEChunk decodes the last JSON data frame of a streamed response.
func lastSSEChunk(t *testing.T, body string) sseChunk {
	t.Helper()
	var last sseChunk
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &last); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
	}
	return last
}

func TestAnIncludeUsageFinalChunkAlwaysCarriesUsage(t *testing.T) {
	// The backend sent no stats at all here, but the client explicitly asked
	// for the usage line by setting include_usage: omitting the field would
	// hand a client that parses `usage` nothing, so an explicit zero is sent.
	srv, _ := frameServer(t, textFrame("hello", devin.StopNormal, true), endFrame())
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", rec.Code, rec.Body)
	}
	last := lastSSEChunk(t, rec.Body.String())
	if last.Usage == nil {
		t.Fatalf("no chunk carried usage despite include_usage: %s", rec.Body)
	}
	if len(last.Choices) != 0 {
		t.Errorf("the usage chunk carries %d choices, want none", len(last.Choices))
	}
	if last.Usage.PromptTokens != 0 || last.Usage.CompletionTokens != 0 || last.Usage.TotalTokens != 0 {
		t.Errorf("usage = %+v, want explicit zeros", last.Usage)
	}

	// The legacy stream behaves the same way.
	srv, _ = frameServer(t, textFrame("hello", devin.StopNormal, true), endFrame())
	rec = do(srv, http.MethodPost, "/v1/completions", "sk-devin-test",
		`{"model":"swe-1.6","stream":true,"stream_options":{"include_usage":true},"prompt":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy status %d (body %s)", rec.Code, rec.Body)
	}
	if last := lastSSEChunk(t, rec.Body.String()); last.Usage == nil {
		t.Errorf("the legacy stream omitted usage despite include_usage: %s", rec.Body)
	}
}

func TestAStreamedToolCallWithoutAnIDGetsThePlaceholder(t *testing.T) {
	// The non-streaming path synthesizes call_N ids in Calls(); the streamed
	// opening delta must carry the same placeholder, or a client cannot
	// correlate the fragments with the call it will execute.
	srv, _ := frameServer(t,
		toolCallFrame("", "get_weather", `{"city":"Paris"}`),
		textFrame("", devin.StopNormal, true),
		endFrame())
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","stream":true,"messages":[{"role":"user","content":"weather in Paris"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"id":"call_0"`) {
		t.Errorf("the streamed tool call never carried the call_0 placeholder: %s", rec.Body)
	}
}

func TestModelFallbackNotesAreCapped(t *testing.T) {
	// A client that invents a different model name on every request used to
	// grow the dedup set without bound; recording stops at the cap and the
	// worst case is one repeated log line.
	srv, _ := frameServer(t, textFrame("ok", devin.StopNormal, true), endFrame())
	for i := 0; i < maxModelFallbackNotes+25; i++ {
		srv.noteModelFallback("invented-"+strconv.Itoa(i), "swe-1.6")
	}
	srv.fallbackMu.Lock()
	n := len(srv.fallbackSeen)
	srv.fallbackMu.Unlock()
	if n != maxModelFallbackNotes {
		t.Fatalf("the fallback set holds %d names, want the cap %d", n, maxModelFallbackNotes)
	}
}
