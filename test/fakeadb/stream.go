package fakeadb

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------
// Progressive, duplex device services
//
// Respond and RespondWith script a device service as ONE payload written
// after the OKAY. That is the right shape for "getprop, and here is the
// answer" and the wrong shape for everything a human holding a phone does. A
// live screen is frames arriving over minutes, and its control socket is
// bytes travelling the other way at the same time; neither is expressible as
// a blob written once. A fake that can only produce blobs makes both
// untestable, which is how a transport bug ends up being found by somebody
// watching a black rectangle instead of by a unit test.
//
// RespondStream is the sibling of RespondWith for that case. The script is a
// function that owns the socket for as long as the service lasts, and the
// protocol's OKAY is still the protocol's — so a fault injected on the
// service string refuses a duplex service exactly as it refuses a scripted
// one.
//
// SyncServer is duplex too and stays separate on purpose. It is a second
// listener speaking its own subset of the host protocol because "sync:" needs
// a whole second framing; merging the two servers is a worthwhile cleanup and
// is not this seam.
// ---------------------------------------------------------------------

// streamReadBuffer bounds what the client->device direction may accumulate
// while a handler is not reading it.
//
// The buffer exists because the two things this seam owes a handler are in
// tension. The handler must be able to read what the client wrote, so the
// bytes cannot be discarded; and the session must notice a hang-up even when
// the handler never reads at all, so SOMETHING has to be blocked on the
// socket. The resolution is that the session always reads, into here, and the
// handler reads from here.
//
// A bound rather than unbounded growth, for the reason SyncServer bounds a
// pushed file: a fake that answers a runaway client with an out-of-memory
// kill reports "the test binary died", which says nothing about what broke. A
// megabyte is far above anything a control socket carries in a test and far
// below anything that hurts. Past it the drain stops reading, which stalls
// the client rather than losing bytes it is about to assert on.
const streamReadBuffer = 1 << 20

// errServerClosed unblocks a session's drain when Close arrives while the
// buffer is full and the handler is not reading. Without it Close would wait
// on a goroutine parked on a condition only that handler could satisfy.
var errServerClosed = errors.New("fakeadb: server closed")

// StreamSession is one live device service, handed to a StreamHandler after
// the protocol's OKAY has gone out. From there the bytes are raw in both
// directions — no framing, no length, nothing this package imposes — which is
// exactly why the handler is given the socket rather than a payload slot.
type StreamSession struct {
	// Devpath is the position the transport was switched to. Service is the
	// device-side service string the client asked for, verbatim, so one
	// prefix-registered handler can still tell "localabstract:scrcpy_1a2b3c4d"
	// from "localabstract:scrcpy_deadbeef".
	Devpath, Service string

	// Reader is the client -> device direction: everything the client has
	// written since the OKAY. It returns io.EOF when the client half-closes
	// or hangs up, and a transport error when the socket broke.
	io.Reader

	// Writer is the device -> client direction. Writes go straight to the
	// socket, unbuffered and unframed, so a handler that writes in chunks
	// produces chunks on the wire — which is the whole point of this type.
	io.Writer

	// Done is closed when the client hangs up or the server closes. A
	// handler that parks — a live screen with nothing new to send, a spawned
	// server process that outlives its first output — must select on it, or
	// Close will wait for a goroutine that is waiting for nothing.
	Done <-chan struct{}

	srv     *Server
	conn    net.Conn
	rec     *Request
	wrote   atomic.Int64
	severed sync.Once
}

// Sever cuts the stream mid-flight with a TCP RST, so the client reads
// ECONNRESET rather than a clean EOF.
//
// The distinction is the one internal/adbwire's taxonomy is built around: a
// reset is a *TransportError with KindPeerClosed, a statement about a socket,
// while a clean close at the end of a service is an ordinary end of stream. A
// test proving that a dying screen does not end a lease needs the former, and
// a fake that could only close politely could not produce it.
//
// It is safe to call from any goroutine, including one other than the
// handler's, and calling it twice does nothing the second time.
func (s *StreamSession) Sever() { s.cut("RESET") }

