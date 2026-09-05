package config

// The client half of the fence proxy is opt-in by certificate. These tests are
// the two edges of that: unset means nothing on the wire changes and the
// summary says so; set means the files must all be there and must parse, at
// boot, with the variable named.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flaviopadilha/device-farmer/test/testpki"
)

// TestFenceClientIsOffUntilAllThreeFilesAreSet. A certificate without a CA
// cannot verify the proxy; a CA without a certificate has nothing to present.
// Either half alone is a deployment that fails its first handshake with a
// message an operator has to decode, so it is refused here instead.
//
// Falsify: replace `set != 3` in FenceClient.problems with `set == 0`.
func TestFenceClientIsOffUntilAllThreeFilesAreSet(t *testing.T) {
	env(t, withDSN(nil))
	cfg, err := Load("jobrunner")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FenceClient.Enabled() || cfg.FenceClient.TLS != nil {
		t.Fatal("no FARM_FENCE_CLIENT_* set, yet the client would dial hosts over TLS")
	}
	if s := cfg.Summary(); !strings.Contains(s, "fence client     = off") ||
		!strings.Contains(s, "in the clear") || !strings.Contains(s, "PostgreSQL only") {
		t.Errorf("the summary does not say plainly that the fence proxy is not in use:\n%s", s)
	}

	dir := t.TempDir()
	certFile, keyFile, caFile := testpki.New(t).WriteClientPEM(dir, "maintenance", "watchdog")
	for _, partial := range []map[string]string{
		{EnvFenceClientCert: certFile},
		{EnvFenceClientCert: certFile, EnvFenceClientKey: keyFile},
		{EnvFenceClientCA: caFile},
	} {
		env(t, withDSN(partial))
		_, err := Load("jobrunner")
		if err == nil {
			t.Fatalf("Load accepted a partial fence client configuration %v", partial)
		}
		for _, name := range []string{EnvFenceClientCert, EnvFenceClientKey, EnvFenceClientCA} {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal of %v does not name %s:\n%v", partial, name, err)
			}
		}
	}
}

// TestFenceClientLoadsAndDescribesTheCertificate. With all three set, every
// adbwire client in the process gets a TLS configuration presenting the
// certificate and trusting the CA, and the startup summary shows the operator
// which certificate — subject, SAN class, expiry — this process will present.
//
// Falsify: stop assigning cfg.FenceClient.TLS in Load.
func TestFenceClientLoadsAndDescribesTheCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, caFile := testpki.New(t).WriteClientPEM(dir, "maintenance", "watchdog")
	env(t, withDSN(map[string]string{
		EnvFenceClientCert: certFile,
		EnvFenceClientKey:  keyFile,
		EnvFenceClientCA:   caFile,
	}))
	cfg, err := Load("watchdog")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.FenceClient.Enabled() || cfg.FenceClient.TLS == nil {
		t.Fatal("all three files set, yet no TLS configuration was built")
	}
	if len(cfg.FenceClient.TLS.Certificates) != 1 || cfg.FenceClient.TLS.RootCAs == nil {
		t.Fatalf("the TLS configuration must present one certificate and trust the CA: %+v", cfg.FenceClient.TLS)
	}
	if cfg.FenceClient.TLS.MinVersion < 0x0304 {
		t.Errorf("MinVersion = %#x, want TLS 1.3; the proxy serves nothing older", cfg.FenceClient.TLS.MinVersion)
	}
	s := cfg.Summary()
	for _, want := range []string{"fence client     = mTLS", certFile, "farm://maintenance/watchdog", "expires"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary omits %q:\n%s", want, s)
		}
	}
	// Validate must agree with Load about a configuration built by hand,
	// including one whose files are set but not yet loaded.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejects the configuration Load accepted: %v", err)
	}
}

// TestFenceClientRefusesFilesThatDoNotParse. The wrong file, an empty CA, a
// key that does not match: each is refused at boot with the variable named,
// rather than an hour later as an opaque handshake error against a host.
//
// Falsify: in FenceClient.build, accept a CA file that holds no PEM
// certificate (make the AppendCertsFromPEM check unreachable); the "ca is
// junk" case then loads. Dropping build() from problems() alone is NOT a
// falsification — Load parses the files a second time and refuses there.
func TestFenceClientRefusesFilesThatDoNotParse(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, caFile := testpki.New(t).WriteClientPEM(dir, "enroll", "enroll")
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.pem")

	for _, tc := range []struct {
		name          string
		cert, key, ca string
		wantVar       string
	}{
		{"cert missing on disk", missing, keyFile, caFile, EnvFenceClientCert},
		{"cert is junk", junk, keyFile, caFile, EnvFenceClientCert},
		{"key does not match", certFile, caFile, caFile, EnvFenceClientKey},
		{"ca is junk", certFile, keyFile, junk, EnvFenceClientCA},
		{"ca missing on disk", certFile, keyFile, missing, EnvFenceClientCA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env(t, withDSN(map[string]string{
				EnvFenceClientCert: tc.cert,
				EnvFenceClientKey:  tc.key,
				EnvFenceClientCA:   tc.ca,
			}))
			_, err := Load("recovery")
			if err == nil {
				t.Fatal("Load accepted a fence client configuration whose files do not parse")
			}
			if !strings.Contains(err.Error(), tc.wantVar) {
				t.Errorf("the refusal does not name %s:\n%v", tc.wantVar, err)
			}
		})
	}
}
