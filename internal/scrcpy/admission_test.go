package scrcpy

// The test this package exists to make possible.
//
// internal/fenceproxy decides admission per host-protocol frame, on a
// connection that has already completed a TLS handshake, already switched an
// ADB transport and already been checked against a live lease's fence. A spawn
// command one character outside the admitted alphabet fails THERE — inside
// somebody's session, on a phone, as a log line about a whitelist that nobody
// reading it will connect to a builder in a different package.
//
// So every command this package can build is pushed through fenceproxy.Admit
// here, with a control-class request, and a refusal fails the build. The
// coupling is real and it should be loud: if fenceproxy.control() is narrowed,
// this test goes red in the same commit rather than on a handset later.
//
// Note what this test does NOT do: it does not import fenceproxy from a
// production file. Nothing in internal/scrcpy knows what a fence is; the two
// packages meet only here, in a _test.go file, which invariants_test.go pins.

import (
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/fenceproxy"
)

const testDevpath = "usb:3-1.4"

// controlRequest is a well-formed control-class connection that has already
// switched its transport to the device it holds a fence for. Everything about
// it except the service string is the happy path, so a refusal in any test
// below is a statement about the service string and nothing else.
func controlRequest(service string) fenceproxy.Request {
	return fenceproxy.Request{
		Identity: fenceproxy.Identity{
			Subject:  "farmd-api",
			Class:    fenceproxy.ClassControl,
			NotAfter: time.Now().Add(time.Hour),
		},
		Claim: fenceproxy.Claim{
			Class:    fenceproxy.ClassControl,
			Devpath:  testDevpath,
			Fence:    4711,
			HasFence: true,
		},
		Service: service,
		Bound:   testDevpath,
	}
}

func admitControl(t *testing.T, service string) fenceproxy.Decision {
	t.Helper()
	now := time.Now()
	view := fenceproxy.View{Floor: 4711, Known: true, ObservedAt: now}
	return fenceproxy.Admit(controlRequest(service), view, now, fenceproxy.DefaultPolicy())
}

// TestEverySpawnCommandThisPackageBuildsIsAdmitted.
//
// The table walks the edges of every bound the builder enforces — the jar id's
// alphabet, both ends of the version's component counts and widths, zero
// arguments and the most allowed, the shortest and longest keys and values, and
// every character the value class permits. If the builder accepts it, the proxy
// must forward it.
func TestEverySpawnCommandThisPackageBuildsIsAdmitted(t *testing.T) {
	jarIDs := []string{
		"000000000000",
		"ffffffffffff",
		"0123456789ab",
		JarID([]byte("a pretend scrcpy-server.jar")),
	}
	versions := []string{"0.0", "3.1", "99.9", "999.999", "1.2.3", "999.999.999", "0.0.0"}
	scids := []SCID{1, 0xbeef, 0x7fffffff, MaxSCID}
	argSets := [][]Arg{
		nil,
		{{Key: "a", Value: "b"}},
		{{Key: "log_level", Value: "verbose"}, {Key: "video_codec", Value: "h265"}},
		{{Key: strings.Repeat("z", MaxKeyLen), Value: strings.Repeat("9", MaxValueLen)}},
		{{Key: "_", Value: "-"}},
		{{Key: "video_bit_rate", Value: "8000000"}, {Key: "max_fps", Value: "60"},
			{Key: "tunnel_forward", Value: "true"}, {Key: "audio", Value: "false"},
			{Key: "control", Value: "true"}, {Key: "cleanup", Value: "false"}},
		manyArgs(MaxUserArgs),
		// Every character the value alphabet admits, in one value.
		{{Key: "alphabet", Value: "ABCXYZabcxyz0189_.-"}},
	}

	tried := 0
	for _, jar := range jarIDs {
		for _, version := range versions {
			for _, scid := range scids {
				for _, args := range argSets {
					s := Spawn{JarID: jar, Version: version, SCID: scid, Args: args}
					service, err := s.Service()
					if err != nil {
						t.Fatalf("Service() for %+v: %v", s, err)
					}
					tried++
					if d := admitControl(t, service); !d.Admitted() {
						t.Fatalf("the proxy refused a command this package built (%s: %s)\n  %s",
							d.Outcome, d.Reason, service)
					}

					sock, err := s.Socket()
					if err != nil {
						t.Fatalf("Socket() for %+v: %v", s, err)
					}
					if d := admitControl(t, sock); !d.Admitted() {
						t.Fatalf("the proxy refused a socket this package built (%s: %s)\n  %s",
							d.Outcome, d.Reason, sock)
					}
				}
			}
		}
	}
	if tried < len(jarIDs)*len(versions)*len(scids)*len(argSets) {
		t.Fatalf("only %d commands were tried; this test would pass too easily", tried)
	}
}

