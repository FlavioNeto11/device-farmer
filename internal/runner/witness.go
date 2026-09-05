package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/lease"
)

// The marker: the device's own record of who is driving it.
//
// A lease can be kept alive by evidence gathered ON THE DEVICE when the
// control plane cannot be reached — see internal/lease/witness.go. This file
// produces that evidence. The job's agent rewrites a small file on the device
// on a timer; the fact that the write succeeded is the proof, because a
// process that can still drive this device is a process whose work is still
// running.
//
// The marker names the lease and the fence deliberately. Evidence that says
// only "somebody was here" is worse than none: a marker left behind by a
// previous holder would look exactly like proof that the current one is alive,
// and the reaper would then decline to reclaim a device nobody is using. With
// the lease id and the fence written down, a marker that belongs to somebody
// else is DETECTABLE, and a marker carrying a fence above ours is a fact worth
// reporting rather than overwriting.
//
// Nothing in this file ends anything. A refresh that fails — the device is
// wedged, the cable was bumped, the shell timed out — is a transport failure
// like any other in this package: it is retried on the next tick inside the
// lease the job still holds. Its only consequence is that the evidence goes
// stale, and stale evidence means the witness loop presents nothing, which is
// exactly the honest outcome.

// Where the marker lives on the device.
//
// The same directory the enrollment brand uses (internal/enroll.BrandDir):
// /data/local/tmp is the one place an ADB shell can reliably write on a stock,
// non-rooted phone. It is under /data, so a factory reset erases it — which is
// correct, since a wiped device is not running anybody's job.
const (
	MarkerDir  = "/data/local/tmp/.farm"
	MarkerPath = MarkerDir + "/lease"

	// markerTmpPrefix begins the scratch path a refresh writes before renaming
	// it over the real one, so a device that loses power mid-write ends up with
	// the old marker or the new one and never with half a fence. A truncated
	// fence parses as somebody else's and would make this holder look like a
	// stranger to itself.
	//
	// The fence is appended, so the scratch path is unique per grant. Two
	// holders overlap on one device for real — the fenced one keeps running
	// until its renewal loop hears from Postgres — and a shared scratch path
	// lets them interleave a truncate and a write into the same file, so that
	// whichever renames second publishes a marker built from both. That heals
	// on the next tick, but it heals by making the device briefly claim a lease
	// nobody is holding, which is the one thing this file's fence field exists
	// to make impossible.
	markerTmpPrefix = MarkerDir + "/lease.new."
)

// Marker-loop defaults.
const (
	// DefaultMarkerInterval is how often the marker is rewritten when the
	// caller sets no cadence: the default witness cadence divided by the
	// number of writes a witness tick is entitled to. It is not a number of
	// its own — the witness may only present evidence that is fresh, so the
	// marker must be refreshed several times per witness tick for a single
	// lost round trip to cost nothing, and config.MarkersPerWitnessTick is
	// the one place that ratio is written down.
	DefaultMarkerInterval = lease.DefaultWitnessInterval / config.MarkersPerWitnessTick

	// DefaultMarkerTimeout bounds one refresh. A refresh that hangs must not
	// consume the interval, or one wedged shell turns into no evidence at all.
	DefaultMarkerTimeout = 20 * time.Second

	// DefaultMarkerRemoveTimeout bounds Remove, separately and much more
	// tightly. Remove sits between a job's verdict and the release of its
	// device, so every second it spends is a second the device stays parked
	// on a finished job — and it is buying nothing that matters: a marker
	// that outlives its lease is detected by the fence written inside it, so
	// a device that will not take the delete in a few seconds is left with a
	// harmless leftover rather than allowed to hold up the release.
	DefaultMarkerRemoveTimeout = 5 * time.Second

	// maxMarkerBytes bounds what is read back from the marker path. A marker is
	// a couple of hundred bytes; the slack exists so an operator can be shown
	// what a stranger put at that path instead, and the cap exists so a device
	// with a large file there cannot make this loop carry it around.
	maxMarkerBytes = 4096
)

