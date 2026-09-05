package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/lease"
)

// The on-device marker: the evidence a witness stands on.
//
// Nothing in witness.go ends anything, and these tests are shaped by that.
// What they check is narrower and more consequential: evidence is presented
// only when the device acknowledged a write; a device that belongs to a newer
// fence silences the marker for good; and everything else — a wire failure,
// our own leftovers, junk at the path, a forged fence — is retried or
// replaced without a verdict of any kind.

func newTestMarker(t *testing.T, dev Conn, mutate func(*MarkerConfig)) *Marker {
	t.Helper()
	cfg := MarkerConfig{Interval: time.Millisecond, Timeout: time.Second, Logger: quietLogger()}
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := NewMarker(dev, placement(), cfg)
	if err != nil {
		t.Fatalf("NewMarker: %v", err)
	}
	return m
}

// foreign is the device's answer when the marker path holds a file at another
// fence: the file's content, and exit 17.
func foreign(content string) ShellOutput {
	return ShellOutput{Stdout: []byte(content), ExitCode: markerForeign, Exited: true}
}

func markerFile(leaseID string, fence int64, deviceID string) string {
	return fmt.Sprintf("lease=%s\nfence=%d\njob=job-0\ndevice=%s\ntouched=1\n", leaseID, fence, deviceID)
}

