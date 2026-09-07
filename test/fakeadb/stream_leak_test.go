package fakeadb

import (
	"fmt"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// What a session leaves behind
// ---------------------------------------------------------------------

// TestASessionWhoseHandlerReturnsLeavesNoGoroutineBehind is the leak the rest of
// this package could not see.
//
// runStream starts two goroutines per session: a drain reading the socket into
// the receive buffer, and a watcher turning "the peer went away or the server
// closed" into the session's Done. When the handler returns while that buffer is
// FULL, the drain is parked in streamBuf.Write on a condition variable — not on
// the socket — so closing the connection wakes nothing at all. The watcher then
// waits on a channel the drain will never close, and the pair of them keep a
// megabyte of buffer alive between them for the life of the process.
//
// It was invisible here because Server.Close is every test's cleanup and Close
// closes s.done, which the watcher turns into a buffer close: every leak this
// package made was collected by the end of the test that made it. internal/demo
// keeps ONE server for the whole life of the process, which is where the same
// code leaks two goroutines and a megabyte per screen session and nothing ever
// collects them.
//
// NOT PARALLEL, on purpose. The assertion is about a process-wide goroutine
// count, and Go resumes its parallel tests only after the sequential ones have
// finished — so running this one sequentially is what makes the count quiet
// enough to assert on at all. A t.Parallel() here would be measuring every other
// test in the package.
//
// Falsify: remove the `defer buf.closeWith(errHandlerReturned)` from runStream.
func TestASessionWhoseHandlerReturnsLeavesNoGoroutineBehind(t *testing.T) {
	const (
		devpath    = "usb:7-5.1"
		iterations = 50

		// Three times the buffer, so the drain must park: it cannot put this
		// much into a megabyte without blocking, and "the drain is parked on a
		// full buffer when the handler returns" is the entire precondition of
		// the leak.
		clientWrites = 3 * streamReadBuffer

		// Slack, because the number being compared is the whole process's
		// goroutine count and some of it is not this test's: the runtime's GC
		// workers and netpoller come and go, the server's own accept loop is in
		// there, and this iteration's client writer has usually noticed its
		// socket is gone but has not always been scheduled to return yet. Eight
		// is far above that noise and far below the growth being looked for,
		// which is two per session and reaches a hundred over the loop.
		slack = 8
	)
	s := Start(t, WithDevices(Device{Serial: "SERLEAK", Devpath: devpath}))

	// A handler that never reads, and returns the moment its buffer is full.
	// Never reading is the ordinary shape of a screen stream; a client that
	// writes at one anyway is the ordinary shape of a control socket pointed at
	// the wrong service. Waiting for the buffer to actually fill is what makes
	// each iteration reproduce the leak rather than merely resemble it.
	s.RespondStream(devpath, "control:", func(sess *StreamSession) error {
		buf := sess.Reader.(*streamBuf)
		for !bufIsFull(buf) {
			select {
			case <-sess.Done:
				return nil
			case <-time.After(time.Millisecond):
			}
		}
		return nil
	})

	baseline := 0
	for i := 0; i < iterations; i++ {
		w := streamWire(t, s, devpath, "control:v1")

		var written atomic.Int64
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			chunk := make([]byte, 64<<10)
			for sent := 0; sent < clientWrites; sent += len(chunk) {
				n, err := w.c.Write(chunk)
				written.Add(int64(n))
				if err != nil {
					// The handler returned and the connection went with it,
					// which is this iteration finishing rather than failing.
					return
				}
			}
		}()

		waitFor(t, func() bool { return s.Stats().Streams == 0 },
			func() string {
				return fmt.Sprintf("iteration %d: the handler never returned; %d bytes reached the "+
					"server, and the buffer it waits to fill is %d", i, written.Load(), streamReadBuffer)
			})
		<-writerDone
		if got := written.Load(); got < streamReadBuffer {
			t.Fatalf("iteration %d: only %d bytes reached the server before its handler returned, "+
				"want at least the %d-byte buffer — the drain was never parked on a full buffer and "+
				"this iteration did not reproduce the leak it is looking for",
				i, got, streamReadBuffer)
		}

		if i == 0 {
			// The baseline is taken after the first session rather than before
			// it, so that whatever the runtime and the server allocate on first
			// use is counted as the floor and not as growth.
			if !goroutinesSettle(runtime.NumGoroutine(), 10*time.Second) {
				t.Fatalf("the goroutine count never stopped moving after one session (%d)",
					runtime.NumGoroutine())
			}
			baseline = runtime.NumGoroutine()
			continue
		}
		if !goroutinesSettle(baseline+slack, 10*time.Second) {
			t.Fatalf("after %d sessions this process is holding %d goroutines against the %d it "+
				"held after one (+%d slack). Each session that ended with a full receive buffer "+
				"left its drain parked on a condition variable and its Done watcher parked on the "+
				"channel that drain will never close — two goroutines and a megabyte of buffer, per "+
				"screen session, for the life of the process",
				i+1, runtime.NumGoroutine(), baseline, slack)
		}
	}
}

