// Package devin implements the wire protocol the Devin CLI uses to talk to its
// inference backend (Codeium's "exa" Connect-RPC service).
//
// The field numbers below were recovered from a live capture of the CLI's own
// traffic (see docs/PROTOCOL.md) and cross-checked against the protobuf
// definitions published by the MIT-licensed dsh-plugin-devin-bridge project.
package devin

import (
	"fmt"
	"strings"

	"devin2proxy/internal/pb"
)

// GetChatMessageRequest field numbers.
const (
	reqMetadata           = 1
	reqPrompt             = 2
	reqChatMessagePrompts = 3
	reqRequestType        = 7
	reqConfiguration      = 8
	reqTools              = 10
	reqTrajectoryRef      = 15
	reqCascadeID          = 16
	reqPlannerMode        = 20
	reqChatModelUID       = 21
	reqExecutionID        = 22
)

// GetChatMessageResponse field numbers.
const (
	respMessageID      = 1
	respTimestamp      = 2
	respDeltaText      = 3
	respDeltaTokens    = 4
	respStopReason     = 5
	respDeltaToolCalls = 6
	respUsage          = 7
	respRedact         = 8
	respDeltaThinking  = 9
	respDeltaSignature = 10
	respThinkingRedact = 11
	respLatency        = 12
	respRequestID      = 17
	respActualModelUID = 23
	// respDimensionGroups carries per-response telemetry ("Response
	// Statistics"). Recognised only so it can be skipped cleanly.
	respDimensionGroups = 28
)

// Metadata field numbers.
const (
	mdIDEName          = 1
	mdExtensionVersion = 2
	mdAPIKey           = 3
	mdLocale           = 4
	mdOS               = 5
	mdIDEVersion       = 7
	mdExtensionName    = 12
	mdField28          = 28
	mdField30          = 30
	mdField31          = 31
)

// ChatMessagePrompt field numbers.
const (
	cmpMessageID         = 1
	cmpSource            = 2
	cmpPrompt            = 3
	cmpToolCalls         = 6
	cmpToolCallID        = 7
	cmpToolResultIsError = 9
	cmpImages            = 10
	cmpThinking          = 11
	cmpSignature         = 12
	cmpThinkingRedacted  = 13
)

// CompletionConfiguration field numbers.
const (
	ccNumCompletions = 1
	ccMaxTokens      = 2
	ccMaxNewlines    = 3
	ccTemperature    = 5
	ccTopK           = 7
	ccTopP           = 8
)

// ModelUsageStats field numbers.
const (
	musInputTokens      = 2
	musOutputTokens     = 3
	musCacheWriteTokens = 4
	musCacheReadTokens  = 5
	musModelUID         = 9
)

// ChatMessageSource values (ChatMessagePrompt.source).
//
// There is no assistant role. Sweeping every candidate on the live backend
// showed only USER, SYSTEM and TOOL are accepted; UNSPECIFIED (0), 3, 5 and 6
// are all refused with "the third-party model provider is experiencing
// issues". The real CLI likewise sends a whole conversation as USER prompts,
// carrying non-user context in XML-ish tags inside the prompt text.
const (
	SourceUnspecified = 0
	SourceUser        = 1
	SourceSystem      = 2
	SourceTool        = 4
)

// ChatMessageRequestType values (GetChatMessageRequest.request_type).
const (
	RequestTypeUnspecified = 0
	RequestTypeCascade     = 5
)

// ConversationalPlannerMode values (GetChatMessageRequest.planner_mode).
const (
	PlannerModeUnspecified = 0
	PlannerModeDefault     = 1
)

// StopReason values (GetChatMessageResponse.stop_reason). StopNormal was
// observed on the wire as 2 for an ordinary end-of-turn; it is absent from the
// trimmed third-party schema, so it is named from the capture.
const (
	StopUnspecified  = 0
	StopIncomplete   = 1
	StopNormal       = 2
	StopMaxTokens    = 3
	StopPartial      = 9
	StopFunctionCall = 10
	StopError        = 13
)

