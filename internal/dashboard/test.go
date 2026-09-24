package dashboard

import (
	"context"
	"net/http"
	"strings"
	"time"

	"devin2proxy/internal/devin"
)

// The Test button on an account sends one real, tiny completion through that
// account and reports what the backend did with it. On success it also lifts the
// account's cooldown, which is the point of the button: an operator who has just
// watched an account answer does not want it sitting out the rest of a hold that a
// 429 or a stale quota reading put it under.
//
// Three decisions here are deliberate:
//
//   - A probe is a real request, so it spends a little of the account's quota. There
//     is no cheaper way to find out whether an account can serve; every unary call
//     this backend offers (status, catalogue) answers happily for a credential the
//     chat endpoint refuses.
//   - Failure changes nothing. The obvious symmetry — a refused probe cools the
//     account — is wrong here, because the operator can point the probe at a model
//     the plan does not include, and a `permission_denied` for a gated model says
//     nothing about the credential. Cooling on that would bench a healthy account.
//   - Success lifts the rotation cooldown *and* the quota hold. They are separate
//     pieces of evidence (a refusal, versus a status call's reading of the plan), and
//     a completion that just arrived outranks both.
const (
	// probeTimeout bounds one probe including the queue for a concurrency slot. A
	// one-word answer takes a couple of seconds; the rest is room for a slow route.
	probeTimeout = 90 * time.Second
	// probeMinInterval is the shortest gap between two probes of the same
	// account. A probe is a real completion and spends real quota, so the Test
	// button is not a health check on a timer — but an operator who clicks it
	// twice in a row should get an explanation, not a silently drained account.
	probeMinInterval = 30 * time.Second
)

// beginProbe atomically checks the throttle and, if the account may be probed,
// records this probe as started. It returns how long to wait when it may not —
// check and mark must not be separate steps, or two simultaneous clicks would
// both pass the check and both spend quota. The map is keyed by credential
// pointer and only grows when an operator presses Test, so entries for removed
// accounts are a handful of bytes each, unbounded only in theory.
func (d *Dashboard) beginProbe(creds *devin.Credentials) time.Duration {
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	if last, ok := d.lastProbe[creds]; ok {
		if wait := probeMinInterval - time.Since(last); wait > 0 {
			return wait
		}
	}
	if d.lastProbe == nil {
		d.lastProbe = map[*devin.Credentials]time.Time{}
	}
	d.lastProbe[creds] = time.Now()
	return 0
}

// handleAccountTest probes one account with one model id and files the outcome.
func (d *Dashboard) handleAccountTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Index *int   `json:"index"`
		Model string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		d.deny(w, r, http.StatusBadRequest, err.Error())
		return
	}
	pool := d.cfg.Pool
	if pool.Len() == 0 {
		d.deny(w, r, http.StatusBadRequest, "there are no accounts in the pool to test")
		return
	}
	// A pointer, not a bare int: a body that names no index would otherwise
	// default to 0 and quietly spend an account the operator did not ask about.
	if body.Index == nil {
		d.deny(w, r, http.StatusBadRequest, "name the account to test")
		return
	}
	creds := pool.CredentialAt(*body.Index)
	if creds == nil {
		d.deny(w, r, http.StatusBadRequest, "no such account")
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		d.deny(w, r, http.StatusBadRequest, "name the model id to test with")
		return
	}
	if wait := d.beginProbe(creds); wait > 0 {
		d.deny(w, r, http.StatusTooManyRequests,
			"this account was just probed; a test spends real quota, so one probe per account per "+
				probeMinInterval.String()+" — try again in "+wait.Round(time.Second).String())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	// Deliberately the pooled credential for this index rather than the pool's own
	// rotation: the operator named an account, and a probe that quietly went to
	// another one would answer a different question than the one asked.
	res := d.cfg.Client.ProbeChat(ctx, creds, model)

	// The probe can take ninety seconds. The pool may have changed around the
	// index during it, so what gets cleared is the credential the probe actually
	// ran against, resolved to its live slot by the pool itself.
	cleared := []string(nil)
	if res.OK {
		cleared = pool.ClearCooldown(creds)
		d.logf("dashboard: tested account (…%s) with %s: served it in %s%s",
			tailOf(creds.APIKey), model,
			res.Duration.Round(time.Millisecond), clearedNote(cleared))
	} else {
		d.logf("dashboard: tested account (…%s) with %s: failed (%s)",
			tailOf(creds.APIKey), model, probeFailure(res))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"probe":    res,
		"cleared":  cleared,
		"accounts": pool.States(),
	})
}

// probeFailure is the short form of a failed probe for the log line.
func probeFailure(res devin.ProbeResult) string {
	switch {
	case res.Error != "":
		return res.Error
	case res.StatusCode != 0:
		return http.StatusText(res.StatusCode)
	default:
		return "no answer"
	}
}

// clearedNote renders what a successful probe lifted, for a log line.
func clearedNote(cleared []string) string {
	if len(cleared) == 0 {
		return ""
	}
	return ", cleared the " + strings.Join(cleared, " and ") + " cooldown"
}
