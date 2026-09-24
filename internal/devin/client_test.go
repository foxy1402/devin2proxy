package devin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
