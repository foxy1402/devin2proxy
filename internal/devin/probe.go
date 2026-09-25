package devin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// A probe is the smallest real request this backend can be asked to serve: one
// short prompt, one model, and enough of the answer read to know the account
// actually produced a completion.
//
// It exists because "did that return 200" is the wrong question for this API. The
// backend answers 200 for a credential it is about to refuse, for a model the plan
// does not cover, and for a missing argument; the refusal arrives inside the stream
// (see docs/PROTOCOL.md). A probe that checked only the status code would report a
// dead token as healthy, which is the opposite of what an operator pressing Test
// wants to know. So a probe reads frames until the stream ends and calls it a
// success only when a real frame arrived and no error followed.
const (
	// probePrompt is deliberately tiny: the point is to prove the account serves
	// this model, not to get an answer worth reading.
	probePrompt = "Reply with the single word: ok"
	// probeSystemPrompt takes the place of a system prompt, which the real path
	// always sends. It is short because prompt length is billed as input tokens.
	probeSystemPrompt = "Health check. Answer with one word."
	// probeMaxTokens is the same floor a real request gets rather than a small
	// ceiling: the model's reasoning bills against the same budget as its answer
	// (measured in the hundreds for even trivial prompts), so 32 tokens ended the
	// stream with MAX_TOKENS before any answer text existed. A test that only
	// proves the account errors differently is not worth its quota.
	probeMaxTokens = 8192
	// probeFrameLimit stops reading a stream that keeps producing text, so a
	// misbehaving backend cannot make one probe run forever.
	probeFrameLimit = 512
)

// ProbeResult is what a probe learned. A refusal is a result rather than an error
// because every outcome here is something to show the operator, including the
// failures: the caller renders the backend's own words either way.
type ProbeResult struct {
	// DurationMS is how long the probe took, in the unit the JSON field name
	// promises. The separate Duration field below cannot be the one on the wire:
	// a time.Duration marshals as an integer of *nanoseconds*, so tagging it
	// duration_ms was a lie three orders of magnitude wide, and the page's
	// "1.5s" rendering was really 1500000s.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Model is the uid that was asked for, and ModelServed is the uid the backend
	// says it used. They differ when a request names something the backend maps
	// elsewhere, which is worth showing rather than hiding.
	Model       string `json:"model"`
	ModelServed string `json:"model_served,omitempty"`

	// OK means the account served this model: HTTP 200 *and* at least one response
	// frame *and* no error frame.
	OK bool `json:"ok"`
	// StatusCode is the HTTP status the backend answered with. It is 0 when no
	// response arrived at all (a dial or TLS failure, a dead route).
	StatusCode int `json:"status_code,omitempty"`
	// ErrorCode is the Connect code from an in-stream failure, such as
	// "permission_denied" or "resource_exhausted".
	ErrorCode string `json:"error_code,omitempty"`
	// Error is the failure as text: the backend's own message where there was one.
	Error string `json:"error,omitempty"`

	// Text is the beginning of what the model said, capped by TextLimit. It is
	// evidence that this was a completion rather than a stream of keep-alives.
	Text string `json:"text,omitempty"`
	// OutputTokens is what the backend billed for the probe, when it reported usage.
	OutputTokens uint64 `json:"output_tokens,omitempty"`

	// Duration is the same measurement as DurationMS, kept for the log line that
	// renders it as a Go duration. It never goes on the wire.
	Duration time.Duration `json:"-"`
}

// TextLimit caps how much of the answer is kept. A probe is not a way to read a
// model's output; the first few words are proof enough that it ran.
const TextLimit = 120

