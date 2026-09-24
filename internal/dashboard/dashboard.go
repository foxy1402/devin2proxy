// Package dashboard serves the operator's control panel: the account pool, the
// outbound routes, and a live view of what the proxy is doing.
//
// It is a second HTTP surface with its own authentication, deliberately separate
// from the OpenAI-compatible one. The API key on /v1 spends quota, so it has to be
// pasteable into a client; the dashboard password guards the credentials
// themselves, so it is never handed out and never stored. The two never share a
// check: holding one grants nothing on the other, and the session cookie is scoped
// to /dashboard so it is not even sent to /v1.
package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/eventlog"
)

// dashboardPrefix is where everything in this package is mounted. The cookie path
// is derived from it.
const dashboardPrefix = "/dashboard"

// Options is everything the dashboard needs from the process it lives in. It is
// explicit rather than reached for globally, so the package can be tested with a
// pool, a client and an event hub of the test's own making.
type Options struct {
	// Pool is the account pool requests rotate through. Never nil; an empty pool
	// means the proxy is falling back to the CLI's stored credential.
	Pool *devin.Pool
	// Client makes the upstream calls the dashboard needs: an account's own status,
	// and a probe through one route.
	Client *devin.Client
	// Events is the log history and live stream. Nil disables the logs view.
	Events *eventlog.Hub
	// Store persists what the dashboard manages.
	Store *Store
	// DevinCLI is the command whose path is shown as part of the sign-in command
	// line, and which the sign-out endpoint runs. Empty means the page falls back to
	// the documented name.
	DevinCLI string
	// LoginArgs are the arguments a person should pass to DevinCLI to sign in. They
	// are what the page puts in the command it offers; the sign-in itself is done by
	// this process, not by running this command.
	LoginArgs []string
	// LogoutArgs are the arguments passed to DevinCLI to clear the credential it
	// holds. They run when the operator asks for the CLI to be signed out, which is
	// how the account already on the machine is parked in the pool before somebody
	// signs in as a different one.
	LogoutArgs []string
	// WebappHost is where the sign-in page lives, e.g. https://app.devin.ai. Empty
	// uses the Devin host.
	WebappHost string
	// APIServerURL is the backend a sign-in exchanges its code against. It is the
	// same value the accounts panel shows as "api server" and the one requests go
	// to. Empty falls back to the pool's own backend and then to the CLI's
	// credential — but naming it explicitly is the right thing for anything that
	// must not reach the live backend, such as a test.
	APIServerURL string
	// EchoURL is fetched through a route to report the address it exits from.
	// Empty skips that part of a route test.
	EchoURL string
	// ProbeTarget is the host:port a route test dials and negotiates TLS with. It is
	// an address rather than a URL, because that is what a dialer takes; empty
	// falls back to the package's own default of the Devin backend.
	ProbeTarget string
	// Egress carries the dial timeouts used when testing a route.
	Egress devin.EgressOptions
	// AllowRemote permits the dashboard from outside this machine. Off by default:
	// the dashboard can add and delete credentials, so reachable-by-accident is not
	// an acceptable default.
	AllowRemote bool
	// AccountsExternal, when non-empty, says the pool is supplied by something the
	// dashboard cannot change (the environment or config.json) and why, so the UI
	// can explain the locked list rather than silently ignoring edits.
	AccountsExternal string
	// ProxiesExternal is the same for the route list.
	ProxiesExternal string
	// Forwarded are the model uids a request is passed through to the backend when a
	// client names one, over and above the advertised list. The Models panel shows
	// them because "swe-1-6-fast is a real uid that this account's plan refuses" is
	// the kind of thing a client author otherwise discovers by being refused.
	Forwarded []string
	// Info is the summary the header shows.
	Info Info
}

// Info is the read-only description of the running process.
type Info struct {
	Addr    string `json:"addr"`
	Backend string `json:"backend"`
	Model   string `json:"model"`
	// APIKey is masked: enough to confirm which key is configured, not enough to
	// use. `-print-key` is the deliberate way to see it.
	APIKey        string   `json:"api_key"`
	Models        []string `json:"models"`
	QuotaCooldown bool     `json:"quota_cooldown"`
	Started       string   `json:"started"`
}

// Dashboard is the HTTP handler for the whole control panel.
type Dashboard struct {
	cfg   Options
	mux   *http.ServeMux
	start time.Time
	// logins holds the sign-ins waiting for a code. It lives in memory only: an
	// interrupted sign-in is finished, not something to resurrect.
	logins *loginFlows
	// catalogue is the last model catalogue read from the backend, for the Models
	// panel. See models.go.
	catalogue catalogueCache
	// probeMu guards lastProbe, the per-account throttle on the Test endpoint.
	probeMu   sync.Mutex
	lastProbe map[*devin.Credentials]time.Time
}

