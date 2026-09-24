package devin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"devin2proxy/internal/pb"
)

// The challenge is the one thing in the sign-in that has an external definition, so
// it is checked against the spec rather than against itself: RFC 7636 appendix B
// gives a verifier and the exact challenge it must produce.
func TestThePKCEChallengeMatchesRFC7636(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := PKCEChallenge(verifier); got != want {
		t.Errorf("PKCEChallenge(%q) = %q, want %q", verifier, got, want)
	}
	// The challenge is the base64url of the verifier's SHA-256 with no padding. Any
	// padding or standard-alphabet character makes the site reject the request with a
	// page that says nothing useful.
	if strings.ContainsAny(want, "+/=") {
		t.Fatal("the reference challenge contains characters base64url must not use")
	}
}

// A verifier has to be 43 characters of base64url: long enough to be a secret, and
// in the alphabet the spec allows.
func TestAGeneratedVerifierIsWellFormed(t *testing.T) {
	flow, err := NewLoginFlow(DefaultWebappHost)
	if err != nil {
		t.Fatalf("NewLoginFlow: %v", err)
	}
	if len(flow.Verifier) != 43 {
		t.Errorf("verifier is %d characters, want 43: %q", len(flow.Verifier), flow.Verifier)
	}
	if strings.ContainsAny(flow.Verifier, "+/=") {
		t.Errorf("verifier %q uses characters outside base64url", flow.Verifier)
	}
	if flow.State == "" || flow.State == flow.Verifier {
		t.Errorf("state %q must be its own value, not the verifier", flow.State)
	}

	// The URL has to carry the challenge for the verifier, not the verifier itself.
	u, err := url.Parse(flow.URL)
	if err != nil {
		t.Fatalf("parse sign-in URL: %v", err)
	}
	q := u.Query()
	if got := q.Get("code_challenge"); got != PKCEChallenge(flow.Verifier) {
		t.Errorf("code_challenge = %q, want the S256 challenge of the verifier", got)
	}
	if strings.Contains(flow.URL, flow.Verifier) {
		t.Error("the sign-in URL contains the verifier; only its challenge may travel")
	}
	if flow.Verifier == "" || q.Get("state") != flow.State {
		t.Errorf("state in the URL is %q, want %q", q.Get("state"), flow.State)
	}
}

// The sign-in URL is what a person's browser is sent to, so every parameter the site
// requires has to be there. select_account matters most: without it the browser
// silently reuses the account already signed in and the same credential comes back,
// which is exactly the failure that makes adding a second account impossible.
//
// redirect_uri must be absent. The site validates it against a fixed allowlist once
// the browser is signed in, and a dashboard callback was refused with "Invalid
// redirect URI" — a dead end for the whole flow. Without the parameter the page
// shows the code, which the operator copies back.
func TestTheSignInURLAsksForTheAccountPickerAndNamesNoCallback(t *testing.T) {
	got, err := SignInURL(DefaultWebappHost, "st4te", "ch4llenge")
	if err != nil {
		t.Fatalf("SignInURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Host != "app.devin.ai" || u.Path != "/auth/cli/continue" {
		t.Errorf("sign-in URL is %s%s, want app.devin.ai/auth/cli/continue", u.Host, u.Path)
	}
	want := map[string]string{
		"prompt":                "select_account",
		"code_challenge":        "ch4llenge",
		"code_challenge_method": "S256",
		"state":                 "st4te",
		// The marker that makes the page show the code for copying; without it there
		// is nothing to paste back.
		"cli_pkce_marker": "1",
	}
	for key, value := range want {
		if got := u.Query().Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if u.Query().Has("redirect_uri") {
		t.Errorf("the sign-in URL carries redirect_uri=%q; the site refuses any it has not "+
			"allowlisted, and the code has to come from the page instead",
			u.Query().Get("redirect_uri"))
	}

	// A host that is not a URL is refused rather than turned into a link to nowhere.
	if _, err := SignInURL("app.devin.ai", "s", "c"); err == nil {
		t.Error("a host with no scheme was accepted")
	}
	// An empty host falls back to the Devin one rather than producing a relative URL.
	fallback, err := SignInURL("", "s", "c")
	if err != nil || !strings.HasPrefix(fallback, DefaultWebappHost) {
		t.Errorf("empty host gave %q (err %v), want the default host", fallback, err)
	}
}

// exchangeStub stands in for the seat-management service, so the exchange can be
// exercised without reaching the network.
func exchangeStub(t *testing.T, status int, body []byte) (*httptest.Server, *[]byte) {
	t.Helper()
	var seen []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ExchangePath {
			t.Errorf("request went to %s, want %s", r.URL.Path, ExchangePath)
		}
		// The exchange is anonymous by nature: there is no credential yet, and one
		// must never be sent.
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("the exchange sent an Authorization header (%q)", auth)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/proto" {
			t.Errorf("content type = %q, want application/proto", ct)
		}
		if v := r.Header.Get("Connect-Protocol-Version"); v != "1" {
			t.Errorf("connect-protocol-version = %q, want 1", v)
		}
		seen = readAll(r)
		w.Header().Set("Content-Type", "application/proto")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func readAll(r *http.Request) []byte {
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 128)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf
		}
	}
}

