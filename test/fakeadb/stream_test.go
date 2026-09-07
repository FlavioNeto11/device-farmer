package fakeadb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// These tests speak the protocol by hand for the reason server_test.go gives:
// a harness and its client sharing a framing implementation hide a matching
// pair of bugs behind a passing test. What a REAL client makes of these
// streams is asserted in internal/adbwire, against the same fake.

// streamWire opens a transport and starts one device service on it, leaving
// the raw stream in front of the caller.
func streamWire(tb testing.TB, s *Server, devpath, service string) *wire {
	tb.Helper()
	w := dial(tb, s)
	w.okBare("host:transport:" + devpath)
	w.okBare(service)
	return w
}

// mustRead reads exactly n bytes or fails.
func mustRead(tb testing.TB, w *wire, n int, what string) []byte {
	tb.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(w.br, buf); err != nil {
		tb.Fatalf("reading %s (%d bytes): %v", what, n, err)
	}
	return buf
}

// TestRespondStreamWritesProgressively is the whole reason this seam exists.
//
// A scripted payload is one write: by the time the client sees any of it, the
// server is finished. A live screen is the opposite, and the claim here is
// stated so that only the progressive server can satisfy it — the handler
// reports how much it had written at the instant the client had already
// consumed the first frame, and the answer has to be "the first frame and
// nothing else".
func TestRespondStreamWritesProgressively(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-1.1"
	s := Start(t, WithDevices(Device{Serial: "SERPROG", Devpath: devpath}))

	read := make(chan struct{})    // closed once the client holds frame one
	wroteAt := make(chan int64, 1) // what the server had written by then
	s.RespondStream(devpath, "screen:", func(sess *StreamSession) error {
		if _, err := sess.Write([]byte("FRAME-1")); err != nil {
			return err
		}
		select {
		case <-read:
		case <-sess.Done:
			return nil
		}
		wroteAt <- sess.Wrote()
		_, err := sess.Write([]byte("FRAME-2"))
		return err
	})

	w := streamWire(t, s, devpath, "screen:live")

	if got := string(mustRead(t, w, 7, "the first frame")); got != "FRAME-1" {
		t.Fatalf("first frame = %q, want %q — the stream was not written progressively", got, "FRAME-1")
	}
	close(read)

	if got := <-wroteAt; got != 7 {
		t.Fatalf("the server had written %d bytes when the client held frame one, want 7", got)
	}
	rest, err := io.ReadAll(w.br)
	if err != nil {
		t.Fatalf("reading the rest of the stream: %v", err)
	}
	if string(rest) != "FRAME-2" {
		t.Fatalf("the rest of the stream = %q, want %q", rest, "FRAME-2")
	}
}

// TestRespondStreamCarriesAMegabyteInChunks is the volume case: a screen is
// many small writes, and every one of them has to arrive, in order, with
// nothing coalesced away and nothing lost to a buffer the fake forgot to
// flush.
func TestRespondStreamCarriesAMegabyteInChunks(t *testing.T) {
	t.Parallel()

	const (
		devpath = "usb:7-1.2"
		chunk   = 4096
		total   = 1 << 20
	)
	want := make([]byte, total)
	for i := range want {
		// A pattern with a period that is not a factor of the chunk size, so
		// a chunk delivered out of order or twice does not reassemble into
		// something that still matches.
		want[i] = byte(i % 251)
	}

	s := Start(t, WithDevices(Device{Serial: "SERBULK", Devpath: devpath}))
	wrote := make(chan int64, 1)
	s.RespondStream(devpath, "screen:", func(sess *StreamSession) error {
		for off := 0; off < total; off += chunk {
			if _, err := sess.Write(want[off : off+chunk]); err != nil {
				return err
			}
		}
		wrote <- sess.Wrote()
		return nil
	})

	w := streamWire(t, s, devpath, "screen:bulk")
	got, err := io.ReadAll(w.br)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if len(got) != total {
		t.Fatalf("read %d bytes, want %d", len(got), total)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the stream did not arrive intact; first difference at byte %d", firstDiff(got, want))
	}
	if n := <-wrote; n != total {
		t.Fatalf("Wrote() = %d, want %d", n, total)
	}
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}

