package recovery

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// This file guards three more fidelity properties of the recovery ladder, on
// top of the three in ladder_fidelity_test.go, and the same invariant outranks
// all of them.
//
//	D  the VERDICT spends the rung, not the attempt. begin advances
//	   farm.device_runtime.ladder_tier before perform runs, because the column
//	   must move under the per-device lock and the action must not run inside
//	   a transaction. Nothing then looked at what perform found, so a refused
//	   rung — no host agent, a kernel without USBDEVFS_RESET, a ganged hub — and
//	   an unreachable host both climbed the ladder exactly as a broken handset
//	   does. Disposition.Escalate existed to answer this and had no caller.
//	E  a ganged refusal is typed end to end. The agent's answer was prose behind
//	   a generic 409; the client wrapped every 409 the same way; the ladder's
//	   metric counted "the rack needs per-port switching" under the label for
//	   "a lease's policy said no".
//	F  farm.v_hub_health.unhealthy is the hub quorum's predicate. The view was a
//	   deny-list and counted quarantined, unknown and recovering devices, so a
//	   fully quarantined hub read 8/8 on the banner and 0/8 in the quorum.
//
// The invariant: a lease ends when the job says so, when a user-written deadline
// elapses, or when a human takes it back. Nothing else. None of these fixes
// may end, shorten or touch a lease; TestRecoveryNeverTouchesALease already
// covers every SQL literal in this package, including the ones added here.

// ---------------------------------------------------------------------------
// Defect D: the verdict spends the rung
// ---------------------------------------------------------------------------

// TestOnlyABrokenHandsetSpendsTheRung pins rungSpent to Disposition.Escalate:
// the ladder climbs on failed and no_change — the rung ran and the hardware is
// still broken — and on nothing else.
//
// Falsify: make rungSpent return true for DispositionRefused, or replace the
// Escalate call with `out != OutcomeRefused` (unreachable then climbs).
func TestOnlyABrokenHandsetSpendsTheRung(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		out    Outcome
		detail map[string]any
		spent  bool
	}{
		{"failed", OutcomeFailed, map[string]any{DetailDisposition: "failed"}, true},
		{"no_change", OutcomeNoChange, map[string]any{DetailDisposition: "no_change"}, true},
		{"refused", OutcomeRefused,
			map[string]any{DetailDisposition: "refused", DetailRefusal: "no host agent"}, false},
		{"unreachable", OutcomeRefused,
			map[string]any{DetailDisposition: "unreachable", DetailRefusal: "nothing answered"}, false},
		{"recovered", OutcomeRecovered, map[string]any{DetailDisposition: "recovered"}, false},
		{"aborted", OutcomeAborted, map[string]any{DetailDisposition: "aborted"}, false},
		// The ladder's own database rungs record no disposition and are read
		// through their outcome, as is any third-party Actuator.
		{"observe", OutcomeNoChange, map[string]any{"action": "observe"}, true},
		{"bare failed", OutcomeFailed, nil, true},
		{"bare refused", OutcomeRefused, nil, false},
	}
	for _, tc := range cases {
		if got := rungSpent(tc.out, tc.detail); got != tc.spent {
			t.Errorf("%s: rungSpent = %v, want %v — only evidence that the hardware is still "+
				"broken may move the ladder to a more disruptive rung", tc.name, got, tc.spent)
		}
	}
}

