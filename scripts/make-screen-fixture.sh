#!/bin/bash
# Regenerate test/fakeadb/testdata/screen.h264, the committed video fixture.
#
# WHY A COMMITTED CLIP AND NOT A GENERATED ONE. There is no Android handset in
# this repository and there is no encoder in the standard library, so the only
# H.264 the fake can serve is H.264 somebody produced in advance. The
# alternative the fixture started with — recognisable ASCII in the payload
# slot — proves that bytes survive the transport and proves nothing about
# whether a decoder can do anything with them. internal/scrcpy's reader and the
# browser's VideoDecoder both take Annex-B access units and neither of them
# cares what this file asserts; they care whether the bytes decode. So the
# bytes have to be real, and real bytes have to be checked in.
#
# Run it from the repository root. It overwrites the fixture in place, so the
# way to verify a change is `git diff --stat` on the result: a regeneration that
# produces a different byte count on the same ffmpeg is a regeneration that
# changed something, and test/fakeadb's own tests pin the count the splitter
# expects.
set -euo pipefail

if ! command -v ffmpeg >/dev/null 2>&1; then
  cat >&2 <<'EOF'
make-screen-fixture.sh: ffmpeg is not on PATH.

This script needs an ffmpeg built with libx264 (check `ffmpeg -version` for
--enable-libx264; without it `-c:v libx264` fails with "Unknown encoder").

  Debian/Ubuntu   sudo apt-get install ffmpeg
  Fedora          sudo dnf install ffmpeg        # RPM Fusion
  macOS           brew install ffmpeg
  Windows         winget install Gyan.FFmpeg     # the full build, not essentials

Nothing in the test suite needs ffmpeg: the fixture is committed, and this
script exists to regenerate it, not to build it on demand.
EOF
  exit 1
fi

out=test/fakeadb/testdata/screen.h264
mkdir -p "$(dirname "$out")"

# Every flag below is load-bearing. Taking one out does not produce a slightly
# different fixture; it produces a fixture that breaks one of the four things
# downstream of it.
#
#   -f lavfi -i testsrc2        A synthetic source, because a fixture generated
#                               from a sample clip inherits that clip's licence
#                               and its provenance question. testsrc2 MOVES —
#                               there is a sweeping bar and a changing frame
#                               counter — so a viewer looking at the dashboard
#                               can tell a live stream from a stuck first frame
#                               without decoding anything, and an inter-coded
#                               frame in this stream actually carries residual
#                               instead of coding a still image as nothing.
#
#   size=288x640 rate=12        A phone-shaped aspect (9:20, the Pixel 6a panel
#                               the fixture claims) at a quarter of its pixel
#                               count, because this file is committed and every
#                               pixel is bytes in the repository forever. 12 fps
#                               rather than 30 for the same reason; the feature
#                               under test is "can a human touch what they see",
#                               which is a latency question, not a smoothness
#                               one.
#
#   -t 4                        Four seconds. Long enough that a loop point is
#                               reached during a test rather than on frame two,
#                               short enough to stay inside the size budget.
#
#   -profile:v baseline -bf 0   Constrained Baseline and no B-frames, because
#                               internal/scrcpy hands a decoder access units in
#                               DECODE order and the scrcpy packet header
#                               carries one timestamp, the PTS. It carries no
#                               DTS. A stream with B-frames needs a decoder that
#                               reorders output, and a reordering decoder needs
#                               to be told the decode timestamp of every access
#                               unit — which is a field this protocol does not
#                               have. So the fixture must be a stream whose
#                               decode order IS its presentation order, and
#                               baseline with zero B-frames is exactly that
#                               constraint spelled as encoder flags.
#
#   -level 3.0 -pix_fmt yuv420p Level 3.0 and 4:2:0 8-bit are what every
#                               browser's VideoDecoder accepts without a
#                               hardware caveat. A fixture that needed a
#                               particular GPU would fail in Chrome on one
#                               machine and pass on the next.
#
#   -crf 34                     The size knob, and the reason it is this high.
#                               88 KB is the budget for a file that lives in git
#                               forever; crf 28 triples it for detail nobody
#                               looks at, since what this clip has to show is
#                               motion and a frame number, not texture.
#
#   -g 12 and the x264-params   The loop is the whole reason these exist.
#   keyint=12:min-keyint=12     ScrcpyConfig.VideoLoop replays the clip from
#   scenecut=0                  packet zero forever, so the stream is spliced to
#                               its own beginning every four seconds, and it is
#                               also joined partway through by any viewer who
#                               opens the dashboard after the session started.
#                               Both of those are seeks, and a seek only decodes
#                               if it lands on an IDR. keyint=min-keyint=12 with
#                               scenecut disabled puts an IDR on exactly every
#                               twelfth frame — a fixed one-second grid rather
#                               than wherever x264's scene detector felt like
#                               putting one, so the loop point is an IDR by
#                               construction and not by luck.
#
#   repeat-headers=1            Each IDR carries its OWN SPS and PPS inline.
#                               Without it x264 writes the parameter sets once,
#                               at the top of the file, and every access unit
#                               after the first loop arrives at a decoder that
#                               was reset and no longer has them — which a
#                               browser reports as a decode error on a stream
#                               that played perfectly for four seconds. It also
#                               makes the config packet the splitter synthesises
#                               a true statement about every keyframe rather
#                               than about the first one only.
#
#   -an -f h264                 No audio track and a raw Annex-B elementary
#                               stream, not a container. scrcpy's video socket
#                               carries naked access units with start codes;
#                               an MP4 would carry length-prefixed NALs in an
#                               moov nobody on this path parses.
ffmpeg -hide_banner -loglevel error -f lavfi -i "testsrc2=size=288x640:rate=12" \
  -t 4 -c:v libx264 -profile:v baseline -level 3.0 -pix_fmt yuv420p -crf 34 \
  -g 12 -bf 0 -x264-params "repeat-headers=1:keyint=12:min-keyint=12:scenecut=0" \
  -an -f h264 -y "$out"

# Report what landed, because the numbers are what test/fakeadb asserts on and a
# silent regeneration that halved the access unit count would otherwise be
# discovered by a test failure in a package that does not mention this script.
bytes=$(wc -c <"$out" | tr -d '[:space:]')
echo "wrote $out: $bytes bytes"
if command -v ffprobe >/dev/null 2>&1; then
  ffprobe -hide_banner -loglevel error -select_streams v:0 \
    -show_entries stream=profile,width,height,nb_read_frames -count_frames \
    -of default=noprint_wrappers=1 "$out"
fi
