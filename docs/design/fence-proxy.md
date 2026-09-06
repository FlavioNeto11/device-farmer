# The mTLS fence proxy

Status: **designed; integrated on the host; the client half is a separate
change.**
Code: `internal/fenceproxy/proxy.go`, `internal/fenceproxy/proxy_test.go`
(the proxy); `internal/node/fence.go`, `internal/node/fencesource.go` (the
host integration: `farmd node` serves it when given TLS material). Section 14
states exactly what is enforced today and what is not.

---

## 1. What is actually broken

The fence works in two of the three places it needs to work.

**In PostgreSQL it works.** Every mutating lease function matches on
`(id, fence)`: `farm.lease_renew`, `farm.lease_witness`, `farm.lease_release`
(`migrations/00002_lease.sql`). `farm.devices.fence_floor` is raised by
`nextval('farm.fence_seq')` on release and on reclaim, and the insert trigger
`farm.trg_leases_sync_device` raises it with `GREATEST` when a lease is granted.
A holder whose fence has fallen below the floor cannot renew, cannot witness and
cannot end the lease that replaced it.

**In the client it works, by cooperation.** `internal/lease` treats zero rows
from `lease_renew` as `ErrFenced`, which is documented as terminal: abort the
job, close every ADB socket, write nothing further.

**At the device it does not work at all.** A client that already holds an open
ADB connection keeps holding it. A client that dials `127.0.0.1:5037` on the
host — or the host's port 5037 from anywhere the network allows — never
presented a fence to begin with. Nothing between the client and the handset has
ever heard of a fence. `GET /api/v1/capabilities` reports this accurately:

    "Fence enforcement at the resource": "not_built"

Several comments in the tree already describe a proxy that does not exist, and
they are the reason this document has to be written before any code is:

| Location | Claim |
|---|---|
| `internal/lease/types.go:90` | "refused at the database and again at the host proxy" |
| `internal/lease/types.go:118` | "compared against `devices.fence_floor` at the host proxy" |
| `internal/lease/types.go:201` | "Any socket still carrying `OldFence` is now refused at the host proxy" |
| `internal/lease/store.go:304` | "any socket still carrying the old fence is refused at the host proxy" |
| `internal/api/leases.go:655`, `:723` | "a socket the old holder still owns would be accepted at the host proxy" |
| `internal/ctl/commands.go:1681` | "fenced out at the host proxy" |
| `internal/config/config.go:313-340` | the startup assertion `SlotRearm > SelfFenceTimeout + FenceSafetyMargin` |
| `README.md:250` | "the **mTLS fence proxy** ... is unbuilt" |

Section 12 lists which of these this design changes, and how.

---

## 2. The invariant this proxy must not break

> A lease ends when the job says so, when a user-written deadline elapses, or
> when a human takes it back. **Nothing else.**

The proxy is a new component sitting on the data path of every ADB byte in the
farm. That is precisely the position from which DeviceFarmer/STF #663 was
committed: a transport-level observation acquired a code path to an allocation
decision. So the proxy is built to make that path *inexpressible*, not merely
discouraged:

* `internal/fenceproxy` imports no database driver, no HTTP client and not
  `internal/lease`. Its only channel to the control plane is the one-method
  `FenceSource` interface, which **reads**. `TestFenceSourceIsReadShaped`
  asserts the method count is exactly one, because the change that would make a
  refusal able to *do* something is the addition of a second method.
* `TestPackageCannotEndALease` parses every production file in the package with
  `go/parser` and fails on any *identifier* containing `release`, `reclaim`,
  `revoke`, `expire` or `deallocate`. It walks the AST rather than the text, so
  comments may explain what is barred (this is the same exemption `doc.go` gets
  in `internal/adbwire`, generalised properly). Certificate expiry is called
  **lapsed** throughout this package for exactly that reason.
* `TestRefusalChangesNoFenceState` drives a refusal and a mid-transfer teardown
  and asserts the fence source's floors are identical afterwards. A proxy that
  "helpfully" told the control plane "I refused this, please clean up" would
  fail it.

**Refusing traffic and ending an allocation are different acts.** The proxy
performs the first and has no vocabulary for the second. A refused connection
yields `*RefusedError`; there is no release reason on it, and no function in
this package can produce one.

---

## 3. Shape

    jobrunner / recovery / watchdog / enroll / operator ctl
              |
              |  TLS 1.3, mutual auth, over the farm network
              v
    +---------------------------------------+
    |  fenceproxy   (on the device host)     |
    |    identity  <- client certificate     |
    |    claim     <- preamble frame         |
    |    fact      <- FenceSource poll       |
    +---------------------------------------+
              |
              |  plain TCP, loopback only
              v
    adb server  127.0.0.1:5037
              |
              |  USB
              v
    handset

The host's ADB server binds loopback only and is unreachable from the network;
the proxy is the only thing that can reach it. That is a deployment property,
not something the proxy can enforce, and it is stated in section 11 as a
precondition rather than as a feature.

### 3.1 Why identity and authorisation are carried separately

The client certificate answers **"who are you"**. The preamble frame answers
**"what do you claim to hold"**.

Putting the fence *in* the certificate is tempting — mTLS would then be the
fence — and it is wrong. A fence changes every time a device changes hands; a
certificate changes when a key rotates. Fusing them makes every fence bump a
PKI operation and makes a PKI outage a device outage. Worse, a six-hour lease
would need a six-hour certificate, which is the opposite of what short-lived
credentials are for.

So the wire is:

    <TLS handshake, mutual>
    -> 002Cfence:v1 class=lease devpath=usb:3-1.4 fence=41207
    -> 000Chost:version                    (the ordinary ADB host protocol)
    <- OKAY0004002b