func SourceName(v int) string {
	switch v {
	case SourceUser:
		return "USER"
	case SourceSystem:
		return "SYSTEM"
	case SourceTool:
		return "TOOL"
	default:
		return fmt.Sprintf("SOURCE(%d)", v)
	}
}

func StopReasonName(v int) string {
	switch v {
	case StopIncomplete:
		return "INCOMPLETE"
	case StopMaxTokens:
		return "MAX_TOKENS"
	case StopPartial:
		return "PARTIAL"
	case StopFunctionCall:
		return "FUNCTION_CALL"
	case StopError:
		return "ERROR"
	case StopUnspecified:
		return "UNSPECIFIED"
	case StopNormal:
		return "STOP"
	default:
		return fmt.Sprintf("STOP(%d)", v)
	}
}

// --------------------------------------------------------------- structures

type Metadata struct {
	IDEName          string
	ExtensionVersion string
	APIKey           string
	Locale           string
	OS               string
	IDEVersion       string
	ExtensionName    string
	Field28          string
	Field30          []uint64
	Field31          string
}

// ImageData carries an inline image on a ChatMessagePrompt.
//
// The field numbers were NOT recovered from a capture — the CLI never sent an
// image while the probe was running, so the original guess (base64_data=1,
// mime_type=2) came from third-party prior art. That guess was wrong: the CLI
// binary contains a reflection table reading
//
//	struct ImageData with 6 elements
//	  width height base64_data mime_type source_path caption
//
// which puts base64_data at 3 and mime_type at 4. The wrong guess failed
// silently rather than loudly, because a length-delimited value in a field the
// backend declares as varint is skipped as unknown — the image simply never
// arrived, and the model answered as if the message were text-only.
const (
	imgWidth      = 1
	imgHeight     = 2
	imgBase64Data = 3
	imgMimeType   = 4
	imgSourcePath = 5
	imgCaption    = 6
)

type ImageData struct {
	// Width and Height are optional; omitting them is fine.
	Width      uint64
	Height     uint64
	Base64Data string
	MimeType   string
	SourcePath string
	Caption    string
}

// ChatToolCall is used in both directions: in a request it carries a completed
// call from history; in a response chunk it carries a streamed delta.
type ChatToolCall struct {
	ID            string
	Name          string
	ArgumentsJSON string
}

type ChatToolDefinition struct {
	Name             string
	Description      string
	JSONSchemaString string
}

// TrajectoryReference field numbers.
const (
	trTrajectoryID   = 1
	trTrajectoryType = 3
	trField4         = 4
)

// TrajectoryReference points the request at a Cortex trajectory. The capture
// shows {trajectory_id: <uuid>, trajectory_type: 4, field4: 14}. Field 4's
// meaning is unknown; it is round-tripped faithfully.
type TrajectoryReference struct {
	TrajectoryID   string
	TrajectoryType int
	Field4         int

	Unknown []UnknownField
}

type ChatMessagePrompt struct {
	MessageID         string
	Source            int
	Prompt            string
	ToolCalls         []ChatToolCall
	ToolCallID        string
	ToolResultIsError bool
	Images            []ImageData
	Thinking          string
	Signature         string
	ThinkingRedacted  bool
}

type CompletionConfiguration struct {
	NumCompletions uint64
	MaxTokens      uint64
	MaxNewlines    uint64
	Temperature    float64
	TopK           uint64
	TopP           float64
}

type ModelUsageStats struct {
	InputTokens      uint64
	OutputTokens     uint64
	CacheWriteTokens uint64
	CacheReadTokens  uint64
	ModelUID         string
}

// NewTrajectoryReference returns a reference to a fresh trajectory.
//
// A request must carry either this or a cascade id the backend already knows;
// omitting both is rejected with invalid_argument. The capture shows the CLI
// sending {trajectory_id: <uuid>, trajectory_type: 4, field4: 14}, and a freshly
// generated uuid is accepted, which is what lets a caller start a new
// conversation.
func NewTrajectoryReference() *TrajectoryReference {
	return &TrajectoryReference{
		TrajectoryID:   MustUUID(),
		TrajectoryType: 4,
		Field4:         14,
	}
}

