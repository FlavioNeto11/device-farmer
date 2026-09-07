package ctl

// `ctl device screen` — the assertions are the ones that decide whether this
// command can be believed as the API's second witness.
//
// Every test here serves the real framing over a real HTTP server. That is not
// thoroughness for its own sake: the whole value of this command is that it
// reads the same bytes the dashboard does, so a test that mocked the decoding
// would be testing a different program. The bytes are synthesised below rather
// than committed, and the last test generates genuine H.264 with ffmpeg and
// asserts the recording is byte-identical to it.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Synthesising the wire
// ---------------------------------------------------------------------------

// The three flag bits of a twelve-byte header.
//
// These mirror internal/scrcpy, which documents them against scrcpy's own
// app/src/demuxer.c: bit 63 says the header is a SESSION header, and only when
// it is clear do bits 62 and 61 mean config and key frame with the low
// sixty-one bits carrying the PTS. A synthesiser that put config in bit 63 —
// the shape a prose summary of this framing tends to collapse into, because it
// describes the session header as a one-time preamble rather than as a header
// that arrives again on every rotation — would emit a stream whose every packet
// reads as a rotation, and internal/scrcpy would report a width where this test
// meant a timestamp. The counts asserted below are what keeps these constants
// honest.
const (
	bitSession  uint64 = 1 << 63
	bitConfig   uint64 = 1 << 62
	bitKeyFrame uint64 = 1 << 61
)

// screenStream builds the body GET …/screen writes.
type screenStream struct {
	bytes.Buffer
}

func newScreenStream() *screenStream {
	s := &screenStream{}
	s.WriteString("h264")
	return s
}

func (s *screenStream) session(w, h uint32) {
	var hdr [12]byte
	binary.BigEndian.PutUint64(hdr[0:8], bitSession)
	binary.BigEndian.PutUint32(hdr[4:8], w)
	binary.BigEndian.PutUint32(hdr[8:12], h)
	s.Write(hdr[:])
}

func (s *screenStream) packet(config, key bool, pts uint64, payload []byte) {
	var hdr [12]byte
	meta := pts
	if config {
		meta |= bitConfig
	}
	if key {
		meta |= bitKeyFrame
	}
	binary.BigEndian.PutUint64(hdr[0:8], meta)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(payload)))
	s.Write(hdr[:])
	s.Write(payload)
}

// ---------------------------------------------------------------------------
// A farm that serves one
// ---------------------------------------------------------------------------

const screenTestSession = "scr-7f3a"

func screenDeviceDoc(leased bool) map[string]any {
	dev := map[string]any{
		"device_id": "dev-uuid-1", "farm_uid": "df-" + strings.Repeat("a", 32),
		"rack_slot": "R1-U3-P2", "host_id": "h01", "adb_devpath": "1-1.4.2",
		"health": "healthy", "pool": "default", "admin_state": "active",
	}
	if leased {
		dev["lease"] = map[string]any{
			"id": "lease-7", "state": "live", "protected": true, "fence": 41,
			"job_id": "J-9", "tenant_id": "acme", "holder": "runner-7",
		}
	}
	return map[string]any{"device": dev}
}

// screenFake is a farm with one device, optionally leased.
func screenFake(t *testing.T, leased bool) *fakeAPI {
	t.Helper()
	api := newFakeAPI(t)
	api.reply("GET /api/v1/devices/d1", http.StatusOK, screenDeviceDoc(leased))
	return api
}

// serveStream registers the screen route with the contract's headers and body.
func (f *fakeAPI) serveStream(body []byte) {
	f.serveScreenFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	})
}

// serveScreenFunc registers the screen route, writing the contract's headers
// first so the handler only has to write bytes.
func (f *fakeAPI) serveScreenFunc(fn func(http.ResponseWriter, *http.Request)) {
	f.mux.HandleFunc("GET /api/v1/devices/d1/screen", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", screenMediaType)
		w.Header().Set(headerScreenSession, screenTestSession)
		w.Header().Set(headerScreenDevice, "dev-uuid-1")
		w.WriteHeader(http.StatusOK)
		fn(w, r)
	})
}

// acceptInput registers the input route, answering the way the contract says.
//
// The count is fixed rather than computed from the body because newFakeAPI's
// recorder has already drained it — which is also what makes the body
// assertions below possible, since they read the recorded copy.
func (f *fakeAPI) acceptInput() {
	f.reply("POST /api/v1/devices/d1/input", http.StatusOK, map[string]any{"accepted": 2})
}

// flush pushes what has been written to the client, so a test can hold a
// stream open at a chosen point.
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// The video path
// ---------------------------------------------------------------------------

// TestScreenWritesThePayloadsAndNothingElse is the command's whole purpose in
// one assertion: what lands in --out is the concatenated payloads, with the
// framing stripped and the config packet kept. A file missing the config packet
// has no SPS or PPS in it, which no decoder can start from — and the failure
// looks like a corrupt recording rather than like a dropped packet.
//
// Falsify: skip config packets when writing to the sink (`if unit.Packet.Config
// { break }` before video.Write), and the golden comparison fails.
func TestScreenWritesThePayloadsAndNothingElse(t *testing.T) {
	config := []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x42}
	key := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x01, 0x02}
	delta := []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0x9a}

	s := newScreenStream()
	s.session(480, 1024)
	s.packet(true, false, 0, config)
	s.packet(false, true, 100000, key)
	s.packet(false, false, 200000, delta)

	api := screenFake(t, false)
	api.serveStream(s.Bytes())

	dst := filepath.Join(t.TempDir(), "rec.h264")
	out, errOut, err := api.run(t, "device", "screen", "d1", "--out", dst, "--seconds", "0")
	if err != nil {
		t.Fatalf("a stream that carried three packets failed: %v\nstderr: %s", err, errOut)
	}

	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("the recording is not readable: %v", readErr)
	}
	want := append(append(append([]byte{}, config...), key...), delta...)
	if !bytes.Equal(got, want) {
		t.Fatalf("the recording is not the payloads:\n got %x\nwant %x", got, want)
	}
	if out != "" {
		t.Errorf("stdout carried something while --out named a file; the video and the report must not "+
			"share a stream:\n%s", out)
	}

	for _, want := range []string{
		screenTestSession,           // which session input must be addressed to
		"h264",                      // what the stream announced
		"video: 480x1024",           // the coordinate space --tap is measured in
		"3 packet(s)",               //
		"1 key frame(s)",            //
		"1 config packet(s)",        // the one a naive recording drops
		"PTS span 0.100s",           // device time, which is not wall time
		"ffplay -f h264",            // the thing an operator otherwise gets wrong
		"no container, no frame ra", // and why
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the report omits %q:\n%s", want, errOut)
		}
	}
}

