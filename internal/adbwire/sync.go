package adbwire

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// The file-sync protocol
//
// "sync:" is an ordinary device-side service — it is started on a transport
// exactly like "shell,v2,raw:" — but everything after the OKAY that answers it
// speaks a DIFFERENT framing from the host protocol this package otherwise
// uses. There is no 4-hex-digit length prefix here at all: a message is a
// 4-byte ASCII id followed by a little-endian uint32, and for the id/length
// forms, that many bytes of payload.
//
// Nothing in this file may therefore touch conn.writeMessage, conn.readMessage
// or conn.readStatus. Mixing the two framings is the classic way to
// desynchronise this stream, and a desynchronised stream does not fail loudly:
// it reads four arbitrary bytes as a length and asks for that many.
//
// Three properties are load-bearing:
//
//  1. Transfers stream. A single fixed staging buffer of SYNC_DATA_MAX carries
//     every chunk in both directions, so a 200 MB artifact costs 64 KiB of
//     heap and not 200 MB — which across twenty-eight devices in one process
//     is the difference between 1.8 MB and 5.6 GB.
//
//  2. A length that arrives from the far end is CHECKED before it is used, and
//     it is never used to size anything. The uint32 in a DATA header is
//     attacker-controlled in the only sense that matters here — a wedged or
//     hostile peer picks it — and a peer that says 0xFFFFFFFF is not asking
//     for four gigabytes of memory, it is failing to speak the protocol.
//
//  3. Every transfer is bounded by the context it was handed, which is not the
//     context the session was opened with. A session lasts as long as the work
//     using it; one transfer lasts as long as the budget somebody wrote down
//     for that step. A device that stops answering without closing anything —
//     no error, no EOF, no reset — is the case where those two diverge, and if
//     a transfer were bounded only by its session, that device would hold a
//     step open for the whole of the work's remaining time. Every read and
//     write below therefore runs under the caller's deadline, and when that
//     deadline is what ends the transfer the result says so instead of blaming
//     the wire for it.
// ---------------------------------------------------------------------------

// SyncService is the device-side service string that switches a transport to
// the file-sync protocol.
const SyncService = "sync:"

const (
	syncIDLen     = 4
	syncHeaderLen = 8 // 4-byte ASCII id + little-endian uint32

	// SyncDataMax is SYNC_DATA_MAX: the largest payload one DATA chunk may
	// carry. It is a protocol constant, not a tuning knob — a peer sending
	// more than this is not speaking the protocol, and this package refuses
	// such a chunk rather than trusting the number in it.
	SyncDataMax = 64 * 1024

	// MaxSyncPath bounds a request payload, matching the limit the adb client
	// enforces on the paths it sends. A remote path longer than this is a
	// caller mistake, and catching it here keeps a malformed request off the
	// wire entirely.
	MaxSyncPath = 1024

	// quitTimeout bounds the courtesy QUIT that Close writes. Close is
	// deferred on paths that have no context left, so this is the only thing
	// standing between a peer that has stopped reading and a Close that never
	// returns.
	quitTimeout = 2 * time.Second
)

// syncID is a 4-byte request or reply identifier. It is compared as an array
// so a mistyped id is a compile error rather than a runtime string mismatch.
type syncID [syncIDLen]byte

// String renders an id for an error message without letting the wire put
// control characters into a log line.
func (i syncID) String() string { return printable(i[:]) }

var (
	idSEND = syncID{'S', 'E', 'N', 'D'}
	idRECV = syncID{'R', 'E', 'C', 'V'}
	// idSTAT is LSTAT_V1 in both directions: the request and the reply carry
	// the same id, and the reply's three uint32 fields are mode, size, mtime.
	idSTAT = syncID{'S', 'T', 'A', 'T'}
	idDATA = syncID{'D', 'A', 'T', 'A'}
	idDONE = syncID{'D', 'O', 'N', 'E'}
	idOKAY = syncID{'O', 'K', 'A', 'Y'}
	idFAIL = syncID{'F', 'A', 'I', 'L'}
	idQUIT = syncID{'Q', 'U', 'I', 'T'}
)

