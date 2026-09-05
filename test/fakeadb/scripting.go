package fakeadb

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------

// Fixture is a scripted starting state. Fixtures are applied in order, either
// at Start/New or later via Apply, and may install goroutines that stop when
// the server closes.
type Fixture func(*Server)

// Apply runs fixtures against a running server.
func (s *Server) Apply(fixtures ...Fixture) {
	for _, f := range fixtures {
		if f != nil {
			f(s)
		}
	}
}

// WithDevices is the trivial fixture: install exactly these devices.
func WithDevices(devs ...Device) Fixture {
	return func(s *Server) {
		for _, d := range devs {
			s.Add(d)
		}
	}
}

// The duplicate-serial trap. STF's own README documents a handset shipping the
// serial "0123456789ABCDEF", and OEMs ship batches of them. Two such devices in
// one rack are two distinct physical positions wearing one name.
const (
	CloneSerial   = "0123456789ABCDEF"
	CloneDevpathA = "usb:3-1.4.1"
	CloneDevpathB = "usb:3-1.4.2"
)

// TwoClonesFixture installs two distinct devices that share the serial
// CloneSerial on different devpaths.
//
// What a test must be able to prove against it:
//
//   - a request addressed by devpath ("host-usb:usb:3-1.4.2:get-state") reaches
//     exactly the intended device, verifiable with RequestsTo;
//   - a request addressed by serial ("host-serial:0123456789ABCDEF:...") is
//     ambiguous and comes back FAIL MsgAmbiguousTarget.
//
// The second half is why farm.slots.adb_devpath is generated and why recovery
// actions are addressed by position: a serial-addressed reset in a rack holding
// clones lands on whichever transport adb happens to hand back, which may be a
// healthy device three hours into a six-hour lease.
//
// The model carries a space on purpose, so the long-listing sanitiser
// ("Pixel 6a" -> "Pixel_6a") is exercised by every test that lists devices.
func TwoClonesFixture() Fixture {
	return WithDevices(
		Device{
			Serial: CloneSerial, Devpath: CloneDevpathA,
			Model: "Pixel 6a", Product: "bluejay", Codename: "bluejay",
			State: StateDevice,
		},
		Device{
			Serial: CloneSerial, Devpath: CloneDevpathB,
			Model: "Pixel 6a", Product: "bluejay", Codename: "bluejay",
			State: StateDevice,
		},
	)
}

// Defaults for FlappingFixture when the caller supplies a zero Device.
const (
	FlapSerial  = "FLAP000000000001"
	FlapDevpath = "usb:2-1.1"
)

// FlappingFixture installs a device that cycles device -> offline -> device
// every halfPeriod until the server closes. Zero fields are defaulted, so
//
//	srv := fakeadb.Start(t, fakeadb.FlappingFixture(fakeadb.Device{}, 20*time.Millisecond))
//
// gives a device that is never reliably in any one state.
//
// A flapping handset is the single most common thing in a real farm and the
// input that STF #663 turns into data loss: every transition pushes a
// track-devices snapshot and may fail an in-flight command. None of that may
// reach lease state. Tests wanting exact transitions should drive SetState
// directly instead; this fixture is for proving that churn changes nothing.
func FlappingFixture(d Device, halfPeriod time.Duration) Fixture {
	return func(s *Server) {
		if d.Serial == "" {
			d.Serial = FlapSerial
		}
		if d.Devpath == "" {
			d.Devpath = FlapDevpath
		}
		if d.State == "" {
			d.State = StateDevice
		}
		dev := s.Add(d)
		s.Flap(dev.Devpath, halfPeriod)
	}
}

// HubFixture fills ports 1..n of one hub: devpaths usb:<bus>-<port>.1 through
// usb:<bus>-<port>.n, all healthy. Devices sharing a hub share a power domain
// and a failure mode, which is what makes a whole-hub event worth a test.
func HubFixture(bus, port, n int) Fixture {
	return func(s *Server) {
		for i := 1; i <= n; i++ {
			s.Add(Device{
				Serial:   fmt.Sprintf("HUB%d%02d%04d", bus, port, i),
				Devpath:  fmt.Sprintf("usb:%d-%d.%d", bus, port, i),
				Model:    "Pixel 6a",
				Product:  "bluejay",
				Codename: "bluejay",
				State:    StateDevice,
			})
		}
	}
}