The preamble uses ADB's own framing (four hex digits of length, then the
payload) so a client needs no second parser, and it is **not acknowledged**.
The refusal, when there is one, lands on the *service* request as a well-formed
`FAIL` frame — a reply every ADB client already knows how to read. That saves a
round trip and means a client sees a refusal as a refusal rather than as a
mysterious connection reset. At 3am that difference is the whole incident.

The `class` word in the preamble is **advisory**. The authoritative class comes
from the certificate (section 9), so a lease client cannot promote itself to
maintenance by editing one word.

### 3.2 The client side

The frame above is written by `internal/adbwire`, behind two options on
`Client`: `WithTLS(*tls.Config)` and `WithAdmissionPreamble(func() (class,
devpath string, token int64, ok bool))`. Every call path dials through one
function, so the frame cannot be skipped by a call that forgot; it is written
right after the handshake, in the same four-hex-digit framing as everything
that follows, and nothing is read back.

Three decisions on that side are worth stating, because each one is the
answer to a question a reader will otherwise ask.

**The preamble is sent only on a TLS connection.** The frame is addressed to
the proxy, and the proxy is the only peer reachable over TLS; a bare ADB server
would read it as a host service and `FAIL` it, breaking every call. So over
plain TCP the option is inert and the bytes on the wire are exactly what they
were before it existed. That is what lets every role in `cmd/farmd` install
both options unconditionally and switch the proxy on with a certificate alone
— `FARM_FENCE_CLIENT_CERT`, `_KEY`, `_CA`, all three or none, parsed at boot —
rather than by every call site remembering to pair the two.

**`adbwire` cannot spell the word `fence`.** The package is under a vocabulary
barrier (`TestPackageCannotReachAllocation`) that scans every production file,
string literals included, for the nouns of the thing a socket error must never
reach. The option is therefore `WithAdmissionPreamble`, the magic word is
assembled from two halves in `client.go`, and the one class that carries a
device token — `lease` — is named by `internal/jobrunner`, which owns the
binding, not by the transport. The watchdog and the recovery ladder announce
`maintenance`; `recovery/adbactuator.go` is under its own barrier and takes the
options as an opaque value from `cmd/farmd`, so it never names the class
either.

**`Placement.Fence` now reaches the wire.** `jobrunner.Dialer` is
`Dial(endpoint, devpath string, admission int64)`, and the default dialer
builds one `adbwire` client per placement whose preamble announces
`class=lease devpath=<placement> fence=<placement's fence>`. The per-endpoint
client cache it replaced would have needed an eviction policy tied to the
lease's lifetime; a client is a dialer, not a connection, so building one per
placement costs a struct and no socket.

What the client half does **not** change is the sentence every ending path
prints. With the proxy opt-in on both sides, "refused at the host proxy" is
true on a host running one and false on a host without, so the revoke response,
the `ctl` confirmation banner and the comments in `internal/lease` now say the
conditional thing: the device's fence floor is raised past the revoked fence,
so the database refuses every write at the old fence; on a host running the
fence proxy the old fence is also refused at the ADB socket, and on a host
without one the holder is relied upon to honour it. One site keeps the old
wording on purpose: the comment inside `farm.lease_release` at
`migrations/00002_lease.sql:434-435` is part of an applied migration, and
applied migrations are not edited. Read it as the design intent it was written
as; the conditional truth lives in the Go prose beside the callers.

`/api/v1/capabilities` reports the same conditionality from the only vantage
point a process has. "Fence enforcement at the resource" is `enabled` when the
process holds a client certificate and `unavailable` when it does not, naming
both halves of the knob — and in both states the detail says enforcement is per
host, because a process can see its own side of the wire and cannot see which
hosts run a proxy.

---

## 4. How the proxy learns the fence floor

**Not per packet. Not per connection. Once per host per poll interval.**

A proxy that consults Postgres on every packet is a per-byte database round
trip: a 200 MB push at a 64 KiB chunk size is 3,200 queries, and twenty-eight
devices on one host doing that at once is a self-inflicted denial of service on
the one database every lease in the farm lives in.

One goroutine per proxy runs:

```sql
SELECT s.adb_devpath, d.fence_floor
  FROM farm.devices d
  JOIN farm.slots   s ON s.id = d.current_slot_id
 WHERE d.host_id = $1
   AND s.adb_devpath IS NOT NULL;
```

every `PollInterval` (default **2s**), and hands the result to `Cache.Apply`.
That is one query per host per two seconds — for a 60-host farm, 30 queries a
second against an index, forever, regardless of how much ADB traffic flows.

`Cache.Apply` is called **on success only**. There is no code path from a poll
error to a cache mutation and none from a poll error to a watcher firing; that
is the structural form of the failure policy in section 5, and
`TestPollErrorsChangeNothing` asserts it.

### 4.1 How fresh the knowledge must be, and why that number

`Policy.MaxStaleness` is the age past which a floor observation may no longer
admit a **new** connection. It is the same quantity `internal/config` already
calls `FARM_NODE_SELF_FENCE_TIMEOUT`, default **20s**, and the number is not
free — `config.Validate` already refuses to start unless

    FARM_SLOT_REARM  >  FARM_NODE_SELF_FENCE_TIMEOUT + FARM_FENCE_SAFETY_MARGIN

Why that inequality is the right bound is worth spelling out, because it is what
makes serving from cache safe rather than merely convenient:

> When a floor moves, the same transaction parks the slot:
> `farm.lease_release` and `farm.lease_reclaim` both set
> `slots.rearm_at = now() + p_rearm`, and `farm.lease_acquire` will not
> allocate a slot whose `rearm_at > now()`. **For the length of the rearm
> window, the device belongs to nobody.** A stale view admitting the previous
> holder during that window therefore cannot cross two holders' traffic on one
> phone — the only thing it permits is a holder that has been fenced continuing
> to poke a device that no one else has yet been given.

Serving from a cache is wrong for the length of the cache. This design makes the
length of the cache shorter than the window in which being wrong costs nothing.

