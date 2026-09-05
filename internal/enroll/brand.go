package enroll

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/flaviopadilha/device-farmer/internal/adbwire"
)

// Where the brand lives on the device.
//
// /data/local/tmp is the one directory an ADB shell can write on a stock,
// non-rooted phone. It is inside /data, so a factory reset erases it — which
// is precisely the case the hardware fingerprint exists to cover, and the
// reason the brand is the strongest signal rather than the only one.
const (
	BrandDir  = "/data/local/tmp/.farm"
	BrandPath = BrandDir + "/uid"

	// brandTmp is written first and renamed over the real path. A phone that
	// loses power halfway through a write must end up with either the old
	// brand or the new one, never with half a uid — a truncated brand matches
	// no device and would send a known phone back down the ladder.
	brandTmp = BrandDir + "/uid.new"
)

// uidRe is farm.devices' CHECK constraint, restated in Go.
//
// It is checked before a uid is ever put into a shell command. A value that
// cannot be a farm uid has no business being written to a device, and the
// pattern also happens to make the value shell-inert — which matters, because
// the only other place a uid could come from is a file on a phone.
var uidRe = regexp.MustCompile(`^df-[0-9a-f]{32}$`)

// IsFarmUID reports whether s has the shape of a farm uid. It is the one
// check a caller outside this package may make before handing a uid to a
// Brander, so a request body cannot name a "uid" the schema would refuse.
func IsFarmUID(s string) bool { return uidRe.MatchString(s) }

// Exit statuses the brand commands use to say something the output cannot.
const (
	// brandAbsent means there is no brand file. It is deliberately distinct
	// from every other status, because "no file" and "the file could not be
	// read" are the difference between branding a new phone and overwriting
	// another device's identity, and `cat 2>/dev/null` gives both of them
	// the same empty output and the same non-zero status.
	brandAbsent = 44

	// brandOccupied means the device found a different brand already in
	// place at the moment of the write.
	brandOccupied = 17
)

// maxBrandBytes bounds what will be read back from the brand path. A farm uid
// is thirty-five bytes; the slack is there so an operator can be shown what a
// stranger put in that file instead, and the cap is there so a device with a
// large file at that path cannot make this loop carry it around.
const maxBrandBytes = 4096

// brandReadCmd prints the brand file, and says by its exit status which of the
// three answers this is: here it is, there is none, or it could not be read.
var brandReadCmd = "if [ -e " + BrandPath + " ]; then cat " + BrandPath +
	"; else exit " + strconv.Itoa(brandAbsent) + "; fi"

// brandWriteCmd re-checks on the device what the caller checked one round trip
// ago, and refuses there rather than here.
//
// The check in [Brander.Brand] is made against a READ, and a read that failed
// for any reason other than the file being absent used to look exactly like an
// unbranded device. Without this line, the one thing this package promises
// never to do — overwrite another device's identity and fuse two devices'
// histories — would rest on a `cat` having succeeded a second earlier.
func brandWriteCmd(uid string) string {
	return brandGuard(uid) + brandInstall(uid)
}

// brandReplaceCmd is the write a rebrand makes. Its device-side check admits
// exactly one brand besides uid: prev, the identity the human authorised the
// overwrite against. A brand that changed between the read and this write is
// refused on the device, as any other brand is for a first write, because the
// authorisation was for a particular identity and not for whatever happens to
// be in the file when the command lands.
//
// The guard in brandWriteCmd cannot serve here: the file a rebrand exists to
// replace holds, by definition, a different uid, and that guard refuses every
// different uid.
func brandReplaceCmd(uid, prev string) string {
	return brandGuard(uid, prev) + brandInstall(uid)
}

// brandGuard exits brandOccupied unless the brand file is absent, empty, or
// holds one of the admitted uids. Every admitted value matches uidRe by the
// time it gets here, so it contains nothing a shell reads as syntax; the
// single quotes are still there, because the safety of this line should not
// rest on a check in another function.
func brandGuard(admitted ...string) string {
	cond := "[ -s " + BrandPath + " ]"
	for _, uid := range admitted {
		cond += " && [ \"$(cat " + BrandPath + " 2>/dev/null)\" != '" + uid + "' ]"
	}
	return "if " + cond + "; then exit " + strconv.Itoa(brandOccupied) + "; fi; "
}

