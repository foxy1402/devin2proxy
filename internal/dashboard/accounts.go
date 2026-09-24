package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"devin2proxy/internal/devin"
)

// The dashboard can add an account two ways. Pasting a token is instant and needs
// nothing else installed. The other way runs the Devin CLI, which is the only thing
// that can obtain a token in the first place — but only its sign-out and the reading
// of the credential it wrote can be driven from here. Its sign-in prompt reads the
// console directly, so a code written to its stdin is accepted by the pipe and never
// seen by the prompt: the sign-in itself happens in a terminal, and the account is
// taken into the pool afterwards. See handleAccountCapture.
//
// The CLI writes the token to its own credentials file, and the dashboard reads it
// from there — it never writes to that file itself.

const (
	// devinTokenPrefix is the only credential format the backend authenticates. It is
	// checked wherever a token enters the pool so a value that could never work is
	// refused here, as a clear error, instead of on the wire as a refusal inside a
	// stream that arrived with HTTP 200.
	devinTokenPrefix = "devin-session-token$"
	// logoutTimeout bounds the sign-out. It removes a local file and asks nothing, so
	// it has no business taking long.
	logoutTimeout = 30 * time.Second
)

// handleAccounts lists the pool. Everything in the response is safe to show: a
// tail, not a token.
func (d *Dashboard) handleAccounts(w http.ResponseWriter, r *http.Request) {
	pool := d.cfg.Pool
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":   pool.States(),
		"count":      pool.Len(),
		"available":  pool.Available(),
		"quota_held": pool.QuotaHeld(),
		// With no accounts at all the proxy is using the CLI's stored credential,
		// which is a legitimate way to run and says nothing about the pool.
		"using_cli_creds": pool.Len() == 0,
		"managed":         d.cfg.AccountsExternal == "",
		"external":        d.cfg.AccountsExternal,
		"backend":         d.backend(),
		// The page shows the sign-in command for the operator to run in a terminal,
		// so it has to be the command this machine actually uses. A wrong path there
		// is a dead end the operator cannot diagnose from the page.
		"sign_in_command": d.signInCommand(),
		"cli_ready":       d.cfg.DevinCLI != "" && len(d.cfg.LoginArgs) > 0,
	})
}

// signInCommand is the sign-in command line as it would be typed, with the
// executable this dashboard is configured to use. A path with a space in it is
// quoted, because the page offers it to be copied and run as-is.
func (d *Dashboard) signInCommand() string {
	cli := d.cfg.DevinCLI
	if cli == "" {
		cli = "devin"
	}
	if strings.ContainsAny(cli, " \t") {
		cli = `"` + cli + `"`
	}
	args := d.cfg.LoginArgs
	if len(args) == 0 {
		args = []string{"auth", "login", "--force-manual-token-flow"}
	}
	return strings.Join(append([]string{cli}, args...), " ")
}

// handleAccountStatus asks the backend about one account, or all of them, and
// files the answers. It is the only endpoint here that spends a network round
// trip per account, so the UI calls it deliberately rather than on a timer.
func (d *Dashboard) handleAccountStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Index *int `json:"index"`
		All   bool `json:"all"`
	}
	if err := decodeBody(r, &body); err != nil {
		d.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	pool := d.cfg.Pool
	if pool.Len() == 0 {
		d.deny(w, r, http.StatusBadRequest, "there are no accounts in the pool to ask about")
		return
	}

	var indexes []int
	switch {
	case body.All:
		for i := 0; i < pool.Len(); i++ {
			indexes = append(indexes, i)
		}
	case body.Index != nil:
		indexes = []int{*body.Index}
	default:
		d.deny(w, r, http.StatusBadRequest, "name an index or set all")
		return
	}

	// Bounded concurrency: a status call is a real request against the account, and
	// firing twenty at once is how a legitimate feature looks like an attack.
	const parallel = 4
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, idx := range indexes {
		creds := pool.CredentialAt(idx)
		if creds == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, creds *devin.Credentials) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			status, err := d.cfg.Client.FetchAccountStatus(ctx, creds)
			if err != nil {
				d.logf("dashboard: account %d (…%s) status unavailable: %v", idx+1, tailOf(creds.APIKey), err)
				return
			}
			pool.Remember(creds, status)
		}(idx, creds)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{"accounts": pool.States()})
}

