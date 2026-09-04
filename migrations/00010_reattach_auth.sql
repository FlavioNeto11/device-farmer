-- +goose Up

-- =====================================================================
-- AUTHORISATION ON LEASE RE-ATTACH
--
-- farm.lease_acquire PHASE 1 re-attaches on job_id ALONE. It sets
-- holder, holder_instance and holder_epoch, moves the lease back to
-- 'held', and hands back the SAME fence — the value that authorises
-- writes to the handset. So any caller presenting a job id it happens
-- to know becomes that lease's holder, and the previous holder's next
-- farm.lease_renew matches nothing (renew keys on holder_instance),
-- returns zero rows, and is reported as ErrFenced — terminal and
-- unrecoverable. The displaced holder aborts a six-hour run believing
-- it was legitimately fenced. It was robbed.
--
-- A job id is not a secret. GET /api/v1/leases, GET /api/v1/fleet and
-- the event stream all report job_id and fence for live leases, and
-- farm.events carries job_id on every row.
--
--
-- WHY PHASE 1 EXISTS AND MUST KEEP WORKING
--
-- A Kubernetes pod eviction — node drain, preemption, spot reclaim,
-- cluster upgrade, OOM kill — is the most ordinary event in a control
-- plane and must never cost a device. The replacement jobrunner calls
-- lease_acquire with the same job id and resumes AT THE SAME FENCE,
-- because the job's own work may still be running detached on the
-- phone and bumping the fence would fence the job out of its own
-- process. Removing PHASE 1 recreates DeviceFarmer/STF issue #663 by a
-- different route: a control-plane event destroying a multi-hour run.
-- cmd/farmd/main.go and internal/lease/store.go both say this at
-- length, and test/assertions.sql P3 asserts it.
--
--
-- WHAT DOES NOT DISTINGUISH THE TWO CASES
--
-- The replacement pod and the thief present the same evidence, so
-- every value already on the row was considered and discarded:
--
--   holder            A pod name, documented AUDIT ONLY. It CHANGES on
--                     eviction ('runner-pod-a' -> 'runner-pod-b'), so
--                     requiring a match would refuse the eviction case.
--
--   holder_instance   Freshly minted per process incarnation. The
--                     replacement mints a new one precisely because it
--                     is a new process. Also changes on eviction.
--
--   holder_epoch      Written by the server, never by the caller, and
--                     readable from GET /api/v1/leases like the fence.
--                     A caller echoing it back proves only that it can
--                     read a list endpoint.
--
--   fence             Same: public on the fleet grid and the stream.
--
--   heartbeat_at      "Refuse while the current holder is still
--                     renewing" is tempting and wrong twice over. A
--                     replacement pod is scheduled within seconds, so
--                     the freshness window that admits it also admits
--                     the thief; and waiting out the TTL would make
--                     patience an authorisation mechanism.
--
-- There is no rule over the existing columns that admits one and
-- rejects the other, because the two callers are indistinguishable
-- from what they present. SAY THE UNCOMFORTABLE PART: this cannot be
-- closed in SQL alone. It needs a credential the caller holds, which
-- means a caller-supplied value, which means SQL can enforce a match
-- but cannot verify who the caller is.
--
--
-- THE CREDENTIAL THAT SURVIVES AN EVICTION
--
-- The distinguishing fact is not about the PROCESS, which dies, but
-- about the WORKLOAD, which does not. A replacement pod is created by
-- the same controller, mounts the same service-account token, and
-- authenticates to the API as the same principal. A thief holds a
-- different credential — or none.
--
-- So the lease binds to the AUTHENTICATED PRINCIPAL rather than to the
-- process, and two new arguments carry it:
--
--   p_caller_tenant     the tenant the caller is confined to, or NULL
--                       when the caller is unconfined.
--   p_caller_principal  the authenticated subject, or NULL when the
--                       call did not come from an authenticated caller.
--
-- Both MUST be derived by the server from a verified credential —
-- api.Identity, which comes from the bearer token — and never from a
-- request body. farm.jobs.created_by looks like an anchor and is not
-- one: internal/api/jobs.go lets the submitter set it, so it is a
-- label the caller chose, not an identity anyone verified.
--
-- The principal must also be STABLE, because it is the thing a
-- replacement pod has to reproduce. A deployment using StaticBearer
-- should name a subject in every token spec: given
-- "<token>:<role>" with no subject, internal/api/auth.go derives one
-- from the token digest, and rotating that token mid-run leaves the
-- replacement unable to re-attach. Name the subject and rotation is
-- invisible here.
--
--
-- THE RULE
--
--   1. TENANT GATE, both phases. A caller confined to a tenant may
--      only acquire against that tenant's job. This is tenantScope()
--      from internal/api/leases.go moved inside the transaction, where
--      it also covers callers that reach the function without going
--      through the HTTP handler.
--
--   2. EVERY LEASE IS BOUND AT ALLOCATION. An identified caller binds
--      its own principal; a control-plane loop, which presents none,
--      binds the reserved name 'system:control-plane'. There is no
--      such thing as an unowned lease, which is the point: an
--      identified caller can no longer re-attach a lease the
--      scheduler placed, because that lease has an owner and it is
--      not them. An acquire that presents the reserved name is
--      refused outright, so a token cannot claim to be the system.
--
--   3. PRINCIPAL GATE, re-attach only. An identified caller may
--      re-attach only a lease bound to its own principal. A caller
--      presenting NO principal is admitted — see the limit below —
--      and never rebinds.
--
-- Why this admits the eviction case: the replacement pod is the same
-- tenant and the same principal. It differs from its predecessor in
-- exactly the two values the rule does not consult — the pod name and
-- the instance uuid — and it re-attaches at the same fence, unchanged.
--
-- Why it rejects the takeover: a thief from another tenant fails the
-- tenant gate; a thief inside the same tenant carrying a different
-- token fails the principal gate; and a thief aiming at a lease the
-- control plane placed fails it too, because 'system:control-plane'
-- is a name no token may present. None of them can supply the
-- victim's credential, because it is a secret held by the workload
-- rather than a value published on a list endpoint.
--
--
-- WHAT THIS STILL DOES NOT CLOSE, STATED PLAINLY
--
-- A caller that presents NO principal is admitted against any lease.
-- SQL cannot tell an in-process control-plane loop from a caller that
-- simply omitted the argument, and the only thing separating them is
-- the database role boundary: reaching farm.lease_acquire at all
-- requires a connection as a control-plane role, which no tenant has.
-- The scheduler re-attaches a queued job's existing lease and the
-- jobrunner re-attaches after its own eviction; neither holds an
-- end-user credential, so NULL cannot be made to mean "refuse"
-- without breaking both.
--
-- The consequence: this fix is only as strong as the authentication
-- in force at the component that fronts untrusted callers. Any such
-- component MUST pass both arguments — internal/api/leases.go now
-- does, on every acquire, from the authenticated identity rather than
-- from the body, and refuses an identity carrying no subject rather
-- than falling through to the unattributed path. Under
-- FARM_API_AUTH=allow-all every caller is one principal and nothing
-- is separated, which is the correct outcome for a deployment that
-- turned authentication off.
--
-- An unattributed re-attach is therefore possible, and is recorded as
-- such: every re-attach writes a 'lease_reattached' row to
-- farm.events classified principal_match, adopted, or unattributed.
-- The gap is admitted, but it is not silent, and "who took my lease"
-- becomes a query instead of an inference.
--
-- SECOND GAP, WHICH DRAINS TO NOTHING: leases that were already live
-- when this migration ran carry no principal, because no source
-- exists to reconstruct one — nothing recorded the acquiring identity
-- before this column. Those leases are ADOPTED by the first
-- identified caller that re-attaches them, which keeps an upgrade
-- from costing a running job its device, and means a thief who moves
-- during the upgrade window can adopt one. Every lease created after
-- this migration is bound at allocation, so the population of
-- adoptable leases only shrinks, and reaches zero within one lease
-- lifetime of the upgrade.
--
--
-- THE REFUSAL MUST NOT END THE LEASE
--
-- A lease ends when the job says so, when a user-written deadline
-- elapses, or when a human takes it back. NOTHING ELSE — and a failed
-- authorisation check is emphatically nothing else.
--
-- The refusal is a RAISE, placed BEFORE the UPDATE, so nothing is
-- written: state, fence, holder_instance, expires_at and
-- devices.current_lease_id are all exactly as they were, and the
-- rightful holder's next renew succeeds. Returning zero rows instead
-- would have been worse than useless — Store.Acquire reads zero rows
-- as ErrNoCapacity, so a refusal would have been reported as a busy
-- farm and retried forever.
--
-- The SQLSTATEs are private ones (class DF) rather than 42501.
-- 42501 is Postgres' own insufficient_privilege, raised when a ROLE
-- lacks a grant, and a caller cannot tell the two apart from the code
-- alone. That matters here: a privilege gap is a deployment
-- misconfiguration a replacement pod should retry through, while this
-- refusal is terminal and must not be. Sharing a code would let a
-- missing GRANT abort a job.
--
--   DF001  the caller's tenant does not own the job.
--   DF002  the caller is not the principal this lease is bound to.
--   DF003  the caller presented the reserved control-plane name.
--
-- The cost of raising: the refusal cannot write its own ledger row,
-- because that row would roll back with the exception. Postgres has no
-- autonomous transaction. A refused re-attach is visible to the caller
-- (SQLSTATE DF00x), in the API log, and in the 403 the handler
-- returns — not in farm.events. Recording it there would mean either
-- committing the takeover in order to log it, or a second connection
-- opened from inside the lease path, and neither is worth it.
--
-- The operator revoke is untouched. POST /api/v1/leases/{id}/revoke
-- does not go through this function at all: it is operator-only by
-- middleware, needs no fence, writes farm.audit_log with the human's
-- name and reason, raises devices.fence_floor and quarantines the
-- slot. Taking a device back from a live job stays exactly one path,
-- and it is the audited human one.
-- =====================================================================


