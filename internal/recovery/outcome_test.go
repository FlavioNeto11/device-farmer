package recovery

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
)

// This file is about one thing: an actuator that tells three different stories
// apart, all the way into farm.recovery_attempts.
//
//	refused     — the rung is not permitted here. The device is fine.
//	failed      — the rung ran and the hardware did not come back. Escalate.
//	unreachable — the agent or the host's adb server did not answer. NO rung
//	              will help, and climbing spends the whole rack's cooldown
//	              budget on a host that is simply gone.
//
// The third collapsing into the second is the DeviceFarmer/STF #663 shape one
// level up: a socket failure becomes evidence about a handset, the ladder
// escalates on that evidence, and phones that were never broken get rebooted
// and quarantined while their leases are still running.

const (
	testDevpath  = "usb:3-1.4"
	testHost     = "rack-a"
	testRackSlot = "A-04"
)

// ---------------------------------------------------------------------------
// A scriptable adb host server
// ---------------------------------------------------------------------------

// reply is one scripted answer. Exactly one shape applies.
type reply struct {
	// payload answers OKAY followed by a length-prefixed body, which is what
	// a host service returns.
	payload string
	// bare answers OKAY with nothing after it, which is what a transport
	// switch, a device-side service start and host:kill return.
	bare bool
	// fail answers a well-formed FAIL with this reason. The connection worked;
	// the request did not.
	fail string
	// rst severs the connection with a TCP RST before answering, so the client
	// reads ECONNRESET rather than EOF.
	rst bool
	// hang accepts the request and never answers it. Only the caller's own
	// deadline ends the call, which is the point: a stub that eventually
	// replied would not test the case at all.
	hang bool
}

// rule is one scripted deviation from a healthy host.
type rule struct {
	// match is a substring of the service string; "" matches every request.
	match string
	rep   reply
	// times caps how often the rule fires. Zero is unlimited.
	times int
	used  int
}

// wireStub is an adb host server that says exactly what a test tells it to.
//
// It exists rather than test/fakeadb because these cases need answers a real
// server gives and a faithful fake will not: a detach the server does not
// implement, a get-state that is severed on every single poll, a host that
// accepts a connection and then says nothing for the whole action budget.
type wireStub struct {
	ln net.Listener

	mu     sync.Mutex
	rules  []*rule
	states []string
	nstate int
	seen   []string

	done chan struct{}
	wg   sync.WaitGroup
}

// startStub brings up a host that answers everything the way a healthy rack
// does, then applies the case's deviations on top.
func startStub(tb testing.TB, states []string, rules []rule) *wireStub {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listening: %v", err)
	}
	if len(states) == 0 {
		states = []string{string(adbwire.StateDevice)}
	}
	s := &wireStub{ln: ln, states: states, done: make(chan struct{})}
	for i := range rules {
		r := rules[i]
		s.rules = append(s.rules, &r)
	}
	s.wg.Add(1)
	go s.accept()
	tb.Cleanup(s.close)
	return s
}

func (s *wireStub) addr() string { return s.ln.Addr().String() }

func (s *wireStub) close() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	_ = s.ln.Close()
	s.wg.Wait()
}

func (s *wireStub) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.serve(c)
	}
}

func (s *wireStub) serve(c net.Conn) {
	defer s.wg.Done()
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		svc, err := readStubFrame(br)
		if err != nil {
			return
		}
		rep := s.dispatch(svc)
		switch {
		case rep.hang:
			<-s.done
			return
		case rep.rst:
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			return
		case rep.fail != "":
			_ = writeStub(c, append([]byte("FAIL"), stubFrame(rep.fail)...))
			return
		case rep.bare:
			// The socket stays open: a transport switch is followed by the
			// device-side service string on the same connection.
			if err := writeStub(c, []byte("OKAY")); err != nil {
				return
			}
		default:
			_ = writeStub(c, append([]byte("OKAY"), stubFrame(rep.payload)...))
			return
		}
	}
}

// dispatch records the request and picks the answer: a scripted rule if one
// matches, otherwise whatever a healthy host would say.
func (s *wireStub) dispatch(svc string) reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, svc)

	for _, r := range s.rules {
		if r.match != "" && !strings.Contains(svc, r.match) {
			continue
		}
		if r.times > 0 && r.used >= r.times {
			continue
		}
		r.used++
		return r.rep
	}

	switch {
	case strings.HasSuffix(svc, ":get-state"):
		// The states slice is consumed one answer at a time and the last entry
		// repeats, so "offline then device" is two words in a table row.
		st := s.states[s.nstate]
		if s.nstate < len(s.states)-1 {
			s.nstate++
		}
		return reply{payload: st}
	case svc == "host:kill", strings.HasPrefix(svc, "host:transport:"), svc == "reboot:":
		return reply{bare: true}
	default:
		return reply{payload: "ok"}
	}
}