With `PollInterval = 2s` and `MaxStaleness = 20s`, the proxy tolerates nine
consecutive failed polls before it starts refusing new connections.

### 4.2 A stale view can still prove fencing

`fence_floor` is **monotonically non-decreasing**: the insert trigger uses
`GREATEST(fence_floor, NEW.fence)`, and release, reclaim and operator revoke all
assign `nextval('farm.fence_seq')`, which only goes up.

Therefore a *stale* observation showing `claim.fence < floor` is still a
**fact**: the floor can only have risen since it was taken. Being fenced is a
one-way door.

So the proxy refuses a fenced claim **regardless of view age**, and the age
budget only ever gates the *permissive* direction. Concretely: during a database
partition, clients that were already fenced before the partition started stay
fenced for its entire duration, with no upper bound. Enforcement does not
evaporate when the database does; only *new* facts do.

(The one way this premise breaks is a manual `setval('farm.fence_seq', ...)`
backwards. That would also break `lease_witness`, which compares against the
same floor. It is a schema-level assumption, and it is written down here so that
anyone tempted to reset the sequence can see what depends on it.)

---

## 5. What happens when the proxy cannot reach Postgres

This is the decision that decides whether the proxy is safe, so it gets its own
section and an explicit answer for each direction.

### 5.1 The two things that must never be conflated

* **"Postgres says a higher floor exists"** is a *fencing fact*. It was read
  successfully. It is monotone, so it never becomes untrue. It justifies
  refusing a new connection and tearing down a live one.
* **"I cannot reach Postgres"** is *not a fact about any fence*. It is a fact
  about a socket. It justifies declining to *start* something new. It justifies
  nothing else.

This is the same distinction `internal/lease` already draws between zero rows
from `lease_renew` (terminal `ErrFenced`) and an error from `lease_renew`
(transient). Collapsing it in either direction recreates #663. The proxy is
built by the same rule: `Decision.Terminal` is true for exactly one outcome —
`refuse_fenced` — and never for `refuse_unknown`.

### 5.2 New connections: fail **closed**, after a budget

| view | claim vs floor | outcome |
|---|---|---|
| fresh (age <= `MaxStaleness`) | `fence >= floor` | **admit** |
| fresh | `fence < floor` | **refuse_fenced** — terminal |
| stale (age > `MaxStaleness`) | `fence < last known floor` | **refuse_fenced** — terminal (4.2) |
| stale | `fence >= last known floor` | **refuse_unknown** — retryable |
| never observed | — | **refuse_unknown** — retryable |

`refuse_unknown` is reported to the client as a `FAIL` frame whose reason says
so in words, and `Decision.Retryable` is true. This matters more than it looks:
in this codebase `adbwire.Client` "holds no connection of its own and opens one
per call", so refusing new connections stalls every job on the host. That cost
is acceptable **only because it is a stall and not a verdict** — the client
retries, its lease is untouched, its renewal runs on a different wire, and
`farm.reaper_arm` refunds the control-plane gap so the outage costs the tenant
zero lease budget. A client that mistook this refusal for a fence would abort a
six-hour job over a database blip, which is why the refusal is a distinct
outcome and not a shared "denied".

### 5.3 In-flight connections: never fail closed on blindness

**An in-flight connection is torn down only by a fencing fact. Blindness never
tears one down, at any duration.**

The asymmetry with 5.2 is the entire design, and it is not a compromise:

* Refusing to *start* a connection costs a retry. The work in flight is zero.
* Severing a connection *mid-transfer* costs the transfer, probably the step,
  and — for a job that cannot resume — the run. A farm-wide sever triggered by a
  database blip is DeviceFarmer/STF #663 arriving through the front door,
  differing from the original only in which component made the mistake.

The two acts have different costs, so they do not get the same rule. Every other
component in this system already behaves this way: `farm.reaper_arm` responds to
blindness by *quiescing*, not by acting harder; an error from `lease_renew` is
explicitly not a fence; `internal/adbwire`'s tracker emits no snapshot when its
socket drops rather than synthesising absence. A proxy that severed live work on
blindness would be the single component that does the opposite.

Structurally: `Cache.Apply` is the only thing that fires a watcher, and it is
only ever called with a successfully read snapshot. There is no error path into
teardown to review, because there is no error path into teardown.

### 5.4 What that costs, and where the cost is paid instead

Fail-open on in-flight has a real residual: a proxy partitioned from Postgres
for longer than `FARM_SLOT_REARM` can still be forwarding an old holder's bytes
at the moment `lease_acquire` hands the same device to a new holder. Two tenants
then share one phone — the exact outcome `config.Validate` refuses to start
over.

The fix is not to sever live work. **The fix is to refuse the allocation**,
because a crossing requires an allocation and allocation is a database act:

```sql
-- REQUIRED COMPANION CHANGE, in farm.lease_acquire's phase-2 predicate,
-- alongside the existing `AND s.rearm_at <= now()`:
AND EXISTS (SELECT 1 FROM farm.component_heartbeat h
             WHERE h.component = 'node:' || d.host_id
               AND h.beat_at > now() - $proxy_freshness)
```

A host whose proxy cannot vouch for its fences is a host whose devices must not
be handed to anyone new. This puts the bound exactly where every other
allocation rule in this system already lives, and it converts the failure mode
from *live work destroyed* into *capacity temporarily unavailable* — which is
the trade this codebase makes everywhere else.

Two notes for whoever writes it:

1. `farm_scheduler` is not currently granted `SELECT` on
   `farm.component_heartbeat` (`migrations/00002_lease.sql`). It will need to
   be. It must **not** be granted to `farm_reaper`: proxy liveness is a capacity
   signal, and giving reclamation a second thing to be blind about is how the
   health firewall gets eroded.
2. The predicate belongs in `lease_acquire` only. It must never appear in
   `lease_reclaim`, `lease_release` or `lease_expire_max_runtime`. A proxy that
   stopped beating must not end anything.