type GetChatMessageRequest struct {
	Metadata           *Metadata
	Prompt             string
	ChatMessagePrompts []ChatMessagePrompt
	RequestType        int
	Configuration      *CompletionConfiguration
	Tools              []ChatToolDefinition
	TrajectoryRef      *TrajectoryReference
	CascadeID          string
	PlannerMode        int
	ChatModelUID       string
	ExecutionID        string

	// Unknown records fields we did not recognise, so a schema mismatch shows
	// up as a log line instead of silently dropping data.
	Unknown []UnknownField
}

type GetChatMessageResponse struct {
	MessageID      string
	TimestampSecs  int64
	TimestampNanos int32
	DeltaText      string
	DeltaTokens    uint64
	StopReason     int
	StopReasonSet  bool
	DeltaToolCalls []ChatToolCall
	Usage          *ModelUsageStats
	Redact         bool
	DeltaThinking  string
	DeltaSignature string
	ThinkingRedact bool
	Latency        float64
	RequestID      string
	ActualModelUID string

	Unknown []UnknownField

	// truncated records that the message ran out mid-field. The field loop
	// cannot tell a clean end from a parse error on its own, and a partial
	// frame must never be presented as a complete one: Stream.recv reports
	// it as a stream error instead of handing back half a delta.
	truncated bool
}

// UnknownField captures a field we had no mapping for.
type UnknownField struct {
	Number int
	Wire   int
	Value  string
}

// ------------------------------------------------------------------ decoding

func decodeImageData(b []byte) ImageData {
	var out ImageData
	r := pb.NewReader(b)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case f == imgWidth && w == pb.WireVarint:
			out.Width = r.Varint()
		case f == imgHeight && w == pb.WireVarint:
			out.Height = r.Varint()
		case f == imgBase64Data && w == pb.WireBytes:
			out.Base64Data = r.String()
		case f == imgMimeType && w == pb.WireBytes:
			out.MimeType = r.String()
		case f == imgSourcePath && w == pb.WireBytes:
			out.SourcePath = r.String()
		case f == imgCaption && w == pb.WireBytes:
			out.Caption = r.String()
		default:
			r.Skip(w)
		}
	}
	return out
}

func decodeChatToolCall(b []byte) ChatToolCall {
	var out ChatToolCall
	r := pb.NewReader(b)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case f == 1 && w == pb.WireBytes:
			out.ID = r.String()
		case f == 2 && w == pb.WireBytes:
			out.Name = r.String()
		case f == 3 && w == pb.WireBytes:
			out.ArgumentsJSON = r.String()
		default:
			r.Skip(w)
		}
	}
	return out
}

func decodeChatToolDefinition(b []byte) ChatToolDefinition {
	var out ChatToolDefinition
	r := pb.NewReader(b)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case f == 1 && w == pb.WireBytes:
			out.Name = r.String()
		case f == 2 && w == pb.WireBytes:
			out.Description = r.String()
		case f == 3 && w == pb.WireBytes:
			out.JSONSchemaString = r.String()
		default:
			r.Skip(w)
		}
	}
	return out
}

func decodeChatMessagePrompt(b []byte) ChatMessagePrompt {
	var out ChatMessagePrompt
	r := pb.NewReader(b)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case f == cmpMessageID && w == pb.WireBytes:
			out.MessageID = r.String()
		case f == cmpSource && w == pb.WireVarint:
			out.Source = int(r.Varint())
		case f == cmpPrompt && w == pb.WireBytes:
			out.Prompt = r.String()
		case f == cmpToolCalls && w == pb.WireBytes:
			out.ToolCalls = append(out.ToolCalls, decodeChatToolCall(r.Bytes()))
		case f == cmpToolCallID && w == pb.WireBytes:
			out.ToolCallID = r.String()
		case f == cmpToolResultIsError && w == pb.WireVarint:
			out.ToolResultIsError = r.Bool()
		case f == cmpImages && w == pb.WireBytes:
			out.Images = append(out.Images, decodeImageData(r.Bytes()))
		case f == cmpThinking && w == pb.WireBytes:
			out.Thinking = r.String()
		case f == cmpSignature && w == pb.WireBytes:
			out.Signature = r.String()
		case f == cmpThinkingRedacted && w == pb.WireVarint:
			out.ThinkingRedacted = r.Bool()
		default:
			r.Skip(w)
		}
	}
	return out
}

