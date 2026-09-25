package main

// TLS for the gateway itself.
//
// The deployment this exists for is a public IP with no domain: a raw
// `ip:port` on a small VM. There is no CA to issue for that and no reverse
// proxy assumed in front, so the proxy has to terminate TLS itself. The
// certificate is therefore self-signed and generated on first start into the
// data directory, then reused — which makes the trust model TOFU (trust on
// first use): the fingerprint is printed at startup, the operator checks it
// once, and because the file lives in the volume it stays the same across
// container restarts and recreations. A certificate regenerated per boot would
// train clients to ignore fingerprint changes, which is the opposite of what
// TOFU needs.
//
// Plaintext is not an option here for what this guards: the API key on /v1 is
// the only thing between the internet and the account's quota, and the
// dashboard password unlocks adding and deleting credentials. Both would
// cross the wire in cleartext without TLS, and the dashboard's session cookie
// is only marked Secure when the request arrived over TLS in the first place.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// certValidity is how long a generated certificate is born valid for. 825 days
// is the longest lifetime browsers still accept for a TLS server certificate;
// past it Safari refuses outright, and an operator who missed a renewal should
// get a loud client error rather than a silently dead gateway, so there is no
// point going longer.
const certValidity = 825 * 24 * time.Hour

// serveCertificate loads the keypair at certPath/keyPath, generating a
// self-signed one there first when it is missing and mayGenerate is set. The
// bool result reports whether it generated. Generation writes both files
// before returning, so a crash between here and the first request still leaves
// a reusable certificate behind rather than one regenerated per restart.
func serveCertificate(certPath, keyPath string, names []string, mayGenerate bool) (tls.Certificate, bool, error) {
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	certExists, keyExists := certErr == nil, keyErr == nil
	switch {
	case certExists && keyExists:
	case certExists != keyExists:
		// Half a pair is a mistake worth naming exactly, because the obvious
		// reading ("the file is right there") is wrong for the missing half.
		missing, present := keyPath, certPath
		if keyExists {
			missing, present = certPath, keyPath
		}
		return tls.Certificate{}, false, fmt.Errorf("found %s but not %s; a certificate needs both halves", present, missing)
	default:
		if !mayGenerate {
			return tls.Certificate{}, false, fmt.Errorf("no certificate at %s; enable tls (or DEVIN2PROXY_TLS=1) to have one generated, or point tls_cert_file and tls_key_file at an existing pair", certPath)
		}
		if err := generateSelfSigned(certPath, keyPath, names); err != nil {
			return tls.Certificate{}, false, err
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, false, fmt.Errorf("%s and %s are not a usable pair: %w", certPath, keyPath, err)
	}
	return cert, !certExists, nil
}

// generateSelfSigned writes a fresh certificate and key to the given paths.
// The key goes out 0600 for the same reason config.json does: whatever can
// read it can impersonate the gateway to every client that trusted it.
func generateSelfSigned(certPath, keyPath string, names []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}
	notBefore := time.Now().Add(-time.Hour) // tolerate a little clock skew
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "devin2proxy"},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyAgreement,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	addNames := func(raw []string) {
		for _, n := range raw {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if ip := net.ParseIP(n); ip != nil {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			} else {
				tmpl.DNSNames = append(tmpl.DNSNames, n)
			}
		}
	}
	addNames(names)
	// Loopback is always in the names: the container's own healthcheck dials
	// 127.0.0.1, and an operator on the same machine should be able to curl
	// without a name warning even before distributing the certificate.
	addNames([]string{"127.0.0.1", "::1", "localhost"})
	// Deduplicate: the same IP can arrive both as a configured name and as the
	// listen address.
	tmpl.IPAddresses = uniqueIPs(tmpl.IPAddresses)
	tmpl.DNSNames = uniqueStrings(tmpl.DNSNames)

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("self-sign: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	for _, dir := range []string{filepath.Dir(certPath), filepath.Dir(keyPath)} {
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dir, err)
			}
		}
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", keyPath, err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", certPath, err)
	}
	return nil
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func uniqueIPs(in []net.IP) []net.IP {
	seen := map[string]bool{}
	var out []net.IP
	for _, ip := range in {
		k := ip.String()
		if !seen[k] {
			seen[k] = true
			out = append(out, ip)
		}
	}
	return out
}

// certFingerprint renders the leaf's SHA-256 as colon-separated hex, the form
// an operator can compare against what a browser or curl reports for the
// presented certificate.
func certFingerprint(cert tls.Certificate) string {
	if len(cert.Certificate) == 0 {
		return ""
	}
	sum := sha256.Sum256(cert.Certificate[0])
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}
	return strings.Join(parts, ":")
}

// warnCertExpiry says out loud what would otherwise only surface as every
// client suddenly refusing the connection: an expired certificate, or one
// about to expire. It is a warning rather than a refusal because the operator
// may be mid-renewal, and a running gateway with a stale certificate is still
// a running gateway.
func warnCertExpiry(cert tls.Certificate, certPath string) {
	leaf := cert.Leaf
	if leaf == nil {
		if len(cert.Certificate) == 0 {
			return
		}
		var err error
		leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return
		}
	}
	now := time.Now()
	switch {
	case now.After(leaf.NotAfter):
		log.Printf("tls: WARNING the certificate at %s expired on %s; clients will refuse the connection until it is replaced",
			certPath, leaf.NotAfter.UTC().Format(time.RFC3339))
	case leaf.NotAfter.Sub(now) < 30*24*time.Hour:
		log.Printf("tls: the certificate at %s expires on %s (within 30 days); replace it before then",
			certPath, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
}

// warnUncoveredNames flags a name the operator asked for that the loaded
// certificate does not actually cover. The pair is reused TOFU-style, so
// tls_names added after the first generation would otherwise be silently
// ignored: the operator gets the startup success and then a name-mismatch
// error on every client, with nothing pointing at the stale certificate as
// the cause. Deleting the pair (or pointing tls_cert_file elsewhere) is the
// fix, and the warning is the hint.
func warnUncoveredNames(cert tls.Certificate, names []string, certPath string) {
	if len(names) == 0 || len(cert.Certificate) == 0 {
		return
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return
	}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		covered := false
		if ip := net.ParseIP(n); ip != nil {
			for _, candidate := range leaf.IPAddresses {
				if candidate.Equal(ip) {
					covered = true
					break
				}
			}
		} else {
			covered = leaf.VerifyHostname(n) == nil
		}
		if !covered {
			log.Printf("tls: WARNING the certificate at %s does not cover the requested name %q; delete the pair to regenerate with the current tls_names", certPath, n)
		}
	}
}