// requests returns every service string the stub was asked for.
func (s *wireStub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// stateReads counts the confirming reads. It is the number this file checks
// before believing any claim of recovery.
func (s *wireStub) stateReads() int {
	n := 0
	for _, svc := range s.requests() {
		if strings.HasSuffix(svc, ":get-state") {
			n++
		}
	}
	return n
}

func readStubFrame(r io.Reader) (string, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", err
	}
	// The protocol's lengths are four ASCII hex digits, not binary, in both
	// directions.
	want, err := strconv.ParseUint(string(hdr[:]), 16, 32)
	if err != nil {
		return "", fmt.Errorf("stub: length prefix %q is not 4 hex digits", hdr)
	}
	buf := make([]byte, want)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func stubFrame(payload string) []byte {
	return append([]byte(fmt.Sprintf("%04x", len(payload))), payload...)
}

func writeStub(c net.Conn, b []byte) error {
	for len(b) > 0 {
		n, err := c.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// deadEndpoint returns an address nothing is listening on, which is how a host
// whose adb server has gone away looks from here.
func deadEndpoint(tb testing.TB) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listening: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// ---------------------------------------------------------------------------
// Fake host agents
// ---------------------------------------------------------------------------

// fakeRunner is a farmd-node agent that answers however the case says.
type fakeRunner struct {
	err error

	mu    sync.Mutex
	calls []string
}

func (f *fakeRunner) USBReset(_ context.Context, hostID, devpath string) error {
	return f.record("USBReset", hostID, devpath)
}

func (f *fakeRunner) PortPower(_ context.Context, hostID, devpath string) error {
	return f.record("PortPower", hostID, devpath)
}

func (f *fakeRunner) record(op, hostID, devpath string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf("%s(%s,%s)", op, hostID, devpath))
	f.mu.Unlock()
	return f.err
}

func (f *fakeRunner) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// faultRunner answers through the RungFault interface instead of the
// sentinels, which is the other contract a host agent may satisfy.
type faultRunner struct{ err rungFaultErr }

func (f faultRunner) USBReset(context.Context, string, string) error  { return f.err }
func (f faultRunner) PortPower(context.Context, string, string) error { return f.err }

type rungFaultErr struct {
	msg         string
	refused     bool
	unreachable bool
}

func (e rungFaultErr) Error() string         { return e.msg }
func (e rungFaultErr) RungRefused() bool     { return e.refused }
func (e rungFaultErr) HostUnreachable() bool { return e.unreachable }

var _ RungFault = rungFaultErr{}

// ---------------------------------------------------------------------------
// The table
// ---------------------------------------------------------------------------

type outcomeCase struct {
	name     string
	tier     int
	tierName string

	// states is what get-state answers, one per poll, the last repeating.
	states []string
	// rules are this case's deviations from a healthy host.
	rules []rule
	// deadHost points the actuator at an address nothing answers.
	deadHost bool
	// noEndpoint leaves farm.hosts.adb_endpoint empty.
	noEndpoint bool
	// devpath overrides farm.slots.adb_devpath, for the cases where the
	// position itself is what the ladder cannot use.
	devpath string

	// runner is the host agent. nil means this farm has none, which is the
	// state tiers 3 and 4 must refuse rather than fake.
	runner HostRunner

	// timeout overrides the action budget, for the cases that must exhaust it.
	timeout time.Duration

	want Disposition
	// reasonHas are substrings the recorded refusal must name, so an operator
	// reading farm.recovery_attempts.refusal learns what is missing.
	reasonHas []string
	// wantAgentCalls asserts whether the host agent was actually invoked.
	wantAgentCalls int
}

func outcomeCases(tb testing.TB) []outcomeCase {
	tb.Helper()

	notFound := fmt.Sprintf("device '%s' not found", testDevpath)
	okRunner := func() *fakeRunner { return &fakeRunner{} }

	return []outcomeCase{
		// ---- tier 1: adb reconnect ------------------------------------
		{
			name: "tier1/recovered after a confirming state read",
			tier: 1, tierName: "adb_reconnect",
			states: []string{"offline", "device"},
			want:   DispositionRecovered,
		},
		{
			name: "tier1/no_change when the position is readable and still offline",
			tier: 1, tierName: "adb_reconnect",
			states: []string{"offline"},
			want:   DispositionNoChange,
		},
		{
			name: "tier1/refused when the server does not implement the verb",
			tier: 1, tierName: "adb_reconnect",
			rules:     []rule{{match: ":reconnect", rep: reply{fail: "unknown host service"}}},
			want:      DispositionRefused,
			reasonHas: []string{"reconnect", testHost, "unknown host service"},
		},
		{
			name: "tier1/refused when the devpath matches more than one transport",
			tier: 1, tierName: "adb_reconnect",
			rules:     []rule{{match: ":reconnect", rep: reply{fail: "more than one device"}}},
			want:      DispositionRefused,
			reasonHas: []string{testDevpath, "usb topology"},
		},
		{
			name: "tier1/failed when the position has no transport at all",
			tier: 1, tierName: "adb_reconnect",
			rules: []rule{{match: ":reconnect", rep: reply{fail: notFound}}},
			want:  DispositionFailed,
		},
		{
			name: "tier1/failed when the server refuses because the device is offline",
			tier: 1, tierName: "adb_reconnect",
			rules: []rule{{match: ":reconnect", rep: reply{fail: "device offline"}}},
			want:  DispositionFailed,
		},
		{
			name: "tier1/unreachable when the adb server is gone",
			tier: 1, tierName: "adb_reconnect",
			deadHost:  true,
			want:      DispositionUnreachable,
			reasonHas: []string{testHost, "no rung on this host will help"},
		},
		{
			name: "tier1/unreachable when the socket is severed mid-request",
			tier: 1, tierName: "adb_reconnect",
			rules:     []rule{{match: ":reconnect", rep: reply{rst: true}}},
			want:      DispositionUnreachable,
			reasonHas: []string{testHost},
		},
		{
			name: "tier1/refused when the host has no adb endpoint recorded",
			tier: 1, tierName: "adb_reconnect",
			noEndpoint: true,
			want:       DispositionRefused,
			reasonHas:  []string{"adb_endpoint", testHost},
		},

		// ---- tier 2: detach then attach --------------------------------
		{
			name: "tier2/recovered only after the attach is confirmed",
			tier: 2, tierName: "transport_reset",
			states: []string{"device"},
			want:   DispositionRecovered,
		},
		{
			name: "tier2/no_change when the attach leaves the device unusable",
			tier: 2, tierName: "transport_reset",
			states: []string{"unauthorized"},
			want:   DispositionNoChange,
		},
		{
			name: "tier2/refused when the server does not implement detach",
			tier: 2, tierName: "transport_reset",
			rules:     []rule{{match: ":detach", rep: reply{fail: "unknown host service"}}},
			want:      DispositionRefused,
			reasonHas: []string{"detach", "unknown host service"},
		},
		{
			name: "tier2/failed when the attach finds no transport",
			tier: 2, tierName: "transport_reset",
			rules: []rule{{match: ":attach", rep: reply{fail: notFound}}},
			want:  DispositionFailed,
		},
		{
			name: "tier2/unreachable when the adb server is gone",
			tier: 2, tierName: "transport_reset",
			deadHost:  true,
			want:      DispositionUnreachable,
			reasonHas: []string{testHost},
		},
		{
			// The detach landed and the attach did not, so the position is now
			// detached — worse than the actuator found it. A refusal that still
			// recited "the device is as it was" would send an operator hunting
			// a hardware fault that is really an unfinished rung.
			name: "tier2/refused after a detach that landed says the position was left detached",
			tier: 2, tierName: "transport_reset",
			rules:     []rule{{match: ":attach", rep: reply{fail: "unknown host service"}}},
			want:      DispositionRefused,
			reasonHas: []string{"attach", "unknown host service", "left detached", testDevpath},
		},

		// ---- tier 3: USBDEVFS_RESET ------------------------------------
		{
			name: "tier3/refused when the farm has no host agent",
			tier: 3, tierName: "usb_reset",
			want:      DispositionRefused,
			reasonHas: []string{"USBDEVFS_RESET", "farmd-node", testHost},
		},
		{
			name: "tier3/refused when the agent declines the rung",
			tier: 3, tierName: "usb_reset",
			runner: &fakeRunner{err: fmt.Errorf("uhubctl is not installed: %w", ErrRungRefused)},
			want:   DispositionRefused,
			reasonHas: []string{"farmd-node", testHost,
				"uhubctl is not installed", "device is as it was"},
			wantAgentCalls: 1,
		},
		{
			name: "tier3/unreachable when the agent cannot be contacted",
			tier: 3, tierName: "usb_reset",
			runner:         &fakeRunner{err: fmt.Errorf("dial tcp: refused: %w", ErrHostUnreachable)},
			want:           DispositionUnreachable,
			reasonHas:      []string{"farmd-node", "nothing was learned about the device"},
			wantAgentCalls: 1,
		},
		{
			name: "tier3/unreachable when the agent answers through RungFault",
			tier: 3, tierName: "usb_reset",
			runner:    faultRunner{err: rungFaultErr{msg: "502 from a proxy", unreachable: true}},
			want:      DispositionUnreachable,
			reasonHas: []string{"502 from a proxy"},
		},
		{
			name: "tier3/refused when the agent answers RungRefused through RungFault",
			tier: 3, tierName: "usb_reset",
			runner:    faultRunner{err: rungFaultErr{msg: "kernel has no USBDEVFS_RESET", refused: true}},
			want:      DispositionRefused,
			reasonHas: []string{"kernel has no USBDEVFS_RESET"},
		},
		{
			name: "tier3/failed when the reset ran and errored on the hardware",
			tier: 3, tierName: "usb_reset",
			runner:         &fakeRunner{err: errors.New("ioctl USBDEVFS_RESET: input/output error")},
			want:           DispositionFailed,
			wantAgentCalls: 1,
		},
		{
			name: "tier3/no_change when the reset ran and the device stayed offline",
			tier: 3, tierName: "usb_reset",
			runner: okRunner(), states: []string{"offline"},
			timeout:        150 * time.Millisecond,
			want:           DispositionNoChange,
			wantAgentCalls: 1,
		},
		{
			name: "tier3/recovered when the reset ran and the device came back",
			tier: 3, tierName: "usb_reset",
			runner: okRunner(), states: []string{"offline", "offline", "device"},
			want:           DispositionRecovered,
			wantAgentCalls: 1,
		},
		{
			name: "tier3/unreachable when the reset ran and the adb server cannot confirm it",
			tier: 3, tierName: "usb_reset",
			runner: okRunner(), deadHost: true,
			want:           DispositionUnreachable,
			reasonHas:      []string{testHost},
			wantAgentCalls: 1,
		},
		{
			name: "tier3/unreachable when the adb server accepts and never answers",
			tier: 3, tierName: "usb_reset",
			runner:         okRunner(),
			rules:          []rule{{match: ":get-state", rep: reply{hang: true}}},
			timeout:        200 * time.Millisecond,
			want:           DispositionUnreachable,
			reasonHas:      []string{"never answered a state read"},
			wantAgentCalls: 1,
		},
		{
			name: "tier3/refused when the rung ran but the host has no adb endpoint",
			tier: 3, tierName: "usb_reset",
			runner: okRunner(), noEndpoint: true,
			want: DispositionRefused,
			reasonHas: []string{"adb_endpoint", "did perform",
				"USBDEVFS_RESET", "the device was touched"},
			wantAgentCalls: 1,
		},
		{
			// A devpath the wire will not address is a fact about
			// farm.slots.adb_devpath, and it fails locally on every single
			// poll without a byte reaching the host. Confirming a rung against
			// it would spend the whole action budget and then report the host
			// as the thing that never answered — and the agent would have been
			// asked to reset a position nothing can afterwards check.
			name: "tier3/refused before the agent is called when the devpath cannot be addressed",
			tier: 3, tierName: "usb_reset",
			runner:  okRunner(),
			devpath: "usb:3-1.4 ; host:kill",
			want:    DispositionRefused,
			reasonHas: []string{"usb:3-1.4 ; host:kill", "farm.slots.adb_devpath",
				testHost},
			wantAgentCalls: 0,
		},

		// ---- tier 4: VBUS power cycle ----------------------------------
		{
			name: "tier4/refused when the farm has no host agent",
			tier: 4, tierName: "port_power",
			want:      DispositionRefused,
			reasonHas: []string{"VBUS power cycle", "farmd-node", testHost},
		},
		{
			name: "tier4/failed when the power cycle ran and errored",
			tier: 4, tierName: "port_power",
			runner:         &fakeRunner{err: errors.New("uhubctl: port 4 did not change state")},
			want:           DispositionFailed,
			wantAgentCalls: 1,
		},
		{
			name: "tier4/unreachable when the agent cannot be contacted",
			tier: 4, tierName: "port_power",
			runner:         &fakeRunner{err: fmt.Errorf("no route to host: %w", ErrHostUnreachable)},
			want:           DispositionUnreachable,
			reasonHas:      []string{"farmd-node", testHost},
			wantAgentCalls: 1,
		},
		{
			name: "tier4/recovered when the port came back",
			tier: 4, tierName: "port_power",
			runner: okRunner(), states: []string{"offline", "device"},
			want:           DispositionRecovered,
			wantAgentCalls: 1,
		},

		// ---- tier 5: device reboot -------------------------------------
		{
			name: "tier5/recovered after the device left and returned",
			tier: 5, tierName: "device_reboot",
			states: []string{"offline", "offline", "device"},
			want:   DispositionRecovered,
		},
		{
			name: "tier5/recovered when the reboot socket dies on cue",
			tier: 5, tierName: "device_reboot",
			rules:  []rule{{match: "reboot:", rep: reply{rst: true}}},
			states: []string{"offline", "device"},
			want:   DispositionRecovered,
		},
		{
			name: "tier5/no_change when the device never came back",
			tier: 5, tierName: "device_reboot",
			states:  []string{"offline"},
			timeout: 150 * time.Millisecond,
			want:    DispositionNoChange,
		},
		{
			name: "tier5/refused when the server will not open a transport at all",
			tier: 5, tierName: "device_reboot",
			rules:     []rule{{match: "host:transport:", rep: reply{fail: "closed"}}},
			want:      DispositionRefused,
			reasonHas: []string{"reboot:", testHost, "closed"},
		},
		{
			name: "tier5/failed when the device is too offline to be rebooted",
			tier: 5, tierName: "device_reboot",
			rules: []rule{{match: "host:transport:", rep: reply{fail: "device offline"}}},
			want:  DispositionFailed,
		},
		{
			name: "tier5/unreachable when the adb server is gone",
			tier: 5, tierName: "device_reboot",
			deadHost:  true,
			want:      DispositionUnreachable,
			reasonHas: []string{testHost, "no rung on this host will help"},
		},
		{
			// The reboot has two phases and only the second one may fail
			// "on cue". A socket severed during the transport switch means the
			// server never saw the word reboot, so there is no reboot to wait
			// for — and the position answering "device" afterwards is the
			// device that was there all along, not one this rung brought back.
			name: "tier5/unreachable when the socket dies before the reboot is delivered",
			tier: 5, tierName: "device_reboot",
			rules:     []rule{{match: "host:transport:", rep: reply{rst: true}}},
			states:    []string{"device"},
			want:      DispositionUnreachable,
			reasonHas: []string{testHost, "no rung on this host will help"},
		},

		// ---- tier 6: not an actuator rung ------------------------------
		{
			name: "tier6/refused because quarantine belongs to the ladder",
			tier: 6, tierName: "quarantine",
			want:      DispositionRefused,
			reasonHas: []string{"not an actuator rung"},
		},

		// ---- tier 7: restart the host adb server -----------------------
		{
			name: "tier7/recovered when the server came back with the device",
			tier: 7, tierName: "adb_restart",
			states: []string{"device"},
			want:   DispositionRecovered,
		},
		{
			name: "tier7/no_change when the server came back and the device did not",
			tier: 7, tierName: "adb_restart",
			states:  []string{"offline"},
			timeout: 150 * time.Millisecond,
			want:    DispositionNoChange,
		},
		{
			name: "tier7/refused when the server will not accept host:kill",
			tier: 7, tierName: "adb_restart",
			rules:     []rule{{match: "host:kill", rep: reply{fail: "operation not permitted"}}},
			want:      DispositionRefused,
			reasonHas: []string{"host:kill", "operation not permitted"},
		},
		{
			name: "tier7/unreachable when the server never comes back",
			tier: 7, tierName: "adb_restart",
			rules:     []rule{{match: ":get-state", rep: reply{rst: true}}},
			timeout:   200 * time.Millisecond,
			want:      DispositionUnreachable,
			reasonHas: []string{testHost},
		},
		{
			name: "tier7/unreachable when the server was already gone",
			tier: 7, tierName: "adb_restart",
			deadHost:  true,
			want:      DispositionUnreachable,
			reasonHas: []string{testHost},
		},
	}
}

// TestActuatorDispositions is the table. Every row asserts the disposition, the
// database columns it renders to, and — for the two answers that stop the
// ladder climbing — that the recorded reason names what is missing.
func TestActuatorDispositions(t *testing.T) {
	t.Parallel()

	for _, tc := range outcomeCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, stub := runCase(t, tc)

			got := DispositionOf(res)
			if got != tc.want {
				t.Fatalf("disposition = %q, want %q (detail %v)", got, tc.want, res.Detail)
			}
			if res.Outcome != tc.want.Outcome() {
				t.Errorf("Result.Outcome = %q, want %q", res.Outcome, tc.want.Outcome())
			}

			// The two columns farm.recovery_attempts keeps for this row.
			out, refusal := Record(res)
			if out != tc.want.Outcome() {
				t.Errorf("Record outcome = %q, want %q", out, tc.want.Outcome())
			}
			switch tc.want {
			case DispositionRefused, DispositionUnreachable:
				if refusal == "" {
					t.Fatal("farm.recovery_attempts.refusal would be NULL for a rung that " +
						"never ran; that column is where an operator learns why the ladder stopped")
				}
				if refusal != RefusalOf(res) {
					t.Errorf("Record refusal = %q but RefusalOf = %q", refusal, RefusalOf(res))
				}
			default:
				if refusal != "" {
					t.Errorf("a %s rung recorded a refusal it does not have: %q", tc.want, refusal)
				}
			}
			for _, want := range tc.reasonHas {
				if !strings.Contains(refusal, want) {
					t.Errorf("refusal does not name %q:\n  %s", want, refusal)
				}
			}

			// Recovery is claimed only on the evidence of a state read.
			if got == DispositionRecovered {
				if n := stub.stateReads(); n == 0 {
					t.Fatal("claimed recovery without a single confirming state read")
				}
				if st, _ := res.Detail[DetailConfirmedState].(string); st != string(adbwire.StateDevice) {
					t.Fatalf("recovered with confirmed_state %q, want %q",
						st, adbwire.StateDevice)
				}
			}

			// A host agent is called exactly when the rung was permitted.
			if f, ok := tc.runner.(*fakeRunner); ok {
				if n := len(f.seen()); n != tc.wantAgentCalls {
					t.Errorf("host agent calls = %d, want %d (%v)", n, tc.wantAgentCalls, f.seen())
				}
			}
		})
	}
}

