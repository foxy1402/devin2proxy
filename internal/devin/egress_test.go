package devin

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- test proxies -------------------------------------------------------
//
// Both proxies are real listeners rather than mocks of our dialers, so the tests
// exercise the handshakes and the tunnel rather than our idea of them. They also
// record the target each request asked for, which is how the "does traffic
// actually go through the route" assertions are made.

type fakeSOCKS5Options struct {
	// requireAuth makes the proxy demand a username and password.
	requireAuth bool
	user, pass  string
	// reply overrides the CONNECT reply code, e.g. 0x05 for "connection refused".
	reply byte
}

type fakeSOCKS5 struct {
	ln   net.Listener
	opts fakeSOCKS5Options

	mu       sync.Mutex
	targets  []string
	refusals int
}

func startFakeSOCKS5(t *testing.T, opts fakeSOCKS5Options) *fakeSOCKS5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSOCKS5{ln: ln, opts: opts}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	return s
}

func (s *fakeSOCKS5) addr() string { return s.ln.Addr().String() }

func (s *fakeSOCKS5) summary() (targets []string, refusals int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.targets...), s.refusals
}

func (s *fakeSOCKS5) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)

	ver, err := br.ReadByte()
	if err != nil || ver != 0x05 {
		return
	}
	n, err := br.ReadByte()
	if err != nil {
		return
	}
	methods := make([]byte, n)
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}

	method := byte(0x00)
	if s.opts.requireAuth {
		method = 0x02
		offered := false
		for _, m := range methods {
			if m == 0x02 {
				offered = true
				break
			}
		}
		if !offered {
			c.Write([]byte{0x05, 0xFF})
			return
		}
	}
	if _, err := c.Write([]byte{0x05, method}); err != nil {
		return
	}

	if method == 0x02 { // RFC 1929
		var hdr [2]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return
		}
		user := make([]byte, hdr[1])
		if _, err := io.ReadFull(br, user); err != nil {
			return
		}
		var plen [1]byte
		if _, err := io.ReadFull(br, plen[:]); err != nil {
			return
		}
		pass := make([]byte, plen[0])
		if _, err := io.ReadFull(br, pass); err != nil {
			return
		}
		if string(user) != s.opts.user || string(pass) != s.opts.pass {
			c.Write([]byte{0x01, 0x01}) // rejected
			return
		}
		c.Write([]byte{0x01, 0x00})
	}

	var head [4]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return
	}
	host, err := readSOCKSAddr(br, head[3])
	if err != nil {
		return
	}
	var portB [2]byte
	if _, err := io.ReadFull(br, portB[:]); err != nil {
		return
	}
	port := int(portB[0])<<8 | int(portB[1])

	s.mu.Lock()
	s.targets = append(s.targets, net.JoinHostPort(host, strconv.Itoa(port)))
	s.mu.Unlock()

	if s.opts.reply != 0 {
		s.mu.Lock()
		s.refusals++
		s.mu.Unlock()
		c.Write([]byte{0x05, s.opts.reply, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}

	up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	pipe(br, c, up)
}

func readSOCKSAddr(br *bufio.Reader, atyp byte) (string, error) {
	switch atyp {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case 0x04:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(br, l[:]); err != nil {
			return "", err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("bad atyp 0x%02x", atyp)
	}
}

// pipe copies in both directions until either side stops. Reads come from br,
// which may already hold bytes past the handshake.
func pipe(br *bufio.Reader, client net.Conn, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, br); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

type fakeHTTPProxy struct {
	ln net.Listener
	// status, when set, is answered instead of establishing a tunnel.
	status int
	// requireAuth answers 407 unless the right Proxy-Authorization arrives.
	requireAuth bool
	user, pass  string

	mu      sync.Mutex
	targets []string
}

func startFakeHTTPProxy(t *testing.T, status int, requireAuth bool, user, pass string) *fakeHTTPProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &fakeHTTPProxy{ln: ln, status: status, requireAuth: requireAuth, user: user, pass: pass}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handle(conn)
		}
	}()
	return p
}

