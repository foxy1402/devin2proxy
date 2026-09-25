// Package pb implements the subset of the protobuf wire format needed to talk
// to the Devin/Codeium Connect-RPC API, with no external dependencies.
//
// Unknown fields are preserved rather than dropped, and Dump renders arbitrary
// wire-format data without a schema. That matters here because the schema was
// reverse-engineered: if a field number is wrong the failure shows up as a
// readable log line instead of silently corrupt output.
package pb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Wire types, per the protobuf encoding spec.
const (
	WireVarint  = 0
	WireFixed64 = 1
	WireBytes   = 2
	WireFixed32 = 5
)

// ------------------------------------------------------------------ encoding

type Writer struct{ buf []byte }

func NewWriter() *Writer { return &Writer{} }

func (w *Writer) Bytes() []byte { return w.buf }

func (w *Writer) varint(v uint64) {
	for v >= 0x80 {
		w.buf = append(w.buf, byte(v)|0x80)
		v >>= 7
	}
	w.buf = append(w.buf, byte(v))
}

func (w *Writer) tag(field, wire int) { w.varint(uint64(field)<<3 | uint64(wire)) }

// Varint writes an explicit varint field, including zero values.
func (w *Writer) Varint(field int, v uint64) {
	w.tag(field, WireVarint)
	w.varint(v)
}

// Uint64 writes a varint field, omitting zero to match proto3 default semantics.
func (w *Writer) Uint64(field int, v uint64) {
	if v == 0 {
		return
	}
	w.Varint(field, v)
}

func (w *Writer) Int64(field int, v int64) {
	if v == 0 {
		return
	}
	w.Varint(field, uint64(v))
}

func (w *Writer) Enum(field int, v int) {
	if v == 0 {
		return
	}
	w.Varint(field, uint64(v))
}

func (w *Writer) Bool(field int, v bool) {
	if !v {
		return
	}
	w.Varint(field, 1)
}

func (w *Writer) Double(field int, v float64) {
	if v == 0 {
		return
	}
	w.tag(field, WireFixed64)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	w.buf = append(w.buf, b[:]...)
}

// Float32 writes a 4-byte little-endian float. The model catalogue carries its
// prices and its rating this way, so a caller that re-encodes a catalogue — a test
// building a fixture, say — needs it; the reader has had the matching Fixed32 all
// along.
func (w *Writer) Float32(field int, v float32) {
	w.tag(field, WireFixed32)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
	w.buf = append(w.buf, b[:]...)
}

// String writes a length-delimited field, omitting empty strings.
func (w *Writer) String(field int, s string) {
	if s == "" {
		return
	}
	w.RawBytes(field, []byte(s))
}

// RawBytes always writes the field, even when empty. Use it for sub-messages,
// where an empty message is still meaningfully present.
func (w *Writer) RawBytes(field int, b []byte) {
	w.tag(field, WireBytes)
	w.varint(uint64(len(b)))
	w.buf = append(w.buf, b...)
}

// Message writes a nested message built by fn.
func (w *Writer) Message(field int, fn func(*Writer)) {
	sub := NewWriter()
	fn(sub)
	w.RawBytes(field, sub.Bytes())
}

// ------------------------------------------------------------------ decoding

type Reader struct {
	buf []byte
	i   int
	err error
}

func NewReader(b []byte) *Reader { return &Reader{buf: b} }

func (r *Reader) Err() error { return r.err }

// Pos returns the current read offset, which lets a caller record where each
// field started and ended.
func (r *Reader) Pos() int { return r.i }

// Remaining reports how many unread bytes are left, after accounting for any
// error. It lets a caller detect a decode that made no progress.
func (r *Reader) Remaining() int {
	if r.err != nil || r.i > len(r.buf) {
		return 0
	}
	return len(r.buf) - r.i
}

func (r *Reader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

// varint reads a base-128 varint. Ten bytes is the spec's ceiling, and the
// tenth carries only bit 63: a final byte of 0x02 would shift its high bit out
// of the uint64 and wrap into a wrong-but-clean value, so anything above 0x01
// there is rejected as malformed rather than decoded to garbage.
func (r *Reader) varint() uint64 {
	var v uint64
	var shift uint
	for {
		if r.i >= len(r.buf) {
			r.fail(errors.New("pb: truncated varint"))
			return 0
		}
		b := r.buf[r.i]
		r.i++
		if shift == 63 && b&0xfe != 0 {
			r.fail(errors.New("pb: varint overflows 64 bits"))
			return 0
		}
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v
		}
		shift += 7
		if shift >= 64 {
			r.fail(errors.New("pb: varint overflows 64 bits"))
			return 0
		}
	}
}

// Field returns the next field number and wire type. ok is false at end of
// input or after an error.
func (r *Reader) Field() (field, wire int, ok bool) {
	if r.err != nil || r.i >= len(r.buf) {
		return 0, 0, false
	}
	tag := r.varint()
	if r.err != nil {
		return 0, 0, false
	}
	if tag>>3 == 0 {
		r.fail(errors.New("pb: field number 0 is invalid"))
		return 0, 0, false
	}
	return int(tag >> 3), int(tag & 7), true
}

func (r *Reader) Varint() uint64 { return r.varint() }

