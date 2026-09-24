package devin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Pool rotates a set of credentials so several free-tier accounts can share the
// load. It exists because the account limit is per account, not per machine, and
// a single free-tier account is easy to exhaust: handing each successive request
// to a different account multiplies the available throughput by the number of
// accounts.
//
// Rotation per request is safe because no server-side conversation state is
// relied on: each request opens a fresh trajectory, and the backend accepts a
// freshly generated one (see docs/PROTOCOL.md). Switching accounts mid-conversation
// therefore costs nothing beyond the history the client already resends.
type Pool struct {
	entries []*Credentials
	// apiServerURL is the backend every entry was built with, kept so an account
	// added later joins the pool on the same backend as the rest.
	apiServerURL string
	// rot walks the entries one per request. See rotationCursor for why rotation
	// is per request rather than per conversation.
	rot rotationCursor

	// status, when set, lets the pool learn how long an exhausted account should
	// stay out instead of guessing. Nil disables quota awareness entirely, which
	// is what a pool with no backend to ask does.
	status StatusFunc

	quotaMu sync.Mutex
	// busy marks the entries whose status is being fetched, so a burst of
	// refusals asks the backend once rather than once per request.
	busy []bool
	// quotaCoolUntil holds, per entry, the deadline a quota extension set. Keeping
	// the deadline rather than a flag is what makes the distinction expire on its
	// own: an account that was held until its reset last week is an ordinary
	// account today, both for /healthz and for the pool-wide-refusal guard.
	quotaCoolUntil []time.Time
	// identities caches what each account's own status call said about it, keyed
	// by the entry's index. It is display data — an email is what an operator
	// recognises in a list — and never consulted when deciding anything, so a
	// status that could not be read costs a blank cell and nothing more.
	identities map[int]*AccountStatus
	// lastRotation is the index the cursor handed out most recently, so a list can
	// show the order requests are actually walking in.
	lastRotation int
}

// StatusFunc reads one account's status. It is called in the background, after a
// request has already been refused, so nothing waits on it.
type StatusFunc func(ctx context.Context, creds *Credentials) (*AccountStatus, error)

// CoolRejected and CoolRateLimited are the cooldown periods applied when an
// account is refused. A rejection is usually a revoked or expired session, which
// will not fix itself quickly, so it sits out longer than a rate limit, which
// typically clears in seconds.
const (
	CoolRejected    = 5 * time.Minute
	CoolRateLimited = 30 * time.Second
)

// MaxQuotaCool bounds how long quota evidence may keep an account out of
// rotation. The daily reset can be most of a day away, and waiting for it is the
// point of the feature, so the cap only exists to stop a misread status from
// parking an account indefinitely.
const MaxQuotaCool = 12 * time.Hour

// NewPool builds a pool from raw token strings. Tokens are the full
// `devin-session-token$<jwt>` values as they appear in credentials.toml; blank
// entries and `#` comments are ignored so a hand-maintained file can be read
// directly.
//
// apiServerURL overrides the backend for every entry, matching the env override
// behaviour of LoadCredentials.
func NewPool(tokens []string, apiServerURL string) *Pool {
	if apiServerURL == "" {
		apiServerURL = defaultAPIServerURL
	}
	p := &Pool{}
	for _, raw := range tokens {
		tok := cleanToken(raw)
		if tok == "" {
			continue
		}
		p.entries = append(p.entries, &Credentials{
			APIKey:       tok,
			APIServerURL: apiServerURL,
			Source:       fmt.Sprintf("pool[%d]", len(p.entries)),
			poolIndex:    len(p.entries),
		})
	}
	p.rot = newRotationCursor(len(p.entries))
	p.busy = make([]bool, len(p.entries))
	p.quotaCoolUntil = make([]time.Time, len(p.entries))
	p.identities = map[int]*AccountStatus{}
	p.lastRotation = -1
	p.apiServerURL = apiServerURL
	return p
}

// cleanToken strips whitespace, quotes and comments from a token line.
func cleanToken(raw string) string {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "#"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.Trim(s, `"'`)
	return s
}

// Len reports how many credentials the pool holds. A nil pool holds none, which
// is how a server running on the CLI's own credential reaches this code.
func (p *Pool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.entries)
}

