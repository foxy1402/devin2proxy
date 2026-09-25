package devin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"devin2proxy/internal/pb"
)

// The schema was recovered from a binary, so the decoders meet input the schema
// never anticipated: packs that run out mid-varint, frames that announce a length
// and then stop short. Every such case used to degrade into an invented zero or a
// half-parsed message handed back as complete. These tests pin the two fixes: a
// packed field commits only what parsed, and a truncated frame is reported
// instead of streamed onward.

func TestAPackedField30NeverInventsAnEntry(t *testing.T) {
	packed := func(payload ...byte) []byte {
		w := pb.NewWriter()
		w.RawBytes(mdField30, payload)
		return w.Bytes()
	}

	// An empty pack carries no values; it used to append the 0 that a failed
	// Varint() returns.
	if got := decodeMetadata(packed()); len(got.Field30) != 0 {
		t.Fatalf("empty pack decoded to %v, want no entries", got.Field30)
	}
	// A pack that starts a varint and runs out must not decode to anything.
	if got := decodeMetadata(packed(0x80)); len(got.Field30) != 0 {
		t.Fatalf("truncated pack decoded to %v, want no entries", got.Field30)
	}
	// A pack that parses some values before running out keeps exactly those.
	if got := decodeMetadata(packed(0x07, 0x80)); len(got.Field30) != 1 || got.Field30[0] != 7 {
		t.Fatalf("partial pack decoded to %v, want [7]", got.Field30)
	}
	// A well-formed pack still round-trips.
	if got := decodeMetadata(packed(0x07, 0x09)); len(got.Field30) != 2 || got.Field30[0] != 7 || got.Field30[1] != 9 {
		t.Fatalf("clean pack decoded to %v, want [7 9]", got.Field30)
	}
}

func TestATruncatedResponseFrameIsFlagged(t *testing.T) {
	w := pb.NewWriter()
	w.String(respDeltaText, "a sentence that will be cut short")
	w.Varint(respStopReason, StopNormal)
	full := w.Bytes()

	if resp := DecodeGetChatMessageResponse(full); resp.truncated {
		t.Fatal("a complete message was flagged as truncated")
	} else if resp.DeltaText == "" || !resp.StopReasonSet {
		t.Fatalf("complete message decoded to %+v", resp)
	}

	// Cut the payload inside the text field: the tag and length still promise
	// more bytes than exist.
	cut := full[:len(full)-10]
	resp := DecodeGetChatMessageResponse(cut)
	if !resp.truncated {
		t.Fatal("a message that ran out mid-field was not flagged as truncated")
	}
	if resp.DeltaText != "" {
		t.Fatalf("the half that parsed was kept as a complete delta: %q", resp.DeltaText)
	}
}

func TestATruncatedRequestFrameIsFlagged(t *testing.T) {
	w := pb.NewWriter()
	w.RawBytes(reqChatMessagePrompts, encodeChatMessagePrompt(ChatMessagePrompt{Source: SourceUser, Prompt: "turn one"}))
	w.RawBytes(reqChatMessagePrompts, encodeChatMessagePrompt(ChatMessagePrompt{Source: SourceUser, Prompt: "turn two that gets cut off"}))
	full := w.Bytes()

	if req := DecodeGetChatMessageRequest(full); req.truncated {
		t.Fatal("a complete message was flagged as truncated")
	} else if len(req.ChatMessagePrompts) != 2 {
		t.Fatalf("complete message decoded to %d prompts", len(req.ChatMessagePrompts))
	}

	// Cut the payload inside the second prompt: the length prefix still
	// promises bytes that do not exist. Appending the empty prompt the failed
	// read returns would invent a turn the client never sent.
	cut := full[:len(full)-10]
	req := DecodeGetChatMessageRequest(cut)
	if !req.truncated {
		t.Fatal("a message that ran out mid-field was not flagged as truncated")
	}
	if len(req.ChatMessagePrompts) != 1 {
		t.Fatalf("the cut frame kept %d prompts, want only the one that parsed", len(req.ChatMessagePrompts))
	}
	// The description has to say so too: a half frame must not pass for a
	// whole one when a capture is being inspected.
	if desc := DescribeRequest(req); !strings.Contains(desc, "truncated") {
		t.Errorf("DescribeRequest hid the truncation:\n%s", desc)
	}
}

