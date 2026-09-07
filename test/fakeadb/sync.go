package fakeadb

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// The file-sync service
//
// "sync:" is a device-side service like any other, but it is INTERACTIVE:
// after its OKAY the socket carries a conversation, not a scripted payload,
// and it speaks a different framing from the host protocol — a 4-byte ASCII
// id plus a little-endian uint32, with no hex length prefix anywhere.
//
// Server.deviceStream answers a device service by writing one scripted blob
// and hanging up, which is the right shape for shell and the wrong shape for
// this. SyncServer is therefore its own listener: it speaks the slice of the
// host protocol a sync client needs (version, the long listing, and a
// transport switch resolved by devpath) and then holds the socket open for as
// long as the sync session lasts. It reuses this package's framing helpers,
// its Device type and its RST-on-close, so a failure produced here looks
// exactly like a failure produced by Server.
// ---------------------------------------------------------------------

// SyncDataMax is SYNC_DATA_MAX, the largest payload one DATA chunk may carry.
// A client that sends more is not speaking the protocol and is told so.
const SyncDataMax = 64 * 1024

// syncRequestMax bounds a request payload. The fake is a peer too: sizing a
// buffer from a number a client picked is the same mistake in either
// direction, and a test that wedges the harness proves nothing.
const syncRequestMax = 8 * 1024

// syncFileMax bounds what one SEND may accumulate in memory.
//
// The fake holds a pushed file whole, which is right for fixtures and wrong
// for a client that has lost track of when a transfer ends. Without this, such
// a client takes the test process down with an out-of-memory kill and the
// failure that gets reported is "the runner died", which says nothing about
// what broke. With it the same bug arrives as a FAIL naming the limit. The
// figure is far above anything a test should be moving and far below anything
// that hurts.
const syncFileMax = 64 << 20

// Sync message ids, exactly as they appear on the wire.
const (
	SyncSend = "SEND"
	SyncRecv = "RECV"
	SyncStat = "STAT"
	SyncList = "LIST"
	SyncDent = "DENT"
	SyncData = "DATA"
	SyncDone = "DONE"
	SyncOkay = "OKAY"
	SyncFail = "FAIL"
	SyncQuit = "QUIT"

	// SyncMkdir is not a sync id. It is how the fake records a "mkdir -p"
	// that arrived over the shell service, so a test can assert on it in the
	// same list as the transfers it was issued between.
	SyncMkdir = "MKDIR"
)

// shellServicePrefix is the device service a shell v2 command travels on.
// The sync server answers exactly one command over it.
const shellServicePrefix = "shell,v2,raw:"

// Shell v2 packet ids, as the fake writes them.
const (
	shellPacketStdout = 1
	shellPacketStderr = 2
	shellPacketExit   = 3
)

// POSIX st_mode bits the fake needs: the type bit it stamps on a stored file,
// and the permission mask the daemon applies to a mode a client sent.
const (
	syncModeTypeMask = 0o170000
	syncModeRegular  = 0o100000
	syncModeDir      = 0o040000
	syncModePerm     = 0o777

	// syncDirMode is what every directory the fake reports looks like. Real
	// directories have modes of their own; these are fixtures, and a test
	// that cares about a directory's permissions is testing the wrong thing.
	syncDirMode = syncModeDir | 0o755
	// syncDirSize is the st_size ext4 reports for a directory.
	syncDirSize = 4096
)

// SyncFile is one file in a device's virtual filesystem.
type SyncFile struct {
	// Mode is the POSIX st_mode as it will be reported by STAT.
	Mode uint32
	// Data is the whole file. These are test fixtures: nothing here is
	// expected to be larger than a test is willing to hold.
	Data []byte
	// MTime is what DONE stamped on it, or what PutFile supplied.
	MTime time.Time
}

// Perm returns the permission bits, which is what a mode assertion usually
// means.
func (f SyncFile) Perm() uint32 { return f.Mode & syncModePerm }

// SyncRequest is one sync request the server received.
type SyncRequest struct {
	At      time.Time
	Devpath string
	ID      string // "SEND", "RECV", "STAT", "LIST", "QUIT", or "MKDIR" for a shell mkdir
	Path    string // the remote path, with SEND's mode already split off; the directory for MKDIR
	Mode    uint32 // the mode a SEND carried, zero otherwise
	Reply   string // "OKAY", "FAIL: <msg>", "STAT", "LIST", "EXIT n", "RESET", "TRUNCATED", "OVERSIZE"
}

// SyncStats are cheap counters a test can assert on. ChunksIn and ChunksOut
// are what prove a large transfer was actually streamed rather than sent as
// one impossible frame.
type SyncStats struct {
	Sessions  int   // sync sessions opened
	Requests  int   // sync requests received
	ChunksIn  int   // DATA chunks received during SEND
	ChunksOut int   // DATA chunks written during RECV
	BytesIn   int64 // payload bytes received
	BytesOut  int64 // payload bytes written
	Faults    int   // injected sync faults applied
}

// ---------------------------------------------------------------------
// Fault injection
// ---------------------------------------------------------------------

// SyncFaultKind is how a scripted sync failure manifests.
type SyncFaultKind int

