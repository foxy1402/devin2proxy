package devin

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// Connect stream flags. A stream is a sequence of FrameData envelopes followed
// by exactly one FrameEndStream envelope.
const (
	// FrameData carries a serialised message.
	FrameData byte = 0x00
	// FrameEndStream terminates the stream. Its payload is JSON, and is `{}`
	// when the stream completed without error.
	FrameEndStream byte = 0x02
)

// Frame is one decoded Connect envelope.
type Frame struct {
	Flag byte
	Data []byte
}

// ParseFrames splits a Connect body into envelopes, each laid out as
// [flags:1][length:4 big-endian][payload].
func ParseFrames(b []byte) ([]Frame, error) {
	var out []Frame
	for len(b) > 0 {
		if len(b) < 5 {
			return out, fmt.Errorf("connect: truncated envelope header (%d bytes left)", len(b))
		}
		flag := b[0]
		n := int(binary.BigEndian.Uint32(b[1:5]))
		if n < 0 || 5+n > len(b) {
			return out, fmt.Errorf("connect: envelope claims %d bytes but only %d remain", n, len(b)-5)
		}
		out = append(out, Frame{Flag: flag, Data: b[5 : 5+n]})
		b = b[5+n:]
	}
	return out, nil
}

// EncodeFrame wraps payload in a Connect envelope.
func EncodeFrame(flag byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// ConnectError is the error object carried by an end-of-stream frame.
type ConnectError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ConnectError) Error() string {
	if e == nil {
		return "connect: unknown error"
	}
	return fmt.Sprintf("connect error %s: %s", e.Code, e.Message)
}

// EndStream is the decoded body of an end-of-stream frame.
type EndStream struct {
	Error    *ConnectError  `json:"error"`
	Metadata map[string]any `json:"metadata"`
}

// ParseEndStream decodes an end-of-stream payload. A stream that finished
// cleanly ends with `{}`, so a nil Error means success.
func ParseEndStream(payload []byte) (*EndStream, error) {
	if len(payload) == 0 {
		return &EndStream{}, nil
	}
	var es EndStream
	if err := json.Unmarshal(payload, &es); err != nil {
		return nil, fmt.Errorf("connect: decode end-of-stream body: %w", err)
	}
	return &es, nil
}
