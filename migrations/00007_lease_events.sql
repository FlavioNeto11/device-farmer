-- +goose Up

-- =====================================================================
-- THE LEDGER OF ENDINGS
--
-- A lease ends when the job says so, when a deadline the user wrote
-- down elapses, or when a human takes it back. That sentence is the
-- whole system — and until this migration the database kept no record
-- of which of the three it was.
--
-- Verified on a running farm before this was written: 75 leases carried
-- released_at, and farm.events contained lease_acquired,
-- job_attempt_started, job_attempt_finished, job_succeeded,
-- job_created, device_offline, device_flapping and hub_quarantined.
-- Not one ending. Every lease in the fleet had been closed and the
-- timeline said nothing about any of it, so the first question of every
-- incident review — "why did this lease end, and who ended it" — could
-- only be answered by reading farm.leases row by row, which is state,
-- not history: it says how the lease looks now, and is overwritten by
-- the next thing that touches the row.
--
-- WHY THIS IS A TRIGGER AND NOT GO CODE
--
-- Four call sites can end a lease today — farm.lease_release,
-- farm.lease_reclaim, farm.lease_expire_max_runtime and the operator
-- revoke in internal/api/leases.go — and each writes (or forgets to
-- write) its own audit row. internal/reaper/reaper.go is the honest
-- example: auditReclaims writes 'lease_reclaimed' AFTER the reclaim
-- transaction has committed, from a separate batch that can fail on its
-- own, which is why that package carries a metric counting reclaims
-- whose trail was lost. The leases are closed; the record is not.
--
-- A trigger on farm.leases inverts that. The ledger row is written in
-- the SAME transaction as the state change, so the record cannot be
-- lost while the ending survives, and — the part that matters for a
-- system that will grow a fifth and sixth ending path — a new caller
-- gets the row for free. It cannot be bypassed by forgetting.
--
-- The direction of failure is deliberate too. If the ledger insert
-- fails, the ending fails and the lease stays held. That is the safe
-- side: release is idempotent, the holder retries, and no device is
-- taken from a running job by a bookkeeping error.
--
-- The Go-side rows are NOT replaced. 'lease_released' and
-- 'lease_revoked' name the human and their typed reason, which the
-- database does not know; 'lease_ended' is the row that always exists.
-- =====================================================================


-- ---------------------------------------------------------------------
-- Who ended it, derived from the reason.
--
-- The vocabulary mirrors the axiom one for one, because that is the
-- classification an incident review actually wants. release_reason has
-- seven values and no connectivity value; a NULL means a lease reached
-- a terminal state without anyone recording why, which is the exact
-- failure this project exists to prevent and is therefore named rather
-- than blanked.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.lease_ended_by(p_reason text)
RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $fn$
  SELECT CASE p_reason
           WHEN 'completed'        THEN 'job'
           WHEN 'failed'           THEN 'job'
           WHEN 'job_cancelled'    THEN 'job'
           WHEN 'max_runtime'      THEN 'deadline'
           WHEN 'operator_revoked' THEN 'operator'
           WHEN 'device_retired'   THEN 'operator'
           WHEN 'holder_expired'   THEN 'reaper'
           ELSE 'unaccounted'
         END
$fn$;
-- +goose StatementEnd

COMMENT ON FUNCTION farm.lease_ended_by(text) IS
  'Classifies a farm.leases.release_reason as job | deadline | operator | reaper | unaccounted.';


-- ---------------------------------------------------------------------
-- Idempotence, by index rather than by convention.
--
-- One ending is one ledger row. The partial unique index makes a second
-- row for the same lease impossible, so the INSERT can name it as its
-- conflict arbiter and do nothing — the backfill below and any future
-- replay are then safe to run twice.
--
-- It is also the read path: "why did this lease end" becomes a single
-- index probe instead of a scan of the timeline.
--
-- Cost is confined to endings. The index covers only kind='lease_ended'
-- rows, so the far more frequent job_attempt_* and device_* inserts
-- never touch it.
-- ---------------------------------------------------------------------

CREATE UNIQUE INDEX IF NOT EXISTS events_lease_ending
  ON farm.events (lease_id) WHERE kind = 'lease_ended';

-- GET /api/v1/events answers "what happened, newest first", and merges
-- this table with farm.audit_log — which has had audit_at since
-- 00003_ops.sql. farm.events never got the matching index, so the half
-- of that merge which grows fastest was answered by reading the whole
-- table and top-N sorting it. Measured: on the running farm's 1469
-- events that half is a Seq Scan plus a top-N heapsort touching 60
-- buffers, and on a synthetic 200k-row copy WITH this index the same
-- newest-200 question is an index-only scan touching 8, independent of
-- how much history the farm has accumulated. Appending to a btree on a
-- monotonic timestamp is the cheapest index maintenance there is.
--
-- goose runs this migration in a transaction, so the build takes a
-- SHARE lock and writers to farm.events wait for it. That is seconds on
-- a farm of this size and is the reason the index is created here
-- rather than left for someone to add CONCURRENTLY during an incident,
-- when the table is larger and the timeline is what they need.
CREATE INDEX IF NOT EXISTS events_at ON farm.events (at DESC);