// TestScreenSendsVideoToStdoutOnlyWhenAsked pins the stream split. stdout is
// the video's when --out is -, and the report is always stderr's: a report
// written to stdout would be spliced into the middle of an elementary stream
// and break every frame after it.
func TestScreenSendsVideoToStdoutOnlyWhenAsked(t *testing.T) {
	payload := []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x11, 0x22}
	s := newScreenStream()
	s.session(320, 640)
	s.packet(false, true, 1, payload)

	api := screenFake(t, false)
	api.serveStream(s.Bytes())

	out, errOut, err := api.run(t, "device", "screen", "d1", "--out", "-", "--seconds", "0")
	if err != nil {
		t.Fatalf("streaming to stdout failed: %v\nstderr: %s", err, errOut)
	}
	if out != string(payload) {
		t.Fatalf("stdout is not exactly the payload:\n got %x\nwant %x", out, payload)
	}
	if !strings.Contains(errOut, "1 packet(s)") {
		t.Errorf("the report did not reach stderr:\n%s", errOut)
	}

	// Without --out nothing is recorded at all, and the command still reports.
	out, errOut, err = api.run(t, "device", "screen", "d1", "--seconds", "0")
	if err != nil {
		t.Fatalf("a run with no --out failed: %v\nstderr: %s", err, errOut)
	}
	if out != "" {
		t.Errorf("a run with no --out wrote video anyway:\n%x", out)
	}
	if !strings.Contains(errOut, "1 packet(s)") || strings.Contains(errOut, "ffplay") {
		t.Errorf("a run with no --out reported wrongly:\n%s", errOut)
	}
}

// TestScreenWithNoPacketsIsNotASuccess. A stream that opens, says h264, gives
// its size and then ends is the single most misleading outcome this route has:
// every HTTP-level thing worked. Exit 0 there would hand a CI job a green tick
// for a black screen, and an operator the conclusion that the farm is fine.
//
// Falsify: return nil from screenOutcome when streamErr is io.EOF regardless of
// the packet count, and this exits 0.
func TestScreenWithNoPacketsIsNotASuccess(t *testing.T) {
	s := newScreenStream()
	s.session(480, 1024)

	api := screenFake(t, false)
	api.serveStream(s.Bytes())

	_, errOut, err := api.run(t, "device", "screen", "d1", "--seconds", "0")
	if err == nil {
		t.Fatal("a stream that produced no video exited 0; a black screen now reads as a working farm")
	}
	if got := ExitCode(err); got != 1 {
		t.Fatalf("zero packets exited %d (%v), want 1", got, err)
	}
	if !strings.Contains(err.Error(), "zero packets") || !strings.Contains(err.Error(), "encoder produced no video") {
		t.Errorf("the message does not say what happened: %q", err.Error())
	}
	// And it must not turn a silent encoder into a claim about the lease.
	if !strings.Contains(err.Error(), "nothing here released anything") {
		t.Errorf("the message leaves the lease in doubt: %q", err.Error())
	}
	if !strings.Contains(errOut, "0 packet(s)") {
		t.Errorf("the summary did not report the count:\n%s", errOut)
	}
}

// TestScreenTransportFailureIsNamedAsOneAndBlamesNoLease is the rule in
// internal/ctl's package doc, asserted. A stream that dies mid-payload is a
// dead socket; the holder still holds the device and the job is still running.
// A message that read "the device went away" would send an operator to revoke a
// lease over a proxy timeout.
//
// Falsify: drop the "no lease, job or session state follows" clause from the
// transport-failure message, or report the truncation as a refusal, and this
// fails.
func TestScreenTransportFailureIsNamedAsOneAndBlamesNoLease(t *testing.T) {
	s := newScreenStream()
	s.session(480, 1024)
	s.packet(false, true, 1, []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0xaa})
	// A header promising sixty-four bytes, followed by six and a closed socket.
	truncated := s.Bytes()
	var hdr [12]byte
	binary.BigEndian.PutUint64(hdr[0:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], 64)
	truncated = append(truncated, hdr[:]...)
	truncated = append(truncated, 0x00, 0x00, 0x00, 0x01, 0x41, 0x9a)

	api := screenFake(t, false)
	api.serveStream(truncated)

	dst := filepath.Join(t.TempDir(), "partial.h264")
	_, errOut, err := api.run(t, "device", "screen", "d1", "--out", dst, "--seconds", "0")
	if err == nil {
		t.Fatal("a stream that ended mid-packet exited 0")
	}
	if got := ExitCode(err); got != 1 {
		t.Fatalf("a truncated stream exited %d (%v), want 1 — it is a transport failure, not a refusal "+
			"and not a partial run", got, err)
	}
	for _, want := range []string{"transport failure", "no lease, job or session state follows",
		"nothing here released anything"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure message omits %q: %q", want, err.Error())
		}
	}
	// The packet that did arrive is still on disk: a cut-short recording is a
	// recording, and a lost flush would silently shorten it.
	got, _ := os.ReadFile(dst)
	if len(got) != 6 {
		t.Errorf("the packet that arrived before the stream died was not flushed: %d byte(s)", len(got))
	}
	if !strings.Contains(errOut, "1 packet(s)") {
		t.Errorf("the summary did not report what did arrive:\n%s", errOut)
	}
}