// Available reports how many credentials are currently in rotation, i.e. not
// cooling down. The count is a snapshot, so it can change before the next
// request.
func (p *Pool) Available() int {
	if p == nil {
		return 0
	}
	p.rot.mu.Lock()
	defer p.rot.mu.Unlock()
	return p.rot.availableLocked(time.Now())
}

// Next returns the next credential in rotation. Entries that are cooling down are
// skipped; see rotationCursor.pick for what happens when they all are.
//
// Returns nil only when the pool is empty.
func (p *Pool) Next() *Credentials {
	if p == nil || len(p.entries) == 0 {
		return nil
	}
	p.rot.mu.Lock()
	defer p.rot.mu.Unlock()
	idx := p.rot.pick(time.Now())
	p.lastRotation = idx
	return p.entries[idx]
}

// Add appends one credential and returns its index, or -1 when the token is blank
// or the pool already holds it.
//
// Adding does not disturb the accounts already in the pool: their cooldowns,
// their quota holds and their places in the rotation are untouched, which matters
// because adding an account to a pool whose others are held until a reset must not
// bring those others back early.
func (p *Pool) Add(raw string) int {
	if p == nil {
		return -1
	}
	tok := cleanToken(raw)
	if tok == "" {
		return -1
	}
	p.rot.mu.Lock()
	defer p.rot.mu.Unlock()
	for _, e := range p.entries {
		if e.APIKey == tok {
			return -1
		}
	}
	idx := len(p.entries)
	p.entries = append(p.entries, &Credentials{
		APIKey:       tok,
		APIServerURL: p.apiServerURL,
		Source:       fmt.Sprintf("dashboard[%d]", idx),
		poolIndex:    idx,
	})
	p.rot.appendSlot()

	p.quotaMu.Lock()
	p.busy = append(p.busy, false)
	p.quotaCoolUntil = append(p.quotaCoolUntil, time.Time{})
	p.quotaMu.Unlock()
	return idx
}

// Remove drops the credential at idx and renumbers the ones after it, so an index
// a caller read from States still means the same account when it acts on it. A
// request already in flight holds its own credential and finishes on it.
func (p *Pool) Remove(idx int) bool {
	if p == nil {
		return false
	}
	p.rot.mu.Lock()
	defer p.rot.mu.Unlock()
	if idx < 0 || idx >= len(p.entries) {
		return false
	}
	p.entries = append(p.entries[:idx], p.entries[idx+1:]...)
	for i, e := range p.entries {
		e.poolIndex = i
	}
	p.rot.removeSlot(idx)
	if p.lastRotation == idx {
		p.lastRotation = -1
	} else if p.lastRotation > idx {
		p.lastRotation--
	}

	p.quotaMu.Lock()
	if idx < len(p.busy) {
		p.busy = append(p.busy[:idx], p.busy[idx+1:]...)
	}
	if idx < len(p.quotaCoolUntil) {
		p.quotaCoolUntil = append(p.quotaCoolUntil[:idx], p.quotaCoolUntil[idx+1:]...)
	}
	// Every cached identity after the removed one now belongs to the account one
	// place earlier, so shift them rather than dropping the lot.
	moved := make(map[int]*AccountStatus, len(p.identities))
	for i, st := range p.identities {
		switch {
		case i < idx:
			moved[i] = st
		case i > idx:
			moved[i-1] = st
		}
	}
	p.identities = moved
	p.quotaMu.Unlock()
	return true
}

// Tokens returns the credentials the pool holds, in rotation order.
//
// This is the one accessor that hands out secrets, and it exists for exactly one
// caller: persisting the list after an account is added or removed. Nothing that
// renders or logs should use it — States is for that.
func (p *Pool) Tokens() []string {
	if p == nil {
		return nil
	}
	p.rot.mu.Lock()
	defer p.rot.mu.Unlock()
	out := make([]string, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.APIKey)
	}
	return out
}

// AccountState is one account as something outside the pool can describe it: a
// tail to recognise it by, what the backend said about it, and why it is or is
// not serving requests right now. It never carries the token.
type AccountState struct {
	Index      int    `json:"index"`
	Tail       string `json:"tail"`
	Source     string `json:"source"`
	Backend    string `json:"backend,omitempty"`
	Available  bool   `json:"available"`
	QuotaHeld  bool   `json:"quota_held"`
	LastServed bool   `json:"last_served"`
	// CoolingUntil is set while the account is out of rotation, and QuotaUntil
	// only when a status call is what put it there.
	CoolingUntil *time.Time     `json:"cooling_until,omitempty"`
	QuotaUntil   *time.Time     `json:"quota_until,omitempty"`
	Status       *AccountStatus `json:"status,omitempty"`
	// StatusPending is true while a status call for this account is in flight, so
	// a list can say "checking" rather than showing a stale answer as current.
	StatusPending bool `json:"status_pending,omitempty"`
}

