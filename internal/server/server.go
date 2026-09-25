// Package server exposes the OpenAI-compatible HTTP surface in front of the
// Devin backend.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/eventlog"
	"devin2proxy/internal/openai"
)

// Config configures the HTTP server.
type Config struct {
	// APIKey gates incoming requests. Empty means every request is refused,
	// which is the safe default for something that spends account quota.
	APIKey string
	// DefaultModel is used when a request names a model we do not recognise.
	DefaultModel string
	// Models is the advertised model list.
	Models []string
	// AllowOrigins is the value for Access-Control-Allow-Origin.
	AllowOrigins string
	// RequestTimeout bounds a single completion.
	RequestTimeout time.Duration
	// MinMaxTokens is the floor applied to a client's max_tokens. See
	// openai.DefaultMinMaxTokens for why it exists.
	MinMaxTokens int
	// MaxToolDescBytes caps each tool description forwarded upstream. See
	// openai.DefaultMaxToolDescBytes for why a cap exists at all. Negative
	// sends descriptions verbatim.
	MaxToolDescBytes int
	// Creds, when set, rotates several accounts, one per request. Leave it nil to
	// use the CLI's own stored credential.
	Creds *devin.Pool
	// Events, when set, receives one event per request for the dashboard's log
	// view. Nil disables the whole observation path.
	Events *eventlog.Hub
	// Dashboard, when set, is mounted under its own prefix with its own
	// authentication. It is a separate surface from the API-key one on purpose: the
	// API key is pasteable into a client, the dashboard password is not.
	Dashboard http.Handler
	// AuthFailureDelay floors how long a wrong key takes to answer. Zero uses
	// authFailureDelay. A test that exercises the 401 path can lower it; nobody
	// on the internet should be able to raise it away, so New clamps it.
	AuthFailureDelay time.Duration
	// CaptureDir, when non-empty, is where every decoded /v1 request body is
	// written as JSON, one file per request. It exists for DEVIN2PROXY_DEBUG_
	// CAPTURE: the exact shape a failing client sent, to replay and bisect
	// offline. Empty (the default) captures nothing.
	CaptureDir string
}

// credentials returns the credential for one request: the next account in the
// pool when one is configured, otherwise the CLI's stored credential. The pool is
// re-consulted per request, so an account that has been cooled down rejoins
// rotation as soon as its cooldown expires.
func (s *Server) credentials() (*devin.Credentials, error) {
	if c := s.cfg.Creds.Next(); c != nil {
		return c, nil
	}
	return devin.LoadCredentials()
}

// getChatMessage issues the upstream request, moving to another account if the
// one it picked is refused.
//
// A refusal arrives as the first frame, before any generation starts, so acting
// on it costs no quota — unlike a mid-stream failure, which cannot be resumed and
// is never retried. This is what keeps one dead or rate-limited account in the
// pool from surfacing as a client-visible error while healthy accounts remain.
//
// The request is built inside the attempt rather than passed in, because the key
// that goes into the request metadata has to be the same account's key as the one
// on the connection. Building it once outside would pair the first account's key
// with whichever account the retry landed on.
//
// Retrying here is for accounts only. A failure of the outbound route is not the
// account's fault and is already retried on another route inside the client, so
// changing account would not help.
func (s *Server) getChatMessage(ctx context.Context, build func(*devin.Credentials) (*devin.GetChatMessageRequest, error)) (*devin.Stream, *devin.Credentials, error) {
	attempts := 1
	if n := s.cfg.Creds.Len(); n > 1 {
		attempts = n
	}

	var lastErr error
	tr := traceFrom(ctx)
	for i := 0; i < attempts; i++ {
		creds, err := s.credentials()
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s", errNoCredential, err)
		}
		// The account is recorded before the attempt, not after: a request that
		// failed is exactly the one whose account an operator wants to see.
		tr.setAccount(creds)
		req, err := build(creds)
		if err != nil {
			// A malformed request fails the same way against every account, so it
			// is the client's problem and never worth a second attempt.
			return nil, nil, &clientError{err}
		}
		stream, err := s.client.GetChatMessage(ctx, creds, req)
		if err == nil {
			// A 200 proves nothing: a refused credential is reported inside the
			// stream, so one frame has to be read before this account can be called
			// good. This costs nothing — the frame is handed back to the caller on
			// its first Recv — and it is the last point at which a retry is free.
			// A stream that ends without any frame at all is a degenerate but
			// successful answer, not a rejected account, so EOF is not an error.
			if perr := stream.Peek(); perr != nil && !errors.Is(perr, io.EOF) {
				err = perr
			}
		}
		if err == nil {
			// Which route carried it, recorded here because the pool rotates per
			// request and the next caller has already been given the next route.
			tr.setRoute(stream.Route)
			return stream, creds, nil
		}
		if stream != nil {
			stream.Close()
		}
		lastErr = err
		tr.fail(err)
		// A request the client abandoned is not recorded against the account: an
		// autocomplete cancel is routine, and cooling the account for it would
		// take a healthy account out of rotation for minutes on every keystroke
		// the user interrupts. The pool checks this too; here it also avoids
		// walking the rest of the pool for a request nobody is waiting for.
		if ctx.Err() != nil {
			return nil, creds, err
		}
		s.cfg.Creds.Report(creds, 0, err)
		if !isAccountRefusal(err) {
			return nil, creds, err
		}
	}
	return nil, nil, lastErr
}