// TestRandomlyBuiltCommandsAreAdmitted.
//
// The table above walks the edges somebody thought of. This walks the interior:
// two thousand commands assembled from random-but-valid parts, on a fixed seed
// so a failure is reproducible and a green run means the same thing tomorrow.
//
// It is here because the failure it guards against is a widening — somebody
// adding a character to validateValue that the proxy's class does not admit —
// and a widening is exactly the change an edge-case table does not notice.
func TestRandomlyBuiltCommandsAreAdmitted(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x5c2c, 0x7079))

	const keyAlphabet = "abcdefghijklmnopqrstuvwxyz_"
	const valueAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-"
	pick := func(alphabet string, n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteByte(alphabet[rng.IntN(len(alphabet))])
		}
		return b.String()
	}

	for i := 0; i < 2000; i++ {
		sum := sha256.Sum256(fmt.Appendf(nil, "jar-%d", i))
		version := fmt.Sprintf("%d.%d", rng.IntN(1000), rng.IntN(1000))
		if rng.IntN(2) == 0 {
			version += fmt.Sprintf(".%d", rng.IntN(1000))
		}

		n := rng.IntN(MaxUserArgs + 1)
		args := make([]Arg, 0, n)
		used := map[string]struct{}{}
		for len(args) < n {
			k := pick(keyAlphabet, 1+rng.IntN(MaxKeyLen))
			if _, dup := used[k]; dup {
				continue
			}
			used[k] = struct{}{}
			args = append(args, Arg{Key: k, Value: pick(valueAlphabet, 1+rng.IntN(MaxValueLen))})
		}

		s := Spawn{
			JarID:   JarIDFromDigest(sum),
			Version: version,
			SCID:    SCID(1 + rng.Uint32N(uint32(MaxSCID))),
			Args:    args,
		}
		service, err := s.Service()
		if err != nil {
			t.Fatalf("iteration %d: Service() for %+v: %v", i, s, err)
		}
		if d := admitControl(t, service); !d.Admitted() {
			t.Fatalf("iteration %d: the proxy refused a command this package built (%s: %s)\n  %s",
				i, d.Outcome, d.Reason, service)
		}
		sock, err := s.Socket()
		if err != nil {
			t.Fatalf("iteration %d: Socket(): %v", i, err)
		}
		if d := admitControl(t, sock); !d.Admitted() {
			t.Fatalf("iteration %d: the proxy refused a socket this package built (%s: %s)\n  %s",
				i, d.Outcome, d.Reason, sock)
		}
	}
}

// TestEveryByteTheBuilderAcceptsIsAByteTheProxyAdmits.
//
// The tables above are made of characters somebody chose, and so is the random
// generator's alphabet. Neither of them can notice a WIDENING: if validateValue
// grew a case for ';' tomorrow, every example above would still be built and
// still be admitted, and the builder would quietly be able to emit a command
// the proxy refuses — or worse, one it admits with a second command chained
// onto the end of it.
//
// So this sweeps all 256 byte values through each position of the command, and
// for every byte the builder accepts, asks the proxy about the command that
// byte produces. It is the only test here whose coverage does not depend on
// somebody having thought of the character.
func TestEveryByteTheBuilderAcceptsIsAByteTheProxyAdmits(t *testing.T) {
	const goodJar = "e3b0c44298fc"

	accepted := map[string]int{}
	check := func(position string, b byte, s Spawn) {
		t.Helper()
		service, err := s.Service()
		if err != nil {
			return
		}
		accepted[position]++
		if d := admitControl(t, service); !d.Admitted() {
			t.Errorf("the builder accepted byte 0x%02x (%q) in %s and the proxy refused the "+
				"command it produced (%s: %s)\n  %s", b, string(b), position, d.Outcome, d.Reason, service)
		}
	}

	for i := 0; i < 256; i++ {
		b := byte(i)
		c := string([]byte{b})

		check("an argument value", b, Spawn{JarID: goodJar, Version: "3.1", SCID: 0xbeef,
			Args: []Arg{{Key: "k", Value: c}}})
		check("an argument key", b, Spawn{JarID: goodJar, Version: "3.1", SCID: 0xbeef,
			Args: []Arg{{Key: c, Value: "v"}}})
		check("a jar id", b, Spawn{JarID: goodJar[:11] + c, Version: "3.1", SCID: 0xbeef})
		check("a version", b, Spawn{JarID: goodJar, Version: "3." + c, SCID: 0xbeef})
	}

	// Every position must have accepted SOMETHING, or the sweep passed by
	// refusing all 256 and asserted nothing at all.
	for _, position := range []string{"an argument value", "an argument key", "a jar id", "a version"} {
		if accepted[position] == 0 {
			t.Errorf("the builder accepted no byte at all in %s; this sweep asserted nothing", position)
		}
	}

	// And the counts are pinned, so that a widening shows up here as a number
	// even when the newly-admitted character happens to be one the proxy also
	// admits. 65 for a value is the 62 of [A-Za-z0-9] plus underscore, dot and
	// hyphen; 27 for a key is a-z plus underscore; 16 for a jar id is the hex
	// digits; 10 for a version is the decimal digits.
	for _, c := range []struct {
		position string
		want     int
	}{
		{"an argument value", 65},
		{"an argument key", 27},
		{"a jar id", 16},
		{"a version", 10},
	} {
		if accepted[c.position] != c.want {
			t.Errorf("the builder accepts %d of 256 bytes in %s, want %d; the alphabet changed",
				accepted[c.position], c.position, c.want)
		}
	}
}