// testJWT is a session token of the shape these endpoints return: three base64url
// segments. Only the shape matters here; the value is made up.
const testJWT = "eyJhbGciOiJIUzI1NiJ9.eyJzZXNzaW9uX2lkIjoiYWJjIn0.c2lnbmF0dXJl"

// The request body is the part that had to be discovered, so it is pinned byte for
// byte: field 1 the code, field 2 the verifier, binary protobuf.
func TestTheExchangeSendsTheCodeAndVerifierAsProtobuf(t *testing.T) {
	srv, seen := exchangeStub(t, http.StatusOK, encodeToken(testJWT))
	_, err := ExchangeDevinCLIPKCECode(context.Background(), srv.URL, "the-code", "the-verifier")
	if err != nil {
		t.Fatalf("ExchangeDevinCLIPKCECode: %v", err)
	}

	w := pb.NewWriter()
	w.String(1, "the-code")
	w.String(2, "the-verifier")
	want := w.Bytes()
	if got := *seen; string(got) != string(want) {
		t.Errorf("request body = %x, want %x", got, want)
	}
}

// A successful exchange yields a token in the form the backend authenticates. The
// marker in front is what tells the backend which kind of session it is, and it is
// the form the CLI itself writes.
func TestTheExchangeReturnsAUsableCredential(t *testing.T) {
	body := encodeToken("eyJhbGciOiJIUzI1NiJ9.eyJzZXNzaW9uX2lkIjoicyJ9.c2ln")
	srv, _ := exchangeStub(t, http.StatusOK, body)
	token, err := ExchangeDevinCLIPKCECode(context.Background(), srv.URL, "code", "verifier")
	if err != nil {
		t.Fatalf("ExchangeDevinCLIPKCECode: %v", err)
	}
	if !strings.HasPrefix(token, SessionTokenPrefix) {
		t.Fatalf("token = %q, want it prefixed like every credential in credentials.toml", token)
	}
	if got := strings.TrimPrefix(token, SessionTokenPrefix); got != "eyJhbGciOiJIUzI1NiJ9.eyJzZXNzaW9uX2lkIjoicyJ9.c2ln" {
		t.Errorf("token body = %q, want the JWT from the response", got)
	}

	// A token that already carries the marker is not given a second one.
	if got := NormalizeSessionToken(SessionTokenPrefix + "abc"); got != SessionTokenPrefix+"abc" {
		t.Errorf("NormalizeSessionToken doubled the marker: %q", got)
	}
	if got := NormalizeSessionToken(""); got != "" {
		t.Errorf("NormalizeSessionToken(\"\") = %q, want empty", got)
	}
}

