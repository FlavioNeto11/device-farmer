package adbwire

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// schemaConnStates is farm.device_runtime.adb_state's CHECK list, copied from
// migrations/00001_core.sql verbatim and in the same order.
//
// It is duplicated here rather than derived, because the point of the test it
// feeds is precisely that the two lists agree: if someone adds a state to the
// schema and not to this package, the watchdog would write a value that
// ParseConnState folds to "unknown" on the way back out, and "we cannot tell"
// is a different alarm from every state it would be hiding.
var schemaConnStates = []string{
	"device", "offline", "unauthorized", "authorizing", "connecting",
	"detached", "no_permissions", "bootloader", "recovery", "sideload",
	"rescue", "host", "absent", "unknown",
}

// TestParseConnStateIsTotalOverTheSchemaVocabulary pins the round trip a
// database read depends on: a value that came out of adb_state must parse to
// itself, for all fourteen values, or a row rewrites itself on every pass
// through this package.
//
// The assertion is spelled string(ParseConnState(v)) == v rather than
// ParseConnState(v).String() == v because ConnState is a defined string type
// with no String method; adding one would change an exported API this test
// does not own.
func TestParseConnStateIsTotalOverTheSchemaVocabulary(t *testing.T) {
	t.Parallel()

	for _, v := range schemaConnStates {
		if got := ParseConnState(v); string(got) != v {
			t.Errorf("ParseConnState(%q) = %q, want %q — not a round trip", v, got, v)
		}
		// Case and surrounding whitespace are normalised away, so the same
		// value arriving from a chattier server still lands on itself.
		if got := ParseConnState("  " + strings.ToUpper(v) + "\r\n"); string(got) != v {
			t.Errorf("ParseConnState(%q, upper-cased and padded) = %q, want %q", v, got, v)
		}
	}

	declared := []ConnState{
		StateDevice, StateOffline, StateUnauthorized, StateAuthorizing,
		StateConnecting, StateDetached, StateNoPermissions, StateBootloader,
		StateRecovery, StateSideload, StateRescue, StateHost, StateAbsent,
		StateUnknown,
	}
	if len(declared) != len(schemaConnStates) {
		t.Fatalf("package declares %d states, the adb_state CHECK list has %d",
			len(declared), len(schemaConnStates))
	}
	for i, want := range schemaConnStates {
		if string(declared[i]) != want {
			t.Errorf("constant %d is %q, the schema's is %q", i, declared[i], want)
		}
	}
}

