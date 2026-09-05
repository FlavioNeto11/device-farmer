-- Quarantine-scope assertions for migration 00014_quarantine_power_domain.sql.
--
-- The defect these encode: farm.quarantines.scope permitted 'slot' and
-- 'power_domain' from the start, and nothing could write a power-domain
-- row because the table had no column to name the domain. Every predicate
-- that asks "does an open quarantine cover this device?" carried a fallback
-- arm for a row shape that could not exist, and farm.v_fleet's arm compared
-- q.slot_id to the device's slot — which, for a row with no slot_id, is
-- NULL IS NOT DISTINCT FROM s.id: false for every slotted device and TRUE
-- for every unslotted one. On mixed hardware the choice was one device or
-- the whole hub.
--
-- The coverage predicate here mirrors internal/recovery coveredByQuarantine
-- and the close handler in internal/api arm for arm; the fidelity tests in
-- internal/recovery hold the Go copies to the schema's scope list, and the
-- DB-backed Go tests drive the ladder's candidate query and the HTTP
-- handlers against the same topology. This file is the schema's own word.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v14.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host, one hub, six slots, six healthy devices.
--
--   slots 1-3  share power domain A (ganged: one switch, three ports)
--   slots 4-6  sit on power domain B (per-port)
--   dev7       enrolled but in no slot — the old v_fleet arm's victim
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r14');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h14','r14','127.0.0.1:5037');
INSERT INTO farm.controllers (host_id, root_bus) VALUES ('h14', 3);
INSERT INTO farm.power_domains (host_id, kind, control, notes)
  VALUES ('h14','ganged','uhubctl','A'), ('h14','per_port','uhubctl','B');
INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
  SELECT 'h14', c.id, '3-1', 7, true FROM farm.controllers c WHERE c.host_id='h14';
INSERT INTO farm.pools (id) VALUES ('default') ON CONFLICT (id) DO NOTHING;

INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number, usb_path,
                        topo_path, rack_slot)
SELECT 'h14', h.id,
       (SELECT p.id FROM farm.power_domains p
         WHERE p.host_id='h14' AND p.notes = CASE WHEN g <= 3 THEN 'A' ELSE 'B' END),
       g, '3-1.' || g,
       ('h14.c3.p3_1.p3_1_' || g)::ltree, 'R14-U1-H1-P' || g
  FROM farm.hubs h, generate_series(1,6) g
 WHERE h.host_id='h14';

INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id, model)
SELECT 'df-' || md5('v14-' || s.usb_path), 'SER14-' || s.port_number, 'default', 'h14', s.id, 'Pixel Test'
  FROM farm.slots s WHERE s.host_id='h14';
INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id, model)
VALUES ('df-' || md5('v14-unslotted'), 'SER14-7', 'default', 'h14', NULL, 'Pixel Test');

INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health)
SELECT d.id, d.host_id, d.current_slot_id, 'device', 'healthy'
  FROM farm.devices d WHERE d.host_id = 'h14';

-- covered(device) is the coverage predicate, arm for arm as the Go copies
-- spell it: scope decides which subject column counts.
CREATE FUNCTION farm.assert_v14_covered(p_device uuid) RETURNS boolean
LANGUAGE sql STABLE AS $probe$
  SELECT EXISTS (
    SELECT 1
      FROM farm.devices d
      LEFT JOIN farm.slots s ON s.id = d.current_slot_id
     WHERE d.id = p_device
       AND EXISTS (
         SELECT 1 FROM farm.quarantines q
          WHERE q.closed_at IS NULL
            AND ( (q.scope = 'device' AND q.device_id = d.id)
               OR (q.scope = 'slot'   AND q.slot_id   = s.id)
               OR (q.scope = 'hub'    AND q.hub_id    = s.hub_id)
               OR (q.scope = 'host'   AND q.host_id   = s.host_id)
               OR (q.scope = 'power_domain' AND q.power_domain_id = s.power_domain_id) )))
$probe$;

-- --------------------------------------------------------------------
-- Assertions
-- --------------------------------------------------------------------
DO $$
DECLARE
  dev     uuid[];
  dev7    uuid;
  pd_a    bigint;
  pd_b    bigint;
  slot4   bigint;
  q_pd    bigint;
  q_slot  bigint;
  q_dev   bigint;
  v_id    bigint;
  v_cnt   int;
  v_state text;
  v_park  bigint;
  i       int;
