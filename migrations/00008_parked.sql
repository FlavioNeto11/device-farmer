-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- PARKED — out of service ON PURPOSE.
--
-- The hole this closes
-- --------------------
-- Charge limiting holds a handset between 40% and 80% by cutting USB
-- VBUS at the port. To every loop in this system that looks like a
-- catastrophe:
--
--   adb reports the device 'absent'
--     -> watchdog.healthFor() maps 'absent' to 'missing'
--     -> after MinBad observations the watchdog writes health='missing'
--     -> recovery ladder candidates() adopts it (it excludes only
--        healthy/retired/quarantined/unknown)
--     -> the ladder climbs — reprobe, reconnect, soft reset, port power
--        cycle — and finally QUARANTINES a perfectly good phone whose
--        only problem was that somebody was looking after its battery.
--
-- device_runtime.suppress_until is the wrong instrument: it is bounded
-- by the watchdog's MaxSuppress because it exists to cover an induced
-- reset lasting seconds, it cannot span a multi-hour charge hold, and it
-- says nothing about WHO decided or WHY. An operator reading the fleet
-- grid during an incident needs the difference between "this device is
-- broken" and "this device is fine and I took it out myself" to be a
-- fact in the database, not a note in a chat channel.
--
-- Where the marker lives, and why it lives there
-- ----------------------------------------------
-- Three planes read this state, and the requirement was that each read
-- it from a table it already has open, with no new join per row on its
-- hot path:
--
--   farm.devices.admin_state = 'parked'   IS THE AUTHORITY.
--
--     * The allocator already refuses anything but 'enabled'
--       (farm.lease_acquire, 00005_correctness.sql) and it already joins
--       farm.devices. Zero new predicates, zero new joins.
--     * The recovery ladder already requires d.admin_state = 'enabled'
--       in candidates() and already joins farm.devices. Same.
--     * The health plane CANNOT WRITE IT. farm_watchdog holds SELECT on
--       farm.devices and nothing more (00002_lease.sql), so "an
--       observation un-parked a device" is not a bug that can be
--       written — it is a privilege the health role does not have.
--     * The value is precedent, not invention: admin_state already
--       carries 'disabled', which means exactly this and which no Go
--       code has ever set. 'parked' is the same idea with a name that
--       says why, a ledger that says who, and a state machine.
--
--   farm.device_runtime.health = 'parked'  IS A MIRROR.
--
--     The watchdog's reconcile statement (internal/watchdog/watchdog.go,
--     write()) reads exactly one table: farm.device_runtime. Requiring
--     it to join farm.devices would put an extra lookup on the busiest
--     statement in the system, so the fact is mirrored onto the row that
--     statement already has open. The CASE there now holds 'parked' the
--     way it already holds 'retired' and 'quarantined'.
--
--     A mirror that can drift is a lie, so it is not defended by
--     convention. Guard B below refuses to let health leave 'parked'
--     until admin_state has, which makes the mirror derived rather than
--     independent: there is exactly one authority, and it is farm.devices.
--
--   farm.device_parks  CARRIES WHO AND WHY.
--
--     Modelled on farm.quarantines (00003_ops.sql): an append-only
--     ledger with one open row per device — who opened it, why, whether
--     automation opened it, and who closed it. It is on nobody's hot
--     path, which is precisely why the who/why belongs here and not in a
--     text column on farm.devices that every allocation query would then
--     carry around.
--
-- Alternatives rejected
-- ---------------------
--   * A boolean on farm.device_runtime alone. The allocator and the
--     ladder would each need a NEW predicate, and — worse — the parked
--     fact would live in the one table the health loop is allowed to
--     overwrite. An administrative decision must not be one watchdog bug
--     away from being erased.
--   * A standalone table as the only home. Correct, and unusable: every
--     one of the three hot paths would grow a join per row to answer
--     "may I touch this device?".
--   * Widening suppress_until. It is a health-plane damper with a bound,
--     and it has no room for an actor or a reason. Making it unbounded
--     would delete the property that makes it safe.
--
-- Undoing it
-- ----------
-- Nothing automatic un-parks a device. The rule is copied from
-- internal/topo/discover.go restore(), which reactivates ONLY slots
-- whose most recent audit row is discovery's own retirement: a human who
-- made a decision has the last word, and a timer waking up and undoing
-- it behind their back is the class of automation this system refuses to
-- contain. Here that becomes: farm.device_unpark(..., p_auto => true)
-- may close only a park that the SAME automated actor opened. Everything
-- else needs a human, and the human is named in farm.audit_log.
--
-- THE LEASE INVARIANT
-- -------------------
-- A lease ends when the job says so, when a user-written deadline
-- elapses, or when a human takes it back. NOTHING ELSE. Parking a device
-- says "do not give this out AGAIN"; it has no opinion whatsoever about
-- work already in progress, exactly as admin_state has never had one.
--
-- That is not left to the reader. farm.device_park and
-- farm.device_unpark run under farm_parker, a role with no privilege of
-- any kind on farm.leases — not UPDATE, not SELECT. A future edit that
-- tried to end a lease from inside a park would raise 42501 rather than
-- destroy six hours of work. It is the same mechanism that makes
-- reclamation blind to health (00002_lease.sql), pointed the other way.
-- =====================================================================