func decodeTrajectoryReference(b []byte) *TrajectoryReference {
	out := &TrajectoryReference{}
	r := pb.NewReader(b)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case f == trTrajectoryID && w == pb.WireBytes:
			out.TrajectoryID = r.String()
		case f == trTrajectoryType && w == pb.WireVarint:
			out.TrajectoryType = int(r.Varint())
		case f == trField4 && w == pb.WireVarint:
			out.Field4 = int(r.Varint())
		default:
			out.Unknown = append(out.Unknown, recordUnknown(f, w, r))
		}
	}
	return out
}

func decodeMetadata(b []byte) *Metadata {
	out := &Metadata{}
	r := pb.NewReader(b)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case f == mdIDEName && w == pb.WireBytes:
			out.IDEName = r.String()
		case f == mdExtensionVersion && w == pb.WireBytes:
			out.ExtensionVersion = r.String()
		case f == mdAPIKey && w == pb.WireBytes:
			out.APIKey = r.String()
		case f == mdLocale && w == pb.WireBytes:
			out.Locale = r.String()
		case f == mdOS && w == pb.WireBytes:
			out.OS = r.String()
		case f == mdIDEVersion && w == pb.WireBytes:
			out.IDEVersion = r.String()
		case f == mdExtensionName && w == pb.WireBytes:
			out.ExtensionName = r.String()
		case f == mdField28 && w == pb.WireBytes:
			out.Field28 = r.String()
		case f == mdField30 && w == pb.WireVarint:
			out.Field30 = append(out.Field30, r.Varint())
		case f == mdField30 && w == pb.WireBytes:
			// Packed repeated varint. Read into a temporary and commit only
			// what parsed cleanly: an empty or truncated pack used to append
			// the 0 that a failed Varint() returns, inventing an entry.
			sub := pb.NewReader(r.Bytes())
			for sub.Remaining() > 0 {
				v := sub.Varint()
				if sub.Err() != nil {
					break
				}
				out.Field30 = append(out.Field30, v)
			}
		case f == mdField31 && w == pb.WireBytes:
			out.Field31 = r.String()
		default:
			r.Skip(w)
		}
	}
	return out
}

func decodeCompletionConfiguration(b []byte) *CompletionConfiguration {
	out := &CompletionConfiguration{}
	r := pb.NewReader(b)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case f == ccNumCompletions && w == pb.WireVarint:
			out.NumCompletions = r.Varint()
		case f == ccMaxTokens && w == pb.WireVarint:
			out.MaxTokens = r.Varint()
		case f == ccMaxNewlines && w == pb.WireVarint:
			out.MaxNewlines = r.Varint()
		case f == ccTemperature && w == pb.WireFixed64:
			out.Temperature = r.Double()
		case f == ccTopK && w == pb.WireVarint:
			out.TopK = r.Varint()
		case f == ccTopP && w == pb.WireFixed64:
			out.TopP = r.Double()
		default:
			r.Skip(w)
		}
	}
	return out
}

func decodeModelUsageStats(b []byte) *ModelUsageStats {
	out := &ModelUsageStats{}
	r := pb.NewReader(b)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case f == musInputTokens && w == pb.WireVarint:
			out.InputTokens = r.Varint()
		case f == musOutputTokens && w == pb.WireVarint:
			out.OutputTokens = r.Varint()
		case f == musCacheWriteTokens && w == pb.WireVarint:
			out.CacheWriteTokens = r.Varint()
		case f == musCacheReadTokens && w == pb.WireVarint:
			out.CacheReadTokens = r.Varint()
		case f == musModelUID && w == pb.WireBytes:
			out.ModelUID = r.String()
		default:
			r.Skip(w)
		}
	}
	return out
}

