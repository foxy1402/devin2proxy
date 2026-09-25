package dashboard

import (
	"context"
	"net/http"
	"strings"
	"time"

	"devin2proxy/internal/devin"
)

// The route pool is the set of outbound proxies requests are sent through.
// Editing it is the one thing on this dashboard that changes how the proxy
// behaves for every subsequent request, so it is done in two steps: validate the
// whole list by building real transports from it, and only then swap it in. A
// list with a typo in it is rejected outright rather than installed and debugged
// from a log line.

// maxProxySpecs bounds a pasted list. Twenty routes is already far past the point
// of diminishing returns, and the cap stops a paste of a hundred thousand lines
// from being built into a hundred thousand transports.
const maxProxySpecs = 50

// handleProxies reports the configured routes.
func (d *Dashboard) handleProxies(w http.ResponseWriter, r *http.Request) {
	pool := d.cfg.Client.Egress()
	writeJSON(w, http.StatusOK, map[string]any{
		"routes": pool.States(),
		"count":  pool.Len(),
		// How many are usable right now, which after a run of failures is the number
		// that matters much more than the total.
		"available": pool.Available(),
		"managed":   d.cfg.ProxiesExternal == "",
		"external":  d.cfg.ProxiesExternal,
		// The stored list with any user:pass replaced by the mask. The page is never
		// given the real credentials, and the mask is put back on save — see
		// restoreCredentials.
		"specs":  maskSpecs(d.proxySpecs()),
		"echo":   d.cfg.EchoURL != "",
		"direct": d.directNote(),
	})
}

// handleProxiesSave replaces the route pool. An empty list is valid and means the
// machine's own connection, which is how a route list is taken back out.
func (d *Dashboard) handleProxiesSave(w http.ResponseWriter, r *http.Request) {
	if d.cfg.ProxiesExternal != "" {
		d.deny(w, r, http.StatusConflict, "the route list is set by "+d.cfg.ProxiesExternal+
			"; remove that to manage it here")
		return
	}
	var body struct {
		Specs []string `json:"specs"`
		Text  string   `json:"text"`
	}
	if err := decodeBody(r, &body); err != nil {
		d.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	specs := body.Specs
	if len(specs) == 0 && body.Text != "" {
		specs = splitSpecs(body.Text)
	}
	// A route the page was shown back with the mask in place is the same route: the
	// credentials are restored from the stored list rather than taken from the mask.
	// Without this, saving the list without touching it would replace every proxy
	// password with "***".
	specs = restoreCredentials(specs, d.proxySpecs())
	if len(specs) > maxProxySpecs {
		d.deny(w, r, http.StatusBadRequest,
			"that is more routes than this proxy is willing to carry (the limit is 50)")
		return
	}

	// Build first, install second. NewEgressPool either produces a working pool or
	// an error naming the line that failed, so a bad paste leaves the running pool
	// untouched instead of replacing a working configuration with a broken one.
	pool, err := devin.NewEgressPool(specs, d.cfg.Egress)
	if err != nil {
		d.deny(w, r, http.StatusBadRequest, "the route list was not installed: "+err.Error())
		return
	}
	// Release the replaced pool's idle connections rather than letting them
	// linger until the idle timeout; in-flight requests on the old pool are
	// unaffected because their connections are not idle.
	if old := d.cfg.Client.SetEgress(pool); old != nil {
		old.Close()
	}

	if d.cfg.Store != nil {
		// Stored cleaned rather than as pasted: this is the list the pool was built
		// from, so a restart rebuilds exactly what is running.
		if err := d.cfg.Store.SetProxies(cleanSpecs(specs)); err != nil {
			d.deny(w, r, http.StatusInternalServerError,
				"the routes are in use, but the list could not be saved: "+err.Error())
			return
		}
	}
	// The specs are logged by host and port only; the credential-stripped forms are
	// what a log line gets, never the pasted URL.
	if pool.Len() == 0 {
		d.logf("dashboard: route list cleared; requests will use this machine's own connection")
	} else {
		d.logf("dashboard: route list replaced with %d route(s): %s",
			pool.Len(), strings.Join(pool.Specs(), ", "))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"routes":    pool.States(),
		"count":     pool.Len(),
		"available": pool.Available(),
		"specs":     maskSpecs(pool.Specs()),
	})
}

