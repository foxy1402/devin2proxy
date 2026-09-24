package devin

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// The egress pool rotates the outbound route the way the account pool rotates
// credentials: one route per request. The reason is address diversity â€” the
// backend meters per account today and does not meter per address, so this is
// insurance against a per-IP limit appearing, and against one exit being blocked
// or throttled while others still work.
//
// The two pools rotate independently. With N accounts and M routes, consecutive
// requests walk both lists, so traffic is spread over the accounts and over the
// routes without the proxy needing to track pairings.

// Egress is one outbound route: a proxy to send requests through. A route built
// with no proxy is the machine's own connection, which is what a nil Next result
// means.
//
// The configured URL is deliberately not kept: it can carry credentials, and
// label plus transport is everything the rest of the package needs.
type Egress struct {
	label string
	tr    *http.Transport
}

// String names the route for logs and errors. Any credentials in the configured
// URL are absent from the label, so this is safe to print.
func (e *Egress) String() string { return e.label }

// EgressError reports a failure to reach the backend that lies in the route
// itself, rather than in the request or the backend.
//
// Broken is the pool's cooldown signal, and it is deliberately narrow: true only
// when the route is *confirmed* unusable â€” it could not be dialled, it refused the
// handshake, or it refused our credentials. A failure raised by the destination
// (an HTTP status, a Connect error, a 429) leaves it false, because a route that
// delivered a response has just proved it works.
type EgressError struct {
	Egress *Egress
	Op     string
	Err    error
	Broken bool
}