// TestRespondStreamReadsTheClientDirection covers the half deviceStream never
// had: a control socket is bytes travelling towards the device, and a fake
// that cannot receive them cannot test an input path at all.
func TestRespondStreamReadsTheClientDirection(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-1.3"
	s := Start(t, WithDevices(Device{Serial: "SERCTL", Devpath: devpath}))

	s.RespondStream(devpath, "control:", func(sess *StreamSession) error {
		var first [4]byte
		if _, err := io.ReadFull(sess, first[:]); err != nil {
			return err
		}
		if _, err := sess.Write(append([]byte("SAW:"), first[:]...)); err != nil {
			return err
		}
		// The client half-closes after its last message, so the rest of the
		// direction reads as an ordinary end of stream rather than as a
		// failure — and the handler must be able to tell the difference.
		tail, err := io.ReadAll(sess)
		if err != nil {
			return err
		}
		_, err = sess.Write(append([]byte("THEN:"), tail...))
		return err
	})

	w := streamWire(t, s, devpath, "control:v1")
	if _, err := w.c.Write([]byte("PING")); err != nil {
		t.Fatalf("writing to the device: %v", err)
	}
	if got := string(mustRead(t, w, 8, "the receipt")); got != "SAW:PING" {
		t.Fatalf("receipt = %q, want %q", got, "SAW:PING")
	}

	if _, err := w.c.Write([]byte("MORE")); err != nil {
		t.Fatalf("writing the tail: %v", err)
	}
	cw, ok := w.c.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("the test connection (%T) cannot half-close", w.c)
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatalf("half-closing: %v", err)
	}

	rest, err := io.ReadAll(w.br)
	if err != nil {
		t.Fatalf("reading the rest: %v", err)
	}
	if string(rest) != "THEN:MORE" {
		t.Fatalf("after the half-close the handler saw %q, want %q", rest, "THEN:MORE")
	}
}

// TestSeverCutsTheStreamMidFlight holds the failure this package exists to
// produce on demand. A handler that stops writing is a quiet screen; a
// handler that severs is a screen whose transport died, and the two must not
// look alike on the wire. What a client makes of the difference is asserted
// in internal/adbwire; here the claim is that the stream does not end the way
// a finished one ends, and that the request log says so.
func TestSeverCutsTheStreamMidFlight(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-1.4"
	s := Start(t, WithDevices(Device{Serial: "SERCUT", Devpath: devpath}))

	read := make(chan struct{})
	afterSever := make(chan error, 1)
	s.RespondStream(devpath, "screen:", func(sess *StreamSession) error {
		if _, err := sess.Write([]byte("HEADER")); err != nil {
			return err
		}
		<-read
		sess.Sever()
		// A severed session is gone in both directions, which a handler
		// discovers the same way anything else does.
		_, err := sess.Write([]byte("TAIL"))
		afterSever <- err
		return nil
	})

	w := streamWire(t, s, devpath, "screen:live")
	if got := string(mustRead(t, w, 6, "the header")); got != "HEADER" {
		t.Fatalf("header = %q", got)
	}
	close(read)

	var buf [8]byte
	_, err := io.ReadFull(w.br, buf[:])
	if err == nil {
		t.Fatalf("the stream kept delivering after a sever: %q", buf)
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("a severed stream ended with %v, which is how a FINISHED stream ends; "+
			"the caller cannot tell a dead transport from a completed service", err)
	}
	if err := <-afterSever; err == nil {
		t.Fatalf("writing after Sever succeeded; the socket was not cut")
	}

	if got := lastReply(t, s, "screen:live"); got != "RESET" {
		t.Fatalf("the request log recorded %q, want RESET", got)
	}
	// The listener survives a severed stream, because a severed stream is a
	// reconnect and nothing more.
	if got := hostOK(t, s, "host:version"); got != "0029" {
		t.Fatalf("the server stopped answering after a sever: %q", got)
	}
}

