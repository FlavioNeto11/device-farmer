package lease

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/config"
)

// =====================================================================
// The witness loop
//
// The property under test is a branch, not a duration: what the loop does when
// the server declines a witness. Getting it wrong looks harmless — "the
// witness was refused, so let go of the device" reads like tidiness — and it
// is DeviceFarmer/STF #663 rebuilt out of on-device evidence. So the refusal
// test below is written twice: once against the loop, and once against a
// deliberately broken loop that DOES cancel, to prove the assertions can tell
// the difference.
// =====================================================================

// stubEvidence stands in for the device-side marker. Fresh by default, because
// most of these tests are about what happens after the evidence is in hand.
type stubEvidence struct {
	mu    sync.Mutex
	stale bool
	none  bool
	calls int
}

func (e *stubEvidence) WitnessedAt() (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	switch {
	case e.none:
		return time.Time{}, false
	case e.stale:
		return time.Now().Add(-24 * time.Hour), true
	default:
		return time.Now(), true
	}
}

func (e *stubEvidence) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// quietWitness is the fast test cadence with the loop's logs discarded; the
// refusal and cap paths both log on purpose.
func quietWitness(interval time.Duration) WitnessConfig {
	return WitnessConfig{
		Interval:       interval,
		Timeout:        2 * time.Second,
		MaxEvidenceAge: time.Hour,
		Logger:         quietConfig(interval).Logger,
	}
}

func startWitness(t *testing.T, h *Holder, ev Evidence, cfg WitnessConfig) *WitnessLoop {
	t.Helper()
	w, err := h.StartWitness(ev, cfg)
	if err != nil {
		t.Fatalf("starting the witness loop: %v", err)
	}
	t.Cleanup(w.Stop)
	return w
}

// ---------------------------------------------------------------------
// A refused witness must not end the job
// ---------------------------------------------------------------------

// witnessOutcome is what the refusal assertions run against: everything an
// observer can see about the holder after the server has declined a witness
// several times over.
type witnessOutcome struct {
	refused   uint64
	cancelled bool
	cause     error
	fenced    bool
}

// assertT is the slice of *testing.T the assertions below need, so the same
// assertions can be run against a recorder in the falsification test.
type assertT interface {
	Helper()
	Errorf(format string, args ...any)
}

// recordingT is an assertT that remembers failures instead of failing.
type recordingT struct{ failures []string }

func (r *recordingT) Helper() {}
func (r *recordingT) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// assertWitnessRefusalWasHarmless is the contract in store.go, as an
// assertion: a refused witness is not a fencing event, must never abort a job,
// and only Renew may report fencing.
func assertWitnessRefusalWasHarmless(t assertT, got witnessOutcome) {
	t.Helper()

	if got.refused == 0 {
		t.Errorf("the loop recorded no refusals; this asserts nothing about the refusal path")
	}
	if got.cancelled {
		t.Errorf("a refused witness cancelled the holder's context (cause %v); "+
			"the extension cap being spent, or a stale fence, is not a reason to abort a running job — "+
			"only Renew may report fencing", got.cause)
	}
	if got.fenced {
		t.Errorf("a refused witness made Fenced() true; the job would be abandoned and its device " +
			"handed away on evidence that says nothing about who holds the lease")
	}
	if got.cause != nil {
		t.Errorf("the holder's cancellation cause is %v after refused witnesses, want nil", got.cause)
	}
}