**Until that predicate exists, the residual window is
`(partition duration - FARM_SLOT_REARM)`, and it is real.** It is smaller than
the window that exists today — today there is no proxy at all — but it is not
zero, and no amount of proxy-side cleverness closes it.

### 5.5 The proxy's own liveness is not on the renewal path

The proxy writes `farm.component_beat('node:<host>')` on its own timer. That
heartbeat is an input to *allocation* (5.4) and to the dashboard. It is never an
input to anything that ends a lease. `internal/node/agent.go` already states
this rule for the host agent's heartbeat; the proxy inherits it verbatim.

---

## 6. Tearing down a connection that goes stale mid-transfer

### 6.1 The guarantee

> **No byte written to the proxy after a fencing fact reaches it is delivered to
> the device.**

Not "the connection closes soon". Not "the connection closes within a timeout".
The client-to-device direction passes through a `gate`: a mutex and a bool.
`gate.Shut()` takes the lock, sets the bool and closes the upstream socket;
`gate.Write` takes the same lock and refuses if the bool is set. A write already
in progress when the fact arrives completes — it is one `io.Copy` buffer,
already handed to the kernel — and **no write begins after it**. That is exact
rather than probabilistic, and it costs one uncontended mutex per 32 KiB.

The device-to-client direction is a plain copy and is not gated. Bytes flowing
*from* a phone to a fenced client are a privacy question, not a safety one, and
the socket close that follows within microseconds ends them anyway.

### 6.2 The sequence

1. A poll returns a snapshot in which `floor > claim.fence`.
2. `Cache.Apply` fires the connection's watcher. This is the only thing that
   ever fires it.
3. `gate.Shut()` — the guarantee in 6.1 takes effect here, before any socket is
   touched.
4. The upstream (ADB-server-side) socket is closed. The ADB server sees EOF
   mid-request and abandons whatever it was doing.
5. The client socket is closed, after a best-effort TLS `close_notify`.

### 6.3 The proxy says nothing, and that is correct

Mid-stream the connection is carrying sync framing, or shell-v2 packet framing,
or raw bytes. The proxy deliberately does not parse any of it — a proxy that
parsed 200 MB streams to find a frame boundary would be a second implementation
of three protocols and a permanent source of desynchronisation bugs. So it has
**no vocabulary in which to say "you are fenced"**, and it does not invent one.

The client sees an unexpected EOF. `internal/adbwire` classifies that as a
`*TransportError` of `KindEOF`, which by this project's own rules is **not** a
fencing verdict and must not end anything. The client learns it is fenced from
its next `lease_renew`, on a different wire, where zero rows is already terminal
and already tested.

That division is deliberate: **the proxy stops bytes; Postgres pronounces
fences.** A proxy that pronounced would be a second source of truth about
allocation, and the second source is always the one that is wrong at 3am.

### 6.4 What a half-written sync SEND leaves on the device

The ADB sync protocol's `SEND` is: `SEND <path>,<mode>`, then `DATA` chunks,
then `DONE <mtime>`, then the daemon replies `OKAY`. `adbd` writes into the
destination path **as chunks arrive** — it does not stage to a temporary file.
So a `SEND` cut at step 4 above leaves, on the handset:

* a **truncated regular file at the destination path**, containing every chunk
  that arrived;
* with the permission bits the client requested, because `adbd` applies the mode
  at create time;
* with **no mtime stamp**, because the mtime travels in `DONE` and `DONE` never
  arrived;
* and no record anywhere on the device that it is incomplete.

`test/fakeadb`'s `doSend` models the *safer* semantics for a client-initiated
abort — "Nothing is stored: a half-written file that looked complete would be
worse than none" — but that applies to the `SyncFail` path, not to a severed
socket, and no real `adbd` behaves that way.

Three consequences, and only two of them are already handled:

1. **A truncated APK is harmless.** `pm install` on a truncated zip fails loudly
   and immediately. This is the good case and needs nothing.
2. **`internal/artifacts` is safe by construction.** `farm.device_artifacts` is
   marked `present` only *after* a push succeeds; a torn push leaves the row
   `failed`, and `EnsureOnDevice` pushes again for the next holder, overwriting
   the truncation. No change needed.
3. **The job scratch directory is not cleaned, and it should be.**
   `internal/runner/steps.go:1399`, `prepareCommand`, is
   `mkdir -p <workDir> || exit 1; ...`. It creates; it does not clear. A
   truncated file left at a deterministic path by a fenced holder is therefore
   still there when the next job runs in the same directory, and a step that
   does `[ -f x ]` (`steps.go:795`) will find it and believe it.

   **REQUIRED COMPANION CHANGE:** `prepareCommand` should clear the scratch
   directory for a *fresh* acquisition — but must **not** clear it on a
   re-attach, because `AcquireResult.Reattached` explicitly means "the device is
   dirty with your own prior state, resume from `jobs.checkpoint`", and wiping
   there would destroy the very state checkpointing exists to preserve. The
   distinction is already available at the call site: `runner.go:752` runs inside
   a run that knows whether it re-attached.

This is the honest answer to "what does a half-written SEND leave on the
device": *a truncated file that nothing currently removes*. The proxy creates the
condition; the runner has to handle it; the two changes ship together or the
proxy makes the farm slightly dirtier than it found it.

---

## 7. Reaching a device that holds no lease

The recovery ladder and the watchdog **must** be able to reach a device with no
lease — an unallocated phone that has gone `offline` is exactly the phone the
ladder exists to repair, and it carries no fence because there is nothing to
fence.

A privileged bypass is a **second credential class**, not an exception, and it
is dangerous in a specific way: `reboot:` and an unrestricted `shell:` on every
handset in the rack is a larger capability than anything the lease class has. So
it is bounded three ways.

### 7.1 The class comes from the certificate, never from the client

`ClassMaintenance` is asserted by a URI SAN on the client certificate (section
9). The preamble's `class=` word is advisory and is overridden by the
certificate on every connection. A lease client cannot promote itself.

