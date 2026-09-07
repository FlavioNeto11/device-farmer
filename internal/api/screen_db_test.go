package api

// The whole screen path over real HTTP, against a real device row.
//
// These need DATABASE_URL pointing at a MIGRATED database and skip without one.
// They reuse scopeFixture from tenant_scope_db_test.go, which seeds a host, a
// hub, three devices, and a live lease on two of them — which is exactly the
// three cases this path branches on: a free device, a leased device, and a
// device that is not in a slot.
//
// A real httptest.Server rather than a ResponseRecorder, and that is not
// incidental. The response headers carry the session id, and a recorder does not
// hand them over until the handler returns — which for a stream is never. A test
// built on a recorder could therefore only assert on streams that had already
// ended, which is the one kind of stream this feature does not have.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/artifacts"
	"github.com/flaviopadilha/device-farmer/internal/config"
	"github.com/flaviopadilha/device-farmer/internal/screen"
)

// ---------------------------------------------------------------------------
// The wire, built from app/src/demuxer.c
// ---------------------------------------------------------------------------

// fakeVideoStream builds the shape a handset produces: a four-byte codec id,
// then twelve-byte headers. A session header has bit 63 set, with width at [4:8]
// and height at [8:12]; a packet header carries CONFIG at bit 62 and KEY_FRAME at
// bit 61, with the payload length at [8:12].
//
// Written out by hand rather than taken from test/fakeadb because that fixture
// currently disagrees with the protocol on all three of those bit positions — its
// CONFIG flag is scrcpy's session bit — so a test built on it would agree with
// the fixture and prove nothing about a phone.
func fakeVideoStream(w, h uint32, payloads ...[]byte) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, 0x68323634) // "h264"

	hdr := make([]byte, 12)
	hdr[0] = 0x80
	binary.BigEndian.PutUint32(hdr[4:8], w)
	binary.BigEndian.PutUint32(hdr[8:12], h)
	out = append(out, hdr...)

	for i, p := range payloads {
		ph := make([]byte, 12)
		meta := uint64(i) * 83_333
		if i == 0 {
			meta |= uint64(1) << 62 // CONFIG
		}
		if i <= 1 {
			meta |= uint64(1) << 61 // KEY_FRAME
		}
		binary.BigEndian.PutUint64(ph[0:8], meta)
		binary.BigEndian.PutUint32(ph[8:12], uint32(len(p)))
		out = append(out, ph...)
		out = append(out, p...)
	}
	return out
}

func fakeShellPacket(s string) []byte {
	b := make([]byte, 5+len(s))
	b[0] = 1 // stdout
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(s)))
	copy(b[5:], s)
	return b
}

// ---------------------------------------------------------------------------
// A device without an ADB server
// ---------------------------------------------------------------------------

// screenFakeDevice answers the three services a session opens.
//
// hold keeps the video stream open after its scripted bytes run out, so a test
// can ask a question of a session that is still live — which is every question
// worth asking about a stream.
type screenFakeDevice struct {
	mu      sync.Mutex
	video   []byte
	hold    bool
	opens   int
	control []byte

	gate   chan struct{} // closed to let a held stream end
	opened chan struct{} // closed once the video reader has been drained
	once   sync.Once
}

func newScreenFakeDevice(video []byte, hold bool) *screenFakeDevice {
	return &screenFakeDevice{
		video:  video,
		hold:   hold,
		gate:   make(chan struct{}),
		opened: make(chan struct{}),
	}
}

func (d *screenFakeDevice) release() { close(d.gate) }

func (d *screenFakeDevice) waitStreaming(t *testing.T) {
	t.Helper()
	select {
	case <-d.opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the video stream never started")
	}
}

func (d *screenFakeDevice) controlBytes() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.control...)
}

func (d *screenFakeDevice) OpenService(ctx context.Context, service string) (io.ReadWriteCloser, error) {
	d.mu.Lock()
	d.opens++
	n := d.opens
	d.mu.Unlock()

	switch {
	case strings.HasPrefix(service, "shell,v2,raw:"):
		return &screenFakeConn{r: strings.NewReader(string(fakeShellPacket("[server] INFO: ready\n")))}, nil

	case n == 2: // the first socket connect is video
		var r io.Reader = strings.NewReader(string(d.video))
		if d.hold {
			r = io.MultiReader(r, &blockUntil{gate: d.gate, opened: d.opened, once: &d.once})
		} else {
			r = io.MultiReader(r, &signalEOF{opened: d.opened, once: &d.once})
		}
		return &screenFakeConn{r: r}, nil

	default: // the second is control
		return &screenFakeConn{w: func(p []byte) {
			d.mu.Lock()
			d.control = append(d.control, p...)
			d.mu.Unlock()
		}}, nil
	}
}