// ---------------------------------------------------------------------------
// POSIX mode translation
// ---------------------------------------------------------------------------

// POSIX st_mode bits, as the wire carries them. These are NOT fs.FileMode
// values: Go's FileMode is its own encoding, and conflating the two silently
// turns a directory into a file with peculiar permissions.
const (
	posixTypeMask = 0o170000
	posixFIFO     = 0o010000
	posixCharDev  = 0o020000
	posixDir      = 0o040000
	posixBlockDev = 0o060000
	posixRegular  = 0o100000
	posixSymlink  = 0o120000
	posixSocket   = 0o140000
	posixSetuid   = 0o4000
	posixSetgid   = 0o2000
	posixSticky   = 0o1000
	posixPermMask = 0o777
)

// posixMode renders a Go file mode as the st_mode a SEND request carries.
//
// The value goes out in DECIMAL, which is not cosmetic: the receiving daemon
// parses it with a base-detecting conversion, so "0755" would be read as octal
// 0755 while "493" is read as decimal 493 — the same number. Emitting the
// permissions as bare octal digits without a radix would set the wrong bits.
func posixMode(m fs.FileMode) uint32 {
	p := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		p |= posixSetuid
	}
	if m&fs.ModeSetgid != 0 {
		p |= posixSetgid
	}
	if m&fs.ModeSticky != 0 {
		p |= posixSticky
	}
	switch {
	case m&fs.ModeSymlink != 0:
		p |= posixSymlink
	case m.IsDir():
		p |= posixDir
	default:
		// The type bits are sent, not just the permissions, because the
		// daemon reads them to decide whether it is being handed a symlink.
		p |= posixRegular
	}
	return p
}

// fileMode is the inverse of posixMode, for a mode read back off the wire.
func fileMode(p uint32) fs.FileMode {
	m := fs.FileMode(p & posixPermMask)
	if p&posixSetuid != 0 {
		m |= fs.ModeSetuid
	}
	if p&posixSetgid != 0 {
		m |= fs.ModeSetgid
	}
	if p&posixSticky != 0 {
		m |= fs.ModeSticky
	}
	switch p & posixTypeMask {
	case posixDir:
		m |= fs.ModeDir
	case posixSymlink:
		m |= fs.ModeSymlink
	case posixCharDev:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case posixBlockDev:
		m |= fs.ModeDevice
	case posixFIFO:
		m |= fs.ModeNamedPipe
	case posixSocket:
		m |= fs.ModeSocket
	}
	return m
}

// ---------------------------------------------------------------------------
// Stat results
// ---------------------------------------------------------------------------

// FileInfo is what LSTAT_V1 reports about one remote path.
type FileInfo struct {
	// Path is the remote path that was asked about, echoed back so a result
	// carried away from its call site still says what it describes.
	Path string

	// Mode is the wire mode translated into Go's encoding.
	Mode fs.FileMode

	// PosixMode is the raw st_mode. It is kept because fs.FileMode cannot
	// carry every type the wire can name, and a caller inspecting an unusual
	// entry should not have to guess what was lost in translation.
	PosixMode uint32

	// Size is the file size in bytes. The v1 stat carries it as a uint32, so
	// a file of 4 GiB or more is reported modulo 2^32; a caller that must be
	// exact about such a file should read it rather than stat it.
	Size int64

	// ModTime is the device-side modification time, zero when the device
	// reported none.
	ModTime time.Time

	// Exists distinguishes "no such path" from "a path whose stat is all
	// zeroes". LSTAT_V1 has no FAIL for a missing path: the daemon answers a
	// failed lstat with a zeroed reply, so absence is inferred from a mode of
	// zero and from nothing else.
	Exists bool
}

// IsDir reports whether the path is a directory.
func (f FileInfo) IsDir() bool { return f.Mode.IsDir() }