### 7.2 The whitelist is over *service strings*, and it is exact

A maintenance connection presents no fence, so the *only* thing standing between
it and a root shell is which service strings it may open. The whitelist is
therefore **exact-match, never prefix-match**, for one reason:

> A shell service string is an arbitrary command line. Any prefix rule over it
> is bypassable with `;`, `&&`, a newline or `$( )`. `shell:getprop ro.x` and
> `shell:getprop ro.x; rm -rf /sdcard` share a prefix.

The shipped default admits exactly:

    host:version                         host:features
    host:devices                         host:devices-l
    host:track-devices                   host:track-devices-l
    host-serial:<devpath>:get-state      host-serial:<devpath>:features
    host-serial:<devpath>:get-serialno   host-serial:<devpath>:get-devpath
    host-serial:<devpath>:reconnect      host-serial:<devpath>:reconnect-offline
    host-serial:<devpath>:detach         host-serial:<devpath>:attach
    host:transport:<devpath>             reboot:

`<devpath>` is the one templated element, and it is matched against `adbwire`'s
own `usb:[0-9A-Za-z][0-9A-Za-z._-]*` shape, so the template cannot smuggle a
second field past the parser. `host:kill` is **not** on the list and must never
be: it stops the ADB server for every device on the host, including the ones
under live leases.

The same rule is enforced one level down, on the regular expressions in
`ServiceRules.DevicePatterns`: a pattern must match the **whole** service
string, and `allows` checks the match span rather than trusting the pattern to
carry `^` and `$`. An unanchored pattern is the prefix hole wearing a different
hat — `getprop ro\.serialno` would otherwise admit
`getprop ro.serialno; su` — and that is not a mistake to leave to whoever writes
the config. `TestWhitelistIsExactNotPrefix` puts a deliberately unanchored
pattern on a whitelist and asserts the extension is still refused.

That list covers `internal/recovery/adbactuator.go` completely — tiers 1, 2 and
5 are `Control` verbs, tier 7 is `reboot:` — and `internal/watchdog`, which uses
only `host:track-devices-l`.

### 7.3 Enrolment gets its own class, because enrolment needs a shell

`internal/enroll` reads properties off a brand-new phone with
`identity.go:293`'s `probeCommand` and `brand.go:63`'s `brandReadCmd`. Both are
package-level values built from constants — they are *fixed literals at runtime*
— so exact matching works on them unchanged, which is a happy accident of how
they were already written.

`brand.go:74`'s `brandWriteCmd(uid)` is the exception: it interpolates a uid. It
is admitted by a compiled pattern whose only variable region is `[0-9a-f-]{36}`,
so the shape of the command is fixed and the uid cannot carry a metacharacter.

`ClassEnroll` gets a separate certificate and a separate whitelist from
`ClassMaintenance` because it is the only class that may open a `shell:` at all,
and the blast radius of the two should not be pooled.

### 7.4 What the proxy deliberately does not decide

A maintenance connection may run a whitelisted verb **regardless of any live
lease on the device**. That is intentional: tiers 3 and 4 exist to repair a USB
link *under* a live lease, with the clock still running and the fence unmoved
(`internal/node/agent.go` says so at length).

Whether a given rung is permitted against a given lease's `disruption_policy` is
a **blast-radius decision, and it is already made** in `internal/recovery`
against `farm.recovery_tiers` and the lease row. The proxy has no policy table,
no lease row and no business re-litigating it. Duplicating the check here would
produce two answers that drift.

The residual is stated plainly: **a stolen maintenance credential can reboot
every phone on a host.** Mitigations, in order of importance: a separate issuing
intermediate so the credential cannot be minted by whatever mints lease certs; a
short lifetime (section 9); and an INFO-level audit line on every maintenance
admission carrying subject, devpath and the exact service string — the one place
in this design where logging is a control and not an observation.

---

## 8. The connection lifecycle

The proxy is asymmetric on purpose:

* **device to client** is spliced immediately and never parsed. The ADB server's
  `OKAY`/`FAIL` replies flow straight through, which is why the proxy never has
  to model the server's state machine.
* **client to device** is read frame-by-frame in host framing, and each frame is
  admitted, until the connection goes opaque.

```
  read preamble frame                    -> Claim (advisory class, devpath, fence)
  loop, at most MaxHostFrames times:
      read frame                         -> service string
      Admit(identity, claim, service, bound, view, now, policy)
          refused -> write FAIL(reason); close; done
      dial upstream on the first admit
      forward the frame verbatim
      if service is host:transport:<dp>  -> bound = dp; keep looping
      else                               -> stop looping; the stream is opaque
  splice both directions, watching for a fencing fact
```

`host:transport:<devpath>` is the one service that does **not** end framing
mode: the client's *next* message is the device service (`sync:`,
`shell,v2,raw:...`, `reboot:`), and that message is what actually has to be
checked. This is where a maintenance whitelist would otherwise be trivially
bypassable — get admitted on the transport switch, then send anything. The loop
is bounded by `MaxHostFrames` (default 4) so a client cannot hold the proxy in
framing mode indefinitely.

For a lease-class connection, admission additionally requires that the service
targets the *claimed* devpath: a client holding a lease on `usb:3-1.4` cannot
address `usb:3-1.5`, and cannot open a device service at all before a transport
switch has bound the connection to its own devpath.

---

## 9. Certificates

### 9.1 Issuance

* One **farm CA**, offline, with two intermediates: `workload` (lease clients)
  and `service` (maintenance and enrolment). The split is what makes 7.4's first
  mitigation real — compromising whatever mints lease certificates does not mint
  a maintenance certificate.
* **Server certificates** are issued to hosts. SAN is the host's DNS name and
  its address. 90-day life, rotated at 60 days.