// States reports every account and its current place in the rotation. The two
// locks are taken in turn rather than nested, so the snapshot can be a few
// microseconds stale against a report landing at the same moment — which for a
// list someone is reading is not a distinction worth a lock order. (Where a
// mutation needs both, the order is rot.mu outermost and quotaMu nested; this
// reader holds neither across the other, so it cannot invert that order.)
func (p *Pool) States() []AccountState {
	if p == nil {
		return nil
	}
	now := time.Now()
	p.rot.mu.Lock()
	cooling := make([]time.Time, len(p.rot.cooling))
	copy(cooling, p.rot.cooling)
	last := p.lastRotation
	entries := make([]*Credentials, len(p.entries))
	copy(entries, p.entries)
	p.rot.mu.Unlock()

	p.quotaMu.Lock()
	quotaUntil := make([]time.Time, len(p.quotaCoolUntil))
	copy(quotaUntil, p.quotaCoolUntil)
	busy := make([]bool, len(p.busy))
	copy(busy, p.busy)
	identities := make(map[int]*AccountStatus, len(p.identities))
	for i, st := range p.identities {
		identities[i] = st
	}
	p.quotaMu.Unlock()

	out := make([]AccountState, 0, len(entries))
	for i, e := range entries {
		st := AccountState{
			Index:      i,
			Tail:       tail(e.APIKey),
			Source:     e.Source,
			Backend:    e.APIServerURL,
			Available:  i >= len(cooling) || !now.Before(cooling[i]),
			LastServed: i == last,
			Status:     identities[i],
		}
		if i < len(cooling) && now.Before(cooling[i]) {
			until := cooling[i]
			st.CoolingUntil = &until
		}
		if i < len(quotaUntil) && now.Before(quotaUntil[i]) {
			until := quotaUntil[i]
			st.QuotaHeld = true
			st.QuotaUntil = &until
		}
		st.StatusPending = i < len(busy) && busy[i]
		out = append(out, st)
	}
	return out
}

// CredentialAt returns the credential at idx, for a caller that needs to ask the
// backend about that account specifically. It returns nil for an unknown index.
func (p *Pool) CredentialAt(idx int) *Credentials {
	if p == nil {
		return nil
	}
	p.rot.mu.Lock()
	defer p.rot.mu.Unlock()
	if idx < 0 || idx >= len(p.entries) {
		return nil
	}
	return p.entries[idx]
}

// indexLocked reports the slot creds currently occupies, or -1 when this pool
// no longer holds it. Callers that captured an index before a wait cannot trust
// it afterwards: Remove splices the per-slot slices and renumbers the
// survivors, so a stale index is either out of range or — worse — a different
// account. The identity check is the same one Report makes. Call it with
// rot.mu held.
func (p *Pool) indexLocked(creds *Credentials) int {
	if creds == nil || creds.poolIndex < 0 || creds.poolIndex >= len(p.entries) {
		return -1
	}
	if p.entries[creds.poolIndex] != creds {
		return -1
	}
	return creds.poolIndex
}

// Remember files what a status call said about one account, so a list can show an
// email instead of a token tail. It is display data: nothing in the pool's
// cooldown decisions reads it.
func (p *Pool) Remember(creds *Credentials, st *AccountStatus) {
	if p == nil || st == nil {
		return
	}
	p.rot.mu.Lock()
	idx := p.indexLocked(creds)
	p.rot.mu.Unlock()
	if idx < 0 {
		return
	}
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()
	if p.identities == nil {
		p.identities = map[int]*AccountStatus{}
	}
	p.identities[idx] = st
}