func TestParseConnStateNormalisesServerText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want ConnState
	}{
		{"empty is unknown, never absent", "", StateUnknown},
		{"whitespace only", " \t\r\n", StateUnknown},
		{"udev sentence with url", "no permissions; see [http://developer.android.com/tools/device.html]", StateNoPermissions},
		{"insufficient permissions phrasing", "insufficient permissions for device", StateNoPermissions},
		{"unauthorized with tail", "unauthorized (RSA key not accepted)", StateUnauthorized},
		{"authorizing with tail", "authorizing device", StateAuthorizing},
		{"connecting with tail", "connecting to daemon", StateConnecting},
		{"a state we have never seen folds to unknown", "quantum", StateUnknown},
		{"mixed case exact value", "SIDELOAD", StateSideload},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseConnState(tc.raw); got != tc.want {
				t.Fatalf("ParseConnState(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestConnStateUsable(t *testing.T) {
	t.Parallel()

	if !StateDevice.Usable() {
		t.Error("StateDevice must be usable; nothing else in the package is")
	}
	for _, v := range schemaConnStates {
		st := ConnState(v)
		if st == StateDevice {
			continue
		}
		if st.Usable() {
			t.Errorf("%q reports Usable; only %q may", st, StateDevice)
		}
	}
}

// TestParseDeviceLineRegressions holds the shapes that review found broken.
// Each case is a listing line that a field-index parser mangles.
func TestParseDeviceLineRegressions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want Device
	}{
		{
			name: "healthy device with the full identity tail",
			line: "SER0001                device usb:3-1.4.1 product:bluejay model:Pixel_6a device:bluejay transport_id:12",
			want: Device{
				Serial: "SER0001", Devpath: "usb:3-1.4.1",
				State: StateDevice, RawState: "device",
				Product: "bluejay", Model: "Pixel_6a", Codename: "bluejay",
				TransportID: 12,
			},
		},
		{
			// Three whitespace-separated words in a whitespace-separated
			// format. Taking field 0 as the serial records "(no" and shifts
			// the state two columns right, so the unauthorized handset an
			// operator is hunting for reports "unknown".
			name: "(no serial number) sentinel does not shift the columns",
			line: "(no serial number)     unauthorized usb:3-1.4.3 transport_id:7",
			want: Device{
				Serial: "", Devpath: "usb:3-1.4.3",
				State: StateUnauthorized, RawState: "unauthorized",
				TransportID: 7,
			},
		},
		{
			name: "(no serial number) followed by a multi-word state",
			line: "(no serial number)     no permissions; see [http://developer.android.com/tools/device.html] usb:3-1.4.4 transport_id:8",
			want: Device{
				Serial: "", Devpath: "usb:3-1.4.4",
				State:    StateNoPermissions,
				RawState: "no permissions; see [http://developer.android.com/tools/device.html]",
				// The bracketed URL survives whole: a parser that let
				// "[http" pass as a key would eat the scheme and the words
				// before it, deleting the useful half of the only message
				// that says which host is missing its udev rules.
				TransportID: 8,
			},
		},
		{
			// The devpath sits to the LEFT of the tail, so an unmodelled
			// key:value encountered on the way back must not stop the walk.
			// If it did, the device would still be listed but with an empty
			// Devpath: absent from ByDevpath and unaddressable for as long as
			// the server kept emitting the field.
			name: "an unmodelled field between devpath and transport_id keeps the devpath",
			line: "SER0002                device usb:3-1.4.2 product:bluejay model:Pixel_6a device:bluejay connection:usb transport_id:9",
			want: Device{
				Serial: "SER0002", Devpath: "usb:3-1.4.2",
				State: StateDevice, RawState: "device",
				Product: "bluejay", Model: "Pixel_6a", Codename: "bluejay",
				TransportID: 9,
				Extra:       map[string]string{"connection": "usb"},
			},
		},
		{
			name: "offline device carries no identity fields",
			line: "SER0003                offline usb:2-1.2 transport_id:3",
			want: Device{
				Serial: "SER0003", Devpath: "usb:2-1.2",
				State: StateOffline, RawState: "offline",
				TransportID: 3,
			},
		},
		{
			name: "sideload keeps its position",
			line: "SER0004                sideload usb:2-1.3 transport_id:4",
			want: Device{
				Serial: "SER0004", Devpath: "usb:2-1.3",
				State: StateSideload, RawState: "sideload",
				TransportID: 4,
			},
		},
		{
			// "device:emu64a" is a known tail key and must not be mistaken
			// for the state, and the absent devpath must stay absent rather
			// than being invented from the serial.
			name: "emulator has no devpath",
			line: "emulator-5554          device product:sdk_gphone64_x86_64 model:sdk_gphone64_x86_64 device:emu64xa transport_id:1",
			want: Device{
				Serial: "emulator-5554", Devpath: "",
				State: StateDevice, RawState: "device",
				Product: "sdk_gphone64_x86_64", Model: "sdk_gphone64_x86_64", Codename: "emu64xa",
				TransportID: 1,
			},
		},
		{
			name: "unparseable transport id leaves the handle zero rather than guessing",
			line: "SER0005                device usb:2-1.4 transport_id:notanumber",
			want: Device{
				Serial: "SER0005", Devpath: "usb:2-1.4",
				State: StateDevice, RawState: "device",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseDeviceList(tc.line)
			if len(got) != 1 {
				t.Fatalf("parseDeviceList(%q) returned %d devices, want 1", tc.line, len(got))
			}
			if !reflect.DeepEqual(got[0], tc.want) {
				t.Fatalf("parseDeviceList(%q)\n got %+v\nwant %+v", tc.line, got[0], tc.want)
			}
		})
	}
}

