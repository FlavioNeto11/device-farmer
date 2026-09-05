package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/fenceproxy"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// ---------------------------------------------------------------------------
// An ephemeral PKI
//
// One CA, one server certificate for 127.0.0.1, and one client certificate per
// credential class, minted in-process. Section 9 of the design document says
// the class travels in a farm://<class>/<service> URI SAN; that is the only
// thing about these certificates the proxy reads.
// ---------------------------------------------------------------------------

type testPKI struct {
	caFile, certFile, keyFile string
	roots                     *x509.CertPool
	lease, maintenance        tls.Certificate

	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()

	caKey := newKey(t)
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "farm test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("minting the CA: %v", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parsing the CA: %v", err)
	}
	p := &testPKI{
		caFile:   filepath.Join(dir, "ca.pem"),
		certFile: filepath.Join(dir, "server.pem"),
		keyFile:  filepath.Join(dir, "server-key.pem"),
		roots:    x509.NewCertPool(),
		ca:       ca,
		caKey:    caKey,
	}
	p.roots.AddCert(ca)
	writeFile(t, p.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))

	p.writeServerCert(t, "proxy.test")
	p.lease = p.clientCert(t, "jobrunner", "farm://lease/jobrunner")
	p.maintenance = p.clientCert(t, "recovery", "farm://maintenance/recovery")
	return p
}

