package lanetls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A throwaway lane PKI: an ECDSA P-384 CA, a server certificate for
// 127.0.0.1, a client certificate carrying an instance URI, a revoked client,
// and the CA's CRL listing it. A named test fixture, never the lane's.
type pki struct {
	dir                          string
	ca                           *x509.Certificate
	caKey                        *ecdsa.PrivateKey
	server, client, revoked, crl string
}

func write(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (p *pki) leaf(t *testing.T, name string, serial int64, server bool) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	uri, _ := url.Parse("urn:uuid:0193b3a0-0000-7000-8000-00000000000" + string(rune('0'+serial%10)))
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, URIs: []*url.URL{uri},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	if server {
		tpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(p.dir, name)
	write(t, base+".crt", "CERTIFICATE", der)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	write(t, base+".key", "EC PRIVATE KEY", keyDER)
	return base
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	p := &pki{dir: t.TempDir()}
	p.caKey, _ = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-fixture lane CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &p.caKey.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	p.ca, _ = x509.ParseCertificate(der)
	write(t, filepath.Join(p.dir, "ca.crt"), "CERTIFICATE", der)
	p.server = p.leaf(t, "server", 2, true)
	p.client = p.leaf(t, "client", 3, false)
	p.revoked = p.leaf(t, "revoked", 4, false)
	crl, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number: big.NewInt(1), ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(time.Hour),
		RevokedCertificateEntries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(4), RevocationTime: time.Now()}},
	}, p.ca, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	p.crl = filepath.Join(p.dir, "ca.crl")
	write(t, p.crl, "X509 CRL", crl)
	return p
}

func (p *pki) material(base string) *Material {
	return &Material{Cert: base + ".crt", Key: base + ".key", CA: filepath.Join(p.dir, "ca.crt"), CRL: p.crl}
}

func TestEnvironmentIsAllOrNothing(t *testing.T) {
	p := newPKI(t)
	set := func(cert, key, ca, crl string) {
		t.Setenv("RE_TLS_CERT", cert)
		t.Setenv("RE_TLS_KEY", key)
		t.Setenv("RE_TLS_CA", ca)
		t.Setenv("RE_TLS_CRL", crl)
	}
	set("", "", "", "")
	if m, err := FromEnvironment(); m != nil || err != nil {
		t.Fatalf("none set is plaintext off the lane: %v %v", m, err)
	}
	for _, partial := range [][4]string{
		{p.server + ".crt", "", "", ""},
		{p.server + ".crt", p.server + ".key", "", ""},
		{"", "", "", p.crl},
		{p.server + ".crt", p.server + ".key", filepath.Join(p.dir, "missing.crt"), ""},
	} {
		set(partial[0], partial[1], partial[2], partial[3])
		if _, err := FromEnvironment(); err == nil {
			t.Fatalf("partial or missing material must refuse to start: %v", partial)
		}
	}
	set(p.server+".crt", p.server+".key", filepath.Join(p.dir, "ca.crt"), p.crl)
	if m, err := FromEnvironment(); err != nil || m == nil {
		t.Fatalf("%v %v", m, err)
	}
	// A CRL another CA signed is refused.
	other := newPKI(t)
	bad := p.material(p.server)
	bad.CRL = other.crl
	if _, err := bad.Server(); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("%v", err)
	}
}

func TestServerHoldsTLS13AndMTLSAndTheCRL(t *testing.T) {
	p := newPKI(t)
	cfg, err := p.material(p.server).Server()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	url := "https://" + ln.Addr().String() + "/"

	get := func(m *Material, mutate func(*tls.Config)) error {
		c, err := m.Client()
		if err != nil {
			return err
		}
		if mutate != nil {
			mutate(c)
		}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: c}, Timeout: 5 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "ok" {
			t.Fatalf("%s", body)
		}
		return nil
	}
	if err := get(p.material(p.client), nil); err != nil {
		t.Fatalf("a lane client is served: %v", err)
	}
	if err := get(p.material(p.client), func(c *tls.Config) { c.Certificates = nil }); err == nil {
		t.Fatal("a client without a certificate must be refused (mTLS)")
	}
	if err := get(p.material(p.revoked), nil); err == nil {
		t.Fatal("a revoked client must be refused")
	}
	if err := get(p.material(p.client), func(c *tls.Config) { c.MinVersion, c.MaxVersion = tls.VersionTLS12, tls.VersionTLS12 }); err == nil {
		t.Fatal("TLS 1.2 must be refused")
	}
	// The client verifies the server: a CA it does not trust is refused.
	other := newPKI(t)
	if err := get(other.material(other.client), nil); err == nil {
		t.Fatal("a server from another CA must be refused")
	}
	// And the hostname: the server's certificate names 127.0.0.1 only.
	if err := get(p.material(p.client), func(c *tls.Config) { c.ServerName = "elsewhere.localhost" }); err == nil {
		t.Fatal("a server certificate for another name must be refused")
	}
}
