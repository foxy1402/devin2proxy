package devin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"devin2proxy/internal/pb"
)

// This file reads the model catalogue: the unary GetCliModelConfigs call the CLI
// makes at startup, one entry per model the account may be offered, each with its
// limits and prices. It costs no chat quota.
//
// The field numbers are not documented anywhere; they were read off the wire and
// are recorded in docs/PROTOCOL.md.

// CLIIdentity is the metadata ide_name the CLI sends, and the only one that gets
// the real catalogue back.
//
// The identity is not cosmetic here. Asked as this proxy's own "devin-cli",
// GetCliModelConfigs answers with a single-entry list of Devin's chat models —
// the same short list the status call reports at field 33 — and no coding model at
// all. The CLI's identity is what returns the full catalogue, which is the question
// this call exists to answer ("what does the CLI see, and what are those models'
// limits"). The chat path is a different matter and keeps its own identity; see
// DefaultMetadata and docs/PROTOCOL.md.
const CLIIdentity = "chisel"

// catalogueTimeout bounds the catalogue fetch. It is one unary call returning a
// few hundred kilobytes, so a long wait means a problem rather than a slow answer.
const catalogueTimeout = 30 * time.Second

// ModelConfig is one model in the catalogue: what a client may ask for, what its
// limits are, and what it costs.
type ModelConfig struct {
	// UID is the value a client sends as chat_model_uid.
	UID  string `json:"uid,omitempty"`
	Name string `json:"name,omitempty"`
	// Aliases are the alternate uids the catalogue lists for this model. They are
	// the backend's aliases, not this proxy's: the proxy's own aliases are in its
	// configuration.
	Aliases []string `json:"aliases,omitempty"`
	// ContextWindow and MaxOutput are token counts, the two numbers a coding client
	// wants before it will send anything.
	ContextWindow uint64 `json:"context_window,omitempty"`
	MaxOutput     uint64 `json:"max_output,omitempty"`
	Tokenizer     string `json:"tokenizer,omitempty"`
	// Capabilities are the field numbers set in the model's flag block. Which bit
	// means what is not established, so they are reported as the raw set rather
	// than named; see docs/PROTOCOL.md.
	Capabilities []int `json:"capabilities,omitempty"`
	// Options are the UI labels the catalogue carries for the model, such as
	// "Speed" or "Reasoning Effort".
	Options []string     `json:"options,omitempty"`
	Prices  []ModelPrice `json:"prices,omitempty"`
	// PlanGated is true when the entry carries the block that marks a model as
	// needing a higher plan than the account asking has. It is what separates
	// swe-1-6-slow (usable here) from swe-1-6-fast (refused).
	PlanGated bool `json:"plan_gated,omitempty"`
}

// ModelPrice is one priced line of a model: "Input 0.5 per 1M tokens".
//
// Only the first amount is carried. The row also holds two further floats whose
// meaning is not established — they are equal to the first for some models and
// wildly different for others — so rather than guess, they are left unread.
type ModelPrice struct {
	Label  string  `json:"label,omitempty"`
	Unit   string  `json:"unit,omitempty"`
	Amount float64 `json:"amount,omitempty"`
	// Tag is the row's own label when it carries one, e.g. "Free" on the Sidekick
	// row every model has.
	Tag  string `json:"tag,omitempty"`
	Note string `json:"note,omitempty"`
}

// FetchModelCatalogue asks the backend for the model catalogue as the CLI sees it.
// It is a plain unary call and spends no chat quota.
func (c *Client) FetchModelCatalogue(ctx context.Context, creds *Credentials) ([]ModelConfig, error) {
	if creds == nil || creds.APIKey == "" {
		return nil, errors.New("devin: no credential available")
	}
	md := DefaultMetadata(creds.APIKey)
	md.IDEName = CLIIdentity
	ctx, cancel := context.WithTimeout(ctx, catalogueTimeout)
	defer cancel()
	body, err := c.PostUnary(ctx, creds, GetCliModelConfigsPath, EncodeMetadataRequest(md))
	if err != nil {
		return nil, err
	}
	models, err := decodeModelCatalogue(body)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, errors.New("devin: the model catalogue came back empty")
	}
	return models, nil
}

// Find returns the entry for a model uid. The match is exact: the catalogue's uids
// are what the backend accepts, so anything else is a different model.
func FindModel(models []ModelConfig, uid string) (ModelConfig, bool) {
	for _, m := range models {
		if m.UID == uid {
			return m, true
		}
	}
	return ModelConfig{}, false
}

// DecodeModelCatalogue reads the catalogue out of a GetCliModelConfigs response.
// Its top level is one repeated field 1, one model per entry. It is the
// best-effort entry point the diagnostic tools use; the fetch path goes through
// decodeModelCatalogue, which reports a corrupt body instead of quietly
// returning whatever happened to parse.
func DecodeModelCatalogue(b []byte) []ModelConfig {
	models, _ := decodeModelCatalogue(b)
	return models
}