const (
	// SyncFaultNone is the zero value: no fault.
	SyncFaultNone SyncFaultKind = iota

	// SyncFaultFail answers with a well-formed FAIL frame. The protocol
	// worked and the request did not — a full disk, a permission denied.
	SyncFaultFail

	// SyncFaultTruncate stops the transfer and closes cleanly, without the
	// DONE that ends it. The client sees an orderly EOF in a place the
	// protocol does not allow one, which is a truncated file wearing the
	// disguise of a successful transfer.
	SyncFaultTruncate

	// SyncFaultReset severs the connection with a TCP RST partway through,
	// so the peer reads ECONNRESET rather than EOF. This is the shape of
	// DeviceFarmer/STF issue #663, applied to a file transfer.
	SyncFaultReset

	// SyncFaultOversize announces a DATA chunk larger than SYNC_DATA_MAX and
	// then sends nothing, holding the socket open. A client that trusts the
	// length reserves memory for a chunk that will never arrive and blocks
	// until its context dies; a client that checks the length first refuses
	// immediately. The fault exists to tell those two apart.
	SyncFaultOversize

	// SyncFaultStall accepts the request and then says nothing at all,
	// holding the socket open indefinitely. Nothing is wrong with the wire:
	// no error, no close, no timeout of its own — just a device that has
	// stopped answering, which is what a wedged adbd looks like from here.
	//
	// It is the only fault that can prove a client's per-transfer deadline is
	// real. Every other failure ends the socket, so a client whose context is
	// decorative still returns; against this one it returns only if the
	// context it was handed actually bounds the read.
	SyncFaultStall
)

func (k SyncFaultKind) String() string {
	switch k {
	case SyncFaultFail:
		return "fail"
	case SyncFaultTruncate:
		return "truncate"
	case SyncFaultReset:
		return "reset"
	case SyncFaultOversize:
		return "oversize"
	case SyncFaultStall:
		return "stall"
	default:
		return "none"
	}
}

// SyncFault is a scripted failure rule. Rules are consulted in registration
// order; the first that matches and is not exhausted fires.
type SyncFault struct {
	// Op is the sync request id the rule applies to, e.g. SyncSend. Empty
	// matches any.
	Op string

	// Devpath restricts the rule to one physical position. Empty matches any.
	Devpath string

	// PathMatch is a substring of the remote path. Empty matches any.
	PathMatch string

	Kind SyncFaultKind

	// Message is the FAIL text for SyncFaultFail.
	Message string

	// AfterChunks is how many DATA chunks pass before SyncFaultTruncate or
	// SyncFaultReset fires. Zero cuts before any data moves. A value beyond
	// the end of the transfer still fires, just before the DONE — so a rule
	// always manifests rather than silently doing nothing.
	AfterChunks int

	// OversizeLen is the length SyncFaultOversize announces. Zero announces
	// one byte more than SYNC_DATA_MAX, which is the smallest violation; a
	// test wanting to prove nothing is sized from it should pass something
	// preposterous.
	OversizeLen uint32

	// Times caps how often the rule fires. Zero means unlimited.
	Times int
}

type syncFaultRule struct {
	spec SyncFault
	used int
}

// ---------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------

// SyncServer is a fake ADB host server that speaks the file-sync service
// against a per-device in-memory filesystem.
type SyncServer struct {
	ln   net.Listener
	done chan struct{}
	wg   sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	nextTID int64
	devices []Device
	files   map[string]map[string]SyncFile
	// dirs holds the directories a test or a mkdir created explicitly. A
	// directory that merely contains a file needs no entry here: it is
	// implied by the file's path, the way it is on a real filesystem.
	dirs     map[string]map[string]struct{}
	faults   []*syncFaultRule
	requests []*SyncRequest
	conns    map[net.Conn]struct{}
	stats    SyncStats
}

// NewSync starts a sync-capable server on an ephemeral loopback port.
// Callers outside a test must Close it; inside a test prefer StartSync.
func NewSync(devs ...Device) (*SyncServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("fakeadb: listen: %w", err)
	}
	s := &SyncServer{
		ln:      ln,
		done:    make(chan struct{}),
		conns:   make(map[net.Conn]struct{}),
		files:   make(map[string]map[string]SyncFile),
		dirs:    make(map[string]map[string]struct{}),
		nextTID: 1,
	}
	s.wg.Add(1)
	go s.accept()
	for _, d := range devs {
		s.Add(d)
	}
	return s, nil
}