// TestRefusedWitnessNeverEndsTheJob drives the loop against a store that
// errors and then refuses, and asserts the job is untouched by both.
func TestRefusedWitnessNeverEndsTheJob(t *testing.T) {
	l := testLease()
	fake := &fakeOps{
		witnessFn: func(call int) (time.Time, bool, error) {
			if call <= 2 {
				// TRANSIENT: the database was briefly unreachable. Retried on
				// the next tick, inside the lease the job still holds.
				return time.Time{}, false, transient(l.ID)
			}
			// REFUSED: farm.lease_witness matched no row.
			return time.Time{}, false, nil
		},
	}

	// An hour between renewals: the witness is the only thing talking, so
	// nothing else can be credited with keeping the job alive.
	h := startHolder(t, fake, l, quietConfig(time.Hour))
	ev := &stubEvidence{}
	w := startWitness(t, h, ev, quietWitness(time.Millisecond))

	// The holder ending is a terminating condition too, and the wrong one. A
	// loop that aborts the job on the first refusal never reaches three, so
	// without this the test would die on a timeout and the assertions that
	// name the actual mistake would never run.
	waitFor(t, "three refused witnesses", func() bool {
		return w.Stats().Refused >= 3 || h.Context().Err() != nil
	})

	assertWitnessRefusalWasHarmless(t, witnessOutcome{
		refused:   w.Stats().Refused,
		cancelled: h.Context().Err() != nil,
		cause:     h.Err(),
		fenced:    h.Fenced(),
	})

	if got := w.Stats().Errors; got < 2 {
		t.Errorf("the loop recorded %d transport errors, want the 2 the store returned; "+
			"a witness that failed on the wire must be retried, not swallowed", got)
	}
	if got := w.Stats().Accepted; got != 0 {
		t.Errorf("the loop counted %d accepted witnesses where every call was refused", got)
	}
	// Neither a refusal nor an error spends an extension: nothing was extended.
	if got := w.Stats().Consecutive; got != 0 {
		t.Errorf("consecutive extensions = %d after only refusals and errors, want 0", got)
	}
}

// TestRefusedWitnessAssertionsCatchTheOppositeBehaviour is the falsification.
//
// It runs the SAME assertions against a loop wired to cancel the holder when a
// witness is refused — the mistake the design forbids — and requires them to
// fail. Without this, TestRefusedWitnessNeverEndsTheJob could be passing
// because it checks nothing.
func TestRefusedWitnessAssertionsCatchTheOppositeBehaviour(t *testing.T) {
	l := testLease()
	fake := &fakeOps{
		witnessFn: func(int) (time.Time, bool, error) { return time.Time{}, false, nil },
	}
	h := startHolder(t, fake, l, quietConfig(time.Hour))

	// The bug, built on purpose: a refused witness treated as a verdict about
	// the lease. This is the only place in the tree that does it.
	cfg := quietWitness(time.Millisecond)
	cfg.Hooks.OnRefused = func(Lease) {
		h.stopOnce.Do(func() { h.cancel(fmt.Errorf("lease %s: %w", l.ID, ErrFenced)) })
	}
	w := startWitness(t, h, &stubEvidence{}, cfg)

	waitDone(t, h, "the broken loop to cancel the holder")

	got := witnessOutcome{
		refused:   w.Stats().Refused,
		cancelled: h.Context().Err() != nil,
		cause:     h.Err(),
		fenced:    h.Fenced(),
	}
	// A mutant that did not actually misbehave would make this test vacuous.
	if !got.cancelled {
		t.Fatal("the deliberately broken loop did not cancel; this test proves nothing about the assertions")
	}

	var rec recordingT
	assertWitnessRefusalWasHarmless(&rec, got)
	if len(rec.failures) == 0 {
		t.Fatal("the refusal assertions passed a loop that cancels the job on a refused witness; " +
			"they would not notice #663 being rebuilt out of on-device evidence")
	}
	t.Logf("the assertions rejected the broken loop, as they must: %v", rec.failures)
}

// ---------------------------------------------------------------------
// The extension cap
// ---------------------------------------------------------------------

