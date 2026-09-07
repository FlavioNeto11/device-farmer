package scrcpy

// The command that starts the server on a handset, and the socket it publishes.
//
// There is exactly one interesting property in this file: a command this
// builder emits is a command internal/fenceproxy's control class admits. Not
// "usually", not "if the caller is careful" — by construction, and pinned by
// admission_test.go, which pushes every shape reachable from here through
// fenceproxy.Admit and fails on a refusal.
//
// That matters because of where the refusal would otherwise land. The proxy
// decides per host-protocol frame, on a connection that has already completed
// a TLS handshake, already switched a transport and already been fenced
// against a live lease. A builder that produced a string one character outside
// the admitted alphabet would fail there — inside somebody's session, on a
// phone, with a log line about a whitelist. The cheap place to find that out is
// a test with no network in it.
//
// The alphabet is not duplicated from the proxy for style. Read
// fenceproxy.control(): the reason a whole-string pattern is safe over a shell
// command line is that "every character class below is alphanumerics, dot,
// slash, underscore, hyphen, equals and space. None of the characters that
// chain, substitute or redirect can appear anywhere in a string this pattern
// accepts". This file's validators are the other end of that argument. They are
// written out as explicit character tests rather than as a second copy of the
// regexps so that a reader can see the alphabet, and so that a refusal names
// the argument that was wrong instead of reporting that a pattern did not
// match.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Where the server jar lives on the device and what starts it.
//
// The directory is /data/local/tmp because that is the one place the shell user
// can write and app_process can read on a device with no root, and it is what
// the fence proxy's pattern has baked into it. Changing either of these
// requires changing the proxy in the same commit; they are not independent
// knobs and they are constants here so that a reader looking for the coupling
// finds it named.
const (
	// ServerDir is the on-device directory the jar is pushed to.
	ServerDir = "/data/local/tmp"

	// ServerClass is the entry point app_process runs.
	ServerClass = "com.genymobile.scrcpy.Server"

	// jarPrefix and jarSuffix bracket the content-addressed name.
	jarPrefix = "scrcpy-server-"
	jarSuffix = ".jar"
)

// Bounds. Each of these is one of the fence proxy's repeat counts, restated
// where the builder can enforce it.
const (
	// JarIDLen is how many hex characters of the jar's sha256 name it.
	//
	// Twelve is not a security parameter and must not be read as one: 48 bits
	// of a digest identifies a file among the handful an operator has uploaded,
	// which is all this name is for. What it buys is that a stale jar and a new
	// one cannot share a path, so a device that was pushed the old one starts
	// the old one and says so in its version handshake rather than half-
	// speaking a framing nobody expects.
	JarIDLen = 12

	// MaxArgs is the total number of key=value arguments the proxy's pattern
	// admits after the version.
	MaxArgs = 12

	// MaxUserArgs is how many of those a caller may supply. scid is emitted by
	// this package rather than typed by the caller — see [SCID] — and it
	// occupies one of the twelve.
	MaxUserArgs = MaxArgs - 1

	// MaxKeyLen and MaxValueLen bound one argument.
	MaxKeyLen   = 24
	MaxValueLen = 32

	// maxVersionPart bounds each dotted component of the version.
	maxVersionPartLen = 3
)

// SCID identifies one server instance, and through it the abstract socket that
// instance publishes.
//
// scrcpy's server creates ONE abstract socket named scrcpy_<scid> and accepts
// up to three connections on it in a fixed order — video, then audio, then
// control. That is why [Spawn.Socket] returns a single name rather than one per
// stream: the two sockets a live screen needs are two connections to the same
// name, which is also why they are two separate ADB transports and therefore
// two separate admissions (docs/design/interactive-control.md §4).
//
// The value is bounded to 31 bits because the server parses it with Java's
// Integer.parseInt on a hex string, which overflows above 2^31-1 and kills the
// server with a NumberFormatException before it has written a byte. Zero is
// refused as well, so that the zero value of a [Spawn] is invalid rather than
// quietly making every instance on a host share the socket name 00000000.
type SCID uint32

// MaxSCID is the largest value the server's parser accepts.
const MaxSCID SCID = 1<<31 - 1

// Valid reports whether this scid can name a socket the server will publish.
func (s SCID) Valid() bool { return s != 0 && s <= MaxSCID }

// String renders the scid the way both ends format it: eight lowercase hex
// digits, zero padded.
func (s SCID) String() string { return fmt.Sprintf("%08x", uint32(s)) }

