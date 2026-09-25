package devin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// A 429 is the one refusal another account can answer immediately, so how many
// times a request is retried on the *same* account before giving up is a decision
// about the caller's alternatives rather than about the account. These tests pin
// both ends of it: the default budget for a caller with one account, and the
// single attempt a caller with several accounts asks for.

// rateLimitedBackend counts requests and always answers 429.
func rateLimitedBackend(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("resource exhausted"))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func attemptRateLimitedRequest(t *testing.T, retries int) int32 {
	t.Helper()
	srv, hits := rateLimitedBackend(t)
	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 5 * time.Second, RateLimitRetries: retries})
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := client.GetChatMessage(ctx, creds, &GetChatMessageRequest{})
	if err == nil {
		t.Fatal("a backend that only answers 429 returned no error")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want an HTTP 429", err)
	}
	return hits.Load()
}

func TestRateLimitRetryBudgetForASingleAccount(t *testing.T) {
	// Nothing else to try, so the request waits and tries the same account again
	// rather than failing on the first refusal.
	if got := attemptRateLimitedRequest(t, 0); got != 3 {
		t.Fatalf("the same account was tried %d times, want the default budget of 3", got)
	}
}

func TestRateLimitRetryBudgetWithAnotherAccount(t *testing.T) {
	// The caller has another account, so the first refusal must come back at once
	// for it to try: three attempts over more than a second against an account that
	// has just said "no quota" is time the next account could have spent answering.
	if got := attemptRateLimitedRequest(t, 1); got != 1 {
		t.Fatalf("the account was tried %d times before failing over, want 1", got)
	}
}

func TestRateLimitRetryBudgetFollowsThePerRequestFunc(t *testing.T) {
	// The static budget is sampled once at startup; a caller whose pool grows and
	// shrinks needs the budget read per request instead. The func wins over the
	// static field, and a zero from it falls back to that field's own reading of
	// zero — the default budget.
	t.Run("a live func is read per request and wins over the static field", func(t *testing.T) {
		srv, hits := rateLimitedBackend(t)
		var budget atomic.Int32
		budget.Store(3)
		client := NewClient(Options{
			MaxConcurrent:        1,
			HeaderTimeout:        5 * time.Second,
			RateLimitRetries:     3,
			RateLimitRetriesFunc: func() int { return int(budget.Load()) },
		})
		creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv.URL}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		go func() {
			// After the first refusal has been counted, cut the budget to one: the
			// request must return rather than retry twice more on the old value.
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if hits.Load() >= 1 {
					budget.Store(1)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		_, err := client.GetChatMessage(ctx, creds, &GetChatMessageRequest{})
		if err == nil {
			t.Fatal("a backend that only answers 429 returned no error")
		}
		if got := hits.Load(); got != 2 {
			t.Fatalf("the account was tried %d times, want 2: one at budget 3, one after the func dropped to 1", got)
		}
	})

	t.Run("a zero from the func falls back to the static field", func(t *testing.T) {
		srv, hits := rateLimitedBackend(t)
		client := NewClient(Options{
			MaxConcurrent:        1,
			HeaderTimeout:        5 * time.Second,
			RateLimitRetries:     2,
			RateLimitRetriesFunc: func() int { return 0 },
		})
		creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv.URL}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := client.GetChatMessage(ctx, creds, &GetChatMessageRequest{})
		if err == nil {
			t.Fatal("a backend that only answers 429 returned no error")
		}
		if got := hits.Load(); got != 2 {
			t.Fatalf("the account was tried %d times, want the static budget of 2", got)
		}
	})
}

