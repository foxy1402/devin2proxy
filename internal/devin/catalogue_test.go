package devin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"devin2proxy/internal/pb"
)

// The catalogue is what the dashboard's Models panel shows, so the decode has to be
// right about the two numbers a client actually depends on (the context window and
// the output ceiling) and honest about the one that is read but not named (the
// capability flags, kept as field numbers rather than interpreted).

func TestDecodeModelCatalogue(t *testing.T) {
	models := DecodeModelCatalogue(encodeTestCatalogue())
	if len(models) != 2 {
		t.Fatalf("decoded %d models, want 2: an entry with no uid is not a model", len(models))
	}

	slow := models[0]
	if slow.UID != "swe-1-6-slow" || slow.Name != "SWE-1.6 Slow" {
		t.Fatalf("first model = %+v", slow)
	}
	if slow.ContextWindow != 200000 || slow.MaxOutput != 128000 {
		t.Fatalf("limits = %d/%d, want 200000/128000", slow.ContextWindow, slow.MaxOutput)
	}
	if slow.Tokenizer != "LLAMA_WITH_SPECIAL" {
		t.Fatalf("tokenizer = %q", slow.Tokenizer)
	}
	if len(slow.Aliases) != 2 || slow.Aliases[0] != "swe-1p6" || slow.Aliases[1] != "swe-1p5" {
		t.Fatalf("aliases = %v", slow.Aliases)
	}
	if len(slow.Capabilities) != 3 || slow.Capabilities[0] != 8 || slow.Capabilities[2] != 12 {
		t.Fatalf("capabilities = %v, want the field numbers 8 11 12", slow.Capabilities)
	}
	if len(slow.Options) != 1 || slow.Options[0] != "Speed" {
		t.Fatalf("options = %v", slow.Options)
	}
	if len(slow.Prices) != 1 {
		t.Fatalf("prices = %+v", slow.Prices)
	}
	price := slow.Prices[0]
	if price.Label != "Input" || price.Unit != "1M tokens" || price.Amount != 0.5 {
		t.Fatalf("price = %+v", price)
	}
	if price.Note != "Higher effort consumes more tokens" {
		t.Fatalf("price note = %q", price.Note)
	}
	if slow.PlanGated {
		t.Error("a model with no gate block came back gated")
	}

	if fast := models[1]; !fast.PlanGated || fast.UID != "swe-1-6-fast" {
		t.Fatalf("second model = %+v, want swe-1-6-fast and gated", fast)
	}
}

func TestFindModelMatchesTheUIDExactly(t *testing.T) {
	models := DecodeModelCatalogue(encodeTestCatalogue())
	if _, ok := FindModel(models, "swe-1-6-slow"); !ok {
		t.Error("an exact uid did not match")
	}
	// The catalogue lists aliases, but a client sends the uid; matching an alias here
	// would make the panel describe a model the backend would refuse.
	if _, ok := FindModel(models, "swe-1p6"); ok {
		t.Error("an alias matched as if it were a uid")
	}
}

// The identity is not cosmetic: asked as this proxy's own name, the backend answers
// with a one-entry chat list instead of the catalogue, so which identity the fetch
// uses is load-bearing and is pinned here rather than left to the caller.
func TestFetchModelCatalogueAsksAsTheCLI(t *testing.T) {
	var gotIdentity string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != GetCliModelConfigsPath {
			t.Errorf("posted to %s, want %s", r.URL.Path, GetCliModelConfigsPath)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/proto" {
			t.Errorf("content-type = %q", ct)
		}
		body := make([]byte, r.ContentLength)
		if _, err := r.Body.Read(body); err != nil && err.Error() != "EOF" {
			t.Errorf("read body: %v", err)
		}
		gotIdentity = metadataIDEName(t, body)
		w.Header().Set("Content-Type", "application/proto")
		w.Write(encodeTestCatalogue())
	}))
	defer stub.Close()

	creds := &Credentials{APIKey: "devin-session-token$x", APIServerURL: stub.URL}
	models, err := NewClient(Options{}).FetchModelCatalogue(context.Background(), creds)
	if err != nil {
		t.Fatalf("FetchModelCatalogue: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("fetch decoded %d models", len(models))
	}
	if gotIdentity != CLIIdentity {
		t.Fatalf("ide_name = %q, want %q", gotIdentity, CLIIdentity)
	}
}