// IsRegular reports whether the path is an ordinary file.
func (f FileInfo) IsRegular() bool { return f.Exists && f.Mode.IsRegular() }

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// SyncConn is a live file-sync session on one device.
//
// It wraps a single socket and carries one request at a time. Requests are
// strictly sequential: the protocol has no request ids, so two transfers
// interleaved on one session would read each other's replies. That is not left
// to a caller to remember — a second request arriving while one is in flight is
// refused as a [UsageError] rather than allowed to desynchronise the stream.
//
// [SyncConn.Close] is the exception, and is safe from another goroutine at any
// time: it is how a blocked transfer is interrupted.
type SyncConn struct {
	st      *Stream
	devpath string

	// buf is the one staging buffer a session ever uses, sized to hold a
	// header and a full chunk. Every transfer reads and writes through it, so
	// the memory a transfer costs does not depend on the size of the file.
	// It is touched only by the goroutine holding busy.
	buf []byte

	// mu guards the three session flags. They are read and written by the
	// goroutine running a request AND by whichever goroutine calls Close to
	// interrupt one, which is the whole reason a lock exists on a type that
	// otherwise serves one caller at a time.
	mu sync.Mutex
	// broken records that the stream is desynchronised — a partial write, an
	// unreadable reply, a refusal the daemon answers by ending the session.
	// Sending another request over it would read someone else's bytes.
	broken bool
	closed bool
	// busy marks a request in flight. Close consults it so that its parting
	// QUIT is never interleaved into the middle of somebody's DATA stream.
	busy bool
}

// Sync opens a file-sync session on the device at devpath.
//
// ctx governs the SESSION: cancelling it closes the socket, which is what
// interrupts a transfer that is already blocked. Each transfer additionally
// takes its own context, and that one bounds the transfer alone — see
// [SyncConn.Pull] — so a session opened for a whole job can still hand each
// step the budget the step was given.
//
// Close it when the transfers are done.
func (c *Client) Sync(ctx context.Context, devpath string) (*SyncConn, error) {
	st, err := c.OpenService(ctx, devpath, SyncService)
	if err != nil {
		return nil, err
	}
	return &SyncConn{st: st, devpath: devpath}, nil
}

// Devpath returns the physical position this session is attached to.
func (s *SyncConn) Devpath() string { return s.devpath }

// Close ends the session and closes its socket. It is idempotent and safe to
// call from another goroutine to interrupt a blocked transfer.
func (s *SyncConn) Close() error {
	s.mu.Lock()
	quit := !s.closed && !s.broken && !s.busy
	s.closed = true
	s.mu.Unlock()

	if quit {
		// QUIT is a courtesy that lets the daemon end its side cleanly. It is
		// best effort on purpose: a device that has already gone away cannot
		// be told anything, and failing Close for that would turn a completed
		// transfer into an error.
		//
		// It is skipped when a transfer is in flight, because this call is
		// then racing that transfer's writes and eight bytes of QUIT landing
		// in the middle of a DATA chunk would corrupt the file being written.
		// Closing the socket is what interrupts a transfer; QUIT is only for
		// the orderly case.
		//
		// The bound is fixed rather than taken from a caller, because Close is
		// deferred on paths that have no context left and must return: a peer
		// whose receive window has stopped moving would otherwise park the
		// eight-byte courtesy write forever.
		disarm := s.st.cn.arm(context.Background(), quitTimeout)
		_ = s.writeRequest(context.Background(), "sync_quit", idQUIT, "")
		disarm()
	}
	return s.st.Close()
}

// ---------------------------------------------------------------------------
// Transfers
// ---------------------------------------------------------------------------

