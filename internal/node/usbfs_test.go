package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flaviopadilha/device-farmer/internal/recovery"
)

// A denied open of a usbfs node is the one tier-3 answer that used to lie. The
// agent never issued the ioctl, so nothing about the handset was learned — but
// the error carried no sentinel, so it arrived at the ladder as a rung that
// ran and did not help, and the ladder answers that by climbing to tier 4 and
// cutting VBUS. These tests walk the same path the ganged refusal has: the
// classifier, the wire, the client, the ladder's own reading.
//
// permError is what os.OpenFile hands back when the udev rule is missing:
// EACCES or EPERM inside an *fs.PathError, both of which os.ErrPermission
// matches. It is built rather than provoked because no build machine has a
// usbfs node to be denied by — which is the whole reason the classification
// lives outside //go:build linux.
const devNode = "/dev/bus/usb/003/017"

func permError() error {
	return &fs.PathError{Op: "open", Path: devNode, Err: fs.ErrPermission}
}

// TestUSBFSPermissionDenialIsARefusalNotAFailedRung pins every reading of the
// error that the recovery path takes: the contract's status table, the reason
// word, and the ladder's own classification, which is the one that decides
// whether tier 4 happens.
//
// Falsify: drop the ErrRefused wrap from usbfsOpenError's permission branch —
// StatusFor goes to 500, ReasonFor to "", and ClassifyHostFault to "failed",
// which is the defect this test exists for. Swapping ErrRefused for
// ErrNotSupported fails it too, on the arm below that refuses the wrong word.
func TestUSBFSPermissionDenialIsARefusalNotAFailedRung(t *testing.T) {
	t.Parallel()

	err := usbfsOpenError(devNode, permError())

	if !IsRefused(err) {
		t.Errorf("a denied open is not a refusal: %v", err)
	}
	if IsUnreachable(err) {
		t.Errorf("a denied open reads as an unreachable agent; the agent answered: %v", err)
	}
	// 501 is "this build cannot do it at all". A udev rule is a fact about the
	// host, and the same binary performs the rung the moment it is fixed.
	if errors.Is(err, ErrNotSupported) {
		t.Errorf("a host permission problem is reported as an unsupported build: %v", err)
	}
	if got := StatusFor(err); got != http.StatusConflict {
		t.Errorf("StatusFor = %d, want 409; only a 5xx may say a rung was attempted", got)
	}
	if got := ReasonFor(err); got != ReasonPolicy {
		t.Errorf("ReasonFor = %q, want %q", got, ReasonPolicy)
	}

	// The reading that decides whether a healthy handset gets its power cut.
	fault := recovery.ClassifyHostFault(err, false)
	if fault.Disposition != recovery.DispositionRefused {
		t.Errorf("the ladder reads a denied open as %q, want %q; %q spends the rung and "+
			"escalates to tier 4 on a device whose only problem is a file mode",
			fault.Disposition, recovery.DispositionRefused, recovery.DispositionFailed)
	}
	if fault.RefusalKind != "" {
		t.Errorf("RefusalKind = %q, want none; this refusal is not the ganged one, whose "+
			"remedy is a purchase order", fault.RefusalKind)
	}

	// The operator's half. farm.recovery_attempts.detail is where this
	// sentence is read at 3am, and it is the only thing that names the fix.
	for _, want := range []string{devNode, "udev rule", "nothing was sent to the device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message lost %q: %v", want, err)
		}
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("the underlying errno was dropped: %v", err)
	}
}

// TestUSBFSOpenFailuresThatAreNotPermissionStayFailedRungs is the other half,
// and the reason the fix is two branches and not the whole function. A node
// that is missing while the host's usbfs is right there says the device sysfs
// named a moment ago re-enumerated or dropped off: that IS a statement about
// the hardware, the ladder should be free to escalate on it, and a blanket
// refusal here would strand a device that needs the next rung.
//
// devBusUSB points at a directory that exists, because the classification turns
// on whether this host has a usbfs at all — see the test below.
//
// Falsify: return the ErrRefused wrap unconditionally in usbfsOpenError.
func TestUSBFSOpenFailuresThatAreNotPermissionStayFailedRungs(t *testing.T) {
	usbfsRootIs(t, t.TempDir())

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"the node is gone", &fs.PathError{Op: "open", Path: devNode, Err: fs.ErrNotExist}},
		{"an unclassified errno", &fs.PathError{Op: "open", Path: devNode,
			Err: errors.New("no such device")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := usbfsOpenError(devNode, tc.err)
			if IsRefused(err) {
				t.Errorf("read as a refusal, so the ladder hands the rung back forever: %v", err)
			}
			if got := StatusFor(err); got != http.StatusInternalServerError {
				t.Errorf("StatusFor = %d, want 500", got)
			}
			if got := recovery.ClassifyHostFault(err, false).Disposition; got != recovery.DispositionFailed {
				t.Errorf("the ladder reads this as %q, want %q", got, recovery.DispositionFailed)
			}
		})
	}
}