// fail ends the connection after a handler returned an error, recording the
// message against the request first. An empty reply leaves the record alone.
//
// IT CLOSES POLITELY, AND DOES NOT RESET, and the difference is the whole
// reason this is separate from [StreamSession.Sever].
//
// A TCP reset discards what the sender has written and the peer has not yet
// read. That queue holds the protocol's own OKAY — written by deviceStream
// immediately before the handler ran — and anything the handler wrote before it
// failed. Resetting here raced all of it: about one run in twenty the client
// could not read even the four status bytes, and the test that failed was
// whichever the kernel chose. There is no way to ask TCP to deliver these bytes
// and THEN reset; that is not a promise the protocol makes.
//
// So the promise this fake makes instead is the one it can keep: what a handler
// wrote, a client receives. A handler that fails mid-stream produces a
// TRUNCATED stream, and truncation is detectable by the thing being truncated —
// a video packet cut short is short, and internal/scrcpy says so. Whether the
// end arrives as a reset or as an EOF is TCP timing, and a test asserting on it
// is asserting on the weather.
//
// [StreamSession.Sever] still resets, because the STF #663 shape is a real
// thing a fake must be able to produce. The difference is that Sever is called
// BY a handler that knows what it has done, and the caller owns the ordering:
// see TestASeveredStreamIsATransportErrorNotARefusal, which writes a header,
// waits for the client to read it, and only then severs.
func (s *StreamSession) fail(reply string) {
	s.severed.Do(func() {
		if s.rec != nil && reply != "" {
			s.srv.note(s.rec, func(r *Request) { r.Reply = reply })
		}
		_ = s.conn.Close()
	})
}

// cut resets the connection once, recording reply against the request first.
func (s *StreamSession) cut(reply string) {
	s.severed.Do(func() {
		if s.rec != nil && reply != "" {
			s.srv.note(s.rec, func(r *Request) { r.Reply = reply })
		}
		sever(s.conn)
	})
}

// Wrote reports how many bytes the handler has written to the client so far.
// It is the cheap assertion for a progressive stream: a test can watch it
// climb without decoding whatever the handler is producing.
func (s *StreamSession) Wrote() int64 { return s.wrote.Load() }

// StreamHandler serves one device service. Returning nil ends the service
// with a clean close, which the client reads as EOF. Returning an error
// severs the connection and records the message on the request, because a
// handler that failed halfway did not finish its stream and a client told
// otherwise would assert on a truncation as though it were the whole answer.
type StreamHandler func(*StreamSession) error

type streamScript struct {
	devpath string // "" matches any device
	prefix  string // service prefix, e.g. "localabstract:scrcpy_"
	h       StreamHandler
}

// RespondStream scripts a device service as a duplex, progressive
// conversation. Registration mirrors RespondWith: devpath "" matches any
// device, servicePrefix "" matches any service, and the most recently
// registered match wins. The handler is called outside the server's lock, so
// it may keep its own state under its own lock and may call back into the
// server — which is how the scrcpy fixture records a spawn and then answers
// the sockets that spawn published.
//
// Stream scripts are consulted before Respond/RespondWith scripts, so a
// device carrying both answers the duplex service duplex and everything else
// with its scripted blob.
//
//	srv.RespondStream(devpath, "localabstract:scrcpy_", func(s *fakeadb.StreamSession) error {
//		for _, frame := range frames {
//			if _, err := s.Write(frame); err != nil {
//				return nil // the viewer closed the tab; that is not a failure
//			}
//		}
//		<-s.Done
//		return nil
//	})
func (s *Server) RespondStream(devpath, servicePrefix string, h StreamHandler) {
	if h == nil {
		panic("fakeadb: RespondStream needs a handler")
	}
	s.mu.Lock()
	s.streams = append(s.streams, streamScript{devpath: devpath, prefix: servicePrefix, h: h})
	s.mu.Unlock()
}

// matchStream picks the most recently registered stream script that applies.
// The lock is released before the handler runs, for the same reason
// matchScript releases it: the handler is free to consult or change the
// server.
func (s *Server) matchStream(d Device, service string) (StreamHandler, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.streams) - 1; i >= 0; i-- {
		sc := s.streams[i]
		if sc.devpath != "" && sc.devpath != d.Devpath {
			continue
		}
		if sc.prefix != "" && !strings.HasPrefix(service, sc.prefix) {
			continue
		}
		return sc.h, true
	}
	return nil, false
}

