package devin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// The unit tests in egress_test.go check the handshakes against fake proxies this
// package also wrote, which cannot catch a shared misreading of RFC 1928. These
// tests close that gap by running against real proxies, and are skipped unless a
// list is supplied:
//
//	set DEVIN2PROXY_TEST_PROXIES=socks5://user:pass@host:1080,socks5://user:pass@host:1081
//	go test ./internal/devin -run TestLive -v
//
// DEVIN2PROXY_TEST_ECHO overrides the URL used to report the exit address; it must
// be an endpoint that echoes the caller's IP as text.
const (
	liveProxiesEnv = "DEVIN2PROXY_TEST_PROXIES"
	liveEchoEnv    = "DEVIN2PROXY_TEST_ECHO"
)

func liveProxyList(t *testing.T) []string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(liveProxiesEnv))
	if raw == "" {
		t.Skipf("set %s to a comma-separated proxy list to run this", liveProxiesEnv)
	}
	var specs []string
	for _, part := range strings.Split(raw, ",") {
		if spec := cleanToken(part); spec != "" {
			specs = append(specs, spec)
		}
	}
	if len(specs) == 0 {
		t.Fatalf("%s held no usable specs", liveProxiesEnv)
	}
	return specs
}

func liveEchoURL() string {
	if v := strings.TrimSpace(os.Getenv(liveEchoEnv)); v != "" {
		return v
	}
	return "https://api.ipify.org"
}

// liveFetch issues one GET over exactly one route, bypassing the pool's rotation
// so each route can be observed on its own.
func liveFetch(ctx context.Context, e *Egress, url string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := e.tr.RoundTrip(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, strings.TrimSpace(string(body)), nil
}

// TestLiveProxyRoutes reports the exit address every configured route actually
// uses, and fails if any route cannot carry a request at all. That last part is
// the real assertion: whether two ports exit the same address is a property of the
// proxy setup, not of this package, so it is reported rather than failed.
func TestLiveProxyRoutes(t *testing.T) {
	specs := liveProxyList(t)
	pool, err := NewEgressPool(specs, EgressOptions{
		DialTimeout:   15 * time.Second,
		HeaderTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewEgressPool: %v", err)
	}
	if pool.Len() != len(specs) {
		t.Fatalf("pool kept %d of %d specs", pool.Len(), len(specs))
	}

	url := liveEchoURL()
	exits := map[string][]string{}
	for i, route := range pool.list {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		status, body, err := liveFetch(ctx, route, url)
		cancel()
		if err != nil {
			t.Errorf("route %d/%d (%s): %v", i+1, pool.Len(), route.label, err)
			continue
		}
		if status != http.StatusOK {
			t.Errorf("route %d/%d (%s): status %d", i+1, pool.Len(), route.label, status)
			continue
		}
		t.Logf("route %d/%d (%s) -> %s", i+1, pool.Len(), route.label, body)
		exits[body] = append(exits[body], route.label)
	}

	if t.Failed() {
		return
	}
	if len(exits) < pool.Len() {
		t.Logf("NOTE: %d routes share %d distinct exit address(es)", pool.Len(), len(exits))
		for addr, routes := range exits {
			if len(routes) > 1 {
				t.Logf("      %s is used by %s", addr, strings.Join(routes, ", "))
			}
		}
	}
}

// TestLiveProxyAuthFailureIsAFaultOfTheRoute checks the classification against a
// real server's real refusal rather than a fake's: a wrong password must be
// recorded as the route being broken, so the pool stops offering it.
func TestLiveProxyAuthFailureIsAFaultOfTheRoute(t *testing.T) {
	specs := liveProxyList(t)

	// Rebuild the first spec with a wrong password, keeping everything else.
	u, err := url.Parse(specs[0])
	if err != nil {
		t.Fatalf("cannot parse %q: %v", specs[0], err)
	}
	u.User = url.UserPassword("wrong", "definitelywrong")

	pool, err := NewEgressPool([]string{u.String()}, EgressOptions{
		DialTimeout:   15 * time.Second,
		HeaderTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewEgressPool: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, _, err = liveFetch(ctx, pool.list[0], liveEchoURL())
	if err == nil {
		t.Skip("the proxy accepted bogus credentials, so there is nothing to classify")
	}

	var egErr *EgressError
	if !errors.As(err, &egErr) {
		t.Skipf("not a route failure, so there is nothing to classify: %v", err)
	}
	if !egErr.Broken {
		t.Fatalf("a real credential refusal should be a broken route, got: %v", err)
	}
	pool.Report(pool.list[0], err)
	if got := pool.Available(); got != 0 {
		t.Fatalf("Available = %d, want 0: the route refused our credentials", got)
	}
	t.Logf("refused as expected: %v", err)
}

// TestLiveUnreachableDestinationDoesNotCoolTheRoute is the other half of the same
// rule against a real server: a failure to reach the *destination* must leave the
// route in rotation, because the route demonstrably carried the request.
func TestLiveUnreachableDestinationDoesNotCoolTheRoute(t *testing.T) {
	specs := liveProxyList(t)
	pool, err := NewEgressPool(specs[:1], EgressOptions{
		DialTimeout:   15 * time.Second,
		HeaderTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewEgressPool: %v", err)
	}

	// Port 1 on loopback is closed everywhere, so the proxy must report that the
	// destination is unreachable rather than that it cannot serve us.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, _, err = liveFetch(ctx, pool.list[0], "http://127.0.0.1:1/")
	if err == nil {
		t.Skip("the proxy reached a closed port, so there is nothing to classify")
	}
	t.Logf("destination failure: %v", err)

	pool.Report(pool.list[0], err)
	if got := pool.Available(); got != 1 {
		t.Fatalf("Available = %d, want 1: failing to reach the destination is not the route's fault", got)
	}
}