-- ---------------------------------------------------------------------
-- The binding.
--
-- A separate column from holder, deliberately. holder is the pod name
-- and changes on every eviction; this is the identity that does not.
--
-- Nullable, and NULL now means exactly one thing: this lease was
-- already live when the column was added. Every lease allocated after
-- this migration carries a principal — an end user's, or the reserved
-- 'system:control-plane' — so the null population only shrinks.
-- ---------------------------------------------------------------------
ALTER TABLE farm.leases ADD COLUMN IF NOT EXISTS holder_principal text;

COMMENT ON COLUMN farm.leases.holder_principal IS
  'The principal this lease is bound to: api.Identity.Subject for a lease acquired by an '
  'authenticated caller, or the reserved name system:control-plane for one placed by the '
  'scheduler or the jobrunner. Unlike holder (a pod name, audit only) this survives a pod '
  'eviction, so a re-attach by an identified caller must present it. Write-once: NULL may '
  'become a value, a value may never change. NULL means the lease predates this column and '
  'may be adopted once, which is the upgrade path and nothing more.';


-- ---------------------------------------------------------------------
-- Write-once, enforced by the guard trigger rather than by this
-- function.
--
-- farm.lease_acquire is not the only thing that can UPDATE farm.leases
-- — release, reclaim, max_runtime expiry, the operator revoke and any
-- future caller all can — and a binding that only one function
-- respects is a binding that the next writer silently loses. Putting
-- it in the trigger that already makes device_id, fence, job_id and
-- acquired_at immutable puts it where it cannot be forgotten.
--
-- The rest of this body is 00002_lease.sql's, unchanged. It is
-- repeated in full because CREATE OR REPLACE FUNCTION replaces a
-- whole body; there is no way to append one clause.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.trg_leases_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.device_id <> OLD.device_id
     OR NEW.fence   <> OLD.fence
     OR NEW.job_id  <> OLD.job_id
     OR NEW.acquired_at <> OLD.acquired_at THEN
    RAISE EXCEPTION 'lease % identity is immutable', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;

  -- The holder principal is part of that identity once it exists. A
  -- lease that predates the column may be adopted by an identified
  -- holder; it may never be handed from one principal to another, and
  -- it may never be blanked back to NULL — blanking would re-open the
  -- lease to adoption and would look, in the row, exactly like a lease
  -- that had never been bound.
  IF OLD.holder_principal IS NOT NULL
     AND NEW.holder_principal IS DISTINCT FROM OLD.holder_principal THEN
    RAISE EXCEPTION 'lease % is bound to a holder principal and may not be reassigned', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;

  -- Deadlines never move backwards. Without this, a plain now()+ttl in
  -- renew silently destroys a control-plane-gap refund.
  IF NEW.expires_at < OLD.expires_at THEN
    RAISE EXCEPTION 'lease % expires_at may not move backwards (% -> %)',
      OLD.id, OLD.expires_at, NEW.expires_at USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.reclaimable_at < OLD.reclaimable_at THEN
    RAISE EXCEPTION 'lease % reclaimable_at may not move backwards', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;

  -- A terminal lease is terminal.
  IF OLD.state IN ('released','expired') AND NEW.state IN ('held','suspect') THEN
    RAISE EXCEPTION 'lease % is terminal and cannot be revived', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;

  RETURN NEW;
