package devin

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// GetChatMessagePath is the Connect-RPC route for streaming chat completions.
const GetChatMessagePath = "/exa.api_server_pb.ApiServerService/GetChatMessage"

// GetCliModelConfigsPath lists the models the account may use. It is a unary
// call, so its body is a bare protobuf message rather than an envelope.
const GetCliModelConfigsPath = "/exa.api_server_pb.ApiServerService/GetCliModelConfigs"

// maxEnvelopeBytes bounds a single response envelope so a malformed length
// cannot make us allocate wildly.
const maxEnvelopeBytes = 32 << 20

// DefaultModel is what the CLI resolves to on this account.
const DefaultModel = "swe-1-6-slow"

// Client speaks the CLI's Connect-RPC protocol over HTTP using the credential
// the CLI stores on disk.
type Client struct {
	direct *http.Transport
	// egress rotates the outbound route, or holds nil for the machine's own
	// connection. It is swapped rather than mutated, because the dashboard can
	// replace the whole route list while requests are in flight and those
	// requests must keep using the transport they started on.
	egress atomic.Pointer[EgressPool]
	sem    chan struct{}
	// rateLimitRetries bounds how many times one account may be retried on a 429
	// before the failure is handed back to the caller. Callers that hold several
	// accounts set it low, because spending the next second retrying an account
	// that has just said "no quota" is worse than asking the next account.
	rateLimitRetries int
	// rateLimitRetriesFunc, when set, is consulted per request instead of the
	// static rateLimitRetries, so a caller whose account count changes at runtime
	// can keep the budget in step with it. See Options.RateLimitRetriesFunc.
	rateLimitRetriesFunc func() int
}

// Options tunes the client. Zero values pick sensible defaults.
type Options struct {
	// MaxConcurrent bounds in-flight upstream calls; the account is rate
	// limited, so exceeding it tends to produce 429s rather than speed.
	MaxConcurrent int
	// HeaderTimeout bounds the wait for response headers only, leaving long
	// generations unbounded except by the caller's context.
	HeaderTimeout time.Duration
	// Egress rotates outbound routes one per request. Nil means direct only.
	Egress *EgressPool
	// RateLimitRetries caps how many times a single request is retried on the
	// same account after an explicit 429. Zero means the default, which is the
	// full per-request retry budget: right for a single account, where waiting
	// is the only recourse. Set it to 1 when the caller can offer another
	// account instead, since a rate limit does not clear in the sub-second
	// backoff a retry costs.
	RateLimitRetries int
	// RateLimitRetriesFunc, when set, replaces RateLimitRetries and is evaluated
	// per request, so a caller whose account pool grows and shrinks at runtime
	// can keep the budget in step with it instead of the value it sampled once
	// at startup. Returning zero falls through to RateLimitRetries and its
	// default, matching that field's own reading of zero.
	RateLimitRetriesFunc func() int
}

func NewClient(opts Options) *Client {
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 4
	}
	if opts.HeaderTimeout <= 0 {
		opts.HeaderTimeout = 120 * time.Second
	}
	c := &Client{
		direct:               newRouteTransport(nil, EgressOptions{HeaderTimeout: opts.HeaderTimeout}),
		sem:                  make(chan struct{}, opts.MaxConcurrent),
		rateLimitRetries:     opts.RateLimitRetries,
		rateLimitRetriesFunc: opts.RateLimitRetriesFunc,
	}
	c.SetEgress(opts.Egress)
	return c
}

// Egress exposes the route pool for health reporting. It may be nil.
func (c *Client) Egress() *EgressPool { return c.egress.Load() }

// SetEgress replaces the route pool. A request that is already in flight keeps
// the transport it started on, so replacing the list cannot break it; the next
// request walks the new list. A nil pool means the machine's own connection.
// The replaced pool is returned so the caller can release its idle
// connections; a caller that ignores the result behaves as before.
func (c *Client) SetEgress(p *EgressPool) *EgressPool { return c.egress.Swap(p) }