// runCase performs one row and returns the Result and the stub that served it.
func runCase(t *testing.T, tc outcomeCase) (Result, *wireStub) {
	t.Helper()

	stub := startStub(t, tc.states, tc.rules)
	endpoint := stub.addr()
	switch {
	case tc.noEndpoint:
		endpoint = ""
	case tc.deadHost:
		endpoint = deadEndpoint(t)
	}

	timeout := tc.timeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	devpath := tc.devpath
	if devpath == "" {
		devpath = testDevpath
	}

	a := NewADBActuator(slog.New(slog.DiscardHandler), tc.runner)
	// The real intervals are seconds; the behaviour under test is which answer
	// comes out, not how long it waits to say it. The confirmation window stays
	// two orders of magnitude above the poll interval so that a slow scheduler
	// costs a case some reads rather than all of them — a window that fits no
	// read at all is its own test, in TestOurOwnTickRateIsNotAHostOutage.
	a.SettleInterval = 2 * time.Millisecond
	a.ControlConfirm = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := a.Recover(ctx, Action{
		Tier: tc.tier, TierName: tc.tierName,
		DeviceID: "11111111-1111-1111-1111-111111111111", SlotID: 7,
		Devpath: devpath, RackSlot: testRackSlot,
		HubID: 3, HubPath: "3-1", HostID: testHost,
		ADBEndpoint: endpoint, Timeout: timeout,
	})
	if err != nil {
		t.Fatalf("Recover returned an error instead of a classified result: %v", err)
	}
	if res.Detail == nil {
		t.Fatal("Result carries no detail, so nothing reaches farm.recovery_attempts.detail")
	}
	if _, ok := res.Detail[DetailDisposition]; !ok {
		t.Fatalf("Result.Detail has no %q key: the three states cannot be told apart in the row",
			DetailDisposition)
	}
	return res, stub
}