// handleAccountAdd adds a token pasted by hand. The token is the same string the
// CLI stores, so this needs nothing installed and works from a remote browser.
func (d *Dashboard) handleAccountAdd(w http.ResponseWriter, r *http.Request) {
	if d.cfg.AccountsExternal != "" {
		d.deny(w, r, http.StatusConflict, "the account pool is set by "+d.cfg.AccountsExternal+
			"; remove that to manage accounts here")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decodeBody(r, &body); err != nil {
		d.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		d.deny(w, r, http.StatusBadRequest, "paste a token first")
		return
	}
	if !strings.HasPrefix(token, devinTokenPrefix) {
		// The two values a person can be holding look nothing alike, and the wrong one
		// is easy to reach for: a code from the sign-in page is a bare 40-odd
		// characters, a session token carries the prefix. Saying which box it belongs
		// in is the difference between a fixable mistake and a dead end.
		d.deny(w, r, http.StatusBadRequest,
			"that does not look like a Devin session token: it must start with "+devinTokenPrefix+
				". If it is the code from a Devin sign-in page, paste it under Add account (OAuth) instead — "+
				"a code is only exchangeable by the sign-in that produced it, which is where its secret lives.")
		return
	}

	idx := d.cfg.Pool.Add(token)
	switch {
	case idx < 0 && d.cfg.Pool.Len() > 0 && containsToken(d.cfg.Pool.Tokens(), token):
		d.deny(w, r, http.StatusConflict, "that account is already in the pool")
		return
	case idx < 0:
		d.deny(w, r, http.StatusBadRequest, "that token could not be added")
		return
	}

	// Store before reporting success: a pool that has an account the store does not
	// would lose it on the next restart.
	if err := d.cfg.Store.SetAccounts(d.cfg.Pool.Tokens()); err != nil {
		d.cfg.Pool.Remove(idx)
		d.deny(w, r, http.StatusInternalServerError, "could not save the account list: "+err.Error())
		return
	}
	d.logf("dashboard: account added to the pool as slot %d (…%s)", idx+1, tailOf(token))

	email, statusErr := d.rememberStatus(r.Context(), idx)
	if statusErr != nil {
		// The account is in the pool either way. A status failure means the token
		// could not be asked about it — which is worth saying, because it usually
		// means the token is bad.
		writeJSON(w, http.StatusOK, map[string]any{
			"accounts": d.cfg.Pool.States(),
			"warning":  "added, but the backend did not answer for it: " + statusErr.Error(),
		})
		return
	}
	d.logf("dashboard: account %d is %s", idx+1, email)
	writeJSON(w, http.StatusOK, map[string]any{"accounts": d.cfg.Pool.States(), "email": email})
}

// handleAccountDelete forgets an account. It removes the token from the pool and
// the store; it cannot sign the account out of Devin, because the session belongs
// to the backend rather than to us.
func (d *Dashboard) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	if d.cfg.AccountsExternal != "" {
		d.deny(w, r, http.StatusConflict, "the account pool is set by "+d.cfg.AccountsExternal+
			"; remove that to manage accounts here")
		return
	}
	var body struct {
		Index *int `json:"index"`
	}
	if err := decodeBody(r, &body); err != nil {
		d.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if body.Index == nil || *body.Index < 0 || *body.Index >= d.cfg.Pool.Len() {
		d.deny(w, r, http.StatusBadRequest, "no such account")
		return
	}
	idx := *body.Index
	// Take a copy of the token before removing, so the log can name which account
	// left without keeping it anywhere else.
	removed := d.cfg.Pool.CredentialAt(idx)
	if !d.cfg.Pool.Remove(idx) {
		d.deny(w, r, http.StatusBadRequest, "no such account")
		return
	}
	if err := d.cfg.Store.SetAccounts(d.cfg.Pool.Tokens()); err != nil {
		d.deny(w, r, http.StatusInternalServerError, "removed it from the pool, but could not save the list: "+err.Error())
		return
	}
	tail := ""
	if removed != nil {
		tail = tailOf(removed.APIKey)
	}
	d.logf("dashboard: account %d (…%s) removed from the pool; %d left",
		idx+1, tail, d.cfg.Pool.Len())
	writeJSON(w, http.StatusOK, map[string]any{"accounts": d.cfg.Pool.States()})
}

// handleAccountCapture takes the account the Devin CLI is signed in to and puts it in
// the pool. It is the second half of adding an account, and the half that works.
//
// The CLI's sign-in cannot be run from here: its prompt reads the console directly, so
// a code written to its stdin is accepted by the pipe and never seen by the prompt.
// Both a pipe and a file redirect were tried and both hang. The sign-in therefore
// happens in a terminal, where it works, and this reads the credential it wrote
// afterwards — the same file the CLI would have written had it been driven from here.
func (d *Dashboard) handleAccountCapture(w http.ResponseWriter, r *http.Request) {
	if d.cfg.AccountsExternal != "" {
		d.deny(w, r, http.StatusConflict, "the account pool is set by "+d.cfg.AccountsExternal+
			"; remove that to manage accounts here")
		return
	}
	creds, err := devin.LoadCredentials()
	if err != nil {
		d.deny(w, r, http.StatusConflict,
			"the Devin CLI is not signed in, so there is no account to take: "+err.Error())
		return
	}
	token := creds.APIKey
	// Only the format the backend authenticates is taken, for the same reason the paste
	// path checks it: anything else would join the rotation and fail every request.
	if !strings.HasPrefix(token, devinTokenPrefix) {
		d.deny(w, r, http.StatusBadRequest,
			"the credential the CLI holds is not a Devin session token, so it cannot be pooled")
		return
	}
	if containsToken(d.cfg.Pool.Tokens(), token) {
		d.deny(w, r, http.StatusConflict, "that account is already in the pool")
		return
	}

	idx := d.cfg.Pool.Add(token)
	if idx < 0 {
		d.deny(w, r, http.StatusBadRequest, "that credential could not be added")
		return
	}
	if d.cfg.Store != nil {
		if err := d.cfg.Store.SetAccounts(d.cfg.Pool.Tokens()); err != nil {
			d.cfg.Pool.Remove(idx)
			d.deny(w, r, http.StatusInternalServerError,
				"the account was found, but the list could not be saved: "+err.Error())
			return
		}
	}

	email, statusErr := d.rememberStatus(r.Context(), idx)
	note := ""
	if statusErr != nil {
		note = "added, but the backend did not answer for it: " + statusErr.Error()
	}
	d.logf("dashboard: took the signed-in account (…%s) from the CLI into the pool", tailOf(token))
	writeJSON(w, http.StatusOK, map[string]any{
		"index":    idx,
		"tail":     tailOf(token),
		"email":    email,
		"note":     note,
		"accounts": d.cfg.Pool.States(),
	})
}

// handleAccountLogout signs the CLI out of whatever account it holds, which is the
// step that has to come before signing in as a different one: the CLI refuses to log
// in while a credential is present. Unlike the sign-in, this needs no terminal.
//
// It is deliberately not a login endpoint: it removes the credential from the CLI, so
// the account in the pool is what keeps serving requests afterwards.
func (d *Dashboard) handleAccountLogout(w http.ResponseWriter, r *http.Request) {
	if d.cfg.AccountsExternal != "" {
		d.deny(w, r, http.StatusConflict, "the account pool is set by "+d.cfg.AccountsExternal+
			"; remove that to manage accounts here")
		return
	}
	if d.cfg.DevinCLI == "" {
		d.deny(w, r, http.StatusServiceUnavailable,
			"the Devin CLI is not configured; set devin_cli in config.json to sign it out")
		return
	}
	// The account being signed out is taken into the pool first, for the same reason the
	// login path does it: signing out removes the only copy of the credential, and an
	// operator who signs out and then fails to sign in as anybody else would otherwise
	// be left with nothing.
	before := ""
	if creds, err := devin.LoadCredentials(); err == nil {
		before = creds.APIKey
	}
	if before != "" {
		d.keepCredential(r.Context(), before)
	}
	lines, err := d.runCLILogout(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "the CLI could not be signed out: " + err.Error(), "output": lines,
		})
		return
	}
	d.logf("dashboard: signed the Devin CLI out of the account it held")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": lines, "accounts": d.cfg.Pool.States()})
}

