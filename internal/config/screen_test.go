package config

// The interactive-control knobs, and the one thing they must never become.
//
// FARM_SCREEN_SESSION_TTL is a timer, in a system whose founding requirement is
// that no timer about connectivity may end a lease. The timer is legitimate —
// it frees a hardware encoder on a phone — and it is one rename away from being
// the idle timeout that LEASE-01 and STF #663 exist to forbid. So the tests
// below pin two different kinds of thing: that the all-or-none pinning of a jar
// works, and that the words this package prints about the TTL say what it ends.
//
// The second kind looks like testing a comment. It is testing the only barrier
// between a correct bound on a socket and an incorrect bound on a lease, which
// is that everybody who reads this configuration is told the difference.

import (
	"strings"
	"testing"
	"time"
)

const testSHA = "3b1f4c0d9e2a6b8c7d5e4f0a1b2c3d4e5f60718293a4b5c6d7e8f9012345678a"

// TestScreenIsOffByDefaultAndSaysSo. A farm that never heard of this feature
// must not acquire it by upgrading, and the routes must refuse with the names of
// the variables rather than with a dial error against a jar nobody pushed.
//
// Falsify: give EnvScreenServerSHA a non-empty default in Load.
func TestScreenIsOffByDefaultAndSaysSo(t *testing.T) {
	env(t, withDSN(nil))
	cfg, err := Load("api")
	if err != nil {
		t.Fatalf("a farm that set only DATABASE_URL was refused: %v", err)
	}
	if cfg.Screen.Enabled() {
		t.Error("the screen path is on with no jar pinned; the first request would push " +
			"whatever artifact happened to be in the store")
	}
	if got := cfg.Screen.describe(); !strings.Contains(got, EnvScreenServerSHA) ||
		!strings.Contains(got, EnvScreenServerVersion) {
		t.Errorf("the summary line for an unconfigured screen path is %q; it must name both "+
			"variables, because that line is the only place an operator learns why the "+
			"dashboard panel is missing", got)
	}

	// The defaults that do apply must still be usable, so that turning the
	// feature on is two variables and not six.
	if cfg.Screen.MaxSize != DefaultScreenMaxSize ||
		cfg.Screen.SessionTTL != DefaultScreenSessionTTL ||
		cfg.Screen.MaxSessions != DefaultScreenMaxSessions {
		t.Errorf("bounds did not default: max size %d, ttl %s, sessions %d",
			cfg.Screen.MaxSize, cfg.Screen.SessionTTL, cfg.Screen.MaxSessions)
	}
}

// TestPinningAJarIsAllOrNone is the pair rule, in both directions, and it
// asserts on WHICH variable each refusal names. An operator who set one of two
// needs to be told about the one they have not set; naming the one they did set
// sends them to re-read a line that is already correct.
//
// Falsify: delete either arm of the switch in Screen.problems.
func TestPinningAJarIsAllOrNone(t *testing.T) {
	t.Run("digest without a version", func(t *testing.T) {
		env(t, withDSN(map[string]string{EnvScreenServerSHA: testSHA}))
		_, err := Load("api")
		if err == nil {
			t.Fatal("Load accepted a pinned jar with no version; the server on the handset " +
				"refuses to start without one, so this is a push that cannot run")
		}
		if !strings.Contains(err.Error(), EnvScreenServerVersion) {
			t.Errorf("the refusal does not name %s, which is the variable that is missing:\n%s",
				EnvScreenServerVersion, err)
		}
	})

	t.Run("version without a digest", func(t *testing.T) {
		env(t, withDSN(map[string]string{EnvScreenServerVersion: "4.1"}))
		_, err := Load("api")
		if err == nil {
			t.Fatal("Load accepted a version with no digest; resolving a jar by name instead " +
				"would make replacing an artifact into running code on every handset")
		}
		if !strings.Contains(err.Error(), EnvScreenServerSHA) {
			t.Errorf("the refusal does not name %s:\n%s", EnvScreenServerSHA, err)
		}
	})
}

// TestAMistypedDigestIsRefusedAtBootNotAtTheFirstScreen. The digest also has to
// pass the blob store's path check before it can name a file, so a value that is
// not 64 hex digits names nothing anywhere. Finding that out at boot costs a
// failed deploy; finding it out on the first request costs an operator who is
// already looking for a phone that is misbehaving.
//
// Falsify: delete the isSHA256 check in Screen.problems.
func TestAMistypedDigestIsRefusedAtBootNotAtTheFirstScreen(t *testing.T) {
	for _, bad := range []string{
		strings.ToUpper(testSHA), // uppercase: hex, but not the shape the store writes
		testSHA[:63],             // one short
		testSHA + "a",            // one long
		"../../etc/passwd",       // the reason the shape is a gate and not a nicety
		strings.Repeat("g", 64),  // right length, not hex
	} {
		env(t, withDSN(map[string]string{
			EnvScreenServerSHA:     bad,
			EnvScreenServerVersion: "4.1",
		}))
		_, err := Load("api")
		if err == nil {
			t.Errorf("Load accepted %s = %q", EnvScreenServerSHA, bad)
			continue
		}
		if !strings.Contains(err.Error(), EnvScreenServerSHA) {
			t.Errorf("%q was refused without naming %s:\n%s", bad, EnvScreenServerSHA, err)
		}
	}
}

