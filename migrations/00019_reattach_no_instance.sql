-- ---------------------------------------------------------------------
-- 00019_reattach_no_instance.sql
--
-- THE HANDOVER LOG STOPS MINTING A COPY OF A LIVE CREDENTIAL.  (SEC-05)
--
-- farm.lease_renew matches on the triple (id, fence, holder_instance).
-- lease_id and fence are published — /api/v1/fleet and the event stream
-- carry both for every live lease — so holder_instance is the whole
-- secret, and internal/api/leases.go withholds it from every listing on
-- exactly that ground.
--
-- 00009 then wrote it into the timeline. Its lease_reattached row
-- carries lease_id in its own column, 'fence' in its detail, and BOTH
-- instance uuids beside them:
--
--     'prior_instance',   existing.holder_instance,
--     'new_instance',     p_holder_instance,
--
-- One record, all three members of the triple, in a jsonb blob that
-- GET /api/v1/events returned verbatim to any token that could read the
-- row — every tenant's, for the unscoped operator role that has no use
-- for the value at all, and its own tenant's for a tenant token, which
-- is the two-CI-shards case the requirement was written about.
--
-- 'new_instance' is the sharp end: it is the value that will answer
-- every renewal for the rest of that lease's life. 'prior_instance' is
-- spent by the time the row exists, since the UPDATE above it has
-- already replaced the lease's holder_instance — but a reader cannot
-- tell a spent one from the current one without knowing which
-- re-attach was the last, so both go.
--
-- WHAT THIS FILE DOES AND WHAT IT DELIBERATELY DOES NOT DO
--
-- It stops the emission. It does NOT rewrite the rows already written,
-- and the read side does not depend on it: redactedDetail in
-- internal/api/events_scope.go subtracts both keys from every detail
-- document the timeline projects, which is what closes the hole for the
-- history a running farm already has. This file is the other half — a
-- ledger that never holds the secret needs no deny-list, and a
-- deny-list only ever covers the keys somebody remembered to name.
--
-- The rows are left alone on purpose. Both values are already in
-- farm.leases: 'new_instance' IS that lease's holder_instance until the
-- next re-attach, and 'prior_instance' is a value that column used to
-- hold. So anyone who can read farm.events can read the live one from
-- an indexed column two tables away, and scrubbing the ledger would
-- edit the forensic record — the thing an incident review quotes —
-- while removing nothing from the reader it was meant to remove it
-- from. The exposure that mattered was the API projection, because that
-- is the one surface where farm.leases.holder_instance was never
-- reachable and farm.events.detail was.
--
-- WHAT REPLACES THEM
--
-- 'instance_changed', a boolean. The diagnostic question the ledger is
-- asked is "who took my lease, and was this a real handover or my own
-- pod re-attaching" — answered by prior_holder, new_holder,
-- prior_principal, authorised, holder_epoch and, for the last part, by
-- whether the instance changed at all. None of those is a credential.
--
-- WHY THE WHOLE FUNCTION IS HERE AGAIN
--
-- CREATE OR REPLACE FUNCTION replaces a body whole; PL/pgSQL has no way
-- to patch two lines of one. The body below is 00009's, copied
-- verbatim, with only the jsonb_build_object changed — the same way
-- 00009 carried 00005's and 00005 carried 00002's. As always, the
-- newest file is the definition.
--
-- THE GAP AT 00018 IS DELIBERATE. It is reserved for a change landing
-- on a parallel branch, which claimed the number first; this file took
-- the next one rather than racing it. migrations/embed.go orders by the
-- parsed version and rejects only DUPLICATE numbers, so a gap costs
-- nothing, and a 00018 that arrives later is not silently skipped: goose
-- is run without WithAllowMissing (cmd/farmd/migrate.go), and on a
-- database that has already recorded 19 it refuses with
--
--     migrate up: error: found 1 missing migrations before current
--     version 19: version 18: ...
--
-- which is a loud failure at deploy time rather than a schema that
-- quietly lacks a migration. If that branch never lands, 18 stays
-- unused; nothing here or in goose needs it to exist.
--
-- The argument list is IDENTICAL to 00009's, so this REPLACES rather
-- than overloads and needs no DROP first. That is the trap 00009
-- documents at length: a CREATE OR REPLACE with a different arg list
-- creates a second function, and every existing call site then dies
-- with "function farm.lease_acquire(...) is not unique". Nothing here
-- may change the signature, the parameter names or the RETURNS TABLE.
-- ---------------------------------------------------------------------

-- +goose Up
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
    --
    -- AND IT NAMES NO INSTANCE UUID. 00019 removed 'prior_instance' and
    -- 'new_instance' from the object below: holder_instance is the only
    -- private member of the triple farm.lease_renew matches on, this row
    -- already carries the other two, and a ledger read by everyone is
    -- the last place a credential should be duplicated into. What the
    -- reader actually needs from those two keys is whether the instance
    -- changed, so that is what is written.
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
                'new_holder',       p_holder,
                'instance_changed', existing.holder_instance IS DISTINCT FROM p_holder_instance,
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

-- +goose StatementBegin
COMMENT ON FUNCTION farm.lease_acquire(uuid, text, uuid, text, text) IS
  'Allocates a device for a job, or re-attaches to the one it already holds at the SAME '
  'fence. p_caller_tenant and p_caller_principal are the AUTHENTICATED caller and must be '
  'derived from a verified credential, never from a request body: they gate the re-attach '
  'so a known job id is no longer enough to take a live lease. Refuses with SQLSTATE DF001 '
  '(wrong tenant), DF002 (wrong principal) or DF003 (reserved principal presented), none of '
  'which write anything, so the lease is left exactly as it was. The lease_reattached ledger '
  'row it writes names no holder_instance: that value is the private member of the triple '
  'farm.lease_renew matches on, and the row already carries the other two.';
-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin
-- Down is a no-op, and that is the whole decision rather than an
-- omission.
--
-- Rolling this migration back means restoring a function that writes a
-- live credential into a table every operator and — through
-- GET /api/v1/events — every tenant can read. Nothing reads the two
-- keys it would restore: the dashboard renders detail as text, no query
-- in this repository selects them, and the diagnostic they carried is
-- served by 'instance_changed' and by holder_epoch. So the rollback has
-- no beneficiary and one victim, which is not a trade worth offering
-- through a command an operator runs at 3am.
--
-- The shape follows 00009's Down, which left the function it replaced
-- in place for the same reason: a rollback that quietly reinstates the
-- defect the migration was written for is worse than no rollback. The
-- previous body is in 00009_reattach_auth.sql for anyone who genuinely
-- needs it, and re-applying it is one deliberate CREATE OR REPLACE.
SELECT 1;
-- +goose StatementEnd