// TestEveryRungCanSayAllThreeThings is the coverage assertion the table exists
// to satisfy: for each rung the actuator owns, all three answers are reachable.
//
// "failed" here is the escalating class — failed or no_change — because those
// are the two the ladder answers by climbing, and the distinction this file
// defends is between climbing and not climbing. Tier 7 has no way to produce a
// bare "failed": killing the adb server either works, is refused, or leaves a
// host that does not answer.
func TestEveryRungCanSayAllThreeThings(t *testing.T) {
	t.Parallel()

	type coverage struct{ refused, escalate, unreachable bool }
	seen := map[int]*coverage{}
	for _, tc := range outcomeCases(t) {
		c := seen[tc.tier]
		if c == nil {
			c = &coverage{}
			seen[tc.tier] = c
		}
		switch {
		case tc.want == DispositionRefused:
			c.refused = true
		case tc.want == DispositionUnreachable:
			c.unreachable = true
		case tc.want.Escalate():
			c.escalate = true
		}
	}

	for _, tier := range []int{1, 2, 3, 4, 5, 7} {
		c := seen[tier]
		if c == nil {
			t.Errorf("tier %d has no cases at all", tier)
			continue
		}
		if !c.refused {
			t.Errorf("tier %d has no refused case: a rung that cannot refuse will fake one", tier)
		}
		if !c.escalate {
			t.Errorf("tier %d has no escalating case", tier)
		}
		if !c.unreachable {
			t.Errorf("tier %d has no unreachable case: an unreachable host on this rung "+
				"would be recorded as a broken handset", tier)
		}
	}

	// Refused and unreachable must never send the ladder up a rung; failed and
	// no_change must.
	for d, want := range map[Disposition]bool{
		DispositionRefused:     false,
		DispositionUnreachable: false,
		DispositionAborted:     false,
		DispositionRecovered:   false,
		DispositionFailed:      true,
		DispositionNoChange:    true,
	} {
		if d.Escalate() != want {
			t.Errorf("%s.Escalate() = %t, want %t", d, d.Escalate(), want)
		}
	}
}