// TestTheSessionBoundsAreBounded. Each of these is a number that reaches a
// handset's encoder or this process's memory, and each has a value that is
// syntactically fine and operationally useless.
//
// Falsify: delete any one of the four range checks in Screen.problems.
func TestTheSessionBoundsAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		kv   map[string]string
		want string
	}{
		{"max size below the floor", map[string]string{EnvScreenMaxSize: "16"}, EnvScreenMaxSize},
		{"max size above the ceiling", map[string]string{EnvScreenMaxSize: "8192"}, EnvScreenMaxSize},
		{"a session with no bound", map[string]string{EnvScreenSessionTTL: "0s"}, EnvScreenSessionTTL},
		{"a negative session bound", map[string]string{EnvScreenSessionTTL: "-1m"}, EnvScreenSessionTTL},
		{"no sessions admitted", map[string]string{EnvScreenMaxSessions: "0"}, EnvScreenMaxSessions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := map[string]string{
				EnvScreenServerSHA:     testSHA,
				EnvScreenServerVersion: "4.1",
			}
			for k, v := range tc.kv {
				kv[k] = v
			}
			env(t, withDSN(kv))
			_, err := Load("api")
			if err == nil {
				t.Fatalf("Load accepted %v", tc.kv)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name %s:\n%s", tc.want, err)
			}
		})
	}
}

// TestTheSummarySaysTheTTLEndsBytesAndNotALease is the test this file exists
// for.
//
// FARM_SCREEN_SESSION_TTL is a timer that fires on silence. The founding
// invariant of this system is that no such timer may end a lease:
// farm.leases.release_reason has seven allowed values and none of them is about
// a connection. The mechanism that keeps this TTL on the right side of that line
// is not a type — a time.Duration cannot tell you what it bounds — it is that
// every operator and every future author who reads the startup block is told, in
// the same breath as the number, that it stops bytes.
//
// So this asserts on prose. That is deliberate. Delete the sentence and the next
// person to add "and release the device" to the session reaper will have no
// reason not to.
//
// Falsify: remove the parenthetical from Screen.describe.
func TestTheSummarySaysTheTTLEndsBytesAndNotALease(t *testing.T) {
	env(t, withDSN(map[string]string{
		EnvScreenServerSHA:     testSHA,
		EnvScreenServerVersion: "4.1",
	}))
	cfg, err := Load("api")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Screen.Enabled() {
		t.Fatal("a farm with both variables set has the screen path off")
	}

	line := cfg.Screen.describe()
	for _, want := range []string{"BYTES", "no lease is ended"} {
		if !strings.Contains(line, want) {
			t.Errorf("the screen summary line does not contain %q:\n  %s\n\n"+
				"That sentence is the only thing standing between a correct bound on a socket "+
				"and an idle timeout on a lease. LEASE-01 says a lease ends when the job says "+
				"so, when a written-down deadline elapses, or when a human takes it back — "+
				"never because of connectivity.", want, line)
		}
	}

	// And the whole startup block must carry it, since that is where an
	// operator actually reads it.
	if s := cfg.Summary(); !strings.Contains(s, "screen           =") {
		t.Error("Summary has no screen line; the block an operator reads at startup does not " +
			"mention the feature at all")
	}
}

// TestTheControlCertificateGetsItsOwnSummaryLine. Two certificates present two
// classes from one process. A summary that printed only the maintenance client
// would let a farm run with the screen path dialling nothing while the line
// above it reported the fence as configured — which is the same farm reading
// "fence client = mTLS" and concluding the screen ought to work.
//
// Falsify: delete the "fence control" Fprintf in Summary.
func TestTheControlCertificateGetsItsOwnSummaryLine(t *testing.T) {
	env(t, withDSN(nil))
	cfg, err := Load("api")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s := cfg.Summary()
	if !strings.Contains(s, "fence control    =") {
		t.Fatal("Summary does not describe the control-class certificate separately from the " +
			"maintenance one")
	}
	// An unconfigured control client must name ITS OWN variables, not the
	// maintenance pair — that is what FenceClient.certVar is for.
	if !strings.Contains(s, EnvFenceControlCert) {
		t.Errorf("the control line does not name %s, so an operator is sent to the wrong "+
			"variable:\n%s", EnvFenceControlCert, s)
	}
}

// TestSessionTTLAcceptsAPlainDuration is here because the knob is a duration in
// a file full of durations and a reviewer should not have to wonder.
func TestSessionTTLAcceptsAPlainDuration(t *testing.T) {
	env(t, withDSN(map[string]string{
		EnvScreenServerSHA:     testSHA,
		EnvScreenServerVersion: "4.1",
		EnvScreenSessionTTL:    "90s",
		EnvScreenMaxSize:       "720",
		EnvScreenMaxSessions:   "2",
	}))
	cfg, err := Load("api")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Screen.SessionTTL != 90*time.Second || cfg.Screen.MaxSize != 720 || cfg.Screen.MaxSessions != 2 {
		t.Errorf("ttl %s, max size %d, sessions %d", cfg.Screen.SessionTTL,
			cfg.Screen.MaxSize, cfg.Screen.MaxSessions)
	}
}