// bufIsFull reports whether a receive buffer is at its bound, which is the state
// in which its writer is parked on a condition only the handler could satisfy.
// Under the buffer's own lock, because pending() is not safe without it.
func bufIsFull(b *streamBuf) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pending() >= b.max
}

// goroutinesSettle waits for the process's goroutine count to come down to limit.
//
// A wait rather than a reading, because a goroutine that has been woken is not a
// goroutine that has already returned: the drain has to come off its condition
// variable, finish its copy and hand the count back, and none of that is
// observable from here. The budget is what separates "has not got there yet" from
// "is never getting there".
func goroutinesSettle(limit int, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if runtime.NumGoroutine() <= limit {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------
// A fault that is not a failure
// ---------------------------------------------------------------------

// TestASlowButCorrectReplyStillRunsItsStreamHandler pins the half of
// deviceStream's fault guard that nothing pinned.
//
// The guard reads `!finish(...) || (f != nil && f.Kind != FaultNone)`, and the two
// halves are there for different reasons. `!finish` catches a fault that killed
// the connection. The second half catches a fault that did NOT: a FaultFail
// leaves the connection alive, and handing the handler a socket the server has
// just refused would write a screen stream on top of a FAIL frame, which is not a
// shape any client is built to parse.
//
// What was untested is the exception inside that second half. FaultNone is a
// scripted rule that honours its Delay and changes nothing else — "a reply that
// is slow but perfectly correct", which is the case a naive timeout mistakes for
// death, and the only reason to script one is to watch a client handle it. A
// guard written `f != nil` would swallow the whole service instead: the client
// gets its OKAY, then silence, and the stream it was scripted to receive never
// starts. No test in this package would have noticed, because no test had ever
// injected a FaultNone on a duplex service.
//
// Falsify: change the guard in deviceStream to `(f != nil)`.
func TestASlowButCorrectReplyStillRunsItsStreamHandler(t *testing.T) {
	t.Parallel()

	const (
		devpath = "usb:7-6.1"
		service = "screen:live"
		frame   = "FRAME-1"
	)
	s := Start(t, WithDevices(Device{Serial: "SERSLOW", Devpath: devpath}))
	s.RespondStream(devpath, "screen:", func(sess *StreamSession) error {
		_, err := sess.Write([]byte(frame))
		return err
	})
	s.Inject(Fault{
		Match:   service,
		Devpath: devpath,
		Kind:    FaultNone,
		Delay:   20 * time.Millisecond,
	})

	w := streamWire(t, s, devpath, service)
	got, err := io.ReadAll(w.br)
	if err != nil {
		t.Fatalf("reading a stream behind a slow-but-correct fault: %v", err)
	}

	// The precondition first, because everything below it is only meaningful if
	// the rule actually fired. A rule that never matched would make this test
	// pass while asserting nothing about faults at all.
	if n := s.Stats().Faults; n != 1 {
		t.Fatalf("%d injected faults were applied, want exactly 1 — the FaultNone rule never "+
			"matched, so this test says nothing about what a matched one does", n)
	}
	if string(got) != frame {
		t.Fatalf("the stream behind a FaultNone rule delivered %q, want %q. FaultNone is a reply "+
			"that is slow and otherwise perfect — the case a naive timeout mistakes for death — so a "+
			"guard that treats the presence of a rule as a refusal swallows the one service the "+
			"rule was scripted to let through", got, frame)
	}
}