// TestWitnessLoopHonoursTheExtensionCap: with no renewal landing, the loop
// presents at most WitnessMaxExtensions witnesses and then stops asking.
//
// The cap is what stops a wedged agent holding a device forever on device-side
// evidence alone. farm.lease_witness enforces it server-side; the loop honours
// it client-side so the requests it keeps sending are not ones it knows will
// be declined.
func TestWitnessLoopHonoursTheExtensionCap(t *testing.T) {
	l := testLease()
	fake := &fakeOps{
		witnessFn: func(int) (time.Time, bool, error) {
			return time.Now().Add(30 * time.Minute), true, nil
		},
	}

	// A cap unlike the package default, so a loop that ignored the holder's
	// configuration and fell back to DefaultWitnessMaxExtensions is visibly
	// wrong rather than accidentally right.
	const capacity = 3
	cfg := quietConfig(time.Hour) // no renewals: nothing resets the counter
	cfg.WitnessMaxExtensions = capacity

	h := startHolder(t, fake, l, cfg)
	w := startWitness(t, h, &stubEvidence{}, quietWitness(time.Millisecond))

	// A loop that ignores the cap never sets CapSpent, so waiting only for
	// that would report a timeout instead of the overrun the count below
	// describes exactly.
	waitFor(t, "the cap to be spent", func() bool {
		return w.Stats().CapSpent || len(fake.witnessCalls()) > 4*capacity
	})

	// Roughly a hundred further ticks. If the cap were not honoured they would
	// all present.
	time.Sleep(100 * time.Millisecond)

	if got := len(fake.witnessCalls()); got != capacity {
		t.Errorf("the store saw %d witness calls under a cap of %d; a wedged agent could hold this "+
			"device on device-side evidence alone", got, capacity)
	}
	if got := w.Stats().Accepted; got != capacity {
		t.Errorf("accepted = %d, want %d", got, capacity)
	}
	for i, c := range fake.witnessCalls() {
		if c.maxExtensions != capacity {
			t.Errorf("witness %d presented a cap of %d, want the holder's %d; an operator who tightened "+
				"the cap would have tightened nothing", i+1, c.maxExtensions, capacity)
		}
		if c.fence != l.Fence {
			t.Errorf("witness %d presented fence %d, want %d", i+1, c.fence, l.Fence)
		}
	}

	// Spending the cap is not an ending. The device is still ours; the reaper
	// simply stops being told about it, and the renewal loop carries on.
	if h.Context().Err() != nil || h.Fenced() {
		t.Errorf("the holder ended when the witness cap was spent: cause %v, fenced %v", h.Err(), h.Fenced())
	}
}

// TestSuccessfulRenewResetsTheWitnessExtensionCounter mirrors farm.lease_renew,
// which sets witness_extensions = 0 on every successful heartbeat.
//
// The cap counts CONSECUTIVE witness-only extensions. Without the reset a long
// job spends its extensions early and has no protection left for the outage it
// eventually meets — the failure this whole mechanism exists to prevent.
func TestSuccessfulRenewResetsTheWitnessExtensionCounter(t *testing.T) {
	l := testLease()
	fake := &fakeOps{
		witnessFn: func(int) (time.Time, bool, error) {
			return time.Now().Add(30 * time.Minute), true, nil
		},
	}

	const capacity = 2
	cfg := quietConfig(time.Millisecond) // renewals land continuously
	cfg.WitnessMaxExtensions = capacity

	h := startHolder(t, fake, l, cfg)
	// Slower than the renewal cadence, so a renewal lands between every pair of
	// witnesses and the counter is reset each time.
	w := startWitness(t, h, &stubEvidence{}, quietWitness(5*time.Millisecond))

	want := uint64(4 * capacity)
	// A loop that never resets parks on its cap and can never reach `want`, so
	// that state ends the wait as well — otherwise the whole failure would be
	// a timeout, and a timeout does not say which of the two loops is broken.
	waitFor(t, fmt.Sprintf("%d accepted witnesses past a cap of %d", want, capacity), func() bool {
		return w.Stats().Accepted >= want || (w.Stats().CapSpent && h.Stats().Renewals >= 5)
	})

	if s := w.Stats(); s.CapSpent {
		t.Errorf("the loop parked on its cap after %d accepted witnesses while %d renewals were landing; "+
			"farm.lease_renew zeroes witness_extensions on every heartbeat, so a job that spent its "+
			"extensions in the first half hour would meet its outage with no protection left",
			s.Accepted, h.Stats().Renewals)
	}
	if got := w.Stats().Consecutive; got > capacity {
		t.Errorf("consecutive extensions = %d, above the cap of %d", got, capacity)
	}
	if h.Stats().Renewals == 0 {
		t.Error("no renewal landed; this test would pass on a loop that ignores renewals entirely")
	}
	if h.Context().Err() != nil {
		t.Errorf("the holder ended while both loops were succeeding: %v", h.Err())
	}
}

