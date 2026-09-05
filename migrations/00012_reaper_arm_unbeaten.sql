-- +goose Up

-- ---------------------------------------------------------------------
-- REAPER ARM — a watched component that has never beaten.
--
-- farm.reaper_arm computed the control-plane gap as
--
--     SELECT min(h.beat_at) FROM farm.component_heartbeat h
--      WHERE h.component = ANY (p_components)
--
-- and farm.component_heartbeat has no seed rows: a component gets a row
-- the first time it calls farm.component_beat and not before. So a name in
-- p_components that had NEVER beaten was simply absent from the minimum.
-- The gap read small, nothing was refunded, and TTL+grace then ran against
-- leases whose holder had never once been given the chance to renew — the
-- BLOCKER 8 mass-reclaim, reached through a component the accounting could
-- not see. When NO watched component had a row, v_prev was NULL, the whole
-- refund was skipped, and the quiesce UPDATE still ran and stamped
-- armed_at as though the arm had meant something.
--
-- The decision: a watched component with no heartbeat row means the reaper
-- REFUSES TO ARM, loudly, and reclaims nothing until that changes.
--
-- Not "treat the missing row as an infinite gap" — that refund would be
-- applied on every arm and would quietly disable the reaper forever with
-- nothing to say so. Not "ignore the name" — that is the bug. A refusal is
-- visible in farm.reaper_state, in farm.events, on GET /api/v1/reaper, in
-- `ctl reaper` and on farm_reaper_unbeaten_components, and it clears by
-- itself the moment the component beats and the reaper arms again.
--
-- The mirror hazard is unchanged here: a component that beat once and was
-- then scaled to zero leaves a STALE row, which still reads as an outage
-- and still refunds every second since it left on every arm. That is the
-- safe direction, it is documented on config.DefaultReaperComponents, and
-- it is a different defect from this one.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
ALTER TABLE farm.reaper_state
  ADD COLUMN last_refusal    text,
  ADD COLUMN last_refusal_at timestamptz,
  ADD CONSTRAINT reaper_state_refusal_pair
    CHECK ((last_refusal IS NULL) = (last_refusal_at IS NULL));
-- +goose StatementEnd

-- A different return shape is a different signature, and CREATE OR REPLACE
-- would leave the interval-returning form in place beside this one.
-- +goose StatementBegin
DROP FUNCTION IF EXISTS farm.reaper_arm(text[], interval);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION farm.reaper_arm(
  p_components text[] DEFAULT ARRAY['reaper','api','scheduler'],
  p_gap_floor  interval DEFAULT interval '60 seconds'
) RETURNS TABLE (armed boolean, gap interval, unbeaten text[])
LANGUAGE plpgsql AS $fn$
#variable_conflict use_column
DECLARE
  v_standing text;
  v_unbeaten text[];
  v_refusal  text;
  v_prev     timestamptz;
  v_comp     text;
  v_gap      interval := interval '0';
  v_quiesce  timestamptz;
BEGIN
  SELECT last_refusal INTO v_standing FROM farm.reaper_state WHERE singleton;

  -- One pass over every watched NAME, joined to whatever row it has. The
  -- old query started from the rows, and a name with no row cannot be
  -- missed by a query that never mentions it. The same pass yields the
  -- oldest beat and its component, so the table is read once whichever
  -- way the arm goes.
  SELECT array_agg(c.name ORDER BY c.name) FILTER (WHERE h.component IS NULL),
         min(h.beat_at),
         (array_agg(h.component ORDER BY h.beat_at) FILTER (WHERE h.component IS NOT NULL))[1]
    INTO v_unbeaten, v_prev, v_comp
    FROM unnest(p_components) AS c(name)
    LEFT JOIN farm.component_heartbeat h ON h.component = c.name;

  IF v_unbeaten IS NOT NULL THEN
    v_refusal := format(
      'refused to arm: watched component(s) %s have never written a heartbeat; '
      'the reaper cannot tell their silence from an outage it should refund, '
      'so nothing is reclaimed until every one of them has beaten',
      array_to_string(v_unbeaten, ', '));

    -- The refusal is the standing state, and it is NOT an arm: armed_at and
    -- quiesce_until are left exactly where the last real arm put them.
    --
    -- Written once per CHANGE of refusal, not once per attempt. The reaper
    -- retries every cycle, and a write every ten seconds that moved only
    -- the timestamp would make last_refusal_at mean "the last retry" —
    -- useless — instead of "when this refusal began", which is the thing
    -- an operator wants to know. The row lock the UPDATE takes is also
    -- what keeps two callers arming at once from both recording the same
    -- change: the second one finds the row already changed and skips.
    UPDATE farm.reaper_state
       SET last_refusal = v_refusal, last_refusal_at = now()
     WHERE singleton AND last_refusal IS DISTINCT FROM v_refusal;

    -- The ledger follows the same rule, for the same reason: a row every
    -- ten seconds would bury the one that says when this started.
    IF FOUND THEN
      INSERT INTO farm.events (kind, actor, detail)
      VALUES ('reaper_arm_refused', 'farm.reaper_arm',
              jsonb_build_object('unbeaten', to_jsonb(v_unbeaten),
                                 'components', to_jsonb(p_components),
                                 'refusal', v_refusal));
    END IF;

    RETURN QUERY SELECT false, interval '0', v_unbeaten;
    RETURN;
  END IF;

  -- From here on every watched name has a row, so the minimum above is
  -- over the whole set and the refund math is the same as it always was.
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
     SET quiesce_until   = v_quiesce,
         armed_at        = now(),
         last_refusal    = NULL,
         last_refusal_at = NULL
   WHERE singleton;

  IF v_standing IS NOT NULL THEN
    INSERT INTO farm.events (kind, actor, detail)
    VALUES ('reaper_armed', 'farm.reaper_arm',
            jsonb_build_object('cleared_refusal', v_standing,
                               'gap', v_gap::text,
                               'quiesce_until', v_quiesce));
  END IF;

  RETURN QUERY SELECT true, v_gap, NULL::text[];
