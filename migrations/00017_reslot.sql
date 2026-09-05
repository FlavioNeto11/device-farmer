-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- An operator surface for where a device sits, and what a slot is called.
--
-- Until now the only thing that could move farm.devices.current_slot_id was
-- farm.resolve_device, as a side effect of a host seeing the device somewhere
-- new. That is the right default — the fleet follows the cable — but it left
-- three ordinary situations with no answer short of hand-written SQL:
--
--   * a phone was re-cabled and the host that would notice is down, so its
--     recovery actions keep aiming at the socket it used to be in;
--   * two rows contest one phone (the case enroll.Brander refuses to resolve
--     on its own) and a human has decided which row is the phone, so the
--     other has to be taken out of the slot before anything can be fixed;
--   * a rack was relabelled and the label an alert prints sends the operator
--     to the wrong shelf.
--
-- The functions below are deliberately small and deliberately strict. A
-- re-slot changes where every recovery action, power cycle and exec for this
-- device will land, so it is refused while a lease is live: a job three hours
-- in must not have its device's address changed under it, however sure the
-- operator is. Nothing here ends a lease, and nothing here can — the lease
-- columns are not touched, and the one write that could make a lease vanish
-- (a device row deletion) is not offered.
--
-- Both functions write farm.audit_log and farm.events in the same
-- transaction as the change, so the record of who moved what lands whole or
-- not at all.
-- =====================================================================
-- +goose StatementEnd

-- +goose StatementBegin
-- Occupancy bookkeeping, shared. This is the tail of farm.resolve_device
-- (00004:360-368, carried forward unchanged through 00011) as a function of
-- its own, so a re-slot writes the same history the enrolment loop writes and
-- the slot_occupancy table has one author's idea of what "moved" means.
--
-- resolve_device keeps its inline copy rather than calling this. Its body is
-- redefined by several migrations already and is the identity ladder, not
-- occupancy; a migration that owns the ladder can switch it over when it next
-- rewrites the function, and until then the two are kept textually identical.
CREATE OR REPLACE FUNCTION farm.slot_occupy(p_slot bigint, p_device uuid) RETURNS void
LANGUAGE plpgsql AS $fn$
BEGIN
  UPDATE farm.slot_occupancy o SET until = now(), reason = 'device moved'
   WHERE o.until IS NULL AND o.device_id = p_device AND o.slot_id <> p_slot;
  UPDATE farm.slot_occupancy o SET until = now(), reason = 'slot reoccupied'
   WHERE o.until IS NULL AND o.slot_id = p_slot AND o.device_id <> p_device;
  INSERT INTO farm.slot_occupancy (slot_id, device_id)
  SELECT p_slot, p_device
   WHERE NOT EXISTS (SELECT 1 FROM farm.slot_occupancy o
                      WHERE o.until IS NULL AND o.slot_id = p_slot AND o.device_id = p_device);
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Move a device to a slot, or with p_slot NULL take it out of its slot.
--
-- Refusals, each with a SQLSTATE the API turns into 409:
--   object_in_use (55006)                    the device holds a live lease, or
--                                            another device holds the slot
--   object_not_in_prerequisite_state (55000) the slot is not active, or is on
--                                            a different host than the device
--
-- The host check is what keeps a device row per host. A phone carried to
-- another host is adopted there by that host's own enrolment; moving the row
-- by hand would put a device on host B with a devpath that only means
-- something on host A.
--
-- Unslotting exists for the contested-identity case: a row that resolution
-- keeps putting in a slot the operator has decided belongs to a different
-- row cannot be displaced any other way, because devices_one_per_slot refuses
-- two rows in one slot and this function refuses to overwrite an occupant.
CREATE OR REPLACE FUNCTION farm.reslot_device(
  p_device uuid,
  p_slot   bigint,
  p_actor  text,
  p_reason text
) RETURNS void
LANGUAGE plpgsql AS $fn$
DECLARE
  d          farm.devices%ROWTYPE;
  s          farm.slots%ROWTYPE;
  v_from     bigint;
  v_occupant uuid;
  v_detail   jsonb;