// ---------------------------------------------------------------------
// Evidence
// ---------------------------------------------------------------------

// TestWitnessIsNotPresentedWithoutFreshEvidence: the witness is on-device
// proof, so a loop with nothing to stand on presents nothing.
//
// A witness sent on a timer alone would be a health probe wearing a holder's
// name: it would tell the reaper that work is running on a device this process
// has not managed to touch for hours.
func TestWitnessIsNotPresentedWithoutFreshEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   *stubEvidence
	}{
		{"no marker was ever written", &stubEvidence{none: true}},
		{"the marker went stale", &stubEvidence{stale: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := testLease()
			fake := &fakeOps{}
			h := startHolder(t, fake, l, quietConfig(time.Hour))

			cfg := quietWitness(time.Millisecond)
			cfg.MaxEvidenceAge = 50 * time.Millisecond
			w := startWitness(t, h, tc.ev, cfg)

			waitFor(t, "the loop to look for evidence", func() bool { return w.Stats().Skipped >= 5 })

			if got := len(fake.witnessCalls()); got != 0 {
				t.Errorf("the store saw %d witness calls with no fresh device-side evidence", got)
			}
			if h.Context().Err() != nil || h.Fenced() {
				t.Errorf("missing evidence ended the job: cause %v, fenced %v; a marker that could not "+
					"be refreshed is not a reason to end anything", h.Err(), h.Fenced())
			}
		})
	}
}

// TestWitnessLoopRefusesToRunWithoutEvidence: the loop cannot be started
// without something that has actually touched the device. Holder.Witness says
// this in a doc comment; here it is refused in code.
func TestWitnessLoopRefusesToRunWithoutEvidence(t *testing.T) {
	h := startHolder(t, &fakeOps{}, testLease(), quietConfig(time.Hour))

	w, err := h.StartWitness(nil, quietWitness(time.Millisecond))
	if err == nil {
		w.Stop()
		t.Fatal("a witness loop started with no source of device-side evidence")
	}
	// Stop is safe on the nil loop a failed start returns, so a caller's
	// `defer w.Stop()` cannot turn a configuration error into a panic.
	w.Stop()
}

// ---------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------