// Flap toggles a device between StateDevice and StateOffline every halfPeriod.
// It returns a stop function; flapping also stops when the server closes.
func (s *Server) Flap(devpath string, halfPeriod time.Duration) (stop func()) {
	if halfPeriod <= 0 {
		halfPeriod = 100 * time.Millisecond
	}
	quit := make(chan struct{})
	var once sync.Once
	stop = func() { once.Do(func() { close(quit) }) }

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return stop
	}
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		t := time.NewTicker(halfPeriod)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				s.Update(devpath, func(d *Device) {
					if d.State == StateDevice {
						d.State = StateOffline
					} else {
						d.State = StateDevice
					}
				})
			case <-quit:
				return
			case <-s.done:
				return
			}
		}
	}()
	return stop
}

// ---------------------------------------------------------------------
// Scripted device output
// ---------------------------------------------------------------------

type script struct {
	devpath string // "" matches any device
	prefix  string // service prefix, e.g. "shell:getprop "
	payload string
	// fn, when set, computes the payload from the service string instead.
	fn func(service string) string
}

// Echo is the default payload a device service returns: the device's identity
// followed by the service it was asked for. A test asserting that a command
// reached one clone and not the other can compare against Echo directly.
func Echo(d Device, service string) string {
	return fmt.Sprintf("fakeadb %s %s %s\n", d.Devpath, d.Serial, service)
}

// Respond scripts the payload a device service returns. devpath "" matches any
// device; servicePrefix "" matches any service; the most recently registered
// match wins. Only the payload is scripted — the OKAY that precedes it and the
// close that follows it are the protocol's, not the script's.
//
//	srv.Respond(fakeadb.CloneDevpathA, "shell:getprop ro.serialno", "0123456789ABCDEF\n")
func (s *Server) Respond(devpath, servicePrefix, payload string) {
	s.mu.Lock()
	s.scripts = append(s.scripts, script{devpath: devpath, prefix: servicePrefix, payload: payload})
	s.mu.Unlock()
}

// RespondWith scripts a device service whose answer depends on what the device
// was told earlier — a file on the phone that reads back what the previous
// command wrote, which is what a brand looks like from the wire. fn receives
// the whole service string and returns the payload. It is called outside the
// server's lock, so it may keep its own state under its own lock, and it may
// call back into the server.
//
//	srv.RespondWith(devpath, "shell,v2,raw:", func(svc string) string { ... })
func (s *Server) RespondWith(devpath, servicePrefix string, fn func(service string) string) {
	s.mu.Lock()
	s.scripts = append(s.scripts, script{devpath: devpath, prefix: servicePrefix, fn: fn})
	s.mu.Unlock()
}

func (s *Server) scriptFor(d Device, service string) []byte {
	sc, ok := s.matchScript(d, service)
	switch {
	case !ok:
		return []byte(Echo(d, service))
	case sc.fn != nil:
		return []byte(sc.fn(service))
	}
	return []byte(sc.payload)
}

// matchScript picks the most recently registered script that applies. The
// lock is released before a scripted function runs, so the function is free
// to consult or change the server.
func (s *Server) matchScript(d Device, service string) (script, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.scripts) - 1; i >= 0; i-- {
		sc := s.scripts[i]
		if sc.devpath != "" && sc.devpath != d.Devpath {
			continue
		}
		if sc.prefix != "" && !strings.HasPrefix(service, sc.prefix) {
			continue
		}
		return sc, true
	}
	return script{}, false
}

// ---------------------------------------------------------------------
// Failure injection
// ---------------------------------------------------------------------

// FaultKind is how a scripted failure manifests on the wire.
type FaultKind int