// DecodeGetChatMessageRequest parses one GetChatMessageRequest message.
func DecodeGetChatMessageRequest(b []byte) *GetChatMessageRequest {
	out := &GetChatMessageRequest{}
	r := pb.NewReader(b)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case f == reqMetadata && w == pb.WireBytes:
			out.Metadata = decodeMetadata(r.Bytes())
		case f == reqPrompt && w == pb.WireBytes:
			out.Prompt = r.String()
		case f == reqChatMessagePrompts && w == pb.WireBytes:
			out.ChatMessagePrompts = append(out.ChatMessagePrompts, decodeChatMessagePrompt(r.Bytes()))
		case f == reqRequestType && w == pb.WireVarint:
			out.RequestType = int(r.Varint())
		case f == reqConfiguration && w == pb.WireBytes:
			out.Configuration = decodeCompletionConfiguration(r.Bytes())
		case f == reqTools && w == pb.WireBytes:
			out.Tools = append(out.Tools, decodeChatToolDefinition(r.Bytes()))
		case f == reqTrajectoryRef && w == pb.WireBytes:
			out.TrajectoryRef = decodeTrajectoryReference(r.Bytes())
		case f == reqCascadeID && w == pb.WireBytes:
			out.CascadeID = r.String()
		case f == reqPlannerMode && w == pb.WireVarint:
			out.PlannerMode = int(r.Varint())
		case f == reqChatModelUID && w == pb.WireBytes:
			out.ChatModelUID = r.String()
		case f == reqExecutionID && w == pb.WireBytes:
			out.ExecutionID = r.String()
		default:
			out.Unknown = append(out.Unknown, recordUnknown(f, w, r))
		}
	}
	return out
}

// DecodeGetChatMessageResponse parses one streamed response chunk.
func DecodeGetChatMessageResponse(b []byte) *GetChatMessageResponse {
	out := &GetChatMessageResponse{}
	r := pb.NewReader(b)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case f == respMessageID && w == pb.WireBytes:
			out.MessageID = r.String()
		case f == respTimestamp && w == pb.WireBytes:
			sub := pb.NewReader(r.Bytes())
			for {
				ff, ww, ok := sub.Field()
				if !ok {
					break
				}
				switch {
				case ff == 1 && ww == pb.WireVarint:
					out.TimestampSecs = int64(sub.Varint())
				case ff == 2 && ww == pb.WireVarint:
					out.TimestampNanos = int32(sub.Varint())
				default:
					sub.Skip(ww)
				}
			}
		case f == respDeltaText && w == pb.WireBytes:
			out.DeltaText = r.String()
		case f == respDeltaTokens && w == pb.WireVarint:
			out.DeltaTokens = r.Varint()
		case f == respStopReason && w == pb.WireVarint:
			out.StopReason = int(r.Varint())
			out.StopReasonSet = true
		case f == respDeltaToolCalls && w == pb.WireBytes:
			out.DeltaToolCalls = append(out.DeltaToolCalls, decodeChatToolCall(r.Bytes()))
		case f == respUsage && w == pb.WireBytes:
			out.Usage = decodeModelUsageStats(r.Bytes())
		case f == respRedact && w == pb.WireVarint:
			out.Redact = r.Bool()
		case f == respDeltaThinking && w == pb.WireBytes:
			out.DeltaThinking = r.String()
		case f == respDeltaSignature && w == pb.WireBytes:
			out.DeltaSignature = r.String()
		case f == respThinkingRedact && w == pb.WireVarint:
			out.ThinkingRedact = r.Bool()
		case f == respLatency && w == pb.WireFixed64:
			out.Latency = r.Double()
		case f == respRequestID && w == pb.WireBytes:
			out.RequestID = r.String()
		case f == respActualModelUID && w == pb.WireBytes:
			out.ActualModelUID = r.String()
		case f == respDimensionGroups && w == pb.WireBytes:
			// Per-response telemetry; parsed past, never surfaced.
			r.Bytes()
		default:
			out.Unknown = append(out.Unknown, recordUnknown(f, w, r))
		}
	}
	out.truncated = r.Err() != nil
	return out
}