func (d *screenFakeDevice) Push(ctx context.Context, r io.Reader, remote string, mode fs.FileMode) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

type screenFakeConn struct {
	r io.Reader
	w func([]byte)
}

func (c *screenFakeConn) Read(p []byte) (int, error) {
	if c.r == nil {
		// The control socket's read half. A real one carries device-to-client
		// messages this feature does not use; blocking rather than returning EOF
		// keeps a closed session distinguishable from an open one.
		select {}
	}
	return c.r.Read(p)
}

func (c *screenFakeConn) Write(p []byte) (int, error) {
	if c.w != nil {
		c.w(p)
	}
	return len(p), nil
}

func (c *screenFakeConn) Close() error { return nil }

// blockUntil signals that the scripted bytes are exhausted, then holds the
// stream open until the test releases it.
type blockUntil struct {
	gate   chan struct{}
	opened chan struct{}
	once   *sync.Once
}

func (b *blockUntil) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.opened) })
	<-b.gate
	return 0, io.EOF
}

// signalEOF is the same signal for a stream that is allowed to end.
type signalEOF struct {
	opened chan struct{}
	once   *sync.Once
}

func (s *signalEOF) Read([]byte) (int, error) {
	s.once.Do(func() { close(s.opened) })
	return 0, io.EOF
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type screenHarness struct {
	t   *testing.T
	url string
	srv *Server
	dev *screenFakeDevice
}

func newScreenHarness(t *testing.T, f *scopeFixture, dev *screenFakeDevice) *screenHarness {
	t.Helper()

	cfg := screenOnConfig()
	s, err := New(cfg, f.pool,
		WithAuthenticator(NewAllowAll(slog.New(slog.DiscardHandler), "tester")),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithScreenArtifacts(recordingStore{}),
		WithScreenDeviceFactory(func(endpoint, devpath string, fence int64) screen.Device {
			return dev
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		s.screens.Close()
		ts.Close()
	})
	return &screenHarness{t: t, url: ts.URL, srv: s, dev: dev}
}

// recordingStore stands in for the artifact store. EnsureOnDevice reporting
// success with no push is the second-and-later session's real behaviour — the
// ledger says the blob is already there — so this is not a weaker fixture than a
// real store would be for these tests.
type recordingStore struct{}

func (recordingStore) EnsureOnDevice(context.Context, string, string, artifacts.PushFunc) (artifacts.EnsureResult, error) {
	return artifacts.EnsureResult{Pushed: false, RemotePath: "/data/local/tmp/scrcpy-server.jar"}, nil
}

func (h *screenHarness) get(path string) *http.Response {
	h.t.Helper()
	resp, err := http.Get(h.url + path)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func (h *screenHarness) post(path string, body any) (*http.Response, string) {
	h.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	resp, err := http.Post(h.url+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

// ---------------------------------------------------------------------------
// The stream
// ---------------------------------------------------------------------------

// TestAScreenStreamsTheBytesThePhoneProduced is the end-to-end assertion, and the
// byte-for-byte comparison is the whole point of it.
//
// The body is the scrcpy stream verbatim. Nothing in this process re-encodes it,
// and nothing reads a length out of it past the sixteen fixed-width bytes
// internal/screen needs for the frame size. A single changed or missing byte is a
// decoder error that an operator sees as a blank panel, with nothing anywhere to
// explain it.
//
// Falsify: have spliceScreen skip sess.Preamble(), or re-serialise the session
// header from Frame() instead of passing the captured bytes through.
func TestAScreenStreamsTheBytesThePhoneProduced(t *testing.T) {
	f := newScopeFixture(t)
	want := fakeVideoStream(460, 1024, []byte("SPSPPS"), []byte("IDR-frame"), []byte("P-frame"))
	h := newScreenHarness(t, f, newScreenFakeDevice(want, false))

	resp := h.get("/api/v1/devices/" + f.devFree + "/screen")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d\n%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != ScreenContentType {
		t.Errorf("Content-Type = %q, want %q — a client that trusted video/h264 would hand a "+
			"decoder twelve-byte headers as though they were NAL units", got, ScreenContentType)
	}
	if resp.Header.Get(HeaderScreenSession) == "" {
		t.Error("no " + HeaderScreenSession + " header, so input has nothing to address")
	}
	if got := resp.Header.Get(HeaderScreenFrame); got != "460x1024" {
		t.Errorf("%s = %q, want 460x1024 — this is the coordinate space input is placed in",
			HeaderScreenFrame, got)
	}

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the stream: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("the body is %d bytes and the device wrote %d; what reaches a decoder is not "+
			"what the phone produced", len(got), len(want))
	}
}

// TestASecondScreenOnOneDeviceIsRefusedAndSaysNotToRetry.
//
// scrcpy is one encoder per session. The refusal has to be distinguishable from
// the session cap, because one of those should be retried later and the other
// should never be retried against this device.
func TestASecondScreenOnOneDeviceIsRefusedAndSaysNotToRetry(t *testing.T) {
	f := newScopeFixture(t)
	dev := newScreenFakeDevice(fakeVideoStream(460, 1024, []byte("SPS")), true)
	h := newScreenHarness(t, f, dev)

	first := h.get("/api/v1/devices/" + f.devFree + "/screen")
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("the first screen got %d", first.StatusCode)
	}
	h.dev.waitStreaming(t)

	second := h.get("/api/v1/devices/" + f.devFree + "/screen")
	defer second.Body.Close()
	body, _ := io.ReadAll(second.Body)

	if second.StatusCode != http.StatusConflict {
		t.Fatalf("the second screen got %d, want 409\n%s", second.StatusCode, body)
	}
	if !strings.Contains(string(body), "session_already_open") {
		t.Errorf("the refusal does not carry session_already_open, so a client cannot tell it "+
			"from the session cap — which it SHOULD retry:\n%s", body)
	}
	dev.release()
}

// TestAScreenOnALeasedDeviceNeedsForceAndAReason, and the refusal must say that
// opening one does not end the lease.
//
// The gate is copied from handleDeviceExec rather than reinvented: a screen on a
// device in the middle of somebody's six-hour run can wreck that run, and the
// holder gets no signal that it happened.
//
// Falsify: delete the `d.Lease != nil && !force` branch in handleDeviceScreen.
func TestAScreenOnALeasedDeviceNeedsForceAndAReason(t *testing.T) {
	f := newScopeFixture(t)
	dev := newScreenFakeDevice(fakeVideoStream(460, 1024, []byte("SPS")), false)
	h := newScreenHarness(t, f, dev)

	var before string
	f.scan(&before, `SELECT state::text FROM farm.leases WHERE id = $1`, f.leaseA)

	resp := h.get("/api/v1/devices/" + f.devA + "/screen")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a leased device streamed without force: %d\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{f.leaseA, "does not end the lease"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the refusal does not contain %q; an operator reading it must be able to "+
				"see whose run they are about to touch, and that they are not ending it:\n%s",
				want, body)
		}
	}

	// A reason without force is still refused, and force without a reason is
	// refused too: the reason is the only record of why, six weeks later.
	resp = h.get("/api/v1/devices/" + f.devA + "/screen?force=true")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("force with no reason got %d, want 400\n%s", resp.StatusCode, body)
	}

	// With both, it streams — and the lease is untouched.
	resp = h.get("/api/v1/devices/" + f.devA + "/screen?force=true&reason=" +
		"investigating+a+stuck+install")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ = io.ReadAll(resp.Body)
		t.Fatalf("force with a reason got %d\n%s", resp.StatusCode, body)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	// Compared against what it WAS, not against a state name written here. The
	// question is whether a screen changed anything, and hard-coding the live
	// state would make this fail for a schema rename rather than for the thing it
	// is about — and, worse, would pass for a rename that also broke the rule.
	var state, reason string
	f.scan(&state, `SELECT state::text FROM farm.leases WHERE id = $1`, f.leaseA)
	f.scan(&reason, `SELECT coalesce(release_reason::text, '') FROM farm.leases WHERE id = $1`, f.leaseA)
	if state != before || reason != "" {
		t.Errorf("the lease was %q with no release reason before a screen was opened on its "+
			"device, and is %q / %q after. A screen session ends BYTES: "+
			"farm.leases.release_reason has seven allowed values and none of them is about a "+
			"socket.", before, state, reason)
	}
}