// Report records the outcome of a request served by creds, so a rejected or
// rate-limited account steps out of rotation. A nil error and a nil creds are
// both no-ops, which keeps the call sites to a single line.
//
// Only a refusal from the backend cools an account. Everything else — a transport
// failure, a broken outbound route, a cancelled request — says nothing about
// whether this account works, so it must not be recorded. A client that aborts an
// autocomplete request is the common case of that, and treating it as a failure
// would take a healthy account out of rotation for five minutes on every cancel.
//
// Two guards keep the pool from benching accounts that are not at fault:
//
//   - A refusal that would leave no account usable at all is treated as a
//     provider-side change rather than a verdict on this key. The accounts that
//     emptied the pool are pulled back to the short cooldown along with it, so one
//     incident cannot walk the pool and park every account for the rejection
//     period. One revoked token does not revoke every token.
//   - Quota evidence can only lengthen a cooldown that a refusal already earned,
//     and a refusal can never shorten a quota hold. See refreshQuota.
//
// refusalDetail extracts what the upstream actually said about a refusal, for
// the log line. It returns "" when the error carries no words — a transport
// failure or a cancel — because then there is nothing to quote.
func refusalDetail(err error) string {
	if err == nil {
		return ""
	}
	var httpErr *HTTPError
	var connectErr *ConnectError
	switch {
	case errors.As(err, &httpErr):
		return collapseDetail(string(httpErr.Body))
	case errors.As(err, &connectErr):
		return collapseDetail(connectErr.Message)
	}
	return ""
}

// detailSuffix renders a refusal detail for a log line, empty when there is
// none. The leading space belongs to the format string it fills.
func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " (" + detail + ")"
}

// collapseDetail flattens one upstream error into a single bounded log
// fragment: an HTML error page is dozens of lines, and one of them must not
// bury the log.
func collapseDetail(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const cap = 160
	if len(s) > cap {
		s = s[:cap] + "…"
	}
	return s
}

func (p *Pool) Report(creds *Credentials, status int, err error) {
	if p == nil || creds == nil || creds.poolIndex < 0 {
		return
	}
	if err != nil {
		// A cancelled or timed-out request is ours or the client's doing, not the
		// account's. This is checked before the error type because a cancel can
		// arrive wrapped inside anything.
		if isContextFailure(err) {
			return
		}
		// Transport failures are not the credential's fault either. Only a refusal
		// is.
		var httpErr *HTTPError
		var connectErr *ConnectError
		switch {
		case errors.As(err, &httpErr):
			status = httpErr.Status
		case errors.As(err, &connectErr):
			status = statusFromConnectCode(connectErr.Code)
		default:
			return
		}
	}
	var cool time.Duration
	switch status {
	case 401, 403:
		cool = CoolRejected
	case 429:
		cool = CoolRateLimited
	default:
		return
	}

	p.rot.mu.Lock()
	idx := creds.poolIndex
	// Verify identity rather than trusting the index: a credential that did not
	// come from this pool carries a zero index, which would otherwise look like
	// slot 0 and knock a healthy account out of rotation.
	if idx < 0 || idx >= len(p.entries) || p.entries[idx] != creds {
		p.rot.mu.Unlock()
		return
	}
	now := time.Now()
	alreadyCooling := p.rot.coolOff(idx, now, now.Add(cool))

	// The whole pool being unusable is the signature of something changing on the
	// provider's side — a rotated credential format, an outage, a gateway in front
	// of it misbehaving — not of this one key going bad. Serving the soonest
	// recovery already keeps requests flowing; what this guard adds is not letting
	// the discovery park the pool. The accounts that emptied it moments ago were
	// parked on exactly this evidence, so they are pulled back with it: without
	// that, one provider-side change walks the pool a request at a time and leaves
	// every account but the last one sitting out the rejection period. A slot held
	// out on quota evidence is exempt, since that hold came from the account's own
	// status rather than from a refusal.
	//
	// The `cool > CoolRateLimited` term is what keeps a rate limit out of this:
	// a 429 cools a slot for 30s, which expires on its own almost immediately, so
	// there is nothing to undo. Only the long rejection cooldown is worth a
	// second look, and only a refusal that empties the pool earns one.
	systemic := false
	if len(p.entries) > 1 && !alreadyCooling && cool > CoolRateLimited && p.rot.availableLocked(now) == 0 {
		held := p.quotaHeldSnapshot(now)
		until := now.Add(CoolRateLimited)
		for i := range p.entries {
			if held[i] {
				continue
			}
			p.rot.clampCooling(i, now, until)
		}
		systemic = true
	}
	available := p.rot.availableLocked(now)
	p.rot.mu.Unlock()

	// The status code alone cannot be acted on: a 403 from a WAF page and a 403
	// from the seat-management service are different emergencies, and only the
	// upstream's own words tell them apart. An earlier deployment logged just
	// "after HTTP 403" against four healthy accounts, which read as four dead
	// keys when it was one blocked address.
	detail := refusalDetail(err)
	switch {
	case systemic:
		log.Printf("account %d/%d (…%s) was refused with HTTP %d%s and no account is left usable; "+
			"holding the whole pool out for only %s because a refusal of every account looks like a "+
			"provider-side change rather than these keys being bad",
			idx+1, len(p.entries), tail(creds.APIKey), status, detailSuffix(detail), CoolRateLimited)
	case !alreadyCooling:
		log.Printf("account %d/%d (…%s) stepped out of rotation for %s after HTTP %d%s; "+
			"%d/%d accounts still available",
			idx+1, len(p.entries), tail(creds.APIKey), cool, status, detailSuffix(detail), available, len(p.entries))
	}

	// A rate limit is the one refusal that can be told how long it will last, so
	// ask. This runs in the background and can only lengthen the cooldown above.
	if status == 429 {
		p.refreshQuota(creds)
	}
}

