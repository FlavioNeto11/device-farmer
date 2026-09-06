package runner

// The round trip a resume actually depends on:
//
//	saveCheckpoint -> farm.jobs.checkpoint jsonb -> parseCheckpoint -> belongsTo
//	  -> planResume
//
// checkpoint_test.go covers the DECISIONS, and it covers them against
// hand-built Checkpoint literals, which is the right shape for them: they are
// pure functions and a literal says exactly what is being decided. What a
// literal cannot say is that the value planResume decides on is the value
// Postgres handed back, or that the write which produced it happened at all.
// Two processes are involved in every resume and they share nothing but that
// column, so the column is the part worth executing.
//
// saveCheckpoint has one line that is the difference between a pod eviction
// costing a job nothing and a pod eviction destroying the work of the attempt
// that replaced it:
//
//	AND COALESCE((checkpoint->>'fence')::bigint, 0) <= $3
//
// It cannot be exercised without a database, so it is exercised with one. Every
// test here skips when DATABASE_URL is unset.

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/jobspec"
	"github.com/flaviopadilha/device-farmer/internal/lease"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// readCheckpointColumn returns farm.jobs.checkpoint exactly as loadJob reads
// it: raw jsonb, ready to be handed to parseCheckpoint. Nothing between the two
// processes of a resume is any richer than these bytes.
func readCheckpointColumn(t *testing.T, pool *pgxpool.Pool, jobID string) []byte {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(t.Context(),
		`SELECT checkpoint FROM farm.jobs WHERE id = $1::uuid`, jobID).Scan(&raw); err != nil {
		t.Fatalf("reading farm.jobs.checkpoint: %v", err)
	}
	return raw
}

// seedPlacement gives the fixture's job the two rows a real placement rests on:
// a device, because farm.job_attempts.device_id is a foreign key, and a held
// lease, because writeJobState refuses to record a verdict without one at this
// fence.
//
// The fence is the one farm.fence_seq minted for that lease rather than a
// number the test chose. It is the whole basis on which a resume concludes that
// the device in front of it is still the device its checkpoint describes, so a
// test that made one up would be testing an integer comparison instead.
func seedPlacement(t *testing.T, pool *pgxpool.Pool, f *jobFixture) Placement {
	t.Helper()
	ctx := t.Context()

	// farm_uid is the branded identity and must match '^df-[0-9a-f]{32}$'. The
	// job's own uuid supplies exactly 32 unique hex digits, so two fixtures in
	// one run cannot collide on it.
	uid := "df-" + strings.ReplaceAll(f.jobID, "-", "")

	var deviceID string
	if err := pool.QueryRow(ctx, `
INSERT INTO farm.devices (farm_uid, pool_id, model) VALUES ($1, $2, 'Test Device')
RETURNING id::text`, uid, f.poolID).Scan(&deviceID); err != nil {
		t.Fatalf("seeding the device: %v", err)
	}

	var leaseID string
	var fence int64
	if err := pool.QueryRow(ctx, `
INSERT INTO farm.leases (device_id, job_id, tenant_id, queue_id, holder, holder_instance,
                         ttl, grace, expires_at, reclaimable_at)
SELECT $1::uuid, j.id, j.tenant_id, j.queue_id, 'runner-test', gen_random_uuid(),
       j.ttl, j.grace, now() + j.ttl, now() + j.ttl + j.grace
  FROM farm.jobs j
 WHERE j.id = $2::uuid
RETURNING id::text, fence`, deviceID, f.jobID).Scan(&leaseID, &fence); err != nil {
		t.Fatalf("seeding the lease: %v", err)
	}

	return Placement{
		JobID: f.jobID, DeviceID: deviceID, LeaseID: leaseID, Fence: fence,
		Devpath: "usb:3-1.4",
	}
}

// storeSpec puts spec on the fixture's job and rewinds the attempt counter, so
// the first Run below claims attempt 1 the way a freshly queued job does.
func storeSpec(t *testing.T, pool *pgxpool.Pool, jobID string, spec jobspec.Spec) {
	t.Helper()
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshalling the spec: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`UPDATE farm.jobs SET spec = $2::jsonb, attempt = 0 WHERE id = $1::uuid`,
		jobID, string(body)); err != nil {
		t.Fatalf("storing the spec: %v", err)
	}
}