// reportQuotaRefusal records a refusal that arrived after generation had already
// started, so the next request skips that account instead of spending an attempt
// on it.
//
// Only the two codes that mean "no quota left" are recorded, and only when the
// backend is the one that said so. A read error, a client that went away, or a
// stream we cut short at a stop sequence says nothing about the account and is
// never recorded — which is also why this is narrow rather than a general
// mid-stream Report: the first-frame refusal is already handled per attempt, and
// everything after it is far more likely to be the connection than the account.
func (s *Server) reportQuotaRefusal(creds *devin.Credentials, err error) {
	if creds == nil || err == nil {
		return
	}
	var httpErr *devin.HTTPError
	var connectErr *devin.ConnectError
	switch {
	case errors.As(err, &httpErr):
		if httpErr.Status != http.StatusTooManyRequests {
			return
		}
	case errors.As(err, &connectErr):
		if connectErr.Code != "resource_exhausted" {
			return
		}
	default:
		return
	}
	log.Printf("account refused mid-stream with a quota error; recording it so the next "+
		"request skips that account: %v", err)
	s.cfg.Creds.Report(creds, 0, err)
}

// errNoCredential reports that no account could be resolved at all — a server
// misconfiguration rather than anything about the request.
var errNoCredential = errors.New("credential unavailable")

// errBackendStop reports a generation the backend itself marked as failed: a
// frame carried stop_reason ERROR (13). It is distinct from a Connect error —
// the stream opened and produced frames, then gave up partway. Rendering it as
// a clean finish_reason would hand the client a truncated answer that looks
// complete, so every relay path surfaces it as the failure it is.
var errBackendStop = errors.New("devin: the backend stopped the generation with an error")

// clientError wraps a failure caused by the request itself, so a handler can
// answer 400 instead of rendering it as an upstream fault.
type clientError struct{ err error }

func (e *clientError) Error() string { return e.err.Error() }
func (e *clientError) Unwrap() error { return e.err }

// writeUpstreamOrClientError answers a getChatMessage failure with the status the
// cause deserves: 400 for a request the backend would reject anyway, 502 for a
// credential that could not be resolved or a route that could not be used, and
// the mapped upstream status otherwise.
func writeUpstreamOrClientError(w http.ResponseWriter, err error) {
	var ce *clientError
	if errors.As(err, &ce) {
		writeError(w, http.StatusBadRequest, ce.Error(), "invalid_request_error", "")
		return
	}
	if errors.Is(err, errNoCredential) {
		writeError(w, http.StatusBadGateway, err.Error(), "api_error", "no_credential")
		return
	}
	var egErr *devin.EgressError
	if errors.As(err, &egErr) {
		// The account is fine and the request never reached the backend, so this is
		// a routing problem rather than a backend one.
		writeError(w, http.StatusBadGateway,
			"no usable outbound route: "+err.Error()+
				"; check the proxy list and its credentials", "api_error", "egress_unavailable")
		return
	}
	writeUpstreamError(w, err)
}

// isAccountRefusal reports whether the failure is this account being unable to
// serve the request, as opposed to a malformed request or a backend fault. Only
// this class is worth retrying on a different account.
//
// A rate limit belongs here even though the credential is fine. The pool exists
// because the allowance is per account, so an account that is out of quota is
// exactly the case another account answers, and returning the 429 to the client
// while a healthy account sits idle would make the pool pointless for the failure
// it was built for. What must not be retried is a request the backend rejected on
// its own terms (invalid_argument, failed_precondition) or a backend fault, since
// every account would answer those the same way.
func isAccountRefusal(err error) bool {
	var httpErr *devin.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
			return true
		}
		return false
	}
	var connectErr *devin.ConnectError
	if errors.As(err, &connectErr) {
		switch connectErr.Code {
		case "unauthenticated", "permission_denied", "resource_exhausted":
			return true
		}
	}
	return false
}

