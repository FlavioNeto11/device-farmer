package adbwire

// The admission preamble: what a connection says about itself to the fence
// proxy fronting a host, and — just as important — when it says nothing.
//
// The proxy's contract (docs/design/fence-proxy.md §3.1) is one frame in ADB's
// own framing, sent after the TLS handshake and before the first host request,
// never acknowledged. The tests here are exempt from this package's vocabulary
// scan, so they can spell the frame out in full and check it byte for byte.

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"

	"github.com/flaviopadilha/device-farmer/test/fakeadb"
	"github.com/flaviopadilha/device-farmer/test/testpki"
)

// readRawFrame reads one length-prefixed frame off a raw socket and returns
// it prefix included, so a test can check the framing as well as the payload.
func readRawFrame(r io.Reader) (string, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", fmt.Errorf("reading a frame header: %w", err)
	}
	n, err := strconv.ParseUint(string(hdr[:]), 16, 32)
	if err != nil {
		return "", fmt.Errorf("frame header %q is not four hex digits", hdr)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("reading a %d-byte frame body: %w", n, err)
	}
	return string(hdr[:]) + string(buf), nil
}

// rawListener accepts one connection, reads the first `want` frames off it and
// closes it. With a TLS config it terminates TLS first, standing in for the
// proxy. It answers nothing: what is under test is what reached the wire.
func rawListener(t *testing.T, cfg *tls.Config, want int) (addr string, frames func() []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if cfg != nil {
		ln = tls.NewListener(ln, cfg)
	}
	t.Cleanup(func() { _ = ln.Close() })

	type capture struct {
		frames []string
		err    error
	}
	out := make(chan capture, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			out <- capture{err: err}
			return
		}
		defer c.Close()
		var got capture
		for len(got.frames) < want {
			f, err := readRawFrame(c)
			if err != nil {
				got.err = err
				break
			}
			got.frames = append(got.frames, f)
		}
		out <- got
	}()
	return ln.Addr().String(), func() []string {
		t.Helper()
		got := <-out
		if got.err != nil {
			t.Fatalf("after %d frame(s): %v", len(got.frames), got.err)
		}
		return got.frames
	}
}

// TestPreambleIsTheFirstFrameOnATLSConnection is the contract, byte for byte:
// after the handshake, the documented frame, then the ordinary host request.
//
// Falsify: drop the writeMessage in Client.dial, or reorder it after the host
// request. Either way the first frame read here is "host:version".
func TestPreambleIsTheFirstFrameOnATLSConnection(t *testing.T) {
	t.Parallel()

	pki := testpki.New(t)
	addr, frames := rawListener(t, pki.ServerConfig(), 2)

	cli := New(addr,
		WithLogger(quietLogger()),
		WithTLS(pki.ClientConfig("lease", "jobrunner")),
		WithAdmissionPreamble(func() (string, string, int64, bool) {
			return "lease", "usb:3-1.4", 41207, true
		}),
	)
	_, _ = cli.Version(testContext(t))

	got := frames()
	const want = "fence:v1 class=lease devpath=usb:3-1.4 fence=41207"
	if got[0] != "0032"+want {
		t.Fatalf("first frame on the wire = %q, want %q (four hex digits of length, then the payload)",
			got[0], "0032"+want)
	}
	if got[1] != "000chost:version" {
		t.Fatalf("second frame = %q, want the ordinary host request %q", got[1], "000chost:version")
	}
}

// TestAClassOnlyPreambleCarriesNoDevpathAndNoToken. A maintenance connection
// is bound to no device, and a frame that said "devpath= fence=0" would be
// refused by the proxy's strict parser rather than admitted on the whitelist.
//
// Falsify: write the devpath and token keys unconditionally in admissionFrame.
func TestAClassOnlyPreambleCarriesNoDevpathAndNoToken(t *testing.T) {
	t.Parallel()

	pki := testpki.New(t)
	addr, frames := rawListener(t, pki.ServerConfig(), 1)

	cli := New(addr,
		WithLogger(quietLogger()),
		WithTLS(pki.ClientConfig(AdmissionClassMaintenance, "watchdog")),
		WithAdmissionPreamble(AdmissionClass(AdmissionClassMaintenance)),
	)
	_, _ = cli.Version(testContext(t))

	got := frames()
	if want := "001afence:v1 class=maintenance"; got[0] != want {
		t.Fatalf("first frame = %q, want %q", got[0], want)
	}
}