// ---- backoff shape -------------------------------------------------------

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   time.Duration
		wantOK bool
	}{
		{"integer seconds", "5", 5 * time.Second, true},
		{"padded", " 7 ", 7 * time.Second, true},
		{"zero means now", "0", 0, true},
		{"capped at the maximum", "3600", maxRetryAfter, true},
		{"blank is ignored", "", 0, false},
		{"an HTTP-date is ignored", "Wed, 21 Oct 2015 07:28:00 GMT", 0, false},
		{"not a number is ignored", "soon", 0, false},
		{"negative is ignored", "-3", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.header)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("parseRetryAfter(%q) = %v, %v; want %v, %v", tc.header, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestTheRetryLadderKeepsItsOrderWithJitter(t *testing.T) {
	// The ladder's shape — 300ms then 900ms — is what keeps a rate-limited
	// account from being retried furiously; jitter may move each wait within
	// ±25% but must not reorder or rescale the steps.
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{2, retryBaseDelay},
		{3, retryBaseDelay * 3},
		{4, retryBaseDelay * 9},
	} {
		t.Run(fmt.Sprintf("attempt %d", tc.attempt), func(t *testing.T) {
			spread := tc.want / 4
			low, high := tc.want-spread, tc.want+spread
			restore := func() { retryJitter = rand.Float64 }
			defer restore()
			// Pin the jitter source at both ends: the wait must reach exactly
			// the ±25% bounds and, by implication, stay inside them elsewhere.
			retryJitter = func() float64 { return 0 }
			if got := retryDelay(tc.attempt); got != low {
				t.Fatalf("retryDelay(%d) at jitter 0 = %s, want the low end %s", tc.attempt, got, low)
			}
			retryJitter = func() float64 { return 1 }
			if got := retryDelay(tc.attempt); got != high {
				t.Fatalf("retryDelay(%d) at jitter 1 = %s, want the high end %s", tc.attempt, got, high)
			}
		})
	}
	// And the default source keeps every wait in bounds.
	restore := func() { retryJitter = rand.Float64 }
	defer restore()
	for range 50 {
		if got := retryDelay(3); got < retryBaseDelay*3*3/4 || got > retryBaseDelay*3*5/4 {
			t.Fatalf("retryDelay(3) = %s, outside the ±25%% band", got)
		}
	}
}

func TestARetryAfterHeaderReplacesTheLadderForOneWait(t *testing.T) {
	// The server said how long the refusal lasts, so that one wait honours the
	// header instead of the ladder — and only that one wait: the value is
	// consumed on use, so a stale Retry-After cannot leak into later attempts.
	// The jitter is pinned so the arithmetic is exact: with a first refusal
	// carrying Retry-After: 2 and a second refusing again, the waits are 2s plus
	// the ladder's unjittered 675ms low end. A stale header would wait another
	// 2s; ignoring the header would wait the ladder twice.
	retryJitter = func() float64 { return 0 }
	defer func() { retryJitter = rand.Float64 }()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 5 * time.Second, RateLimitRetries: 3})
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv.URL}
	start := time.Now()
	_, err := client.GetChatMessage(context.Background(), creds, &GetChatMessageRequest{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a backend that only answers 429 returned no error")
	}
	if hits.Load() != 3 {
		t.Fatalf("the backend saw %d requests, want the default budget of 3", hits.Load())
	}
	if elapsed < 2500*time.Millisecond || elapsed > 3500*time.Millisecond {
		t.Fatalf("finished in %s; want 2s (Retry-After) + 675ms (unjittered ladder), and only one of each", elapsed)
	}
}

// ---- stream lifecycle ----------------------------------------------------