// answerFor is what a healthy device says to one command. prepare asks every
// device it touches whether setsid is there; answering it here keeps it out of
// every test's own table, where it would only be noise.
func answerFor(stdout map[string]string, command string) string {
	if strings.Contains(command, "command -v setsid") {
		return "setsid"
	}
	return stdout[command]
}

// answeringDevice exits 0 for every command, with stdout for the ones named.
func answeringDevice(stdout map[string]string) *fakeConn {
	return &fakeConn{shell: func(_ context.Context, _ int, command string) (ShellOutput, error) {
		return okShell(answerFor(stdout, command)), nil
	}}
}

// deviceThatBlocksOn behaves like answeringDevice until it is asked to run
// block, at which point it closes the returned channel and waits for the run
// context to end. That is what a step in progress looks like from the outside
// when the pod running it is evicted underneath it — and closing the channel
// only once the command has arrived is what makes the interruption land in a
// known place rather than wherever a sleep happened to leave it.
func deviceThatBlocksOn(block string, stdout map[string]string) (*fakeConn, <-chan struct{}) {
	reached := make(chan struct{})
	var once sync.Once
	dev := &fakeConn{shell: func(ctx context.Context, _ int, command string) (ShellOutput, error) {
		if command != block {
			return okShell(answerFor(stdout, command)), nil
		}
		once.Do(func() { close(reached) })
		<-ctx.Done()
		return ShellOutput{}, ctx.Err()
	}}
	return dev, reached
}

// jobStates returns the job's attempt counter and state.
func jobStateOf(t *testing.T, pool *pgxpool.Pool, jobID string) (attempt int, state string) {
	t.Helper()
	if err := pool.QueryRow(t.Context(),
		`SELECT attempt, state FROM farm.jobs WHERE id = $1::uuid`, jobID).Scan(&attempt, &state); err != nil {
		t.Fatalf("reading the job row: %v", err)
	}
	return attempt, state
}

// stepRow is one row of farm.job_steps as these tests read it.
type stepRow struct {
	index   int
	id      string
	state   string
	detail  string
	errText string
}