func (e *EgressError) Error() string {
	// A route is normally attached by the dialer that raised this. The unattributed
	// case is a programming error rather than a runtime one, and says so instead of
	// claiming the failure was on the direct connection.
	if e.Egress == nil {
		return fmt.Sprintf("devin: unattributed route error: %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("devin: route %s: %s: %v", e.Egress.label, e.Op, e.Err)
}

func (e *EgressError) Unwrap() error { return e.Err }

// RouteProbe is what a test of one route found.
type RouteProbe struct {
	// Spec is the route as configured, with any credentials removed.
	Spec string `json:"spec"`
	// OK is true when a tunnel to the backend could be opened and TLS negotiated
	// through the route: the whole path a request would take, minus the request.
	OK bool `json:"ok"`
	// ExitIP is the address the route exits from, when an echo service could be
	// reached through it. It is the reason to run more than one route, so it is
	// worth a second round trip; an empty value means the echo failed, not that
	// the route is broken.
	ExitIP      string `json:"exit_ip,omitempty"`
	LatencyMS   int64  `json:"latency_ms,omitempty"`
	Error       string `json:"error,omitempty"`
	Credentials bool   `json:"credentials,omitempty"`
}

// ProbeEgress builds one route from a spec and tests it end to end: TCP to the
// proxy, its handshake, a tunnel to the backend, and a TLS handshake inside it.
// Nothing is sent to the backend, so it costs no quota.
//
// echoURL, when set, is fetched through the same route to report the exit
// address. It must be an endpoint that answers with the caller's address as
// plain text.
func ProbeEgress(ctx context.Context, spec string, opts EgressOptions, backend, echoURL string) RouteProbe {
	probe := RouteProbe{Spec: stripEgressCredentials(spec)}
	if backend == "" {
		backend = defaultAPIServerHost
	}
	// A probe is the one place these are not already defaulted by the caller, and
	// an unbounded dial here would hang a page rather than report a dead route.
	if opts.HeaderTimeout <= 0 {
		opts.HeaderTimeout = 120 * time.Second
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 20 * time.Second
	}
	e, err := newEgress(spec, opts)
	if err != nil {
		// newEgress already strips credentials from its errors (they reach the
		// dashboard page), so this is passed through rather than wrapped with the
		// raw spec.
		probe.Error = err.Error()
		return probe
	}
	probe.Credentials = e.label != "direct" && strings.Contains(spec, "@")

	start := time.Now()
	conn, err := dialEgress(ctx, e, backend, opts.DialTimeout)
	if err != nil {
		probe.Error = egressReason(err)
		return probe
	}
	// A handshake proves the tunnel carries real traffic rather than merely
	// accepting a connection, which is what a proxy that answers and then drops
	// everything would otherwise look like.
	tlsConn := tls.Client(conn, &tls.Config{ServerName: hostOnly(backend)})
	handshakeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = tlsConn.HandshakeContext(handshakeCtx)
	cancel()
	tlsConn.Close()
	if err != nil {
		probe.Error = "tunnel opened, but TLS through it failed: " + err.Error()
		return probe
	}
	probe.OK = true
	probe.LatencyMS = time.Since(start).Milliseconds()

	if echoURL != "" {
		if ip := fetchExitIP(ctx, e, echoURL); ip != "" {
			probe.ExitIP = ip
		}
	}
	return probe
}

// dialEgress opens the TCP connection a probe needs, using the route's own dialer
// where it has one.
//
// A direct route has no dialer: its transport is left on Go's default, which is
// reached through a nil DialContext rather than a function. Calling that field
// directly is a nil dereference â€” it panicked a live dashboard request â€” so the
// fallback here is the same dialer the transport would have used.
func dialEgress(ctx context.Context, e *Egress, addr string, timeout time.Duration) (net.Conn, error) {
	if e.tr.DialContext != nil {
		return e.tr.DialContext(ctx, "tcp", addr)
	}
	return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
}

// fetchExitIP asks an echo service what address it sees, through one route. An
// error is not worth reporting: the route has already been proven, and the echo
// service is somebody else's uptime.
func fetchExitIP(ctx context.Context, e *Egress, url string) string {
	client := &http.Client{Transport: e.tr, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil || resp.StatusCode != http.StatusOK {
		return ""
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

// egressReason reduces a dial failure to the part that says why, without the
// request context Go wraps around it.
func egressReason(err error) string {
	var egErr *EgressError
	if errors.As(err, &egErr) && egErr.Err != nil {
		return egErr.Err.Error()
	}
	return err.Error()
}

// CredentialMask is what replaces a route's user and password when a route is
// displayed. It is a fixed, recognisable string rather than a URL-escaped one, so
// it reads the same way it was typed and so the dashboard can put a route back
// together when the operator saves a list it never saw the credentials for.
const CredentialMask = "***:***"

// stripEgressCredentials replaces user:pass in a URL with CredentialMask, leaving
// the scheme and host intact. It works on the text rather than through url.URL,
// because a URL round trip escapes the mask into %2A%2A%2A and the masked route is
// meant to be readable â€” it is shown to a person and matched against what they
// pasted back.
func stripEgressCredentials(spec string) string {
	schemeEnd := strings.Index(spec, "://")
	if schemeEnd < 0 {
		return spec
	}
	rest := spec[schemeEnd+3:]
	// LastIndex, not Index: a password may legitimately contain an @.
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return spec
	}
	// A path or query may also contain an @, so the userinfo only counts as one
	// when it is before any slash.
	if slash := strings.IndexAny(rest, "/?#"); slash >= 0 && slash < at {
		return spec
	}
	return spec[:schemeEnd+3] + CredentialMask + "@" + rest[at+1:]
}

// HasMaskedCredentials reports whether a spec still carries the mask, which means
// the page it came from was never given the real credentials.
func HasMaskedCredentials(spec string) bool {
	return strings.Contains(spec, CredentialMask+"@")
}

// MaskSpec is stripEgressCredentials under a name that says what it is for: the
// form of a route that is safe to display.
func MaskSpec(spec string) string { return stripEgressCredentials(spec) }

func hostOnly(hostPort string) string {
	if i := strings.LastIndex(hostPort, ":"); i > 0 {
		return hostPort[:i]
	}
	return hostPort
}

// defaultAPIServerHost is the backend a probe dials: the same host requests go
// to, so a route that passes here is a route that will carry a completion.
const defaultAPIServerHost = "server.codeium.com:443"

// EgressPool rotates outbound routes one per request.
type EgressPool struct {
	list []*Egress
	// specs are the configured routes with any credentials removed, kept so a
	// listing can name the route an operator configured without the password in it
	// ever leaving the process.
	specs []string
	rot   rotationCursor
	// last is the index Next handed out most recently, or -1 before anything has
	// been sent. Its only reader is a listing, which uses it to show the order
	// requests are walking in; a fresh pool must not claim a route was used.
	last int
}

// RouteState is one route as a listing needs it: what it is, whether it can be
// used right now, and for how much longer it cannot.
type RouteState struct {
	Index int    `json:"index"`
	Label string `json:"label"`
	// Spec is the configured route with any credentials replaced by a placeholder.
	Spec string `json:"spec,omitempty"`
	// Available is false while the route is out of rotation after a failure.
	Available    bool       `json:"available"`
	CoolingUntil *time.Time `json:"cooling_until,omitempty"`
	// LastServed marks the route the most recent request went out through.
	LastServed bool `json:"last_served,omitempty"`
	// Direct marks the machine's own connection, listed when the route list mixes
	// `direct` in with proxies.
	Direct bool `json:"direct,omitempty"`
}

// States reports every route and its place in the rotation. It is display data:
// nothing in the proxy consults it, so a snapshot a few microseconds stale is
// not worth a wider lock.
//
// A pool with no routes reports an empty list rather than nil, because this is
// JSON-bound: nil marshals to null, and a client that expects a list and gets
// null has to defend against it. An empty list is the honest answer here.
func (p *EgressPool) States() []RouteState {
	if p == nil {
		return []RouteState{}
	}
	now := time.Now()
	p.rot.mu.Lock()
	cooling := make([]time.Time, len(p.rot.cooling))
	copy(cooling, p.rot.cooling)
	last := p.last
	p.rot.mu.Unlock()

	out := make([]RouteState, 0, len(p.list))
	for i, e := range p.list {
		st := RouteState{
			Index:      i,
			Label:      e.label,
			Available:  i >= len(cooling) || !now.Before(cooling[i]),
			LastServed: i == last,
			Direct:     e.label == "direct",
		}
		if i < len(p.specs) {
			st.Spec = p.specs[i]
		}
		if i < len(cooling) && now.Before(cooling[i]) {
			until := cooling[i]
			st.CoolingUntil = &until
		}
		out = append(out, st)
	}
	return out
}

// Specs returns the configured routes with credentials removed, in rotation
// order. It is what a listing shows when the pool came from a file or the
// environment rather than from the dashboard.
func (p *EgressPool) Specs() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.specs...)
}

// CoolEgress is how long a route that is confirmed unusable stays out of
// rotation. It is matched to the account cooldown: long enough that a dead exit
// stops costing a wasted attempt on most requests, short enough that a repaired
// one comes back without a restart.
const CoolEgress = 5 * time.Minute

// EgressOptions configures the transports the pool builds.
type EgressOptions struct {
	// HeaderTimeout bounds the wait for response headers through the route.
	HeaderTimeout time.Duration
	// DialTimeout bounds the TCP connect to the proxy and its handshake, so a
	// black-holed exit cannot hang a request indefinitely.
	DialTimeout time.Duration
}

// NewEgressPool parses route specs and builds one transport per route. Blank
// entries and `#` comments are skipped, so a hand-maintained list can be pasted
// in. No routes configured returns a nil pool, which means the machine's own
// connection and costs a nil check at every use site rather than an if-else.
func NewEgressPool(specs []string, opts EgressOptions) (*EgressPool, error) {
	if opts.HeaderTimeout <= 0 {
		opts.HeaderTimeout = 120 * time.Second
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 20 * time.Second
	}
	p := &EgressPool{last: -1}
	for _, raw := range specs {
		spec := strings.TrimSpace(raw)
		if spec == "" || strings.HasPrefix(spec, "#") {
			continue
		}
		e, err := newEgress(spec, opts)
		if err != nil {
			return nil, err
		}
		p.list = append(p.list, e)
		p.specs = append(p.specs, stripEgressCredentials(spec))
	}
	if len(p.list) == 0 {
		return nil, nil
	}
	p.rot = newRotationCursor(len(p.list))
	return p, nil
}

// Len reports how many routes the pool holds.
func (p *EgressPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.list)
}

// Available reports how many routes are currently in rotation.
func (p *EgressPool) Available() int {
	if p == nil {
		return 0
	}
	p.rot.mu.Lock()
	defer p.rot.mu.Unlock()
	return p.rot.availableLocked(time.Now())
}

// Next returns the next route in rotation, or nil to use the machine's own
// connection.
func (p *EgressPool) Next() *Egress {
	if p == nil || len(p.list) == 0 {
		return nil
	}
	p.rot.mu.Lock()
	defer p.rot.mu.Unlock()
	idx := p.rot.pick(time.Now())
	p.last = idx
	return p.list[idx]
}

// Report records the outcome of a request that went out through e. Unlike the
// account pool this is *not* driven by HTTP status: a status came back through
// the route, which means the route works. Only an EgressError marked Broken,
// meaning the route could not be used at all, takes it out of rotation.
//
// A cancelled or timed-out request is ignored for the same reason it is ignored
// for an account: it reports the client giving up, not a fault.
func (p *EgressPool) Report(e *Egress, err error) {
	if p == nil || e == nil {
		return
	}
	if isContextFailure(err) {
		return
	}
	var egErr *EgressError
	if !errors.As(err, &egErr) || !egErr.Broken {
		return
	}

	p.rot.mu.Lock()
	defer p.rot.mu.Unlock()
	idx := -1
	for i, candidate := range p.list {
		if candidate == e {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	now := time.Now()
	if p.rot.coolOff(idx, now, now.Add(CoolEgress)) {
		return
	}
	log.Printf("route %d/%d (%s) stepped out of rotation for %s: %v; "+
		"%d/%d routes still available",
		idx+1, len(p.list), e.label, CoolEgress, egErr.Err,
		p.rot.availableLocked(now), len(p.list))
}

// newEgress parses one spec into a route with its own transport.
//
//	socks5://[user:pass@]host:port    SOCKS5, hostname resolved by the proxy
//	socks5h://[user:pass@]host:port   the same, spelled the way curl spells it
//	http://[user:pass@]host:port      HTTP CONNECT
//	https://[user:pass@]host:port     HTTP CONNECT over TLS to the proxy
//	direct                            the machine's own connection, to mix with proxies
func newEgress(spec string, opts EgressOptions) (*Egress, error) {
	if spec == "direct" || spec == "direct://" {
		return &Egress{label: "direct", tr: newRouteTransport(nil, opts)}, nil
	}

	u, err := url.Parse(spec)
	if err != nil {
		// The spec may carry user:pass, and this error reaches the dashboard page
		// and the log view — both places credentials must never appear. Strip it
		// the same way a displayed route is stripped.
		return nil, fmt.Errorf("egress %q: %w", MaskSpec(spec), err)
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http", "https":
	default:
		return nil, fmt.Errorf("egress %q: unsupported scheme %q "+
			"(use socks5://, socks5h://, http://, https://, or the word direct)", MaskSpec(spec), u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("egress %q: no host:port", MaskSpec(spec))
	}
	if _, port := u.Hostname(), u.Port(); port == "" {
		return nil, fmt.Errorf("egress %q: no port", MaskSpec(spec))
	} else if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("egress %q: bad port %q", MaskSpec(spec), port)
	}

	var user, pass string
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}

	scheme, proxyAddr := u.Scheme, u.Host
	// The label is what reaches logs and error messages, so it carries the
	// scheme and address but never the credentials.
	e := &Egress{label: scheme + "://" + proxyAddr}

	// The dialer is handed the route it belongs to so the errors it raises name
	// it. That is why the route is built before its transport rather than in one
	// literal.
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		switch scheme {
		case "socks5", "socks5h":
			return dialSOCKS5(e, ctx, proxyAddr, user, pass, addr, opts.DialTimeout)
		default:
			return dialHTTPConnect(e, ctx, scheme, proxyAddr, user, pass, addr, opts.DialTimeout)
		}
	}
	e.tr = newRouteTransport(dial, opts)
	return e, nil
}

// newRouteTransport builds the transport for one route. When dial is nil the
// transport is left on Go's default dialer, which is what the proxy used before
// routes existed, so the direct path is unchanged.
func newRouteTransport(dial func(context.Context, string, string) (net.Conn, error), opts EgressOptions) *http.Transport {
	tr := &http.Transport{
		// The CLI forces HTTP/1.1 against this backend, so match it instead of
		// gambling on the server's HTTP/2 handling.
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: opts.HeaderTimeout,
	}
	if dial != nil {
		// No Proxy field: DialContext already owns the route, and setting both
		// would try to tunnel through the proxy twice.
		tr.DialContext = dial
	}
	return tr
}

// dialSOCKS5 opens a TCP stream to addr through a SOCKS5 proxy (RFC 1928), with
// username/password auth when the URL carries it (RFC 1929).
//
// The hostname is always sent to the proxy to resolve (address type 0x03) rather
// than resolved here. That is what the standard Go SOCKS5 dialer does, and it is
// the only option that keeps this machine's DNS out of the path.
func dialSOCKS5(e *Egress, ctx context.Context, proxyAddr, user, pass, target string, timeout time.Duration) (net.Conn, error) {
	conn, err := dialProxy(ctx, proxyAddr, timeout)
	if err != nil {
		return nil, broken(e, "dial proxy "+proxyAddr, err)
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	// Bound the handshake so a black-holed exit cannot hold the request open. The
	// deadline is cleared once the tunnel is up, because the generation that
	// follows legitimately runs for minutes.
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := socks5Handshake(e, conn, user, pass, target); err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return conn, nil
}

func socks5Handshake(e *Egress, conn net.Conn, user, pass, target string) error {
	// Greeting: offer no-auth, and username/password when we have credentials.
	methods := []byte{0x00}
	if user != "" {
		methods = append(methods, 0x02)
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return broken(e, "socks5 greeting", err)
	}
	var choice [2]byte
	if _, err := io.ReadFull(conn, choice[:]); err != nil {
		return broken(e, "socks5 greeting reply", err)
	}
	if choice[0] != 0x05 {
		return broken(e, "socks5 greeting reply", fmt.Errorf("not a SOCKS5 proxy (version %d)", choice[0]))
	}
	switch choice[1] {
	case 0x00:
	case 0x02:
		if user == "" {
			return broken(e, "socks5 auth", errors.New("proxy requires a username and password"))
		}
		// RFC 1929: version, then both fields length-prefixed.
		msg := []byte{0x01, byte(len(user))}
		msg = append(msg, user...)
		msg = append(msg, byte(len(pass)))
		msg = append(msg, pass...)
		if _, err := conn.Write(msg); err != nil {
			return broken(e, "socks5 auth", err)
		}
		var authReply [2]byte
		if _, err := io.ReadFull(conn, authReply[:]); err != nil {
			return broken(e, "socks5 auth reply", err)
		}
		if authReply[1] != 0x00 {
			// The route is working and refusing us: confirmed unusable as
			// configured, so it earns a cooldown.
			return broken(e, "socks5 auth", errors.New("proxy rejected the username or password"))
		}
	case 0xFF:
		return broken(e, "socks5 auth", errors.New("proxy accepts none of the offered auth methods"))
	default:
		return broken(e, "socks5 auth", fmt.Errorf("proxy selected unsupported auth method 0x%02x", choice[1]))
	}

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return &EgressError{Egress: e, Op: "socks5 target", Err: err}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return &EgressError{Egress: e, Op: "socks5 target", Err: err}
	}
	req := []byte{0x05, 0x01, 0x00} // VER, CONNECT, RSV
	switch ip := net.ParseIP(host); {
	case ip == nil:
		if len(host) > 255 {
			return &EgressError{Egress: e, Op: "socks5 target", Err: fmt.Errorf("hostname too long: %d bytes", len(host))}
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	case ip.To4() != nil:
		req = append(req, 0x01)
		req = append(req, ip.To4()...)
	default:
		req = append(req, 0x04)
		req = append(req, ip.To16()...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return broken(e, "socks5 connect", err)
	}

	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return broken(e, "socks5 connect reply", err)
	}
	if reply[0] != 0x05 {
		return broken(e, "socks5 connect reply", fmt.Errorf("not a SOCKS5 reply (version %d)", reply[0]))
	}
	if reply[1] != 0x00 {
		return socks5ReplyError(e, reply[1])
	}
	// The reply carries the proxy's bound address, which we have no use for but
	// must consume to leave the stream at the start of the tunnel.
	var skip int
	switch reply[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		var n [1]byte
		if _, err := io.ReadFull(conn, n[:]); err != nil {
			return broken(e, "socks5 connect reply", err)
		}
		skip = int(n[0])
	default:
		return broken(e, "socks5 connect reply", fmt.Errorf("unknown address type 0x%02x", reply[3]))
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip)+2); err != nil {
		return broken(e, "socks5 connect reply", err)
	}
	return nil
}

// socks5ReplyError maps a SOCKS5 reply code onto an EgressError, splitting the
// codes that say the proxy is unusable from the ones that say the destination
// could not be reached. A route that got as far as reporting "connection
// refused" has proved it works, so it is not cooled for it.
func socks5ReplyError(e *Egress, code byte) error {
	reason := map[byte]string{
		0x01: "general SOCKS server failure",
		0x02: "connection not allowed by ruleset",
		0x03: "network unreachable",
		0x04: "host unreachable",
		0x05: "connection refused",
		0x06: "TTL expired",
		0x07: "command not supported",
		0x08: "address type not supported",
	}[code]
	if reason == "" {
		reason = fmt.Sprintf("unknown reply code 0x%02x", code)
	}
	err := errors.New("socks5 connect: " + reason)
	switch code {
	case 0x01, 0x02, 0x07, 0x08:
		// The proxy itself failed, refused the CONNECT, or cannot CONNECT to a
		// hostname at all â€” none of which a retry fixes.
		return broken(e, "socks5 connect", err)
	default:
		// 0x03, 0x04, 0x05, 0x06 all describe the path to the destination.
		return &EgressError{Egress: e, Op: "socks5 connect", Err: err}
	}
}

// dialHTTPConnect opens a TCP stream to addr through an HTTP proxy using the
// CONNECT method, which is the only method that carries a TLS session to the
// destination intact.
func dialHTTPConnect(e *Egress, ctx context.Context, scheme, proxyAddr, user, pass, target string, timeout time.Duration) (net.Conn, error) {
	conn, err := dialProxy(ctx, proxyAddr, timeout)
	if err != nil {
		return nil, broken(e, "dial proxy "+proxyAddr, err)
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	if scheme == "https" {
		host, _, _ := net.SplitHostPort(proxyAddr)
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		hsCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := tlsConn.HandshakeContext(hsCtx); err != nil {
			// TLS to the proxy itself failing is the route's fault: either it is
			// not speaking TLS on this port or something is intercepting it.
			return nil, broken(e, "tls to proxy", err)
		}
		conn = tlsConn
	}

	_ = conn.SetDeadline(time.Now().Add(timeout))
	// Written by hand rather than through http.Request.Write, so the CONNECT line
	// is visible here rather than depending on net/http's special-casing of the
	// method.
	head := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if user != "" || pass != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		head += "Proxy-Authorization: Basic " + cred + "\r\n"
	}
	if _, err := io.WriteString(conn, head+"\r\n"); err != nil {
		return nil, broken(e, "write CONNECT", err)
	}

	// ReadResponse consumes from this reader, so it is kept: it may have read past
	// the response headers into the tunnel, and those bytes are the start of the
	// TLS handshake with the destination.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, broken(e, "read CONNECT reply", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		message := "proxy answered " + resp.Status
		if detail := strings.TrimSpace(string(body)); detail != "" {
			message += ": " + strings.ReplaceAll(detail, "\n", " ")
		}
		err := errors.New(message)
		switch resp.StatusCode {
		case http.StatusProxyAuthRequired, http.StatusForbidden:
			// Refusing us is confirmed unusable, and a cooldown stops the pool
			// from offering it on every request.
			return nil, broken(e, "CONNECT", err)
		default:
			// Any other status came back *through* the route, which proves the
			// route works. A 429 especially: a rate limit seen through a proxy is
			// evidence the proxy is fine, so it must never take the route out of
			// rotation.
			return nil, &EgressError{Egress: e, Op: "CONNECT", Err: err}
		}
	}
	_ = conn.SetDeadline(time.Time{})

	ok = true
	// Reads come from br so nothing buffered past the headers is lost; writes go
	// straight to the socket. This is the whole tunnel from here on. A 200 to
	// CONNECT declares no body, so there is no response body to consume first.
	return &tunnelConn{Conn: conn, r: br}, nil
}

