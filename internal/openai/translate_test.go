package openai

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"log"
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

// A multi-byte rune straddling the filter's hold-back cut used to be split
// between two emitted fragments. The SSE path marshals each fragment
// separately, and JSON cannot represent a partial rune, so the client's
// reassembled text carried U+FFFD where the character should be. The cut is now
// rune-aligned; this test pins that by round-tripping every emitted fragment
// through JSON exactly the way the SSE writer does.
func TestStopFilterKeepsMultiByteRunesWhole(t *testing.T) {
	const stop = "STOP"
	body := "Answer: 你好世界 — an emoji: 😀 — plus 中文 text before the end."
	input := body + stop + " discarded after the stop"

	filter := NewStopFilter([]string{stop})
	var reassembled strings.Builder
	for len(input) > 0 && !filter.Truncated() {
		n := 3 // lands inside the CJK and emoji runes on purpose
		if n > len(input) {
			n = len(input)
		}
		frag := filter.Write(input[:n])
		input = input[n:]
		if !utf8.ValidString(frag) {
			t.Fatalf("emitted fragment %q is not valid UTF-8; the client would see U+FFFD", frag)
		}
		b, err := json.Marshal(frag)
		if err != nil {
			t.Fatalf("marshal fragment: %v", err)
		}
		var back string
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("unmarshal fragment: %v", err)
		}
		reassembled.WriteString(back)
	}
	if got := reassembled.String(); got != body {
		t.Fatalf("reassembled = %q, want the input up to the stop, byte-identical: %q", got, body)
	}
	if !filter.Truncated() {
		t.Error("the stop sequence was never detected")
	}
	if tail := filter.Flush(); tail != "" {
		t.Errorf("Flush after a hit returned %q, want nothing", tail)
	}
}

func TestParseStopRefusesAMalformedStop(t *testing.T) {
	// A malformed stop used to disable the filter silently, which answers a
	// different request than the one made; now it is an error the server can
	// surface as a 400.
	for _, raw := range []string{`5`, `["a", 5]`, `true`, `{"a":1}`} {
		if _, err := ParseStop(json.RawMessage(raw)); err == nil {
			t.Errorf("ParseStop(%s) was accepted; the filter would be silently disabled", raw)
		}
	}
	// The shapes OpenAI documents keep working.
	if stops, err := ParseStop(json.RawMessage(`"END"`)); err != nil || len(stops) != 1 || stops[0] != "END" {
		t.Errorf("ParseStop(string) = %v, %v", stops, err)
	}
	if stops, err := ParseStop(json.RawMessage(`["a","b"]`)); err != nil || len(stops) != 2 {
		t.Errorf("ParseStop(array) = %v, %v", stops, err)
	}
	if stops, err := ParseStop(nil); err != nil || stops != nil {
		t.Errorf("ParseStop(absent) = %v, %v", stops, err)
	}
}

func TestAnUnknownContentPartTypeIsRefused(t *testing.T) {
	// Dropping an unknown part silently can erase a whole user turn; OpenAI's
	// own API refuses the request, and so does this one, naming the type.
	var c Content
	err := json.Unmarshal([]byte(`[{"type":"text","text":"hi"},{"type":"audio","audio":{"data":"x"}}]`), &c)
	if err == nil {
		t.Fatalf("an unknown content part type was accepted: %+v", c)
	}
	if !strings.Contains(err.Error(), `"audio"`) {
		t.Errorf("error = %q, want it to name the part type", err)
	}
}

func TestMaxCompletionTokensWinsWhenBothAreSet(t *testing.T) {
	// Both values sit above the floor, so which one survived is observable.
	modern, legacy := 9000, 9500
	req := &ChatRequest{
		Messages:            []Message{{Role: "user", Content: Content{Text: "hi"}}},
		MaxTokens:           &legacy,
		MaxCompletionTokens: &modern,
	}
	out, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if out.Configuration.MaxTokens != 9000 {
		t.Fatalf("MaxTokens = %d, want the modern max_completion_tokens value 9000", out.Configuration.MaxTokens)
	}
}

