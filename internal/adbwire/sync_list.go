package adbwire

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// LIST
//
// LIST is the directory half of the sync protocol. The request is the id and a
// path, exactly like RECV; the reply is a run of DENT frames, one per entry,
// and then DONE. A DENT is not the 8-byte id-and-length header every other
// reply uses: it is the id followed by FOUR uint32s — mode, size, mtime and
// the length of the name — and then the name itself. DONE, when it ends a
// listing, is the same 20-byte shape with every field zero, because the
// daemon writes the whole dent struct for it rather than the 8-byte status.
// Reading only eight bytes of that DONE would leave twelve on the socket for
// the next request to trip over.
//
// The daemon lists what readdir gives it, and readdir gives it "." and ".."
// before anything else. They are dropped from the result, because no caller
// wants them — but they are counted first, because they are the only evidence
// this version of the protocol offers that the directory exists at all. A path
// the daemon cannot open is answered with DONE and nothing before it, not with
// a FAIL: the v1 LIST, like the v1 stat, has no way to say "no such directory"
// except by saying nothing. A directory that exists always says at least ".".
// ---------------------------------------------------------------------------

var (
	idLIST = syncID{'L', 'I', 'S', 'T'}
	idDENT = syncID{'D', 'E', 'N', 'T'}
)

// dentTailLen is what follows the 8-byte header in a DENT or a listing's
// DONE: size, mtime and name length.
const dentTailLen = 12

// NotExistError means a LIST named a directory the device does not have, or
// cannot open. It matches fs.ErrNotExist under errors.Is, so a caller can
// treat "the directory is not there" the way it treats the same answer from
// a local filesystem — and, more to the point, never mistake it for the wire
// having failed.
type NotExistError struct {
	// Op is the wire operation.
	Op string
	// Devpath is the position that was asked.
	Devpath string
	// Path is the directory that is not there.
	Path string
	// Reason is how the device said so: the FAIL text when it sent one, or a
	// note that it listed nothing at all.
	Reason string
}

// Error implements error.
func (e *NotExistError) Error() string {
	return fmt.Sprintf("adbwire: %s: no such directory %q on devpath=%s: %s",
		e.Op, e.Path, e.Devpath, e.Reason)
}

// Is lets errors.Is(err, fs.ErrNotExist) work.
func (e *NotExistError) Is(target error) bool { return target == fs.ErrNotExist }

// List reports the entries of dir on the device, without "." and "..",
// sorted by name.
//
// A directory the device does not have is a *NotExistError, which
// errors.Is(err, fs.ErrNotExist) recognises; it is not a transport failure and
// must not be retried as one. Sizes are 32-bit for the same reason Stat's are
// — DENT carries st_size as a uint32 — and every regular file in the result
// is marked [FileInfo.SizeTruncated]. LIST_V2 would fix both and is not
// implemented.
//
// The order the daemon sends entries in is readdir's, which is whatever the
// filesystem feels like; sorting here is what lets two listings of the same
// directory compare equal.
func (s *SyncConn) List(ctx context.Context, dir string) ([]FileInfo, error) {
	const op = "sync_list"

	if err := validateRemotePath(op, dir); err != nil {
		return nil, err
	}
	if err := s.begin(op); err != nil {
		return nil, err
	}
	defer s.end()

	disarm := s.bound(ctx)
	defer disarm()

	if err := s.writeRequest(ctx, op, idLIST, dir); err != nil {
		return nil, err
	}

	buf := s.staging()
	var entries []FileInfo
	// raw counts every DENT, dot entries included. Zero of them after DONE
	// is how the v1 daemon says the directory could not be opened.
	raw := 0
	for {
		id, second, err := s.readHeader(ctx, op)
		if err != nil {
			return nil, err
		}
		switch id {
		case idDENT:
			var tail [dentTailLen]byte
			if _, rerr := io.ReadFull(s.st, tail[:]); rerr != nil {
				s.markBroken()
				return nil, s.wrapRead(ctx, op, rerr)
			}
			size := binary.LittleEndian.Uint32(tail[0:4])
			mtime := binary.LittleEndian.Uint32(tail[4:8])
			n := binary.LittleEndian.Uint32(tail[8:12])
			// Checked before it is used for anything, like a DATA length. A
			// name is one path component, and a peer announcing one longer
			// than a whole path may be is not describing a file.
			if n > MaxSyncPath {
				s.markBroken()
				return nil, s.frameErr(op, fmt.Sprintf(
					"DENT name of %d bytes exceeds the %d-byte path limit for %s on devpath=%s",
					n, MaxSyncPath, dir, s.devpath))
			}
			if _, rerr := io.ReadFull(s.st, buf[:n]); rerr != nil {
				s.markBroken()
				return nil, s.wrapRead(ctx, op, rerr)
			}
			raw++
			name := string(buf[:n])
			if name == "." || name == ".." {
				// Counted above, dropped here: they prove the directory is
				// there and say nothing else a caller could use.
				continue
			}
			entries = append(entries, fileInfoFromWire(joinRemote(dir, name), name, second, size, mtime))

		case idDONE:
			// The listing's DONE is a full dent, not a status. Its tail is
			// consumed and ignored so the session stays aligned for whatever
			// comes next.
			var tail [dentTailLen]byte
			if _, rerr := io.ReadFull(s.st, tail[:]); rerr != nil {
				s.markBroken()
				return nil, s.wrapRead(ctx, op, rerr)
			}
			if raw == 0 {
				return nil, &NotExistError{
					Op: op, Devpath: s.devpath, Path: dir,
					Reason: "the device listed nothing, not even \".\", which is how LIST answers a directory it cannot open",
				}
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
			return entries, nil

		case idFAIL:
			reason, rerr := s.readReason(ctx, op, second)
			if rerr != nil {
				return nil, rerr
			}
			if absentReason(reason) {
				// A daemon that does say "no such directory" in a FAIL gets
				// the same typed answer as one that says it by listing
				// nothing. The session is finished either way: a FAIL ends
				// it on the daemon's side.
				s.markBroken()
				protocolFailuresTotal.WithLabelValues(op, "sync_refused").Inc()
				return nil, &NotExistError{Op: op, Devpath: s.devpath, Path: dir, Reason: reason}
			}
			return nil, s.refused(op, dir, reason)

		default:
			s.markBroken()
			return nil, s.frameErr(op, fmt.Sprintf("expected DENT, DONE or FAIL, got %q", id))
		}
	}
}

// List reports the entries of dir on the device at devpath. It opens a
// session, lists, and closes it.
func (c *Client) List(ctx context.Context, devpath, dir string) ([]FileInfo, error) {
	const op = "sync_list"
	if err := validateRemotePath(op, dir); err != nil {
		return nil, err
	}
	s, err := c.Sync(ctx, devpath)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.List(ctx, dir)
}

// absentReason reports whether a FAIL's text is the daemon's way of saying
// the path is not there. These are errno strings, spelled the way bionic
// spells them.
func absentReason(reason string) bool {
	low := strings.ToLower(reason)
	return strings.Contains(low, "no such file or directory") ||
		strings.Contains(low, "not a directory")
}

// joinRemote appends an entry name to the directory it was listed from,
// without doubling the slash a caller may have left on the directory.
func joinRemote(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}