// TestScreenStopsAtWhicheverLimitComesFirst. --frames and --seconds are both
// limits, and a stream is endless, so a command that honoured only one of them
// would either record forever or stop early on a slow encoder.
func TestScreenStopsAtWhicheverLimitComesFirst(t *testing.T) {
	t.Run("frames", func(t *testing.T) {
		s := newScreenStream()
		s.session(480, 1024)
		for i := 0; i < 6; i++ {
			s.packet(false, i == 0, uint64(i)*1000, []byte{byte(i), byte(i)})
		}
		api := screenFake(t, false)
		// The handler holds the stream open after writing, so nothing but the
		// frame limit can end this run.
		body := s.Bytes()
		api.serveScreenFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(body)
			flush(w)
			<-r.Context().Done()
		})

		dst := filepath.Join(t.TempDir(), "two.h264")
		_, errOut, err := api.run(t, "device", "screen", "d1", "--out", dst, "--frames", "2")
		if err != nil {
			t.Fatalf("--frames 2 failed: %v\nstderr: %s", err, errOut)
		}
		if !strings.Contains(errOut, "2 packet(s), 1 key frame(s), 0 config packet(s)") {
			t.Errorf("--frames 2 did not stop at two packets:\n%s", errOut)
		}
		got, _ := os.ReadFile(dst)
		if want := []byte{0, 0, 1, 1}; !bytes.Equal(got, want) {
			t.Errorf("the recording is %x, want the first two payloads %x", got, want)
		}
	})

	// Falsify: remove the time.AfterFunc that cancels the stream context and
	// this subtest hangs until the test binary's own deadline.
	t.Run("seconds", func(t *testing.T) {
		s := newScreenStream()
		s.session(480, 1024)
		s.packet(false, true, 1, []byte{0xaa, 0xbb})
		body := s.Bytes()

		api := screenFake(t, false)
		api.serveScreenFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(body)
			flush(w)
			<-r.Context().Done()
		})

		start := time.Now()
		_, errOut, err := api.run(t, "device", "screen", "d1", "--seconds", "1")
		if err != nil {
			t.Fatalf("--seconds 1 against a stream that never ends failed: %v\nstderr: %s", err, errOut)
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Fatalf("--seconds 1 ran for %s; the deadline did not stop the stream", elapsed)
		}
		if !strings.Contains(errOut, "1 packet(s)") {
			t.Errorf("the summary did not report the packet that arrived:\n%s", errOut)
		}
	})
}

// TestScreenRejectsABodyThatIsNotAScreen. A 200 carrying a proxy's HTML is the
// likeliest wrong answer on this route, and decoding it as a codec id reports
// "codec 0x3c21444f" — which sends the reader to grep the scrcpy source for a
// number that is the first four bytes of "<!DO".
func TestScreenRejectsABodyThatIsNotAScreen(t *testing.T) {
	api := screenFake(t, false)
	api.mux.HandleFunc("GET /api/v1/devices/d1/screen", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!DOCTYPE html><title>Sign in</title>"))
	})

	_, _, err := api.run(t, "device", "screen", "d1", "--seconds", "0")
	if err == nil {
		t.Fatal("an HTML login page was accepted as a screen stream")
	}
	if !strings.Contains(err.Error(), "text/html") || !strings.Contains(err.Error(), screenMediaType) {
		t.Errorf("the message does not name what arrived and what was expected: %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// The destructive gate
// ---------------------------------------------------------------------------

// TestScreenRefusesALeasedDeviceWithoutAReason. A screen on a leased device is
// exec's blast radius: a process started on a phone somebody's job is using,
// with --tap injecting real input into their run. The reason lands in
// farm.audit_log beside the operator's name and is the only surviving record of
// why somebody watched a tenant's screen, so it is demanded before the
// confirmation rather than after — and before one byte reaches the handset.
//
// Falsify: move the requireReason call after e.confirm, or gate it on *force
// alone, and the first case here reaches the screen route.
func TestScreenRefusesALeasedDeviceWithoutAReason(t *testing.T) {
	api := screenFake(t, true)
	api.serveStream(newScreenStream().Bytes())

	_, _, err := api.run(t, "device", "screen", "d1", "--yes")
	if got := ExitCode(err); got != 2 {
		t.Fatalf("a leased device with no --reason exited %d (%v), want 2", got, err)
	}
	if !strings.Contains(err.Error(), "--reason") || !strings.Contains(err.Error(), "audit_log") {
		t.Errorf("the refusal does not say what is missing or why: %q", err.Error())
	}
	for _, r := range api.requests() {
		if strings.Contains(r.Path, "/screen") {
			t.Fatalf("an unreasoned screen reached the wire: %+v", r)
		}
	}

	// With a reason but no terminal and no --yes there is nobody to confirm
	// with, which is the same refusal every destructive command here gives.
	_, errOut, err := api.run(t, "device", "screen", "d1", "--reason", "triage")
	if got := ExitCode(err); got != 2 {
		t.Fatalf("a leased device with nobody to confirm exited %d (%v), want 2", got, err)
	}
	// The blast radius is printed before the question, always — so it is in the
	// scrollback even when the answer was no.
	for _, want := range []string{"holds a live lease", "runner-7", "J-9", "acme", "Without --force"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the preflight omits %q:\n%s", want, errOut)
		}
	}
	for _, r := range api.requests() {
		if strings.Contains(r.Path, "/screen") {
			t.Fatalf("an unconfirmed screen reached the wire: %+v", r)
		}
	}
}

// TestScreenForcingALeasedDeviceSendsForceAndReason. --force is a query
// parameter the server reads, and a --reason that stayed on the client is an
// unaudited look at somebody else's screen. The headline has to say what makes
// this different from an idle device, too: the holder is given no signal.
func TestScreenForcingALeasedDeviceSendsForceAndReason(t *testing.T) {
	s := newScreenStream()
	s.session(480, 1024)
	s.packet(false, true, 5, []byte{0x01, 0x02, 0x03})

	api := screenFake(t, true)
	api.serveStream(s.Bytes())

	_, errOut, err := api.run(t, "device", "screen", "d1", "--force", "--reason", "operator triage",
		"--yes", "--seconds", "0", "--max-size", "720")
	if err != nil {
		t.Fatalf("a forced screen failed: %v\nstderr: %s", err, errOut)
	}
	var screen *recorded
	for i, r := range api.requests() {
		if strings.HasSuffix(r.Path, "/screen") {
			screen = &api.requests()[i]
		}
	}
	if screen == nil {
		t.Fatal("the screen route was never called")
	}
	for _, want := range []string{"force=true", "reason=operator+triage", "max_size=720"} {
		if !strings.Contains(screen.Query, want) {
			t.Errorf("the request ?%s omits %s", screen.Query, want)
		}
	}
	for _, want := range []string{"WHILE SOMEBODY'S JOB IS USING IT", "no signal", "ends their lease"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the forced headline omits %q:\n%s", want, errOut)
		}
	}
}