* **Client certificates** are issued to *workloads*, not to leases. `CN` is the
  audit subject and is written straight into the log line. The class travels in
  a URI SAN, `farm://<class>/<service>` — `farm://lease/jobrunner`,
  `farm://maintenance/recovery`, `farm://enroll/enroller`. A URI SAN is used
  rather than an OU because SANs are the field TLS libraries are built to
  constrain, and it composes with SPIFFE for a deployment that already has one.
* **24-hour life, renewed every 8 hours.** Three renewal opportunities before
  expiry.

### 9.2 Rotation

`tls.Config.GetCertificate` and `GetClientCertificate` are supplied as functions
that re-read from disk when the file changes. Certificates are **never** loaded
once at startup and cached for the process lifetime.

The reason is a rule, not an optimisation: **restarting the proxy to pick up a
certificate would sever every live connection on the host.** A PKI operation
must not be a data-path event. Any rotation scheme that requires a restart is
rejected on those grounds alone.

### 9.3 Expiry mid-lease — the case that matters

Go's TLS stack checks peer certificate validity **at handshake time only**. It
does not re-check during a connection. That gives the right behaviour for free,
and it is worth stating as a deliberate choice rather than as an accident:

* **A connection whose client certificate lapses mid-transfer stays up.** Same
  rule as 5.3: a credential clock is not a fencing fact, and a six-hour transfer
  must not die because a certificate crossed a boundary.
* **A new connection with a lapsed certificate is refused.** Correct — but since
  `adbwire` opens a connection per call, "refused at the next call" is
  effectively "refused immediately". So the client's renewal loop is
  load-bearing, and its failure must behave like 5.2: a stall, never a verdict.

Two constraints follow, and both are numbers someone has to get right:

1. **Certificate life must exceed the longest survivable control-plane
   outage.** `farm.reaper_arm` refunds a control-plane gap and quiesces for the
   longest TTL it could have missed, so a lease *survives* a multi-hour outage by
   design. If the certificate that lets the holder reach its device does not
   survive the same outage, the refund buys nothing. A 24-hour life against an
   8-hour renewal period survives a 16-hour outage; that is the sizing rule.
2. **A renewal failure must never end a lease.** It makes new connections fail,
   which is retryable, and the holder keeps renewing its lease on a different
   wire the whole time.

### 9.4 Refusing readably instead of resetting

A handshake rejected for an expired certificate gives the client
`remote error: tls: bad certificate` and nothing else — no expiry time, no
subject, no advice.

So the proxy uses `ClientAuth: tls.RequireAnyClientCert` and does chain
verification itself in `VerifyPeerCertificate`:

* full `x509.Certificate.Verify` against the pool, with
  `KeyUsages: ExtKeyUsageClientAuth`;
* if it fails for **any** reason other than time, the handshake fails — no
  cryptographic latitude is taken here;
* if it fails **only** with `x509.CertificateInvalidError{Reason: x509.Expired}`,
  the handshake completes and the identity is carried through with its
  `NotAfter`, and `Admit` refuses it as `refuse_cert_lapsed` with a `FAIL` frame
  that names the instant it lapsed and says to renew and retry.

The connection is refused either way. The difference is entirely in what the
operator reads at 3am, which is why `Identity.NotAfter` is an input to the pure
admission function rather than a detail of the TLS layer.

---

## 10. The admission decision

```go
func Admit(req Request, view View, now time.Time, pol Policy) Decision
```

Pure: no context, no I/O, no clock of its own, no database. Every input is a
value. This is why the test suite needs no `DATABASE_URL` and no hardware, and it
is the property that makes the matrix in 5.2 reviewable as a table instead of as
a call graph.

Order of checks, and the order is deliberate — identity before claim before
target before fence, so that a client with a lapsed certificate is told about the
certificate rather than about a fence it may well still hold:

1. class known, else `refuse_identity`
2. certificate not lapsed, else `refuse_cert_lapsed` *(retryable)*
3. service parses, else `refuse_malformed`
4. `host:kill` and anything else outside every class's reach, else
   `refuse_service`
5. maintenance and enrolment: whitelist, then admit or `refuse_service`. **No
   fence is consulted, by design**
6. lease: claim well formed, else `refuse_malformed`
7. lease: service targets the claimed devpath, else `refuse_target`
8. lease: the fence matrix of 5.2

`Decision` carries `Terminal` and `Retryable` as separate fields rather than one
enum because the difference between them is the difference between "abort the
job" and "try again in a second", and a client that cannot tell them apart either
destroys work or retries a permanent failure forever. `Terminal` is true for
exactly one outcome, and a test asserts that over the whole outcome set.

---

## 11. Preconditions this design does not enforce

Stated as preconditions rather than dressed up as features:

1. **The host's ADB server must bind loopback only.** If `adb -a` is running on
   `0.0.0.0:5037`, the proxy is a door next to an open wall. This is a host
   configuration and the proxy cannot check it.
2. **The proxy and the ADB server must share a trust boundary** — same host,
   ideally the same process supervisor. There is no authentication on the
   loopback hop and there should not be.
3. **Clients must reach devices only through the proxy.** Nothing enforces that
   at the network layer here; a deployment does it with firewall rules.
4. **5.4's allocation predicate must exist** for the in-flight fail-open to be
   fully safe. Until it does, the residual window in 5.4 is real.
5. **`adbwire` needs a dial seam.** `WithDialer` takes a concrete `*net.Dialer`,
   so a client cannot today hand `adbwire` a connection that has already done a
   TLS handshake and sent a preamble. A
   `WithDialFunc(func(ctx, addr) (net.Conn, error))` option is the minimal
   change. `proxy_test.go`'s `sidecar` helper is a working model of exactly what
   that seam has to do.

---

## 12. What this design changes about the story already in the tree

The comments listed in section 1 promise a proxy. This design keeps every one of
them true, with one clarification and two corrections that belong to other
units.