func waitMarker(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// levels returns the level of every captured line containing substr, in
// order: the marker's repeated-failure policy is a statement about levels.
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

// The marker's default cadence is the witness's default cadence divided by
// the ratio config owns, and not a number of its own.
func TestDefaultMarkerIntervalFollowsTheWitnessCadence(t *testing.T) {
	t.Parallel()
	if want := config.MarkerIntervalFor(lease.DefaultWitnessInterval); DefaultMarkerInterval != want {
		t.Fatalf("DefaultMarkerInterval = %s, want %s (lease.DefaultWitnessInterval / config.MarkersPerWitnessTick)",
			DefaultMarkerInterval, want)
	}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

// A marker that names no lease is indistinguishable from a previous holder's
// leftovers, and a field with a line break in it forges a second field. Both
// are refused before anything is written.
func TestNewMarkerRefusesEvidenceItCouldNotStandBehind(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		dev    Conn
		mutate func(*Placement)
		want   string
	}{
		{"no device connection", nil, nil, "device connection"},
		{"no lease id", &fakeConn{}, func(p *Placement) { p.LeaseID = "" }, "lease id"},
		{"no devpath", &fakeConn{}, func(p *Placement) { p.Devpath = "" }, "devpath"},
		{"no fence", &fakeConn{}, func(p *Placement) { p.Fence = 0 }, "fence"},
		{"a line break in the job id", &fakeConn{}, func(p *Placement) { p.JobID = "job\nfence=99" }, "line break"},
		{"a NUL in the device id", &fakeConn{}, func(p *Placement) { p.DeviceID = "dev\x00" }, "line break or NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := placement()
			if tc.mutate != nil {
				tc.mutate(&p)
			}
			m, err := NewMarker(tc.dev, p, MarkerConfig{})
			if err == nil {
				t.Fatalf("NewMarker accepted %+v", p)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if m != nil {
				t.Fatal("a refused marker was returned alongside the error")
			}
		})
	}

	t.Run("a marker that was never written presents nothing", func(t *testing.T) {
		m := newTestMarker(t, &fakeConn{}, nil)
		if at, ok := m.WitnessedAt(); ok || !at.IsZero() {
			t.Fatalf("WitnessedAt = %s, %v before any refresh; a witness would be presented for a "+
				"device this process has never touched", at, ok)
		}
	})
}

// ---------------------------------------------------------------------------
// Refresh: the write is the proof
// ---------------------------------------------------------------------------

func TestMarkerRefreshIsTheOnlyThingThatProducesEvidence(t *testing.T) {
	t.Parallel()

	dev := &fakeConn{}
	m := newTestMarker(t, dev, nil)

	before := time.Now()
	if err := m.Refresh(stepCtx(t)); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	at, ok := m.WitnessedAt()
	if !ok {
		t.Fatal("the device acknowledged the write and the marker still presents no evidence")
	}
	if at.Before(before) || at.After(time.Now()) {
		t.Fatalf("WitnessedAt = %s, want a reading taken during the refresh", at)
	}
	if st := m.Stats(); st.Refreshes != 1 || st.Failures != 0 || st.LastError != nil {
		t.Fatalf("Stats = %+v after one clean refresh", st)
	}

	// What went to the device: the marker names this lease and this fence,
	// is written to a scratch path first and renamed over the real one, and
	// refuses to overwrite a file at any other fence.
	cmd := dev.commands()[0]
	for _, want := range []string{
		`mkdir -p "` + MarkerDir + `"`,
		"lease=lease-1", "fence=42", "job=job-1", "device=dev-1",
		`"` + MarkerDir + `/lease.new.42"`,
		`mv -f "` + MarkerDir + `/lease.new.42" "` + MarkerPath + `"`,
		"exit " + fmt.Sprint(markerForeign),
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("refresh command lacks %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "-gt") {
		t.Errorf("an ordinary refresh used the replacing guard, which overwrites any lower fence:\n%s", cmd)
	}
}

// A wire failure leaves the previous refresh standing. The evidence ages, the
// witness loop decides for itself when it is too old to present, and nothing
// here concludes anything about the lease.
func TestMarkerWireFailureKeepsTheLastEvidenceAndEndsNothing(t *testing.T) {
	t.Parallel()

	dev := &fakeConn{shell: func(_ context.Context, call int, _ string) (ShellOutput, error) {
		switch call {
		case 1:
			return okShell(""), nil
		case 2:
			return ShellOutput{}, transportErr("read tcp 127.0.0.1:5037: connection reset by peer")
		case 3:
			return ShellOutput{Stdout: []byte("half")}, nil // the stream ended without an exit
		default:
			return ShellOutput{ExitCode: 1, Stderr: []byte("mv: Read-only file system\n"), Exited: true}, nil
		}
	}}
	m := newTestMarker(t, dev, nil)
	ctx := stepCtx(t)

	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	first, _ := m.WitnessedAt()

	for i, want := range []string{"connection reset", "without an exit status", "exit status 1: mv: Read-only"} {
		err := m.Refresh(ctx)
		if err == nil {
			t.Fatalf("refresh %d: a write the device did not acknowledge was reported as a success", i+2)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refresh %d: err = %v, want it to say %q", i+2, err, want)
		}
		if errors.Is(err, ErrMarkerSuperseded) {
			t.Fatalf("refresh %d: a wire failure was reported as supersession, which is permanent", i+2)
		}
		at, ok := m.WitnessedAt()
		if !ok || !at.Equal(first) {
			t.Fatalf("refresh %d: WitnessedAt = %s, %v; a failed write must leave the last successful "+
				"one standing so the witness loop can age it out on its own terms", i+2, at, ok)
		}
	}
	if st := m.Stats(); st.Refreshes != 1 || st.Failures != 3 || st.LastError == nil || st.Superseded {
		t.Fatalf("Stats = %+v after one success and three failures", st)
	}
}

// A refresh the supervisor cancelled is neither a success nor a failure. The
// job finishing and the supervisor stopping the marker while a write is in
// flight is the ordinary end of every placement, and it must not go on record
// — in the stats, in the metrics they feed, or in the log — as evidence that
// failed.
//
// Falsify: make Refresh count every error, cancelled or not.
func TestMarkerRefreshCutShortByItsCallerCountsAsNothing(t *testing.T) {
	t.Parallel()

	dev := &fakeConn{shell: func(ctx context.Context, call int, _ string) (ShellOutput, error) {
		if call == 1 {
			return okShell(""), nil
		}
		// The device is mid-write when the caller lets go.
		<-ctx.Done()
		return ShellOutput{}, ctx.Err()
	}}
	m := newTestMarker(t, dev, nil)
	if err := m.Refresh(stepCtx(t)); err != nil {
		t.Fatal(err)
	}
	first, _ := m.WitnessedAt()

	ctx, cancel := context.WithCancel(stepCtx(t))
	go func() {
		waitMarker(t, "the second write to be in flight", func() bool { return dev.calls() >= 2 })
		cancel()
	}()
	err := m.Refresh(ctx)
	if err == nil {
		t.Fatal("a write that never landed was reported as a success")
	}
	if errors.Is(err, ErrMarkerSuperseded) {
		t.Fatalf("err = %v; a cancellation was reported as supersession, which is permanent", err)
	}
	st := m.Stats()
	if st.Failures != 0 || st.LastError != nil {
		t.Fatalf("Stats = %+v; the supervisor cancelling a refresh was counted as the device failing one", st)
	}
	if st.Refreshes != 1 {
		t.Fatalf("Stats = %+v; a cancelled write was counted as landed", st)
	}
	if at, ok := m.WitnessedAt(); !ok || !at.Equal(first) {
		t.Fatalf("WitnessedAt = %s, %v; the last acknowledged write must stand", at, ok)
	}

	// The timeout is the marker's own budget, and running out of it IS a
	// failure: that is a device that did not answer, not a caller that
	// stopped asking.
	slow := &fakeConn{shell: func(ctx context.Context, _ int, _ string) (ShellOutput, error) {
		<-ctx.Done()
		return ShellOutput{}, ctx.Err()
	}}
	m = newTestMarker(t, slow, func(c *MarkerConfig) { c.Timeout = 5 * time.Millisecond })
	if err := m.Refresh(stepCtx(t)); err == nil {
		t.Fatal("a refresh that ran out of its budget was reported as a success")
	}
	if st := m.Stats(); st.Failures != 1 || st.LastError == nil {
		t.Fatalf("Stats = %+v; a wedged device that ate the whole refresh budget was not counted", st)
	}
}

// Every reset tier above 'none' reboots the phone, and for the length of that
// reboot every refresh fails. One Warn per outage is the signal; a Warn per
// tick is an alert that fires on every clean reset in the farm.
//
// Falsify: log every failure at Warn.
func TestMarkerRepeatedFailuresAreOneWarningAndThenInfo(t *testing.T) {
	t.Parallel()

	const outage = 4
	dev := &fakeConn{shell: func(_ context.Context, call int, _ string) (ShellOutput, error) {
		if call > 1 && call <= 1+outage {
			return ShellOutput{}, transportErr("device offline")
		}
		return okShell(""), nil
	}}
	logs := &logCapture{}
	m := newTestMarker(t, dev, func(c *MarkerConfig) { c.Logger = logs.logger() })
	ctx, cancel := context.WithCancel(stepCtx(t))
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	waitMarker(t, "the outage to end and a refresh to land again", func() bool { return m.Stats().Refreshes >= 2 })
	cancel()
	<-done

	got := logs.levels("could not refresh the on-device lease marker")
	if len(got) != outage {
		t.Fatalf("%d failure lines for %d failed refreshes", len(got), outage)
	}
	for i, lvl := range got {
		want := slog.LevelInfo
		if i == 0 {
			want = slog.LevelWarn
		}
		if lvl != want {
			t.Errorf("failure %d logged at %s, want %s: one Warn per outage, Info for the rest", i+1, lvl, want)
		}
	}
	if n := logs.count("is being refreshed again"); n != 1 {
		t.Fatalf("the end of the outage was logged %d times, want once", n)
	}
	if st := m.Stats(); st.Failures != outage {
		t.Fatalf("Stats = %+v; quieter logs must not mean fewer counted failures", st)
	}
}

// Remove has a budget of its own, well under the refresh timeout, because the
// device's release waits behind it and a leftover marker is harmless.
//
// Falsify: bound Remove by Timeout instead of RemoveTimeout.
func TestMarkerRemoveHasItsOwnShortBudget(t *testing.T) {
	t.Parallel()

	var cfg MarkerConfig
	cfg.applyDefaults()
	if cfg.RemoveTimeout != DefaultMarkerRemoveTimeout || cfg.RemoveTimeout >= cfg.Timeout {
		t.Fatalf("RemoveTimeout = %s, Timeout = %s; the delete must be bounded more tightly than a refresh",
			cfg.RemoveTimeout, cfg.Timeout)
	}

	wedged := &fakeConn{shell: func(ctx context.Context, _ int, _ string) (ShellOutput, error) {
		<-ctx.Done()
		return ShellOutput{}, ctx.Err()
	}}
	m := newTestMarker(t, wedged, func(c *MarkerConfig) {
		c.Timeout = 10 * time.Second
		c.RemoveTimeout = 20 * time.Millisecond
	})
	start := time.Now()
	err := m.Remove(stepCtx(t))
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Remove took %s against a device that would not answer; the release was waiting", took)
	}
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the budget running out", err)
	}
	if _, ok := m.WitnessedAt(); ok {
		t.Fatal("evidence still presented after a Remove that was attempted")
	}
}

