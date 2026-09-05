package config

// U10 — the fence proxy's knobs. The environment discipline is the one
// config_test.go states: every test starts from a known-empty environment.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type pemFiles struct{ cert, key, ca string }

// testPEMs mints one self-signed certificate and writes it out as the proxy's
// certificate, its key, and — since it signs itself — a CA that parses. What
// the files SAY is not under test here; that they open and parse is.
func testPEMs(t *testing.T) pemFiles {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "farm test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := pemFiles{
		cert: filepath.Join(dir, "server.pem"),
		key:  filepath.Join(dir, "server-key.pem"),
		ca:   filepath.Join(dir, "ca.pem"),
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for path, data := range map[string][]byte{
		p.cert: certPEM,
		p.ca:   certPEM,
		p.key:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// TestFenceMaterialIsAllOrNone. One or two of the three PEMs is a proxy that
// would either admit anyone or fail every handshake, and both look like a
// healthy host from the cluster; the only visible failure is refusing to boot.
func TestFenceMaterialIsAllOrNone(t *testing.T) {
	pems := testPEMs(t)
	cases := []struct {
		name string
		envs map[string]string
		want []string
	}{
		{"cert only", map[string]string{EnvFenceTLSCert: pems.cert},
			[]string{"1 of the three is set", EnvFenceTLSKey, EnvFenceTLSCA}},
		{"cert and key", map[string]string{EnvFenceTLSCert: pems.cert, EnvFenceTLSKey: pems.key},
			[]string{"2 of the three are set"}},
		{"ca only", map[string]string{EnvFenceTLSCA: pems.ca},
			[]string{"1 of the three is set", "cannot listen"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env(t, withDSN(c.envs))
			_, err := Load("node")
			if err == nil {
				t.Fatal("a partial set of fence PEMs was accepted")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not mention %q:\n%v", w, err)
				}
			}
		})
	}

	env(t, withDSN(map[string]string{
		EnvFenceTLSCert: pems.cert, EnvFenceTLSKey: pems.key, EnvFenceTLSCA: pems.ca,
	}))
	cfg, err := Load("node")
	if err != nil {
		t.Fatalf("all three refused: %v", err)
	}
	if !cfg.Fence.Enabled() {
		t.Error("all three set and Fence.Enabled() is false")
	}
}

// TestFencePEMsMustExistAndParse. The files are opened at preflight, for
// every role that carries the variables, so a bad path fails the rollout and
// not the first handshake on a host that is already advertising the proxy.
func TestFencePEMsMustExistAndParse(t *testing.T) {
	pems := testPEMs(t)
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("-----BEGIN NOTHING-----\nAAAA\n-----END NOTHING-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.pem")

	cases := []struct {
		name string
		envs map[string]string
		want string
	}{
		{"missing certificate",
			map[string]string{EnvFenceTLSCert: missing, EnvFenceTLSKey: pems.key, EnvFenceTLSCA: pems.ca},
			EnvFenceTLSCert},
		{"garbage key",
			map[string]string{EnvFenceTLSCert: pems.cert, EnvFenceTLSKey: garbage, EnvFenceTLSCA: pems.ca},
			"private key"},
		{"missing CA",
			map[string]string{EnvFenceTLSCert: pems.cert, EnvFenceTLSKey: pems.key, EnvFenceTLSCA: missing},
			EnvFenceTLSCA},
		{"CA with no certificate in it",
			map[string]string{EnvFenceTLSCert: pems.cert, EnvFenceTLSKey: pems.key, EnvFenceTLSCA: garbage},
			"no PEM-encoded certificate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env(t, withDSN(c.envs))
			_, err := Load("node")
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error does not mention %q:\n%v", c.want, err)
			}
		})
	}
}

// TestFenceAddressesAndPollAreValidated. The listen address must be
// bindable, the advertised one must name a host — it is what other machines
// dial — and the poll must land more often than the view is allowed to age,
// or a healthy proxy refuses everything between polls.
func TestFenceAddressesAndPollAreValidated(t *testing.T) {
	cases := []struct {
		name string
		envs map[string]string
		want []string
	}{
		{"listen without a port", map[string]string{EnvFenceListen: "5038"}, []string{EnvFenceListen}},
		{"advertise without a host", map[string]string{EnvFenceAdvertise: ":5038"},
			[]string{EnvFenceAdvertise, "names no host"}},
		{"advertise on the wildcard", map[string]string{EnvFenceAdvertise: "0.0.0.0:5038"},
			[]string{EnvFenceAdvertise, "names no host"}},
		{"poll not positive", map[string]string{EnvFencePollInterval: "0s"}, []string{EnvFencePollInterval}},
		{"poll at the staleness budget", map[string]string{EnvFencePollInterval: "20s"},
			[]string{EnvFencePollInterval, EnvNodeSelfFence, "stale before it is refreshed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env(t, withDSN(c.envs))
			_, err := Load("api")
			if err == nil {
				t.Fatal("accepted")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not mention %q:\n%v", w, err)
				}
			}
		})
	}

	env(t, withDSN(map[string]string{EnvFenceAdvertise: "h01.lab:5038", EnvFencePollInterval: "19s"}))
	if _, err := Load("api"); err != nil {
		t.Errorf("a routable advertise address and a poll inside the budget were refused: %v", err)
	}
}

// TestSummaryStatesWhetherTheFenceIsEnforced. The self-fence timeout and the
// margin are printed by every role whether or not a proxy exists; a summary
// that printed them alone would read as a fence enforced at the device on a
// farm with no proxy at all.
func TestSummaryStatesWhetherTheFenceIsEnforced(t *testing.T) {
	env(t, withDSN(nil))
	off, err := Load("node")
	if err != nil {
		t.Fatal(err)
	}
	s := off.Summary()
	for _, want := range []string{"fence proxy      = off", "NOT enforced at the device", EnvFenceTLSCert} {
		if !strings.Contains(s, want) {
			t.Errorf("summary with the proxy off omits %q:\n%s", want, s)
		}
	}

	pems := testPEMs(t)
	env(t, withDSN(map[string]string{
		EnvFenceTLSCert: pems.cert, EnvFenceTLSKey: pems.key, EnvFenceTLSCA: pems.ca,
		EnvFenceAdvertise: "h01.lab:5038",
	}))
	on, err := Load("node")
	if err != nil {
		t.Fatal(err)
	}
	s = on.Summary()
	for _, want := range []string{"fence proxy      = on", ":5038", "h01.lab:5038", "2s", "20s"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary with the proxy on omits %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "served by the node role only") {
		t.Errorf("the node role's own summary disclaims serving the proxy:\n%s", s)
	}

	env(t, withDSN(map[string]string{
		EnvFenceTLSCert: pems.cert, EnvFenceTLSKey: pems.key, EnvFenceTLSCA: pems.ca,
	}))
	other, err := Load("watchdog")
	if err != nil {
		t.Fatal(err)
	}
	if s := other.Summary(); !strings.Contains(s, "served by the node role only") {
		t.Errorf("a non-node role with the knobs set does not say who serves the proxy:\n%s", s)
	}
}