// Push streams r to remote on the device, creating it with mode.
//
// The reader is consumed in SYNC_DATA_MAX chunks and never held whole, so the
// cost of pushing a 200 MB artifact is one 64 KiB buffer.
//
// mode is sent verbatim, translated into st_mode. This package applies no
// policy to it: what permissions a job wants on a file it pushed is the job's
// business, and a wire package that second-guessed it would be lying about
// what it sent.
func (s *SyncConn) Push(ctx context.Context, r io.Reader, remote string, mode fs.FileMode) error {
	const op = "sync_push"

	if err := validateRemotePath(op, remote); err != nil {
		return err
	}
	if r == nil {
		return &UsageError{Op: op, Detail: "nil source reader", Value: remote}
	}
	if err := s.begin(op); err != nil {
		return err
	}
	defer s.end()

	disarm := s.bound(ctx)
	defer disarm()

	// SEND's payload is the path and the mode joined by a comma. The daemon
	// splits on the LAST comma, so a remote path containing commas is legal
	// and needs no escaping.
	spec := remote + "," + strconv.FormatUint(uint64(posixMode(mode)), 10)
	if err := s.writeRequest(ctx, op, idSEND, spec); err != nil {
		return err
	}

	buf := s.staging()
	copy(buf[:syncIDLen], idDATA[:])
	for {
		// Checked every chunk, not left to the socket. Two things depend on
		// it: a cancelled push stops at the next chunk boundary rather than
		// running a multi-gigabyte artifact to completion for a caller that
		// has stopped caring, and a source that keeps answering (0, nil) —
		// legal for an io.Reader, and the shape a wedged pipe takes — cannot
		// spin here forever without ever touching the socket the deadline
		// lives on.
		if ce := contextError(ctx, op); ce != nil {
			s.markBroken()
			return ce
		}
		// The header is written from the same buffer as the payload, so one
		// chunk is one write. Two writes per chunk would put a 4-byte segment
		// in front of every 64 KiB of a multi-gigabyte transfer.
		n, rerr := r.Read(buf[syncHeaderLen:])
		if n > 0 {
			binary.LittleEndian.PutUint32(buf[syncIDLen:syncHeaderLen], uint32(n))
			if err := s.write(ctx, op, buf[:syncHeaderLen+n]); err != nil {
				return err
			}
		}
		if rerr == nil {
			continue
		}
		if rerr == io.EOF {
			break
		}
		// The source died with part of the file already on the wire. The
		// device is fine and the socket is fine; this is neither a transport
		// failure nor a refusal, so it is not dressed up as one. The session
		// is finished either way — the daemon is mid-file and will never be
		// told how the file ends.
		s.markBroken()
		return fmt.Errorf("adbwire: %s: reading the source for %s on devpath=%s: %w",
			op, remote, s.devpath, rerr)
	}

	// DONE carries the modification time to stamp on the file, in place of a
	// length. The host clock is the only clock available: the payload is a
	// stream, not a file, and has no mtime of its own. Truncating to uint32
	// is the protocol's choice, not ours; it runs out in 2106.
	copy(buf[:syncIDLen], idDONE[:])
	binary.LittleEndian.PutUint32(buf[syncIDLen:syncHeaderLen], uint32(time.Now().Unix()))
	if err := s.write(ctx, op, buf[:syncHeaderLen]); err != nil {
		return err
	}

	return s.readOutcome(ctx, op, remote)
}