func (p *fakeHTTPProxy) addr() string { return p.ln.Addr().String() }

func (p *fakeHTTPProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

func (p *fakeHTTPProxy) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n")
		return
	}
	p.mu.Lock()
	p.targets = append(p.targets, req.Host)
	p.mu.Unlock()

	if p.requireAuth {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(p.user+":"+p.pass))
		if req.Header.Get("Proxy-Authorization") != want {
			io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n"+
				"Proxy-Authenticate: Basic realm=\"test\"\r\nContent-Length: 0\r\n\r\n")
			return
		}
	}
	if p.status != 0 {
		fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", p.status, http.StatusText(p.status))
		return
	}

	up, err := net.Dial("tcp", req.Host)
	if err != nil {
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer up.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	pipe(br, c, up)
}

// ---- fake Connect backend ----------------------------------------------

// startFakeBackend answers any request with one Connect data frame carrying
// delta_text "ok" and a clean end-of-stream, which is what the proxy's stream
// reader expects from a successful call. status, when non-zero, is returned
// instead as a plain HTTP error.
func startFakeBackend(t *testing.T, status int) (addr string, hits *int32) {
	t.Helper()
	var mu sync.Mutex
	var count int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	body := append(
		EncodeFrame(FrameData, []byte{0x1a, 0x02, 'o', 'k'}), // field 3, delta_text "ok"
		EncodeFrame(FrameEndStream, []byte("{}"))...,
	)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		count++
		mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		w.Write(body)
	})}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(ln)

	return ln.Addr().String(), &count
}

func waitHits(t *testing.T, hits *int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(hits) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("backend saw %d request(s), want %d", atomic.LoadInt32(hits), want)
}

// ---- spec parsing -------------------------------------------------------

func TestNewEgressPoolParsesSpecs(t *testing.T) {
	pool, err := NewEgressPool([]string{
		"socks5://127.0.0.1:1080",
		"socks5h://user:pass@proxy.example:1081",
		"http://127.0.0.1:3128",
		"https://user:pass@proxy.example:8443",
		"direct",
		"# a comment",
		"",
		"   ",
	}, EgressOptions{})
	if err != nil {
		t.Fatalf("NewEgressPool: %v", err)
	}
	if got := pool.Len(); got != 5 {
		t.Fatalf("Len = %d, want 5 (comments and blanks skipped)", got)
	}
	labels := map[string]bool{}
	for i := 0; i < pool.Len(); i++ {
		labels[pool.Next().String()] = true
	}
	for _, want := range []string{
		"socks5://127.0.0.1:1080",
		"socks5h://proxy.example:1081",
		"http://127.0.0.1:3128",
		"https://proxy.example:8443",
		"direct",
	} {
		if !labels[want] {
			t.Errorf("label %q missing from %v", want, labels)
		}
	}
	// A label reaches logs, so credentials must never appear in it.
	for label := range labels {
		if strings.Contains(label, "user") || strings.Contains(label, "pass") {
			t.Errorf("label %q leaks credentials", label)
		}
	}
}

func TestNewEgressPoolRejectsBadSpecs(t *testing.T) {
	cases := map[string]string{
		"no scheme":    "127.0.0.1:1080",
		"bad scheme":   "ftp://127.0.0.1:21",
		"no port":      "socks5://127.0.0.1",
		"bad port":     "socks5://127.0.0.1:99999",
		"non-numeric":  "socks5://127.0.0.1:abc",
		"missing host": "socks5://",
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewEgressPool([]string{spec}, EgressOptions{}); err == nil {
				t.Fatalf("NewEgressPool(%q) accepted a spec it should reject", spec)
			}
		})
	}
}

func TestNewEgressPoolEmpty(t *testing.T) {
	pool, err := NewEgressPool(nil, EgressOptions{})
	if err != nil {
		t.Fatalf("NewEgressPool: %v", err)
	}
	if pool != nil {
		t.Fatal("an empty spec list should give a nil pool, so the direct path needs no special case")
	}
	// Every method must tolerate that nil.
	if pool.Len() != 0 || pool.Available() != 0 || pool.Next() != nil {
		t.Fatal("nil pool misbehaved")
	}
	pool.Report(nil, errors.New("x"))
}

