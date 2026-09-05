package jobrunner

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/lease"
	"github.com/flaviopadilha/device-farmer/internal/runner"
)

// The witness, wired: for every placement this loop builds the on-device
// marker on the job's device connection and hands it to the holder's witness
// loop. The property under test is that the wiring exists and that nothing
// about it can end a lease — a witness that cannot land, a marker that cannot
// be built, a device that will not take the delete, none of them touch the
// holder — and that nothing about it makes the placement wait.

// fakeConn is runner.Conn without a phone. It records every shell command and,
// unless told otherwise, answers exit 0, which is what a device that accepted
// the marker write says.
type fakeConn struct {
	mu    sync.Mutex
	cmds  []string
	shell func(ctx context.Context, command string) (runner.ShellOutput, error)
}

func (f *fakeConn) Shell(ctx context.Context, command string) (runner.ShellOutput, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, command)
	fn := f.shell
	f.mu.Unlock()
	if fn == nil {
		return runner.ShellOutput{Exited: true}, nil
	}
	return fn(ctx, command)
}

func (f *fakeConn) Push(_ context.Context, r io.Reader, _ string, _ fs.FileMode) error {
	_, _ = io.Copy(io.Discard, r)
	return nil
}

func (f *fakeConn) Pull(context.Context, string, io.Writer) error { return nil }

func (f *fakeConn) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

