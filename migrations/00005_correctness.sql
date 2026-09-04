-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- Four correctness defects in the core, found by documenting it.
--
-- None of these are missing features. Each is a code path that exists,
-- is reachable, and does something other than what its own callers and
-- its own API responses say it does. They are grouped here because they
-- share one property: an operator would only discover them at the worst
-- possible moment.
-- =====================================================================


-- ---------------------------------------------------------------------
-- 1. farm.resolve_device: the hardware-fingerprint rung cannot execute.
--
--    It calls min(d.id) on a uuid column, and PostgreSQL has no min()
--    for uuid:
--        ERROR: function min(uuid) does not exist
--
--    So the SECOND-strongest identity signal in the whole system throws
--    the moment two devices share a fingerprint — which is exactly the
--    case it exists to adjudicate. Enrollment fails with a SQL error
--    rather than resolving or adopting, and the device never joins.
--
--    Rewritten to count and fetch separately: unambiguous means exactly
--    one row, and one row does not need an aggregate to identify it.
-- ---------------------------------------------------------------------
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.resolve_device(
  p_host_id    text,
  p_usb_path   text,
  p_farm_uid   text,
  p_hw_fp      bytea,
  p_serial     text,
  p_pool_id    text DEFAULT 'default',
  p_props      jsonb DEFAULT '{}'::jsonb
) RETURNS TABLE (device_id uuid, resolution text, slot_id bigint)
LANGUAGE plpgsql AS $fn$
#variable_conflict use_column
DECLARE
  v_slot  bigint;
  v_dev   uuid;
  v_res   text;
  v_uid   text;
  v_cnt   int;
BEGIN
  SELECT s.id INTO v_slot FROM farm.slots s
   WHERE s.host_id = p_host_id AND s.usb_path = p_usb_path;
  IF v_slot IS NULL THEN
    RAISE EXCEPTION 'no slot registered at %/% — run topology discovery first',
      p_host_id, p_usb_path USING ERRCODE = 'no_data_found';
  END IF;

  -- 1. The device is carrying our brand.
  IF p_farm_uid IS NOT NULL AND p_farm_uid <> '' THEN
    SELECT d.id INTO v_dev FROM farm.devices d WHERE d.farm_uid = p_farm_uid;
    IF v_dev IS NOT NULL THEN
      v_res := 'branded_uid';
    END IF;
  END IF;

  -- 2. Hardware fingerprint. Count first, then fetch: a fingerprint that
  --    matches more than one device identifies nothing, and one that
  --    matches exactly one needs no aggregate to pick it out.
  IF v_dev IS NULL AND p_hw_fp IS NOT NULL THEN
    SELECT count(*) INTO v_cnt FROM farm.devices d WHERE d.hw_fingerprint = p_hw_fp;
    IF v_cnt = 1 THEN
      SELECT d.id INTO v_dev FROM farm.devices d WHERE d.hw_fingerprint = p_hw_fp;
      v_res := 'hw_fingerprint';
    ELSIF v_cnt > 1 THEN
      -- Two devices claiming one fingerprint is a data problem worth
      -- recording, not a coin flip between them.
      v_res := 'ambiguous';
    END IF;
  END IF;

  -- 3. Serial, but ONLY together with the slot it was last seen in.
  --    A serial on its own identifies nothing: clone serials are real.
  IF v_dev IS NULL AND p_serial IS NOT NULL AND p_serial <> '' THEN
    SELECT count(*) INTO v_cnt FROM farm.devices d WHERE d.adb_serial = p_serial;
    IF v_cnt = 1 THEN
      SELECT d.id INTO v_dev FROM farm.devices d
       WHERE d.adb_serial = p_serial AND d.current_slot_id = v_slot;
      IF v_dev IS NOT NULL THEN
        v_res := 'serial_and_slot';
      END IF;
    ELSIF v_cnt > 1 THEN
      v_res := 'ambiguous';
      UPDATE farm.devices d SET serial_ambiguous = true
       WHERE d.adb_serial = p_serial AND NOT d.serial_ambiguous;
    END IF;
  END IF;

  -- 4. Nothing matched: adopt it.
  IF v_dev IS NULL THEN
    v_uid := 'df-' || encode(gen_random_bytes(16), 'hex');
    INSERT INTO farm.devices (
      farm_uid, adb_serial, hw_fingerprint, pool_id, host_id, current_slot_id,
      manufacturer, model, product, device_codename,
      android_release, sdk_int, build_fingerprint)
    VALUES (
      v_uid, nullif(p_serial, ''), p_hw_fp, p_pool_id, p_host_id, v_slot,
      p_props->>'manufacturer', p_props->>'model', p_props->>'product',
      p_props->>'device_codename', p_props->>'android_release',
      nullif(p_props->>'sdk_int', '')::int, p_props->>'build_fingerprint')
    RETURNING id INTO v_dev;

    INSERT INTO farm.device_runtime (device_id, host_id, slot_id)
    VALUES (v_dev, p_host_id, v_slot)
    ON CONFLICT (device_id) DO NOTHING;

    v_res := 'adopted_new';

    -- Ambiguity is only visible AFTER the insert. When the second clone of
    -- a duplicated serial arrives, the count at the moment of its own
    -- lookup is still one, so neither device would ever be flagged.
    IF p_serial IS NOT NULL AND p_serial <> '' THEN
      SELECT count(*) INTO v_cnt FROM farm.devices d WHERE d.adb_serial = p_serial;
      IF v_cnt > 1 THEN
        UPDATE farm.devices d SET serial_ambiguous = true
         WHERE d.adb_serial = p_serial AND NOT d.serial_ambiguous;
      END IF;
    END IF;
  ELSE
    UPDATE farm.devices d
       SET host_id         = p_host_id,
           current_slot_id = v_slot,
           adb_serial      = COALESCE(nullif(p_serial, ''), d.adb_serial),
           hw_fingerprint  = COALESCE(p_hw_fp, d.hw_fingerprint),
           manufacturer    = COALESCE(p_props->>'manufacturer', d.manufacturer),
           model           = COALESCE(p_props->>'model', d.model),
           android_release = COALESCE(p_props->>'android_release', d.android_release),
           sdk_int         = COALESCE(nullif(p_props->>'sdk_int', '')::int, d.sdk_int),
           build_fingerprint = COALESCE(p_props->>'build_fingerprint', d.build_fingerprint),
           updated_at      = now()
     WHERE d.id = v_dev;
  END IF;

  UPDATE farm.slot_occupancy o SET until = now(), reason = 'device moved'
   WHERE o.until IS NULL AND o.device_id = v_dev AND o.slot_id <> v_slot;
  UPDATE farm.slot_occupancy o SET until = now(), reason = 'slot reoccupied'
   WHERE o.until IS NULL AND o.slot_id = v_slot AND o.device_id <> v_dev;
  INSERT INTO farm.slot_occupancy (slot_id, device_id)
  SELECT v_slot, v_dev
   WHERE NOT EXISTS (SELECT 1 FROM farm.slot_occupancy o
                      WHERE o.until IS NULL AND o.slot_id = v_slot AND o.device_id = v_dev);

  device_id := v_dev; resolution := v_res; slot_id := v_slot;
  RETURN NEXT;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