func TestParseDeviceListSkipsNoise(t *testing.T) {
	t.Parallel()

	payload := strings.Join([]string{
		"* daemon not running; starting now at tcp:5037",
		"* daemon started successfully",
		"SER0001                device usb:3-1.4.1 transport_id:1\r",
		"",
		"   ",
		"truncated",
		"SER0002                device usb:3-1.4.2 transport_id:2",
	}, "\n")

	got := parseDeviceList(payload)
	if len(got) != 2 {
		t.Fatalf("parsed %d devices from a noisy listing, want 2: %+v", len(got), got)
	}
	if got[0].Devpath != "usb:3-1.4.1" || got[1].Devpath != "usb:3-1.4.2" {
		t.Fatalf("listing order or devpaths wrong: %+v", got)
	}
	// The \r of a CRLF listing must not end up inside the transport id or the
	// state.
	if got[0].TransportID != 1 || got[0].State != StateDevice {
		t.Fatalf("CRLF line parsed as %+v", got[0])
	}
}

func TestSnapshotIndexesByPositionNotBySerial(t *testing.T) {
	t.Parallel()

	const dupA = "0123456789ABCDEF"
	snap := Snapshot{Devices: []Device{
		{Serial: dupA, Devpath: "usb:3-1.4.1", State: StateDevice},
		{Serial: dupA, Devpath: "usb:3-1.4.2", State: StateDevice},
		{Serial: "ZZZDUP", Devpath: "usb:3-1.5.1", State: StateOffline},
		{Serial: "ZZZDUP", Devpath: "usb:3-1.5.2", State: StateDevice},
		{Serial: "UNIQUE", Devpath: "usb:3-1.6.1", State: StateDevice},
		{Serial: "emulator-5554", Devpath: "", State: StateDevice},
		{Serial: "", Devpath: "usb:3-1.7.1", State: StateUnauthorized},
		{Serial: "", Devpath: "usb:3-1.7.2", State: StateUnauthorized},
	}}

	idx := snap.ByDevpath()
	if len(idx) != 7 {
		t.Fatalf("ByDevpath indexed %d devices, want 7 (the emulator has no position to key on)", len(idx))
	}
	if d := idx["usb:3-1.4.2"]; d.Serial != dupA {
		t.Fatalf("usb:3-1.4.2 indexed as %+v", d)
	}

	got := snap.AmbiguousSerials()
	want := []string{dupA, "ZZZDUP"}
	if !slices.Equal(got, want) {
		t.Fatalf("AmbiguousSerials() = %v, want %v (sorted, so two identical observations look identical)", got, want)
	}
	// Two devices whose serial the server has not read share the empty string.
	// Counting that as a collision would manufacture an ambiguity out of every
	// pair of unauthorized handsets on a host.
	for _, s := range got {
		if s == "" {
			t.Fatal("the empty serial was reported as ambiguous")
		}
	}
}

func TestDeviceTargetRefusesToFallBackToSerial(t *testing.T) {
	t.Parallel()

	positioned := Device{Serial: "SER0001", Devpath: "usb:3-1.4.1"}
	got, err := positioned.Target()
	if err != nil || got != "usb:3-1.4.1" {
		t.Fatalf("Target() = %q, %v; want the devpath and no error", got, err)
	}

	emulator := Device{Serial: "emulator-5554"}
	if _, err := emulator.Target(); !errors.Is(err, ErrInvalidDevpath) {
		t.Fatalf("Target() on a device with no devpath = %v, want ErrInvalidDevpath", err)
	} else if !strings.Contains(err.Error(), "emulator-5554") {
		t.Fatalf("error %q does not name the device an operator would have to look for", err)
	}
}