// TestRecoveredNeedsAConfirmingStateRead drives every rung to a clean success at
// the verb and then denies it the confirmation, three different ways.
//
// None of them may report a recovery. A false "recovered" is worse than a false
// "failed": it closes the incident, suppresses the page, and hands the next
// job a device that is still broken.
func TestRecoveredNeedsAConfirmingStateRead(t *testing.T) {
	t.Parallel()

	denials := []struct {
		name  string
		rules []rule
		state string
	}{
		{name: "the position answers offline", state: "offline"},
		{name: "the position answers unauthorized", state: "unauthorized"},
		{
			name:  "the position has no transport",
			state: "device",
			rules: []rule{{match: ":get-state", rep: reply{fail: fmt.Sprintf("device '%s' not found", testDevpath)}}},
		},
		{
			name:  "the state read is severed every time",
			state: "device",
			rules: []rule{{match: ":get-state", rep: reply{rst: true}}},
		},
	}

	for _, tier := range []struct {
		n      int
		name   string
		runner HostRunner
	}{
		{1, "adb_reconnect", nil},
		{2, "transport_reset", nil},
		{3, "usb_reset", &fakeRunner{}},
		{4, "port_power", &fakeRunner{}},
		{5, "device_reboot", nil},
		{7, "adb_restart", nil},
	} {
		for _, d := range denials {
			t.Run(fmt.Sprintf("tier%d/%s", tier.n, d.name), func(t *testing.T) {
				t.Parallel()
				res, stub := runCase(t, outcomeCase{
					tier: tier.n, tierName: tier.name,
					states: []string{d.state}, rules: d.rules,
					runner:  tier.runner,
					timeout: 200 * time.Millisecond,
					want:    DispositionNoChange, // unused here
				})
				if got := DispositionOf(res); got == DispositionRecovered {
					t.Fatalf("claimed recovery with no confirming read (%d state reads, detail %v)",
						stub.stateReads(), res.Detail)
				}
			})
		}
	}
}

