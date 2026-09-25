package dashboard

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store is the dashboard's own persistent state: the admin password's hash, what
// it has been asked to manage, and the record of failed logins.
//
// It is separate from config.json on purpose. config.json is the operator's file,
// hand-edited, and the README tells people to keep credentials out of it; this
// file is the dashboard's, written by the dashboard, and holds the tokens the
// dashboard was given. Both are 0600 and both are gitignored.
type Store struct {
	// Path is where the state lives. An empty path keeps it in memory, which is
	// what the tests use and what `-dashboard-password` does before writing one.
	Path string

	mu   sync.Mutex
	data storeData
}

const storeVersion = 1

type storeData struct {
	Version int `json:"version"`
	// Password is the salt and hash. The password itself is never stored anywhere,
	// and never logged.
	Password *passwordRecord `json:"password,omitempty"`
	// SessionSecret signs session cookies. Persisted so restarting the proxy does
	// not sign the operator out, and regenerated only if it is missing.
	SessionSecret string `json:"session_secret,omitempty"`
	// SessionEpoch invalidates every outstanding session at once. Logging out
	// bumps it, which is what makes "log out" mean log out rather than "clear my
	// cookie and hope".
	SessionEpoch int `json:"session_epoch,omitempty"`
	// Accounts are the tokens the dashboard manages, in rotation order. They are
	// the pool's contents, written back whenever the pool changes.
	Accounts []string `json:"accounts,omitempty"`
	// Proxies are the outbound routes the dashboard manages, exactly as pasted,
	// minus blank lines and comments.
	Proxies []string `json:"proxies,omitempty"`
	// Bans records failed logins by client address, so restarting the proxy does
	// not clear a punishment that was in force.
	Bans map[string]*banRecord `json:"bans,omitempty"`
}

type passwordRecord struct {
	Algo string `json:"algo"`
	Iter int    `json:"iter"`
	Salt string `json:"salt"`
	Hash string `json:"hash"`
}

// banRecord is one client address's failed-login history.
type banRecord struct {
	// Strikes is how many failures in a row without a success.
	Strikes int `json:"strikes"`
	// Until is when the current ban lifts; zero when there is no ban.
	Until time.Time `json:"until,omitempty"`
	// Last is when the most recent failure happened, used to forget a stale
	// history rather than punishing it forever.
	Last time.Time `json:"last,omitempty"`
}

// OpenStore reads the dashboard state, creating an empty one if the file does not
// exist yet. A file that exists but cannot be parsed is an error rather than a
// silent reset: it would otherwise throw away the managed accounts. So is a file
// written by a newer version of this program — decoding it with fields this
// version does not know and writing it back out would quietly drop them.
func OpenStore(path string) (*Store, error) {
	s := &Store{Path: path, data: storeData{Version: storeVersion, Bans: map[string]*banRecord{}}}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		// Version 0 is a file written before the field existed; anything above the
		// version this build knows is someone else's future and is refused rather
		// than half-understood.
		if s.data.Version > storeVersion {
			return nil, fmt.Errorf("%s: version %d is newer than this build understands (%d)",
				path, s.data.Version, storeVersion)
		}
		if s.data.Bans == nil {
			s.data.Bans = map[string]*banRecord{}
		}
		return s, nil
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	default:
		return nil, err
	}
}

// Save writes the state. It is called with the lock already held by the mutators
// below, and never by a reader.
func (s *Store) save() error {
	if s == nil || s.Path == "" {
		return nil
	}
	s.data.Version = storeVersion
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// Written beside the target and renamed over it, so a crash mid-write cannot
	// leave a half-written file where the accounts were. The temporary file gets a
	// unique name rather than a fixed one, so two writers — or a leftover from a
	// crash — cannot collide, and it carries 0600 like the file it becomes: it
	// holds the same credentials.
	f, err := os.CreateTemp(filepath.Dir(s.Path), filepath.Base(s.Path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// CreateTemp already opens with 0600; set explicitly so the mode is part of
	// the contract rather than an accident of the standard library.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	// Flushed to the disk before the rename: a rename that out-races its own data
	// is the one failure mode a crash can still produce here.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// SetPassword records a new password hash, replacing any previous one.
func (s *Store) SetPassword(rec *passwordRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Password = rec
	return s.save()
}

// Password returns the stored record, or nil when no password has been set.
func (s *Store) Password() *passwordRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Password == nil {
		return nil
	}
	cp := *s.data.Password
	return &cp
}

// SessionSecret returns the signing secret, generating one on first use so a
// session survives a restart without the operator having to configure anything.
func (s *Store) SessionSecret() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.SessionSecret == "" {
		secret, err := randomBytes(32)
		if err != nil {
			return nil, err
		}
		s.data.SessionSecret = base64.RawStdEncoding.EncodeToString(secret)
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	secret, err := base64.RawStdEncoding.DecodeString(s.data.SessionSecret)
	if err != nil {
		return nil, fmt.Errorf("stored session secret is not valid base64: %w", err)
	}
	return secret, nil
}

// SessionEpoch reports the current epoch and bumps it. Bumping invalidates every
// session that was issued before it.
func (s *Store) SessionEpoch() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.SessionEpoch
}

func (s *Store) BumpSessionEpoch() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.SessionEpoch++
	return s.data.SessionEpoch, s.save()
}

// Accounts returns the managed tokens.
func (s *Store) Accounts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.data.Accounts...)
}

// SetAccounts replaces the managed token list and writes it out.
func (s *Store) SetAccounts(tokens []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Accounts = append([]string(nil), tokens...)
	return s.save()
}

// Proxies returns the managed route list.
func (s *Store) Proxies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.data.Proxies...)
}

// HasProxies reports whether the store carries a proxies key at all, as opposed
// to carrying an empty one. The distinction matters for the route listing: an
// empty stored list is an operator's deliberate "no routes", while an absent key
// says the store has never managed them and the running pool's own list is worth
// showing instead.
func (s *Store) HasProxies() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Proxies != nil
}

// SetProxies replaces the managed route list and writes it out. An empty list is
// kept as an empty list rather than collapsed into "no key": clearing the routes
// is a decision the store should remember distinctly from never having managed
// them (see HasProxies).
func (s *Store) SetProxies(specs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Proxies = append(make([]string, 0, len(specs)), specs...)
	return s.save()
}

// banFor returns the stored record for an address, creating it on demand. Call it
// with the lock held.
func (s *Store) banFor(addr string) *banRecord {
	if s.data.Bans == nil {
		s.data.Bans = map[string]*banRecord{}
	}
	rec := s.data.Bans[addr]
	if rec == nil {
		rec = &banRecord{}
		s.data.Bans[addr] = rec
	}
	return rec
}

// forgetStaleBans drops records that are neither banned nor recently active, so
// the map cannot grow forever from addresses that tried once and went away.
func (s *Store) forgetStaleBans(now time.Time) {
	for addr, rec := range s.data.Bans {
		if rec.Until.After(now) {
			continue
		}
		if now.Sub(rec.Last) > staleAfter {
			delete(s.data.Bans, addr)
		}
	}
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}
