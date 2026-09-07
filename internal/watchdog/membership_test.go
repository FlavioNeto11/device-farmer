package watchdog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a log sink several goroutines write to at once: the timed cycle,
// the battery poller and one reader per host all share the Watchdog's logger.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitUntil polls a predicate rather than sleeping a guessed interval, so the
// test is as fast as the loop is and does not fail on a slow machine.
func waitUntil(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, what)
}

// TestAnEmptyHostListIsNotAnError is the assertion the watchdog role's crash
// loop was missing.
//
// A farm whose schema has just been migrated has an EMPTY farm.hosts. Hosts
// arrive by enrolment, from a `farmd node` started minutes, days or a rack
// delivery later, so "no hosts to watch" is the normal first state of a new
// installation rather than a misconfiguration. cmd/farmd used to read it as one:
// it enumerated farm.hosts once at startup and returned "no hosts registered;
// set FARM_HOST_ID or seed farm.hosts", which made the farm profile in
// docker-compose.yml restart this role forever on every fresh install — 14 hours
// and a few thousand restarts on the machine that found it — and, because a
// process that is not running writes no farm.component_heartbeat row, made
// /api/v1/capabilities report the health plane dead for as long as the farm had
// no phones in it.
//
// This loop is what that fatal was standing in front of, and it already did the
// right thing. The two halves below are the whole of it:
//
//	phase 1  an empty list is a cycle that found nothing. Run keeps running,
//	         keeps beating, and says so once.
//	phase 2  a host enrolled while the loop is running is adopted within one
//	         Interval, with no restart — which is what makes waiting strictly
//	         better than exiting, rather than merely quieter.
//
// Break either half and the role that depends on it goes back to being either a
// crash loop or a process that supervises nothing forever.
//
// The host id is unique per test and does not exist when Run starts, so the host
// list this watchdog sees is empty whatever else the shared scratch database
// holds. That is the same zero-row path the fleet-wide shape takes on a farm
// with no hosts at all; cmd/farmd's TestWatchdogConfigTakesItsShapeFromTheHostID
// covers which of the two shapes a process chooses.
func TestAnEmptyHostListIsNotAnError(t *testing.T) {
	pool := requireDB(t)

	const host = "empty-host-list-test"

	// Phase 1 asserts about an EMPTY host list, so the row phase 2 inserts must
	// not survive into another run. The scratch database is created fresh per
	// `go test` invocation, but a run killed between the insert and t.Cleanup —
	// Ctrl-C, a -timeout kill, a panic elsewhere in the package — leaves it
	// behind in a database somebody reused, and every later run would then fail
	// in phase 1 for a reason that has nothing to do with the code. Removed
	// here as well as in Cleanup so the test repairs whatever it finds.
	drop := func() {
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM farm.hosts WHERE id = $1`, host); err != nil {
			t.Fatalf("clear the test host: %v", err)
		}
	}
	drop()
	t.Cleanup(drop)

	var buf syncBuffer
	w, err := New(Config{
		Pool:      pool,
		Component: "watchdog-test:" + host,
		HostID:    host,
		// Fast, so the test waits on the loop rather than the other way round.
		// Resync is left at its default: nothing is ever observed here.
		Interval:    100 * time.Millisecond,
		CallTimeout: 5 * time.Second,
		Logger:      slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Phase 1. Nothing to watch, and the loop is alive anyway.
	waitUntil(t, 10*time.Second, "the loop to report that it has no hosts yet", func() bool {
		return strings.Contains(buf.String(), "no hosts to watch yet")
	})
	select {
	case err := <-done:
		t.Fatalf("Run returned %v on a farm with no matching host; an empty farm.hosts is "+
			"what a freshly migrated installation looks like, and a role that exits over it "+
			"restarts forever and beats never", err)
	default:
	}

	// The heartbeat is the operator-visible half: /api/v1/capabilities reads
	// farm.component_heartbeat, so a health plane that is idle must be
	// distinguishable there from one that is gone.
	waitUntil(t, 10*time.Second, "a farm.component_heartbeat row for an idle watchdog", func() bool {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM farm.component_heartbeat WHERE component = $1`,
			"watchdog-test:"+host).Scan(&n); err != nil {
			return false
		}
		return n == 1
	})

	// Phase 2. Enrolment, while the loop runs. 127.0.0.1:1 is deliberately a
	// port nothing listens on: this test is about MEMBERSHIP, and a reader that
	// cannot connect still has to be started — adbwire reconnects on its own and
	// publishes nothing in the meantime, which is the behaviour that keeps a
	// dropped socket from being read as an empty device list.
	if _, err := pool.Exec(ctx,
		`INSERT INTO farm.hosts (id, adb_endpoint) VALUES ($1, '127.0.0.1:1')`, host); err != nil {
		t.Fatalf("enrol a host: %v", err)
	}

	waitUntil(t, 10*time.Second, "the host enrolled after startup to be adopted", func() bool {
		return strings.Contains(buf.String(), "started host reader")
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v on cancellation, want nil: a SIGTERM is an orderly stop "+
			"for a daemon loop, and this one holds no lease to lose", err)
	}
}
