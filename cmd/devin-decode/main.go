// Command devin-decode replays a capture recorded by devin-probe through the
// typed decoder, so a captured request or response stream can be read in full
// without spending any account quota.
//
//	devin-decode -file captures-relay\capture-018-request.txt
//	devin-decode -file captures-relay\capture-018-response.txt
package main

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/pb"
)

func main() {
	path := flag.String("file", "", "capture file written by devin-probe, or - for stdin")
	kind := flag.String("kind", "auto", "auto | request | response")
	raw := flag.Bool("raw", false, "also print a schema-less hex decode of each frame")
	flag.Parse()

	if *path == "" {
		fmt.Fprintln(os.Stderr, "usage: devin-decode -file <capture.txt> [-kind request|response]")
		os.Exit(2)
	}

	body, err := readCaptureHex(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read capture: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("capture: %s (%d bytes)\n\n", *path, len(body))

	frames, ferr := devin.ParseFrames(body)
	if ferr != nil {
		fmt.Printf("! envelope parse: %v\n", ferr)
		fmt.Printf("! falling back to treating the whole body as one message\n\n")
		frames = []devin.Frame{{Flag: devin.FrameData, Data: body}}
	}

	k := resolveKind(*kind, *path, frames)

	for i, fr := range frames {
		switch {
		case fr.Flag&devin.FrameEndStream != 0:
			es, err := devin.ParseEndStream(fr.Data)
			if err != nil {
				fmt.Printf("frame %d: END-STREAM (undecodable): %s\n", i, fr.Data)
				continue
			}
			if es.Error != nil {
				fmt.Printf("frame %d: END-STREAM error=%v\n", i, es.Error)
			} else {
				fmt.Printf("frame %d: END-STREAM ok metadata=%v\n", i, es.Metadata)
			}
			continue
		}

		fmt.Printf("frame %d: flag=0x%02x len=%d\n", i, fr.Flag, len(fr.Data))
		if k == "response" {
			fmt.Printf("  %s\n", devin.DescribeResponse(devin.DecodeGetChatMessageResponse(fr.Data)))
		} else {
			fmt.Printf("%s", devin.DescribeRequest(devin.DecodeGetChatMessageRequest(fr.Data)))
		}
		if *raw {
			fmt.Printf("  --- raw ---\n")
			for _, line := range strings.Split(strings.TrimRight(pb.Dump(fr.Data), "\n"), "\n") {
				fmt.Printf("  %s\n", line)
			}
		}
		fmt.Println()
	}
}

// resolveKind decides whether the capture holds a request or a response when
// the caller did not say. The filename is the most reliable signal because the
// probe names files accordingly; otherwise a response is recognisable by its
// bot-<uuid> message id.
func resolveKind(kind, path string, frames []devin.Frame) string {
	if kind == "request" || kind == "response" {
		return kind
	}
	lower := strings.ToLower(path)
	if strings.Contains(lower, "response") {
		return "response"
	}
	if strings.Contains(lower, "request") {
		return "request"
	}
	for _, fr := range frames {
		if fr.Flag&devin.FrameEndStream != 0 {
			continue
		}
		if resp := devin.DecodeGetChatMessageResponse(fr.Data); strings.HasPrefix(resp.MessageID, "bot-") {
			return "response"
		}
	}
	return "request"
}

// readCaptureHex pulls the full body out of a probe capture file, which stores
// it as a single `hex: <hex>` line.
func readCaptureHex(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Captured bodies can be tens of kilobytes, so the default 64 KiB line
	// limit is not enough.
	sc.Buffer(make([]byte, 0, 1<<20), 1<<27)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "hex: ") {
			continue
		}
		h := strings.TrimSpace(strings.TrimPrefix(line, "hex: "))
		// The probe truncates only its stdout copy; be tolerant anyway.
		if i := strings.Index(h, "..."); i >= 0 {
			h = h[:i]
		}
		h = strings.TrimSpace(h)
		if h == "" {
			return nil, fmt.Errorf("empty hex field")
		}
		return hex.DecodeString(h)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no `hex:` line found; is this a devin-probe capture?")
}
