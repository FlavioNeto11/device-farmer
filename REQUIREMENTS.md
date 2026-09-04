# Requirements register

One hundred and one requirements this project has been given — functional and
non-functional — in one place, with where each came from, whether it holds
today, and the evidence for that verdict. They were mined from nine commits, the
88 gap entries across the six area pages under `internal/ui/assets/docs/`, the
migrations that are the real contract, and three external findings that never
touched the code. Five rows came from reviewing the code while writing this file
and are in no other inventory: `API-02`, `API-09`, `SEC-08`, `OBS-09`, `OBS-10`.

The same register renders in the product, under **Docs → Requirements**
(`internal/ui/assets/docs/requirements.json`), beside the capability panel it
reconciles against.

There are three reasons this file exists rather than the requirements living in
commit messages, in the six area pages under the Docs tab, and in a research
folder nobody opens.

The first is that a requirement and a gap are the same object seen from two
sides. The Docs tab documents 88 gaps beautifully and never says what any of
them was supposed to be doing. "The fence is not enforced at the resource" is
only legible next to "a fenced holder must not be able to reach the device".

The second is that the origin matters as much as the status. A requirement that
came out of an incident in somebody else's system (STF #663), a requirement that
came out of a supplier's acceptable-use policy, and a requirement that came out
of a fire-test paper are three different kinds of obligation, and they decay
differently. A contract clause changes when the supplier changes it. A physics
result does not.

The third is that a status claim is worthless without the command that produced
it. Every status below was checked against this tree. Where the check is a
one-liner, the one-liner is in the row.

## Status vocabulary

| Status | Meaning |
|---|---|
| `met` | Implemented, and demonstrated to work. |
| `partial` | Implemented, with a named hole. The row names it. |
| `not_built` | Nothing in the tree implements it. |
| `linux_only` | Implemented, reachable only on Linux — and not reachable in any deployment that ships today. |
| `unverified` | Implemented and plausibly correct, never observed working. |
| `decided` | Raised, answered by a decision rather than by code. No implementation is owed. |

`met` is the only status that means "stop reading". Everything else is a
liability with a name.

## How to read an ID and an origin

IDs are `AREA-NN`. The area is where the requirement lives, not where it is
violated: the fence-at-the-resource requirement is `SEC-04` even though its
absence is felt most in the lease protocol.

Origins:

| Origin | What it is |
|---|---|
| `c:<sha>` | A commit whose message raises or closes the requirement. |
| `gap:<area>.<n>` | Entry *n* of the `gaps` array in `internal/ui/assets/docs/<area>.json`. |
| `README` | A claim in README.md that an operator will read as a promise. |
| `cap:features` | An entry in `featureStatuses()` in `internal/api/capabilities.go`. |
| `schema` | The migration is the contract; the column or constraint is the requirement. |
| `research:*` | An external finding — a supplier contract, a fire-test result, a code table. |

## A note on what "verified" means here

Statuses in this file were checked three ways, in descending order of strength:
executed against a live farm (those checks are the ones the six area pages
carry, 304 examples of them); executed as a grep or a build against this tree;
or read from the code. Where a row says `not_built` because a symbol has no
caller, the grep is in the row and you can re-run it. Where a row says
`unverified`, nobody has watched it work and the row says so rather than
inheriting confidence from the code being tidy.

Two structural facts worth stating once, because several rows depend on them:

- **The demo is not the system.** `internal/demo` writes `farm.device_runtime`,
  `farm.recovery_attempts`, `farm.quarantines`, `farm.events` and `farm.jobs.state`
  with its own rules. Rows attributed to the demo prove the schema works and
  prove nothing about the loop whose job it is.
- **Nothing runs as its own Postgres role.** `farm_reaper`, `farm_scheduler` and
  `farm_watchdog` are created `NOLOGIN`, and every process connects with one
  `DATABASE_URL`. The role firewall is correct DDL that no deployment assumes.

---

## LEASE — the lease protocol

The founding requirement and everything that protects it.

| ID | Requirement | Origin | Status | Evidence |
|---|---|---|---|---|
| LEASE-01 | A lease ends when the job says so, when a written-down deadline elapses, or when a human takes it back. Never because of connectivity. | `README`, `c:9ab3520`, STF #663 | `met` | `farm.leases.release_reason` is CHECK-constrained to seven values with no connectivity term (`migrations/00001_core.sql`). The failure is unrepresentable, not handled. |
| LEASE-02 | Lease liveness, job liveness and device health are three clocks, separated by table, by function and by Postgres role. | `c:9ab3520` | `met` | `farm.leases` and `farm.device_runtime` are distinct tables with distinct writers; `internal/watchdog` has no lease write path; `internal/obs.TransportBlip` takes no lease identifier and returns nothing, so transport code has no value to branch on. |
| LEASE-03 | Reclamation must be structurally unable to read device health. | `c:9ab3520` (BLOCKER 3) | `partial` | The DDL is right — `farm.lease_reclaim` carries a function-level `SET role = farm_reaper` and `SELECT` on `farm.device_runtime` is revoked from that role (`migrations/00002_lease.sql`). Nothing assumes it: no Go code issues `SET ROLE`, the three roles are `NOLOGIN`, and the demo connects as a superuser. The protection actually in force is the Go import barrier — real, but a property of this build. `gap:lease.10`, `gap:devices.8` |
| LEASE-04 | The witness cap counts only *consecutive* witness-only extensions, so a renew resets it. | `c:9ab3520` (BLOCKER 7) | `met` | `migrations/00002_lease.sql:328`. Without it a job expires 12 minutes into any run that witnesses once. |
| LEASE-05 | A control-plane outage is refunded to the tenant, computed across every component on the renewal path. | `c:9ab3520` (BLOCKER 8) | `partial` | `farm.reaper_arm` computes the gap correctly for the components it is given. Three holes: `jobrunner` beats but is not in `DefaultReaperComponents`; a watched component that has *never* beaten contributes nothing to the `min()` rather than reading as infinitely stale; and `FARM_COMPONENT` is honoured only by the `api` role, so a name added to the watch list may never be written. `gap:operate.5`, `gap:operate.6`, `gap:operate.7` |
| LEASE-06 | Every mutating lease function matches on `(id, fence)`, so a stale holder's write is rejected in the database. | `c:9ab3520` | `met` | `migrations/00002_lease.sql`; `test/assertions.sql` proves the pod-eviction re-attach at an unchanged fence. |
| LEASE-07 | A zero-row renew (fenced, terminal) must never be conflated with a transient database error (retry). | `c:8adcc51` | `met` in code, `not_built` in test | `internal/lease/holder.go` distinguishes `ErrFenced` from a transient error, and the API distinguishes 410 from 503. The branch its own doc comment calls "the single most consequential decision in this project" has no test: the `leaseOps` interface exists so a stub can drive it, and nothing does. `gap:lease.6` |
| LEASE-08 | The `max_runtime` sweep must fence the ended holder and rearm its slot, as release and reclaim do. | `schema`, `gap:lease.3` | `partial` | `farm.lease_expire_max_runtime` bumps neither `fence_floor` nor `slots.rearm_at`, has no `protected` filter, and is gated by neither `reaper_state.enabled` nor `quiesce_until`. Verified: after a sweep the ended holder's write at the same fence was **accepted**. Marking a job protected does not protect it from its own `max_runtime`. |
| LEASE-09 | A job that is demonstrably alive on the device but blind to the control plane can extend its lease (witness). | `schema`, `config`, `gap:lease.4` | `not_built` | `farm.lease_witness`, `Store.Witness`, `Holder.Witness`, `FARM_LEASE_WITNESS_INTERVAL` and `FARM_LEASE_WITNESS_MAX_EXT` all exist. `Holder.Witness` has one caller and it is inside `internal/lease`; no API route, no on-device marker. Across 1117 leases, `witness_at` is NULL on every one. |
| LEASE-10 | A protected lease is never auto-reclaimed; it is held and a human is paged. | `schema`, `gap:lease.7`, `gap:lease.13` | `partial` | The behaviour is correct — proved separately by inserting leases with past deadlines. Its regression test is not watching it: assertion P10 cannot move `expires_at` backwards (the guard trigger forbids it), so `lease_mark_suspect` and `lease_reclaim` both match zero rows and the assertion passes trivially. Deleting `AND l.protected = false` from `lease_reclaim` would not fail the suite. And nobody is paged: "hold and page" is one WARN line plus two unexported counters. |
| LEASE-11 | The automatic release path has an operator-reachable kill switch. | `schema`, `gap:lease.12` | `not_built` | `farm.reaper_state.enabled` is read by `lease_reclaim` on every call and written by no API route, no `ctl` command and no dashboard control. Turning the reaper off in an incident is a hand-written UPDATE with no audit row. It also does not stop `max_runtime` (LEASE-08). |
| LEASE-12 | `FARM_LEASE_RENEW_INTERVAL` changes the renewal cadence. | `config`, `gap:lease.5` | `not_built` | `cmd/farmd/roles.go` builds `jobrunner.Config` with no `HolderConfig`, so every holder uses `lease.DefaultRenewInterval` (60s). The configured value is validated against the TTL at startup and echoed on `/api/v1/capabilities` as if in force. The startup assertion guaranteeing three renewals inside the TTL is guarding a value no holder reads; the value they do read satisfies it by coincidence. |
| LEASE-13 | `FARM_REAPER_COMPONENTS` cannot be set to something that fuses the health clock into the lease clock. | `c:9ab3520`, `gap:lease.11` | `partial` | The config refuses to *omit* a renewal-path component. Nothing refuses to *add* one. Adding `watchdog` — which reads like more thorough monitoring — makes a health-plane outage extend every lease in the farm, which is the exact fusion this system exists to prevent. Prohibited in two doc comments, enforced nowhere. |
| LEASE-14 | The way a lease ended is recoverable from an event log at 3am. | `gap:lease.2` | `partial` | `farm.events` is not that log. Measured: 1239 leases with `released_at` set, 0 `lease_released` events, 0 `lease_reclaimed`, 1 `lease_revoked`. `sweepMaxRuntime` writes no event at all, and the 1637 `lease_acquired` rows were written by the demo. The authoritative record is `farm.leases.state` plus `release_reason`. |

