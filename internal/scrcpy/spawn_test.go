package scrcpy

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func TestJarIDIsTheFirstTwelveHexOfTheSha256(t *testing.T) {
	// SHA256("") = e3b0c44298fc1c149afbf4c8996fb924…
	if got, want := JarID(nil), "e3b0c44298fc"; got != want {
		t.Errorf("JarID(nil) = %q, want %q", got, want)
	}
	if got, want := JarID([]byte{}), "e3b0c44298fc"; got != want {
		t.Errorf("JarID(empty) = %q, want %q", got, want)
	}
	if got := JarID([]byte("scrcpy")); got == JarID(nil) {
		t.Error("two different jars produced the same id")
	}
	if n := len(JarID([]byte("scrcpy"))); n != JarIDLen {
		t.Errorf("JarID is %d characters, want %d", n, JarIDLen)
	}

	// The digest form must agree with the byte-slice form, or a caller
	// streaming the jar past a hash gets a different name than one holding it
	// in memory — and then the CLASSPATH names a file that was never pushed.
	sum := sha256.Sum256([]byte("scrcpy"))
	if a, b := JarIDFromDigest(sum), JarID([]byte("scrcpy")); a != b {
		t.Errorf("JarIDFromDigest = %q but JarID = %q", a, b)
	}
}

// TestSpawnServiceIsOneFixedString.
//
// Written out rather than assembled, because the string is a contract with two
// other things: the fence proxy's pattern, which admits it, and the server on
// the device, which parses it. Either of them changing without this changing is
// a failure at 3am on somebody's session.
func TestSpawnServiceIsOneFixedString(t *testing.T) {
	s := Spawn{
		JarID:   "e3b0c44298fc",
		Version: "3.1",
		SCID:    0xbeef,
		Args: []Arg{
			{Key: "log_level", Value: "info"},
			{Key: "video_codec", Value: "h264"},
			{Key: "max_size", Value: "1024"},
		},
	}

	got, err := s.Service()
	if err != nil {
		t.Fatalf("Service(): %v", err)
	}
	const want = "shell,v2,raw:CLASSPATH=/data/local/tmp/scrcpy-server-e3b0c44298fc.jar " +
		"app_process / com.genymobile.scrcpy.Server 3.1 scid=0000beef " +
		"log_level=info video_codec=h264 max_size=1024"
	if got != want {
		t.Errorf("Service() =\n  %q\nwant\n  %q", got, want)
	}

	path, err := s.JarPath()
	if err != nil {
		t.Fatalf("JarPath(): %v", err)
	}
	if pathWant := "/data/local/tmp/scrcpy-server-e3b0c44298fc.jar"; path != pathWant {
		t.Errorf("JarPath() = %q, want %q", path, pathWant)
	}
	if !strings.Contains(got, "CLASSPATH="+path+" ") {
		t.Errorf("the CLASSPATH in %q is not the path JarPath() reports (%q); the push and the "+
			"spawn would disagree about which jar runs", got, path)
	}

	sock, err := s.Socket()
	if err != nil {
		t.Fatalf("Socket(): %v", err)
	}
	if sockWant := "localabstract:scrcpy_0000beef"; sock != sockWant {
		t.Errorf("Socket() = %q, want %q", sock, sockWant)
	}
	if !strings.Contains(got, " scid=0000beef") {
		t.Errorf("the scid in %q does not match the socket %q; the client would wait forever on a "+
			"name the server never publishes", got, sock)
	}
}

// TestSCIDIsOwnedByTheSpawn.
//
// The scid appears twice — once in the command line, once in the socket name —
// and the two must agree. Letting a caller pass it as an Arg would make them
// able to disagree, which presents as a client blocked on a socket that never
// appears rather than as an error anybody can read.
func TestSCIDIsOwnedByTheSpawn(t *testing.T) {
	s := Spawn{JarID: "e3b0c44298fc", Version: "3.1", SCID: 1,
		Args: []Arg{{Key: "scid", Value: "0000dead"}}}
	_, err := s.Service()
	var buildErr *BuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("Service() with a caller-supplied scid = %v, want a *BuildError", err)
	}
	if buildErr.Part != "argument key" || buildErr.Value != "scid" {
		t.Errorf("BuildError blames %s %q, want argument key \"scid\"", buildErr.Part, buildErr.Value)
	}
}

func TestSCIDBounds(t *testing.T) {
	for _, c := range []struct {
		name string
		scid SCID
		ok   bool
	}{
		{"zero", 0, false},
		{"one", 1, true},
		{"the largest a Java int parses", MaxSCID, true},
		{"one past it", MaxSCID + 1, false},
		{"the largest u32", 0xffffffff, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := Spawn{JarID: "e3b0c44298fc", Version: "3.1", SCID: c.scid}
			_, sockErr := s.Socket()
			_, svcErr := s.Service()
			if c.ok {
				if sockErr != nil || svcErr != nil {
					t.Fatalf("Socket() = %v, Service() = %v; want both to succeed", sockErr, svcErr)
				}
				return
			}
			if sockErr == nil {
				t.Error("Socket() succeeded")
			}
			if svcErr == nil {
				t.Error("Service() succeeded; a spawn whose socket cannot be named must not start")
			}
		})
	}

	// Eight lowercase hex, zero padded, both places.
	if got := SCID(0x0a).String(); got != "0000000a" {
		t.Errorf("SCID(10).String() = %q, want %q", got, "0000000a")
	}
}

