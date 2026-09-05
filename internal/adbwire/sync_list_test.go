package adbwire

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// The two halves of the sync protocol this package could not speak before:
// LIST, which is the only way to learn what a directory holds, and mkdir,
// which the protocol does not have at all and which therefore goes through
// the shell — the one place a remote path reaches a shell rather than a
// length-prefixed field.

// ---------------------------------------------------------------------------
// LIST
// ---------------------------------------------------------------------------

// listFixture puts a small tree on a device: two files, a directory holding a
// file, an empty directory, and a sibling that must not leak into the listing.
func listFixture(srv *fakeadb.SyncServer, devpath, dir string) {
	srv.PutFile(devpath, dir+"/b.apk", 0o644, pattern(3000))
	srv.PutFile(devpath, dir+"/a.txt", 0o600, []byte("ten bytes!"))
	srv.PutFile(devpath, dir+"/sub/deep.bin", 0o644, []byte("x"))
	srv.Mkdir(devpath, dir+"/empty")
	srv.PutFile(devpath, dir+"-not/z", 0o644, []byte("z"))
}

func TestSyncListReportsEntriesWithoutTheDotEntries(t *testing.T) {
	t.Parallel()

	const devpath = "usb:9-1.1"
	const dir = "/data/local/tmp/listing"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCLIST", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)
	listFixture(srv, devpath, dir)

	entries, err := cli.List(ctx, devpath, dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	want := []string{"a.txt", "b.apk", "empty", "sub"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v, want %v: sorted, without \".\" and \"..\", and without the sibling directory", names, want)
	}

	by := map[string]FileInfo{}
	for _, e := range entries {
		by[e.Name] = e
		if e.Path != dir+"/"+e.Name {
			t.Errorf("%s: Path = %q, want the directory joined with the name", e.Name, e.Path)
		}
		if !e.Exists {
			t.Errorf("%s: a listed entry reports Exists false", e.Name)
		}
	}
	if f := by["a.txt"]; !f.IsRegular() || f.Size != 10 || f.Mode.Perm() != 0o600 {
		t.Errorf("a.txt = %+v, want a 10-byte regular file with mode 0600", f)
	}
	if f := by["b.apk"]; f.Size != 3000 || f.ModTime.IsZero() {
		t.Errorf("b.apk = %+v, want size 3000 and a modification time", f)
	}
	for _, name := range []string{"a.txt", "b.apk"} {
		if !by[name].SizeTruncated {
			t.Errorf("%s: SizeTruncated = false; DENT carries st_size as a uint32, so a regular file's size may have lost its high bits", name)
		}
	}
	for _, name := range []string{"empty", "sub"} {
		d := by[name]
		if !d.IsDir() {
			t.Errorf("%s = %+v, want a directory", name, d)
		}
		if d.SizeTruncated {
			t.Errorf("%s: SizeTruncated = true; a directory's st_size cannot reach 4 GiB", name)
		}
	}

	// The fake saw one LIST, for exactly this path, and answered it.
	var seen int
	for _, req := range srv.SyncRequests() {
		if req.ID == fakeadb.SyncList {
			seen++
			if req.Path != dir || req.Reply != "LIST" {
				t.Errorf("LIST request = %+v, want path %q answered LIST", req, dir)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("the fake saw %d LIST requests, want 1", seen)
	}
}

// TestSyncListLeavesTheSessionAlignedForTheNextRequest is the framing trap.
// The DONE that ends a listing is a full 20-byte dent, not the 8-byte status
// every other reply uses; a client that read only eight bytes of it would
// leave twelve on the socket for the next request to read as its answer.
func TestSyncListLeavesTheSessionAlignedForTheNextRequest(t *testing.T) {
	t.Parallel()

	const devpath = "usb:9-1.2"
	const dir = "/data/local/tmp/aligned"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCALIGN", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)
	listFixture(srv, devpath, dir)

	s, err := cli.Sync(ctx, devpath)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	defer s.Close()

	if _, err := s.List(ctx, dir); err != nil {
		t.Fatalf("first List: %v", err)
	}
	fi, err := s.Stat(ctx, dir+"/a.txt")
	if err != nil {
		t.Fatalf("Stat after a List on the same session: %v", err)
	}
	if !fi.Exists || fi.Size != 10 {
		t.Fatalf("Stat after a List = %+v, want the 10-byte file; the session read the listing's DONE tail as the stat reply", fi)
	}
	// An empty directory's listing is nothing but the dot entries and DONE,
	// which is the shortest listing there is and the easiest to misread.
	empty, err := s.List(ctx, dir+"/empty")
	if err != nil {
		t.Fatalf("List of an empty directory: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("an empty directory listed %v", empty)
	}
	if _, err := s.List(ctx, dir); err != nil {
		t.Fatalf("List after an empty listing: %v", err)
	}
}

// TestSyncListOfAMissingDirectoryIsNotExistAndNotTransport separates the
// answer "that directory is not there" from "the wire broke". A caller that
// retried the first as the second would sit in a retry loop for a step's whole
// budget waiting for a directory to appear; one that treated the second as
// the first would report a missing directory on a device that merely dropped.
func TestSyncListOfAMissingDirectoryIsNotExistAndNotTransport(t *testing.T) {
	t.Parallel()

	const devpath = "usb:9-1.3"
	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCNODIR", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	assertNotExist := func(t *testing.T, dir string, entries []FileInfo, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("List of %s = %v with no error", dir, entries)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("List of %s = %v (%T); want errors.Is(err, fs.ErrNotExist)", dir, err, err)
		}
		var ne *NotExistError
		if !errors.As(err, &ne) {
			t.Fatalf("List of %s = %v (%T), want a *NotExistError", dir, err, err)
		}
		if ne.Path != dir || ne.Devpath != devpath {
			t.Fatalf("NotExistError = %+v, want it to name %s on %s", ne, dir, devpath)
		}
		if IsTransport(err) || IsCanceled(err) || IsProtocol(err) {
			t.Fatalf("a missing directory was classified as %v", err)
		}
		if entries != nil {
			t.Fatalf("a missing directory still produced entries: %v", entries)
		}
	}

	t.Run("the v1 daemon lists nothing at all", func(t *testing.T) {
		const dir = "/data/local/tmp/nowhere"
		entries, err := cli.List(ctx, devpath, dir)
		assertNotExist(t, dir, entries, err)
		var ne *NotExistError
		errors.As(err, &ne)
		if !strings.Contains(ne.Reason, "listed nothing") {
			t.Fatalf("Reason = %q, want it to say how the daemon answered", ne.Reason)
		}
	})

	t.Run("a daemon that says so in a FAIL", func(t *testing.T) {
		const dir = "/data/local/tmp/refused-absent"
		srv.FailSyncNext(fakeadb.SyncList, "opendir failed: No such file or directory")
		entries, err := cli.List(ctx, devpath, dir)
		assertNotExist(t, dir, entries, err)
		var ne *NotExistError
		errors.As(err, &ne)
		if !strings.Contains(ne.Reason, "No such file or directory") {
			t.Fatalf("Reason = %q, want the daemon's own words", ne.Reason)
		}
	})

	t.Run("a FAIL that is not about existence stays a refusal", func(t *testing.T) {
		const dir = "/data/local/tmp/private"
		srv.FailSyncNext(fakeadb.SyncList, "opendir failed: Permission denied")
		_, err := cli.List(ctx, devpath, dir)
		if !IsProtocol(err) {
			t.Fatalf("List against Permission denied = %v (%T), want a protocol error", err, err)
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a directory the device would not open was reported as not existing: %v", err)
		}
		if IsTransport(err) {
			t.Fatalf("a refusal was classified as a transport failure: %v", err)
		}
	})
}

// TestSyncListCutShortIsATransportErrorNotAnAbsentDirectory is the sharper
// half of the previous test. A listing that ends without DONE has the same
// shape on the wire as one that was never answered, and "nothing came back"
// is also how a v1 daemon says a directory is not there. The two must never
// be confused: one is retried, the other is a fact about the device.
func TestSyncListCutShortIsATransportErrorNotAnAbsentDirectory(t *testing.T) {
	t.Parallel()

	const dir = "/data/local/tmp/cut"

	for _, tc := range []struct {
		name    string
		devpath string
		fault   fakeadb.SyncFault
	}{
		{"closed after one entry", "usb:9-2.1",
			fakeadb.SyncFault{Op: fakeadb.SyncList, Kind: fakeadb.SyncFaultTruncate, AfterChunks: 1, Times: 1}},
		{"closed before any entry", "usb:9-2.2",
			fakeadb.SyncFault{Op: fakeadb.SyncList, Kind: fakeadb.SyncFaultTruncate, Times: 1}},
		{"reset mid-listing", "usb:9-2.3",
			fakeadb.SyncFault{Op: fakeadb.SyncList, Kind: fakeadb.SyncFaultReset, AfterChunks: 1, Times: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			devpath := tc.devpath
			srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCCUT", Devpath: devpath})
			cli := syncClient(t, srv)
			ctx := testContext(t)
			listFixture(srv, devpath, dir)
			srv.InjectSync(tc.fault)

			entries, err := cli.List(ctx, devpath, dir)
			if err == nil {
				t.Fatalf("List = %v with no error across a cut listing", entries)
			}
			if !IsTransport(err) {
				t.Fatalf("List across a cut listing = %v (%T), want a *TransportError", err, err)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a cut listing was reported as a missing directory: %v", err)
			}
			if entries != nil {
				t.Fatalf("a cut listing still returned entries: %v", entries)
			}
		})
	}
}

func TestSyncListRefusesAPathTheProtocolCannotCarry(t *testing.T) {
	t.Parallel()

	const devpath = "usb:9-3.1"
	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCBADPATH", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	for _, dir := range []string{"", "/data/local/tmp/a\x00b", "/" + strings.Repeat("x", MaxSyncPath)} {
		_, err := cli.List(ctx, devpath, dir)
		var ue *UsageError
		if !errors.As(err, &ue) {
			t.Fatalf("List(%q) = %v (%T), want a *UsageError", dir, err, err)
		}
		if err := cli.MkdirAll(ctx, devpath, dir); !errors.As(err, &ue) {
			t.Fatalf("MkdirAll(%q) = %v (%T), want a *UsageError", dir, err, err)
		}
	}
	if reqs := srv.SyncRequests(); len(reqs) != 0 {
		t.Fatalf("a path the protocol cannot carry reached the wire: %+v", reqs)
	}
}