// TestOurOwnTickRateIsNotAHostOutage pins the cheapest way to manufacture the
// answer this file exists to avoid.
//
// "The adb server never answered a state read" is a claim about a host, and the
// ladder answers it by stopping. But the confirmation window and the polling
// interval are both local configuration, and a window shorter than one interval
// closes before a single read is even attempted. Reporting that as a silent
// host would take a farm whose intervals were retuned by one line of config and
// tell its operator every host had gone away.
func TestOurOwnTickRateIsNotAHostOutage(t *testing.T) {
	t.Parallel()

	stub := startStub(t, []string{"device"}, nil)

	a := NewADBActuator(slog.New(slog.DiscardHandler), nil)
	// A poll interval an order of magnitude longer than the window it has to
	// poll inside. The device underneath is healthy and says so on the first
	// read anyone bothers to take.
	a.SettleInterval = 2 * time.Second
	a.ControlConfirm = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := a.Recover(ctx, Action{
		Tier: 1, TierName: "adb_reconnect",
		DeviceID: "11111111-1111-1111-1111-111111111111", SlotID: 7,
		Devpath: testDevpath, RackSlot: testRackSlot,
		HubID: 3, HubPath: "3-1", HostID: testHost,
		ADBEndpoint: stub.addr(), Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := stub.stateReads(); n == 0 {
		t.Errorf("the confirmation window took no state reads at all, so the rung " +
			"reported on a device it never looked at")
	}
	if got := DispositionOf(res); got == DispositionUnreachable {
		t.Fatalf("a healthy host was reported %q because the poll interval (%s) is longer "+
			"than the window it polls in (%s); that is this loop's own clock recorded as an "+
			"outage, and every rung on the host stops on it (detail %v)",
			got, a.SettleInterval, a.ControlConfirm, res.Detail)
	}
	if got := DispositionOf(res); got != DispositionRecovered {
		t.Errorf("disposition = %q, want %q: the position answered %q on the first read",
			got, DispositionRecovered, adbwire.StateDevice)
	}
}

// TestTheLoopGoingAwayIsNotAHostVerdict covers the other end of the same
// mistake: not our clock, but our shutdown.
//
// When farmd is restarted, every read still in flight fails at once. The last
// of those errors is a real socket error, and reading it as the verdict writes
// "this host stopped answering, no rung on it will help" into the record of a
// rung that was interrupted rather than answered. The outcome that reaches
// farm.recovery_attempts matters twice over: 'aborted' counts toward the tier's
// cooldown and hourly budget, and 'refused' — which is what an unreachable host
// renders to — does not. So a tier 7 that DID kill the host's adb server and
// then lost its confirmation to a deploy would be recorded as costing nothing,
// and the next process is free to sever every device on that host again.
//
// This drives confirm directly. The ordering it defends is only reachable when
// the loop dies AFTER the verb landed and while a read is outstanding, and
// pinning that window from the outside takes a sleep race rather than a test.
func TestTheLoopGoingAwayIsNotAHostVerdict(t *testing.T) {
	t.Parallel()

	a := NewADBActuator(slog.New(slog.DiscardHandler), nil)
	a.SettleInterval = 2 * time.Millisecond

	// The ladder's context is already gone: this process is shutting down.
	parent, stop := context.WithCancel(context.Background())
	stop()
	// The action's own budget is untouched, so nothing but the shutdown ends
	// the wait.
	actx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	act := Action{
		Tier: 7, TierName: "adb_restart",
		Devpath: testDevpath, RackSlot: testRackSlot, HostID: testHost,
		ADBEndpoint: deadEndpoint(t), Timeout: 100 * time.Millisecond,
	}
	r := &rung{parent: parent, ctx: actx, act: act, log: slog.New(slog.DiscardHandler)}

	// tolerateServerDown, because tier 7 has just killed the server it is now
	// waiting on. Every probe fails with a real transport error.
	res := a.confirm(r, adbwire.New(act.ADBEndpoint), "adb_restart", 0, true,
		map[string]any{"adb_server_restarted": true})

	if got := DispositionOf(res); got != DispositionAborted {
		t.Fatalf("disposition = %q, want %q: this process shutting down was recorded as a "+
			"verdict about host %s (detail %v)", got, DispositionAborted, testHost, res.Detail)
	}
	if out, _ := Record(res); out != OutcomeAborted {
		t.Errorf("farm.recovery_attempts.outcome = %q, want %q; %q is excluded from the "+
			"tier's cooldown and hourly budget, so a rung that ran would cost nothing",
			out, OutcomeAborted, OutcomeRefused)
	}
	// Interrupted is not the same as unexamined: the error that was in flight
	// still belongs in the row, whatever this rung declined to conclude from it.
	if _, ok := res.Detail["error"].(string); !ok {
		t.Errorf("the socket error the shutdown interrupted was dropped from the detail: %v",
			res.Detail)
	}
	if n, _ := res.Detail["state_probes"].(int); n == 0 {
		t.Errorf("state_probes = 0, so the abort was decided without a single read: %v",
			res.Detail)
	}
}

// TestADetachThatLandedIsNotADeviceLeftAlone guards the claim a tier 2 refusal
// makes about the world.
//
// The rung is two verbs. Once the detach has landed, the position is no longer
// claimed by the adb server, and it stays that way until something attaches it
// again. A refusal reciting the template's "the device is as it was" would send
// an operator hunting a hardware fault that is really a half-finished rung.
func TestADetachThatLandedIsNotADeviceLeftAlone(t *testing.T) {
	t.Parallel()

	res, stub := runCase(t, outcomeCase{
		tier: 2, tierName: "transport_reset",
		rules: []rule{{match: ":attach", rep: reply{fail: "unknown host service"}}},
	})

	if got := DispositionOf(res); got != DispositionRefused {
		t.Fatalf("disposition = %q, want %q (detail %v)", got, DispositionRefused, res.Detail)
	}
	// The claim is only worth checking if the detach really went out.
	var detached bool
	for _, svc := range stub.requests() {
		if strings.HasSuffix(svc, ":detach") {
			detached = true
		}
	}
	if !detached {
		t.Fatal("the detach never reached the wire, so this case is not the one being guarded")
	}
	if left, _ := res.Detail["detached"].(bool); !left {
		t.Errorf("the row does not record that the position was left detached: %v", res.Detail)
	}

	_, refusal := Record(res)
	if strings.Contains(refusal, "device is as it was") {
		t.Errorf("the refusal claims the device was untouched after a detach that landed:\n  %s",
			refusal)
	}
	for _, want := range []string{"left detached", testDevpath, testHost} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal does not name %q:\n  %s", want, refusal)
		}
	}
}