// Pull streams remote off the device into w.
//
// Chunks are copied through the session's staging buffer, so the file's size
// does not become this process's memory. w is written as bytes arrive; a
// transfer that fails partway leaves what arrived already written, because
// there is nowhere else to have put it.
//
// ctx bounds THIS transfer and nothing else. A session opened for a whole job
// can therefore hand each step the budget that step was given, and a device
// that goes silent costs that budget rather than the job. When it is ctx that
// ends the transfer the result is a *CanceledError — a caller getting what it
// asked for — never a *TransportError blaming the wire.
func (s *SyncConn) Pull(ctx context.Context, remote string, w io.Writer) error {
	const op = "sync_pull"

	if err := validateRemotePath(op, remote); err != nil {
		return err
	}
	if w == nil {
		return &UsageError{Op: op, Detail: "nil destination writer", Value: remote}
	}
	if err := s.begin(op); err != nil {
		return err
	}
	defer s.end()

	disarm := s.bound(ctx)
	defer disarm()

	if err := s.writeRequest(ctx, op, idRECV, remote); err != nil {
		return err
	}

	buf := s.staging()
	for {
		id, n, err := s.readHeader(ctx, op)
		if err != nil {
			return err
		}
		switch id {
		case idDATA:
			// Checked BEFORE the number is used for anything. n is chosen by
			// the far end, and the only honest response to a chunk larger
			// than the protocol permits is to stop: a peer that sends one is
			// not speaking the protocol, and believing it would let it decide
			// how much memory this process reserves.
			if n > SyncDataMax {
				s.markBroken()
				return s.frameErr(op, fmt.Sprintf(
					"DATA chunk of %d bytes exceeds the %d-byte SYNC_DATA_MAX for %s on devpath=%s",
					n, SyncDataMax, remote, s.devpath))
			}
			if n == 0 {
				continue
			}
			if _, rerr := io.ReadFull(s.st, buf[:n]); rerr != nil {
				s.markBroken()
				return s.wrapRead(ctx, op, rerr)
			}
			// The short-write check is not redundant with the error check.
			// io.Writer's contract says a short write must report an error,
			// but this destination is supplied by a caller — in the runner it
			// is one end of a pipe feeding a content-addressed store — and a
			// writer that quietly consumed less than it was given would put a
			// file on the wire, a different file in the store, and a valid
			// digest of the wrong bytes on the step row. Nothing downstream
			// could tell.
			nw, werr := w.Write(buf[:n])
			if werr == nil && nw != int(n) {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				// The sink failed, not the device. Reported as itself, and
				// the session is abandoned because the rest of the file is
				// still queued behind it on the socket.
				s.markBroken()
				return fmt.Errorf("adbwire: %s: writing %s from devpath=%s to the destination: %w",
					op, remote, s.devpath, werr)
			}

		case idDONE:
			return nil

		case idFAIL:
			reason, rerr := s.readReason(ctx, op, n)
			if rerr != nil {
				return rerr
			}
			return s.refused(op, remote, reason)

		default:
			s.markBroken()
			return s.frameErr(op, fmt.Sprintf("expected DATA, DONE or FAIL, got %q", id))
		}
	}
}

// Stat reports what the device knows about remote, following no symlinks.
//
// A missing path is NOT an error: the v1 stat has no FAIL for one, and the
// daemon answers with a zeroed reply. That is reported as Exists false, which
// is what makes this usable as an existence check.
func (s *SyncConn) Stat(ctx context.Context, remote string) (FileInfo, error) {
	const op = "sync_stat"

	if err := validateRemotePath(op, remote); err != nil {
		return FileInfo{}, err
	}
	if err := s.begin(op); err != nil {
		return FileInfo{}, err
	}
	defer s.end()

	disarm := s.bound(ctx)
	defer disarm()

	if err := s.writeRequest(ctx, op, idSTAT, remote); err != nil {
		return FileInfo{}, err
	}

	// The reply is the id followed by three uint32s, so the second word of
	// the header is the mode rather than a length.
	id, mode, err := s.readHeader(ctx, op)
	if err != nil {
		return FileInfo{}, err
	}
	switch id {
	case idSTAT:
	case idFAIL:
		reason, rerr := s.readReason(ctx, op, mode)
		if rerr != nil {
			return FileInfo{}, rerr
		}
		return FileInfo{}, s.refused(op, remote, reason)
	default:
		s.markBroken()
		return FileInfo{}, s.frameErr(op, fmt.Sprintf("expected STAT or FAIL, got %q", id))
	}

	// The three fields after the id are read in full or the answer is thrown
	// away. A reply cut short here has an id and a zero mode, which is bit for
	// bit what the daemon sends for a path that does not exist — so a client
	// that took what arrived would report a device that hung up mid-answer as
	// "the file is not there", and a job would go on to recreate something
	// that was already present.
	var rest [8]byte
	if _, rerr := io.ReadFull(s.st, rest[:]); rerr != nil {
		s.markBroken()
		return FileInfo{}, s.wrapRead(ctx, op, rerr)
	}
	size := binary.LittleEndian.Uint32(rest[0:4])
	mtime := binary.LittleEndian.Uint32(rest[4:8])

	fi := FileInfo{
		Path:      remote,
		Mode:      fileMode(mode),
		PosixMode: mode,
		Size:      int64(size),
		Exists:    mode != 0,
	}
	if fi.Exists && mtime != 0 {
		fi.ModTime = time.Unix(int64(mtime), 0)
	}
	return fi, nil
}

