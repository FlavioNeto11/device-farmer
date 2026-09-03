-- +goose Up

-- =====================================================================
-- THE AXIOM
--
-- A lease is ended by the job, by a deadline the user wrote down, or by
-- a human. Nothing else. Not a socket error, not a probe timeout, not a
-- device going offline, not a pod dying.
--
-- THREE CLOCKS THAT ARE NEVER COLLAPSED
--   1. Lease liveness  — leases.heartbeat_at / expires_at. Answers only
--                        "does the entity holding this lease still exist?"
--   2. Job liveness    — device-side progress. An alerting concern that
--                        can NEVER release a device.
--   3. Device health   — device_runtime.adb_state. Drives the watchdog
--                        and touches the lease exactly never.
--
-- DeviceFarmer/STF issue #663 fuses (3) and a transport failure of (1)
-- into a release decision. That fusion is the entire bug. Here the two
-- are separated by table, by function, and by Postgres role.
-- =====================================================================

-- ---------------------------------------------------------------------
-- Roles. BLOCKER 3 FIX.
--
-- The draft design placed lease_acquire and lease_reclaim in the same
-- process, which voided the firewall: lease_acquire must read
-- device_runtime (health) to allocate, while lease_reclaim must be
-- unable to. One role cannot both have and not have that SELECT.
--
-- Resolution: farm_reaper is a distinct role with SELECT revoked on
-- device_runtime, and lease_reclaim() executes SET LOCAL ROLE farm_reaper
-- for the duration of its transaction. Even called from the scheduler's
-- own connection, reclamation is structurally blind to health.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'farm_reaper') THEN
    CREATE ROLE farm_reaper NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'farm_scheduler') THEN
    CREATE ROLE farm_scheduler NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'farm_watchdog') THEN
    CREATE ROLE farm_watchdog NOLOGIN;
  END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
  EXECUTE 'GRANT USAGE ON SCHEMA farm TO farm_reaper, farm_scheduler, farm_watchdog';

  -- The reaper sees allocation state and nothing else.
  EXECUTE 'GRANT SELECT, UPDATE ON farm.leases        TO farm_reaper';
  EXECUTE 'GRANT SELECT, UPDATE ON farm.devices       TO farm_reaper';
  EXECUTE 'GRANT SELECT, UPDATE ON farm.slots         TO farm_reaper';
  EXECUTE 'GRANT SELECT         ON farm.reaper_state  TO farm_reaper';
  EXECUTE 'GRANT SELECT         ON farm.control_plane_gap TO farm_reaper';
  EXECUTE 'GRANT INSERT         ON farm.events        TO farm_reaper';
  EXECUTE 'GRANT USAGE          ON SEQUENCE farm.fence_seq TO farm_reaper';

  -- THE FIREWALL. Health is invisible to reclamation, enforced by the
  -- database rather than by a style guide.
  EXECUTE 'REVOKE ALL ON farm.device_runtime FROM farm_reaper';

  -- The scheduler allocates, so it must read health.
  EXECUTE 'GRANT SELECT ON farm.device_runtime TO farm_scheduler';
  EXECUTE 'GRANT SELECT, INSERT, UPDATE ON farm.leases  TO farm_scheduler';
  EXECUTE 'GRANT SELECT, UPDATE ON farm.devices TO farm_scheduler';
  EXECUTE 'GRANT SELECT ON farm.slots, farm.jobs, farm.pools TO farm_scheduler';
  EXECUTE 'GRANT USAGE  ON SEQUENCE farm.fence_seq TO farm_scheduler';

  -- The watchdog writes health and may never touch a lease.
  EXECUTE 'GRANT SELECT, INSERT, UPDATE ON farm.device_runtime TO farm_watchdog';
  EXECUTE 'GRANT SELECT ON farm.devices, farm.slots, farm.hosts TO farm_watchdog';
  EXECUTE 'REVOKE ALL ON farm.leases FROM farm_watchdog';
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- Guard trigger: lease identity is immutable and deadlines are monotonic.
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

