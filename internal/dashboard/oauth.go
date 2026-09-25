package dashboard

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"devin2proxy/internal/devin"
)

// Adding an account from the dashboard.
//
// The sign-in is a PKCE flow the proxy runs itself. The page asks for a URL, the
// person signs in and pastes the code back, and the code is exchanged for a session
// token in this process — the Devin CLI is not involved. The CLI was only ever the
// thing performing that exchange, and it cannot be driven from a service (its
// prompt reads the console), so doing it directly is both simpler and the only way
// the flow can work from a remote browser.
//
// The sign-in URL carries no redirect_uri, and that is not an oversight: the site
// refuses any redirect URI that is not on its allowlist, and a dashboard path is
// not. The code is therefore copied out of the page the sign-in site renders, and
// pasted into the box that asked for the URL. The verifier stays in this process,
// which is what makes a code useless to anyone else who sees it.

const (
	// loginFlowTTL is how long a sign-in URL stays usable. A sign-in that is
	// abandoned at the account picker should not leave a verifier lying around
	// indefinitely, and the code itself expires on the server's side too.
	loginFlowTTL = 10 * time.Minute
	// maxLoginFlows bounds the pending set. Each entry is a few hundred bytes and
	// belongs to an operator who is actively signing in; more than a handful means
	// something is retrying rather than a person.
	maxLoginFlows = 8
)

// pendingLogin is one sign-in waiting for its code.
type pendingLogin struct {
	verifier  string
	started   time.Time
	apiServer string
	// webHost is kept so a log line can name where the sign-in went.
	webHost string
}

// loginFlows holds the sign-ins in progress, keyed by state. They are deliberately
// not persisted: a sign-in half-done when the process exits is finished, and
// resurrecting it would mean keeping a verifier on disk for no gain.
type loginFlows struct {
	mu    sync.Mutex
	flows map[string]pendingLogin
}

func newLoginFlows() *loginFlows {
	return &loginFlows{flows: map[string]pendingLogin{}}
}

// add records a flow and returns false when the pending set is full, which the
// caller reports rather than evicting somebody else's in-progress sign-in.
func (l *loginFlows) add(state, verifier, apiServer, webHost string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	if len(l.flows) >= maxLoginFlows {
		return false
	}
	l.flows[state] = pendingLogin{
		verifier:  verifier,
		started:   now,
		apiServer: apiServer,
		webHost:   webHost,
	}
	return true
}

// take removes a flow and returns it. A code is exchanged at most once: the state
// is consumed on the first attempt even if that attempt fails, because a second
// exchange of the same code would be refused by the server anyway and leaving the
// entry would let a retry loop keep trying.
func (l *loginFlows) take(state string, now time.Time) (pendingLogin, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	flow, ok := l.flows[state]
	if ok {
		delete(l.flows, state)
	}
	return flow, ok
}

func (l *loginFlows) pruneLocked(now time.Time) {
	for state, flow := range l.flows {
		if now.Sub(flow.started) > loginFlowTTL {
			delete(l.flows, state)
		}
	}
}

// handleOAuthStart begins a sign-in: it makes a PKCE pair and hands back the URL to
// open. Nothing is sent to the page that would let anyone else finish the flow.
func (d *Dashboard) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if d.cfg.AccountsExternal != "" {
		d.deny(w, r, http.StatusConflict, "the account pool is set by "+d.cfg.AccountsExternal+
			"; remove that to manage accounts here")
		return
	}
	// The URL is where the browser goes; the code it shows afterwards is what comes
	// back here. Nothing about the flow is sent to the page, so a URL that leaks
	// still cannot be finished without the verifier.
	host := d.webappHost()
	flow, err := devin.NewLoginFlow(host)
	if err != nil {
		d.logf("dashboard: could not start a sign-in: %v", err)
		d.deny(w, r, http.StatusInternalServerError, "could not start a sign-in: "+err.Error())
		return
	}
	if !d.logins.add(flow.State, flow.Verifier, d.apiServer(), host, time.Now()) {
		d.deny(w, r, http.StatusConflict,
			"too many sign-ins are already waiting for a code; finish or abandon one first")
		return
	}
	d.logf("dashboard: sign-in started against %s", host)
	writeJSON(w, http.StatusOK, map[string]any{
		"state":     flow.State,
		"url":       flow.URL,
		"expires_s": int(loginFlowTTL.Seconds()),
	})
}

