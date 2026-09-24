package devin

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"devin2proxy/internal/pb"
)

// This file holds byte-level surgery helpers used to work out which request
// fields the backend actually requires. They operate on raw protobuf so every
// field we have not modelled still round-trips unchanged.

// FilterFields returns a copy of a protobuf message with the given top-level
// field numbers removed. Every other field is preserved byte for byte, which is
// what makes it useful for bisecting requirements: dropping a field this way
// cannot disturb anything else.
func FilterFields(b []byte, drop map[int]bool) ([]byte, error) {
	if len(drop) == 0 {
		return b, nil
	}
	r := pb.NewReader(b)
	out := make([]byte, 0, len(b))
	for {
		start := r.Pos()
		field, wire, ok := r.Field()
		if !ok {
			break
		}
		r.Skip(wire)
		if err := r.Err(); err != nil {
			return nil, fmt.Errorf("filter fields: %w", err)
		}
		if drop[field] {
			continue
		}
		out = append(out, b[start:r.Pos()]...)
	}
	if err := r.Err(); err != nil {
		return nil, fmt.Errorf("filter fields: %w", err)
	}
	return out, nil
}

// FilterSubMessage rewrites the length-delimited field targetField by dropping
// the listed sub-field numbers from inside it, leaving every other top-level
// field untouched.
func FilterSubMessage(b []byte, targetField int, drop map[int]bool) ([]byte, error) {
	if len(drop) == 0 {
		return b, nil
	}
	r := pb.NewReader(b)
	out := make([]byte, 0, len(b))
	found := false
	for {
		start := r.Pos()
		field, wire, ok := r.Field()
		if !ok {
			break
		}
		if field == targetField && wire == pb.WireBytes {
			sub := r.Bytes()
			if err := r.Err(); err != nil {
				return nil, fmt.Errorf("filter sub-message: %w", err)
			}
			filtered, err := FilterFields(sub, drop)
			if err != nil {
				return nil, err
			}
			found = true
			// Re-emit the field with a corrected length prefix.
			var key [10]byte
			n := binary.PutUvarint(key[:], uint64(targetField)<<3|uint64(pb.WireBytes))
			out = append(out, key[:n]...)
			n = binary.PutUvarint(key[:], uint64(len(filtered)))
			out = append(out, key[:n]...)
			out = append(out, filtered...)
			continue
		}
		r.Skip(wire)
		if err := r.Err(); err != nil {
			return nil, fmt.Errorf("filter sub-message: %w", err)
		}
		out = append(out, b[start:r.Pos()]...)
	}
	if err := r.Err(); err != nil {
		return nil, fmt.Errorf("filter sub-message: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("field %d is not present as a sub-message", targetField)
	}
	return out, nil
}

// ReadCaptureBody loads the raw body from a devin-probe capture file, which
// stores it as a single `hex: <hex>` line.
func ReadCaptureBody(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Captured bodies run to tens of kilobytes, well past the 64 KiB default
	// line limit.
	sc.Buffer(make([]byte, 0, 1<<20), 1<<27)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "hex: ") {
			continue
		}
		h := strings.TrimSpace(strings.TrimPrefix(line, "hex: "))
		// The probe only truncates its stdout copy, but be tolerant anyway.
		if i := strings.Index(h, "..."); i >= 0 {
			h = h[:i]
		}
		h = strings.TrimSpace(h)
		if h == "" {
			return nil, fmt.Errorf("%s: empty hex field", path)
		}
		return hex.DecodeString(h)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%s: no `hex:` line found; is this a devin-probe capture?", path)
}

// RequestPayload extracts the single request message from a captured request
// body, unwrapping the Connect envelope when present.
func RequestPayload(body []byte) ([]byte, error) {
	frames, ferr := ParseFrames(body)
	if ferr == nil {
		for _, fr := range frames {
			if fr.Flag == FrameData {
				return fr.Data, nil
			}
		}
	}
	// Unary requests are a bare message with no envelope.
	if isPlausibleMessage(body) {
		return body, nil
	}
	return nil, fmt.Errorf("capture holds no request message (envelope error: %v)", ferr)
}

// isPlausibleMessage reports whether b parses as a protobuf message with at
// least one field, which distinguishes a bare unary body from junk.
func isPlausibleMessage(b []byte) bool {
	r := pb.NewReader(b)
	if _, _, ok := r.Field(); !ok {
		return false
	}
	return r.Err() == nil
}