-- +goose StatementBegin
CREATE TRIGGER leases_guard BEFORE UPDATE ON farm.leases
  FOR EACH ROW EXECUTE FUNCTION farm.trg_leases_guard();
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- Denormalisation trigger: devices.current_lease_id and fence_floor.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.trg_leases_sync_device() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'INSERT' THEN
    UPDATE farm.devices
       SET current_lease_id = NEW.id,
           fence_floor      = GREATEST(fence_floor, NEW.fence),
           updated_at       = now()
     WHERE id = NEW.device_id;
  ELSIF NEW.state IN ('released','expired') AND OLD.state IN ('held','suspect') THEN
    UPDATE farm.devices
       SET current_lease_id = NULL,
           last_released_at = now(),
           updated_at       = now()
     WHERE id = NEW.device_id AND current_lease_id = NEW.id;
  END IF;
  RETURN NULL;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER leases_sync_device
  AFTER INSERT OR UPDATE OF state ON farm.leases
  FOR EACH ROW EXECUTE FUNCTION farm.trg_leases_sync_device();
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- ACQUIRE — idempotent on job_id.
--
-- PHASE 1 re-attaches an existing live lease and NEVER bumps the fence:
-- the job's own work may still be running detached on the device, and
-- bumping would fence out its own process.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.lease_acquire(
  p_job_id          uuid,
  p_holder          text,
  p_holder_instance uuid
) RETURNS TABLE (
  lease_id       uuid,
  device_id      uuid,
  slot_id        bigint,
  fence          bigint,
  expires_at     timestamptz,
  reclaimable_at timestamptz,
  reattached     boolean
)
LANGUAGE plpgsql AS $fn$
DECLARE
  j          farm.jobs%ROWTYPE;
  existing   farm.leases%ROWTYPE;
  v_dev      uuid;
  v_slot     bigint;
  v_rejected uuid[] := '{}';
  v_protected boolean;
BEGIN
  SELECT * INTO j FROM farm.jobs WHERE id = p_job_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'job % not found', p_job_id USING ERRCODE = 'no_data_found';
  END IF;

  -- ---- PHASE 1: re-attach -------------------------------------------
  -- A Kubernetes pod eviction (node drain, preemption, spot reclaim,
  -- cluster upgrade, OOM restart) is not evidence of death. The
  -- replacement pod calls acquire with the same job_id and gets the same
  -- lease, the same device and the SAME FENCE back.
  SELECT * INTO existing FROM farm.leases l
   WHERE l.job_id = p_job_id AND l.state IN ('held','suspect')
   FOR NO KEY UPDATE;

  IF FOUND THEN
    UPDATE farm.leases l
       SET holder          = p_holder,
           holder_instance = p_holder_instance,
           holder_epoch    = l.holder_epoch + 1,
           state           = 'held',
           heartbeat_at    = now(),
           heartbeat_seq   = l.heartbeat_seq + 1,
           expires_at      = GREATEST(l.expires_at,     now() + l.ttl),
           reclaimable_at  = GREATEST(l.reclaimable_at, now() + l.ttl + l.grace)
     WHERE l.id = existing.id
    RETURNING l.id, l.device_id, l.slot_id, l.fence, l.expires_at, l.reclaimable_at, true
      INTO lease_id, device_id, slot_id, fence, expires_at, reclaimable_at, reattached;
    RETURN NEXT;
    RETURN;
  END IF;

  -- ---- PHASE 2: allocate --------------------------------------------
  LOOP
    SELECT d.id, d.current_slot_id INTO v_dev, v_slot
      FROM farm.devices d
      JOIN farm.device_runtime r ON r.device_id = d.id
      JOIN farm.slots s          ON s.id = d.current_slot_id
     WHERE d.pool_id = j.pool_id
       AND d.admin_state = 'enabled'
       AND d.current_lease_id IS NULL      -- trigger-maintained, replaces an anti-join
       AND r.adb_state = 'device'
       AND r.health    = 'healthy'
       AND s.state     = 'active'
       AND s.rearm_at <= now()             -- post-reclaim fence quarantine
       AND (j.pin_device IS NULL OR d.id = j.pin_device)
       AND NOT (d.id = ANY (v_rejected))
     ORDER BY d.failure_score ASC, d.last_released_at NULLS FIRST, d.id
     LIMIT 1
     FOR NO KEY UPDATE OF d SKIP LOCKED;

    IF NOT FOUND THEN
      RETURN;   -- no capacity; caller re-queues
    END IF;

    -- ORDER BY materialises the sort BEFORE locking, so the row we
    -- locked may no longer qualify. Re-check under the lock.
    PERFORM 1
       FROM farm.devices d
       JOIN farm.device_runtime r ON r.device_id = d.id
       JOIN farm.slots s          ON s.id = d.current_slot_id
      WHERE d.id = v_dev
        AND d.admin_state = 'enabled'
        AND d.current_lease_id IS NULL
        AND r.adb_state = 'device'
        AND r.health    = 'healthy'
        AND s.state     = 'active'
        AND s.rearm_at <= now();

    EXIT WHEN FOUND;
    -- SKIP LOCKED does not skip locks we ourselves hold.
    v_rejected := v_rejected || v_dev;
  END LOOP;

  -- Long jobs are never auto-reclaimed: hold and page instead.
  v_protected := j.protected
    OR COALESCE(j.expected_duration > interval '30 minutes', false);

  INSERT INTO farm.leases (
    device_id, slot_id, job_id, tenant_id, queue_id, holder, holder_instance,
    protected, disruption_policy, ttl, grace, expires_at, reclaimable_at)
  VALUES (
    v_dev, v_slot, p_job_id, j.tenant_id, j.queue_id, p_holder, p_holder_instance,
    v_protected, j.disruption_policy, j.ttl, j.grace,
    now() + j.ttl, now() + j.ttl + j.grace)
  -- The WHERE predicate MUST be restated or the partial index is not
  -- inferred as the arbiter.
  ON CONFLICT (device_id) WHERE state IN ('held','suspect') DO NOTHING
  RETURNING id, farm.leases.device_id, farm.leases.slot_id, farm.leases.fence,
            farm.leases.expires_at, farm.leases.reclaimable_at, false
    INTO lease_id, device_id, slot_id, fence, expires_at, reclaimable_at, reattached;

  IF lease_id IS NULL THEN
    RETURN;   -- lost the race; caller retries
  END IF;

  RETURN NEXT;
