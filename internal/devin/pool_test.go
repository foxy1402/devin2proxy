package devin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tokenAt(i int) string {
	return fmt.Sprintf("devin-session-token$jwt-%d", i)
}

func poolOf(n int) *Pool {
	tokens := make([]string, n)
	for i := range tokens {
		tokens[i] = tokenAt(i)
	}
	return NewPool(tokens, "")
}

// rotationIs checks that n consecutive Next calls hand out the credentials in the
// order given, without depending on which one the pool happens to start from.
func rotationIs(t *testing.T, p *Pool, want ...string) {
	t.Helper()
	first := p.Next()
	if first == nil {
		t.Fatal("Next returned nil")
	}
	got := []string{first.APIKey}
	for range want[1:] {
		got = append(got, p.Next().APIKey)
	}
	// A pure rotation is a cyclic shift, so compare against the rotation of want
	// that starts at got[0].
	start := -1
	for i, w := range want {
		if w == got[0] {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("Next returned an unexpected credential: %v", got)
	}
	for i := range got {
		if got[i] != want[(start+i)%len(want)] {
			t.Fatalf("rotation order = %v, want the rotation of %v starting at %q", got, want, got[0])
		}
	}
}

func TestPoolRotatesOnePerRequest(t *testing.T) {
	// The user's requirement: switch account after a single request, so N accounts
	// carry N times the per-account limit.
	p := poolOf(3)
	if got := p.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3", got)
	}
	if got := p.Available(); got != 3 {
		t.Fatalf("Available = %d, want 3", got)
	}
	rotationIs(t, p, tokenAt(0), tokenAt(1), tokenAt(2), tokenAt(0), tokenAt(1), tokenAt(2))
}

func TestPoolSingleAccountIsStable(t *testing.T) {
	p := poolOf(1)
	for i := 0; i < 3; i++ {
		if got := p.Next(); got == nil || got.APIKey != tokenAt(0) {
			t.Fatalf("call %d: Next = %v, want the only token", i, got)
		}
	}
}

func TestPoolEmptyAndNil(t *testing.T) {
	var nilPool *Pool
	if got := nilPool.Len(); got != 0 {
		t.Fatalf("nil Len = %d, want 0", got)
	}
	if got := nilPool.Next(); got != nil {
		t.Fatalf("nil Next = %v, want nil", got)
	}
	if got := nilPool.Available(); got != 0 {
		t.Fatalf("nil Available = %d, want 0", got)
	}
	// Report on a nil pool must not panic: every call site is a single line and
	// none of them checks.
	nilPool.Report(&Credentials{APIKey: tokenAt(0)}, 401, nil)

	if got := NewPool([]string{"", "   ", "# comment only"}, "").Next(); got != nil {
		t.Fatalf("blank-only pool returned %v, want nil", got)
	}
}

func TestPoolSkipsCoolingAccount(t *testing.T) {
	p := poolOf(3)
	p.Report(p.entries[1], 401, nil)

	if got := p.Available(); got != 2 {
		t.Fatalf("Available after one rejection = %d, want 2", got)
	}
	// The rejected account must not come up again across a full cycle.
	for i := 0; i < 4; i++ {
		if got := p.Next(); got.APIKey == tokenAt(1) {
			t.Fatalf("call %d: cooling account returned to rotation early", i)
		}
	}
}

func TestPoolReclaimsAccountAfterCooldown(t *testing.T) {
	p := poolOf(2)
	p.Report(p.entries[1], 429, nil)

	// Expire the cooldown behind the pool's back; the same effect a real 30s wait
	// would have, without the 30s.
	p.rot.cooling[1] = time.Now().Add(-time.Second)

	if got := p.Available(); got != 2 {
		t.Fatalf("Available after cooldown expired = %d, want 2", got)
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		seen[p.Next().APIKey] = true
	}
	if !seen[tokenAt(1)] {
		t.Fatal("recovered account never returned to rotation")
	}
}

