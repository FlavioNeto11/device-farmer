# Interactive control: a live screen and a human's hands

Status: **investigated, not built.** Nothing in this document exists in the
tree. It is the case for a decision, written so the decision can be made once
and cheaply, and so that a later reader can tell what was measured from what
was assumed.

Method: six investigations (three of the outside world, three of this tree),
three independent designs, a judge panel that scored them harshly, and an
adversarial pass instructed to refute by default. Every load-bearing claim
below carries where it came from. Where the investigation contradicted the
brief it was given — which happened four times, including twice about this
project's own founding story — the contradiction is reported, not smoothed.

---

## 1. The headline: the hard half is already decided

The question "how does a human hold a device without reintroducing STF #663"
reads like the hard part. **It was answered, and written down, before this
investigation started.**

`internal/api/specs.go:546-552`:

> Absent, null and `{}` all mean "no automated instructions", which is what
> `farm.jobs.spec`'s own DEFAULT `'{}'` means and **what an interactive lease
> looks like: a human takes the device, and the job carries no steps for a
> runner to execute.** Those are accepted.

It is said in three places, not one: the API's spec parser above,
`internal/ctl/commands_job.go:240`, and a whole function —
`interactiveLeaseNote` at `internal/ctl/commands_spec.go:353` — that exists to
explain it to a user who submits a spec with no steps.

So an interactive session is **a `farm.jobs` row with an empty spec and a
`max_runtime` the human wrote down**. The scheduler places it through
`farm.lease_acquire` knowing nothing about specs. The jobrunner claims it and
runs `lease.Holder`'s renewal loop, which contacts no device and no browser.
The deadline is `max_runtime`, which `migrations/00001_core.sql:283` already
calls "the ONLY user-supplied clock that may end a lease automatically".

**What is missing is not a tenure model. It is a pipe.**

---

## 2. A correction to this project's founding story

This project's README, its register and this author have all repeated that
DeviceFarmer/STF issue #663 is "a ~90 minute `ECONNRESET` releases a device
mid-run" — a policy defect, a control plane that ends a lease because a socket
died. The investigation read the issue.

**The thread says something narrower and sharper.** Read directly
(five comments, still open, created 2023-05-20): the reporter is running a
six-hour instrumented test, not an interactive session. In the first comment
they report that the websocket connections die after ~1.5 hours with
`--screen-jpeg-quality` at 25 and hold at 0–1, over a direct IPsec link between
a GKE cluster and an on-prem ADB host. A maintainer answered that this "sounds
like a network issue… not due to STF itself" and asked for a tcpdump. The
reporter agreed to go and look. Nobody came back.

Two things must be said carefully, because the tree has been repeating a
version of this that the thread does not support.

**The screen stream is implicated, but the causal chain is inference.** The
correlation with JPEG quality is the reporter's own and is strong. The step
from there to the auto-release — ADB traffic stops, so STF's `userActivity`
stops, so the idle timer expires — is a reading of STF's code, **not something
the issue states**. Its `--group-timeout` (default 900s) appears nowhere in the
thread.

**And "open and unanswered" was wrong.** It was answered, twice, by a
maintainer. On the narrow question they were probably right: a saturated tunnel
is not STF's bug.

Three things follow, and they cut in opposite directions.