// brandInstall writes uid atomically: to a temporary file, then renamed over
// the brand path.
func brandInstall(uid string) string {
	return "mkdir -p " + BrandDir +
		" && printf '%s' '" + uid + "' > " + brandTmp +
		" && chmod 600 " + brandTmp +
		" && mv -f " + brandTmp + " " + BrandPath +
		// The brand's whole purpose is to still be there after the phone
		// reboots — including after the abrupt reboots a recovery ladder
		// causes — so it is flushed rather than left in page cache. A device
		// whose toybox has no usable sync must not fail the write that
		// already succeeded, hence the fallbacks.
		" && { sync " + BrandPath + " 2>/dev/null || sync 2>/dev/null || true; }"
}

// BrandOutcome is what a branding attempt did.
type BrandOutcome string

const (
	// BrandWritten: the device now carries the uid and did not before.
	BrandWritten BrandOutcome = "written"
	// BrandAlready: the device already carried exactly this uid. Branding is
	// idempotent, so this is a success and costs no write.
	BrandAlready BrandOutcome = "already"
	// BrandConflict: the device carries a DIFFERENT uid. Nothing was written.
	BrandConflict BrandOutcome = "conflict"
	// BrandFailed: the write or its verification did not go through.
	BrandFailed BrandOutcome = "failed"
)

// ConflictError is returned when a device already carries a different brand.
//
// This is refused rather than resolved, and it is the loudest thing this
// package can say. Two farm uids on one device means two rows in farm.devices
// believe they are this phone; overwriting one of them would merge two
// devices' histories — their failure scores, their quarantines, their lease
// records — into a single row, and there would be no way afterwards to tell
// which measurements belonged to which phone.
type ConflictError struct {
	Devpath string
	// Have is the uid already on the device, Want the one we were asked to
	// write.
	Have string
	Want string
}

func (e *ConflictError) Error() string {
	if e.Have == "" {
		// The device refused the write itself and its brand could not then be
		// read back. Which uid is on it is unknown; that it is not ours is
		// not.
		return fmt.Sprintf("enroll: device at %s carries a brand that is not %s and would not say which; "+
			"nothing was overwritten — read %s on that device to find out whose it is",
			e.Devpath, e.Want, BrandPath)
	}
	return fmt.Sprintf("enroll: device at %s is already branded %s, refusing to overwrite it with %s; "+
		"one of those two device rows is wrong and a human must retire it",
		e.Devpath, e.Have, e.Want)
}

// Brander reads and writes the farm uid on a device.
type Brander struct {
	sh      Shell
	timeout time.Duration
	log     *slog.Logger
}

// NewBrander returns a Brander that talks to devices through sh.
func NewBrander(sh Shell, timeout time.Duration, log *slog.Logger) *Brander {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	if log == nil {
		log = slog.Default()
	}
	return &Brander{sh: sh, timeout: timeout, log: log}
}