// TestFinishHandsBackTheRungInTheAttemptsTransaction is the source-shape half
// of defect D: the compensating write exists, it is guarded on the value begin
// wrote, and it commits with the attempt row rather than beside it.
//
// Falsify, one at a time: delete the device_runtime UPDATE; drop the
// `AND ladder_tier = $3::int` guard; move the UPDATE after tx.Commit; write
// updated_at from it.
func TestFinishHandsBackTheRungInTheAttemptsTransaction(t *testing.T) {
	t.Parallel()

	body := funcBody(t, "ladder.go", "finish")

	mustContain := []struct {
		frag string
		why  string
	}{
		{"rungSpent(", "the decision must be the disposition's, through Escalate, not a " +
			"switch on the outcome column"},
		{"l.cfg.Pool.Begin(", "the row and the rung must land together"},
		{"UPDATE farm.recovery_attempts", "the attempt row is still closed here"},
		{"UPDATE farm.device_runtime", "the compensating write that hands the rung back"},
		{"SET ladder_tier = $2::int", "back to the rung that was NOT spent, i.e. t.Tier"},
		{"AND ladder_tier = $3::int", "guarded on what begin wrote: a reset to 0 made in " +
			"the meantime by reconcileQuarantines was made on better information and stands"},
		{"rungAfter(t)", "the guard value is begin's, derived the same way"},
		{"tx.Commit(", "the transaction has to commit"},
	}
	for _, m := range mustContain {
		if !strings.Contains(body, m.frag) {
			t.Fatalf("finish no longer contains %q: %s", m.frag, m.why)
		}
	}

	begin := strings.Index(body, "l.cfg.Pool.Begin(")
	attempts := strings.Index(body, "UPDATE farm.recovery_attempts")
	runtime := strings.Index(body, "UPDATE farm.device_runtime")
	commit := strings.Index(body, "tx.Commit(")
	if !(begin < attempts && attempts < runtime && runtime < commit) {
		t.Fatalf("finish's statements are out of order (begin=%d, attempts=%d, runtime=%d, "+
			"commit=%d); the rung must be handed back inside the transaction that closes "+
			"the row, or a row saying \"not spent\" can sit beside a column that climbed",
			begin, attempts, runtime, commit)
	}

	// The compensating statement writes the rung and nothing else. updated_at
	// is the watchdog's signature on a health value; suppress_until is what
	// paces a rung the farm cannot perform; health is not this loop's to
	// claim from here.
	stmt := body[runtime:]
	stmt = stmt[:strings.Index(stmt, "`")]
	for _, forbidden := range []string{"updated_at", "suppress_until", "health"} {
		if strings.Contains(stmt, forbidden) {
			t.Fatalf("the hand-back statement writes %q:\n%s", forbidden, stmt)
		}
	}
}

// scriptedActuator answers every rung with one prepared Result, so a test can
// choose the verdict and watch what the ladder does with it.
type scriptedActuator struct{ res Result }

func (s scriptedActuator) Recover(context.Context, Action) (Result, error) {
	// A fresh map per call: the ladder writes into Detail.
	detail := make(map[string]any, len(s.res.Detail))
	for k, v := range s.res.Detail {
		detail[k] = v
	}
	return Result{Outcome: s.res.Outcome, Detail: detail}, nil
}

// scratchPool connects to DATABASE_URL, or skips: this half of defect D can
// only be shown against the real begin/perform/finish sequence, and that
// sequence is three transactions and an advisory lock.
func scratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("no DATABASE_URL; this case needs a real, migrated database")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedLadderFixture builds one host with n offline devices on one hub and
// returns them as the candidates the ladder would have selected. The host id
// is unique per run and every row is removed on cleanup, so the fixture is
// safe against a development database as well as a scratch one.
func seedLadderFixture(t *testing.T, pool *pgxpool.Pool, n int, ladderTier int) []candidate {
	t.Helper()
	ctx := context.Background()
	// ltree labels admit letters, digits and underscore only.
	host := fmt.Sprintf("u3%x", time.Now().UnixNano()&0xffffffff)

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("fixture: %v\n%s", err, q)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM farm.recovery_attempts WHERE host_id = $1`,
			`DELETE FROM farm.events WHERE device_id IN (SELECT id FROM farm.devices WHERE host_id = $1)`,
			`DELETE FROM farm.devices WHERE host_id = $1`,
			`DELETE FROM farm.slots WHERE host_id = $1`,
			`DELETE FROM farm.hosts WHERE id = $1`,
			// The pool is this run's too (its id is the host id), so the
			// database is left exactly as found: test/assertions*.sql insert
			// 'default' without ON CONFLICT and run on the same scratch DB.
			`DELETE FROM farm.pools WHERE id = $1`,
		} {
			if _, err := pool.Exec(ctx, q, host); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})

	exec(`INSERT INTO farm.hosts (id, adb_endpoint) VALUES ($1, '127.0.0.1:1')`, host)
	exec(`INSERT INTO farm.controllers (host_id, root_bus) VALUES ($1, 3)`, host)
	exec(`INSERT INTO farm.power_domains (host_id, kind, control) VALUES ($1, 'per_port', 'uhubctl')`, host)
	exec(`INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
	      SELECT $1, c.id, '3-1', 7, true FROM farm.controllers c WHERE c.host_id = $1`, host)
	exec(`INSERT INTO farm.pools (id) VALUES ($1)`, host)
	exec(`INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number, usb_path, topo_path, rack_slot)
	      SELECT $1, h.id, p.id, g, '3-1.' || g, ($1 || '.c3.p3_1.p3_1_' || g)::ltree, 'U3-P' || g
	        FROM farm.hubs h, farm.power_domains p, generate_series(1, $2::int) g
	       WHERE h.host_id = $1 AND p.host_id = $1`, host, n)
	exec(`INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id)
	      SELECT 'df-' || md5($1 || s.usb_path), 'U3-' || s.port_number, $1, $1, s.id
	        FROM farm.slots s WHERE s.host_id = $1`, host)
	// Offline for long enough to be past any debounce, at the requested rung.
	exec(`INSERT INTO farm.device_runtime
	        (device_id, host_id, slot_id, adb_state, health, health_since, ladder_tier)
	      SELECT d.id, d.host_id, d.current_slot_id, 'offline', 'offline',
	             now() - interval '10 minutes', $2::int
	        FROM farm.devices d WHERE d.host_id = $1`, host, ladderTier)

	rows, err := pool.Query(ctx, `