## DEV — device identity, topology and health

| ID | Requirement | Origin | Status | Evidence |
|---|---|---|---|---|
| DEV-01 | Every call that targets a physical position is addressed by USB devpath, never by serial. | `README`, `c:8adcc51` (BLOCKER 2) | `met` | `internal/adbwire` has zero internal dependencies and a test that fails the build if allocation vocabulary appears anywhere in it — it caught a comment its own author wrote. A clone test asserts a devpath-addressed command reaches exactly one of two devices sharing `0123456789ABCDEF`. |
| DEV-02 | One function owns "is this the device we think it is", resolving strongest evidence first: brand, fingerprint, serial-and-slot, adopt. | `c:91f3aa2` | `partial` | `farm.resolve_device` is that function and the ladder is right. Two rungs do not work. Rung 2 calls `min()` on a `uuid` column and PostgreSQL 17 has no `min(uuid)` aggregate, so any resolution reaching it raises 42883 and the sighting is recorded `pending` forever. Rung 3 sets `v_res := 'ambiguous'` but leaves `v_dev` NULL, and rung 4 tests only `IF v_dev IS NULL`, so a clone serial in an empty slot is adopted as new instead of being flagged. `gap:devices.0`, `gap:devices.1` |
| DEV-03 | A device sharing a serial with another is flagged durably, not just logged. | `schema`, `gap:devices.11` | `partial` | `farm.devices.serial_ambiguous` is written only from inside `farm.resolve_device`. The watchdog and the enroller detect collisions and only log. On any farm where enrollment is not running — which is every farm today (DEV-04) — a new clone pair produces a log line, an unregistered gauge, and no durable flag. |
| DEV-04 | A handset plugged into a host joins the fleet without a human writing SQL. | `c:40c03e4`, `cap:features`, `gap:operate.3` | `not_built` | `enroll.New` has exactly one construction site, inside `runNode`, and `farmd node` cannot start (OPS-04). No process anywhere calls `farm.resolve_device`. `/api/v1/capabilities` reports this honestly as `unavailable`. |
| DEV-05 | A host reports its USB tree and the controller, hub, power domain and slot rows grow to fit. | `c:91f3aa2`, `c:40c03e4` | `linux_only` | `farm.register_slot` works and was verified directly. `internal/topo/sysfs.go` refuses on non-Linux, and the only role that would call it cannot start. `farm.slot_occupancy` has 0 rows on the running system and all 56 devices have `hw_fingerprint IS NULL`. `gap:devices.2` |
| DEV-06 | A hub without per-port switching gets ONE ganged power domain, so the ladder cannot cycle seven devices to fix one. | `c:91f3aa2`, `README` | `met` in schema, `partial` in practice | The model is right and `farm.register_slot` builds it. The demo seed gives every hub one domain including hubs whose `power_domains.kind` is `per_port`, so tier 4's blast radius is the whole hub even on hardware that could switch one port. Whether that is wrong depends on your rack — but a per-port hub modelled as one domain produces refusals that have nothing to do with your hardware. `gap:recovery.15` |
| DEV-07 | A slot is marked, never deleted, and a hub that vanishes is reconciled. | `schema`, `gap:devices.6` | `not_built` | Marking works. Reconciliation is off in every shipped deployment: `topo.Config`'s `RetireVanished`, `Overrides`, `IncludeRootHubs`, `AdoptEmpty`, `MinPorts`, `DryRun` and `MaxRetireFraction` are wired to no config field or flag. A hub you unplug leaves its slots `active` forever. |
| DEV-08 | An operator can register a slot, re-slot a device or rebrand one without hand-written SQL. | `gap:devices.7` | `not_built` | The physical API surface is topology read, slot power, device exec and host drain/undrain. `farm.register_slot` is SQL-only; `enroll.Brander.Rebrand` has no callers. The brand-conflict error tells the operator "a human must retire it" and gives them no command with which to do it. |
| DEV-09 | Battery state of charge, battery temperature and the charge gate are collected per device. | `schema` (`00001_core.sql:209-211`), `research:lithium` | `not_built` | The columns exist, `farm.v_fleet` exposes them, `internal/api/fleet.go` serialises them and `ctl` prints them. The only writer in the tree is `internal/demo/seed.go`, once, at seed time. No watchdog code runs `dumpsys battery`. This is the register's sharpest row: a fleet whose fire mitigation is charge limiting and early thermal detection (HW-03) has a schema for both and collects neither. |
| DEV-10 | Health is a token bucket, not a raw counter, so a device cannot flap between healthy and quarantined on every blip. | `schema`, `c:5acd825` | `partial` | The damper is implemented and asymmetric by design. On the demo, `farm.device_runtime` has two writers with different rules — the real watchdog and the simulator, which has its own damper CASE and ignores `suppress_until` entirely. Every `degraded` row visible on this system came from the simulator. Calibrating a threshold against this demo is calibrating against the simulator. `gap:devices.9` |
| DEV-11 | The ADB host protocol and the sync (file transfer) protocol are spoken natively, with no shelling out to `adb`. | `c:8adcc51`, `c:cf1b690` | `partial` | Both are implemented, and the sync work found that the per-transfer context was decorative — against a silent device a step's written-down timeout could never fire. The client implements SEND, RECV, LSTAT_V1 and QUIT only: no directory enumeration, no mkdir (a push to a missing parent fails at the daemon), no STAT_V2, and `FileInfo.Size` is truncated from a `uint32`, so a file of 4 GiB or more stats as its size modulo 2^32. `gap:devices.12` |
| DEV-12 | The health plane can never touch a lease. | `c:9ab3520`, `cap:features` | `met` | `REVOKE ALL ON farm.leases FROM farm_watchdog`, plus the Go import barrier. See LEASE-03 for why the first half binds nothing at runtime; the second half holds. |