// Read returns the uid branded on the device at devpath, or "" when it carries
// none.
//
// A missing file is not an error: an unbranded device is the normal state of a
// phone that was just plugged in. A file that exists and could not be read IS
// an error, and the two are told apart by the command's exit status rather
// than by its output, because they produce identical output and the caller
// turns "no brand" into a write. Content that is not a farm uid is an error
// too, because something other than this system wrote to that path and
// trusting it would mean adopting a stranger's idea of who this device is.
func (b *Brander) Read(ctx context.Context, devpath string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	res, err := b.sh.Shell(cctx, devpath, brandReadCmd)
	if err != nil {
		return "", fmt.Errorf("enroll: read the brand at %s: %w", devpath, err)
	}
	if res == nil {
		return "", fmt.Errorf("enroll: read the brand at %s: the shell returned neither output nor an error",
			devpath)
	}

	// res.Exited is false only when the stream ended without an exit frame, on
	// which nothing can be concluded from a status. There the old rule stands:
	// no output means no brand.
	if res.Exited {
		switch {
		case res.ExitCode == brandAbsent:
			return "", nil
		case res.ExitCode != 0:
			return "", fmt.Errorf(
				"enroll: %s exists on the device at %s but could not be read (status %d)%s; "+
					"nothing will be branded onto it until that read succeeds — check the device's "+
					"storage and whether it is still authorized",
				BrandPath, devpath, res.ExitCode, stderrHint(res))
		}
	}

	if len(res.Stdout) > maxBrandBytes {
		// "returned", not "holds": the shell's output is itself capped, so all
		// that is known is that the file is at least this big. Either way it is
		// not a thirty-five byte uid.
		return "", fmt.Errorf(
			"enroll: %s on the device at %s returned %d bytes and cannot be a farm uid; "+
				"remove that file so this device can be branded",
			BrandPath, devpath, len(res.Stdout))
	}

	uid := strings.TrimSpace(string(res.Stdout))
	if uid == "" {
		// The file is there and empty, which names no device. Reported as
		// unbranded on purpose, so a write that lost power halfway can be
		// replaced instead of blocking this device forever.
		return "", nil
	}
	if !uidRe.MatchString(uid) {
		return "", fmt.Errorf(
			"enroll: %s on the device at %s holds %q, which is not a farm uid; "+
				"something other than this farm wrote that file — remove it so this device can be branded",
			BrandPath, devpath, truncate(uid, 64))
	}
	return uid, nil
}

// stderrHint appends a device's own complaint to an error, when it made one.
func stderrHint(res *adbwire.ShellResult) string {
	msg := truncate(strings.TrimSpace(string(res.Stderr)), 200)
	if msg == "" {
		return ""
	}
	return ": " + msg
}

// Brand writes uid onto the device at devpath so that the strongest resolution
// signal exists the next time this phone is seen.
//
// It is idempotent: a device already carrying this uid is left alone. It never
// overwrites a different uid — that returns a *ConflictError and writes
// nothing. Rebrand is the only way past that, and it says so out loud.
//
// The write is verified by reading the file back over a fresh connection. A
// write whose result is not read back is a write we only believe happened, and
// an unbranded device that we think is branded is a device that gets adopted
// twice.
func (b *Brander) Brand(ctx context.Context, devpath, uid string) (BrandOutcome, error) {
	if !uidRe.MatchString(uid) {
		return BrandFailed, fmt.Errorf("enroll: %q is not a farm uid and will not be written to %s",
			truncate(uid, 64), devpath)
	}

	have, err := b.Read(ctx, devpath)
	if err != nil {
		return BrandFailed, err
	}
	switch {
	case have == uid:
		return BrandAlready, nil
	case have != "":
		return BrandConflict, &ConflictError{Devpath: devpath, Have: have, Want: uid}
	}
	return b.write(ctx, devpath, uid, brandWriteCmd(uid))
}

