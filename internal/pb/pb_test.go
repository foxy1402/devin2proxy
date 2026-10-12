package pb

import (
	"strings"
	"testing"
)

// The schema was reverse-engineered, so this reader meets bytes its callers
// never chose: lengths that announce far more than the buffer holds, varints
// that use their tenth byte for bits a uint64 cannot carry. Every such input
// used to degrade into a wrong-but-clean value or an out-of-bounds panic; these
// tests pin the rule that malformed input is an error, never a crash.

func TestAVarintTenthByteAboveBit63IsMalformed(t *testing.T) {
	// Ten bytes is the spec's ceiling and the tenth carries only bit 63. A
	// final byte of 0x02 shifts its high bit out of the uint64 and wraps to 0,
	// which used to decode with err == nil.
	r := NewReader([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02})
	v := r.Varint()
	if r.Err() == nil {
		t.Fatalf("a tenth byte of 0x02 decoded to %d without an error", v)
	}
}

func TestAVarintTenthByteMayCarryBit63(t *testing.T) {
	// 0x01 in the tenth position is the legitimate encoding of 2^63 and must
	// still decode.
	r := NewReader([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01})
	if v := r.Varint(); v != 1<<63 || r.Err() != nil {
		t.Fatalf("varint = %d err = %v, want 2^63 and no error", v, r.Err())
	}
}

// hugeLength is the varint encoding of 2^63.
var hugeLength = []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}

func TestSkipSurvivesAHugeLengthPrefix(t *testing.T) {
	// A length of 2^63 used to be converted to int inside Skip before any
	// bounds check: negative on a 64-bit build, the cursor check passed, and
	// the next Field() indexed before the start of the buffer.
	r := NewReader(append([]byte{0x0a /* field 1, wire 2 */}, hugeLength...))
	field, wire, ok := r.Field()
	if !ok || field != 1 || wire != WireBytes {
		t.Fatalf("field = %d wire = %d ok = %v", field, wire, ok)
	}
	r.Skip(wire)
	if r.Err() == nil {
		t.Fatal("a length prefix of 2^63 was skipped without an error")
	}
	// The reader must stop cleanly rather than carry a negative cursor into
	// the next field.
	if _, _, ok := r.Field(); ok {
		t.Fatal("the reader offered another field after the overrun")
	}
}

func TestSkipRejectsALengthThatRunsPastTheEnd(t *testing.T) {
	// Not just the 2^63 case: any length prefix promising more bytes than the
	// buffer holds is the same failure, and the message names it.
	r := NewReader([]byte{0x0a, 0x05, 'a', 'b'})
	r.Skip(WireBytes)
	if err := r.Err(); err == nil || !strings.Contains(err.Error(), "runs past end of message") {
		t.Fatalf("err = %v, want the field-runs-past-end error", err)
	}
}

func TestBytesRejectsAGiantLength(t *testing.T) {
	// The comparison is in uint64 precisely so a length that would truncate
	// into a small positive int on a 32-bit build cannot slip through.
	for _, length := range [][]byte{hugeLength, {0xff, 0xff, 0xff, 0xff, 0x07}} {
		r := NewReader(append([]byte{0x0a}, length...))
		if _, _, ok := r.Field(); !ok {
			t.Fatalf("the field header itself did not parse: %v", r.Err())
		}
		if b := r.Bytes(); b != nil || r.Err() == nil {
			t.Fatalf("Bytes on a length of %x returned %x err %v", length, b, r.Err())
		}
	}
}

func TestSkipStillSkipsWellFormedFields(t *testing.T) {
	w := NewWriter()
	w.Varint(3, 7)
	w.Double(4, 2.5)
	w.Float32(6, 1.5)
	w.RawBytes(9, []byte("payload"))

	r := NewReader(w.Bytes())
	for r.Err() == nil {
		_, wire, ok := r.Field()
		if !ok {
			break
		}
		r.Skip(wire)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("skipping well-formed fields failed: %v", err)
	}
	if r.Pos() != len(w.Bytes()) {
		t.Fatalf("skipping stopped at %d of %d bytes", r.Pos(), len(w.Bytes()))
	}
}

// TestPackedVarintsRoundTrip pins the encoder against the decoder: a packed
// repeated field must come back as the values that went in, which is what lets
// Metadata's field 30 match the packed form the CLI sends.
func TestPackedVarintsRoundTrip(t *testing.T) {
	w := NewWriter()
	w.PackedVarints(30, []uint64{7, 9, 300, 0, 1})
	got := decodePacked(t, w.Bytes(), 30)
	want := []uint64{7, 9, 300, 0, 1}
	if len(got) != len(want) {
		t.Fatalf("decoded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("decoded %v, want %v", got, want)
		}
	}
}

// TestPackedVarintsEmptyWritesNothing keeps an empty repeated field off the
// wire entirely, matching proto3's omission of an empty repeated field.
func TestPackedVarintsEmptyWritesNothing(t *testing.T) {
	w := NewWriter()
	w.PackedVarints(30, nil)
	if len(w.Bytes()) != 0 {
		t.Fatalf("empty PackedVarints wrote % x, want nothing", w.Bytes())
	}
}

// decodePacked reads back a packed varint field by number.
func decodePacked(t *testing.T, b []byte, wantField int) []uint64 {
	t.Helper()
	var out []uint64
	r := NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			break
		}
		if field == wantField && wire == WireBytes {
			sub := NewReader(r.Bytes())
			for sub.Remaining() > 0 {
				v := sub.Varint()
				if sub.Err() != nil {
					break
				}
				out = append(out, v)
			}
			continue
		}
		r.Skip(wire)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}