// TestScreenOnAnIdleDeviceAsksNobody. A device nobody holds has nothing to
// disturb, and demanding --reason for it would train operators to pass the flag
// by reflex — which is how the reason on the destructive commands becomes
// "triage" forever.
func TestScreenOnAnIdleDeviceAsksNobody(t *testing.T) {
	s := newScreenStream()
	s.session(480, 1024)
	s.packet(false, true, 5, []byte{0x09})

	api := screenFake(t, false)
	api.serveStream(s.Bytes())

	_, errOut, err := api.run(t, "device", "screen", "d1", "--seconds", "0")
	if err != nil {
		t.Fatalf("a screen on an idle device failed: %v\nstderr: %s", err, errOut)
	}
	if strings.Contains(errOut, "Type \"yes\"") || strings.Contains(errOut, "proceeding (--yes)") {
		t.Errorf("an idle device was treated as destructive:\n%s", errOut)
	}
	var screen *recorded
	for i, r := range api.requests() {
		if strings.HasSuffix(r.Path, "/screen") {
			screen = &api.requests()[i]
		}
	}
	if screen == nil {
		t.Fatal("the screen route was never called")
	}
	if strings.Contains(screen.Query, "force") {
		t.Errorf("force was sent without being asked for: ?%s", screen.Query)
	}
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

// TestScreenRefusalsAreAnswersWithTheirDetailRendered. Each of the contract's
// refusals is a sentence an operator acts on, and all of the actionable content
// is in the envelope's detail — which *RemoteError does not print. A 409 must
// also stay exit 3: a script that retries on 1 and stops on 3 is a script that
// does not fight the farm.
func TestScreenRefusalsAreAnswersWithTheirDetailRendered(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		code     string
		message  string
		detail   map[string]any
		wantExit int
		wantErr  []string
		wantOut  []string
	}{
		{
			name: "a session is already open", status: http.StatusConflict, code: "conflict",
			message: "a screen session is already open on this device",
			detail:  map[string]any{"reason": "session_already_open"},
			// 3, not 1: the farm worked and said no.
			wantExit: 3,
			wantErr:  []string{"a screen session is already open"},
			wantOut:  []string{"One encoder, one session", "no lease was touched"},
		},
		{
			name: "a live lease and no force", status: http.StatusConflict, code: "conflict",
			message: "this device holds a live lease",
			detail: map[string]any{"lease_id": "lease-7", "job_id": "J-9", "tenant_id": "acme",
				"holder": "runner-7", "protected": true},
			wantExit: 3,
			wantErr:  []string{"holds a live lease"},
			wantOut:  []string{"lease-7", "runner-7", "--force --reason", "untouched"},
		},
		{
			name: "the farm has no screen server", status: http.StatusServiceUnavailable, code: "unavailable",
			message: "this farm has no screen server configured",
			detail: map[string]any{"fault": "configuration",
				"remedy": "set FARM_SCRCPY_SERVER_JAR and FARM_FENCE_CLIENT_CERT"},
			// 1, not 3: nothing was refused, the farm cannot do this at all.
			wantExit: 1,
			wantErr:  []string{"no screen server configured"},
			wantOut:  []string{"FARM_SCRCPY_SERVER_JAR", "farm to configure, not a device"},
		},
		{
			name: "the handset would not start it", status: http.StatusBadGateway, code: "adb_error",
			message:  "could not start the screen server on the device",
			wantExit: 1,
			wantErr:  []string{"could not start the screen server"},
			wantOut:  []string{"ctl device <id>", "nothing here released anything"},
		},
		{
			name: "a tenant credential", status: http.StatusForbidden, code: "forbidden",
			message:  "a screen is operator-only",
			wantExit: 1,
			wantErr:  []string{"operator-only"},
			wantOut:  []string{"another tenant's app", "no tenant-scoped version"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := screenFake(t, false)
			envelope := map[string]any{"code": c.code, "message": c.message}
			if c.detail != nil {
				envelope["detail"] = c.detail
			}
			api.reply("GET /api/v1/devices/d1/screen", c.status, map[string]any{"error": envelope})

			_, errOut, err := api.run(t, "device", "screen", "d1", "--reason", "triage", "--yes", "--seconds", "0")
			if got := ExitCode(err); got != c.wantExit {
				t.Fatalf("%s exited %d (%v), want %d", c.name, got, err, c.wantExit)
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the envelope's message was not rendered (%q): %q", want, err.Error())
				}
			}
			for _, want := range c.wantOut {
				if !strings.Contains(errOut, want) {
					t.Errorf("the detail's guidance omits %q:\n%s", want, errOut)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The input path
// ---------------------------------------------------------------------------

// TestScreenTapsWaitForAKeyFrameThenSendADownAndAnUp. Before a key frame there
// is nothing decodable on the wire, so there is no evidence that the screen
// being aimed at is the screen the phone is showing. And the down and the up
// travel in one request because a down that was accepted without its up leaves
// the handset believing a finger is still on the glass.
//
// Falsify: move the sendTaps call so it fires on the first packet rather than
// on the first key frame, and the ordering assertion below sees "input" before
// "keyframe".
func TestScreenTapsWaitForAKeyFrameThenSendADownAndAnUp(t *testing.T) {
	var (
		mu       sync.Mutex
		order    []string
		inputGot = make(chan struct{}, 1)
	)
	record := func(what string) {
		mu.Lock()
		order = append(order, what)
		mu.Unlock()
	}

	api := screenFake(t, false)
	api.mux.HandleFunc("POST /api/v1/devices/d1/input", func(w http.ResponseWriter, r *http.Request) {
		record("input")
		select {
		case inputGot <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": 2})
	})

	head := newScreenStream()
	head.session(480, 1024)
	head.packet(true, false, 0, []byte{0x67, 0x42})     // config: not displayable
	head.packet(false, false, 1000, []byte{0x41, 0x9a}) // a delta frame: not a start point
	tail := &screenStream{}
	tail.packet(false, true, 2000, []byte{0x65, 0x88})

	api.serveScreenFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(head.Bytes())
		flush(w)
		// Give a tap that fires too early time to arrive. Waiting on the input
		// route rather than only on a clock is what makes the wrong behaviour
		// fail fast instead of flaking.
		select {
		case <-inputGot:
		case <-time.After(250 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		record("keyframe")
		_, _ = w.Write(tail.Bytes())
		flush(w)
	})

	_, errOut, err := api.run(t, "device", "screen", "d1", "--tap", "120,340", "--seconds", "0")
	if err != nil {
		t.Fatalf("a tapped screen failed: %v\nstderr: %s", err, errOut)
	}

	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if got != "keyframe,input" {
		t.Fatalf("the tap did not wait for a key frame: %q", got)
	}

	var body string
	for _, r := range api.requests() {
		if strings.HasSuffix(r.Path, "/input") {
			body = r.Body
		}
	}
	var sent screenInputRequest
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("the input body is not the contract's document (%v): %s", err, body)
	}
	if sent.Session != screenTestSession {
		t.Errorf("the tap was addressed to session %q, not the one the stream gave (%q)",
			sent.Session, screenTestSession)
	}
	if len(sent.Events) != 2 {
		t.Fatalf("a tap sent %d event(s), want a down and an up: %s", len(sent.Events), body)
	}
	down, up := sent.Events[0], sent.Events[1]
	if down.Type != "touch" || down.Action != "down" || down.X != 120 || down.Y != 340 || down.Pressure != 1 {
		t.Errorf("the down is wrong: %+v", down)
	}
	if up.Type != "touch" || up.Action != "up" || up.X != 120 || up.Y != 340 {
		t.Errorf("the up is wrong: %+v", up)
	}
	if down.PointerID != up.PointerID {
		t.Errorf("the down and the up name different fingers (%d and %d); the handset would believe one "+
			"is still on the glass", down.PointerID, up.PointerID)
	}
	if !strings.Contains(errOut, "tap 1/1 at 120,340: accepted 2") {
		t.Errorf("the tap's outcome was not reported:\n%s", errOut)
	}
}

// TestScreenTapOutsideTheFrameIsRefusedAgainstTheVideoSize. The coordinate
// space is the VIDEO's, after --max-size scaling and rotation, and the mistake
// this catches is the one the design document calls out: device pixels typed
// where video coordinates belong. The server would answer 400; the frame size
// is on this side, so the sentence an operator needs can be said here.
func TestScreenTapOutsideTheFrameIsRefusedAgainstTheVideoSize(t *testing.T) {
	s := newScreenStream()
	s.session(480, 1024)
	s.packet(false, true, 1, []byte{0x65, 0x01})

	api := screenFake(t, false)
	api.serveStream(s.Bytes())
	api.acceptInput()

	_, errOut, err := api.run(t, "device", "screen", "d1", "--tap", "1080,2400", "--tap", "10,20", "--seconds", "0")
	// The video arrived, so this is partial, not a failure of the command.
	if got := ExitCode(err); got != 4 {
		t.Fatalf("a tap outside the frame exited %d (%v), want 4: the video arrived", got, err)
	}
	for _, want := range []string{"NOT SENT", "outside the 480x1024 video frame", "VIDEO coordinates",
		"not device pixels", "tap 2/2 at 10,20: accepted 2"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the report omits %q:\n%s", want, errOut)
		}
	}
	// The out-of-frame tap never reached the wire; the in-frame one did.
	inputs := 0
	for _, r := range api.requests() {
		if strings.HasSuffix(r.Path, "/input") {
			inputs++
			if strings.Contains(r.Body, "1080") {
				t.Errorf("a tap known to be outside the frame was sent anyway: %s", r.Body)
			}
		}
	}
	if inputs != 1 {
		t.Errorf("%d input request(s) were sent, want 1", inputs)
	}
}

// TestScreenTapRefusedBySessionIsPartialNotFailure. A 409 session_not_here
// means this replica does not hold the session: the video is already on disk
// and only the input went nowhere. Exit 1 would tell a script the recording
// failed, and exit 3 would say the whole action was refused.
func TestScreenTapRefusedBySessionIsPartialNotFailure(t *testing.T) {
	s := newScreenStream()
	s.session(480, 1024)
	s.packet(false, true, 1, []byte{0x65, 0x01})

	api := screenFake(t, false)
	api.serveStream(s.Bytes())
	api.reply("POST /api/v1/devices/d1/input", http.StatusConflict, map[string]any{
		"error": map[string]any{"code": "conflict", "message": "this replica does not hold that session",
			"detail": map[string]any{"reason": "session_not_here"}},
	})

	dst := filepath.Join(t.TempDir(), "rec.h264")
	_, errOut, err := api.run(t, "device", "screen", "d1", "--tap", "10,20", "--out", dst, "--seconds", "0")
	if got := ExitCode(err); got != 4 {
		t.Fatalf("a refused tap exited %d (%v), want 4", got, err)
	}
	if !strings.Contains(err.Error(), "1 of 1 tap(s)") || !strings.Contains(err.Error(), "video arrived") {
		t.Errorf("the outcome does not say which half worked: %q", err.Error())
	}
	if !strings.Contains(errOut, "does not hold that session") {
		t.Errorf("the refusal's own words are missing:\n%s", errOut)
	}
	if got, _ := os.ReadFile(dst); len(got) != 2 {
		t.Errorf("the video was not recorded even though only the input was refused: %d byte(s)", len(got))
	}
}

// TestScreenRejectsAMalformedTapBeforeTouchingAnything. A --tap that cannot be
// parsed is a typo, and discovering it after a screen server has started on a
// handset wastes a device's time for nothing.
func TestScreenRejectsAMalformedTapBeforeTouchingAnything(t *testing.T) {
	for _, bad := range []string{"120", "a,b", "-1,20", "120,"} {
		api := screenFake(t, false)
		api.serveStream(newScreenStream().Bytes())
		_, _, err := api.run(t, "device", "screen", "d1", "--tap", bad)
		if got := ExitCode(err); got != 2 {
			t.Errorf("--tap %q exited %d (%v), want 2", bad, got, err)
		}
		if n := len(api.requests()); n != 0 {
			t.Errorf("--tap %q still called the server %d time(s)", bad, n)
		}
	}
}

// TestScreenRefusesToShareStdout. --out - and -o json both write to stdout, and
// the mixture is neither playable nor parseable. Naming the conflict beats
// emitting it and letting ffplay find out.
func TestScreenRefusesToShareStdout(t *testing.T) {
	api := screenFake(t, false)
	api.serveStream(newScreenStream().Bytes())
	_, _, err := api.run(t, "device", "screen", "d1", "--out", "-", "-o", "json")
	if got := ExitCode(err); got != 2 {
		t.Fatalf("--out - with -o json exited %d (%v), want 2", got, err)
	}
	if n := len(api.requests()); n != 0 {
		t.Errorf("the refused invocation still called the server %d time(s)", n)
	}
}

// TestScreenJSONSummaryIsTheMeasurement. -o json is what a CI job reads, and
// the numbers it needs are counts of what arrived — which no server document
// can carry, because only this side counted them.
func TestScreenJSONSummaryIsTheMeasurement(t *testing.T) {
	s := newScreenStream()
	s.session(480, 1024)
	s.packet(true, false, 0, []byte{0x67})
	s.packet(false, true, 100000, []byte{0x65, 0x01})
	s.packet(false, false, 183333, []byte{0x41})

	api := screenFake(t, false)
	api.serveStream(s.Bytes())
	api.acceptInput()

	out, errOut, err := api.run(t, "device", "screen", "d1", "-o", "json", "--tap", "5,6", "--seconds", "0")
	if err != nil {
		t.Fatalf("-o json failed: %v\nstderr: %s", err, errOut)
	}
	var got screenSummary
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the summary is not JSON (%v): %s", err, out)
	}
	if got.Session != screenTestSession || got.DeviceID != "dev-uuid-1" || got.Codec != "h264" {
		t.Errorf("the summary misidentifies the stream: %+v", got)
	}
	if got.Width != 480 || got.Height != 1024 {
		t.Errorf("the summary reports %dx%d, not the session header's 480x1024", got.Width, got.Height)
	}
	if got.Packets != 3 || got.KeyFrames != 1 || got.ConfigPackets != 1 || got.SessionHeaders != 1 {
		t.Errorf("the counts are wrong: %+v", got)
	}
	if got.Bytes != 4 {
		t.Errorf("the summary counted %d payload byte(s), want 4", got.Bytes)
	}
	if got.PTSSpanUS != 83333 {
		t.Errorf("the PTS span is %d µs, want 83333 — a config packet's stamp must not be in it", got.PTSSpanUS)
	}
	if len(got.Taps) != 1 || got.Taps[0].Accepted != 2 || got.Taps[0].Error != "" {
		t.Errorf("the tap outcome is not in the summary: %+v", got.Taps)
	}
}

// TestScreenSaysSoWhenATapNeverLeft. The tap fires on the first key frame, so a
// window that contains none — or a stream that named no session — sends
// nothing. Reporting that as success would give a CI job a green tick for a run
// that never called the input route at all, which is the one outcome this
// command must not produce, since being the witness for that route is why it
// exists.
//
// Falsify: delete the `if len(r.taps) > 0 && !tapped` block after the read loop
// and this exits 0 in silence.
func TestScreenSaysSoWhenATapNeverLeft(t *testing.T) {
	t.Run("no key frame in the window", func(t *testing.T) {
		s := newScreenStream()
		s.session(480, 1024)
		s.packet(false, false, 1000, []byte{0x41, 0x9a}) // a delta frame, and only that

		api := screenFake(t, false)
		api.serveStream(s.Bytes())
		api.acceptInput()

		_, errOut, err := api.run(t, "device", "screen", "d1", "--tap", "10,20", "--seconds", "0")
		if got := ExitCode(err); got != 4 {
			t.Fatalf("a tap that never left exited %d (%v), want 4", got, err)
		}
		if !strings.Contains(errOut, "NOT SENT") || !strings.Contains(errOut, "no key frame arrived") {
			t.Errorf("nothing said the tap was never sent:\n%s", errOut)
		}
		for _, r := range api.requests() {
			if strings.HasSuffix(r.Path, "/input") {
				t.Errorf("input was sent with no key frame to aim at: %+v", r)
			}
		}
	})

	t.Run("no session header", func(t *testing.T) {
		s := newScreenStream()
		s.session(480, 1024)
		s.packet(false, true, 1000, []byte{0x65, 0x01})

		api := screenFake(t, false)
		body := s.Bytes()
		api.mux.HandleFunc("GET /api/v1/devices/d1/screen", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", screenMediaType)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		})

		_, errOut, err := api.run(t, "device", "screen", "d1", "--tap", "10,20", "--seconds", "0")
		if got := ExitCode(err); got != 4 {
			t.Fatalf("a stream with no session header exited %d (%v), want 4", got, err)
		}
		if !strings.Contains(errOut, headerScreenSession) {
			t.Errorf("the report does not name the missing header:\n%s", errOut)
		}
	})
}

// TestScreenRefusalDoesNotEatAnExistingRecording. --out is opened before the
// request so a bad path fails before a handset is touched, and emptied only
// when a byte is about to land. Truncating at open would mean a 409 — the
// likeliest answer on this route, since one encoder means one session —
// destroys this morning's capture on the way to saying no.
//
// Falsify: go back to os.Create for the sink and this file comes back empty.
func TestScreenRefusalDoesNotEatAnExistingRecording(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "this-morning.h264")
	prior := []byte("a recording somebody already made")
	if err := os.WriteFile(dst, prior, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	api := screenFake(t, false)
	api.reply("GET /api/v1/devices/d1/screen", http.StatusConflict, map[string]any{
		"error": map[string]any{"code": "conflict", "message": "a screen session is already open",
			"detail": map[string]any{"reason": "session_already_open"}},
	})

	_, _, err := api.run(t, "device", "screen", "d1", "--out", dst, "--seconds", "0")
	if got := ExitCode(err); got != 3 {
		t.Fatalf("a refused screen exited %d (%v), want 3", got, err)
	}
	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("the prior recording is gone: %v", readErr)
	}
	if !bytes.Equal(got, prior) {
		t.Fatalf("a refusal rewrote a recording it never had bytes for: %q", got)
	}
}

// TestScreenRecordingLimitDoesNotEatARefusal. --seconds says how much video to
// record; the route has work to do before the first byte of it exists — the jar
// push, a process on the handset, two admissions. Letting the recording limit
// cancel that would report a 409 an operator has to read as "context canceled",
// and would fail a healthy device on a loaded farm. --timeout is the bound on
// the answer, --seconds on the video.
//
// Falsify: arm the --seconds timer before openScreen again, and this exits 1
// with no explanation of the conflict on stderr.
func TestScreenRecordingLimitDoesNotEatARefusal(t *testing.T) {
	api := screenFake(t, false)
	api.mux.HandleFunc("GET /api/v1/devices/d1/screen", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(1200 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "conflict", "message": "a screen session is already open",
				"detail": map[string]any{"reason": "session_already_open"}},
		})
	})

	_, errOut, err := api.run(t, "device", "screen", "d1", "--seconds", "1")
	if got := ExitCode(err); got != 3 {
		t.Fatalf("a refusal that took longer than --seconds exited %d (%v), want 3", got, err)
	}
	if !strings.Contains(errOut, "One encoder, one session") {
		t.Errorf("the refusal's guidance was lost to the recording limit:\n%s", errOut)
	}
}