// TestWitnessLoopStopsCleanly covers both ways the loop ends: Stop, and the
// holder's context ending under it.
func TestWitnessLoopStopsCleanly(t *testing.T) {
	t.Run("Stop is idempotent and blocks until the goroutine is gone", func(t *testing.T) {
		fake := &fakeOps{}
		h := startHolder(t, fake, testLease(), quietConfig(time.Hour))
		ev := &stubEvidence{}
		w := startWitness(t, h, ev, quietWitness(time.Millisecond))

		waitFor(t, "the loop to present a witness", func() bool { return w.Stats().Accepted >= 1 })
		w.Stop()
		w.Stop() // idempotent

		// No tick may run after Stop returns: the loop's goroutine is gone, so
		// the evidence source is never consulted again.
		before := ev.count()
		time.Sleep(20 * time.Millisecond)
		if after := ev.count(); after != before {
			t.Errorf("the evidence source was consulted %d more times after Stop returned", after-before)
		}
	})

	t.Run("the loop exits when the lease does", func(t *testing.T) {
		h := startHolder(t, &fakeOps{}, testLease(), quietConfig(time.Hour))
		ev := &stubEvidence{}
		w := startWitness(t, h, ev, quietWitness(time.Millisecond))

		waitFor(t, "the loop to present a witness", func() bool { return w.Stats().Accepted >= 1 })

		h.Stop() // SIGTERM: the lease survives, this process does not
		waitFor(t, "the witness loop to notice the holder is gone", func() bool {
			before := ev.count()
			time.Sleep(5 * time.Millisecond)
			return ev.count() == before
		})

		// And Stop on an already-exited loop still returns rather than
		// blocking on a goroutine that has closed its own door.
		done := make(chan struct{})
		go func() { w.Stop(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop blocked after the loop had already exited")
		}
	})
}

// gatedEvidence parks the witness loop inside presentOnce, so a test can
// arrange the exact interleaving the loop has to get right: a tick already
// waiting in the ticker's buffer at the instant the holder lets go of the
// lease.
type gatedEvidence struct {
	mu     sync.Mutex
	calls  int
	parked bool

	gate    chan struct{} // released by the test
	entered chan struct{} // closed when the loop is parked
}

func armedEvidence() *gatedEvidence {
	return &gatedEvidence{gate: make(chan struct{}), entered: make(chan struct{})}
}

// WitnessedAt blocks the first time it is called and never again, so the loop
// is parked exactly once and runs at full speed afterwards.
func (e *gatedEvidence) WitnessedAt() (time.Time, bool) {
	e.mu.Lock()
	e.calls++
	first := !e.parked
	e.parked = true
	e.mu.Unlock()

	if first {
		close(e.entered)
		<-e.gate
	}
	return time.Now(), true
}

// TestWitnessIsNotPresentedAfterTheHolderLetsGo
//
// Stop is SIGTERM, a node drain, an eviction: the process goes away and THE
// LEASE DELIBERATELY DOES NOT. It stays live at the same fence so the
// replacement pod can re-attach and resume. A witness presented after that
// point lands on a lease this process is in the middle of walking away from
// and pushes its reclaimable_at out by a grace period, holding a device whose
// job has no runner — the reaper's own path made slower by the thing that was
// supposed to be evidence of work in progress.
//
// The loop has to notice, and the reason it might not is that select picks at
// random among ready cases: when a tick is already buffered as the holder's
// context closes, "present a witness" and "the lease is gone" are both ready
// and the coin decides. So this runs the interleaving many times. A loop that
// only checks once loses the flip about half the time, which over this many
// trials is not a thing that can pass by luck.
func TestWitnessIsNotPresentedAfterTheHolderLetsGo(t *testing.T) {
	const trials = 25

	late := 0
	for i := range trials {
		func() {
			fake := &fakeOps{
				witnessFn: func(int) (time.Time, bool, error) {
					return time.Now().Add(30 * time.Minute), true, nil
				},
			}
			h := startHolder(t, fake, testLease(), quietConfig(time.Hour))
			ev := armedEvidence()
			w := startWitness(t, h, ev, quietWitness(2*time.Millisecond))
			defer w.Stop()

			select {
			case <-ev.entered:
			case <-time.After(10 * time.Second):
				t.Fatalf("trial %d: the loop never reached the evidence source", i)
			}

			// Long enough for the ticker to fire while the loop is parked, so
			// a tick is waiting in its one-slot buffer.
			time.Sleep(6 * time.Millisecond)
			h.Stop()       // the lease survives; this process does not
			close(ev.gate) // let the witness that was already in flight finish

			// Several more ticks' worth of chances to take the wrong branch.
			time.Sleep(15 * time.Millisecond)
			w.Stop()

			if n := len(fake.witnessCalls()); n > 1 {
				late += n - 1
			}
		}()
	}

	if late > 0 {
		t.Errorf("%d witnesses were presented after the holder had let go of its lease, across %d trials; "+
			"each one extends reclaimable_at on a lease whose runner is gone, so the device is held "+
			"past the point where the reaper should have it back", late, trials)
	}
}

// TestWitnessStatsAccountForEveryRoundTrip: Presented is issued round trips and
// Accepted, Refused and Errors partition them.
//
// A branch that returns without counting is how a loop comes to look healthy on
// a dashboard while it has quietly stopped presenting anything, which is the
// state in which a control-plane outage costs a device.
func TestWitnessStatsAccountForEveryRoundTrip(t *testing.T) {
	fake := &fakeOps{
		witnessFn: func(call int) (time.Time, bool, error) {
			switch call % 3 {
			case 0:
				return time.Time{}, false, transient(testLease().ID)
			case 1:
				return time.Time{}, false, nil
			default:
				return time.Now().Add(30 * time.Minute), true, nil
			}
		},
	}
	cfg := quietConfig(time.Millisecond) // renewals land, so the cap keeps resetting
	cfg.WitnessMaxExtensions = 2
	h := startHolder(t, fake, testLease(), cfg)
	w := startWitness(t, h, &stubEvidence{}, quietWitness(time.Millisecond))

	waitFor(t, "a mix of outcomes", func() bool {
		s := w.Stats()
		return s.Accepted >= 3 && s.Refused >= 3 && s.Errors >= 3
	})
	w.Stop() // no round trip can be in flight once this returns

	s := w.Stats()
	if got := s.Accepted + s.Refused + s.Errors; got != s.Presented {
		t.Errorf("presented %d round trips but accounted for %d (accepted %d, refused %d, errors %d)",
			s.Presented, got, s.Accepted, s.Refused, s.Errors)
	}
	if uint64(len(fake.witnessCalls())) != s.Presented {
		t.Errorf("the store saw %d witness calls, the loop counted %d presented",
			len(fake.witnessCalls()), s.Presented)
	}
	if h.Context().Err() != nil || h.Fenced() {
		t.Errorf("the holder ended over a mixture of ordinary witness outcomes: %v", h.Err())
	}
}

// TestWitnessOnlyMovesTheHolderDeadlineOutward is the loop's view of the
// property holder_test.go asserts for a single call: an accepted witness
// pushes the cached reclaimable_at out, and a refusal leaves it alone.
func TestWitnessOnlyMovesTheHolderDeadlineOutward(t *testing.T) {
	l := testLease()
	far := l.ReclaimableAt.Add(20 * time.Minute)

	var refusing sync.Once
	refused := make(chan struct{})
	fake := &fakeOps{
		witnessFn: func(call int) (time.Time, bool, error) {
			if call == 1 {
				return far, true, nil
			}
			refusing.Do(func() { close(refused) })
			return time.Time{}, false, nil
		},
	}
	h := startHolder(t, fake, l, quietConfig(time.Hour))
	w := startWitness(t, h, &stubEvidence{}, quietWitness(time.Millisecond))

	select {
	case <-refused:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a refused witness")
	}
	waitFor(t, "the refusal to be recorded", func() bool { return w.Stats().Refused >= 1 })

	if got := h.Lease().ReclaimableAt; !got.Equal(far) {
		t.Errorf("cached reclaimable_at = %s, want the witnessed %s", got, far)
	}
	if err := h.Err(); err != nil {
		t.Errorf("the holder ended on a witness outcome: %v", err)
	}
}

// =====================================================================
// Against Postgres: the outage the witness exists for
// =====================================================================

// pgRelay is a TCP relay in front of the scratch database that a test can cut
// and restore, so a control-plane outage can be staged for ONE holder's
// connections without touching the pool every other test is using.
//
// The listener stays open for the life of the relay. An outage severs every
// relayed connection and refuses new ones at the door; it does not close the
// listening socket, because a restore that has to bind the same port again is
// a restore that races whatever else on the machine wanted a port in the
// meantime.
type pgRelay struct {
	t      *testing.T
	target string
	addr   string
	ln     net.Listener

	mu    sync.Mutex
	down  bool
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

func startRelay(t *testing.T, target string) *pgRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the relay: %v", err)
	}
	r := &pgRelay{t: t, target: target, addr: ln.Addr().String(), ln: ln, conns: map[net.Conn]struct{}{}}
	r.wg.Add(1)
	go r.serve()
	t.Cleanup(r.close)
	return r
}