func TestPoolServesFromAllCoolingPool(t *testing.T) {
	// Failing the request outright would be worse than retrying a credential that
	// may already have recovered, so the soonest-recovering entry is served.
	p := poolOf(2)
	p.Report(p.entries[0], 401, nil)
	p.Report(p.entries[1], 429, nil)

	if got := p.Available(); got != 0 {
		t.Fatalf("Available with everything cooling = %d, want 0", got)
	}
	if got := p.Next(); got == nil {
		t.Fatal("Next returned nil while every entry was cooling; want a best-effort pick")
	}
	// The rate-limited account recovers sooner, so it is the better bet.
	p2 := poolOf(2)
	p2.Report(p2.entries[0], 401, nil) // 5 minutes
	p2.Report(p2.entries[1], 429, nil) // 30 seconds
	if got := p2.Next(); got.APIKey != tokenAt(1) {
		t.Fatalf("picked %q, want the soonest-recovering account", got.APIKey)
	}
}

func TestPoolReportIgnoresForeignCredential(t *testing.T) {
	// A credential loaded from the CLI file carries poolIndex -1, and one built by
	// hand carries 0 — which would otherwise look like slot 0 and knock a healthy
	// account out of rotation.
	for _, creds := range []*Credentials{
		{APIKey: tokenAt(0), poolIndex: -1},
		{APIKey: tokenAt(0)},
		{APIKey: tokenAt(0), poolIndex: 99},
	} {
		p := poolOf(2)
		p.Report(creds, 401, nil)
		if got := p.Available(); got != 2 {
			t.Fatalf("poolIndex %d: foreign credential cooled a healthy account (%d available)", creds.poolIndex, got)
		}
	}
	// The same index with a different credential is also not a match.
	p := poolOf(2)
	p.Report(&Credentials{APIKey: tokenAt(0), poolIndex: 0}, 401, nil)
	if got := p.Available(); got != 2 {
		t.Fatalf("wrong credential at a valid index cooled slot 0 (%d available)", got)
	}
}

func TestPoolReportClassification(t *testing.T) {
	// Only an HTTP status says anything about the credential. A transport failure
	// or a backend fault must leave the pool untouched, or a flaky network would
	// drain every account out of rotation.
	cases := []struct {
		name       string
		status     int
		err        error
		wantCooled int
	}{
		{"401 cools", 0, &HTTPError{Status: 401, Body: []byte("nope")}, 1},
		{"403 cools", 0, &HTTPError{Status: 403, Body: []byte("nope")}, 1},
		{"429 cools briefly", 0, &HTTPError{Status: 429}, 1},
		{"500 does not cool", 0, &HTTPError{Status: 500, Body: []byte("backend")}, 0},
		{"transport error does not cool", 0, errors.New("dial tcp: connection refused"), 0},
		{"connect internal does not cool", 0, &ConnectError{Code: "internal", Message: "boom"}, 0},
		{"connect unauthenticated cools", 0, &ConnectError{Code: "unauthenticated", Message: "bad key"}, 1},
		{"connect resource_exhausted cools", 0, &ConnectError{Code: "resource_exhausted", Message: "slow down"}, 1},
		// A client that aborts an autocomplete request is routine, not a fault of
		// the account it happened to be using. Cooling for it would take a healthy
		// account out of rotation for five minutes every time the user types over a
		// suggestion.
		{"client abort does not cool", 0, context.Canceled, 0},
		{"client abort wrapped does not cool", 0, fmt.Errorf("devin: upstream request: %w", context.Canceled), 0},
		{"request timeout does not cool", 0, context.DeadlineExceeded, 0},
		{"wrapped deadline does not cool", 0, fmt.Errorf("read stream: %w", context.DeadlineExceeded), 0},
		{"os deadline does not cool", 0, os.ErrDeadlineExceeded, 0},
		// An HTTP status can never be the reason for a cancel, but a cancel can
		// arrive wrapped inside a refusal by a proxy in front of the backend.
		{"abort inside an upstream error does not cool", 0, &ConnectError{
			Code: "unavailable", Message: "proxy: " + context.Canceled.Error(),
		}, 0},
		// The rest of the request matrix a coding IDE produces. A backend fault, a
		// malformed request and a request the backend timed out on all say something
		// about the moment, not about the credential.
		{"503 does not cool", 0, &HTTPError{Status: 503, Body: []byte("down")}, 0},
		{"client-closed does not cool", 0, &HTTPError{Status: 499}, 0},
		{"connect invalid_argument does not cool", 0, &ConnectError{Code: "invalid_argument"}, 0},
		{"connect failed_precondition does not cool", 0, &ConnectError{Code: "failed_precondition"}, 0},
		{"connect deadline_exceeded does not cool", 0, &ConnectError{Code: "deadline_exceeded"}, 0},
		{"connect unavailable does not cool", 0, &ConnectError{Code: "unavailable"}, 0},
		{"connect permission_denied cools", 0, &ConnectError{Code: "permission_denied"}, 1},
		{"a route that could not be dialled does not cool the account", 0,
			&EgressError{Op: "dial", Err: errors.New("socks5: connection refused"), Broken: true}, 0},
		{"an abort reported alongside a route does not cool", 0,
			&EgressError{Op: "roundtrip", Err: context.Canceled}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := poolOf(2)
			p.Report(p.entries[0], tc.status, tc.err)
			if got := p.Available(); got != 2-tc.wantCooled {
				t.Fatalf("Available = %d, want %d", got, 2-tc.wantCooled)
			}
		})
	}
}

