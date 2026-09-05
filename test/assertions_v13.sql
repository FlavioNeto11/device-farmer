-- Assertions for migration 00013_hub_health_aligned.sql.
--
-- The defect these encode: farm.v_hub_health.unhealthy was a deny-list
-- (NOT IN healthy/retired/parked) while the recovery ladder's hub-fault
-- quorum is an allow-list (offline, unauthorized, missing, degraded). A hub
-- whose devices were all quarantined read 7/7 unhealthy on the fleet banner
-- and 0/7 in the quorum at the same instant. The view now carries the
-- ladder's predicate, and this file checks the two agree on EVERY value
-- farm.device_runtime.health may hold — not just the ones that happened to
-- differ when the bug was found.
--
-- The predicate below is spelled out rather than derived, on purpose: a Go
-- test (internal/recovery, TestHubHealthViewCarriesTheQuorumPredicate) pins
-- this file, the migration and recovery.UnhealthyStates to one list. If the
-- ladder's list changes and this file does not, that test fails the build;
-- if this file changes and the view does not, H2 fails here.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v13.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host, one hub, four slots, four devices.
--
-- Devices are inserted directly, healthy, with a device_runtime row each,
-- so every assertion starts from a hub the banner has nothing to say about.
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r13');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h13','r13','127.0.0.1:5037');
INSERT INTO farm.controllers (host_id, root_bus) VALUES ('h13', 3);
INSERT INTO farm.power_domains (host_id, kind, control)
  VALUES ('h13','per_port','uhubctl');
INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
  SELECT 'h13', c.id, '3-1', 7, true FROM farm.controllers c WHERE c.host_id='h13';
INSERT INTO farm.pools (id) VALUES ('default') ON CONFLICT (id) DO NOTHING;

INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number, usb_path,
                        topo_path, rack_slot)
SELECT 'h13', h.id, p.id, g, '3-1.' || g,
       ('h13.c3.p3_1.p3_1_' || g)::ltree, 'R13-U1-H1-P' || g
  FROM farm.hubs h, farm.power_domains p, generate_series(1,4) g
 WHERE h.host_id='h13' AND p.host_id='h13';

INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id, model)
SELECT 'df-' || md5('v13:' || s.usb_path),
       'SER13-' || s.port_number, 'default', 'h13', s.id, 'Pixel Test'
  FROM farm.slots s WHERE s.host_id='h13';

INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health)
SELECT d.id, d.host_id, d.current_slot_id, 'device', 'healthy'
  FROM farm.devices d WHERE d.host_id = 'h13';

-- --------------------------------------------------------------------
-- Assertions
-- --------------------------------------------------------------------
DO $$
DECLARE
  -- THE predicate: recovery.UnhealthyStates, spelled the way the ladder
  -- sends it. Pinned to the Go slice by a build-time test; see the header.
  c_unhealthy CONSTANT text[] := ARRAY['offline','unauthorized','missing','degraded'];
  -- Every value farm.device_runtime.health may hold, per the CHECK
  -- constraint 00008 left in place. H1 proves this list IS that constraint.
  c_all       CONSTANT text[] := ARRAY['unknown','booting','healthy','degraded','offline',
                                       'unauthorized','missing','recovering','parked',
                                       'quarantined','retired'];
  v_hub     bigint;
  v_dev1    uuid;
  v_dev2    uuid;
  v_health  text;
  v_cnt     int;
  v_since   timestamptz;
  v_def     text;
  v_expect  int;