// rememberStatus fetches and files one account's status, returning the name it
// reports.
func (d *Dashboard) rememberStatus(ctx context.Context, idx int) (string, error) {
	creds := d.cfg.Pool.CredentialAt(idx)
	if creds == nil {
		return "", errors.New("no such account")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	status, err := d.cfg.Client.FetchAccountStatus(ctx, creds)
	if err != nil {
		return "", err
	}
	d.cfg.Pool.Remember(creds, status)
	return status.String(), nil
}

// keepCredential puts the account the CLI is holding into the pool before it is
// signed out. The sign-out deletes the only copy on the machine, so without this an
// operator who signs out and then fails to sign in as anybody else would be left with
// no credential at all — the CLI signed out and the pool empty.
//
// A token in a format the backend does not authenticate is not kept: it would join
// the rotation and fail every request it was given, which is the opposite of what
// pooling accounts is for.
func (d *Dashboard) keepCredential(ctx context.Context, token string) {
	if !strings.HasPrefix(token, devinTokenPrefix) || containsToken(d.cfg.Pool.Tokens(), token) {
		return
	}
	idx := d.cfg.Pool.Add(token)
	if idx < 0 {
		return
	}
	if d.cfg.Store != nil {
		if err := d.cfg.Store.SetAccounts(d.cfg.Pool.Tokens()); err != nil {
			d.cfg.Pool.Remove(idx)
			d.logf("dashboard: could not keep the signed-in account: %v", err)
			return
		}
	}
	d.logf("dashboard: kept the signed-in account (…%s) in the pool before signing out", tailOf(token))
	// Fetched so the pool shows an identity for it rather than a bare token tail,
	// matching what the capture path does for an account it takes.
	if _, err := d.rememberStatus(ctx, idx); err != nil {
		d.logf("dashboard: kept the signed-in account but could not read its status: %v", err)
	}
}

// runCLILogout runs the configured sign-out command and returns what it printed.
//
// Signing out is safe to do from here, unlike signing in: it only removes a local
// file and asks nothing, so it works with no terminal behind it.
func (d *Dashboard) runCLILogout(ctx context.Context) ([]string, error) {
	if len(d.cfg.LogoutArgs) == 0 {
		// Running the CLI with no arguments would open its help text rather than sign
		// anything out, so this is refused before a process is spawned.
		return nil, errors.New("no sign-out command is configured")
	}
	ctx, cancel := context.WithTimeout(ctx, logoutTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.cfg.DevinCLI, d.cfg.LogoutArgs...)
	out, err := cmd.CombinedOutput()
	lines := outputLines(out)
	if err != nil {
		return lines, fmt.Errorf("%s %s: %w", d.cfg.DevinCLI, strings.Join(d.cfg.LogoutArgs, " "), err)
	}
	return lines, nil
}

// outputLines splits a command's output into the non-blank lines worth showing, with
// the terminal control sequences it drew them with removed.
func outputLines(out []byte) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\r\n"), "\n") {
		line = stripANSI(strings.TrimRight(line, "\r"))
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// ansiPattern matches the terminal control sequences the CLI writes when it draws a
// prompt. They are the difference between the flow reading as "Code:" and reading as
// a line of "[?2004h[?2026h" with colour codes around every word, which is what the
// page showed before this.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][A-Z0-9]|\x1b[=>]`)

func stripANSI(s string) string { return ansiPattern.ReplaceAllString(s, "") }

// backend reports the server the pool's accounts were built against.
func (d *Dashboard) backend() string {
	if creds := d.cfg.Pool.CredentialAt(0); creds != nil {
		return creds.APIServerURL
	}
	if creds, err := devin.LoadCredentials(); err == nil {
		return creds.APIServerURL
	}
	return ""
}

// containsToken reports whether a token list holds an exact token.
func containsToken(tokens []string, token string) bool {
	for _, t := range tokens {
		if t == token {
			return true
		}
	}
	return false
}

// tailOf is the last few characters of a token. It matches what the proxy's own
// logs print, so a line in the log view and a row in the account list name the
// same account the same way. It is the only form of a token that is ever shown.
func tailOf(token string) string {
	const keep = 6
	if len(token) <= keep {
		return "…"
	}
	return token[len(token)-keep:]
}
