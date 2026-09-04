package adbwire

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// syncClient returns a client pointed at a sync-capable fake.
func syncClient(tb testing.TB, srv *fakeadb.SyncServer) *Client {
	tb.Helper()
	return New(srv.Addr(), WithLogger(quietLogger()))
}

// pattern builds deterministic bytes that make a misordered or dropped chunk
// visible. A block of zeroes or of one repeated byte would let a transfer that
// reassembled chunks in the wrong order still compare equal.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/251)
	}
	return b
}

// TestSyncDataMaxMatchesTheFake guards the one constant both sides of every
// test in this file depend on. If they ever disagree, a chunk-count assertion
// stops meaning what it says and starts passing by luck.
func TestSyncDataMaxMatchesTheFake(t *testing.T) {
	t.Parallel()
	if SyncDataMax != fakeadb.SyncDataMax {
		t.Fatalf("SyncDataMax = %d, fakeadb.SyncDataMax = %d", SyncDataMax, fakeadb.SyncDataMax)
	}
	if SyncDataMax != 64*1024 {
		t.Fatalf("SyncDataMax = %d, want the protocol's 64 KiB", SyncDataMax)
	}
}

// ---------------------------------------------------------------------------
// Round trips
// ---------------------------------------------------------------------------

func TestSyncPushPullRoundTrip(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.1"
	const remote = "/data/local/tmp/run.sh"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCROUND", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	payload := []byte("#!/system/bin/sh\nexec /system/bin/logcat -d\n")
	if err := cli.Push(ctx, devpath, bytes.NewReader(payload), remote, 0o755); err != nil {
		t.Fatalf("Push: %v", err)
	}

	stored, ok := srv.File(devpath, remote)
	if !ok {
		t.Fatalf("the device has no %s; it has %v", remote, srv.Paths(devpath))
	}
	if !bytes.Equal(stored.Data, payload) {
		t.Fatalf("stored %q, want %q", stored.Data, payload)
	}
	if stored.Perm() != 0o755 {
		t.Fatalf("stored mode = %#o, want %#o", stored.Perm(), 0o755)
	}

	// The mode crosses the wire as a DECIMAL st_mode. A client that formatted
	// it as bare octal digits would look right in a hex dump and set the
	// wrong bits on the device: the daemon parses the field with a
	// base-detecting conversion, so "0755" is octal 493 and "755" is decimal
	// 755. This asserts the number the fake actually parsed.
	reqs := srv.SyncRequests()
	if len(reqs) == 0 || reqs[0].ID != fakeadb.SyncSend {
		t.Fatalf("first sync request = %+v, want a SEND", reqs)
	}
	if want := uint32(0o100755); reqs[0].Mode != want {
		t.Fatalf("SEND carried st_mode %#o, want %#o (S_IFREG|0755)", reqs[0].Mode, want)
	}
	if reqs[0].Path != remote {
		t.Fatalf("SEND carried path %q, want %q", reqs[0].Path, remote)
	}

	var back bytes.Buffer
	if err := cli.Pull(ctx, devpath, remote, &back); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !bytes.Equal(back.Bytes(), payload) {
		t.Fatalf("pulled %q, want %q", back.Bytes(), payload)
	}

	fi, err := cli.Stat(ctx, devpath, remote)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	switch {
	case !fi.Exists:
		t.Fatalf("Stat reports the file we just pushed as absent: %+v", fi)
	case fi.Path != remote:
		t.Fatalf("Stat path = %q, want %q", fi.Path, remote)
	case fi.Size != int64(len(payload)):
		t.Fatalf("Stat size = %d, want %d", fi.Size, len(payload))
	case fi.Mode.Perm() != 0o755:
		t.Fatalf("Stat mode = %v, want perm %#o", fi.Mode, 0o755)
	case !fi.IsRegular():
		t.Fatalf("Stat says %v is not a regular file", fi.Mode)
	case fi.IsDir():
		t.Fatalf("Stat says a pushed file is a directory: %v", fi.Mode)
	}
	// The mtime is stamped by DONE, so a zero or an epoch value would mean the
	// field never made the round trip.
	if fi.ModTime.IsZero() || time.Since(fi.ModTime) > time.Hour {
		t.Fatalf("Stat mtime = %v, want a time from this test run", fi.ModTime)
	}
}