// New builds the dashboard and its routes.
func New(opts Options) *Dashboard {
	// A pool is never nil: an empty one is a legitimate state (the proxy is using
	// the CLI's own credential), and every handler would otherwise need a nil
	// check to say so.
	if opts.Pool == nil {
		opts.Pool = devin.NewPool(nil, "")
	}
	// A store is never nil either. One with no path keeps its state in memory, which
	// is what a test wants and what a process with no writable directory gets.
	if opts.Store == nil {
		store, err := OpenStore("")
		if err != nil {
			// OpenStore only fails on a path it cannot read, and there is no path.
			panic("dashboard: " + err.Error())
		}
		opts.Store = store
	}
	// A client is never nil: the status and route endpoints need one, and a
	// default-configured client makes no request until it is asked to.
	if opts.Client == nil {
		opts.Client = devin.NewClient(devin.Options{MaxConcurrent: 1})
	}
	d := &Dashboard{
		cfg:    opts,
		mux:    http.NewServeMux(),
		start:  time.Now(),
		logins: newLoginFlows(),
	}
	d.routes()
	return d
}

// routes wires the API. The method-and-path patterns are the standard library's
// own, so no router is needed.
func (d *Dashboard) routes() {
	d.mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dashboardPrefix+"/", http.StatusFound)
	})
	d.mux.HandleFunc("GET /dashboard/", d.handleUI)

	// Unauthenticated on purpose: the login page has to be reachable to log in.
	// Every one of these is rate-limited, and none of them reveals anything except
	// whether a password is set.
	d.mux.HandleFunc("GET /dashboard/api/session", d.handleSession)
	d.mux.HandleFunc("POST /dashboard/api/login", d.handleLogin)
	d.mux.HandleFunc("POST /dashboard/api/logout", d.guard(d.handleLogout))

	// Everything below requires a session.
	d.mux.HandleFunc("GET /dashboard/api/overview", d.guard(d.handleOverview))
	d.mux.HandleFunc("GET /dashboard/api/accounts", d.guard(d.handleAccounts))
	d.mux.HandleFunc("POST /dashboard/api/accounts/status", d.guard(d.handleAccountStatus))
	// Test sends one tiny real completion through one account, and lifts its
	// cooldown when the backend serves it. See test.go.
	d.mux.HandleFunc("POST /dashboard/api/accounts/test", d.guard(d.handleAccountTest))
	d.mux.HandleFunc("POST /dashboard/api/accounts/add", d.guard(d.handleAccountAdd))
	d.mux.HandleFunc("POST /dashboard/api/accounts/delete", d.guard(d.handleAccountDelete))

	// Adding an account by sign-in: the page asks for a URL, the person signs in and
	// pastes the code back, and this process exchanges it. Two calls, because the
	// code comes from a browser in between.
	d.mux.HandleFunc("POST /dashboard/api/accounts/oauth/start", d.guard(d.handleOAuthStart))
	d.mux.HandleFunc("POST /dashboard/api/accounts/oauth/finish", d.guard(d.handleOAuthFinish))

	// The CLI on this machine is still worth driving for the two things it can do
	// unattended: park the account it holds in the pool, and sign itself out so a
	// different account can be used there.
	d.mux.HandleFunc("POST /dashboard/api/accounts/logout", d.guard(d.handleAccountLogout))
	d.mux.HandleFunc("POST /dashboard/api/accounts/capture", d.guard(d.handleAccountCapture))

	d.mux.HandleFunc("GET /dashboard/api/models", d.guard(d.handleModels))

	d.mux.HandleFunc("GET /dashboard/api/proxies", d.guard(d.handleProxies))
	d.mux.HandleFunc("POST /dashboard/api/proxies", d.guard(d.handleProxiesSave))
	d.mux.HandleFunc("POST /dashboard/api/proxies/test", d.guard(d.handleProxyTest))

	d.mux.HandleFunc("GET /dashboard/api/logs/recent", d.guard(d.handleLogsRecent))
	d.mux.HandleFunc("GET /dashboard/api/logs", d.guard(d.handleLogStream))
}

func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) { d.mux.ServeHTTP(w, r) }

// Handler returns the dashboard for mounting under its prefix.
func (d *Dashboard) Handler() http.Handler { return d }