// Exit statuses the marker commands use to say what the output cannot.
const (
	// markerForeign means the device found a marker naming a different fence at
	// the moment of the write. Its content is printed, because "somebody else's
	// marker" and "our own marker from a previous attempt" call for opposite
	// responses and produce identical failures otherwise.
	markerForeign = 17

	// markerAbsent means there is no marker file at all — the ordinary state of
	// a device that was just allocated.
	markerAbsent = 44
)

// ErrMarkerSuperseded reports that the device carries a marker at a HIGHER
// fence than ours: another holder has been granted this device.
//
// It is returned by Refresh, it clears this marker's evidence, and it ends
// nothing. Whether this job still holds its lease is a question only
// farm.lease_renew answers, and it will answer it on the renewal loop's own
// schedule; a file on a device is not permitted to pre-empt that. What this
// error does buy is honesty: we stop claiming device-side proof of work on a
// device that is no longer ours to prove anything about.
var ErrMarkerSuperseded = errors.New("runner: the device carries a lease marker at a higher fence")

// ErrNoMarker reports that the device carries no marker file.
var ErrNoMarker = errors.New("runner: the device carries no lease marker")

// MarkerState is a marker file as read back off a device.
type MarkerState struct {
	LeaseID  string
	JobID    string
	DeviceID string
	Fence    int64

	// TouchedUnix is the device's OWN clock reading at the last refresh, in
	// seconds. It is written for a human reading the file over `adb shell`, and
	// is never compared against anything here and never sent to the database.
	// Freshness that matters is measured by the process doing the refreshing,
	// against its own monotonic clock.
	TouchedUnix int64

	// Raw is the file as it was read, bounded and sanitised, so an operator can
	// be shown what is actually on the device when it parses as nothing.
	Raw string
}

// MarkerConfig configures a Marker. The zero value is valid.
type MarkerConfig struct {
	// Interval is the refresh cadence. The supervisor derives it from the
	// witness cadence (config.MarkerIntervalFor) so the evidence window and
	// the writes that fill it come from one rule.
	Interval time.Duration

	// Timeout bounds one refresh round trip.
	Timeout time.Duration

	// RemoveTimeout bounds Remove. See DefaultMarkerRemoveTimeout for why it
	// is a separate, shorter budget than Timeout.
	RemoveTimeout time.Duration

	Logger *slog.Logger
}