func TestValidateDevpath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"linux hub position", "usb:3-1.4.2", true},
		{"root port", "usb:1-1", true},
		{"deep hub chain", "usb:2-1.4.3.2", true},
		{"darwin location id", "usb:0123456789ABCDEF", true},
		{"empty", "", false},
		{"missing scheme", "3-1.4.2", false},
		{"scheme only", "usb:", false},
		{"upper case scheme", "USB:3-1.1", false},
		// The devpath is interpolated into a colon-delimited service string.
		// A value carrying its own colon could terminate the field early and
		// retarget the request at another device — and the request behind that
		// mistake may be a reboot aimed at a phone hours into somebody's run.
		{"embedded colon retargets the service string", "usb:3-1.4.2:reboot", false},
		{"embedded space", "usb:3-1.4.2 usb:3-1.4.1", false},
		{"embedded newline", "usb:3-1.4.2\nhost:kill", false},
		{"embedded nul", "usb:3-1.4.2\x00", false},
		{"leading punctuation in the body", "usb:-1", false},
		{"underscore body start", "usb:_3", false},
		{"tcp target is not a position", "tcp:5555", false},
		{"implausibly long", "usb:" + strings.Repeat("1", maxDevpathLen), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateDevpath(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateDevpath(%q) = %v, want nil", tc.in, err)
				}
			} else {
				if err == nil {
					t.Fatalf("ValidateDevpath(%q) accepted an unaddressable value", tc.in)
				}
				if !errors.Is(err, ErrInvalidDevpath) {
					t.Fatalf("ValidateDevpath(%q) = %v, which does not match ErrInvalidDevpath", tc.in, err)
				}
			}
			// The listing parser and the addressing calls must never disagree
			// about what a devpath is: a position recorded from a listing has
			// to be one this package will accept as a target.
			if isDevpath(tc.in) != tc.ok {
				t.Fatalf("isDevpath(%q) = %t but ValidateDevpath says %t; the two predicates have drifted",
					tc.in, isDevpath(tc.in), tc.ok)
			}
		})
	}
}

func TestValidateServiceString(t *testing.T) {
	t.Parallel()

	if err := validateServiceString("op", "shell,v2,raw:ls -la /sdcard"); err != nil {
		t.Fatalf("a legitimate service string was refused: %v", err)
	}
	for _, bad := range []string{"", "shell:echo \x00", strings.Repeat("x", maxMessage+1)} {
		err := validateServiceString("op", bad)
		if err == nil {
			t.Fatalf("validateServiceString accepted %d bytes / %q", len(bad), truncateForMessage(bad))
		}
		var ue *UsageError
		if !errors.As(err, &ue) {
			t.Fatalf("validateServiceString(%q) = %T, want *UsageError", truncateForMessage(bad), err)
		}
	}
}

func truncateForMessage(s string) string {
	if len(s) > 32 {
		return s[:32] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// shell v2 framing
// ---------------------------------------------------------------------------

func TestShellServiceRequestsV2Raw(t *testing.T) {
	t.Parallel()

	// v2 is what carries a real exit status; raw is what stops the pty from
	// mangling line endings in a captured log.
	if got, want := ShellService("logcat -d"), "shell,v2,raw:logcat -d"; got != want {
		t.Fatalf("ShellService = %q, want %q", got, want)
	}
}

func TestShellPacketRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writes := []struct {
		id      ShellPacketID
		payload []byte
	}{
		{ShellStdout, []byte("hello\n")},
		{ShellStderr, []byte("warn\n")},
		{ShellWindowSize, []byte("80x24")},
		{ShellStdout, nil},
		{ShellExit, []byte{7}},
	}
	for _, w := range writes {
		if err := WriteShellPacket(&buf, w.id, w.payload); err != nil {
			t.Fatalf("WriteShellPacket(%d): %v", w.id, err)
		}
	}

	pr := NewShellPacketReader(&buf)
	for i, w := range writes {
		id, payload, err := pr.Next()
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if id != w.id {
			t.Fatalf("packet %d id = %d, want %d", i, id, w.id)
		}
		if !bytes.Equal(payload, w.payload) && !(len(payload) == 0 && len(w.payload) == 0) {
			t.Fatalf("packet %d payload = %q, want %q", i, payload, w.payload)
		}
	}
	if _, _, err := pr.Next(); err != io.EOF {
		t.Fatalf("after the last packet: %v, want io.EOF on a clean boundary", err)
	}
}

func TestWriteShellPacketRefusesOversizePayload(t *testing.T) {
	t.Parallel()

	err := WriteShellPacket(io.Discard, ShellStdin, make([]byte, MaxShellPacket+1))
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("WriteShellPacket with an oversize payload = %v (%T), want *UsageError", err, err)
	}
}