// ---------------------------------------------------------------------------
// Somebody else's marker
// ---------------------------------------------------------------------------

// The marker path is world-writable on a stock device, so what is found there
// is untrusted text. Only a marker that parses, names a HIGHER fence and names
// THIS device may silence the marker — and it silences it for good.
func TestMarkerGoesSilentOnlyForANewerHolderOfThisDevice(t *testing.T) {
	t.Parallel()

	t.Run("a newer fence on this device", func(t *testing.T) {
		dev := &fakeConn{shell: func(_ context.Context, call int, _ string) (ShellOutput, error) {
			if call == 1 {
				return foreign(markerFile("lease-9", 43, "dev-1")), nil
			}
			return okShell(""), nil
		}}
		m := newTestMarker(t, dev, nil)

		err := m.Refresh(stepCtx(t))
		if !errors.Is(err, ErrMarkerSuperseded) {
			t.Fatalf("err = %v, want ErrMarkerSuperseded", err)
		}
		if n := dev.calls(); n != 1 {
			t.Fatalf("the marker issued %d commands; a newer holder's marker must not be overwritten", n)
		}
		if _, ok := m.WitnessedAt(); ok {
			t.Fatal("evidence is still presented for a device that belongs to a newer fence")
		}
		if st := m.Stats(); !st.Superseded || st.LastError == nil {
			t.Fatalf("Stats = %+v, want Superseded with the reason recorded", st)
		}

		// One-way. Even a write the device later accepts presents nothing:
		// evidence that outlives the thing it was evidence of is the failure
		// the fence field exists to prevent.
		if err := m.Refresh(stepCtx(t)); err != nil {
			t.Fatalf("a later refresh: %v", err)
		}
		if _, ok := m.WitnessedAt(); ok {
			t.Fatal("a superseded marker presented evidence again after a later write")
		}
	})

	for _, tc := range []struct {
		name    string
		content string
	}{
		{"a lower fence: our own leftovers", markerFile("lease-1", 41, "dev-1")},
		{"something that is not a marker", "hello world\n"},
		{"a higher fence naming another device", markerFile("lease-9", 99, "dev-other")},
		{"a bare number", "fence=99\n"},
	} {
		t.Run(tc.name+" is replaced", func(t *testing.T) {
			dev := &fakeConn{shell: func(_ context.Context, call int, _ string) (ShellOutput, error) {
				if call == 1 {
					return foreign(tc.content), nil
				}
				return okShell(""), nil
			}}
			m := newTestMarker(t, dev, nil)

			if err := m.Refresh(stepCtx(t)); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			cmds := dev.commands()
			if len(cmds) != 2 {
				t.Fatalf("the marker issued %d commands, want the refusal and then the replacement", len(cmds))
			}
			if !strings.Contains(cmds[1], "-gt") {
				t.Fatalf("the replacement did not use the numeric strictly-higher guard:\n%s", cmds[1])
			}
			if _, ok := m.WitnessedAt(); !ok {
				t.Fatal("the device accepted the replacement and no evidence is presented")
			}
			if m.Stats().Superseded {
				t.Fatal("replacing a file that is not a newer holder's marker marked this marker superseded")
			}
		})
	}

	t.Run("the device declines the replacement", func(t *testing.T) {
		// Between the read and the write the file changed. What the device
		// shows decides: a newer holder's marker ends the evidence; a forged
		// fence with no lease in it is retried, because going permanently
		// quiet on that would let anything that can write one line to
		// /data/local/tmp switch off a job's witness.
		for _, tc := range []struct {
			name       string
			shown      string
			superseded bool
		}{
			{"with a newer holder's marker", markerFile("lease-9", 99, "dev-1"), true},
			{"with a forged fence", "fence=99\n", false},
			{"with a newer marker for another device", markerFile("lease-9", 99, "dev-other"), false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dev := &fakeConn{shell: func(_ context.Context, call int, _ string) (ShellOutput, error) {
					if call == 1 {
						return foreign("junk\n"), nil
					}
					return foreign(tc.shown), nil
				}}
				m := newTestMarker(t, dev, nil)

				err := m.Refresh(stepCtx(t))
				if err == nil {
					t.Fatal("a declined replacement was reported as a success")
				}
				if got := errors.Is(err, ErrMarkerSuperseded); got != tc.superseded {
					t.Fatalf("superseded = %v (%v), want %v", got, err, tc.superseded)
				}
				if got := m.Stats().Superseded; got != tc.superseded {
					t.Fatalf("Stats.Superseded = %v, want %v", got, tc.superseded)
				}
				if _, ok := m.WitnessedAt(); ok {
					t.Fatal("evidence presented although no write was ever acknowledged")
				}
			})
		}
	})
}