## JOB — the step model, execution and configuration

| ID | Requirement | Origin | Status | Evidence |
|---|---|---|---|---|
| JOB-01 | The step vocabulary is closed and the database owns it, so a spec written today means the same thing when it resumes tomorrow. | `c:91f3aa2`, `c:40c03e4` | `met` | `farm.step_kinds` carries the ten kinds and the idempotent flag a resume needs. `TestKindTableMatchesMigration` pins the Go vocabulary to the migration and fails if they drift. |
| JOB-02 | A transport failure inside a step is retried inside the lease, with no attempt cap, bounded only by a deadline the job's author wrote down. | `README`, `c:5acd825`, `c:cf1b690` | `met` | Observed end to end: a job survived 8 ADB transport blips across 4 renewals and completed all 10 steps; a later run survived six transport failures inside the lease that held them. This is the thesis, and it is the one that works. |
| JOB-03 | Long work runs detached on the device so no socket is the source of truth. | `README`, `c:cf1b690` | `partial` | Implemented, and the demo's own soak was rewritten from 2000 ADB round trips into one detached command plus a poll. The exit code is never checked: `execShellDetached` returns once the launch succeeds and `reattachDetached` only tests `result_path` for existence. A detached command that exited 137 produces a green step and a green `wait_for`. `gap:jobs.10` |
| JOB-04 | A job checkpoints and resumes, re-running nothing whose side effect would repeat. | `c:40c03e4` | `unverified` | The code is there and the idempotent flag it keys on is there. The demo has never exercised the resume path. `internal/runner` has no test files, and `runner.Conn` is documented as an interface specifically so `planResume` and `isRetryable` could be driven by a fake — that fake does not exist. `gap:jobs.7` |
| JOB-05 | A spec is validated before a device is allocated to it. | `gap:jobs.0`, `gap:jobs.1` | `not_built` | `specSubmissionError` exists in `internal/api/specs.go:576`, is documented as "the gate POST /api/v1/jobs applies to a spec", and is called from nowhere. `handleJobCreate` inserts `req.Spec` straight into the table. `ctl validate` runs only the local structural rules and **exits 0** on a spec naming digests that are not in the store. Verified live: seven jobs failed with `jobspec: invalid spec`, each having burned an attempt on a *different* real device. |
| JOB-06 | A reset tier expands into a real step list from the job's profile, so reset is generic. | `c:40c03e4`, `README` | `not_built` in practice | The expansion is implemented and `GET /api/v1/specs/resets` shows it. `farm.jobs.profile_id` is never written by any code path — two hits in the tree, both reads. Every reset step above `none` expands to zero sub-steps, reports `state = ok`, and cleans nothing. An operator watching for green steps between tenants is watching nothing happen. `gap:jobs.2` |
| JOB-07 | The per-job knobs the runner reads are settable when the job is filed. | `schema`, `gap:jobs.3` | `not_built` | `profile_id`, `reset_tier`, `max_attempts` and `resumable` are all in `farm.jobs` and none is in `jobCreateRequest` or in any `ctl` flag. Every job runs at `max_attempts` 3 and `resumable` true whether that suits it or not. A job that must never be retried on a second device can only be configured by an UPDATE between the insert and the placement. |
| JOB-08 | An operator can see which step failed, and what it printed, without a database session. | `gap:jobs.4`, `gap:surface.5` | `not_built` | `farm.job_steps` and `farm.job_attempts` — the two tables built to answer "which step failed" and "is this a job problem or a device problem" — have no route. `GET /api/v1/jobs/{id}` returns exactly two keys. `ctl job <id>` prints the spec and the lease history. The SSE `job` digest carries five fields, none of them a step. |
| JOB-09 | A step or attempt row whose process died is closed by something. | `gap:jobs.6` | `not_built` | `internal/runner` is the only writer and there is no sweeper. The partial index `job_steps_live` exists and no query uses it. Verified: a job reading `succeeded` with an attempt row open 19 minutes at `outcome IS NULL` and a step still `running`. These never resolve, and they poison the jobrunner's own claim predicate. |
| JOB-10 | A job's reported state agrees with its step rows. | `gap:surface.4` | `partial` | It does not. Reproduced twice: an `install` whose push failed left step 1 at `running`, step 2 never created, `farm.jobs.state` = `succeeded`, and the lease released `completed`. The API will tell you a job succeeded when its APK never landed. The `farm.device_artifacts` ledger is the only place the truth survives. |
| JOB-11 | Placement honours the job's selector. | `gap:surface.3` | `not_built` | `farm.jobs.selector` is stored, validated and echoed. The string `selector` does not appear anywhere in `migrations/00002_lease.sql`; `lease_acquire` filters on pool, admin_state, current lease, adb_state, health, slot state, rearm and `pin_device`. A job asking for `{"model":"Pixel 7"}` gets whatever is next in the failure-score ordering. Only `pin_device` narrows placement. |
| JOB-12 | A payload cannot lie about its kind, and an unrunnable spec cannot be accepted unchecked. | `c:cf1b690` | `met` | Found by running it: both `Shell` and `*Shell` satisfied `Payload` because `Kind()` has a value receiver, and the pointer form **skipped every validation rule**. Both paths now normalise, with a regression test that fails if the rules stop firing. `Kind()` on a typed nil also panicked, which would have crashed an API handler validating somebody's submission. |

## REC — recovery, blast radius and quarantine