// ---------------------------------------------------------------------------
// One-shot, position-addressed helpers
// ---------------------------------------------------------------------------

// Push streams r to remote on the device at devpath, creating it with mode.
// It opens a session, performs the transfer and closes it.
func (c *Client) Push(ctx context.Context, devpath string, r io.Reader, remote string, mode fs.FileMode) error {
	const op = "sync_push"
	// Checked before anything is dialled: a caller mistake should not cost a
	// socket, and it should not reach the device at all. Every mistake the
	// session method can catch is caught here, so the rule holds for all of
	// them and not only for the path.
	if err := validateRemotePath(op, remote); err != nil {
		return err
	}
	if r == nil {
		return &UsageError{Op: op, Detail: "nil source reader", Value: remote}
	}
	s, err := c.Sync(ctx, devpath)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Push(ctx, r, remote, mode)
}

// Pull streams remote off the device at devpath into w.
func (c *Client) Pull(ctx context.Context, devpath, remote string, w io.Writer) error {
	const op = "sync_pull"
	if err := validateRemotePath(op, remote); err != nil {
		return err
	}
	if w == nil {
		return &UsageError{Op: op, Detail: "nil destination writer", Value: remote}
	}
	s, err := c.Sync(ctx, devpath)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Pull(ctx, remote, w)
}

// Stat reports what the device at devpath knows about remote.
func (c *Client) Stat(ctx context.Context, devpath, remote string) (FileInfo, error) {
	const op = "sync_stat"
	if err := validateRemotePath(op, remote); err != nil {
		return FileInfo{}, err
	}
	s, err := c.Sync(ctx, devpath)
	if err != nil {
		return FileInfo{}, err
	}
	defer s.Close()
	return s.Stat(ctx, remote)
}

// ---------------------------------------------------------------------------
// Framing
// ---------------------------------------------------------------------------

// staging returns the session's single transfer buffer, sized once.
func (s *SyncConn) staging() []byte {
	if s.buf == nil {
		s.buf = make([]byte, syncHeaderLen+SyncDataMax)
	}
	return s.buf
}

// begin claims the session for one request, refusing a session that cannot
// carry one — and refusing a second concurrent request rather than letting two
// callers read each other's replies off a protocol that has no request ids.
func (s *SyncConn) begin(op string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return &UsageError{Op: op, Detail: "sync session is closed", Value: s.devpath}
	case s.broken:
		return &UsageError{
			Op:     op,
			Detail: "sync session ended on an earlier failure and cannot carry another request; open a new one",
			Value:  s.devpath,
		}
	case s.busy:
		return &UsageError{
			Op:     op,
			Detail: "sync session already has a request in flight; the protocol has no request ids, so one session carries one transfer at a time",
			Value:  s.devpath,
		}
	}
	s.busy = true
	return nil
}

// end releases the session claim taken by begin.
func (s *SyncConn) end() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
}

// markBroken records that the stream is desynchronised.
func (s *SyncConn) markBroken() {
	s.mu.Lock()
	s.broken = true
	s.mu.Unlock()
}