type Server struct {
	cfg    Config
	client *devin.Client
	mux    *http.ServeMux
	opts   openai.Options
	// modelFallbacks dedupes the "model not advertised" notice. An IDE
	// configured for, say, "gpt-4" would otherwise log the same line on every
	// autocomplete request and bury anything that matters.
	modelFallbacks sync.Map
	// captureWarn reports a broken capture directory once; request capture is
	// a debug aid and must not turn into a log flood.
	captureWarn sync.Once
}

func New(cfg Config, client *devin.Client) *Server {
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = devin.DefaultModel
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Minute
	}
	if cfg.AllowOrigins == "" {
		cfg.AllowOrigins = "*"
	}
	if cfg.AuthFailureDelay == 0 {
		cfg.AuthFailureDelay = authFailureDelay
	}
	s := &Server{
		cfg:    cfg,
		client: client,
		mux:    http.NewServeMux(),
		opts:   openai.Options{MinMaxTokens: cfg.MinMaxTokens, MaxToolDescBytes: cfg.MaxToolDescBytes},
	}
	s.mux.HandleFunc("/v1/chat/completions", s.auth(s.handleChatCompletions))
	s.mux.HandleFunc("/v1/completions", s.auth(s.handleCompletions))
	s.mux.HandleFunc("/v1/models", s.auth(s.handleModels))
	s.mux.HandleFunc("/v1/embeddings", s.auth(s.handleUnsupported("embeddings")))
	s.mux.HandleFunc("/healthz", s.handleHealth)
	if cfg.Dashboard != nil {
		// The dashboard brings its own routes and its own session check, and is
		// deliberately not behind s.auth: its pages are HTML for a browser, and the
		// API key would have to be typed into a form to reach them.
		s.mux.Handle("/dashboard", cfg.Dashboard)
		s.mux.Handle("/dashboard/", cfg.Dashboard)
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The wrapper is outside the mux so a request is measured from the moment it
	// arrives, including the 404s and the authentication failures.
	s.observe(s.mux).ServeHTTP(w, r)
}

// authFailureDelay is a floor on how long a wrong key takes to answer. On a
// public deployment the API key is the only thing between the internet and the
// account's quota, and unlike the dashboard's password it has no ban ladder
// behind it — a client that guesses wrong simply gets a 401 and asks again a
// millisecond later. The delay makes online guessing pointless (a few guesses
// per second at best) while a correct key never pays it, so autocomplete and
// streaming are untouched. The dashboard's login applies the same idea to its
// own surface.
const authFailureDelay = 250 * time.Millisecond