func TestDrainShellV2(t *testing.T) {
	t.Parallel()

	build := func(pkts ...struct {
		id      ShellPacketID
		payload []byte
	}) []byte {
		var b bytes.Buffer
		for _, p := range pkts {
			if err := WriteShellPacket(&b, p.id, p.payload); err != nil {
				t.Fatalf("building fixture: %v", err)
			}
		}
		return b.Bytes()
	}
	type pkt = struct {
		id      ShellPacketID
		payload []byte
	}

	t.Run("demultiplexes and records the exit status", func(t *testing.T) {
		t.Parallel()
		stream := build(
			pkt{ShellStdout, []byte("out1")},
			pkt{ShellStderr, []byte("err1")},
			pkt{ShellStdout, []byte("out2")},
			pkt{ShellWindowSize, []byte("ignored")},
			pkt{ShellExit, []byte{3}},
		)
		res, err := DrainShellV2(bytes.NewReader(stream), 0)
		if err != nil {
			t.Fatalf("DrainShellV2: %v", err)
		}
		if string(res.Stdout) != "out1out2" || string(res.Stderr) != "err1" {
			t.Fatalf("stdout=%q stderr=%q", res.Stdout, res.Stderr)
		}
		if !res.Exited || res.ExitCode != 3 || res.Truncated {
			t.Fatalf("exited=%t code=%d truncated=%t", res.Exited, res.ExitCode, res.Truncated)
		}
	})

	t.Run("a stream that ends without an exit frame is not a failure", func(t *testing.T) {
		t.Parallel()
		res, err := DrainShellV2(bytes.NewReader(build(pkt{ShellStdout, []byte("partial")})), 0)
		if err != nil {
			t.Fatalf("DrainShellV2: %v", err)
		}
		// -1 and Exited=false: "the device never told us" is not "it exited 0".
		if res.Exited || res.ExitCode != -1 {
			t.Fatalf("exited=%t code=%d, want false/-1", res.Exited, res.ExitCode)
		}
	})

	t.Run("an empty exit frame does not fabricate a status", func(t *testing.T) {
		t.Parallel()
		res, err := DrainShellV2(bytes.NewReader(build(pkt{ShellExit, nil})), 0)
		if err != nil {
			t.Fatalf("DrainShellV2: %v", err)
		}
		if res.Exited || res.ExitCode != -1 {
			t.Fatalf("exited=%t code=%d, want false/-1", res.Exited, res.ExitCode)
		}
	})

	t.Run("a chatty command is truncated, not failed", func(t *testing.T) {
		t.Parallel()
		stream := build(
			pkt{ShellStdout, []byte("0123456789")},
			pkt{ShellStdout, []byte("abcdefghij")},
			pkt{ShellExit, []byte{0}},
		)
		res, err := DrainShellV2(bytes.NewReader(stream), 12)
		if err != nil {
			t.Fatalf("DrainShellV2: %v", err)
		}
		if !res.Truncated || len(res.Stdout) != 12 {
			t.Fatalf("truncated=%t stdout=%q", res.Truncated, res.Stdout)
		}
		// The exit frame still lands: the cap bounds capture, not parsing.
		if !res.Exited || res.ExitCode != 0 {
			t.Fatalf("exited=%t code=%d", res.Exited, res.ExitCode)
		}
	})

	t.Run("a stream cut mid-payload is an unexpected EOF", func(t *testing.T) {
		t.Parallel()
		full := build(pkt{ShellStdout, []byte("abcdef")})
		_, err := DrainShellV2(bytes.NewReader(full[:shellHeaderLen+2]), 0)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("cut mid-payload = %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("a stream cut mid-header is an unexpected EOF", func(t *testing.T) {
		t.Parallel()
		_, err := DrainShellV2(bytes.NewReader([]byte{byte(ShellStdout), 0x01, 0x00}), 0)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("cut mid-header = %v, want io.ErrUnexpectedEOF", err)
		}
	})

	t.Run("a desynchronised length is refused before it is allocated", func(t *testing.T) {
		t.Parallel()
		// 0x00200001 bytes claimed by four garbage bytes. Without the cap this
		// asks the process for two megabytes on no evidence at all, and a
		// larger garbage value asks for four gigabytes.
		oversize := []byte{byte(ShellStdout), 0x01, 0x00, 0x20, 0x00}
		_, err := DrainShellV2(bytes.NewReader(oversize), 0)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversize packet = %v, want a cap error", err)
		}
	})
}