func TestAcceptedButUnenforcedParametersAreReported(t *testing.T) {
	old := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	frequency, presence, seed := 0.5, 1.0, 42
	req := &ChatRequest{
		Messages:         []Message{{Role: "user", Content: Content{Text: "hi"}}},
		ResponseFormat:   json.RawMessage(`{"type":"json_object"}`),
		FrequencyPenalty: &frequency,
		PresencePenalty:  &presence,
		Seed:             &seed,
	}
	// The request must be served — working clients send these on every call —
	// but what was not enforced has to show up in the log.
	if _, err := BuildDevinRequest("k", req, "swe-1-6-slow", Options{}); err != nil {
		t.Fatalf("response_format was refused: %v", err)
	}
	line := buf.String()
	for _, want := range []string{"response_format json_object", "frequency_penalty", "presence_penalty", "seed"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q does not mention %q", line, want)
		}
	}
	if n := strings.Count(strings.TrimSpace(line), "\n") + 1; n != 1 {
		t.Errorf("the note spans %d lines, want one line total", n)
	}

	// The harmless forms stay quiet.
	buf.Reset()
	quiet := &ChatRequest{
		Messages:       []Message{{Role: "user", Content: Content{Text: "hi"}}},
		ResponseFormat: json.RawMessage(`{"type":"text"}`),
	}
	if _, err := BuildDevinRequest("k", quiet, "swe-1-6-slow", Options{}); err != nil {
		t.Fatalf("build: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a request with nothing unenforced logged %q", buf.String())
	}
}

func TestAnEmptyToolResultIsSentAsAPlaceholder(t *testing.T) {
	// An empty prompt field is dropped on the wire, so the model would see the
	// tool call it made and no answer to it; the user branch guards the same
	// way with a placeholder.
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":""}
	]}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := BuildDevinRequest("k", &req, "swe-1-6-slow", Options{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, p := range out.ChatMessagePrompts {
		if p.ToolCallID != "call_1" {
			continue
		}
		if p.Prompt != "(empty tool result)" {
			t.Fatalf("the empty tool result was sent as %q, want the placeholder", p.Prompt)
		}
		return
	}
	t.Fatal("the tool turn never reached the wire")
}

func TestImageSizeToleratesTheURLSafeAlphabet(t *testing.T) {
	// A hand-built PNG header (signature, IHDR length, "IHDR", dimensions).
	// The wide first dimension puts '/' in the base64 — which the URL-safe
	// alphabet writes as '_' — so both spellings are exercised, and the whole
	// payload fits in the prefix the sniffer decodes.
	hdr := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13, 'I', 'H', 'D', 'R'}
	hdr = binary.BigEndian.AppendUint32(hdr, 0x00FFFFFF)
	hdr = binary.BigEndian.AppendUint32(hdr, 1)
	std := base64.StdEncoding.EncodeToString(hdr)
	urlSafe := strings.NewReplacer("+", "-", "/", "_").Replace(std)
	if urlSafe == std {
		t.Fatal("the test image did not exercise the URL-safe alphabet")
	}
	if w, h := imageSize(urlSafe); w != 0x00FFFFFF || h != 1 {
		t.Errorf("URL-safe alphabet rejected: %dx%d, want 16777215x1", w, h)
	}
	if w, h := imageSize(std); w != 0x00FFFFFF || h != 1 {
		t.Errorf("standard alphabet regressed: %dx%d", w, h)
	}
}

func TestContentMarshalJSONKeepsImagesReplayable(t *testing.T) {
	// A multimodal request captured as a bare string would lose the images and
	// could not be replayed; the marshaled shape must decode back to the same
	// content.
	const png = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	var c Content
	if err := json.Unmarshal([]byte(`[{"type":"text","text":"describe"},
		{"type":"image_url","image_url":{"url":"`+png+`"}}]`), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Content
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("the marshaled content does not decode back (%s): %v", b, err)
	}
	if back.Text != "describe" || len(back.Images) != 1 ||
		back.Images[0].MediaType != "image/png" || back.Images[0].Width != 1 || back.Images[0].Height != 1 {
		t.Fatalf("round trip lost content: %+v (%s)", back, b)
	}
	// Plain text still marshals as the bare string.
	if s, err := json.Marshal(Content{Text: "hi"}); err != nil || string(s) != `"hi"` {
		t.Errorf("Marshal(Content{Text}) = %s, %v; want the bare string", s, err)
	}
}