func (c *MarkerConfig) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = DefaultMarkerInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultMarkerTimeout
	}
	if c.RemoveTimeout <= 0 {
		c.RemoveTimeout = DefaultMarkerRemoveTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// MarkerStats is a snapshot of marker health, for the supervisor's logs.
type MarkerStats struct {
	Refreshes uint64
	Failures  uint64

	// LastRefresh is when a refresh last succeeded; zero when none has.
	LastRefresh time.Time

	// Superseded records that the device carried a marker at a higher fence.
	// Once true this marker presents no evidence ever again; only a fresh
	// Marker, for whatever lease the job is placed under next, starts over.
	Superseded bool

	// LastError is the most recent refresh failure, kept for the log line the
	// supervisor writes when a job ends.
	LastError error
}

// Marker keeps the on-device lease marker fresh for one placement.
//
// It satisfies lease.Evidence, so the witness loop can only present a witness
// when this marker was genuinely written to the device.
type Marker struct {
	dev  Conn
	cfg  MarkerConfig
	log  *slog.Logger
	body string // the file content, fixed for the life of the placement
	tmp  string // this grant's scratch path, fixed for the life of the placement

	place Placement

	mu sync.Mutex
	// silent stops WitnessedAt reporting anything, whatever the stats say.
	// Set when the device turns out to belong to a newer fence and when the
	// marker is deleted: in both cases the proof is gone, and evidence that
	// outlives the thing it was evidence of is the failure this marker's fence
	// field exists to prevent.
	silent bool
	stats  MarkerStats
	// streak counts consecutive failed refreshes, for the log: the first one
	// is worth a Warn, the rest of a reboot's worth are not.
	streak uint64
}

var _ lease.Evidence = (*Marker)(nil)

// NewMarker returns a Marker for one placement on one device connection.
func NewMarker(dev Conn, p Placement, cfg MarkerConfig) (*Marker, error) {
	if dev == nil {
		return nil, errors.New("runner: marker needs a device connection")
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	if p.LeaseID == "" {
		return nil, errors.New("runner: marker needs a lease id; evidence that names no lease is " +
			"indistinguishable from a previous holder's leftovers")
	}
	// The marker is a line-based format read back by machines and by humans. A
	// field carrying a newline would forge a second field, so it is refused
	// here rather than written and misparsed later. Everything is single-quoted
	// on its way into the shell as well; neither guard is a substitute for the
	// other.
	for name, v := range map[string]string{
		"lease id": p.LeaseID, "job id": p.JobID, "device id": p.DeviceID,
	} {
		if strings.ContainsAny(v, "\n\r\x00") {
			return nil, fmt.Errorf("runner: marker %s contains a line break or NUL and cannot be written", name)
		}
	}
	cfg.applyDefaults()

	m := &Marker{
		dev:   dev,
		cfg:   cfg,
		place: p,
		log: cfg.Logger.With("component", "marker", "job_id", p.JobID,
			"device_id", p.DeviceID, "lease_id", p.LeaseID, "fence", p.Fence),
	}
	m.body = m.content()
	m.tmp = markerTmpPrefix + strconv.FormatInt(p.Fence, 10)
	return m, nil
}

// WitnessedAt implements lease.Evidence: when the marker was last written to
// the device, and false when there is nothing to stand on.
//
// The instant is this process's own reading taken at the moment the device
// acknowledged the write. It is never sent anywhere — it exists so the witness
// loop can tell "our agent touched the device a moment ago" from "our agent
// has not managed to touch it for ten minutes", and present a witness only in
// the first case.
func (m *Marker) WitnessedAt() (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.silent || m.stats.LastRefresh.IsZero() {
		return time.Time{}, false
	}
	return m.stats.LastRefresh, true
}

// Stats returns a snapshot of marker health.
func (m *Marker) Stats() MarkerStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

// Run refreshes the marker immediately and then on the configured cadence,
// returning when ctx ends or when the device turns out to belong to a newer
// fence.
//
// It returns nothing, and that is the point: there is no outcome of this loop
// that a caller should act on. A device that cannot be written to is retried;
// a device that belongs to somebody else stops producing evidence and this
// loop goes quiet. Neither is a reason to end a job — that verdict comes from
// farm.lease_renew, on a different wire — and a signature with an error in it
// would eventually be wired to one.
//
// ctx must descend from the holder's context, so the marker stops being
// refreshed the moment the lease is no longer held.
func (m *Marker) Run(ctx context.Context) {
	if !m.refreshLogged(ctx) {
		return
	}

	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A tick and the cancellation can come due together, and select
			// picks between ready cases at random. A refresh issued after the
			// supervisor said stop is a round trip to a device it may be
			// about to delete the marker from.
			if ctx.Err() != nil {
				return
			}
			if !m.refreshLogged(ctx) {
				return
			}
		}
	}
}

// refreshLogged refreshes once and reports whether there is any point trying
// again.
func (m *Marker) refreshLogged(ctx context.Context) (continueRefreshing bool) {
	err := m.Refresh(ctx)
	switch {
	case ctx.Err() != nil:
		// The supervisor ended the loop. Whatever the round trip returned is
		// not a fact about the device, so nothing is logged and nothing was
		// counted (see Refresh).
		return false

	case err == nil:
		if n := m.recovered(); n > 0 {
			// The counterpart of the Warn below, so an operator reading a
			// device's log can see the outage close and how long it lasted
			// without counting Info lines.
			m.log.Info("the on-device lease marker is being refreshed again", "path", MarkerPath,
				"failed_refreshes", n)
		}
		return true

	case errors.Is(err, ErrMarkerSuperseded):
		// Nothing to write and nothing to prove: this device is somebody
		// else's now. Said once, at Warn, and then this loop is done. Whether
		// THIS lease has ended is a separate question, asked of Postgres by the
		// renewal loop and answered by it alone.
		m.log.Warn("the device carries a newer holder's lease marker; no further evidence will be produced",
			"path", MarkerPath, "err", err)
		return false

	default:
		// Warn once, never Error, and Info for the repeats. The job is fine,
		// the lease is fine, and the only consequence is that a witness will
		// not be presented until this succeeds again. And the repeats are
		// expected: every reset tier above 'none' reboots the phone, and for
		// the length of that reboot every tick fails in exactly this way. One
		// Warn per outage is the signal; a Warn per tick is a false alarm
		// that fires on every clean reset in the farm.
		level, streak := slog.LevelInfo, m.failStreak()
		if streak == 1 {
			level = slog.LevelWarn
		}
		m.log.Log(context.Background(), level,
			"could not refresh the on-device lease marker (the job and the lease are untouched)",
			"path", MarkerPath, "consecutive_failures", streak, "err", err)
		return true
	}
}