// TestAStreamHandlerErrorIsRecordedAndSevers keeps a failing fixture loud. A
// handler that returned an error and then closed politely would hand the
// client a truncated stream that looks complete, and the test would fail
// somewhere else entirely.
func TestAStreamHandlerErrorIsRecordedAndTruncates(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-1.5"
	s := Start(t, WithDevices(Device{Serial: "SERERR", Devpath: devpath}))
	s.RespondStream(devpath, "screen:", func(sess *StreamSession) error {
		if _, err := sess.Write([]byte("PART")); err != nil {
			return err
		}
		return errors.New("the fixture ran out of frames")
	})

	w := streamWire(t, s, devpath, "screen:live")
	// What the handler wrote, the client receives — and then the stream ends.
	//
	// This asserted that ReadAll ERRORED, which meant the fake had to reset a
	// connection it had written four bytes on a moment earlier. A reset
	// discards what the peer has not yet read, including the protocol's own
	// OKAY, so the assertion held only when the kernel happened to deliver
	// first: the suite failed about one run in twenty, here and in the scrcpy
	// fixture's two refusal tests.
	//
	// The property that matters survives, and is stronger: the stream is
	// TRUNCATED. "PART" arrives, nothing follows it, and the request log names
	// the reason. A caller detects the truncation the way anything detects one,
	// by the thing being truncated — internal/scrcpy reports a packet cut short
	// as exactly that. An explicit Sever still resets; see
	// TestASeveredStreamIsATransportErrorNotARefusal, which is careful to let
	// the client read before it severs.
	got, _ := io.ReadAll(w.br)
	if string(got) != "PART" {
		t.Fatalf("the client received %q, want %q: a handler that failed halfway still wrote "+
			"that much, and a fake that swallowed it would make every truncation look like a "+
			"connection that was never accepted", got, "PART")
	}

	want := "ERROR: the fixture ran out of frames"
	if got := lastReply(t, s, "screen:live"); got != want {
		t.Fatalf("the request log recorded %q, want %q", got, want)
	}
}

// TestAnInjectedFaultStillRefusesADuplexService is why the OKAY goes out
// through finish rather than straight down the socket. Failure injection is
// scripted against the service string, and a duplex service is a service.
func TestAnInjectedFaultStillRefusesADuplexService(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-1.6"
	s := Start(t, WithDevices(Device{Serial: "SERFLT", Devpath: devpath}))

	ran := make(chan struct{}, 1)
	s.RespondStream(devpath, "screen:", func(sess *StreamSession) error {
		ran <- struct{}{}
		return nil
	})

	s.FailNext("screen:", "no screen on this device")
	w := dial(t, s)
	w.okBare("host:transport:" + devpath)
	if got := w.fail("screen:live"); got != "no screen on this device" {
		t.Fatalf("FAIL reason = %q", got)
	}
	select {
	case <-ran:
		t.Fatalf("the handler ran for a service the server refused")
	default:
	}

	// And with no fault in the way the same registration serves.
	if got := streamAll(t, s, devpath, "screen:live"); got != "" {
		t.Fatalf("the unfaulted stream carried %q", got)
	}
	if len(ran) != 1 {
		t.Fatalf("the handler ran %d times after the fault was spent, want once", len(ran))
	}
}

// TestStreamScriptsWinOverScriptedPayloads pins the precedence deviceStream
// applies, and the matching rules it inherits from RespondWith.
func TestStreamScriptsWinOverScriptedPayloads(t *testing.T) {
	t.Parallel()

	const (
		devA = "usb:7-2.1"
		devB = "usb:7-2.2"
	)
	s := Start(t, WithDevices(
		Device{Serial: "SERA", Devpath: devA},
		Device{Serial: "SERB", Devpath: devB},
	))

	s.Respond("", "screen:", "a scripted blob\n")
	s.RespondStream("", "screen:", func(sess *StreamSession) error {
		_, err := sess.Write([]byte("any device"))
		return err
	})
	s.RespondStream(devB, "screen:", func(sess *StreamSession) error {
		_, err := sess.Write([]byte("device B"))
		return err
	})

	// The most recently registered match wins, and a devpath-scoped stream
	// script only fires for that position.
	if got := streamAll(t, s, devB, "screen:live"); got != "device B" {
		t.Fatalf("device B answered %q", got)
	}
	if got := streamAll(t, s, devA, "screen:live"); got != "any device" {
		t.Fatalf("device A answered %q", got)
	}
	// A service no stream script matches still gets its scripted payload.
	if got := streamAll(t, s, devA, "shell:id"); got != Echo(mustDevice(t, s, devA), "shell:id") {
		t.Fatalf("an unmatched service answered %q", got)
	}
}