-- ---------------------------------------------------------------------
-- 2. Selector matching, so farm.jobs.selector stops being decoration.
--
--    The column is stored, validated and echoed by the API, and no
--    allocator has ever read it: a job asking for Android 13+ has been
--    landing on Android 9 since the column was added.
--
--    The vocabulary is deliberately small and closed. Every key is a
--    hard filter; an unknown key is a REFUSAL rather than an ignored
--    line, because silently ignoring a constraint is how a job runs on
--    hardware its author excluded on purpose.
-- ---------------------------------------------------------------------
CREATE OR REPLACE FUNCTION farm.selector_unknown_keys(p_selector jsonb)
RETURNS text[]
LANGUAGE sql IMMUTABLE AS $fn$
  SELECT COALESCE(array_agg(k ORDER BY k), '{}')
    FROM jsonb_object_keys(COALESCE(p_selector, '{}'::jsonb)) AS k
   WHERE k NOT IN ('model', 'model_in', 'manufacturer', 'sdk_min', 'sdk_max',
                   'android_release', 'abi', 'labels', 'host_in', 'not_host_in');
$fn$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.device_matches(p_device uuid, p_selector jsonb)
RETURNS boolean
LANGUAGE plpgsql STABLE AS $fn$
DECLARE
  d farm.devices%ROWTYPE;
  s jsonb := COALESCE(p_selector, '{}'::jsonb);