// SetStatusSource enables quota-aware cooldowns: after a rate limit, the pool asks
// the backend how much quota the account has left and, when some counter reads
// zero, holds the account out until the reset instead of retrying every 30s.
//
// Passing nil disables it, which is the default, so a pool with no way to reach
// the backend behaves exactly as it did before.
func (p *Pool) SetStatusSource(fn StatusFunc) {
	if p == nil {
		return
	}
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()
	p.status = fn
}

// ClearCooldown brings one account back into rotation immediately, and reports what
// it lifted ("rotation", "quota") so the caller can say so.
//
// A cooldown otherwise only ever expires. This is the one exception, and it exists
// for one caller: a probe an operator asked for, which has just watched the account
// serve a real completion. That is direct evidence about *this* account, so unlike
// the pool-wide guard it touches one slot and leaves every other cooldown alone. It
// also clears the quota hold, which is a different kind of evidence — an account
// that answers a completion is not out of quota, whatever the last status call
// said.
//
// It takes the credential rather than an index because the caller resolved that
// index before a probe that can take ninety seconds; by now the pool may have
// changed around it. An account the pool no longer holds, or one it never held,
// clears nothing.
func (p *Pool) ClearCooldown(creds *Credentials) []string {
	if p == nil {
		return nil
	}
	now := time.Now()
	var cleared []string

	p.rot.mu.Lock()
	idx := p.indexLocked(creds)
	if idx >= 0 {
		if now.Before(p.rot.cooling[idx]) {
			p.rot.cooling[idx] = time.Time{}
			cleared = append(cleared, "rotation")
		}
		p.quotaMu.Lock()
		if now.Before(p.quotaCoolUntil[idx]) {
			p.quotaCoolUntil[idx] = time.Time{}
			cleared = append(cleared, "quota")
		}
		p.quotaMu.Unlock()
	}
	p.rot.mu.Unlock()

	return cleared
}

// QuotaHeld reports how many accounts are currently out of rotation on quota
// evidence, for /healthz. A hold stops counting the moment its deadline passes,
// since the account is back in rotation at that point whatever the status said.
func (p *Pool) QuotaHeld() int {
	if p == nil {
		return 0
	}
	now := time.Now()
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()
	n := 0
	for i := range p.quotaCoolUntil {
		if now.Before(p.quotaCoolUntil[i]) {
			n++
		}
	}
	return n
}

// quotaHeldLocked reports whether a slot is currently held out on quota evidence,
// which is what /healthz counts and what the pool-wide guard exempts. Call it with
// quotaMu held.
func (p *Pool) quotaHeldLocked(idx int, now time.Time) bool {
	return idx >= 0 && idx < len(p.quotaCoolUntil) && now.Before(p.quotaCoolUntil[idx])
}

// quotaHeldSnapshot reports, per entry, whether it is out of rotation on quota
// evidence. It is read under the rotation lock by Report's pool-wide guard, which
// needs the whole set at once to decide what it may pull back.
func (p *Pool) quotaHeldSnapshot(now time.Time) []bool {
	held := make([]bool, len(p.entries))
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()
	for i := range held {
		held[i] = p.quotaHeldLocked(i, now)
	}
	return held
}