// ---------------------------------------------------------------------------
// Run: a loop with no outcome a caller could act on
// ---------------------------------------------------------------------------

func TestMarkerRunRefreshesOnACadenceAndStopsWithItsContext(t *testing.T) {
	t.Parallel()

	t.Run("stops when the holder's context ends", func(t *testing.T) {
		dev := &fakeConn{}
		m := newTestMarker(t, dev, nil)
		ctx, cancel := context.WithCancel(stepCtx(t))

		done := make(chan struct{})
		go func() { defer close(done); m.Run(ctx) }()

		waitMarker(t, "three refreshes", func() bool { return dev.calls() >= 3 })
		if _, ok := m.WitnessedAt(); !ok {
			t.Fatal("no evidence after three acknowledged refreshes")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after its context ended")
		}
		n := dev.calls()
		time.Sleep(10 * time.Millisecond)
		if dev.calls() != n {
			t.Fatal("the marker was refreshed after Run returned")
		}
	})

	t.Run("goes quiet on its own for a newer holder", func(t *testing.T) {
		dev := &fakeConn{shell: func(_ context.Context, call int, _ string) (ShellOutput, error) {
			if call <= 2 {
				return okShell(""), nil
			}
			return foreign(markerFile("lease-9", 43, "dev-1")), nil
		}}
		m := newTestMarker(t, dev, nil)

		done := make(chan struct{})
		go func() { defer close(done); m.Run(stepCtx(t)) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run kept refreshing a device that belongs to a newer fence")
		}
		if _, ok := m.WitnessedAt(); ok {
			t.Fatal("evidence is still presented after the loop found a newer holder's marker")
		}
	})

	t.Run("keeps trying through a device that cannot be written", func(t *testing.T) {
		dev := &fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
			return ShellOutput{}, transportErr("device offline")
		}}
		m := newTestMarker(t, dev, nil)
		ctx, cancel := context.WithCancel(stepCtx(t))
		defer cancel()

		done := make(chan struct{})
		go func() { defer close(done); m.Run(ctx) }()

		waitMarker(t, "three attempts against a device that will not answer", func() bool { return dev.calls() >= 3 })
		select {
		case <-done:
			t.Fatal("Run gave up on a device that could not be written; a wedged shell is retried inside the lease")
		default:
		}
		if _, ok := m.WitnessedAt(); ok {
			t.Fatal("evidence presented for a device that never acknowledged a write")
		}
		cancel()
		<-done
	})
}