func TestPoolReportUsesStatusArgumentWithoutError(t *testing.T) {
	// The Connect path never carries an HTTPError, so the handler passes the
	// status it derived from the Connect error code.
	p := poolOf(1)
	p.Report(p.entries[0], 429, nil)
	if got := p.Available(); got != 0 {
		t.Fatalf("Available = %d, want 0 after a 429 reported without an error", got)
	}
}

func TestPoolLogsOneLinePerRefusalEpisode(t *testing.T) {
	// Concurrent requests fail on the same account at once, so duplicates must not
	// each log a line — but an account that recovers and is refused again is a new
	// episode and must be visible, which is why the dedupe is transient rather
	// than permanent.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	p := poolOf(2)
	for i := 0; i < 3; i++ {
		p.Report(p.entries[0], 401, nil)
	}
	if got := strings.Count(buf.String(), "stepped out"); got != 1 {
		t.Fatalf("repeated reports produced %d lines, want 1:\n%s", got, buf.String())
	}

	p.rot.cooling[0] = time.Now().Add(-time.Second)
	p.Report(p.entries[0], 401, nil)
	if got := strings.Count(buf.String(), "stepped out"); got != 2 {
		t.Fatalf("refusal after recovery produced %d lines in total, want 2:\n%s", got, buf.String())
	}
}

