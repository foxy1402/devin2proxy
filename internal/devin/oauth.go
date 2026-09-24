package devin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"devin2proxy/internal/pb"
)

// This file is the sign-in: a PKCE code exchange this proxy performs itself, so
// that a second or third account can be added from the dashboard without the Devin
// CLI being involved at all.
//
// The CLI is not needed because the sign-in has two halves and only the first one
// is interactive. It opens a browser at a URL carrying a PKCE challenge, the person
// signs in and copies the code the page shows, and then the code is exchanged for a
// session token over a plain Connect call. That exchange is what the CLI was doing
// for us; it is one HTTP request, so the proxy can make it directly.
//
// Everything here was established against the real endpoints, not inferred:
//
//   - The exchange is a Connect unary RPC on the same seat-management service the
//     status call already uses:
//     /exa.seat_management_pb.SeatManagementService/ExchangeDevinCLIPKCECode.
//     A GET on that path answers 405 with `allow: POST`, and a nonexistent method
//     on the same service answers 404, so the path is confirmed rather than guessed.
//   - The body is binary protobuf (field 1 code, field 2 code_verifier) with the
//     usual Connect headers. Sending the same body as JSON is rejected by the
//     server with a syntax error naming the first key, while the binary form is
//     parsed and answered with a business error about the code itself — which is
//     how the encoding was settled.
//   - The sign-in URL carries no redirect_uri. The site checks that parameter
//     against a fixed allowlist, and a dashboard path is refused outright with
//     "Invalid redirect URI" once a signed-in browser gets that far — the check
//     happens after authentication, so it is invisible to an unauthenticated
//     fetch and was found only from a real sign-in. The CLI never sends such a
//     URI either: its two builders, both in the binary, are a loopback callback
//     (http://127.0.0.1:<port>/callback, its path fixed by the same allowlist)
//     and no redirect_uri at all. This proxy sends none, which is the CLI's
//     manual flow: the page renders the code for copying. That keeps the flow
//     working from a remote browser, where a loopback callback would land on the
//     wrong machine, and it asks nothing of the site that the CLI does not.
//   - cli_pkce_marker=1 is what makes the page show the code rather than only
//     redirecting, and prompt=select_account is what makes it offer the account
//     picker a second account needs.
//
// Both halves have since been run against the real backend with a real account: the
// URL renders the code in a signed-in browser, and that code came back from the
// exchange as a credential carrying the marker below. The decoder still reads the
// response structurally rather than by a field number, because that costs nothing
// and a response that grows a second string field shows up in the log line naming
// the field it used instead of being resolved silently. An exchange that succeeds
// but yields a token the backend refuses is reported as such rather than stored.

// SessionTokenPrefix is the marker the backend expects in front of a session
// token, and the form every credential in credentials.toml carries.
const SessionTokenPrefix = "devin-session-token$"

// DefaultWebappHost is where the sign-in page lives. It is the host the CLI's own
// manual flow uses; a different deployment is reachable by configuration.
const DefaultWebappHost = "https://app.devin.ai"

// ExchangePath is the seat-management RPC that trades a PKCE code for a session
// token. It sits on the API server, next to the status call.
const ExchangePath = "/exa.seat_management_pb.SeatManagementService/ExchangeDevinCLIPKCECode"

// exchangeTimeout bounds the whole exchange. It is a single small request to a
// service that answers in well under a second, so a long wait means something is
// wrong rather than slow.
const exchangeTimeout = 30 * time.Second

// LoginFlow is one sign-in in progress: the verifier that must stay secret until
// the code comes back, and the URL to send the person to.
type LoginFlow struct {
	// Verifier is the PKCE secret. It never leaves the process it was made in.
	Verifier string
	// State ties the returned code to this flow. It travels through the browser.
	State string
	// URL is the sign-in page to open.
	URL string
}