BEGIN
  SELECT h.id INTO v_hub FROM farm.hubs h WHERE h.host_id = 'h13';
  SELECT d.id INTO v_dev1 FROM farm.devices d JOIN farm.slots s ON s.id = d.current_slot_id
   WHERE s.host_id = 'h13' AND s.port_number = 1;
  SELECT d.id INTO v_dev2 FROM farm.devices d JOIN farm.slots s ON s.id = d.current_slot_id
   WHERE s.host_id = 'h13' AND s.port_number = 2;

  -- ============================================================
  -- H1  The list this file walks IS the CHECK constraint. A health
  --     value added to the schema and not to c_all would otherwise
  --     be a value nobody tested the view against.
  -- ============================================================
  SELECT pg_get_constraintdef(c.oid) INTO v_def
    FROM pg_constraint c
   WHERE c.conrelid = 'farm.device_runtime'::regclass
     AND c.conname = 'device_runtime_health_check';
  IF v_def IS NULL THEN
    RAISE EXCEPTION 'H1 FAILED: device_runtime_health_check is gone; the health vocabulary is unconstrained';
  END IF;
  FOREACH v_health IN ARRAY c_all LOOP
    IF position('''' || v_health || '''' IN v_def) = 0 THEN
      RAISE EXCEPTION 'H1 FAILED: this file expects health % but the CHECK does not admit it: %',
        v_health, v_def;
    END IF;
  END LOOP;
  -- Count the quoted literals in the CHECK: one per admitted value, so a
  -- value the CHECK admits and c_all lacks shows up as a count mismatch.
  SELECT count(*) INTO v_cnt FROM regexp_matches(v_def, '''[a-z_]+''', 'g');
  IF v_cnt <> array_length(c_all, 1) THEN
    RAISE EXCEPTION 'H1 FAILED: the CHECK admits % health values and this file walks %; '
                    'the view was not tested against every value: %',
      v_cnt, array_length(c_all, 1), v_def;
  END IF;
  -- And the predicate is a subset of the vocabulary, or it counts nothing.
  FOREACH v_health IN ARRAY c_unhealthy LOOP
    IF NOT (v_health = ANY (c_all)) THEN
      RAISE EXCEPTION 'H1 FAILED: the unhealthy predicate names %, which health cannot hold', v_health;
    END IF;
  END LOOP;
  RAISE NOTICE 'H1  ok  the walk below covers every value the CHECK admits (%)', v_cnt;

  -- ============================================================
  -- H2  THE HEADLINE. For EVERY health value, the view's unhealthy
  --     count equals the ladder's predicate applied to one device.
  --     One device is moved through every state while its three
  --     neighbours stay healthy, so the expected answer is exactly
  --     0 or 1 and nothing about the fixture can hide a mismatch.
  -- ============================================================
  FOREACH v_health IN ARRAY c_all LOOP
    UPDATE farm.device_runtime SET health = v_health, health_since = now()
     WHERE device_id = v_dev1;
    v_expect := CASE WHEN v_health = ANY (c_unhealthy) THEN 1 ELSE 0 END;

    SELECT unhealthy, worst_since INTO v_cnt, v_since
      FROM farm.v_hub_health WHERE hub_id = v_hub;
    IF v_cnt <> v_expect THEN
      RAISE EXCEPTION 'H2 FAILED: health % -> v_hub_health.unhealthy = %, the ladder''s predicate says %',
        v_health, v_cnt, v_expect;
    END IF;
    -- worst_since is "when the fault evidence started": present exactly
    -- when there is evidence, absent otherwise.
    IF (v_since IS NOT NULL) <> (v_expect = 1) THEN
      RAISE EXCEPTION 'H2 FAILED: health % -> worst_since is % but unhealthy is %',
        v_health, v_since, v_expect;
    END IF;
    -- The same device read through the ladder's own predicate, as SQL, so the
    -- comparison is view-against-predicate and not view-against-this-file.
    SELECT count(*) INTO v_cnt FROM farm.device_runtime r
     WHERE r.device_id = v_dev1
       AND r.health IN ('offline','unauthorized','missing','degraded');
    IF v_cnt <> v_expect THEN
      RAISE EXCEPTION 'H2 FAILED: the predicate as SQL counted % for health %, expected %',
        v_cnt, v_health, v_expect;
    END IF;
  END LOOP;
  UPDATE farm.device_runtime SET health = 'healthy', health_since = now() WHERE device_id = v_dev1;
  RAISE NOTICE 'H2  ok  view and quorum predicate agree on all % health values', array_length(c_all, 1);

  -- ============================================================
  -- H3  The case that was verified live. A hub the ladder has
  --     quarantined — every device 'quarantined', written in one
  --     sweep with one health_since — reads 0 unhealthy, because
  --     that is what the quorum reads and the quarantine is what
  --     farm.quarantines reports. Before 00013 this read 4/4.
  -- ============================================================
  UPDATE farm.device_runtime r SET health = 'quarantined', health_since = now()
    FROM farm.devices d WHERE r.device_id = d.id AND d.host_id = 'h13';
  SELECT unhealthy, worst_since INTO v_cnt, v_since FROM farm.v_hub_health WHERE hub_id = v_hub;
  IF v_cnt <> 0 OR v_since IS NOT NULL THEN
    RAISE EXCEPTION 'H3 FAILED: a fully quarantined hub reads % unhealthy since %; the banner '
                    'would show a hub shedding devices for the ladder''s own bookkeeping', v_cnt, v_since;
  END IF;
  RAISE NOTICE 'H3  ok  a quarantined hub is 0 unhealthy on the banner, as it is in the quorum';

  -- ============================================================
  -- H4  The unknown sweep. reconcileQuarantines writes 'unknown' to
  --     every device on the hub in one statement when the operator
  --     closes the quarantine. Same health_since for all of them:
  --     spread 0, quorum unanimous if it were counted. It is not.
  -- ============================================================
  UPDATE farm.device_runtime r SET health = 'unknown', health_since = now(), ladder_tier = 0
    FROM farm.devices d WHERE r.device_id = d.id AND d.host_id = 'h13';
  SELECT unhealthy INTO v_cnt FROM farm.v_hub_health WHERE hub_id = v_hub;
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'H4 FAILED: the hub the operator just cleared reads % unhealthy', v_cnt;
  END IF;
  RAISE NOTICE 'H4  ok  a hub just released to ''unknown'' is not a hub fault';

  -- ============================================================
  -- H5  The control, in both directions: real evidence still counts,
  --     and the count is exact. Two devices genuinely offline on a
  --     hub of four read 2, and worst_since is the LATER of the two
  --     — a quorum spread needs both ends, and the banner shows the
  --     end an operator can act on.
  -- ============================================================
  UPDATE farm.device_runtime SET health = 'healthy', health_since = now()
    FROM farm.devices d WHERE device_runtime.device_id = d.id AND d.host_id = 'h13';
  UPDATE farm.device_runtime SET health = 'offline', health_since = now() - interval '5 minutes'
   WHERE device_id = v_dev1;
  UPDATE farm.device_runtime SET health = 'missing', health_since = now() - interval '1 minute'
   WHERE device_id = v_dev2;
  SELECT unhealthy, worst_since INTO v_cnt, v_since FROM farm.v_hub_health WHERE hub_id = v_hub;
  IF v_cnt <> 2 THEN
    RAISE EXCEPTION 'H5 FAILED: two devices with fault evidence read as % unhealthy', v_cnt;
  END IF;
  IF v_since IS NULL OR v_since < now() - interval '2 minutes' THEN
    RAISE EXCEPTION 'H5 FAILED: worst_since = %, expected the later of the two faults', v_since;
  END IF;
  RAISE NOTICE 'H5  ok  genuine faults still count, exactly';

  -- ============================================================
  -- H6  Parked stays uncounted (00008 K9 preserved): a deliberate
  --     hold is not evidence, and the allow-list keeps that promise
  --     without having to name 'parked' at all.
  -- ============================================================
  UPDATE farm.device_runtime SET health = 'parked', health_since = now() WHERE device_id = v_dev2;
  SELECT unhealthy INTO v_cnt FROM farm.v_hub_health WHERE hub_id = v_hub;
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'H6 FAILED: one offline and one parked read as % unhealthy, expected 1', v_cnt;
  END IF;
  RAISE NOTICE 'H6  ok  a parked device is still not a hub fault';

  -- ============================================================
  -- H7  NO LEASE WAS TOUCHED. A view is a query; this file moved
  --     health through eleven values and nothing else. A lease ends
  --     when the job says so, when a deadline a human wrote elapses,
  --     or when a human takes it back — and nothing here is any of
  --     those.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.leases l
    JOIN farm.devices d ON d.id = l.device_id
   WHERE d.host_id = 'h13';
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'H7 FAILED: aligning a view created % lease row(s)', v_cnt;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.devices
   WHERE host_id = 'h13' AND current_lease_id IS NOT NULL;
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'H7 FAILED: % device(s) on h13 acquired a lease pointer', v_cnt;
  END IF;
  RAISE NOTICE 'H7  ok  no lease was touched';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v13 ASSERTIONS PASSED';
END $$;

ROLLBACK;