// auth enforces the bearer key. It fails closed: with no key configured nothing
// is served, so a misconfiguration cannot expose the account.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.cfg.AllowOrigins)
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if s.cfg.APIKey == "" {
			writeError(w, http.StatusServiceUnavailable, "server has no API key configured; set DEVIN2PROXY_API_KEY", "configuration_error", "")
			return
		}
		token := bearerToken(r)
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.APIKey)) != 1 {
			time.Sleep(s.cfg.AuthFailureDelay)
			writeError(w, http.StatusUnauthorized, "invalid API key", "authentication_error", "invalid_api_key")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{"status": "ok"}
	switch {
	case s.cfg.Creds.Len() > 0:
		// With a pool, report the account count rather than reading the file: the
		// file may not even exist, since a pool replaces it entirely.
		status["credential"] = "pool"
		status["accounts"] = s.cfg.Creds.Len()
		status["accounts_available"] = s.cfg.Creds.Available()
		if held := s.cfg.Creds.QuotaHeld(); held > 0 {
			status["accounts_quota_held"] = held
		}
	default:
		// /healthz answers without authentication, so it says only whether a
		// credential exists. The error text names local paths (the credentials
		// file), and the backend URL is configuration rather than health — both
		// belong on the dashboard, which guards them with a session.
		if _, err := devin.LoadCredentials(); err != nil {
			status["credential"] = "unavailable"
		} else {
			status["credential"] = "loaded"
		}
	}
	// Routes are reported whether or not a credential pool is in use, since the
	// two are independent.
	if routes := s.client.Egress(); routes.Len() > 0 {
		status["proxies"] = routes.Len()
		status["proxies_available"] = routes.Available()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	list := openai.ModelList{Object: "list"}
	for _, id := range s.cfg.Models {
		list.Data = append(list.Data, openai.Model{
			ID:      id,
			Object:  "model",
			Created: now,
			OwnedBy: "devin",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

// noteModelFallback logs a model substitution once per name, so a chatty client
// cannot flood the log with the same line.
func (s *Server) noteModelFallback(requested, used string) {
	if _, seen := s.modelFallbacks.LoadOrStore(strings.TrimSpace(requested), true); !seen {
		log.Printf("model %q not advertised; using %q (further occurrences are not logged)", requested, used)
	}
}

// warnEmptyAnswer explains the one failure that looks like a bug but is not: the
// backend spent the whole token budget on the model's reasoning, so a successful
// request returns no visible text. Without this the empty response gives no clue
// what to change.
func warnEmptyAnswer(model, endpoint string) {
	log.Printf("%s: %s returned no text with finish_reason \"length\": the token budget was spent on the model's "+
		"reasoning. Raise min_max_tokens (currently the floor in config.json) or the client's max_tokens",
		endpoint, model)
}

// backendModelUIDs are the model identifiers the Devin backend accepts in
// chat_model_uid. Everything else a client asks for is an alias, and an
// unknown uid comes back from the backend as an opaque failed_precondition
// rather than a clear "no such model".
var backendModelUIDs = map[string]bool{
	"swe-1-6-slow": true,
	"swe-1-6-fast": true,
}

// IsBackendModelUID reports whether name is one of the identifiers the backend
// accepts as chat_model_uid. The advertised model list may contain anything —
// aliases are resolved — but the configured default is sent to the backend
// verbatim, so a typo in it is a refusal on every request rather than a
// startup problem, unless main checks it up front.
func IsBackendModelUID(name string) bool {
	return backendModelUIDs[name]
}

// ForwardedModelUIDs lists those identifiers, sorted, for anything outside this
// package that has to explain the same routing: the dashboard's Models panel shows
// each of them with what the catalogue says about it, which is how
// "swe-1-6-fast is refused on this plan" becomes visible before a client hits it.
func ForwardedModelUIDs() []string {
	out := make([]string, 0, len(backendModelUIDs))
	for uid := range backendModelUIDs {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}

// handleUnsupported answers an OpenAI route this proxy cannot serve. Ide-style
// clients probe /v1/embeddings while configuring, and a clear 501 tells them (and
// whoever is reading the logs) that this is a deliberate gap rather than a
// misconfigured URL.
func (s *Server) handleUnsupported(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotImplemented,
			"/v1/"+name+" is not supported by this proxy: the Devin backend exposes no "+name+" capability",
			"invalid_request_error", "not_implemented")
	}
}

// resolveModel maps a requested model name onto a backend model uid. Aliases
// and unknown names fall back to the default rather than failing, because
// clients routinely send whatever model string they were configured with.
func (s *Server) resolveModel(requested string) (uid string, known bool) {
	name := strings.TrimSpace(requested)
	if name == "" {
		return s.cfg.DefaultModel, true
	}
	if backendModelUIDs[name] {
		return name, true
	}
	for _, m := range s.cfg.Models {
		if strings.EqualFold(m, name) {
			return s.cfg.DefaultModel, true
		}
	}
	return s.cfg.DefaultModel, false
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	var req openai.ChatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	s.captureRequest(req, "chat")

	model, known := s.resolveModel(req.Model)
	if !known {
		// Not fatal: fall back, but say so because a silent substitution is
		// otherwise invisible.
		s.noteModelFallback(req.Model, model)
	}
	traceFrom(r.Context()).setModel(model, req.Stream)

	// A client that hangs up mid-stream must stop costing quota, so the upstream
	// request is cancelled the moment the connection goes away.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	stream, creds, err := s.getChatMessage(ctx, func(c *devin.Credentials) (*devin.GetChatMessageRequest, error) {
		built, buildErr := openai.BuildDevinRequest(c.APIKey, &req, model, s.opts)
		if buildErr == nil {
			devin.EmitRequestShape(built)
		}
		return built, buildErr
	})
	if err != nil {
		// A client that has gone away has nothing to read an error status, and
		// writing one would only produce a log line about a broken pipe.
		if ctx.Err() == nil {
			writeUpstreamOrClientError(w, err)
		}
		return
	}
	defer stream.Close()

	stops := openai.ParseStop(req.Stop)
	if req.Stream {
		s.streamChunks(ctx, cancel, w, stream, creds, model, req.StreamOptions, stops)
		return
	}
	s.writeWholeCompletion(ctx, w, stream, creds, model, stops)
}

// sseWriter writes server-sent events and remembers the first write failure.
//
// Tracking the failure is what makes an abort cheap: once the client has gone,
// the handler cancels the upstream request instead of reading a stream nobody
// is listening to. Without this the proxy would keep consuming the account's
// quota until the model finished on its own.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	err     error
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return &sseWriter{w: w, flusher: flusher}, true
}

// json writes one data frame.
func (s *sseWriter) json(v any) {
	if s.err != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.data(string(b))
}

// data writes one already-encoded data frame.
func (s *sseWriter) data(payload string) {
	if s.err != nil {
		return
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		s.err = err
		return
	}
	s.flusher.Flush()
}

func (s *sseWriter) done() { s.data("[DONE]") }

// failed reports whether the client has gone away.
func (s *sseWriter) failed() bool { return s.err != nil }

// streamChunks relays the backend stream as OpenAI SSE.
func (s *Server) streamChunks(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, stream *devin.Stream, creds *devin.Credentials, model string, opts *openai.StreamOptions, stops []string) {
	out, ok := newSSEWriter(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported by server", "api_error", "")
		return
	}

	id := "chatcmpl-" + devin.MustUUID()
	created := time.Now().Unix()

	// OpenAI sends an initial chunk carrying the role.
	out.json(openai.ChatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{Role: "assistant"}}},
	})

	var finish string
	var tools openai.ToolCallAccumulator
	var usage *devin.ModelUsageStats
	filter := openai.NewStopFilter(stops)
	sawText := false
	tr := traceFrom(ctx)

	for {
		if out.failed() {
			// The client hung up; stop paying for this stream.
			log.Printf("client aborted the request; cancelling upstream stream")
			cancel()
			return
		}
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				// The client is gone. The backend request was issued with this
				// context, so cancelling it stops the generation upstream instead
				// of letting it burn account quota to completion.
				log.Printf("client disconnected; cancelled the upstream stream")
				return
			}
			// The response is already committed, so the only way to report a
			// mid-stream failure is in-band, then stop.
			log.Printf("stream error: %v", err)
			tr.fail(err)
			s.reportQuotaRefusal(creds, err)
			out.data(string(openai.MarshalError(err.Error(), "api_error", "upstream_error")))
			out.done()
			return
		}

		if chunk.StopReasonSet {
			if chunk.StopReason == devin.StopError {
				// The backend gave up mid-generation. The response is already
				// committed, so report it in-band like any other mid-stream
				// failure rather than finishing as if the answer were complete.
				log.Printf("backend reported an errored stop; ending the stream with an in-band error")
				tr.fail(errBackendStop)
				out.data(string(openai.MarshalError(errBackendStop.Error(), "api_error", "upstream_error")))
				out.done()
				return
			}
			finish = openai.FinishReason(chunk.StopReason)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if len(chunk.DeltaToolCalls) > 0 {
			// Fragments of one call must keep a single index, so clients
			// reassemble the arguments correctly.
			var deltas []openai.ToolCallDelta
			for _, u := range tools.Add(chunk.DeltaToolCalls) {
				d := openai.ToolCallDelta{
					Index:    u.Index,
					Function: openai.ToolCallFunctionDelta{Name: u.Name, Arguments: u.Arguments},
				}
				if u.Started {
					d.ID = u.ID
					d.Type = "function"
				}
				deltas = append(deltas, d)
			}
			out.json(openai.ChatCompletionChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{ToolCalls: deltas}}},
			})
		}
		if chunk.DeltaThinking != "" {
			out.json(openai.ChatCompletionChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{ReasoningContent: chunk.DeltaThinking}}},
			})
		}
		if text := filter.Write(chunk.DeltaText); text != "" {
			sawText = true
			out.json(openai.ChatCompletionChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{Content: text}}},
			})
		}
		if filter.Truncated() {
			// A stop sequence means the answer is already complete, so stop here
			// rather than paying for tokens that will be discarded. The deferred
			// Close aborts the upstream request.
			break
		}
	}

	if tail := filter.Flush(); tail != "" {
		sawText = true
		out.json(openai.ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{Content: tail}}},
		})
	}

	if filter.Truncated() {
		finish = "stop"
	} else if finish == "" {
		if tools.Len() > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	// A backend that delivers tool calls and then stops normally still produced
	// a tool call: clients branch on finish_reason to decide whether to execute,
	// and "stop" alongside a tool_calls payload would make them skip it. Not so
	// when a stop sequence truncated the answer — the arguments are incomplete,
	// and "stop" is the honest report.
	if tools.Len() > 0 && finish == "stop" && !filter.Truncated() {
		finish = "tool_calls"
	}
	if tools.Len() == 0 && !sawText && finish == "length" {
		warnEmptyAnswer(model, "chat.completions")
	}
	tr.setFinish(finish)
	if u := openai.UsageFromStats(usage); u != nil {
		tr.setTokens(u.PromptTokens, u.CompletionTokens)
	}
	out.json(openai.ChatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []openai.ChunkChoice{{Index: 0, Delta: openai.Delta{}, FinishReason: &finish}},
	})

	if opts != nil && opts.IncludeUsage {
		out.json(openai.ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []openai.ChunkChoice{}, Usage: openai.UsageFromStats(usage),
		})
	}

	out.done()
}