// guard enforces the two rules that apply to every data endpoint: the request has
// to come from this machine (unless remote access was asked for), and it has to
// carry a valid session.
func (d *Dashboard) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Cross-site requests are refused on the Origin header as well as by the
		// cookie's SameSite attribute, because a state-changing endpoint here
		// deletes credentials and one check is not a policy.
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			d.deny(w, r, http.StatusForbidden, "cross-origin request refused")
			return
		}
		if !d.cfg.AllowRemote && !isLoopbackAddr(r.RemoteAddr) {
			d.deny(w, r, http.StatusForbidden,
				"the dashboard is reachable from this machine only; set dashboard_allow_remote to change that")
			return
		}
		if d.cfg.Store.Password() == nil {
			d.deny(w, r, http.StatusServiceUnavailable,
				"no dashboard password is set; start the proxy with -dashboard-password to choose one")
			return
		}
		if !d.authenticated(r) {
			d.deny(w, r, http.StatusUnauthorized, "not signed in")
			return
		}
		next(w, r)
	}
}

// sameOrigin reports whether an Origin header names this host.
func sameOrigin(origin, host string) bool {
	trimmed := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://"), "/")
	return strings.EqualFold(trimmed, host)
}

// deny answers with a JSON error the page can display. A request that wants the
// page itself gets a short HTML explanation instead, so a stale tab does not show
// raw JSON.
func (d *Dashboard) deny(w http.ResponseWriter, r *http.Request, status int, message string) {
	if r != nil && strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>devin2proxy dashboard</title>"+
			"<body style=\"font:14px/1.5 system-ui;padding:2rem;background:#111;color:#eee\">"+
			"<h1 style=\"font-size:1rem\">%s</h1></body>", htmlEscape(message))
		return
	}
	writeJSON(w, status, map[string]any{"error": message})
}

// handleSession is the endpoint the page polls to decide between the login form
// and the panel. It is reachable without a session, so it reveals only whether the
// dashboard is configured, whether this client is signed in, and how long a ban
// has left to run.
func (d *Dashboard) handleSession(w http.ResponseWriter, r *http.Request) {
	if !d.cfg.AllowRemote && !isLoopbackAddr(r.RemoteAddr) {
		d.deny(w, r, http.StatusForbidden,
			"the dashboard is reachable from this machine only")
		return
	}
	authed := d.authenticated(r)
	state := d.loginStateFor(clientAddr(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":    d.cfg.Store.Password() != nil,
		"authenticated": authed,
		"login":         state,
		"remote":        !isLoopbackAddr(r.RemoteAddr),
	})
}

func (d *Dashboard) handleLogin(w http.ResponseWriter, r *http.Request) {
	addr := clientAddr(r)
	if !d.cfg.AllowRemote && !isLoopbackAddr(r.RemoteAddr) {
		d.deny(w, r, http.StatusForbidden, "the dashboard is reachable from this machine only")
		return
	}
	rec := d.cfg.Store.Password()
	if rec == nil {
		d.deny(w, r, http.StatusServiceUnavailable,
			"no dashboard password is set; start the proxy with -dashboard-password to choose one")
		return
	}
	// The ban is checked before the password is looked at, so a banned client
	// learns nothing about whether its guess was right — and costs no CPU either.
	if state := d.loginStateFor(addr); state.Banned {
		d.deny(w, r, http.StatusTooManyRequests,
			fmt.Sprintf("too many failed attempts; try again in %s", humanDuration(time.Until(state.Until))))
		d.logf("%s", loginLockedMessage(addr, state))
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		d.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}

	started := time.Now()
	if !verifyPassword(rec, body.Password) {
		// The key derivation normally makes a wrong password cost a few hundred
		// milliseconds to answer. When the stored record is malformed it
		// returns false instantly instead, and a suspiciously fast refusal is
		// a signal. The floor keeps every wrong-password answer at least
		// loginFailureDelay long.
		if wait := loginFailureDelay - time.Since(started); wait > 0 {
			time.Sleep(wait)
		}
		state := d.recordLoginFailure(addr)
		d.logf("dashboard: failed login from %s (%d in a row)", addr, state.Strikes)
		if state.Banned {
			d.logf("%s", loginLockedMessage(addr, state))
			d.deny(w, r, http.StatusTooManyRequests,
				fmt.Sprintf("wrong password; too many failed attempts, so logins are banned for %s",
					humanDuration(time.Until(state.Until))))
			return
		}
		d.deny(w, r, http.StatusUnauthorized,
			fmt.Sprintf("wrong password; %s before logins are banned", attemptsPhrase(state.Remaining)))
		return
	}

	d.recordLoginSuccess(addr)
	value, err := d.newSession(time.Now())
	if err != nil {
		d.deny(w, r, http.StatusInternalServerError, "could not start a session: "+err.Error())
		return
	}
	d.setSessionCookie(w, r, value)
	d.logf("dashboard: %s signed in", addr)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
}