// ProbeChat asks the backend to serve one tiny completion from this account, on the
// given model, and reads enough of the stream to say whether it worked.
//
// It spends a small amount of the account's quota — this is a real request, which is
// the only kind that proves anything — so it is meant for an operator pressing a
// button, not for a health check on a timer.
func (c *Client) ProbeChat(ctx context.Context, creds *Credentials, modelUID string) ProbeResult {
	started := time.Now()
	res := ProbeResult{Model: modelUID}
	finish := func() ProbeResult {
		res.Duration = time.Since(started)
		res.DurationMS = res.Duration.Milliseconds()
		return res
	}

	if creds == nil || creds.APIKey == "" {
		res.Error = "no credential for this account"
		return finish()
	}
	if strings.TrimSpace(modelUID) == "" {
		// Not refused here for a reason of its own: an empty chat_model_uid is
		// accepted by the backend and silently served by the default model, which
		// would make the probe look like it tested something it did not.
		res.Error = "no model id to probe with"
		return finish()
	}

	req := &GetChatMessageRequest{
		Metadata: DefaultMetadata(creds.APIKey),
		Prompt:   probeSystemPrompt,
		ChatMessagePrompts: []ChatMessagePrompt{{
			MessageID: MustUUID(),
			Source:    SourceUser,
			Prompt:    probePrompt,
		}},
		RequestType: RequestTypeCascade,
		Configuration: &CompletionConfiguration{
			NumCompletions: 1,
			MaxTokens:      probeMaxTokens,
			// Every sampling field must be non-zero: with the configuration
			// present but temperature/top_p/top_k/max_newlines omitted the
			// backend answers invalid_argument, which is exactly the failure
			// the first version of this probe reported on a healthy account.
			// These are the values the real request path always sends.
			Temperature: 1.0,
			TopP:        0.95,
			TopK:        40,
			MaxNewlines: 400,
		},
		TrajectoryRef: NewTrajectoryReference(),
		PlannerMode:   PlannerModeDefault,
		ChatModelUID:  modelUID,
	}
	// With the shape hook installed, the probe's request prints next to the
	// client requests in the same log: the whole point of the probe in this
	// mode is to be the known-good shape a failing client can be diffed against.
	EmitRequestShape(req)

	stream, err := c.GetChatMessage(ctx, creds, req)
	if err != nil {
		// Anything that stopped the request before a stream opened: a refused
		// credential carries an HTTP status, a dead route or a timeout carries none.
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			res.StatusCode = httpErr.Status
		}
		var connectErr *ConnectError
		if errors.As(err, &connectErr) {
			res.ErrorCode = connectErr.Code
		}
		res.Error = err.Error()
		return finish()
	}
	defer stream.Close()

	// Past this point the backend answered 200, which on its own means nothing.
	res.StatusCode = http.StatusOK
	var frames int
	for {
		frame, err := stream.Recv()
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				// A clean end. Success depends on having seen a frame: a stream that
				// ended without ever sending one is an empty answer, not a healthy
				// account.
				if frames == 0 {
					res.Error = "the backend closed the stream without sending anything"
				} else {
					res.OK = true
				}
			default:
				var connectErr *ConnectError
				if errors.As(err, &connectErr) {
					res.ErrorCode = connectErr.Code
					res.Error = connectErr.Message
					if res.Error == "" {
						res.Error = connectErr.Error()
					}
				} else {
					res.Error = err.Error()
				}
			}
			return finish()
		}
		if frame == nil {
			continue
		}
		frames++
		if frame.ActualModelUID != "" {
			res.ModelServed = frame.ActualModelUID
		}
		if frame.DeltaText != "" && len(res.Text) < TextLimit {
			res.Text = capText(res.Text+frame.DeltaText, TextLimit)
		}
		if frame.Usage != nil && frame.Usage.OutputTokens > 0 {
			res.OutputTokens = frame.Usage.OutputTokens
		}
		if frames >= probeFrameLimit {
			// Cut it off rather than reading a runaway stream to the end. What has
			// arrived is enough to answer the question that was asked.
			res.OK = true
			res.Error = ""
			return finish()
		}
	}
}

// capText truncates to n bytes on a rune boundary, so a cap in the middle of a
// multi-byte character cannot leave a broken byte to be rendered on the page.
func capText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	// DecodeLastRuneInString reports a partial rune as RuneError with a width of
	// one byte, which is the signal that the cut landed inside a character. Only
	// the incomplete rune is dropped; anything malformed further back is left as
	// the backend sent it.
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