func recordUnknown(f, w int, r *pb.Reader) UnknownField {
	uf := UnknownField{Number: f, Wire: w}
	switch w {
	case pb.WireVarint:
		uf.Value = fmt.Sprintf("varint %d", r.Varint())
	case pb.WireFixed64:
		uf.Value = fmt.Sprintf("fixed64 %v", r.Double())
	case pb.WireFixed32:
		r.Skip(w)
		uf.Value = "fixed32"
	case pb.WireBytes:
		raw := r.Bytes()
		if len(raw) <= 64 {
			uf.Value = fmt.Sprintf("bytes %x", raw)
		} else {
			uf.Value = fmt.Sprintf("bytes[%d] %x...", len(raw), raw[:64])
		}
	default:
		r.Skip(w)
		uf.Value = "unsupported"
	}
	return uf
}

// ----------------------------------------------------------------- encoding

func (m *Metadata) appendTo(w *pb.Writer) {
	w.String(mdIDEName, m.IDEName)
	w.String(mdExtensionVersion, m.ExtensionVersion)
	w.String(mdAPIKey, m.APIKey)
	w.String(mdLocale, m.Locale)
	w.String(mdOS, m.OS)
	w.String(mdIDEVersion, m.IDEVersion)
	w.String(mdExtensionName, m.ExtensionName)
	w.String(mdField28, m.Field28)
	for _, v := range m.Field30 {
		w.Varint(mdField30, v)
	}
	w.String(mdField31, m.Field31)
}

// EncodeMetadataRequest builds the request body the seat-management RPCs expect.
//
// GetUserStatus and GetCliTeamSettings both take a request whose only field is a
// Metadata message at field 1 — the same message GetChatMessage embeds there —
// and that message's field 3 carries the session token. Sending an empty body
// instead is rejected with HTTP 400 invalid_argument, which is how the shape was
// found: a capture of `devin auth status` shows a 982-byte body beginning
// `0a d3 07` (field 1, 979 bytes) and continuing with the token at field 3.
func EncodeMetadataRequest(m *Metadata) []byte {
	w := pb.NewWriter()
	w.RawBytes(reqMetadata, EncodeMetadata(m))
	return w.Bytes()
}

// EncodeMetadata returns the Metadata message on its own.
func EncodeMetadata(m *Metadata) []byte {
	sub := pb.NewWriter()
	m.appendTo(sub)
	return sub.Bytes()
}

func encodeImageData(in ImageData) []byte {
	sub := pb.NewWriter()
	sub.Uint64(imgWidth, in.Width)
	sub.Uint64(imgHeight, in.Height)
	sub.String(imgBase64Data, in.Base64Data)
	sub.String(imgMimeType, in.MimeType)
	sub.String(imgSourcePath, in.SourcePath)
	sub.String(imgCaption, in.Caption)
	return sub.Bytes()
}

func encodeChatToolCall(in ChatToolCall) []byte {
	sub := pb.NewWriter()
	sub.String(1, in.ID)
	sub.String(2, in.Name)
	sub.String(3, in.ArgumentsJSON)
	return sub.Bytes()
}

func encodeChatToolDefinition(in ChatToolDefinition) []byte {
	sub := pb.NewWriter()
	sub.String(1, in.Name)
	sub.String(2, in.Description)
	sub.String(3, in.JSONSchemaString)
	return sub.Bytes()
}

func encodeChatMessagePrompt(in ChatMessagePrompt) []byte {
	sub := pb.NewWriter()
	sub.String(cmpMessageID, in.MessageID)
	sub.Enum(cmpSource, in.Source)
	sub.String(cmpPrompt, in.Prompt)
	for _, tc := range in.ToolCalls {
		sub.RawBytes(cmpToolCalls, encodeChatToolCall(tc))
	}
	sub.String(cmpToolCallID, in.ToolCallID)
	sub.Bool(cmpToolResultIsError, in.ToolResultIsError)
	for _, img := range in.Images {
		sub.RawBytes(cmpImages, encodeImageData(img))
	}
	sub.String(cmpThinking, in.Thinking)
	sub.String(cmpSignature, in.Signature)
	sub.Bool(cmpThinkingRedacted, in.ThinkingRedacted)
	return sub.Bytes()
}