// TestVerbReplyIsNotProof pins the specific lie: the adb server answering OKAY
// to a maintenance verb is the server accepting a request, not a device coming
// back, and a rung that stops there would report a recovery on every wedged
// handset whose server is still healthy.
func TestVerbReplyIsNotProof(t *testing.T) {
	t.Parallel()

	res, stub := runCase(t, outcomeCase{
		tier: 1, tierName: "adb_reconnect",
		states:  []string{"offline"},
		timeout: 200 * time.Millisecond,
	})
	if got := DispositionOf(res); got != DispositionNoChange {
		t.Fatalf("disposition = %q, want %q", got, DispositionNoChange)
	}
	if stub.stateReads() == 0 {
		t.Fatal("the rung never read the device's state back, so it had nothing to go on")
	}
	if st, _ := res.Detail[DetailConfirmedState].(string); st != "offline" {
		t.Errorf("confirmed_state = %q, want the state actually read back", st)
	}
}

// TestDetachIsNotConfirmed guards the pairing in tier 2. A detach that worked
// leaves the position unusable on purpose; confirming after it would read a
// working detach as a dead device and a failed detach as a recovery.
func TestDetachIsNotConfirmed(t *testing.T) {
	t.Parallel()

	res, stub := runCase(t, outcomeCase{
		tier: 2, tierName: "transport_reset",
		states: []string{"device"},
	})
	if got := DispositionOf(res); got != DispositionRecovered {
		t.Fatalf("disposition = %q, want %q (detail %v)", got, DispositionRecovered, res.Detail)
	}

	var order []string
	for _, svc := range stub.requests() {
		switch {
		case strings.HasSuffix(svc, ":detach"):
			order = append(order, "detach")
		case strings.HasSuffix(svc, ":attach"):
			order = append(order, "attach")
		case strings.HasSuffix(svc, ":get-state"):
			order = append(order, "get-state")
		}
	}
	if len(order) < 3 || order[0] != "detach" || order[1] != "attach" {
		t.Fatalf("tier 2 did not detach, then attach, then confirm: %v", order)
	}
	for _, step := range order[2:] {
		if step != "get-state" {
			t.Fatalf("a state read happened between the detach and the attach: %v", order)
		}
	}
}

// TestRefusedRungLeavesTheDeviceAlone checks the claim a refusal makes. A
// refused rung says the device is exactly as it was, so it must not have
// touched it: no verb, no transport, no agent call.
func TestRefusedRungLeavesTheDeviceAlone(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	for _, tc := range []outcomeCase{
		{name: "no host agent", tier: 3, tierName: "usb_reset"},
		{name: "not an actuator rung", tier: 6, tierName: "quarantine", runner: runner},
		{name: "no adb endpoint", tier: 1, tierName: "adb_reconnect", noEndpoint: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, stub := runCase(t, tc)
			if got := DispositionOf(res); got != DispositionRefused {
				t.Fatalf("disposition = %q, want %q", got, DispositionRefused)
			}
			if reqs := stub.requests(); len(reqs) != 0 {
				t.Errorf("a refused rung still spoke to the adb server: %v", reqs)
			}
			if calls := runner.seen(); len(calls) != 0 {
				t.Errorf("a refused rung still called the host agent: %v", calls)
			}
		})
	}
}

// TestOutcomesStayInsideTheCheckConstraint runs the whole table and asserts
// that every value the actuator produces is one farm.recovery_attempts.outcome
// will accept. A value outside it fails the INSERT, and a forensic record that
// cannot be written is a ladder that stops explaining itself.
func TestOutcomesStayInsideTheCheckConstraint(t *testing.T) {
	t.Parallel()

	allowed := map[Outcome]bool{
		OutcomeRecovered: true, OutcomeNoChange: true, OutcomeFailed: true,
		OutcomeRefused: true, OutcomeAborted: true,
	}
	for _, tc := range outcomeCases(t) {
		res, _ := runCase(t, tc)
		out, _ := Record(res)
		if !allowed[out] {
			t.Errorf("%s: outcome %q is not in the CHECK list", tc.name, out)
		}
	}
}