// TestScreenFramingFaultIsNotBlamedOnTheSocket. A length above
// scrcpy.MaxPacket is refused before it sizes anything — that is the whole
// reason internal/scrcpy exists — and it is a statement about the encoder, not
// about the network, which delivered every byte it was handed. Calling it a
// transport failure sends an operator to look at cabling.
func TestScreenFramingFaultIsNotBlamedOnTheSocket(t *testing.T) {
	s := newScreenStream()
	s.session(480, 1024)
	s.packet(false, true, 1, []byte{0x65, 0x01})
	body := s.Bytes()
	// A header declaring sixty-four mebibytes, which is sixteen times the cap.
	var hdr [12]byte
	binary.BigEndian.PutUint64(hdr[0:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], 64<<20)
	body = append(body, hdr[:]...)

	api := screenFake(t, false)
	api.serveStream(body)

	_, _, err := api.run(t, "device", "screen", "d1", "--seconds", "0")
	if got := ExitCode(err); got != 1 {
		t.Fatalf("an impossible packet length exited %d (%v), want 1", got, err)
	}
	if strings.Contains(err.Error(), "transport failure") {
		t.Errorf("a framing fault was reported as a transport failure: %q", err.Error())
	}
	for _, want := range []string{"stopped making sense", "Nothing was allocated", "not the socket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message omits %q: %q", want, err.Error())
		}
	}
}