func TestCleanToken(t *testing.T) {
	cases := map[string]string{
		"devin-session-token$abc":      "devin-session-token$abc",
		"  devin-session-token$abc  ":  "devin-session-token$abc",
		`"devin-session-token$abc"`:    "devin-session-token$abc",
		"devin-session-token$abc # me": "devin-session-token$abc",
		"# whole line comment":         "",
		"":                             "",
	}
	for in, want := range cases {
		if got := cleanToken(in); got != want {
			t.Errorf("cleanToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTokensFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.txt")
	body := "# accounts for the free tier\n" +
		tokenAt(0) + "\n" +
		"\n" +
		"  " + tokenAt(1) + "  \n" +
		tokenAt(2) + " # second machine\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := TokensFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{tokenAt(0), tokenAt(1), tokenAt(2)}
	if len(got) != len(want) {
		t.Fatalf("got %d tokens %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token %d = %q, want %q", i, got[i], want[i])
		}
	}

	if _, err := TokensFromFile(filepath.Join(dir, "missing.txt")); err == nil {
		t.Fatal("missing file returned no error")
	}
}

func TestNewPoolAppliesAPIServerURL(t *testing.T) {
	p := NewPool([]string{tokenAt(0)}, "http://127.0.0.1:8787")
	if got := p.Next().APIServerURL; got != "http://127.0.0.1:8787" {
		t.Fatalf("APIServerURL = %q, want the override", got)
	}
	// Empty falls back to the public backend, matching LoadCredentials.
	if got := poolOf(1).Next().APIServerURL; got != defaultAPIServerURL {
		t.Fatalf("APIServerURL = %q, want %q", got, defaultAPIServerURL)
	}
}

func TestPoolSurvivesARetryStormOfCancels(t *testing.T) {
	// A coding IDE interrupts constantly: a keystroke supersedes an autocomplete, a
	// user hits stop, a tool loop times out, a window closes mid-stream, a proxy
	// drops the connection. None of that is the account's doing, so none of it may
	// move the pool. This deserves a test of its own rather than one more row in the
	// table above, because it is the shape the pool sees most often in practice and
	// the damage would be cumulative: a pool that benched a key per interruption
	// would be empty within minutes of ordinary typing.
	p := poolOf(3)
	interruptions := []error{
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("devin: reading stream: %w", context.Canceled),
		os.ErrDeadlineExceeded,
		&EgressError{Op: "roundtrip", Err: context.Canceled},
		&HTTPError{Status: 499, Body: []byte("client closed request")},
	}
	for i := 0; i < 60; i++ {
		p.Report(p.Next(), 0, interruptions[i%len(interruptions)])
	}

	if got := p.Available(); got != 3 {
		t.Fatalf("Available = %d, want 3: no interruption may take an account out of rotation", got)
	}
	for i := range p.entries {
		if until := coolingUntilFor(p, i); !until.IsZero() {
			t.Fatalf("account %d was benched until %s by client-side interruptions", i, until)
		}
	}
}

// The crash this pins: remove the slot the cursor points at while every account
// is cooling, and the next request panics. The walk in pick mods its index so a
// stranded position survives it, but the all-cooling fallback indexes
// cooling[next] directly — and removing the last slot with next pointing at it
// is exactly what strands it.
func TestPoolRemoveOfTheCursorsSlotWhileAllAreCooling(t *testing.T) {
	p := poolOf(2)
	p.Next() // advance the cursor to slot 1
	p.Report(p.entries[0], 401, nil)
	p.Report(p.entries[1], 401, nil)
	if got := p.Available(); got != 0 {
		t.Fatalf("Available = %d before the removal, want 0", got)
	}
	if !p.Remove(1) {
		t.Fatal("Remove(1) failed")
	}
	if got := p.Next(); got == nil || got.APIKey != tokenAt(0) {
		t.Fatalf("Next = %v, want the surviving account rather than a panic", got)
	}
}

func TestRemoveSlotKeepsTheWalkPositionInRange(t *testing.T) {
	c := newRotationCursor(3)
	c.next = 2
	c.removeSlot(2) // the slot next pointed at, and the last one
	if c.next >= len(c.cooling) {
		t.Fatalf("next = %d after removing the last slot of %d", c.next, len(c.cooling))
	}
	// The fallback path is what crashed: cool every slot and pick.
	now := time.Now()
	for i := range c.cooling {
		c.cooling[i] = now.Add(time.Minute)
	}
	if idx := c.pick(now); idx < 0 || idx >= len(c.cooling) {
		t.Fatalf("pick returned %d with %d slots", idx, len(c.cooling))
	}
	// Draining to empty must not leave a position either.
	c2 := newRotationCursor(1)
	c2.removeSlot(0)
	if c2.next != 0 {
		t.Fatalf("next = %d on an empty cursor", c2.next)
	}
	if idx := c2.pick(time.Now()); idx != -1 {
		t.Fatalf("pick on an empty cursor returned %d, want -1", idx)
	}
}

func TestClearCooldownLiftsBothHoldsOnOneAccount(t *testing.T) {
	// A successful probe is direct evidence about one account, so it may lift what
	// a guess put on that account — and only that account. The others are held on
	// the same kind of evidence, or on none, and must be left alone.
	p := poolOf(3)
	p.Report(p.entries[0], 429, nil)
	p.Report(p.entries[1], 401, nil)
	p.applyQuota(p.entries[0], statusAt(time.Now().Add(4*time.Hour), 0))

	cleared := p.ClearCooldown(p.entries[0])
	if len(cleared) != 2 {
		t.Fatalf("cleared = %v, want both the rotation cooldown and the quota hold", cleared)
	}
	if until := coolingUntilFor(p, 0); !until.IsZero() {
		t.Fatalf("account 0 is still cooling until %s", until)
	}
	if got := p.Available(); got != 2 {
		t.Fatalf("Available = %d, want 2: account 0 is back and account 1 is still held", got)
	}
	if until := coolingUntilFor(p, 1); until.IsZero() {
		t.Fatal("clearing account 0 also cleared account 1")
	}
}

func TestClearCooldownReportsOnlyWhatItLifted(t *testing.T) {
	// The endpoint tells the operator what changed, so a healthy account must not
	// be described as having had something lifted from it.
	p := poolOf(2)
	if got := p.ClearCooldown(p.entries[0]); got != nil {
		t.Fatalf("cleared = %v, want none for an account that was not cooling", got)
	}

	// A lapsed cooldown is not a cooldown: the account is already back in rotation.
	p.Report(p.entries[0], 429, nil)
	p.rot.mu.Lock()
	p.rot.cooling[0] = time.Now().Add(-time.Second)
	p.rot.mu.Unlock()
	if got := p.ClearCooldown(p.entries[0]); got != nil {
		t.Fatalf("cleared = %v, want none for a cooldown that had already lapsed", got)
	}
}

func TestClearCooldownIgnoresAnAccountItDoesNotHold(t *testing.T) {
	// The dashboard resolves the account before a probe that can take ninety
	// seconds; by the time the answer arrives the pool may have changed. What
	// reaches ClearCooldown is then a foreign credential, a nil one, or one the
	// pool has dropped — none may panic, and none may clear.
	p := poolOf(2)
	p.Report(p.entries[0], 401, nil)

	foreign := &Credentials{APIKey: tokenAt(9), poolIndex: 0}
	if got := p.ClearCooldown(foreign); got != nil {
		t.Fatalf("ClearCooldown(foreign) = %v, want none", got)
	}
	if got := p.ClearCooldown(nil); got != nil {
		t.Fatalf("ClearCooldown(nil) = %v, want none", got)
	}
	removed := p.entries[1]
	p.Remove(1)
	if got := p.ClearCooldown(removed); got != nil {
		t.Fatalf("ClearCooldown(dropped account) = %v, want none", got)
	}
	if until := coolingUntilFor(p, 0); until.IsZero() {
		t.Fatal("a no-op clear lifted account 0's cooldown")
	}
}

// The stale-index race the credential-keyed API exists to close: a status fetch
// for account 0 is in flight, the operator deletes account 0, and the answer
// lands. Acting on the captured index would extend the cooldown of whoever now
// occupies slot 0 — the survivor gets benched on evidence about an account that
// is gone.
func TestLateQuotaEvidenceAboutARemovedAccountTouchesNobody(t *testing.T) {
	p := poolOf(2)
	p.Report(p.entries[0], 429, nil)
	removed := p.entries[0]
	if !p.Remove(0) {
		t.Fatal("Remove(0) failed")
	}
	if got := p.Available(); got != 1 {
		t.Fatalf("Available = %d after the removal, want the survivor warm", got)
	}

	p.applyQuota(removed, statusAt(time.Now().Add(4*time.Hour), 0))
	p.Remember(removed, &AccountStatus{Email: "gone@example.com"})

	if until := coolingUntilFor(p, 0); !until.IsZero() {
		t.Fatalf("the survivor was held out until %s by a removed account's late status", until)
	}
	if got := p.QuotaHeld(); got != 0 {
		t.Fatalf("QuotaHeld = %d, want 0", got)
	}
	for _, st := range p.States() {
		if st.Status != nil {
			t.Fatalf("account %d carries the removed account's identity: %+v", st.Index, st.Status)
		}
	}
}