func stepRowsOf(t *testing.T, pool *pgxpool.Pool, jobID string) []stepRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT step_index, step_id, state, detail::text, COALESCE(error, '')
  FROM farm.job_steps WHERE job_id = $1::uuid ORDER BY step_index`, jobID)
	if err != nil {
		t.Fatalf("reading farm.job_steps: %v", err)
	}
	defer rows.Close()
	var out []stepRow
	for rows.Next() {
		var x stepRow
		if err := rows.Scan(&x.index, &x.id, &x.state, &x.detail, &x.errText); err != nil {
			t.Fatalf("scanning a step row: %v", err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading farm.job_steps: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// The column itself
// ---------------------------------------------------------------------------

// The whole chain, in one test: write the progress, read the column back, ask
// it which placement it belongs to, and plan a resume from it against the
// vocabulary as farm.step_kinds holds it. Nothing here is restated in Go.
//
// The point is the seam. planResume's own decisions are proved by literals
// elsewhere; a serialisation that quietly disagreed with those literals — a
// field that does not survive the jsonb column, a marker that is cleared in
// memory and not on the wire — would show up here as a resume that plans the
// wrong thing while every pure test stayed green.
//
// Falsify: drop `json:"completed"` from Checkpoint and watch the record of what
// already ran fail to survive the column the resume reads it out of.
func TestACheckpointSurvivesThePostgresColumnItIsResumedFrom(t *testing.T) {
	pool := requireDB(t)
	f := newJobFixture(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	r, _ := testRunner(t, func(c *Config) { c.Pool = pool })
	p := Placement{JobID: f.jobID, DeviceID: "dev-1", Devpath: "usb:3-1.4", LeaseID: "lease-1", Fence: 42}
	spec := resumeSpec()

	// farm.jobs.checkpoint defaults to '{}', not to NULL. A job that has never
	// been placed must therefore read back as NO checkpoint, rather than as an
	// empty one that belongs to whoever asks first.
	if got := parseCheckpoint(readCheckpointColumn(t, pool, f.jobID)); got.belongsTo(p) {
		t.Fatalf("a job that has never checkpointed offered %+v to resume from", got)
	}

	kinds, err := r.loadKinds(ctx)
	if err != nil {
		t.Fatalf("loading farm.step_kinds: %v", err)
	}

	c := newCheckpoint(2, p, spec)
	c.markInFlight(0, spec.Steps[0])
	c.markDone(0, spec.Steps[0])
	r.saveCheckpoint(ctx, r.log, p, c)

	back := parseCheckpoint(readCheckpointColumn(t, pool, f.jobID))
	if back.Version != checkpointVersion || back.Attempt != 2 || back.Fence != p.Fence ||
		back.DeviceID != p.DeviceID || back.SpecHash != specHash(spec) || back.LastIndex != 0 {
		t.Fatalf("the checkpoint came back as %+v, want the one that was written: %+v", back, c)
	}
	if len(back.Completed) != 1 || back.Completed[0].Index != 0 || back.Completed[0].ID != "clean" {
		t.Fatalf("the completed list came back as %+v", back.Completed)
	}
	if back.InFlight != nil {
		t.Fatalf("markDone cleared the in-flight marker in memory but not through the column: %+v", back.InFlight)
	}
	if !back.belongsTo(p) {
		t.Fatal("the placement that wrote this checkpoint could not resume from it")
	}

	pl, err := planResume(spec, back, kinds, true)
	if err != nil {
		t.Fatalf("planResume refused the checkpoint Postgres returned: %v", err)
	}
	if !pl.skip[0] {
		t.Fatal("the step recorded complete would be run a second time")
	}
	for i := 1; i < len(spec.Steps); i++ {
		if pl.skip[i] {
			t.Fatalf("step %d was skipped although nothing recorded it as done", i)
		}
	}

	// The other half of the record: a step that had begun and had not
	// finished. It goes down BEFORE a non-idempotent step runs, so that "we do
	// not know whether this happened" is a stored fact rather than an inference
	// from silence — and it has to survive the column to be one.
	c.markInFlight(1, spec.Steps[1])
	r.saveCheckpoint(ctx, r.log, p, c)

	back = parseCheckpoint(readCheckpointColumn(t, pool, f.jobID))
	if back.InFlight == nil || back.InFlight.Index != 1 || back.InFlight.ID != "seed" ||
		back.InFlight.Kind != jobspec.KindShell {
		t.Fatalf("the in-flight marker came back as %+v", back.InFlight)
	}
	pl, err = planResume(spec, back, kinds, true)
	if err == nil {
		t.Fatal("a resume planned from the column agreed to re-run an interrupted shell step")
	}
	if !strings.Contains(err.Error(), "not idempotent") || pl.refusedIndex != 1 {
		t.Fatalf("refusal = %q at index %d, want it to name the step and why", err, pl.refusedIndex)
	}
}

// updated_at is stamped by Postgres inside the same statement that writes the
// progress, and the proof is that nothing about a time leaves this process: the
// Checkpoint the runner marshals has no field for one. That is what stops a
// runner whose clock is an hour fast from making its own progress look like the
// newest there is — the value is not one it is in a position to state.
//
// The window it must land in is read from the server on either side of the
// write, so the test never has an opinion about what time it is either.
//
// Falsify: give Checkpoint an UpdatedAt field the runner fills in from
// time.Now() — the body it sends starts carrying a timestamp of its own, which
// is exactly the refactor this guards against.
func TestTheCheckpointsTimestampComesFromTheServerAndNotFromTheRunner(t *testing.T) {
	pool := requireDB(t)
	f := newJobFixture(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	r, _ := testRunner(t, func(c *Config) { c.Pool = pool })
	p := Placement{JobID: f.jobID, DeviceID: "dev-1", Devpath: "usb:3-1.4", Fence: 3}
	c := newCheckpoint(1, p, resumeSpec())

	body, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshalling the checkpoint: %v", err)
	}
	if bytes.Contains(body, []byte("updated_at")) {
		t.Fatalf("the runner sent a time of its own with the progress: %s", body)
	}

	serverNow := func() time.Time {
		t.Helper()
		var at time.Time
		if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&at); err != nil {
			t.Fatalf("asking the server what time it is: %v", err)
		}
		return at
	}

	before := serverNow()
	r.saveCheckpoint(ctx, r.log, p, c)
	after := serverNow()

	var stamped bool
	var at time.Time
	if err := pool.QueryRow(ctx, `
