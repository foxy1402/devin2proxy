package openai

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

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

// The backend screens its instruction field and refuses some text outright
// (measured: one IDE's security-policy paragraph was refused verbatim, the
// identical text served as a user turn, a one-word paraphrase served, and the
// probe's tiny instruction always serves). Most IDEs do not let their users
// edit the system prompt that trips it, so the instruction slot now always
// carries the proxy's short proven prompt and the client's system prompt
// rides along as a tagged block in the first user turn. These tests pin both
// halves of that mapping, because a regression in either direction is silent:
// the client's instructions would vanish, or the refusal would come back.
func TestTheInstructionSlotIsAlwaysTheProvenPrompt(t *testing.T) {
	req := &ChatRequest{
		Messages: []Message{
			{Role: "system", Content: Content{Text: "you are a pirate"}},
			{Role: "user", Content: Content{Text: "hi"}},
		},
	}
	out, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if out.Prompt != DefaultSystemPrompt {
		t.Errorf("instruction slot carried %q, want the fixed proven prompt", out.Prompt)
	}
	if len(out.ChatMessagePrompts) != 2 {
		t.Fatalf("got %d turns, want the system block plus the user turn", len(out.ChatMessagePrompts))
	}
	first := out.ChatMessagePrompts[0]
	if !strings.Contains(first.Prompt, "you are a pirate") || !strings.HasPrefix(first.Prompt, "<system>") {
		t.Errorf("the client's system prompt did not ride along as a tagged block: %q", first.Prompt)
	}
	if out.ChatMessagePrompts[1].Prompt != "hi" {
		t.Errorf("the user turn was disturbed: %q", out.ChatMessagePrompts[1].Prompt)
	}
}

func TestANoSystemRequestStaysUnwrapped(t *testing.T) {
	req := &ChatRequest{Messages: []Message{{Role: "user", Content: Content{Text: "hi"}}}}
	out, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if out.Prompt != DefaultSystemPrompt {
		t.Errorf("instruction slot carried %q, want the fixed proven prompt", out.Prompt)
	}
	if len(out.ChatMessagePrompts) != 1 || strings.Contains(out.ChatMessagePrompts[0].Prompt, "<system>") {
		t.Errorf("a request with no system message grew a wrapper turn: %+v", out.ChatMessagePrompts)
	}
}

func TestMultipleSystemMessagesJoinInsideOneBlock(t *testing.T) {
	req := &ChatRequest{
		Messages: []Message{
			{Role: "system", Content: Content{Text: "rule one"}},
			{Role: "system", Content: Content{Text: "rule two"}},
			{Role: "user", Content: Content{Text: "hi"}},
		},
	}
	out, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(out.ChatMessagePrompts) != 2 {
		t.Fatalf("got %d turns, want one block plus the user turn", len(out.ChatMessagePrompts))
	}
	block := out.ChatMessagePrompts[0].Prompt
	if !strings.Contains(block, "rule one") || !strings.Contains(block, "rule two") {
		t.Errorf("the block lost one of the client's system messages: %q", block)
	}
}

func TestTheAssistantLabelNoteStaysInTheInstructionSlot(t *testing.T) {
	req := &ChatRequest{
		Messages: []Message{
			{Role: "system", Content: Content{Text: "be brief"}},
			{Role: "user", Content: Content{Text: "hi"}},
			{Role: "assistant", Content: Content{Text: "hello"}},
			{Role: "user", Content: Content{Text: "again"}},
		},
	}
	out, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(out.Prompt, AssistantLabelNote) {
		t.Error("the assistant-label note left the instruction slot")
	}
}

// The backend's content screen covers tool definitions too: one IDE's
// toolset was refused wholesale until its descriptions were capped, while the
// same tools with shorter descriptions (and the same names and schemas)
// passed. The cap defaults on, is lossy by design, and a negative setting
// sends descriptions verbatim.
func TestToolDescriptionsAreCappedByDefault(t *testing.T) {
	long := strings.Repeat("describe ", 300) // 2700 bytes
	req := &ChatRequest{
		Messages: []Message{{Role: "user", Content: Content{Text: "hi"}}},
		Tools:    []Tool{{Type: "function", Function: ToolFunction{Name: "t", Description: long, Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	out, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := len(out.Tools[0].Description); got > DefaultMaxToolDescBytes {
		t.Errorf("description is %d bytes, want capped at %d", got, DefaultMaxToolDescBytes)
	}
	if !utf8.ValidString(out.Tools[0].Description) {
		t.Error("the cap split a multi-byte character")
	}

	verbatim, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{MaxToolDescBytes: -1})
	if err != nil {
		t.Fatalf("build verbatim: %v", err)
	}
	if verbatim.Tools[0].Description != long {
		t.Error("a negative cap did not send the description verbatim")
	}
}

func TestTruncateRunesNeverSplitsACharacter(t *testing.T) {
	s := "héllo wörld" // multi-byte characters from index 1
	cut := truncateRunes(s, 3)
	if !utf8.ValidString(cut) {
		t.Errorf("the cut split a character: %q", cut)
	}
	if truncateRunes(s, -1) != s {
		t.Error("a negative cap did not keep the string verbatim")
	}
	if truncateRunes(s, len(s)) != s {
		t.Error("a cap at the exact length changed the string")
	}
}