END $fn$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- RENEW — the only thing that keeps a lease alive.
--
-- Issued by the job supervisor on a wall-clock timer over a DIFFERENT
-- WIRE from the ADB data path. Takes no device lock and contacts no
-- device. This path separation is the mechanism, not a happy accident.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.lease_renew(
  p_lease_id        uuid,
  p_fence           bigint,
  p_holder_instance uuid
) RETURNS TABLE (
  expires_at      timestamptz,
  reclaimable_at  timestamptz,
  was_suspect     boolean
)
LANGUAGE sql AS $fn$
  WITH pre AS (
    SELECT l.id, l.state FROM farm.leases l WHERE l.id = p_lease_id
  ), upd AS (
    UPDATE farm.leases l
       SET heartbeat_at   = now(),
           heartbeat_seq  = l.heartbeat_seq + 1,
           expires_at     = GREATEST(l.expires_at,     now() + l.ttl),
           reclaimable_at = GREATEST(l.reclaimable_at, now() + l.ttl + l.grace),
           state          = 'held',        -- self-heals suspect -> held
           -- BLOCKER 7 FIX: the witness cap counts only CONSECUTIVE
           -- witness-only extensions. Without this reset a 12-minute job
           -- exhausts the cap and the witness protects nothing for the
           -- rest of the run.
           witness_extensions = 0
     WHERE l.id = p_lease_id
       AND l.fence = p_fence
       AND l.holder_instance = p_holder_instance
       AND l.state IN ('held','suspect')
    RETURNING l.id, l.expires_at, l.reclaimable_at
  )
  SELECT u.expires_at, u.reclaimable_at, (p.state = 'suspect')
    FROM upd u JOIN pre p ON p.id = u.id;
$fn$;
-- +goose StatementEnd

-- ZERO ROWS RETURNED MEANS YOU ARE FENCED: abort the job, close every
-- ADB socket, write nothing. The pre-image comes from a sibling CTE
-- because RETURNING yields the POST-update row, so `RETURNING
-- state='suspect'` after `SET state='held'` is always false — a metric
-- that would silently read zero forever.

-- ---------------------------------------------------------------------
-- WITNESS — on-device proof that the HOLDER is alive.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.lease_witness(
  p_lease_id       uuid,
  p_fence          bigint,
  p_max_extensions int DEFAULT 12
) RETURNS timestamptz
LANGUAGE sql AS $fn$
  UPDATE farm.leases l
     SET witness_at         = now(),
         witness_extensions = l.witness_extensions + 1,
         reclaimable_at     = GREATEST(l.reclaimable_at, now() + l.grace)
   WHERE l.id = p_lease_id
     AND l.fence = p_fence
     AND l.state IN ('held','suspect')
     AND l.witness_extensions < p_max_extensions
     -- A witness carrying a fence at or below the device floor is stale.
     AND l.fence > (SELECT d.fence_floor FROM farm.devices d WHERE d.id = l.device_id)
  RETURNING l.reclaimable_at;
$fn$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- RELEASE — the normal end.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.lease_release(
  p_lease_id uuid,
  p_fence    bigint,
  p_reason   text,
  p_rearm    interval DEFAULT interval '35 seconds'
) RETURNS boolean
LANGUAGE plpgsql AS $fn$
DECLARE
  v_dev  uuid;
  v_slot bigint;