// writeWholeCompletion aggregates the stream into a single response.
func (s *Server) writeWholeCompletion(ctx context.Context, w http.ResponseWriter, stream *devin.Stream, creds *devin.Credentials, model string, stops []string) {
	var sb, reasoning strings.Builder
	var tools openai.ToolCallAccumulator
	var usage *devin.ModelUsageStats
	finish := ""
	filter := openai.NewStopFilter(stops)
	sawText := false
	tr := traceFrom(ctx)

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			tr.fail(err)
			s.reportQuotaRefusal(creds, err)
			writeUpstreamError(w, err)
			return
		}
		if chunk.StopReasonSet {
			if chunk.StopReason == devin.StopError {
				// A partial answer presented as a finished one. The response has
				// not been committed yet, so this can still be a real error.
				tr.fail(errBackendStop)
				writeUpstreamError(w, errBackendStop)
				return
			}
			finish = openai.FinishReason(chunk.StopReason)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if text := filter.Write(chunk.DeltaText); text != "" {
			sawText = true
			sb.WriteString(text)
		}
		reasoning.WriteString(chunk.DeltaThinking)
		if len(chunk.DeltaToolCalls) > 0 {
			tools.Add(chunk.DeltaToolCalls)
		}
		if filter.Truncated() {
			break
		}
	}

	if tail := filter.Flush(); tail != "" {
		sawText = true
		sb.WriteString(tail)
	}

	if filter.Truncated() {
		finish = "stop"
	} else if finish == "" {
		if tools.Len() > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	// Same rule as the streaming path: a delivered tool call with a normal stop
	// is still a tool call, and "stop" would make the client skip executing it.
	if tools.Len() > 0 && finish == "stop" && !filter.Truncated() {
		finish = "tool_calls"
	}
	if tools.Len() == 0 && !sawText && finish == "length" {
		warnEmptyAnswer(model, "chat.completions")
	}
	tr.setFinish(finish)
	// The streaming path records usage the same way; without this the log view
	// showed no token counts for non-streaming completions, which is most of
	// what an operator wants to see per request.
	if u := openai.UsageFromStats(usage); u != nil {
		tr.setTokens(u.PromptTokens, u.CompletionTokens)
	}

	resp := openai.ChatCompletion{
		ID:      "chatcmpl-" + devin.MustUUID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openai.Choice{{
			Index: 0,
			Message: openai.ResponseMessage{
				Role:             "assistant",
				Content:          sb.String(),
				ReasoningContent: reasoning.String(),
				ToolCalls:        tools.Calls(),
			},
			FinishReason: finish,
		}},
		Usage: openai.UsageFromStats(usage),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// handleCompletions serves the legacy text-completion endpoint, which is what
// coding tools use for inline autocomplete. It supports streaming, fill-in-the-
// middle via `suffix`, `stop`, and `echo`, and reuses the chat translation by
// wrapping the prompt as a single turn.
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}
	var legacy openai.CompletionRequest
	if err := decodeJSON(r, &legacy); err != nil {
		writeDecodeError(w, err)
		return
	}
	s.captureRequest(legacy, "completions")

	chat, err := openai.BuildFIMRequest(&legacy)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return
	}
	prompt, _ := legacy.PromptText()
	stops := openai.ParseStop(legacy.Stop)

	model, known := s.resolveModel(chat.Model)
	if !known {
		s.noteModelFallback(chat.Model, model)
	}
	traceFrom(r.Context()).setModel(model, chat.Stream)
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	stream, creds, err := s.getChatMessage(ctx, func(c *devin.Credentials) (*devin.GetChatMessageRequest, error) {
		built, buildErr := openai.BuildDevinRequest(c.APIKey, chat, model, s.opts)
		if buildErr == nil {
			devin.EmitRequestShape(built)
		}
		return built, buildErr
	})
	if err != nil {
		if ctx.Err() == nil {
			writeUpstreamOrClientError(w, err)
		}
		return
	}
	defer stream.Close()

	id := "cmpl-" + devin.MustUUID()
	created := time.Now().Unix()

	if chat.Stream {
		s.streamCompletion(ctx, cancel, w, stream, creds, id, created, model, prompt, legacy.Echo, stops, legacy.StreamOptions)
		return
	}

	filter := openai.NewStopFilter(stops)
	var sb strings.Builder
	var usage *devin.ModelUsageStats
	finish := ""
	tr := traceFrom(ctx)
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			tr.fail(err)
			s.reportQuotaRefusal(creds, err)
			writeUpstreamError(w, err)
			return
		}
		if chunk.StopReasonSet {
			if chunk.StopReason == devin.StopError {
				tr.fail(errBackendStop)
				writeUpstreamError(w, errBackendStop)
				return
			}
			finish = openai.FinishReason(chunk.StopReason)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		sb.WriteString(filter.Write(chunk.DeltaText))
		if filter.Truncated() {
			break
		}
	}
	sb.WriteString(filter.Flush())
	if filter.Truncated() {
		finish = "stop"
	} else if finish == "" {
		finish = "stop"
	}
	if sb.Len() == 0 && finish == "length" {
		warnEmptyAnswer(model, "completions")
	}

	text := sb.String()
	if legacy.Echo {
		text = prompt + text
	}
	tr.setFinish(finish)
	if u := openai.UsageFromStats(usage); u != nil {
		tr.setTokens(u.PromptTokens, u.CompletionTokens)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(openai.Completion{
		ID:      id,
		Object:  "text_completion",
		Created: created,
		Model:   model,
		Choices: []openai.CompletionChoice{{Index: 0, Text: text, FinishReason: finish}},
		Usage:   openai.UsageFromStats(usage),
	})
}