// StartSync is NewSync for tests: it fails the test on error and registers
// cleanup, so the listener and every connection are gone before the test
// returns.
func StartSync(tb testing.TB, devs ...Device) *SyncServer {
	tb.Helper()
	s, err := NewSync(devs...)
	if err != nil {
		tb.Fatalf("fakeadb: %v", err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	return s
}

// Addr is the host:port to dial.
func (s *SyncServer) Addr() string { return s.ln.Addr().String() }

// Close stops the listener, severs every open connection and waits for every
// goroutine the server started. It is idempotent.
func (s *SyncServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = make(map[net.Conn]struct{})
	s.mu.Unlock()

	err := s.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return err
}

// Add installs a device. Devpath is mandatory and unique: it is the physical
// position, and two rows claiming one position is a bug in the test.
func (s *SyncServer) Add(d Device) Device {
	if d.Devpath == "" {
		panic("fakeadb: Device.Devpath is required — the physical position is the key")
	}
	if d.State == "" {
		d.State = StateDevice
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.devices {
		if e.Devpath == d.Devpath {
			panic("fakeadb: duplicate devpath " + d.Devpath)
		}
	}
	if d.TransportID == 0 {
		d.TransportID = s.nextTID
		s.nextTID++
	}
	d.Features = append([]string(nil), d.Features...)
	s.devices = append(s.devices, d)
	if s.files[d.Devpath] == nil {
		s.files[d.Devpath] = make(map[string]SyncFile)
	}
	if s.dirs[d.Devpath] == nil {
		s.dirs[d.Devpath] = make(map[string]struct{})
	}
	return d
}

// SetState changes a device's wire state, so a test can prove that a transfer
// to a device that is not up is refused at the transport switch.
func (s *SyncServer) SetState(devpath string, st State) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.devices {
		if s.devices[i].Devpath == devpath {
			s.devices[i].State = st
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// The virtual filesystem
// ---------------------------------------------------------------------

// PutFile installs a file on a device, so a pull has something to pull. A
// mode with no type bits is stored as a regular file; a zero mode becomes
// 0644, because a file no test can read is never what the test meant.
func (s *SyncServer) PutFile(devpath, path string, mode uint32, data []byte) {
	if mode&syncModePerm == 0 {
		mode |= 0o644
	}
	if mode&syncModeTypeMask == 0 {
		mode |= syncModeRegular
	}
	s.putFile(devpath, path, mode, data, time.Now())
}

func (s *SyncServer) putFile(devpath, path string, mode uint32, data []byte, mtime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files[devpath] == nil {
		s.files[devpath] = make(map[string]SyncFile)
	}
	s.files[devpath][path] = SyncFile{
		Mode:  mode,
		Data:  append([]byte(nil), data...),
		MTime: mtime,
	}
}

// File returns one file from a device's filesystem.
func (s *SyncServer) File(devpath, path string) (SyncFile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[devpath][path]
	if !ok {
		return SyncFile{}, false
	}
	f.Data = append([]byte(nil), f.Data...)
	return f, true
}

// Paths returns the sorted paths present on a device. Sorted because an
// assertion against map order is an assertion against nothing.
func (s *SyncServer) Paths(devpath string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.files[devpath]))
	for p := range s.files[devpath] {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// Mkdir creates a directory on a device, parents included, so a LIST or a
// STAT of an empty directory has something to report. A directory that holds
// a file needs no Mkdir: it exists by implication, as it would on a device.
func (s *SyncServer) Mkdir(devpath, dir string) {
	dir = cleanDir(dir)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirs[devpath] == nil {
		s.dirs[devpath] = make(map[string]struct{})
	}
	s.dirs[devpath][dir] = struct{}{}
}

// Dirs returns the sorted directories a device holds explicitly — those made
// by Mkdir or by a client's mkdir — and not the ones implied by files.
func (s *SyncServer) Dirs(devpath string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.dirs[devpath]))
	for d := range s.dirs[devpath] {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// cleanDir strips the trailing slash a client may leave on a directory, so
// "/x/" and "/x" name one directory. The root keeps its one character.
func cleanDir(dir string) string {
	if len(dir) > 1 {
		dir = strings.TrimRight(dir, "/")
		if dir == "" {
			dir = "/"
		}
	}
	return dir
}

// syncEntry is one line of a listing before it is framed.
type syncEntry struct {
	name  string
	mode  uint32
	size  uint32
	mtime uint32
}

// listDir reports whether dir exists on a device and, if so, its entries in
// name order with "." and ".." first — which is what readdir on a real device
// yields, and what a client must be prepared to drop.
//
// Existence is by implication as much as by declaration: a directory exists
// if it was made, or if any file or made directory lies beneath it.
func (s *SyncServer) listDir(devpath, dir string) ([]syncEntry, bool) {
	dir = cleanDir(dir)
	prefix := dir + "/"
	if dir == "/" {
		prefix = "/"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, exists := s.dirs[devpath][dir]
	if dir == "/" {
		exists = true
	}
	children := map[string]syncEntry{}
	child := func(rest string, leaf syncEntry) {
		exists = true
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			// Something lives deeper down: the first component is a
			// directory of this one, whatever the leaf was.
			name := rest[:i]
			children[name] = syncEntry{name: name, mode: syncDirMode, size: syncDirSize}
			return
		}
		leaf.name = rest
		children[rest] = leaf
	}
	for p, f := range s.files[devpath] {
		if rest, ok := strings.CutPrefix(p, prefix); ok && rest != "" {
			child(rest, syncEntry{mode: f.Mode, size: uint32(len(f.Data)), mtime: uint32(f.MTime.Unix())})
		}
	}
	for d := range s.dirs[devpath] {
		if rest, ok := strings.CutPrefix(d, prefix); ok && rest != "" {
			child(rest, syncEntry{mode: syncDirMode, size: syncDirSize})
		}
	}
	if !exists {
		return nil, false
	}

	names := make([]string, 0, len(children))
	for n := range children {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([]syncEntry, 0, len(names)+2)
	out = append(out,
		syncEntry{name: ".", mode: syncDirMode, size: syncDirSize},
		syncEntry{name: "..", mode: syncDirMode, size: syncDirSize})
	for _, n := range names {
		out = append(out, children[n])
	}
	return out, true
}

// isDir reports whether a path is a directory a STAT should describe.
func (s *SyncServer) isDir(devpath, p string) bool {
	_, ok := s.listDir(devpath, p)
	return ok
}

// ---------------------------------------------------------------------
// Observation and fault registration
// ---------------------------------------------------------------------

// SyncRequests returns every sync request received, in arrival order.
func (s *SyncServer) SyncRequests() []SyncRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SyncRequest, 0, len(s.requests))
	for _, r := range s.requests {
		out = append(out, *r)
	}
	return out
}

// Stats returns the counters.
func (s *SyncServer) Stats() SyncStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// InjectSync registers a fault rule.
func (s *SyncServer) InjectSync(f SyncFault) {
	s.mu.Lock()
	s.faults = append(s.faults, &syncFaultRule{spec: f})
	s.mu.Unlock()
}

// ClearSyncFaults removes every rule, including partially consumed ones.
func (s *SyncServer) ClearSyncFaults() {
	s.mu.Lock()
	s.faults = nil
	s.mu.Unlock()
}

// FailSyncNext answers the next matching request with FAIL and message.
func (s *SyncServer) FailSyncNext(op, message string) {
	s.InjectSync(SyncFault{Op: op, Kind: SyncFaultFail, Message: message, Times: 1})
}

// ResetSyncAfter severs the next matching transfer with a RST once chunks
// data chunks have moved.
func (s *SyncServer) ResetSyncAfter(op string, chunks int) {
	s.InjectSync(SyncFault{Op: op, Kind: SyncFaultReset, AfterChunks: chunks, Times: 1})
}

// TruncateSyncAfter ends the next matching transfer cleanly, without DONE,
// once chunks data chunks have moved.
func (s *SyncServer) TruncateSyncAfter(op string, chunks int) {
	s.InjectSync(SyncFault{Op: op, Kind: SyncFaultTruncate, AfterChunks: chunks, Times: 1})
}

// OversizeSyncChunk makes the next RECV announce a DATA chunk of length bytes
// and send none of them. Pass 0 for the smallest violation of SYNC_DATA_MAX.
func (s *SyncServer) OversizeSyncChunk(length uint32) {
	s.InjectSync(SyncFault{Op: SyncRecv, Kind: SyncFaultOversize, OversizeLen: length, Times: 1})
}

// StallSyncNext makes the next matching request go unanswered, with the socket
// held open. Use it to prove that a client's own deadline ends a transfer a
// silent device would otherwise hold forever.
func (s *SyncServer) StallSyncNext(op string) {
	s.InjectSync(SyncFault{Op: op, Kind: SyncFaultStall, Times: 1})
}

func (s *SyncServer) takeSyncFault(op, devpath, path string) *SyncFault {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.faults {
		if r.spec.Op != "" && r.spec.Op != op {
			continue
		}
		if r.spec.Devpath != "" && r.spec.Devpath != devpath {
			continue
		}
		if r.spec.PathMatch != "" && !strings.Contains(path, r.spec.PathMatch) {
			continue
		}
		if r.spec.Times > 0 && r.used >= r.spec.Times {
			continue
		}
		r.used++
		s.stats.Faults++
		spec := r.spec
		return &spec
	}
	return nil
}

func faultMessage(f *SyncFault, fallback string) string {
	if f.Message != "" {
		return f.Message
	}
	return fallback
}

func (f *SyncFault) oversizeLen() uint32 {
	if f.OversizeLen == 0 {
		return SyncDataMax + 1
	}
	return f.OversizeLen
}

// ---------------------------------------------------------------------
// Host protocol
// ---------------------------------------------------------------------

func (s *SyncServer) accept() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serve(c)
	}
}

func (s *SyncServer) drop(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	_ = c.Close()
}

func (s *SyncServer) serve(c net.Conn) {
	defer s.wg.Done()
	defer s.drop(c)

	br := bufio.NewReader(c)
	svc, err := readFrame(br)
	if err != nil {
		return
	}
	_, cmd, ok := splitService(svc)
	if !ok {
		_ = writeAll(c, failBytes("unknown host service "+svc))
		return
	}

	switch {
	case cmd == "version":
		_ = writeAll(c, okayFrame(fmt.Sprintf("%04x", DefaultHostVersion)))
	case cmd == "devices", cmd == "devices-l":
		_ = writeAll(c, okayFrame(s.list(cmd == "devices-l")))
	case strings.HasPrefix(cmd, "transport:"):
		s.transport(c, br, cmd[len("transport:"):])
	default:
		_ = writeAll(c, failBytes("fakeadb: the sync server does not implement "+svc))
	}
}

func (s *SyncServer) list(long bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, d := range s.devices {
		if d.State == StateAbsent {
			continue
		}
		serial := d.Serial
		if serial == "" {
			serial = noSerial
		}
		if !long {
			fmt.Fprintf(&b, "%s\t%s\n", serial, d.State)
			continue
		}
		fmt.Fprintf(&b, "%-22s %s", serial, d.State)
		appendField(&b, "", d.Devpath, false)
		if d.State == StateDevice {
			appendField(&b, "product:", d.Product, false)
			appendField(&b, "model:", d.Model, true)
			appendField(&b, "device:", d.Codename, false)
		}
		fmt.Fprintf(&b, " transport_id:%d\n", d.TransportID)
	}
	return b.String()
}

// resolve mirrors atransport::MatchesTarget: a target matches a device by
// serial OR by devpath. Two clones sharing a serial both match, and the
// answer to that is a refusal rather than a guess — which is the whole reason
// every call in adbwire addresses a position.
func (s *SyncServer) resolve(text string) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m []Device
	for _, d := range s.devices {
		if d.State == StateAbsent {
			continue
		}
		if text == "" || d.Serial == text || d.Devpath == text {
			m = append(m, d)
		}
	}
	switch {
	case len(m) == 1:
		return m[0], nil
	case len(m) == 0:
		if text != "" {
			return Device{}, errors.New(MsgNotFound(text))
		}
		return Device{}, errors.New(MsgNoDevices)
	default:
		if text != "" {
			return Device{}, errors.New(MsgAmbiguousTarget)
		}
		return Device{}, errors.New(MsgMultipleDevices)
	}
}

func (s *SyncServer) transport(c net.Conn, br *bufio.Reader, text string) {
	d, err := s.resolve(text)
	if err != nil {
		_ = writeAll(c, failBytes(err.Error()))
		return
	}
	if d.State != StateDevice {
		_ = writeAll(c, failBytes(stateError(d.State)))
		return
	}
	if writeAll(c, []byte("OKAY")) != nil {
		return
	}

	// The service string is length-prefixed like any host request; only what
	// comes after its OKAY changes framing.
	svc, err := readFrame(br)
	if err != nil {
		return
	}
	switch {
	case svc == "sync:":
		if writeAll(c, []byte("OKAY")) != nil {
			return
		}
		s.mu.Lock()
		s.stats.Sessions++
		s.mu.Unlock()
		s.serveSync(c, br, d.Devpath)

	case strings.HasPrefix(svc, shellServicePrefix):
		// The one shell command a sync client needs: the protocol has no
		// mkdir, so a client that wants a directory has to ask the shell for
		// it, and a fake that could not answer would leave "push into a
		// directory that does not exist yet" untestable end to end.
		if writeAll(c, []byte("OKAY")) != nil {
			return
		}
		s.serveShell(c, d.Devpath, strings.TrimPrefix(svc, shellServicePrefix))

	default:
		_ = writeAll(c, failBytes("fakeadb: the sync server implements sync: and shell mkdir, not "+svc))
	}
}

// ---------------------------------------------------------------------
// Sync framing
// ---------------------------------------------------------------------

func syncHeader(id string, n uint32) []byte {
	b := make([]byte, 8)
	copy(b[:4], id)
	binary.LittleEndian.PutUint32(b[4:], n)
	return b
}

func syncOkay() []byte { return syncHeader(SyncOkay, 0) }

func syncFailBytes(msg string) []byte {
	return append(syncHeader(SyncFail, uint32(len(msg))), msg...)
}

// readSyncHeader reads an id and the uint32 that follows it.
func readSyncHeader(r io.Reader) (string, uint32, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", 0, err
	}
	return string(hdr[:4]), binary.LittleEndian.Uint32(hdr[4:]), nil
}

