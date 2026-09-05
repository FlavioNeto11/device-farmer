// Package e2e is the acceptance harness: it drives the SHIPPED farmd binary as
// real operating-system processes, against a real PostgreSQL, over real
// sockets.
//
// # Why processes and not httptest
//
// Every other test in this tree runs the control plane in-process, and they are
// the right shape for what they assert: internal/scheduler proves what
// farm.lease_acquire does under concurrency, internal/jobrunner proves that no
// transport failure can name a release reason. What none of them can prove is
// that the thing an operator deploys works — that `farmd api` reads
// FARM_API_TOKENS, that `farmd scheduler` and `farmd jobrunner` in SEPARATE
// processes agree about a lease, that `farmd ctl` talks to the API a real
// deployment serves, that the embedded migrations produce the schema the roles
// query. An in-process test wires the packages together itself and so cannot
// fail when cmd/farmd wires them wrongly; two of the bugs this repository has
// already fixed — a metrics listener no role bound, a witness cadence no role
// passed — lived exactly there, in the gap between a package that was correct
// and a binary that never called it.
//
// So the harness starts subprocesses. The only fake is the hardware: one
// [github.com/flaviopadilha/device-farmer/test/fakeadb] server per seeded host,
// in this test process, speaking the real ADB host protocol on a real loopback
// socket, with farm.hosts.adb_endpoint pointed at its real address. Everything
// above the socket is the shipped binary.
//
// # One database per scenario
//
// newFarm creates, migrates and drops a database named after the test. That
// is not tidiness. The reaper, the janitor and the recovery ladder are
// farm-wide sweeps: two scenarios sharing a database would have each other's
// fixtures swept, and every assertion of the form "the sweep closed this row"
// would quietly degrade into "some sweep closed this row" — which is an
// assertion that passes when the system is broken.
//
// # What a scenario may assume
//
// Without DATABASE_URL every scenario skips, and TestMain does not even build
// the binary: `go test ./...` has to stay green on a laptop with no Postgres or
// it stops being run at all.
//
// With DATABASE_URL set, a scenario gets a farm whose physical tree came from
// internal/demo's seeder — the same racks, hubs, slots, power domains, clone
// pair and faulty hub the demo shows — hardware that answers, the roles it
// asked for running as processes, and a fixture that reports, rather than
// hides, a role that died on its own.
//
// # The founding invariant, restated for scenario authors
//
// A lease ends when the job says so, when a user-written deadline elapses, or
// when a human takes it back. Nothing else. The harness deliberately offers no
// helper that ends one: a scenario that wants a lease gone must make the job
// end, let the deadline elapse, or call the operator's own revoke route through
// the API, exactly as a human would.
//
// The assertion to reach for is farm.lease_ended_by(release_reason), which
// classifies an ending as job, deadline, operator, reaper or unaccounted. Read
// it with readLeases in smoke_test.go, which quotes every lease row of a job:
// the shape a broken farm fails in is a SECOND lease row, or a first one that
// ended with somebody else's reason.
package e2e