// streamCompletion relays the backend stream as legacy text-completion SSE.
// Note the shape difference from chat: the choice carries `text` directly, with
// no `delta` wrapper.
func (s *Server) streamCompletion(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, stream *devin.Stream, creds *devin.Credentials, id string, created int64, model, prompt string, echo bool, stops []string, opts *openai.StreamOptions) {
	out, ok := newSSEWriter(w)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported by server", "api_error", "")
		return
	}

	send := func(text string, finish *string) {
		out.json(openai.CompletionChunk{
			ID: id, Object: "text_completion", Created: created, Model: model,
			Choices: []openai.CompletionChunkChoice{{Text: text, Index: 0, FinishReason: finish}},
		})
	}

	if echo {
		send(prompt, nil)
	}

	filter := openai.NewStopFilter(stops)
	var usage *devin.ModelUsageStats
	finish := ""
	sawText := false
	tr := traceFrom(ctx)

	for {
		if out.failed() {
			log.Printf("client aborted the request; cancelled the upstream stream")
			cancel()
			return
		}
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				log.Printf("client disconnected; cancelled the upstream stream")
				return
			}
			log.Printf("completion stream error: %v", err)
			tr.fail(err)
			s.reportQuotaRefusal(creds, err)
			out.data(string(openai.MarshalError(err.Error(), "api_error", "upstream_error")))
			out.done()
			return
		}
		if chunk.StopReasonSet {
			if chunk.StopReason == devin.StopError {
				log.Printf("backend reported an errored stop; ending the completion stream with an in-band error")
				tr.fail(errBackendStop)
				out.data(string(openai.MarshalError(errBackendStop.Error(), "api_error", "upstream_error")))
				out.done()
				return
			}
			finish = openai.FinishReason(chunk.StopReason)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if text := filter.Write(chunk.DeltaText); text != "" {
			sawText = true
			send(text, nil)
		}
		if filter.Truncated() {
			break
		}
	}

	if tail := filter.Flush(); tail != "" {
		sawText = true
		send(tail, nil)
	}
	if filter.Truncated() || finish == "" {
		finish = "stop"
	}
	if !sawText && finish == "length" {
		warnEmptyAnswer(model, "completions")
	}
	tr.setFinish(finish)
	if u := openai.UsageFromStats(usage); u != nil {
		tr.setTokens(u.PromptTokens, u.CompletionTokens)
	}
	send("", &finish)

	if opts != nil && opts.IncludeUsage {
		out.json(openai.CompletionChunk{
			ID: id, Object: "text_completion", Created: created, Model: model,
			Choices: []openai.CompletionChunkChoice{}, Usage: openai.UsageFromStats(usage),
		})
	}

	out.done()
}

