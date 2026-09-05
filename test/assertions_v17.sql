-- Assertions for migration 00017_reslot.sql: the operator surface for slots.
--
-- Two functions are under test. farm.reslot_device moves a device to a slot
-- by hand, which changes where every recovery action for that device will
-- land; farm.relabel_slot renames the label an alert prints. The assertions
-- that matter most are the refusals: a device holding a live lease keeps its
-- address, however sure the operator is, and a slot another device holds is
-- never overwritten. The success case is checked for the thing that is easy
-- to get half-right — occupancy history must read the same whether the
-- enrolment loop or a human moved the device.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v17.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host with four slots registered THROUGH farm.register_slot,
-- because that is the path the API's register route takes; a second host
-- with one slot, for the cross-host refusal; two devices adopted through
-- farm.resolve_device, so their occupancy rows were written by the same
-- code the re-slot has to agree with.
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r17');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h17','r17','127.0.0.1:5037');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h17b','r17','127.0.0.1:5038');
INSERT INTO farm.pools (id) VALUES ('default') ON CONFLICT (id) DO NOTHING;
INSERT INTO farm.tenants (id) VALUES ('acme17');
INSERT INTO farm.queues (id, tenant_id) VALUES ('q17','acme17');

SELECT farm.register_slot('h17', '3-1.' || g, '3-1', g, 'Test Hub', 7, true, 'R17-U1-H1-P' || g)
  FROM generate_series(1, 4) g;
SELECT farm.register_slot('h17b', '3-1.1', '3-1', 1, 'Test Hub', 7, true, 'R17-U2-H1-P1');

-- A long job, so the lease it gets is the protected six-hour kind: the one a
-- re-slot must never disturb.
INSERT INTO farm.jobs (id, tenant_id, queue_id, pool_id, expected_duration)
VALUES ('17171717-0000-0000-0000-000000000001','acme17','q17','default', interval '6 hours');

-- --------------------------------------------------------------------
-- Assertions
-- --------------------------------------------------------------------
DO $$
DECLARE
  r         record;
  dev1      uuid;
  dev2      uuid;
  slot1     bigint;
  slot2     bigint;
  slot3     bigint;
  slot4     bigint;
  slot_b    bigint;
  v_lease   uuid;
  v_fence   bigint;
  v_slot    bigint;
  v_cnt     int;
  v_state   text;
  v_label   text;
  v_prev    text;
  v_reason  text;
  v_detail  jsonb;