// TestSpawnRefusesWhatTheProxyWouldRefuse tabulates the alphabet.
//
// The positive direction — that everything accepted here is admitted — is
// admission_test.go's job, and it is the more important half. This half is
// about the message: a caller who typed an uppercase key wants to be told
// which key, not that a regular expression did not match.
func TestSpawnRefusesWhatTheProxyWouldRefuse(t *testing.T) {
	base := func() Spawn {
		return Spawn{JarID: "e3b0c44298fc", Version: "3.1", SCID: 0xbeef}
	}
	longKey := strings.Repeat("a", MaxKeyLen)
	longValue := strings.Repeat("v", MaxValueLen)

	for _, c := range []struct {
		name string
		mut  func(*Spawn)
		part string
		ok   bool
	}{
		{"a good baseline", func(*Spawn) {}, "", true},
		{"a short jar id", func(s *Spawn) { s.JarID = "e3b0c4" }, "jar id", false},
		{"an uppercase jar id", func(s *Spawn) { s.JarID = "E3B0C44298FC" }, "jar id", false},
		{"a jar id with a slash", func(s *Spawn) { s.JarID = "e3b0c4/298fc" }, "jar id", false},
		{"a path traversal in the jar id", func(s *Spawn) { s.JarID = "../../etc/p" }, "jar id", false},

		{"a three-part version", func(s *Spawn) { s.Version = "3.1.4" }, "", true},
		{"the widest version", func(s *Spawn) { s.Version = "999.999.999" }, "", true},
		{"a bare major", func(s *Spawn) { s.Version = "3" }, "version", false},
		{"four parts", func(s *Spawn) { s.Version = "1.2.3.4" }, "version", false},
		{"a four-digit component", func(s *Spawn) { s.Version = "1000.1" }, "version", false},
		{"a release-candidate suffix", func(s *Spawn) { s.Version = "3.1-rc1" }, "version", false},
		{"an empty version", func(s *Spawn) { s.Version = "" }, "version", false},

		{"no arguments at all", func(s *Spawn) { s.Args = nil }, "", true},
		{"the longest key and value", func(s *Spawn) {
			s.Args = []Arg{{Key: longKey, Value: longValue}}
		}, "", true},
		{"a key one character too long", func(s *Spawn) {
			s.Args = []Arg{{Key: longKey + "a", Value: "x"}}
		}, "argument key", false},
		{"a value one character too long", func(s *Spawn) {
			s.Args = []Arg{{Key: "k", Value: longValue + "v"}}
		}, "argument value", false},
		{"an empty key", func(s *Spawn) { s.Args = []Arg{{Key: "", Value: "x"}} }, "argument key", false},
		{"an empty value", func(s *Spawn) { s.Args = []Arg{{Key: "k", Value: ""}} }, "argument value", false},
		{"a digit in a key", func(s *Spawn) {
			s.Args = []Arg{{Key: "codec2", Value: "x"}}
		}, "argument key", false},
		{"an uppercase key", func(s *Spawn) {
			s.Args = []Arg{{Key: "Max_size", Value: "x"}}
		}, "argument key", false},
		{"a space in a value", func(s *Spawn) {
			s.Args = []Arg{{Key: "k", Value: "a b"}}
		}, "argument value", false},
		{"a semicolon in a value", func(s *Spawn) {
			s.Args = []Arg{{Key: "k", Value: "x;id"}}
		}, "argument value", false},
		{"a command substitution in a value", func(s *Spawn) {
			s.Args = []Arg{{Key: "k", Value: "$(id)"}}
		}, "argument value", false},
		{"a newline in a value", func(s *Spawn) {
			s.Args = []Arg{{Key: "k", Value: "x\nid"}}
		}, "argument value", false},
		{"a redirection in a value", func(s *Spawn) {
			s.Args = []Arg{{Key: "k", Value: "x>/sdcard/y"}}
		}, "argument value", false},

		{"the most arguments allowed", func(s *Spawn) { s.Args = manyArgs(MaxUserArgs) }, "", true},
		{"one argument too many", func(s *Spawn) { s.Args = manyArgs(MaxUserArgs + 1) }, "arguments", false},
		{"a repeated key", func(s *Spawn) {
			s.Args = []Arg{{Key: "max_size", Value: "1024"}, {Key: "max_size", Value: "800"}}
		}, "argument key", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := base()
			c.mut(&s)
			got, err := s.Service()
			if c.ok {
				if err != nil {
					t.Fatalf("Service() = %v, want a command", err)
				}
				if got == "" {
					t.Fatal("Service() returned an empty string with no error")
				}
				return
			}
			var buildErr *BuildError
			if !errors.As(err, &buildErr) {
				t.Fatalf("Service() = %q, %v; want a *BuildError", got, err)
			}
			if buildErr.Part != c.part {
				t.Errorf("BuildError blames %q, want %q (message: %v)", buildErr.Part, c.part, err)
			}
			if got != "" {
				t.Errorf("Service() returned %q alongside its error; a refused command must not "+
					"be reachable by a caller that ignored the error", got)
			}
		})
	}
}

// manyArgs builds n distinct valid arguments. The keys are letters because a
// key may not contain a digit.
func manyArgs(n int) []Arg {
	out := make([]Arg, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Arg{Key: strings.Repeat("k", i+1), Value: "v"})
	}
	return out
}