// handleProxyTest probes one route end to end. It is deliberately per-route and
// on demand: a probe opens a tunnel to the backend, and doing that to every route
// on every page load would be a self-inflicted load test.
func (d *Dashboard) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Spec  string `json:"spec"`
		Index *int   `json:"index"`
	}
	if err := decodeBody(r, &body); err != nil {
		d.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	spec := strings.TrimSpace(body.Spec)
	if spec == "" && body.Index != nil {
		// Testing a route already in the list: its spec is the stored one, which is
		// the only place the credentials still are.
		specs := d.proxySpecs()
		if *body.Index < 0 || *body.Index >= len(specs) {
			d.deny(w, r, http.StatusBadRequest, "no such route")
			return
		}
		spec = specs[*body.Index]
	}
	if spec == "" {
		d.deny(w, r, http.StatusBadRequest, "give a route to test, or the index of one in the list")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// An address, not the backend URL: a dialer given "http://host:port" fails with
	// "too many colons in address". The target is what the tunnel is opened to and
	// what TLS is negotiated against, so it has to be the real thing.
	probe := devin.ProbeEgress(ctx, spec, d.cfg.Egress, d.cfg.ProbeTarget, d.cfg.EchoURL)
	// The probe result is logged because a failed route test is usually read in the
	// log view rather than in the box that produced it.
	if probe.OK {
		exit := probe.ExitIP
		if exit == "" {
			exit = "unknown"
		}
		d.logf("dashboard: route test %s ok in %dms, exits from %s",
			probe.Spec, probe.LatencyMS, exit)
	} else {
		d.logf("dashboard: route test %s failed: %s", probe.Spec, probe.Error)
	}
	writeJSON(w, http.StatusOK, map[string]any{"probe": probe})
}

// proxySpecs is the list the textarea shows: what the dashboard manages if it
// manages anything, otherwise what the process was configured with.
//
// The fallback to the running pool's own list applies only when the store has no
// proxies key at all — a pool built from config or the environment before the
// dashboard ever managed routes. Once the store manages the list, its answer wins
// even when it is empty, because an empty stored list is an operator's deliberate
// "no routes". Feeding pool.Specs() in over it would hand the page
// credential-stripped lines and a later save would write the mask back as if it
// were real credentials.
func (d *Dashboard) proxySpecs() []string {
	if d.cfg.ProxiesExternal == "" && d.cfg.Store != nil && d.cfg.Store.HasProxies() {
		return d.cfg.Store.Proxies()
	}
	if pool := d.cfg.Client.Egress(); pool.Len() > 0 {
		return pool.Specs()
	}
	return nil
}

// directNote explains what happens when the list is empty, which is not an error
// but is worth saying out loud: requests then leave from this machine.
func (d *Dashboard) directNote() string {
	if d.cfg.Client.Egress().Len() > 0 {
		return ""
	}
	return "no routes configured; requests use this machine's own connection"
}

// splitSpecs turns a pasted block into a list, one route per line. Comments and
// blank lines are dropped here so the count the limit is applied to is the count
// of real routes.
func splitSpecs(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// cleanSpecs drops the entries a pool would ignore anyway, so what is stored is
// what is in use.
func cleanSpecs(specs []string) []string {
	var out []string
	for _, s := range specs {
		s = strings.TrimSpace(s)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		out = append(out, s)
	}
	return out
}

// maskSpecs replaces every route's credentials with the mask, which is the only
// form a response body carries. It delegates to the devin package so the page and
// the log lines show exactly the same thing.
//
// An empty list stays an empty list rather than becoming nil: this goes into a
// response body, and null there is not a list.
func maskSpecs(specs []string) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, devin.MaskSpec(s))
	}
	return out
}

// restoreCredentials puts the real credentials back into any route that came back
// with the mask in it. A masked line is matched against the stored list by
// everything except its credentials, so editing the host or the port of a route
// works while a password the page never saw survives the round trip.
//
// The position in the list is tried first: two routes that differ only in their
// credentials — the same exit host and port under two accounts — mask to the
// identical string, and a first-match scan would quietly give every saved copy
// of that line the first route's password. When the list has not been reordered,
// position identifies the route exactly. Only if the stored route at that
// position does not match is a scan over the whole list used, which is the
// ambiguous case the position check exists to avoid.
//
// A stored line that is itself the masked form is never used as the restoration
// source: it carries no credentials, so "restoring" it would write the mask into
// the pool as if the mask were the password. That can only happen when the list
// the page was shown came from the running pool rather than the store, and the
// honest outcome for such a line is to leave it as it arrived.
//
// A masked line that matches nothing stored is left as it is: the pool will then
// reject it, which is the honest outcome for a route nobody can authenticate.
func restoreCredentials(specs, stored []string) []string {
	if len(specs) == 0 {
		return specs
	}
	out := make([]string, 0, len(specs))
	for i, spec := range specs {
		if !devin.HasMaskedCredentials(spec) {
			out = append(out, spec)
			continue
		}
		restored := spec
		if i < len(stored) && devin.MaskSpec(stored[i]) == spec && !devin.HasMaskedCredentials(stored[i]) {
			restored = stored[i]
		} else {
			for _, known := range stored {
				if devin.MaskSpec(known) == spec && !devin.HasMaskedCredentials(known) {
					restored = known
					break
				}
			}
		}
		out = append(out, restored)
	}
	return out
}