// TestScreenPlaybackHintOnStdoutNamesThePipe. `ffplay -f h264 stdout` sends
// somebody looking for a file that does not exist, which is a worse answer than
// no hint at all.
func TestScreenPlaybackHintOnStdoutNamesThePipe(t *testing.T) {
	s := newScreenStream()
	s.session(320, 640)
	s.packet(false, true, 1, []byte{0x65, 0x11})

	api := screenFake(t, false)
	api.serveStream(s.Bytes())

	_, errOut, err := api.run(t, "device", "screen", "d1", "--out", "-", "--seconds", "0")
	if err != nil {
		t.Fatalf("streaming to stdout failed: %v\nstderr: %s", err, errOut)
	}
	if strings.Contains(errOut, "ffplay -f h264 stdout") {
		t.Errorf("the hint names a file called stdout:\n%s", errOut)
	}
	if !strings.Contains(errOut, "--out - | ffplay") {
		t.Errorf("the hint does not say how to play a pipe:\n%s", errOut)
	}
}

// ---------------------------------------------------------------------------
// Against real H.264
// ---------------------------------------------------------------------------

// annexUnit is one access unit split out of an elementary stream.
type annexUnit struct {
	payload []byte
	config  bool
	key     bool
}

// splitAnnexB cuts an elementary stream into units LOSSLESSLY: concatenating
// every payload back together reproduces the input byte for byte, which is what
// makes the assertion in the test below meaningful. Parameter sets travel as a
// config packet and each VCL NAL as its own packet, which is the shape scrcpy
// puts on the wire.
func splitAnnexB(t *testing.T, raw []byte) []annexUnit {
	t.Helper()
	var starts []int
	for i := 0; i+2 < len(raw); i++ {
		if raw[i] == 0 && raw[i+1] == 0 && raw[i+2] == 1 {
			s := i
			if s > 0 && raw[s-1] == 0 {
				s--
			}
			if len(starts) > 0 && s <= starts[len(starts)-1] {
				continue
			}
			starts = append(starts, s)
		}
	}
	if len(starts) == 0 || starts[0] != 0 {
		t.Fatalf("this is not an Annex-B stream: %d start code(s), first at %v", len(starts), starts)
	}

	var (
		units   []annexUnit
		pending []byte
	)
	for i, s := range starts {
		end := len(raw)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		chunk := raw[s:end]
		// The NAL header is the byte after the start code, which is three or
		// four bytes long depending on whether it carries a leading zero.
		off := 3
		if chunk[0] == 0 && chunk[1] == 0 && chunk[2] == 0 {
			off = 4
		}
		if off >= len(chunk) {
			t.Fatalf("a start code at %d is followed by nothing", s)
		}
		nal := chunk[off] & 0x1f
		if nal >= 1 && nal <= 5 {
			if len(pending) > 0 {
				units = append(units, annexUnit{payload: pending, config: true})
				pending = nil
			}
			units = append(units, annexUnit{payload: chunk, key: nal == 5})
			continue
		}
		pending = append(pending, chunk...)
	}
	if len(pending) > 0 {
		units = append(units, annexUnit{payload: pending, config: true})
	}
	return units
}