// rawSpawnService assembles a command with no validation at all, so a test can
// ask what the proxy would have done with a string the builder refused to
// produce. It must stay a byte-for-byte copy of Spawn.Service's assembly and
// nothing else; the test below is what keeps it honest, because a divergence
// would show up as a proxy admission of a string the builder never emits.
func rawSpawnService(jar, version, scid string, args []Arg) string {
	var b strings.Builder
	b.WriteString("shell,v2,raw:CLASSPATH=" + ServerDir + "/" + jarPrefix + jar + jarSuffix)
	b.WriteString(" app_process / " + ServerClass + " " + version + " scid=" + scid)
	for _, a := range args {
		b.WriteString(" " + a.Key + "=" + a.Value)
	}
	return b.String()
}

// TestTheBuilderIsNotStricterThanTheProxyForNoReason.
//
// A validator that refuses things the proxy would have admitted is not a safety
// property, it is a feature nobody can use, and it rots — the next person
// widens it to unblock themselves without knowing which of the two ends was
// authoritative. So every refusal above is checked in the other direction here:
// the string the builder declined to produce is one the proxy declines too.
//
// The single deliberate exception is a repeated key, which the proxy admits
// (its pattern counts groups, not names) and the builder refuses. That is a
// caller-intent check, not a security one, and it is listed so that the
// exception is a decision rather than an oversight.
func TestTheBuilderIsNotStricterThanTheProxyForNoReason(t *testing.T) {
	const goodJar = "e3b0c44298fc"
	const goodVersion = "3.1"
	const goodSCID = "0000beef"

	for _, c := range []struct {
		name    string
		service string
	}{
		{"an uppercase jar id", rawSpawnService("E3B0C44298FC", goodVersion, goodSCID, nil)},
		{"a short jar id", rawSpawnService("e3b0c4", goodVersion, goodSCID, nil)},
		{"a traversal in the jar id", rawSpawnService("../../etc/p", goodVersion, goodSCID, nil)},
		{"a bare major version", rawSpawnService(goodJar, "3", goodSCID, nil)},
		{"a four-digit version component", rawSpawnService(goodJar, "1000.1", goodSCID, nil)},
		{"a release-candidate version", rawSpawnService(goodJar, "3.1-rc1", goodSCID, nil)},
		{"a four-part version", rawSpawnService(goodJar, "1.2.3.4", goodSCID, nil)},
		{"an uppercase argument key", rawSpawnService(goodJar, goodVersion, goodSCID,
			[]Arg{{Key: "Max_size", Value: "1024"}})},
		{"a digit in an argument key", rawSpawnService(goodJar, goodVersion, goodSCID,
			[]Arg{{Key: "codec2", Value: "x"}})},
		{"a space in a value", rawSpawnService(goodJar, goodVersion, goodSCID,
			[]Arg{{Key: "k", Value: "a b"}})},
		{"a semicolon in a value", rawSpawnService(goodJar, goodVersion, goodSCID,
			[]Arg{{Key: "k", Value: "x;id"}})},
		{"a command substitution in a value", rawSpawnService(goodJar, goodVersion, goodSCID,
			[]Arg{{Key: "k", Value: "$(id)"}})},
		{"a redirection in a value", rawSpawnService(goodJar, goodVersion, goodSCID,
			[]Arg{{Key: "k", Value: "x>/sdcard/y"}})},
		{"a key one character too long", rawSpawnService(goodJar, goodVersion, goodSCID,
			[]Arg{{Key: strings.Repeat("a", MaxKeyLen+1), Value: "x"}})},
		{"a value one character too long", rawSpawnService(goodJar, goodVersion, goodSCID,
			[]Arg{{Key: "k", Value: strings.Repeat("v", MaxValueLen+1)}})},
		{"one argument too many", rawSpawnService(goodJar, goodVersion, goodSCID,
			manyArgs(MaxUserArgs+1))},
		{"a socket name that is not eight hex", "localabstract:scrcpy_beef"},
		{"any other abstract socket", "localabstract:something_else"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if d := admitControl(t, c.service); d.Admitted() {
				t.Errorf("the proxy ADMITTED a string the builder refuses to produce; one of the "+
					"two is wrong and it is not obvious which:\n  %s", c.service)
			}
		})
	}

	// The exception, stated as an assertion so that it cannot quietly become
	// the rule.
	dup := rawSpawnService(goodJar, goodVersion, goodSCID,
		[]Arg{{Key: "max_size", Value: "1024"}, {Key: "max_size", Value: "800"}})
	if d := admitControl(t, dup); !d.Admitted() {
		t.Errorf("the proxy refused a repeated key (%s: %s); the comment above claims it admits "+
			"one, and that claim is now wrong", d.Outcome, d.Reason)
	}
}