// writeServerCert (re)writes the proxy's certificate and key to disk. cn is
// what a test reads back to prove which certificate is being served.
func (p *testPKI) writeServerCert(t *testing.T, cn string) {
	t.Helper()
	key := newKey(t)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatalf("minting the server certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling the server key: %v", err)
	}
	writeFile(t, p.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, p.keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func (p *testPKI) clientCert(t *testing.T, cn, uri string) tls.Certificate {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parsing %q: %v", uri, err)
	}
	key := newKey(t)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatalf("minting the %s client certificate: %v", cn, err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// client is a TLS configuration that trusts the test CA and presents cert.
func (p *testPKI) client(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		RootCAs:      p.roots,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	return key
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// The wire, from the client's side
// ---------------------------------------------------------------------------

// newTestAgentPool is the zero pool newTestAgent uses: New only stores it,
// and nothing these tests drive reaches the database through it.
func newTestAgentPool() *pgxpool.Pool { return &pgxpool.Pool{} }

// staticFloors is a FenceSource with a fixed answer.
type staticFloors map[string]int64

func (s staticFloors) Floors(context.Context) (fenceproxy.Snapshot, error) {
	return fenceproxy.Snapshot{Floors: map[string]int64(s)}, nil
}

func frame(payload string) []byte {
	return []byte(fmt.Sprintf("%04x%s", len(payload), payload))
}

func readFrameFrom(t *testing.T, r io.Reader) string {
	t.Helper()
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		t.Fatalf("reading a length prefix: %v", err)
	}
	var n int
	if _, err := fmt.Sscanf(string(hdr[:]), "%04x", &n); err != nil {
		t.Fatalf("length prefix %q: %v", hdr, err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("reading a %d-byte frame: %v", n, err)
	}
	return string(buf)
}

// ask opens one mTLS connection to the proxy, sends the preamble and one
// service request, and returns the status word plus the FAIL reason if any.
func ask(t *testing.T, addr string, tlsCfg *tls.Config, preamble, service string) (status, reason string) {
	t.Helper()
	d := &net.Dialer{Timeout: 5 * time.Second}
	c, err := tls.DialWithDialer(d, "tcp", addr, tlsCfg)
	if err != nil {
		t.Fatalf("dialling the proxy: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := c.Write(frame(preamble)); err != nil {
		t.Fatalf("writing the preamble: %v", err)
	}
	if _, err := c.Write(frame(service)); err != nil {
		t.Fatalf("writing the service request: %v", err)
	}
	var st [4]byte
	if _, err := io.ReadFull(c, st[:]); err != nil {
		t.Fatalf("reading the status word: %v", err)
	}
	status = string(st[:])
	if status == "FAIL" {
		reason = readFrameFrom(t, c)
	}
	return status, reason
}

// startFence builds an agent with the proxy on, runs its fence loop, and
// waits until it is listening and has read the floors once.
func startFence(t *testing.T, pki *testPKI, upstream string, floors fenceproxy.FenceSource, mutate func(*Config)) (*Agent, *recorder) {
	t.Helper()
	cfg := Config{
		HostID:      "h01",
		ADBEndpoint: upstream,
		Fence: FenceConfig{
			CertFile: pki.certFile, KeyFile: pki.keyFile, CAFile: pki.caFile,
			Listen:       "127.0.0.1:0",
			Advertise:    "proxy.test:5038",
			PollInterval: 10 * time.Millisecond,
			Floors:       floors,
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a, rec := newTestAgent(t, cfg)
	a.fence.backoff = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.fenceLoop(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the fence loop did not stop within 10s of cancellation")
		}
	})
	waitFor(t, 10*time.Second, "the proxy to listen", func() bool { return a.FenceAddr() != "" })
	return a, rec
}

const devA = "usb:3-1.4"

// TestFenceProxyRefusesBelowTheFloorAndAdmitsAbove is the end-to-end proof
// that the fence reaches the device: a real fenceproxy.Server, real mTLS on
// both sides, a real fakeadb behind it. A lease-class client whose fence is
// below the floor gets a readable FAIL on its transport switch and never
// reaches the adb server; one at the floor is switched through and gets the
// server's OKAY.
func TestFenceProxyRefusesBelowTheFloorAndAdmitsAbove(t *testing.T) {
	pki := newTestPKI(t)
	adb := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SER1", Devpath: devA}))
	a, _ := startFence(t, pki, adb.Addr(), staticFloors{devA: 100}, nil)
	waitFor(t, 10*time.Second, "the first floor read", func() bool { return a.fence.cache.View(devA).Known })

	status, reason := ask(t, a.FenceAddr(), pki.client(pki.lease),
		"fence:v1 class=lease devpath="+devA+" fence=99", "host:transport:"+devA)
	if status != "FAIL" {
		t.Fatalf("a fence below the floor got %q, want FAIL", status)
	}
	if !strings.Contains(reason, "below the floor") {
		t.Errorf("the FAIL reason does not say the fence is below the floor: %q", reason)
	}
	if n := len(adb.RequestsTo(devA)); n != 0 {
		t.Errorf("the refused connection reached the adb server: %d requests to %s", n, devA)
	}

	status, reason = ask(t, a.FenceAddr(), pki.client(pki.lease),
		"fence:v1 class=lease devpath="+devA+" fence=100", "host:transport:"+devA)
	if status != "OKAY" {
		t.Fatalf("a fence at the floor got %q (%s), want OKAY from the adb server", status, reason)
	}
	waitFor(t, 10*time.Second, "the admitted transport switch to reach the adb server", func() bool {
		return len(adb.RequestsTo(devA)) == 1
	})
}

// TestFenceProxyClassComesFromTheCertificate: the preamble says "maintenance",
// the certificate says "lease", and the certificate wins — the connection is
// held to the lease rules, which do not include host:kill for anyone. A real
// maintenance certificate opens what its whitelist allows and nothing more.
func TestFenceProxyClassComesFromTheCertificate(t *testing.T) {
	pki := newTestPKI(t)
	adb := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SER1", Devpath: devA}))
	a, _ := startFence(t, pki, adb.Addr(), staticFloors{devA: 100}, nil)
	waitFor(t, 10*time.Second, "the first floor read", func() bool { return a.fence.cache.View(devA).Known })

	status, reason := ask(t, a.FenceAddr(), pki.client(pki.lease),
		"fence:v1 class=maintenance devpath="+devA+" fence=100", "host:devices-l")
	if status != "FAIL" || !strings.Contains(reason, "lease-class") {
		t.Errorf("a lease certificate claiming maintenance in its preamble got %q %q; "+
			"want the lease rules applied", status, reason)
	}

	status, _ = ask(t, a.FenceAddr(), pki.client(pki.maintenance),
		"fence:v1 class=maintenance", "host:devices-l")
	if status != "OKAY" {
		t.Errorf("a maintenance certificate was refused host:devices-l: %q", status)
	}
	status, reason = ask(t, a.FenceAddr(), pki.client(pki.maintenance),
		"fence:v1 class=maintenance", "host:kill")
	if status != "FAIL" || !strings.Contains(reason, "whitelist") {
		t.Errorf("a maintenance certificate was allowed host:kill: %q %q", status, reason)
	}
}

// TestFenceProxyRefusesAClientTheCADidNotSign: mTLS is the door. A client
// certificate from another CA does not complete the handshake at all, so the
// request behind it — one that WOULD be admitted on the fence alone — never
// gets a status word, let alone reaches the adb server.
func TestFenceProxyRefusesAClientTheCADidNotSign(t *testing.T) {
	pki := newTestPKI(t)
	other := newTestPKI(t)
	adb := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SER1", Devpath: devA}))
	a, _ := startFence(t, pki, adb.Addr(), staticFloors{devA: 100}, nil)
	waitFor(t, 10*time.Second, "the first floor read", func() bool { return a.fence.cache.View(devA).Known })

	// Trusts the proxy's CA, presents a certificate the proxy's CA did not sign.
	cfg := pki.client(other.lease)
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", a.FenceAddr(), cfg)
	if err == nil {
		// TLS 1.3 reports the server's rejection on the first read, not the
		// dial. A fence at the floor and a transport switch is the request
		// the proxy admits from a client it trusts; from this one it must
		// answer with an alert, never a status word.
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write(frame("fence:v1 class=lease devpath=" + devA + " fence=100"))
		_, _ = c.Write(frame("host:transport:" + devA))
		var b [4]byte
		_, err = io.ReadFull(c, b[:])
		_ = c.Close()
		if err == nil {
			t.Fatalf("a client certificate from a foreign CA was admitted through the "+
				"handshake and its request answered %q", b)
		}
	}
	if n := len(adb.RequestsTo(devA)); n != 0 {
		t.Errorf("the foreign client reached the adb server: %d requests to %s", n, devA)
	}
}

// TestFenceProxyRestartsAListenerThatDied: the accept loop is not allowed to
// stay dead. A listener whose Accept fails is replaced, with backoff, and the
// advertised endpoint never changes in the meantime.
func TestFenceProxyRestartsAListenerThatDied(t *testing.T) {
	pki := newTestPKI(t)
	adb := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: "SER1", Devpath: devA}))

	cfg := Config{
		HostID:      "h01",
		ADBEndpoint: adb.Addr(),
		Fence: FenceConfig{
			CertFile: pki.certFile, KeyFile: pki.keyFile, CAFile: pki.caFile,
			Listen:       "127.0.0.1:0",
			Advertise:    "proxy.test:5038",
			PollInterval: 10 * time.Millisecond,
			Floors:       staticFloors{devA: 100},
		},
	}
	a, rec := newTestAgent(t, cfg)
	a.fence.backoff = time.Millisecond

	// The first listener dies on its first accept; the second is real.
	realListen := a.fence.listen
	attempts := 0
	a.fence.listen = func(addr string) (net.Listener, error) {
		attempts++
		if attempts == 1 {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			return &dyingListener{Listener: ln}, nil
		}
		return realListen(addr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = a.fenceLoop(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	waitFor(t, 10*time.Second, "the replacement listener", func() bool { return attempts >= 2 && a.FenceAddr() != "" })
	waitFor(t, 10*time.Second, "the first floor read", func() bool { return a.fence.cache.View(devA).Known })

	if got := a.advertisedEndpoint(); got != "proxy.test:5038" {
		t.Errorf("the advertised endpoint moved to %q while the listener was restarted", got)
	}
	status, reason := ask(t, a.FenceAddr(), pki.client(pki.lease),
		"fence:v1 class=lease devpath="+devA+" fence=100", "host:transport:"+devA)
	if status != "OKAY" {
		t.Fatalf("the replacement listener does not serve: %q %q", status, reason)
	}

	var restartLogged bool
	for _, r := range rec.above(slog.LevelError) {
		if strings.Contains(r.Message, "restarting it") {
			restartLogged = true
		}
	}
	if !restartLogged {
		t.Errorf("the listener restart was not logged as an error:\n%s",
			strings.Join(messages(rec.above(slog.LevelInfo)), "\n"))
	}
}

// dyingListener fails its first Accept the way a listener whose descriptor
// was taken away does.
type dyingListener struct{ net.Listener }

func (l *dyingListener) Accept() (net.Conn, error) {
	_ = l.Listener.Close()
	return nil, errors.New("accept: bad file descriptor")
}

// TestCertReloaderPicksUpARotationWithoutRestart: design document section
// 9.2. A rotation is a file write, and the next handshake serves it.
func TestCertReloaderPicksUpARotationWithoutRestart(t *testing.T) {
	pki := newTestPKI(t)
	r, err := newCertReloader(pki.certFile, pki.keyFile, slog.New(&recorder{}))
	if err != nil {
		t.Fatal(err)
	}
	r.recheck = 0

	leafCN := func() string {
		c, err := r.get(nil)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(c.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return leaf.Subject.CommonName
	}
	if got := leafCN(); got != "proxy.test" {
		t.Fatalf("serving %q, want the certificate on disk", got)
	}

	pki.writeServerCert(t, "proxy.test rotated")
	// Filesystems round modification times; make the write unmistakable.
	then := time.Now().Add(2 * time.Second)
	for _, f := range []string{pki.certFile, pki.keyFile} {
		if err := os.Chtimes(f, then, then); err != nil {
			t.Fatal(err)
		}
	}
	if got := leafCN(); got != "proxy.test rotated" {
		t.Fatalf("serving %q after the rotation, want the new certificate", got)
	}

	// A broken rotation keeps the last good certificate in service.
	writeFile(t, pki.certFile, []byte("not a certificate"))
	later := then.Add(2 * time.Second)
	if err := os.Chtimes(pki.certFile, later, later); err != nil {
		t.Fatal(err)
	}
	if got := leafCN(); got != "proxy.test rotated" {
		t.Fatalf("serving %q after a broken rotation, want the previous certificate kept", got)
	}
}

// TestNewRefusesPartialOrBrokenFenceMaterial: one or two of the three PEMs is
// a misconfiguration, not a proxy with fewer features, and a file that does
// not parse is refused before the agent registers anything.
func TestNewRefusesPartialOrBrokenFenceMaterial(t *testing.T) {
	pki := newTestPKI(t)

	_, err := New(Config{Pool: newTestAgentPool(), HostID: "h01",
		Fence: FenceConfig{CertFile: pki.certFile, KeyFile: pki.keyFile, Listen: ":0"}})
	if err == nil || !strings.Contains(err.Error(), "2 of the three") {
		t.Errorf("two of three PEMs accepted: %v", err)
	}

	bad := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, bad, []byte("-----BEGIN NOTHING-----\n"))
	_, err = New(Config{Pool: newTestAgentPool(), HostID: "h01",
		Fence: FenceConfig{CertFile: pki.certFile, KeyFile: pki.keyFile, CAFile: bad, Listen: ":0"}})
	if err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Errorf("an unparseable CA accepted: %v", err)
	}

	_, err = New(Config{Pool: newTestAgentPool(), HostID: "h01",
		Fence: FenceConfig{CertFile: pki.caFile, KeyFile: pki.keyFile, CAFile: pki.caFile, Listen: ":0"}})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("a certificate that does not match its key accepted: %v", err)
	}

	// All three, and the zero value, are fine.
	if _, err := New(Config{Pool: newTestAgentPool(), HostID: "h01", Fence: FenceConfig{
		CertFile: pki.certFile, KeyFile: pki.keyFile, CAFile: pki.caFile,
		Listen: "127.0.0.1:5038"}}); err != nil {
		t.Errorf("a complete configuration refused: %v", err)
	}
	if _, err := New(Config{Pool: newTestAgentPool(), HostID: "h01"}); err != nil {
		t.Errorf("the proxy off refused: %v", err)
	}
}

// TestAdvertiseAddr pins the derivation an operator gets when they set the
// listener and nothing else.
func TestAdvertiseAddr(t *testing.T) {
	ifaces := func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
			&net.IPNet{IP: net.ParseIP("2001:db8::7"), Mask: net.CIDRMask(64, 128)},
			&net.IPNet{IP: net.ParseIP("10.20.0.11"), Mask: net.CIDRMask(24, 32)},
		}, nil
	}
	noIfaces := func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}}, nil
	}

	cases := []struct {
		name, listen, adb string
		ifaces            func() ([]net.Addr, error)
		want              string
		wantErr           bool
	}{
		{"a specific listen host is advertised as written", "10.0.0.5:5038", "127.0.0.1:5037", ifaces, "10.0.0.5:5038", false},
		{"loopback bound explicitly is honoured", "127.0.0.1:5038", "127.0.0.1:5037", ifaces, "127.0.0.1:5038", false},
		{"wildcard takes the routable adb host", ":5038", "10.20.0.11:5037", ifaces, "10.20.0.11:5038", false},
		{"0.0.0.0 takes the routable adb host", "0.0.0.0:5038", "h01.lab:5037", ifaces, "h01.lab:5038", false},
		{"a loopback adb host is not advertised", ":5038", "127.0.0.1:5037", ifaces, "10.20.0.11:5038", false},
		{"localhost is loopback", "[::]:5038", "localhost:5037", ifaces, "10.20.0.11:5038", false},
		{"IPv6 when there is no IPv4", ":5038", "127.0.0.1:5037",
			func() ([]net.Addr, error) {
				return []net.Addr{&net.IPNet{IP: net.ParseIP("2001:db8::7"), Mask: net.CIDRMask(64, 128)}}, nil
			}, "[2001:db8::7]:5038", false},
		{"nothing reachable is an error, not loopback", ":5038", "127.0.0.1:5037", noIfaces, "", true},
		{"a listen address without a port is an error", "10.0.0.5", "127.0.0.1:5037", ifaces, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := AdvertiseAddr(c.listen, c.adb, c.ifaces)
			if c.wantErr {
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
				if !strings.Contains(err.Error(), "FARM_FENCE_ADVERTISE") && !strings.Contains(err.Error(), "listen address") {
					t.Errorf("the error does not say what to set: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("AdvertiseAddr(%q, %q) = %q, want %q", c.listen, c.adb, got, c.want)
			}
		})
	}
}

