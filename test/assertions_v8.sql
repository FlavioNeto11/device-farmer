-- Assertions for migration 00008: a deliberately parked device.
--
-- The failure this prevents is quiet and expensive. Charge limiting holds a
-- handset between 40% and 80% by cutting USB VBUS, the device disappears from
-- the ADB tracker, the watchdog calls that 'missing', the recovery ladder
-- adopts it as broken, climbs to a port power cycle, and quarantines a
-- perfectly good phone. Every assertion below is one link in that chain,
-- broken on purpose.
--
-- The last group is the one that matters most. A lease ends when the job says
-- so, when a user-written deadline elapses, or when a human takes it back.
-- NOTHING ELSE. Parking a device is none of the three, and P6 proves it is not
-- merely absent from the code but unrepresentable in the role the code runs as.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v8.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host, one hub, four slots, four healthy devices.
--
--   dev1  the subject: parked, observed, laddered, unparked
--   dev2  the leased one: THE INVARIANT
--   dev3  parked by automation, unparked by the same automation
--   dev4  parked by automation, and somebody else tries to undo it
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r1');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h01','r1','127.0.0.1:5037');
INSERT INTO farm.controllers (host_id, root_bus) VALUES ('h01', 3);
INSERT INTO farm.power_domains (host_id, kind, control)
  VALUES ('h01','per_port','uhubctl');
INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
  SELECT 'h01', c.id, '3-1', 7, true FROM farm.controllers c WHERE c.host_id='h01';
INSERT INTO farm.pools (id) VALUES ('default');
INSERT INTO farm.tenants (id) VALUES ('acme');
INSERT INTO farm.queues (id, tenant_id) VALUES ('q1','acme');

INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number, usb_path,
                        topo_path, rack_slot)
SELECT 'h01', h.id, p.id, g, '3-1.' || g,
       ('h01.c3.p3_1.p3_1_' || g)::ltree, 'R1-U1-H1-P' || g
  FROM farm.hubs h, farm.power_domains p, generate_series(1,4) g
 WHERE h.host_id='h01' AND p.host_id='h01';

INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id, model)
SELECT 'df-' || lpad(md5(s.usb_path), 32, '0'),
       'SER' || s.port_number, 'default', 'h01', s.id, 'Pixel Test'
  FROM farm.slots s WHERE s.host_id='h01';

INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health)
SELECT d.id, d.host_id, d.current_slot_id, 'device', 'healthy' FROM farm.devices d;

-- A long job, so lease_acquire marks it protected: the lease the invariant is
-- about is the six-hour kind that STF #663 destroys.
INSERT INTO farm.jobs (id, tenant_id, queue_id, pool_id, expected_duration)
VALUES ('88888888-0000-0000-0000-000000000001','acme','q1','default', interval '6 hours');
INSERT INTO farm.jobs (id, tenant_id, queue_id, pool_id, expected_duration)
VALUES ('88888888-0000-0000-0000-000000000002','acme','q1','default', interval '6 hours');

-- Probes carrying the role scoping of the two planes that must not be able to
-- un-park a device or end a lease. If the firewalls hold, both raise 42501.
--
-- farm_watchdog is the health plane: SELECT on farm.devices and nothing more.
CREATE FUNCTION farm.assert_watchdog_cannot_unpark(p_device uuid) RETURNS int
LANGUAGE plpgsql SET role = farm_watchdog AS $probe$
BEGIN
  UPDATE farm.devices SET admin_state = 'enabled' WHERE id = p_device;
  RETURN 1;
END $probe$;

-- farm_parker is the role farm.device_park and farm.device_unpark run as.
CREATE FUNCTION farm.assert_parker_cannot_read_leases() RETURNS int
LANGUAGE plpgsql SET role = farm_parker AS $probe$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM farm.leases;
  RETURN n;
END $probe$;

CREATE FUNCTION farm.assert_parker_cannot_end_a_lease() RETURNS int
LANGUAGE plpgsql SET role = farm_parker AS $probe$
BEGIN
  UPDATE farm.leases SET state = 'released', released_at = now(),
                         release_reason = 'operator_revoked'
   WHERE state IN ('held','suspect');
  RETURN 1;
END $probe$;