// count returns how many recorded commands contain substr.
func (f *fakeConn) count(substr string) int {
	n := 0
	for _, c := range f.commands() {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// refusing answers every delete with a non-zero exit and everything else with
// exit 0.
func refusing(cmd string) (runner.ShellOutput, error) {
	if strings.Contains(cmd, "rm -f") {
		return runner.ShellOutput{ExitCode: 17, Exited: true}, nil
	}
	return runner.ShellOutput{Exited: true}, nil
}

// newerHolders answers the first write with another holder's marker at a
// higher fence on this device — the one thing that silences a marker for
// good — and everything after it with exit 0.
func newerHolders() func(context.Context, string) (runner.ShellOutput, error) {
	var once sync.Once
	return func(_ context.Context, _ string) (runner.ShellOutput, error) {
		out := runner.ShellOutput{Exited: true}
		once.Do(func() {
			out = runner.ShellOutput{
				Stdout:   []byte("lease=99999999-9999-4999-8999-999999999999\nfence=6\njob=job-9\ndevice=dev-1\n"),
				ExitCode: 17, Exited: true,
			}
		})
		return out, nil
	}
}

// unanswering is a device that never takes a write.
func unanswering(context.Context, string) (runner.ShellOutput, error) {
	return runner.ShellOutput{}, errors.New("device offline")
}

func witnessPlacement() runner.Placement {
	return runner.Placement{
		JobID: "job-1", LeaseID: "11111111-1111-4111-8111-111111111111", Fence: 5,
		DeviceID: "dev-1", Devpath: "usb:3-1.4", Endpoint: "127.0.0.1:5037",
	}
}

func fastWitness(c *Config) {
	c.WitnessConfig.Interval = 2 * time.Millisecond
	c.MarkerConfig.Interval = time.Millisecond
	c.MarkerConfig.Timeout = time.Second
	c.MarkerConfig.RemoveTimeout = time.Second
}

// witnessFixture is one placement's witness over a blind holder and a fake
// device, on the fast test cadence.
type witnessFixture struct {
	jr   *JobRunner
	logs *logCapture
	h    *lease.Holder
	dev  *fakeConn
	wit  *witness
}

// startTestWitness starts a witness for an ordinary placement. dev may be
// nil for a device that accepts everything; mutate may be nil.
func startTestWitness(t *testing.T, dev *fakeConn, mutate func(*runner.Placement)) witnessFixture {
	t.Helper()
	jr, logs := testLoop(t, fastWitness)
	if dev == nil {
		dev = &fakeConn{}
	}
	p := witnessPlacement()
	if mutate != nil {
		mutate(&p)
	}
	f := witnessFixture{jr: jr, logs: logs, h: blindHolder(t), dev: dev}
	f.wit = jr.startWitness(f.h, dev, p, jr.log)
	t.Cleanup(f.wit.stop)
	return f
}

// settled waits for the marker goroutine to be gone and the summary written,
// so a test can count commands and lines without racing either.
func (f witnessFixture) settled(t *testing.T) {
	t.Helper()
	for _, ch := range []<-chan struct{}{f.wit.done, f.wit.reported} {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatal("the witness did not settle after being stopped")
		}
	}
}

// levels returns the level of every captured line containing substr.
func (c *logCapture) levels(substr string) []slog.Level {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []slog.Level
	for _, r := range c.recs {
		if strings.Contains(r.Message, substr) {
			out = append(out, r.Level)
		}
	}
	return out
}

// The marker is written to the device, the loop stands on it and presents a
// witness, and a witness that cannot reach the database ends nothing.
func TestEveryPlacementStartsAMarkerAndAWitness(t *testing.T) {
	t.Parallel()

	f := startTestWitness(t, nil, nil)
	if f.wit == nil {
		t.Fatal("no witness was started for an ordinary placement")
	}

	waitFor(t, "the marker to be refreshed on its cadence", func() bool {
		return f.dev.count(runner.MarkerPath) >= 3
	})
	// What is on the device names THIS lease at THIS fence, so a marker left
	// behind by a previous holder can be told from ours.
	if f.dev.count("lease=11111111-1111-4111-8111-111111111111") == 0 || f.dev.count("fence=5") == 0 {
		t.Fatalf("the marker does not name the lease and the fence:\n%s", strings.Join(f.dev.commands(), "\n"))
	}

	// The loop presented on the marker's evidence. The database is not there,
	// so every presentation fails on the wire — and that is exactly the
	// failure that must cost nothing.
	waitFor(t, "a witness to be presented and fail on the wire", func() bool {
		return f.wit.loop.Stats().Errors >= 1
	})
	if f.h.Fenced() {
		t.Fatal("a witness that could not reach the database was reported as fencing")
	}
	if err := f.h.Context().Err(); err != nil {
		t.Fatalf("a witness that could not reach the database cancelled the job: %v", context.Cause(f.h.Context()))
	}

	// Stopping is quiet and complete: no refresh and no witness after it.
	f.wit.stop()
	f.wit.stop() // idempotent
	f.settled(t)
	n := len(f.dev.commands())
	presented := f.wit.loop.Stats().Presented
	time.Sleep(10 * time.Millisecond)
	if len(f.dev.commands()) != n {
		t.Fatal("the marker was refreshed after the witness was stopped")
	}
	if f.wit.loop.Stats().Presented != presented {
		t.Fatal("a witness was presented after the loop was stopped")
	}
	if f.logs.count("witness stopped") != 1 {
		t.Fatalf("the witness summary was written %d times, want once", f.logs.count("witness stopped"))
	}
	// A clean stop cut the last refresh short, and that is not a failure of
	// the device's: the summary of a placement whose evidence landed every
	// time must say so.
	if st := f.wit.marker.Stats(); st.Failures != 0 {
		t.Fatalf("Stats = %+v; stopping the witness was counted as the marker failing", st)
	}
	// And the lease is still the holder's to keep or give back.
	if f.h.Fenced() || f.h.Context().Err() != nil {
		t.Fatal("stopping the witness ended the holder")
	}
}

// A placement whose marker cannot be constructed still runs. The witness is
// protection a job may lack, never a precondition for running it.
func TestAPlacementWithoutAMarkerStillRuns(t *testing.T) {
	t.Parallel()

	f := startTestWitness(t, nil, func(p *runner.Placement) { p.LeaseID = "" })
	if f.wit != nil {
		t.Fatal("a witness was started with no lease id to write into the marker")
	}
	if f.logs.count("no on-device lease marker") != 1 {
		t.Fatal("running without a witness was not said out loud")
	}
	if len(f.dev.commands()) != 0 {
		t.Fatalf("the device was written to without a marker: %v", f.dev.commands())
	}

	// The nil witness is what runJob holds for this placement; every call on
	// it must be a no-op, so the placement's own unwinding is unchanged.
	f.wit.stop()
	f.wit.remove(context.Background())
	if f.h.Fenced() || f.h.Context().Err() != nil {
		t.Fatal("a placement without a witness lost its lease")
	}
}

// Stopping the witness waits for nothing. The placement's unwinding — the
// holder's stop, the release, the return of runJob on SIGTERM — must not sit
// behind a round trip to the device or the database.
//
// Falsify: make stop block on <-w.done.
func TestStoppingTheWitnessWaitsForNothing(t *testing.T) {
	t.Parallel()

	// A device that ignores cancellation and answers when the test says so:
	// the worst a wire can do to a caller that is waiting on it. Released on
	// the way out whatever happens, so a stop that DOES block cannot wedge
	// the cleanup behind it as well.
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	var wedgedOnce sync.Once
	dev := &fakeConn{shell: func(_ context.Context, _ string) (runner.ShellOutput, error) {
		wedged := false
		wedgedOnce.Do(func() { wedged = true })
		if wedged {
			<-release
		}
		return runner.ShellOutput{Exited: true}, nil
	}}
	f := startTestWitness(t, dev, nil)
	waitFor(t, "the first refresh to be in flight", func() bool { return len(dev.commands()) >= 1 })

	stopped := make(chan struct{})
	go func() { defer close(stopped); f.wit.stop() }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop waited on a refresh that was in flight; the placement's unwinding was behind the device")
	}
	select {
	case <-f.wit.done:
		t.Fatal("the marker goroutine was gone while its refresh was still wedged; the test proved nothing")
	default:
	}

	releaseOnce()
	f.settled(t)
	if f.logs.count("witness stopped") != 1 {
		t.Fatalf("the summary was written %d times once the goroutines were gone, want once",
			f.logs.count("witness stopped"))
	}
}