// EncodeGetChatMessageRequest serialises a request.
func EncodeGetChatMessageRequest(in *GetChatMessageRequest) []byte {
	w := pb.NewWriter()
	if in.Metadata != nil {
		sub := pb.NewWriter()
		in.Metadata.appendTo(sub)
		w.RawBytes(reqMetadata, sub.Bytes())
	}
	w.String(reqPrompt, in.Prompt)
	for _, cmp := range in.ChatMessagePrompts {
		w.RawBytes(reqChatMessagePrompts, encodeChatMessagePrompt(cmp))
	}
	w.Enum(reqRequestType, in.RequestType)
	if in.Configuration != nil {
		sub := pb.NewWriter()
		sub.Uint64(ccNumCompletions, in.Configuration.NumCompletions)
		sub.Uint64(ccMaxTokens, in.Configuration.MaxTokens)
		sub.Uint64(ccMaxNewlines, in.Configuration.MaxNewlines)
		sub.Double(ccTemperature, in.Configuration.Temperature)
		sub.Uint64(ccTopK, in.Configuration.TopK)
		sub.Double(ccTopP, in.Configuration.TopP)
		w.RawBytes(reqConfiguration, sub.Bytes())
	}
	for _, t := range in.Tools {
		w.RawBytes(reqTools, encodeChatToolDefinition(t))
	}
	if in.TrajectoryRef != nil {
		sub := pb.NewWriter()
		sub.String(trTrajectoryID, in.TrajectoryRef.TrajectoryID)
		sub.Enum(trTrajectoryType, in.TrajectoryRef.TrajectoryType)
		sub.Varint(trField4, uint64(in.TrajectoryRef.Field4))
		w.RawBytes(reqTrajectoryRef, sub.Bytes())
	}
	w.String(reqCascadeID, in.CascadeID)
	w.Enum(reqPlannerMode, in.PlannerMode)
	w.String(reqChatModelUID, in.ChatModelUID)
	w.String(reqExecutionID, in.ExecutionID)
	return w.Bytes()
}

// ----------------------------------------------------------------- describing

// DescribeRequest renders a request for human inspection.
func DescribeRequest(in *GetChatMessageRequest) string {
	var sb strings.Builder
	sb.WriteString("GetChatMessageRequest\n")
	if m := in.Metadata; m != nil {
		fmt.Fprintf(&sb, "  metadata            : ide_name=%q ext_ver=%q api_key=%s locale=%q os=%q ide_ver=%q ext_name=%q\n",
			m.IDEName, m.ExtensionVersion, redactSecret(m.APIKey), m.Locale, m.OS, m.IDEVersion, m.ExtensionName)
		fmt.Fprintf(&sb, "                        field28=%q field30=%v field31=%s\n",
			m.Field28, m.Field30, redactHex(m.Field31))
	} else {
		sb.WriteString("  metadata            : <absent>\n")
	}
	fmt.Fprintf(&sb, "  prompt              : %d bytes\n", len(in.Prompt))
	if in.Prompt != "" {
		sb.WriteString(indentBlock(truncate(in.Prompt, 1200), "      "))
	}
	fmt.Fprintf(&sb, "  chat_message_prompts: %d entries\n", len(in.ChatMessagePrompts))
	for i, c := range in.ChatMessagePrompts {
		fmt.Fprintf(&sb, "    [%d] source=%s message_id=%q prompt=%d bytes tool_calls=%d tool_call_id=%q images=%d thinking=%d bytes redacted=%v\n",
			i, SourceName(c.Source), c.MessageID, len(c.Prompt), len(c.ToolCalls), c.ToolCallID, len(c.Images), len(c.Thinking), c.ThinkingRedacted)
		if c.Prompt != "" {
			sb.WriteString(indentBlock(truncate(c.Prompt, 400), "        "))
		}
	}
	fmt.Fprintf(&sb, "  request_type        : %d\n", in.RequestType)
	if c := in.Configuration; c != nil {
		fmt.Fprintf(&sb, "  configuration       : max_tokens=%d temperature=%v top_p=%v top_k=%d num_completions=%d max_newlines=%d\n",
			c.MaxTokens, c.Temperature, c.TopP, c.TopK, c.NumCompletions, c.MaxNewlines)
	} else {
		sb.WriteString("  configuration       : <absent>\n")
	}
	fmt.Fprintf(&sb, "  tools               : %d entries\n", len(in.Tools))
	for i, t := range in.Tools {
		if i < 40 {
			fmt.Fprintf(&sb, "    [%d] %q (%d byte schema)\n", i, t.Name, len(t.JSONSchemaString))
		}
	}
	if len(in.Tools) > 40 {
		fmt.Fprintf(&sb, "    ... and %d more\n", len(in.Tools)-40)
	}
	fmt.Fprintf(&sb, "  cascade_id          : %q\n", in.CascadeID)
	if tr := in.TrajectoryRef; tr != nil {
		fmt.Fprintf(&sb, "  trajectory_reference: id=%q type=%d field4=%d\n", tr.TrajectoryID, tr.TrajectoryType, tr.Field4)
	} else {
		sb.WriteString("  trajectory_reference: <absent>\n")
	}
	fmt.Fprintf(&sb, "  planner_mode        : %d\n", in.PlannerMode)
	fmt.Fprintf(&sb, "  chat_model_uid      : %q\n", in.ChatModelUID)
	fmt.Fprintf(&sb, "  execution_id        : %q\n", in.ExecutionID)
	writeUnknown(&sb, in.Unknown)
	return sb.String()
}

