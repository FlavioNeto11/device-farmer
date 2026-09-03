// Package fakeadb is an in-process ADB host server for tests.
//
// It listens on 127.0.0.1 and speaks the real adb host protocol — 4-hex-digit
// length framing, OKAY/FAIL replies, host:devices-l, host:track-devices-l,
// transport switching — so internal/adbwire can be exercised end to end with
// zero hardware and zero flakiness budget.
//
// # Why this exists
//
// The invariant this project defends is that a transport failure may never end
// a lease. DeviceFarmer/STF issue #663 releases a device after a ~90-minute
// ECONNRESET on the tracking socket, destroying multi-hour work. Proving we do
// not do that requires producing transport failures on demand: a FAIL reply, a
// connection severed mid-stream with a TCP RST, a server that accepts the
// connection and then says nothing until the caller's context deadline fires.
// Real hardware produces those events rarely and at inconvenient hours. This
// package produces them in microseconds, deterministically, in a unit test.
//
// The fake deliberately knows nothing about leases. It has no lease vocabulary,
// no database, and no opinion about what a caller should do with a severed
// socket. Its whole contribution is making the failure cheap to reproduce; the
// assertion that the failure changed no lease row belongs in the lease tests.
//
// # Addressing
//
// A device here is keyed by its devpath — "usb:<bus>-<port>[.<port>...]", the
// same string farm.slots.adb_devpath generates — because the physical USB
// position is the stable object and the ADB serial is merely an observation.
// Matching mirrors atransport::MatchesTarget in the real adb: a target string
// matches a device by serial OR by devpath. Two devices sharing an OEM serial
// therefore both match a serial-addressed request, and the fake answers FAIL
// "more than one device", exactly as adb does. TwoClonesFixture exists to make
// a test of that ambiguity a two-line affair.
//
// # Usage
//
//	srv := fakeadb.Start(t)
//	srv.Add(fakeadb.Device{Serial: "X", Devpath: "usb:3-1.1"})
//	cli := adbwire.Dial(srv.Addr())
//
//	// Same server, with the duplicate-serial trap pre-loaded:
//	srv := fakeadb.Start(t, fakeadb.TwoClonesFixture())
//
//	// Sever the tracking socket after two snapshots, the #663 shape:
//	srv.ResetAfter("host:track-devices", 2)
//
// Start registers cleanup with t, so the listener, every open connection and
// every scripted goroutine are gone when the test returns.
//
// # Concurrency
//
// Every exported method is safe to call from any goroutine, including while
// connections are being served. Mutating the device table wakes every open
// host:track-devices stream with a fresh snapshot, which is how a test drives a
// device offline underneath a running client.
package fakeadb