// TestNothingChangesOnTheWireWithoutACertificate is the deployment promise:
// with no TLS configuration the first bytes a host sees are the host request,
// whether or not the preamble option was installed. Installing the option
// everywhere and switching it on with a certificate alone depends on this.
//
// Falsify: remove the `c.tls == nil` half of the guard in Client.dial. The
// "preamble installed, no TLS" case then puts the frame on a plain socket.
func TestNothingChangesOnTheWireWithoutACertificate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opts []Option
	}{
		{"no option at all", nil},
		{"preamble installed, no TLS", []Option{
			WithAdmissionPreamble(func() (string, string, int64, bool) {
				return "lease", "usb:3-1.4", 41207, true
			}),
		}},
		// The shape every role in cmd/farmd builds: both options installed
		// unconditionally, the TLS config nil because FARM_FENCE_CLIENT_* is
		// unset. This is the case the deployment promise is actually made on.
		{"preamble installed, WithTLS(nil)", []Option{
			WithTLS(nil),
			WithAdmissionPreamble(AdmissionClass(AdmissionClassMaintenance)),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			addr, frames := rawListener(t, nil, 1)
			cli := New(addr, append([]Option{WithLogger(quietLogger())}, tc.opts...)...)
			_, _ = cli.Version(testContext(t))

			got := frames()
			if got[0] != "000chost:version" {
				t.Fatalf("first frame on a plain connection = %q, want %q; the preamble must never reach a bare ADB server",
					got[0], "000chost:version")
			}
		})
	}
}

// TestThePreambleIsConsumedByTheProxyAndTheCallSucceeds drives a whole call
// through the fake proxy-plus-server: mutual TLS with a client certificate
// verified against the test CA, the preamble stripped, and host:version
// answered as usual. It is also the proof that ServerName is filled in from
// the endpoint — the client config carries none, and a handshake against a
// certificate for 127.0.0.1 would not complete without it.
//
// Falsify: delete the ServerName resolution in New; the handshake fails with
// "either ServerName or InsecureSkipVerify must be specified".
func TestThePreambleIsConsumedByTheProxyAndTheCallSucceeds(t *testing.T) {
	t.Parallel()

	pki := testpki.New(t)
	srv := fakeadb.StartTLS(t, pki.ServerConfig())
	cli := New(srv.Addr(),
		WithLogger(quietLogger()),
		WithTLS(pki.ClientConfig(AdmissionClassMaintenance, "recovery")),
		WithAdmissionPreamble(AdmissionClass(AdmissionClassMaintenance)),
	)

	v, err := cli.Version(testContext(t))
	if err != nil {
		t.Fatalf("Version over TLS with a preamble: %v", err)
	}
	if v != fakeadb.DefaultHostVersion {
		t.Fatalf("version = %d, want %d", v, fakeadb.DefaultHostVersion)
	}
	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("the fake recorded %d requests, want 1", len(reqs))
	}
	if reqs[0].Service != "host:version" {
		t.Fatalf("service = %q, want host:version; the preamble leaked into the request stream", reqs[0].Service)
	}
	if want := fakeadb.PreamblePrefix + " class=" + AdmissionClassMaintenance; reqs[0].Preamble != want {
		t.Fatalf("preamble recorded with the request = %q, want %q", reqs[0].Preamble, want)
	}
}

// TestAHandshakeTheClientCannotVerifyIsADialFailure. A server certificate the
// client's roots do not vouch for fails inside the dial, and it is reported as
// KindDial: from this side the host could not be reached, and the caller's
// retry policy for an unreachable host is the right one. It must not surface
// later as a write or read failure mid-request.
//
// Falsify: replace HandshakeContext with a lazy handshake (drop the call); the
// failure then arrives from the first write as KindWrite.
func TestAHandshakeTheClientCannotVerifyIsADialFailure(t *testing.T) {
	t.Parallel()

	serverPKI := testpki.New(t)
	strangerPKI := testpki.New(t)
	srv := fakeadb.StartTLS(t, serverPKI.ServerConfig())

	cli := New(srv.Addr(),
		WithLogger(quietLogger()),
		WithTLS(strangerPKI.ClientConfig(AdmissionClassMaintenance, "watchdog")),
	)
	_, err := cli.Version(testContext(t))
	te, ok := AsTransport(err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *TransportError", err, err)
	}
	if te.Kind != KindDial {
		t.Fatalf("kind = %s, want %s; a handshake that cannot complete is a host that cannot be reached", te.Kind, KindDial)
	}
}

// TestAdmissionFrameRefusesWhatWouldBecomeASecondKey. The frame is a
// space-separated key=value list, so a class or devpath carrying a space or
// an '=' would be parsed by the proxy as a key the client never meant to send.
// Refused before the dial, as a usage error, rather than sent and refused as
// malformed.
func TestAdmissionFrameRefusesWhatWouldBecomeASecondKey(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, class, devpath string
	}{
		{"empty class", "", ""},
		{"class with a space", "lease bypass=yes", "usb:1-1"},
		{"class with an equals", "class=lease", "usb:1-1"},
		{"devpath with a space", "lease", "usb:1-1 fence=999999"},
		{"devpath that is not a devpath", "lease", "../../etc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := admissionFrame(tc.class, tc.devpath, 1)
			var ue *UsageError
			if !errors.As(err, &ue) {
				t.Fatalf("admissionFrame(%q, %q) err = %v, want a *UsageError", tc.class, tc.devpath, err)
			}
		})
	}

	got, err := admissionFrame("lease", "usb:3-1.4", 41207)
	if err != nil {
		t.Fatalf("a well-formed claim was refused: %v", err)
	}
	if want := "fence:v1 class=lease devpath=usb:3-1.4 fence=41207"; got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}