func TestAnUnknownFieldInsideATrajectoryReferenceIsDescribed(t *testing.T) {
	w := pb.NewWriter()
	w.Message(reqTrajectoryRef, func(ref *pb.Writer) {
		ref.String(trTrajectoryID, "8f0c9cba-6fde-4d1f-a4e9-1f2b3c4d5e6f")
		ref.Varint(9, 77) // a field number we have no name for
	})
	req := DecodeGetChatMessageRequest(w.Bytes())
	if req.TrajectoryRef == nil || len(req.TrajectoryRef.Unknown) != 1 {
		t.Fatalf("the unknown field inside the reference was not recorded: %+v", req.TrajectoryRef)
	}
	desc := DescribeRequest(req)
	if !strings.Contains(desc, "unknown fields") || !strings.Contains(desc, "9 (wire 0)") {
		t.Errorf("DescribeRequest dropped the reference's unknown fields:\n%s", desc)
	}
}

func TestTrajectoryField4IsEmittedOnlyWhenPresent(t *testing.T) {
	// The captured shape round-trips byte for byte.
	w := pb.NewWriter()
	w.Message(reqTrajectoryRef, func(ref *pb.Writer) {
		ref.String(trTrajectoryID, "8f0c9cba-6fde-4d1f-a4e9-1f2b3c4d5e6f")
		ref.Enum(trTrajectoryType, 4)
		ref.Varint(trField4, 14)
	})
	req := DecodeGetChatMessageRequest(w.Bytes())
	if req.TrajectoryRef == nil || !req.TrajectoryRef.field4Set {
		t.Fatal("a decoded field 4 did not set its presence flag")
	}
	if back := EncodeGetChatMessageRequest(req); !bytes.Equal(back, w.Bytes()) {
		t.Errorf("round trip changed the reference: %x != %x", back, w.Bytes())
	}

	// A reference whose wire form lacked field 4 must not grow one on the way
	// back out: re-encoding absence as an explicit zero changes what the
	// backend sees.
	w = pb.NewWriter()
	w.Message(reqTrajectoryRef, func(ref *pb.Writer) {
		ref.String(trTrajectoryID, "8f0c9cba-6fde-4d1f-a4e9-1f2b3c4d5e6f")
		ref.Enum(trTrajectoryType, 4)
	})
	req = DecodeGetChatMessageRequest(w.Bytes())
	if req.TrajectoryRef == nil || req.TrajectoryRef.field4Set {
		t.Fatal("field 4 was marked present without being on the wire")
	}
	if back := EncodeGetChatMessageRequest(req); !bytes.Equal(back, w.Bytes()) {
		t.Errorf("round trip invented field 4: %x != %x", back, w.Bytes())
	}

	// A reference built in code always carries the captured value.
	req = &GetChatMessageRequest{TrajectoryRef: NewTrajectoryReference()}
	raw := EncodeGetChatMessageRequest(req)
	if !bytes.Contains(raw, []byte{trField4 << 3, 14}) {
		t.Errorf("a built reference lost field 4: %x", raw)
	}
}

func TestAStreamReportsATruncatedFrameInsteadOfHalfADelta(t *testing.T) {
	w := pb.NewWriter()
	w.String(respDeltaText, "this answer is about to be cut in half")
	truncatedFrame := EncodeFrame(FrameData, w.Bytes()[:len(w.Bytes())-8])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(truncatedFrame)
		w.Write(endStream("", ""))
	}))
	t.Cleanup(srv.Close)

	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 5 * time.Second})
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := client.GetChatMessage(ctx, creds, &GetChatMessageRequest{})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	resp, err := stream.Recv()
	stream.Close()
	if err == nil {
		t.Fatalf("Recv returned a truncated frame as complete: %+v", resp)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v, want it to name the truncation", err)
	}
	// Peek must report the same thing, since the pool's refusal check reads the
	// first frame through it. The client allows one in-flight stream, so the
	// first must be closed before this one can open.
	stream2, err := client.GetChatMessage(ctx, creds, &GetChatMessageRequest{})
	if err != nil {
		t.Fatalf("second stream: %v", err)
	}
	defer stream2.Close()
	if err := stream2.Peek(); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("Peek = %v, want the same truncation error", err)
	}
}
