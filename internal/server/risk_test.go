package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