// TestOpeningAndClosingAScreenLeavesTheLeaseAloneAndSaysSoInTheAudit.
//
// The audit row is the thing an auditor reads six weeks later, and "screen.close
// on a device under lease" is exactly the line that invites the wrong conclusion.
// So the row says what it did not do, in words.
//
// Falsify: delete the lease_effect key from the close detail.
func TestOpeningAndClosingAScreenLeavesTheLeaseAloneAndSaysSoInTheAudit(t *testing.T) {
	f := newScopeFixture(t)
	dev := newScreenFakeDevice(fakeVideoStream(460, 1024, []byte("SPS")), false)
	h := newScreenHarness(t, f, dev)

	resp := h.get("/api/v1/devices/" + f.devFree + "/screen")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// The close row is written from a detached context after the handler
	// returns, so it may land a moment later.
	deadline := time.Now().Add(5 * time.Second)
	var actions []string
	for time.Now().Before(deadline) {
		actions = f.auditActions("device:" + f.devFree)
		if len(actions) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(actions) < 2 {
		t.Fatalf("audit rows for the session: %v; want screen.open and screen.close", actions)
	}

	var detail string
	f.scan(&detail, `
SELECT detail::text FROM farm.audit_log
 WHERE subject = $1 AND action = 'screen.close'
 ORDER BY at DESC LIMIT 1`, "device:"+f.devFree)
	for _, want := range []string{"lease_effect", "never a lease", "duration_ms", "inputs"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the screen.close audit detail is missing %q:\n%s", want, detail)
		}
	}
}