// route picks the transport for one attempt, with the route it belongs to so the
// caller can report the outcome against it. A nil route means the machine's own
// connection.
func (c *Client) route() (*http.Transport, *Egress) {
	if e := c.egress.Load().Next(); e != nil {
		return e.tr, e
	}
	return c.direct, nil
}

// attempts bounds how many times a single request may be retried. It grows with
// the number of routes so that a run of dead exits is survivable, but stays
// capped: each attempt is a fresh dial, and a request that cannot get out within
// a few of them is a configuration problem rather than something to keep trying.
func (c *Client) attempts() int {
	n := maxStreamAttempts
	if routes := c.egress.Load().Len(); routes+1 > n {
		n = routes + 1
	}
	if n > maxAttemptsWithRoutes {
		n = maxAttemptsWithRoutes
	}
	return n
}

// rateLimitBudget is how many attempts one request may spend on 429s from the
// same account. Callers with several accounts lower it to one, so the first
// refusal sends the request to the next account rather than into a backoff.
// A RateLimitRetriesFunc, when configured, is read here — per request — and
// wins over the static value; a zero from either means the full default budget.
func (c *Client) rateLimitBudget() int {
	if c.rateLimitRetriesFunc != nil {
		if n := c.rateLimitRetriesFunc(); n > 0 {
			return n
		}
	}
	if c.rateLimitRetries > 0 {
		return c.rateLimitRetries
	}
	return maxStreamAttempts
}

// HTTPError reports a non-200 response from the backend.
type HTTPError struct {
	Status      int
	ContentType string
	Body        []byte
}

func (e *HTTPError) Error() string {
	detail := string(e.Body)
	if len(detail) > 300 {
		detail = detail[:300] + "..."
	}
	return fmt.Sprintf("devin: upstream HTTP %d (content-type %s): %s", e.Status, e.ContentType, detail)
}

// BasicAuthValue reproduces the header the CLI sends byte for byte: the Basic
// scheme with the credential as both username and password, separated by a
// hyphen and deliberately not base64-encoded.
func BasicAuthValue(token string) string {
	return "Basic " + token + "-" + token
}

// Stream is a live GetChatMessage response.
type Stream struct {
	body    io.ReadCloser
	release func()
	closed  bool
	// stopCleanup cancels the finalizer safety net registered when the stream
	// was opened; Close runs before it returns so the resources are released
	// exactly once. Nil for a stream built without one.
	stopCleanup *runtime.Cleanup
	// Route names the outbound route this stream was opened through, empty for the
	// machine's own connection. It is recorded here rather than looked up later
	// because the pool rotates per request: by the time a caller reports the
	// outcome, the next route has already been handed out.
	Route string
	// held is a frame that has been read but not yet handed to the caller. It
	// exists so the first frame can be inspected before the stream is committed
	// to: a refused credential comes back as HTTP 200 with the refusal in the
	// end-of-stream frame, so nothing is visible until a frame is actually read.
	held    *GetChatMessageResponse
	heldErr error
	hasHeld bool
}

// routeLabel names a route for a log line. A nil route is the machine's own
// connection, which is worth naming too: otherwise a log line is ambiguous
// between "direct" and "the label was lost".
func routeLabel(e *Egress) string {
	if e == nil {
		return "direct"
	}
	return e.label
}

// Recv returns the next response chunk. It reports io.EOF when the server ends
// the stream cleanly, or a *ConnectError when the server reports a failure
// inside the end-of-stream frame.
func (s *Stream) Recv() (*GetChatMessageResponse, error) {
	if s.hasHeld {
		s.hasHeld = false
		return s.held, s.heldErr
	}
	return s.recv()
}