// Arg is one key=value argument to the server.
//
// The keys are scrcpy's own — video_codec, max_size, tunnel_forward, control,
// audio, log_level and the rest — and this package deliberately does not
// enumerate them. A version of the server that gained an option would need a
// Go release to pass it, and the version handshake already refuses an argument
// the server does not know, with a message on a stream somebody is watching.
// What this package enforces is the SHAPE, because the shape is what the proxy
// admits and what keeps a shell metacharacter out of a command line.
type Arg struct {
	Key   string
	Value string
}

// Spawn is everything needed to start one server instance.
type Spawn struct {
	// JarID is the twelve hex characters of the jar's sha256 that name it on
	// the device. Build it with [JarID] rather than typing it.
	JarID string

	// Version is the server's own version string, e.g. "3.1" or "3.1.4". It is
	// passed as an argument because the framing and the command line are
	// coupled to the jar an operator uploaded: a server that does not
	// recognise the version it was handed refuses to start and says so, which
	// turns a silent garbage-frame parse into a named error.
	Version string

	// SCID names the abstract socket this instance will publish.
	SCID SCID

	// Args are the caller's options, at most [MaxUserArgs] of them, in the
	// order they will appear on the command line. Order is not significant to
	// the server — these are keyword arguments — but it is preserved so that
	// two calls with the same input produce the same string, which is what
	// makes a golden test of this possible at all.
	Args []Arg
}

// JarID names a server jar by content: the first [JarIDLen] hex characters of
// its sha256.
//
// It takes the whole jar as bytes rather than a path or a reader because this
// package does no I/O. The caller has the file; hashing it is one line there.
func JarID(jar []byte) string {
	sum := sha256.Sum256(jar)
	return JarIDFromDigest(sum)
}

// JarIDFromDigest names a jar from a digest already computed elsewhere, for a
// caller streaming the file past a hash on its way to the device rather than
// holding it in memory.
func JarIDFromDigest(sum [sha256.Size]byte) string {
	return hex.EncodeToString(sum[:JarIDLen/2])
}

// BuildError says which part of a spawn command would not have been admitted,
// and why.
//
// One type rather than a sentinel per field, because the useful question a
// caller asks is "which of the things I supplied was wrong" and the useful
// answer is a string an operator can act on. errors.As reaches Part and Value
// for a caller that wants to point at a form field.
type BuildError struct {
	// Part names the field: "jar id", "version", "scid", "argument key",
	// "argument value", "arguments".
	Part string

	// Value is what was supplied, quoted into the message.
	Value string

	// Reason completes the sentence "…is not usable because".
	Reason string
}

func (e *BuildError) Error() string {
	return fmt.Sprintf("scrcpy: %s %q: %s", e.Part, e.Value, e.Reason)
}

// JarPath is where the jar must be pushed for [Spawn.Service] to find it.
//
// It validates, and it returns the path rather than exposing the concatenation,
// because the CLASSPATH in the command and the destination of the push are the
// same string and must not be able to disagree. A caller that builds one of
// them by hand has reintroduced exactly the failure this returns.
func (s Spawn) JarPath() (string, error) {
	if err := validateJarID(s.JarID); err != nil {
		return "", err
	}
	return ServerDir + "/" + jarPrefix + s.JarID + jarSuffix, nil
}

// Socket is the ADB service string that opens the server's abstract socket.
//
// Both the video connection and the control connection open this same name, in
// that order, and each of them is its own ADB transport and therefore its own
// admission at the fence proxy.
func (s Spawn) Socket() (string, error) {
	if !s.SCID.Valid() {
		return "", &BuildError{
			Part:  "scid",
			Value: s.SCID.String(),
			Reason: fmt.Sprintf("must be between 1 and %d; zero would make every instance on a host "+
				"share one socket name, and a value above %d overflows the server's Integer.parseInt",
				uint32(MaxSCID), uint32(MaxSCID)),
		}
	}
	return "localabstract:scrcpy_" + s.SCID.String(), nil
}

