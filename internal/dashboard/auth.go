package dashboard

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The password is stored as PBKDF2-HMAC-SHA256, which is in the standard library
// and needs no dependency. The iteration count is the OWASP figure for this
// construction; it also happens to be a rate limit, since every attempt costs a
// few hundred milliseconds of CPU that an attacker pays too.
const (
	passwordAlgo       = "pbkdf2-sha256"
	passwordIterations = 210_000
	passwordKeyLen     = 32
	// MinPasswordLength is low on purpose: this proxy binds to loopback and its
	// dashboard guards a quota pool, not a bank. A long password is still better,
	// and the login is rate-limited either way.
	MinPasswordLength = 8
)

// Login throttling. Three failures earn a one-minute ban; every further failure
// doubles it, up to a day. The ban is per client address and persists across
// restarts, because a punishment that a restart clears is not a punishment.
const (
	banAfterFailures = 3
	firstBan         = time.Minute
	maxBan           = 24 * time.Hour
	// staleAfter is how long an address's history is kept once it is no longer
	// banned, so an address that mistyped once is not remembered forever.
	staleAfter = 24 * time.Hour
	// loginFailureDelay is a floor on how long a wrong password takes to answer.
	// The key derivation is the real cost; this only makes sure the answer does
	// not come back suspiciously fast when the stored hash is missing or short.
	loginFailureDelay = 150 * time.Millisecond
)

const (
	sessionCookie = "d2p_session"
	sessionTTL    = 12 * time.Hour
)

// sessionPayload is what a session cookie carries. It holds no identity, because
// there is only one user; its job is to be unforgeable and to expire.
type sessionPayload struct {
	Epoch int    `json:"e"`
	Exp   int64  `json:"x"`
	Nonce string `json:"n"`
}

// hashPassword derives a new record. A random salt per password means two
// dashboards with the same password share no hash.
func hashPassword(password string) (*passwordRecord, error) {
	salt, err := randomBytes(16)
	if err != nil {
		return nil, err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeyLen)
	if err != nil {
		return nil, err
	}
	return &passwordRecord{
		Algo: passwordAlgo,
		Iter: passwordIterations,
		Salt: base64.RawStdEncoding.EncodeToString(salt),
		Hash: base64.RawStdEncoding.EncodeToString(key),
	}, nil
}

// verifyPassword compares a candidate against a stored record in constant time
// with respect to the hash, so a wrong password cannot be narrowed down by
// timing.
func verifyPassword(rec *passwordRecord, password string) bool {
	if rec == nil || rec.Hash == "" || rec.Salt == "" {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(rec.Salt)
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(rec.Hash)
	if err != nil {
		return false
	}
	iter := rec.Iter
	if iter <= 0 {
		iter = passwordIterations
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// loginState is what the UI needs to explain a locked-out login.
type loginState struct {
	Banned bool      `json:"banned"`
	Until  time.Time `json:"until,omitempty"`
	// Strikes is how many failures are on record for this address.
	Strikes int `json:"strikes"`
	// Remaining is how many attempts are left before the next ban.
	Remaining int `json:"remaining"`
	// WaitSeconds is how long until the ban lifts, for a countdown.
	WaitSeconds int `json:"wait_seconds,omitempty"`
}

// loginStateFor reports the current state for a client address without changing
// anything, so the login page can show a countdown before a password is typed.
func (d *Dashboard) loginStateFor(addr string) loginState {
	now := time.Now()
	d.cfg.Store.mu.Lock()
	defer d.cfg.Store.mu.Unlock()
	rec := d.cfg.Store.data.Bans[addr]
	if rec == nil {
		return loginState{Remaining: banAfterFailures}
	}
	st := loginState{Strikes: rec.Strikes, Remaining: banAttemptsLeft(rec.Strikes)}
	if rec.Until.After(now) {
		st.Banned = true
		st.Until = rec.Until
		st.WaitSeconds = int(rec.Until.Sub(now).Seconds()) + 1
	}
	return st
}

// banAttemptsLeft is how many more failures are allowed before a ban begins.
func banAttemptsLeft(strikes int) int {
	if left := banAfterFailures - strikes; left > 0 {
		return left
	}
	return 0
}

// banDuration is the punishment for a given number of consecutive failures. The
// first two failures are free — mistyping a password twice is ordinary — and the
// third starts a doubling ladder that is capped so a mistyped password after a
// long ban cannot lock the owner out for a week.
func banDuration(strikes int) time.Duration {
	if strikes < banAfterFailures {
		return 0
	}
	ban := firstBan
	for i := banAfterFailures; i < strikes; i++ {
		ban *= 2
		if ban >= maxBan {
			return maxBan
		}
	}
	return ban
}

// recordLoginFailure counts one failed attempt and returns the state afterwards,
// including a new ban when the failure crossed the threshold.
func (d *Dashboard) recordLoginFailure(addr string) loginState {
	now := time.Now()
	d.cfg.Store.mu.Lock()
	rec := d.cfg.Store.banFor(addr)
	rec.Strikes++
	rec.Last = now
	if ban := banDuration(rec.Strikes); ban > 0 {
		rec.Until = now.Add(ban)
	}
	st := loginState{Strikes: rec.Strikes, Remaining: banAttemptsLeft(rec.Strikes)}
	if rec.Until.After(now) {
		st.Banned = true
		st.Until = rec.Until
		st.WaitSeconds = int(rec.Until.Sub(now).Seconds()) + 1
	}
	d.cfg.Store.forgetStaleBans(now)
	err := d.cfg.Store.save()
	d.cfg.Store.mu.Unlock()
	if err != nil {
		d.logf("dashboard: could not persist the login-failure record: %v", err)
	}
	return st
}

// recordLoginSuccess clears the address's failure history. A successful login is
// the only thing that does, so guessing slowly does not creep back to a clean
// slate.
func (d *Dashboard) recordLoginSuccess(addr string) {
	d.cfg.Store.mu.Lock()
	delete(d.cfg.Store.data.Bans, addr)
	err := d.cfg.Store.save()
	d.cfg.Store.mu.Unlock()
	if err != nil {
		d.logf("dashboard: could not persist the cleared login-failure record: %v", err)
	}
}

// newSession issues a signed cookie value: the payload and an HMAC over it. No
// server-side storage is needed, and the epoch inside ties it to the current
// login generation, which is what makes a logout take effect everywhere.
func (d *Dashboard) newSession(now time.Time) (string, error) {
	secret, err := d.cfg.Store.SessionSecret()
	if err != nil {
		return "", err
	}
	nonce, err := randomBytes(12)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(sessionPayload{
		Epoch: d.cfg.Store.SessionEpoch(),
		Exp:   now.Add(sessionTTL).Unix(),
		Nonce: base64.RawStdEncoding.EncodeToString(nonce),
	})
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// validSession reports whether a cookie value is one this dashboard issued, has
// not expired, and belongs to the current epoch.
func (d *Dashboard) validSession(value string, now time.Time) bool {
	secret, err := d.cfg.Store.SessionSecret()
	if err != nil {
		return false
	}
	payloadB64, sigB64, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	var p sessionPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	if p.Epoch != d.cfg.Store.SessionEpoch() {
		return false
	}
	return now.Unix() < p.Exp
}

func (d *Dashboard) setSessionCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     dashboardPrefix,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure only over TLS: setting it on plain HTTP would make the cookie
		// vanish on the loopback setup this is normally run as.
		Secure: r.TLS != nil,
		MaxAge: int(sessionTTL.Seconds()),
	})
}

func (d *Dashboard) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     dashboardPrefix,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   -1,
	})
}