BEGIN
  IF s = '{}'::jsonb THEN
    RETURN true;
  END IF;

  SELECT * INTO d FROM farm.devices WHERE id = p_device;
  IF NOT FOUND THEN
    RETURN false;
  END IF;

  IF s ? 'model'        AND d.model IS DISTINCT FROM (s->>'model')               THEN RETURN false; END IF;
  IF s ? 'manufacturer' AND d.manufacturer IS DISTINCT FROM (s->>'manufacturer') THEN RETURN false; END IF;
  IF s ? 'android_release' AND d.android_release IS DISTINCT FROM (s->>'android_release') THEN RETURN false; END IF;

  IF s ? 'model_in' AND NOT (d.model = ANY (
        SELECT jsonb_array_elements_text(s->'model_in'))) THEN RETURN false; END IF;

  -- An unknown sdk_int cannot satisfy a bound. Treating NULL as "passes"
  -- would place a job that asked for Android 13+ onto a handset whose
  -- version was never read, which is the failure the bound exists to stop.
  IF s ? 'sdk_min' AND (d.sdk_int IS NULL OR d.sdk_int < (s->>'sdk_min')::int) THEN RETURN false; END IF;
  IF s ? 'sdk_max' AND (d.sdk_int IS NULL OR d.sdk_int > (s->>'sdk_max')::int) THEN RETURN false; END IF;

  IF s ? 'abi' AND NOT ((s->>'abi') = ANY (d.abis)) THEN RETURN false; END IF;

  -- labels is containment: every key/value the job named must be present.
  IF s ? 'labels' AND NOT (d.labels @> (s->'labels')) THEN RETURN false; END IF;

  IF s ? 'host_in' AND NOT (d.host_id = ANY (
        SELECT jsonb_array_elements_text(s->'host_in'))) THEN RETURN false; END IF;
  IF s ? 'not_host_in' AND (d.host_id = ANY (
        SELECT jsonb_array_elements_text(s->'not_host_in'))) THEN RETURN false; END IF;

  RETURN true;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
-- ---------------------------------------------------------------------
-- 3. Draining a host must actually stop placement on it.
--
--    POST /api/v1/hosts/{id}/drain sets farm.hosts.admin_state='draining'
--    and answers "no new leases will be placed on this host". The
--    allocator filtered farm.devices.admin_state — the DEVICE's — and
--    never looked at the host's, so the sentence was false and an
--    operator draining a host for maintenance kept receiving new work on
--    it while waiting for the live leases to end.
--
-- 4. A max_runtime expiry must fence and quarantine like every other
--    ending.
--
--    lease_release and lease_reclaim both raise devices.fence_floor and
--    set slots.rearm_at, so a client that has lost its lease cannot act
--    on the device afterwards. lease_expire_max_runtime did neither: the
--    device went straight back into the pool with the old fence still
--    below the floor, and a slow client could write to a handset that
--    now belonged to the next job.
-- ---------------------------------------------------------------------
CREATE OR REPLACE FUNCTION farm.lease_acquire(
  p_job_id          uuid,
  p_holder          text,
  p_holder_instance uuid
) RETURNS TABLE (
  lease_id uuid, device_id uuid, slot_id bigint, fence bigint,
  expires_at timestamptz, reclaimable_at timestamptz, reattached boolean
)
LANGUAGE plpgsql AS $fn$
#variable_conflict use_column
DECLARE
  j          farm.jobs%ROWTYPE;
  existing   farm.leases%ROWTYPE;
  v_dev      uuid;
  v_slot     bigint;
  v_rejected uuid[] := '{}';
  v_protected boolean;
  v_unknown  text[];