// TestTheRawAssemblerStillMatchesTheBuilder.
//
// rawSpawnService exists to ask the proxy about strings the builder will not
// produce, which only means anything while the two assemble identically. This
// is that check.
func TestTheRawAssemblerStillMatchesTheBuilder(t *testing.T) {
	s := Spawn{
		JarID:   "e3b0c44298fc",
		Version: "3.1.4",
		SCID:    0xbeef,
		Args:    []Arg{{Key: "log_level", Value: "info"}, {Key: "max_size", Value: "1024"}},
	}
	built, err := s.Service()
	if err != nil {
		t.Fatalf("Service(): %v", err)
	}
	raw := rawSpawnService(s.JarID, s.Version, s.SCID.String(), s.Args)
	if built != raw {
		t.Errorf("the raw assembler has drifted from Spawn.Service:\n  builder %q\n  raw     %q",
			built, raw)
	}
}

// TestControlClassStillNeedsItsFenceAndItsTransport.
//
// Not an assertion about this package — it is an assertion about the assumption
// this whole file rests on. Every admission above is granted to a request that
// carries a fence at or above the floor and is bound to the devpath it claims.
// If any of those stopped mattering, the tests above would keep passing while
// meaning much less, so the conditions are checked by removing them one at a
// time.
func TestControlClassStillNeedsItsFenceAndItsTransport(t *testing.T) {
	s := Spawn{JarID: "e3b0c44298fc", Version: "3.1", SCID: 0xbeef}
	service, err := s.Service()
	if err != nil {
		t.Fatalf("Service(): %v", err)
	}
	now := time.Now()
	fresh := fenceproxy.View{Floor: 4711, Known: true, ObservedAt: now}
	pol := fenceproxy.DefaultPolicy()

	if d := fenceproxy.Admit(controlRequest(service), fresh, now, pol); !d.Admitted() {
		t.Fatalf("the baseline request was refused (%s: %s)", d.Outcome, d.Reason)
	}

	unbound := controlRequest(service)
	unbound.Bound = ""
	if d := fenceproxy.Admit(unbound, fresh, now, pol); d.Admitted() {
		t.Error("a spawn was admitted on a connection with no transport switched to the claimed device")
	}

	unfenced := controlRequest(service)
	unfenced.Claim.HasFence = false
	if d := fenceproxy.Admit(unfenced, fresh, now, pol); d.Admitted() {
		t.Error("a spawn was admitted on a control-class connection presenting no fence")
	}

	stale := controlRequest(service)
	stale.Claim.Fence = 4710
	if d := fenceproxy.Admit(stale, fresh, now, pol); d.Admitted() {
		t.Error("a spawn was admitted on a fence below the floor; the lease it claims is over")
	}

	// And the class matters: the same command from the maintenance credential,
	// which the recovery ladder holds on every phone in the rack, must not open
	// a screen.
	asMaintenance := controlRequest(service)
	asMaintenance.Identity.Class = fenceproxy.ClassMaintenance
	if d := fenceproxy.Admit(asMaintenance, fresh, now, pol); d.Admitted() {
		t.Error("a maintenance-class connection was admitted to spawn a scrcpy server")
	}
}
