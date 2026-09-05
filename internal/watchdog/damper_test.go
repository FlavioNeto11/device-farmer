package watchdog

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/flaviopadilha/device-farmer/internal/obs"
)

// The damper has one definition. These tests hold the three edges of that
// claim: the Go mirror says what the SQL says (over a grid, on a real server),
// the reconcile statement is built on the SQL and not on a copy of it, and the
// arms mean what the package comment promises.

// TestDampArms is the package comment as assertions, one arm at a time.
//
// Falsify: swap the order of the "falling" and "rising" arms in Damp, or drop
// the credits >= 1 clause from the 'unknown' arm.
func TestDampArms(t *testing.T) {
	t.Parallel()

	base := DampInput{MinBad: DefaultMinBad, MinGood: DefaultMinGood, Credits: 10}
	with := func(f func(*DampInput)) DampInput {
		in := base
		f(&in)
		return in
	}

	cases := []struct {
		name string
		in   DampInput
		want obs.HealthState
	}{
		{"retired is administrative and never overwritten",
			with(func(i *DampInput) { i.Current, i.Candidate, i.ConsecGood = obs.HealthRetired, obs.HealthHealthy, 9 }),
			obs.HealthRetired},
		{"quarantined belongs to the ladder",
			with(func(i *DampInput) { i.Current, i.Candidate, i.ConsecGood = obs.HealthQuarantined, obs.HealthHealthy, 9 }),
			obs.HealthQuarantined},
		{"parked is out of service on purpose, and absence is not evidence",
			with(func(i *DampInput) { i.Current, i.Candidate, i.ConsecBad = obs.HealthParked, obs.HealthMissing, 9 }),
			obs.HealthParked},
		{"a suppressed drop proves nothing",
			with(func(i *DampInput) {
				i.Current, i.Candidate, i.ConsecBad, i.Suppressed = obs.HealthHealthy, obs.HealthOffline, 9, true
			}),
			obs.HealthHealthy},
		{"a suppressed recovery is not believed either",
			with(func(i *DampInput) {
				i.Current, i.Candidate, i.ConsecGood, i.Suppressed = obs.HealthRecovering, obs.HealthHealthy, 9, true
			}),
			obs.HealthRecovering},
		{"no change is no change",
			with(func(i *DampInput) { i.Current, i.Candidate = obs.HealthOffline, obs.HealthOffline }),
			obs.HealthOffline},
		{"unknown needs one good look and a token",
			with(func(i *DampInput) { i.Current, i.Candidate, i.ConsecGood = obs.HealthUnknown, obs.HealthHealthy, 1 }),
			obs.HealthHealthy},
		{"unknown with an empty bucket waits",
			with(func(i *DampInput) {
				i.Current, i.Candidate, i.ConsecGood, i.Credits = obs.HealthUnknown, obs.HealthHealthy, 1, 0.5
			}),
			obs.HealthUnknown},
		{"one bad observation is not a fall",
			with(func(i *DampInput) { i.Current, i.Candidate, i.ConsecBad = obs.HealthHealthy, obs.HealthOffline, 1 }),
			obs.HealthHealthy},
		{"falling is free: an empty bucket does not keep a failing device schedulable",
			with(func(i *DampInput) {
				i.Current, i.Candidate, i.ConsecBad, i.Credits = obs.HealthHealthy, obs.HealthOffline, 2, 0
			}),
			obs.HealthOffline},
		{"rising needs the hysteresis",
			with(func(i *DampInput) { i.Current, i.Candidate, i.ConsecGood = obs.HealthOffline, obs.HealthHealthy, 2 }),
			obs.HealthOffline},
		{"rising needs a token even after the hysteresis",
			with(func(i *DampInput) {
				i.Current, i.Candidate, i.ConsecGood, i.Credits = obs.HealthOffline, obs.HealthHealthy, 3, 0.9
			}),
			obs.HealthOffline},
		{"rising with both",
			with(func(i *DampInput) {
				i.Current, i.Candidate, i.ConsecGood, i.Credits = obs.HealthOffline, obs.HealthHealthy, 3, 1
			}),
			obs.HealthHealthy},
		{"a bad-to-bad move is still a fall, debounced",
			with(func(i *DampInput) { i.Current, i.Candidate, i.ConsecBad = obs.HealthOffline, obs.HealthMissing, 2 }),
			obs.HealthMissing},
	}
	for _, tc := range cases {
		if got := Damp(tc.in); got != tc.want {
			t.Errorf("%s: Damp(%+v) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestReconcileStatementIsBuiltOnDamperSQL is what makes DamperSQL the real
// damper rather than a documentation copy of it: the statement write executes
// contains the exported expression verbatim, and write executes that statement
// and no inline one.
//
// Falsify: paste the CASE back into reconcileSQL instead of concatenating
// DamperSQL, or give write a local `const q` again.
func TestReconcileStatementIsBuiltOnDamperSQL(t *testing.T) {
	t.Parallel()

	if !strings.Contains(reconcileSQL, DamperSQL) {
		t.Fatal("reconcileSQL does not embed DamperSQL: the watchdog is running a damper " +
			"other than the one it exports, and every other writer of health is mirroring the wrong one")
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "watchdog.go", nil, 0)
	if err != nil {
		t.Fatalf("parse watchdog.go: %v", err)
	}
	var write *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "write" && fd.Recv != nil {
			write = fd
		}
	}
	if write == nil {
		t.Fatal("watchdog.go has no (*Watchdog).write")
	}
	usesShared, inlineSQL := false, false
	ast.Inspect(write.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if x.Name == "reconcileSQL" {
				usesShared = true
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING && strings.Contains(x.Value, "device_runtime") {
				inlineSQL = true
			}
		}
		return true
	})
	if !usesShared {
		t.Error("(*Watchdog).write does not reference reconcileSQL")
	}
	if inlineSQL {
		t.Error("(*Watchdog).write carries an inline statement against farm.device_runtime; " +
			"the reconcile must be the package-level reconcileSQL so the damper it runs is the one exported")
	}
}

// TestDamperSQLAgreesWithDamp evaluates DamperSQL on a real PostgreSQL over a
// grid of every input the expression reads and compares each verdict with
// Damp. It needs only a connection, not the schema: the expression is
// evaluated over a VALUES row source.
//
// Falsify: change any one arm of either definition — e.g. make Damp's
// 'unknown' arm ignore credits — and the grid reports the cells that differ.
func TestDamperSQLAgreesWithDamp(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; the SQL half of the damper cannot be evaluated")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open DATABASE_URL: %v", err)
	}
	defer pool.Close()

	currents := []obs.HealthState{
		obs.HealthUnknown, obs.HealthBooting, obs.HealthHealthy, obs.HealthDegraded,
		obs.HealthOffline, obs.HealthUnauthorized, obs.HealthMissing, obs.HealthRecovering,
		obs.HealthParked, obs.HealthQuarantined, obs.HealthRetired,
	}
	candidates := []obs.HealthState{
		obs.HealthHealthy, obs.HealthOffline, obs.HealthUnauthorized, obs.HealthBooting,
		obs.HealthMissing, obs.HealthRecovering, obs.HealthDegraded, obs.HealthUnknown,
	}
	consecs := []int{0, 1, 2, 3}
	credits := []float64{0, 0.5, 1, 10}
	thresholds := [][2]int{{DefaultMinBad, DefaultMinGood}, {1, 1}}

	var (
		curs, cands             []string
		supps, bads             []bool
		cbads, cgoods, mbs, mgs []int32
		creds                   []float64
		inputs                  []DampInput
	)
	for _, cur := range currents {
		for _, cand := range candidates {
			for _, supp := range []bool{false, true} {
				for _, cb := range consecs {
					for _, cg := range consecs {
						for _, cr := range credits {
							for _, th := range thresholds {
								in := DampInput{Current: cur, Candidate: cand, Suppressed: supp,
									ConsecBad: cb, ConsecGood: cg, Credits: cr, MinBad: th[0], MinGood: th[1]}
								inputs = append(inputs, in)
								curs = append(curs, string(cur))
								cands = append(cands, string(cand))
								supps = append(supps, supp)
								bads = append(bads, cand != obs.HealthHealthy)
								cbads = append(cbads, int32(cb))
								cgoods = append(cgoods, int32(cg))
								creds = append(creds, cr)
								mbs = append(mbs, int32(th[0]))
								mgs = append(mgs, int32(th[1]))
							}
						}
					}
				}
			}
		}
	}

	rows, err := pool.Query(ctx, `
SELECT `+DamperSQL+`
  FROM unnest($1::text[], $2::boolean[], $3::text[], $4::boolean[], $5::int[], $6::int[],
              $7::numeric[], $8::int[], $9::int[])
       AS c(cur_health, suppressed, candidate, bad, consec_bad, consec_good, credits, min_bad, min_good)`,
		curs, supps, cands, bads, cbads, cgoods, creds, mbs, mgs)
	if err != nil {
		t.Fatalf("evaluate DamperSQL: %v", err)
	}
	defer rows.Close()

	i, mismatches := 0, 0
	for rows.Next() {
		var got string
		if err := rows.Scan(&got); err != nil {
			t.Fatalf("scan verdict %d: %v", i, err)
		}
		if i >= len(inputs) {
			t.Fatalf("DamperSQL returned more rows than inputs")
		}
		if want := Damp(inputs[i]); obs.HealthState(got) != want {
			mismatches++
			if mismatches <= 10 {
				t.Errorf("cell %d %+v: SQL says %q, Damp says %q", i, inputs[i], got, want)
			}
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("evaluate DamperSQL: %v", err)
	}
	if i != len(inputs) {
		t.Fatalf("DamperSQL returned %d verdicts for %d inputs", i, len(inputs))
	}
	if mismatches > 0 {
		t.Fatalf("%d of %d cells disagree between DamperSQL and Damp", mismatches, len(inputs))
	}
}