**Kept as written:** `types.go:90`, `types.go:118`, `types.go:201`,
`store.go:304`, `leases.go:655`, `leases.go:723`, `ctl/commands.go:1681`. A
socket carrying a stale fence *is* refused at the host proxy — within one poll
interval in the normal case, and immediately for any new connection.

**Needed one clause — `internal/config/config.go`, the `FARM_SLOT_REARM`
assertion** (done with the host integration; the prose now says what section
14 says). The assertion's prose said the proxy "only discovers that its fence is
stale after its self-fence timeout elapses; until then it is still holding open
ADB sockets and still forwarding the old job's commands to the phone." Two
corrections:

* In the normal case, discovery takes one `PollInterval` (2s), not
  `SelfFenceTimeout` (20s). The 20s is the budget for *new* admissions while
  blind, not the latency of noticing a fact.
* When the proxy is blind it does **not** sever live sockets at 20s, and this
  design says it must not (5.3). The inequality the assertion enforces is still
  correct and still worth enforcing — it is now sufficient rather than necessary
  — but the sentence explaining *why* should point at 5.4's allocation predicate
  rather than at a teardown that will not happen.

**Needs a fix that is not in this unit — `farm.lease_expire_max_runtime`.** The
function in `migrations/00002_lease.sql` sets `state = 'expired'` on the lease
and nothing else. `farm.trg_leases_sync_device` then clears
`devices.current_lease_id`, but **it does not raise `fence_floor` and does not
set `slots.rearm_at`**, and no later migration in this tree does either. So on
the max-runtime path:

* the departed holder's fence still sits at the floor and still passes every
  check — including this proxy's;
* the slot is immediately schedulable, with no rearm quarantine at all.

Both premises this design leans on (4.1, 4.2) are therefore absent on exactly one
of the three ending paths, and it happens to be the path that fires on
long-running jobs — the ones with the most work to lose. The proxy cannot
compensate: it can only refuse what the floor tells it to refuse, and on this
path the floor was never moved.

The fix belongs in a migration, next to the other two ending paths:

```sql
-- in farm.lease_expire_max_runtime, mirroring farm.lease_release:
UPDATE farm.devices SET fence_floor = nextval('farm.fence_seq') WHERE id = <dev>;
UPDATE farm.slots   SET rearm_at    = now() + p_rearm           WHERE id = <slot>;
```

**Needs one word — `README.md:250` and `internal/api/capabilities.go:318`**,
once the integration lands: `"Fence enforcement at the resource"` moves from
`not_built` to `degraded` when no proxy is beating on a host and to `enabled`
when one is. It stays `not_built` until then, and this unit does not change it,
because the proxy is not wired in and a capability report that lies is worse than
one that says `not_built`.

---

## 13. What is built here, and what is not

**Built and tested** in `internal/fenceproxy`:

* `Admit` — the pure admission decision, exercised as a table over every row of
  5.2 and every outcome in section 10.
* `ParseService` — the ADB service-string classifier, including the
  `host:transport` case that section 8 turns on.
* `ParsePreamble` — the claim parser, which refuses what it cannot parse rather
  than defaulting.
* `Cache` — floor knowledge, freshness, and the watcher that fires on a fencing
  fact and on nothing else.
* `Session` — the section 8 lifecycle: framing mode, admission per frame, the
  `FAIL` refusal path, the splice, the `gate`, and teardown.
* `Serve` — the accept loop, and `ServerTLSConfig` / `IdentityFromState` for the
  9.4 configuration.
* End-to-end tests against `test/fakeadb` and its `SyncServer`, including a real
  `adbwire.Client` driven through the proxy via a sidecar, and a `SEND` cut
  mid-transfer by a fence bump.

**Built since, in the host integration** (section 14):

* The Postgres-backed `FenceSource` — `node.hostFloors`, the section 4 query
  through the pool the node agent already owns.
* Wiring into `cmd/farmd` and `internal/node`: `farmd node` serves the proxy
  when given TLS material and advertises it as the host's endpoint.

**Deliberately not built here**, because it belongs elsewhere:

* The client-side preamble sender, which needs section 11's `adbwire` dial seam.
* Certificate issuance machinery. Section 9 specifies it; no CA is invented in Go
  here.
* The three companion changes flagged above: 5.4's allocation predicate, 6.4's
  scratch-directory clear, and section 12's `lease_expire_max_runtime` fix.

---

## 14. Status: integrated on the host

`farmd node` serves this proxy. Everything below is what the tree does today,
not what it is meant to do, and the last subsection is the list of what it
still does not do.

### 14.1 What turns it on, and what it refuses

Three environment variables on the device host, read by `internal/config` and
served by the node role alone:

| Variable | Meaning | Default |
|---|---|---|
| `FARM_FENCE_TLS_CERT` | the proxy's certificate (PEM) | unset |
| `FARM_FENCE_TLS_KEY` | its private key | unset |
| `FARM_FENCE_TLS_CA` | the roots a client certificate must chain to | unset |
| `FARM_FENCE_LISTEN` | the proxy's bind address | `:5038` |
| `FARM_FENCE_ADVERTISE` | what is written to `farm.hosts.adb_endpoint` | derived, see 14.2 |
| `FARM_FENCE_POLL_INTERVAL` | section 4's poll | `2s` |

The three PEM paths are **all or none**. One or two is refused at config
validation, naming the variables: a proxy with a certificate and no client CA
would admit anyone, and one with a CA and no certificate cannot listen. All
three are **opened and parsed at startup**, by every role that carries them,
so a bad path or a corrupt file fails the rollout rather than the first
handshake on a host that is already advertising the proxy. The node agent
loads them a second time into its reloader (14.5) before it registers anything.

With the three unset, **nothing changes**: the agent advertises
`FARM_ADB_ENDPOINT` as it always did, logs one WARN at startup saying that no
proxy is configured and the fence is not enforced at the device, and the
config summary of every role prints

    fence proxy      = off (FARM_FENCE_TLS_CERT/FARM_FENCE_TLS_KEY/FARM_FENCE_TLS_CA unset) — the fence is NOT enforced at the device; …