BEGIN
  SELECT array_agg(d.id ORDER BY s.port_number) INTO dev
    FROM farm.devices d JOIN farm.slots s ON s.id = d.current_slot_id
   WHERE d.host_id = 'h14';
  SELECT d.id INTO dev7 FROM farm.devices d WHERE d.host_id = 'h14' AND d.current_slot_id IS NULL;
  SELECT id INTO pd_a FROM farm.power_domains WHERE host_id = 'h14' AND notes = 'A';
  SELECT id INTO pd_b FROM farm.power_domains WHERE host_id = 'h14' AND notes = 'B';
  SELECT id INTO slot4 FROM farm.slots WHERE host_id = 'h14' AND port_number = 4;
  IF array_length(dev, 1) <> 6 OR dev7 IS NULL OR pd_a IS NULL OR pd_b IS NULL THEN
    RAISE EXCEPTION 'fixture did not build: % slotted devices, unslotted %, domains %/%',
      array_length(dev, 1), dev7, pd_a, pd_b;
  END IF;

  -- ============================================================
  -- V1  The schema: the column, the one-open-row index, and the
  --     CHECK that a row names the subject its scope requires.
  -- ============================================================
  PERFORM 1 FROM information_schema.columns
   WHERE table_schema = 'farm' AND table_name = 'quarantines' AND column_name = 'power_domain_id';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'V1 FAILED: farm.quarantines has no power_domain_id column';
  END IF;
  PERFORM 1 FROM pg_indexes
   WHERE schemaname = 'farm' AND tablename = 'quarantines' AND indexname = 'q_open_power_domain';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'V1 FAILED: no q_open_power_domain partial unique index';
  END IF;
  PERFORM 1 FROM pg_constraint
   WHERE conrelid = 'farm.quarantines'::regclass AND conname = 'quarantines_subject_matches_scope';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'V1 FAILED: no quarantines_subject_matches_scope CHECK';
  END IF;
  RAISE NOTICE 'V1  ok  column, index and CHECK are in place';

  -- ============================================================
  -- V2  A row cannot name a scope and no subject. That is the
  --     row shape every coverage predicate would silently treat
  --     as covering nothing. The ladder's own shape — a device
  --     row that also carries slot_id and host_id — is still
  --     accepted: extra columns are informational, not forbidden.
  -- ============================================================
  BEGIN
    INSERT INTO farm.quarantines (scope, host_id, reason) VALUES ('power_domain', 'h14', 'no subject');
    RAISE EXCEPTION 'V2 FAILED: a power_domain row with no power_domain_id was accepted';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'V2a ok  a power_domain row must name its domain';
  END;
  BEGIN
    INSERT INTO farm.quarantines (scope, host_id, reason) VALUES ('slot', 'h14', 'no subject');
    RAISE EXCEPTION 'V2 FAILED: a slot row with no slot_id was accepted';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'V2b ok  a slot row must name its slot';
  END;
  BEGIN
    INSERT INTO farm.quarantines (scope, device_id, hub_id, reason)
    VALUES ('device', NULL, (SELECT id FROM farm.hubs WHERE host_id='h14'), 'wrong column');
    RAISE EXCEPTION 'V2 FAILED: a device row naming only a hub was accepted';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'V2c ok  a device row cannot borrow another scope''s column';
  END;
  INSERT INTO farm.quarantines (scope, device_id, slot_id, host_id, reason, auto)
  VALUES ('device', dev[6], (SELECT current_slot_id FROM farm.devices WHERE id = dev[6]), 'h14', 'ladder shape', true)
  RETURNING id INTO v_id;
  UPDATE farm.quarantines SET closed_at = now(), closed_by = 'v14' WHERE id = v_id;
  RAISE NOTICE 'V2d ok  the ladder''s device row, with slot_id and host_id beside it, is still accepted';

  -- ============================================================
  -- V3  THE HEADLINE. A power-domain quarantine covers every
  --     slot wired to that switch and no other. Both the
  --     predicate and farm.v_fleet — what the operator looks at —
  --     must agree, and neither may change a device's health to
  --     get there: the row alone is the fact.
  -- ============================================================
  INSERT INTO farm.quarantines (scope, power_domain_id, host_id, reason, auto)
  VALUES ('power_domain', pd_a, 'h14', 'ganged switch browns out under load', false)
  RETURNING id INTO q_pd;

  FOR i IN 1..3 LOOP
    IF NOT farm.assert_v14_covered(dev[i]) THEN
      RAISE EXCEPTION 'V3 FAILED: device in slot % is on domain A and reads as NOT covered', i;
    END IF;
    SELECT quarantine_id INTO v_id FROM farm.v_fleet WHERE device_id = dev[i];
    IF v_id IS DISTINCT FROM q_pd THEN
      RAISE EXCEPTION 'V3 FAILED: v_fleet shows quarantine % for slot %, want %', v_id, i, q_pd;
    END IF;
  END LOOP;
  FOR i IN 4..6 LOOP
    IF farm.assert_v14_covered(dev[i]) THEN
      RAISE EXCEPTION 'V3 FAILED: device in slot % is on domain B and reads as covered', i;
    END IF;
    SELECT quarantine_id INTO v_id FROM farm.v_fleet WHERE device_id = dev[i];
    IF v_id IS NOT NULL THEN
      RAISE EXCEPTION 'V3 FAILED: v_fleet shows quarantine % for slot %, which is on domain B', v_id, i;
    END IF;
  END LOOP;
  SELECT count(*) INTO v_cnt FROM farm.device_runtime r
   WHERE r.device_id = ANY(dev) AND r.health <> 'healthy';
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'V3 FAILED: inserting a quarantine row changed % device(s) health; the row is the fact', v_cnt;
  END IF;
  RAISE NOTICE 'V3  ok  a power-domain quarantine covers exactly the slots on that switch';

  -- ============================================================
  -- V4  The unslotted device is NOT under the domain quarantine.
  --     Before 00014, v_fleet's arm was `q.slot_id IS NOT
  --     DISTINCT FROM s.id`, which for a row with no slot_id and
  --     a device with no slot is NULL IS NOT DISTINCT FROM NULL:
  --     true. Every handset on the bench would have shown as
  --     quarantined by a switch it is not plugged into.
  -- ============================================================
  IF farm.assert_v14_covered(dev7) THEN
    RAISE EXCEPTION 'V4 FAILED: an unslotted device reads as covered by a power-domain quarantine';
  END IF;
  SELECT quarantine_id INTO v_id FROM farm.v_fleet WHERE device_id = dev7;
  IF v_id IS NOT NULL THEN
    RAISE EXCEPTION 'V4 FAILED: v_fleet shows the unslotted device under quarantine %', v_id;
  END IF;
  RAISE NOTICE 'V4  ok  a device in no slot is on no power domain';

  -- ============================================================
  -- V5  One open quarantine per domain, like every other scope.
  -- ============================================================
  BEGIN
    INSERT INTO farm.quarantines (scope, power_domain_id, host_id, reason)
    VALUES ('power_domain', pd_a, 'h14', 'again');
    RAISE EXCEPTION 'V5 FAILED: a second open quarantine on the same domain was accepted';
  EXCEPTION WHEN unique_violation THEN
    RAISE NOTICE 'V5  ok  a second open row on the same domain is refused';
  END;

  -- ============================================================
  -- V6  A slot quarantine covers exactly that slot. Its
  --     neighbours on the same hub and the same domain are
  --     untouched.
  -- ============================================================
  INSERT INTO farm.quarantines (scope, slot_id, host_id, reason, auto)
  VALUES ('slot', slot4, 'h14', 'port chews cables', false)
  RETURNING id INTO q_slot;
  IF NOT farm.assert_v14_covered(dev[4]) THEN
    RAISE EXCEPTION 'V6 FAILED: the device in the quarantined slot reads as not covered';
  END IF;
  SELECT quarantine_id INTO v_id FROM farm.v_fleet WHERE device_id = dev[4];
  IF v_id IS DISTINCT FROM q_slot THEN
    RAISE EXCEPTION 'V6 FAILED: v_fleet shows quarantine % for slot 4, want %', v_id, q_slot;
  END IF;
  IF farm.assert_v14_covered(dev[5]) OR farm.assert_v14_covered(dev[6]) THEN
    RAISE EXCEPTION 'V6 FAILED: a slot quarantine spilled onto a neighbouring slot';
  END IF;
  RAISE NOTICE 'V6  ok  a slot quarantine covers one slot';

  -- ============================================================
  -- V7  Closing a row frees only what nothing else covers. With
  --     a device row on slot 1 open beside the domain row,
  --     closing the domain row leaves slot 1 covered and frees
  --     slots 2 and 3; slot 4 stays under its own row throughout.
  -- ============================================================
  INSERT INTO farm.quarantines (scope, device_id, host_id, reason, auto)
  VALUES ('device', dev[1], 'h14', 'this one is also cracked', false)
  RETURNING id INTO q_dev;

  UPDATE farm.quarantines SET closed_at = now(), closed_by = 'v14' WHERE id = q_pd;

  IF NOT farm.assert_v14_covered(dev[1]) THEN
    RAISE EXCEPTION 'V7 FAILED: closing the domain row freed a device its own row still covers';
  END IF;
  SELECT quarantine_id INTO v_id FROM farm.v_fleet WHERE device_id = dev[1];
  IF v_id IS DISTINCT FROM q_dev THEN
    RAISE EXCEPTION 'V7 FAILED: v_fleet shows quarantine % for slot 1, want its device row %', v_id, q_dev;
  END IF;
  IF farm.assert_v14_covered(dev[2]) OR farm.assert_v14_covered(dev[3]) THEN
    RAISE EXCEPTION 'V7 FAILED: slots 2 and 3 are still covered after the domain row closed';
  END IF;
  IF NOT farm.assert_v14_covered(dev[4]) THEN
    RAISE EXCEPTION 'V7 FAILED: closing the domain row freed the slot-quarantined device';
  END IF;

  UPDATE farm.quarantines SET closed_at = now(), closed_by = 'v14' WHERE id IN (q_dev, q_slot);
  SELECT count(*) INTO v_cnt FROM unnest(dev) AS d(id) WHERE farm.assert_v14_covered(d.id);
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'V7 FAILED: % device(s) still read as covered with every row closed', v_cnt;
  END IF;
  RAISE NOTICE 'V7  ok  closing a row frees only the devices no other open row covers';

  -- ============================================================
  -- V8  A human's park keeps the last word. Automation — a
  --     caller that declares p_auto — may close only a park it
  --     opened itself; alice's park is not its to reverse, and
  --     the device stays parked. A person then reverses it.
  -- ============================================================
  v_park := farm.device_park(dev[6], 'alice', 'shelf is being rewired at 18:00');
  BEGIN
    PERFORM farm.device_unpark(dev[6], 'charge-limiter', 'battery back at 80%', true);
    RAISE EXCEPTION 'V8 FAILED: automation reversed a human''s park';
  EXCEPTION WHEN insufficient_privilege THEN
    NULL;
  END;
  SELECT admin_state INTO v_state FROM farm.devices WHERE id = dev[6];
  IF v_state <> 'parked' THEN
    RAISE EXCEPTION 'V8 FAILED: after the refusal the device is %, not parked', v_state;
  END IF;
  PERFORM 1 FROM farm.device_parks WHERE id = v_park AND closed_at IS NULL;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'V8 FAILED: the refused unpark closed the park row anyway';
  END IF;
  PERFORM farm.device_unpark(dev[6], 'bob', 'rewiring finished', false);
  SELECT admin_state INTO v_state FROM farm.devices WHERE id = dev[6];
  IF v_state <> 'enabled' THEN
    RAISE EXCEPTION 'V8 FAILED: a human could not reverse the park (state %)', v_state;
  END IF;
  RAISE NOTICE 'V8  ok  device_unpark(p_auto => true) cannot reverse a human''s park';

  -- ============================================================
  -- V9  NO LEASE WAS TOUCHED. Quarantine and park govern the
  --     NEXT allocation. A lease ends when the job says so, when
  --     a user-written deadline elapses, or when a human takes it
  --     back — and nothing in this file is any of those.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.leases l
    JOIN farm.devices d ON d.id = l.device_id
   WHERE d.host_id = 'h14';
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'V9 FAILED: % lease row(s) appeared on h14', v_cnt;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.devices
   WHERE host_id = 'h14' AND current_lease_id IS NOT NULL;
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'V9 FAILED: a device on h14 acquired a lease pointer';
  END IF;
  RAISE NOTICE 'V9  ok  nothing here touched a lease';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v14 ASSERTIONS PASSED';
END $$;

ROLLBACK;
