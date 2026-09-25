// Package openai defines the subset of the OpenAI HTTP API this proxy serves,
// plus the mapping to and from the Devin backend's protobuf messages.
package openai

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
)

// ----------------------------------------------------------------- requests

// ChatRequest is the body of POST /v1/chat/completions.
type ChatRequest struct {
	Model               string          `json:"model"`
	Messages            []Message       `json:"messages"`
	Stream              bool            `json:"stream"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
	Tools               []Tool          `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	StreamOptions       *StreamOptions  `json:"stream_options"`
	N                   *int            `json:"n"`
	// ResponseFormat and the sampling knobs below are accepted so a client that
	// sends them is not told the field is unknown, but the backend has no
	// equivalent for any of them. BuildDevinRequest reports the ones a request
	// actually carries — once, in one line — instead of either refusing them or
	// applying them silently while the client believes otherwise.
	ResponseFormat   json.RawMessage `json:"response_format"`
	FrequencyPenalty *float64        `json:"frequency_penalty"`
	PresencePenalty  *float64        `json:"presence_penalty"`
	Seed             *int            `json:"seed"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Message is one OpenAI chat message. Content is either a plain string or an
// array of typed parts, which Content handles transparently.
type Message struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content"`
	Name       string     `json:"name"`
	ToolCallID string     `json:"tool_call_id"`
	ToolCalls  []ToolCall `json:"tool_calls"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Content holds a message body that may arrive as a string or as an array of
// parts, and retains any inline images.
type Content struct {
	Text   string
	Images []Image
}

// Image is an inline image extracted from a content part. Width and Height are
// read from the image's own header, because the backend needs them.
type Image struct {
	MIMEBase64 string
	MediaType  string
	Width      uint64
	Height     uint64
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

func (c *Content) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	// A plain string is the common case.
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		c.Text = s
		return nil
	}
	var parts []contentPart
	if err := json.Unmarshal(b, &parts); err != nil {
		return fmt.Errorf("content must be a string or an array of parts: %w", err)
	}
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			c.Text += p.Text
		case "image_url", "input_image":
			if p.ImageURL == nil {
				return fmt.Errorf("content part %q carries no image_url", p.Type)
			}
			img, ok := parseDataURL(p.ImageURL.URL)
			if !ok {
				// Fail loudly rather than drop the image: the backend takes
				// inline base64 only, and a silently discarded image produces
				// a confident answer about a picture the model never saw.
				return fmt.Errorf("images must be sent as data: URLs with inline base64; "+
					"the backend cannot fetch %q", p.ImageURL.URL)
			}
			c.Images = append(c.Images, img)
		default:
			// Unknown parts used to be dropped silently, which can erase a whole
			// user turn when the text rode on the dropped part. OpenAI's own API
			// rejects an unknown part type, and a refusal that names the type
			// tells the client what to fix instead of answering as though the
			// content never existed.
			return fmt.Errorf("content part type %q is not supported: only text and image parts are accepted", p.Type)
		}
	}
	return nil
}

// MarshalJSON writes a plain string, matching what clients expect back — except
// when the content carries inline images. Then it writes the array-of-parts
// shape the request itself arrived in: a multimodal request captured as a bare
// string would lose the images entirely, and the request-capture feature exists
// so a failing request can be replayed and bisected offline. The parts shape is
// exactly what UnmarshalJSON accepts, so a capture round-trips.
func (c Content) MarshalJSON() ([]byte, error) {
	if len(c.Images) == 0 {
		return json.Marshal(c.Text)
	}
	parts := make([]contentPart, 0, len(c.Images)+1)
	if c.Text != "" {
		parts = append(parts, contentPart{Type: "text", Text: c.Text})
	}
	for _, img := range c.Images {
		parts = append(parts, contentPart{
			Type: "image_url",
			ImageURL: &struct {
				URL string `json:"url"`
			}{URL: "data:" + img.MediaType + ";base64," + img.MIMEBase64},
		})
	}
	return json.Marshal(parts)
}

// parseDataURL splits a data: URL into its media type and base64 payload. Only
// inline data is supported: the backend takes base64 image bytes, not URLs.
func parseDataURL(u string) (Image, bool) {
	const prefix = "data:"
	if len(u) < len(prefix) || u[:len(prefix)] != prefix {
		return Image{}, false
	}
	rest := u[len(prefix):]
	comma := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == ',' {
			comma = i
			break
		}
	}
	if comma < 0 {
		return Image{}, false
	}
	meta := rest[:comma]
	payload := rest[comma+1:]
	mediaType := meta
	for i := 0; i < len(meta); i++ {
		if meta[i] == ';' {
			mediaType = meta[:i]
			break
		}
	}
	w, h := imageSize(payload)
	return Image{MIMEBase64: payload, MediaType: mediaType, Width: w, Height: h}, true
}

// imageSize reads the pixel dimensions out of an inline image's own header.
//
// The backend's ImageData carries width and height as real fields, and sending
// them as zero is not equivalent to omitting them: measured against the live
// backend, an inline image with no dimensions is answered as though its content
// were unreadable, which is indistinguishable from a model that cannot see
// images at all. Only PNG and JPEG are sniffed, since those are what a data: URL
// carries in practice; anything else is left at zero.
// imageHeaderBytes is how much decoded image data the dimension sniffer can
// ever need: a PNG's IHDR sits in the first 24 bytes, and a JPEG's
// start-of-frame marker precedes the scan data, comfortably inside a few KiB.
// Sniffing from a prefix keeps a multi-megabyte data URL — already buffered
// whole in the request body — from being decoded a second time just to read a
// header.
const imageHeaderBytes = 8 << 10

// decodeImageHeader decodes roughly the first imageHeaderBytes of the payload.
// The URL-safe alphabet appears in real data URLs, and the sniffer only reads
// bytes, so it is normalised to the standard alphabet rather than rejected; the
// string is cut to whole base64 quanta first, so a prefix decodes without
// needing the padding that only the final quantum carries.
func decodeImageHeader(b64 string) []byte {
	if strings.ContainsAny(b64, "-_") {
		b64 = strings.NewReplacer("-", "+", "_", "/").Replace(b64)
	}
	if n := (imageHeaderBytes/3 + 1) * 4; len(b64) > n {
		b64 = b64[:n]
	}
	b64 = b64[:len(b64)-len(b64)%4]
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil
	}
	return raw
}

func imageSize(b64 string) (uint64, uint64) {
	raw := decodeImageHeader(b64)
	// PNG: an 8-byte signature, then the IHDR chunk whose type is followed by
	// width and height as big-endian uint32.
	if len(raw) >= 24 && bytes.Equal(raw[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) &&
		string(raw[12:16]) == "IHDR" {
		return uint64(binary.BigEndian.Uint32(raw[16:20])), uint64(binary.BigEndian.Uint32(raw[20:24]))
	}
	// JPEG: walk the marker segments looking for a start-of-frame, which is the
	// only place the dimensions appear.
	if len(raw) >= 4 && raw[0] == 0xFF && raw[1] == 0xD8 {
		for i := 2; i+4 <= len(raw); {
			if raw[i] != 0xFF {
				i++
				continue
			}
			marker := raw[i+1]
			// Standalone markers carry no length field.
			if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
				i += 2
				continue
			}
			segLen := int(binary.BigEndian.Uint16(raw[i+2 : i+4]))
			if segLen < 2 {
				break
			}
			if isStartOfFrame(marker) {
				if i+9 > len(raw) {
					break
				}
				h := binary.BigEndian.Uint16(raw[i+5 : i+7])
				w := binary.BigEndian.Uint16(raw[i+7 : i+9])
				return uint64(w), uint64(h)
			}
			i += 2 + segLen
		}
	}
	return 0, 0
}

func isStartOfFrame(marker byte) bool {
	switch marker {
	case 0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF:
		return true
	}
	return false
}

// ----------------------------------------------------------------- responses

type ChatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type ResponseMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
}

type ToolCallDelta struct {
	Index    int                   `json:"index"`
	ID       string                `json:"id,omitempty"`
	Type     string                `json:"type,omitempty"`
	Function ToolCallFunctionDelta `json:"function"`
}

// ToolCallFunctionDelta carries a tool call's name on the frame that opens it
// and a slice of its JSON arguments on every later frame.
type ToolCallFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// ErrorResponse is the OpenAI error envelope.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// ----------------------------------------------------------------- completions

// CompletionRequest is the body of the legacy POST /v1/completions. Coding
// tools use this endpoint for inline autocomplete (fill-in-the-middle), which is
// why Suffix, Stop and Stream all matter here.
type CompletionRequest struct {
	Model         string          `json:"model"`
	Prompt        json.RawMessage `json:"prompt"`
	Suffix        string          `json:"suffix"`
	Stream        bool            `json:"stream"`
	StreamOptions *StreamOptions  `json:"stream_options"`
	MaxTokens     *int            `json:"max_tokens"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	Stop          json.RawMessage `json:"stop"`
	Echo          bool            `json:"echo"`
}