func TestFetchModelCatalogueRefusesAnEmptyAnswer(t *testing.T) {
	// An empty body decodes to no models, which must be an error: reporting "0
	// models" as the catalogue would show a panel that looks like a real answer.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/proto")
	}))
	defer stub.Close()

	creds := &Credentials{APIKey: "devin-session-token$x", APIServerURL: stub.URL}
	if _, err := NewClient(Options{}).FetchModelCatalogue(context.Background(), creds); err == nil {
		t.Fatal("an empty catalogue was accepted")
	}
}

// A response cut inside the last entry: that entry's length prefix still
// promises bytes the response does not carry. The reader's error used to be
// invisible, and the entries before the cut came back looking like the whole
// catalogue.
func TestACorruptCatalogueIsAnErrorNotAHalfCatalogue(t *testing.T) {
	cut := encodeTestCatalogue()[:len(encodeTestCatalogue())-5]

	models, err := decodeModelCatalogue(cut)
	if err == nil {
		t.Fatal("a corrupt catalogue decoded without an error")
	}
	if len(models) != 0 {
		t.Errorf("a corrupt catalogue still returned %d entries", len(models))
	}
	// The exported entry point keeps its signature for the tools; it must not
	// present the half that parsed as a complete answer either.
	if models := DecodeModelCatalogue(cut); len(models) != 0 {
		t.Errorf("DecodeModelCatalogue returned %d entries for a corrupt body", len(models))
	}
}

func TestFetchModelCatalogueRefusesACorruptBody(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/proto")
		w.Write(encodeTestCatalogue()[:40]) // a cut inside an entry: a length that cannot be satisfied
	}))
	defer stub.Close()

	creds := &Credentials{APIKey: "devin-session-token$x", APIServerURL: stub.URL}
	if _, err := NewClient(Options{}).FetchModelCatalogue(context.Background(), creds); err == nil {
		t.Fatal("a corrupt catalogue body was accepted")
	}
}

// metadataIDEName reads field 1 (the Metadata message) → field 1 (ide_name) out of a
// metadata request body.
func metadataIDEName(t *testing.T, body []byte) string {
	t.Helper()
	r := pb.NewReader(body)
	field, wire, ok := r.Field()
	if !ok || field != 1 || wire != pb.WireBytes {
		t.Fatalf("the request body does not start with the Metadata message: %d bytes", len(body))
	}
	inner := pb.NewReader(r.Bytes())
	f, w, ok := inner.Field()
	if !ok || f != 1 || w != pb.WireBytes {
		t.Fatal("the Metadata message has no ide_name at field 1")
	}
	return inner.String()
}

// encodeTestCatalogue builds a two-model catalogue in the live shape: one model with
// limits, flags, labels, aliases and a priced row, one gated behind a higher plan,
// and one nameless entry that must be dropped.
func encodeTestCatalogue() []byte {
	w := pb.NewWriter()
	w.Message(1, func(m *pb.Writer) {
		m.String(1, "SWE-1.6 Slow")
		m.String(22, "swe-1-6-slow")
		m.Message(23, func(cfg *pb.Writer) {
			cfg.Varint(4, 200000)
			cfg.String(5, "LLAMA_WITH_SPECIAL")
			cfg.Message(6, func(flags *pb.Writer) {
				flags.Varint(8, 1)
				flags.Varint(11, 1)
				flags.Varint(12, 1)
			})
			cfg.Varint(13, 128000)
			cfg.String(20, "swe-1p6")
			cfg.String(20, "swe-1p5")
		})
		m.Message(30, func(opt *pb.Writer) {
			opt.Message(2, func(row *pb.Writer) { row.String(1, "Speed") })
		})
		m.Message(32, func(p *pb.Writer) {
			p.String(1, "Input")
			p.Float32(2, 0.5)
			p.String(3, "1M tokens")
			p.String(7, "Higher effort consumes more tokens")
		})
	})
	w.Message(1, func(m *pb.Writer) {
		m.String(1, "SWE-1.6 Fast")
		m.String(22, "swe-1-6-fast")
		m.Message(33, func(gate *pb.Writer) { gate.Varint(1, 1) })
	})
	w.Message(1, func(m *pb.Writer) { m.String(1, "no uid") })
	return w.Bytes()
}

// A catalogue of this size is the real one, so nothing in the decoder may depend on
// the entries being few or the fields being present in any order.
func TestDecodeModelCatalogueJSONShape(t *testing.T) {
	models := DecodeModelCatalogue(encodeTestCatalogue())
	b, err := json.Marshal(models)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back []map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back[0]["uid"] != "swe-1-6-slow" || back[0]["context_window"] != float64(200000) {
		t.Fatalf("json = %v", back[0])
	}
	if _, present := back[0]["plan_gated"]; present {
		t.Error("an ungated model should omit plan_gated rather than say false")
	}
}