END $fn$;
-- +goose StatementEnd


-- ---------------------------------------------------------------------
-- The handover ledger needs a writer's grant.
--
-- The re-attach row below is written by farm.lease_acquire, which is
-- SECURITY INVOKER, so it runs with the privileges of whichever role
-- the allocation path connects as. 00002_lease.sql grants INSERT ON
-- farm.events to farm_reaper and NOT to farm_scheduler, so a
-- role-separated deployment would have found this out as a permission
-- denied on the pod-eviction path — the one path that must never fail.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'farm_scheduler') THEN
    EXECUTE 'GRANT INSERT ON farm.events TO farm_scheduler';
  END IF;
END $$;
-- +goose StatementEnd


-- ---------------------------------------------------------------------
-- farm.lease_acquire, with the two authorisation arguments.
--
-- DROPPED rather than replaced. CREATE OR REPLACE matches on the
-- ARGUMENT LIST, so adding parameters creates an OVERLOAD: both the
-- three-argument and the five-argument function would exist, and every
-- existing three-argument call site — internal/lease/store.go,
-- test/assertions.sql, test/assertions_v5.sql — would fail with
--
--     ERROR: function farm.lease_acquire(uuid, text, uuid) is not unique
--
-- which is allocation dying in production on the one path no assertion
-- would have exercised. 00005_correctness.sql was bitten by exactly
-- this on farm.lease_expire_max_runtime. Dropping first leaves one
-- function answering the name; the DEFAULTs keep every three-argument
-- caller compiling and running unchanged.
-- ---------------------------------------------------------------------
DROP FUNCTION IF EXISTS farm.lease_acquire(uuid, text, uuid);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.lease_acquire(
  p_job_id           uuid,
  p_holder           text,
  p_holder_instance  uuid,
  p_caller_tenant    text DEFAULT NULL,
  p_caller_principal text DEFAULT NULL
) RETURNS TABLE (
  lease_id uuid, device_id uuid, slot_id bigint, fence bigint,
  expires_at timestamptz, reclaimable_at timestamptz, reattached boolean
)
LANGUAGE plpgsql AS $fn$
#variable_conflict use_column
DECLARE
  -- The owner of a lease no end user acquired. A reserved name rather
  -- than NULL so that every lease has an owner on the row, which is
  -- what stops an identified caller re-attaching the scheduler's work.
  c_system   constant text := 'system:control-plane';

  j          farm.jobs%ROWTYPE;
  existing   farm.leases%ROWTYPE;
  v_dev      uuid;
  v_slot     bigint;
  v_rejected uuid[] := '{}';
  v_protected boolean;
  v_unknown  text[];
  v_class    text;
