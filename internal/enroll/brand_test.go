package enroll

// What these tests protect: the one thing this package promises never to do is
// overwrite another device's brand, because two farm uids on one phone means
// two rows in farm.devices believe they are it, and merging them loses which
// measurements belonged to which handset. So a brand is written only onto a
// device that carries none, the write is believed only after it is read back,
// and a different uid — however it is discovered — is a refusal, never a
// correction.

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

const (
	uidA = "df-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	uidB = "df-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	uidC = "df-cccccccccccccccccccccccccccccccc"
)

var (
	readService  = adbwire.ShellService(brandReadCmd)
	probeService = adbwire.ShellService(probeCommand)
	// writePrefix is the part of brandWriteCmd before the uid, which is all a
	// script registered before the uid is known can match on.
	writePrefix = adbwire.ShellService("if [ -s " + BrandPath + " ] && [")

	uidInCommand = regexp.MustCompile(`df-[0-9a-f]{32}`)
)

// ---------------------------------------------------------------------------
// A rack of phones that keep their brand file
// ---------------------------------------------------------------------------

// rack is a fakeadb server whose phones remember what was written to
// BrandPath.
//
// fakeadb scripts are static, and the uid a device gets branded with is minted
// by farm.resolve_device at the moment of adoption, so no script written before
// a test runs can know it. The rack therefore sits between the enroller and
// the real client: every command still crosses the wire and the real protocol,
// and when a brand write comes back with status 0 the rack re-scripts that
// device's brand read and its probe to say so — which is exactly what the
// phone's own filesystem would do.
type rack struct {
	t   *testing.T
	srv *fakeadb.Server
	*adbwire.Client

	mu    sync.Mutex
	props map[string]map[string]string
	brand map[string]string
}

func newRack(t *testing.T, fixtures ...fakeadb.Fixture) *rack {
	t.Helper()
	srv := fakeadb.Start(t, fixtures...)
	return &rack{
		t:      t,
		srv:    srv,
		Client: dial(srv),
		props:  map[string]map[string]string{},
		brand:  map[string]string{},
	}
}

// phone adds an unbranded, healthy device and scripts what it answers.
func (r *rack) phone(devpath, serial string, props map[string]string) {
	r.t.Helper()
	r.srv.Add(fakeadb.Device{Serial: serial, Devpath: devpath, Model: props[propModel],
		Product: props[propName], Codename: props[propDevice]})
	r.script(devpath, props)
}

// script gives an already-present device its getprop answers and an empty
// brand path. The write command is scripted to succeed; what it wrote is what
// the rack then answers.
func (r *rack) script(devpath string, props map[string]string) {
	r.t.Helper()
	r.mu.Lock()
	r.props[devpath] = props
	r.mu.Unlock()
	r.srv.Respond(devpath, writePrefix, shellV2(r.t, "", 0))
	r.setBrand(devpath, "")
}

// setBrand is the phone's filesystem: what the brand path holds from now on.
func (r *rack) setBrand(devpath, uid string) {
	r.t.Helper()
	r.mu.Lock()
	r.brand[devpath] = uid
	props := r.props[devpath]
	r.mu.Unlock()
	if uid == "" {
		r.srv.Respond(devpath, readService, shellV2(r.t, "", brandAbsent))
	} else {
		r.srv.Respond(devpath, readService, shellV2(r.t, uid+"\n", 0))
	}
	r.srv.Respond(devpath, probeService, shellV2(r.t, probeAnswer(uid, props), 0))
}

// brandOf reports what the phone at devpath currently carries.
func (r *rack) brandOf(devpath string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.brand[devpath]
}

// Shell forwards to the wire and watches for a successful brand write.
func (r *rack) Shell(ctx context.Context, devpath, command string) (*adbwire.ShellResult, error) {
	res, err := r.Client.Shell(ctx, devpath, command)
	if err == nil && res != nil && res.Exited && res.ExitCode == 0 && strings.Contains(command, brandTmp) {
		if uid := uidInCommand.FindString(command); uid != "" {
			r.setBrand(devpath, uid)
		}
	}
	return res, err
}

// shellServices lists, in order, the shell commands one device received,
// reduced to which of the three commands each was.
func shellServices(srv *fakeadb.Server, devpath string) []string {
	var out []string
	for _, req := range srv.RequestsTo(devpath) {
		switch {
		case req.Service == readService:
			out = append(out, "read")
		case strings.HasPrefix(req.Service, writePrefix):
			out = append(out, "write")
		case req.Service == probeService:
			out = append(out, "probe")
		case strings.HasPrefix(req.Service, "shell"):
			out = append(out, "other")
		}
	}
	return out
}