BEGIN
  SELECT * INTO j FROM farm.jobs WHERE id = p_job_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'no such job %', p_job_id USING ERRCODE = 'no_data_found';
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
    UPDATE farm.leases l
       SET holder = p_holder,
           holder_instance = p_holder_instance,
           holder_epoch = l.holder_epoch + 1,
           state = 'held',
           heartbeat_at = now(),
           expires_at = GREATEST(l.expires_at, now() + l.ttl),
           reclaimable_at = GREATEST(l.reclaimable_at, now() + l.ttl + l.grace),
           witness_extensions = 0
     WHERE l.id = existing.id
    RETURNING l.id, l.device_id, l.slot_id, l.fence, l.expires_at, l.reclaimable_at, true
      INTO lease_id, device_id, slot_id, fence, expires_at, reclaimable_at, reattached;
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
    protected, disruption_policy, ttl, grace, expires_at, reclaimable_at)
  VALUES (
    v_dev, v_slot, p_job_id, j.tenant_id, j.queue_id, p_holder, p_holder_instance,
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
-- CREATE OR REPLACE matches on the ARGUMENT LIST, so adding p_rearm creates
-- an OVERLOAD rather than replacing the original. Both would then exist, and
-- the one-argument call in internal/lease/store.go:460 becomes ambiguous:
--
--   ERROR: function farm.lease_expire_max_runtime(integer) is not unique
--
-- which is the reaper failing in production on a path no SQL assertion here
-- exercises, because these assertions call the two-argument form. The Go test
-- added alongside this migration is what caught it. Drop the old signature
-- explicitly so exactly one function answers that name.
DROP FUNCTION IF EXISTS farm.lease_expire_max_runtime(int);

CREATE OR REPLACE FUNCTION farm.lease_expire_max_runtime(
  p_limit int DEFAULT 100,
  p_rearm interval DEFAULT interval '35 seconds'
) RETURNS TABLE (lease_id uuid, device_id uuid, job_id uuid, fence bigint)
LANGUAGE plpgsql AS $fn$
#variable_conflict use_column
BEGIN
  RETURN QUERY
  WITH due AS (
    SELECT l.id
      FROM farm.leases l
      JOIN farm.jobs   j ON j.id = l.job_id
     WHERE l.state IN ('held','suspect')
       AND j.max_runtime IS NOT NULL
       AND l.acquired_at + j.max_runtime <= now()
     ORDER BY l.acquired_at
     LIMIT p_limit
     FOR UPDATE OF l SKIP LOCKED
  ),
  closed AS (
    UPDATE farm.leases l
       SET state = 'expired', released_at = now(), release_reason = 'max_runtime'
      FROM due
     WHERE l.id = due.id
    RETURNING l.id, l.device_id, l.job_id, l.fence, l.slot_id
  ),
  -- A max_runtime ending is an ending like any other, so it fences like
  -- one. Without this the device returned to the pool with its floor
  -- still at or below the fence the previous holder is carrying, and a
  -- client that had not yet noticed could write to a handset that now
  -- belonged to somebody else.
  fenced AS (
    UPDATE farm.devices d
       SET fence_floor = nextval('farm.fence_seq'),
           last_released_at = now()
      FROM closed c
     WHERE d.id = c.device_id
    RETURNING d.id AS device_id
  ),
  rearmed AS (
    UPDATE farm.slots s
       SET rearm_at = now() + p_rearm
      FROM closed c
     WHERE s.id = c.slot_id
    RETURNING s.id
  )
  SELECT c.id, c.device_id, c.job_id, c.fence FROM closed c;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
-- ---------------------------------------------------------------------
-- v_fleet showed only device-scoped quarantines, so a hub- or
-- host-scoped one — the kind the correlation logic actually opens when a
-- hub sheds its devices — was invisible on the very grid an operator
-- stares at during that incident.
-- ---------------------------------------------------------------------
CREATE OR REPLACE VIEW farm.v_fleet AS
SELECT d.id                AS device_id,
       d.farm_uid, d.adb_serial, d.serial_ambiguous, d.model, d.manufacturer,
       d.android_release, d.sdk_int, d.pool_id, d.admin_state, d.labels,
       d.failure_score, d.fence_floor,
       s.id                AS slot_id,
       s.rack_slot, s.usb_path, s.adb_devpath,
       s.state             AS slot_state,
       s.rearm_at,
       hb.id               AS hub_id,
       hb.usb_path         AS hub_path,
       hb.vbus_switchable,
       ho.id               AS host_id,
       ho.adb_endpoint,
       ho.admin_state      AS host_admin_state,
       r.adb_state, r.health, r.health_since, r.battery_pct, r.battery_temp_dc,
       r.consec_bad, r.ladder_tier, r.last_seen_at,
       l.id                AS lease_id,
       l.fence, l.state    AS lease_state,
       l.protected, l.job_id, l.tenant_id, l.holder,
       l.acquired_at, l.expires_at, l.reclaimable_at,
       q.id                AS quarantine_id,
       q.reason            AS quarantine_reason,
       q.scope             AS quarantine_scope
  FROM farm.devices d
  LEFT JOIN farm.slots s          ON s.id = d.current_slot_id
  LEFT JOIN farm.hubs hb          ON hb.id = s.hub_id
  LEFT JOIN farm.hosts ho         ON ho.id = d.host_id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
  LEFT JOIN farm.leases l         ON l.id = d.current_lease_id
  LEFT JOIN LATERAL (
    -- The narrowest open quarantine that covers this device, whatever
    -- scope it was opened at.
    SELECT q.id, q.reason, q.scope
      FROM farm.quarantines q
     WHERE q.closed_at IS NULL
       AND (   (q.scope = 'device' AND q.device_id = d.id)
            OR (q.scope = 'slot'   AND q.slot_id   = s.id)
            OR (q.scope = 'hub'    AND q.hub_id    = hb.id)
            OR (q.scope = 'host'   AND q.host_id   = ho.id)
            OR (q.scope = 'power_domain' AND q.slot_id IS NOT DISTINCT FROM s.id))
     ORDER BY CASE q.scope WHEN 'device' THEN 0 WHEN 'slot' THEN 1
                           WHEN 'power_domain' THEN 2 WHEN 'hub' THEN 3
                           ELSE 4 END
     LIMIT 1
  ) q ON true;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The previous definitions live in 00002 and 00004; rolling back means
-- re-running those files' function bodies, which goose cannot do for a
-- CREATE OR REPLACE. Down is therefore a deliberate no-op: these are bug
-- fixes, and reverting them restores the bugs.
SELECT 1;
-- +goose StatementEnd