// TestCloseUnblocksParkedHandlersAndWaitsForTheirGoroutines is the property
// the whole package depends on and the one nothing else would catch. A
// handler parked on Done, a drain still holding a socket: either surviving
// Close does not fail the test that leaked it, it flakes the next test in the
// package, which is the worst kind of harness bug there is.
//
// It is deliberately not parallel: it counts goroutines, and a count only
// means anything while nothing else in the package is running.
func TestCloseUnblocksParkedHandlersAndWaitsForTheirGoroutines(t *testing.T) {
	const sessions = 64

	base := settledGoroutines()

	s, err := New()
	if err != nil {
		t.Fatalf("fakeadb: %v", err)
	}
	defer func() { _ = s.Close() }()

	parked := make(chan struct{}, sessions)
	s.RespondStream("", "screen:", func(sess *StreamSession) error {
		parked <- struct{}{}
		<-sess.Done
		return nil
	})

	for i := 0; i < sessions; i++ {
		devpath := devpathFor(i)
		s.Add(Device{Serial: "SERPARK", Devpath: devpath})
		streamWire(t, s, devpath, "screen:live")
	}
	for i := 0; i < sessions; i++ {
		<-parked
	}
	if got := s.Stats().Streams; got != sessions {
		t.Fatalf("Stats().Streams = %d, want %d", got, sessions)
	}

	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("Close did not return: %d handlers are still parked on Done", s.Stats().Streams)
	}

	if got := s.Stats().Streams; got != 0 {
		t.Fatalf("Stats().Streams = %d after Close, want 0", got)
	}

	// The claim is not "eventually": Close returns only once every goroutine
	// it started has returned, so the count is already back the instant it
	// does. The slack is for the runtime's own workers, which come and go
	// under any test; it is a fortieth of what an uncounted drain per session
	// would leave behind, so it cannot absorb the bug it is here to catch.
	if n := runtime.NumGoroutine(); n > base+sessions/4 {
		t.Fatalf("%d goroutines the instant Close returned, from a baseline of %d; "+
			"Close is not waiting for everything it started", n, base)
	}
}

func devpathFor(i int) string { return fmt.Sprintf("usb:8-1.%d", i+1) }