const (
	// FaultNone is the zero value: no fault was applied. A rule carrying it
	// still honours Delay, which is the way to script a reply that is slow
	// but perfectly correct — the case a naive timeout mistakes for death.
	FaultNone FaultKind = iota

	// FaultFail answers with a well-formed FAIL frame. The protocol worked;
	// the request did not.
	FaultFail

	// FaultReset severs the connection with a TCP RST, so the peer reads
	// ECONNRESET rather than EOF. With AfterBytes it cuts the wire partway
	// through a reply, leaving the client holding half a frame. This is the
	// exact shape of DeviceFarmer/STF issue #663.
	FaultReset

	// FaultHang accepts the request and answers nothing, ever. The only thing
	// that ends the call is the caller's context deadline — which is the
	// point: a fake that eventually replied would test nothing.
	FaultHang
)

func (k FaultKind) String() string {
	switch k {
	case FaultFail:
		return "fail"
	case FaultReset:
		return "reset"
	case FaultHang:
		return "hang"
	default:
		return "none"
	}
}

// Fault is a scripted failure rule. Rules are consulted in registration order;
// the first that matches and is neither skipped nor exhausted fires.
type Fault struct {
	// Match is a substring of the service string, e.g. "host:track-devices"
	// or "shell:". Empty matches every request.
	Match string

	// Devpath restricts the rule to one device: the device a request resolved
	// to, or the literal target when it resolved to none. Empty matches any.
	Devpath string

	Kind FaultKind

	// Message is the FAIL text for FaultFail.
	Message string

	// AfterBytes is how much of the reply FaultReset writes before severing.
	// Zero severs before a single byte goes out; a value inside the header
	// leaves the client with a truncated length prefix.
	AfterBytes int

	// Delay is applied before the fault takes effect, for a slow reply that
	// still eventually arrives, or a stream that dies mid-flight.
	Delay time.Duration

	// Skip lets this many matching opportunities pass untouched first. On a
	// track-devices stream, opportunities are snapshots, so Skip is "sever
	// after N device-list updates".
	Skip int

	// Times caps how often the rule fires. Zero means unlimited.
	Times int
}

type faultRule struct {
	spec    Fault
	skipped int
	used    int
}

// Inject registers a fault rule.
func (s *Server) Inject(f Fault) {
	s.mu.Lock()
	s.faults = append(s.faults, &faultRule{spec: f})
	s.mu.Unlock()
}

// ClearFaults removes every rule, including partially consumed ones.
func (s *Server) ClearFaults() {
	s.mu.Lock()
	s.faults = nil
	s.mu.Unlock()
}

// FailNext answers the next matching request with FAIL and message.
func (s *Server) FailNext(match, message string) {
	s.Inject(Fault{Match: match, Kind: FaultFail, Message: message, Times: 1})
}

// ResetNext severs the next matching request after afterBytes bytes of reply,
// with a RST so the client sees ECONNRESET. Pass 0 to sever before replying.
func (s *Server) ResetNext(match string, afterBytes int) {
	s.Inject(Fault{Match: match, Kind: FaultReset, AfterBytes: afterBytes, Times: 1})
}

// ResetAfter lets opportunities matching requests through, then severs the
// next one. On a stream an opportunity is one snapshot:
//
//	srv.ResetAfter("host:track-devices", 2) // die after two device-list updates
func (s *Server) ResetAfter(match string, opportunities int) {
	s.Inject(Fault{Match: match, Kind: FaultReset, Skip: opportunities, Times: 1})
}

// HangNext accepts the next matching request and never answers it.
func (s *Server) HangNext(match string) {
	s.Inject(Fault{Match: match, Kind: FaultHang, Times: 1})
}

// takeFault selects and consumes the rule that applies to this request.
func (s *Server) takeFault(service, devpath string) *Fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.faults {
		if r.spec.Devpath != "" && r.spec.Devpath != devpath {
			continue
		}
		if r.spec.Match != "" && !strings.Contains(service, r.spec.Match) {
			continue
		}
		if r.spec.Times > 0 && r.used >= r.spec.Times {
			continue
		}
		if r.skipped < r.spec.Skip {
			r.skipped++
			continue
		}
		r.used++
		s.stats.Faults++
		spec := r.spec
		return &spec
	}
	return nil
}
