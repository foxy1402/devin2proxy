package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"devin2proxy/internal/devin"
)

// Defaults mirror what the Devin CLI sends. max_newlines and top_k must be
// non-zero: the backend rejects the request with invalid_argument when they are
// left at zero, which is why they are always populated.
const (
	DefaultMaxTokens   = 128000
	DefaultTemperature = 1.0
	DefaultTopP        = 0.95
	DefaultTopK        = 40
	DefaultMaxNewlines = 400
)

// Sampling parameters are clamped into the range the backend accepts, which is
// narrower than OpenAI's in one direction and equal in the other. Measured
// against the live backend (tools/temperature-sweep.mjs):
//
//	temperature 0      -> 400 "an internal error occurred"
//	temperature 0.001  -> ok      temperature 2.0 -> ok      temperature 2.5 -> 502
//	top_p       0      -> 400 "an internal error occurred"
//	top_p       0.001  -> ok      top_p       1.0 -> ok
//
// The zero case is the one that matters in practice: a coding IDE asking for
// deterministic edits sends temperature 0, which is valid in OpenAI's API but
// fails here with a message that names no field. Clamping to the smallest
// accepted value keeps the request deterministic rather than silently falling
// back to the much hotter default.
const (
	MinTemperature = 0.001
	MaxTemperature = 2.0
	MinTopP        = 0.001
	MaxTopP        = 1.0
)

func clampTemperature(v float64) float64 {
	switch {
	case v <= 0:
		return MinTemperature
	case v > MaxTemperature:
		return MaxTemperature
	default:
		return v
	}
}

func clampTopP(v float64) float64 {
	switch {
	case v <= 0:
		return MinTopP
	case v > MaxTopP:
		return MaxTopP
	default:
		return v
	}
}

// AssistantLabel prefixes a replayed assistant turn. The backend has no
// assistant role (see devin.SourceUser), so earlier replies are sent as user
// prompts; the label plus AssistantLabelNote keep the model from reading its own
// words as user input.
const (
	AssistantLabel     = "Assistant: "
	AssistantLabelNote = "The conversation below may include the assistant's own earlier replies, " +
		"each introduced by the label \"Assistant:\". Text without that label is from the user."
)

// DefaultMinMaxTokens is the floor applied to a client's max_tokens.
//
// The backend charges the model's reasoning to the same budget as the answer,
// and the reasoning routinely runs to several thousand tokens: measured values
// are 663 for a fill-in-the-middle request whose answer was three tokens, 3308
// for naming the colour of an inline PNG, and 8046 for the same question phrased
// more open-endedly. An autocomplete client that asks for 64 tokens therefore
// gets an empty string with finish_reason "length", because the budget was spent
// before any answer text existed. Raising small requests to this floor is what
// makes them return anything at all.
//
// max_tokens is a ceiling rather than a spend — the model stops on its own — so
// a generous floor costs nothing unless the model would have rambled anyway. The
// earlier value of 4096 was too low: on an image prompt the model spent exactly
// 4096 tokens on reasoning and returned an empty answer, which is the worst
// outcome both ways, since the quota is consumed and nothing comes back.
const DefaultMinMaxTokens = 8192

// DefaultMaxToolDescBytes caps how much of a tool's description is forwarded.
// The backend's content screen covers tool definitions too, and it refuses
// whole request shapes over description text: one IDE's toolset — every other
// part of it servable, the same tools with shorter descriptions accepted —
// was refused wholesale until its descriptions were capped (measured
// 2026-09-25: 35 real tools pass with 600-byte descriptions and fail at
// 1000). The cap is indiscriminate lossy trimming: names, parameters and
// schemas — the parts function calling actually needs — are never touched,
// and a negative setting sends descriptions verbatim for an operator who
// would rather debug the refusal.
const DefaultMaxToolDescBytes = 512

// Options tunes the mapping from an OpenAI request to a backend request.
type Options struct {
	// MinMaxTokens overrides DefaultMinMaxTokens when positive.
	MinMaxTokens int
	// MaxToolDescBytes caps each tool description's length, on a rune boundary.
	// Zero uses DefaultMaxToolDescBytes; a negative value sends descriptions
	// verbatim. See DefaultMaxToolDescBytes for why a cap exists at all.
	MaxToolDescBytes int
}