// ---------------------------------------------------------------------------
// mkdir
// ---------------------------------------------------------------------------

// TestMkdirAllMakesTheDirectoryAsOneShellWord is the injection guard. The
// path carries every character a shell would otherwise act on — a quote, a
// semicolon, a variable, a command substitution, spaces — and the fake's
// shell accepts only a single-quoted word, so the directory can exist
// afterwards only if the quoting held.
func TestMkdirAllMakesTheDirectoryAsOneShellWord(t *testing.T) {
	t.Parallel()

	const devpath = "usb:9-4.1"
	const dir = "/data/local/tmp/it's; rm -rf $HOME `uname` \"quoted\" dir"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCMKDIR", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	if err := cli.MkdirAll(ctx, devpath, dir); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if dirs := srv.Dirs(devpath); len(dirs) != 1 || dirs[0] != dir {
		t.Fatalf("the device holds %q, want exactly %q", dirs, dir)
	}

	var req *fakeadb.SyncRequest
	for _, r := range srv.SyncRequests() {
		if r.ID == fakeadb.SyncMkdir {
			r := r
			req = &r
		}
	}
	if req == nil {
		t.Fatal("the fake recorded no mkdir")
	}
	if req.Path != dir || req.Reply != "EXIT 0" {
		t.Fatalf("mkdir request = %+v, want the whole path as one word, exited 0", *req)
	}

	// The directory is real to the rest of the protocol: it lists as empty
	// and stats as a directory.
	entries, err := cli.List(ctx, devpath, dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("List of the new directory = %v, %v; want an empty listing", entries, err)
	}
	fi, err := cli.Stat(ctx, devpath, dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("Stat of the new directory = %+v, %v; want a directory", fi, err)
	}

	// Like os.MkdirAll, making it again is a success.
	if err := cli.MkdirAll(ctx, devpath, dir); err != nil {
		t.Fatalf("MkdirAll of an existing directory: %v", err)
	}

	// And the guard is real: the same path sent bare is refused by the fake's
	// shell, which is what makes the passing case above evidence rather than
	// a fake that says yes to everything.
	res, err := cli.Shell(ctx, devpath, "mkdir -p "+dir)
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if !res.Exited || res.ExitCode != 127 {
		t.Fatalf("a bare path exited %d (exited %t); the fake's shell must refuse anything but one quoted word", res.ExitCode, res.Exited)
	}
}