func TestEgressPoolRotatesOnePerRequest(t *testing.T) {
	pool, err := NewEgressPool([]string{
		"socks5://127.0.0.1:1080",
		"http://127.0.0.1:3128",
		"direct",
	}, EgressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The same contract as the account pool: one route per request, in order,
	// wrapping around.
	var seen []string
	for i := 0; i < 6; i++ {
		seen = append(seen, pool.Next().String())
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] == seen[i-1] {
			t.Fatalf("route repeated back to back: %v", seen)
		}
	}
	if seen[0] != seen[3] || seen[1] != seen[4] || seen[2] != seen[5] {
		t.Fatalf("not a 3-cycle: %v", seen)
	}
}

// ---- cooldown rules ----------------------------------------------------

func TestEgressReportCoolsOnlyOnBrokenRoute(t *testing.T) {
	// This is the rule that keeps a working proxy in rotation: anything that came
	// back through the route proves the route works, however unwelcome it is.
	cases := []struct {
		name   string
		err    func(*Egress) error
		cooled bool
	}{
		{"dial failure is the route's fault", func(e *Egress) error { return broken(e, "dial", errors.New("connection refused")) }, true},
		{"destination 429 is not", func(*Egress) error { return &HTTPError{Status: 429, Body: []byte("slow down")} }, false},
		{"destination 500 is not", func(*Egress) error { return &HTTPError{Status: 500} }, false},
		{"credential refusal is not", func(*Egress) error {
			return &ConnectError{Code: "unauthenticated", Message: "bad token"}
		}, false},
		{"client abort is not", func(*Egress) error { return context.Canceled }, false},
		{"deadline is not", func(*Egress) error { return context.DeadlineExceeded }, false},
		{"destination unreachable is not", func(e *Egress) error {
			return &EgressError{Egress: e, Op: "socks5 connect", Err: errors.New("host unreachable")}
		}, false},
		{"plain error is not", func(*Egress) error { return errors.New("something else") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := NewEgressPool([]string{"socks5://127.0.0.1:1080", "http://127.0.0.1:3128"}, EgressOptions{})
			if err != nil {
				t.Fatal(err)
			}
			route := pool.Next()
			for i := 0; i < 3; i++ {
				pool.Report(route, tc.err(route))
			}
			want := 2
			if tc.cooled {
				want = 1
			}
			if got := pool.Available(); got != want {
				t.Fatalf("Available = %d, want %d", got, want)
			}
		})
	}
}

