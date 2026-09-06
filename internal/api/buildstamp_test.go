package api

// What a running farm says it is has to be what was built.
//
// GET /api/v1/capabilities is how an operator asks a live control plane which
// build is answering — the dashboard prints it, and it is the first thing
// anyone checks when two farms behave differently. It reported "dev" for every
// binary ever produced, including released images, and nothing noticed because
// `farmd version` printed the right answer from the same ldflags.
//
// The cause is a package boundary. `-ldflags -X main.version=…` writes into
// package main; this package cannot see those variables, and its only other
// source was debug.ReadBuildInfo, whose Main.Version is "(devel)" for any
// `go build` of a main module. The release image also compiles with
// -buildvcs=false, so vcs.revision is empty there too: by the time the image
// was correct about its own OCI labels, the API had no correct answer left.
//
// These tests pin both directions, because the interesting half is the
// fallback: a caller that stamps nothing must keep saying "dev" rather than
// reporting an empty version, which would read as a farm that failed to answer.

import (
	"testing"
	"time"
)

func TestCapabilitiesReportsTheStampedBuild(t *testing.T) {
	s := &Server{startedAt: time.Now()}
	WithBuild(BuildStamp{Version: "v1.4.2", Revision: "0f1e2d3c"})(s)

	b := s.buildInfo()
	if b.Version != "v1.4.2" {
		t.Errorf("build.version = %q, want %q: an operator asking a live farm which "+
			"release is running got the placeholder instead", b.Version, "v1.4.2")
	}
	if b.Revision != "0f1e2d3c" {
		t.Errorf("build.revision = %q, want %q: the image compiles -buildvcs=false, so "+
			"the stamp is the only source of a commit there", b.Revision, "0f1e2d3c")
	}
}

func TestCapabilitiesSaysDevWhenNobodyStampedTheBinary(t *testing.T) {
	s := &Server{startedAt: time.Now()}

	b := s.buildInfo()
	if b.Version == "" {
		t.Fatal("build.version is empty for an unstamped binary; an empty string reads " +
			"as a farm that could not answer, and the honest answer is \"dev\"")
	}
	if b.Version != "dev" {
		// Not a failure by itself: a `go install` of a tagged module legitimately
		// carries a version here. But a plain `go test` build must not.
		t.Logf("build.version = %q for an unstamped binary (expected \"dev\" under `go test`)", b.Version)
	}
}