// ---------------------------------------------------------------------------
// Read and Remove
// ---------------------------------------------------------------------------

func TestMarkerReadAndRemove(t *testing.T) {
	t.Parallel()

	t.Run("read parses whatever is there", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			out  ShellOutput
			err  error
		}{
			{"a marker", okShell(markerFile("lease-7", 7, "dev-1")), nil},
			{"no marker", ShellOutput{ExitCode: markerAbsent, Exited: true}, ErrNoMarker},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := newTestMarker(t, &fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
					return tc.out, nil
				}}, nil)
				st, err := m.Read(stepCtx(t))
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				if tc.err == nil && (st.LeaseID != "lease-7" || st.Fence != 7 || st.DeviceID != "dev-1" || st.TouchedUnix != 1) {
					t.Fatalf("Read = %+v", st)
				}
			})
		}
	})

	t.Run("remove silences the evidence before deleting the proof", func(t *testing.T) {
		dev := &fakeConn{}
		m := newTestMarker(t, dev, nil)
		if err := m.Refresh(stepCtx(t)); err != nil {
			t.Fatal(err)
		}
		if err := m.Remove(stepCtx(t)); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if _, ok := m.WitnessedAt(); ok {
			t.Fatal("evidence is still presented for a marker this process deleted")
		}
		cmd := dev.commands()[1]
		for _, want := range []string{`rm -f "` + MarkerPath + `"`, `"` + MarkerDir + `/lease.new."*`, "exit " + fmt.Sprint(markerForeign)} {
			if !strings.Contains(cmd, want) {
				t.Errorf("remove command lacks %q:\n%s", want, cmd)
			}
		}
	})

	t.Run("remove leaves another holder's marker in place", func(t *testing.T) {
		m := newTestMarker(t, &fakeConn{shell: func(context.Context, int, string) (ShellOutput, error) {
			return foreign(""), nil
		}}, nil)
		err := m.Remove(stepCtx(t))
		if err == nil || !strings.Contains(err.Error(), "another holder's marker") {
			t.Fatalf("err = %v, want a refusal naming the other holder", err)
		}
	})
}

