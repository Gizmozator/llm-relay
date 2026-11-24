package tlscert_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llm-relay/internal/tlscert"
)

func TestGenerate(t *testing.T) {
	c, err := tlscert.Generate([]string{"relay.example.fi", "10.0.0.5", "relay.example.fi"})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(c.CertPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("cert PEM not decodable")
	}
	der := block.Bytes
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "relay.example.fi" {
		t.Errorf("DNSNames = %v", cert.DNSNames)
	}
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "10.0.0.5" {
		t.Errorf("IPAddresses = %v", cert.IPAddresses)
	}
	if cert.NotAfter.Sub(cert.NotBefore) < 3600*24*time.Hour {
		t.Errorf("validity too short: %s", cert.NotAfter)
	}
	if c.Fingerprint != tlscert.Fingerprint(der) {
		t.Error("fingerprint mismatch")
	}
	if !strings.HasPrefix(c.Fingerprint, "sha256:") || len(c.Fingerprint) != 7+64 {
		t.Errorf("fingerprint format = %q", c.Fingerprint)
	}
	// The key PEM must load back and pair with the cert (that is what the
	// TLS listener does with the stored row).
	if _, err = tls.X509KeyPair([]byte(c.CertPEM), []byte(c.KeyPEM)); err != nil {
		t.Errorf("keypair does not load: %v", err)
	}
}

func TestGenerateRejectsBadHosts(t *testing.T) {
	tooMany := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		tooMany = append(tooMany, "host"+string(rune('a'+i))+".example.fi")
	}
	for _, hosts := range [][]string{
		{},
		{""},
		{"not a host"},
		{"http://relay.example.fi"},
		{"-bad.start"},
		{"trailing-dot.example.fi."},
		tooMany,
	} {
		if _, err := tlscert.Generate(hosts); err == nil {
			t.Errorf("hosts %q must be rejected", hosts)
		}
	}
}

func TestParsePin(t *testing.T) {
	hex64 := strings.Repeat("ab", 32)
	colonified := strings.ToUpper(strings.Join(strings.Split(strings.Repeat("AB", 32), ""), ":"))
	for _, in := range []string{
		"sha256:" + hex64,
		hex64,
		strings.ToUpper(hex64),
		"sha256:" + colonified,
		"  " + hex64 + " ",
	} {
		got, err := tlscert.ParsePin(in)
		if err != nil {
			t.Fatalf("ParsePin(%q): %v", in, err)
		}
		if got != hex64 {
			t.Errorf("ParsePin(%q) = %q, want %q", in, got, hex64)
		}
	}
	for _, in := range []string{"", "sha256:", strings.Repeat("z", 64), hex64[:63]} {
		if _, err := tlscert.ParsePin(in); err == nil {
			t.Errorf("ParsePin(%q) must fail", in)
		}
	}
}

// The pin model end to end: a TLS server with the generated cert, dialed
// with the client config the dialers use. Correct pin connects; wrong pin
// and no-pin (self-signed, no CA) are refused.
func TestPinHandshake(t *testing.T) {
	c, err := tlscert.Generate([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tlscert.ServerHandshakeOK(r) {
			t.Error("server must see TLS handshake state")
		}
		w.WriteHeader(http.StatusOK)
	}))
	cert, err := tls.X509KeyPair([]byte(c.CertPEM), []byte(c.KeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	// Correct pin: connects.
	cfg, err := tlscert.DialConfig(c.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if status := getStatus(t, srv.URL, cfg); status != http.StatusOK {
		t.Fatalf("pinned dial = %d, want 200", status)
	}
	// Wrong pin: refused.
	wrong, err := tlscert.DialConfig("sha256:" + strings.Repeat("cd", 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := getStatusErr(srv.URL, wrong); err == nil {
		t.Fatal("wrong pin must refuse the connection")
	}
	// No pin + self-signed: normal verification refuses it (the operator
	// must either pin or use a CA-issued cert).
	if _, err := getStatusErr(srv.URL, nil); err == nil {
		t.Fatal("self-signed without pin must be refused")
	}
}

// MITM regression (reviewer finding): a peer that replays the pinned cert as
// a SECOND chain element while authenticating with its OWN leaf must be
// refused. Under a "any cert in the chain matches" rule this passes and the
// token leaks to the attacker; binding the pin to the leaf (rawCerts[0]) is
// what closes it.
func TestPinBindsToLeafNotChain(t *testing.T) {
	victim, err := tlscert.Generate([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := tlscert.Generate([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	victimKP, err := tls.X509KeyPair([]byte(victim.CertPEM), []byte(victim.KeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	attackerKP, err := tls.X509KeyPair([]byte(attacker.CertPEM), []byte(attacker.KeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	// The attacker terminates the handshake with its own key but appends the
	// victim's (public) cert so a chain-scanning verifier would find the pin.
	mitm := tls.Certificate{
		Certificate: [][]byte{attackerKP.Certificate[0], victimKP.Certificate[0]},
		PrivateKey:  attackerKP.PrivateKey,
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{mitm}}
	srv.StartTLS()
	defer srv.Close()

	cfg, err := tlscert.DialConfig(victim.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := getStatusErr(srv.URL, cfg); err == nil {
		t.Fatal("MITM presenting [attacker_leaf, pinned_cert] must be refused")
	}
}

func TestDialConfigEmpty(t *testing.T) {
	cfg, err := tlscert.DialConfig("   ")
	if err != nil || cfg != nil {
		t.Fatalf("empty pin = (%v, %v), want (nil, nil)", cfg, err)
	}
	if _, err := tlscert.DialConfig("sha256:zz"); err == nil {
		t.Fatal("malformed pin must be rejected at config time")
	}
}

func getStatus(t *testing.T, url string, cfg *tls.Config) int {
	t.Helper()
	status, err := getStatusErr(url, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func getStatusErr(url string, cfg *tls.Config) (int, error) {
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}