BEGIN
  UPDATE farm.leases
     SET state = 'released', released_at = now(), release_reason = p_reason
   WHERE id = p_lease_id AND fence = p_fence AND state IN ('held','suspect')
  RETURNING device_id, slot_id INTO v_dev, v_slot;

  IF v_dev IS NULL THEN
    RETURN false;
  END IF;

  -- Bump the floor so any socket still carrying the old fence is refused
  -- at the host proxy.
  UPDATE farm.devices SET fence_floor = nextval('farm.fence_seq') WHERE id = v_dev;
  IF v_slot IS NOT NULL THEN
    UPDATE farm.slots SET rearm_at = now() + p_rearm WHERE id = v_slot;
  END IF;
  RETURN true;
END $fn$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- REAPER ARM — cold-start quiescence and gap refund.
--
-- BLOCKER 8 FIX: the draft keyed the gap exclusively on the reaper's own
-- heartbeat. If farm-api were down (bad deploy, ingress misconfig, cert
-- expiry, OOM) while the reaper and Postgres stayed healthy, the reaper's
-- heartbeat would be fresh, no gap would be recorded, and after TTL+grace
-- every unprotected lease in the farm would be reclaimed — precisely the
-- mass-reclaim the design exists to prevent.
--
-- The gap is now computed across EVERY component on the renewal path.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.reaper_arm(
  p_components text[] DEFAULT ARRAY['reaper','api','scheduler'],
  p_gap_floor  interval DEFAULT interval '60 seconds'
) RETURNS interval
LANGUAGE plpgsql AS $fn$
DECLARE
  v_prev    timestamptz;
  v_comp    text;
  v_gap     interval := interval '0';
  v_quiesce timestamptz;
BEGIN
  SELECT min(h.beat_at), (array_agg(h.component ORDER BY h.beat_at))[1]
    INTO v_prev, v_comp
    FROM farm.component_heartbeat h
   WHERE h.component = ANY (p_components);

  IF v_prev IS NOT NULL AND now() - v_prev > p_gap_floor THEN
    v_gap := now() - v_prev;
    INSERT INTO farm.control_plane_gap (component, started_at, ended_at)
    VALUES (v_comp, v_prev, now());

    -- Refund our downtime. A control-plane outage costs the tenant
    -- exactly zero lease budget.
    UPDATE farm.leases
       SET expires_at     = expires_at + v_gap,
           reclaimable_at = reclaimable_at + v_gap
     WHERE state IN ('held','suspect');
  END IF;

  -- A reaper that has just been blind must not mass-revoke at the moment
  -- of restoration. The delay derives from the longest TTL it could have
  -- missed, not from a guessed constant.
  SELECT now() + COALESCE(max(ttl), interval '15 minutes') INTO v_quiesce
    FROM farm.leases WHERE state IN ('held','suspect');

  UPDATE farm.reaper_state
     SET quiesce_until = v_quiesce, armed_at = now()
   WHERE singleton;

  RETURN v_gap;
END $fn$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- SUSPECT SWEEP — alerts. Releases nothing.
--
-- Entering `suspect` does NOTHING: no reset, no reboot, no reallocation.
-- The device stays unschedulable and stays with its holder. A heartbeat
-- anywhere in the grace band self-heals suspect -> held at the SAME
-- fence with zero work lost.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.lease_mark_suspect(p_limit int DEFAULT 500)
RETURNS TABLE (lease_id uuid, device_id uuid, job_id uuid, protected boolean)
LANGUAGE sql AS $fn$
  UPDATE farm.leases l
     SET state = 'suspect'
   WHERE l.id IN (
     SELECT s.id FROM farm.leases s
      WHERE s.state = 'held' AND s.expires_at < now()
      ORDER BY s.expires_at
      FOR NO KEY UPDATE OF s SKIP LOCKED
      LIMIT p_limit)
     AND l.state = 'held' AND l.expires_at < now()
  RETURNING l.id, l.device_id, l.job_id, l.protected;
$fn$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- RECLAIM — THE ONLY AUTOMATIC RELEASE PATH IN THE SYSTEM.
--
-- Note what this function does NOT read: farm.device_runtime. It cannot.
-- SET LOCAL ROLE farm_reaper strips the privilege for the duration of
-- the transaction, so health is structurally invisible to reclamation.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.lease_reclaim(
  p_limit int DEFAULT 100,
  p_rearm interval DEFAULT interval '35 seconds'
) RETURNS TABLE (
  lease_id  uuid,
  device_id uuid,
  slot_id   bigint,
  job_id    uuid,
  old_fence bigint,
  new_floor bigint
)
LANGUAGE plpgsql AS $fn$
DECLARE
  st farm.reaper_state%ROWTYPE;
