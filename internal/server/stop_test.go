package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/pb"
)

// The backend can give up mid-generation by sending a frame with stop_reason
// ERROR (13). Presenting that as a clean finish_reason would hand the client a
// truncated answer that looks complete — worse than an error, because nothing
// downstream notices. Every relay path has to surface it as the failure it is.

// stopBackend answers the chat path with the given frames.
func stopBackend(t *testing.T, frames ...[]byte) string {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, f := range frames {
			w.Write(f)
		}
	}))
	t.Cleanup(backend.Close)
	return backend.URL
}

func textFrame(delta string, stop int, stopSet bool) []byte {
	w := pb.NewWriter()
	w.String(3, delta) // delta_text
	if stopSet {
		w.Varint(5, uint64(stop)) // stop_reason
	}
	return devin.EncodeFrame(devin.FrameData, w.Bytes())
}

func endFrame() []byte {
	return devin.EncodeFrame(devin.FrameEndStream, []byte("{}"))
}

func stopServer(t *testing.T, frames ...[]byte) *Server {
	t.Helper()
	return New(Config{
		APIKey: "sk-devin-test",
		Models: []string{"swe-1.6"},
		Creds:  devin.NewPool([]string{"devin-session-token$stub"}, stopBackend(t, frames...)),
	}, devin.NewClient(devin.Options{MaxConcurrent: 1}))
}

func TestANonStreamingAnswerWithAnErroredStopIsAnError(t *testing.T) {
	srv := stopServer(t, textFrame("half an answer", devin.StopError, true), endFrame())
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 (body %s)", rec.Code, rec.Body)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if body.Error.Code != "upstream_error" {
		t.Errorf("code = %q, want upstream_error", body.Error.Code)
	}
}

func TestAStreamedAnswerWithAnErroredStopCarriesAnInBandError(t *testing.T) {
	srv := stopServer(t, textFrame("half an", 0, false), textFrame(" answer", devin.StopError, true), endFrame())
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream_error") {
		t.Errorf("the stream never reported the backend's error: %s", body)
	}
	// The stream must not end with a clean finish_reason frame after the error.
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("an errored generation was presented as a clean stop: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("the stream did not terminate with [DONE]: %q", body)
	}
}

func TestALegacyCompletionWithAnErroredStopIsAnError(t *testing.T) {
	srv := stopServer(t, textFrame("partial", devin.StopError, true), endFrame())
	rec := do(srv, http.MethodPost, "/v1/completions", "sk-devin-test",
		`{"model":"swe-1.6","prompt":"hi"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 (body %s)", rec.Code, rec.Body)
	}
}

// A normal generation must keep working exactly as before: the interception is
// on stop_reason ERROR only.
func TestACompletedAnswerStillFinishesCleanly(t *testing.T) {
	srv := stopServer(t, textFrame("all good", devin.StopNormal, true), endFrame())
	rec := do(srv, http.MethodPost, "/v1/chat/completions", "sk-devin-test",
		`{"model":"swe-1.6","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", rec.Code, rec.Body)
	}
	var body struct {
		Choices []struct {
			Message      struct{ Content string } `json:"message"`
			FinishReason string                   `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body)
	}
	if len(body.Choices) != 1 || body.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices = %+v, want one clean stop", body.Choices)
	}
	if body.Choices[0].Message.Content != "all good" {
		t.Errorf("content = %q", body.Choices[0].Message.Content)
	}
}