// maxRequestBodyBytes caps a request body. 8 MiB is far more than any real
// completion request; without the cap a huge paste would be buffered whole.
const maxRequestBodyBytes = 8 << 20

// errBodyTooLarge reports a body over the cap, so the handler can answer 413
// instead of the confusing JSON parse error a truncated document produces.
var errBodyTooLarge = errors.New("request body too large")

// captureRequest writes the decoded request body to CaptureDir, one file per
// request, when DEVIN2PROXY_DEBUG_CAPTURE turned capture on. It is the exact
// shape the client sent — what translate will consume — so a request the
// backend refuses can be replayed and bisected offline. Best effort: a
// capture failure must never fail the request, and an unwritable directory
// is announced once rather than per request.
func (s *Server) captureRequest(v any, kind string) {
	if s.cfg.CaptureDir == "" {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	name := fmt.Sprintf("capture-%s-%d.json", kind, time.Now().UnixNano())
	path := filepath.Join(s.cfg.CaptureDir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		s.captureWarnOnce(path, err)
		return
	}
	log.Printf("captured %s request: %s", kind, name)
}

func (s *Server) captureWarnOnce(path string, err error) {
	s.captureWarn.Do(func() {
		log.Printf("request capture: could not write %s: %v (further capture failures stay quiet)", path, err)
	})
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	// One byte past the cap is read so "at the limit" and "over it" are
	// distinguishable.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxRequestBodyBytes {
		return errBodyTooLarge
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	// A second value after the first used to be ignored: `{...} {...}` decoded
	// as the first object alone, and the client never learned its second
	// document was dropped.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid JSON body: unexpected data after the top-level value")
	}
	return nil
}