// TestRecordIsTheSeamTheLadderWritesThrough exercises Record against Results
// this actuator never produces, because that is exactly where it earns its
// keep: the ladder calls it on whatever an Actuator hands back, including the
// control-plane rungs it builds itself and any implementation that predates
// [Disposition] entirely.
//
// The table above cannot reach these — every path in adbactuator.go stamps a
// disposition and a reason — so without this the fallbacks are code nobody has
// ever run, guarding the two columns an operator reads at 3am.
func TestRecordIsTheSeamTheLadderWritesThrough(t *testing.T) {
	t.Parallel()

	inCheck := map[Outcome]bool{
		OutcomeRecovered: true, OutcomeNoChange: true, OutcomeFailed: true,
		OutcomeRefused: true, OutcomeAborted: true,
	}

	for _, tc := range []struct {
		name        string
		in          Result
		wantOutcome Outcome
		wantReason  bool
	}{{
		// What perform() builds for observe, quarantine and host_drain.
		name:        "a bare outcome with no detail at all",
		in:          Result{Outcome: OutcomeNoChange},
		wantOutcome: OutcomeNoChange,
	}, {
		name:        "a refusal an actuator gave no reason for",
		in:          Result{Outcome: OutcomeRefused},
		wantOutcome: OutcomeRefused,
		wantReason:  true,
	}, {
		name: "an unreachable host an actuator gave no reason for",
		in: Result{Outcome: OutcomeRefused, Detail: map[string]any{
			DetailDisposition: string(DispositionUnreachable)}},
		wantOutcome: OutcomeRefused,
		wantReason:  true,
	}, {
		name: "a disposition this build has never heard of",
		in: Result{Outcome: OutcomeRecovered, Detail: map[string]any{
			DetailDisposition: "half_recovered"}},
		// Never the caller's optimistic Outcome: an unknown claim is not a
		// proven recovery, and a false recovered closes an incident nobody
		// fixed.
		wantOutcome: OutcomeFailed,
	}, {
		name: "a disposition recorded as something other than a string",
		in: Result{Outcome: OutcomeNoChange, Detail: map[string]any{
			DetailDisposition: 7}},
		wantOutcome: OutcomeNoChange,
	}, {
		name: "a recovery that also carries leftover refusal text",
		in: Result{Outcome: OutcomeRecovered, Detail: map[string]any{
			DetailDisposition: string(DispositionRecovered),
			DetailRefusal:     "left over from an earlier rung"}},
		wantOutcome: OutcomeRecovered,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			out, refusal := Record(tc.in)
			if out != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", out, tc.wantOutcome)
			}
			if !inCheck[out] {
				t.Errorf("outcome %q is not in the CHECK list, so the INSERT fails and the "+
					"attempt goes unrecorded", out)
			}
			switch {
			case tc.wantReason && refusal == "":
				t.Errorf("farm.recovery_attempts.refusal would be NULL: the ladder stopped " +
					"and the row does not say why")
			case !tc.wantReason && refusal != "":
				t.Errorf("a %q row carries refusal text it has no business having: %q",
					out, refusal)
			}
		})
	}

	// Every disposition, including ones no rung produces today, must render to
	// a value the column accepts. A new one that did not would fail the INSERT
	// and lose the attempt entirely.
	for _, d := range []Disposition{
		DispositionRecovered, DispositionNoChange, DispositionFailed,
		DispositionRefused, DispositionUnreachable, DispositionAborted,
		Disposition(""), Disposition("something_later"),
	} {
		if !inCheck[d.Outcome()] {
			t.Errorf("%q renders to outcome %q, which is not in the CHECK list",
				d, d.Outcome())
		}
	}
}

// TestNothingHereCanEndALease is the barrier.
//
// A lease ends when the job says so, when a deadline a human wrote down
// elapses, or when a human takes it back. Recovery is none of those. This
// actuator runs while a lease is live and its holder is mid-run, so the only
// safe design is one where ending a lease is not reachable from here at all —
// not by calling into the package that owns leases, and not by reaching a
// database it could write one from.
//
// Two checks, because the barrier has two halves: what the file may import, and
// what its code may name. Comments are excluded from the second: stating the
// barrier requires naming what is barred.
func TestNothingHereCanEndALease(t *testing.T) {
	t.Parallel()

	const file = "adbactuator.go"

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	// An allowlist rather than a denylist: a new import that could reach a
	// lease has to be added here deliberately, in a test that says why.
	allowed := map[string]bool{
		`"context"`:  true,
		`"errors"`:   true,
		`"fmt"`:      true,
		`"log/slog"`: true,
		`"strings"`:  true,
		`"sync"`:     true,
		`"time"`:     true,
		`"github.com/flaviopadilha/device-farmer/internal/adbwire"`: true,
	}
	if len(parsed.Imports) == 0 {
		t.Fatal("the import scan read nothing; it is asserting nothing")
	}
	for _, imp := range parsed.Imports {
		if !allowed[imp.Path.Value] {
			t.Errorf("%s imports %s, which is not on the actuator's allowlist; "+
				"a recovery action must not be able to reach a lease, a reaper or a "+
				"database, even transitively", file, imp.Path.Value)
		}
	}

	// Reparsed without comments, then printed: what remains is code only.
	code := token.NewFileSet()
	body, err := parser.ParseFile(code, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, code, body); err != nil {
		t.Fatalf("printing %s: %v", file, err)
	}
	// No word boundaries: the call that ended a lease would be spelled
	// endTheLease or fenceToken far sooner than it would be spelled "lease",
	// and a check that only caught the standalone noun would miss every
	// realistic breach.
	forbidden := regexp.MustCompile(
		`(?i)(lease|release|revoke|reclaim|evict|preempt|expire|fence|holder)`)
	// A scan that cannot fail is worse than no scan: it reports a barrier that
	// is not there. Prove the pattern still bites, and that there is code to
	// point it at.
	for _, breach := range []string{"func endTheLease(id string) {}", "l.fenceToken = 0"} {
		if !forbidden.MatchString(breach) {
			t.Fatalf("the vocabulary pattern no longer matches %q", breach)
		}
	}
	if buf.Len() < 1000 {
		t.Fatalf("the code scan read only %d bytes; it is asserting nothing", buf.Len())
	}
	for i, line := range strings.Split(buf.String(), "\n") {
		if m := forbidden.FindString(line); m != "" {
			t.Errorf("%s code line %d names %q; nothing in this file may act on a lease: %s",
				file, i+1, m, strings.TrimSpace(line))
		}
	}
}

// TestActuatorSpeaksOnlyMaintenanceVerbs is the other half of the barrier,
// observed rather than parsed: across the whole table, every service string the
// actuator put on the wire is a maintenance verb or a state read.
func TestActuatorSpeaksOnlyMaintenanceVerbs(t *testing.T) {
	t.Parallel()

	permitted := regexp.MustCompile(`^(` +
		`host:kill|` +
		`host:transport:usb:[0-9]+-[0-9.]+|` +
		`reboot:|` +
		`host-usb:usb:[0-9]+-[0-9.]+:(get-state|reconnect|detach|attach)` +
		`)$`)

	total := 0
	for _, tc := range outcomeCases(t) {
		_, stub := runCase(t, tc)
		for _, svc := range stub.requests() {
			total++
			if !permitted.MatchString(svc) {
				t.Errorf("%s: the actuator sent %q, which is not a maintenance verb", tc.name, svc)
			}
		}
	}
	if total == 0 {
		t.Fatal("the wire scan observed no requests; it is asserting nothing")
	}
}
