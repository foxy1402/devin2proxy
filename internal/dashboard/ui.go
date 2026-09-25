package dashboard

import (
	_ "embed"
	"net/http"
)

// uiHTML is the whole control panel, embedded so the binary is the only thing
// that has to be deployed. It is one file with no build step and no network
// dependency: the dashboard is often reachable only from the machine it runs on,
// and a page that needed a CDN would not load there.
//
//go:embed ui.html
var uiHTML []byte

// handleUI serves the page. It is deliberately unauthenticated: the page itself
// contains no data, only the JavaScript that fetches it, and a login form has to
// be reachable before anyone is signed in. Everything it can display comes from
// the API behind guard.
//
// It does honour the same loopback rule as the session and login endpoints: with
// remote access off, a machine that could not log in should not be served the
// panel's shell either, so the dashboard is uniformly this-machine-only.
func (d *Dashboard) handleUI(w http.ResponseWriter, r *http.Request) {
	if !d.cfg.AllowRemote && !isLoopbackAddr(r.RemoteAddr) {
		d.deny(w, r, http.StatusForbidden,
			"the dashboard is reachable from this machine only; set dashboard_allow_remote to change that")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is static, but an operator who restarts the proxy and reloads
	// should not be shown yesterday's JavaScript.
	w.Header().Set("Cache-Control", "no-store")
	// A page that is entirely self-contained needs no inline scripts to be allowed,
	// but it does build its DOM from strings, so the two headers a browser would
	// otherwise enforce are stated here rather than relaxed later.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// A default-deny content policy shaped to what the page actually does: one
	// inline script and one inline stylesheet, fetches to this origin and nothing
	// else — no images, no frames, no other hosts. The page keeps a full-access
	// session token in sessionStorage, so anything that could load a script from
	// somewhere else would be a way out with that token; the policy closes that
	// door even if a later edit reaches for one.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
			"connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(http.StatusOK)
	w.Write(uiHTML)
}
