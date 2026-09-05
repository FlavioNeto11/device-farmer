# DeviceFarmerProtectedLeaseSuspect

**Severity:** critical · **Group:** `device-farmer.lost-work`

```promql
max by (pool, tenant) (farm_lease_suspect{protected="true"}) > 0    for 5m
```

## What fired

A lease marked `protected` stopped renewing for longer than its TTL, so the
reaper's suspect sweep marked it. It is still holding its device.

## What it means

This is the alert the phrase "hold and page" refers to, and it is the only page
in this system whose response is a *decision* rather than a repair.

An unprotected lease that goes suspect is reclaimed automatically once it passes
`ttl + grace`: the device goes back to the pool and the work is lost. A
**protected** lease is never reclaimed. `farm.lease_reclaim` skips it by
construction, so it will sit in `suspect` indefinitely, holding its phone, until
a person decides one of two things:

- the holder is coming back — a supervisor is restarting, a node is rebooting,
  a network partition is healing — in which case **do nothing**. The next
  successful heartbeat moves the lease back to `held` at the same fence, and the
  job continues as though nothing happened; or
- the holder is gone for good, in which case you revoke the lease by hand and
  the job's work is deliberately discarded.

Refusing to make that choice automatically is the design. A protected lease is
protected because somebody wrote down that the work in it is expensive enough
that a machine should not be allowed to throw it away.

**Nothing has been released.** Suspect is an alerting state and nothing more.
It reallocates no device, closes no socket and aborts no job.

## What is NOT wrong

- **A rolling deploy of the holder.** A supervisor restarting across a node
  drain will miss a renewal or two. `FARM_LEASE_RENEW_INTERVAL` is sized so that
  three attempts fit inside one TTL; two consecutive failures are ordinary. The
  5-minute `for:` exists to swallow exactly this, but a slow image pull can
  outlast it.
- **A control-plane gap.** If the API or the reaper was down, holders could not
  renew *through no fault of their own*. Check for a gap first (step 2) — if one
  is being refunded, the lease may already be recovering on its own.
- **A transport blip on the device.** Irrelevant. Lease liveness and device
  health are separate clocks; a phone that went `offline` on ADB has no bearing
  on whether its holder is renewing.
- **A high `farm_lease_suspect{protected="false"}`.** Different alert, different
  meaning, and not this one. Unprotected suspects self-heal or get reclaimed.

## First three checks

**1. Which lease, on which device, at which rack slot.**

```sh
farmd ctl leases --state suspect
```

Or, for the full picture including the physical position and how long it has
been silent:

```sh
psql "$PGURL" -c "
SELECT l.id, l.job_id, l.tenant_id, l.holder, l.holder_instance,
       s.rack_slot, d.farm_uid,
       now() - l.heartbeat_at AS silent_for,
       l.expires_at, l.reclaimable_at
  FROM farm.leases l
  JOIN farm.devices d ON d.id = l.device_id
  LEFT JOIN farm.slots s ON s.id = l.slot_id
 WHERE l.state = 'suspect' AND l.protected
 ORDER BY l.heartbeat_at"
```

`holder` and `holder_instance` name the process that should be renewing.
`silent_for` is the number that decides everything below.

**2. Was this our outage rather than theirs?**

```sh
curl -s "$FARM_API_URL/api/v1/capabilities" | jq '.roles'   # needs no token
psql "$PGURL" -c "
SELECT component, now() - beat_at AS age FROM farm.component_heartbeat ORDER BY age DESC"
psql "$PGURL" -c "
SELECT component, started_at, ended_at, ended_at - started_at AS gap
  FROM farm.control_plane_gap ORDER BY started_at DESC LIMIT 5"
```

If `api`, `scheduler` or `reaper` has a large `age`, or a gap was recorded that
covers the silence, the holder was probably renewing into a control plane that
was not there. The reaper refunds that time to the lease when it next arms; wait
for the refund rather than revoking.

**3. Is the holder alive?**

Ask the holder's own infrastructure, not this one — a CI runner pod, a
long-lived test harness, a laptop. `holder_instance` is what to look for. If the
job is one of ours, its steps say how far it got:

```sh
farmd ctl job <job-id>
farmd ctl job steps <job-id>
```

A job whose last step finished thirty seconds before the silence began was
almost certainly killed. A job mid-step may still be running on the phone with a
dead supervisor above it, and the device itself can be asked:

```sh
farmd ctl device <device-id>
```

## The decision

**If the holder is coming back:** do nothing. Silence the alert for as long as
you expect the restart to take. The lease self-heals at the same fence.

**If the holder is gone:** revoke, and understand that this discards the work.

```sh
farmd ctl lease revoke <lease-id> --reason "holder <instance> gone; confirmed <what you checked>"
```

`--reason` is mandatory and lands in `farm.audit_log` next to your name, because
this is the one action in the system where a human destroys a tenant's work on
purpose. Write the reason for the person who reads it in a month.

After the revoke the slot enters its rearm window (`FARM_SLOT_REARM`, 35s by
default) before it can be allocated again. That delay is not a bug: it
guarantees the previous holder's ADB sockets are severed before a new job
touches the phone.

## When to escalate

- **More than a handful at once, across tenants.** That is not several dead
  holders; it is one dead thing above them. Go to
  [control-plane-gap.md](control-plane-gap.md) and
  [reaper-not-leading.md](reaper-not-leading.md) before revoking anything.
- **The same tenant, repeatedly, over days.** Their supervisor is not renewing
  reliably. That is a conversation with the tenant, not a nightly revoke.
- **You cannot tell whether the holder is alive.** Do not guess. A protected
  lease costs one device; a wrong revoke costs the hours already spent on it.
  Escalate to the tenant and let the lease sit — sitting is what it is for.