// serveSync runs one sync session until the client quits, the session fails,
// or the socket dies.
func (s *SyncServer) serveSync(c net.Conn, br *bufio.Reader, devpath string) {
	for {
		id, n, err := readSyncHeader(br)
		if err != nil {
			return
		}
		if n > syncRequestMax {
			_ = writeAll(c, syncFailBytes(fmt.Sprintf(
				"request of %d bytes exceeds the %d-byte limit", n, syncRequestMax)))
			return
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			return
		}

		switch id {
		case SyncSend:
			path, mode := splitSendSpec(string(payload))
			rec := s.recordSync(devpath, id, path, mode)
			if !s.doSend(c, br, devpath, path, mode, rec) {
				return
			}
		case SyncRecv:
			path := string(payload)
			rec := s.recordSync(devpath, id, path, 0)
			if !s.doRecv(c, devpath, path, rec) {
				return
			}
		case SyncStat:
			path := string(payload)
			rec := s.recordSync(devpath, id, path, 0)
			if !s.doStat(c, devpath, path, rec) {
				return
			}
		case SyncList:
			path := string(payload)
			rec := s.recordSync(devpath, id, path, 0)
			if !s.doList(c, devpath, path, rec) {
				return
			}
		case SyncQuit:
			s.recordSync(devpath, id, "", 0)
			return
		default:
			s.recordSync(devpath, id, string(payload), 0)
			_ = writeAll(c, syncFailBytes("unknown sync id "+id))
			return
		}
	}
}