| ID | Requirement | Origin | Status | Evidence |
|---|---|---|---|---|
| REC-01 | Tiers are stored, not hard-coded, each with a blast radius and the minimum lease disruption policy it requires. | `c:197e6f9` | `met` | `farm.recovery_tiers`. The Docs tab renders the ladder from the table, so a new rung appears without an edit. |
| REC-02 | A rung whose blast radius exceeds what a live lease permits is refused and the refusal recorded, never quietly downgraded. | `c:197e6f9`, `README` | `met` in code, `unverified` in practice | `internal/recovery` reads `farm.leases` in exactly one function, only to decide whether it is allowed to act, and writes none. On this demo the ladder's per-device escalation path has never executed: of 48 refusal rows, 37 were written by the simulator, 11 by the API slot-power handler, and 0 by the ladder. `gap:devices.10`, `gap:recovery.17` |
| REC-03 | Tiers 3 (USBDEVFS_RESET) and 4 (VBUS power) execute against real hardware. | `c:40c03e4`, `README`, `gap:recovery.1` | `not_built` | `farmd node` serves `POST /node/v1/usb-reset` and `/node/v1/port-power`. Nothing in the tree is an HTTP client for those routes, and `cmd/farmd/roles.go` constructs `recovery.NewADBActuator(log, nil)` — a nil `HostRunner` — for the `recovery`, `all` and `demo` roles alike. Two of nine rungs are decorative. A device a USB reset would have fixed reaches quarantine two rungs early. |
| REC-04 | A rung that cannot run is refused with a reason naming what is missing, rather than reported as ineffective. | `c:5acd825` | `met` | This is the right call and it is made: `internal/recovery/adbactuator.go` records `tier N needs … which only a farmd-node agent can do`. Reporting `no_change` for a rung never attempted would quarantine a device whose real problem is that nobody is listening on the host. |
| REC-05 | A refusal is queryable as a refusal. | `gap:recovery.2` | `partial` | `Ladder.perform`'s switch passes through recovered/no_change/failed and maps everything else to `OutcomeFailed`, putting the text in `detail.refusal`. So `WHERE outcome='refused'` — the obvious query, and the one the dashboard uses — silently misses every missing-host-agent and platform-unsupported refusal. |
| REC-06 | Several devices failing together on one hub is ONE incident, not N. | `c:197e6f9`, `README` | `met`, with two reporting holes | `farm.v_hub_health` is the correlation and hub-scoped quarantine is the action. But `farm.v_fleet` joins quarantines on `device_id`, which is NULL for hub- and host-scoped rows, so the grid shows a quarantined device with no explanation; and the view's `unhealthy` (`NOT IN ('healthy','retired')`) is a different definition from the ladder's (`offline, unauthorized, missing, degraded`), so the banner and the quarantine reason report different counts on the same hub. `gap:recovery.7`, `gap:recovery.8`, `gap:devices.13` |
| REC-07 | Draining a host stops new leases landing on it. | `gap:recovery.0` | `not_built` | `POST /hosts/{id}/drain` sets `farm.hosts.admin_state='draining'` and returns "no new leases will be placed on this host". `farm.lease_acquire` never reads it — `farm.hosts` appears once in `migrations/00002_lease.sql`, in a GRANT. Verified live: h02 was drained and seven new leases were acquired on it in the next sixty seconds. The only mechanism that actually stops allocation is a host-scoped quarantine. |
| REC-08 | Closing a quarantine returns its devices to the pool. | `gap:recovery.9`, `gap:recovery.10` | `partial` | `handleQuarantineClose` updates `farm.quarantines`; the health write that makes devices schedulable lives in the ladder's `reconcileQuarantines`. With the recovery role down, an operator can close every quarantine in the fleet, get 200 every time, and no device comes back. It also never restores `farm.devices.admin_state`, so any path that parks a device there leaves it permanently unschedulable *and* invisible to recovery. |
| REC-09 | Quarantine can be scoped to the granularity of the actual fault. | `schema`, `gap:recovery.13` | `partial` | `scope` permits `device`, `hub`, `host`, `slot` and `power_domain`, and there is a partial unique index for `slot`. No code writes `slot` or `power_domain`, and there is no `power_domain_id` column, so a power-domain quarantine has no way to name its subject. On mixed hardware the choice is one device or the whole hub. |
| REC-10 | The ladder's first action on a settled unhealthy device is the cheapest rung. | `schema`, `gap:recovery.12` | `partial` | Tier 0 (`observe`) is unreachable: `ladder_tier` defaults to 0 and `next(tiers, cur)` returns the first rung with `tier > cur`, so the ladder starts at tier 1. The "do nothing for one debounce window" behaviour still happens — from `DefaultDebounce` plus the watchdog's `consec_bad >= 2` — but the table advertises a rung that never runs and a budget that is never spent. |
| REC-11 | An action recorded as an open recovery attempt is closed by whoever performs it. | `gap:recovery.3` | `not_built` | `POST /slots/{id}/power` writes a tier-4 attempt row and promises "the host agent closes recovery attempt N with its outcome". No code anywhere reads `farm.recovery_attempts`. Verified: three rows open with NULL outcome, the oldest 28 minutes. Each consumes one of the tier's four hourly budget slots for that power domain. A 202 means "recorded", not "switched". |
| REC-12 | Bulk selection does not sweep up devices that are quarantined or unhealthy. | `gap:recovery.14` | `not_built` | `expandBulkSelector` filters on `adb_devpath IS NOT NULL`, `adb_endpoint IS NOT NULL` and `admin_state='enabled'`. Verified live: a run against h02 hit all seven devices of a quarantined hub and five errored. Harmless for a `getprop`; a `reboot` across such a selector produces a wave of failures and a misleading error rate. |
| REC-13 | Recovery history is queryable by the questions an operator actually asks. | `gap:recovery.16` | `not_built` | `GET /api/v1/recovery` takes `device`, `host` and `limit`. "Show me every refusal at tier 4 in the last hour" cannot be asked through the API or the dashboard. The `refusal` column exists so the UI can explain a gap; the endpoint does not let you find the gap. |

## API — the HTTP surface, the CLI and artifacts

