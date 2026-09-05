// Package adbwire is a native client for the ADB *server* host protocol,
// spoken directly over TCP. It never shells out to the adb binary.
//
// # The import barrier
//
// This package MUST NOT import
// github.com/flaviopadilha/device-farmer/internal/lease, and it must contain
// no allocation vocabulary. This file is the sole exception: stating the
// barrier requires naming the thing being barred, so a CI vocabulary check
// should scan every file in this package EXCEPT doc.go.
//
// The barrier is not stylistic. DeviceFarmer/STF issue #663 (open and
// unanswered since 2023) reports a device being handed to a different job in
// the middle of a run after roughly ninety minutes of ECONNRESET on the ADB
// transport, destroying multi-hour work. The defect there was not the socket
// error — socket errors are normal and permanent in a farm of hundreds of
// USB-attached phones. The defect was that a socket error had a code path to
// an allocation decision.
//
// Here that path does not exist. A transport failure in this package has
// exactly two permitted effects:
//
//  1. a typed error is returned to the immediate caller, and
//  2. a counter is incremented.
//
// Nothing in this package writes to the database, and nothing in it can
// reach the allocation tables even transitively, because it cannot see the
// package that owns them. A caller that receives a *TransportError may
// reconnect, retry, alert, or give up. It may not translate the error into a
// reason for taking a device away from the job that owns it — and to make
// that inexpressible rather than merely discouraged, the schema's
// release_reason CHECK constraint has no connectivity value at all.
//
// # Addressing: devpath, never serial
//
// Every function here that targets a physical position takes a *devpath* —
// the USB tree position the ADB server reports for a transport, such as
// "usb:3-1.4.2" — and never an OEM serial. Duplicate serials are real: STF's
// own README documents a device shipping with serial "0123456789ABCDEF", and
// two such clones in one rack are indistinguishable by serial. A recovery
// action (port power cycle, reboot, reset) that resolved a target by serial
// could land on the wrong clone and kill a healthy device in the middle of a
// six-hour run.
//
// Serial addressing exists in exactly one place, [Client.UnsafeBySerial], and
// carries that name deliberately. It is for interactive tooling and for
// bootstrap enrolment of a device whose devpath is not yet known. No recovery
// or health path may call it.
//
// # Three clocks
//
// This package observes exactly one thing: whether the ADB server can be
// reached and what it says about the devices attached to it. That is device
// health, and it is the third of three clocks that the system never
// collapses. Snapshots emitted by [Tracker] are inputs to the watchdog and to
// nothing else.
//
// A consequence worth stating explicitly: when the tracker's connection
// drops, this package emits NO snapshot. It does not synthesise an empty list
// and it does not mark anything absent. Losing the socket to the ADB server
// is evidence about the socket, not about the phones. The last state the
// server actually reported stands until the server itself reports otherwise.
//
// # The admission preamble
//
// A host may put its ADB server behind the fence proxy
// (docs/design/fence-proxy.md), which admits a connection only over mutual
// TLS and only after one frame announcing what the connection claims to
// hold: "fence:v1 class=<class> devpath=<devpath> fence=<token>", in ADB's
// own four-hex-digit framing, never acknowledged. [WithTLS] and
// [WithAdmissionPreamble] are that client half. The barrier above is why the
// option is not called WithFencePreamble and why the frame's magic word is
// assembled rather than spelled in client.go: this package can carry a
// number to the wire, and must stay unable to say what the number means.
// The one class that carries a device token is likewise named by the package
// that owns the binding — the job runner — and not here.
//
// The frame is sent only on a TLS connection. Over plain TCP the option is
// inert, so a deployment installs both options everywhere and switches the
// proxy on with a certificate alone; a bare ADB server never sees the frame.
//
// # Concurrency
//
// A [Client] is safe for concurrent use; it holds no connection of its own
// and opens one per call. A [Stream] and a [Transport] wrap a single socket
// and are not safe for concurrent use, except that Close may be called from
// another goroutine to interrupt a blocked read.
package adbwire