func (o Options) minMaxTokens() uint64 {
	if o.MinMaxTokens > 0 {
		return uint64(o.MinMaxTokens)
	}
	return DefaultMinMaxTokens
}

// maxToolDescBytes resolves the effective description cap: the configured
// value, the default, or unlimited when the operator asked for it.
func (o Options) maxToolDescBytes() int {
	if o.MaxToolDescBytes > 0 {
		return o.MaxToolDescBytes
	}
	if o.MaxToolDescBytes < 0 {
		return -1
	}
	return DefaultMaxToolDescBytes
}

// DefaultSystemPrompt is used when the caller sends no system message. It is
// deliberately plain: the CLI's own system prompt describes an agent with a
// tool box, which is not what a chat completion endpoint wants.
const DefaultSystemPrompt = "You are a helpful assistant. Answer the user's request directly and concisely."

// ------------------------------------------------------------------ stop

// ParseStop reads the `stop` field, which OpenAI accepts as either a single
// string or an array of them.
func ParseStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil
	}
	out := many[:0]
	for _, s := range many {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// StopFilter truncates generated text at the first stop sequence and reports
// whether one was hit.
//
// A stop sequence can straddle two streamed fragments, so it is not enough to
// inspect each fragment on its own: text that might still turn out to be the
// start of a stop sequence has to be held back until the next fragment arrives
// or the stream ends. Without that, a stop string split across frames leaks
// into the caller's completion.
type StopFilter struct {
	stops   []string
	longest int
	pending string
	stopped bool
}

func NewStopFilter(stops []string) *StopFilter {
	f := &StopFilter{stops: stops}
	for _, s := range stops {
		if len(s) > f.longest {
			f.longest = len(s)
		}
	}
	return f
}

// Truncated reports whether a stop sequence has been seen.
func (f *StopFilter) Truncated() bool { return f.stopped }

// Write consumes a fragment and returns the text that is safe to emit now. Once
// a stop sequence has been seen everything is dropped: a stop means generation
// ends here, so emission is never resumed.
func (f *StopFilter) Write(s string) string {
	if len(f.stops) == 0 {
		return s
	}
	if f.stopped {
		return ""
	}
	f.pending += s
	if i, _ := f.indexOfStop(); i >= 0 {
		out := f.pending[:i]
		f.pending = ""
		f.stopped = true
		return out
	}
	// Hold back a suffix that could still become a stop sequence.
	keep := f.longest - 1
	if keep > len(f.pending) {
		keep = len(f.pending)
	}
	out := f.pending[:len(f.pending)-keep]
	f.pending = f.pending[len(f.pending)-keep:]
	return out
}

// Flush returns whatever is still held back once the stream has ended.
func (f *StopFilter) Flush() string {
	if f.stopped {
		return ""
	}
	out := f.pending
	f.pending = ""
	return out
}

func (f *StopFilter) indexOfStop() (int, string) {
	best, found := -1, ""
	for _, s := range f.stops {
		if i := strings.Index(f.pending, s); i >= 0 && (best < 0 || i < best) {
			best, found = i, s
		}
	}
	return best, found
}

// ------------------------------------------------------------------ fill-in-the-middle

// The backend is chat-only: it has no native fill-in-the-middle endpoint. FIM
// is therefore emulated with an instruction prompt, which is what other
// chat-backed proxies do. The tags and the explicit "only the middle" wording
// matter, because the model otherwise helpfully rewrites the whole file.
const (
	fimSystemPrompt = "You are a code completion engine. You are given the code before the cursor (PREFIX) " +
		"and the code after the cursor (SUFFIX). Reply with ONLY the text that belongs between them. " +
		"Do not repeat any part of the prefix or the suffix. Do not add explanation, commentary, " +
		"or markdown code fences. Match the surrounding indentation and style."

	continuationSystemPrompt = "You are a code and text completion engine. Continue the input exactly where " +
		"it stops. Reply with ONLY the continuation. Do not repeat the input, and do not add explanation " +
		"or markdown code fences."
)

// BuildFIMRequest turns a legacy completion request into a chat request. With a
// suffix it becomes a fill-in-the-middle instruction; without one it becomes a
// plain continuation.
func BuildFIMRequest(req *CompletionRequest) (*ChatRequest, error) {
	prompt, err := req.PromptText()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(prompt) == "" && strings.TrimSpace(req.Suffix) == "" {
		return nil, fmt.Errorf("prompt must not be empty")
	}

	system := continuationSystemPrompt
	var user string
	if req.Suffix != "" {
		system = fimSystemPrompt
		user = "<PREFIX>\n" + prompt + "\n</PREFIX>\n<SUFFIX>\n" + req.Suffix + "\n</SUFFIX>\n\n" +
			"Reply with only the text that goes between the prefix and the suffix:"
	} else {
		user = "<INPUT>\n" + prompt + "\n</INPUT>\n\nReply with only the continuation:"
	}

	return &ChatRequest{
		Model:         req.Model,
		Stream:        req.Stream,
		StreamOptions: req.StreamOptions,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		Messages:      []Message{{Role: "system", Content: Content{Text: system}}, {Role: "user", Content: Content{Text: user}}},
	}, nil
}

// BuildDevinRequest converts an OpenAI chat request into the backend's protobuf
// request.
func BuildDevinRequest(apiKey string, req *ChatRequest, model string, opts Options) (*devin.GetChatMessageRequest, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages must not be empty")
	}

	var systemParts []string
	var prompts []devin.ChatMessagePrompt
	var sawAssistant bool

	for i, m := range req.Messages {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			if m.Content.Text != "" {
				systemParts = append(systemParts, m.Content.Text)
			}
		case "user":
			if m.Content.Text == "" && len(m.Content.Images) == 0 {
				continue
			}
			p := devin.ChatMessagePrompt{
				MessageID: devin.MustUUID(),
				Source:    devin.SourceUser,
				Prompt:    m.Content.Text,
			}
			for _, img := range m.Content.Images {
				p.Images = append(p.Images, devin.ImageData{
					Base64Data: img.MIMEBase64,
					MimeType:   img.MediaType,
					Width:      img.Width,
					Height:     img.Height,
				})
			}
			prompts = append(prompts, p)

		case "assistant":
			if m.Content.Text == "" && len(m.ToolCalls) == 0 {
				continue
			}
			// Sent as a user prompt carrying the label, not as an assistant
			// turn: the backend refuses any source other than USER/SYSTEM/TOOL.
			// A turn that only calls a tool has no text of its own, but the
			// prompt must not be empty either: an empty prompt carrying tool
			// calls is rejected with 502 "third-party model provider is
			// experiencing issues", while the same tool calls alongside any
			// non-empty text succeed. So a tool-only turn gets a placeholder.
			prompt := m.Content.Text
			if prompt == "" {
				prompt = "(tool call)"
			}
			sawAssistant = true
			prompt = AssistantLabel + prompt
			p := devin.ChatMessagePrompt{
				MessageID: devin.MustUUID(),
				Source:    devin.SourceUser,
				Prompt:    prompt,
			}
			for _, tc := range m.ToolCalls {
				p.ToolCalls = append(p.ToolCalls, devin.ChatToolCall{
					ID:            tc.ID,
					Name:          tc.Function.Name,
					ArgumentsJSON: tc.Function.Arguments,
				})
			}
			prompts = append(prompts, p)

		case "tool", "function":
			prompts = append(prompts, devin.ChatMessagePrompt{
				MessageID:  devin.MustUUID(),
				Source:     devin.SourceTool,
				Prompt:     m.Content.Text,
				ToolCallID: m.ToolCallID,
			})

		default:
			return nil, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}

	if len(prompts) == 0 {
		return nil, fmt.Errorf("messages must contain at least one user, assistant or tool turn")
	}

	// The backend screens its instruction field and refuses some text outright
	// with an opaque permission_denied (measured 2026-09-25: one IDE's
	// security-policy paragraph was refused verbatim, the identical text was
	// served as a user turn, a one-word paraphrase was served, and the probe's
	// tiny instruction always serves). Most IDEs do not let their users edit
	// the system prompt that trips it, so the instruction slot always carries
	// this proxy's short, proven prompt — the shape the dashboard's Test uses —
	// and the client's own system prompt rides along as a tagged context block
	// in the first user turn, the way the CLI itself carries non-user context
	// on the wire. Nothing the client sent is dropped.
	instruction := DefaultSystemPrompt
	if sawAssistant {
		instruction += "\n\n" + AssistantLabelNote
	}
	if system := strings.TrimSpace(strings.Join(systemParts, "\n\n")); system != "" {
		prompts = append([]devin.ChatMessagePrompt{{
			MessageID: devin.MustUUID(),
			Source:    devin.SourceUser,
			Prompt:    "<system>\n" + system + "\n</system>",
		}}, prompts...)
	}

	// n and tool_choice used to be read into the request and then ignored, so a
	// client asking for three candidates silently got one, and a client forcing
	// or disabling tool use silently got the backend's own decision. Both are
	// now answered honestly: "n=1" and "auto" work, anything else is a 400.
	if req.N != nil && *req.N != 1 {
		return nil, fmt.Errorf("n must be 1 or omitted, got %d: the backend serves one completion per request", *req.N)
	}
	toolChoice := ""
	if len(req.ToolChoice) > 0 {
		toolChoice = strings.TrimSpace(string(req.ToolChoice))
	}
	skipTools := false
	switch toolChoice {
	case "", "null", `"auto"`:
		// The default: the backend is given the tool definitions and decides.
	case `"none"`:
		// The one non-default value this proxy can honour exactly: sending no
		// tool definitions at all is what "do not call tools" means here.
		skipTools = true
	default:
		return nil, fmt.Errorf("tool_choice %s is not supported: the backend decides on its own whether to call a tool", toolChoice)
	}

	maxTokens := uint64(DefaultMaxTokens)
	if v := firstInt(req.MaxTokens, req.MaxCompletionTokens); v != nil {
		// A client that asks for zero or a negative budget is confused, and
		// quietly substituting the default would answer a different question
		// than the one asked. OpenAI itself rejects max_tokens < 1.
		if *v < 1 {
			return nil, fmt.Errorf("max_tokens must be at least 1, got %d", *v)
		}
		maxTokens = uint64(*v)
	}
	if floor := opts.minMaxTokens(); maxTokens < floor {
		maxTokens = floor
	}
	temperature := DefaultTemperature
	if req.Temperature != nil {
		temperature = clampTemperature(*req.Temperature)
	}
	topP := DefaultTopP
	if req.TopP != nil {
		topP = clampTopP(*req.TopP)
	}

	out := &devin.GetChatMessageRequest{
		Metadata:           devin.DefaultMetadata(apiKey),
		Prompt:             instruction,
		ChatMessagePrompts: prompts,
		RequestType:        devin.RequestTypeCascade,
		Configuration: &devin.CompletionConfiguration{
			NumCompletions: 1,
			MaxTokens:      maxTokens,
			Temperature:    temperature,
			TopP:           topP,
			TopK:           DefaultTopK,
			MaxNewlines:    DefaultMaxNewlines,
		},
		// A fresh trajectory is what starts a new conversation. Without it (and
		// without a cascade id the backend already knows) it rejects the call.
		TrajectoryRef: devin.NewTrajectoryReference(),
		PlannerMode:   devin.PlannerModeDefault,
		ChatModelUID:  model,
	}

	for _, t := range req.Tools {
		if skipTools {
			break
		}
		if t.Type != "" && t.Type != "function" {
			return nil, fmt.Errorf("tools: only type \"function\" is supported, got %q", t.Type)
		}
		schema := string(t.Function.Parameters)
		if schema == "" {
			schema = "{}"
		}
		out.Tools = append(out.Tools, devin.ChatToolDefinition{
			Name:             t.Function.Name,
			Description:      truncateRunes(t.Function.Description, opts.maxToolDescBytes()),
			JSONSchemaString: schema,
		})
	}

	return out, nil
}