BEGIN
  SELECT id INTO slot1  FROM farm.slots WHERE host_id = 'h17'  AND usb_path = '3-1.1';
  SELECT id INTO slot2  FROM farm.slots WHERE host_id = 'h17'  AND usb_path = '3-1.2';
  SELECT id INTO slot3  FROM farm.slots WHERE host_id = 'h17'  AND usb_path = '3-1.3';
  SELECT id INTO slot4  FROM farm.slots WHERE host_id = 'h17'  AND usb_path = '3-1.4';
  SELECT id INTO slot_b FROM farm.slots WHERE host_id = 'h17b' AND usb_path = '3-1.1';

  SELECT * INTO r FROM farm.resolve_device('h17', '3-1.1', NULL, NULL, 'SER-17-A');
  dev1 := r.device_id;
  SELECT * INTO r FROM farm.resolve_device('h17', '3-1.2', NULL, NULL, 'SER-17-B');
  dev2 := r.device_id;
  UPDATE farm.device_runtime SET adb_state = 'device', health = 'healthy'
   WHERE device_id IN (dev1, dev2);

  UPDATE farm.jobs SET pin_device = dev1 WHERE id = '17171717-0000-0000-0000-000000000001';
  SELECT lease_id, fence INTO v_lease, v_fence FROM farm.lease_acquire(
    '17171717-0000-0000-0000-000000000001', 'runner-pod-17', gen_random_uuid());
  IF v_lease IS NULL THEN
    RAISE EXCEPTION 'FIXTURE FAILED: could not acquire the lease the invariant is about';
  END IF;

  -- ============================================================
  -- R1  THE INVARIANT. A device holding a live lease keeps its
  --     slot. Nothing about the lease changes either: not its
  --     state, not its fence, not the device's pointer to it. And
  --     a refusal leaves no audit row claiming a move happened.
  -- ============================================================
  BEGIN
    PERFORM farm.reslot_device(dev1, slot3, 'alice', 'R1: must be refused');
    RAISE EXCEPTION 'R1 FAILED: a leased device was re-slotted';
  EXCEPTION WHEN object_in_use THEN
    NULL;
  END;
  SELECT current_slot_id INTO v_slot FROM farm.devices WHERE id = dev1;
  IF v_slot <> slot1 THEN
    RAISE EXCEPTION 'R1 FAILED: the refusal still moved the device to slot %', v_slot;
  END IF;
  SELECT * INTO r FROM farm.leases WHERE id = v_lease;
  IF r.state <> 'held' OR r.fence <> v_fence OR r.released_at IS NOT NULL THEN
    RAISE EXCEPTION 'R1 FAILED: the refusal touched the lease (state %, fence % -> %)',
      r.state, v_fence, r.fence;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.audit_log
   WHERE action = 'device.reslot' AND subject = 'device:' || dev1;
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'R1 FAILED: a refused re-slot wrote % audit row(s)', v_cnt;
  END IF;
  RAISE NOTICE 'R1  ok  a leased device keeps its slot, and its lease is untouched';

  -- ============================================================
  -- R2  A slot another device holds is never overwritten. The
  --     schema's devices_one_per_slot would refuse the UPDATE
  --     anyway; this refusal says WHICH device is in the way
  --     instead of failing on an index name.
  -- ============================================================
  BEGIN
    PERFORM farm.reslot_device(dev2, slot1, 'alice', 'R2: must be refused');
    RAISE EXCEPTION 'R2 FAILED: a device was re-slotted onto an occupied slot';
  EXCEPTION WHEN object_in_use THEN
    GET STACKED DIAGNOSTICS v_reason = MESSAGE_TEXT;
  END;
  IF position(dev1::text IN v_reason) = 0 THEN
    RAISE EXCEPTION 'R2 FAILED: the refusal does not name the occupant: %', v_reason;
  END IF;
  SELECT current_slot_id INTO v_slot FROM farm.devices WHERE id = dev2;
  IF v_slot <> slot2 THEN
    RAISE EXCEPTION 'R2 FAILED: the refusal still moved the device';
  END IF;
  RAISE NOTICE 'R2  ok  an occupied slot is refused, naming the occupant';

  -- ============================================================
  -- R3  A slot that is not active, and a slot on another host,
  --     are both refused. A device row belongs to one host; the
  --     host that sees a re-cabled phone adopts it itself.
  -- ============================================================
  UPDATE farm.slots SET state = 'disabled' WHERE id = slot4;
  BEGIN
    PERFORM farm.reslot_device(dev2, slot4, 'alice', 'R3: must be refused');
    RAISE EXCEPTION 'R3 FAILED: a device was re-slotted into a disabled slot';
  EXCEPTION WHEN object_not_in_prerequisite_state THEN
    NULL;
  END;
  BEGIN
    PERFORM farm.reslot_device(dev2, slot_b, 'alice', 'R3: must be refused');
    RAISE EXCEPTION 'R3 FAILED: a device was re-slotted onto another host';
  EXCEPTION WHEN object_not_in_prerequisite_state THEN
    NULL;
  END;
  RAISE NOTICE 'R3  ok  a disabled slot and a slot on another host are refused';

  -- ============================================================
  -- R4  THE MOVE. A free device goes to a free, active slot, and
  --     every table that says where a device is agrees: the
  --     device row, the runtime row, and the occupancy history —
  --     which must read exactly as if resolve_device had seen the
  --     device there, because the same code writes both.
  -- ============================================================
  PERFORM farm.reslot_device(dev2, slot3, 'alice', 'R4: re-cabled to port 3');

  SELECT current_slot_id INTO v_slot FROM farm.devices WHERE id = dev2;
  IF v_slot <> slot3 THEN
    RAISE EXCEPTION 'R4 FAILED: devices.current_slot_id is %, want %', v_slot, slot3;
  END IF;
  SELECT slot_id INTO v_slot FROM farm.device_runtime WHERE device_id = dev2;
  IF v_slot <> slot3 THEN
    RAISE EXCEPTION 'R4 FAILED: device_runtime.slot_id is %, want %', v_slot, slot3;
  END IF;
  -- The old slot reads empty.
  SELECT count(*) INTO v_cnt FROM farm.slot_occupancy WHERE slot_id = slot2 AND until IS NULL;
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'R4 FAILED: slot % still has % open occupancy row(s)', slot2, v_cnt;
  END IF;
  SELECT reason INTO v_reason FROM farm.slot_occupancy
   WHERE slot_id = slot2 AND device_id = dev2 AND until IS NOT NULL;
  IF v_reason IS DISTINCT FROM 'device moved' THEN
    RAISE EXCEPTION 'R4 FAILED: the closed tenancy reads %, not the reason resolve_device writes', v_reason;
  END IF;
  -- The new slot holds the device, and only the device.
  SELECT count(*) INTO v_cnt FROM farm.slot_occupancy WHERE slot_id = slot3 AND until IS NULL;
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'R4 FAILED: slot % has % open occupancy row(s), want 1', slot3, v_cnt;
  END IF;
  PERFORM 1 FROM farm.slot_occupancy WHERE slot_id = slot3 AND device_id = dev2 AND until IS NULL;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'R4 FAILED: the open occupancy of slot % is not device %', slot3, dev2;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.slot_occupancy WHERE device_id = dev2 AND until IS NULL;
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'R4 FAILED: device % has % open tenancies, want 1', dev2, v_cnt;
  END IF;
  -- And the fleet view agrees with the tables.
  SELECT slot_id INTO v_slot FROM farm.v_fleet WHERE device_id = dev2;
  IF v_slot <> slot3 THEN
    RAISE EXCEPTION 'R4 FAILED: v_fleet still places the device in slot %', v_slot;
  END IF;
  RAISE NOTICE 'R4  ok  the move is consistent across devices, runtime, occupancy and v_fleet';

  -- ============================================================
  -- R5  The move is audited and on the timeline, in the same
  --     transaction, with the operator's name and both slots.
  -- ============================================================
  SELECT actor, reason, detail INTO r FROM farm.audit_log
   WHERE action = 'device.reslot' AND subject = 'device:' || dev2
   ORDER BY id DESC LIMIT 1;
  IF r.actor IS DISTINCT FROM 'alice' OR r.reason IS DISTINCT FROM 'R4: re-cabled to port 3' THEN
    RAISE EXCEPTION 'R5 FAILED: the audit row names % / %', r.actor, r.reason;
  END IF;
  IF (r.detail->>'from_slot_id')::bigint <> slot2 OR (r.detail->>'to_slot_id')::bigint <> slot3 THEN
    RAISE EXCEPTION 'R5 FAILED: the audit detail does not carry both slots: %', r.detail;
  END IF;
  PERFORM 1 FROM farm.events
   WHERE kind = 'device_reslotted' AND device_id = dev2 AND slot_id = slot3 AND actor = 'alice';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'R5 FAILED: no device_reslotted event for the move';
  END IF;
  RAISE NOTICE 'R5  ok  the move is audited and on the timeline';

  -- ============================================================
  -- R6  Unslotting. A row that resolution keeps placing in a slot
  --     the operator has assigned to a different row can only be
  --     displaced by taking it out; its open tenancy closes and
  --     the slot is then free for the other row.
  -- ============================================================
  PERFORM farm.reslot_device(dev2, NULL, 'alice', 'R6: this row is not the phone in port 3');
  SELECT current_slot_id INTO v_slot FROM farm.devices WHERE id = dev2;
  IF v_slot IS NOT NULL THEN
    RAISE EXCEPTION 'R6 FAILED: the device is still in slot %', v_slot;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.slot_occupancy WHERE device_id = dev2 AND until IS NULL;
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'R6 FAILED: an unslotted device still has % open tenancies', v_cnt;
  END IF;
  -- Put it back, through the same function, so the fixture is whole again.
  PERFORM farm.reslot_device(dev2, slot3, 'alice', 'R6: and back');
  RAISE NOTICE 'R6  ok  a device can be taken out of its slot, and put back';

  -- ============================================================
  -- R7  Relabel writes the audit row, with the label it replaced.
  -- ============================================================
  SELECT rack_slot INTO v_prev FROM farm.slots WHERE id = slot3;
  PERFORM farm.relabel_slot(slot3, '  R17-BENCH-3  ', 'alice', 'R7: shelf relabelled');
  SELECT rack_slot INTO v_label FROM farm.slots WHERE id = slot3;
  IF v_label IS DISTINCT FROM 'R17-BENCH-3' THEN
    RAISE EXCEPTION 'R7 FAILED: the slot reads %, want the trimmed label', v_label;
  END IF;
  SELECT actor, reason, detail INTO r FROM farm.audit_log
   WHERE action = 'slot.relabel' AND subject = 'slot:' || slot3
   ORDER BY id DESC LIMIT 1;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'R7 FAILED: relabel wrote no audit row';
  END IF;
  IF r.actor <> 'alice' OR r.reason <> 'R7: shelf relabelled' THEN
    RAISE EXCEPTION 'R7 FAILED: the audit row names % / %', r.actor, r.reason;
  END IF;
  IF r.detail->>'previous_rack_slot' IS DISTINCT FROM v_prev
     OR r.detail->>'rack_slot' IS DISTINCT FROM 'R17-BENCH-3' THEN
    RAISE EXCEPTION 'R7 FAILED: the audit detail does not carry old and new: %', r.detail;
  END IF;
  PERFORM 1 FROM farm.events WHERE kind = 'slot_relabelled' AND slot_id = slot3 AND actor = 'alice';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'R7 FAILED: no slot_relabelled event';
  END IF;
  RAISE NOTICE 'R7  ok  relabel is audited with the label it replaced';

  -- ============================================================
  -- R8  A label names one socket. Giving slot 2 the label slot 3
  --     now wears is refused; clearing a label is allowed and is
  --     the explicit way to remove one.
  -- ============================================================
  BEGIN
    PERFORM farm.relabel_slot(slot2, 'R17-BENCH-3', 'alice', 'R8: must be refused');
    RAISE EXCEPTION 'R8 FAILED: two slots now carry one label';
  EXCEPTION WHEN unique_violation THEN
    NULL;
  END;
  PERFORM farm.relabel_slot(slot2, '', 'alice', 'R8: label removed');
  SELECT rack_slot INTO v_label FROM farm.slots WHERE id = slot2;
  IF v_label IS NOT NULL THEN
    RAISE EXCEPTION 'R8 FAILED: an empty relabel left the label %', v_label;
  END IF;
  RAISE NOTICE 'R8  ok  duplicate labels are refused; an empty label clears';

  -- ============================================================
  -- R9  Neither function accepts an unsigned action. The reason
  --     is the only record, weeks later, of why a device's
  --     address changed.
  -- ============================================================
  BEGIN
    PERFORM farm.reslot_device(dev2, slot2, 'alice', '   ');
    RAISE EXCEPTION 'R9 FAILED: a re-slot with no reason was accepted';
  EXCEPTION WHEN invalid_parameter_value THEN
    NULL;
  END;
  BEGIN
    PERFORM farm.relabel_slot(slot2, 'X', '', 'R9');
    RAISE EXCEPTION 'R9 FAILED: a relabel with no actor was accepted';
  EXCEPTION WHEN invalid_parameter_value THEN
    NULL;
  END;
  RAISE NOTICE 'R9  ok  an actor and a reason are both required';

  -- ============================================================
  -- R10 NO LEASE WAS TOUCHED. Everything above is bookkeeping
  --     about where a device sits and what a socket is called. A
  --     lease ends when the job says so, when a user-written
  --     deadline elapses, or when a human takes it back — and
  --     nothing in this file is any of those.
  -- ============================================================
  SELECT * INTO r FROM farm.leases WHERE id = v_lease;
  IF r.state <> 'held' OR r.fence <> v_fence OR r.released_at IS NOT NULL THEN
    RAISE EXCEPTION 'R10 FAILED: the lease changed (state %, fence % -> %, released %)',
      r.state, v_fence, r.fence, r.released_at;
  END IF;
  SELECT current_lease_id INTO v_lease FROM farm.devices WHERE id = dev1;
  IF v_lease IS DISTINCT FROM r.id THEN
    RAISE EXCEPTION 'R10 FAILED: the device lost its pointer to the live lease';
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.leases l JOIN farm.devices d ON d.id = l.device_id
   WHERE d.host_id IN ('h17', 'h17b');
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'R10 FAILED: expected exactly the one fixture lease, found %', v_cnt;
  END IF;
  RAISE NOTICE 'R10 ok  no lease was touched';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v17 ASSERTIONS PASSED';
END $$;

ROLLBACK;