-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- 1. The vocabulary.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
ALTER TABLE farm.devices DROP CONSTRAINT IF EXISTS devices_admin_state_check;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE farm.devices ADD CONSTRAINT devices_admin_state_check
  CHECK (admin_state IN ('enabled','disabled','parked','quarantined','retired'));
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE farm.device_runtime DROP CONSTRAINT IF EXISTS device_runtime_health_check;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE farm.device_runtime ADD CONSTRAINT device_runtime_health_check
  CHECK (health IN ('unknown','booting','healthy','degraded','offline','unauthorized',
                    'missing','recovering','parked','quarantined','retired'));
-- +goose StatementEnd

-- +goose StatementBegin
-- A CHECK dropped by the wrong name leaves the OLD constraint in place and
-- the new one beside it, so 'parked' would be rejected by a constraint
-- nobody remembered — after this migration reported success. Name the
-- survivors instead of trusting the two pairs of statements above.
DO $$
DECLARE v_left text;
BEGIN
  SELECT string_agg(c.conname, ', ') INTO v_left
    FROM pg_constraint c
   WHERE c.contype = 'c'
     AND ((c.conrelid = 'farm.devices'::regclass
           AND c.conname <> 'devices_admin_state_check'
           AND pg_get_constraintdef(c.oid) LIKE '%admin_state%')
       OR (c.conrelid = 'farm.device_runtime'::regclass
           AND c.conname <> 'device_runtime_health_check'
           AND pg_get_constraintdef(c.oid) LIKE '%health %'));
  IF v_left IS NOT NULL THEN
    RAISE EXCEPTION 'a second CHECK still constrains the parked columns: %; '
                    'the widening above did not take effect', v_left;
  END IF;
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- 2. The ledger: who parked it, why, and whether a human did it.
--
-- Shaped after farm.quarantines on purpose: an operator who can read one
-- can read the other, and the API that closes a quarantine and the API
-- that unparks a device have the same shape.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS farm.device_parks (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  device_id    uuid NOT NULL REFERENCES farm.devices(id) ON DELETE CASCADE,

  -- Both NOT NULL and both non-blank. "Parked" with no reason is
  -- indistinguishable from a fault, which is the confusion this state
  -- exists to end.
  reason       text NOT NULL CHECK (btrim(reason)    <> ''),
  opened_by    text NOT NULL CHECK (btrim(opened_by) <> ''),
  opened_at    timestamptz NOT NULL DEFAULT now(),

  -- auto marks a hold opened by a control loop rather than by a person
  -- (charge limiting is the first of them). It is what farm.device_unpark
  -- consults to decide whether automation is reversing its OWN decision
  -- or somebody else's.
  auto         boolean NOT NULL DEFAULT false,

  closed_at    timestamptz,
  closed_by    text,
  close_reason text,

  CHECK (closed_at IS NULL OR closed_at >= opened_at),
  CHECK ((closed_at IS NULL) = (closed_by IS NULL))
);
-- +goose StatementEnd