// refreshQuota asks the backend how much quota the account has left and hands the
// answer to applyQuota. It is only ever called after a request was refused.
//
// That ordering is the whole safety argument. A failure here changes nothing at
// all — no status, no cooldown — so an unreachable or misparsed status can never
// bench a working account; the account keeps the cooldown the backend already
// earned. This is the opposite of a pre-flight quota check, which would bet an
// account's place in rotation on a number nobody has verified.
//
// An account already held out until a known reset is not asked about again: the
// answer would be the same until that reset, so re-asking would only turn a
// request arriving while the pool is exhausted into another status call.
func (p *Pool) refreshQuota(creds *Credentials) {
	if p == nil {
		return
	}
	p.rot.mu.Lock()
	idx := p.indexLocked(creds)
	p.rot.mu.Unlock()
	if idx < 0 {
		return
	}
	p.quotaMu.Lock()
	fn := p.status
	busy := fn == nil || p.busy[idx] || p.quotaHeldLocked(idx, time.Now())
	if !busy {
		p.busy[idx] = true
	}
	p.quotaMu.Unlock()
	if busy {
		return
	}

	go func() {
		defer p.releaseBusy(creds)
		ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
		defer cancel()
		st, err := fn(ctx, creds)
		if err != nil {
			log.Printf("account (…%s) quota status unavailable (%v); keeping its existing cooldown",
				tail(creds.APIKey), err)
			return
		}
		// The answer names the account, so keep it: a list that can show an email
		// instead of a token tail gets it for free from the calls the cooldown
		// logic was making anyway.
		p.Remember(creds, st)
		p.applyQuota(creds, st)
	}()
}

// releaseBusy clears the in-flight mark for creds, if the pool still holds it.
// The index captured when the fetch started is not usable here: a Remove during
// the fetch splices busy and renumbers the survivors.
func (p *Pool) releaseBusy(creds *Credentials) {
	p.rot.mu.Lock()
	idx := p.indexLocked(creds)
	if idx >= 0 && idx < len(p.busy) {
		p.quotaMu.Lock()
		p.busy[idx] = false
		p.quotaMu.Unlock()
	}
	p.rot.mu.Unlock()
}

// applyQuota holds an account out until its quota resets, when the status says
// some counter is empty. It only ever extends a cooldown: a slot that has already
// served requests again is left alone, however stale the evidence.
func (p *Pool) applyQuota(creds *Credentials, st *AccountStatus) {
	now := time.Now()
	until := st.ResetDeadline(now)
	if until.IsZero() {
		return
	}
	if limit := now.Add(MaxQuotaCool); until.After(limit) {
		until = limit
	}

	// Resolve the live slot and act on it under one holding of the rotation
	// lock: the status this answers was fetched over a window in which the
	// account may have been removed, or the list renumbered around it.
	p.rot.mu.Lock()
	idx := p.indexLocked(creds)
	extended := idx >= 0 && p.rot.extendCooling(idx, now, until)
	if extended {
		p.quotaMu.Lock()
		p.quotaCoolUntil[idx] = until
		p.quotaMu.Unlock()
	}
	total := len(p.entries)
	p.rot.mu.Unlock()
	if !extended {
		return
	}

	log.Printf("account %d/%d (%s) reports no quota left; holding it out until %s instead of "+
		"retrying every %s", idx+1, total, st, until.UTC().Format(time.RFC3339), CoolRateLimited)
}

// isContextFailure reports whether a request failed because it was cancelled or
// ran out of time, which is never a fault of the credential or the route it took.
func isContextFailure(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded)
}

// statusFromConnectCode maps a Connect error code onto the HTTP status the same
// condition would carry, so that the cooldown policy is decided in one place
// rather than at each call site. Code 0 means "nothing to learn from this".
func statusFromConnectCode(code string) int {
	switch code {
	case "unauthenticated":
		return 401
	case "permission_denied":
		return 403
	case "resource_exhausted":
		return 429
	default:
		return 0
	}
}

// tail returns the last few characters of a secret, enough to tell two accounts
// apart in a log without writing the credential itself.
func tail(s string) string {
	const n = 6
	if len(s) <= n {
		return "…"
	}
	return s[len(s)-n:]
}

// TokensFromFile reads a token list, one per line, ignoring blanks and comments.
func TokensFromFile(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if tok := cleanToken(line); tok != "" {
			out = append(out, tok)
		}
	}
	return out, nil
}
