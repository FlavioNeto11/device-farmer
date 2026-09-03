package adbwire

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Wire framing constants.
//
// Every request on this protocol is a 4-hex-digit ASCII length followed by
// that many bytes of payload: "000Chost:version". Every response begins with
// a 4-byte status, "OKAY" or "FAIL"; a FAIL is followed by a length-prefixed
// UTF-8 reason.
const (
	prefixLen = 4
	statusLen = 4

	// maxMessage is the largest payload the length prefix can describe.
	maxMessage = 0xFFFF

	// readBufSize is generous because the same buffered reader carries
	// device-side service streams, not just tiny host replies.
	readBufSize = 32 * 1024
)

var (
	statusOKAY = [statusLen]byte{'O', 'K', 'A', 'Y'}
	statusFAIL = [statusLen]byte{'F', 'A', 'I', 'L'}
)

// conn is one socket to an ADB server, plus the framing rules for it.
//
// Most host services are one-shot: the server answers and closes. A few
// (track-devices, and any connection switched to a device transport) stay
// open indefinitely, which is why deadlines here are always supplied per call
// rather than fixed on the socket.
type conn struct {
	nc       net.Conn
	br       *bufio.Reader
	endpoint string

	// devpath is set once the socket has been switched to a device. It is
	// carried purely so a transport failure can name the physical position
	// it happened against.
	devpath string

	closeOnce sync.Once
	closeErr  error
}

// dialConn opens a socket to the ADB server. The context bounds the dial;
// fallback bounds it when the context carries no deadline of its own, so a
// wedged host cannot park a caller forever.
func dialConn(ctx context.Context, d *net.Dialer, endpoint string, fallback time.Duration) (*conn, error) {
	const op = "dial"

	dialer := d
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	dctx := ctx
	if _, ok := ctx.Deadline(); !ok && fallback > 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, fallback)
		defer cancel()
	}

	nc, err := dialer.DialContext(dctx, "tcp", endpoint)
	if err != nil {
		if ce := contextError(ctx, op); ce != nil {
			return nil, ce
		}
		return nil, newTransportError(op, endpoint, "", classify(err, KindDial), err)
	}
	if tc, ok := nc.(*net.TCPConn); ok {
		// The protocol is a stream of very small request frames to a
		// loopback or rack-local server. Nagle buys nothing here and
		// costs a round trip of latency on every call.
		_ = tc.SetNoDelay(true)
	}
	dialsTotal.Inc()
	return &conn{
		nc:       nc,
		br:       bufio.NewReaderSize(nc, readBufSize),
		endpoint: endpoint,
	}, nil
}

// arm applies the caller's deadline to the socket and wires cancellation to
// it, returning the function that undoes both.
//
// A deadline alone is not enough: a context cancelled without a deadline
// would leave a parked read parked. context.AfterFunc pushes the socket
// deadline into the past instead, which fails the in-flight syscall
// immediately, and the caller then reports the context error rather than the
// induced timeout.
func (c *conn) arm(ctx context.Context, fallback time.Duration) (disarm func()) {
	dl, ok := ctx.Deadline()
	if !ok && fallback > 0 {
		dl, ok = time.Now().Add(fallback), true
	}
	if ok {
		_ = c.nc.SetDeadline(dl)
	} else {
		_ = c.nc.SetDeadline(time.Time{})
	}
	stop := context.AfterFunc(ctx, func() {
		_ = c.nc.SetDeadline(time.Unix(1, 0))
	})
	return func() {
		stop()
		_ = c.nc.SetDeadline(time.Time{})
	}
}

// wrap converts a raw socket error into this package's typed form, checking
// the caller's context first so a deliberate cancellation is never counted as
// a host failure.
func (c *conn) wrap(ctx context.Context, op string, def ErrKind, err error) error {
	if ce := contextError(ctx, op); ce != nil {
		return ce
	}
	return newTransportError(op, c.endpoint, c.devpath, classify(err, def), err)
}

// frameErr reports a peer that is not speaking the host protocol.
func (c *conn) frameErr(op, detail string) error {
	return newTransportError(op, c.endpoint, c.devpath, KindFrame, fmt.Errorf("%s", detail))
}

// writeMessage sends one length-prefixed request.
func (c *conn) writeMessage(ctx context.Context, op, payload string, fallback time.Duration) error {
	if len(payload) > maxMessage {
		return &UsageError{
			Op:     op,
			Detail: fmt.Sprintf("request of %d bytes exceeds the %d-byte length prefix", len(payload), maxMessage),
		}
	}
	buf := make([]byte, prefixLen+len(payload))
	putHex4(buf[:prefixLen], len(payload))
	copy(buf[prefixLen:], payload)

	disarm := c.arm(ctx, fallback)
	defer disarm()

	if _, err := c.nc.Write(buf); err != nil {
		return c.wrap(ctx, op, KindWrite, err)
	}
	return nil
}

