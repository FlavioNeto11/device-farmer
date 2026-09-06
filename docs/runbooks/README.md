# Runbooks

One file per alert in `deploy/helm/device-farmer/templates/prometheusrule.yaml`.
Every alert's `runbook_url` annotation points here, so the page you are holding
has a link to the file that tells you what to do about it.

These are written for 03:00. That means: the first paragraph says whether
anything is on fire, the second says what is *not* wrong, and the commands are
copy-pasteable rather than illustrative.

## The one thing to know before you touch anything

**A lease ends when its job ends, when a deadline the user wrote down elapses,
or when a human revokes it. Nothing else.**

Not a socket error, not a probe timeout, not a device going offline, not a pod
dying, and not an alert firing. If a step in one of these runbooks looks like it
would release a device as a side effect, it is a bug in the runbook — none of
them do. The only command here that ends a lease is `ctl lease revoke`, and it
is always spelled out as a decision, never as a step.

This matters because the failure mode this project exists to prevent is the
opposite habit: DeviceFarmer/STF issue #663, where a transport error is read as
a device loss and a ninety-minute `ECONNRESET` on a perfectly healthy phone
destroys somebody's multi-hour test run. That bug is far more often introduced
by an operator procedure than by a line of code.

**`farmd ctl endings` is how you check it.** With a lease id it answers "how did
THIS one end"; without one it lists what has been ending, filterable by class —
`job`, `deadline`, `operator`, `reaper` — and by time. It reads the ledger a
trigger on `farm.leases` writes inside the same transaction as every ending, so
an ending cannot be missing from it while the lease is closed. Two answers there
are incidents rather than information: `unaccounted`, meaning a lease was closed
with no reason recorded at all, and a terminal lease the ledger has no row for.
The command says so on `stderr` in both cases.

## Setup, once per shell

```sh
export FARM_API_URL=https://device-farmer.example.com
export FARM_API_TOKEN=...                 # a credential with the operator role
export PGURL='postgres://farm@postgres.example.com:5432/device_farmer?sslmode=require'
```

`farmd ctl` drives the HTTP API and nothing else — it has no database
credentials — so everything it can do is audited in `farm.audit_log` under your
name. The `psql` queries below are read-only unless a runbook says otherwise in
capital letters. Prefer `ctl`; reach for `psql` when you need a join `ctl` does
not offer.

## The alerts

### Work is being destroyed, or a human is being waited on

| Alert | Runbook |
| --- | --- |
| `DeviceFarmerProtectedLeaseSuspect` | [protected-lease-suspect.md](protected-lease-suspect.md) |
| `DeviceFarmerLeaseReclaimed` | [lease-reclaimed.md](lease-reclaimed.md) |
| `DeviceFarmerLeaseFenced` | [lease-fenced.md](lease-fenced.md) |

### The control plane

| Alert | Runbook |
| --- | --- |
| `DeviceFarmerAPIDown` | [api-down.md](api-down.md) |
| `DeviceFarmerAPIAuthOpen` | [api-auth-open.md](api-auth-open.md) |
| `DeviceFarmerControlPlaneGap`, `DeviceFarmerControlPlaneGapBudget` | [control-plane-gap.md](control-plane-gap.md) |
| `DeviceFarmerComponentBeatFailing` | [component-beat-failing.md](component-beat-failing.md) |
| `DeviceFarmerReaperNotLeading`, `DeviceFarmerSchedulerNotLeading` | [reaper-not-leading.md](reaper-not-leading.md) |

### Hardware

| Alert | Runbook |
| --- | --- |
| `DeviceFarmerHubDown`, `DeviceFarmerHubDegraded` | [hub-correlated-failure.md](hub-correlated-failure.md) |
| `DeviceFarmerDevicesQuarantined`, `DeviceFarmerHubQuarantined` | [devices-quarantined.md](devices-quarantined.md) |
| `DeviceFarmerRecoveryRefusedGanged` | [recovery-refused-ganged.md](recovery-refused-ganged.md) |
| `DeviceFarmerRecoveryRefusedPolicy` | [recovery-refused-policy.md](recovery-refused-policy.md) |
| `DeviceFarmerRecoveryFailing` | [recovery-failing.md](recovery-failing.md) |
| `DeviceFarmerBatteryAnomaly` | [battery-anomaly.md](battery-anomaly.md) |

### Jobs and coverage

| Alert | Runbook |
| --- | --- |
| `DeviceFarmerJobsFailing` | [jobs-failing.md](jobs-failing.md) |
| `DeviceFarmerAlertingBlind` | [alerting-blind.md](alerting-blind.md) |

## Reading a rack_slot

Alerts about a device or a slot carry `rack_slot`, and it is the whole point of
the label: `R2-U14-H3.1.4-P5` is rack 2, shelf 14, the hub reached through USB
path `3-1.4`, socket 5. A UUID sends you to a spreadsheet and then to the wrong
phone; this sends you to a place.

Hub-level alerts carry a `rack_slot` of the form `H3.1.4-P*`, because the alert
is about a hub and not a socket. The rack and shelf come from `farm.hosts`:

```sh
psql "$PGURL" -c "SELECT id, rack_id, rack_unit, adb_endpoint FROM farm.hosts ORDER BY id"
```

## What is never a page

`farm_adb_transport_blips_total`. If you ever find yourself adding a rule over
it, read `internal/obs/doc.go` first — the reasoning is long, and it is load
bearing.
