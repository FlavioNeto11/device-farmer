//go:build linux

// The Linux half of charge gating. It reuses uhubctl.go's guards verbatim
// rather than reimplementing them: the kernel floor, the hub read and the blast
// radius are the same checks tier 4 makes, applied to a verb that is strictly
// more disruptive because it does not put the power back on its own.
//
// On every other GOOS the portable chargePlatform value stays in place and each
// request is refused with a reason naming the platform.

package node

import (
	"context"
	"fmt"
)

// Wiring the real implementation in from init keeps chargegate.go free of build
// tags, the same way hostops.go wires platformOps.
func init() { chargePlatform = linuxChargeGate{} }

type linuxChargeGate struct{}

// resolveChargePort validates the devpath and names the hub port behind it.
//
// The devpath it returns is the CANONICAL spelling, so "3-1.4.2" and
// "usb:3-1.4.2" name one gate rather than two, and a status read back by the
// control plane matches the devpath in farm.slots.
func (linuxChargeGate) resolveChargePort(devpath string) (chargePort, error) {
	usbPath, err := usbPathOf(devpath)
	if err != nil {
		return chargePort{}, err
	}
	hub, port, err := hubLocation(usbPath)
	if err != nil {
		return chargePort{}, err
	}
	return chargePort{
		Devpath: devpathOf(usbPath), USBPath: usbPath, Hub: hub, Port: port,
	}, nil
}

// setChargePower drives one port's VBUS.
//
// The two directions are not symmetrical, on purpose:
//
//	off  every guard tier 4 has, because holding a port dark for hours is
//	     more disruptive than darkening it for three seconds
//	on   no guards at all, because restoring power cannot disturb anybody:
//	     nothing behind that hub loses anything by getting power back, and a
//	     kernel-floor check on this path would make an old host unable to
//	     release a gate it should never have been given in the first place
//
// The touched return is what the caller's bookkeeping runs on. It stays false
// through every check that happens before a power-changing command is issued,
// so a refusal cannot be mistaken for a port whose state changed.
func (linuxChargeGate) setChargePower(ctx context.Context, p chargePort, on bool, acknowledged []string, o opsConfig) (touched bool, err error) {
	bin, err := uhubctlPath(o.uhubctl)
	if err != nil {
		return false, err
	}
	log := o.log.With("devpath", p.Devpath, "usb_path", p.USBPath, "hub", p.Hub, "port", p.Port)

	if on {
		log.Info("restoring VBUS to a gated port", "uhubctl", bin)
		if err := powerOn(ctx, bin, p.Hub, p.Port); err != nil {
			return true, err
		}
		// No wait for the device to enumerate. This verb's contract is the
		// PORT's power, and a phone that has been dark for an hour may be flat
		// enough to take minutes to appear. Calling that a failed restore would
		// report a hardware fault for a port that has exactly the power it was
		// asked for, and the enrollment loop is what notices the device coming
		// back anyway.
		log.Info("VBUS restored; the device will re-enumerate on its own")
		return true, nil
	}

	// A gate on a kernel that undoes port power is not a weaker gate, it is a
	// fiction: uhubctl reports success, the USB core re-powers the port, and
	// the control plane spends the next six hours believing it is protecting a
	// battery that is charging to 100%.
	if err := (linuxPlatform{}).checkKernelFloor(); err != nil {
		return false, err
	}

	hubs, err := readHubs(ctx, bin, p.Hub)
	if err != nil {
		return false, err
	}
	for _, h := range hubs {
		log.Info("hub reported by uhubctl", "hub", h.location,
			"per_port_power", h.perPort, "descriptor", h.info,
			"occupied_ports", len(h.children))
	}
	// The same last line tier 4 has, against live sysfs, and it matters more
	// here: on a ganged hub this call does not darken the neighbours for three
	// seconds, it darkens them until somebody releases the gate.
	if err := checkBlastRadius(hubs, p.Hub, p.Port, p.USBPath, acknowledged); err != nil {
		return false, err
	}

	// Whether the device is enumerated right now decides what can be proved
	// afterwards. A port that is already dark — the ordinary case when a gate
	// is re-asserted — cannot be watched disappearing.
	wasPresent := present(p.USBPath)

	// The guard is armed BEFORE the port is switched off, and it is the reason
	// a failed gate is not a dark port: uhubctl can fail partway (it switches
	// the USB3 hub and errors on the USB2 companion), so a non-zero exit is not
	// evidence that power is still on. What differs from portPower is when the
	// guard is disarmed — there, only after power was put back; here, once the
	// gate is established, because a gate that succeeded is SUPPOSED to leave
	// the port dark. From that moment the deadline in chargegate.go is what
	// turns the port back on.
	gated := false
	defer func() {
		if gated {
			return
		}
		// Detached: the caller's deadline expiring is not a reason to leave a
		// port dark that nobody is now tracking.
		restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
		defer cancel()
		if rerr := powerOn(restore, bin, p.Hub, p.Port); rerr != nil {
			log.Error("THE PORT IS PROBABLY LEFT WITHOUT POWER: the charge gate could not be "+
				"established and the restore failed too; no hold was recorded, so nothing "+
				"in this agent will retry — a human must run "+
				"`uhubctl -l <hub> -p <port> -a on` on this host", "err", rerr)
		}
	}()

	log.Info("cutting VBUS and holding it off", "uhubctl", bin, "device_enumerated", wasPresent)
	if _, err := runUhubctl(ctx, bin, p.Hub, p.Port, "off"); err != nil {
		return true, err
	}

	if wasPresent {
		if err := waitAbsent(ctx, p.USBPath, offVerifyWindow); err != nil {
			if ctx.Err() != nil {
				return true, fmt.Errorf("node: the charge gate for %s was cancelled while the "+
					"port was dark; power is being restored regardless: %w", p.Devpath, ctx.Err())
			}
			return true, fmt.Errorf("node: uhubctl reported the port off but the device at %s "+
				"is still enumerated %s later, so VBUS was not actually cut; a charge gate "+
				"this host cannot hold would report a protected battery while the phone "+
				"charged to full: %w", p.Devpath, offVerifyWindow, err)
		}
		log.Info("device left the bus; the port is dark and will stay dark until the gate expires")
	}

	gated = true
	return true, nil
}