// tunnelConn reads through a buffered reader while writing to the socket, which
// is what a CONNECT tunnel needs after the reply headers have been parsed.
type tunnelConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *tunnelConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// dialProxy opens the TCP connection to the proxy itself. A failure here is
// always the route's fault, since nothing else is involved yet.
func dialProxy(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// RoutesFromFile reads a proxy list, one URL per line, ignoring blanks and
// comments. It is the route equivalent of TokensFromFile and shares that file's
// comment handling, so both lists can be maintained the same way.
func RoutesFromFile(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if spec := cleanToken(line); spec != "" {
			out = append(out, spec)
		}
	}
	return out, nil
}

// broken wraps err as a route failure that justifies a cooldown, unless the
// request itself was cancelled: a client that gave up mid-dial must not cost the
// route its place in the rotation.
func broken(e *Egress, op string, err error) error {
	if isContextFailure(err) {
		return &EgressError{Egress: e, Op: op, Err: err}
	}
	return &EgressError{Egress: e, Op: op, Err: err, Broken: true}
}

// withRoute names the route an error came through. The dialers name themselves,
// so this covers only what is raised after a route carried the request â€” a TLS
// failure or a timeout waiting for the backend's response headers â€” where the
// route is known but the dialer was not the one reporting.
//
// It never sets Broken: an error that got this far is not proof the route is
// unusable, whatever it says.
func withRoute(e *Egress, err error) error {
	if err == nil {
		return nil
	}
	var egErr *EgressError
	if errors.As(err, &egErr) {
		// Already attributed where it was raised, which knows more than we do.
		return err
	}
	return &EgressError{Egress: e, Op: "upstream request", Err: err}
}
