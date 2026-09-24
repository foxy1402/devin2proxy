package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGenerateSelfSignedProducesAUsablePair(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	if err := generateSelfSigned(certPath, keyPath, []string{"203.0.113.7", "gateway.example"}); err != nil {
		t.Fatalf("generateSelfSigned: %v", err)
	}
	if runtime.GOOS != "windows" {
		// Windows does not carry these permission bits; the check is for the
		// Linux container, where the key file is what an attacker would copy.
		if fi, err := os.Stat(keyPath); err != nil {
			t.Fatalf("stat key: %v", err)
		} else if fi.Mode().Perm() != 0o600 {
			t.Errorf("key mode = %v, want 0600", fi.Mode().Perm())
		}
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("the generated pair does not load: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	// The names that were asked for, in the right form.
	var hasIP bool
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.ParseIP("203.0.113.7")) {
			hasIP = true
		}
	}
	if !hasIP {
		t.Errorf("IP SANs = %v, want 203.0.113.7 among them", leaf.IPAddresses)
	}
	if !strings.Contains(strings.Join(leaf.DNSNames, ","), "gateway.example") {
		t.Errorf("DNS SANs = %v, want gateway.example", leaf.DNSNames)
	}
	// Loopback is always in, so the container's own healthcheck and a local
	// curl verify the name even when the operator named nothing.
	var hasLoopback bool
	for _, ip := range leaf.IPAddresses {
		if ip.IsLoopback() {
			hasLoopback = true
		}
	}
	if !hasLoopback {
		t.Error("no loopback IP in the SANs")
	}
	if !strings.Contains(strings.Join(leaf.DNSNames, ","), "localhost") {
		t.Errorf("DNS SANs = %v, want localhost", leaf.DNSNames)
	}
	// A server certificate, valid for roughly the browser-ceiling window.
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("ext key usage = %v, want serverAuth only", leaf.ExtKeyUsage)
	}
	if life := leaf.NotAfter.Sub(leaf.NotBefore); life < 800*24*time.Hour || life > certValidity+time.Hour {
		t.Errorf("validity = %v, want about %v", life, certValidity)
	}
}

func TestServeCertificateReusesAndRefusesHalfPairs(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	// Nothing on disk and no permission to generate: an error that names the
	// knob to turn, not a silent plaintext fallback.
	if _, _, err := serveCertificate(certPath, keyPath, nil, false); err == nil {
		t.Fatal("serveCertificate without a pair and without generation succeeded")
	}

	// First real call generates; the second must reuse the same bytes rather
	// than mint a new certificate — TOFU depends on the fingerprint staying.
	first, generated, err := serveCertificate(certPath, keyPath, []string{"198.51.100.9"}, true)
	if err != nil {
		t.Fatalf("serveCertificate: %v", err)
	}
	if !generated {
		t.Error("the first call did not report that it generated")
	}
	second, generated, err := serveCertificate(certPath, keyPath, []string{"ignored.on.reuse"}, true)
	if err != nil {
		t.Fatalf("serveCertificate (reuse): %v", err)
	}
	if generated {
		t.Error("the second call generated again; the certificate is not stable across restarts")
	}
	if string(first.Certificate[0]) != string(second.Certificate[0]) {
		t.Error("the reused certificate differs from the generated one")
	}

	// Half a pair is the mistake worth catching: delete the key, leave the cert.
	os.Remove(keyPath)
	if _, _, err := serveCertificate(certPath, keyPath, nil, true); err == nil ||
		!strings.Contains(err.Error(), "both halves") {
		t.Fatalf("half-pair error = %v, want one naming the missing half", err)
	}
}

func TestCertFingerprintIsColonHex(t *testing.T) {
	dir := t.TempDir()
	cert, _, err := serveCertificate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), nil, true)
	if err != nil {
		t.Fatalf("serveCertificate: %v", err)
	}
	fp := certFingerprint(cert)
	// 32 bytes, two hex digits each, joined by 31 colons.
	if len(fp) != 32*2+31 {
		t.Fatalf("fingerprint %q has length %d, want 95", fp, len(fp))
	}
	for _, part := range strings.Split(fp, ":") {
		if len(part) != 2 || strings.ToUpper(part) != part {
			t.Errorf("fingerprint part %q is not an uppercase hex byte", part)
		}
	}
}

// The generated certificate has to survive an actual handshake verified
// against itself: that is the whole TOFU story, and every piece short of it
// (parses, loads, fingerprints) would still pass with a certificate no client
// could ever trust.
func TestTheGeneratedCertificateCompletesAHandshake(t *testing.T) {
	dir := t.TempDir()
	cert, _, err := serveCertificate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), nil, true)
	if err != nil {
		t.Fatalf("serveCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		// The connection state is the proof the handshake negotiated TLS.
		if r.TLS == nil {
			t.Error("the handler saw no TLS connection state")
		}
		w.Write([]byte(`{"status":"ok"}`))
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	defer ts.Close()

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
	resp, err := client.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("handshake against the generated certificate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := resp.TLS; got == nil || got.Version < tls.VersionTLS12 {
		t.Errorf("negotiated TLS = %+v, want 1.2 or better", got)
	}
}

// warnCertExpiry must not panic on any certificate shape the loader can hand
// it, and must stay silent for a fresh one.
func TestWarnCertExpiryDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	cert, _, err := serveCertificate(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), nil, true)
	if err != nil {
		t.Fatalf("serveCertificate: %v", err)
	}
	warnCertExpiry(cert, filepath.Join(dir, "tls.crt"))
	warnCertExpiry(tls.Certificate{}, "nonexistent")
}

// The PEM block types are what openssl and every client library key on; a
// wrong label makes the file unreadable in ways that look like corruption.
func TestGeneratedPEMLabelsAreStandard(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	if err := generateSelfSigned(certPath, keyPath, nil); err != nil {
		t.Fatalf("generateSelfSigned: %v", err)
	}
	assertPEM(t, certPath, "CERTIFICATE")
	assertPEM(t, keyPath, "EC PRIVATE KEY")
}

func assertPEM(t *testing.T, path, wantType string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatalf("%s is not PEM", path)
	}
	if block.Type != wantType {
		t.Errorf("%s PEM type = %q, want %q", path, block.Type, wantType)
	}
}