// Refresh writes the marker once.
//
// A transport failure is returned as an ordinary error and leaves the previous
// refresh time standing: the evidence ages, and the witness loop decides for
// itself when it has become too old to present. ErrMarkerSuperseded is
// different in one respect only — it clears the evidence immediately, because
// a device that has been granted to a newer fence is one we can prove nothing
// about.
//
// A refresh cut short because ctx ended is neither a success nor a failure. It
// is returned as an error, since no write landed, but it is not counted: the
// supervisor cancelling a refresh when the job finishes says nothing about
// the device, and a placement that ended cleanly must not go on record as one
// whose evidence failed.
func (m *Marker) Refresh(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()

	err := m.write(cctx)
	switch {
	case err == nil:
		return m.refreshed()
	case errors.Is(err, ErrMarkerSuperseded):
		// Recorded by supersededBy, which is the only thing that produces it.
		return err
	case ctx.Err() != nil:
		// The caller's context, not the timeout's: a refresh that ran out of
		// its own budget is a wedged device and counts, a refresh the
		// supervisor cancelled is nothing.
		return err
	default:
		return m.failed(err)
	}
}

// write is one refresh round trip, without the accounting.
func (m *Marker) write(ctx context.Context) error {
	out, err := m.dev.Shell(ctx, m.writeCmd(false))
	if err != nil {
		return fmt.Errorf("runner: write %s: %w", MarkerPath, err)
	}
	if !out.Exited {
		// The stream ended before the device said anything. Reading that as
		// success would have us present a witness for a write that may never
		// have landed.
		return fmt.Errorf("runner: write %s: the shell stream ended without an exit status", MarkerPath)
	}

	switch out.ExitCode {
	case 0:
		return nil

	case markerForeign:
		return m.resolveForeign(ctx, out)

	default:
		return fmt.Errorf("runner: write %s: exit status %d%s",
			MarkerPath, out.ExitCode, stderrHint(out))
	}
}

// resolveForeign decides what to do about a marker that names a different
// fence, having just been shown its content by the device.
//
// It shares the refresh's context, and so its remaining budget, on purpose:
// one refresh is one bounded operation however many round trips it takes, and
// a replacement that runs out of time is simply retried on the next tick.
func (m *Marker) resolveForeign(ctx context.Context, out ShellOutput) error {
	st, perr := ParseMarker(out.Stdout)

	switch {
	case m.newerHolder(st, perr):
		// Somebody newer holds this device. Reported and recorded; not acted
		// on. The renewal loop is the only thing that may conclude anything
		// about our lease from this, and it asks Postgres, not a phone.
		return m.supersededBy(st)

	case perr == nil:
		// An older fence: our own leftovers from a previous attempt, or a
		// holder that has already been fenced out. Ours now, so it is replaced
		// — loudly enough that an operator reading the log can see which lease
		// was overwritten.
		m.log.Warn("replacing a stale lease marker left on the device",
			"path", MarkerPath, "previous_lease_id", truncateMarker(st.LeaseID, 128),
			"previous_fence", st.Fence, "previous_job_id", truncateMarker(st.JobID, 128))

	default:
		// Something that is not a marker is at that path. Same response: this
		// device is ours at this fence, and the file is ours to own.
		m.log.Warn("the lease marker path holds something that is not a marker; replacing it",
			"path", MarkerPath, "content", truncateMarker(st.Raw, 200), "err", perr)
	}

	forced, err := m.dev.Shell(ctx, m.writeCmd(true))
	if err != nil {
		return fmt.Errorf("runner: replace %s: %w", MarkerPath, err)
	}
	if !forced.Exited {
		return fmt.Errorf("runner: replace %s: the shell stream ended without an exit status", MarkerPath)
	}

	switch forced.ExitCode {
	case 0:
		return nil

	case markerForeign:
		// The file changed under us between the read above and the write, into
		// something carrying a higher fence, and the device declined rather
		// than let us stamp our fence over it.
		now, nerr := ParseMarker(forced.Stdout)
		if m.newerHolder(now, nerr) {
			return m.supersededBy(now)
		}
		// It refused, but what it showed us is not a newer holder's marker —
		// a forged fence with no lease in it, most likely, and the marker path
		// is world-writable on a stock device. Retryable, and deliberately NOT
		// a supersession: going permanently quiet would let anything that can
		// write one line to /data/local/tmp disable this job's witness for the
		// rest of its run.
		return fmt.Errorf("runner: replace %s: the device declined the write and shows %q",
			MarkerPath, truncateMarker(now.Raw, 200))

	default:
		return fmt.Errorf("runner: replace %s: exit status %d%s",
			MarkerPath, forced.ExitCode, stderrHint(forced))
	}
}