// runStream hands the raw connection to a stream handler and does the
// bookkeeping the handler must not be trusted to do.
//
// It runs on the connection goroutine, which serve already counted in s.wg,
// so Close waits for the handler to return. The two goroutines it starts are
// counted too — for the reason watchPeer spells out at length: a goroutine
// still holding a socket after Close returns does not fail the test that
// leaked it, it flakes the NEXT test in the package.
func (s *Server) runStream(c net.Conn, br *bufio.Reader, rec *Request, devpath, svc string, h StreamHandler) {
	s.mu.Lock()
	s.stats.Streams++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.stats.Streams--
		s.mu.Unlock()
	}()

	sess := &StreamSession{Devpath: devpath, Service: svc, srv: s, conn: c, rec: rec}
	buf := newStreamBuf(streamReadBuffer)
	sess.Reader = buf
	sess.Writer = &streamWriter{sess: sess, c: c}

	// The drain is this seam's watchPeer. It cannot BE watchPeer, which
	// discards what it reads: the whole point of a duplex session is that the
	// handler gets those bytes. What it keeps from watchPeer is the property
	// that matters — something is always blocked on the socket, so a hang-up
	// is noticed even by a handler that only ever writes. A screen stream
	// that only discovered the viewer had gone on its next write would park
	// forever on a quiet farm, exactly the leak watchPeer was added to stop.
	gone := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(gone)
		_, err := io.Copy(buf, br)
		buf.closeWith(err)
	}()

	// Done is its own goroutine rather than gone under another name, so that
	// "closed when the server closes OR the peer hangs up" is structurally
	// true instead of true because Close happens to sever every connection.
	// It also carries the one unblock the buffer cannot do for itself.
	done := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		select {
		case <-gone:
		case <-s.done:
			buf.closeWith(errServerClosed)
		}
	}()
	sess.Done = done

	if err := h(sess); err != nil {
		// RECORDED BEFORE THE SOCKET DIES, and the order is not cosmetic.
		// The note is filed BEFORE the connection ends, so a test that reads
		// the request log the moment its own read finishes cannot race a
		// message filed afterwards — and would have raced it in the direction
		// that hides it, leaving somebody to debug a fixture that had said
		// exactly what was wrong. The empty reply on fail keeps this line from
		// being overwritten.
		s.note(rec, func(r *Request) { r.Reply = "ERROR: " + err.Error() })
		sess.fail("")
	}
}

// streamWriter is the device -> client half, counting what it delivers.
type streamWriter struct {
	sess *StreamSession
	c    net.Conn
}

func (w *streamWriter) Write(p []byte) (int, error) {
	n, err := w.c.Write(p)
	if n > 0 {
		w.sess.wrote.Add(int64(n))
	}
	return n, err
}

// streamBuf is the session's receive buffer: written by the drain, read by
// the handler, bounded at max bytes of unread input.
type streamBuf struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  []byte
	off  int
	err  error
	max  int
}

func newStreamBuf(max int) *streamBuf {
	b := &streamBuf{max: max}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *streamBuf) pending() int { return len(b.buf) - b.off }

// Write blocks while the buffer is full, which stalls the client rather than
// dropping what it sent. It fails once the source has been closed.
func (b *streamBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := 0
	for len(p) > 0 {
		for b.pending() >= b.max && b.err == nil {
			b.cond.Wait()
		}
		if b.err != nil {
			return total, b.err
		}
		n := b.max - b.pending()
		if n > len(p) {
			n = len(p)
		}
		b.buf = append(b.buf, p[:n]...)
		p = p[n:]
		total += n
		b.cond.Broadcast()
	}
	return total, nil
}

// Read blocks until there is something to read or the source is finished. The
// buffered bytes are always drained before the error is reported, so a client
// that wrote and then hung up hands the handler everything it wrote.
func (b *streamBuf) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.pending() == 0 && b.err == nil {
		b.cond.Wait()
	}
	if b.pending() == 0 {
		return 0, b.err
	}
	n := copy(p, b.buf[b.off:])
	b.off += n
	if b.off == len(b.buf) {
		// Fully drained: rewind onto the same array rather than letting the
		// offset walk it forever and make every append allocate.
		b.buf = b.buf[:0]
		b.off = 0
	}
	b.cond.Broadcast()
	return n, nil
}

// closeWith records why there will be no more input. A nil error is the
// ordinary end of a stream, which readers must see as io.EOF and not as
// success with nothing to show for it.
func (b *streamBuf) closeWith(err error) {
	if err == nil {
		err = io.EOF
	}
	b.mu.Lock()
	if b.err == nil {
		b.err = err
	}
	b.mu.Unlock()
	b.cond.Broadcast()
}
