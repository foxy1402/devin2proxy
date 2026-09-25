package dashboard

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"devin2proxy/internal/devin"
)

// The Models panel asks the backend for its model catalogue: the same unary
// GetCliModelConfigs call the Devin CLI makes at startup, which returns every model
// with its limits and prices and costs no chat quota. What it is *for* is showing
// what the model this proxy actually drives can do — context window, output limit,
// tokenizer, price — which is information no client's documentation states and
// which the proxy would otherwise only be able to describe from its own config.
//
// It is asked as the CLI's identity (see devin.CLIIdentity): the proxy's own
// identity gets a one-entry list of Devin's chat models back instead of the
// catalogue, so there would be nothing to show. That choice, and the fact that it
// applies to this one call and not to the chat path, is recorded in
// docs/PROTOCOL.md.

const (
	// catalogueTTL is how long a catalogue is reused. It changes when Devin ships
	// models, which is not an hourly event, and each fetch is a few hundred
	// kilobytes; ten minutes keeps the panel current without asking repeatedly.
	catalogueTTL = 10 * time.Minute
	// catalogueFetchTimeout bounds one fetch and the whole fallback walk over the
	// accounts, because the page is waiting on it.
	catalogueFetchTimeout = 25 * time.Second
	// catalogueAttempts is how many accounts are tried before giving up. A pool can
	// hold a dead token (it cools those out of rotation but keeps them listed), and
	// the catalogue is the same for every account on one plan, so trying the next
	// one is worth it — but not unboundedly.
	catalogueAttempts = 3
)

// catalogueCache is the last catalogue this process read, plus why the last
// attempt failed if it did. It is kept in memory only: a catalogue that outlives
// the process would be a second copy of something the backend is happy to resend.
type catalogueCache struct {
	// mu is held across a fetch, not just around the fields. The fetch happens at
	// most once per TTL from a page that is only ever open by hand, so serializing
	// it is simpler than a singleflight and turns a burst of refreshes into one
	// upstream call.
	mu      sync.Mutex
	models  []devin.ModelConfig
	fetched time.Time
	// account is the tail of the credential the catalogue was read with, so the
	// panel can say whose plan the entry list describes.
	account string
	// fail is the last failure. It is kept alongside a good catalogue rather than
	// replacing it: stale limits are more useful than none, and the page shows the
	// error next to them.
	fail error
}