// readStatus reads the 4-byte status word. On FAIL it reads the server's
// reason and returns the most specific typed error that reason supports.
//
// target and bySerial describe what the request was aimed at, so a refusal
// can be reported as "no transport for devpath=usb:3-1.4.2" rather than as an
// anonymous protocol failure.
func (c *conn) readStatus(ctx context.Context, op, service, target string, bySerial bool, fallback time.Duration) error {
	disarm := c.arm(ctx, fallback)
	defer disarm()

	var st [statusLen]byte
	if _, err := io.ReadFull(c.br, st[:]); err != nil {
		return c.wrap(ctx, op, KindRead, err)
	}
	switch st {
	case statusOKAY:
		return nil
	case statusFAIL:
		reason := c.readFailReason()
		return failToError(op, service, target, bySerial, reason)
	default:
		return c.frameErr(op, fmt.Sprintf("expected OKAY or FAIL, got %q", printable(st[:])))
	}
}

// readFailReason reads the message that follows FAIL, tolerating the older
// servers that write the reason without a length prefix. A reason is
// diagnostic text; failing to read it must never mask the refusal itself, so
// this returns a best-effort string and no error.
// The socket deadline armed by readStatus is still in force, so this cannot
// park indefinitely.
func (c *conn) readFailReason() string {
	var hdr [prefixLen]byte
	n, err := io.ReadFull(c.br, hdr[:])
	if err != nil {
		if n == 0 {
			return "(no reason supplied)"
		}
		return strings.TrimSpace(string(hdr[:n]))
	}
	if want, ok := parseHex4(hdr[:]); ok && want <= maxMessage {
		buf := make([]byte, want)
		got, rerr := io.ReadFull(c.br, buf)
		if rerr != nil && got == 0 {
			return "(truncated reason)"
		}
		return strings.TrimSpace(string(buf[:got]))
	}
	// Unframed: the four bytes we already have start the text.
	rest, _ := io.ReadAll(io.LimitReader(c.br, maxMessage))
	return strings.TrimSpace(string(hdr[:]) + string(rest))
}

// readMessage reads one length-prefixed response payload.
func (c *conn) readMessage(ctx context.Context, op string, fallback time.Duration) (string, error) {
	disarm := c.arm(ctx, fallback)
	defer disarm()

	var hdr [prefixLen]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return "", c.wrap(ctx, op, KindRead, err)
	}
	want, ok := parseHex4(hdr[:])
	if !ok {
		return "", c.frameErr(op, fmt.Sprintf("length prefix %q is not 4 hex digits", printable(hdr[:])))
	}
	if want == 0 {
		return "", nil
	}
	buf := make([]byte, want)
	if _, err := io.ReadFull(c.br, buf); err != nil {
		return "", c.wrap(ctx, op, KindRead, err)
	}
	return string(buf), nil
}

// Close is idempotent and safe to call from another goroutine to interrupt a
// blocked read.
func (c *conn) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.nc.Close() })
	return c.closeErr
}

// ---------------------------------------------------------------------------
// Hex helpers. The protocol's lengths are ASCII hex, not binary, in both
// directions; the server emits lowercase and accepts either case.
// ---------------------------------------------------------------------------

func putHex4(dst []byte, n int) {
	const digits = "0123456789abcdef"
	dst[0] = digits[(n>>12)&0xf]
	dst[1] = digits[(n>>8)&0xf]
	dst[2] = digits[(n>>4)&0xf]
	dst[3] = digits[n&0xf]
}

func parseHex4(b []byte) (int, bool) {
	if len(b) != prefixLen {
		return 0, false
	}
	n := 0
	for _, ch := range b {
		var v int
		switch {
		case ch >= '0' && ch <= '9':
			v = int(ch - '0')
		case ch >= 'a' && ch <= 'f':
			v = int(ch-'a') + 10
		case ch >= 'A' && ch <= 'F':
			v = int(ch-'A') + 10
		default:
			return 0, false
		}
		n = n<<4 | v
	}
	return n, true
}

// printable renders unexpected wire bytes for an error message without
// letting control characters reach a log line.
func printable(b []byte) string {
	var sb strings.Builder
	for _, ch := range b {
		if ch >= 0x20 && ch < 0x7f {
			sb.WriteByte(ch)
			continue
		}
		sb.WriteString(fmt.Sprintf("\\x%02x", ch))
	}
	return sb.String()
}