// TestSyncLargeTransferSpansChunks proves the transfer is streamed in
// SYNC_DATA_MAX pieces in both directions rather than sent as one frame the
// protocol cannot express — and, by construction, that neither side ever holds
// more than one chunk.
func TestSyncLargeTransferSpansChunks(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.2"
	const remote = "/data/local/tmp/big.apk"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCBIG", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	// Three full chunks and a short tail: the tail is what catches an
	// implementation that only handles whole multiples of the chunk size.
	big := pattern(3*SyncDataMax + 7)

	if err := cli.Push(ctx, devpath, bytes.NewReader(big), remote, 0o644); err != nil {
		t.Fatalf("Push of %d bytes: %v", len(big), err)
	}

	st := srv.Stats()
	if st.ChunksIn != 4 {
		t.Fatalf("the device received %d DATA chunks for %d bytes, want 4", st.ChunksIn, len(big))
	}
	if st.BytesIn != int64(len(big)) {
		t.Fatalf("the device received %d bytes, want %d", st.BytesIn, len(big))
	}
	stored, ok := srv.File(devpath, remote)
	if !ok || !bytes.Equal(stored.Data, big) {
		t.Fatalf("the reassembled file does not match the source (present=%t, %d bytes)", ok, len(stored.Data))
	}

	var back bytes.Buffer
	if err := cli.Pull(ctx, devpath, remote, &back); err != nil {
		t.Fatalf("Pull of %d bytes: %v", len(big), err)
	}
	if !bytes.Equal(back.Bytes(), big) {
		t.Fatalf("pulled %d bytes that do not match the %d pushed", back.Len(), len(big))
	}
	if st = srv.Stats(); st.ChunksOut != 4 {
		t.Fatalf("the device sent %d DATA chunks, want 4", st.ChunksOut)
	}
}