so that the self-fence timeout and margin printed just above it cannot be read
as a fence enforced on a farm that has no proxy.

### 14.2 What it advertises — no new column

When the proxy is on, the agent writes **the proxy's address**, not the ADB
server's, to `farm.hosts.adb_endpoint` on every registration. There is no
second column and no flag: the endpoint column already means "what the cluster
dials to reach this host's devices", and every reader of it — the jobrunner's
`Placement`, the watchdog, the recovery ladder, the API — dials whatever is
there. That is the whole integration on the reading side.

The address is `FARM_FENCE_ADVERTISE` when set, and otherwise derived by
`node.AdvertiseAddr` from the listen address: a listener bound to a specific
host advertises exactly that (loopback included — an operator who bound
`127.0.0.1` has a single-box farm and meant it); a wildcard listener takes the
host part of `FARM_ADB_ENDPOINT` when it is routable, else the first
global-unicast address of a local interface, and **never infers loopback**,
because `127.0.0.1:5038` written into `farm.hosts` by inference is an endpoint
no other machine can dial on a row that reads as healthy. When nothing
qualifies the agent refuses to start and says to set the variable.

The advertised endpoint **stays the proxy's while the proxy is down** (14.4).
The endpoint is a promise about where the fence is enforced, not a liveness
report, and the one thing it must never do is point at the unfenced server
because the fenced one stopped.

### 14.3 How it learns the floors — `FARM_NODE_SELF_FENCE_TIMEOUT` finds its consumer

`node.hostFloors` is the `FenceSource`: the section 4 query, once per host per
`FARM_FENCE_POLL_INTERVAL`, through the pool the agent already owns for
registration and enrolment. It is the interface's one method and nothing else
— the node package still imports neither `internal/lease` nor anything that
ends one. A host with no positions reads as an empty snapshot, not an error,
because an error is blindness and, past the budget, blindness refuses every
new connection to a host that simply has nothing plugged in yet. The snapshot
carries no timestamp; `Cache` stamps the read with the proxy's own clock.

`Policy.MaxStaleness` is **`FARM_NODE_SELF_FENCE_TIMEOUT`**, which until this
integration was validated against `FARM_SLOT_REARM` in every process and
consumed by none. It now means exactly what 4.1 says: the age past which the
proxy's last successful read may no longer admit a *new* connection. It tears
nothing down. `config.Validate` keeps `FARM_SLOT_REARM` above it plus
`FARM_FENCE_SAFETY_MARGIN`, and refuses a poll interval at or above it, since a
poll that lands less often than the view is allowed to age refuses every new
connection on a perfectly healthy farm.

### 14.4 How it fails

The listener is one goroutine beside the poller. When it dies — a bind
failure, an accept error — the agent logs at ERROR, counts
`farm_node_fence_proxy_restarts_total`, and binds again after a backoff that
doubles from 1s to 30s and resets after a minute of health. Sessions already
spliced through the dead listener drain behind the new one; shutdown waits
five seconds for them and then leaves them to end with the process, which
affects no lease.

While the listener is down, **nothing on the host is reachable**, and that is
the intended failure: the advertised endpoint is the proxy's, clients fail
closed and retry, and the alternative — falling back to the unfenced server —
would mean the thing that enforces the fence can be removed by making it
crash. `farm_node_fence_proxy_up` is 0 for the duration; on a host with no
material configured it is also 0, and the distinction is the WARN in 14.1.

### 14.5 Certificates

The proxy's certificate and key are re-read from disk when either file's
modification time changes, checked at most once a second on the handshake
path. A rotation is a file write; the next handshake serves it; no restart, no
severed connection (9.2). A rotation that does not parse is logged and the
previous certificate stays in service, because a broken file on disk must not
take a working listener down and the operator who wrote it is the one who
needs to hear about it.

The client's class comes from its certificate's `farm://<class>/` URI SAN and
never from its preamble (7.1); the node package adds nothing to that.

### 14.6 The proof

In `internal/node`, against a real `fenceproxy.Server`, real mTLS on both
sides from an in-test CA, and `test/fakeadb` behind it:

* a lease-class client presenting a fence **below** the floor gets a readable
  `FAIL` on `host:transport:` and never reaches the adb server; **at** the
  floor it is switched through and gets the server's `OKAY`;
* a lease certificate whose preamble claims `maintenance` is held to the lease
  rules; a maintenance certificate opens `host:devices-l` and is refused
  `host:kill`;
* a client certificate from a foreign CA does not complete the handshake, and
  its request — one the fence alone would admit — reaches nothing;
* a listener whose `Accept` fails is replaced, with the advertised endpoint
  unchanged throughout;
* a rotated certificate is served on the next handshake and a broken rotation
  keeps the previous one;
* against a scratch Postgres, `hostFloors` reads every position on the host
  and no other host's, an empty host is an empty snapshot, and
  `farm.hosts.adb_endpoint` carries the proxy's address when the proxy is on
  and the adb server's when it is not.

### 14.7 What is still not enforced

* **The client half.** Nothing in the tree yet presents a certificate or writes
  the `fence:v1` preamble: `adbwire` has no TLS dial seam and no preamble
  option. Turning the proxy on today therefore makes a host unreachable to
  every existing client — the jobrunner, the watchdog, the recovery ladder,
  `ctl` — which is the fail-closed property working as designed, and the
  reason the capability report keeps `"Fence enforcement at the resource"` at
  `not_built` until the client half lands and can say `enabled` truthfully.
* **Certificate issuance.** Section 9 specifies it; the chart carries the
  cluster's client material in one Secret (`fenceProxy` in `values.yaml`) and
  refuses to render it half-set or unmounted, but no CA is minted anywhere.
* **The three companion changes** of section 13, unchanged.
