// Command devin-probe captures the HTTP traffic the Devin CLI sends to its
// inference backend, so the wire protocol can be read rather than guessed.
//
// Point the CLI at it with the documented base-URL override:
//
//	WINDSURF_API_SERVER_URL=http://127.0.0.1:8787 devin -p "hi"
//
// With -relay the request is forwarded to the real upstream and the response is
// streamed back untouched, so the CLI behaves normally while both directions
// are recorded. Without -relay the request is recorded and a Connect
// end-of-stream error is returned, which captures the request without spending
// any account quota.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"devin2proxy/internal/pb"
)

var (
	addr    = flag.String("addr", "127.0.0.1:8787", "listen address")
	outDir  = flag.String("out", "captures", "directory to write captured traffic into")
	relayTo = flag.String("relay", "", "upstream base URL to forward to, e.g. https://server.codeium.com (empty = don't forward)")
	quiet   = flag.Bool("quiet", false, "only log a one-line summary per request")
)

var (
	seqMu sync.Mutex
	seq   int
)

// Secrets appear in headers and inside protobuf string fields. Mask them before
// anything reaches the log or disk.
var tokenRe = regexp.MustCompile(`devin-session-token\$[A-Za-z0-9._\-]{8,}`)

func redact(s string) string {
	if len(s) > 30 {
		return s[:30] + fmt.Sprintf("...(len %d)", len(s))
	}
	return s
}

func redactToken(s string) string {
	return tokenRe.ReplaceAllStringFunc(s, func(m string) string {
		return m[:30] + fmt.Sprintf("...(len %d)", len(m))
	})
}

// sensitiveHeaders are values we never write out in full.
var sensitiveHeaders = map[string]bool{
	"authorization":        true,
	"proxy-authorization":  true,
	"cookie":               true,
	"x-api-key":            true,
	"x-codeium-csrf-token": true,
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("create out dir: %v", err)
	}

	if *relayTo != "" {
		if _, err := url.Parse(*relayTo); err != nil {
			log.Fatalf("bad -relay URL: %v", err)
		}
	}

	log.Printf("devin-probe listening on http://%s", *addr)
	if *relayTo == "" {
		log.Printf("mode: CAPTURE ONLY (no quota spent) - requests are recorded and then rejected")
	} else {
		log.Printf("mode: RELAY to %s (response streamed back to the CLI)", *relayTo)
	}
	log.Printf("run:  set WINDSURF_API_SERVER_URL=http://%s  then invoke devin", *addr)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           http.HandlerFunc(handle),
		ReadHeaderTimeout: 30 * time.Second,
		// No write timeout: relayed chat responses are long-lived streams.
	}
	log.Fatal(srv.ListenAndServe())
}

func nextSeq() int {
	seqMu.Lock()
	defer seqMu.Unlock()
	seq++
	return seq
}

func handle(w http.ResponseWriter, r *http.Request) {
	n := nextSeq()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[%d] read body: %v", n, err)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	prefix := fmt.Sprintf("capture-%03d", n)
	log.Printf("")
	log.Printf("================ REQUEST %d ================", n)
	log.Printf("%s %s %s", r.Method, r.URL.Path, r.Proto)
	if r.URL.RawQuery != "" {
		log.Printf("query: %s", r.URL.RawQuery)
	}
	for _, k := range sortedKeys(r.Header) {
		v := strings.Join(r.Header.Values(k), ", ")
		if sensitiveHeaders[strings.ToLower(k)] {
			v = redact(v)
		}
		log.Printf("header %s: %s", k, v)
	}
	log.Printf("body: %d bytes", len(body))
	log.Printf("body hex: %s", hexPrefix(body, 256))

	requestText := describeBody(body)
	if !*quiet {
		log.Printf("--- request decode ---\n%s------------------------", requestText)
	}

	writeCapture(prefix+"-request.txt", fmt.Sprintf(
		"%s %s %s\n%s\n\nbody %d bytes\nhex: %x\n\n--- decode ---\n%s\n",
		r.Method, r.URL.Path, r.Proto, headerBlock(r.Header), len(body), body, requestText))

	// Resource requests for the model catalogue are unary rather than streams.
	isStreaming := strings.Contains(r.Header.Get("Content-Type"), "connect+proto")

	if *relayTo == "" {
		writeConnectEndStream(w, "unimplemented", "devin-probe: captured, not forwarded")
		log.Printf("[%d] captured (no relay); returned end-stream error", n)
		return
	}

	relay(w, r, body, n, prefix, isStreaming)
}