// TestTheScreenEventNamesTheLeaseItRanAlongside.
//
// This is the hole device_exec still has: it records its event without a
// lease_id, so the tenant whose job was touched cannot see it in their own
// timeline. Repeating that here would have been the easy thing to do, because the
// event helper does not require the field.
//
// Falsify: drop the LeaseID assignment from screenEvent.
func TestTheScreenEventNamesTheLeaseItRanAlongside(t *testing.T) {
	f := newScopeFixture(t)
	dev := newScreenFakeDevice(fakeVideoStream(460, 1024, []byte("SPS")), false)
	h := newScreenHarness(t, f, dev)

	resp := h.get("/api/v1/devices/" + f.devA + "/screen?force=true&reason=checking")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		f.scan(&n, `
SELECT count(*) FROM farm.events
 WHERE device_id = $1::uuid AND kind = 'screen_open' AND lease_id = $2::uuid`,
			f.devA, f.leaseA)
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("no screen_open event names lease %s, so the tenant whose job was running on this "+
		"device cannot see in their own timeline that somebody watched its screen", f.leaseA)
}

// ---------------------------------------------------------------------------
// Input
// ---------------------------------------------------------------------------

// TestInputReachesTheDeviceAndIsCountedNotLogged.
//
// The count is the whole audit story for input, and it is a deliberate ceiling: a
// row per touch would be thousands a minute, so "what did this person do with the
// phone" is not answerable and the trail does not pretend otherwise.
func TestInputReachesTheDeviceAndIsCountedNotLogged(t *testing.T) {
	f := newScopeFixture(t)
	dev := newScreenFakeDevice(fakeVideoStream(460, 1024, []byte("SPS")), true)
	h := newScreenHarness(t, f, dev)

	resp := h.get("/api/v1/devices/" + f.devFree + "/screen")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	session := resp.Header.Get(HeaderScreenSession)
	h.dev.waitStreaming(t)

	post, body := h.post("/api/v1/devices/"+f.devFree+"/input", map[string]any{
		"session": session,
		"events": []map[string]any{
			{"type": "touch", "action": "down", "x": 100, "y": 200, "pressure": 1},
			{"type": "touch", "action": "up", "x": 100, "y": 200},
		},
	})
	if post.StatusCode != http.StatusOK {
		t.Fatalf("input got %d\n%s", post.StatusCode, body)
	}
	var out inputResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if out.Accepted != 2 || out.Total != 2 {
		t.Errorf("accepted %d total %d, want 2 and 2", out.Accepted, out.Total)
	}
	if len(h.dev.controlBytes()) == 0 {
		t.Error("nothing reached the control socket")
	}
	dev.release()
}

