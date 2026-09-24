// Command devin-models prints the model catalogue: uid, name, context window,
// output limit, tokenizer, capability flags, UI labels and price per 1M tokens.
//
// Two ways in, neither of which spends chat quota:
//
//	devin-models -live                 call GetCliModelConfigs for real (~250 models)
//	devin-models -file captures-relay\capture-005-response.txt
//
// The capture path exists so a catalogue seen once can be re-read without asking
// the backend again; `-live` is what the dashboard's Models tab uses. The decode
// itself lives in internal/devin/catalogue.go so both share one reading of the
// field layout, and that layout is recorded in docs/PROTOCOL.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/pb"
)

func main() {
	path := flag.String("file", "", "capture file holding a GetCliModelConfigs response")
	live := flag.Bool("live", false, "call GetCliModelConfigs for real instead of reading a capture")
	ide := flag.String("ide", devin.CLIIdentity, "with -live: metadata.ide_name. The default, chisel, is what the CLI sends and the only identity that returns the coding catalogue; \"devin-cli\" returns the one-entry chat list instead")
	uid := flag.String("uid", "", "only print the entry whose model uid matches (substring)")
	available := flag.Bool("available", false, "only print models that are not gated behind a higher plan")
	dump := flag.Bool("dump", false, "also print the raw wire dump of each matching entry")
	flag.Parse()

	var body []byte
	switch {
	case *live:
		creds, err := devin.LoadCredentials()
		if err != nil {
			fmt.Fprintf(os.Stderr, "credential: %v\n", err)
			os.Exit(1)
		}
		md := devin.DefaultMetadata(creds.APIKey)
		if *ide != "" {
			md.IDEName = *ide
		}
		client := devin.NewClient(devin.Options{})
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		body, err = client.PostUnary(ctx, creds, devin.GetCliModelConfigsPath, devin.EncodeMetadataRequest(md))
		if err != nil {
			fmt.Fprintf(os.Stderr, "GetCliModelConfigs: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("live response: %d bytes (ide_name=%s)\n", len(body), md.IDEName)
	case *path != "":
		captured, err := devin.ReadCaptureBody(*path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read capture: %v\n", err)
			os.Exit(1)
		}
		body = captured
	default:
		fmt.Fprintln(os.Stderr, "usage: devin-models {-file <capture.txt> | -live} [-uid swe] [-available] [-ide chisel] [-dump]")
		os.Exit(2)
	}

	// A unary response is a bare message, but tolerate an enveloped one too.
	payload, err := devin.RequestPayload(body)
	if err != nil {
		payload = body
	}

	models := devin.DecodeModelCatalogue(payload)
	if len(models) == 0 {
		fmt.Fprintf(os.Stderr, "no model entries found (%d bytes)\n", len(payload))
		os.Exit(1)
	}
	// The dump walks the payload a second time and prints the matching entry's raw
	// fields. Both walks read the same repeated field 1 in the same order, so the
	// indexes line up; this is the tool that discovered the layout, and keeping it
	// is how a new field gets found without a second decoder in the library.
	raws := rawEntries(payload)

	matched, usable := 0, 0
	for i, m := range models {
		if !m.PlanGated {
			usable++
		}
		if *uid != "" && !strings.Contains(m.UID, *uid) {
			continue
		}
		if *available && m.PlanGated {
			continue
		}
		matched++
		printModel(m)
		if *dump {
			fmt.Println("  --- raw ---")
			for _, line := range strings.Split(strings.TrimRight(pb.Dump(raws[i]), "\n"), "\n") {
				fmt.Printf("  %s\n", line)
			}
			fmt.Println()
		}
	}
	fmt.Printf("%d model(s) matched of %d in the catalogue; %d are not gated behind a higher plan\n",
		matched, len(models), usable)
}

// rawEntries returns each model's raw bytes from a catalogue response.
func rawEntries(b []byte) [][]byte {
	var out [][]byte
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			return out
		}
		if field == 1 && wire == pb.WireBytes {
			out = append(out, r.Bytes())
			continue
		}
		r.Skip(wire)
	}
}

func printModel(m devin.ModelConfig) {
	fmt.Printf("%s\n", m.UID)
	if m.Name != "" {
		fmt.Printf("  name         %s\n", m.Name)
	}
	if len(m.Aliases) > 0 {
		fmt.Printf("  aliases      %s\n", strings.Join(m.Aliases, ", "))
	}
	fmt.Printf("  context      %s tokens\n", commas(m.ContextWindow))
	fmt.Printf("  max output   %s tokens\n", commas(m.MaxOutput))
	if m.Tokenizer != "" {
		fmt.Printf("  tokenizer    %s\n", m.Tokenizer)
	}
	if len(m.Capabilities) > 0 {
		fmt.Printf("  capabilities %v\n", m.Capabilities)
	}
	if len(m.Options) > 0 {
		fmt.Printf("  options      %s\n", strings.Join(m.Options, ", "))
	}
	for _, p := range m.Prices {
		line := fmt.Sprintf("  price %-13s %g per %s", p.Label, p.Amount, p.Unit)
		if p.Tag != "" {
			line += "  [" + p.Tag + "]"
		}
		if p.Note != "" {
			line += "  " + p.Note
		}
		fmt.Println(line)
	}
	if m.PlanGated {
		fmt.Printf("  access       Pro only\n")
	} else {
		fmt.Printf("  access       available on this plan\n")
	}
	fmt.Println()
}

func commas(n uint64) string {
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