func TestEgressReportIgnoresForeignRoute(t *testing.T) {
	pool, err := NewEgressPool([]string{"socks5://127.0.0.1:1080"}, EgressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A route from another pool, or none at all.
	stranger, err := NewEgressPool([]string{"http://127.0.0.1:3128"}, EgressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pool.Report(stranger.Next(), broken(stranger.Next(), "dial", errors.New("nope")))
	pool.Report(nil, broken(nil, "dial", errors.New("nope")))
	if got := pool.Available(); got != 1 {
		t.Fatalf("a foreign route cooled slot 0 (%d available)", got)
	}
}

func TestSocks5ReplyCodesSplitRouteFromDestination(t *testing.T) {
	// 0x01/0x02/0x07/0x08 say the proxy cannot serve us; the rest describe the
	// path to the destination, which the proxy is not responsible for.
	expected := map[byte]bool{
		0x01: true, 0x02: true, 0x03: false, 0x04: false,
		0x05: false, 0x06: false, 0x07: true, 0x08: true,
	}
	for code, wantBroken := range expected {
		err := socks5ReplyError(&Egress{label: "socks5://127.0.0.1:1080"}, code)
		var egErr *EgressError
		if !errors.As(err, &egErr) {
			t.Fatalf("code 0x%02x: not an EgressError", code)
		}
		if egErr.Broken != wantBroken {
			t.Errorf("code 0x%02x: Broken = %v, want %v (%v)", code, egErr.Broken, wantBroken, err)
		}
		if egErr.Egress == nil {
			t.Errorf("code 0x%02x: error does not name the route", code)
		}
	}
}

// ---- dialing through real proxies --------------------------------------

func TestSOCKS5TunnelCarriesTheRequest(t *testing.T) {
	backend, hits := startFakeBackend(t, 0)
	proxy := startFakeSOCKS5(t, fakeSOCKS5Options{})

	pool, err := NewEgressPool([]string{"socks5://" + proxy.addr()}, EgressOptions{HeaderTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Options{Egress: pool, MaxConcurrent: 1})

	text, err := callOnce(t, client, "http://"+backend)
	if err != nil {
		t.Fatalf("through SOCKS5: %v", err)
	}
	if text != "ok" {
		t.Fatalf("delta text = %q, want \"ok\"", text)
	}
	waitHits(t, hits, 1)

	targets, _ := proxy.summary()
	if len(targets) != 1 || targets[0] != backend {
		t.Fatalf("proxy was asked for %v, want [%s]", targets, backend)
	}
}

func TestSOCKS5UsernamePasswordAuth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    string
		opts    fakeSOCKS5Options
		wantErr bool
	}{
		{
			name: "correct credentials",
			spec: "socks5://alice:s3cret@%s",
			opts: fakeSOCKS5Options{requireAuth: true, user: "alice", pass: "s3cret"},
		},
		{
			name:    "wrong credentials",
			spec:    "socks5://alice:wrong@%s",
			opts:    fakeSOCKS5Options{requireAuth: true, user: "alice", pass: "s3cret"},
			wantErr: true,
		},
		{
			name:    "no credentials offered",
			spec:    "socks5://%s",
			opts:    fakeSOCKS5Options{requireAuth: true, user: "alice", pass: "s3cret"},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, _ := startFakeBackend(t, 0)
			proxy := startFakeSOCKS5(t, tc.opts)
			spec := fmt.Sprintf(tc.spec, proxy.addr())
			pool, err := NewEgressPool([]string{spec}, EgressOptions{HeaderTimeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			client := NewClient(Options{Egress: pool, MaxConcurrent: 1})

			_, err = callOnce(t, client, "http://"+backend)
			if tc.wantErr && err == nil {
				t.Fatal("expected a failure through a proxy that refuses us")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected failure: %v", err)
			}
			if tc.wantErr {
				// A refusal is the route's own verdict on us, so it is cooled;
				// otherwise the pool would offer it on every request.
				var egErr *EgressError
				if !errors.As(err, &egErr) || !egErr.Broken {
					t.Fatalf("refusal should be a broken-route error, got %#v", err)
				}
				if got := pool.Available(); got != 0 {
					t.Fatalf("Available = %d, want 0 after the route refused us", got)
				}
			}
		})
	}
}

func TestHTTPProxyTunnelCarriesTheRequest(t *testing.T) {
	backend, hits := startFakeBackend(t, 0)
	proxy := startFakeHTTPProxy(t, 0, false, "", "")

	pool, err := NewEgressPool([]string{"http://" + proxy.addr()}, EgressOptions{HeaderTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Options{Egress: pool, MaxConcurrent: 1})

	text, err := callOnce(t, client, "http://"+backend)
	if err != nil {
		t.Fatalf("through an HTTP proxy: %v", err)
	}
	if text != "ok" {
		t.Fatalf("delta text = %q, want \"ok\"", text)
	}
	waitHits(t, hits, 1)
	if seen := proxy.seen(); len(seen) != 1 || seen[0] != backend {
		t.Fatalf("proxy was asked to CONNECT %v, want [%s]", seen, backend)
	}
}

func TestHTTPProxyAuthAndStatusHandling(t *testing.T) {
	t.Run("407 from the proxy cools it", func(t *testing.T) {
		backend, _ := startFakeBackend(t, 0)
		proxy := startFakeHTTPProxy(t, 0, true, "alice", "s3cret")
		pool, err := NewEgressPool([]string{"http://alice:wrong@" + proxy.addr()}, EgressOptions{HeaderTimeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		client := NewClient(Options{Egress: pool, MaxConcurrent: 1})
		if _, err := callOnce(t, client, "http://"+backend); err == nil {
			t.Fatal("expected the proxy to refuse us")
		}
		if got := pool.Available(); got != 0 {
			t.Fatalf("Available = %d, want 0: a proxy refusing our credentials is confirmed unusable", got)
		}
	})

	t.Run("backend 429 through the proxy keeps it in rotation", func(t *testing.T) {
		// The scenario the rule exists for: a rate limit comes back from the
		// destination through a proxy that carried it perfectly. The proxy has just
		// proved it works, so it stays in rotation — the account is the thing that
		// is rate limited, not the exit.
		backend, hits := startFakeBackend(t, http.StatusTooManyRequests)
		proxy := startFakeSOCKS5(t, fakeSOCKS5Options{})
		pool, err := NewEgressPool([]string{"socks5://" + proxy.addr()}, EgressOptions{HeaderTimeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		client := NewClient(Options{Egress: pool, MaxConcurrent: 1})
		_, err = callOnce(t, client, "http://"+backend)
		if err == nil {
			t.Fatal("expected the 429 to surface")
		}
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != http.StatusTooManyRequests {
			t.Fatalf("error = %#v, want an HTTPError with status 429", err)
		}
		waitHits(t, hits, 1)
		if got := pool.Available(); got != 1 {
			t.Fatalf("Available = %d, want 1: a 429 arriving through a route proves the route works", got)
		}
	})

	t.Run("proxy answering 429 to CONNECT is not fatal", func(t *testing.T) {
		// A 429 from the proxy itself is still a response from something that works,
		// so it is not grounds for a cooldown either.
		backend, _ := startFakeBackend(t, 0)
		proxy := startFakeHTTPProxy(t, http.StatusTooManyRequests, false, "", "")
		pool, err := NewEgressPool([]string{"http://" + proxy.addr()}, EgressOptions{HeaderTimeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		client := NewClient(Options{Egress: pool, MaxConcurrent: 1})
		_, err = callOnce(t, client, "http://"+backend)
		if err == nil {
			t.Fatal("expected the CONNECT refusal to surface")
		}
		var egErr *EgressError
		if !errors.As(err, &egErr) {
			t.Fatalf("error = %#v, want an EgressError", err)
		}
		if egErr.Broken {
			t.Fatalf("a 429 from the proxy must not be treated as a broken route: %v", err)
		}
		if got := pool.Available(); got != 1 {
			t.Fatalf("Available = %d, want 1", got)
		}
	})

	t.Run("correct credentials connect", func(t *testing.T) {
		backend, hits := startFakeBackend(t, 0)
		proxy := startFakeHTTPProxy(t, 0, true, "alice", "s3cret")
		pool, err := NewEgressPool([]string{"http://alice:s3cret@" + proxy.addr()}, EgressOptions{HeaderTimeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		client := NewClient(Options{Egress: pool, MaxConcurrent: 1})
		if _, err := callOnce(t, client, "http://"+backend); err != nil {
			t.Fatalf("with correct credentials: %v", err)
		}
		waitHits(t, hits, 1)
	})
}

// ---- routing resilience ------------------------------------------------

func TestDeadRouteFailsOverToTheNext(t *testing.T) {
	backend, hits := startFakeBackend(t, 0)
	good := startFakeSOCKS5(t, fakeSOCKS5Options{})

	// A port nothing is listening on: the dial fails immediately.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()

	pool, err := NewEgressPool([]string{
		"socks5://" + deadAddr,
		"socks5://" + good.addr(),
	}, EgressOptions{HeaderTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Options{Egress: pool, MaxConcurrent: 1})

	// Two routes, so the request that lands on the dead one must recover on the
	// next without the client seeing anything.
	for i := 0; i < 4; i++ {
		text, err := callOnce(t, client, "http://"+backend)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if text != "ok" {
			t.Fatalf("request %d: delta text = %q", i, text)
		}
	}
	waitHits(t, hits, 4)

	if got := pool.Available(); got != 1 {
		t.Fatalf("Available = %d, want 1: the dead route should be out of rotation", got)
	}
}

func TestClientAbortDoesNotCoolTheRoute(t *testing.T) {
	// A client that hangs up mid-request must not cost the route its place, for
	// the same reason it must not cost the account its place.
	backend, _ := startFakeBackend(t, 0)
	proxy := startFakeSOCKS5(t, fakeSOCKS5Options{})
	pool, err := NewEgressPool([]string{"socks5://" + proxy.addr()}, EgressOptions{HeaderTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(Options{Egress: pool, MaxConcurrent: 1})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.GetChatMessage(ctx, &Credentials{APIKey: "devin-session-token$x", APIServerURL: "http://" + backend}, &GetChatMessageRequest{}); err == nil {
		t.Fatal("expected the cancelled request to fail")
	}
	if got := pool.Available(); got != 1 {
		t.Fatalf("Available = %d, want 1: a cancelled request says nothing about the route", got)
	}
}

// callOnce sends one request through client and returns the delta text of the
// first frame, which also proves the first-frame peek and the stream reader work
// over the tunnel.
func callOnce(t *testing.T, client *Client, baseURL string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	creds := &Credentials{APIKey: "devin-session-token$test", APIServerURL: baseURL}
	stream, err := client.GetChatMessage(ctx, creds, &GetChatMessageRequest{ChatModelUID: "swe-1-6-slow"})
	if err != nil {
		return "", err
	}
	defer stream.Close()

	resp, err := stream.Recv()
	if err != nil {
		return "", err
	}
	return resp.DeltaText, nil
}

// ProbeEgress dials the route itself, outside http.Transport. A direct route has no
// dialer of its own — its transport is left on Go's default, which is reached
// through a nil DialContext — so calling that field directly is a nil dereference.
// It panicked a live dashboard request, which is why it has a test of its own
// despite looking like the least risky thing on the page.
func TestProbeEgressOnADirectRouteDoesNotPanic(t *testing.T) {
	// Nothing listening: the probe must report the dial failure.
	dead := freePort(t)
	probe := ProbeEgress(context.Background(), "direct", EgressOptions{}, dead, "")
	if probe.OK {
		t.Fatalf("a probe of a closed port reported ok: %+v", probe)
	}
	if probe.Error == "" {
		t.Error("a probe of a closed port reported no error")
	}
	if probe.Spec != "direct" {
		t.Errorf("probe.Spec = %q, want direct", probe.Spec)
	}

	// A listener that accepts and then says nothing: the tunnel opens and the TLS
	// handshake fails, which is the other half of the same path.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	plain := ProbeEgress(context.Background(), "direct", EgressOptions{}, ln.Addr().String(), "")
	if plain.OK {
		t.Fatalf("a probe of a non-TLS listener reported ok: %+v", plain)
	}
	if !strings.Contains(plain.Error, "TLS") {
		t.Errorf("probe.Error = %q, want a TLS failure", plain.Error)
	}
}

// TestProbeEgressMasksCredentials pins the form of a probe result: it is shown on
// the page and written to the log, so the password must not be in it.
func TestProbeEgressMasksCredentials(t *testing.T) {
	dead := freePort(t)
	probe := ProbeEgress(context.Background(), "socks5://alice:hunter2@"+dead, EgressOptions{}, dead, "")
	if strings.Contains(probe.Spec, "hunter2") || strings.Contains(probe.Spec, "alice") {
		t.Fatalf("probe.Spec = %q, which carries the credentials", probe.Spec)
	}
	if !probe.Credentials {
		t.Error("probe.Credentials is false for a route that has them")
	}
	if probe.OK {
		t.Errorf("a probe of a closed port reported ok: %+v", probe)
	}
}

// freePort returns a loopback address nothing is listening on.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}