// Peek reads one frame and remembers it, so the next Recv returns that same frame
// again. It reports the same errors Recv would.
//
// This is what makes a refused credential visible while a retry is still free:
// the account is not known to work until a frame has been read, and by the time
// the caller has read one it may already have written a response to its own
// client.
func (s *Stream) Peek() error {
	if s.hasHeld {
		return s.heldErr
	}
	s.held, s.heldErr = s.recv()
	s.hasHeld = true
	return s.heldErr
}

func (s *Stream) recv() (*GetChatMessageResponse, error) {
	for {
		var header [5]byte
		if _, err := io.ReadFull(s.body, header[:]); err != nil {
			switch {
			case errors.Is(err, io.EOF):
				return nil, io.EOF
			case errors.Is(err, io.ErrUnexpectedEOF):
				return nil, fmt.Errorf("devin: truncated envelope header: %w", err)
			default:
				return nil, fmt.Errorf("devin: read stream: %w", err)
			}
		}

		flag := header[0]
		n := int(binary.BigEndian.Uint32(header[1:5]))
		if n < 0 || n > maxEnvelopeBytes {
			return nil, fmt.Errorf("devin: implausible envelope length %d", n)
		}
		data := make([]byte, n)
		if n > 0 {
			if _, err := io.ReadFull(s.body, data); err != nil {
				return nil, fmt.Errorf("devin: truncated envelope body: %w", err)
			}
		}

		if flag&FrameEndStream != 0 {
			es, err := ParseEndStream(data)
			if err != nil {
				return nil, err
			}
			if es.Error != nil {
				return nil, es.Error
			}
			return nil, io.EOF
		}
		// Skip unknown frame flags rather than failing, so a new frame type
		// does not break the proxy. Skipping silently is not an option either:
		// a flag the backend starts setting on data frames — a compression bit,
		// say — would otherwise drop every frame without a trace, so the skip
		// is logged and a future change has somewhere to be noticed.
		if flag != FrameData {
			log.Printf("devin: skipping stream frame with unexpected flag 0x%02x (%d bytes)", flag, n)
			continue
		}
		resp := DecodeGetChatMessageResponse(data)
		if resp.truncated {
			// The frame announced its length and then ran out mid-message.
			// Handing back the half that parsed would present a truncated
			// delta as a complete one; the stream is over either way, so
			// report it.
			return nil, fmt.Errorf("devin: response frame is truncated (%d bytes)", len(data))
		}
		return resp, nil
	}
}

// Close releases the stream and its concurrency slot. It is safe to call twice.
func (s *Stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stopCleanup != nil {
		s.stopCleanup.Stop()
	}
	if s.release != nil {
		s.release()
	}
	return s.body.Close()
}

// GetChatMessage starts a streaming completion and returns the open stream. The
// caller must Close it.
func (c *Client) GetChatMessage(ctx context.Context, creds *Credentials, req *GetChatMessageRequest) (*Stream, error) {
	return c.postStream(ctx, creds, EncodeGetChatMessageRequest(req))
}

// PostStreamRaw sends an already-encoded GetChatMessageRequest. It exists so a
// captured request can be replayed verbatim, which is how the minimal set of
// fields the backend requires was determined.
func (c *Client) PostStreamRaw(ctx context.Context, creds *Credentials, payload []byte) (*Stream, error) {
	return c.postStream(ctx, creds, payload)
}

// Retries apply only to failures where the backend cannot have started
// generating: a connection that was never established, an unusable outbound
// route, or an explicit rate-limit rejection. Anything else is returned as-is,
// because retrying a request the backend may already have processed would spend
// the account's quota twice — the opposite of what a retry is for here.
const (
	maxStreamAttempts = 3
	// maxAttemptsWithRoutes caps the retry count when a route pool is configured,
	// so a long list of dead exits cannot turn one request into a long walk
	// through it.
	maxAttemptsWithRoutes = 6
	retryBaseDelay        = 300 * time.Millisecond
)

