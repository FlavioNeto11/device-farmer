# Validating a USB hub before you buy fifty of them

A bench procedure. Run it on **one** hub, from the model and firmware revision
you intend to order, **before** the purchase order for the rest.

## Why this exists

Recovery tier 4 cuts VBUS to a port. It is the last rung that fixes a device
without a human walking to the rack, and cutting VBUS is also the only mechanism
by which a phone in this rack could ever be stopped from charging — the
mitigation [siting.md §2](siting.md#2-clean-agent-suppression-does-not-work-on-lithium)
argues for. (Charge limiting itself is **not implemented**; see
[siting.md §7.6](siting.md#7-what-research-did-not-establish). This procedure
buys the capability, not the policy — and it is a hardware capability, so it
cannot be bought back later.)

Both of those depend on a claim the hub makes about itself, and **hubs lie about
this specific claim.**

uhubctl's own README says so:

> "while most hubs will cut off data USB connection, some may still not cut off
> VBUS to port, which means connected phone may still continue to charge from
> port that is powered off"

Its compatibility table is explicitly incomplete, and some entries in it work on
**6 of 7 ports** — the same physical model, one port that does not switch.

That is the whole problem in one sentence: **the device disappearing from
`adb devices` proves the data lines were cut, and proves nothing about VBUS.**
Every naive test — and every "it worked when I tried it" — confirms the thing
that was never in doubt. A hub that cuts data but not power will pass a casual
check, ship in quantity, and then leave you with a fleet where tier 4 reports
success while the phone never lost power and the battery never stopped
charging.

Fifty hubs is a purchase you make once. Spend an afternoon first.

---

## The three ways this goes wrong

Each has its own step below, because each fails differently and is caught by a
different observation.

1. **The hub cuts data but not VBUS.** uhubctl exits 0, the device vanishes from
   sysfs, and 5 V is still on the pins. Caught only by §4.
2. **The kernel undoes the power-off.** Below Linux 6.0 the USB core re-powers a
   port that was switched off behind its back. uhubctl sends
   `CLEAR_FEATURE(PORT_POWER)`, the hub obeys, and the kernel turns it straight
   back on. The cycle reports success and the device never saw one. Caught by
   §2 — and refused outright in production by `checkKernelFloor` in
   `internal/node/uhubctl.go`.
3. **Switching is ganged, not per-port.** One switch feeds every downstream
   port, so "cycle this port" is really "cycle these seven". On a live rack that
   turns one operator's recovery decision into six destroyed jobs. Caught by §5.

Failure 3 is the one the schema models: `farm.power_domains.kind` is
`'per_port'`, `'ganged'` or `'none'`, and the host agent re-derives it from
**live sysfs** in `checkBlastRadius` rather than trusting the row, precisely
because the database can be wrong about hardware somebody re-cabled.

---

## 1. What you need on the bench

| Item | Why this specifically |
|---|---|
| The hub under test, one unit | Same model **and firmware revision** as the order |
| A Linux host, kernel ≥ 6.0 | Below that, every result is meaningless — see §2 |
| `uhubctl`, installed | The tool the agent actually shells out to |
| **An inline USB power meter** | The decisive instrument. Reads VBUS volts and amps between port and load |
| **2+ dumb USB loads** — an LED lamp or a fan | Battery-free, so "it went dark" means "power went away" |
| One Android handset from the fleet | Only for §6, the re-enumeration check |
| A multimeter | Fallback for VBUS if you have no inline meter |

**Do not use a phone as the load for §4.** A phone has a battery. It keeps
running whether or not VBUS is present, so watching a phone is watching the
data lines — exactly the observation that cannot distinguish a good hub from a
bad one. The dumb load is the point of the whole procedure.

Record everything as you go. There is a worksheet at the end.

---

## 2. The kernel floor — check this first, or discard everything after it

```bash
uname -r
```

**Must be 6.0 or higher.** Not "close to", not "backported". Below 6.0 the USB
core re-powers a port that was switched off behind its back, so a test on such
a host measures the kernel, not the hub, and it measures it wrong: the port
appears to switch and does not.

The version is parsed from the numeric head, so `6.1.0-18-amd64` and `6.1-rc4`
both read as 6.1 (`kernelVersion` in `internal/node/hostops.go`). `5.15.0-91`
reads as 5.15 and is below the floor.

This is not advisory. `internal/node/uhubctl.go` refuses recovery tier 4 outright
below this version, and it refuses rather than trying because a cycle that is
silently undone reports success and repairs nothing — which teaches the recovery
ladder that tier 4 was tried and did not help, so it escalates to quarantining a
device whose only real problem is that the rung which would have fixed it was
never actually performed.

In production the agent reports the same string into `farm.hosts.kernel_release`,
so you can check a whole fleet without shelling into each host. It is not in the
`hosts` table output — ask for JSON:

```bash
farmd ctl hosts -o json | jq -r '.hosts[] | [.id, .kernel_release] | @tsv'
```

**If the bench host is below 6.0, stop.** Upgrade it. Do not "test anyway and
adjust" — there is nothing to adjust, the observations are simply false.

---

## 3. What the hardware claims about itself

Two independent claims, from two sources. Read both. Believe neither yet.

### 3a. The kernel's reading of the hub descriptor

Find the hub's USB path (`3-1.4`, `3-1`, and so on) with `lsusb -t` or by
looking under `/sys/bus/usb/devices/`. Then:

```bash
cat /sys/bus/usb/devices/3-1.4/wHubCharacteristics
```

Bits 1..0 are the **Logical Power Switching Mode** of the USB hub descriptor:

| Bits | Meaning | `farm.power_domains.kind` |
|---|---|---|
| `00` | ganged — one switch for all ports | `ganged` |
| `01` | individual — per-port switching | `per_port` |
| `1x` | no power switching | `none` |

This is the first thing `hubPower` in `internal/topo/sysfs.go` reads, and when
it parses, it is what topology discovery records. The file is not present on
every hub; if it is missing or will not parse as hex, go to 3b.

### 3b. The per-port `disable` control

When the descriptor is missing or unparseable, the kernel's per-port objects are
the fallback. They live under the hub's interface directory and are named after
it:

```bash
ls -l /sys/bus/usb/devices/3-1.4/3-1.4:1.0/3-1.4-port*/disable
```

The USB core only publishes `disable` for ports whose power it can actually cut,
and **it has to be writable** — a read-only `disable` is a status readout, not a
control, and cannot cycle anything. Count them:

- present and writable on **every** port → per-port,
- on **no** port → none,
- on **some** ports → **unknown**, and unknown is treated as not switchable.

That last case is the "6 of 7 ports" hub from uhubctl's compatibility table, and
it is why §5 tests every port rather than sampling one.

Root hubs are named differently: the directory is `usb3`, so the port objects
are `usb3-port1` and so on, not `3-0-port1`.

### 3c. What uhubctl claims

```bash
uhubctl                  # every switchable hub the tool can find
uhubctl -l 3-1.4         # status for one hub
```

The bracketed tail of each header line is the descriptor uhubctl read off the
hardware, and its last field is the switching mode:

```
Current status for hub 3-1.4 [05e3:0608 GenesysLogic USB2.1 Hub, USB 2.10, 4 ports, ppps]
```

`ppps` means per-port power switching. Anything else — `ganged`, or a descriptor
that will not parse — is treated as ganged by
`readHubs`/`hubStatus.perPort`, because assuming per-port switching on a hub
that does not have it is how one recovery action becomes seven broken jobs.

**Expect two lines per receptacle on USB 3.** A USB 3 socket appears to the
kernel as two hubs — a SuperSpeed one and a USB 2 companion — and one physical
port is one port on each. uhubctl switches both by default, which is what
actually cuts power to the socket. `--exact` switches only one half and can
leave a USB 2 phone powered; the agent deliberately does not pass it. **Do not
pass `--exact` on the bench either**, or you will be testing something the
production code never does.

Note both claims on the worksheet. If 3a/3b and 3c disagree, that is itself
worth recording — but neither of them settles the question. §4 does.

---

## 4. Does VBUS actually drop? — the decisive test

This is the step the whole document exists for.

**Wire it up:** hub port 1 → inline USB power meter → dumb load (LED lamp or
fan). Nothing else in the hub.

**Baseline.** Read the meter. You should see roughly 5 V and a non-zero current.
Write both down. If you do not see current, the load is not drawing and the test
will not distinguish anything.

**Cut power:**

```bash
uhubctl -l 3-1.4 -p 1 -a off
```

**Now look at the meter, not at the terminal.**

| Meter reads | Verdict |
|---|---|
| **0.00 V**, current 0 (a decaying tail to zero over a second or two is fine — that is the load's own capacitance draining) | VBUS is genuinely cut. Port 1 passes. |
| **~5 V still present**, load still drawing, lamp still lit | **FAIL.** The hub cut the data lines and left power on. This is the uhubctl README's warning, in your hands. |

There is no third outcome with a dumb load, and that is the point of using one.
A lamp is not a battery: no light means no power, and light means power. Had you
used a phone here, it would have vanished from `adb devices` in **both** rows and
told you nothing.

If the port fails, you are done with this hub. No amount of software
configuration changes it.

**Restore power before moving on:**

```bash
uhubctl -l 3-1.4 -p 1 -a on
```

Then confirm on the meter that ~5 V is back. A port left dark is a device
removed from the farm until a human notices — which is why `powerOn` in
`internal/node/uhubctl.go` is the only step in that file that retries, and why a
failed restore logs in capitals.

**Cross-check with the terminal, afterwards and only afterwards.** uhubctl
exiting 0 is not evidence. A non-zero exit is not evidence that power is still
on either: uhubctl can fail partway — switching the USB 3 hub and then erroring
on the USB 2 companion — which is exactly why the agent arms its restore guard
*before* issuing the off command rather than after.

---

## 5. Per-port or ganged — the neighbour test

Blast radius is decided here. On a ganged hub, cutting one port cuts them all,
and the difference is invisible from the control plane.

**Wire it up:** a dumb load in **port 1** and a second dumb load in **port 2**.
Ideally a meter on each; one meter plus a visible lamp on the other port is
enough.

```bash
uhubctl -l 3-1.4 -p 1 -a off
```

| Observation | Verdict |
|---|---|
| Port 1 dark, **port 2 still powered** | Per-port switching, for this pair |
| **Both** dark | **Ganged.** One switch, whatever the descriptor says |
| Port 1 still powered | Go back to §4; this hub already failed |

Restore with `-a on` and check both loads are back.

**Live sysfs beats the descriptor and beats the database.** If the neighbour
goes dark on a hub whose descriptor said `ppps`, the hub is ganged. Record
ganged. This is the same asymmetry the agent enforces: positive evidence is
required to claim per-port, and the absence of evidence is never read as
capability, because erring toward ganged costs a wider blast radius and a
refusal, while erring the other way costs somebody's six-hour run.

### Test every port, not one

The "6 of 7 ports" entries in uhubctl's compatibility table are real: the same
hub model, one port that will not switch. Move the load through **every port**
and repeat §4 for each:

```bash
for p in 1 2 3 4 5 6 7; do
  echo "=== port $p ==="
  uhubctl -l 3-1.4 -p "$p" -a off
  read -r -p "meter reading for port $p, then Enter to restore: " _
  uhubctl -l 3-1.4 -p "$p" -a on
done
```

Move the load between iterations; the loop only sequences the commands and
pauses for you to read the instrument. **A hub is per-port only if every port
passes.** One port that does not switch makes the whole hub `unknown` to
topology discovery, which treats it as not switchable — so a hub that is
per-port on six of seven ports is, for this farm, not a per-port hub.

---

## 6. Does the phone come back?

Now, and only now, use a real handset. Plug one fleet device into a port that
passed §4 and §5.

```bash
adb devices                          # note it present
uhubctl -l 3-1.4 -p 1 -a off
sleep 3
uhubctl -l 3-1.4 -p 1 -a on
```

Time three things and write them down:

1. **How long until the device leaves sysfs after the off.** The agent allows
   **2 s** (`offVerifyWindow`). A real VBUS cut disconnects in milliseconds; if
   your hub takes longer than two seconds, the agent will report that VBUS was
   not actually cut and refuse to treat tier 4 as working here.
2. **How long VBUS should stay off.** The default is **3 s**
   (`DefaultPowerOffSettle`) — long enough that the phone's USB PHY and the
   hub's port controller both see a real disconnect rather than a glitch. If
   your handsets need longer, that is a real finding: raise
   `PowerOffSettle` on the node agent, and write down why.
3. **How long until it re-enumerates after the on.** The agent allows **30 s**
   (`DefaultPortReturnTimeout`). Handsets that habitually take longer will
   produce tier-4 failures that are really timeouts.

A device that never comes back after §4 and §5 passed points at the cable or the
handset, not the hub — swap the cable first.

---

## 7. Recording the verdict in `farm.power_domains`

A power domain models **what a single power switch actually controls**. One row
per hub, keyed to the hub by `control_addr`:

```sql
CREATE TABLE farm.power_domains (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  host_id       text NOT NULL REFERENCES farm.hosts(id) ON DELETE CASCADE,
  kind          text NOT NULL CHECK (kind IN ('per_port','ganged','none')),
  control       text NOT NULL CHECK (control IN ('uhubctl','smarthub','pdu','none')),
  control_addr  text,          -- the hub's usb_path, e.g. '3-1.4'
  notes         text
);
```

### The row usually writes itself

Topology discovery creates the domain the first time it sees the hub
(`farm.register_slot`, `migrations/00004_operate.sql`) and thereafter reconciles
it against sysfs on every pass (`reconcilePower`, `internal/topo/discover.go`).
Look at what it decided:

```sql
SELECT pd.id, pd.kind, pd.control, pd.control_addr, h.model, h.port_count
  FROM farm.power_domains pd
  LEFT JOIN farm.hubs h
         ON h.host_id = pd.host_id AND h.usb_path = pd.control_addr
 WHERE pd.host_id = 'host-01'
 ORDER BY pd.control_addr;
```

If discovery says what the bench says, you are done — nothing to write.

### When the bench disagrees with sysfs, the bench wins

The case that matters is failure 1: the descriptor says `ppps`, uhubctl says
`ppps`, discovery records `per_port`, and your meter measured 5.06 V with the
port off. Discovery cannot see that. Only you can, and you have to say so:

```sql
UPDATE farm.power_domains
   SET kind    = 'none',
       control = 'none',
       notes   = 'bench 2026-09-04 fp: uhubctl and wHubCharacteristics both report ppps, but VBUS measured 5.06 V on an inline meter with the port off, on all 7 ports. Data lines cut, power not. Tier 4 cannot work on this hub model.'
 WHERE host_id = 'host-01' AND control_addr = '3-1.4';
```

`kind = 'none'` is the honest record: there is no switch here that controls
anything. `control = 'none'` says no mechanism can drive it.

**This edit survives discovery, and that is not an accident.** `reconcilePower`
only ever moves a domain between the two exact shapes `register_slot` itself
writes — `('per_port','uhubctl')` → `('ganged','none')` on negative evidence, and
`('ganged','none')` → `('per_port','uhubctl')` on positive. A row reading
`('none','none')` matches neither `WHERE` clause, so the next discovery pass
leaves your verdict alone. The same protection is why a domain an operator
pointed at a PDU or a smart hub is never overwritten from sysfs: an external
switch is authoritative over what the kernel infers.

For a hub that genuinely cuts VBUS but does so for all ports at once, record:

```sql
UPDATE farm.power_domains
   SET kind = 'ganged', control = 'none',
       notes = 'bench 2026-09-04 fp: cutting port 1 also killed the load on port 2.'
 WHERE host_id = 'host-01' AND control_addr = '3-1.4';
```

Discovery will usually reach `ganged` by itself from `wHubCharacteristics`, but
writing it explicitly with the evidence in `notes` costs nothing and answers the
next person's question.

**Two cautions.**

- `notes` is free text and is the right place for the bench record — date,
  who ran it, what was measured. Keep it factual; it is what an operator reads
  at 03:00 when they disagree with a domain.
- The demo seeder (`internal/demo/seed.go`) uses `notes` as its idempotency key,
  because `farm.power_domains` has no natural one. Never point `make demo` at a
  real farm's database.

### What the record does and does not do

Be clear about this, because it is easy to over-read.

Recording `kind = 'none'` **does not stop the ladder from attempting tier 4.**
Tier selection walks `farm.recovery_tiers` in order and does not consult
`power_domains.kind` to skip a rung; `kind` is read in `checkBlastRadius` to
decide *scope* and to label a refusal as `ganged`
(`internal/recovery/ladder.go`). What actually stops a doomed tier 4 is the host
agent: `readHubs` returns "no power-switchable hub at this location", the attempt
is recorded as failed with that reason, and the ladder moves on.

What the record **does** buy you:

- The blast radius for a `power_domain`-scoped tier covers every slot sharing
  the domain, so a ganged hub refuses instead of destroying a neighbour's job.
  A slot with **no** power domain at all widens the radius to the whole hub,
  because an unknown power topology is not a per-port one — the conservative
  reading, and one that can only ever cause a refusal.
- `farm_recovery_attempts_total` broken down by outcome tells you the rack needs
  per-port switching, rather than telling you the ladder is broken.
- The next operator reads `notes` instead of re-running your afternoon.

---

## 8. When a hub fails

**Fails §4 (VBUS not cut).** The hub is unusable for tier 4 and forecloses
charge limiting permanently. Do not buy it. If it is already in the rack: record
`('none','none')` with the evidence, accept that the ladder stops at tier 3 for
every device behind it, and plan a replacement. There is no software workaround
— the capability is absent from the hardware.

**Fails §5 (ganged).** Usable, with a permanently wider blast radius. Every
tier-4 attempt on any device behind it must clear the disruption policy of every
live lease on the hub, so on a busy rack the rung will mostly refuse. Acceptable
for a shelf of devices that run the same job, poor for mixed tenancy. Record
`('ganged','none')` and stop being surprised by refusals.

**Fails on some ports only.** Treat it as failed, and record the verdict by hand
— this is the case discovery is least able to see.

If discovery reaches the port-count fallback of §3b, it resolves "controllable on
6 of 7 ports" to `unknown` and treats that as not switchable, which is the right
answer. But if `wHubCharacteristics` parses, `hubPower` returns on that alone and
never counts ports, so a hub whose descriptor claims `ppps` while one port does
not switch is recorded as `per_port` — and tier 4 on that one port will fail
forever with nothing in the database to explain why. Your bench result is the
only thing that knows. Write it.

Do not populate only the working ports and rely on remembering which they were.
The next person to re-cable the rack will not know, and the slot numbering will
have moved.

**Fails §2 (kernel).** Not the hub's fault. Upgrade the host to Linux ≥ 6.0 and
re-run everything from §3; the results you have are about the kernel.

In every case, **write the outcome down against the model and firmware
revision.** Vendors change silicon behind a stable model number, which is why
this procedure is per-order and not per-lifetime.

---

## Worksheet

Copy this per hub. Fill it in on the bench, not from memory.

```
Hub model / part no:      ____________________   Firmware rev: __________
Vendor:ProductID:         ____________________   Ports: _______
Bench host kernel:        ____________________   (must be >= 6.0)
uhubctl version:          ____________________
Date / operator:          ____________________

§3a wHubCharacteristics:  0x______   -> bits 1..0 = ____  (00 ganged / 01 per-port / 1x none)
§3b writable 'disable':   ____ of ____ ports
§3c uhubctl descriptor:   ______________________________  (ppps? ganged?)
     hub lines reported:  ____  (2 per receptacle on USB 3 is expected)

§4 VBUS drop, per port    baseline V/A        with port off V/A       pass?
   port 1                 ______ / ______     ______ / ______         Y / N
   port 2                 ______ / ______     ______ / ______         Y / N
   port 3                 ______ / ______     ______ / ______         Y / N
   port 4                 ______ / ______     ______ / ______         Y / N
   port 5                 ______ / ______     ______ / ______         Y / N
   port 6                 ______ / ______     ______ / ______         Y / N
   port 7                 ______ / ______     ______ / ______         Y / N

§5 neighbour test:        cut port 1 -> port 2 stayed powered?  Y / N
                          VERDICT: per_port / ganged / none

§6 handset:               left sysfs after ______ ms   (agent allows 2000 ms)
                          re-enumerated after ______ s (agent allows 30 s)
                          settle used ______ s         (default 3 s)

RECORDED AS: kind = ____________  control = ____________
             host_id = ____________  control_addr = ____________
DECISION:    buy / do not buy / buy with ganged blast radius accepted
```

---

## Sources and the code this maps to

- **uhubctl README** — the VBUS warning quoted at the top, and the compatibility
  table with its incomplete and partial-port entries.
- **Linux ≥ 6.0** — below it the USB core re-powers a port switched off behind
  its back. Enforced by `checkKernelFloor`, `internal/node/uhubctl.go`.
- **USB hub descriptor**, `wHubCharacteristics` bits 1..0 — Logical Power
  Switching Mode. Read by `hubPower`, `internal/topo/sysfs.go`.
- `internal/node/uhubctl.go` — `readHubs` (USB 3 duality and why `--exact` is not
  used), `checkBlastRadius` (live sysfs over the database), `powerOn` (the
  retrying restore), `offVerifyWindow`.
- `internal/topo/sysfs.go` — `hubPower`, `countPortControls`, `writableAttr`;
  the rule that positive evidence is required to claim per-port.
- `internal/topo/discover.go` — `reconcilePower`, and which row shapes it will
  and will not overwrite.
- `migrations/00001_core.sql` — `farm.power_domains`.
- `migrations/00004_operate.sql` — `farm.register_slot`, which creates the domain
  on first sight.
- `internal/recovery/ladder.go` — `checkBlastRadius`, where `kind` is read.
- [siting.md](siting.md) — why the ability to cut VBUS matters beyond recovery,
  and §7.6 for the fact that the charge-limiting policy itself is unbuilt.