// TestScreenRecordsRealH264ByteForByte is the end-to-end: genuine H.264 from
// ffmpeg, framed the way the route frames it, served over a real HTTP server,
// decoded by internal/scrcpy and written out by this command. The recording
// must be byte-identical to what ffmpeg produced — not merely the right length,
// not merely playable — because an elementary stream that lost or reordered one
// byte is a stream whose failure surfaces in somebody's player weeks later.
//
// The fixture is generated rather than committed: the committed one lives in
// test/fakeadb/testdata and belongs to another unit.
//
// Falsify: write unit.Packet.Payload[1:] to the sink, or drop the config
// packets, and the comparison fails with a length difference.
func TestScreenRecordsRealH264ByteForByte(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not on PATH, and this test generates its fixture rather than committing one")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.h264")
	gen := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=288x640:rate=12", "-t", "1",
		"-c:v", "libx264", "-profile:v", "baseline", "-pix_fmt", "yuv420p", "-crf", "34",
		"-g", "12", "-bf", "0",
		"-x264-params", "repeat-headers=1:keyint=12:min-keyint=12:scenecut=0",
		"-an", "-f", "h264", "-y", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not build a fixture (%v): %s", err, out)
	}
	raw, err := os.ReadFile(src)
	if err != nil || len(raw) == 0 {
		t.Fatalf("ffmpeg wrote nothing usable: %v, %d bytes", err, len(raw))
	}

	units := splitAnnexB(t, raw)
	var configs, keys int
	s := newScreenStream()
	s.session(288, 640)
	for i, u := range units {
		if u.config {
			configs++
		}
		if u.key {
			keys++
		}
		s.packet(u.config, u.key, uint64(i)*83333, u.payload)
	}
	if configs == 0 || keys == 0 || len(units) < 12 {
		t.Fatalf("the fixture is not a usable stream: %d unit(s), %d config, %d key frames",
			len(units), configs, keys)
	}

	api := screenFake(t, false)
	api.serveStream(s.Bytes())

	dst := filepath.Join(dir, "recorded.h264")
	_, errOut, err := api.run(t, "device", "screen", "d1", "--out", dst, "--seconds", "0")
	if err != nil {
		t.Fatalf("recording a real stream failed: %v\nstderr: %s", err, errOut)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("the recording is not readable: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("the recording is not what the device sent: %d bytes recorded, %d sent; first "+
			"difference at %d", len(got), len(raw), firstDiff(got, raw))
	}
	if !strings.Contains(errOut, "video: 288x640") {
		t.Errorf("the report does not name the video size:\n%s", errOut)
	}
	if !strings.Contains(errOut, "ffplay -f h264") {
		t.Errorf("the report does not say how to view a container-less file:\n%s", errOut)
	}
}

// firstDiff names where two byte slices part company, so a golden failure says
// where to look instead of only that it failed.
func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return len(a)
}
