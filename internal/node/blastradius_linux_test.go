//go:build linux

package node

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flaviopadilha/device-farmer/internal/recovery"
)

// TestBlastRadiusRefusalIsTheGangedOne drives the real checkBlastRadius — the
// function in front of uhubctl, where the ganged refusal is born — over a
// sysfs it builds itself. The wire test in ganged_refusal_test.go uses a
// platform fake that mirrors this function's answer; this one asks the
// function. Without it, wrapping ErrRefused here instead of ErrGangedDomain
// was caught by nothing, and every ganged refusal in production would quietly
// have become refused_policy.
//
// Linux-only because uhubctl.go and hostops.go are; on a Windows or macOS
// build machine run it cross-compiled: GOOS=linux go test -c ./internal/node/
// and execute the binary under WSL or a container.
//
// It is not parallel: it redirects sysfsUSBDevices for its duration.
//
// Falsify: wrap ErrRefused instead of ErrGangedDomain in checkBlastRadius's
// final return; or make note() skip descendants (the phone behind the nested
// hub is then not named).
func TestBlastRadiusRefusalIsTheGangedOne(t *testing.T) {
	root := t.TempDir()
	// Port 3 carries a hub with one phone behind it. Its interface entry is
	// there too, because sysfs lists those beside the devices.
	for _, p := range []string{"3-1.2", "3-1.3", "3-1.3.1", "3-1.4"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p, "devnum"), []byte("5\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "3-1.3.1:1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := sysfsUSBDevices
	sysfsUSBDevices = root
	t.Cleanup(func() { sysfsUSBDevices = prev })

	children := map[int]string{2: "3-1.2", 3: "3-1.3", 4: "3-1.4"}
	ganged := []hubStatus{{location: "3-1", perPort: false, children: children}}
	perPort := []hubStatus{{location: "3-1", perPort: true, children: children}}

	cases := []struct {
		name   string
		hubs   []hubStatus
		ack    []string
		ganged bool   // want ErrGangedDomain
		policy bool   // want ErrRefused without ErrGangedDomain
		named  string // must appear in the refusal
		spared string // must not
	}{
		{"ganged hub, nothing acknowledged", ganged, nil, true, false,
			"3 device(s) nobody authorised — usb:3-1.2 (on hub 3-1), usb:3-1.3 (on hub 3-1), " +
				"usb:3-1.3.1 (on hub 3-1)", ""},
		// Acknowledging the hub on port 3 is not acknowledging the phone
		// plugged into it; that phone may hold a separate lease.
		{"ganged hub, neighbours acknowledged but not the phone behind the nested hub", ganged,
			[]string{"usb:3-1.2", "usb:3-1.3"}, true, false,
			"1 device(s) nobody authorised — usb:3-1.3.1 (on hub 3-1)", "usb:3-1.2 (on hub"},
		{"ganged hub, everything acknowledged", ganged,
			[]string{"usb:3-1.2", "usb:3-1.3", "usb:3-1.3.1"}, false, false, "", ""},
		{"per-port hub disturbs only the target", perPort, nil, false, false, "", ""},
		// A serial where a devpath belongs authorises nothing, and that is
		// the agent's own policy, not a ganged domain.
		{"an acknowledgement that is not a position", ganged, []string{"HT7A1B00123"},
			false, true, "not a USB position", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBlastRadius(tc.hubs, "3-1", 4, "3-1.4", tc.ack)
			if !tc.ganged && !tc.policy {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the cycle was allowed")
			}
			if got := errors.Is(err, ErrGangedDomain); got != tc.ganged {
				t.Errorf("errors.Is(ErrGangedDomain) = %v, want %v: %v", got, tc.ganged, err)
			}
			if !errors.Is(err, ErrRefused) || !errors.Is(err, recovery.ErrRungRefused) {
				t.Errorf("every blast-radius answer is a refusal on both sides of the seam: %v", err)
			}
			wantReason := ReasonPolicy
			if tc.ganged {
				wantReason = ReasonGanged
			}
			if got := ReasonFor(err); got != wantReason {
				t.Errorf("ReasonFor = %q, want %q", got, wantReason)
			}
			if tc.named != "" && !strings.Contains(err.Error(), tc.named) {
				t.Errorf("the refusal does not name %q:\n%v", tc.named, err)
			}
			if tc.spared != "" && strings.Contains(err.Error(), tc.spared) {
				t.Errorf("the refusal names an acknowledged device %q:\n%v", tc.spared, err)
			}
		})
	}
}