// DescribeResponse renders one response chunk for human inspection.
func DescribeResponse(in *GetChatMessageResponse) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "message_id=%q request_id=%q", in.MessageID, in.RequestID)
	if in.TimestampSecs != 0 {
		fmt.Fprintf(&sb, " ts=%d.%09d", in.TimestampSecs, in.TimestampNanos)
	}
	if in.StopReasonSet {
		fmt.Fprintf(&sb, " stop=%s", StopReasonName(in.StopReason))
	}
	if in.Latency != 0 {
		fmt.Fprintf(&sb, " latency=%.3f", in.Latency)
	}
	if in.DeltaTokens != 0 {
		fmt.Fprintf(&sb, " delta_tokens=%d", in.DeltaTokens)
	}
	if in.ActualModelUID != "" {
		fmt.Fprintf(&sb, " model=%q", in.ActualModelUID)
	}
	for _, tc := range in.DeltaToolCalls {
		fmt.Fprintf(&sb, " tool_call{id=%q name=%q args=%q}", tc.ID, tc.Name, tc.ArgumentsJSON)
	}
	if in.DeltaThinking != "" {
		fmt.Fprintf(&sb, " thinking=%q", in.DeltaThinking)
	}
	if in.DeltaSignature != "" {
		fmt.Fprintf(&sb, " signature=%d bytes", len(in.DeltaSignature))
	}
	if in.ThinkingRedact {
		sb.WriteString(" thinking_redacted")
	}
	if in.Redact {
		sb.WriteString(" redact")
	}
	if in.DeltaText != "" {
		fmt.Fprintf(&sb, " text=%q", in.DeltaText)
	}
	if u := in.Usage; u != nil {
		fmt.Fprintf(&sb, " usage{in=%d out=%d cache_w=%d cache_r=%d model=%q}",
			u.InputTokens, u.OutputTokens, u.CacheWriteTokens, u.CacheReadTokens, u.ModelUID)
	}
	writeUnknown(&sb, in.Unknown)
	return sb.String()
}

func writeUnknown(sb *strings.Builder, unknown []UnknownField) {
	if len(unknown) == 0 {
		return
	}
	sb.WriteString("\n  unknown fields:")
	for _, u := range unknown {
		fmt.Fprintf(sb, "\n    %d (wire %d): %s", u.Number, u.Wire, u.Value)
	}
}

func redactSecret(s string) string {
	if s == "" {
		return `""`
	}
	if len(s) <= 24 {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q...(len %d)", s[:24], len(s))
}

func redactHex(s string) string {
	if s == "" {
		return `""`
	}
	if len(s) <= 24 {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q...(len %d)", s[:24], len(s))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n... [truncated, %d bytes total]", len(s))
}

func indentBlock(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}
