# Siting the device lab

Where to physically put 50-60 handsets, and why.

This document exists because renting devices from a managed device cloud is
excluded by contract for this workload. The farm therefore runs on hardware we
own, in a room somebody has to choose. That turns a question people usually
answer with a purchase order into a physical one, and the physical answers are
not the ones most people expect.

**Scope.** This covers the room: whether fire code blocks it, whether a
colocation provider will, what mitigation actually works on lithium cells, and
how far a USB cable reaches. It does not cover rack mechanics, host sizing, or
network design. The last section is an explicit list of what research did *not*
establish, and it is long enough to matter — read it before treating this
document as a plan.

---

## 1. Fire code is not the barrier, and the arithmetic is not close

The reflex objection to a room full of lithium cells is that fire code
prohibits it. For this fleet size it does not, and it is worth knowing why so
the conversation with a building owner or an authority having jurisdiction can
move on to the questions that do matter.

The International Fire Code sets its lithium-ion energy-storage threshold at
**20 kWh**: IFC 2021 Table 1207.1.1. Sixty handsets at 15-20 Wh each is
**0.9-1.2 kWh** — roughly twenty times below the trigger.

**Cite the edition.** That table is renumbered **1207.1.3 in IFC 2024**. The
threshold did not move; the pointer did. An AHJ adopts one specific edition,
and quoting a table number from a different one is how a correct argument gets
dismissed as sloppy. Find out which edition your jurisdiction adopted before
you cite anything.

The stronger argument is that the arithmetic never needs to be performed.
Section 1207 governs **stationary energy storage systems** — batteries
installed to store energy and give it back. Handsets under test are equipment
in use, not an ESS. Section 1207 is out of scope before the numbers, and the
20 kWh comparison is only the belt to that argument's braces.

### The storage hazard is a different section, and it is not out of scope

Phones on a shelf are not phones in use. **IFC 2024 Section 320 governs the
storage of lithium batteries**, and it carries a carve-out at **≤30% state of
charge**. That has no bearing on the racked fleet, and direct bearing on:

- spare handsets held for replacement,
- spare batteries,
- devices retired from the fleet and not yet disposed of,
- anything in a cupboard, a box, or a store room.

The operational consequence is a habit, not a document: **spares go on the
shelf discharged to 30% or below, and they are re-checked**. A drawer of
fully-charged spare handsets is the part of this installation most likely to
attract a code finding, and it is the part nobody thinks about because it is
not in the rack.

---

## 2. Clean-agent suppression does not work on lithium

**This is the single most important thing in this document.** If you take one
sentence away, take this one: a clean-agent suppression system in the room does
not make a lithium fire safe, and buying one does not close the risk.