// The marker is deleted only once nothing can write it back, only when there
// is something of ours to delete, and a device that refuses the delete costs
// nothing.
func TestTheMarkerIsRemovedOnlyAfterTheWitnessIsSilent(t *testing.T) {
	t.Parallel()

	t.Run("the device takes the delete", func(t *testing.T) {
		f := startTestWitness(t, nil, nil)
		waitFor(t, "evidence", func() bool { _, ok := f.wit.marker.WitnessedAt(); return ok })

		f.wit.remove(context.Background())
		cmds := f.dev.commands()
		last := cmds[len(cmds)-1]
		if !strings.Contains(last, "rm -f") || !strings.Contains(last, runner.MarkerPath) {
			t.Fatalf("the last command on the device is not the delete:\n%s", last)
		}
		if _, ok := f.wit.marker.WitnessedAt(); ok {
			t.Fatal("evidence is still presented for a marker this placement deleted")
		}
		time.Sleep(10 * time.Millisecond)
		if got := f.dev.commands(); len(got) != len(cmds) {
			t.Fatalf("the marker was written again after it was removed:\n%s", got[len(got)-1])
		}
		if f.logs.count("could not remove") != 0 {
			t.Fatal("a delete the device accepted was logged as a failure")
		}
	})

	t.Run("the device refuses", func(t *testing.T) {
		f := startTestWitness(t, &fakeConn{shell: func(_ context.Context, cmd string) (runner.ShellOutput, error) {
			return refusing(cmd)
		}}, nil)
		waitFor(t, "evidence", func() bool { _, ok := f.wit.marker.WitnessedAt(); return ok })

		f.wit.remove(context.Background())
		if f.logs.count("could not remove the on-device lease marker") != 1 {
			t.Fatal("a refused delete was not logged")
		}
		if f.h.Fenced() || f.h.Context().Err() != nil {
			t.Fatal("a refused delete ended the holder")
		}
	})

	// Falsify the two below: delete unconditionally in remove.
	t.Run("a newer holder's marker is left alone", func(t *testing.T) {
		f := startTestWitness(t, &fakeConn{shell: newerHolders()}, nil)
		waitFor(t, "the marker to find the newer holder", func() bool { return f.wit.marker.Stats().Superseded })

		f.wit.remove(context.Background())
		if n := f.dev.count("rm -f"); n != 0 {
			t.Fatalf("%d delete(s) were sent to a device that belongs to a newer holder", n)
		}
		if f.logs.count("it is a newer holder's, not ours") != 1 {
			t.Fatal("leaving the newer holder's marker alone was not said")
		}
		f.settled(t)
		// The marker warned once when it found the file; the summary must
		// not warn a second time about the same thing.
		if got := f.logs.levels("witness stopped"); len(got) != 1 || got[0] != slog.LevelInfo {
			t.Fatalf("summary levels = %v, want one Info line; the marker already warned", got)
		}
	})

	t.Run("a marker that never landed is not deleted", func(t *testing.T) {
		f := startTestWitness(t, &fakeConn{shell: unanswering}, nil)
		waitFor(t, "three refreshes to fail", func() bool { return f.wit.marker.Stats().Failures >= 3 })

		start := time.Now()
		f.wit.remove(context.Background())
		if took := time.Since(start); took > time.Second {
			t.Fatalf("remove took %s against a device that never took a write; the release was waiting", took)
		}
		if n := f.dev.count("rm -f"); n != 0 {
			t.Fatalf("%d delete(s) were sent to a device on which no write ever landed", n)
		}
		if f.logs.count("no write of ours ever landed") != 1 {
			t.Fatal("skipping the delete was not said")
		}
	})
}