// logRecorder captures slog records so a test can assert on the severity of
// what was said: a rebrand is the only record that two device rows once
// pointed at one phone, and it must be impossible to filter out.
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logRecorder) WithGroup(string) slog.Handler      { return h }

// lines renders every record at or above level with its attributes.
func (h *logRecorder) lines(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		if r.Level < level {
			continue
		}
		var b strings.Builder
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(" " + a.Key + "=" + a.Value.String())
			return true
		})
		out = append(out, b.String())
	}
	return out
}

func newBrander(sh Shell, timeout time.Duration) *Brander {
	return NewBrander(sh, timeout, quietLogger())
}

// ---------------------------------------------------------------------------
// Read
// ---------------------------------------------------------------------------

// TestReadTellsAbsentFromUnreadable: "no file" and "the file could not be
// read" produce identical output, and the caller turns the first into a
// write. Only the exit status tells them apart.
//
// Falsify: delete the `case res.ExitCode == brandAbsent` arm in Read — an
// unbranded phone then reads as an error and is never branded.
func TestReadTellsAbsentFromUnreadable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		devpath string
		payload string
		wantUID string
		wantErr []string // substrings the error must carry; nil means no error
	}{
		{"absent", "usb:3-1.1", shellV2(t, "", brandAbsent), "", nil},
		{"present", "usb:3-1.2", shellV2(t, uidA+"\n", 0), uidA, nil},
		{"present but empty is a torn write, replaceable", "usb:3-1.3", shellV2(t, "\n", 0), "", nil},
		{"present but unreadable", "usb:3-1.4",
			shellV2Stderr(t, "", "cat: "+BrandPath+": Permission denied", 1), "",
			[]string{BrandPath, "usb:3-1.4", "status 1", "Permission denied"}},
		{"a stranger's file", "usb:3-1.5", shellV2(t, "hello\n", 0), "",
			[]string{"not a farm uid", "usb:3-1.5"}},
		{"a large file", "usb:3-1.6", shellV2(t, strings.Repeat("x", maxBrandBytes+1), 0), "",
			[]string{"cannot be a farm uid"}},
	}

	srv := fakeadb.Start(t)
	for _, c := range cases {
		srv.Add(fakeadb.Device{Serial: "SER-" + c.devpath, Devpath: c.devpath})
		srv.Respond(c.devpath, readService, c.payload)
	}
	b := newBrander(dial(srv), 2*time.Second)

	for _, c := range cases {
		uid, err := b.Read(t.Context(), c.devpath)
		if c.wantErr == nil {
			if err != nil || uid != c.wantUID {
				t.Errorf("%s: Read = (%q, %v), want (%q, nil)", c.name, uid, err, c.wantUID)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: Read = (%q, nil), want an error", c.name, uid)
			continue
		}
		for _, s := range c.wantErr {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("%s: error %q does not mention %q", c.name, err, s)
			}
		}
	}

	// A device that never answers is an error naming the position, so the
	// caller never mistakes silence for an unbranded phone.
	const silent = "usb:3-1.7"
	srv.Add(fakeadb.Device{Serial: "SER-silent", Devpath: silent})
	srv.Inject(fakeadb.Fault{Match: "shell", Devpath: silent, Kind: fakeadb.FaultHang})
	quick := newBrander(dial(srv), 300*time.Millisecond)
	if uid, err := quick.Read(t.Context(), silent); err == nil || !strings.Contains(err.Error(), silent) {
		t.Errorf("a hung read returned (%q, %v), want an error naming %s", uid, err, silent)
	}
	assertNoSerialAddressing(t, srv, "SER-silent", "SER-usb:3-1.1")
}

// ---------------------------------------------------------------------------
// Brand
// ---------------------------------------------------------------------------

