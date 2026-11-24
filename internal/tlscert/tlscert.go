// Package tlscert is the shared certificate machinery for machine-to-machine
// TLS (plan 2026-11-24-remote-tls-v1): self-signed generation for relays
// without a CA cert, SHA-256 fingerprint pinning for the dialing side
// (federation spoke and remote-backend connector alike), and the pin parse
// both sides speak. One implementation, tested once, used by the TLS
// listener and both dialers.
package tlscert

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
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Validity is deliberately long (10 years): these certs are pinned by
// fingerprint, not chained, so expiry is an operator choice, not a
// protocol requirement. The Settings view shows the date.
const Validity = 3650 * 24 * time.Hour

// MaxHosts bounds the SAN list (a generate request is admin-authenticated,
// but a bounded input keeps cert size sane).
const MaxHosts = 10

var dnsLabel = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?$`)

// Cert is a freshly generated certificate plus everything the caller needs
// to store and display it.
type Cert struct {
	CertPEM     string
	KeyPEM      string
	Fingerprint string // "sha256:<hex>"
	NotAfter    string // RFC3339 UTC
	SANs        []string
}

// Generate mints an ECDSA P-256 self-signed leaf for the given hosts (DNS
// names and/or IPs, 1..MaxHosts). server_auth EKU: it serves the relay's
// TLS listener, nothing else.
func Generate(hosts []string) (*Cert, error) {
	dns, ips, err := splitHosts(hosts)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("tlscert: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("tlscert: serial: %w", err)
	}
	now := time.Now().UTC()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dns0(dns, ips)},
		NotBefore:    now.Add(-time.Hour), // clock skew allowance
		NotAfter:     now.Add(Validity),
		// A plain self-signed leaf, not a CA: it is served as the listener's
		// leaf and pinned by fingerprint, and never signs another certificate.
		// CA:TRUE would be a footgun (installing this PEM in a trust store
		// would make any cert signed by this key valid for any name) and
		// strict verifiers reject CA:TRUE as a server leaf.
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("tlscert: create cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("tlscert: marshal key: %w", err)
	}
	// Report the SANs the certificate actually carries (trimmed, lowercased,
	// deduped, empties dropped) — not the raw caller slice — so the value
	// persisted and shown in the UI matches the cert an operator distributes
	// the fingerprint for.
	sans := append([]string(nil), dns...)
	for _, ip := range ips {
		sans = append(sans, ip.String())
	}
	return &Cert{
		CertPEM:     string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:      string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		Fingerprint: Fingerprint(der),
		NotAfter:    tmpl.NotAfter.Format(time.RFC3339),
		SANs:        sans,
	}, nil
}

func splitHosts(hosts []string) (dns []string, ips []net.IP, err error) {
	seen := map[string]bool{}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if seen[h] {
			continue
		}
		seen[h] = true
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
			continue
		}
		if validDNSName(h) {
			dns = append(dns, strings.ToLower(h))
			continue
		}
		return nil, nil, fmt.Errorf("tlscert: %q is neither a valid IP nor a DNS name", h)
	}
	if n := len(dns) + len(ips); n == 0 {
		return nil, nil, fmt.Errorf("tlscert: at least one host (DNS name or IP) is required")
	} else if n > MaxHosts {
		return nil, nil, fmt.Errorf("tlscert: %d hosts, maximum %d", n, MaxHosts)
	}
	return dns, ips, nil
}

func validDNSName(h string) bool {
	if len(h) > 253 || strings.HasSuffix(h, ".") {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if !dnsLabel.MatchString(label) {
			return false
		}
	}
	return true
}

func dns0(dns []string, ips []net.IP) string {
	if len(dns) > 0 {
		return dns[0]
	}
	if len(ips) > 0 {
		return ips[0].String()
	}
	return "llm-relay"
}

func hashHex(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Fingerprint is the pin format: "sha256:" + lowercase hex of the SHA-256
// over the DER certificate.
func Fingerprint(der []byte) string {
	return "sha256:" + hashHex(der)
}

// ParsePin normalizes a pasted pin to bare lowercase hex. Accepts the
// displayed "sha256:<hex>" form, bare hex, and colon-separated hex
// (openssl-style). Returns the 64-char hex string.
func ParsePin(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "sha256:")
	s = strings.TrimPrefix(s, "SHA256:")
	s = strings.ReplaceAll(s, ":", "")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ToLower(s)
	if len(s) != 64 {
		return "", fmt.Errorf("tlscert: pin must be 64 hex chars (sha256 fingerprint), got %d", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("tlscert: pin is not hex: %w", err)
	}
	return s, nil
}

// DialConfig builds the client TLS config for a dialer (spoke dialer,
// connector). Empty pin → nil: normal CA verification (the reverse-proxy
// case with a real cert). A pin means "trust exactly this certificate":
// verification is skipped and the peer's LEAF certificate must have a
// SHA-256 equal to the pin. The pin is bound to the leaf (not "any cert in
// the chain") on purpose: under InsecureSkipVerify Go does no chain
// validation, so a scan over all presented certs would let a MITM replay the
// pinned cert as a second chain element while authenticating with its own
// leaf — the token would then ride a channel the pin appears to vouch for.
// The listener serves a single self-signed leaf and the CA/proxy case uses no
// pin at all, so leaf-only binding costs nothing here and closes that hole.
func DialConfig(pin string) (*tls.Config, error) {
	if strings.TrimSpace(pin) == "" {
		return nil, nil
	}
	want, err := ParsePin(pin)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		// InsecureSkipVerify is required for the pin model: chain
		// verification would reject a self-signed leaf before any pin
		// check could run. VerifyPeerCertificate below is the actual
		// verifier — it runs on every handshake with the raw chain.
		InsecureSkipVerify:    true, //nolint:gosec // replaced by pin verification below
		VerifyPeerCertificate: pinVerifier(want),
		MinVersion:            tls.VersionTLS12,
	}, nil
}

func pinVerifier(wantHex string) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("tlscert: peer presented no certificate")
		}
		// rawCerts[0] is the leaf the peer authenticated with (the TLS
		// handshake proves possession of its private key). Bind the pin to
		// it alone — a later chain element is attacker-chosen and proves
		// nothing about who terminated the handshake.
		if hashHex(rawCerts[0]) != wantHex {
			return fmt.Errorf("tlscert: peer leaf certificate fingerprint does not match the pin")
		}
		return nil
	}
}

// ServerHandshakeOK answers the server-side gate question "did this request
// arrive over TLS?" for a raw *http.Request: r.TLS is set on direct TLS
// connections; behind a TLS-terminating reverse proxy it is nil at origin,
// and the caller decides separately whether X-Forwarded-Proto may be
// trusted (TrustProxyHeaders).
func ServerHandshakeOK(r *http.Request) bool {
	return r.TLS != nil
}