// TestAdvertisedEndpointIsTheProxyWhenItIsOn is the in-memory half of the
// farm.hosts assertion; fencesource_test.go proves the row. The endpoint stays
// the proxy's while the proxy is not listening: clients fail closed rather
// than falling back to the unfenced server.
func TestAdvertisedEndpointIsTheProxyWhenItIsOn(t *testing.T) {
	pki := newTestPKI(t)
	on, _ := newTestAgent(t, Config{HostID: "h01", ADBEndpoint: "10.20.0.11:5037", Fence: FenceConfig{
		CertFile: pki.certFile, KeyFile: pki.keyFile, CAFile: pki.caFile,
		Listen: ":5038",
	}})
	if got := on.advertisedEndpoint(); got != "10.20.0.11:5038" {
		t.Errorf("with the proxy on and no advertise address, advertising %q; want the adb host on the proxy port", got)
	}
	if on.FenceAddr() != "" {
		t.Error("the proxy reports a listener before its loop has run")
	}

	off, _ := newTestAgent(t, Config{HostID: "h01", ADBEndpoint: "10.20.0.11:5037"})
	if got := off.advertisedEndpoint(); got != "10.20.0.11:5037" {
		t.Errorf("with the proxy off, advertising %q; want the adb server", got)
	}
}