// handleOAuthFinish exchanges the pasted code and puts the account in the pool.
//
// The token is verified before it is kept: an exchange that succeeds but returns a
// credential the backend will not authenticate would otherwise join the rotation
// and fail every request handed to it. The status call is the same one the pool
// uses, so a credential that passes here is one the pool can serve with.
func (d *Dashboard) handleOAuthFinish(w http.ResponseWriter, r *http.Request) {
	if d.cfg.AccountsExternal != "" {
		d.deny(w, r, http.StatusConflict, "the account pool is set by "+d.cfg.AccountsExternal+
			"; remove that to manage accounts here")
		return
	}
	var body struct {
		State string `json:"state"`
		Code  string `json:"code"`
	}
	if err := decodeBody(r, &body); err != nil {
		d.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		d.deny(w, r, http.StatusBadRequest, "paste the code the sign-in page gave you")
		return
	}
	state := strings.TrimSpace(body.State)
	if state == "" {
		// A single sign-in in progress is the common case, and the page knows the
		// state it was given; accepting the only pending one keeps a refreshed page
		// from stranding a valid code.
		if only, ok := d.logins.onlyPending(time.Now()); ok {
			state = only
		} else {
			d.deny(w, r, http.StatusBadRequest, "no sign-in is waiting for a code; start one first")
			return
		}
	}
	flow, ok := d.logins.take(state, time.Now())
	if !ok {
		d.deny(w, r, http.StatusGone,
			"that sign-in has expired or was already used; start it again to get a fresh link")
		return
	}

	apiServer := flow.apiServer
	if apiServer == "" {
		apiServer = d.apiServer()
	}
	token, err := devin.ExchangeDevinCLIPKCECode(r.Context(), apiServer, code, flow.verifier)
	if err != nil {
		// The backend's own words are the useful part: an expired code says so, and
		// signing in again is the fix.
		d.logf("dashboard: sign-in exchange failed: %v", err)
		d.deny(w, r, http.StatusBadGateway, err.Error())
		return
	}
	if !strings.HasPrefix(token, devin.SessionTokenPrefix) {
		d.deny(w, r, http.StatusBadGateway, "the exchange returned a token in an unexpected form")
		return
	}
	if containsToken(d.cfg.Pool.Tokens(), token) {
		d.deny(w, r, http.StatusConflict, "that account is already in the pool")
		return
	}

	// Verified before it is kept, so the pool never holds a credential it cannot
	// serve with. The account status call is the same one the pool uses, and it
	// answers for a working credential and fails for anything else.
	creds := &devin.Credentials{APIKey: token, APIServerURL: apiServer, Source: "sign-in"}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	status, statusErr := d.cfg.Client.FetchAccountStatus(ctx, creds)
	if statusErr != nil {
		d.logf("dashboard: sign-in produced a credential the backend refused: %v", statusErr)
		d.deny(w, r, http.StatusBadGateway,
			"the code was accepted but the resulting credential was refused by the backend: "+statusErr.Error())
		return
	}

	idx := d.cfg.Pool.Add(token)
	if idx < 0 {
		d.deny(w, r, http.StatusConflict, "that account is already in the pool")
		return
	}
	if err := d.cfg.Store.SetAccounts(d.cfg.Pool.Tokens()); err != nil {
		d.cfg.Pool.Remove(idx)
		d.deny(w, r, http.StatusInternalServerError, "could not save the account list: "+err.Error())
		return
	}
	if status != nil {
		// Filed against the pool's own entry for this account, not the temporary
		// credential the status was checked with: Remember resolves identity, and
		// these are two pointers to the same token.
		d.cfg.Pool.Remember(d.cfg.Pool.CredentialAt(idx), status)
	}
	email := ""
	if status != nil {
		email = status.Email
	}
	// Named by tail in the log, and only by tail: the account is the credential.
	d.logf("dashboard: account added to the pool as slot %d (…%s) by sign-in", idx+1, tailOf(token))
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": d.cfg.Pool.States(),
		"email":    email,
		"index":    idx,
		"tail":     tailOf(token),
	})
}

// webappHost is where sign-ins are sent, with the public Devin site as the default so
// a process that names no host still gets a working link rather than a relative one.
func (d *Dashboard) webappHost() string {
	if host := strings.TrimSpace(d.cfg.WebappHost); host != "" {
		return host
	}
	return devin.DefaultWebappHost
}

// apiServer is the backend a sign-in exchanges its code against. It is the address
// requests go to, so a sign-in lands where the pool serves from. The configured
// value comes first, so a process — or a test — that names its backend never has the
// exchange reach somewhere else through a fallback it did not ask for.
func (d *Dashboard) apiServer() string {
	if d.cfg.APIServerURL != "" {
		return d.cfg.APIServerURL
	}
	if backend := d.backend(); backend != "" {
		return backend
	}
	return devin.DefaultAPIServerURL()
}

// onlyPending returns the state of the single sign-in waiting, and false when there
// is none or more than one. It exists so a page reloaded between the two steps can
// still finish a code the operator already has.
func (l *loginFlows) onlyPending(now time.Time) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	if len(l.flows) != 1 {
		return "", false
	}
	for state := range l.flows {
		return state, true
	}
	return "", false
}