// Rebrand replaces the uid on a device that is already branded.
//
// It exists for one situation: a human has established that the row named by
// prev is the wrong device and is correcting the record. The caller must state
// the uid it expects to find, so a rebrand cannot be aimed at a device whose
// brand changed since it was read, and must give a reason, which is logged at
// ERROR level together with both uids. Nothing in the enrollment loop calls
// this. Overwriting a brand is a decision a person makes and signs.
func (b *Brander) Rebrand(ctx context.Context, devpath, uid, prev, reason string) (BrandOutcome, error) {
	if !uidRe.MatchString(uid) {
		return BrandFailed, fmt.Errorf("enroll: %q is not a farm uid and will not be written to %s",
			truncate(uid, 64), devpath)
	}
	if reason == "" {
		return BrandFailed, fmt.Errorf("enroll: rebranding %s from %s to %s requires a reason",
			devpath, prev, uid)
	}

	have, err := b.Read(ctx, devpath)
	if err != nil {
		return BrandFailed, err
	}
	if have != prev {
		// Said in full, because the caller's whole justification for
		// overwriting a brand was a belief about which device this is, and
		// that belief has just turned out to be about a different phone.
		return BrandConflict, fmt.Errorf(
			"enroll: rebranding %s to %s was authorised on the understanding that it carries %q, "+
				"but it carries %q; nothing was written — establish which device this is before "+
				"trying again: %w",
			devpath, uid, truncate(prev, 64), truncate(have, 64),
			&ConflictError{Devpath: devpath, Have: have, Want: uid})
	}
	if have == uid {
		return BrandAlready, nil
	}

	// ERROR, not Info. This line is the only record that two device rows once
	// pointed at this phone, and the only place an operator can later find out
	// which history was abandoned.
	b.log.Error("REBRANDING a device: its previous identity is being abandoned",
		"devpath", devpath, "previous_uid", have, "new_uid", uid, "reason", reason)

	// have is what Read returned, so it is a farm uid or empty. Empty means
	// the file was absent or blank: nothing is being replaced, and the write
	// is the ordinary one with the ordinary guard.
	cmd := brandWriteCmd(uid)
	if have != "" {
		cmd = brandReplaceCmd(uid, have)
	}
	return b.write(ctx, devpath, uid, cmd)
}

// write runs cmd, which installs uid on the device, and verifies the result
// by reading it back.
//
// uid is known to match uidRe by the time it gets here, and so is every other
// uid in cmd, so the command contains nothing a shell reads as syntax.
func (b *Brander) write(ctx context.Context, devpath, uid, cmd string) (BrandOutcome, error) {
	cctx, cancel := context.WithTimeout(ctx, b.timeout)
	res, err := b.sh.Shell(cctx, devpath, cmd)
	cancel()
	if err != nil {
		return BrandFailed, fmt.Errorf("enroll: write the brand to %s: %w", devpath, err)
	}
	if res == nil {
		return BrandFailed, fmt.Errorf(
			"enroll: write the brand to %s: the shell returned neither output nor an error", devpath)
	}

	if res.Exited && res.ExitCode == brandOccupied {
		// The device itself refused: a different brand was in place at the
		// instant of the write, however the read a moment earlier came out.
		// Read it back so the operator is told WHICH identity is on the phone
		// rather than merely that there is one.
		have, rerr := b.Read(ctx, devpath)
		ce := &ConflictError{Devpath: devpath, Have: have, Want: uid}
		if rerr != nil {
			return BrandConflict, fmt.Errorf("%w (reading it back failed too: %v)", ce, rerr)
		}
		return BrandConflict, ce
	}

	// Any other non-zero status is a warning, not the verdict. The read-back
	// below is the verdict: it is the state of the device, and the only thing
	// that decides whether this phone will be recognised next time.
	if res.Exited && res.ExitCode != 0 {
		b.log.Warn("branding command reported a non-zero status; verifying the file itself",
			"devpath", devpath, "exit_code", res.ExitCode,
			"stderr", truncate(strings.TrimSpace(string(res.Stderr)), 200))
	}

	got, err := b.Read(ctx, devpath)
	if err != nil {
		return BrandFailed, fmt.Errorf("enroll: verify the brand on %s: %w", devpath, err)
	}
	if got != uid {
		return BrandFailed, fmt.Errorf(
			"enroll: the brand on %s reads back as %q after writing %s; the device did not keep it — "+
				"check whether %s is writable there",
			devpath, truncate(got, 64), uid, BrandDir)
	}
	return BrandWritten, nil
}

// truncate bounds a device-supplied string on its way into a log line or an
// error. Everything in this file that came off a phone passes through it.
func truncate(s string, n int) string {
	s = strings.ToValidUTF8(strings.Map(printableRune, s), "")
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary. These strings end up inside a jsonb document,
	// and half a rune is not text.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func printableRune(r rune) rune {
	if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f) {
		return r
	}
	return -1
}

// compile-time assertion that the ADB client is a usable Shell, so a change to
// either signature fails here rather than at a wiring site.
var _ Shell = (*adbwire.Client)(nil)