**The countermeasure is still right, and is now better motivated.** STF cannot
distinguish "the human is gone" from "a packet was dropped" — it measures
intent by ZeroMQ traffic on the device's own channel, so a user who is present
but *quiet* (reading a crash dialog, waiting on a build) is kicked at fifteen
minutes, while a user who closed their laptop keeps the device for the full
fifteen. It fails in both directions at once. There is no renewal API, and the
2022 request for a time-left label and an extend button (openstf#546) is still
open. Refusing connectivity endings is the right answer to that.

**And the screen stream is the leading suspect.** Not proven — see above — but
the reporter's own bisection points there, and it means the feature this
document proposes is plausibly the feature that triggered the incident this
project was built as a countermeasure to. That is not an argument against
building it. It is an argument that the *transport's* bandwidth behaviour is a
first-class safety property here rather than a performance detail, and it is the
single strongest reason to prefer an on-device hardware encoder over full JPEG
frames (§3).

**The README's summary has been corrected** in this same change: it claimed the
issue was unanswered, and it asserted a mechanism the thread does not state.
What survives, and is the whole argument, is that no step in the chain had to be
a bug for six hours of work to be destroyed — so the defensible move is to make
the outcome unrepresentable rather than to argue about whose fault the packet
was.

---

## 3. Transport: scrcpy, and it is not close

Two candidates, both Apache-2.0, both root-free.

| | **scrcpy** (Genymobile) | **minicap + minitouch** (DeviceFarmer) |
|---|---|---|
| Screen | H.264/H.265/AV1/VP8/VP9 via on-device `MediaCodec` | **JPEG only**, every frame independent |
| Bandwidth | interframe compression | scales with resolution × FPS, no interframe |
| Device side | one 717 KB dexed jar, `app_process`, `shell` user | native `.so` **only up to android-30**, then a Kotlin APK |
| Input | reflected `InputManager.injectInputEvent`, or `/dev/uhid` | evdev — **but needs `adb root`**, else silently dropped |
| Input on Android 10+ | unchanged | proxied to `STFService.apk`; minitouch becomes a text relay |
| Concurrent viewers | one encoder per session (by design) | `listen(sfd, 1)` — exactly one client, ever |
| Support floor | API 21+ | API 21+, with Android 15/16 gaps open on minicap |
| Browser | **Annex-B H.264 decodes directly in WebCodecs — no transcode** | JPEG, trivially, at the bandwidth cost above |

Three findings decide it.

**The brief was wrong that STF's stack is unmaintained.** DeviceFarmer/stf took
commits the day of the investigation and its CI runs API 21 through 37.2
(Android 17); minitouch got NDK 29 and 16 KB page-alignment work in August 2026.
Only **minicap** is the laggard — last substantive commit September 2025, with
Android 15/16 issues unanswered — and the project routed around it: the
published npm package ships `minicap.so` only for android-21..30, so every
device from Android 12 up is already served by an APK grabber. The rejection is
not "these are dead". It is that the *screen* half is JPEG-only, and that on
modern Android the *input* half is an APK reflecting into hidden framework APIs
— which is what scrcpy already is, with one fewer moving part.

**`input tap` is not a fallback.** scrcpy does not shell out; it reflects
`InputManager.injectInputEvent(InputEvent, int)` and builds `MotionEvent`s
directly. A `shell:input tap X Y` path spawns a JVM per touch and cannot express
a drag at all.

**The Go control plane can be a byte pipe.** scrcpy's video framing is a 4-byte
codec id, a 12-byte session header, then per packet a 12-byte header (flags +
61-bit PTS) and a `u32` length. That parses with `encoding/binary` and nothing
else, and the payload is Annex-B H.264 that a browser's `VideoDecoder` accepts
as-is. **No transcode, no new Go dependency** — which matters, because this tree
allows exactly three.

**One correction the adversary made:** scrcpy's touch coordinates are in the
*video* coordinate space (after `--max-size`, `--crop` and the rotation filter),
not raw device pixels, and the accompanying `screenWidth/screenHeight` is a
staleness guard, not a rescaling convenience. A proxy that assumes device pixels
will send taps to the wrong place on any cropped or downscaled stream.

---

## 4. Where the bytes would travel — and one more correction

`internal/adbwire` already has every primitive this needs. `OpenService` and
`ShellStream` return a raw duplex `*Stream` with `Read`/`Write`/`CloseWrite`/
`SetDeadline`; `Sync` pushes the server jar; `WithTLS` and
`WithAdmissionPreamble` carry the fence.

**The fence proxy's "client half" is not missing.** `docs/design/fence-proxy.md`
said it was; that document was stale and has been corrected in this same change.
`internal/adbwire/doc.go:70` names `WithTLS` and `WithAdmissionPreamble` as
exactly that client half, `internal/api/server.go:112` installs them
unconditionally, `FARM_FENCE_CLIENT_CERT/KEY/CA` exist in `internal/config`, and
`REQUIREMENTS.md` SEC-04 has said "Both halves ship" all along. A duplex device
stream **already survives the proxy**: it reads the `fence:v1` preamble, admits
per host-protocol frame against the polled `fence_floor`, then goes opaque and
splices.

What genuinely does not exist:

- **A byte-stream surface anywhere.** `internal/node` exposes five JSON routes
  with 8–64 KiB body caps. A frame may not travel through it: that would put
  bulk data on the same bearer token that cuts power to rack ports.
- **Permission to open the service.** `internal/fenceproxy/proxy.go:652` gives
  the maintenance class exactly `Device: []string{"reboot:"}`. Today a
  screen or input service is refused with `OutcomeRefuseService`. The api also
  dials *maintenance* class; a screen stream needs *lease* class, which widens
  its reach from one service to any device service on a leased phone.
- **Two sockets, two admissions.** One ADB transport carries one service, so
  screen and input are two connections, each with its own preamble.
- **Nothing may live in `internal/adbwire`.** `TestPackageCannotReachAllocation`
  fails the build on the words lease, fence, holder, quarantine, allocation in
  any production file there, and the package may not import `pgx`,
  `database/sql`, `internal/lease`, `internal/reaper` or `internal/scheduler`.
  A guard test also forbids `internal/fenceproxy` from importing `net/http`.

---

## 5. The real blocker is not the transport. It is tenant scoping.

A framebuffer is the first thing this codebase cannot scope.

Every existing tenant mask nulls **named lease-identity fields** on a structured
object (`internal/api/fleet.go:128-134`). The rule is deliberately generous in
the other direction — a device is never hidden, because "a tenant that cannot
see a device cannot understand why it is queueing for one". **A framebuffer
offers nothing to null.** It shows whatever is on the screen, including another
tenant's app, their credentials, their data.

The codebase has faced this exact shape once: bulk shell output. The answer was
not a cleverer mask. **Both the read and the write were made operator-only**,
with a guard test pinning it.

Two consequences a design must accept:

1. `tenant("GET …/screen")` **fails the build** unless its handler body
   literally contains `tenantScope(` or the route is added to
   `tenantReadAllowlist` with a written reason. There is no third option.
2. The credential problem is already solved once and cannot be reused. The
   token may never appear in a query parameter — a written rule in two places —
   and `stream_ticket.go` exists solely to honour it. But `redeem` refuses
   anything that is not `GET` on exactly `/api/v1/stream`, counting it as
   `misrouted` because **that is what a leaked ticket looks like**. A second
   ticket kind is needed, not a widened one.

And the audit log cannot express this at all: `farm.audit_log` is one row per
point-in-time act. "A human held this device from T1 to T2" is answerable with
open/close rows. "What did they *do* with it" is not, and a design that implies
otherwise is lying to an operator.

---

## 6. Three designs, judged

Three independent designs, scored 0–10 on invariant safety, reuse of existing
mechanism, operational honesty, and implementability.

| | inv | reuse | honest | impl | total |
|---|---|---|---|---|---|
| **The Booked Session** — empty-spec job + deadline | 8 | 9 | 8 | 5 | **30** |
| **The Attended Job** — a new `interactive` step kind | 6 | 9 | 7 | 6 | 28 |
| **Attach, don't hold** — new `farm.control_sessions` table | 6 | 9 | 8 | 4 | 27 |

All three agree on the one thing that matters: **when the tab closes, nothing
happens.** Not one write. `heartbeat_at` keeps advancing on the jobrunner's
wall-clock timer, the witness marker keeps refreshing, the device stays held.
That is right because a closed tab is indistinguishable from a dropped VPN, a
laptop lid, a Wi-Fi handoff, a corporate proxy's idle timeout, or a tab the
browser discarded under memory pressure — which is #663's exact shape.

**The recommendation is the Booked Session's shape**, because it is the one the
tree already documents in three places and needs **no migration at all**: no new
column, no new table, no change to the `release_reason` CHECK.

But the judges found real holes, and two are worth carrying forward.

### The clock nobody wrote

The two step-kind designs put the human's session inside a step. Every step runs
under `context.WithTimeoutCause(ctx, r.stepTimeout(spec, st), ErrStepTimeout)`
(`internal/runner/runner.go:666`), and a step error sets
`out.ReleaseReason = lease.ReasonFailed` (`runner.go:721`). The code's own
comment is the giveaway:

> Step timeout, or something no retry could fix. This is the step ending badly,
> which is a job failure — not a lease failure.

For an automated job that reasoning is correct and the ending is legitimate:
the job said so. **For a human session it is a clock the human did not write,
wearing the job's clothes.** The invariant permits "when the job says so"; it
does not permit "when a timeout the system chose says so" merely because the
system spelled it as a job.

This is the sharpest argument for the empty-spec shape: with no steps, there is
no step timeout.

### The extend route is the designed-in path back to #663

Every design wants `POST /api/v1/jobs/{id}/deadline`, because `max_runtime` is
measured from `acquired_at` and nothing moves it — a human whose two hours
expire mid-repro loses the device and re-queues. STF has the same gap and has
had it open since 2022.

An extend route is defensible: it writes `farm.jobs.max_runtime`, which is *the*
user-supplied clock. But shrink its grain, drop its audit row, or let the page
call it on a timer, and **presence silently becomes tenure again** — after which
a dropped Wi-Fi ends a session. No guard test can catch that. If this is built,
it must be a deliberate human act with a written reason, never a keep-alive.

### The bill, stated plainly

A closed tab holds a device until the deadline. On a small farm with generous
defaults, one forgotten tab idles a phone for an hour while people queue. This
is not a defect in the design; **it is the price of refusing connectivity
endings**, and any mitigation that measures presence is the defect coming back.
The honest mitigations are: make the deadline visible and small by default, make
the held-but-unattended state loud in the UI, and give operators a cheap audited
revoke — which already exists.

---

## 7. What I would build, in order

1. **Correct the record first.** The README's #663 summary, and the
   `fence-proxy.md` status (done in this change). A design that argues from a
   stale document is how this investigation started.
2. **Open the service.** Add the scrcpy service to the fence proxy's class
   policy and decide whether the api dials lease class or a new narrower one.
   Nothing works before this and it is where the security argument lives.
3. **One device, one direction, no browser.** Push the vendored
   `scrcpy-server` with `adbwire.Sync`, start it with `ShellStream`, parse the
   framing, write H.264 to a file. Play the file. This is the whole transport
   risk, and it costs a day.
4. **Operator-only screen route**, with a second ticket kind. Accept
   operator-only; do not invent a framebuffer mask.
5. **Input**, last, and behind the same gate. A tap is a write to a phone
   somebody may be holding.
6. **Only then** decide whether tenants ever get this, and what the audit row
   has to say for that to be defensible.

## 8. What I would not build

- **Anything that measures presence.** No websocket liveness, no "are you still
  there", no idle timer. That is #663.
- **A framebuffer tenant mask.** There is nothing to mask. Operator-only is the
  answer this codebase already reached once.
- **A stream through `internal/node`.** Bulk bytes on the token that cuts VBUS.
- **minicap.** Not because it is dead — it is not — but because JPEG frames are
  what saturated the tunnel in #663.