| ID | Requirement | Origin | Status | Evidence |
|---|---|---|---|---|
| API-01 | Every non-2xx body is one envelope, and 410 fenced (abort) is distinguishable from 503 transient (lease untouched). | `c:5acd825`, `c:8adcc51` | `met` | Conflating those two is STF #663 with a different trigger, and the two codes carry that distinction all the way to `internal/lease`. |
| API-02 | The role an endpoint needs is chosen at registration, not inside the handler, so no endpoint can exist without a level. | `c:5acd825` | `partial` | `buildHandler` wraps every route in `tenant(...)` or `operator(...)` — **except one**. `GET /api/v1/capabilities` is registered with a bare `mux.HandleFunc` at `internal/api/router.go:55`. The exception is deliberate and the comment reasons it well ("an operator debugging a farm whose auth is the broken thing still needs to see its state"), but the invariant as written in `Handler`'s own doc comment, and restated in the Interfaces page summary, admits no exception. See SEC-08 for what it costs. Under `AllowAll` the split never decides anything anyway (SEC-01). |
| API-03 | An artifact's bytes are fetchable over HTTP, so roles can be split across hosts. | `gap:surface.6` | `not_built` | Five metadata routes are mounted; `Store.Get` and `Backend.Open` have no HTTP caller, and the `url` field is a `file:///…` path on the API host's own disk. The runner works only because it is the same process tree on the same disk. The artifact path does not survive the split-role deployment the compose file describes. |
| API-04 | Disk under the artifact directory is reclaimable. | `gap:surface.7` | `not_built` | DELETE removes the row and retains the bytes by design; `artifacts.Backend` has no removal verb and no sweeper ships. Verified: blobs on disk with no matching row. The design reasoning is sound — an unreferenced content-addressed blob is inert — but nothing tells you when to reclaim or how much is reclaimable. |
| API-05 | A method mismatch returns 405 with an `Allow` header. | `gap:surface.11` | `not_built` | `mux.Handle("/api/v1/", …)` matches everything under the prefix, so Go's method-mismatch handling never fires and both directions return `404 not_found`. The message names the method, which is enough to debug; a generic client that special-cases 405 retries a method error as a missing resource. |
| API-06 | `ctl` exit codes distinguish "the control plane failed" from "a target failed". | `gap:surface.11` | `partial` | `ctl bulk` returns 1 when a run completed with any target error. A bulk across 7 devices with 6 ok, 1 transport error and run state `done` exits 1, so a script treating 1 as a control-plane failure pages on a single offline handset. |
| API-07 | `ctl` validates against the server, which is the authority. | `gap:jobs.1` | `not_built` | No `ctl` code references `specs/validate`, `specs/kinds` or `specs/resets`. Both `validate` and `submit` run `jobspec.Parse` locally. The comment beside that call says "The server validates too and is the authority" and is wrong on both counts. |
| API-08 | Exit code 3 (`not implemented`) means something. | `gap:operate.15` | `not_built` | `notImplemented()` has no callers, so exit 3 is unreachable. Harmless, except that it also strands `config.Summary()`, whose only call site it is (OBS-06). |
| API-09 | The Docs reference recovers from a transient fetch failure. | `internal/ui/assets/docs.js` | `not_built` | `ensureArea` caches `{error: …}` on failure, and its own guard `if (cache.has(area) \|\| pending.has(area)) return` then blocks every retry for the life of the page — clicking the card again re-renders the same error box without re-fetching. `ensureMeta` latches the same way on `metaErr`. One dropped request on exactly the flaky link this product is designed for makes an area unreadable until a full reload, with no retry affordance. |

## SEC — authentication, authorisation and the fence at the resource

| ID | Requirement | Origin | Status | Evidence |
|---|---|---|---|---|
| SEC-01 | Callers are authenticated, and the operator role is not handed to everyone. | `README`, `cap:features`, `gap:surface.0` | `not_built` | `cmd/farmd/roles.go:148` appends `api.WithAuthenticator(api.NewAllowAll(log, "anonymous"))` unconditionally. Anyone who can reach the port can revoke any lease, drain any host, power-cycle any slot, bulk-exec across the fleet and delete artifacts. Verified: 200 with no header, 200 with a fabricated bearer token, on operator-only routes. |
| SEC-02 | The remedy the system prints for SEC-01 works. | `gap:lease.1`, `gap:operate.0` | `not_built` | `api.AuthenticatorFromEnv` — the function that reads `FARM_API_TOKENS` and would return a constant-time `StaticBearer` — has **zero** callers: `grep -rn AuthenticatorFromEnv --include=*.go . \| grep -v internal/api/auth.go` returns nothing. `/api/v1/capabilities` reports `"open":true` with the fix "set FARM_API_TOKENS to a token list", which does nothing. A wrong remedy is worse than no remedy: it converts an open port into an open port somebody believes they closed. |
| SEC-03 | The audit trail records who, not only what. | `schema`, `gap:operate.0` | `partial` | Every `farm.audit_log` row written through the API carries actor `anonymous`, including operator `reason` text on revokes and drains. The trail is complete and anonymous. |
| SEC-04 | A fenced holder cannot reach the device. | `README`, `cap:features`, `gap:lease.0`, `gap:surface.1` | `not_built` | The fence is enforced in PostgreSQL and honoured by cooperating clients. No Go file reads `devices.fence_floor` to gate an ADB operation. `FARM_NODE_SELF_FENCE_TIMEOUT` is validated at startup and has no consumer. **The user-facing text says otherwise**: the revoke response body and `ctl lease revoke` both print "any socket still carrying fence N is refused at the host proxy", and five code comments say the same. The real barrier after a revoke is the 35-second slot rearm — and the old holder may not notice its own eviction for a full renewal interval (60s), which is longer. Size the rearm accordingly. |
| SEC-05 | The secret half of the renew triple does not leak. | `gap:surface.2` | `not_built` | `handleLeaseRelease` skips a tenant check on renew *because* a code comment asserts `holder_instance` "is returned only to the acquirer and is never exposed by a farm-wide view". `GET /api/v1/leases` returns it for every lease. So `lease_id`, `fence` and `holder_instance` — exactly what `farm.lease_renew` matches on — are readable from one unauthenticated GET. Even with tokens wired, two CI shards in one tenant could take each other's leases. |
| SEC-06 | A job id is not sufficient to take a live lease away from its holder. | `gap:surface.10` | `not_built` | `farm.lease_acquire` is idempotent on `job_id` and its re-attach path overwrites `holder` and `holder_instance`. The previous process's next renew returns 410 at an *unchanged* fence — indistinguishable from a release. Job ids are readable from `/api/v1/jobs`, `/fleet` and the event stream. Acquire also never reads `farm.jobs.state`, so it hands out a device to a cancelled job until the runner's preflight notices. |
| SEC-07 | Read surfaces are tenant-scoped. | `gap:surface.8` | `partial` | `tenantScope` is applied in eight places. `/events`, `/fleet`, `/topology`, `/hosts`, `/recovery`, `/bulk` and `/stream` are not among them. A tenant token would read every other tenant's audit rows and every live lease's identifiers. The stream is the hardest: one shared poller fans out one shared snapshot, so scoping it means a poller per tenant. |
| SEC-08 | Wiring a token closes every route that carries fleet state. | `internal/api/router.go:55` | `not_built` | `GET /api/v1/capabilities` is the one route with no role (API-02), so it is the surface that stays open in the deployment where SEC-01 and SEC-02 are finally fixed. It returns device, host, live-lease, quarantine, job and artifact counts; the VCS revision; the schema version; the lease TTL, grace, renewal and rearm windows; the reaper gap floor; per-role liveness — and, while auth is open, a sentence saying anyone who reaches the port can revoke leases and power-cycle slots. The fix keeps the reasoning intact: serve the build, schema and auth halves unauthenticated, and put the fleet counts and timings behind the tenant role. |

## OBS — observability

