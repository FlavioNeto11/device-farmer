// Package testpki mints a throwaway certificate authority for tests that need
// a real TLS handshake: an in-memory CA, a server certificate for the loopback
// address, and client certificates carrying the farm://<class>/<service> URI
// SAN the fence proxy reads its credential class from.
//
// Nothing here is reusable outside a test. Keys are P-256 and generated per
// call, lifetimes are an hour, and the CA is never written anywhere unless a
// test asks for PEM files on disk to feed a configuration loader.
package testpki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PKI is one throwaway authority.
type PKI struct {
	tb     testing.TB
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte
	pool   *x509.CertPool
	serial int64
}

// New mints a CA. It fails the test on any error.
func New(tb testing.TB) *PKI {
	tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatalf("testpki: generating the CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "testpki CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		tb.Fatalf("testpki: signing the CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		tb.Fatalf("testpki: parsing the CA: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &PKI{
		tb:     tb,
		caCert: cert,
		caKey:  key,
		caPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pool:   pool,
		serial: 1,
	}
}

// Pool returns the CA as a verification pool.
func (p *PKI) Pool() *x509.CertPool { return p.pool }

// CAPEM returns the CA certificate in PEM form.
func (p *PKI) CAPEM() []byte { return p.caPEM }

// issue signs one leaf. The template's public key, serial and validity are
// filled in here so a caller only states what the certificate is FOR.
func (p *PKI) issue(tmpl *x509.Certificate) (tls.Certificate, []byte, []byte) {
	p.tb.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		p.tb.Fatalf("testpki: generating a leaf key: %v", err)
	}
	p.serial++
	tmpl.SerialNumber = big.NewInt(p.serial)
	tmpl.NotBefore = time.Now().Add(-time.Minute)
	tmpl.NotAfter = time.Now().Add(time.Hour)
	tmpl.BasicConstraintsValid = true
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		p.tb.Fatalf("testpki: signing a leaf: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		p.tb.Fatalf("testpki: encoding a leaf key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	tc, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		p.tb.Fatalf("testpki: pairing a leaf: %v", err)
	}
	return tc, certPEM, keyPEM
}

// ServerConfig returns a TLS configuration for a listener on the loopback
// address that requires and verifies a client certificate from this CA.
func (p *PKI) ServerConfig() *tls.Config {
	p.tb.Helper()
	cert, _, _ := p.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:    []string{"localhost"},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    p.pool,
	}
}

// ClientConfig returns a TLS configuration presenting a client certificate
// whose URI SAN is farm://<class>/<service>, verified against this CA.
//
// ServerName is left empty on purpose: the client under test is expected to
// fill it from the endpoint it dials, and a test that pre-filled it would not
// be testing that.
func (p *PKI) ClientConfig(class, service string) *tls.Config {
	p.tb.Helper()
	cert, _, _ := p.client(class, service)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      p.pool,
	}
}

// WriteClientPEM writes a client certificate, its key and the CA to dir and
// returns the three paths, for a test that feeds a configuration loader.
func (p *PKI) WriteClientPEM(dir, class, service string) (certFile, keyFile, caFile string) {
	p.tb.Helper()
	_, certPEM, keyPEM := p.client(class, service)
	certFile = filepath.Join(dir, "client.crt")
	keyFile = filepath.Join(dir, "client.key")
	caFile = filepath.Join(dir, "ca.crt")
	for _, f := range []struct {
		path string
		data []byte
	}{{certFile, certPEM}, {keyFile, keyPEM}, {caFile, p.caPEM}} {
		if err := os.WriteFile(f.path, f.data, 0o600); err != nil {
			p.tb.Fatalf("testpki: writing %s: %v", f.path, err)
		}
	}
	return certFile, keyFile, caFile
}

func (p *PKI) client(class, service string) (tls.Certificate, []byte, []byte) {
	p.tb.Helper()
	return p.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: class + "/" + service},
		URIs:        []*url.URL{{Scheme: "farm", Host: class, Path: "/" + service}},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
}