-- --------------------------------------------------------------------
-- Assertions
-- --------------------------------------------------------------------
DO $$
DECLARE
  dev1        uuid;
  dev2        uuid;
  dev3        uuid;
  dev4        uuid;
  v_park      bigint;
  v_park2     bigint;
  p           record;
  a           record;
  v_lease     uuid;
  v_fence     bigint;
  v_state     text;
  v_health    text;
  v_adb       text;
  v_cnt       int;
  v_since     timestamptz;
  v_tier      int;
  v_bad       int;
BEGIN
  SELECT d.id INTO dev1 FROM farm.devices d JOIN farm.slots s ON s.id = d.current_slot_id
   WHERE s.port_number = 1;
  SELECT d.id INTO dev2 FROM farm.devices d JOIN farm.slots s ON s.id = d.current_slot_id
   WHERE s.port_number = 2;
  SELECT d.id INTO dev3 FROM farm.devices d JOIN farm.slots s ON s.id = d.current_slot_id
   WHERE s.port_number = 3;
  SELECT d.id INTO dev4 FROM farm.devices d JOIN farm.slots s ON s.id = d.current_slot_id
   WHERE s.port_number = 4;

  -- ============================================================
  -- K1  Parking names a person and a reason, and both survive.
  --     "Out of service" with no author is indistinguishable from
  --     a fault, which is the confusion this state exists to end.
  -- ============================================================
  v_park := farm.device_park(dev1, 'alice', 'battery hold: charge limiter 40-80%');

  SELECT * INTO p FROM farm.device_parks WHERE id = v_park;
  IF p.opened_by <> 'alice' THEN
    RAISE EXCEPTION 'K1 FAILED: park recorded opener % , expected alice', p.opened_by;
  END IF;
  IF p.reason <> 'battery hold: charge limiter 40-80%' THEN
    RAISE EXCEPTION 'K1 FAILED: park lost its reason (got %)', p.reason;
  END IF;
  IF p.closed_at IS NOT NULL OR p.auto THEN
    RAISE EXCEPTION 'K1 FAILED: a fresh human park is closed or marked automatic';
  END IF;

  SELECT count(*) INTO v_cnt FROM farm.audit_log
   WHERE action = 'device.park' AND actor = 'alice'
     AND subject = 'device:' || dev1::text
     AND reason = 'battery hold: charge limiter 40-80%';
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'K1 FAILED: % audit rows name the human who parked it, expected 1', v_cnt;
  END IF;

  SELECT admin_state INTO v_state FROM farm.devices WHERE id = dev1;
  SELECT health      INTO v_health FROM farm.device_runtime WHERE device_id = dev1;
  IF v_state <> 'parked' OR v_health <> 'parked' THEN
    RAISE EXCEPTION 'K1 FAILED: admin_state=% health=%, expected parked/parked', v_state, v_health;
  END IF;
  RAISE NOTICE 'K1  ok  park records who (alice), why, and lands on both planes';

  -- ============================================================
  -- K2  A parked device is not allocated. The allocator needs no
  --     new predicate for this: admin_state has always been the
  --     gate, and 'parked' is a value it already refuses.
  -- ============================================================
  UPDATE farm.jobs SET pin_device = dev1 WHERE id = '88888888-0000-0000-0000-000000000001';
  SELECT count(*) INTO v_cnt FROM farm.lease_acquire(
    '88888888-0000-0000-0000-000000000001', 'runner-pod-a', gen_random_uuid());
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'K2 FAILED: the allocator handed out a parked device';
  END IF;

  -- The control. Without it this assertion also passes on a broken
  -- fixture in which nothing at all is allocatable.
  UPDATE farm.jobs SET pin_device = dev3 WHERE id = '88888888-0000-0000-0000-000000000001';
  SELECT lease_id, fence INTO v_lease, v_fence FROM farm.lease_acquire(
    '88888888-0000-0000-0000-000000000001', 'runner-pod-a', gen_random_uuid());
  IF v_lease IS NULL THEN
    RAISE EXCEPTION 'K2 FAILED: the control device was not allocatable either; '
                    'the fixture proves nothing';
  END IF;
  PERFORM farm.lease_release(v_lease, v_fence, 'completed');
  RAISE NOTICE 'K2  ok  parked device refused, unparked device granted';

  -- ============================================================
  -- K3  THE HEADLINE. The observation loop cannot overwrite it.
  --
  --     This is the exact write the watchdog performs when VBUS is
  --     cut: adb reports the device absent, healthFor() calls that
  --     'missing', and consec_bad passes MinBad. It is issued here
  --     RAW — without the watchdog's own CASE — so the assertion
  --     tests the guarantee in the database rather than a copy of
  --     the Go query.
  -- ============================================================
  UPDATE farm.device_runtime
     SET adb_state = 'absent', health = 'missing', health_since = now(),
         consec_bad = 9, updated_at = now()
   WHERE device_id = dev1;

  SELECT health, adb_state, consec_bad INTO v_health, v_adb, v_bad
    FROM farm.device_runtime WHERE device_id = dev1;
  IF v_health <> 'parked' THEN
    RAISE EXCEPTION 'K3 FAILED: an observation overwrote parked with %', v_health;
  END IF;
  -- The guard holds the VERDICT, not the row. What the wire is doing is
  -- still recorded truthfully; only the conclusion drawn from it is refused.
  IF v_adb <> 'absent' OR v_bad <> 9 THEN
    RAISE EXCEPTION 'K3 FAILED: the guard froze the whole row (adb_state=%, consec_bad=%)',
      v_adb, v_bad;
  END IF;
  RAISE NOTICE 'K3  ok  health stays parked while adb_state still reports absent';

  -- ============================================================
  -- K4  The recovery ladder does not adopt it.
  --
  --     The WHERE clause is copied from internal/recovery/ladder.go
  --     candidates(). Two independent predicates exclude a parked
  --     device — the health value and the admin_state — so this
  --     still proves the exclusion even if the copy drifts.
  -- ============================================================
  UPDATE farm.device_runtime SET health_since = now() - interval '1 hour'
   WHERE device_id = dev1;

  SELECT count(*) INTO v_cnt
    FROM farm.device_runtime r
    JOIN farm.devices d ON d.id = r.device_id
    JOIN farm.slots   s ON s.id = d.current_slot_id
    JOIN farm.hubs   hb ON hb.id = s.hub_id
    JOIN farm.hosts  ho ON ho.id = s.host_id
   WHERE r.device_id = dev1
     AND r.health NOT IN ('healthy','retired','quarantined','parked','unknown')
     AND r.adb_state <> 'device'
     AND r.health_since < now() - interval '30 seconds'
     AND (r.suppress_until IS NULL
          OR (r.suppress_until <= now() AND r.updated_at > r.suppress_until))
     AND d.admin_state = 'enabled'
     AND s.state = 'active'
     AND ho.admin_state <> 'disabled';
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'K4 FAILED: the recovery ladder adopted a parked device';
  END IF;
  RAISE NOTICE 'K4  ok  a parked device is not a recovery candidate';

  -- ============================================================
  -- K5  Unparking is explicit, and automation cannot do it.
  --
  --     Three ways the state could be undone behind the operator's
  --     back, all refused: a bare UPDATE, the health plane, and a
  --     control loop reversing somebody else's decision.
  -- ============================================================
  BEGIN
    UPDATE farm.devices SET admin_state = 'enabled' WHERE id = dev1;
    RAISE EXCEPTION 'K5 FAILED: a bare UPDATE un-parked the device';
  EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'K5a ok  a bare UPDATE cannot un-park (guard raised)';
  END;

  BEGIN
    PERFORM farm.assert_watchdog_cannot_unpark(dev1);
    RAISE EXCEPTION 'K5 FAILED: the watchdog role wrote farm.devices.admin_state';
  EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'K5b ok  the health plane has no privilege to un-park anything';
  END;

  BEGIN
    PERFORM farm.device_unpark(dev1, 'charge-limiter', 'battery back at 80%', true);
    RAISE EXCEPTION 'K5 FAILED: automation reversed a human''s park';
  EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'K5c ok  automation may not reverse a park a human opened';
  END;

  -- The other half of the rule, copied from topo restore(): automation DOES
  -- get to reverse its own decision. A charge limiter that could not release
  -- its own hold would need a human every time a battery reached 80%.
  v_park2 := farm.device_park(dev3, 'charge-limiter', 'battery below 40%', true);
  PERFORM farm.device_unpark(dev3, 'charge-limiter', 'battery back at 80%', true);
  SELECT admin_state INTO v_state FROM farm.devices WHERE id = dev3;
  IF v_state <> 'enabled' THEN
    RAISE EXCEPTION 'K5 FAILED: automation could not reverse its own park (state %)', v_state;
  END IF;
  RAISE NOTICE 'K5d ok  automation reverses its OWN park';

  -- ...but only its own. A second loop is as much of a stranger as a timer.
  PERFORM farm.device_park(dev4, 'charge-limiter', 'battery below 40%', true);
  BEGIN
    PERFORM farm.device_unpark(dev4, 'some-other-loop', 'looked fine to me', true);
    RAISE EXCEPTION 'K5 FAILED: one control loop reversed another one''s park';
  EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'K5e ok  automation may not reverse another loop''s park';
  END;

  -- ============================================================
  -- K6  THE INVARIANT. Parking a device does not end its lease.
  --
  --     A lease ends when the job says so, when a user-written
  --     deadline elapses, or when a human takes it back. Parking is
  --     none of those three. admin_state governs ALLOCATION and has
  --     never had an opinion about work already in progress.
  -- ============================================================
  UPDATE farm.jobs SET pin_device = dev2 WHERE id = '88888888-0000-0000-0000-000000000002';
  SELECT lease_id, fence INTO v_lease, v_fence FROM farm.lease_acquire(
    '88888888-0000-0000-0000-000000000002', 'runner-pod-b', gen_random_uuid());
  IF v_lease IS NULL THEN
    RAISE EXCEPTION 'K6 FAILED: could not acquire the lease the invariant is about';
  END IF;

  PERFORM farm.device_park(dev2, 'alice', 'shelf is being rewired at 18:00');

  SELECT * INTO a FROM farm.leases WHERE id = v_lease;
  IF a.state <> 'held' THEN
    RAISE EXCEPTION 'K6 FAILED: parking moved the lease to %', a.state;
  END IF;
  IF a.released_at IS NOT NULL OR a.release_reason IS NOT NULL THEN
    RAISE EXCEPTION 'K6 FAILED: parking released the lease (% at %)',
      a.release_reason, a.released_at;
  END IF;
  IF a.fence <> v_fence THEN
    RAISE EXCEPTION 'K6 FAILED: parking bumped the fence (% -> %)', v_fence, a.fence;
  END IF;

  SELECT current_lease_id INTO v_lease FROM farm.devices WHERE id = dev2;
  IF v_lease IS DISTINCT FROM a.id THEN
    RAISE EXCEPTION 'K6 FAILED: parking cleared the device''s live lease pointer';
  END IF;

  -- The ledger trigger from 00007 writes a 'lease_ended' row on ANY ending,
  -- so its absence is independent evidence that nothing ended.
  SELECT count(*) INTO v_cnt FROM farm.events e
   WHERE e.kind = 'lease_ended' AND e.lease_id = a.id;
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'K6 FAILED: the lease ledger recorded an ending during a park';
  END IF;
  RAISE NOTICE 'K6  ok  the lease survives the park: held, same fence, no ending';

  -- Unparking is equally uninterested in the lease.
  PERFORM farm.device_unpark(dev2, 'alice', 'rewiring finished');
  SELECT * INTO a FROM farm.leases WHERE id = a.id;
  IF a.state <> 'held' OR a.released_at IS NOT NULL THEN
    RAISE EXCEPTION 'K6 FAILED: unparking ended the lease (state %)', a.state;
  END IF;
  RAISE NOTICE 'K6b ok  the lease survives the unpark too';

  -- ...and it is not left to the reader. The role the park functions run as
  -- cannot end a lease because it cannot so much as count them. This is the
  -- STF #663 firewall from 00002_lease.sql, pointed the other way.
  BEGIN
    PERFORM farm.assert_parker_cannot_read_leases();
    RAISE EXCEPTION 'K6 FAILED: the parking role can read farm.leases';
  EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'K6c ok  farm.leases is unreadable to the role that parks';
  END;

  BEGIN
    PERFORM farm.assert_parker_cannot_end_a_lease();
    RAISE EXCEPTION 'K6 FAILED: the parking role can release a lease';
  EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'K6d ok  no future edit to farm.device_park can end a lease';
  END;

  -- ============================================================
  -- K7  Park refuses the inputs that would make it useless.
  -- ============================================================
  BEGIN
    PERFORM farm.device_park(dev3, '  ', 'no author');
    RAISE EXCEPTION 'K7 FAILED: an anonymous park was accepted';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'K7a ok  parking requires an actor';
  END;

  BEGIN
    PERFORM farm.device_park(dev3, 'alice', '   ');
    RAISE EXCEPTION 'K7 FAILED: a park with no reason was accepted';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'K7b ok  parking requires a reason';
  END;

  BEGIN
    PERFORM farm.device_park(dev1, 'bob', 'parking it again');
    RAISE EXCEPTION 'K7 FAILED: a device was parked twice';
  EXCEPTION WHEN unique_violation THEN
    RAISE NOTICE 'K7c ok  one open park per device';
  END;

  BEGIN
    PERFORM farm.device_unpark(dev3, 'alice', 'it was never parked');
    RAISE EXCEPTION 'K7 FAILED: unparking a device that is not parked succeeded';
  EXCEPTION WHEN no_data_found THEN
    RAISE NOTICE 'K7d ok  unparking something that is not parked is an error';
  END;

  -- ============================================================
  -- K8  Unparking hands the device back to the health plane, not
  --     to the allocator. 'unknown' is "look at it again"; nobody
  --     has observed this device since the hold began, and
  --     'healthy' would be an assumption the allocator acts on.
  -- ============================================================
  UPDATE farm.device_runtime SET ladder_tier = 4 WHERE device_id = dev1;
  PERFORM farm.device_unpark(dev1, 'alice', 'battery back at 80%');

  SELECT d.admin_state, r.health, r.consec_bad, r.ladder_tier
    INTO v_state, v_health, v_bad, v_tier
    FROM farm.devices d JOIN farm.device_runtime r ON r.device_id = d.id
   WHERE d.id = dev1;
  IF v_state <> 'enabled' THEN
    RAISE EXCEPTION 'K8 FAILED: unpark left admin_state at %', v_state;
  END IF;
  IF v_health <> 'unknown' THEN
    RAISE EXCEPTION 'K8 FAILED: unpark set health to % (must be unknown, never healthy)', v_health;
  END IF;
  IF v_bad <> 0 OR v_tier <> 0 THEN
    RAISE EXCEPTION 'K8 FAILED: unpark kept stale counters (consec_bad=%, ladder_tier=%)',
      v_bad, v_tier;
  END IF;

  SELECT * INTO p FROM farm.device_parks WHERE id = v_park;
  IF p.closed_at IS NULL OR p.closed_by <> 'alice' THEN
    RAISE EXCEPTION 'K8 FAILED: the ledger row was not closed by a named human';
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.audit_log
   WHERE action = 'device.unpark' AND subject = 'device:' || dev1::text;
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'K8 FAILED: % audit rows for the unpark, expected 1', v_cnt;
  END IF;
  RAISE NOTICE 'K8  ok  unpark returns the device at health unknown, ledger closed';

  -- ============================================================
  -- K9  The correlation banner does not report a deliberate hold
  --     as a hub shedding devices. Charge-limit half a shelf and
  --     an operator must not be shown a hub fault on the screen
  --     they stare at during an incident.
  -- ============================================================
  -- The three devices that have been through a park are back at 'unknown';
  -- give them the good look the watchdog would give them, so the only device
  -- on this hub that is not 'healthy' is the one that is parked.
  UPDATE farm.device_runtime SET health = 'healthy', health_since = now()
   WHERE device_id IN (dev1, dev2, dev3);

  SELECT unhealthy, worst_since INTO v_cnt, v_since
    FROM farm.v_hub_health WHERE host_id = 'h01';
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'K9 FAILED: v_hub_health counts % unhealthy devices; dev4 is parked, '
                    'and nothing on this hub is broken', v_cnt;
  END IF;
  IF v_since IS NOT NULL THEN
    RAISE EXCEPTION 'K9 FAILED: v_hub_health dates a hub fault from a parked device';
  END IF;

  -- The control: the view must still see a real fault. Without this the
  -- assertion above passes on a view that counts nothing at all.
  UPDATE farm.device_runtime SET health = 'missing', health_since = now()
   WHERE device_id = dev1;
  SELECT unhealthy INTO v_cnt FROM farm.v_hub_health WHERE host_id = 'h01';
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'K9 FAILED: v_hub_health counted % unhealthy with one device '
                    'genuinely missing, expected 1', v_cnt;
  END IF;
  RAISE NOTICE 'K9  ok  a parked device is not a hub fault; a missing one still is';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v8 ASSERTIONS PASSED';
END $$;

ROLLBACK;