// TestBrandWritesVerifiesAndIsIdempotent: an unbranded phone gets exactly
// read, write, read-back; a phone already carrying the uid gets one read and
// no write.
//
// Falsify: in Brand, return BrandWritten from the `have == uid` case — the
// second call then claims a write it did not make.
func TestBrandWritesVerifiesAndIsIdempotent(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4.1"
	r := newRack(t)
	r.phone(devpath, pixelSerial, pixelProps(pixelSerial))
	b := newBrander(r, 2*time.Second)

	outcome, err := b.Brand(t.Context(), devpath, uidA)
	if err != nil || outcome != BrandWritten {
		t.Fatalf("Brand = (%s, %v), want written", outcome, err)
	}
	if got := r.brandOf(devpath); got != uidA {
		t.Fatalf("the phone carries %q after branding", got)
	}
	if got := strings.Join(shellServices(r.srv, devpath), ","); got != "read,write,read" {
		t.Fatalf("commands = %s, want read,write,read", got)
	}

	outcome, err = b.Brand(t.Context(), devpath, uidA)
	if err != nil || outcome != BrandAlready {
		t.Fatalf("second Brand = (%s, %v), want already", outcome, err)
	}
	if got := strings.Join(shellServices(r.srv, devpath), ","); got != "read,write,read,read" {
		t.Fatalf("commands = %s, want one more read and no write", got)
	}
	assertNoSerialAddressing(t, r.srv, pixelSerial)
}

// TestBrandDoesNotBelieveAnUnverifiedWrite: a write the device reported as
// successful but did not keep is a failure, not a brand. An unbranded device
// we believe is branded is a device that gets adopted twice.
//
// Falsify: in write, return BrandWritten after the status check without the
// read-back.
func TestBrandDoesNotBelieveAnUnverifiedWrite(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: pixelSerial, Devpath: devpath}))
	// Static scripts: the write says 0, the file is still absent afterwards.
	srv.Respond(devpath, readService, shellV2(t, "", brandAbsent))
	srv.Respond(devpath, writePrefix, shellV2(t, "", 0))
	b := newBrander(dial(srv), 2*time.Second)

	outcome, err := b.Brand(t.Context(), devpath, uidA)
	if outcome != BrandFailed || err == nil {
		t.Fatalf("Brand = (%s, %v), want failed with an error", outcome, err)
	}
	if !strings.Contains(err.Error(), "did not keep it") || !strings.Contains(err.Error(), devpath) {
		t.Errorf("error = %v, want it to say the device did not keep the brand at %s", err, devpath)
	}
	if got := strings.Join(shellServices(srv, devpath), ","); got != "read,write,read" {
		t.Errorf("commands = %s, want the read-back to have happened", got)
	}
}

// TestBrandRefusesToOverwriteAnotherUID: a different uid on the device is a
// *ConflictError naming both, and no write reaches the phone.
//
// Falsify: delete the `case have != ""` arm in Brand.
func TestBrandRefusesToOverwriteAnotherUID(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: pixelSerial, Devpath: devpath}))
	srv.Respond(devpath, readService, shellV2(t, uidB+"\n", 0))
	srv.Respond(devpath, writePrefix, shellV2(t, "", 0))
	b := newBrander(dial(srv), 2*time.Second)

	outcome, err := b.Brand(t.Context(), devpath, uidA)
	if outcome != BrandConflict {
		t.Fatalf("Brand = (%s, %v), want conflict", outcome, err)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Have != uidB || ce.Want != uidA || ce.Devpath != devpath {
		t.Fatalf("error = %v, want a *ConflictError{%s, %s, %s}", err, devpath, uidB, uidA)
	}
	if !strings.Contains(err.Error(), uidA) || !strings.Contains(err.Error(), uidB) {
		t.Errorf("the refusal does not name both uids: %v", err)
	}
	if got := strings.Join(shellServices(srv, devpath), ","); got != "read" {
		t.Errorf("commands = %s, want only the read", got)
	}
}

// TestBrandHonoursTheDeviceSideRefusal: the write command re-checks on the
// device what the read said a round trip earlier. When the device refuses,
// nothing was overwritten, and the operator is told so even when the brand
// cannot then be read back.
//
// Falsify: delete the `res.ExitCode == brandOccupied` branch in write — the
// refusal is then reported as a failed write rather than a conflict.
func TestBrandHonoursTheDeviceSideRefusal(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: pixelSerial, Devpath: devpath}))
	// The file reads as empty — a torn write, which Read reports as
	// unbranded — but at the instant of the write the device finds another
	// brand in place and exits brandOccupied.
	srv.Respond(devpath, readService, shellV2(t, "\n", 0))
	srv.Respond(devpath, writePrefix, shellV2(t, "", brandOccupied))
	b := newBrander(dial(srv), 2*time.Second)

	outcome, err := b.Brand(t.Context(), devpath, uidA)
	if outcome != BrandConflict {
		t.Fatalf("Brand = (%s, %v), want conflict", outcome, err)
	}
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Have != "" || ce.Want != uidA {
		t.Fatalf("error = %v, want a *ConflictError with an unknown Have", err)
	}
	if !strings.Contains(err.Error(), "would not say which") || !strings.Contains(err.Error(), "nothing was overwritten") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}

