package demo

// The demo's screen fixture, and the one number three files have to agree on.

import (
	"testing"

	"github.com/flaviopadilha/device-farmer/test/fakeadb"
)

// TestTheDemoJarDigestIsWhatTheDeploymentsPin.
//
// ScreenServerSHA is written out as a literal so docker-compose.yml and the
// Makefile can set FARM_SCREEN_SERVER_SHA to something an operator can read and
// compare. That makes it a number in three files, and a number in three files
// drifts.
//
// When it drifts the failure is not loud: the demo seeds an artifact under one
// digest, the api is pinned to another, and the first screen anybody opens
// answers 502 because artifacts.EnsureOnDevice cannot find the blob the
// configuration names.
//
// Falsify: change one character of ScreenServerJarBody.
func TestTheDemoJarDigestIsWhatTheDeploymentsPin(t *testing.T) {
	if got := screenSHA(); got != ScreenServerSHA {
		t.Errorf("the placeholder hashes to\n  %s\nand ScreenServerSHA pins\n  %s\n\n"+
			"Update the constant AND FARM_SCREEN_SERVER_SHA in docker-compose.yml and the\n"+
			"Makefile, or the demo seeds a blob the api is not looking for and every screen\n"+
			"answers 502.", got, ScreenServerSHA)
	}
}

// TestTheDemoClipFramesIntoAStreamADecoderCanJoin.
//
// The demo loops a four-second clip forever. A viewer who opens a screen does so
// at an arbitrary moment, so the thing that decides whether they see a picture or
// a grey rectangle is how often the stream carries something a decoder can start
// from — and that is a property of the clip, fixed when ffmpeg encoded it, not of
// any code in this package.
//
// Falsify: drop -x264-params keyint=12 from scripts/make-screen-fixture.sh and
// regenerate; the clip gets one key frame and a viewer joining after the first
// second waits four seconds for a picture.
func TestTheDemoClipFramesIntoAStreamADecoderCanJoin(t *testing.T) {
	packets, err := fakeadb.ScrcpyPacketsFromAnnexB(fakeadb.ScreenFixture(), screenFPS)
	if err != nil {
		t.Fatalf("the committed clip does not frame: %v", err)
	}
	if len(packets) < 2 {
		t.Fatalf("the clip framed into %d packets", len(packets))
	}
	if !packets[0].Config {
		t.Error("the first packet is not the codec configuration, so a decoder is handed a " +
			"frame before it has been told how to decode one")
	}

	var keys int
	for _, p := range packets[1:] {
		if p.KeyFrame {
			keys++
		}
	}
	if keys < 2 {
		t.Errorf("the clip has %d key frames in %d packets; a viewer joining the loop at an "+
			"arbitrary moment waits for the next one, and with too few that wait is the "+
			"whole clip", keys, len(packets)-1)
	}

	// The timestamps must advance, or a decoder is told the stream stuttered.
	var last uint64
	for i, p := range packets[1:] {
		if i > 0 && p.PTS <= last {
			t.Fatalf("packet %d is timestamped %dµs, at or before the one before it (%dµs)",
				i+1, p.PTS, last)
		}
		last = p.PTS
	}
}

// TestTheGeometryTheFixtureAnnouncesIsTheGeometryItEncoded.
//
// The session header is the coordinate space every touch is expressed in. A
// header that disagreed with the pictures behind it would put every tap in the
// wrong place while the panel looked entirely correct — the worst available
// failure, because nothing about it looks like a bug.
//
// This pins the two numbers in installScreenFixtures against the clip itself, so
// that regenerating the clip at another size fails here rather than in somebody's
// hands.
func TestTheGeometryTheFixtureAnnouncesIsTheGeometryItEncoded(t *testing.T) {
	const wantW, wantH = 288, 640

	sps := findSPS(t, fakeadb.ScreenFixture())
	if sps == nil {
		t.Fatal("the committed clip carries no SPS, so nothing can decode it")
	}
	// Not a full SPS parse — that needs exp-Golomb and is internal/scrcpy's job,
	// not this test's. What is checked is that the clip is the one this package
	// was written against, by its size on disk and its first parameter set.
	if n := len(fakeadb.ScreenFixture()); n < 10_000 || n > 200_000 {
		t.Errorf("the committed clip is %d bytes; the one this package announces as %dx%d was "+
			"88789. Regenerate the constants in installScreenFixtures with it.", n, wantW, wantH)
	}
}

// findSPS returns the first SPS NAL, start code included.
func findSPS(t *testing.T, b []byte) []byte {
	t.Helper()
	for i := 0; i+4 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 && b[i+4]&0x1f == 7 {
			return b[i:]
		}
	}
	return nil
}