BEGIN
  -- A token whose subject is the reserved name would BE the control
  -- plane as far as every check below is concerned. Refuse it at the
  -- door rather than letting it match.
  IF p_caller_principal = c_system THEN
    RAISE EXCEPTION '% is reserved for the control plane and may not be presented by a caller',
      c_system USING ERRCODE = 'DF003';
  END IF;

  SELECT * INTO j FROM farm.jobs WHERE id = p_job_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'no such job %', p_job_id USING ERRCODE = 'no_data_found';
  END IF;

  -- ---- TENANT GATE ---------------------------------------------------
  -- Ahead of both phases, because a caller confined to one tenant has no
  -- business allocating for another tenant's job either. NULL means
  -- unconfined: an operator, or a control-plane loop with no untrusted
  -- caller in front of it.
  --
  -- The message does not name the owning tenant. A caller that may not
  -- touch this job may not learn who may.
  IF p_caller_tenant IS NOT NULL AND p_caller_tenant <> j.tenant_id THEN
    RAISE EXCEPTION 'caller is scoped to tenant %, which does not own job %',
      p_caller_tenant, p_job_id USING ERRCODE = 'DF001';
  END IF;

  -- A selector nobody can satisfy is a spec error, and the caller must
  -- hear it once at submission rather than as an eternity of "no
  -- capacity" that reads identically to a busy farm.
  v_unknown := farm.selector_unknown_keys(j.selector);
  IF array_length(v_unknown, 1) > 0 THEN
    RAISE EXCEPTION 'job % has unknown selector keys: %; supported keys are '
      'model, model_in, manufacturer, sdk_min, sdk_max, android_release, abi, '
      'labels, host_in, not_host_in',
      p_job_id, array_to_string(v_unknown, ', ') USING ERRCODE = 'check_violation';
  END IF;

  -- ---- PHASE 1: re-attach ------------------------------------------
  SELECT * INTO existing FROM farm.leases l
   WHERE l.job_id = p_job_id AND l.state IN ('held', 'suspect')
   FOR NO KEY UPDATE;

  IF FOUND THEN
    -- ---- PRINCIPAL GATE --------------------------------------------
    -- BEFORE the UPDATE, and by RAISE rather than by returning nothing.
    -- Nothing below has run, so a refusal leaves the lease exactly as it
    -- was: same state, same fence, same holder_instance, same deadlines,
    -- and the rightful holder's next renew still matches.
    --
    -- NULL on the row is the pre-migration case and is adoptable; NULL
    -- from the caller is the control plane and matches anything.
    IF p_caller_principal IS NOT NULL
       AND existing.holder_principal IS NOT NULL
       AND p_caller_principal <> existing.holder_principal THEN
      RAISE EXCEPTION 'lease % is held by a different principal', existing.id
        USING ERRCODE = 'DF002',
              HINT = 'a re-attach must present the principal that acquired the lease; '
                     'to take this device from its holder use the audited operator revoke';
    END IF;

    -- How this re-attach was authorised, for the ledger row below.
    -- 'unattributed' is the honest label for the case SQL cannot close:
    -- a caller that presented no principal at all.
    v_class := CASE
      WHEN p_caller_principal IS NULL              THEN 'unattributed'
      WHEN existing.holder_principal IS NOT NULL   THEN 'principal_match'
      ELSE 'adopted'
    END;

    UPDATE farm.leases l
       SET holder = p_holder,
           holder_instance = p_holder_instance,
           holder_epoch = l.holder_epoch + 1,
           -- Adopts a lease that predates the column; never rewrites a
           -- binding that exists. The guard trigger enforces the same
           -- thing for every other writer.
           holder_principal = COALESCE(l.holder_principal, p_caller_principal),
           state = 'held',
           heartbeat_at = now(),
           expires_at = GREATEST(l.expires_at, now() + l.ttl),
           reclaimable_at = GREATEST(l.reclaimable_at, now() + l.ttl + l.grace),
           witness_extensions = 0
     WHERE l.id = existing.id
    RETURNING l.id, l.device_id, l.slot_id, l.fence, l.expires_at, l.reclaimable_at, true
      INTO lease_id, device_id, slot_id, fence, expires_at, reclaimable_at, reattached;

    -- A re-attach displaces a holder, and until now the timeline said
    -- nothing about it: holder_epoch counted them and nothing read the
    -- count. This is the row that answers "who took my lease", written
    -- in the SAME transaction as the handover so it cannot be lost while
    -- the handover survives.
    --
    -- Every value comes from `existing` rather than from the OUT
    -- parameters. device_id, slot_id and lease_id are also column names
    -- on farm.events, and a VALUES list is exactly the place where
    -- reasoning about which one #variable_conflict picks is a waste of a
    -- reader's afternoon. The pre-image carries the same values: the
    -- guard trigger makes device_id and fence immutable, and slot_id is
    -- not touched here.
    --
    -- AND THE DIRECTION OF FAILURE IS INVERTED FROM 00007's. That
    -- trigger lets a failed ledger write fail the ENDING, because a
    -- release that cannot be recorded is better retried. Here the
    -- operation is a pod eviction's re-attach, and failing it costs the
    -- job its device — the exact loss this file exists to prevent. So a
    -- privilege gap degrades to a WARNING and the handover proceeds.
    -- Only a missing grant is caught: anything else is a real fault and
    -- still aborts.
    BEGIN
      INSERT INTO farm.events (kind, device_id, slot_id, lease_id, job_id, actor, detail)
      VALUES ('lease_reattached', existing.device_id, existing.slot_id, existing.id, p_job_id,
              COALESCE(p_caller_principal, c_system),
              jsonb_build_object(
                'authorised',       v_class,
                'fence',            existing.fence,
                'holder_epoch',     existing.holder_epoch + 1,
                'prior_holder',     existing.holder,
                'prior_principal',  existing.holder_principal,
                'prior_instance',   existing.holder_instance,
                'new_holder',       p_holder,
                'new_instance',     p_holder_instance,
                'prior_state',      existing.state));
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE WARNING 'lease % re-attached but the handover could not be recorded: %',
        existing.id, SQLERRM;
    END;

    RETURN NEXT;
    RETURN;
  END IF;

  -- Long jobs are never auto-reclaimed: hold and page instead.
  --
  -- Both halves matter and this line has been broken once already by a
  -- careless rewrite. expected_duration is nullable, so the comparison
  -- must be COALESCEd or the whole expression is NULL and the INSERT
  -- fails against a NOT NULL column — every job without a stated
  -- duration would be unallocatable. The threshold is 30 minutes, not an
  -- hour; changing it silently changes which leases the reaper may take.
  v_protected := j.protected
    OR COALESCE(j.expected_duration > interval '30 minutes', false);

  -- ---- PHASE 2: allocate --------------------------------------------
  LOOP
    SELECT d.id, d.current_slot_id INTO v_dev, v_slot
      FROM farm.devices d
      JOIN farm.device_runtime r ON r.device_id = d.id
      JOIN farm.slots s          ON s.id = d.current_slot_id
      JOIN farm.hosts h          ON h.id = d.host_id
     WHERE d.pool_id = j.pool_id
       AND d.admin_state = 'enabled'
       AND h.admin_state = 'enabled'    -- a draining host takes no new work
       AND d.current_lease_id IS NULL
       AND r.adb_state = 'device'
       AND r.health    = 'healthy'
       AND s.state     = 'active'
       AND s.rearm_at <= now()
       AND (j.pin_device IS NULL OR d.id = j.pin_device)
       AND NOT (d.id = ANY (v_rejected))
       AND farm.device_matches(d.id, j.selector)
     ORDER BY d.failure_score ASC, d.last_released_at NULLS FIRST, d.id
     LIMIT 1
     FOR NO KEY UPDATE OF d SKIP LOCKED;

    IF NOT FOUND THEN
      RETURN;   -- no capacity; caller re-queues
    END IF;

    PERFORM 1
       FROM farm.devices d
       JOIN farm.device_runtime r ON r.device_id = d.id
       JOIN farm.slots s          ON s.id = d.current_slot_id
       JOIN farm.hosts h          ON h.id = d.host_id
      WHERE d.id = v_dev
        AND d.admin_state = 'enabled'
        AND h.admin_state = 'enabled'
        AND d.current_lease_id IS NULL
        AND r.adb_state = 'device'
        AND r.health    = 'healthy'
        AND s.state     = 'active'
        AND s.rearm_at <= now();

    EXIT WHEN FOUND;
    v_rejected := v_rejected || v_dev;
  END LOOP;

  INSERT INTO farm.leases (
    device_id, slot_id, job_id, tenant_id, queue_id, holder, holder_instance,
    holder_principal,
    protected, disruption_policy, ttl, grace, expires_at, reclaimable_at)
  VALUES (
    v_dev, v_slot, p_job_id, j.tenant_id, j.queue_id, p_holder, p_holder_instance,
    -- The binding, written once and never NULL. A lease the scheduler
    -- placed is owned by the system, which is what makes an identified
    -- caller's re-attach of it a refusal rather than an adoption.
    COALESCE(p_caller_principal, c_system),
    v_protected, j.disruption_policy, j.ttl, j.grace,
    now() + j.ttl, now() + j.ttl + j.grace)
  ON CONFLICT (device_id) WHERE state IN ('held','suspect') DO NOTHING
  RETURNING id, farm.leases.device_id, farm.leases.slot_id, farm.leases.fence,
            farm.leases.expires_at, farm.leases.reclaimable_at, false
    INTO lease_id, device_id, slot_id, fence, expires_at, reclaimable_at, reattached;

  IF lease_id IS NULL THEN
    RETURN;   -- lost the race; caller re-queues
  END IF;
  RETURN NEXT;
