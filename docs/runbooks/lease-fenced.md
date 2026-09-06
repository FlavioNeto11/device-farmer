# DeviceFarmerLeaseFenced

**Severity:** critical · **Group:** `device-farmer.lost-work`

```promql
sum(increase(farm_lease_renew_failures_total{kind="fenced"}[10m])) > 0
```

## What fired

`farm.lease_renew` returned **zero rows** for a lease a holder believed it
still had. The holder took that as proof the lease was gone, aborted the job and
closed every ADB socket it had open on the device.

## What it means

This is the same event as [lease-reclaimed.md](lease-reclaimed.md), seen from
the other end. `DeviceFarmerLeaseReclaimed` is the control plane saying "I took
it"; this is the holder saying "it was taken from me".

Zero rows from `lease_renew` is the one unambiguous fence signal in the whole
system. Everything else that can go wrong during a renewal — a dial error to
Postgres, a statement timeout, a serialization failure, an exhausted pool — is
recorded as `kind="transient"` and means nothing at all about the lease. Those
are retried, the deadline is untouched and no alert fires on them, deliberately:
treating a database hiccup as a fence is how a healthy job gets killed.

**A fence is the system working.** It is what stops two processes writing to one
phone through two different lease generations. The question this page asks is
not "why did it fence" but **"what took the lease away, and was that right?"**

## What is NOT wrong

- **The abort.** The holder did exactly the right thing. Nothing was written to
  the device after the fence, which is the entire point.
- **A fence right after an operator revoke.** Expected, and it will be paired
  with `reason="operator_revoked"` in `farm.v_lease_endings` and a row in
  `farm.audit_log` with a name on it.
- **A fence right after a `max_runtime` expiry.** Also expected: a deadline the
  user wrote down elapsed, the lease ended, and the next renewal found nothing.
- **`kind="transient"` climbing at the same time.** That is a database or
  network problem, not a fence, and it destroyed nothing. It is worth a look but
  it is not this alert.

## First three checks

**1. Find the ending that caused the fence.** A fence is always downstream of a
release; the release row says who did it and why.

If the alert named a lease, ask about that one directly:

```sh
farmd ctl endings <lease id>
```

Otherwise, everything that ended around the fence:

```sh
farmd ctl endings --since 30m
```

Without a token, the same rows:

```sh
psql "$PGURL" -c "
SELECT ended_at, lease_id, job_id, tenant_id, holder, release_reason, ended_by,
       held_seconds, heartbeat_age_s
  FROM farm.v_lease_endings
 WHERE ended_at > now() - interval '30 minutes'
 ORDER BY ended_at DESC"
```

Read `release_reason` — the `REASON` column — and route on it:

| `release_reason` | What actually happened | Where to go |
| --- | --- | --- |
| `operator_revoked` | A human took it back | `farm.audit_log`; ask them |
| `holder_expired` | The control plane reclaimed it | [lease-reclaimed.md](lease-reclaimed.md) |
| `max_runtime` | A deadline the user set elapsed | Nothing to fix; tell the tenant |
| `device_retired` | The device left the fleet | [devices-quarantined.md](devices-quarantined.md) |
| `completed` / `failed` | The job ended normally | See "the benign shape" below |
| *(no matching row)* | Nothing released it | **Escalate.** See below. |

**2. Check who revoked, if anyone did.**

```sh
psql "$PGURL" -c "
SELECT at, actor, action, subject, reason
  FROM farm.audit_log
 WHERE at > now() - interval '1 hour'
 ORDER BY at DESC LIMIT 20"
```

**3. Confirm the device is not being written to by a ghost.**

```sh
farmd ctl device <device-id>
psql "$PGURL" -c "
SELECT d.farm_uid, s.rack_slot, s.state, s.rearm_at, r.health, r.adb_state,
       d.current_lease_id, d.fence_floor
  FROM farm.devices d
  LEFT JOIN farm.slots s ON s.id = d.current_slot_id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
 WHERE d.id = '<device-id>'"
```

`fence_floor` is the guard: any lease with a fence below it cannot write. The
slot's `rearm_at` in the future means the position is deliberately unschedulable
until the old holder's sockets are certainly gone.

## The benign shape

A fence paired with `release_reason` of `completed` or `failed` on the *same*
lease usually means the job's own supervisor released the lease and a stray
renewal from a sibling goroutine arrived a moment later. It destroyed nothing —
the work had already finished. If that is the whole story, the alert is noise
for this occurrence; the counter still deserves a glance next week to see
whether one holder does it constantly.

## When to escalate

- **A fence with no matching row in the ledger.** `farmd ctl endings <lease id>`
  says so in as many words: `ended: no` on a lease whose state is `released` or
  `expired`, and a warning naming this file. The lease reached a terminal state
  without a recorded release. That is a database-level problem — a row deleted
  out of band, a fence column moved, a migration mid-flight — and it needs an
  engineer immediately. Every holder in the farm is trusting that column.
- **An ending whose `ENDED BY` reads `unaccounted`.** The lease was closed with
  no `release_reason` at all, so it names none of the three ways a lease may
  end. Same severity as the row above. `farmd ctl endings` counts them at the
  bottom of the listing — over the rows listed, so a page it warns was cut can
  hide more of them further back.
- **Fences arriving in a burst across many tenants.** Look for a mass reclaim
  first ([lease-reclaimed.md](lease-reclaimed.md)), then for a control-plane
  gap ([control-plane-gap.md](control-plane-gap.md)).
- **A fence on a `protected` lease that nobody revoked.** Protected leases are
  never reclaimed automatically, so a fence on one without an audit-log entry
  should not be possible.