-- ---------------------------------------------------------------------
-- The trigger function.
--
-- SECURITY DEFINER on purpose, and the reason is the API role rather
-- than the reaper: farm_reaper already carries GRANT INSERT ON
-- farm.events (00002_lease.sql), while a deployment may connect the API
-- as a role narrower than the schema owner. An INSERT privilege gap
-- must not be able to turn a lease release into an error, and must not
-- be able to buy silence in the ledger by dropping a grant.
-- search_path is emptied and every name is schema-qualified, so nothing
-- here can be resolved against a caller-controlled schema.
--
-- Say the uncomfortable half out loud. SECURITY DEFINER runs this body
-- as the schema owner, so the blindness that `SET role = farm_reaper`
-- buys farm.lease_reclaim — REVOKE ALL ON farm.device_runtime, the
-- STF #663 firewall — does NOT extend into this function. A future edit
-- here could read device health inside the reclaim transaction.
--
-- What keeps that from becoming a release is structural rather than
-- polite. The trigger is AFTER and returns NULL, so it cannot alter the
-- row; it reads only NEW, so it has no path to another lease; and the
-- single influence it retains over the outcome is to raise, which rolls
-- the ending back and leaves the device with its holder. Health can
-- therefore make an ending FAIL here, never happen. The opposite
-- arrangement — a trigger that could release — is the one the axiom
-- forbids, and no privilege granted to this function creates it.
--
-- `SET role = farm_reaper` would close even that, and is deliberately
-- not used: the owner must be a member of farm_reaper for it to work,
-- 00002 creates that role only IF NOT EXISTS, and on a database where a
-- DBA pre-created it the SET fails and EVERY lease ending in the farm
-- fails with it. That is a large blast radius bought for a hypothetical
-- edit, and this file prefers the smaller failure.
--
-- Everything written comes from NEW. No joins, no lookups, no reads of
-- farm.jobs or farm.devices: this fires on every ending on a busy farm,
-- and a ledger that costs a query per ending is a ledger someone
-- eventually turns off.
--
-- Note what is NOT recorded: device health. A ledger row that reported
-- the device's adb_state next to the ending would invite exactly the
-- causal reading — "released because it went offline" — that the schema
-- exists to make unrepresentable.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.trg_leases_ledger() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = ''
AS $fn$
DECLARE
  -- Every automatic path sets released_at = now() in the same statement,
  -- and the operator revoke does too. The fallback covers a future caller
  -- that closes a lease without stamping it: the ending instant is still a
  -- server clock, never a client's.
  v_ended timestamptz := COALESCE(NEW.released_at, pg_catalog.now());
  v_held  interval;
  v_by    text := farm.lease_ended_by(NEW.release_reason);
BEGIN
  v_held := v_ended - NEW.acquired_at;

  INSERT INTO farm.events (at, kind, device_id, slot_id, lease_id, job_id, actor, detail)
  VALUES (
    v_ended,
    'lease_ended',
    NEW.device_id,
    NEW.slot_id,
    NEW.id,
    NEW.job_id,
    -- actor names the CLASS of ender, which is all the database can
    -- honestly know. When a named human is involved, the same
    -- transaction's farm.audit_log row carries the name and the reason
    -- they typed.
    v_by,
    pg_catalog.jsonb_build_object(
      'release_reason',    NEW.release_reason,
      'ended_by',          v_by,
      'terminal_state',    NEW.state,
      'tenant_id',         NEW.tenant_id,
      'queue_id',          NEW.queue_id,
      'fence',             NEW.fence,
      'holder',            NEW.holder,
      'holder_epoch',      NEW.holder_epoch,
      'protected',         NEW.protected,
      -- How long the lease actually held the device, in both the form a
      -- query can aggregate and the form a human reads at 3am.
      'held_seconds',      pg_catalog.round(EXTRACT(epoch FROM v_held)::numeric, 3),
      'held',              v_held::text,
      'acquired_at',       NEW.acquired_at,
      'ended_at',          v_ended,
      -- The heartbeat and deadline trio is what separates "the holder
      -- was alive and said stop" from "the holder went silent and the
      -- reaper acted". Reading it later from farm.leases is not the
      -- same: renew moves these on every beat.
      'last_heartbeat_at', NEW.heartbeat_at,
      'heartbeat_seq',     NEW.heartbeat_seq,
      'heartbeat_age_s',   pg_catalog.round(EXTRACT(epoch FROM (v_ended - NEW.heartbeat_at))::numeric, 3),
      'expires_at',        NEW.expires_at,
      'reclaimable_at',    NEW.reclaimable_at,
      'witness_at',        NEW.witness_at,
      'witness_extensions', NEW.witness_extensions
    ))
  ON CONFLICT (lease_id) WHERE kind = 'lease_ended' DO NOTHING;

  RETURN NULL;
END $fn$;
-- +goose StatementEnd

-- The WHEN clause is the reason this is cheap. farm.lease_renew writes
-- state='held' on every heartbeat of every live lease — by far the
-- hottest UPDATE in the system — and each of those evaluates two
-- boolean comparisons against constants and never enters plpgsql. Only
-- a live -> terminal transition calls the function, and the guard
-- trigger in 00002_lease.sql makes that transition one-way, so a lease
-- can produce at most one ledger row even before the unique index has
-- an opinion.