// TestACoordinateOutsideTheFrameIsABadRequestThatSaysWhichSpace.
//
// A 1080x2400 panel streamed at max_size 1024 is 460x1024, so a caller sending
// device pixels misses by more than half the screen. The only thing that catches
// it is the frame refusing what is outside itself — and the refusal has to name
// the coordinate space, or the caller reads it as an off-by-one and clamps.
//
// Falsify: build the Position from x/y directly in screen.Touch.appendTo instead
// of through Screen.At.
func TestACoordinateOutsideTheFrameIsABadRequestThatSaysWhichSpace(t *testing.T) {
	f := newScopeFixture(t)
	dev := newScreenFakeDevice(fakeVideoStream(460, 1024, []byte("SPS")), true)
	h := newScreenHarness(t, f, dev)

	resp := h.get("/api/v1/devices/" + f.devFree + "/screen")
	defer resp.Body.Close()
	session := resp.Header.Get(HeaderScreenSession)
	h.dev.waitStreaming(t)

	post, body := h.post("/api/v1/devices/"+f.devFree+"/input", map[string]any{
		"session": session,
		"events":  []map[string]any{{"type": "touch", "action": "down", "x": 900, "y": 1200}},
	})
	if post.StatusCode != http.StatusBadRequest {
		t.Fatalf("a device-pixel coordinate on a 460x1024 frame got %d, want 400\n%s",
			post.StatusCode, body)
	}
	if !strings.Contains(body, "video space") {
		t.Errorf("the refusal does not say which coordinate space is expected, so the caller "+
			"reads it as an off-by-one:\n%s", body)
	}

	// And nothing reached the device: a refused batch sends none of itself.
	if n := len(h.dev.controlBytes()); n != 0 {
		t.Errorf("%d bytes reached the control socket from a refused batch", n)
	}
	dev.release()
}

// TestInputCannotBeAimedAtADeviceTheSessionIsNotShowing. The path is where an id
// is easiest to edit, and a session that could be pointed at a different device
// would make the one-session-per-device rule meaningless.
//
// Falsify: delete the sameDevice check in handleDeviceInput.
func TestInputCannotBeAimedAtADeviceTheSessionIsNotShowing(t *testing.T) {
	f := newScopeFixture(t)
	dev := newScreenFakeDevice(fakeVideoStream(460, 1024, []byte("SPS")), true)
	h := newScreenHarness(t, f, dev)

	resp := h.get("/api/v1/devices/" + f.devFree + "/screen")
	defer resp.Body.Close()
	session := resp.Header.Get(HeaderScreenSession)
	h.dev.waitStreaming(t)

	post, body := h.post("/api/v1/devices/"+f.devB+"/input", map[string]any{
		"session": session,
		"events":  []map[string]any{{"type": "key", "action": "down", "keycode": 4}},
	})
	if post.StatusCode != http.StatusConflict {
		t.Fatalf("input for %s was accepted on a session showing %s: %d\n%s",
			f.devB, f.devFree, post.StatusCode, body)
	}
	dev.release()
}

// TestADeviceWithNoSlotIsRefusedBeforeAnythingIsDialled. There is no physical
// address to stream from, and finding that out costs one row rather than a
// transport and a push.
func TestADeviceWithNoSlotIsRefusedBeforeAnythingIsDialled(t *testing.T) {
	f := newScopeFixture(t)
	dev := newScreenFakeDevice(nil, false)
	h := newScreenHarness(t, f, dev)

	var orphan string
	f.scan(&orphan, `
INSERT INTO farm.devices (pool_id, farm_uid, adb_serial, model)
VALUES ($1, 'df-' || replace(gen_random_uuid()::text, '-', ''), 'ORPHAN-' || $2, 'test')
RETURNING id::text`, f.poolID, f.host)
	// Registered AFTER the fixture's own teardown, so it runs BEFORE it: cleanups
	// are LIFO, and the pool this device references is deleted by that teardown.
	t.Cleanup(func() {
		f.exec(`DELETE FROM farm.devices WHERE id = $1::uuid`, orphan)
	})

	resp := h.get("/api/v1/devices/" + orphan + "/screen")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409\n%s", resp.StatusCode, body)
	}
	if dev.opens != 0 {
		t.Errorf("%d services were opened against a device that is not in a slot", dev.opens)
	}
}

// auditActions lists the audit actions recorded against a subject.
func (f *scopeFixture) auditActions(subject string) []string {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx,
		`SELECT action FROM farm.audit_log WHERE subject = $1 ORDER BY at`, subject)
	if err != nil {
		f.t.Fatalf("audit query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			f.t.Fatalf("scan: %v", err)
		}
		out = append(out, a)
	}
	return out
}

var _ = config.DefaultScreenMaxSize
