package devin

import (
	"bytes"
	"testing"

	"devin2proxy/internal/pb"
)

// TestDefaultMetadataMatchesTheChatCapture pins the chat path's metadata to a live
// capture of CLI 3000.11.3 GetChatMessage (2026-10-13): identity "devin-cli", the
// running version in both version fields, extension name "chisel", and field 28
// also "chisel". Field 31 (the opaque per-request blob) is deliberately absent.
func TestDefaultMetadataMatchesTheChatCapture(t *testing.T) {
	md := DefaultMetadata("devin-session-token$xyz")
	if md.IDEName != "devin-cli" {
		t.Errorf("ide_name = %q, want devin-cli", md.IDEName)
	}
	if md.ExtensionVersion != "3000.11.3" || md.IDEVersion != "3000.11.3" {
		t.Errorf("versions = %q/%q, want 3000.11.3/3000.11.3", md.ExtensionVersion, md.IDEVersion)
	}
	if md.ExtensionName != "chisel" {
		t.Errorf("extension_name = %q, want chisel", md.ExtensionName)
	}
	if md.Field28 != "chisel" {
		t.Errorf("field28 = %q, want chisel", md.Field28)
	}
	if md.Field31 != "" {
		t.Errorf("field31 = %q, want empty (omitted, backend accepts it)", md.Field31)
	}
}

// TestUnaryMetadataIsTheCLIUnaryShape pins the unary identity separate from the
// chat one: ide_name "chisel" (not "devin-cli") and no field 28, matching the
// CLI's GetUserStatus/GetCliTeamSettings/GetCliModelConfigs captures.
func TestUnaryMetadataIsTheCLIUnaryShape(t *testing.T) {
	md := UnaryMetadata("devin-session-token$xyz")
	if md.IDEName != "chisel" {
		t.Errorf("ide_name = %q, want chisel", md.IDEName)
	}
	if md.ExtensionName != "chisel" {
		t.Errorf("extension_name = %q, want chisel", md.ExtensionName)
	}
	if md.ExtensionVersion != "3000.11.3" || md.IDEVersion != "3000.11.3" {
		t.Errorf("versions = %q/%q, want 3000.11.3/3000.11.3", md.ExtensionVersion, md.IDEVersion)
	}
	if md.Field28 != "" {
		t.Errorf("field28 = %q, want empty: the CLI does not send field 28 on unary calls", md.Field28)
	}
}

// TestMetadataField28RoundTrips pins that the chat metadata's field 28 is encoded
// at field 28 and read back, so the wire carries it and a decoder can see it.
func TestMetadataField28RoundTrips(t *testing.T) {
	md := DefaultMetadata("tok")
	raw := EncodeMetadata(md)
	var have28 bool
	r := pb.NewReader(raw)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		if f == mdField28 && w == pb.WireBytes {
			have28 = true
			if got := r.String(); got != "chisel" {
				t.Errorf("metadata field 28 = %q, want chisel", got)
			}
			continue
		}
		r.Skip(w)
	}
	if !have28 {
		t.Fatal("the encoded chat metadata never carried field 28")
	}

	back := decodeMetadata(raw)
	if back.Field28 != "chisel" {
		t.Errorf("decoded field28 = %q, want chisel", back.Field28)
	}
	if back.IDEName != "devin-cli" {
		t.Errorf("decoded ide_name = %q, want devin-cli", back.IDEName)
	}
}

// TestRequestField28RoundTrips pins the top-level request codename: it encodes at
// field 28 and decodes back, so the proxy's generated codename reaches the wire.
func TestRequestField28RoundTrips(t *testing.T) {
	req := &GetChatMessageRequest{
		Metadata:     &Metadata{IDEName: "devin-cli"},
		Prompt:       "p",
		ChatModelUID: "swe-1-6-slow",
		CascadeID:    "11111111-2222-4333-8444-555555555555",
		Field28:      "warm-soul",
	}
	raw := EncodeGetChatMessageRequest(req)

	back := DecodeGetChatMessageRequest(raw)
	if back.Field28 != "warm-soul" {
		t.Errorf("request field28 = %q, want warm-soul", back.Field28)
	}
	if back.CascadeID != req.CascadeID {
		t.Errorf("cascade_id = %q, want %q", back.CascadeID, req.CascadeID)
	}
	// Field 28 must be encoded at the top level, not only in the metadata block.
	var topLevel bool
	r := pb.NewReader(raw)
	for {
		f, w, ok := r.Field()
		if !ok {
			break
		}
		if f == reqField28 && w == pb.WireBytes {
			topLevel = true
			if got := r.String(); got != "warm-soul" {
				t.Errorf("top-level field 28 = %q", got)
			}
			continue
		}
		r.Skip(w)
	}
	if !topLevel {
		t.Fatal("the request never carried a top-level field 28")
	}
}

// TestSessionCodenameShape pins that a generated codename is an adjective-noun
// pair, never a fixed literal the capture observed. The exact value is random, so
// only the shape and the non-identity with the observed literals are asserted.
func TestSessionCodenameShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := SessionCodename()
		parts := bytes.Split([]byte(c), []byte("-"))
		if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) == 0 {
			t.Fatalf("codename %q is not adjective-noun", c)
		}
		if c == "jagged-felidae" || c == "amusing-icon" || c == "warm-soul" || c == "accurate-cornucopia" {
			t.Fatalf("codename %q is a literal observed in a capture; it must be generated", c)
		}
		seen[c] = true
	}
	// Not a hard requirement, but 200 draws from a 24x24 space repeating entirely
	// would mean the generator is stuck.
	if len(seen) < 5 {
		t.Errorf("only %d distinct codenames in 200 draws; the generator may be stuck", len(seen))
	}
}

// TestTrajectoryReferenceRoundShapes pins the field-4 difference between the
// opening round (14 present) and a continuation (field 4 omitted), and that the
// wire math matches: the continuation's encoding must be exactly 1 byte shorter
// (the field 4 entry is a 2-byte tag+varint), and decoding each round-trips.
func TestTrajectoryReferenceRoundShapes(t *testing.T) {
	first := NewTrajectoryReference()
	if first.Field4 != 14 || !first.field4Set {
		t.Fatalf("opening reference = %+v, want field4=14 set", first)
	}
	cont := NewTrajectoryReferenceContinuation()
	if cont.Field4 != 0 || cont.field4Set {
		t.Fatalf("continuation reference = %+v, want field4 unset", cont)
	}

	enc := func(ref *TrajectoryReference) []byte {
		w := pb.NewWriter()
		w.String(trTrajectoryID, ref.TrajectoryID)
		w.Enum(trTrajectoryType, ref.TrajectoryType)
		if ref.field4Set || ref.Field4 != 0 {
			w.Varint(trField4, uint64(ref.Field4))
		}
		return w.Bytes()
	}
	firstRaw := enc(first)
	contRaw := enc(cont)
	if len(contRaw) != len(firstRaw)-2 {
		t.Errorf("continuation encoding is %d bytes, opening is %d; want the continuation 2 shorter",
			len(contRaw), len(firstRaw))
	}

	if back := decodeTrajectoryReference(firstRaw); back.Field4 != 14 || !back.field4Set {
		t.Errorf("decoded opening reference = %+v, want field4=14 set", back)
	}
	if back := decodeTrajectoryReference(contRaw); back.Field4 != 0 || back.field4Set {
		t.Errorf("decoded continuation reference = %+v, want field4 unset", back)
	}
}