BEGIN
  SELECT * INTO st FROM farm.reaper_state WHERE singleton;
  IF NOT st.enabled OR now() < st.quiesce_until THEN
    RETURN;   -- quiesce gate
  END IF;

  -- BLOCKER 3 FIX: drop to the reaper role for this transaction.
  SET LOCAL ROLE farm_reaper;

  RETURN QUERY
  WITH cand AS (
    SELECT l.id FROM farm.leases l
     WHERE l.state = 'suspect'
       AND l.reclaimable_at < now()
       AND l.protected = false                 -- hold and page instead
       AND (l.witness_at IS NULL OR l.witness_at < now() - l.grace)
       -- Never reclaim across a control-plane gap, for ANY component.
       AND NOT EXISTS (
             SELECT 1 FROM farm.control_plane_gap g
              WHERE g.ended_at > l.heartbeat_at
                AND g.ended_at > now() - interval '6 hours')
     ORDER BY l.reclaimable_at
     FOR NO KEY UPDATE OF l SKIP LOCKED
     LIMIT p_limit
  ), closed AS (
    UPDATE farm.leases l
       SET state = 'expired', released_at = now(), release_reason = 'holder_expired'
      FROM cand c
     WHERE l.id = c.id AND l.state = 'suspect' AND l.reclaimable_at < now()
    RETURNING l.id, l.device_id, l.slot_id, l.job_id, l.fence
  ), fenced AS (
    UPDATE farm.devices d
       SET fence_floor = nextval('farm.fence_seq')
      FROM closed c
     WHERE d.id = c.device_id
    RETURNING d.id AS device_id, d.fence_floor
  ), quar AS (
    UPDATE farm.slots s
       SET rearm_at = now() + p_rearm       -- must exceed the proxy self-fence
      FROM closed c
     WHERE s.id = c.slot_id
    RETURNING s.id
  )
  SELECT c.id, c.device_id, c.slot_id, c.job_id, c.fence, f.fence_floor
    FROM closed c JOIN fenced f ON f.device_id = c.device_id;
END $fn$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- MAX RUNTIME — the only other automatic release, and it fires on a
-- value the user wrote down in jobs.max_runtime.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.lease_expire_max_runtime(p_limit int DEFAULT 100)
RETURNS TABLE (lease_id uuid, device_id uuid, job_id uuid)
LANGUAGE sql AS $fn$
  UPDATE farm.leases l
     SET state = 'expired', released_at = now(), release_reason = 'max_runtime'
   WHERE l.id IN (
     SELECT s.id FROM farm.leases s
       JOIN farm.jobs j ON j.id = s.job_id
      WHERE s.state IN ('held','suspect')
        AND j.max_runtime IS NOT NULL
        AND now() > s.acquired_at + j.max_runtime
      ORDER BY s.acquired_at
      FOR NO KEY UPDATE OF s SKIP LOCKED
      LIMIT p_limit)
  RETURNING l.id, l.device_id, l.job_id;
$fn$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- Component heartbeat, written by every component on the renewal path.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.component_beat(p_component text)
RETURNS void
LANGUAGE sql AS $fn$
  INSERT INTO farm.component_heartbeat (component, beat_at)
  VALUES (p_component, now())
  ON CONFLICT (component) DO UPDATE SET beat_at = now();
$fn$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS farm.component_beat(text);
DROP FUNCTION IF EXISTS farm.lease_expire_max_runtime(int);
DROP FUNCTION IF EXISTS farm.lease_reclaim(int, interval);
DROP FUNCTION IF EXISTS farm.lease_mark_suspect(int);
DROP FUNCTION IF EXISTS farm.reaper_arm(text[], interval);
DROP FUNCTION IF EXISTS farm.lease_release(uuid, bigint, text, interval);
DROP FUNCTION IF EXISTS farm.lease_witness(uuid, bigint, int);
DROP FUNCTION IF EXISTS farm.lease_renew(uuid, bigint, uuid);
DROP FUNCTION IF EXISTS farm.lease_acquire(uuid, text, uuid);
DROP TRIGGER IF EXISTS leases_sync_device ON farm.leases;
DROP TRIGGER IF EXISTS leases_guard ON farm.leases;
DROP FUNCTION IF EXISTS farm.trg_leases_sync_device();
DROP FUNCTION IF EXISTS farm.trg_leases_guard();
-- +goose StatementEnd