func (r *Reader) Uint64() uint64 { return r.varint() }

func (r *Reader) Bool() bool { return r.varint() != 0 }

func (r *Reader) Double() float64 {
	if r.i+8 > len(r.buf) {
		r.fail(errors.New("pb: truncated fixed64"))
		return 0
	}
	v := math.Float64frombits(binary.LittleEndian.Uint64(r.buf[r.i:]))
	r.i += 8
	return v
}

// Fixed32 reads a fixed32 as its raw bits. Prices in the model catalogue are
// fixed32, so reading them as a float would lose the distinction between a real
// value and the bit pattern of a small integer.
func (r *Reader) Fixed32() uint32 {
	if r.i+4 > len(r.buf) {
		r.fail(errors.New("pb: truncated fixed32"))
		return 0
	}
	v := binary.LittleEndian.Uint32(r.buf[r.i:])
	r.i += 4
	return v
}

func (r *Reader) Bytes() []byte {
	n := r.varint()
	if r.err != nil {
		return nil
	}
	// Compare in uint64 before converting to int: on a 32-bit build a huge
	// length would truncate into a small valid-looking one and hand back a
	// slice the sender never sent.
	if n > uint64(len(r.buf)-r.i) {
		r.fail(fmt.Errorf("pb: truncated length-delimited field (want %d, have %d)", n, len(r.buf)-r.i))
		return nil
	}
	b := r.buf[r.i : r.i+int(n)]
	r.i += int(n)
	return b
}

func (r *Reader) String() string { return string(r.Bytes()) }

func (r *Reader) Skip(wire int) {
	switch wire {
	case WireVarint:
		r.varint()
	case WireFixed64:
		r.i += 8
	case WireBytes:
		n := r.varint()
		if r.err != nil {
			return
		}
		// Compare in uint64 before converting to int: a length of 2^63
		// becomes a negative int, and a negative cursor slips past the
		// bounds check below, so the next Field() would index before the
		// start of the buffer and panic.
		if n > uint64(len(r.buf)-r.i) {
			r.fail(errors.New("pb: field runs past end of message"))
			return
		}
		r.i += int(n)
	case WireFixed32:
		r.i += 4
	default:
		r.fail(fmt.Errorf("pb: unsupported wire type %d", wire))
		return
	}
	if r.err == nil && r.i > len(r.buf) {
		r.fail(errors.New("pb: field runs past end of message"))
	}
}

// ------------------------------------------------------------------ dumping

// maxDumpDepth bounds recursion so malformed input cannot blow the stack.
const maxDumpDepth = 12

// Dump renders wire-format data as indented text without any schema. Prints
// that look like text are shown as strings; everything else is recursed into as
// a nested message, which is how sub-messages are discovered.
func Dump(b []byte) string {
	var sb strings.Builder
	dump(&sb, b, 0)
	return sb.String()
}

func dump(sb *strings.Builder, b []byte, depth int) {
	indent := strings.Repeat("  ", depth)
	r := NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			break
		}
		switch wire {
		case WireVarint:
			fmt.Fprintf(sb, "%s%d: varint %d\n", indent, field, r.Varint())
		case WireFixed64:
			fmt.Fprintf(sb, "%s%d: fixed64 %v\n", indent, field, r.Double())
		case WireFixed32:
			bits := r.Fixed32()
			fmt.Fprintf(sb, "%s%d: fixed32 %v (bits 0x%08x)\n", indent, field, math.Float32frombits(bits), bits)
		case WireBytes:
			raw := r.Bytes()
			if isText(raw) {
				fmt.Fprintf(sb, "%s%d: string %s\n", indent, field, quoteTruncated(raw))
			} else if depth < maxDumpDepth {
				fmt.Fprintf(sb, "%s%d: message (%d bytes)\n", indent, field, len(raw))
				dump(sb, raw, depth+1)
			} else {
				fmt.Fprintf(sb, "%s%d: bytes[%d] %x\n", indent, field, len(raw), raw)
			}
		}
	}
	if err := r.Err(); err != nil {
		fmt.Fprintf(sb, "%s! %v\n", indent, err)
	}
}

// dumpStringLimit caps how much of a long string field the dump prints, so a
// multi-kilobyte prompt does not bury the rest of the structure.
const dumpStringLimit = 1500

func quoteTruncated(b []byte) string {
	if len(b) <= dumpStringLimit {
		return fmt.Sprintf("%q", string(b))
	}
	return fmt.Sprintf("%q... [truncated, %d bytes total]", string(b[:dumpStringLimit]), len(b))
}

// isText reports whether b looks like human-readable text, which is how the
// schema-less dump decides between printing a field as a string and recursing
// into it as a nested message. The threshold is deliberately strict: a nested
// message contains tag bytes that are almost always non-printable, so requiring
// ~95% printable runes keeps messages from being mistaken for text.
func isText(b []byte) bool {
	if len(b) == 0 || !utf8.Valid(b) {
		return false
	}
	var printable, total int
	for _, r := range string(b) {
		total++
		if unicode.IsPrint(r) || r == '\n' || r == '\t' || r == '\r' {
			printable++
		}
	}
	if total == 0 {
		return false
	}
	return float64(printable)/float64(total) >= 0.95
}