-- +goose StatementBegin
-- One open park per device. This is the constraint that makes "is this
-- device parked?" a question with one answer, and it turns a double-park
-- into an error instead of an orphaned ledger row.
CREATE UNIQUE INDEX IF NOT EXISTS device_parks_open
  ON farm.device_parks (device_id) WHERE closed_at IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS device_parks_open_any
  ON farm.device_parks (opened_at DESC) WHERE closed_at IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON TABLE farm.device_parks IS
  'Deliberate holds. One open row per device: who took it out of service, why, and whether automation or a human did it.';
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- 3. THE FIREWALL. Parking cannot end a lease, because the role it runs
--    as cannot see one.
--
--    farm_parker is to farm.leases what farm_reaper is to
--    farm.device_runtime: not discouraged, unrepresentable.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
DO $$
BEGIN
  -- Check for the login user that has no CREATEROLE, catch for the migrator
  -- creating this same cluster-wide role against another database at the
  -- same moment. 00002_lease.sql says why both halves at length.
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'farm_parker') THEN
    BEGIN CREATE ROLE farm_parker NOLOGIN;
    EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END;
  END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
  EXECUTE 'GRANT USAGE ON SCHEMA farm TO farm_parker';

  -- The authority, the mirror and the ledger. Nothing else.
  EXECUTE 'GRANT SELECT, UPDATE         ON farm.devices        TO farm_parker';
  EXECUTE 'GRANT SELECT, INSERT, UPDATE ON farm.device_runtime TO farm_parker';
  EXECUTE 'GRANT SELECT, INSERT, UPDATE ON farm.device_parks   TO farm_parker';
  EXECUTE 'GRANT INSERT                 ON farm.audit_log      TO farm_parker';
  EXECUTE 'GRANT INSERT                 ON farm.events         TO farm_parker';

  -- THE INVARIANT, enforced by Postgres. A lease ends when the job says
  -- so, when a user-written deadline elapses, or when a human takes it
  -- back. Parking is none of those three, so the role that parks may not
  -- so much as look at farm.leases.
  EXECUTE 'REVOKE ALL ON farm.leases FROM farm_parker';

  -- farm.device_park carries `SET role = farm_parker`, and SET ROLE
  -- requires the caller to be a member. The API pool connects as the
  -- migration owner; in a deployment where that role is not a superuser
  -- the functions below would fail on their first call without this.
  EXECUTE format('GRANT farm_parker TO %I', current_user);