// TestMkdirAllKeepsARefusalApartFromADroppedStream is the classification the
// runner depends on: a mkdir that RAN and was refused is the device's answer
// and will be the same answer next time; a stream that ended before the
// device reported a status is the wire, and is retried.
func TestMkdirAllKeepsARefusalApartFromADroppedStream(t *testing.T) {
	t.Parallel()

	const devpath = "usb:9-4.2"
	const dir = "/system/priv-app/new"

	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCMKFAIL", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	t.Run("the device refused", func(t *testing.T) {
		srv.InjectSync(fakeadb.SyncFault{Op: fakeadb.SyncMkdir, Kind: fakeadb.SyncFaultFail,
			Message: "mkdir: '/system/priv-app/new': Read-only file system", Times: 1})

		err := cli.MkdirAll(ctx, devpath, dir)
		var ce *CommandError
		if !errors.As(err, &ce) {
			t.Fatalf("MkdirAll against a refusal = %v (%T), want a *CommandError", err, err)
		}
		if ce.ExitCode != 1 || !strings.Contains(ce.Stderr, "Read-only file system") {
			t.Fatalf("CommandError = %+v, want exit 1 and the shell's own words", ce)
		}
		if ce.Devpath != devpath || ce.Command != "mkdir -p '"+dir+"'" {
			t.Fatalf("CommandError = %+v, want the position and the command as it went out", ce)
		}
		if IsTransport(err) || IsProtocol(err) || IsCanceled(err) {
			t.Fatalf("a shell refusal was classified as %v", err)
		}
		if dirs := srv.Dirs(devpath); len(dirs) != 0 {
			t.Fatalf("a refused mkdir left a directory: %v", dirs)
		}
	})

	t.Run("the stream ended without a status", func(t *testing.T) {
		srv.InjectSync(fakeadb.SyncFault{Op: fakeadb.SyncMkdir, Kind: fakeadb.SyncFaultTruncate, Times: 1})

		err := cli.MkdirAll(ctx, devpath, dir)
		te, ok := AsTransport(err)
		if !ok {
			t.Fatalf("MkdirAll across a dropped stream = %v (%T), want a *TransportError", err, err)
		}
		if !te.PeerClosed() {
			t.Fatalf("Kind = %v, want peer_closed", te.Kind)
		}
		var ce *CommandError
		if errors.As(err, &ce) {
			t.Fatalf("a dropped stream was reported as the device's answer: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Stat's 32-bit size
// ---------------------------------------------------------------------------

// The v1 stat carries st_size as a uint32 and says nothing about what did not
// fit. The result cannot know whether THIS file lost bits, so it says so for
// every regular file and for nothing that cannot be that large.
func TestStatMarksThe32BitSizeAsTruncatedForRegularFilesOnly(t *testing.T) {
	t.Parallel()

	const devpath = "usb:9-5.1"
	srv := fakeadb.StartSync(t, fakeadb.Device{Serial: "SYNCSIZE", Devpath: devpath})
	cli := syncClient(t, srv)
	ctx := testContext(t)

	srv.PutFile(devpath, "/data/local/tmp/file", 0o644, []byte("small"))
	srv.Mkdir(devpath, "/data/local/tmp/dir")

	fi, err := cli.Stat(ctx, devpath, "/data/local/tmp/file")
	if err != nil || !fi.SizeTruncated {
		t.Fatalf("Stat of a regular file = %+v, %v; want SizeTruncated true", fi, err)
	}
	fi, err = cli.Stat(ctx, devpath, "/data/local/tmp/dir")
	if err != nil || !fi.IsDir() || fi.SizeTruncated {
		t.Fatalf("Stat of a directory = %+v, %v; want a directory with SizeTruncated false", fi, err)
	}
	fi, err = cli.Stat(ctx, devpath, "/data/local/tmp/absent")
	if err != nil || fi.Exists || fi.SizeTruncated {
		t.Fatalf("Stat of a missing path = %+v, %v; want Exists false and SizeTruncated false", fi, err)
	}
}