// retryJitter is the uniform [0,1) sample the backoff is spread with. It is a
// variable so a test can pin it; the default is math/rand/v2's package
// generator, which is safe for concurrent use and needs no seeding.
var retryJitter = rand.Float64

// jitter spreads a wait over ±25% of itself. Several requests that failed
// together retry together, and an unjittered ladder would send every one of
// them back to the backend in the same millisecond; a spread breaks them up
// without changing the shape of the ladder.
func jitter(want time.Duration) time.Duration {
	if want <= 0 {
		return 0
	}
	spread := want / 4
	return want - spread + time.Duration(retryJitter()*float64(2*spread))
}

func retryDelay(attempt int) time.Duration {
	// attempt is 1-based and only called for attempt > 1: 300ms, then 900ms.
	d := retryBaseDelay
	for i := 2; i < attempt; i++ {
		d *= 3
	}
	return jitter(d)
}

// maxRetryAfter caps how long a Retry-After header may hold one attempt back.
// The header is the server's estimate, not ours, and a misconfigured upstream
// asking for minutes would otherwise park the request on a wait far longer
// than the ladder it replaces.
const maxRetryAfter = 15 * time.Second

// parseRetryAfter reads the integer-seconds form of a Retry-After header — the
// form this backend sends — and bounds it. Anything else (blank, not a number,
// the HTTP-date form, negative) is ignored: the ladder is a fine default, and
// guessing at a date format is worse than using it.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0, false
	}
	d := time.Duration(secs) * time.Second
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d, true
}