// Completion is the legacy non-streaming completion response.
type Completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   *Usage             `json:"usage,omitempty"`
}

type CompletionChoice struct {
	Index        int    `json:"index"`
	Text         string `json:"text"`
	Logprobs     any    `json:"logprobs"`
	FinishReason string `json:"finish_reason"`
}

// CompletionChunk is one frame of a streamed legacy completion. The legacy shape
// carries `text` directly on the choice; there is no `delta` wrapper as there is
// in chat chunks.
type CompletionChunk struct {
	ID      string                  `json:"id"`
	Object  string                  `json:"object"`
	Created int64                   `json:"created"`
	Model   string                  `json:"model"`
	Choices []CompletionChunkChoice `json:"choices"`
	Usage   *Usage                  `json:"usage,omitempty"`
}

type CompletionChunkChoice struct {
	Text         string  `json:"text"`
	Index        int     `json:"index"`
	Logprobs     any     `json:"logprobs"`
	FinishReason *string `json:"finish_reason"`
}

// PromptText flattens the legacy prompt field, which may be a string or an
// array of strings.
func (r *CompletionRequest) PromptText() (string, error) {
	var s string
	if err := json.Unmarshal(r.Prompt, &s); err == nil {
		return s, nil
	}
	var parts []string
	if err := json.Unmarshal(r.Prompt, &parts); err != nil {
		return "", fmt.Errorf("prompt must be a string or array of strings: %w", err)
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out, nil
}
