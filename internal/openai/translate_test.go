package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"devin2proxy/internal/devin"
)

func TestContentRejectsARemoteImageURL(t *testing.T) {
	// The backend takes inline base64 only. Dropping a remote image quietly
	// would produce a confident answer about a picture the model never saw,
	// so the request has to fail instead.
	var c Content
	err := json.Unmarshal([]byte(`[{"type":"text","text":"what is in this image?"},
		{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]`), &c)
	if err == nil {
		t.Fatalf("a remote image URL was accepted and silently dropped: %+v", c)
	}
	if !strings.Contains(err.Error(), "data:") {
		t.Errorf("error = %q, want it to say images must be inline data URLs", err)
	}
}

func TestContentKeepsInlineImages(t *testing.T) {
	// A 1x1 PNG, the smallest real one.
	const png = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	var c Content
	if err := json.Unmarshal([]byte(`[{"type":"text","text":"describe"},
		{"type":"image_url","image_url":{"url":"`+png+`"}}]`), &c); err != nil {
		t.Fatalf("inline image refused: %v", err)
	}
	if c.Text != "describe" {
		t.Errorf("Text = %q", c.Text)
	}
	if len(c.Images) != 1 {
		t.Fatalf("Images = %d, want 1", len(c.Images))
	}
	if c.Images[0].MediaType != "image/png" {
		t.Errorf("MediaType = %q", c.Images[0].MediaType)
	}
	// The backend needs real dimensions: an image with none is answered as
	// though its content were unreadable.
	if c.Images[0].Width != 1 || c.Images[0].Height != 1 {
		t.Errorf("dimensions = %dx%d, want 1x1", c.Images[0].Width, c.Images[0].Height)
	}
}

func TestContentRefusesAnImagePartWithNoURL(t *testing.T) {
	var c Content
	if err := json.Unmarshal([]byte(`[{"type":"image_url"}]`), &c); err == nil {
		t.Fatal("an image part with no image_url was accepted")
	}
}

// The OpenAI face is where a client's JSON becomes a backend request, so the
// failures that matter here are the ones a client would otherwise never hear
// about: a parameter quietly substituted, an image quietly dropped.

// A frame carrying stop_reason ERROR is intercepted by the server before it
// reaches FinishReason, but the mapping must still not dress a failed
// generation up as anything other than an ordinary end.
func TestFinishReasonNeverInventsALoss(t *testing.T) {
	cases := map[int]string{
		devin.StopNormal:       "stop",
		devin.StopError:        "stop",
		devin.StopUnspecified:  "stop",
		devin.StopMaxTokens:    "length",
		devin.StopPartial:      "length",
		devin.StopIncomplete:   "length",
		devin.StopFunctionCall: "tool_calls",
	}
	for stop, want := range cases {
		if got := FinishReason(stop); got != want {
			t.Errorf("FinishReason(%d) = %q, want %q", stop, got, want)
		}
	}
}

func TestBuildDevinRequestRejectsNonPositiveMaxTokens(t *testing.T) {
	neg := -5
	zero := 0
	for _, tc := range []struct {
		name string
		v    *int
	}{
		{"negative", &neg},
		{"zero", &zero},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &ChatRequest{
				Messages:  []Message{{Role: "user", Content: Content{Text: "hi"}}},
				MaxTokens: tc.v,
			}
			_, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{})
			if err == nil {
				t.Fatalf("max_tokens %d was accepted; a confused client should hear back", *tc.v)
			}
			if !strings.Contains(err.Error(), "max_tokens") {
				t.Errorf("error = %q, want it to name max_tokens", err)
			}
		})
	}
}

func TestBuildDevinRequestRaisesSmallBudgets(t *testing.T) {
	// A positive max_tokens below the floor is raised, not refused: an
	// autocomplete client asking for 64 tokens is normal and the reasoning
	// budget would otherwise swallow the whole answer.
	small := 64
	req := &ChatRequest{
		Messages:  []Message{{Role: "user", Content: Content{Text: "hi"}}},
		MaxTokens: &small,
	}
	out, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{})
	if err != nil {
		t.Fatalf("BuildDevinRequest: %v", err)
	}
	if got := out.Configuration.MaxTokens; got != DefaultMinMaxTokens {
		t.Fatalf("MaxTokens = %d, want the floor %d", got, DefaultMinMaxTokens)
	}
}

func TestBuildDevinRequestRefusesAnHNItCannotHonour(t *testing.T) {
	three := 3
	one := 1
	req := &ChatRequest{
		Messages: []Message{{Role: "user", Content: Content{Text: "hi"}}},
		N:        &three,
	}
	_, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{})
	if err == nil {
		t.Fatal("n=3 was accepted; the backend serves one completion, so the client must be told")
	}
	if !strings.Contains(err.Error(), "n must be 1") {
		t.Errorf("error = %q", err)
	}
	req.N = &one
	if _, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{}); err != nil {
		t.Errorf("n=1 must work: %v", err)
	}
}

func TestBuildDevinRequestHonoursToolChoiceExactly(t *testing.T) {
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "get_weather"}}}
	base := func() *ChatRequest {
		return &ChatRequest{
			Messages: []Message{{Role: "user", Content: Content{Text: "hi"}}},
			Tools:    tools,
		}
	}

	// The default and "auto" both mean: give the backend the definitions and
	// let it decide.
	for _, choice := range []string{"", "null", `"auto"`} {
		req := base()
		if choice != "" {
			req.ToolChoice = json.RawMessage(choice)
		}
		out, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{})
		if err != nil {
			t.Fatalf("tool_choice %q: %v", choice, err)
		}
		if len(out.Tools) != 1 {
			t.Errorf("tool_choice %q sent %d tools, want the definition passed through", choice, len(out.Tools))
		}
	}

	// "none" is honoured by withholding the definitions entirely — the only
	// mechanism the backend has for "do not call tools".
	req := base()
	req.ToolChoice = json.RawMessage(`"none"`)
	out, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{})
	if err != nil {
		t.Fatalf("tool_choice none: %v", err)
	}
	if len(out.Tools) != 0 {
		t.Errorf("tool_choice \"none\" still sent %d tools", len(out.Tools))
	}

	// "required" and named choices force behaviour the backend cannot be told
	// about. Accepting them silently would answer a different question than the
	// one asked, so they are refused.
	for _, choice := range []string{`"required"`, `{"type":"function","function":{"name":"get_weather"}}`} {
		req := base()
		req.ToolChoice = json.RawMessage(choice)
		if _, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{}); err == nil {
			t.Errorf("tool_choice %s was accepted; the backend cannot be forced", choice)
		}
	}
}