// TestNoUSBFSAtAllIsAnUnsupportedRungNotAFailedOne is how an agent says "I am
// the software half only". A container handed /sys but not /dev/bus/usb
// discovers topology and enrols devices and can never issue this ioctl; every
// tier-3 call there fails at the open with ENOENT, and without this branch each
// one is filed against a handset nobody touched.
//
// 501, not 409: this is the case ErrNotSupported was written for — the rung has
// no implementation on this host at all, and no udev rule will change it.
//
// Falsify: drop the isDir(devBusUSB) condition, and a missing usbfs reads as a
// failed rung again; or point usbfsRootIs at t.TempDir(), and the same error
// becomes the hardware statement the test above pins.
func TestNoUSBFSAtAllIsAnUnsupportedRungNotAFailedOne(t *testing.T) {
	usbfsRootIs(t, filepath.Join(t.TempDir(), "there-is-no-usbfs-here"))

	err := usbfsOpenError(devNode, &fs.PathError{Op: "open", Path: devNode, Err: fs.ErrNotExist})

	if !errors.Is(err, ErrNotSupported) {
		t.Errorf("a host with no usbfs does not report an unsupported rung: %v", err)
	}
	if got := StatusFor(err); got != http.StatusNotImplemented {
		t.Errorf("StatusFor = %d, want 501", got)
	}
	if got := ReasonFor(err); got != ReasonUnsupported {
		t.Errorf("ReasonFor = %q, want %q", got, ReasonUnsupported)
	}
	if got := recovery.ClassifyHostFault(err, false).Disposition; got != recovery.DispositionRefused {
		t.Errorf("the ladder reads this as %q, want %q; %q spends tier 3 and escalates to a "+
			"VBUS cut on a host that cannot even see the device nodes",
			got, recovery.DispositionRefused, recovery.DispositionFailed)
	}
	// The operator's half again: this one is a deployment fact, and the
	// sentence has to name the mount that is missing.
	if !strings.Contains(err.Error(), "/dev/bus/usb was not mounted in") {
		t.Errorf("the message does not name what a container operator has to fix: %v", err)
	}
}

// usbfsRootIs points the classification at a path of the test's choosing, the
// way blastradius_linux_test.go points the blast-radius check at sysfsUSBDevices.
// Tests that call it cannot run in parallel with each other.
func usbfsRootIs(t *testing.T, root string) {
	t.Helper()
	prev := devBusUSB
	t.Cleanup(func() { devBusUSB = prev })
	devBusUSB = root
}

// TestPermissionDeniedTierThreeCrossesTheWireAsARefusal drives the real
// Agent.Handler with the error usbfsOpenError actually builds, reads the wire,
// then reads it back through Client and asks what the ladder asks. Between the
// device host and the control plane there is an HTTP hop, and this is the hop
// where a refusal used to turn into a 500.
//
// Falsify: drop the ErrRefused arm from opHandler's status switch, or the
// "reason" key from its JSON.
func TestPermissionDeniedTierThreeCrossesTheWireAsARefusal(t *testing.T) {
	prev := platform
	t.Cleanup(func() { platform = prev })
	platform = fakePlatform{usbResetErr: usbfsOpenError(devNode, permError())}

	h, err := testAgent(t, testHost).Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	payload, _ := json.Marshal(OpRequest{HostID: testHost, Devpath: testPath})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+PathUSBReset, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", PathUSBReset, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409; a 5xx here is the agent claiming it touched the "+
			"port: %s", resp.StatusCode, body)
	}
	var out OpResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("the agent's answer is not this API's JSON: %s", body)
	}
	if !out.Refused || out.Reason != ReasonPolicy {
		t.Fatalf("OpResponse = %+v, want refused with reason %q", out, ReasonPolicy)
	}

	err = newClient(t, srv.URL).USBReset(context.Background(), testHost, testPath)
	if err == nil {
		t.Fatal("a denied open came back as a completed rung")
	}
	if !IsRefused(err) || IsUnreachable(err) {
		t.Errorf("a denied open must be a refusal and not an outage: %v", err)
	}
	if !errors.Is(err, recovery.ErrRungRefused) {
		t.Errorf("the ladder cannot classify this at all: %v", err)
	}
	if got := recovery.ClassifyHostFault(err, false).Disposition; got != recovery.DispositionRefused {
		t.Errorf("across the wire the ladder reads %q, want %q", got, recovery.DispositionRefused)
	}
	if !strings.Contains(err.Error(), "udev rule") {
		t.Errorf("the agent's own words were dropped on the way: %v", err)
	}
}