END $$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- 4. The guards. Two triggers, and neither costs anything on the hot
--    path: their WHEN clause is a comparison on the tuple already in
--    hand, so no lookup happens unless a parked device is actually being
--    changed.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
-- Guard A: admin_state may not leave 'parked' while a park is open.
--
-- This is what makes "unparking is explicit" true rather than merely
-- intended. An UPDATE that flips admin_state back to 'enabled' — a
-- reconciler, a bulk fix-up, a well-meaning hand at a psql prompt —
-- raises instead of silently discarding a human's decision along with
-- the reason they recorded for it.
--
-- It RAISES rather than holding the value, because no bulk statement in
-- this system legitimately sweeps admin_state: the two that exist are
-- scoped to 'enabled' and to 'quarantined' respectively. An attempt to
-- un-park by UPDATE is therefore always a defect, and a defect that
-- fails loudly is the cheap kind.
CREATE OR REPLACE FUNCTION farm.trg_devices_park_guard() RETURNS trigger
LANGUAGE plpgsql
-- SECURITY DEFINER so the guard holds for every writer of farm.devices,
-- including farm_reaper and farm_scheduler, neither of which has any
-- privilege on farm.device_parks. A guard that can be switched off by
-- withholding a read is not a guard.
SECURITY DEFINER SET search_path = pg_catalog, farm
AS $fn$
BEGIN
  IF EXISTS (SELECT 1 FROM farm.device_parks p
              WHERE p.device_id = OLD.id AND p.closed_at IS NULL) THEN
    RAISE EXCEPTION 'device % is parked and cannot be un-parked by an UPDATE', OLD.id
      USING ERRCODE = 'insufficient_privilege',
            HINT    = 'call farm.device_unpark(device, actor, reason); a park names a '
                      'person and a reason, and automation does not get to erase either';
  END IF;
  RETURN NEW;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TRIGGER IF EXISTS devices_park_guard ON farm.devices;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER devices_park_guard
  BEFORE UPDATE OF admin_state ON farm.devices
  FOR EACH ROW
  WHEN (OLD.admin_state = 'parked' AND NEW.admin_state IS DISTINCT FROM OLD.admin_state)
  EXECUTE FUNCTION farm.trg_devices_park_guard();
-- +goose StatementEnd