func (c *Client) postStream(ctx context.Context, creds *Credentials, payload []byte) (*Stream, error) {
	if creds == nil || creds.APIKey == "" {
		return nil, errors.New("devin: no credential available")
	}
	base := creds.APIServerURL
	if base == "" {
		base = defaultAPIServerURL
	}

	frame := EncodeFrame(FrameData, payload)

	var lastErr error
	// A route failure means nothing reached the backend, so the next attempt uses
	// the next route and does not wait: the delay exists for rate limits, which
	// need time to clear.
	wait := true
	rateLimited := 0
	// retryAfter carries a Retry-After deadline from the most recent 429 into the
	// wait before the next attempt, replacing the ladder for that one wait. It is
	// consumed on use, so a later refusal without the header falls back to the
	// ladder rather than reusing a stale value.
	var retryAfter time.Duration
	for attempt := 1; attempt <= c.attempts(); attempt++ {
		if attempt > 1 && wait {
			delay := retryDelay(attempt)
			if retryAfter > 0 {
				delay, retryAfter = retryAfter, 0
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		wait = true

		// The body is consumed by the first attempt, so each retry needs a fresh
		// request over a fresh reader.
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+GetChatMessagePath, bytes.NewReader(frame))
		if err != nil {
			return nil, fmt.Errorf("devin: build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/connect+proto")
		httpReq.Header.Set("Connect-Protocol-Version", "1")
		httpReq.Header.Set("Accept", "*/*")
		httpReq.Header.Set("Authorization", BasicAuthValue(creds.APIKey))
		httpReq.ContentLength = int64(len(frame))

		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		tr, route := c.route()
		resp, err := tr.RoundTrip(httpReq)
		if err != nil {
			<-c.sem
			if ctx.Err() != nil {
				// The caller gave up; a retry would be pointless, and the failure
				// says nothing about the route it happened to be using.
				return nil, err
			}
			lastErr = withRoute(route, err)
			c.Egress().Report(route, lastErr)
			wait = false
			continue
		}
		if resp.StatusCode == http.StatusOK {
			s := &Stream{
				body:    resp.Body,
				release: func() { <-c.sem },
				Route:   routeLabel(route),
			}
			// A stream abandoned without Close holds its semaphore slot and its
			// connection open for the life of the process, so a cleanup is
			// registered as a safety net: if the stream becomes unreachable
			// without having been closed, the slot is released and the body
			// shut. The cleanup carries only the values it needs — never the
			// stream itself, which would keep it reachable and stop the cleanup
			// from ever running — and Close stops it before releasing the same
			// resources itself, so exactly one of the two ever fires.
			cleanup := runtime.AddCleanup(s, func(abandoned struct {
				release func()
				body    io.ReadCloser
			}) {
				abandoned.release()
				abandoned.body.Close()
			}, struct {
				release func()
				body    io.ReadCloser
			}{release: s.release, body: s.body})
			s.stopCleanup = &cleanup
			return s, nil
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		<-c.sem
		lastErr = &HTTPError{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: body}
		if resp.StatusCode != http.StatusTooManyRequests {
			return nil, lastErr
		}
		// The account said "not now", which is the one failure another account can
		// answer immediately. When the caller holds more than one, the budget is one
		// attempt, so this returns to let it try the next account instead of waiting
		// out a backoff against an account that has just refused.
		rateLimited++
		if rateLimited >= c.rateLimitBudget() {
			return nil, lastErr
		}
		// The server said how long the refusal lasts, so honour it for the next
		// wait instead of the ladder — bounded, and ignored entirely when the
		// header carries something this proxy does not read.
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
			retryAfter = d
		}
	}
	return nil, lastErr
}

// maxUnaryResponseBytes bounds how much of a unary response is read. The call
// is a small protobuf answer in every known case; the bound is a malformed or
// hostile backend defence, and reading one byte past it is how an overflow is
// told apart from a response that exactly fits.
const maxUnaryResponseBytes = 8 << 20

// PostUnary sends a unary (non-streaming) Connect request, which uses a bare
// protobuf body and the content type application/proto.
func (c *Client) PostUnary(ctx context.Context, creds *Credentials, path string, payload []byte) ([]byte, error) {
	if creds == nil || creds.APIKey == "" {
		return nil, errors.New("devin: no credential available")
	}
	base := creds.APIServerURL
	if base == "" {
		base = defaultAPIServerURL
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("devin: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/proto")
	httpReq.Header.Set("Connect-Protocol-Version", "1")
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("Authorization", BasicAuthValue(creds.APIKey))
	httpReq.ContentLength = int64(len(payload))

	// Unary calls take the same concurrency slot a stream does. Without this, a
	// burst of catalogue or status calls would bypass the cap the semaphore
	// exists to enforce, and those are exactly the calls a dashboard fan-out
	// makes in a burst.
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()

	// The catalogue call is made once at startup, not per request, so it takes the
	// next route and no retry of its own; a route failure here is reported the
	// same way a streaming one is, so a dead exit is cooled either way.
	tr, route := c.route()
	resp, err := tr.RoundTrip(httpReq)
	if err != nil {
		err = withRoute(route, err)
		c.Egress().Report(route, err)
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUnaryResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("devin: read response: %w", err)
	}
	// LimitReader reads without complaining at the cap, so an oversized response
	// would otherwise be handed back silently truncated — half a catalogue, say,
	// with the rest missing and nothing to say so. One extra byte read is what
	// turns that into an error.
	if len(body) > maxUnaryResponseBytes {
		return nil, fmt.Errorf("devin: unary response exceeds %d bytes", maxUnaryResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: body}
	}
	return body, nil
}

// DefaultMetadata returns the metadata block the CLI sends, limited to the
// fields the backend actually needs. The large opaque field 31 the CLI also
// sends was omitted and the backend accepted the request without it.
func DefaultMetadata(apiKey string) *Metadata {
	return &Metadata{
		IDEName:          "devin-cli",
		ExtensionVersion: "3000.11.1",
		APIKey:           apiKey,
		Locale:           "en",
		OS:               platformOS(),
		IDEVersion:       "3000.11.1",
		ExtensionName:    "chisel",
	}
}

func platformOS() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	default:
		return runtime.GOOS
	}
}