// bound applies ctx to the socket for the duration of one request, and returns
// the function that lifts it again.
//
// Without this the context each transfer is handed would be decorative. The
// stream's socket is closed only by the SESSION context, so a caller that
// passed a step's own budget to a single Pull would watch that budget elapse
// while the read stayed parked on a device that had gone quiet — the deadline
// a human wrote down, silently not applying to the one call it was written
// for. There is no fallback: a transfer's honest bound is the caller's
// context, and inventing a per-call timeout here would put a ceiling on how
// large a file this package can move.
//
// The deadline is deliberately NOT copied from ctx.Deadline() onto the socket,
// which is what conn.arm does for the host protocol. Doing both would leave
// two clocks racing for the same instant: the netpoller enforcing the socket
// deadline, and the runtime timer that marks the context done. The netpoller
// frequently wins by a few microseconds, and the read then fails with an i/o
// timeout while ctx.Err() is still nil — so the caller's own budget expiring
// would be reported as a socket timeout most of the time and as a cancellation
// the rest, which is worse than either. context.AfterFunc is documented to run
// only after the context is done, so routing both cancellation and expiry
// through it makes the classification the same on every run.
func (s *SyncConn) bound(ctx context.Context) (disarm func()) {
	nc := s.st.cn.nc
	_ = nc.SetDeadline(time.Time{})
	// Pushing the deadline into the past fails the in-flight syscall at once
	// without closing the socket, which is what lets a bounded transfer end
	// while leaving the session's own lifetime to the session's context.
	stop := context.AfterFunc(ctx, func() { _ = nc.SetDeadline(time.Unix(1, 0)) })
	return func() {
		stop()
		_ = nc.SetDeadline(time.Time{})
	}
}

// writeRequest sends one id-and-payload request.
func (s *SyncConn) writeRequest(ctx context.Context, op string, id syncID, payload string) error {
	if len(payload) > MaxSyncPath {
		return &UsageError{
			Op:     op,
			Detail: fmt.Sprintf("sync request of %d bytes exceeds the %d-byte limit", len(payload), MaxSyncPath),
		}
	}
	buf := make([]byte, syncHeaderLen+len(payload))
	copy(buf[:syncIDLen], id[:])
	binary.LittleEndian.PutUint32(buf[syncIDLen:syncHeaderLen], uint32(len(payload)))
	copy(buf[syncHeaderLen:], payload)
	return s.write(ctx, op, buf)
}

// write puts p on the wire in full.
func (s *SyncConn) write(ctx context.Context, op string, p []byte) error {
	for len(p) > 0 {
		n, err := s.st.Write(p)
		if err != nil {
			// A partial write leaves the daemon mid-message with no way to
			// resynchronise, so the session is finished whatever the caller
			// does next.
			s.markBroken()
			return s.reclassify(ctx, op, err)
		}
		p = p[n:]
	}
	return nil
}

// reclassify re-labels a failure the Stream already typed against the SESSION
// context, when it was in fact this request's own context that ended it.
//
// The Stream cannot know which it was: it was handed the session's context
// when the socket was opened, and the deadline that just fired came from the
// context of one transfer. Left alone, a step timeout would be reported as a
// socket timeout — a host failure in the metrics and in the log, for a device
// that did nothing wrong — and IsCanceled would answer false for a caller
// getting exactly what it asked for.
func (s *SyncConn) reclassify(ctx context.Context, op string, err error) error {
	if ce := contextError(ctx, op); ce != nil {
		return ce
	}
	return err
}

// readHeader reads an id and the uint32 that follows it. The second value is
// a length for DATA, FAIL and requests, an mtime for DONE, and a mode for a
// STAT reply — the protocol reuses the field rather than adding one.
func (s *SyncConn) readHeader(ctx context.Context, op string) (syncID, uint32, error) {
	var hdr [syncHeaderLen]byte
	if _, err := io.ReadFull(s.st, hdr[:]); err != nil {
		s.markBroken()
		return syncID{}, 0, s.wrapRead(ctx, op, err)
	}
	var id syncID
	copy(id[:], hdr[:syncIDLen])
	return id, binary.LittleEndian.Uint32(hdr[syncIDLen:]), nil
}