// splitSendSpec peels the mode off a SEND payload. The daemon splits on the
// LAST comma, so a remote path containing commas survives the round trip.
func splitSendSpec(spec string) (string, uint32) {
	i := strings.LastIndexByte(spec, ',')
	if i < 0 {
		return spec, 0
	}
	// Base 0, as the daemon parses it: a decimal st_mode has no leading zero
	// and is read as decimal, while a leading zero would be read as octal.
	// A client that formats 0755 as "0755" and one that formats it as "493"
	// must both end up with the same permissions.
	v, err := strconv.ParseUint(strings.TrimSpace(spec[i+1:]), 0, 32)
	if err != nil {
		return spec[:i], 0
	}
	return spec[:i], uint32(v)
}

func (s *SyncServer) recordSync(devpath, id, path string, mode uint32) *SyncRequest {
	rec := &SyncRequest{At: time.Now(), Devpath: devpath, ID: id, Path: path, Mode: mode}
	s.mu.Lock()
	s.requests = append(s.requests, rec)
	s.stats.Requests++
	s.mu.Unlock()
	return rec
}

func (s *SyncServer) note(rec *SyncRequest, reply string) {
	s.mu.Lock()
	rec.Reply = reply
	s.mu.Unlock()
}

func (s *SyncServer) countIn(n int) {
	s.mu.Lock()
	s.stats.ChunksIn++
	s.stats.BytesIn += int64(n)
	s.mu.Unlock()
}