// Service is the ADB service string that starts the server.
//
// The shape is fixed by internal/fenceproxy's control whitelist and every
// component of it is validated above rather than trusted:
//
//	shell,v2,raw:CLASSPATH=<jar> app_process / com.genymobile.scrcpy.Server <version> <k=v>…
//
// The shell is v2 and raw because the server's own output on stderr is how a
// version mismatch or a missing class reports itself, and a pty would rewrite
// those bytes; raw keeps them exactly as the device wrote them.
func (s Spawn) Service() (string, error) {
	jar, err := s.JarPath()
	if err != nil {
		return "", err
	}
	if err := validateVersion(s.Version); err != nil {
		return "", err
	}
	if _, err := s.Socket(); err != nil {
		return "", err
	}
	if len(s.Args) > MaxUserArgs {
		return "", &BuildError{
			Part:  "arguments",
			Value: fmt.Sprint(len(s.Args)),
			Reason: fmt.Sprintf("at most %d may be supplied; the proxy's pattern admits %d and scid takes one",
				MaxUserArgs, MaxArgs),
		}
	}

	// scid first, and owned by this package. It is the one argument whose value
	// another part of this file also renders — into the socket name — and
	// letting a caller pass it as an Arg would make the two able to disagree,
	// which presents as a client waiting forever on a socket the server never
	// published.
	seen := map[string]struct{}{"scid": {}}
	var b strings.Builder
	b.WriteString("shell,v2,raw:CLASSPATH=")
	b.WriteString(jar)
	b.WriteString(" app_process / ")
	b.WriteString(ServerClass)
	b.WriteByte(' ')
	b.WriteString(s.Version)
	b.WriteString(" scid=")
	b.WriteString(s.SCID.String())

	for _, a := range s.Args {
		if err := validateKey(a.Key); err != nil {
			return "", err
		}
		if err := validateValue(a.Value); err != nil {
			return "", err
		}
		if _, dup := seen[a.Key]; dup {
			return "", &BuildError{
				Part:  "argument key",
				Value: a.Key,
				Reason: "appears twice; the server takes the last occurrence and the caller " +
					"almost certainly meant the first",
			}
		}
		seen[a.Key] = struct{}{}
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(a.Value)
	}
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// The alphabet
//
// Each of these is one repeat count and one character class from
// fenceproxy.control(), enforced where a caller can be told what was wrong. The
// classes are written as explicit comparisons rather than as ranges in a table
// because a reader auditing this against the proxy is checking characters, and
// a table would hide the one that matters.
// ---------------------------------------------------------------------------

func validateJarID(id string) error {
	bad := func(reason string) error {
		return &BuildError{Part: "jar id", Value: id, Reason: reason}
	}
	if len(id) != JarIDLen {
		return bad(fmt.Sprintf("must be exactly %d characters; build it with JarID", JarIDLen))
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return bad("must be lowercase hex; an uppercase digit is the usual cause and " +
				"hex.EncodeToString does not produce one")
		}
	}
	return nil
}

// validateVersion accepts one to three dotted numeric components of at most
// three digits each, which is [0-9]{1,3}\.[0-9]{1,3}(\.[0-9]{1,3})? — note that
// the middle component is required. A bare "3" is refused here because the
// proxy refuses it, and refusing it in the same commit as the proxy is cheaper
// than discovering it on a phone.
func validateVersion(v string) error {
	bad := func(reason string) error {
		return &BuildError{Part: "version", Value: v, Reason: reason}
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return bad("must be two or three dot-separated numbers, e.g. \"3.1\" or \"3.1.4\"")
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > maxVersionPartLen {
			return bad(fmt.Sprintf("each component must be 1 to %d digits", maxVersionPartLen))
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return bad("components must be digits only; a suffix like \"-rc1\" is not admitted")
			}
		}
	}
	return nil
}

// validateKey accepts [a-z_]{1,24}. No digits: that is scrcpy's own naming and
// the proxy's class, and widening it here would build commands the proxy
// refuses.
func validateKey(k string) error {
	bad := func(reason string) error {
		return &BuildError{Part: "argument key", Value: k, Reason: reason}
	}
	if len(k) == 0 || len(k) > MaxKeyLen {
		return bad(fmt.Sprintf("must be 1 to %d characters", MaxKeyLen))
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if (c < 'a' || c > 'z') && c != '_' {
			return bad("must be lowercase letters and underscores only")
		}
	}
	return nil
}

// validateValue accepts [A-Za-z0-9_.-]{1,32}.
//
// Read the exclusions rather than the inclusions: no space, so a value cannot
// become two arguments; no semicolon, ampersand, pipe, backtick, dollar,
// newline or redirection, so a value cannot extend the command line. That is
// the whole of why a whole-string pattern over a shell command is safe here,
// and it is why this list must never grow a wildcard.
func validateValue(v string) error {
	bad := func(reason string) error {
		return &BuildError{Part: "argument value", Value: v, Reason: reason}
	}
	if len(v) == 0 || len(v) > MaxValueLen {
		return bad(fmt.Sprintf("must be 1 to %d characters", MaxValueLen))
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == '-':
		default:
			return bad("must be letters, digits, underscore, dot and hyphen only; " +
				"nothing that can chain, substitute or redirect a shell command is admitted")
		}
	}
	return nil
}
