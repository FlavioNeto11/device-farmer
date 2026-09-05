package jobrunner

import (
	"testing"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
	"github.com/flaviopadilha/device-farmer/test/testpki"
)

// TestTheDefaultDialerPresentsThePlacementsFenceOnTheWire. Placement.Fence used
// to stop at the runner's own bookkeeping; on a host behind the fence proxy it
// now has to reach the socket, in the lease class, bound to the placement's
// devpath, or the proxy refuses the connection. This drives the default Dialer
// against the fake proxy-plus-server over mutual TLS and reads back what
// preceded the first request.
//
// The fake has no device at the position, so the transport switch is refused
// by the host — which is the host's business and not this test's. The claim
// under test was made before that answer.
//
// Falsify: return 0 for the token in JobRunner.dial's preamble, or drop the
// WithAdmissionPreamble option there. The recorded preamble then carries
// fence=0 or is empty.
func TestTheDefaultDialerPresentsThePlacementsFenceOnTheWire(t *testing.T) {
	t.Parallel()

	pki := testpki.New(t)
	srv := fakeadb.StartTLS(t, pki.ServerConfig())
	jr, _ := testLoop(t, func(c *Config) {
		c.ADBOptions = []adbwire.Option{adbwire.WithTLS(pki.ClientConfig("lease", "jobrunner"))}
	})

	dev, err := jr.dial(srv.Addr(), "usb:3-1.4", 41207)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, _ = dev.Shell(t.Context(), "true")

	reqs := srv.Requests()
	if len(reqs) == 0 {
		t.Fatal("nothing reached the host; the handshake or the preamble failed before the first request")
	}
	if reqs[0].Service != "host:transport:usb:3-1.4" {
		t.Fatalf("first request = %q, want the transport switch to the placement's devpath", reqs[0].Service)
	}
	if want := fakeadb.PreamblePrefix + " class=lease devpath=usb:3-1.4 fence=41207"; reqs[0].Preamble != want {
		t.Fatalf("preamble before the first request = %q, want %q", reqs[0].Preamble, want)
	}
}

// TestADialerWithoutACertificateChangesNothingOnTheWire is the other half:
// the default Dialer installs the preamble unconditionally, and on a plain
// host that must cost nothing. The fake here is a bare ADB server, which would
// FAIL a preamble as an unknown host service.
//
// Falsify: remove the TLS gate in adbwire's Client.dial.
func TestADialerWithoutACertificateChangesNothingOnTheWire(t *testing.T) {
	t.Parallel()

	srv := fakeadb.Start(t)
	srv.Add(fakeadb.Device{Serial: "X", Devpath: "usb:3-1.4"})
	jr, _ := testLoop(t, nil)

	dev, err := jr.dial(srv.Addr(), "usb:3-1.4", 41207)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, _ = dev.Shell(t.Context(), "true")

	reqs := srv.Requests()
	if len(reqs) == 0 {
		t.Fatal("nothing reached the host")
	}
	if reqs[0].Preamble != "" || reqs[0].Service != "host:transport:usb:3-1.4" {
		t.Fatalf("on a plain host the first frame must be the transport switch and nothing else; got preamble %q, service %q",
			reqs[0].Preamble, reqs[0].Service)
	}
}