// A refused code is the common failure — expired, or already used — and the
// backend's own sentence is what tells the operator to sign in again. Losing it
// behind "HTTP 401" would leave them guessing.
func TestARefusedCodeKeepsTheBackendsOwnMessage(t *testing.T) {
	body := []byte(`{"code":"unauthenticated","message":"Invalid or expired code. The code may have expired or already been used."}`)
	srv, _ := exchangeStub(t, http.StatusUnauthorized, body)
	_, err := ExchangeDevinCLIPKCECode(context.Background(), srv.URL, "stale", "verifier")
	if err == nil {
		t.Fatal("a refused code was reported as success")
	}
	if !strings.Contains(err.Error(), "Invalid or expired code") {
		t.Errorf("error = %v, want it to carry the backend's message", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want it to carry the status", err)
	}
}

// The response field holding the token could not be confirmed without a real code,
// so the decoder reads it structurally. Each of these shapes is one the endpoint
// could plausibly answer with.
func TestTheTokenIsFoundWhateverFieldItArrivesIn(t *testing.T) {
	jwt := testJWT
	cases := []struct {
		name  string
		body  []byte
		want  string
		field int
	}{
		{"field 1 (the expected shape)", encodeToken(jwt), jwt, 1},
		{"field 2, with another string ahead of it", encodeFields("account note", jwt), jwt, 2},
		{"already prefixed", encodeFields(SessionTokenPrefix + jwt), SessionTokenPrefix + jwt, 1},
		{"json, camelCase", []byte(`{"sessionToken":"` + jwt + `"}`), jwt, 0},
		{"json, snake_case with other keys", []byte(`{"email":"a@b.c","session_token":"` + jwt + `"}`), jwt, 0},
		{"json, api_key", []byte(`{"api_key":"` + jwt + `"}`), jwt, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, field, err := DecodeExchangeResponse(tc.body)
			if err != nil {
				t.Fatalf("DecodeExchangeResponse: %v", err)
			}
			if token != tc.want {
				t.Errorf("token = %q, want %q", token, tc.want)
			}
			if field != tc.field {
				t.Errorf("field = %d, want %d", field, tc.field)
			}
		})
	}
}

// A response with nothing usable in it must fail rather than hand an empty string to
// the pool, where it would become an account that fails every request.
func TestAnEmptyExchangeResponseIsAnError(t *testing.T) {
	for _, body := range [][]byte{nil, {}, []byte(`{}`), []byte(`{"sessionToken":"  "}`),
		encodeFields(""), {0x08, 0x01} /* a numeric field, no string at all */} {
		if token, _, err := DecodeExchangeResponse(body); err == nil {
			t.Errorf("body %x was accepted and gave token %q", body, token)
		}
	}
}

// encodeToken builds the response the endpoint is expected to return: the token at
// field 1, which is what the message's single named field suggests.
func encodeToken(token string) []byte { return encodeFields(token) }

func encodeFields(values ...string) []byte {
	w := pb.NewWriter()
	for i, v := range values {
		w.String(i+1, v)
	}
	return w.Bytes()
}

// The verifier is a secret and the code is single-use, so both have to be refused
// before a request is made when they are missing.
func TestTheExchangeRefusesWithoutACodeOrVerifier(t *testing.T) {
	if _, err := ExchangeDevinCLIPKCECode(context.Background(), "http://127.0.0.1:1", "", "v"); err == nil {
		t.Error("an empty code was accepted")
	}
	if _, err := ExchangeDevinCLIPKCECode(context.Background(), "http://127.0.0.1:1", "c", ""); err == nil {
		t.Error("a missing verifier was accepted")
	}
}

// The challenge is only ever the SHA-256 of the verifier, so two different verifiers
// cannot collide into the same challenge by accident of encoding.
func TestDifferentVerifiersGiveDifferentChallenges(t *testing.T) {
	a := PKCEChallenge("verifier-one")
	b := PKCEChallenge("verifier-two")
	if a == b {
		t.Fatal("two verifiers produced the same challenge")
	}
	sum := sha256.Sum256([]byte("verifier-one"))
	if a != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Error("the challenge is not the base64url SHA-256 of the verifier")
	}
}