BEGIN
  IF p_actor IS NULL OR btrim(p_actor) = '' THEN
    RAISE EXCEPTION 'reslot_device: an actor is required' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF p_reason IS NULL OR btrim(p_reason) = '' THEN
    RAISE EXCEPTION 'reslot_device: a reason is required' USING ERRCODE = 'invalid_parameter_value';
  END IF;

  -- The device row is locked first. farm.lease_acquire ends by updating this
  -- row's current_lease_id through the lease trigger, so a grant racing this
  -- call waits here and then sees the device in its new slot, rather than
  -- landing a lease on a device whose address is about to change.
  SELECT * INTO d FROM farm.devices WHERE id = p_device FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'no device %', p_device USING ERRCODE = 'no_data_found';
  END IF;
  IF d.current_lease_id IS NOT NULL THEN
    RAISE EXCEPTION 'device % holds live lease %; its slot cannot change while a job is using it',
      p_device, d.current_lease_id USING ERRCODE = 'object_in_use';
  END IF;
  v_from := d.current_slot_id;

  IF p_slot IS NOT NULL THEN
    SELECT * INTO s FROM farm.slots WHERE id = p_slot FOR UPDATE;
    IF NOT FOUND THEN
      RAISE EXCEPTION 'no slot %', p_slot USING ERRCODE = 'no_data_found';
    END IF;
    IF s.state <> 'active' THEN
      RAISE EXCEPTION 'slot % is %, not active', p_slot, s.state
        USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF d.host_id IS NOT NULL AND d.host_id <> s.host_id THEN
      RAISE EXCEPTION 'slot % is on host %, but device % belongs to host %; a device row never changes host by hand',
        p_slot, s.host_id, p_device, d.host_id USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    SELECT o.id INTO v_occupant FROM farm.devices o
     WHERE o.current_slot_id = p_slot AND o.id <> p_device;
    IF v_occupant IS NOT NULL THEN
      RAISE EXCEPTION 'slot % is occupied by device %', p_slot, v_occupant
        USING ERRCODE = 'object_in_use';
    END IF;

    UPDATE farm.devices
       SET current_slot_id = p_slot, host_id = s.host_id, updated_at = now()
     WHERE id = p_device;
    UPDATE farm.device_runtime
       SET slot_id = p_slot, host_id = s.host_id, updated_at = now()
     WHERE device_id = p_device;
    PERFORM farm.slot_occupy(p_slot, p_device);
  ELSE
    UPDATE farm.devices SET current_slot_id = NULL, updated_at = now() WHERE id = p_device;
    UPDATE farm.device_runtime SET slot_id = NULL, updated_at = now() WHERE device_id = p_device;
    UPDATE farm.slot_occupancy o SET until = now(), reason = 'device unslotted'
     WHERE o.until IS NULL AND o.device_id = p_device;
  END IF;

  v_detail := jsonb_build_object(
    'device_id',    p_device,
    'farm_uid',     d.farm_uid,
    'host_id',      COALESCE(s.host_id, d.host_id),
    'from_slot_id', v_from,
    'to_slot_id',   p_slot,
    'to_usb_path',  s.usb_path,
    'to_rack_slot', s.rack_slot,
    'moved',        v_from IS DISTINCT FROM p_slot);

  INSERT INTO farm.audit_log (actor, action, subject, reason, detail)
  VALUES (p_actor, 'device.reslot', 'device:' || p_device, p_reason, v_detail);
  INSERT INTO farm.events (kind, device_id, slot_id, actor, detail)
  VALUES ('device_reslotted', p_device, p_slot, p_actor, v_detail);
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Rename a slot's human-facing label. An empty or NULL label clears it: this
-- is the explicit path, unlike register_slot, where NULL means "not stated".
--
-- Two slots wearing one label is refused (unique_violation, 23505), because a
-- label that names two sockets sends an operator confidently to the wrong
-- one. There is no unique index on rack_slot — topology discovery labels a
-- whole host at once and checks its own plan — so the rule lives here, in the
-- one place a single label is written by hand.
CREATE OR REPLACE FUNCTION farm.relabel_slot(
  p_slot      bigint,
  p_rack_slot text,
  p_actor     text,
  p_reason    text
) RETURNS void
LANGUAGE plpgsql AS $fn$
DECLARE
  s       farm.slots%ROWTYPE;
  v_new   text;
  v_other bigint;
  v_detail jsonb;
BEGIN
  IF p_actor IS NULL OR btrim(p_actor) = '' THEN
    RAISE EXCEPTION 'relabel_slot: an actor is required' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  IF p_reason IS NULL OR btrim(p_reason) = '' THEN
    RAISE EXCEPTION 'relabel_slot: a reason is required' USING ERRCODE = 'invalid_parameter_value';
  END IF;

  SELECT * INTO s FROM farm.slots WHERE id = p_slot FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'no slot %', p_slot USING ERRCODE = 'no_data_found';
  END IF;

  v_new := nullif(btrim(p_rack_slot), '');
  IF v_new IS NOT NULL THEN
    SELECT o.id INTO v_other FROM farm.slots o WHERE o.rack_slot = v_new AND o.id <> p_slot;
    IF v_other IS NOT NULL THEN
      RAISE EXCEPTION 'slot % already carries the label %; a label must name one socket', v_other, v_new
        USING ERRCODE = 'unique_violation';
    END IF;
  END IF;

  UPDATE farm.slots SET rack_slot = v_new WHERE id = p_slot;

  v_detail := jsonb_build_object(
    'slot_id',            p_slot,
    'host_id',            s.host_id,
    'usb_path',           s.usb_path,
    'previous_rack_slot', s.rack_slot,
    'rack_slot',          v_new);

  INSERT INTO farm.audit_log (actor, action, subject, reason, detail)
  VALUES (p_actor, 'slot.relabel', 'slot:' || p_slot, p_reason, v_detail);
  INSERT INTO farm.events (kind, slot_id, actor, detail)
  VALUES ('slot_relabelled', p_slot, p_actor, v_detail);
END $fn$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS farm.relabel_slot(bigint, text, text, text);
DROP FUNCTION IF EXISTS farm.reslot_device(uuid, bigint, text, text);
DROP FUNCTION IF EXISTS farm.slot_occupy(bigint, uuid);
-- +goose StatementEnd
