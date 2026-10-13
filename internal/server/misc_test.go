package server

import (
	"net/http"
	"strings"
	"testing"

	"devin2proxy/internal/devin"
)

// TestModelFallbackKeyIsBounded pins B4: the dedup key is verbatim client input, so
// a multi-megabyte model name must be truncated before it is stored, or the map
// (capped at 512 entries) could hold hundreds of megabytes.
func TestModelFallbackKeyIsBounded(t *testing.T) {
	srv, _ := frameServer(t, textFrame("ok", 2, true), endFrame())
	long := strings.Repeat("x", 1<<20) // 1 MiB
	srv.noteModelFallback(long, "swe-1.6")

	srv.fallbackMu.Lock()
	defer srv.fallbackMu.Unlock()
	for k := range srv.fallbackSeen {
		if len(k) > maxModelFallbackKeyBytes+len("…") {
			t.Fatalf("stored a fallback key of %d bytes, cap is %d", len(k), maxModelFallbackKeyBytes)
		}
	}
	if len(srv.fallbackSeen) == 0 {
		t.Fatal("the long name was not recorded at all")
	}
}

// TestHealthzCarriesCORSHeaders pins B10: /healthz gets the same cross-origin
// treatment as /v1/*, so a browser probe does not fail preflight.
func TestHealthzCarriesCORSHeaders(t *testing.T) {
	srv := New(Config{
		APIKey:       "sk-devin-test",
		Models:       []string{"swe-1.6"},
		AllowOrigins: "https://example.test",
		Creds:        devin.NewPool(nil, ""),
	}, devin.NewClient(devin.Options{MaxConcurrent: 1}))
	rec := do(srv, http.MethodGet, "/healthz", "", "")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://example.test" {
		t.Errorf("healthz Access-Control-Allow-Origin = %q, want the configured origin", got)
	}
}