SELECT d.id::text, s.id, s.adb_devpath, COALESCE(s.rack_slot, ''), s.hub_id,
       hb.usb_path, s.power_domain_id
  FROM farm.devices d
  JOIN farm.slots s ON s.id = d.current_slot_id
  JOIN farm.hubs hb ON hb.id = s.hub_id
 WHERE d.host_id = $1
 ORDER BY s.port_number`, host)
	if err != nil {
		t.Fatalf("fixture read: %v", err)
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		c := candidate{Health: "offline", LadderTier: ladderTier, HostID: host, ADBEndpoint: "127.0.0.1:1"}
		if err := rows.Scan(&c.DeviceID, &c.SlotID, &c.Devpath, &c.RackSlot, &c.HubID,
			&c.HubPath, &c.PowerDomain); err != nil {
			t.Fatalf("fixture scan: %v", err)
		}
		out = append(out, c)
	}
	if len(out) != n {
		t.Fatalf("fixture produced %d candidates, want %d", len(out), n)
	}
	return out
}

// TestRefusedRungLeavesTheLadderTierIntact is defect D against the real
// begin/perform/finish sequence.
//
// Six devices start at ladder_tier 1. Each is given one rung with a different
// verdict, and the column afterwards says whether the ladder climbed:
//
//	failed, no_change   → 2: the rung ran and the hardware is still broken
//	refused, unreachable, recovered, aborted → 1: the rung was not spent
//
// Before the fix every one of them read 2, and a host whose agent had gone
// away marched its handsets to reboot and quarantine three minutes at a time.
//
// Falsify: delete the device_runtime UPDATE from finish. The refused and
// unreachable cases then read 2.
func TestRefusedRungLeavesTheLadderTierIntact(t *testing.T) {
	pool := scratchPool(t)
	ctx := context.Background()

	verdicts := []struct {
		name  string
		res   Result
		tier  int
		spent bool
	}{
		{"failed", Result{Outcome: OutcomeFailed,
			Detail: map[string]any{DetailDisposition: string(DispositionFailed)}}, 2, true},
		{"no_change", Result{Outcome: OutcomeNoChange,
			Detail: map[string]any{DetailDisposition: string(DispositionNoChange)}}, 2, true},
		{"refused", Result{Outcome: OutcomeRefused, Detail: map[string]any{
			DetailDisposition: string(DispositionRefused),
			DetailRefusal:     "tier 1 (adb_reconnect) needs an adb server and there is none"}}, 1, false},
		{"unreachable", Result{Outcome: OutcomeRefused, Detail: map[string]any{
			DetailDisposition: string(DispositionUnreachable),
			DetailRefusal:     "the adb server never answered"}}, 1, false},
		{"recovered", Result{Outcome: OutcomeRecovered,
			Detail: map[string]any{DetailDisposition: string(DispositionRecovered)}}, 1, false},
		{"aborted", Result{Outcome: OutcomeAborted,
			Detail: map[string]any{DetailDisposition: string(DispositionAborted)}}, 1, false},
	}

	cands := seedLadderFixture(t, pool, len(verdicts), 1)

	for i, v := range verdicts {
		c := cands[i]
		t.Run(v.name, func(t *testing.T) {
			l, err := New(Config{
				Pool:     pool,
				Actuator: scriptedActuator{res: v.res},
				Logger:   slog.New(slog.DiscardHandler),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			tiers, err := l.tiers(ctx)
			if err != nil {
				t.Fatalf("tiers: %v", err)
			}
			if rung := next(tiers, c.LadderTier); rung.Tier != 1 {
				t.Fatalf("the fixture expects tier 1 to be the next rung, got %d (%s); "+
					"farm.recovery_tiers on this database is not the shipped ladder",
					rung.Tier, rung.Name)
			}

			l.attempt(ctx, c, tiers)

			var tier int
			if err := pool.QueryRow(ctx,
				`SELECT ladder_tier FROM farm.device_runtime WHERE device_id = $1::uuid`,
				c.DeviceID).Scan(&tier); err != nil {
				t.Fatalf("reading ladder_tier: %v", err)
			}
			var outcome *string
			var spent *bool
			if err := pool.QueryRow(ctx, `