// TestBrandRefusesANonUID: a value that cannot be a farm uid never reaches a
// shell command, whatever the device carries.
func TestBrandRefusesANonUID(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4.1"
	srv := fakeadb.Start(t, fakeadb.WithDevices(fakeadb.Device{Serial: pixelSerial, Devpath: devpath}))
	b := newBrander(dial(srv), 2*time.Second)

	for _, bad := range []string{"", "df-not-a-uid", "DF-" + strings.Repeat("a", 32), uidA + "'; reboot; '"} {
		if outcome, err := b.Brand(t.Context(), devpath, bad); outcome != BrandFailed || err == nil {
			t.Errorf("Brand(%q) = (%s, %v), want failed", bad, outcome, err)
		}
	}
	if n := len(srv.Requests()); n != 0 {
		t.Errorf("%d requests reached the server for uids that are not uids", n)
	}
}

// ---------------------------------------------------------------------------
// Rebrand
// ---------------------------------------------------------------------------

// TestRebrandDemandsTheExpectedPreviousUIDAndAReason: overwriting a brand is a
// decision a person signs. The caller must name the uid it expects to find,
// give a reason, and the abandonment is logged at ERROR with both uids.
//
// Falsify: delete the `have != prev` check in Rebrand — a rebrand aimed at
// the wrong phone then goes through.
func TestRebrandDemandsTheExpectedPreviousUIDAndAReason(t *testing.T) {
	t.Parallel()

	const devpath = "usb:3-1.4.1"
	r := newRack(t)
	r.phone(devpath, pixelSerial, pixelProps(pixelSerial))
	r.setBrand(devpath, uidA)
	rec := &logRecorder{}
	b := NewBrander(r, 2*time.Second, slog.New(rec))

	// Aimed at a phone that turns out to carry something else.
	outcome, err := b.Rebrand(t.Context(), devpath, uidB, uidC, "row df-cc… is the wrong device")
	var ce *ConflictError
	if outcome != BrandConflict || !errors.As(err, &ce) || ce.Have != uidA || ce.Want != uidB {
		t.Fatalf("Rebrand with the wrong prev = (%s, %v), want conflict naming %s", outcome, err, uidA)
	}
	if got := r.brandOf(devpath); got != uidA {
		t.Fatalf("the phone now carries %q; a mis-aimed rebrand wrote", got)
	}

	// Without a reason, and with a value that is not a uid: refused before
	// any command is sent.
	before := len(r.srv.RequestsTo(devpath))
	if outcome, err := b.Rebrand(t.Context(), devpath, uidB, uidA, ""); outcome != BrandFailed || err == nil {
		t.Errorf("Rebrand without a reason = (%s, %v)", outcome, err)
	}
	if outcome, err := b.Rebrand(t.Context(), devpath, "df-nope", uidA, "typo"); outcome != BrandFailed || err == nil {
		t.Errorf("Rebrand with a non-uid = (%s, %v)", outcome, err)
	}
	if after := len(r.srv.RequestsTo(devpath)); after != before {
		t.Errorf("%d commands were sent for rebrands that were refused up front", after-before)
	}

	// The signed case.
	outcome, err = b.Rebrand(t.Context(), devpath, uidB, uidA, "row df-aa… was retired by alice, ticket 4211")
	if err != nil || outcome != BrandWritten {
		t.Fatalf("Rebrand = (%s, %v), want written", outcome, err)
	}
	if got := r.brandOf(devpath); got != uidB {
		t.Fatalf("the phone carries %q after the rebrand", got)
	}
	errs := rec.lines(slog.LevelError)
	if len(errs) != 1 || !strings.Contains(errs[0], uidA) || !strings.Contains(errs[0], uidB) ||
		!strings.Contains(errs[0], "ticket 4211") {
		t.Fatalf("the abandonment was not logged at ERROR with both uids and the reason: %q", errs)
	}

	// Rebranding to what is already there is a no-op, not a second abandonment.
	if outcome, err := b.Rebrand(t.Context(), devpath, uidB, uidB, "again"); err != nil || outcome != BrandAlready {
		t.Errorf("repeat Rebrand = (%s, %v), want already", outcome, err)
	}
	if n := len(rec.lines(slog.LevelError)); n != 1 {
		t.Errorf("%d ERROR lines after a no-op rebrand, want the original 1", n)
	}
	assertNoSerialAddressing(t, r.srv, pixelSerial)
}