-- +goose StatementBegin
-- Guard B: health may not leave 'parked' while the device is parked.
--
-- This is the one that stops the observation loop. The watchdog's own
-- CASE already holds the value; this makes that promise structural.
-- Whatever health the ADB tracker implies, and whichever loop writes it,
-- a parked device keeps reporting 'parked' until the authority in
-- farm.devices says otherwise.
--
-- It HOLDS the value rather than raising, deliberately, and the contrast
-- with Guard A is the point. Health is written in sweeps — the ladder
-- quarantines every device on a hub in one statement — and one parked
-- handset must not be able to abort a statement that is taking twelve
-- broken ones out of service. Holding is also exactly what the watchdog's
-- CASE does for 'retired' and 'quarantined', so a reader who knows that
-- path already knows this one.
CREATE OR REPLACE FUNCTION farm.trg_device_runtime_park_guard() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER SET search_path = pg_catalog, farm
AS $fn$
BEGIN
  IF EXISTS (SELECT 1 FROM farm.devices d
              WHERE d.id = OLD.device_id AND d.admin_state = 'parked') THEN
    NEW.health       := OLD.health;
    NEW.health_since := OLD.health_since;
  END IF;
  RETURN NEW;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TRIGGER IF EXISTS device_runtime_park_guard ON farm.device_runtime;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER device_runtime_park_guard
  BEFORE UPDATE OF health ON farm.device_runtime
  FOR EACH ROW
  WHEN (OLD.health = 'parked' AND NEW.health IS DISTINCT FROM OLD.health)
  EXECUTE FUNCTION farm.trg_device_runtime_park_guard();
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- 5. The state machine. These two functions are the only supported way
--    in and out of 'parked'.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.device_park(
  p_device uuid,
  p_actor  text,
  p_reason text,
  p_auto   boolean DEFAULT false
) RETURNS bigint
LANGUAGE plpgsql
-- A function-level SET is saved on entry and RESTORED ON EXIT, so the role
-- change cannot leak into the rest of the caller's transaction. `SET LOCAL
-- ROLE` in the body would leak it, and the next unrelated statement would
-- fail on a table farm_parker cannot see. Same mechanism, and the same
-- reason, as farm.lease_reclaim.
SET role = farm_parker
AS $fn$
DECLARE
  v_state text;
  v_park  bigint;
BEGIN
  IF p_actor IS NULL OR btrim(p_actor) = '' THEN
    RAISE EXCEPTION 'parking a device requires an actor: somebody owns this decision'
      USING ERRCODE = 'check_violation';
  END IF;
  IF p_reason IS NULL OR btrim(p_reason) = '' THEN
    RAISE EXCEPTION 'parking a device requires a reason: without one it is indistinguishable from a fault'
      USING ERRCODE = 'check_violation';
  END IF;

  SELECT d.admin_state INTO v_state
    FROM farm.devices d WHERE d.id = p_device FOR NO KEY UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'no such device %', p_device USING ERRCODE = 'no_data_found';
  END IF;
  IF v_state = 'parked' THEN
    RAISE EXCEPTION 'device % is already parked', p_device USING ERRCODE = 'unique_violation';
  END IF;
  -- Parking a retired or quarantined device would overwrite the reason it
  -- is already out of service with a weaker one, and unparking would then
  -- hand it back as 'enabled'. Refuse: parking is for a device that would
  -- otherwise be schedulable.
  IF v_state <> 'enabled' THEN
    RAISE EXCEPTION 'device % is %, not enabled; parking would overwrite that', p_device, v_state
      USING ERRCODE = 'check_violation';
  END IF;

  -- Ledger first, then the authority, then the mirror. Guard A reads the
  -- ledger and Guard B reads the authority, so this is the one order in
  -- which each guard sees a world that is already consistent.
  INSERT INTO farm.device_parks (device_id, reason, opened_by, auto)
  VALUES (p_device, btrim(p_reason), btrim(p_actor), p_auto)
  RETURNING id INTO v_park;

  UPDATE farm.devices SET admin_state = 'parked', updated_at = now()
   WHERE id = p_device;

  -- A device enrolled but never observed has no runtime row yet, so this
  -- is an upsert rather than an update: the mirror must exist even for a
  -- device the watchdog has not reached, or the fleet grid would call a
  -- parked handset 'unknown' the moment the first snapshot creates its row.
  INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health)
  SELECT d.id, d.host_id, d.current_slot_id, 'unknown', 'parked'
    FROM farm.devices d WHERE d.id = p_device
  ON CONFLICT (device_id) DO UPDATE
     SET health = 'parked', health_since = now(), updated_at = now();

  -- NOTE what is absent from this function: farm.leases. Not a read, not
  -- a write, not a count. The role it runs as could not do any of the
  -- three if a future edit tried.

  INSERT INTO farm.audit_log (actor, action, subject, reason, detail)
  VALUES (btrim(p_actor), 'device.park', 'device:' || p_device::text, btrim(p_reason),
          jsonb_build_object('park_id', v_park, 'auto', p_auto));

  INSERT INTO farm.events (kind, device_id, actor, detail)
  VALUES ('device_parked', p_device, btrim(p_actor),
          jsonb_build_object('park_id', v_park, 'auto', p_auto, 'reason', btrim(p_reason)));

  RETURN v_park;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON FUNCTION farm.device_park(uuid, text, text, boolean) IS
  'Take a device out of service on purpose. Stops allocation and recovery; never touches its lease.';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.device_unpark(
  p_device uuid,
  p_actor  text,
  p_reason text    DEFAULT NULL,
  p_auto   boolean DEFAULT false
) RETURNS bigint
LANGUAGE plpgsql
SET role = farm_parker
AS $fn$
DECLARE
  v_park farm.device_parks%ROWTYPE;
BEGIN
  IF p_actor IS NULL OR btrim(p_actor) = '' THEN
    RAISE EXCEPTION 'unparking a device requires an actor' USING ERRCODE = 'check_violation';
  END IF;

  SELECT * INTO v_park FROM farm.device_parks p
   WHERE p.device_id = p_device AND p.closed_at IS NULL
   FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'device % is not parked', p_device USING ERRCODE = 'no_data_found';
  END IF;

  -- THE RULE, copied from internal/topo/discover.go restore(): automation
  -- reverses only its OWN decision. restore() reactivates a slot only when
  -- the most recent audit row for it is discovery's own retirement, so a
  -- human who put a port into maintenance keeps the last word. The same
  -- sentence here: a caller that declares itself automation may close only
  -- a park that was opened automatically, by the same actor. Anything else
  -- — a human's park, or another loop's — needs a person.
  IF p_auto AND NOT (v_park.auto AND v_park.opened_by = btrim(p_actor)) THEN
    RAISE EXCEPTION 'automation % may not reverse a park opened by % (auto=%)',
                    btrim(p_actor), v_park.opened_by, v_park.auto
      USING ERRCODE = 'insufficient_privilege',
            HINT    = 'automation reverses only its own park; this one needs a human';
  END IF;

  -- Ledger first: Guard A refuses to let admin_state leave 'parked' while
  -- an open park row exists, and this is the statement that closes it.
  UPDATE farm.device_parks
     SET closed_at    = now(),
         closed_by    = btrim(p_actor),
         close_reason = nullif(btrim(coalesce(p_reason, '')), '')
   WHERE id = v_park.id;

  UPDATE farm.devices SET admin_state = 'enabled', updated_at = now()
   WHERE id = p_device AND admin_state = 'parked';

  -- Back to 'unknown', never to 'healthy'. Unparking is a human saying
  -- "look at it again", not a probe: the device may have been off the wire
  -- for hours and nobody has observed it since. The counters and the ladder
  -- rung reset for the same reason a closed quarantine resets them — the
  -- next decision must be made on fresh evidence, not on what was true
  -- before the hold started.
  UPDATE farm.device_runtime
     SET health = 'unknown', health_since = now(),
         consec_bad = 0, consec_good = 0, ladder_tier = 0, updated_at = now()
   WHERE device_id = p_device AND health = 'parked';

  INSERT INTO farm.audit_log (actor, action, subject, reason, detail)
  VALUES (btrim(p_actor), 'device.unpark', 'device:' || p_device::text,
          nullif(btrim(coalesce(p_reason, '')), ''),
          jsonb_build_object('park_id', v_park.id, 'auto', p_auto,
                             'opened_by', v_park.opened_by,
                             'parked_for_seconds',
                             EXTRACT(EPOCH FROM (now() - v_park.opened_at))));

  INSERT INTO farm.events (kind, device_id, actor, detail)
  VALUES ('device_unparked', p_device, btrim(p_actor),
          jsonb_build_object('park_id', v_park.id, 'auto', p_auto,
                             'opened_by', v_park.opened_by));

  RETURN v_park.id;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON FUNCTION farm.device_unpark(uuid, text, text, boolean) IS
  'Put a parked device back in service. p_auto => true may close only a park the same automated actor opened.';
-- +goose StatementEnd

-- ---------------------------------------------------------------------
-- 6. The correlation banner must not call a deliberate hold a fault.
--
--    v_hub_health counts everything that is not healthy or retired as
--    "unhealthy". Charge-limit four handsets on one hub and an operator
--    would be shown a hub shedding half its devices — the exact alert
--    this state exists to prevent, on the exact screen they stare at
--    during an incident.
--
--    The ladder's own quorum (internal/recovery/ladder.go,
--    unhealthyPredicate) is an allow-list of positive fault evidence and
--    was already safe. This view was not.
-- ---------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE VIEW farm.v_hub_health AS
SELECT h.id                AS hub_id,
       h.host_id,
       h.usb_path,
       h.model,
       h.vbus_switchable,
       count(d.id)                                                            AS devices,
       count(*) FILTER (WHERE r.health = 'healthy')                           AS healthy,
       count(*) FILTER (WHERE r.health NOT IN ('healthy','retired','parked')) AS unhealthy,
       max(r.health_since) FILTER (WHERE r.health NOT IN ('healthy','parked')) AS worst_since
  FROM farm.hubs h
  LEFT JOIN farm.slots s          ON s.hub_id = h.id
  LEFT JOIN farm.devices d        ON d.current_slot_id = s.id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
 GROUP BY h.id, h.host_id, h.usb_path, h.model, h.vbus_switchable;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS device_runtime_park_guard ON farm.device_runtime;
DROP TRIGGER IF EXISTS devices_park_guard ON farm.devices;
DROP FUNCTION IF EXISTS farm.trg_device_runtime_park_guard();
DROP FUNCTION IF EXISTS farm.trg_devices_park_guard();
DROP FUNCTION IF EXISTS farm.device_unpark(uuid, text, text, boolean);
DROP FUNCTION IF EXISTS farm.device_park(uuid, text, text, boolean);

-- Any device still parked has to land on a value the narrowed CHECK
-- accepts, or the ALTER below fails on rows nobody looked at. 'disabled'
-- is the honest destination: it is what 'parked' specialised, and it
-- still keeps the allocator away.
--
-- The open ledger rows must be CLOSED here, not merely left behind.
-- After a down-and-up cycle an open row would describe a park that no
-- longer exists on either plane, and the device would be stranded:
-- device_park refuses it because it is 'disabled', and device_unpark
-- closes the row and then updates WHERE admin_state = 'parked', which
-- matches nothing — reporting success while the device stays disabled
-- forever. Closing them says what actually happened. The rows
-- themselves stay: rolling back the mechanism is a change of plan,
-- deleting the trail it wrote is an incident.
UPDATE farm.device_parks
   SET closed_at    = now(),
       closed_by    = 'migration:00008_parked down',
       close_reason = 'the parked state was rolled back; the device was left disabled'
 WHERE closed_at IS NULL;
UPDATE farm.device_runtime SET health = 'unknown', health_since = now()
 WHERE health = 'parked';
UPDATE farm.devices SET admin_state = 'disabled', updated_at = now()
 WHERE admin_state = 'parked';

ALTER TABLE farm.device_runtime DROP CONSTRAINT IF EXISTS device_runtime_health_check;
ALTER TABLE farm.device_runtime ADD CONSTRAINT device_runtime_health_check
  CHECK (health IN ('unknown','booting','healthy','degraded','offline','unauthorized',
                    'missing','recovering','quarantined','retired'));

ALTER TABLE farm.devices DROP CONSTRAINT IF EXISTS devices_admin_state_check;
ALTER TABLE farm.devices ADD CONSTRAINT devices_admin_state_check
  CHECK (admin_state IN ('enabled','disabled','quarantined','retired'));

CREATE OR REPLACE VIEW farm.v_hub_health AS
SELECT h.id                AS hub_id,
       h.host_id,
       h.usb_path,
       h.model,
       h.vbus_switchable,
       count(d.id)                                              AS devices,
       count(*) FILTER (WHERE r.health = 'healthy')             AS healthy,
       count(*) FILTER (WHERE r.health NOT IN ('healthy','retired')) AS unhealthy,
       max(r.health_since) FILTER (WHERE r.health <> 'healthy')  AS worst_since
  FROM farm.hubs h
  LEFT JOIN farm.slots s          ON s.hub_id = h.id
  LEFT JOIN farm.devices d        ON d.current_slot_id = s.id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
 GROUP BY h.id, h.host_id, h.usb_path, h.model, h.vbus_switchable;

-- farm.device_parks is NOT dropped, and the closed rows above are why.
-- Rolling back the mechanism is a change of plan; deleting the record of
-- which handsets a human took out of service, and why, is an incident.
-- The Up path creates it IF NOT EXISTS, so a re-apply finds the history
-- intact, and the partial unique index only covers OPEN rows, so a
-- device with a closed park from a previous cycle can be parked again.
-- Same choice, for the same reason, as the 'lease_ended' rows in 00007.
--
-- The role is left in place too. Dropping a role that still holds grants
-- elsewhere fails, and a NOLOGIN role with no privileges is inert.
-- +goose StatementEnd
