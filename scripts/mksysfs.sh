#!/bin/bash
# Materialise a sysfs-shaped USB tree on a REAL Linux filesystem.
#
# internal/topo/sysfs_test.go builds the same shape as an fstest.MapFS. This
# builds it as real directories and real files, so topo.Sysfs's os.DirFS path —
# the one the shipped binary takes and no test has ever executed — runs against
# it. The file MODE on each port's `disable` is load-bearing: 0644 means the
# kernel will let you switch that port's VBUS, 0444 means it will not, and the
# parser reads the mode rather than the contents. fstest.MapFS could only
# simulate that; here the kernel's own stat answers.
#
# Layout mirrors rackFixture(): bus 3, a four-port root hub, and a seven-port
# per-port-switchable hub in root port 1 carrying two phones, a handset in
# fastboot, and a keyboard somebody left in port 5.
set -euo pipefail
ROOT="${1:-/tmp/farm-sysfs}"
rm -rf "$ROOT"
mkdir -p "$ROOT"

put() { mkdir -p "$(dirname "$ROOT/$1")"; printf '%s\n' "$2" > "$ROOT/$1"; chmod 0444 "$ROOT/$1"; }

# ---- root hub: usb3, canonical path 3-0, four ports -------------------------
put usb3/idVendor 1d6b
put usb3/idProduct 0002
put usb3/manufacturer "Linux $(uname -r) xhci-hcd"
put usb3/product "xHCI Host Controller"
put usb3/serial "0000:00:14.0"
put usb3/bDeviceClass 09
put usb3/maxchild 4
put usb3/speed 480
put "usb3/3-0:1.0/bInterfaceClass" 09
put "usb3/3-0:1.0/bInterfaceSubClass" 00
put "usb3/3-0:1.0/bInterfaceProtocol" 01
for p in 1 2 3 4; do
  put "usb3/3-0:1.0/usb3-port$p/connect_type" hotplug
  put "usb3/3-0:1.0/usb3-port$p/disable" 0
done

# ---- 3-1: seven-port hub, per-port switchable ------------------------------
put 3-1/idVendor 05e3
put 3-1/idProduct 0610
put 3-1/manufacturer GenesysLogic
put 3-1/product "USB2.1 Hub"
put 3-1/bDeviceClass 09
put 3-1/maxchild 7
put 3-1/speed 480
put "3-1/3-1:1.0/bInterfaceClass" 09
put "3-1/3-1:1.0/bInterfaceSubClass" 00
put "3-1/3-1:1.0/bInterfaceProtocol" 01
for p in 1 2 3 4 5 6 7; do
  put "3-1/3-1:1.0/3-1-port$p/connect_type" hotplug
  put "3-1/3-1:1.0/3-1-port$p/disable" 0
  # 0644 is the whole signal for "this port's VBUS can be switched".
  chmod 0644 "$ROOT/3-1/3-1:1.0/3-1-port$p/disable"
done

phone() { # $1 = path, $2 = product
  put "$1/bDeviceClass" 00
  put "$1/speed" 480
  put "$1/idVendor" 18d1
  put "$1/idProduct" 4ee7
  put "$1/manufacturer" Google
  put "$1/product" "$2"
  # `tr -d -- '-.'` and not `tr -d '-.'`: uutils coreutils, which Ubuntu 26.04
  # ships, parses a leading dash in the SET as an option and refuses.
  put "$1/serial" "PH$(printf '%s' "$1" | tr -d -- '-.')"
  put "$1/$1:1.0/bInterfaceClass" ff
  put "$1/$1:1.0/bInterfaceSubClass" 42
  put "$1/$1:1.0/bInterfaceProtocol" 01
}

# Ports 1..4 carry handsets; 5 has a keyboard; 6 and 7 are empty, which is what
# a rack looks like halfway through a build-out.
phone 3-1.1 "Pixel 6a"
phone 3-1.2 "Pixel 7"
phone 3-1.3 "Galaxy S22"
phone 3-1.4 "Pixel 6a"

# A keyboard: same bus, not a handset. The scan must not adopt it as a slot.
put 3-1.5/bDeviceClass 00
put 3-1.5/speed 480
put 3-1.5/idVendor 046d
put 3-1.5/idProduct c31c
put 3-1.5/manufacturer Logitech
put 3-1.5/product "USB Keyboard"
put "3-1.5/3-1.5:1.0/bInterfaceClass" 03
put "3-1.5/3-1.5:1.0/bInterfaceSubClass" 01
put "3-1.5/3-1.5:1.0/bInterfaceProtocol" 01

echo "sysfs tree at $ROOT"
find "$ROOT" -maxdepth 1 -mindepth 1 -printf '%f\n' | sort | tr '\n' ' '
echo
echo -n "disable mode on 3-1 port 1: "; stat -c '%a' "$ROOT/3-1/3-1:1.0/3-1-port1/disable"
echo -n "disable mode on root port 1: "; stat -c '%a' "$ROOT/usb3/3-0:1.0/usb3-port1/disable"