// settledGoroutines waits for the runtime to stop churning and returns the
// count. Without it the baseline picks up whatever the previous test left
// mid-exit and the comparison measures that instead.
func settledGoroutines() int {
	last := runtime.NumGoroutine()
	stable := 0
	for i := 0; i < 200 && stable < 5; i++ {
		time.Sleep(2 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == last {
			stable++
			continue
		}
		last, stable = n, 0
	}
	return last
}

// TestAQuietHandlerStillNoticesAHangUp is the leak watchPeer was added to
// stop, in its duplex form. A screen stream writes and does not read, so
// nothing it does would discover that the viewer closed the tab; a session
// that only noticed on its next write would park forever on a farm where
// nothing is moving.
func TestAQuietHandlerStillNoticesAHangUp(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-3.1"
	s := Start(t, WithDevices(Device{Serial: "SERQUIET", Devpath: devpath}))

	left := make(chan struct{})
	s.RespondStream(devpath, "screen:", func(sess *StreamSession) error {
		// Not one read, not one write after the first: only Done can end
		// this handler.
		if _, err := sess.Write([]byte("HI")); err != nil {
			return err
		}
		<-sess.Done
		close(left)
		return nil
	})

	w := streamWire(t, s, devpath, "screen:live")
	if got := string(mustRead(t, w, 2, "the greeting")); got != "HI" {
		t.Fatalf("greeting = %q", got)
	}
	sever(w.c)

	select {
	case <-left:
	case <-time.After(20 * time.Second):
		t.Fatalf("the handler is still parked after the client hung up")
	}
}

// TestSeverAllEndsEveryParkedStream is the same property under the
// adb-server-restart shape: every socket in the farm dies at once and no
// handler is left holding one.
func TestSeverAllEndsEveryParkedStream(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-3.2"
	s := Start(t, WithDevices(Device{Serial: "SERMASS", Devpath: devpath}))

	parked := make(chan struct{}, 4)
	ended := make(chan struct{}, 4)
	s.RespondStream(devpath, "screen:", func(sess *StreamSession) error {
		parked <- struct{}{}
		<-sess.Done
		ended <- struct{}{}
		return nil
	})

	for i := 0; i < 4; i++ {
		streamWire(t, s, devpath, "screen:live")
		<-parked
	}
	if got := s.SeverAll(); got != 4 {
		t.Fatalf("SeverAll severed %d connections, want 4", got)
	}
	for i := 0; i < 4; i++ {
		select {
		case <-ended:
		case <-time.After(20 * time.Second):
			t.Fatalf("%d of 4 handlers ended after SeverAll", i)
		}
	}
	if got := s.Stats().Streams; got != 0 {
		t.Fatalf("Stats().Streams = %d after SeverAll, want 0", got)
	}
}

// TestTheDrainKeepsReadingWhileAHandlerIsBusy proves the receive buffer does
// its job: a client that writes and then hangs up must still hand the handler
// everything it wrote, because a control message dropped because the handler
// was mid-frame would be an input bug that is not in the input code.
func TestTheDrainKeepsReadingWhileAHandlerIsBusy(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-3.3"
	s := Start(t, WithDevices(Device{Serial: "SERDRAIN", Devpath: devpath}))

	body := bytes.Repeat([]byte("0123456789"), 4096) // 40 KiB, several reads
	got := make(chan []byte, 1)
	start := make(chan struct{})
	s.RespondStream(devpath, "control:", func(sess *StreamSession) error {
		// Busy while the client writes and leaves.
		<-start
		b, err := io.ReadAll(sess)
		if err != nil {
			return err
		}
		got <- b
		return nil
	})

	w := streamWire(t, s, devpath, "control:v1")
	if _, err := w.c.Write(body); err != nil {
		t.Fatalf("writing: %v", err)
	}
	// A clean close, deliberately: a reset discards whatever the kernel had
	// not delivered yet, so severing here would be testing the stack's
	// discard rules rather than the buffer's.
	if err := w.c.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	close(start)

	select {
	case b := <-got:
		if !bytes.Equal(b, body) {
			t.Fatalf("the handler read %d bytes, want %d", len(b), len(body))
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the handler never finished reading")
	}
}

// streamAll opens one device service and reads it to the end.
func streamAll(tb testing.TB, s *Server, devpath, service string) string {
	tb.Helper()
	w := streamWire(tb, s, devpath, service)
	out, err := io.ReadAll(w.br)
	if err != nil {
		tb.Fatalf("reading %q from %s: %v", service, devpath, err)
	}
	return string(out)
}

// lastReply returns the recorded reply of the most recent request for a
// service.
func lastReply(tb testing.TB, s *Server, service string) string {
	tb.Helper()
	reqs := s.Requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Service == service {
			return reqs[i].Reply
		}
	}
	tb.Fatalf("no request for %q was recorded: %+v", service, reqs)
	return ""
}

// TestStreamBufDrainsBeforeReportingTheEnd pins the buffer's own rule
// directly, because the alternative — reporting the end as soon as the source
// is finished — loses whatever was still in flight, and loses it only under a
// race that no assertion above would reproduce reliably.
func TestStreamBufDrainsBeforeReportingTheEnd(t *testing.T) {
	t.Parallel()

	b := newStreamBuf(64)
	if _, err := b.Write([]byte("tail")); err != nil {
		t.Fatalf("write: %v", err)
	}
	b.closeWith(nil)

	out, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("read after close: %v", err)
	}
	if string(out) != "tail" {
		t.Fatalf("read %q after the source ended, want %q", out, "tail")
	}

	// And a closed buffer reports the ordinary end of a stream rather than
	// success with nothing to show for it.
	var one [1]byte
	if _, err := b.Read(one[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("read on a drained buffer = %v, want io.EOF", err)
	}
}

// TestCloseUnblocksAHandlerWhoseInputNobodyIsReading is the deadlock the
// bound would otherwise introduce, and the one failure mode a harness must
// not have: a hung Close hangs CI, and the stack trace it eventually prints
// points at a condition variable rather than at the test that wedged it.
//
// A handler that writes and never reads is the ordinary shape of a screen
// stream. Give it a client that talks anyway — a control socket wired to the
// wrong service, say — and the session's drain fills its buffer and parks on
// a condition only that handler could satisfy. Nothing in the socket layer
// can wake it, so Close has to.
func TestCloseUnblocksAHandlerWhoseInputNobodyIsReading(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-4.1"
	s := Start(t, WithDevices(Device{Serial: "SERFULL", Devpath: devpath}))

	parked := make(chan struct{})
	s.RespondStream(devpath, "control:", func(sess *StreamSession) error {
		close(parked)
		<-sess.Done // not one read, ever
		return nil
	})

	w := streamWire(t, s, devpath, "control:v1")
	<-parked

	var written atomic.Int64
	go func() {
		chunk := make([]byte, 64<<10)
		for i := 0; i < (streamReadBuffer/len(chunk))+16; i++ {
			n, err := w.c.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()

	// Wait for the client to stop making progress, which is what "the
	// buffer, and every socket buffer behind it, is full" looks like from
	// out here. Without this the test would still pass and would prove
	// nothing, so it is a precondition rather than a delay.
	if !stalled(&written, 100*time.Millisecond, 20*time.Second) {
		t.Fatalf("the client never stopped making progress (%d bytes); "+
			"the receive buffer was never filled and this test proved nothing", written.Load())
	}
	if got := written.Load(); got < streamReadBuffer {
		t.Fatalf("only %d bytes reached the server before it stalled, want at least the %d-byte buffer",
			got, streamReadBuffer)
	}

	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("Close did not return: the drain is parked on a buffer only the handler could empty")
	}
}

// stalled reports whether n stopped changing for quiet, within budget.
func stalled(n *atomic.Int64, quiet, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	last := n.Load()
	still := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
		if got := n.Load(); got != last {
			last, still = got, time.Now()
			continue
		}
		if time.Since(still) >= quiet {
			return true
		}
	}
	return false
}

// TestClosingAFullBufferWakesItsWriter is the same guard one layer down,
// where it can be stated without a socket in the way.
func TestClosingAFullBufferWakesItsWriter(t *testing.T) {
	t.Parallel()

	b := newStreamBuf(8)
	if _, err := b.Write([]byte("01234567")); err != nil {
		t.Fatalf("filling the buffer: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := b.Write([]byte("8"))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a write into a full buffer returned early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	b.closeWith(errServerClosed)
	select {
	case err := <-done:
		if !errors.Is(err, errServerClosed) {
			t.Fatalf("the woken write returned %v, want the close's own reason", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("closing a full buffer did not wake the writer parked on it")
	}
}

// TestStreamBufStallsRatherThanDropping is the bound's contract. A test that
// pushes more at an idle handler than the buffer holds must be slowed down,
// never quietly shortened.
func TestStreamBufStallsRatherThanDropping(t *testing.T) {
	t.Parallel()

	b := newStreamBuf(8)
	blocked := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(blocked)
		_, err := b.Write([]byte("0123456789abcdef"))
		done <- err
	}()
	<-blocked

	select {
	case err := <-done:
		t.Fatalf("a 16-byte write into an 8-byte buffer returned early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	out := make([]byte, 16)
	n, err := io.ReadFull(b, out)
	if err != nil {
		t.Fatalf("reading the stalled write: %v (%d bytes)", err, n)
	}
	if string(out) != "0123456789abcdef" {
		t.Fatalf("read %q, want the whole write", out)
	}
	if err := <-done; err != nil {
		t.Fatalf("the stalled write ended with %v", err)
	}
}