-- +goose StatementBegin
CREATE TRIGGER leases_ledger
  AFTER UPDATE OF state ON farm.leases
  FOR EACH ROW
  WHEN (OLD.state IN ('held','suspect') AND NEW.state IN ('released','expired'))
  EXECUTE FUNCTION farm.trg_leases_ledger();
-- +goose StatementEnd


-- ---------------------------------------------------------------------
-- Backfill, bounded.
--
-- Shipping the ledger with a hole where last week's incidents were is
-- an odd kind of audit trail, so recent endings are reconstructed from
-- farm.leases. Two limits keep it honest:
--
--   * the window is 30 days, so this migration does not rewrite years
--     of history into one transaction on a farm that has been running;
--   * only leases that recorded released_at are included. A terminal
--     lease with no ending instant cannot be dated, and inventing one
--     from expires_at would put a fabricated timestamp in the one table
--     an incident review is supposed to trust.
--
-- Rows are dated with released_at, not now(): the ledger is a timeline,
-- and a reconstructed row that claims to have happened at migration
-- time would be worse than no row. 'backfilled' says which is which, so
-- nobody mistakes a reconstruction for something written at the time —
-- in particular heartbeat_age_s here is measured against the lease's
-- CURRENT heartbeat_at, which is the last one it ever recorded.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
INSERT INTO farm.events (at, kind, device_id, slot_id, lease_id, job_id, actor, detail)
SELECT l.released_at,
       'lease_ended',
       l.device_id,
       l.slot_id,
       l.id,
       l.job_id,
       farm.lease_ended_by(l.release_reason),
       jsonb_build_object(
         'release_reason',    l.release_reason,
         'ended_by',          farm.lease_ended_by(l.release_reason),
         'terminal_state',    l.state,
         'tenant_id',         l.tenant_id,
         'queue_id',          l.queue_id,
         'fence',             l.fence,
         'holder',            l.holder,
         'holder_epoch',      l.holder_epoch,
         'protected',         l.protected,
         'held_seconds',      round(extract(epoch FROM (l.released_at - l.acquired_at))::numeric, 3),
         'held',              (l.released_at - l.acquired_at)::text,
         'acquired_at',       l.acquired_at,
         'ended_at',          l.released_at,
         'last_heartbeat_at', l.heartbeat_at,
         'heartbeat_seq',     l.heartbeat_seq,
         'heartbeat_age_s',   round(extract(epoch FROM (l.released_at - l.heartbeat_at))::numeric, 3),
         'expires_at',        l.expires_at,
         'reclaimable_at',    l.reclaimable_at,
         'witness_at',        l.witness_at,
         'witness_extensions', l.witness_extensions,
         'backfilled',        true)
  FROM farm.leases l
 WHERE l.state IN ('released','expired')
   AND l.released_at IS NOT NULL
   AND l.released_at > now() - interval '30 days'
ON CONFLICT (lease_id) WHERE kind = 'lease_ended' DO NOTHING;
-- +goose StatementEnd


-- ---------------------------------------------------------------------
-- The one question, as a view.
--
-- "Why did this lease end and who ended it" should not require anybody
-- to remember the shape of a jsonb detail at 3am.
-- ---------------------------------------------------------------------

CREATE OR REPLACE VIEW farm.v_lease_endings AS
SELECT e.at                                            AS ended_at,
       e.lease_id,
       e.device_id,
       e.slot_id,
       e.job_id,
       e.detail ->> 'tenant_id'                        AS tenant_id,
       (e.detail ->> 'fence')::bigint                  AS fence,
       e.detail ->> 'release_reason'                   AS release_reason,
       e.actor                                         AS ended_by,
       (e.detail ->> 'held_seconds')::numeric          AS held_seconds,
       (e.detail ->> 'heartbeat_age_s')::numeric       AS heartbeat_age_s,
       e.detail ->> 'holder'                           AS holder,
       (e.detail ->> 'protected')::boolean             AS protected,
       COALESCE((e.detail ->> 'backfilled')::boolean, false) AS backfilled
  FROM farm.events e
 WHERE e.kind = 'lease_ended';

COMMENT ON VIEW farm.v_lease_endings IS
  'Every lease ending, written by the trigger on farm.leases: reason, class of ender, and how long the device was held.';

-- +goose Down
-- +goose StatementBegin
DROP VIEW IF EXISTS farm.v_lease_endings;
DROP TRIGGER IF EXISTS leases_ledger ON farm.leases;
DROP FUNCTION IF EXISTS farm.trg_leases_ledger();
DROP INDEX IF EXISTS farm.events_at;
DROP INDEX IF EXISTS farm.events_lease_ending;
DROP FUNCTION IF EXISTS farm.lease_ended_by(text);
-- The 'lease_ended' rows themselves are deliberately left where they
-- are. Rolling back the mechanism that writes an audit trail is a
-- change of plan; deleting the trail it already wrote is an incident.
-- +goose StatementEnd