Peer-reviewed testing (*Fire Safety Journal*,
[doi:10.1016/j.firesaf.2021.103296](https://doi.org/10.1016/j.firesaf.2021.103296))
put Novec 1230 against a 12-cell 18650 array:

- At **8.5 vol%** — *above* the design concentration used for conventional
  fire — the agent "failed to suppress flaming combustion and did not prevent
  propagation of thermal runaway through the array."
- At **15.2 vol%**, nearly double, **full propagation still occurred in about a
  third of tests**.

The number is alarming. The mechanism is the part that should change your
design.

In the same work, a **pure-nitrogen baseline** — no oxygen, and therefore no
flame at all — **still propagated**. Thermal runaway in a lithium cell is
exothermic decomposition of the cell's own chemistry. It carries its own
oxidiser and it heats its neighbours by conduction and radiation. Fire
suppression suppresses *fire*. It has nothing to say to a cell cooking the cell
next to it.

So: **preventing combustion does not prevent propagation.** A suppression
system sized and certified for a server room is answering a question a battery
array is not asking.

### What follows from that

Mitigation has to attack propagation and energy, not flame:

| Lever | What it buys | Why it works where suppression does not |
|---|---|---|
| **Containment** | Confines a runaway to one enclosure | The failure mode is cell-to-cell heating; a barrier interrupts it |
| **Spacing** | Raises the energy needed to reach a neighbour | Conduction and radiation both fall off with distance |
| **Charge limiting** | Lowers the stored energy in every cell | A cell at 60% has less to release than one at 100%; IFC 2024 §320's ≤30% carve-out is the code's own acknowledgement that state of charge changes the hazard |
| **Early detection** | Buys evacuation and intervention time | Runaway announces itself — off-gassing, swelling, heat — before it is visible |

Charge limiting is the only row of that table this project could plausibly
reach in software, and it is why [hub-validation.md](hub-validation.md) is a
prerequisite rather than an optimisation: cutting VBUS is the mechanism by
which a phone stops charging, so a fleet on hubs that cannot actually cut VBUS
sits at 100% state of charge permanently, with no software able to change that.

**Be clear about what exists today.** The farm can cut VBUS to a port — that is
recovery tier 4, `internal/node/uhubctl.go`. **There is no charge-limiting
control loop in this codebase**: nothing reads a battery level, nothing decides
to hold a device at 60%, nothing schedules power off the charge state. Passing
the bench procedure buys the *capability*; the policy that would use it is
unbuilt and is listed in §7. Do not read this row of the table as a control
that is currently in force.

**Do not** specify a clean-agent system and record the hazard as handled. If
one is present for other reasons, treat it as protecting the building's
conventional combustibles, and mitigate the cells separately.

---

## 3. The real barrier is operator policy, and it is unresolved

The constraint that actually decides whether this lab can live in a colocation
facility is **the provider's own policy**, not code.

**Research could not establish any colocation provider's actual stance, in
either direction.** That is stated plainly because the alternative — assuming
they will say yes, or assuming they will say no — is how hardware gets bought
before the room exists. There is no published policy that was found saying
colocation providers accept racks of handsets, and none saying they refuse.

**This is a question to settle in writing with a named provider before buying
hardware.** Concretely, get an answer in the contract or in an email from
someone empowered to give one:

- Are lithium-ion batteries permitted in the cabinet at all, and in what
  quantity?
- Does the answer change for equipment in use versus spares in storage?
- Is the customer required to carry insurance naming the aggregate of cells?
- What are the notification and evacuation obligations on a thermal event?
- Who bears the cost of a hall evacuation caused by our equipment?

Every one of those has a bearing on whether colocation is cheaper than a room
of your own, and none of them is answerable from the outside.

---

## 4. The only precedent found, and why it does not transfer

The single piece of first-hand evidence located is **Meta's device lab**, which
moved out of the Menlo Park offices and into the **Prineville data centre**.
Meta's stated reason was scale, not safety:

> "we'd need to scale to nine of these rooms in our Menlo Park headquarters
> within a year. This wouldn't work."

(Meta Engineering, on the mobile device lab at the Prineville data centre.)

Two reasons this does not settle the colocation question:

1. **Prineville is Meta's own data centre.** They write the policy they are
   complying with. A first-party facility saying yes to its own device lab is
   not evidence about what a third-party provider will accept from a tenant.
2. **The scale is ~2,000 devices** — about thirty-three times this project. It
   demonstrates that the engineering problem is solvable at scale by an
   organisation that owns the building. It says nothing about a 60-handset
   tenant in somebody else's hall.

The precedent is useful for layout lessons (§5). It is not useful as an
argument to a provider.

---

## 5. USB cable reach is a layout constraint, not a detail

This is where the Meta experience is directly transferable, because it is
physics rather than policy.

Meta's plastic **"gondola"** rack design was abandoned in part because
**"the short length of the USB cables caused a lot of issues."** Their metal
**"Sled"** design failed differently: it **lost all Wi-Fi to EMI**. Two racks,
two physical failures, neither of them about fire.

The hard numbers, and they are short:

- **Passive USB 2.0: about 5 m.**
- **Passive USB 3.x: about 3 m.**

Both are propagation-delay-limited, not a marketing figure you can beat with
better copper. Active cables and hub chains extend reach, at the cost of
another component in the recovery path — an active cable is a device that can
itself wedge, and it is one the ladder cannot power-cycle independently.

What that means for the room:

- **The host lives with its phones.** A 3-5 m budget is a rack, not a room. You
  do not run USB from a host at one end of the hall to a shelf at the other.
- **Budget the whole path**, not the cable: host port → hub → phone. A 2 m hub
  uplink plus a 2 m device cable is already at the USB 3.x limit.
- **Metal enclosures cost you radio.** If jobs need Wi-Fi — and Android jobs
  usually do — an all-metal rack is a decision to install per-shelf APs or to
  accept EMI-degraded, flaky wireless. Meta hit this and the design did not
  survive it.
- **Cable reach and spacing pull in opposite directions.** §2 wants cells apart;
  USB wants them close. That tension is real and has to be resolved on the
  actual shelf, not on paper.

---

## 6. Recommendation

Derived from what survives above, not asserted:

**Own or rent a room under direct control. Open, ventilated shelving. Hubs with
per-port VBUS control, validated on the bench, so that charge limiting is
buildable. Colocation treated as a path to negotiate in writing, not an
assumption.**

The reasoning, step by step:

- **A room under direct control**, because §3 is unresolved. Direct control is
  the only option that does not depend on an answer nobody has yet. It is not a
  claim that colocation is worse; it is the option that can proceed today.
- **Open, ventilated shelving**, because §2 says mitigation is spacing and heat
  removal. Open shelving gives both, and a sealed cabinet without engineered
  ventilation gives neither while looking tidier.
- **Hubs that can actually cut VBUS**, because that is the mechanism charge
  limiting would need, it lowers stored energy across the whole fleet rather
  than containing one failure, and it is a hardware choice that cannot be
  corrected later in software. Buy the capability now even though the policy
  that uses it is not written yet (§7.6).
- **Colocation negotiated in writing**, because it may well be the right answer
  — it is simply not yet a known one, and §4 is not the evidence that would make
  it one.

---

## 7. What research did NOT establish

Everything above is sourced. The following is not, and is listed here so that
nobody mistakes its absence for its resolution. **An operator must answer these
locally.** A plan that treats them as solved is a plan with six unpriced
risks. The sixth is ours rather than the room's, and it is listed here for the
same reason: so nobody mistakes a recommendation for a control.

1. **Battery swell rates and detection lead time.** Swelling is a known
   precursor to runaway. How long a handset swells before it becomes dangerous,
   and whether that window is long enough to catch by inspection at any
   realistic cadence, was not established. Consequence: the inspection interval
   in your runbook has no evidence behind it. Pick one, write down that it is a
   guess, and revise it when you have local data.

2. **Insurance treatment of an aggregate of lithium cells.** Whether 60
   handsets in one room constitute a disclosable accumulation, whether a
   standard policy excludes it, and what a carrier requires in mitigation, was
   not established. Consequence: ask your carrier before the hardware arrives,
   not after a claim.

3. **Electrolyte off-gassing detection.** Off-gassing precedes flame and is the
   detection lever §2 recommends. Which detector technologies actually work at
   this cell count, what they cost, what they false-alarm on, and what
   integration they need, was not established. Consequence: "early detection"
   in §2 is a principle without a product recommendation behind it.

4. **Containment cabinets.** Whether purpose-built lithium containment
   enclosures exist at handset scale, what they cost, and whether they conflict
   with the ventilation §6 calls for, was not established. Consequence: the
   containment row of §2's table is the least actionable one.

5. **Electrical and thermal requirements for the room.** Circuit sizing, heat
   rejection for the hosts and chargers together, and whether ambient
   temperature control is needed to keep cells in a safe band, were not
   established. Consequence: nothing in this document tells you how many amps
   or how much cooling to specify. That is a local calculation from your actual
   host and charger inventory.

6. **Charge limiting is not implemented.** §2 lists it as the mitigation this
   project could reach, and the recommendation in §6 buys the hardware for it.
   **No code in this repository does it.** Nothing reads a battery level,
   nothing holds a device below a target state of charge, nothing decides to
   cut power on that basis; the only VBUS control that exists is recovery
   tier 4, driven by device health. Consequence: after following this document
   and [hub-validation.md](hub-validation.md) in full, the fleet still charges
   to 100% and stays there. What you will have bought is the ability to build
   the control, not the control. Whoever writes it also has to answer what
   target state of charge is right, whether cutting VBUS interferes with jobs
   that need a charging device, and how a device is topped up before a long run.

Three of these — off-gassing detection, containment, and charge limiting — are
the mitigations §2 points at, and none of them is in place. Naming them as
unresolved is not a hedge: it is the honest state of this design, and closing
them is the next piece of work. Until then, the safety story for this lab is
spacing, ventilation, discharged spares, and human attention.

---

## Sources

- **International Fire Code 2021**, Table 1207.1.1 — 20 kWh lithium-ion
  energy-storage threshold. Renumbered **1207.1.3 in IFC 2024**. Section 1207
  scopes to stationary energy storage systems.
- **International Fire Code 2024**, Section 320 — storage of lithium batteries;
  carve-out at ≤30% state of charge.
- **Fire Safety Journal**, [doi:10.1016/j.firesaf.2021.103296](https://doi.org/10.1016/j.firesaf.2021.103296)
  — Novec 1230 against a 12-cell 18650 array: failure to suppress at 8.5 vol%,
  full propagation in ~1/3 of tests at 15.2 vol%, and propagation in a
  pure-nitrogen baseline.
- **Meta Engineering**, on the mobile device lab at the Prineville data centre
  — the Menlo Park scaling statement, the gondola's USB cable-length failure,
  and the Sled's Wi-Fi/EMI failure. ~2,000 devices, first-party facility.
- **USB cable reach**: passive USB 2.0 ~5 m, passive USB 3.x ~3 m,
  propagation-delay limited.
- [hub-validation.md](hub-validation.md) — the bench procedure that makes
  charge limiting real, and `internal/node/uhubctl.go` for what the software
  does with the result.