func firstInt(vals ...*int) *int {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// FinishReason maps the backend's stop reason onto OpenAI's vocabulary.
//
// StopError is not a finish reason at all: the server treats a frame carrying
// it as a mid-stream failure and reports an error, so this case only catches a
// caller that reaches the mapping directly. It returns "stop" rather than
// pretending the answer was complete.
func FinishReason(stopReason int) string {
	switch stopReason {
	case devin.StopMaxTokens, devin.StopPartial, devin.StopIncomplete:
		return "length"
	case devin.StopFunctionCall:
		return "tool_calls"
	case devin.StopError:
		return "stop"
	default:
		return "stop"
	}
}

// ToolCallUpdate is one streamed tool-call fragment resolved against the
// accumulated list.
type ToolCallUpdate struct {
	// Index is the position of the tool call this fragment belongs to.
	Index int
	// Started is true for the fragment that opened a new tool call.
	Started bool
	// ID and Name are set only on the opening fragment.
	ID   string
	Name string
	// Arguments is the fragment's slice of the JSON arguments, to be appended.
	Arguments string
}

// ToolCallAccumulator merges the backend's streamed tool-call fragments.
//
// The backend sends one delta carrying the call's id and name, then further
// deltas carrying only slices of the JSON arguments with no id. Treating each
// delta as a whole call yields a burst of nameless calls with truncated
// arguments, so fragments are appended by position here instead.
type ToolCallAccumulator struct {
	calls []ToolCall
}

// Add merges one frame's deltas, returning how each fragment resolved.
func (a *ToolCallAccumulator) Add(deltas []devin.ChatToolCall) []ToolCallUpdate {
	out := make([]ToolCallUpdate, 0, len(deltas))
	for _, d := range deltas {
		if d.ID != "" || len(a.calls) == 0 {
			a.calls = append(a.calls, ToolCall{ID: d.ID, Type: "function"})
			cur := &a.calls[len(a.calls)-1]
			cur.Function.Name = d.Name
			cur.Function.Arguments = d.ArgumentsJSON
			out = append(out, ToolCallUpdate{
				Index:     len(a.calls) - 1,
				Started:   true,
				ID:        d.ID,
				Name:      d.Name,
				Arguments: d.ArgumentsJSON,
			})
			continue
		}
		cur := &a.calls[len(a.calls)-1]
		if d.Name != "" && cur.Function.Name == "" {
			cur.Function.Name = d.Name
		}
		cur.Function.Arguments += d.ArgumentsJSON
		out = append(out, ToolCallUpdate{
			Index:     len(a.calls) - 1,
			Name:      d.Name,
			Arguments: d.ArgumentsJSON,
		})
	}
	return out
}

// Calls returns the merged tool calls, with placeholder ids supplied where the
// backend omitted one.
func (a *ToolCallAccumulator) Calls() []ToolCall {
	if len(a.calls) == 0 {
		return nil
	}
	out := make([]ToolCall, len(a.calls))
	copy(out, a.calls)
	for i := range out {
		if out[i].ID == "" {
			out[i].ID = fmt.Sprintf("call_%d", i)
		}
	}
	return out
}

// Len reports how many tool calls have been opened.
func (a *ToolCallAccumulator) Len() int { return len(a.calls) }

// UsageFromStats converts backend token accounting to OpenAI usage. The backend
// sends a usage message on early frames carrying only the model uid and zero
// counts, and generation stopped by a stop sequence never reports totals at all;
// in both cases a zero usage is omitted rather than reported, because a client
// shows "0 tokens" as fact.
func UsageFromStats(u *devin.ModelUsageStats) *Usage {
	if u == nil {
		return nil
	}
	prompt := int(u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens)
	completion := int(u.OutputTokens)
	if prompt == 0 && completion == 0 {
		return nil
	}
	return &Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
}

// MarshalError renders an OpenAI-shaped error body.
func MarshalError(message, errType, code string) []byte {
	b, err := json.Marshal(ErrorResponse{Error: ErrorDetail{
		Message: message,
		Type:    errType,
		Code:    code,
	}})
	if err != nil {
		return []byte(`{"error":{"message":"internal error","type":"api_error"}}`)
	}
	return b
}

// truncateRunes cuts s to max bytes on a rune boundary, so the cut cannot
// split a multi-byte character and leave a broken byte for the model to read.
// max below zero means unlimited; zero-length results stay empty.
func truncateRunes(s string, max int) string {
	if max < 0 || len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}