// ---------------------------------------------------------------------------
// ParseMarker
// ---------------------------------------------------------------------------

// The device-side guard reads the fence with `head -n 1`, so the first fence
// line is the one that counts here too: two readers of one file disagreeing
// about which fence it claims is how "we may overwrite this" and "we may not"
// get decided by different values.
func TestParseMarkerFirstFenceWinsAndJunkIsNotAMarker(t *testing.T) {
	t.Parallel()

	st, err := ParseMarker([]byte("lease=a\nfence=5\nfence=99\njob=j\n"))
	if err != nil || st.Fence != 5 {
		t.Fatalf("ParseMarker = %+v, %v; want the first fence", st, err)
	}

	for _, bad := range []string{"", "fence=5\n", "lease=a\n", "lease=a\nfence=five\n", "not a marker at all"} {
		if _, err := ParseMarker([]byte(bad)); err == nil {
			t.Errorf("ParseMarker(%q) accepted a file with no lease or no numeric fence", bad)
		}
	}

	// Untrusted text from a world-writable path: control characters are
	// stripped on the way into a log line, and the read is bounded.
	st, err = ParseMarker([]byte("lease=a\x07\nfence=1\n" + strings.Repeat("x", 2*maxMarkerBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if st.LeaseID != "a" || strings.ContainsRune(st.Raw, '\x07') || len(st.Raw) > maxMarkerBytes {
		t.Fatalf("ParseMarker did not sanitise or bound the file: lease %q, raw %d bytes", st.LeaseID, len(st.Raw))
	}
}