// The three cadences are one rule. The marker is rewritten several times per
// witness tick, the evidence window is measured in marker intervals, and
// what the jobrunner derives is what config.Summary prints.
//
// Falsify: derive MaxEvidenceAge from the witness interval.
func TestTheWitnessTripleIsDerivedFromOneRule(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		witness time.Duration
	}{
		{"the default cadence", 0},
		{"a tightened cadence", 40 * time.Second},
		{"a relaxed cadence", 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{WitnessConfig: lease.WitnessConfig{Interval: tc.witness}}
			c.applyDefaults()

			want := config.Lease{WitnessInterval: tc.witness}
			if tc.witness == 0 {
				want.WitnessInterval = lease.DefaultWitnessInterval
			}
			if c.WitnessConfig.Interval != want.WitnessInterval {
				t.Fatalf("WitnessConfig.Interval = %s, want %s", c.WitnessConfig.Interval, want.WitnessInterval)
			}
			if c.MarkerConfig.Interval != want.MarkerInterval() {
				t.Fatalf("MarkerConfig.Interval = %s, want %s: the marker cadence is not the one config prints",
					c.MarkerConfig.Interval, want.MarkerInterval())
			}
			if c.WitnessConfig.MaxEvidenceAge != want.MaxEvidenceAge() {
				t.Fatalf("MaxEvidenceAge = %s, want %s: the evidence window is not the one config prints",
					c.WitnessConfig.MaxEvidenceAge, want.MaxEvidenceAge())
			}
			// The window is shorter than a witness tick, whatever the cadence:
			// a device nobody has reached since the last tick never reads as
			// demonstrably alive on this one.
			if c.WitnessConfig.MaxEvidenceAge >= c.WitnessConfig.Interval {
				t.Fatalf("MaxEvidenceAge %s is not below the witness interval %s",
					c.WitnessConfig.MaxEvidenceAge, c.WitnessConfig.Interval)
			}
			if c.MarkerConfig.Timeout > c.MarkerConfig.Interval {
				t.Fatalf("MarkerConfig.Timeout %s outlives the interval %s", c.MarkerConfig.Timeout, c.MarkerConfig.Interval)
			}
		})
	}

	t.Run("the default marker cadence is the default witness cadence divided by the ratio", func(t *testing.T) {
		var c Config
		c.applyDefaults()
		if c.MarkerConfig.Interval != runner.DefaultMarkerInterval {
			t.Fatalf("MarkerConfig.Interval = %s under the default witness cadence, want runner.DefaultMarkerInterval %s",
				c.MarkerConfig.Interval, runner.DefaultMarkerInterval)
		}
	})

	t.Run("an explicit marker cadence is kept and the window follows it", func(t *testing.T) {
		c := Config{
			WitnessConfig: lease.WitnessConfig{Interval: 2 * time.Minute},
			MarkerConfig:  runner.MarkerConfig{Interval: 10 * time.Second},
		}
		c.applyDefaults()
		if c.MarkerConfig.Interval != 10*time.Second {
			t.Fatalf("an explicit marker cadence was overridden: %s", c.MarkerConfig.Interval)
		}
		if want := config.MaxEvidenceAgeFor(10 * time.Second); c.WitnessConfig.MaxEvidenceAge != want {
			t.Fatalf("MaxEvidenceAge = %s, want %s: the window must follow the marker that is actually running",
				c.WitnessConfig.MaxEvidenceAge, want)
		}
	})
}

// A triple set by hand that the rule would not have produced is refused at
// construction, not discovered as a witness that is skipped on every tick.
//
// Falsify: drop the validateWitness call from New.
func TestNewRefusesAWitnessTripleThatCannotWork(t *testing.T) {
	t.Parallel()

	pool := unreachablePool(t)
	base := func() Config {
		return Config{
			Pool: pool, Store: lease.NewStore(pool), Runner: stubExecutor{},
			Holder: "farmd-0", HolderInstance: "22222222-2222-4222-8222-222222222222",
			Logger: quietLogger(),
		}
	}
	for _, tc := range []struct {
		name string
		set  func(*Config)
		want string
	}{
		{
			"marker slower than the evidence window",
			func(c *Config) {
				c.MarkerConfig.Interval = time.Minute
				c.WitnessConfig.MaxEvidenceAge = time.Minute
			},
			"every witness would be skipped",
		},
		{
			"marker slower than the witness",
			func(c *Config) {
				c.WitnessConfig.Interval = time.Minute
				c.MarkerConfig.Interval = 90 * time.Second
			},
			"at least once per witness tick",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.set(&c)
			_, err := New(c)
			if err == nil {
				t.Fatal("New accepted a witness triple under which no witness could ever be presented")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to say %q", err, tc.want)
			}
		})
	}

	// And the guard against overcorrecting: a coherent explicit triple, and
	// the derived one, are both fine.
	c := base()
	c.WitnessConfig.Interval = time.Minute
	c.MarkerConfig.Interval = 15 * time.Second
	c.WitnessConfig.MaxEvidenceAge = 45 * time.Second
	if _, err := New(c); err != nil {
		t.Fatalf("New refused a coherent triple: %v", err)
	}
	if _, err := New(base()); err != nil {
		t.Fatalf("New refused the derived triple: %v", err)
	}
}