| ID | Requirement | Origin | Status | Evidence |
|---|---|---|---|---|
| OBS-01 | The two series that mean "work was destroyed" are armed from the first scrape of a fresh process, not from the first casualty. | `internal/obs` doc, `c:8adcc51` | `met` | `obs.Register` pre-creates the `lease_reaped_total` and `lease_renew_failures_total` children. A counter with no observations has no series, and `increase(x[15m]) > 0` over a series that does not exist never fires. This is the one metrics requirement that is fully honoured. |
| OBS-02 | Each role exports its own metrics. | `gap:operate.10`, `gap:lease.8`, `gap:jobs.5`, `gap:devices.3` | `not_built` | Nine packages define `Collectors()`; exactly one call site exists in production code, `internal/api/server.go:369`, and it registers `adbwire`'s. Everything the scheduler, reaper, jobrunner, recovery, watchdog, topo, enroll and node measure is dead code — including `farm_scheduler_leader` and `farm_reaper_leader`, whose sum across replicas must be exactly 1, and `farm_jobrunner_panics_total`, whose own help text says to alert on any rate above zero. |
| OBS-03 | OBS-02 is a one-line fix. | `gap:recovery.4` | `not_built` — **and it is not** | `internal/obs` declares `recoveryAttempts` as namespace `farm` + name `recovery_attempts_total` with five labels; `internal/recovery` declares `attemptsTotal` as namespace `farm` + subsystem `recovery` + name `attempts_total` with two. Both resolve to `farm_recovery_attempts_total`. Prometheus refuses the second registration, and `newRegistry` propagates the error — so adding `recovery.Collectors()` makes `farmd` **fail to start**. Fixing OBS-02 requires renaming one of the two first. Sequence it that way or the first attempt looks like a regression. |
| OBS-04 | The Go runtime and process collectors are exported. | `gap:operate.10` | `not_built` | `internal/obs` deliberately leaves this to the binary, and the binary does not do it. No goroutine count, no RSS, no GC. |
| OBS-05 | `FARM_METRICS_ADDR` binds a listener. | `config`, `gap:operate.9` | `not_built` | `MetricsAddr` appears in `config.go` and in the summary string and nowhere else; `/metrics` is served on `FARM_API_ADDR`. The Dockerfile's `EXPOSE 9090` documents a port nothing listens on. |
| OBS-06 | A running `farmd` can be asked what configuration it booted with. | `gap:operate.9`, `gap:operate.15` | `not_built` | `config.Summary()`'s only call site is inside `notImplemented()`, which has no callers (API-08). Reconstructing the effective config means reading the process environment — and four of those variables are validated and then ignored (LEASE-12, LEASE-09, OBS-05, OPS-09), so the environment does not tell you the truth either. |
| OBS-07 | A rising `refused_ganged` rate tells you the rack needs per-port power switching. | `internal/obs` doc, `gap:recovery.5` | `not_built` | The only producer of `OutcomeRefusedGanged` is the ladder's per-device path, which has never run here; the demo hardcodes `refused_policy` even for its own ganged refusals. Live `/metrics` carries three outcome values. Ganged refusals are indistinguishable from policy refusals; you have to read the `refusal` text. |
| OBS-08 | Alerting rules, runbooks and a pager path ship with the tree. | `gap:lease.13` | `not_built` | There is no `docs/`, no `deploy/`, no `.github/`. "Hold and page" is one WARN log line plus two counters that are not exported (OBS-02). A protected lease that goes suspect is held indefinitely and correctly, and nobody is told. |
| OBS-09 | The capability panel says so when it could not measure, instead of reporting the unmeasured value. | `internal/api/capabilities.go` | `not_built` | All three probes swallow their error: `schemaInfo` and `fleetCounts` discard it with `_ = s.pool.QueryRow(...)`, and `roleStatuses` fills its map only `if err == nil`. `handleCapabilities` then returns 200 unconditionally. With Postgres unreachable — or the pool merely saturated, which the package doc warns about by name — the page renders `schema v0`, the note "no migrations applied; run farmd migrate up", every role "never beat", every count zero, and "No reaper is beating: an abandoned lease will never be reclaimed". Nine false statements, none flagged; and the 200 means the dashboard's own control-plane banner never fires either. The most alarming screen in the product appears during the one incident where it is not the truth. |
| OBS-10 | `FARM_MIGRATIONS_TABLE` is honoured by everything that reads the migration table. | `config`, `cmd/farmd/migrate.go:189` | `not_built` | `migrate` calls `goose.SetTableName(cfg.MigrationsTable)`; `schemaInfo` hardcodes `FROM goose_db_version`, unqualified, while the default is `public.goose_db_version` — so it also depends silently on `search_path`. An operator who overrides the variable gets a failing query and, because of OBS-09, is then told their fully migrated database has no schema. |

## TEST — what is actually proved

| ID | Requirement | Origin | Status | Evidence |
|---|---|---|---|---|
| TEST-01 | The lease protocol is proved against a real PostgreSQL. | `c:8adcc51`, `README` | `met` | `test/assertions.sql` — 15 assertions, all passing. Running them against PostgreSQL 17.10 rather than reading the SQL found three defects: `#variable_conflict` shadowing in two functions, a `lease_witness` comparison that could never match, and a `SET LOCAL ROLE` that leaked into the caller's transaction. |
| TEST-02 | The assertions run against the database you care about. | `gap:lease.14` | `partial` | The fixture inserts `racks('r1')`, `hosts('h01')`, `pools('default')` and `tenants('acme')`, all of which the demo seed owns, so the suite aborts on a duplicate key before the first assertion. Verifying the protocol means a scratch database — which means you are not verifying the one you care about. |
| TEST-03 | The most expensive failure mode has a regression test that is watching it. | `gap:lease.7` | `partial` | See LEASE-10: assertion P10 passes trivially because it cannot construct the state it is meant to test. |
| TEST-04 | The Go decision points are tested. | `gap:lease.6`, `gap:jobs.7`, `gap:devices.4` | `not_built` | Seven `_test.go` files exist, in `internal/adbwire` (4), `internal/jobspec` (2) and `test/fakeadb` (1). `internal/lease`, `reaper`, `scheduler`, `api`, `runner`, `jobrunner`, `recovery`, `enroll`, `topo`, `watchdog`, `config`, `artifacts`, `ctl`, `node`, `obs` and `demo` report `[no test files]`. Two of the SQL defects in DEV-02 would each have been caught by one test. |
| TEST-05 | The suite runs on every change. | `gap:lease.6` | `not_built` | There is no `.github` directory. Nothing runs `go test`, `gofmt` or the assertions automatically. |
| TEST-06 | Documentation claims are executed before they ship. | `c:ae7ac8c` | `met` | The verification pass ran 304 examples against a live farm, found 108 wrong and fixed them, and corrected 98 claims against the files they cite. A third of documentation written from reading code did not survive being run. That ratio is the argument for the method, and it is the reason this register cites commands rather than conclusions. |

## OPS — deployment and operability