// handleModels serves the catalogue for the Models panel. It answers from the cache
// when it can, and fetches when there is nothing to answer with, when the cache is
// older than the TTL, or when the page asks for a refresh.
func (d *Dashboard) handleModels(w http.ResponseWriter, r *http.Request) {
	models, fetched, account, fail := d.catalogue.snapshot()
	refresh := r.URL.Query().Get("refresh") != ""
	if refresh || fetched.IsZero() || time.Since(fetched) > catalogueTTL {
		ctx, cancel := context.WithTimeout(r.Context(), catalogueFetchTimeout)
		defer cancel()
		models, fetched, account, fail = d.fetchCatalogue(ctx, refresh)
	}
	// Recomputed after the fetch branch: fetched is reassigned there, and a
	// catalogue that was just read must not be reported with the age of the one
	// it replaced.
	age := time.Since(fetched)

	served := d.servedModels(models)
	available := make([]devin.ModelConfig, 0, len(models))
	gated := 0
	for _, m := range models {
		if m.PlanGated {
			gated++
			continue
		}
		available = append(available, m)
	}

	resp := map[string]any{
		// served is what this proxy answers to, in the order it advertises them, each
		// with the catalogue entry it resolves to.
		"served": served,
		// details are the catalogue entries behind those uids, which is what the panel
		// expands into limits and prices.
		"details": detailsOf(served, models),
		// available are the catalogue's models this account's plan does not gate. On a
		// free account there is exactly one, which is the whole reason for the panel.
		"available":      available,
		"catalogue_size": len(models),
		"gated":          gated,
		"identity":       devin.CLIIdentity,
		"fetched":        rfc3339OrEmpty(fetched),
		"age_seconds":    int(age.Seconds()),
		// probe_models is every uid in the catalogue, for the Accounts pool's test
		// dropdown: the point of that control is to ask "does this account serve
		// *this* model", so it offers what the CLI itself would accept. probe_default
		// is the model this proxy serves by default, which is the useful thing to
		// probe with — it is the uid a client's request actually becomes.
		"probe_models":  probeEntries(served, models),
		"probe_default": d.cfg.Info.Model,
	}
	if account != "" {
		resp["account"] = account
	}
	if fail != nil {
		resp["error"] = fail.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// servedModel is one identifier this proxy accepts, and the catalogue entry it ends
// up as.
type servedModel struct {
	ID  string `json:"id"`
	UID string `json:"uid"`
	// Default marks the model every unrecognised name is routed to.
	Default bool `json:"default"`
	// Alias is true for an advertised name that is not itself a backend uid.
	Alias bool `json:"alias,omitempty"`
	// Forwarded marks a uid the proxy passes through when a client names it, even
	// though it is not advertised in the model list.
	Forwarded bool `json:"forwarded,omitempty"`
	// InCatalogue is false when the catalogue has no such uid at all, which is worth
	// saying rather than showing an empty detail block.
	InCatalogue bool `json:"in_catalogue"`
}

// servedModels maps this proxy's identifiers onto the catalogue: the advertised
// names, the default, and the uids the server forwards when asked for by name.
func (d *Dashboard) servedModels(catalogue []devin.ModelConfig) []servedModel {
	var out []servedModel
	seen := map[string]bool{}

	// Advisory set first, so an advertised name that is also a real uid keeps its
	// place in the list.
	for _, id := range d.cfg.Info.Models {
		uid := d.resolveServed(id)
		out = append(out, servedModel{
			ID:          id,
			UID:         uid,
			Default:     id == d.cfg.Info.Model,
			Alias:       uid != id,
			InCatalogue: hasModel(catalogue, uid),
		})
		seen[id] = true
	}
	for _, uid := range d.cfg.Forwarded {
		if seen[uid] {
			continue
		}
		out = append(out, servedModel{
			ID:          uid,
			UID:         uid,
			Forwarded:   true,
			InCatalogue: hasModel(catalogue, uid),
		})
	}
	return out
}

// resolveServed maps one advertised name onto the uid a request for it becomes,
// mirroring the server's own rule: a name that is a backend uid is forwarded, and
// anything else falls back to the default.
func (d *Dashboard) resolveServed(id string) string {
	if id == d.cfg.Info.Model || d.cfg.Info.Model == "" {
		return d.cfg.Info.Model
	}
	for _, uid := range d.cfg.Forwarded {
		if id == uid {
			return id
		}
	}
	return d.cfg.Info.Model
}

// detailsOf collects the catalogue entries behind the served uids, once each and in
// the order they appear among the served list.
func detailsOf(served []servedModel, catalogue []devin.ModelConfig) []devin.ModelConfig {
	var out []devin.ModelConfig
	seen := map[string]bool{}
	for _, s := range served {
		if seen[s.UID] {
			continue
		}
		seen[s.UID] = true
		if m, ok := devin.FindModel(catalogue, s.UID); ok {
			out = append(out, m)
		}
	}
	return out
}

func hasModel(catalogue []devin.ModelConfig, uid string) bool {
	_, ok := devin.FindModel(catalogue, uid)
	return ok
}

// probeEntry is one model id offered to the Accounts pool's test control. Every
// catalogue uid is listed, not just the two this proxy forwards: an operator
// probing a gated model is asking a real question ("is this account allowed on
// that"), and the answer is worth seeing even though the probe will come back
// refused.
type probeEntry struct {
	UID  string `json:"uid"`
	Name string `json:"name,omitempty"`
	// Gated marks the models the account's plan does not include, so the page can
	// group them apart instead of mixing a long list of unprobeable ids into the
	// ones that matter.
	Gated bool `json:"gated,omitempty"`
	// Served marks a uid this proxy passes through, which is the set a probe is
	// actually informative about.
	Served bool `json:"served,omitempty"`
}

// probeEntries orders the catalogue for the dropdown: what this proxy serves first
// (so the default is at hand), then the models the plan allows, then the gated ones.
// The order is the only difference between the three groups — the page draws the
// separators from these flags.
func probeEntries(served []servedModel, catalogue []devin.ModelConfig) []probeEntry {
	var out []probeEntry
	seen := map[string]bool{}

	for _, s := range served {
		if seen[s.UID] || s.UID == "" {
			continue
		}
		seen[s.UID] = true
		e := probeEntry{UID: s.UID, Served: true}
		if m, ok := devin.FindModel(catalogue, s.UID); ok {
			e.Name = m.Label()
			e.Gated = m.PlanGated
		}
		out = append(out, e)
	}
	// Two passes over the catalogue so the ungated models come before the gated ones
	// without sorting 248 entries.
	for _, gated := range []bool{false, true} {
		for _, m := range catalogue {
			if seen[m.UID] || m.PlanGated != gated {
				continue
			}
			seen[m.UID] = true
			out = append(out, probeEntry{UID: m.UID, Name: m.Label(), Gated: gated})
		}
	}
	return out
}

// snapshot returns the cached catalogue without fetching.
func (c *catalogueCache) snapshot() ([]devin.ModelConfig, time.Time, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.models, c.fetched, c.account, c.fail
}

// fetchCatalogue reads the catalogue, keeping the previous one if this attempt
// fails. force skips the TTL recheck for a refresh the page asked for. It is not
// set for a cold cache: a zero fetched time fails the recheck on its own, so two
// page loads that arrive together are serialised by the lock and the second finds
// the first's catalogue already inside the TTL instead of fetching again.
func (d *Dashboard) fetchCatalogue(ctx context.Context, force bool) ([]devin.ModelConfig, time.Time, string, error) {
	d.catalogue.mu.Lock()
	defer d.catalogue.mu.Unlock()
	if !force && time.Since(d.catalogue.fetched) <= catalogueTTL {
		return d.catalogue.models, d.catalogue.fetched, d.catalogue.account, d.catalogue.fail
	}

	var lastErr error
	for i, creds := range d.catalogueCredentials() {
		if i >= catalogueAttempts {
			break
		}
		models, err := d.cfg.Client.FetchModelCatalogue(ctx, creds)
		if err != nil {
			lastErr = err
			continue
		}
		d.catalogue.models = models
		d.catalogue.fetched = time.Now()
		d.catalogue.account = tailOf(creds.APIKey)
		d.catalogue.fail = nil
		d.logf("dashboard: read the model catalogue: %d models, %d available on this plan (as %s)",
			len(models), countAvailable(models), devin.CLIIdentity)
		return d.catalogue.models, d.catalogue.fetched, d.catalogue.account, nil
	}
	if lastErr == nil {
		lastErr = errNoCredentialForCatalogue
	}
	d.catalogue.fail = lastErr
	d.logf("dashboard: could not read the model catalogue: %v", lastErr)
	return d.catalogue.models, d.catalogue.fetched, d.catalogue.account, lastErr
}

// catalogueCredentials lists the credentials to try for a catalogue read: the pool
// in order, then the CLI's own stored credential when the pool is empty. It is the
// same precedence a request uses, so the panel describes the account that would
// serve the request.
func (d *Dashboard) catalogueCredentials() []*devin.Credentials {
	var out []*devin.Credentials
	for i := 0; i < d.cfg.Pool.Len(); i++ {
		if c := d.cfg.Pool.CredentialAt(i); c != nil {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		if c, err := devin.LoadCredentials(); err == nil {
			out = append(out, c)
		}
	}
	return out
}

func countAvailable(models []devin.ModelConfig) int {
	n := 0
	for _, m := range models {
		if !m.PlanGated {
			n++
		}
	}
	return n
}

func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

var errNoCredentialForCatalogue = errors.New(
	"no credential to read the model catalogue with: the pool is empty and the Devin CLI is not signed in")