// writeDecodeError answers a decodeJSON failure with the status the cause
// deserves: 413 for a body over the cap, 400 for anything malformed.
func writeDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error", "body_too_large")
		return
	}
	writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
}

func writeError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(openai.MarshalError(message, errType, code))
}

// writeUpstreamError maps a backend failure onto a useful HTTP status.
func writeUpstreamError(w http.ResponseWriter, err error) {
	var httpErr *devin.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			writeError(w, http.StatusBadGateway,
				"Devin rejected the credential; run `devin auth login` to refresh it", "api_error", "credential_rejected")
			return
		case http.StatusTooManyRequests:
			writeError(w, http.StatusTooManyRequests, "Devin rate limit reached: "+httpErr.Error(), "rate_limit_error", "rate_limit_exceeded")
			return
		}
		writeError(w, http.StatusBadGateway, httpErr.Error(), "api_error", "upstream_error")
		return
	}

	var connectErr *devin.ConnectError
	if errors.As(err, &connectErr) {
		switch connectErr.Code {
		case "unauthenticated", "permission_denied":
			writeError(w, http.StatusBadGateway,
				"Devin rejected the credential: "+connectErr.Message, "api_error", "credential_rejected")
		case "invalid_argument":
			writeError(w, http.StatusBadRequest, connectErr.Message, "invalid_request_error", "invalid_argument")
		case "resource_exhausted":
			writeError(w, http.StatusTooManyRequests, connectErr.Message, "rate_limit_error", "rate_limit_exceeded")
		case "failed_precondition":
			// The backend reports an unsupported model, an exhausted plan, or a
			// rejected field combination as this one opaque code.
			writeError(w, http.StatusBadGateway,
				"Devin refused the request (failed_precondition): "+connectErr.Message+
					"; this usually means the account's plan does not serve the requested model",
				"api_error", "model_unavailable")
		default:
			writeError(w, http.StatusBadGateway, connectErr.Error(), "api_error", "upstream_error")
		}
		return
	}

	writeError(w, http.StatusBadGateway, err.Error(), "api_error", "upstream_error")
}