// TestAnAbandonedStreamEventuallyReleasesItsSlot pins the safety net for a
// caller that drops a *Stream without Close: the semaphore slot and the body
// must be reclaimed when the stream becomes unreachable, or one leaked stream
// per request would quietly cap the proxy at its own concurrency limit.
func TestAnAbandonedStreamEventuallyReleasesItsSlot(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A body that never finishes, so nothing but the cleanup could reclaim
		// the stream's resources.
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		w.Write(EncodeFrame(FrameData, []byte{0x1a, 0x02, 'o', 'k'}))
		w.(http.Flusher).Flush()
		<-release
	}))
	// Close is registered first so it runs LAST: httptest.Server.Close waits
	// for the handler above to return, and the handler waits on release.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 5 * time.Second})
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv.URL}

	open := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s, err := client.GetChatMessage(ctx, creds, &GetChatMessageRequest{})
		if err != nil {
			t.Errorf("GetChatMessage: %v", err)
			return
		}
		// Deliberately not closed: the only reference dies with this frame.
		_ = s
	}
	open()

	// The cleanup runs when the garbage collector gets to it; nudge it until the
	// slot comes free rather than pretending GC is instant.
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case client.sem <- struct{}{}:
			<-client.sem
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the abandoned stream never released its semaphore slot")
		}
		runtime.GC()
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStreamCloseIsIdempotentAndReleasesOnce(t *testing.T) {
	baseURL, _ := startFakeBackendHTTP(t)
	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 5 * time.Second})
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: baseURL}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := client.GetChatMessage(ctx, creds, &GetChatMessageRequest{})
	if err != nil {
		t.Fatalf("GetChatMessage: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// A second Close must neither error nor release the slot again: with one
	// slot, a double release would block the next acquire forever.
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a second Close blocked, meaning the slot was released twice")
	}

	// And the slot really is free for the next request.
	if _, err := client.GetChatMessage(ctx, creds, &GetChatMessageRequest{}); err != nil {
		t.Fatalf("the slot was not reusable after Close: %v", err)
	}
}

// startFakeBackendHTTP is startFakeBackend with an http:// URL, for the stream
// lifecycle tests that talk to the client directly.
func startFakeBackendHTTP(t *testing.T) (string, *int32) {
	t.Helper()
	addr, hits := startFakeBackend(t, 0)
	return "http://" + addr, hits
}

// ---- unary ---------------------------------------------------------------

func TestPostUnaryTakesAConcurrencySlot(t *testing.T) {
	// The semaphore is the only cap on in-flight upstream calls, so a unary call
	// that skipped it would undercut the limit whenever a dashboard burst of
	// catalogue or status calls landed. Fill the one slot before the call: the
	// unary request must wait for it rather than walk straight through.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte{0x0a, 0x02, 'o', 'k'}) // field 1, "ok"
	}))
	t.Cleanup(srv.Close)

	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 5 * time.Second})
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv.URL}

	// Hold the slot, then make the unary call from a goroutine that must block.
	client.sem <- struct{}{}
	done := make(chan error, 1)
	go func() {
		_, err := client.PostUnary(context.Background(), creds, GetCliModelConfigsPath, nil)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("PostUnary ran without a free slot (err %v); it is not taking the semaphore", err)
	case <-time.After(100 * time.Millisecond):
	}
	<-client.sem
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PostUnary after the slot freed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PostUnary never ran after the slot was released")
	}
}

func TestPostUnaryRejectsAnOversizedResponse(t *testing.T) {
	// LimitReader reads to its cap without complaint, so a response past the cap
	// used to come back silently truncated. Reading one byte past the cap is
	// what turns that into an error instead of half a catalogue.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write(make([]byte, maxUnaryResponseBytes+16))
	}))
	t.Cleanup(srv.Close)

	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 5 * time.Second})
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv.URL}
	_, err := client.PostUnary(context.Background(), creds, GetCliModelConfigsPath, nil)
	if err == nil {
		t.Fatal("an oversized unary response was accepted")
	}
	// A response of exactly the cap is not an overflow.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write(make([]byte, maxUnaryResponseBytes))
	}))
	t.Cleanup(srv2.Close)
	creds2 := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv2.URL}
	if _, err := client.PostUnary(context.Background(), creds2, GetCliModelConfigsPath, nil); err != nil {
		t.Fatalf("a response of exactly the cap was rejected: %v", err)
	}
}

func TestPostUnaryReportsACancelledContextWithoutCoolingTheRoute(t *testing.T) {
	// A caller deadline that reached Report must stay non-Broken for the route:
	// the caller gave up, and the direct route here did nothing wrong.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
	}))
	t.Cleanup(srv.Close)

	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 5 * time.Second})
	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.PostUnary(ctx, creds, GetCliModelConfigsPath, nil); err == nil {
		t.Fatal("a timed-out unary call returned no error")
	}
}