func (r *pgRelay) serve() {
	defer r.wg.Done()
	for {
		c, err := r.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.DialTimeout("tcp", r.target, 5*time.Second)
		if err != nil || !r.track(c, up) {
			c.Close()
			if up != nil {
				up.Close()
			}
			continue
		}
		go relay(up, c)
		go relay(c, up)
	}
}

func relay(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	dst.Close()
}

// track registers a relayed pair, refusing it during an outage: a connection
// accepted in the instant cut is severing the others would otherwise survive
// it.
func (r *pgRelay) track(c, up net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.down {
		return false
	}
	r.conns[c] = struct{}{}
	r.conns[up] = struct{}{}
	return true
}

// cut is the outage: every relayed connection is severed and nothing new gets
// through, so the holder's next round trip fails on the wire and every
// reconnect after it is turned away.
func (r *pgRelay) cut() {
	r.mu.Lock()
	r.down = true
	conns := r.conns
	r.conns = map[net.Conn]struct{}{}
	r.mu.Unlock()
	for c := range conns {
		c.Close()
	}
}

// restore lets connections through again, on the same address, so the pool
// that was pointed at it reconnects without being told anything.
func (r *pgRelay) restore() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.down = false
}

func (r *pgRelay) close() {
	r.ln.Close()
	r.cut()
	r.wg.Wait()
}