// newerHolder reports whether st is a marker a LATER holder of THIS device
// wrote, which is the only thing that may stop this marker for good.
//
// The three conditions are all load-bearing, because the marker path is
// world-writable on a stock device and its content is untrusted text like any
// other device output. Supersession is a one-way latch — no further evidence,
// for the rest of the placement — so anything that can write one line to
// /data/local/tmp would otherwise be able to switch off a job's witness by
// naming a large enough fence:
//
//   - it has to parse as a marker at all, so a bare number is not enough;
//   - it has to name a fence above ours, since a lower one is our own
//     leftovers or a holder that has already been fenced out;
//   - it has to name THIS device. A newer holder of this device writes the
//     same farm.devices id we do, and a forger has to know that id to be
//     believed. A mismatch is not a newer holder, so it is treated as the
//     junk it is: replaced when the device lets us, retried when it does not,
//     and never a reason to go quiet permanently.
func (m *Marker) newerHolder(st MarkerState, perr error) bool {
	return perr == nil && st.Fence > m.place.Fence && st.DeviceID == m.place.DeviceID
}

// supersededBy records that st, read off the device, carries a fence above ours
// and stops this marker presenting evidence.
//
// It ends nothing. Whether this job still holds its lease is a question only
// farm.lease_renew answers.
func (m *Marker) supersededBy(st MarkerState) error {
	err := fmt.Errorf("%w: %s names lease %s at fence %d, above ours (%d); "+
		"no witness will be presented for this device until farm.lease_renew says what happened",
		ErrMarkerSuperseded, MarkerPath, truncateMarker(st.LeaseID, 128), st.Fence, m.place.Fence)
	m.superseded(err)
	return err
}

// Read returns the marker currently on the device, whoever wrote it.
//
// ErrNoMarker means there is none, which is the ordinary state of a freshly
// allocated device and not a failure of any kind.
func (m *Marker) Read(ctx context.Context) (MarkerState, error) {
	cctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()

	out, err := m.dev.Shell(cctx, "if [ -e "+dq(MarkerPath)+" ]; then cat "+dq(MarkerPath)+
		"; else exit "+strconv.Itoa(markerAbsent)+"; fi")
	if err != nil {
		return MarkerState{}, fmt.Errorf("runner: read %s: %w", MarkerPath, err)
	}
	if !out.Exited {
		return MarkerState{}, fmt.Errorf("runner: read %s: the shell stream ended without an exit status", MarkerPath)
	}
	switch out.ExitCode {
	case 0:
		return ParseMarker(out.Stdout)
	case markerAbsent:
		return MarkerState{}, ErrNoMarker
	default:
		return MarkerState{}, fmt.Errorf("runner: read %s: exit status %d%s",
			MarkerPath, out.ExitCode, stderrHint(out))
	}
}