// authenticated reports whether the request carries a valid session, in the
// cookie or as an Authorization: Bearer header.
//
// The header path exists because the cookie alone is not portable: to a
// browser, http://127.0.0.1 is a "trustworthy" origin and an insecure cookie
// is accepted without comment, while http://<public-ip> is not — a
// privacy-configured browser can simply refuse to store the cookie, and the
// login then loops forever: the server signs the client in, the client never
// keeps the proof. The login response therefore hands the same value to the
// page as JSON, and the page sends it as a header on every call afterwards.
// The cookie is still set (browsers that keep it keep working, and a value
// held only in JavaScript is one XSS away from being stolen, so both rails
// run at once deliberately). A bearer header is also immune to CSRF by
// construction, which the Origin check already covers.
func (d *Dashboard) authenticated(r *http.Request) bool {
	if c, err := r.Cookie(sessionCookie); err == nil && d.validSession(c.Value, time.Now()) {
		return true
	}
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return d.validSession(strings.TrimSpace(token), time.Now())
	}
	return false
}

// clientAddr is the key a ban is filed under: the address, without the port, so
// a browser opening a new connection does not get a fresh allowance.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "" {
		host = "unknown"
	}
	return host
}

// isLoopbackAddr reports whether an address is on this machine.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ErrPasswordTooShort is returned when a configured password is below the floor.
var ErrPasswordTooShort = errors.New("dashboard password is too short")

// SetPasswordText hashes a password chosen by the operator and stores the hash,
// replacing any previous one. The password itself is not kept.
func (s *Store) SetPasswordText(password string) error {
	if len(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	rec, err := hashPassword(password)
	if err != nil {
		return err
	}
	return s.SetPassword(rec)
}

// GeneratePassword makes a random password and stores its hash, returning the
// password itself so the caller can show it once. This is the fallback for a
// process started with no password configured: only the hash is persisted, so the
// value lives in exactly one place — the line that printed it — and a restart
// cannot reveal it.
//
// The alphabet leaves out the characters that are easy to misread when a password
// is copied by eye out of a terminal: no O/0, no I/l/1.
func (s *Store) GeneratePassword() (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	const length = 20
	raw, err := randomBytes(length)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, b := range raw {
		sb.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	password := sb.String()
	if err := s.SetPasswordText(password); err != nil {
		return "", err
	}
	return password, nil
}

// loginLockedMessage explains a ban in the log, without saying which password was
// tried or how close it was.
func loginLockedMessage(addr string, st loginState) string {
	return fmt.Sprintf("dashboard: login from %s refused; banned for another %s after %d failed attempt(s)",
		addr, st.Until.Sub(time.Now()).Round(time.Second), st.Strikes)
}

// parsePositiveInt is a small helper for numeric form fields.
func parsePositiveInt(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