// decodeModelCatalogue is DecodeModelCatalogue with the reader's error surfaced.
// A corrupt catalogue — a length that runs past the end of the buffer, say — used
// to decode to the entries before the corruption with no signal, presenting half
// a catalogue as the whole thing.
func decodeModelCatalogue(b []byte) ([]ModelConfig, error) {
	var out []ModelConfig
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			break
		}
		if field == 1 && wire == pb.WireBytes {
			if m, ok := decodeModelConfig(r.Bytes()); ok {
				out = append(out, m)
			}
			continue
		}
		r.Skip(wire)
	}
	if r.Err() != nil {
		return nil, fmt.Errorf("devin: corrupt model catalogue: %w", r.Err())
	}
	return out, nil
}

// decodeModelConfig reads one model. An entry with no uid is not a model and is
// dropped: the uid is the one field every model has and the only one a client uses.
func decodeModelConfig(b []byte) (ModelConfig, bool) {
	var m ModelConfig
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case field == 1 && wire == pb.WireBytes:
			m.Name = r.String()
		case field == 3 && wire == pb.WireFixed32:
			// A rating, unset (-1) for most models and 3 for SWE-1.6 Slow. Nothing
			// reads it, but it is what the catalogue calls it.
			r.Fixed32()
		case field == 18 && wire == pb.WireVarint:
			// Mirrors the context window at the top level; the nested copy below is
			// the one that is always present.
			r.Uint64()
		case field == 22 && wire == pb.WireBytes:
			m.UID = r.String()
		case field == 23 && wire == pb.WireBytes:
			decodeModelLimits(r.Bytes(), &m)
		case field == 30 && wire == pb.WireBytes:
			m.Options = append(m.Options, decodeModelOptions(r.Bytes())...)
		case field == 32 && wire == pb.WireBytes:
			m.Prices = append(m.Prices, decodeModelPrice(r.Bytes()))
		case field == 33 && wire == pb.WireBytes:
			m.PlanGated = true
			r.Skip(wire)
		default:
			r.Skip(wire)
		}
	}
	return m, m.UID != ""
}

// decodeModelLimits reads the nested config: the context window at field 4, the
// output ceiling at field 13, the tokenizer's name at field 5, the capability flags
// at field 6 and the aliases at field 20. The context window was confirmed against
// the catalogue's own UI strings, which say "1M Context" for exactly the models
// where this reads 1000000.
func decodeModelLimits(b []byte, m *ModelConfig) {
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			return
		}
		switch {
		case field == 4 && wire == pb.WireVarint:
			m.ContextWindow = r.Uint64()
		case field == 5 && wire == pb.WireBytes:
			m.Tokenizer = r.String()
		case field == 6 && wire == pb.WireBytes:
			m.Capabilities = fieldNumbers(r.Bytes())
		case field == 13 && wire == pb.WireVarint:
			m.MaxOutput = r.Uint64()
		case field == 20 && wire == pb.WireBytes:
			m.Aliases = append(m.Aliases, r.String())
		default:
			r.Skip(wire)
		}
	}
}

// fieldNumbers returns the field numbers present in a message, which for the
// capability block is the set of flags that are switched on. The numbers are kept
// rather than mapped to names: which bit means what would be a guess.
func fieldNumbers(b []byte) []int {
	var keys []int
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			break
		}
		keys = append(keys, field)
		r.Skip(wire)
	}
	sort.Ints(keys)
	return keys
}

// decodeModelOptions reads the display block, which carries UI labels such as
// "Speed" or "1M Context" one nesting level down.
func decodeModelOptions(b []byte) []string {
	var names []string
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			return names
		}
		if field == 2 && wire == pb.WireBytes {
			inner := pb.NewReader(r.Bytes())
			for {
				f, w, ok := inner.Field()
				if !ok {
					break
				}
				if f == 1 && w == pb.WireBytes {
					names = append(names, inner.String())
					continue
				}
				inner.Skip(w)
			}
			continue
		}
		r.Skip(wire)
	}
}

// decodeModelPrice reads one priced line: its label at field 1, the amount at
// field 2, the unit at field 3, a note at field 7 and an optional tag at field 8.
func decodeModelPrice(b []byte) ModelPrice {
	var p ModelPrice
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			return p
		}
		switch {
		case field == 1 && wire == pb.WireBytes:
			p.Label = r.String()
		case field == 2 && wire == pb.WireFixed32:
			p.Amount = float64(math.Float32frombits(r.Fixed32()))
		case field == 3 && wire == pb.WireBytes:
			p.Unit = r.String()
		case field == 7 && wire == pb.WireBytes:
			p.Note = r.String()
		case field == 8 && wire == pb.WireBytes:
			p.Tag = r.String()
		default:
			r.Skip(wire)
		}
	}
}

// Label renders a model for a log line.
func (m ModelConfig) Label() string {
	if m.Name == "" {
		return m.UID
	}
	return m.Name + " (" + m.UID + ")"
}