SELECT checkpoint ? 'updated_at',
       COALESCE((checkpoint->>'updated_at')::timestamptz, 'epoch'::timestamptz)
  FROM farm.jobs WHERE id = $1::uuid`, f.jobID).Scan(&stamped, &at); err != nil {
		t.Fatalf("reading the checkpoint timestamp: %v", err)
	}
	if !stamped {
		t.Fatal("the checkpoint carries no updated_at; nothing says when this progress was recorded")
	}
	if at.Before(before) || at.After(after) {
		t.Fatalf("updated_at is %s, outside the window the server itself passed through (%s..%s)",
			at, before, after)
	}
}

// The fence guard, which is the most safety-critical line in checkpoint.go.
//
// A process that lost its lease an hour ago and is only now getting its
// statement through must not overwrite the progress of the attempt that
// replaced it: its fence is below the one recorded, so the UPDATE matches
// nothing. Without this the zombie's older, shorter list of completed steps
// lands on top of its replacement's, and the replacement then re-runs work it
// had already done — on a device somebody else is now driving.
//
// Zero rows is an ordinary outcome, logged and not raised: by the time it
// happens there is nothing left to abort and the server's copy is the newer one
// anyway.
//
// Falsify: delete `AND COALESCE((checkpoint->>'fence')::bigint, 0) <= $3` from
// saveCheckpoint and watch the zombie's single completed step replace its
// replacement's two.
func TestAStaleHolderCannotOverwriteTheProgressOfTheAttemptThatReplacedIt(t *testing.T) {
	pool := requireDB(t)
	f := newJobFixture(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	r, logs := testRunner(t, func(c *Config) { c.Pool = pool })
	spec := resumeSpec()

	// One job, one device, two lease incarnations. The zombie is the evicted
	// process whose statement is still in flight; live is the attempt that
	// actually holds the device now.
	zombie := Placement{JobID: f.jobID, DeviceID: "dev-1", Devpath: "usb:3-1.4", Fence: 7}
	live := zombie
	live.Fence = 8

	// progress returns a checkpoint for p with steps 0..upto recorded done.
	progress := func(p Placement, attempt, upto int) Checkpoint {
		c := newCheckpoint(attempt, p, spec)
		for i := 0; i <= upto; i++ {
			c.markDone(i, spec.Steps[i])
		}
		return c
	}
	stored := func() Checkpoint { return parseCheckpoint(readCheckpointColumn(t, pool, f.jobID)) }

	r.saveCheckpoint(ctx, r.log, live, progress(live, 2, 1))
	if got := stored(); got.Fence != live.Fence || len(got.Completed) != 2 {
		t.Fatalf("the live attempt's own progress did not land: %+v", got)
	}

	r.saveCheckpoint(ctx, r.log, zombie, progress(zombie, 1, 0))
	if got := stored(); got.Fence != live.Fence || got.Attempt != 2 || len(got.Completed) != 2 {
		t.Fatalf("a holder at fence %d overwrote the progress of the attempt at fence %d: %+v",
			zombie.Fence, live.Fence, got)
	}
	// The line an operator reads. A silent refusal here would look exactly like
	// a checkpoint that was never written at all.
	if n := logs.count("checkpoint not written: a newer fence holds this job"); n != 1 {
		t.Fatalf("the refused write was logged %d time(s), want exactly 1", n)
	}

	// Equal is not stale. It is the ordinary case: the same holder saving after
	// every step of its own run, at the fence it has held all along.
	r.saveCheckpoint(ctx, r.log, live, progress(live, 2, 2))
	if got := stored(); len(got.Completed) != 3 {
		t.Fatalf("a holder was fenced out by its own fence: %+v", got)
	}

	// And the next incarnation, at a higher fence, writes freely: it is the one
	// holding the device, and its record of what has been done is the one that
	// counts from here.
	next := live
	next.Fence = 9
	r.saveCheckpoint(ctx, r.log, next, progress(next, 3, 0))
	if got := stored(); got.Fence != next.Fence || got.Attempt != 3 || len(got.Completed) != 1 {
		t.Fatalf("a newer fence could not record its own progress: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// A resume, driven across a real interruption
// ---------------------------------------------------------------------------

// A pod eviction is the most ordinary event in a Kubernetes control plane, and
// this is what it costs the job: nothing. The replacement acquires the same
// lease by job id, gets the same device at the same fence, reads out of the
// column what its predecessor left there, and carries on — without spending an
// attempt and without re-running the step that already ran.
//
// Both halves are real placements: two Runner.Run calls against one job, with
// an interruption in between, sharing nothing but farm.jobs. The interruption
// lands inside an assert, which the vocabulary calls idempotent, so no
// in-flight marker is written for it and what the replacement inherits is one
// completed step and a clean slate after it.
//
// Falsify: make execute ignore plan.skip and run every step, and watch "echo
// one" turn up in the second process's command list.
func TestAnEvictedRunResumesWithoutRepeatingWhatItFinished(t *testing.T) {
	pool := requireDB(t)
	f := newJobFixture(t, pool)
	p := seedPlacement(t, pool, f)

	const probe = "getprop sys.boot_completed"
	spec := jobspec.New(
		jobspec.Step{ID: "one", Payload: jobspec.Shell{Command: "echo one"}},
		jobspec.Step{ID: "probe", Payload: jobspec.Assert{
			Probe: probe, Operator: jobspec.OpEQ, Value: "1"}},
		jobspec.Step{ID: "three", Payload: jobspec.Shell{Command: "echo three"}},
	)
	// A budget short enough that a broken test fails instead of hanging, and
	// long enough that nothing on the healthy path ever reaches it.
	spec.DefaultTimeout = jobspec.Duration(10 * time.Second)
	storeSpec(t, pool, f.jobID, spec)

	r, _ := testRunner(t, func(c *Config) { c.Pool = pool })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// --- the process that gets evicted --------------------------------------
	held, evict := context.WithCancel(ctx)
	defer evict()

	dev1, reached := deviceThatBlocksOn(probe, nil)
	go func() {
		<-reached
		evict()
	}()

	out1, err := r.Run(ctx, fakeHolder{ctx: held}, p, dev1)
	if err != nil {
		t.Fatalf("the first placement could not do its bookkeeping: %v", err)
	}
	if out1.State != StateAbandoned {
		t.Fatalf("the evicted placement ended %q (%s), want abandoned: it has no verdict to give",
			out1.State, out1.Error)
	}
	if out1.ReleaseReason != "" {
		t.Fatalf("an evicted placement asked for the lease to be released (%q); "+
			"the replacement needs that lease to resume", out1.ReleaseReason)
	}
	if out1.Attempt != 1 {
		t.Fatalf("the first placement ran as attempt %d, want 1", out1.Attempt)
	}

	// What the replacement will find. Checked before the second Run, because a
	// resume that works for the wrong reason is worth catching: this is the
	// record, and this is the plan it produces.
	back := parseCheckpoint(readCheckpointColumn(t, pool, f.jobID))
	if !back.belongsTo(p) {
		t.Fatalf("the checkpoint left behind does not describe this placement: %+v", back)
	}
	if back.Attempt != 1 || back.LastIndex != 0 {
		t.Fatalf("checkpoint = %+v, want attempt 1 stopped at step 0", back)
	}
	if len(back.Completed) != 1 || back.Completed[0].ID != "one" {
		t.Fatalf("completed = %+v, want the one step that finished", back.Completed)
	}
	if back.InFlight != nil {
		t.Fatalf("an idempotent step left an in-flight marker behind: %+v", back.InFlight)
	}

	kinds, err := r.loadKinds(ctx)
	if err != nil {
		t.Fatalf("loading farm.step_kinds: %v", err)
	}
	pl, err := planResume(spec, back, kinds, true)
	if err != nil {
		t.Fatalf("the checkpoint of an interrupted idempotent step was refused: %v", err)
	}
	if !pl.skip[0] || pl.skip[1] || pl.skip[2] {
		t.Fatalf("plan = %+v, want only the finished step skipped", pl.skip)
	}

	// --- its replacement ----------------------------------------------------
	dev2 := answeringDevice(map[string]string{probe: "1"})
	out2, err := r.Run(ctx, fakeHolder{ctx: ctx}, p, dev2)
	if err != nil {
		t.Fatalf("the resumed placement could not do its bookkeeping: %v", err)
	}
	if out2.State != StateSucceeded {
		t.Fatalf("the resumed placement ended %q (%s), want succeeded", out2.State, out2.Error)
	}
	if out2.Attempt != 1 {
		t.Fatalf("the resume ran as attempt %d; a pod eviction must not spend the job's budget", out2.Attempt)
	}

	// The assertion this requirement is about: the side effect did not happen
	// twice. The second process is a different Conn with its own command log,
	// so this is what a replacement actually said to the device.
	cmds := dev2.commands()
	if slices.Contains(cmds, "echo one") {
		t.Fatalf("the replacement re-ran a step that had already finished: %v", cmds)
	}
	if !slices.Contains(cmds, probe) || !slices.Contains(cmds, "echo three") {
		t.Fatalf("the replacement did not run the work that was left: %v", cmds)
	}
	if out2.Skipped != 1 || out2.Steps != 2 {
		t.Fatalf("out = %+v, want 1 step skipped and 2 run", out2)
	}

	attempt, state := jobStateOf(t, pool, f.jobID)
	if attempt != 1 {
		t.Fatalf("farm.jobs.attempt = %d after a resume, want 1: no attempt was claimed", attempt)
	}
	if state != "succeeded" {
		t.Fatalf("farm.jobs.state = %q, want succeeded", state)
	}

	// What an operator reads at 3am: a complete step list in which the skipped
	// row says why it was skipped. 'skipped' also means "never reached" after a
	// failure, and the reason is what tells the two apart.
	rows := stepRowsOf(t, pool, f.jobID)
	if len(rows) != 3 {
		t.Fatalf("%d step row(s), want one per step: %+v", len(rows), rows)
	}
	if rows[0].state != "skipped" || !strings.Contains(rows[0].detail, "completed in an earlier attempt") {
		t.Fatalf("step row 0 = %+v, want it skipped and saying why", rows[0])
	}
	if rows[1].state != "ok" || rows[2].state != "ok" {
		t.Fatalf("step rows 1 and 2 = %+v, %+v, want both ok", rows[1], rows[2])
	}

	// The finished job's checkpoint records every step, so a late statement
	// from the evicted process cannot make it look unfinished.
	if final := parseCheckpoint(readCheckpointColumn(t, pool, f.jobID)); len(final.Completed) != 3 {
		t.Fatalf("the final checkpoint records %+v, want all three steps", final.Completed)
	}
}

// The refusal, driven across a real interruption rather than assembled from a
// literal.
//
// The interrupted step is a shell command, which farm.step_kinds says may not
// be repeated, and the checkpoint that says so was written by a process that no
// longer exists. Re-running it would repeat a side effect that may well have
// already happened — this one is spelled as a charge on purpose. A job that
// fails with a clear message is strictly better than a farm that silently does
// things twice.
//
// The strongest thing this test says is the command count: the replacement did
// not touch the device AT ALL. The refusal happens before the work directory is
// even prepared, so there is no window in which a second charge could be sent.
//
// Falsify: make stepIsIdempotent default to true, or delete planResume's
// default arm, and watch the replacement send the charge a second time.
func TestAnEvictedRunRefusesToRepeatTheStepItWasInterruptedIn(t *testing.T) {
	pool := requireDB(t)
	f := newJobFixture(t, pool)
	p := seedPlacement(t, pool, f)

	const charge = "am broadcast -a com.acme.CHARGE_CARD"
	spec := jobspec.New(
		jobspec.Step{ID: "one", Payload: jobspec.Shell{Command: "echo one"}},
		jobspec.Step{ID: "charge", Payload: jobspec.Shell{Command: charge}},
	)
	spec.DefaultTimeout = jobspec.Duration(10 * time.Second)
	storeSpec(t, pool, f.jobID, spec)

	r, _ := testRunner(t, func(c *Config) { c.Pool = pool })
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// The premise the refusal rests on, asked of the database that will decide
	// it rather than of the Go mirror of that table. If farm.step_kinds ever
	// calls 'shell' idempotent this test stops meaning what it says, and it
	// should say so here rather than quietly assert nothing.
	kinds, err := r.loadKinds(ctx)
	if err != nil {
		t.Fatalf("loading farm.step_kinds: %v", err)
	}
	if kinds[jobspec.KindShell].Idempotent {
		t.Fatal("farm.step_kinds now calls 'shell' idempotent; this test no longer tests a refusal")
	}

	// --- the process that gets evicted, mid-charge ---------------------------
	held, evict := context.WithCancel(ctx)
	defer evict()

	dev1, reached := deviceThatBlocksOn(charge, nil)
	go func() {
		<-reached
		evict()
	}()

	out1, err := r.Run(ctx, fakeHolder{ctx: held}, p, dev1)
	if err != nil {
		t.Fatalf("the first placement could not do its bookkeeping: %v", err)
	}
	if out1.State != StateAbandoned {
		t.Fatalf("the evicted placement ended %q (%s), want abandoned", out1.State, out1.Error)
	}
	if !slices.Contains(dev1.commands(), charge) {
		t.Fatalf("the first placement never reached the step it was interrupted in: %v", dev1.commands())
	}

	// The recorded fact the refusal will rest on: this step had begun and had
	// not finished. It was written BEFORE the command went out, which is the
	// only ordering under which "we do not know" is knowable at all.
	back := parseCheckpoint(readCheckpointColumn(t, pool, f.jobID))
	if back.InFlight == nil || back.InFlight.Index != 1 || back.InFlight.ID != "charge" ||
		back.InFlight.Kind != jobspec.KindShell {
		t.Fatalf("in-flight marker = %+v, want the charge recorded as interrupted", back.InFlight)
	}

	// --- its replacement -----------------------------------------------------
	dev2 := answeringDevice(nil)
	out2, err := r.Run(ctx, fakeHolder{ctx: ctx}, p, dev2)
	if err != nil {
		t.Fatalf("the resumed placement could not do its bookkeeping: %v", err)
	}
	if n := dev2.calls(); n != 0 {
		t.Fatalf("the replacement sent %d command(s) to the device: %v", n, dev2.commands())
	}
	if out2.State != StateFailed {
		t.Fatalf("the resumed placement ended %q (%s), want failed", out2.State, out2.Error)
	}
	if !strings.Contains(out2.Error, "not idempotent") || !strings.Contains(out2.Error, "charge") {
		t.Fatalf("failure = %q, want it to name the step and why it may not be repeated", out2.Error)
	}
	if out2.ReleaseReason != lease.ReasonFailed {
		t.Fatalf("ReleaseReason = %q, want %q", out2.ReleaseReason, lease.ReasonFailed)
	}

	// Terminal, not re-queued. Re-queueing would place the job on a fresh
	// device and run the whole spec from the start — which is the very second
	// charge this refusal exists to prevent.
	attempt, state := jobStateOf(t, pool, f.jobID)
	if state != "failed" {
		t.Fatalf("farm.jobs.state = %q, want failed: a re-queued job would repeat the step", state)
	}
	if attempt != 1 {
		t.Fatalf("farm.jobs.attempt = %d, want 1: a refused resume claims no new attempt", attempt)
	}

	// The step's own row carries the refusal, so the answer to "why did this
	// job fail" is on the step that could not be repeated.
	rows := stepRowsOf(t, pool, f.jobID)
	if len(rows) != 2 {
		t.Fatalf("%d step row(s), want one per step: %+v", len(rows), rows)
	}
	if rows[1].state != "failed" || !strings.Contains(rows[1].detail, `"resume_refused": true`) {
		t.Fatalf("step row 1 = %+v, want it failed and marked as a refused resume", rows[1])
	}
	if !strings.Contains(rows[1].errText, "not idempotent") {
		t.Fatalf("step row 1 error = %q, want it to say why the step was not repeated", rows[1].errText)
	}
}