func (s *SyncServer) countOut(n int) {
	s.mu.Lock()
	s.stats.ChunksOut++
	s.stats.BytesOut += int64(n)
	s.mu.Unlock()
}

// ---------------------------------------------------------------------
// SEND / RECV / STAT
// ---------------------------------------------------------------------

// doSend consumes DATA chunks until DONE and stores the result. It reports
// whether the session may continue.
func (s *SyncServer) doSend(c net.Conn, br *bufio.Reader, devpath, path string, mode uint32, rec *SyncRequest) bool {
	f := s.takeSyncFault(SyncSend, devpath, path)
	var data []byte
	chunks := 0

	for {
		// Checked before each chunk rather than after, so AfterChunks: 0
		// means "cut before a single byte of the file moves".
		if f != nil && chunks >= f.AfterChunks {
			switch f.Kind {
			case SyncFaultReset:
				s.note(rec, "RESET")
				sever(c)
				return false
			case SyncFaultTruncate:
				s.note(rec, "TRUNCATED")
				_ = c.Close()
				return false
			}
		}

		id, n, err := readSyncHeader(br)
		if err != nil {
			return false
		}
		switch id {
		case SyncData:
			if n > SyncDataMax {
				s.note(rec, "FAIL: oversized chunk")
				_ = writeAll(c, syncFailBytes(fmt.Sprintf(
					"DATA chunk of %d bytes exceeds SYNC_DATA_MAX", n)))
				return false
			}
			// Refused before the allocation, not after it: the point of a
			// bound is that the number a peer sent never decides how much
			// memory this process reserves, and that rule does not stop
			// applying because the peer is the test.
			if int64(len(data))+int64(n) > syncFileMax {
				s.note(rec, "FAIL: oversized file")
				_ = writeAll(c, syncFailBytes(fmt.Sprintf(
					"file exceeds the fake's %d-byte limit; a real device has a disk, this has a heap", syncFileMax)))
				return false
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(br, buf); err != nil {
				return false
			}
			data = append(data, buf...)
			chunks++
			s.countIn(int(n))

		case SyncDone:
			if f != nil && f.Kind == SyncFaultStall {
				// The whole file arrived and the acknowledgement never does.
				// A client that waits here forever has no deadline of its own.
				s.note(rec, "STALL")
				s.hold(c)
				return false
			}
			if f != nil && f.Kind == SyncFaultFail {
				msg := faultMessage(f, "write failed: No space left on device")
				s.note(rec, "FAIL: "+msg)
				_ = writeAll(c, syncFailBytes(msg))
				return false
			}
			// The mode is masked exactly as the daemon masks it, and the type
			// bits a client sent are replaced with "regular file" — the
			// daemon creates a file, whatever the client claimed.
			s.putFile(devpath, path, (mode&syncModePerm)|syncModeRegular, data, time.Unix(int64(n), 0))
			s.note(rec, "OKAY")
			return writeAll(c, syncOkay()) == nil

		case SyncFail:
			// The client is abandoning the transfer. Nothing is stored: a
			// half-written file that looked complete would be worse than none.
			s.note(rec, "ABORTED")
			return false

		default:
			s.note(rec, "FAIL: unexpected id")
			_ = writeAll(c, syncFailBytes("unexpected sync id "+id+" during SEND"))
			return false
		}
	}
}

// doRecv streams a file back in SYNC_DATA_MAX chunks.
func (s *SyncServer) doRecv(c net.Conn, devpath, path string, rec *SyncRequest) bool {
	f := s.takeSyncFault(SyncRecv, devpath, path)

	if f != nil {
		switch f.Kind {
		case SyncFaultOversize:
			// A header and nothing behind it. The socket is then held open so
			// that a client which believed the length blocks on a read that
			// can never be satisfied, instead of being let off by an EOF.
			s.note(rec, "OVERSIZE")
			if writeAll(c, syncHeader(SyncData, f.oversizeLen())) != nil {
				return false
			}
			s.hold(c)
			return false
		case SyncFaultStall:
			s.note(rec, "STALL")
			s.hold(c)
			return false
		case SyncFaultFail:
			msg := faultMessage(f, "open failed: Permission denied")
			s.note(rec, "FAIL: "+msg)
			_ = writeAll(c, syncFailBytes(msg))
			return false
		}
	}

	file, ok := s.File(devpath, path)
	if !ok {
		// What adbd answers for a path it cannot open: a FAIL carrying the
		// errno text, not a zero-length success.
		msg := "open failed: No such file or directory"
		s.note(rec, "FAIL: "+msg)
		_ = writeAll(c, syncFailBytes(msg))
		return false
	}

	sent := 0
	for off := 0; off < len(file.Data); off += SyncDataMax {
		if f != nil && sent >= f.AfterChunks && s.cut(c, f, rec) {
			return false
		}
		end := min(off+SyncDataMax, len(file.Data))
		chunk := file.Data[off:end]
		if writeAll(c, syncHeader(SyncData, uint32(len(chunk)))) != nil {
			return false
		}
		if writeAll(c, chunk) != nil {
			return false
		}
		sent++
		s.countOut(len(chunk))
	}
	// A rule whose AfterChunks lies beyond the end of the file still fires,
	// here, in place of the DONE. A fault that silently did nothing would
	// make a test pass for the wrong reason.
	if f != nil && s.cut(c, f, rec) {
		return false
	}

	s.note(rec, "OKAY")
	return writeAll(c, syncHeader(SyncDone, 0)) == nil
}

// cut applies a reset or truncate fault, reporting whether it fired.
func (s *SyncServer) cut(c net.Conn, f *SyncFault, rec *SyncRequest) bool {
	switch f.Kind {
	case SyncFaultReset:
		s.note(rec, "RESET")
		sever(c)
		return true
	case SyncFaultTruncate:
		s.note(rec, "TRUNCATED")
		_ = c.Close()
		return true
	}
	return false
}

// doStat answers LSTAT_V1: the id, then mode, size and mtime.
//
// A missing path is answered with a zeroed reply and no FAIL, which is how
// the v1 protocol says "no such path" — there is no other way for it to say
// so, and a client that expects a refusal here will hang against real adbd.
func (s *SyncServer) doStat(c net.Conn, devpath, path string, rec *SyncRequest) bool {
	f := s.takeSyncFault(SyncStat, devpath, path)
	if f != nil {
		switch f.Kind {
		case SyncFaultFail:
			msg := faultMessage(f, "stat failed: Permission denied")
			s.note(rec, "FAIL: "+msg)
			_ = writeAll(c, syncFailBytes(msg))
			return false
		case SyncFaultStall:
			s.note(rec, "STALL")
			s.hold(c)
			return false
		case SyncFaultReset:
			s.note(rec, "RESET")
			sever(c)
			return false
		case SyncFaultTruncate:
			// Half a reply: the id and the mode, then nothing. The client is
			// left mid-message, which is a different failure from a clean EOF
			// at a message boundary.
			s.note(rec, "TRUNCATED")
			_ = writeAll(c, syncHeader(SyncStat, 0))
			_ = c.Close()
			return false
		}
	}

	out := make([]byte, 16)
	copy(out[:4], SyncStat)
	switch file, ok := s.File(devpath, path); {
	case ok:
		binary.LittleEndian.PutUint32(out[4:8], file.Mode)
		binary.LittleEndian.PutUint32(out[8:12], uint32(len(file.Data)))
		binary.LittleEndian.PutUint32(out[12:16], uint32(file.MTime.Unix()))
		s.note(rec, "STAT")
	case s.isDir(devpath, path):
		binary.LittleEndian.PutUint32(out[4:8], syncDirMode)
		binary.LittleEndian.PutUint32(out[8:12], syncDirSize)
		binary.LittleEndian.PutUint32(out[12:16], uint32(time.Now().Unix()))
		s.note(rec, "STAT: dir")
	default:
		s.note(rec, "STAT: absent")
	}
	return writeAll(c, out) == nil
}

// doList answers LIST: one DENT per entry, "." and ".." included as readdir
// includes them, then a DONE that is a full dent with every field zero — the
// daemon writes sizeof(dent) for it, not the 8-byte status.
//
// A directory the device does not have is answered with DONE and nothing
// before it. That is not a shortcut: v1 LIST has no FAIL for a path it cannot
// open, and a client that expects one will hang against real adbd.
func (s *SyncServer) doList(c net.Conn, devpath, dir string, rec *SyncRequest) bool {
	f := s.takeSyncFault(SyncList, devpath, dir)
	if f != nil {
		switch f.Kind {
		case SyncFaultFail:
			msg := faultMessage(f, "opendir failed: Permission denied")
			s.note(rec, "FAIL: "+msg)
			_ = writeAll(c, syncFailBytes(msg))
			return false
		case SyncFaultStall:
			s.note(rec, "STALL")
			s.hold(c)
			return false
		case SyncFaultReset:
			s.note(rec, "RESET")
			sever(c)
			return false
		}
	}

	entries, ok := s.listDir(devpath, dir)
	if !ok {
		s.note(rec, "LIST: absent")
		return writeAll(c, syncDent(SyncDone, syncEntry{})) == nil
	}
	for i, e := range entries {
		if f != nil && f.Kind == SyncFaultTruncate && i >= f.AfterChunks {
			// Cut mid-listing: the client has entries and no DONE, which is
			// a different failure from a directory that is not there.
			s.note(rec, "TRUNCATED")
			_ = c.Close()
			return false
		}
		if writeAll(c, syncDent(SyncDent, e)) != nil {
			return false
		}
	}
	if f != nil && f.Kind == SyncFaultTruncate {
		s.note(rec, "TRUNCATED")
		_ = c.Close()
		return false
	}
	s.note(rec, "LIST")
	return writeAll(c, syncDent(SyncDone, syncEntry{})) == nil
}

// syncDent frames one directory entry, or the DONE that ends a listing.
func syncDent(id string, e syncEntry) []byte {
	b := make([]byte, 20, 20+len(e.name))
	copy(b[:4], id)
	binary.LittleEndian.PutUint32(b[4:8], e.mode)
	binary.LittleEndian.PutUint32(b[8:12], e.size)
	binary.LittleEndian.PutUint32(b[12:16], e.mtime)
	binary.LittleEndian.PutUint32(b[16:20], uint32(len(e.name)))
	return append(b, e.name...)
}

// ---------------------------------------------------------------------
// The shell, as far as mkdir
// ---------------------------------------------------------------------

// serveShell answers the one shell command the sync server understands:
// "mkdir -p" of a single-quoted word. Anything else exits 127 with a stderr
// line saying so, because a fake that silently exits 0 for a command it
// never ran is a fake that lets a mis-quoted path pass.
//
// The word is UNQUOTED here, deliberately. A client that sent the path bare,
// or double-quoted, or with a quote left unescaped, would not parse as one
// word and would be told so with an exit status — which is the only way this
// fake can hold a client to the quoting it claims to do.
func (s *SyncServer) serveShell(c net.Conn, devpath, command string) {
	rec := s.recordSync(devpath, SyncMkdir, "", 0)
	quoted, ok := strings.CutPrefix(command, "mkdir -p ")
	dir, parsed := "", false
	if ok {
		dir, parsed = unquoteShellWord(quoted)
	}
	if !parsed {
		s.note(rec, "EXIT 127")
		_ = writeAll(c, shellPacket(shellPacketStderr,
			"sh: fakeadb answers only mkdir -p of one single-quoted word, not: "+command+"\n"))
		_ = writeAll(c, shellPacket(shellPacketExit, "\x7f"))
		return
	}
	s.mu.Lock()
	rec.Path = dir
	s.mu.Unlock()

	if f := s.takeSyncFault(SyncMkdir, devpath, dir); f != nil {
		switch f.Kind {
		case SyncFaultFail:
			msg := faultMessage(f, "mkdir: '"+dir+"': Permission denied")
			s.note(rec, "EXIT 1")
			_ = writeAll(c, shellPacket(shellPacketStderr, msg+"\n"))
			_ = writeAll(c, shellPacket(shellPacketExit, "\x01"))
			return
		case SyncFaultStall:
			s.note(rec, "STALL")
			s.hold(c)
			return
		case SyncFaultReset:
			s.note(rec, "RESET")
			sever(c)
			return
		case SyncFaultTruncate:
			// The stream ends with no exit frame at all: the shape of a
			// device that hung up mid-command.
			s.note(rec, "TRUNCATED")
			_ = c.Close()
			return
		}
	}
	s.Mkdir(devpath, dir)
	s.note(rec, "EXIT 0")
	_ = writeAll(c, shellPacket(shellPacketExit, "\x00"))
}

// unquoteShellWord accepts exactly the form a careful client produces — a run
// of single-quoted segments, each joined to the next by a backslash-escaped
// quote (which is how a quote inside the word is spelled) — and returns the
// word the shell would see. Anything else is refused: a bare path, a
// double-quoted one, a second word after the first.
func unquoteShellWord(s string) (string, bool) {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] != '\'' {
			return "", false
		}
		j := strings.IndexByte(s[i+1:], '\'')
		if j < 0 {
			return "", false
		}
		b.WriteString(s[i+1 : i+1+j])
		i += j + 2
		if i < len(s) {
			if !strings.HasPrefix(s[i:], `\'`) {
				return "", false
			}
			b.WriteByte('\'')
			i += 2
		}
	}
	return b.String(), b.Len() > 0
}

// shellPacket frames one shell v2 packet: an id byte, a little-endian
// length, and the payload.
func shellPacket(id byte, payload string) []byte {
	b := make([]byte, 5, 5+len(payload))
	b[0] = id
	binary.LittleEndian.PutUint32(b[1:5], uint32(len(payload)))
	return append(b, payload...)
}

// hold keeps the connection open, writing nothing, until the client gives up
// or the server shuts down.
func (s *SyncServer) hold(c net.Conn) {
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_, _ = io.Copy(io.Discard, c)
	}()
	select {
	case <-gone:
	case <-s.done:
		_ = c.Close()
		<-gone
	}
}