| ID | Requirement | Origin | Status | Evidence |
|---|---|---|---|---|
| OPS-01 | One static binary; every role is a subcommand, so a reaper cannot run a different commit than the scheduler it races with. | `README`, `c:d872818` | `met` | Distroless, `CGO_ENABLED=0`, no shell, no package manager, and **no default role** — a manifest with a typo fails instead of starting something plausible. |
| OPS-02 | Migrations travel inside the binary and run from any working directory. | `c:5acd825` | `met` | `cmd/farmd` adopts the migrations package's `embed.FS`. The package refuses to be empty in two ways: a `//go:embed` pattern matching no files is a build error, and an init-time check catches "at least one file" not being "the schema" — because goose asked for zero migrations applies zero, reports success and exits 0, and the first symptom is a query error in a role nobody was watching. |
| OPS-03 | The system is runnable with no hardware, against the real control plane. | `c:5acd825`, `README` | `met` | 56 simulated devices across 2 in-process fake ADB servers, driving the real scheduler, lease store, reaper, recovery ladder and job runner. The fake is only the hardware. Also `docker compose up -d` and `scripts/dev-up.ps1`, which stands up a throwaway PostgreSQL on a private port and never touches an existing service. |
| OPS-04 | The host agent runs. | `gap:operate.1` | `not_built` | `runNode` passes `Addr: cfg.NodeAddr` and never passes `Token`; `node.New` errors whenever `Addr != "" && Token == ""`; and `FARM_NODE_ADDR` cannot be blanked because the loader treats an empty value as unset and returns the `:8082` default. **There is no environment that satisfies the guard.** This one row is why DEV-04, DEV-05 and REC-03 are all `not_built`: it is the cheapest fix in the register and it unblocks the most. |
| OPS-05 | Each host agent is distinguishable in the heartbeat table. | `gap:operate.1`, `gap:recovery.6` | `not_built` | `runNode` hardcodes `Component: "node"`, discarding the package's own `"node:" + hostID` default. In a multi-host fleet every agent would write the same row, so one healthy host's beat hides a dead host — exactly what the per-host key exists to prevent. |
| OPS-06 | `docker compose --profile farm up -d` starts a real farm. | `gap:operate.11` | `partial` | The `demo` service has no `profiles:` key, so the documented command also starts simulated hardware writing into the same database as the real control plane — and the second of `demo` and `api` to start fails to bind 8420. Workaround: `--scale demo=0`. `docker compose --profile farm config` shows this before you deploy, without a daemon. |
| OPS-07 | Kubernetes manifests or a Helm chart ship. | `README`, `cap:features`, `gap:operate.12` | `not_built` | The README's architecture diagram puts the control plane in Kubernetes and nothing in the repo will put it there. An operator writes their own Deployments and with them their own answers to things the code has opinions about: singleton replica counts, PodDisruptionBudgets that respect the advisory-lock leaders, the migrate Job's ordering, and `terminationGracePeriodSeconds` exceeding `FARM_SHUTDOWN_GRACE`. |
| OPS-08 | SIGTERM drains work and releases nothing. | `c:ae7ac8c`, `gap:operate` | `met` | Deliberate, and the one thing to carry away from the operating page. A shutdown that released leases would be the same failure as a socket error releasing them. |
| OPS-09 | `FARM_COMPONENT` and `FARM_SHUTDOWN_GRACE` are honoured by the roles that read them. | `gap:operate.4`, `gap:operate.9` | `partial` | `FARM_COMPONENT` is honoured by the `api` role only; every other role hardcodes its component name, so a name set to satisfy the preflight never reaches `farm.component_heartbeat` — and therefore buys nothing when added to `FARM_REAPER_COMPONENTS` (LEASE-05). `FARM_SHUTDOWN_GRACE` has one consumer and affects only roles that serve HTTP. |
| OPS-10 | A default profile exists, so a reset tier has meaning on a fresh farm. | `c:d872818` | `met` | Seeded. Without it `GET /api/v1/specs/resets` could only answer 404 on a fresh farm, because `medium` is defined as "uninstall everything the profile does not own". Note this makes the tier *definable*, not *effective* — see JOB-06. |

## HW — the physical farm, its suppliers and its hazards

These are the requirements that do not live in the code, and two of them are the
reason the code exists at all. They are recorded here because an operator who
rediscovers them during procurement, or during an incident, discovers them at the
worst possible moment.

| ID | Requirement | Origin | Status | Evidence |
|---|---|---|---|---|
| HW-01 | Continuous, unattended, long-running use of third-party apps on managed device clouds. | `research:contract` | `decided` — **excluded by contract; own hardware is the path** | Not a pricing or capability judgement; four suppliers exclude it in writing. Sauce Labs restricts use to "legitimate testing or validation" in its AUP, and ToS §1.1 makes the AUP a condition of the licence. AWS Service Terms §35.2 forbids rooting or unlocking and forbids installing "persistent software on devices". BrowserStack §4.3 requires the customer to warrant rights over "the application package itself". Kobiton §2.3 forbids re-providing device access. Renting is not an option that was rejected; it is an option that does not exist for this workload. |
| HW-02 | ~50–60 handsets across a small number of bare-metal hosts. | `README` | `met` (design), `unverified` (hardware) | 28 devices per host is STF's own reference build: four powered seven-port hubs on a dedicated PCIe USB 3.0 card. Two hosts for 50–60; three if you want to drain one for maintenance without stopping — which today is a physical drain, because the software one does not work (REC-07). |
| HW-03 | Fire mitigation appropriate to a rack of lithium cells. | `research:lithium` | `not_built` in the product; `decided` in approach | Clean-agent suppression does **not** stop a lithium event. In the Fire Safety Journal work, Novec 1230 at 8.5 vol% failed both to suppress the fire and to prevent propagation to neighbouring cells; propagation occurred even in a pure-nitrogen atmosphere, because thermal runaway supplies its own oxidiser. Buying a clean-agent bottle and considering the hazard handled is the failure mode this row exists to prevent. The mitigations that do work are physical and procedural: containment, cell-to-cell spacing, charge limiting, and early detection. The product supports **none** of them — see DEV-09: the schema has `battery_pct`, `battery_temp_dc` and `charge_gate`, and nothing outside the demo seed has ever written a value into any of the three. |
| HW-04 | Compliance with the fire code's energy-storage threshold. | `research:firecode` | `decided` — not the binding constraint | IFC Table 1207.1.1 sets the lithium-ion ESS permitting threshold at 20 kWh. A phone battery is roughly 17 Wh, so about 60 handsets is on the order of 1 kWh — some 5% of the trigger. The code is not what stops you. **Operator and landlord policy is**, and it is a conversation to have before the hardware arrives rather than after. Do not read this row as "the hazard is small": HW-03 is unchanged by it. |
| HW-05 | Per-port USB power switching, so tier 4 can cycle one port. | `c:91f3aa2`, `README` | `partial` | The schema models `per_port` and `ganged` domains and the ladder respects the difference — a hub without per-port switching gets one ganged domain, which is what stops it cycling seven devices to fix one. Two things blunt it: the demo seed gives even `per_port` hubs a single domain (DEV-06), and the `acknowledged` field that would let the control plane tell the agent "every lease in this domain permits it" is never populated, so on ganged hardware the agent refuses tier 4 unconditionally. `gap:recovery.11` |
| HW-06 | Linux 6.0 or newer on every host. | `c:40c03e4`, `README` | `met` in code, `unverified` on hardware | Below 6.0 the kernel silently re-powers a disabled port, so a cycle that appears to succeed did nothing — which is worse than a refusal, because it teaches the ladder to escalate past a rung that never ran. `internal/node` enforces the floor. Nothing in `internal/node` or `internal/topo` has ever run against a phone; budget a bring-up period on the first physical host, which cannot begin until OPS-04 is fixed. |
| HW-07 | A PostgreSQL version the schema actually runs on is stated somewhere. | `gap:devices.0` | `not_built` | `docker-compose.yml` pins `postgres:17-alpine` and nothing anywhere declares a version requirement — while `farm.resolve_device` rung 2 needs a `min(uuid)` aggregate that PostgreSQL 17 does not have (DEV-02). The error handler for this case even logs "check the PostgreSQL version against migrations/00004_operate.sql", so the failure was anticipated and never gated. A version floor belongs in the migration and in the compose file. |

---

## Reconciling `featureStatuses()`

`internal/api/capabilities.go` hand-maintains a twelve-entry feature list that
the Docs tab renders as "what is enabled, and how". Six entries compute their
state from a heartbeat; six are hardcoded. It is the most-read status surface in
the product, so where it disagrees with this register, that is a defect in the
list.

Three defects belong to the panel rather than to any entry in it: it is
registered with no role, so it is the one surface a token does not close
(SEC-08); it discards the error from all three of its database probes and
answers 200 regardless, so an unreachable database renders as a confident
diagnosis (OBS-09); and it reads `goose_db_version` directly rather than
`cfg.MigrationsTable` (OBS-10).