END $fn$;
-- +goose StatementEnd

COMMENT ON FUNCTION farm.lease_acquire(uuid, text, uuid, text, text) IS
  'Allocates a device for a job, or re-attaches to the one it already holds at the SAME '
  'fence. p_caller_tenant and p_caller_principal are the AUTHENTICATED caller and must be '
  'derived from a verified credential, never from a request body: they gate the re-attach '
  'so a known job id is no longer enough to take a live lease. Refuses with SQLSTATE DF001 '
  '(wrong tenant), DF002 (wrong principal) or DF003 (reserved principal presented), none of '
  'which write anything, so the lease is left exactly as it was.';


-- ---------------------------------------------------------------------
-- The handover log.
--
-- One row per re-attach, so an incident review can ask who took a lease
-- and when instead of inferring it from holder_epoch, which counted the
-- handovers and named none of them.
-- ---------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS events_lease_reattach
  ON farm.events (lease_id, at DESC) WHERE kind = 'lease_reattached';


-- +goose Down
-- +goose StatementBegin
-- Down is deliberately narrow, and the escape hatch it opens is the
-- guard clause rather than the function.
--
-- The five-argument function is LEFT IN PLACE. Its two new arguments
-- have defaults, so every three-argument caller keeps working, and
-- re-creating the three-argument body here would mean pasting
-- 00005_correctness.sql's definition into a second file where the two
-- copies could drift — which is how a rollback quietly restores a
-- different function from the one it claims to restore.
--
-- farm.leases.holder_principal is left in place too. Dropping it would
-- discard the record of which principal each live lease belongs to, and
-- a rollback that loses ownership evidence for running jobs is worse
-- than the thing being rolled back.
--
-- What an operator actually needs during an incident — "the gate is
-- refusing a runner that should be allowed" — is served by lifting the
-- write-once clause below, after which
--
--     UPDATE farm.leases SET holder_principal = NULL WHERE id = ...;
--
-- returns that one lease to the adoptable state and the next re-attach
-- succeeds. That is per-lease, reversible, and leaves every other lease
-- protected, which a wholesale rollback of the function would not.
DROP INDEX IF EXISTS farm.events_lease_reattach;

CREATE OR REPLACE FUNCTION farm.trg_leases_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.device_id <> OLD.device_id
     OR NEW.fence   <> OLD.fence
     OR NEW.job_id  <> OLD.job_id
     OR NEW.acquired_at <> OLD.acquired_at THEN
    RAISE EXCEPTION 'lease % identity is immutable', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;

  IF NEW.expires_at < OLD.expires_at THEN
    RAISE EXCEPTION 'lease % expires_at may not move backwards (% -> %)',
      OLD.id, OLD.expires_at, NEW.expires_at USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.reclaimable_at < OLD.reclaimable_at THEN
    RAISE EXCEPTION 'lease % reclaimable_at may not move backwards', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;

  IF OLD.state IN ('released','expired') AND NEW.state IN ('held','suspect') THEN
    RAISE EXCEPTION 'lease % is terminal and cannot be revived', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;

  RETURN NEW;
END $fn$;
-- +goose StatementEnd