// Remove deletes the marker, best effort.
//
// Worth doing when a job ends cleanly so the next holder does not have to
// reason about leftovers — but only a courtesy: a marker that outlives its
// lease is detectable by its fence, which is why the fence is in it.
//
// It deletes the marker only while the marker is still OURS. A supervisor that
// tidies up after a fenced job would otherwise erase the evidence of whoever
// was granted the device next, and "the device is ours to write to" is a
// question this file never answers from its own memory.
//
// It is bounded by RemoveTimeout, not Timeout: the device's release waits
// behind this call, and a leftover is harmless.
func (m *Marker) Remove(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, m.cfg.RemoveTimeout)
	defer cancel()

	m.hush() // stop presenting evidence before deleting the proof

	// The scratch paths go with it, including any a holder at another fence
	// left behind: the marker directory lives on a device that may run
	// thousands of jobs before its next wipe.
	out, err := m.dev.Shell(cctx, "if [ -s "+dq(MarkerPath)+" ] && [ \"$("+readFenceCmd()+")\" != "+
		shQuote(strconv.FormatInt(m.place.Fence, 10))+" ]; then exit "+strconv.Itoa(markerForeign)+"; fi; "+
		"rm -f "+dq(MarkerPath)+" "+dq(markerTmpPrefix)+"*")
	if err != nil {
		return fmt.Errorf("runner: remove %s: %w", MarkerPath, err)
	}
	if !out.Exited {
		return fmt.Errorf("runner: remove %s: the shell stream ended without an exit status", MarkerPath)
	}
	switch out.ExitCode {
	case 0:
		return nil
	case markerForeign:
		return fmt.Errorf("runner: remove %s: the device carries another holder's marker; left in place", MarkerPath)
	default:
		return fmt.Errorf("runner: remove %s: exit status %d%s", MarkerPath, out.ExitCode, stderrHint(out))
	}
}

// content is the file the device will hold: line-based, machine-readable, and
// legible to a human with an adb shell at 3am.
func (m *Marker) content() string {
	var b strings.Builder
	fmt.Fprintf(&b, "lease=%s\n", m.place.LeaseID)
	fmt.Fprintf(&b, "fence=%d\n", m.place.Fence)
	fmt.Fprintf(&b, "job=%s\n", m.place.JobID)
	fmt.Fprintf(&b, "device=%s\n", m.place.DeviceID)
	return b.String()
}

// readFenceCmd prints the marker's first fence line, or nothing.
//
// First and not last, matching [ParseMarker]: the device and this process must
// never disagree about which fence a file claims, or the guard below refuses a
// write that the Go code has already concluded is ours to make.
func readFenceCmd() string {
	return "sed -n 's/^fence=//p' " + dq(MarkerPath) + " 2>/dev/null | head -n 1"
}

// writeCmd builds the refresh command.
//
// Both forms re-check ON THE DEVICE, in the same shell that does the writing,
// what we checked a round trip ago. That is the whole point: we never overwrite
// a newer holder's evidence on the strength of a `cat` that succeeded a moment
// earlier, and there is no instant between deciding the marker is ours to take
// and taking it.
//
// The two guards differ because they are answering different questions:
//
//   - The ordinary refresh refuses any fence that is not exactly ours and
//     prints what it found, so [Marker.resolveForeign] can tell an operator
//     whose marker it is about to replace.
//   - The replacement has already made that decision, so it refuses only a
//     STRICTLY HIGHER fence — the one case where the file changed under us
//     into something we may not touch. The comparison is numeric, so a value
//     the device would render differently from strconv (a leading zero, say)
//     cannot make the two forms disagree forever and stall the marker.
//     Anything that is not a number counts as no marker at all.
//
// The write goes to a temporary path and is renamed over the real one, so there
// is no instant at which the marker is half a fence.
//
// touched is filled in by the DEVICE's own clock. No timestamp of ours is
// written to it and none is read back for a decision.
func (m *Marker) writeCmd(replacing bool) string {
	fence := shQuote(strconv.FormatInt(m.place.Fence, 10))

	var b strings.Builder
	b.WriteString("mkdir -p " + dq(MarkerDir) + " || exit 1; ")
	if replacing {
		b.WriteString("f=$(" + readFenceCmd() + "); case \"$f\" in ''|*[!0-9]*) f=0 ;; esac; " +
			"if [ \"$f\" -gt " + fence + " ]; then cat " + dq(MarkerPath) +
			"; exit " + strconv.Itoa(markerForeign) + "; fi; ")
	} else {
		b.WriteString("if [ -s " + dq(MarkerPath) + " ] && [ \"$(" + readFenceCmd() + ")\" != " + fence +
			" ]; then cat " + dq(MarkerPath) + "; exit " + strconv.Itoa(markerForeign) + "; fi; ")
	}
	b.WriteString("{ printf '%s' " + shQuote(m.body) +
		"; printf 'touched=%s\\n' \"$(date +%s 2>/dev/null || echo 0)\"; } > " + dq(m.tmp) +
		" && chmod 600 " + dq(m.tmp) +
		" && mv -f " + dq(m.tmp) + " " + dq(MarkerPath))
	return b.String()
}