// readOutcome reads the OKAY or FAIL that ends a SEND.
func (s *SyncConn) readOutcome(ctx context.Context, op, remote string) error {
	id, n, err := s.readHeader(ctx, op)
	if err != nil {
		return err
	}
	switch id {
	case idOKAY:
		return nil
	case idFAIL:
		reason, rerr := s.readReason(ctx, op, n)
		if rerr != nil {
			return rerr
		}
		return s.refused(op, remote, reason)
	default:
		s.markBroken()
		return s.frameErr(op, fmt.Sprintf("expected OKAY or FAIL after the transfer, got %q", id))
	}
}

// readReason reads the message that follows FAIL.
//
// The length is bounded by the same rule as a data chunk. A refusal reason is
// diagnostic text; a peer claiming a gigabyte of it is not explaining itself,
// it is failing to speak the protocol.
func (s *SyncConn) readReason(ctx context.Context, op string, n uint32) (string, error) {
	if n > SyncDataMax {
		s.markBroken()
		return "", s.frameErr(op, fmt.Sprintf(
			"FAIL reason of %d bytes exceeds the %d-byte SYNC_DATA_MAX", n, SyncDataMax))
	}
	if n == 0 {
		return "(no reason supplied)", nil
	}
	buf := s.staging()
	if _, err := io.ReadFull(s.st, buf[:n]); err != nil {
		s.markBroken()
		return "", s.wrapRead(ctx, op, err)
	}
	return strings.TrimSpace(string(buf[:n])), nil
}

// refused turns a FAIL into a typed refusal and counts it.
//
// The session is marked finished: the daemon ends a sync session on the
// request it could not serve, so a second request written over this socket
// would be answered by nothing at all. Opening another session costs one dial.
func (s *SyncConn) refused(op, remote, reason string) error {
	s.markBroken()
	protocolFailuresTotal.WithLabelValues(op, "sync_refused").Inc()
	return &ProtocolError{
		Op:      op,
		Service: SyncService + remote,
		Devpath: s.devpath,
		Reason:  reason,
	}
}

// frameErr reports a peer that is not speaking the sync protocol.
func (s *SyncConn) frameErr(op, detail string) error {
	return s.st.cn.frameErr(op, detail)
}

// wrapRead classifies a read failure that arrived untyped.
//
// io.ReadFull hands back a bare io.EOF at a message boundary and a bare
// io.ErrUnexpectedEOF partway through one; both mean the device hung up
// mid-session, and both must be classified and counted rather than escaping as
// anonymous errors. An error the Stream already typed is passed through
// unchanged so a cancellation is never re-labelled as a host failure.
//
// This request's own context is consulted FIRST, before either branch. The
// Stream types its errors against the session's context, which is still live
// while a single transfer's budget runs out, so without this check the caller's
// own deadline would come back wearing a socket timeout's clothes.
func (s *SyncConn) wrapRead(ctx context.Context, op string, err error) error {
	if ce := contextError(ctx, op); ce != nil {
		return ce
	}
	if IsTransport(err) || IsCanceled(err) {
		return err
	}
	return s.st.cn.wrap(ctx, op, KindRead, err)
}

// validateRemotePath rejects a path the protocol cannot carry.
//
// NUL is the one character that must be refused: the payload is
// length-prefixed, so the wire would carry it happily, but the daemon opens
// the path as a C string and would act on a silently truncated prefix — which
// for a push means writing a file somewhere nobody asked for.
func validateRemotePath(op, remote string) error {
	switch {
	case remote == "":
		return &UsageError{Op: op, Detail: "empty remote path"}
	case strings.IndexByte(remote, 0) >= 0:
		return &UsageError{Op: op, Detail: "remote path contains NUL", Value: remote}
	case len(remote) > MaxSyncPath:
		return &UsageError{
			Op:     op,
			Detail: fmt.Sprintf("remote path of %d bytes exceeds the %d-byte limit", len(remote), MaxSyncPath),
		}
	}
	return nil
}