func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Bumping the epoch invalidates every session, not just this browser's, which
	// is what makes logging out mean something if a cookie has been copied.
	if _, err := d.cfg.Store.BumpSessionEpoch(); err != nil {
		d.deny(w, r, http.StatusInternalServerError, "could not end the session: "+err.Error())
		return
	}
	d.clearSessionCookie(w, r)
	d.logf("dashboard: %s signed out; all sessions invalidated", clientAddr(r))
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

func (d *Dashboard) handleOverview(w http.ResponseWriter, r *http.Request) {
	info := d.cfg.Info
	info.Started = d.start.UTC().Format(time.RFC3339)
	pool := d.cfg.Pool
	routes := d.cfg.Client.Egress().States()

	resp := map[string]any{
		"info": info,
		"pool": map[string]any{
			"accounts":          pool.Len(),
			"available":         pool.Available(),
			"quota_held":        pool.QuotaHeld(),
			"using_cli_creds":   pool.Len() == 0,
			"external":          d.cfg.AccountsExternal,
			"quota_cooldown_on": d.cfg.Info.QuotaCooldown,
		},
		"routes": map[string]any{
			"count":     len(routes),
			"available": countAvailableRoutes(routes),
			"external":  d.cfg.ProxiesExternal,
			"list":      routes,
		},
		"events": map[string]any{
			"enabled": d.cfg.Events != nil,
			"dropped": d.cfg.Events.Dropped(),
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func countAvailableRoutes(routes []devin.RouteState) int {
	n := 0
	for _, route := range routes {
		if route.Available {
			n++
		}
	}
	return n
}

// logsPageSize is how many events the Logs view holds. The view shows the
// newest first and prunes older ones as new ones arrive, so the history is a
// window onto the latest activity rather than something that grows while it is
// watched. 200 rows is enough to see what just happened without a DOM that
// gets heavier the longer the tab stays open.
const logsPageSize = 200

// handleLogsRecent returns the retained history, newest first, which is what a
// page polling every few seconds shows. The limit is a guard, not a page size:
// the view asks for its own window and the endpoint refuses anything absurd.
func (d *Dashboard) handleLogsRecent(w http.ResponseWriter, r *http.Request) {
	if d.cfg.Events == nil {
		writeJSON(w, http.StatusOK, map[string]any{"events": []any{}, "enabled": false})
		return
	}
	limit, ok := parsePositiveInt(r.URL.Query().Get("limit"))
	if !ok || limit == 0 || limit > logsPageSize {
		limit = logsPageSize
	}
	all := d.cfg.Events.Recent()
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	// Newest first, so the latest line is already at the top when the page draws
	// it — nobody should have to scroll for what just happened.
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"events":  all,
		"dropped": d.cfg.Events.Dropped(),
		"enabled": true,
	})
}

// handleLogStream is the live view the page used to hold open as a streaming
// connection; it now redirects to the polling endpoint instead. It is kept as a
// route rather than removed so an older page already open does not break: a 410
// with the replacement path is a signpost, not a silent 404.
func (d *Dashboard) handleLogStream(w http.ResponseWriter, r *http.Request) {
	d.deny(w, r, http.StatusGone, "the log stream has moved to GET /dashboard/api/logs/recent")
}

// logf writes to the process log, which is also the event stream: main tees the
// standard logger into the hub, so one call is both a line in the console and a
// row in the dashboard's log view.
func (d *Dashboard) logf(format string, args ...any) {
	log.Printf(format, args...)
}

// decodeBody reads a bounded JSON body, so a huge one cannot be used to exhaust
// memory through the dashboard.
func decodeBody(r *http.Request, dst any) error {
	defer r.Body.Close()
	// A limit rather than MaxBytesReader: the decoder rejects a truncated document
	// anyway, and this needs no ResponseWriter to report the overflow through.
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("invalid request body: " + err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	b, err := json.Marshal(v)
	if err != nil {
		// Nothing useful can be sent now; the status is already written.
		return
	}
	w.Write(b)
}

// humanDuration renders a wait the way a person reads it, for the ban messages.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds())+1)
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes())+1)
	default:
		return fmt.Sprintf("%.1f hours", d.Hours())
	}
}

// attemptsPhrase renders the remaining-attempts warning.
func attemptsPhrase(n int) string {
	if n <= 0 {
		return "no attempts left"
	}
	if n == 1 {
		return "1 attempt left"
	}
	return fmt.Sprintf("%d attempts left", n)
}

// htmlEscape escapes the handful of characters that matter when a message is
// placed in the stub page, avoiding a dependency on html/template for four
// replacements.
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
