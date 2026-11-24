// Package lanetls is the suite's runtime TLS contract for the Go pair
// (RealityEngine_GO#6; RealityEngine_CI docs/TRANSPORT_SECURITY.md,
// config/transport-profile.json):
//
//   - RE_TLS_CERT, RE_TLS_KEY and RE_TLS_CA are all set, or none is. A
//     partial set refuses to start: there is no fallback to plaintext once
//     any TLS material is named. RE_TLS_CRL is optional; named, it must parse
//     and be signed by the CA, and every peer is checked against it.
//   - The server speaks TLS 1.3 only, requires and verifies a client
//     certificate from the lane CA (mTLS), and prefers X25519MLKEM768, then
//     X25519 and secp384r1.
//   - The client presents the instance certificate, verifies the server
//     against the lane CA with hostname checks on, and applies the CRL.
//
// What it cannot do: crypto/tls does not restrict TLS 1.3 cipher suites
// (Config.CipherSuites governs TLS 1.0-1.2 only), so a Go listener also
// accepts TLS_AES_128_GCM_SHA256. The suite's pattern for Go services is a
// loopback listener behind an OpenSSL front that serves the profile
// (TRANSPORT_SECURITY.md, 2026-11-12); this package is the listener's half.
package lanetls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// Material is the lane's TLS inputs.
type Material struct {
	Cert, Key, CA, CRL string
}

// FromEnvironment reads RE_TLS_*: nil when none is set, an error when the set
// is partial or names a file that is not there.
func FromEnvironment() (*Material, error) {
	m := Material{Cert: os.Getenv("RE_TLS_CERT"), Key: os.Getenv("RE_TLS_KEY"), CA: os.Getenv("RE_TLS_CA"), CRL: os.Getenv("RE_TLS_CRL")}
	set := 0
	for _, v := range []string{m.Cert, m.Key, m.CA} {
		if v != "" {
			set++
		}
	}
	if set == 0 {
		if m.CRL != "" {
			return nil, errors.New("RE_TLS_CRL is set without RE_TLS_CERT, RE_TLS_KEY and RE_TLS_CA")
		}
		return nil, nil
	}
	if set != 3 {
		return nil, errors.New("RE_TLS_CERT, RE_TLS_KEY and RE_TLS_CA must all be set, or none (no plaintext fallback)")
	}
	for name, path := range map[string]string{"RE_TLS_CERT": m.Cert, "RE_TLS_KEY": m.Key, "RE_TLS_CA": m.CA, "RE_TLS_CRL": m.CRL} {
		if path == "" {
			continue
		}
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return nil, fmt.Errorf("%s names %s, which is not a file", name, path)
		}
	}
	return &m, nil
}

// curves is the profile's groups in preference order.
var curves = []tls.CurveID{tls.X25519MLKEM768, tls.X25519, tls.CurveP384}

type loaded struct {
	cert tls.Certificate
	pool *x509.CertPool
	crl  *x509.RevocationList
}

func (m *Material) load() (*loaded, error) {
	cert, err := tls.LoadX509KeyPair(m.Cert, m.Key)
	if err != nil {
		return nil, fmt.Errorf("RE_TLS_CERT/RE_TLS_KEY: %w", err)
	}
	caPEM, err := os.ReadFile(m.CA)
	if err != nil {
		return nil, fmt.Errorf("RE_TLS_CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("RE_TLS_CA %s holds no certificate", m.CA)
	}
	l := &loaded{cert: cert, pool: pool}
	if m.CRL != "" {
		if l.crl, err = readCRL(m.CRL, caPEM); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// readCRL parses the CRL (PEM or DER) and checks that one of the CA's
// certificates signed it.
func readCRL(path string, caPEM []byte) (*x509.RevocationList, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("RE_TLS_CRL: %w", err)
	}
	der := data
	if block := pemBlock(data, "X509 CRL"); block != nil {
		der = block
	}
	crl, err := x509.ParseRevocationList(der)
	if err != nil {
		return nil, fmt.Errorf("RE_TLS_CRL %s: %w", path, err)
	}
	for _, ca := range pemCerts(caPEM) {
		if crl.CheckSignatureFrom(ca) == nil {
			return crl, nil
		}
	}
	return nil, fmt.Errorf("RE_TLS_CRL %s is not signed by RE_TLS_CA", path)
}

// revoked refuses a verified chain whose leaf the CRL lists.
func (l *loaded) revoked(chains [][]*x509.Certificate) error {
	if l.crl == nil {
		return nil
	}
	for _, chain := range chains {
		if len(chain) == 0 {
			continue
		}
		for _, entry := range l.crl.RevokedCertificateEntries {
			if entry.SerialNumber.Cmp(chain[0].SerialNumber) == 0 {
				return fmt.Errorf("certificate serial %s is revoked", chain[0].SerialNumber)
			}
		}
	}
	return nil
}

// Server is the listener's configuration: TLS 1.3, mTLS against the lane CA,
// the CRL checked on every client.
func (m *Material) Server() (*tls.Config, error) {
	l, err := m.load()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, CurvePreferences: curves,
		Certificates: []tls.Certificate{l.cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: l.pool,
		VerifyPeerCertificate: func(_ [][]byte, chains [][]*x509.Certificate) error { return l.revoked(chains) },
	}, nil
}

// Client is an outbound call's configuration: the instance certificate, the
// lane CA with hostname verification, and the CRL.
func (m *Material) Client() (*tls.Config, error) {
	l, err := m.load()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, CurvePreferences: curves,
		Certificates: []tls.Certificate{l.cert}, RootCAs: l.pool,
		VerifyPeerCertificate: func(_ [][]byte, chains [][]*x509.Certificate) error { return l.revoked(chains) },
	}, nil
}