SELECT outcome, (detail->>'rung_spent')::bool
  FROM farm.recovery_attempts WHERE device_id = $1::uuid
 ORDER BY id DESC LIMIT 1`, c.DeviceID).Scan(&outcome, &spent); err != nil {
				t.Fatalf("reading the attempt row: %v", err)
			}
			if outcome == nil {
				t.Fatal("the attempt row was never finished")
			}
			if *outcome != string(v.res.Outcome) {
				t.Errorf("attempt outcome = %q, want %q", *outcome, v.res.Outcome)
			}
			if spent == nil || *spent != v.spent {
				t.Errorf("detail.rung_spent = %v, want %v", spent, v.spent)
			}
			if tier != v.tier {
				t.Fatalf("after a %s verdict ladder_tier = %d, want %d. A rung is spent by "+
					"evidence that the hardware is still broken, and a %s verdict is not that",
					v.name, tier, v.tier, v.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Defect E: the ganged refusal keeps its name
// ---------------------------------------------------------------------------

// TestGangedRefusalIsClassifiedByItsSentinel: a HostRunner error that wraps
// ErrRungRefusedGanged is a refusal — nothing ran — AND carries
// refusal_kind=ganged into the detail, which is what the ladder's metric and an
// operator's psql read. A refusal that wraps only ErrRungRefused carries no
// kind, so nothing downstream can mistake "no uhubctl" for "buy per-port hubs".
//
// Falsify: drop the ErrRungRefusedGanged check from classifyHostFault.
func TestGangedRefusalIsClassifiedByItsSentinel(t *testing.T) {
	t.Parallel()

	ganged := &fakeRunner{err: fmt.Errorf(
		"node: %w: cycling port 4 of hub 3-1 shares one power domain with 2 device(s) "+
			"nobody authorised", ErrRungRefusedGanged)}
	res, err := NewADBActuator(nil, ganged).Recover(context.Background(), tier4(nil))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if d := DispositionOf(res); d != DispositionRefused {
		t.Fatalf("disposition = %q, want refused: a ganged hub is not a broken phone", d)
	}
	if got := RefusalKindOf(res); got != RefusalKindGanged {
		t.Fatalf("Detail[%s] = %q, want %q", DetailRefusalKind, got, RefusalKindGanged)
	}
	if !strings.Contains(RefusalOf(res), "power domain") {
		t.Errorf("the refusal text lost the agent's own words: %q", RefusalOf(res))
	}

	plain := &fakeRunner{err: fmt.Errorf("uhubctl is not installed: %w", ErrRungRefused)}
	res, err = NewADBActuator(nil, plain).Recover(context.Background(), tier4(nil))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if d := DispositionOf(res); d != DispositionRefused {
		t.Fatalf("disposition = %q, want refused", d)
	}
	if got := RefusalKindOf(res); got != "" {
		t.Fatalf("a refusal that named no kind was given one: %q", got)
	}
}

// TestGangedRefusalCountsAsRefusedGanged pins the fold from a verdict onto
// obs's outcome label. The two refusal labels are the whole of OBS-07: a rising
// refused_ganged rate says the rack needs per-port switching, and it can only
// say that if nothing else is filed under it and nothing ganged is filed
// elsewhere.
//
// Falsify: return obs.OutcomeRefusedPolicy for every refusal in obsOutcome.
func TestGangedRefusalCountsAsRefusedGanged(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		out    Outcome
		detail map[string]any
		want   obs.RecoveryOutcome
	}{
		{"agent ganged refusal", OutcomeRefused, map[string]any{
			DetailDisposition: "refused", DetailRefusalKind: RefusalKindGanged}, obs.OutcomeRefusedGanged},
		{"agent refusal, no kind", OutcomeRefused, map[string]any{
			DetailDisposition: "refused"}, obs.OutcomeRefusedPolicy},
		{"bare refusal from a third-party actuator", OutcomeRefused, nil, obs.OutcomeRefusedPolicy},
		{"unreachable host", OutcomeRefused, map[string]any{
			DetailDisposition: "unreachable"}, obs.OutcomeFailed},
		{"recovered", OutcomeRecovered, map[string]any{DetailDisposition: "recovered"}, obs.OutcomeRecovered},
		{"failed", OutcomeFailed, map[string]any{DetailDisposition: "failed"}, obs.OutcomeFailed},
		{"no_change", OutcomeNoChange, map[string]any{DetailDisposition: "no_change"}, obs.OutcomeFailed},
		{"aborted", OutcomeAborted, map[string]any{DetailDisposition: "aborted"}, obs.OutcomeFailed},
	}
	for _, tc := range cases {
		if got := obsOutcome(tc.out, tc.detail); got != tc.want {
			t.Errorf("%s: obsOutcome = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Defect F: one predicate, three places
// ---------------------------------------------------------------------------

// TestHubHealthViewCarriesTheQuorumPredicate pins migrations/00013 and
// test/assertions_v13.sql to recovery.UnhealthyStates. The view and the quorum
// disagreed for as long as they were two lists; they stay one list only if a
// change to any of the three fails the build.
//
// Falsify: add 'recovering' to UnhealthyStates without touching the SQL, or
// put the deny-list back in the migration's Up section.
func TestHubHealthViewCarriesTheQuorumPredicate(t *testing.T) {
	t.Parallel()

	want := []string{"offline", "unauthorized", "missing", "degraded"}
	if !reflect.DeepEqual(UnhealthyStates, want) {
		t.Fatalf("UnhealthyStates = %v, want %v; the doc comment lists why each other value is "+
			"excluded — argue with it there before changing this", UnhealthyStates, want)
	}
	list := UnhealthySQL()
	if unhealthyPredicate != "r.health IN ("+list+")" {
		t.Fatalf("unhealthyPredicate = %q is not built from UnhealthyStates", unhealthyPredicate)
	}

	for _, f := range []struct {
		path string
		min  int
	}{
		// unhealthy and worst_since in the Up section.
		{"../../migrations/00013_hub_health_aligned.sql", 2},
		// c_unhealthy, and the predicate re-stated as SQL in H2.
		{"../../test/assertions_v13.sql", 2},
	} {
		src, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatalf("read %s: %v", f.path, err)
		}
		text := string(src)
		if i := strings.Index(text, "-- +goose Down"); i >= 0 {
			text = text[:i]
		}
		// Statements only: both files explain the deny-list they replaced, and
		// the explanation is entitled to quote it.
		var code []string
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				code = append(code, line)
			}
		}
		text = strings.Join(code, "\n")
		if n := strings.Count(text, list); n < f.min {
			t.Errorf("%s spells the quorum predicate %d time(s), want at least %d: it must carry "+
				"exactly %s", f.path, n, f.min, list)
		}
		if strings.Contains(text, "NOT IN ('healthy'") {
			t.Errorf("%s still carries the deny-list the view used to have; a deny-list counts "+
				"quarantined, unknown and recovering devices as hub-fault evidence", f.path)
		}
	}
}