func relay(w http.ResponseWriter, r *http.Request, body []byte, n int, prefix string, isStreaming bool) {
	target := strings.TrimRight(*relayTo, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		log.Printf("[%d] build upstream request: %v", n, err)
		http.Error(w, "upstream request error", http.StatusBadGateway)
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Host = ""
	req.ContentLength = int64(len(body))

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		log.Printf("[%d] upstream error: %v", n, err)
		writeConnectEndStream(w, "unavailable", "upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()

	log.Printf("--- upstream response %d ---", resp.StatusCode)
	for _, k := range sortedKeys(resp.Header) {
		log.Printf("  %s: %s", k, strings.Join(resp.Header.Values(k), ", "))
	}

	copyHeadersExcept(w.Header(), resp.Header)
	// Flush per frame so the CLI sees tokens as they arrive.
	flusher, _ := w.(http.Flusher)
	w.WriteHeader(resp.StatusCode)
	if flusher != nil {
		flusher.Flush()
	}

	var captured bytes.Buffer
	buf := make([]byte, 32*1024)
	for {
		rn, rerr := resp.Body.Read(buf)
		if rn > 0 {
			chunk := buf[:rn]
			captured.Write(chunk)
			if _, werr := w.Write(chunk); werr != nil {
				log.Printf("[%d] client write stopped: %v", n, werr)
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				log.Printf("[%d] upstream read: %v", n, rerr)
			}
			break
		}
	}

	respText := describeBody(captured.Bytes())
	if !*quiet {
		log.Printf("--- response decode ---\n%s------------------------", respText)
	}
	writeCapture(prefix+"-response.txt", fmt.Sprintf(
		"status %d\n%s\n\nbody %d bytes\nhex: %x\n\n--- decode ---\n%s\n",
		resp.StatusCode, headerBlock(resp.Header), captured.Len(), captured.Bytes(), respText))
	log.Printf("[%d] done", n)
}

// describeBody unwraps Connect's 5-byte envelope when present and dumps each
// frame, falling back to a raw protobuf dump for un-enveloped bodies.
func describeBody(b []byte) string {
	if len(b) == 0 {
		return "(empty)"
	}
	frames, err := parseFrames(b)
	if err != nil || len(frames) == 0 {
		return "(not Connect-enveloped; raw protobuf dump)\n" + redactToken(pb.Dump(b))
	}
	var sb strings.Builder
	for i, fr := range frames {
		fmt.Fprintf(&sb, "frame %d: flag=0x%02x len=%d\n", i, fr.Flag, len(fr.Data))
		switch {
		case fr.Flag&0x02 != 0:
			// End-of-stream frame carries a JSON blob, not protobuf.
			fmt.Fprintf(&sb, "  end-stream JSON: %s\n", redactToken(string(fr.Data)))
		default:
			sb.WriteString(indent(redactToken(pb.Dump(fr.Data)), "  "))
		}
	}
	return sb.String()
}

type frame struct {
	Flag byte
	Data []byte
}

// parseFrames splits Connect envelopes: [flags:1][length:4 big-endian][payload].
func parseFrames(b []byte) ([]frame, error) {
	var out []frame
	for len(b) > 0 {
		if len(b) < 5 {
			return out, fmt.Errorf("truncated envelope header (%d bytes left)", len(b))
		}
		flags := b[0]
		n := int(binary.BigEndian.Uint32(b[1:5]))
		if 5+n > len(b) {
			return out, fmt.Errorf("envelope claims %d bytes but only %d remain", n, len(b)-5)
		}
		out = append(out, frame{Flag: flags, Data: b[5 : 5+n]})
		b = b[5+n:]
	}
	return out, nil
}

func writeConnectEndStream(w http.ResponseWriter, code, msg string) {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": code, "message": msg},
	})
	var buf bytes.Buffer
	buf.WriteByte(0x02)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(payload)))
	buf.Write(l[:])
	buf.Write(payload)

	w.Header().Set("Content-Type", "application/connect+proto")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func writeCapture(name, content string) {
	path := filepath.Join(*outDir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		log.Printf("write %s: %v", path, err)
	}
}

func headerBlock(h http.Header) string {
	var sb strings.Builder
	for _, k := range sortedKeys(h) {
		sb.WriteString(k + ": " + strings.Join(h.Values(k), ", ") + "\n")
	}
	return sb.String()
}

func sortedKeys(h http.Header) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// hopByHopHeaders must not be forwarded by a proxy.
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"host":                true,
	"content-length":      true,
}

func copyHeaders(dst, src http.Header) { copyFiltered(dst, src, func(string) bool { return false }) }

func copyHeadersExcept(dst, src http.Header) {
	copyFiltered(dst, src, func(k string) bool { return hopByHopHeaders[k] })
}

func copyFiltered(dst, src http.Header, drop func(string) bool) {
	for k, vs := range src {
		if drop(strings.ToLower(k)) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}

func hexPrefix(b []byte, max int) string {
	if len(b) > max {
		return fmt.Sprintf("%x...(truncated, %d bytes total)", b[:max], len(b))
	}
	return fmt.Sprintf("%x", b)
}