// NewLoginFlow builds a PKCE flow for the given webapp host.
func NewLoginFlow(webappHost string) (*LoginFlow, error) {
	verifier, err := randomURLSafe(32)
	if err != nil {
		return nil, err
	}
	state, err := randomURLSafe(16)
	if err != nil {
		return nil, err
	}
	signIn, err := SignInURL(webappHost, state, PKCEChallenge(verifier))
	if err != nil {
		return nil, err
	}
	return &LoginFlow{Verifier: verifier, State: state, URL: signIn}, nil
}

// SignInURL builds the authorize URL. It is separated from NewLoginFlow so a test
// can pin the exact query string without generating randomness.
//
// There is no redirect_uri, deliberately: the site only accepts one on a loopback
// callback it has on its allowlist, and answers anything else with "Invalid
// redirect URI". Without it the page shows the code, which is what the operator
// copies back, and the flow works the same from anywhere.
func SignInURL(webappHost, state, challenge string) (string, error) {
	host := strings.TrimSpace(webappHost)
	if host == "" {
		host = DefaultWebappHost
	}
	u, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("devin: sign-in host %q: %w", webappHost, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("devin: sign-in host %q must be an http or https URL", webappHost)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/auth/cli/continue"
	q := u.Query()
	q.Set("state", state)
	// select_account rather than login: adding a second account has to offer the
	// account picker, or the browser silently reuses the account already signed in
	// and the same credential comes back twice.
	q.Set("prompt", "select_account")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	// The marker that makes the page show the code instead of only redirecting.
	q.Set("cli_pkce_marker", "1")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// PKCEChallenge is the S256 challenge for a verifier: the base64url of its SHA-256,
// with no padding, which is what RFC 7636 section 4.2 specifies.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randomURLSafe returns n random bytes as base64url without padding, the alphabet
// PKCE requires. 32 bytes gives the 43-character verifier the spec calls for and
// what the CLI itself uses.
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("devin: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ExchangeDevinCLIPKCECode trades a sign-in code for a session token.
//
// It is deliberately unauthenticated: the whole point is that there is no
// credential yet. It does not go through the egress routes either — those are for
// inference traffic, and a sign-in that silently left through a stranger's proxy
// would be a surprise.
func ExchangeDevinCLIPKCECode(ctx context.Context, apiServerURL, code, verifier string) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", errors.New("devin: no sign-in code was given")
	}
	if verifier == "" {
		return "", errors.New("devin: no PKCE verifier available for this sign-in")
	}
	base := strings.TrimSuffix(strings.TrimSpace(apiServerURL), "/")
	if base == "" {
		base = defaultAPIServerURL
	}

	w := pb.NewWriter()
	w.String(1, code)
	w.String(2, verifier)

	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+ExchangePath, bytes.NewReader(w.Bytes()))
	if err != nil {
		return "", fmt.Errorf("devin: build exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "application/proto, application/json")
	req.ContentLength = int64(len(w.Bytes()))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("devin: exchange request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("devin: read exchange response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", exchangeError(resp, body)
	}
	token, field, err := DecodeExchangeResponse(body)
	if err != nil {
		return "", err
	}
	// The decoder reports the protobuf field it found the token in (0 for a JSON
	// answer, where there is no field number). Anything past the expected 1 means
	// the response grew a string field ahead of the token, and this line is how
	// that becomes visible instead of the proxy quietly trusting the wrong field.
	if field > 1 {
		log.Printf("devin: the exchange token came from response field %d, not the expected 1", field)
	}
	return NormalizeSessionToken(token), nil
}

// exchangeError turns a failed exchange into something a person can act on. The
// Connect error body carries the backend's own sentence, which is the useful part:
// an expired code says so, and re-running the sign-in is the fix.
func exchangeError(resp *http.Response, body []byte) error {
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	detail := strings.TrimSpace(string(body))
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Message != "" {
		detail = envelope.Message
		if envelope.Code != "" {
			detail = envelope.Code + ": " + envelope.Message
		}
	}
	if detail == "" {
		detail = resp.Status
	}
	if len(detail) > 400 {
		detail = detail[:400]
	}
	return fmt.Errorf("devin: the sign-in code was refused (HTTP %d): %s", resp.StatusCode, detail)
}

// DecodeExchangeResponse pulls the session token out of the exchange response.
//
// The response message is small and every field of it is either the token or a
// detail about the account, so rather than hard-code a field number that could not
// be confirmed without a real code, it is read structurally: the token is the
// string field that is prefixed like a session token, or failing that one that
// reads like the JWT these tokens are, or failing that the first non-empty string.
// The field number it settled on is returned so a log line can say which one it
// was — if a future response grows a second string field, that log line is how the
// choice becomes visible instead of silent.
func DecodeExchangeResponse(body []byte) (token string, field int, err error) {
	if len(body) == 0 {
		return "", 0, errors.New("devin: the exchange returned nothing")
	}
	// The body is not trimmed before it is parsed as protobuf, and that matters: the
	// tag byte for field 1 is 0x0A, which is also a newline. Trimming leading
	// whitespace off a protobuf message eats its first field — the token itself —
	// and leaves the rest to be misread as a length. Whitespace is only skipped to
	// peek at whether this is JSON.
	if start := skipSpace(body); start < len(body) && body[start] == '{' {
		return decodeExchangeJSON(body[start:])
	}

	var firstString string
	var firstField int
	r := pb.NewReader(body)
	for {
		f, wire, ok := r.Field()
		if !ok {
			break
		}
		if wire != 2 {
			r.Skip(wire)
			continue
		}
		val := string(r.Bytes())
		if val == "" {
			continue
		}
		if strings.HasPrefix(val, SessionTokenPrefix) {
			return val, f, nil
		}
		if looksLikeJWT(val) && firstString == "" {
			firstString, firstField = val, f
			continue
		}
		if firstString == "" && printable(val) {
			firstString, firstField = val, f
		}
	}
	if err := r.Err(); err != nil {
		return "", 0, fmt.Errorf("devin: the exchange response could not be read: %w", err)
	}
	if firstString == "" {
		return "", 0, errors.New("devin: the exchange returned no session token")
	}
	return firstString, firstField, nil
}

// decodeExchangeJSON reads a token out of a JSON response, for a server that answers
// in Connect's JSON rather than protobuf.
func decodeExchangeJSON(body []byte) (string, int, error) {
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", 0, fmt.Errorf("devin: the exchange response could not be read: %w", err)
	}
	// Ordered by how specific the name is, so a response carrying both a token and
	// an unrelated string still yields the token.
	for _, key := range []string{"sessionToken", "session_token", "apiKey", "api_key", "token"} {
		if v, ok := fields[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), 0, nil
		}
	}
	return "", 0, errors.New("devin: the exchange response carried no session token")
}

// NormalizeSessionToken puts the token in the form the backend authenticates.
//
// The token comes back as a JWT and the marker in front of it is what tells the
// backend which kind of session it is; the CLI writes exactly this form into
// credentials.toml. A token that already carries the marker is left alone, so this
// is safe whichever way the server answers.
func NormalizeSessionToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" || strings.HasPrefix(token, SessionTokenPrefix) {
		return token
	}
	return SessionTokenPrefix + token
}

// skipSpace returns the offset of the first byte that is not ASCII whitespace.
func skipSpace(b []byte) int {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// looksLikeJWT reports whether a value has the shape of the session tokens these
// endpoints return: three base64url segments separated by dots.
func looksLikeJWT(s string) bool {
	parts := strings.Split(s, ".")
	return len(parts) == 3 && len(parts[0]) >= 8 && len(parts[1]) >= 8 && len(parts[2]) >= 8
}

// printable reports whether a value looks like a token rather than binary noise, so
// a protobuf field holding something else is never mistaken for one.
func printable(s string) bool {
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return len(s) >= 16
}