// ParseMarker reads a marker file's bytes.
//
// It is deliberately tolerant of everything except the two fields that decide
// whose marker this is. A file with no lease and no fence in it is not a
// marker, however much else it contains.
func ParseMarker(b []byte) (MarkerState, error) {
	if len(b) > maxMarkerBytes {
		b = b[:maxMarkerBytes]
	}
	st := MarkerState{Raw: sanitiseMarker(string(b))}

	var fenceSeen bool
	for _, line := range strings.Split(st.Raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "lease":
			st.LeaseID = value
		case "job":
			st.JobID = value
		case "device":
			st.DeviceID = value
		case "fence":
			// FIRST wins, because the device-side guard reads the file with
			// `head -n 1`. A file with two fence lines is forged, and the two
			// readers of it disagreeing about which one counts is how "we may
			// overwrite this" and "we may not" end up being decided by
			// different values in the same file.
			if fenceSeen {
				continue
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return st, fmt.Errorf("runner: marker fence %q is not a number: %w", truncateMarker(value, 64), err)
			}
			st.Fence, fenceSeen = n, true
		case "touched":
			// Advisory, and written by a clock we do not control. An
			// unparseable value costs nothing.
			st.TouchedUnix, _ = strconv.ParseInt(value, 10, 64)
		}
	}

	if st.LeaseID == "" || !fenceSeen {
		return st, fmt.Errorf("runner: %q is not a lease marker", truncateMarker(st.Raw, 128))
	}
	return st, nil
}

// sanitiseMarker strips control characters from a file that came off a phone,
// on its way into a log line or an error. The marker path is world-writable on
// a stock device, so its content is untrusted text like any other device
// output.
func sanitiseMarker(s string) string {
	return strings.ToValidUTF8(strings.Map(func(r rune) rune {
		if r == '\n' || (r >= 0x20 && r != 0x7f) {
			return r
		}
		return -1
	}, s), "")
}

func truncateMarker(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary: these strings reach farm.job_steps.detail, which
	// is jsonb, and half a rune is not text.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// stderrHint appends a device's own complaint to an error, when it made one.
func stderrHint(out ShellOutput) string {
	msg := strings.TrimSpace(sanitiseMarker(string(out.Stderr)))
	if msg == "" {
		return ""
	}
	return ": " + truncateMarker(msg, 200)
}

func (m *Marker) refreshed() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.Refreshes++
	m.stats.LastRefresh = time.Now()
	m.stats.LastError = nil
	return nil
}

func (m *Marker) failed(err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.Failures++
	m.stats.LastError = err
	m.streak++
	return err
}

// failStreak is how many refreshes in a row have failed, this one included.
func (m *Marker) failStreak() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streak
}

// recovered closes a failure streak after a successful refresh and reports how
// long it was; zero when there was none.
func (m *Marker) recovered() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.streak
	m.streak = 0
	return n
}

// superseded records that a newer fence owns this device and stops the marker
// presenting evidence about it.
//
// It records the error as well, so the line the supervisor writes when the job
// ends says why the evidence stopped rather than reporting whatever transport
// failure happened to come last.
func (m *Marker) superseded(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.Superseded = true
	m.stats.LastError = err
	m.silent = true
}

// hush stops the marker presenting evidence without claiming anything about
// who owns the device.
func (m *Marker) hush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.silent = true
}