// TestSyncSessionCarriesManyRequests proves one socket serves a whole sequence
// of transfers. Re-dialling per file would cost a transport switch for every
// step of a job that pushes a dozen of them.
func TestSyncSessionCarriesManyRequests(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.3"
	const remote = "/data/local/tmp/session.bin"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCSESS", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	s, err := cli.Sync(ctx, devpath)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	defer s.Close()

	if s.Devpath() != devpath {
		t.Fatalf("Devpath() = %q, want %q", s.Devpath(), devpath)
	}

	before, err := s.Stat(ctx, remote)
	if err != nil {
		t.Fatalf("Stat before the push: %v", err)
	}
	if before.Exists {
		t.Fatalf("a path that was never written reports Exists: %+v", before)
	}

	payload := pattern(4096)
	if err := s.Push(ctx, bytes.NewReader(payload), remote, 0o600); err != nil {
		t.Fatalf("Push: %v", err)
	}

	after, err := s.Stat(ctx, remote)
	if err != nil {
		t.Fatalf("Stat after the push: %v", err)
	}
	if !after.Exists || after.Size != int64(len(payload)) {
		t.Fatalf("Stat after the push = %+v, want an existing file of %d bytes", after, len(payload))
	}

	var back bytes.Buffer
	if err := s.Pull(ctx, remote, &back); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if !bytes.Equal(back.Bytes(), payload) {
		t.Fatalf("pulled %d bytes, want the %d pushed", back.Len(), len(payload))
	}

	if got := srv.Stats().Sessions; got != 1 {
		t.Fatalf("four requests opened %d sync sessions, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Existence
// ---------------------------------------------------------------------------

// TestSyncStatOnMissingPathIsNotAnError pins the one thing about LSTAT_V1 that
// a caller cannot guess: a missing path is answered with a zeroed reply, never
// with a FAIL. A client that waited for a refusal here would wait forever.
func TestSyncStatOnMissingPathIsNotAnError(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4"
	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCSTAT", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	fi, err := cli.Stat(ctx, devpath, "/data/local/tmp/never-written")
	if err != nil {
		t.Fatalf("Stat of a missing path: %v; want no error and Exists false", err)
	}
	switch {
	case fi.Exists:
		t.Fatalf("a missing path reports Exists: %+v", fi)
	case fi.Size != 0:
		t.Fatalf("a missing path has size %d", fi.Size)
	case fi.Mode != 0:
		t.Fatalf("a missing path has mode %v", fi.Mode)
	case !fi.ModTime.IsZero():
		t.Fatalf("a missing path has mtime %v", fi.ModTime)
	case fi.IsRegular():
		t.Fatalf("a missing path claims to be a regular file")
	}

	// And a path that IS there is distinguishable, which is the whole point of
	// using this as an existence check.
	const present = "/data/local/tmp/present"
	srv.PutFile(devpath, present, 0o640, []byte("0123456789"))
	fi, err = cli.Stat(ctx, devpath, present)
	if err != nil {
		t.Fatalf("Stat of a present path: %v", err)
	}
	if !fi.Exists || fi.Size != 10 || fi.Mode.Perm() != 0o640 {
		t.Fatalf("Stat = %+v, want a 10-byte file with mode 0640", fi)
	}
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

// TestSyncFailIsAProtocolError separates the two failures a caller must react
// to differently: the device refused in its own words (retrying the same
// request will fail the same way) versus the wire broke (retrying is exactly
// the right move).
func TestSyncFailIsAProtocolError(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.5"
	const remote = "/data/local/tmp/refused.bin"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCFAIL", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	const reason = "write failed: No space left on device"
	srv.FailSyncNext(fakeadb.SyncSend, reason)

	err := cli.Push(ctx, devpath, bytes.NewReader(pattern(1024)), remote, 0o644)
	if err == nil {
		t.Fatal("Push succeeded against a device that answered FAIL")
	}
	if !IsProtocol(err) {
		t.Fatalf("Push after FAIL = %v (%T), want a protocol error", err, err)
	}
	if IsTransport(err) {
		t.Fatalf("a FAIL was classified as a transport failure: %v", err)
	}
	if IsCanceled(err) {
		t.Fatalf("a FAIL was classified as cancellation: %v", err)
	}
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("Push error %T is not a *ProtocolError", err)
	}
	if !strings.Contains(pe.Reason, "No space left") {
		t.Fatalf("Reason = %q, want the device's own words", pe.Reason)
	}
	if pe.Devpath != devpath {
		t.Fatalf("Devpath = %q, want %q; a refusal must name the position it happened at", pe.Devpath, devpath)
	}
	if _, ok := srv.File(devpath, remote); ok {
		t.Fatalf("a refused transfer left a file behind: %v", srv.Paths(devpath))
	}

	// A pull of a path the device does not have is the same shape: the daemon
	// answers RECV with FAIL and the errno text.
	var sink bytes.Buffer
	err = cli.Pull(ctx, devpath, "/data/local/tmp/absent", &sink)
	if !IsProtocol(err) {
		t.Fatalf("Pull of a missing path = %v (%T), want a protocol error", err, err)
	}
	if sink.Len() != 0 {
		t.Fatalf("a refused pull wrote %d bytes to the destination", sink.Len())
	}
}

// TestSyncSessionRefusesRequestsAfterAFailure proves the session is not reused
// blindly. The daemon ends a sync session on the request it could not serve,
// so a second request written over the same socket would read whatever came
// next as if it were an answer.
func TestSyncSessionRefusesRequestsAfterAFailure(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.6"
	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCDESYNC", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	s, err := cli.Sync(ctx, devpath)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	defer s.Close()

	srv.FailSyncNext(fakeadb.SyncSend, "permission denied")
	if err := s.Push(ctx, bytes.NewReader([]byte("x")), "/data/local/tmp/x", 0o644); !IsProtocol(err) {
		t.Fatalf("Push = %v, want a protocol error", err)
	}

	err = s.Push(ctx, bytes.NewReader([]byte("y")), "/data/local/tmp/y", 0o644)
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("a second request on a finished session = %v (%T), want a *UsageError", err, err)
	}
	if _, serr := s.Stat(ctx, "/data/local/tmp/x"); !errors.As(serr, &ue) {
		t.Fatalf("Stat on a finished session = %v (%T), want a *UsageError", serr, serr)
	}
}

// ---------------------------------------------------------------------------
// Transport failures
//
// Every case here must surface as a *TransportError and nothing else. What a
// caller does with one is its own business; what this package must never do is
// dress a broken socket up as anything but a broken socket.
// ---------------------------------------------------------------------------

func TestSyncResetMidTransferIsATransportError(t *testing.T) {
	t.Parallel()

	const remote = "/data/local/tmp/severed.bin"
	big := pattern(3*SyncDataMax + 7)

	t.Run("push", func(t *testing.T) {
		t.Parallel()

		const devpath = "usb:4-1.1"
		srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCRSTPUSH", Devpath: devpath})
		cli := syncClient(t, srv)
		ctx := testContext(t)

		srv.ResetSyncAfter(fakeadb.SyncSend, 1)

		err := cli.Push(ctx, devpath, bytes.NewReader(big), remote, 0o644)
		if err == nil {
			t.Fatal("Push reported success across a severed connection")
		}
		// The Kind is deliberately not asserted. A RST can be observed on the
		// write that races it or on the read that follows, and both are
		// honest reports of the same event; what matters is that neither is
		// mistaken for a refusal or for cancellation.
		te, ok := AsTransport(err)
		if !ok {
			t.Fatalf("Push after a reset = %v (%T), want a *TransportError", err, err)
		}
		if te.Devpath != devpath {
			t.Fatalf("Devpath = %q, want %q", te.Devpath, devpath)
		}
		if IsProtocol(err) || IsCanceled(err) {
			t.Fatalf("a severed socket was classified as %v", err)
		}
		if _, present := srv.File(devpath, remote); present {
			t.Fatalf("a severed transfer left a file behind: %v", srv.Paths(devpath))
		}
	})

	t.Run("pull", func(t *testing.T) {
		t.Parallel()

		const devpath = "usb:4-1.2"
		srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCRSTPULL", Devpath: devpath})
		cli := syncClient(t, srv)
		ctx := testContext(t)

		srv.PutFile(devpath, remote, 0o644, big)
		srv.ResetSyncAfter(fakeadb.SyncRecv, 1)

		var sink bytes.Buffer
		err := cli.Pull(ctx, devpath, remote, &sink)
		if err == nil {
			t.Fatal("Pull reported success across a severed connection")
		}
		if !IsTransport(err) {
			t.Fatalf("Pull after a reset = %v (%T), want a *TransportError", err, err)
		}
		if IsProtocol(err) || IsCanceled(err) {
			t.Fatalf("a severed socket was classified as %v", err)
		}
		if sink.Len() >= len(big) {
			t.Fatalf("a severed pull delivered %d of %d bytes and still failed", sink.Len(), len(big))
		}
	})
}

// TestSyncTruncatedTransferIsATransportError covers the quieter failure: the
// peer closes cleanly in a place the protocol does not allow one. There is no
// error on the socket at all — just an EOF where a DONE belonged — and a
// client that treated it as the end of the file would write a truncated
// artifact and call it a success.
func TestSyncTruncatedTransferIsATransportError(t *testing.T) {
	t.Parallel()

	const devpath = "usb:4-1.3"
	const remote = "/data/local/tmp/truncated.bin"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCTRUNC", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	big := pattern(3*SyncDataMax + 7)
	srv.PutFile(devpath, remote, 0o644, big)
	srv.TruncateSyncAfter(fakeadb.SyncRecv, 2)

	var sink bytes.Buffer
	err := cli.Pull(ctx, devpath, remote, &sink)
	if err == nil {
		t.Fatalf("Pull reported success after %d of %d bytes", sink.Len(), len(big))
	}
	te, ok := AsTransport(err)
	if !ok {
		t.Fatalf("Pull after a truncated transfer = %v (%T), want a *TransportError", err, err)
	}
	// A clean close carries the bytes already written before its FIN, so this
	// is exact rather than approximate: the classification must be
	// peer_closed, not a timeout and not a frame error.
	if !te.PeerClosed() {
		t.Fatalf("Kind = %v, want peer_closed", te.Kind)
	}
	if want := 2 * SyncDataMax; sink.Len() != want {
		t.Fatalf("the destination holds %d bytes, want the %d that arrived before the close", sink.Len(), want)
	}
}

// TestSyncRefusesOversizedChunk is the hostile-peer case. The length in a DATA
// header is chosen by the far end; a client that believes it hands that end
// control of how much memory this process reserves.
//
// The fake sends the header and then holds the socket open without sending a
// byte, so a client that sized a buffer from the length would block until the
// test's context expires. Returning promptly with a frame error IS the
// assertion; the checks below only confirm what it returned.
func TestSyncRefusesOversizedChunk(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		length uint32
	}{
		{"one byte over SYNC_DATA_MAX", SyncDataMax + 1},
		{"the whole address space", 0xFFFFFFFF},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			devpath := "usb:5-1." + string(rune('1'+i))
			const remote = "/data/local/tmp/hostile.bin"

			srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCHOSTILE", Devpath: devpath})
			cli := syncClient(t, srv)
			ctx := testContext(t)

			srv.PutFile(devpath, remote, 0o644, []byte("the real file is tiny"))
			srv.OversizeSyncChunk(tc.length)

			var sink bytes.Buffer
			err := cli.Pull(ctx, devpath, remote, &sink)
			if err == nil {
				t.Fatal("Pull accepted a chunk larger than the protocol permits")
			}
			te, ok := AsTransport(err)
			if !ok {
				t.Fatalf("Pull = %v (%T), want a *TransportError", err, err)
			}
			if te.Kind != KindFrame {
				t.Fatalf("Kind = %v, want frame: a peer that overruns the cap is not speaking the protocol", te.Kind)
			}
			if !strings.Contains(err.Error(), "SYNC_DATA_MAX") {
				t.Fatalf("the error does not say what was violated: %v", err)
			}
			if sink.Len() != 0 {
				t.Fatalf("the destination received %d bytes from a refused chunk", sink.Len())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Addressing and caller mistakes
// ---------------------------------------------------------------------------

// TestSyncAddressesAPositionNotASerial runs against two devices that share one
// OEM serial. A transfer must land on the position it named and nowhere else:
// pushing an APK onto the wrong clone means running somebody else's build on a
// device that is hours into its own work.
func TestSyncAddressesAPositionNotASerial(t *testing.T) {
	t.Parallel()

	const remote = "/data/local/tmp/clone.apk"

	srv := fakeadb.StartSync(t,
		fakeadb.Device{Serial: fakeadb.CloneSerial, Devpath: fakeadb.CloneDevpathA, Model: "Pixel 6a"},
		fakeadb.Device{Serial: fakeadb.CloneSerial, Devpath: fakeadb.CloneDevpathB, Model: "Pixel 6a"},
	)
	cli := syncClient(t, srv)
	ctx := testContext(t)

	payload := pattern(2048)
	if err := cli.Push(ctx, fakeadb.CloneDevpathB, bytes.NewReader(payload), remote, 0o644); err != nil {
		t.Fatalf("Push to %s: %v", fakeadb.CloneDevpathB, err)
	}

	if got, ok := srv.File(fakeadb.CloneDevpathB, remote); !ok || !bytes.Equal(got.Data, payload) {
		t.Fatalf("the addressed clone %s did not receive the file", fakeadb.CloneDevpathB)
	}
	if paths := srv.Paths(fakeadb.CloneDevpathA); len(paths) != 0 {
		t.Fatalf("the other clone %s received %v", fakeadb.CloneDevpathA, paths)
	}
	for _, r := range srv.SyncRequests() {
		if r.Devpath != fakeadb.CloneDevpathB {
			t.Fatalf("a sync request reached %s, but only %s was addressed", r.Devpath, fakeadb.CloneDevpathB)
		}
	}

	// The two filesystems are genuinely separate, so an existence check
	// against the wrong position answers honestly rather than sharing state.
	fi, err := cli.Stat(ctx, fakeadb.CloneDevpathA, remote)
	if err != nil {
		t.Fatalf("Stat on the other clone: %v", err)
	}
	if fi.Exists {
		t.Fatalf("the file appeared on %s as well", fakeadb.CloneDevpathA)
	}
}

func TestSyncOnAnAbsentDeviceIsNotFound(t *testing.T) {
	t.Parallel()

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCPRESENT", Devpath: "usb:6-1.1"})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	err := cli.Push(ctx, "usb:6-1.9", bytes.NewReader([]byte("x")), "/data/local/tmp/x", 0o644)
	if !IsNotFound(err) {
		t.Fatalf("Push to an absent position = %v (%T), want ErrNotFound", err, err)
	}

	// A device that is present but not up is a refusal, not an absence: the
	// server has a transport and will not switch to it.
	srv.SetState("usb:6-1.1", fakeadb.StateOffline)
	var sink bytes.Buffer
	err = cli.Pull(ctx, "usb:6-1.1", "/data/local/tmp/x", &sink)
	if err == nil || IsNotFound(err) {
		t.Fatalf("Pull from an offline device = %v, want a refusal", err)
	}
	if !IsProtocol(err) {
		t.Fatalf("Pull from an offline device = %v (%T), want a protocol error", err, err)
	}
}

// TestSyncRejectsMalformedInputBeforeDialling checks the mistakes that must
// never reach the wire. A NUL in particular is not cosmetic: the payload is
// length-prefixed so the wire would carry it, and the daemon would then act on
// the truncated prefix — writing a file to a path nobody asked for.
func TestSyncRejectsMalformedInputBeforeDialling(t *testing.T) {
	t.Parallel()

	const devpath = "usb:6-2.1"
	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCVALID", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	paths := map[string]string{
		"empty":     "",
		"nul":       "/data/local/tmp/a\x00b",
		"too long":  "/" + strings.Repeat("a", MaxSyncPath),
		"nul first": "\x00",
	}
	for name, remote := range paths {
		t.Run(name, func(t *testing.T) {
			var ue *UsageError
			if err := cli.Push(ctx, devpath, bytes.NewReader(nil), remote, 0o644); !errors.As(err, &ue) {
				t.Fatalf("Push(%q) = %v (%T), want a *UsageError", remote, err, err)
			}
			if err := cli.Pull(ctx, devpath, remote, io.Discard); !errors.As(err, &ue) {
				t.Fatalf("Pull(%q) = %v (%T), want a *UsageError", remote, err, err)
			}
			if _, err := cli.Stat(ctx, devpath, remote); !errors.As(err, &ue) {
				t.Fatalf("Stat(%q) = %v (%T), want a *UsageError", remote, err, err)
			}
		})
	}

	// A devpath that is not a position is refused by the same check every
	// other targeted call in this package uses.
	err := cli.Push(ctx, "3-1.1", bytes.NewReader(nil), "/data/local/tmp/x", 0o644)
	if !errors.Is(err, ErrInvalidDevpath) {
		t.Fatalf("Push with a serial-shaped target = %v, want ErrInvalidDevpath", err)
	}

	if got := srv.Stats().Sessions; got != 0 {
		t.Fatalf("%d sync sessions were opened for requests that never should have been dialled", got)
	}
}

// ---------------------------------------------------------------------------
// Mode translation
// ---------------------------------------------------------------------------

// TestSyncModeTranslationRoundTrips checks the two conversions independently
// of the wire. fs.FileMode and st_mode are different encodings that happen to
// agree on the low nine bits, and treating them as interchangeable turns a
// directory into a file with peculiar permissions.
func TestSyncModeTranslationRoundTrips(t *testing.T) {
	t.Parallel()

	cases := []struct {
		mode  fs.FileMode
		posix uint32
	}{
		{0o644, 0o100644},
		{0o755, 0o100755},
		{0o600, 0o100600},
		{fs.ModeDir | 0o755, 0o040755},
		{fs.ModeSymlink | 0o777, 0o120777},
		{fs.ModeSetuid | 0o755, 0o104755},
		{fs.ModeSticky | 0o777, 0o101777},
	}
	for _, tc := range cases {
		if got := posixMode(tc.mode); got != tc.posix {
			t.Errorf("posixMode(%v) = %#o, want %#o", tc.mode, got, tc.posix)
		}
		if got := fileMode(tc.posix); got != tc.mode {
			t.Errorf("fileMode(%#o) = %v, want %v", tc.posix, got, tc.mode)
		}
	}
}

// ---------------------------------------------------------------------------
// Deadlines that belong to the caller
//
// A session lives as long as a job; a transfer lives as long as the step that
// asked for it. Those are different clocks, and the second one is the clock a
// user wrote down. A context handed to a single transfer that did not actually
// bound that transfer would be the quietest kind of broken: every test above
// still passes, because every failure they script ends the socket, and only a
// device that goes silent without closing anything tells the two apart.
// ---------------------------------------------------------------------------

// TestSyncTransferIsBoundedByItsOwnContext puts a long-lived session under a
// short-lived transfer and stalls the device. The transfer must end on its own
// deadline, the session must survive it, and the result must read as the
// caller getting what it asked for rather than as a host failure — otherwise a
// step timeout is counted and logged as a broken device.
func TestSyncTransferIsBoundedByItsOwnContext(t *testing.T) {
	t.Parallel()

	const remote = "/data/local/tmp/stalled.bin"

	// The session context is the long one: it stands in for a job that may run
	// for hours. Nothing in these subtests may depend on it ending.
	run := func(t *testing.T, devpath, op string, call func(*SyncConn, context.Context) error) {
		t.Helper()

		srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCSTALL", Devpath: devpath})
		cli := syncClient(t, srv)
		session := testContext(t)

		srv.PutFile(devpath, remote, 0o644, pattern(4096))

		s, err := cli.Sync(session, devpath)
		if err != nil {
			t.Fatalf("Sync: %v", err)
		}
		defer s.Close()

		srv.StallSyncNext(op)

		// Short enough that a transfer which ignored it would instead be
		// stopped by testTimeout, thirty seconds later — which is a test
		// failure and not a pass.
		step, cancel := context.WithTimeout(session, 300*time.Millisecond)
		defer cancel()

		started := time.Now()
		err = call(s, step)
		elapsed := time.Since(started)

		if err == nil {
			t.Fatalf("%s returned success from a device that never answered", op)
		}
		if elapsed > 10*time.Second {
			t.Fatalf("%s took %v to notice a 300ms deadline; its context is decorative", op, elapsed)
		}
		if !IsCanceled(err) {
			t.Fatalf("%s = %v (%T), want a cancellation: the caller's own deadline ended it", op, err, err)
		}
		if IsTransport(err) {
			t.Fatalf("%s blamed the wire for the caller's deadline: %v", op, err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s = %v, want it to unwrap to context.DeadlineExceeded", op, err)
		}
		if session.Err() != nil {
			t.Fatalf("one transfer's deadline ended the whole session: %v", session.Err())
		}

		// The stream is desynchronised — the device may still answer the
		// abandoned request — so the session must refuse to carry another one
		// rather than read that answer as if it belonged to the next caller.
		var ue *UsageError
		if _, serr := s.Stat(session, remote); !errors.As(serr, &ue) {
			t.Fatalf("a request after an abandoned transfer = %v (%T), want a *UsageError", serr, serr)
		}
	}

	t.Run("pull", func(t *testing.T) {
		t.Parallel()
		run(t, "usb:7-1.1", fakeadb.SyncRecv, func(s *SyncConn, ctx context.Context) error {
			return s.Pull(ctx, remote, io.Discard)
		})
	})

	t.Run("push", func(t *testing.T) {
		t.Parallel()
		// The stall lands on the acknowledgement: every byte reached the
		// device and the OKAY never comes, which is the case a client is most
		// tempted to call success.
		run(t, "usb:7-1.2", fakeadb.SyncSend, func(s *SyncConn, ctx context.Context) error {
			return s.Push(ctx, bytes.NewReader(pattern(1024)), remote, 0o644)
		})
	})

	t.Run("stat", func(t *testing.T) {
		t.Parallel()
		run(t, "usb:7-1.3", fakeadb.SyncStat, func(s *SyncConn, ctx context.Context) error {
			_, err := s.Stat(ctx, remote)
			return err
		})
	})
}

// TestSyncCloseInterruptsABlockedTransfer covers the other way out of a silent
// device: Close, from whichever goroutine is supervising. It is documented as
// safe to call at any time, which is only true if it neither races the flags a
// transfer is writing nor injects its parting QUIT into the middle of one.
func TestSyncCloseInterruptsABlockedTransfer(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-2.1"
	const remote = "/data/local/tmp/interrupted.bin"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCCLOSE", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	srv.PutFile(devpath, remote, 0o644, pattern(4096))

	s, err := cli.Sync(ctx, devpath)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	srv.StallSyncNext(fakeadb.SyncRecv)

	done := make(chan error, 1)
	go func() { done <- s.Pull(ctx, remote, io.Discard) }()

	// Wait until the request is demonstrably in flight, so this closes across
	// a blocked read rather than before one.
	waitForSyncRequest(t, srv, fakeadb.SyncRecv)
	if cerr := s.Close(); cerr != nil {
		t.Logf("Close across a blocked transfer reported %v", cerr)
	}

	select {
	case perr := <-done:
		if perr == nil {
			t.Fatal("Pull reported success after its session was closed underneath it")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not interrupt a blocked Pull")
	}

	// No QUIT may have been written: it would have landed inside the RECV the
	// device was still serving.
	for _, r := range srv.SyncRequests() {
		if r.ID == fakeadb.SyncQuit {
			t.Fatalf("Close wrote QUIT into the middle of a transfer: %+v", srv.SyncRequests())
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close is not idempotent: %v", err)
	}
}

// TestSyncSessionRefusesConcurrentRequests pins what a session does when two
// callers reach it at once. The protocol has no request ids, so the second
// request's reply is indistinguishable from the first's; interleaving them
// does not fail loudly, it silently swaps two files' contents. A refusal is
// the only answer that cannot corrupt anything.
func TestSyncSessionRefusesConcurrentRequests(t *testing.T) {
	t.Parallel()

	const devpath = "usb:7-3.1"
	const remote = "/data/local/tmp/contended.bin"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCBUSY", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	srv.PutFile(devpath, remote, 0o644, pattern(4096))

	s, err := cli.Sync(ctx, devpath)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	defer s.Close()

	srv.StallSyncNext(fakeadb.SyncRecv)

	held, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Pull(held, remote, io.Discard)
	}()
	waitForSyncRequest(t, srv, fakeadb.SyncRecv)

	var ue *UsageError
	if _, serr := s.Stat(ctx, remote); !errors.As(serr, &ue) {
		t.Fatalf("Stat during an in-flight Pull = %v (%T), want a *UsageError", serr, serr)
	}
	if perr := s.Push(ctx, bytes.NewReader([]byte("x")), remote, 0o644); !errors.As(perr, &ue) {
		t.Fatalf("Push during an in-flight Pull = %v (%T), want a *UsageError", perr, perr)
	}
	// Nothing may have reached the device: a refused request that still wrote
	// its header would have desynchronised the very stream it was protecting.
	for _, r := range srv.SyncRequests() {
		if r.ID != fakeadb.SyncRecv {
			t.Fatalf("a refused request reached the device: %+v", r)
		}
	}

	cancel()
	<-done
}

// waitForSyncRequest blocks until the device has seen a request of the given
// id, so a test that means "while a transfer is in flight" is not quietly
// testing "just before one starts".
func waitForSyncRequest(tb testing.TB, srv *fakeadb.SyncServer, id string) {
	tb.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range srv.SyncRequests() {
			if r.ID == id {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	tb.Fatalf("no %s request reached the device", id)
}

// ---------------------------------------------------------------------------
// Failures that are not the wire's fault
//
// The source of a push and the destination of a pull belong to the caller. A
// failure in either is neither a transport failure nor a refusal, and calling
// it one would send a caller off to retry a socket that was working perfectly.
// What matters far more is that neither is ever reported as success.
// ---------------------------------------------------------------------------

// errWriter fails after letting through a fixed number of bytes.
type errWriter struct {
	limit int
	seen  int
	err   error
}

func (w *errWriter) Write(p []byte) (int, error) {
	if w.seen+len(p) <= w.limit {
		w.seen += len(p)
		return len(p), nil
	}
	n := w.limit - w.seen
	w.seen = w.limit
	return n, w.err
}

// shortWriter is the writer that breaks its own contract: it reports fewer
// bytes than it was given and no error at all. io.Writer forbids this, which
// is exactly why a client must not assume it away — the cost of believing it
// is a truncated artifact carrying a valid digest of the wrong bytes.
type shortWriter struct{ seen int }

func (w *shortWriter) Write(p []byte) (int, error) {
	n := len(p) / 2
	w.seen += n
	return n, nil
}

func TestSyncDestinationFailureIsNotTheWire(t *testing.T) {
	t.Parallel()

	const remote = "/data/local/tmp/sink.bin"
	big := pattern(3*SyncDataMax + 7)

	t.Run("writer returns an error", func(t *testing.T) {
		t.Parallel()

		const devpath = "usb:8-1.1"
		srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCSINK", Devpath: devpath})
		cli := syncClient(t, srv)
		ctx := testContext(t)

		srv.PutFile(devpath, remote, 0o644, big)

		boom := errors.New("artifact store went away")
		err := cli.Pull(ctx, devpath, remote, &errWriter{limit: SyncDataMax, err: boom})
		if err == nil {
			t.Fatal("Pull reported success though the destination refused the bytes")
		}
		if !errors.Is(err, boom) {
			t.Fatalf("Pull = %v, want it to carry the destination's own error", err)
		}
		if IsTransport(err) || IsProtocol(err) || IsCanceled(err) {
			t.Fatalf("a failing destination was blamed on the device: %v (%T)", err, err)
		}
	})

	t.Run("writer silently truncates", func(t *testing.T) {
		t.Parallel()

		const devpath = "usb:8-1.2"
		srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCSHORT", Devpath: devpath})
		cli := syncClient(t, srv)
		ctx := testContext(t)

		srv.PutFile(devpath, remote, 0o644, big)

		w := &shortWriter{}
		err := cli.Pull(ctx, devpath, remote, w)
		if err == nil {
			t.Fatalf("Pull reported success after writing %d of %d bytes", w.seen, len(big))
		}
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Pull = %v, want io.ErrShortWrite", err)
		}
	})
}

// errReader yields limit bytes of pattern and then fails.
type errReader struct {
	limit int
	seen  int
	err   error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.seen >= r.limit {
		return 0, r.err
	}
	n := min(len(p), r.limit-r.seen)
	for i := range n {
		p[i] = byte((r.seen + i) * 31)
	}
	r.seen += n
	return n, nil
}

func TestSyncSourceFailureIsNotTheWire(t *testing.T) {
	t.Parallel()

	const devpath = "usb:8-2.1"
	const remote = "/data/local/tmp/source.bin"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCSRC", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	boom := errors.New("blob store read failed")
	err := cli.Push(ctx, devpath, &errReader{limit: SyncDataMax + 512, err: boom}, remote, 0o644)
	if err == nil {
		t.Fatal("Push reported success though the source never finished")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("Push = %v, want it to carry the source's own error", err)
	}
	if IsTransport(err) || IsProtocol(err) || IsCanceled(err) {
		t.Fatalf("a failing source was blamed on the device: %v (%T)", err, err)
	}
	// The daemon was never told how the file ends, so no file may exist. A
	// half-written artifact that looked complete is worse than none.
	if _, ok := srv.File(devpath, remote); ok {
		t.Fatalf("an abandoned push left a file behind: %v", srv.Paths(devpath))
	}
}

// TestSyncTruncatedStatIsNotAnAbsentFile is the sharpest silent failure in the
// file. LSTAT_V1 answers a missing path with a zeroed reply, so a reply that
// is cut off after the mode is byte for byte the beginning of "no such path".
// A client that returned what it had would tell a job the file is not there,
// and the job would go on to recreate something that already exists — on a
// device whose connection had just died.
func TestSyncTruncatedStatIsNotAnAbsentFile(t *testing.T) {
	t.Parallel()

	const devpath = "usb:8-3.1"
	const remote = "/data/local/tmp/halfstat"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCHALF", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	srv.PutFile(devpath, remote, 0o644, []byte("this file is really there"))
	srv.InjectSync(fakeadb.SyncFault{Op: fakeadb.SyncStat, Kind: fakeadb.SyncFaultTruncate, Times: 1})

	fi, err := cli.Stat(ctx, devpath, remote)
	if err == nil {
		t.Fatalf("Stat = %+v with no error; a reply cut short must not be read as an answer", fi)
	}
	te, ok := AsTransport(err)
	if !ok {
		t.Fatalf("Stat after a truncated reply = %v (%T), want a *TransportError", err, err)
	}
	if !te.PeerClosed() {
		t.Fatalf("Kind = %v, want peer_closed", te.Kind)
	}
	if fi.Exists {
		t.Fatalf("a failed Stat still claims the file exists: %+v", fi)
	}
}