| Feature | It reports | Register says | Reconciliation |
|---|---|---|---|
| Device leasing | `enabled` (hardcoded) | LEASE-01 `met` | Agrees. |
| Job execution | `enabled` iff `jobrunner` beats | JOB-02 `met`, JOB-05 `not_built` | Understates the risk. Jobs execute; they are not validated first, so `enabled` covers a path that allocates a real device to an unrunnable spec. |
| Automatic reclamation | "The only automatic release path in the system" | LEASE-08 | **Wrong.** `farm.lease_expire_max_runtime` is a second automatic path, and it is gated by neither `reaper_state.enabled` nor `quiesce_until` — so this reads `unavailable` while leases are still being ended. |
| Health monitoring | `enabled` iff `watchdog` beats | DEV-10 `partial` | On the demo the simulator is a second writer with different rules. True in production, misleading here. |
| Recovery ladder | Detail branches on whether `node` beats: "A host agent is present, so tiers 3 and 4 can act" | REC-03 `not_built` | **Wrong predicate.** The actuator's `HostRunner` is nil unconditionally at `cmd/farmd/roles.go:206`. A beating `node` would make this sentence appear and it would still be false. Key it on the wiring, not the heartbeat. |
| Dynamic enrollment | `unavailable` when nothing beats | DEV-04 `not_built` | Right answer, incomplete reason. It is not that no enroller is beating; it is that no enroller **can** beat (OPS-04). |
| File transfer | `enabled` (hardcoded) | DEV-11 `partial`, `gap:jobs.8` | Overstates. No directory enumeration, no mkdir, sizes truncated above 4 GiB — and push, install and pull have never completed on this deployment. |
| Artifacts | `enabled` (hardcoded) | API-03, API-04 `not_built` | Overstates. Content addressing and the `EnsureOnDevice` skip work; there is no way to fetch bytes over HTTP and no way to reclaim disk. |
| Live updates | `enabled` (hardcoded) | SEC-07 `partial` | True. Unscoped, which the entry does not say. |
| Authentication | `not_built`, with a fix | SEC-01, SEC-02 | State agrees. The **fix string is wrong** — `authInfo()` names `FARM_API_TOKENS`, which has no consumer. |
| Fence enforcement at the resource | `not_built` | SEC-04 | Agrees, and is the most honest entry in the list. It is contradicted elsewhere in the product: the revoke response body says the opposite. |
| Helm chart | `not_built` | OPS-07 | Agrees. |
| *(absent)* | — | REC-07, JOB-06, JOB-11, LEASE-09, OBS-02 | Five things an operator would reasonably assume exist are not in the list at all: host drain, reset tiers, selector placement, witness extensions, per-role metrics. A feature list that names only built features cannot warn you about the ones you assumed. |

## Where the requirements came from

| Origin | Raised | Notable |
|---|---|---|
| `c:9ab3520` core schema and lease protocol | LEASE-01..06, LEASE-13, DEV-12 | The founding commit. Applies three fixes a design critique flagged as blockers, all three still holding. |
| `c:8adcc51` Go foundation | DEV-01, LEASE-07, API-01, TEST-01 | Three defects found by running the SQL against PostgreSQL 17.10 rather than reading it. |
| `c:197e6f9` recovery ladder, quarantine, views | REC-01, REC-02, REC-06 | Tiers stored rather than hard-coded; quarantine scoped so six devices on one hub is one incident. |
| `c:5acd825` operator interface, control loops, demo | OPS-02, OPS-03, REC-04, API-02 | The import-barrier test caught a comment its own author wrote. |
| `c:91f3aa2` dynamic enrollment, topology, step model | DEV-02, DEV-06, JOB-01, HW-05 | A serial alone identifies nothing; the clone flag is only computable after the insert. |
| `c:40c03e4` runner, enrollment, topo, host agent | JOB-04, JOB-06, REC-03, HW-06 | Hardening caught `pm clear` failing on a package that is not installed — every first reset of every newly enrolled device would have quarantined a healthy phone for being clean. |
| `c:cf1b690` jobs execute on devices | JOB-02, JOB-03, JOB-12, DEV-11 | Four defects found by running it: no file transfer at all, a decorative per-transfer context, pointer payloads skipping every validation rule, and `Kind()` panicking on a typed nil. |
| `c:d872818` packaging | OPS-01, OPS-06, OPS-10 | No default role, so a manifest typo fails instead of starting something plausible. |
| `c:ae7ac8c` Docs tab | TEST-06, and the 88 gap entries behind most rows above | 304 examples executed, 108 wrong, 98 claims corrected. |
| `research:contract` | HW-01 | Four suppliers, four separate clauses. The decision that makes this a hardware project. |
| `research:lithium` | HW-03, DEV-09 | Novec 1230 at 8.5 vol% failed to suppress and failed to prevent propagation; propagation occurred in pure nitrogen. |
| `research:firecode` | HW-04 | IFC Table 1207.1.1: 20 kWh. ~60 handsets ≈ 1 kWh. |
| review while writing this file | API-02, API-09, SEC-08, OBS-09, OBS-10 | Five requirements in no other inventory. The register found them by asking what each surface was *supposed* to guarantee, which is the question a gap list does not ask. |

## What the register argues for next

Ordered by unblocked-per-unit-of-work, not by severity.

1. **OPS-04 — make `farmd node` startable.** One guard and one default. It is the
   sole blocker under DEV-04, DEV-05, REC-03 and HW-06, and nothing else in the
   register unblocks four rows.
2. **JOB-05 — call `specSubmissionError`.** The function is written, documented as
   the gate, and never called. One call site stops real devices being allocated to
   specs that cannot run.
3. **SEC-02 — call `AuthenticatorFromEnv`, or delete the fix string.** Either close
   the port or stop telling operators they closed it. The second half is a one-line
   change and is more urgent than the first, because a wrong remedy is worse than a
   documented absence.
4. **OBS-09 — stop the capability panel diagnosing an outage as an empty farm.**
   Three `_ =` discards and one status code. During a database outage the panel
   currently states nine specific untrue things and suppresses the dashboard's own
   "control plane is not answering" banner by answering 200.
5. **REC-07 — teach `lease_acquire` about `hosts.admin_state`.** One predicate. The
   endpoint already exists and already lies.
6. **OBS-03 then OBS-02 — rename the colliding metric, then register the collectors.**
   In that order, or `farmd` fails to start and the fix looks like a regression.
7. **JOB-06/JOB-07 — write `profile_id` and the three sibling columns from the API.**
   Until then every reset between tenants is a green step that cleans nothing.
8. **DEV-09 — collect battery telemetry.** The columns, the view, the API field and
   the CLI column all exist. This is the only row in the register where a physical
   safety mitigation (HW-03) is waiting on a `dumpsys` call.
9. **DEV-02/HW-07 — fix the `min(uuid)` rung and declare a PostgreSQL floor.**
   Sequence this with OPS-04: enrollment starting and immediately parking every new
   device at `pending` is a worse failure than enrollment not starting.
10. **TEST-04/TEST-05 — a test for `isRetryable` and `planResume`, and something that
   runs it.** A regression in either reintroduces #663, which is the one failure this
   project defines itself against.