// relayedPool is a pool onto the scratch database that reaches it only
// through the relay. Everything else about the connection — TLS included:
// the relay forwards bytes and negotiates nothing, so a server that requires
// TLS still gets it — is the fixture's own.
func relayedPool(t *testing.T, src *pgxpool.Pool, relayAddr string) *pgxpool.Pool {
	t.Helper()
	host, port, err := net.SplitHostPort(relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return clonePool(t, src, func(cfg *pgxpool.Config) {
		cfg.ConnConfig.Host = host
		cfg.ConnConfig.Port = uint16(n)
		// No fallback hosts: a pool that could route around the relay would
		// route around the outage.
		cfg.ConnConfig.Fallbacks = nil
		cfg.ConnConfig.ConnectTimeout = 2 * time.Second
		cfg.MinConns = 0
		cfg.MaxConns = 2
	})
}

func (f *fixture) witnessAt(t *testing.T, leaseID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.pool.QueryRow(t.Context(),
		`SELECT witness_at FROM farm.leases WHERE id = $1::uuid`, leaseID).Scan(&at); err != nil {
		t.Fatalf("read witness_at: %v", err)
	}
	return at
}

// TestWitnessKeepsTheDeviceThroughAControlPlaneOutage is the mechanism end to
// end, against Postgres, in the order it happens at 3am:
//
//  1. The job runs. Renewals land, and so do witnesses — presented on every
//     tick, because nobody knows which tick is the last one before the
//     outage.
//  2. The control plane goes away. Every renewal and every witness from this
//     holder fails on the wire. The holder is not fenced and the job is not
//     cancelled: a database that cannot be reached is not a verdict.
//  3. TTL+grace elapses while it is away. The reaper sweeps and reclaims an
//     identical lease that has no witness — the positive control — and passes
//     over this one, because farm.lease_reclaim honours a witness younger
//     than one grace period. This is the line that would otherwise be
//     DeviceFarmer/STF #663 arriving through the control plane.
//  4. The control plane comes back. The next renewal self-heals the lease at
//     the same fence, and the job never noticed.
//
// The evidence here is the stub, because this package cannot import the
// marker that produces it in a running farm; internal/jobrunner's own tests
// cover that the marker is what the loop is handed.
func TestWitnessKeepsTheDeviceThroughAControlPlaneOutage(t *testing.T) {
	f := newFixture(t, 2)
	requireReaperMayAct(t, f.pool)
	ctx := t.Context()

	cc := f.pool.Config().ConnConfig
	if strings.HasPrefix(cc.Host, "/") {
		t.Skipf("%s reaches Postgres over a unix socket; the relay needs TCP", config.EnvDatabaseURL)
	}
	rly := startRelay(t, net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port))))
	blind := NewStore(relayedPool(t, f.pool, rly.addr))

	_, witnessed := f.acquire(t)
	_, control := f.acquire(t)

	// The holder and its witness loop are the only things that go through the
	// relay. The fixture's own pool is the operator's view, and it stays up.
	h := NewHolder(ctx, blind, witnessed, quietConfig(20*time.Millisecond))
	t.Cleanup(h.Stop)
	w := startWitness(t, h, &stubEvidence{}, quietWitness(20*time.Millisecond))

	// 1. Healthy.
	waitFor(t, "a renewal and a witness through the relay", func() bool {
		return h.Stats().Renewals >= 1 && w.Stats().Accepted >= 1
	})
	if f.witnessAt(t, witnessed.ID) == nil {
		t.Fatal("witness_at is NULL after the loop reported an accepted witness")
	}

	// 2. The outage.
	rly.cut()
	waitFor(t, "renewals to fail on the wire", func() bool { return h.Stats().ConsecutiveFailures >= 3 })
	waitFor(t, "a witness to fail on the wire", func() bool { return w.Stats().Errors >= 1 })
	if h.Fenced() || h.Context().Err() != nil {
		t.Fatalf("the outage ended the job: fenced %v, cause %v", h.Fenced(), h.Err())
	}

	// 3. TTL+grace elapses while the control plane is away. Both leases are
	// swept to suspect, which is an alert and releases nothing.
	f.abandon(t, witnessed.ID)
	f.abandon(t, control.ID)

	got, err := f.store.Reclaim(ctx, DefaultReclaimBatch, time.Second)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if _, ok := findReclaimed(got, control.ID); !ok {
		t.Fatalf("the unwitnessed control lease was not reclaimed, so this test proves nothing about "+
			"the witness; the reaper took %+v", got)
	}
	if _, ok := findReclaimed(got, witnessed.ID); ok {
		t.Fatal("the witnessed lease was reclaimed past ttl+grace; the job was demonstrably alive on the " +
			"device and the whole mechanism exists for this moment")
	}
	if state, _ := f.leaseState(t, witnessed.ID); state != "suspect" {
		t.Fatalf("witnessed lease is %q after the sweep, want suspect (held by the witness, awaiting its holder)", state)
	}
	if h.Fenced() || h.Context().Err() != nil {
		t.Fatalf("the reaper's pass ended the job: fenced %v, cause %v", h.Fenced(), h.Err())
	}

	// 4. The control plane returns. The very next renewal that lands heals
	// the lease at the same fence.
	before := h.Stats().Renewals
	rly.restore()
	waitFor(t, "a renewal to land after the outage", func() bool { return h.Stats().Renewals > before })

	if state, _ := f.leaseState(t, witnessed.ID); state != "held" {
		t.Fatalf("witnessed lease is %q after the control plane returned, want held", state)
	}
	if h.Fenced() || h.Context().Err() != nil {
		t.Fatalf("the job did not survive the outage: fenced %v, cause %v", h.Fenced(), h.Err())
	}
	if h.Lease().Fence != witnessed.Fence {
		t.Fatalf("fence = %d after the outage, want the same %d; the job's detached work is still running "+
			"under it", h.Lease().Fence, witnessed.Fence)
	}
}