END $fn$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- RECLAIM — the gate learns about the refusal.
--
-- The body is 00002's, unchanged except for one more condition on the
-- gate. It is enforced HERE, in the only automatic release path, rather
-- than left to the loop that calls it: a refusal recorded by one caller
-- (the API's enable, the reaper's own arm) must gate every caller, and a
-- future caller that never arms at all must find the gate shut too.
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
LANGUAGE plpgsql
-- The role firewall from 00002, kept verbatim: a function-level SET is
-- restored on exit, and running as farm_reaper is what makes device health
-- structurally invisible to reclamation. See the original for the full
-- rationale.
SET role = farm_reaper
AS $fn$
#variable_conflict use_column
DECLARE
  st farm.reaper_state%ROWTYPE;
BEGIN
  SELECT * INTO st FROM farm.reaper_state WHERE singleton;
  -- quiesce gate, kill switch, and the refusal: while the last arm refused,
  -- the reaper is not armed and this function reclaims nothing.
  IF NOT st.enabled OR now() < st.quiesce_until OR st.last_refusal_at IS NOT NULL THEN
    RETURN;
  END IF;

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

-- +goose Down

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
LANGUAGE plpgsql
SET role = farm_reaper
AS $fn$
#variable_conflict use_column
DECLARE
  st farm.reaper_state%ROWTYPE;
BEGIN
  SELECT * INTO st FROM farm.reaper_state WHERE singleton;
  IF NOT st.enabled OR now() < st.quiesce_until THEN
    RETURN;
  END IF;

  RETURN QUERY
  WITH cand AS (
    SELECT l.id FROM farm.leases l
     WHERE l.state = 'suspect'
       AND l.reclaimable_at < now()
       AND l.protected = false
       AND (l.witness_at IS NULL OR l.witness_at < now() - l.grace)
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
       SET rearm_at = now() + p_rearm
      FROM closed c
     WHERE s.id = c.slot_id
    RETURNING s.id
  )
  SELECT c.id, c.device_id, c.slot_id, c.job_id, c.fence, f.fence_floor
    FROM closed c JOIN fenced f ON f.device_id = c.device_id;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION IF EXISTS farm.reaper_arm(text[], interval);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION farm.reaper_arm(
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
    UPDATE farm.leases
       SET expires_at     = expires_at + v_gap,
           reclaimable_at = reclaimable_at + v_gap
     WHERE state IN ('held','suspect');
  END IF;

  SELECT now() + COALESCE(max(ttl), interval '15 minutes') INTO v_quiesce
    FROM farm.leases WHERE state IN ('held','suspect');

  UPDATE farm.reaper_state
     SET quiesce_until = v_quiesce, armed_at = now()
   WHERE singleton;

  RETURN v_gap;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE farm.reaper_state
  DROP CONSTRAINT IF EXISTS reaper_state_refusal_pair,
  DROP COLUMN IF EXISTS last_refusal_at,
  DROP COLUMN IF EXISTS last_refusal;
-- +goose StatementEnd
