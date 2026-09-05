-- Battery-history assertions for migration 00016_battery_readings.sql.
--
-- The gap these encode: the fleet remembered ONE battery number per device,
-- overwritten every minute, so a cell heating at two degrees a minute was
-- indistinguishable from a cell that had always been warm. docs/siting.md §2
-- is the reason that matters — clean-agent suppression does not stop a
-- lithium event, and early detection is one of the four mitigations that do.
-- 00016 adds the history table the detector reads, the prune that keeps it
-- small, and the privileges the watchdog needs to write both — while leaving
-- the watchdog exactly as blind to farm.leases as it was.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v16.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host, two slots, two devices adopted through the
-- enrolment path so the device_runtime rows exist the way the reader
-- expects them.
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r16');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h16','r16','127.0.0.1:5037');
INSERT INTO farm.pools (id) VALUES ('default') ON CONFLICT (id) DO NOTHING;

SELECT farm.register_slot('h16','3-1.1','3-1',1,'hub',7,false,'R16-U1-H3.1-P1');
SELECT farm.register_slot('h16','3-1.2','3-1',2,'hub',7,false,'R16-U1-H3.1-P2');

SELECT * FROM farm.resolve_device('h16','3-1.1', NULL, '\x16'::bytea, 'SER-16A', 'default', '{}'::jsonb);
SELECT * FROM farm.resolve_device('h16','3-1.2', NULL, '\x17'::bytea, 'SER-16B', 'default', '{}'::jsonb);

-- Probes carrying the watchdog's role scoping. The first three must
-- SUCCEED — they are the writes 00016 exists to permit. The last two must
-- raise 42501: the health plane may say a cell is hot, and may not read or
-- end the lease on the device it belongs to.
-- The reading is backdated by half a minute: now() is fixed for the whole
-- of this transaction, so a second default-timestamped row for the same
-- device would collide with V1's on the (device_id, at) key. The watchdog
-- never meets this — one cycle is one statement, one transaction — and a
-- collision there is absorbed by its ON CONFLICT DO NOTHING.
CREATE FUNCTION farm.assert_watchdog_writes_history(p_device uuid) RETURNS int
LANGUAGE plpgsql SET role = farm_watchdog AS $probe$
BEGIN
  INSERT INTO farm.battery_readings (device_id, at, pct, temp_dc)
  VALUES (p_device, now() - interval '30 seconds', 61, 311);
  RETURN 1;
END $probe$;

CREATE FUNCTION farm.assert_watchdog_prunes() RETURNS bigint
LANGUAGE plpgsql SET role = farm_watchdog AS $probe$
BEGIN
  RETURN farm.battery_readings_prune();
END $probe$;

-- The detail is a parameter so the same probe can carry a complete finding
-- and, in V4, one with the position missing or blank.
CREATE FUNCTION farm.assert_watchdog_raises(p_device uuid, p_slot bigint, p_detail jsonb) RETURNS int
LANGUAGE plpgsql SET role = farm_watchdog AS $probe$
BEGIN
  INSERT INTO farm.events (kind, device_id, slot_id, actor, detail)
  VALUES ('battery_anomaly', p_device, p_slot, 'watchdog:h16', p_detail);
  RETURN 1;
END $probe$;

CREATE FUNCTION farm.assert_watchdog_cannot_read_leases() RETURNS int
LANGUAGE plpgsql SET role = farm_watchdog AS $probe$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM farm.leases;
  RETURN n;
END $probe$;

CREATE FUNCTION farm.assert_watchdog_cannot_end_a_lease() RETURNS int
LANGUAGE plpgsql SET role = farm_watchdog AS $probe$
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
  v_a       uuid;
  v_b       uuid;
  v_slot_a  bigint;
  v_cnt     int;
  v_deleted bigint;
  v_detail  jsonb;
BEGIN
  SELECT d.id, d.current_slot_id INTO v_a, v_slot_a
    FROM farm.devices d WHERE d.adb_serial = 'SER-16A';
  SELECT d.id INTO v_b FROM farm.devices d WHERE d.adb_serial = 'SER-16B';

  -- ============================================================
  -- V1  A reading is one row keyed by the server's clock, and it
  --     honours the SAME unit checks the runtime columns carry.
  --     Out-of-range values are refused, not clamped: a clamped
  --     reading is an invented observation, and this table exists
  --     to be fitted a slope through.
  -- ============================================================
  INSERT INTO farm.battery_readings (device_id, pct, temp_dc) VALUES (v_a, 87, 293);
  INSERT INTO farm.battery_readings (device_id, at, temp_dc)
  VALUES (v_a, now() - interval '1 minute', 305);        -- a temperature-only answer
  SELECT count(*) INTO v_cnt FROM farm.battery_readings WHERE device_id = v_a;
  IF v_cnt <> 2 THEN
    RAISE EXCEPTION 'V1 FAILED: expected 2 readings, found %', v_cnt;
  END IF;

  BEGIN
    INSERT INTO farm.battery_readings (device_id, at, pct) VALUES (v_a, now() - interval '2 minutes', 101);
    RAISE EXCEPTION 'V1 FAILED: a level of 101%% was accepted';
  EXCEPTION WHEN check_violation THEN NULL;
  END;
  BEGIN
    INSERT INTO farm.battery_readings (device_id, at, temp_dc) VALUES (v_a, now() - interval '3 minutes', 1501);
    RAISE EXCEPTION 'V1 FAILED: a temperature above the 00010 bound was accepted';
  EXCEPTION WHEN check_violation THEN NULL;
  END;
  BEGIN
    INSERT INTO farm.battery_readings (device_id, at, temp_dc) VALUES (v_a, now() - interval '4 minutes', -401);
    RAISE EXCEPTION 'V1 FAILED: a temperature below the 00010 bound was accepted';
  EXCEPTION WHEN check_violation THEN NULL;
  END;
  BEGIN
    INSERT INTO farm.battery_readings (device_id, at) VALUES (v_a, now() - interval '5 minutes');
    RAISE EXCEPTION 'V1 FAILED: a reading with neither value was accepted';
  EXCEPTION WHEN check_violation THEN NULL;
  END;
  RAISE NOTICE 'V1  ok  readings insert, and the unit checks refuse what the runtime columns refuse';

  -- ============================================================
  -- V2  THE PRUNE KEEPS THE WINDOW. Rows older than the default
  --     keep go; rows inside it stay; the count it returns is the
  --     count it deleted.
  -- ============================================================
  INSERT INTO farm.battery_readings (device_id, at, pct) VALUES
    (v_b, now() - interval '8 days', 50),
    (v_b, now() - interval '7 days 1 minute', 50),
    (v_b, now() - interval '6 days 23 hours', 50),
    (v_b, now() - interval '1 hour', 50),
    (v_b, now(), 50);
  v_deleted := farm.battery_readings_prune();
  IF v_deleted <> 2 THEN
    RAISE EXCEPTION 'V2 FAILED: prune reported % deleted, want 2', v_deleted;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.battery_readings WHERE device_id = v_b;
  IF v_cnt <> 3 THEN
    RAISE EXCEPTION 'V2 FAILED: % rows survived, want the 3 inside seven days', v_cnt;
  END IF;
  -- And a second pass finds nothing left to do: the hourly prune from two
  -- per-host watchdogs costs the second one a scan and nothing else.
  v_deleted := farm.battery_readings_prune();
  IF v_deleted <> 0 THEN
    RAISE EXCEPTION 'V2 FAILED: a second prune deleted % rows', v_deleted;
  END IF;
  -- A tighter keep is honoured when it is at least the detector's window.
  v_deleted := farm.battery_readings_prune(interval '2 hours');
  IF v_deleted <> 1 THEN
    RAISE EXCEPTION 'V2 FAILED: prune(2 hours) deleted % rows, want the one from 6 days 23 hours ago', v_deleted;
  END IF;
  RAISE NOTICE 'V2  ok  prune deletes past the keep, returns the count, and is idempotent';

  -- ============================================================
  -- V3  A KEEP THAT WOULD BLIND THE DETECTOR IS REFUSED. swell.go
  --     reads the last 30 minutes and holds a finding for 60; a
  --     retention below an hour deletes the evidence an open alert
  --     is standing on.
  -- ============================================================
  BEGIN
    PERFORM farm.battery_readings_prune(interval '30 minutes');
    RAISE EXCEPTION 'V3 FAILED: a 30-minute keep was honoured';
  EXCEPTION WHEN invalid_parameter_value THEN NULL;
  END;
  BEGIN
    PERFORM farm.battery_readings_prune(NULL::interval);
    RAISE EXCEPTION 'V3 FAILED: a NULL keep was honoured';
  EXCEPTION WHEN invalid_parameter_value THEN NULL;
  END;
  -- Exactly one hour is the floor, and it is allowed.
  PERFORM farm.battery_readings_prune(interval '1 hour');
  RAISE NOTICE 'V3  ok  a keep below one hour is refused; one hour is the floor';

  -- ============================================================
  -- V4  THE WATCHDOG ROLE CAN DO EXACTLY THE THREE NEW THINGS.
  --     Write a reading, trim the table, and put a line in the
  --     ledger with a rack_slot a human can walk to.
  -- ============================================================
  PERFORM farm.assert_watchdog_writes_history(v_a);
  PERFORM farm.assert_watchdog_prunes();
  PERFORM farm.assert_watchdog_raises(v_a, v_slot_a,
    '{"kind":"temp_rise","value":30,"threshold":20,"unit":"dC/min",'
    '"rack_slot":"R16-U1-H3.1-P1","devpath":"usb:3-1.1","host":"h16"}'::jsonb);
  SELECT detail INTO v_detail FROM farm.events
   WHERE kind = 'battery_anomaly' AND device_id = v_a
   ORDER BY id DESC LIMIT 1;
  IF v_detail IS NULL THEN
    RAISE EXCEPTION 'V4 FAILED: the watchdog role could not write a battery_anomaly event';
  END IF;
  IF v_detail->>'rack_slot' IS NULL OR v_detail->>'rack_slot' = '' THEN
    RAISE EXCEPTION 'V4 FAILED: the anomaly event names no rack_slot; a human cannot walk to a uuid';
  END IF;
  IF (v_detail->>'value')::numeric <= (v_detail->>'threshold')::numeric THEN
    RAISE EXCEPTION 'V4 FAILED: the anomaly event''s value does not exceed its threshold';
  END IF;

  -- The position is MANDATORY, and the schema says so, not just the
  -- writer: an anomaly with the key missing, or present and blank, is
  -- refused. Other kinds keep their free-form detail — the CHECK is
  -- scoped to this one.
  BEGIN
    PERFORM farm.assert_watchdog_raises(v_a, v_slot_a,
      '{"kind":"temp_max","value":470,"threshold":450,"unit":"dC","devpath":"usb:3-1.1","host":"h16"}'::jsonb);
    RAISE EXCEPTION 'V4 FAILED: a battery_anomaly with no rack_slot was accepted';
  EXCEPTION WHEN check_violation THEN NULL;
  END;
  BEGIN
    PERFORM farm.assert_watchdog_raises(v_a, v_slot_a,
      '{"kind":"temp_max","value":470,"threshold":450,"unit":"dC","rack_slot":"","devpath":"usb:3-1.1","host":"h16"}'::jsonb);
    RAISE EXCEPTION 'V4 FAILED: a battery_anomaly with a blank rack_slot was accepted';
  EXCEPTION WHEN check_violation THEN NULL;
  END;
  INSERT INTO farm.events (kind, device_id, actor, detail)
  VALUES ('device_note', v_a, 'assertions_v16', '{"free":"form"}'::jsonb);
  SELECT count(*) INTO v_cnt FROM farm.events WHERE kind = 'battery_anomaly' AND device_id = v_a;
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'V4 FAILED: % battery_anomaly rows for the device, want exactly the one that named a position', v_cnt;
  END IF;
  RAISE NOTICE 'V4  ok  farm_watchdog writes readings, prunes, and raises an event; one without a rack_slot is refused';

  -- ============================================================
  -- V5  ...AND NOTHING ELSE. The one rule: a hot battery is a
  --     reason for a human to walk, never a reason for the health
  --     plane to read or end a lease. Re-asserted, not assumed,
  --     because 00016 is the first migration to widen this role's
  --     grants since 00002 created it.
  -- ============================================================
  BEGIN
    PERFORM farm.assert_watchdog_cannot_read_leases();
    RAISE EXCEPTION 'V5 FAILED: farm_watchdog can read farm.leases';
  EXCEPTION WHEN insufficient_privilege THEN NULL;
  END;
  BEGIN
    PERFORM farm.assert_watchdog_cannot_end_a_lease();
    RAISE EXCEPTION 'V5 FAILED: farm_watchdog can end a lease';
  EXCEPTION WHEN insufficient_privilege THEN NULL;
  END;
  IF has_table_privilege('farm_watchdog', 'farm.leases', 'SELECT, INSERT, UPDATE, DELETE') THEN
    RAISE EXCEPTION 'V5 FAILED: farm_watchdog holds a privilege on farm.leases';
  END IF;
  IF has_table_privilege('farm_watchdog', 'farm.battery_readings', 'UPDATE') THEN
    RAISE EXCEPTION 'V5 FAILED: farm_watchdog can rewrite history; readings are append-only from its side';
  END IF;
  RAISE NOTICE 'V5  ok  farm_watchdog still cannot read or end a lease, and cannot rewrite a reading';

  -- ============================================================
  -- V6  CASCADE. A retired-and-deleted device takes its history
  --     with it; nothing is left pointing at a device that no
  --     longer exists.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.battery_readings WHERE device_id = v_a;
  IF v_cnt = 0 THEN
    RAISE EXCEPTION 'V6 FAILED: the fixture device has no readings to cascade';
  END IF;
  DELETE FROM farm.devices WHERE id = v_a;
  SELECT count(*) INTO v_cnt FROM farm.battery_readings WHERE device_id = v_a;
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'V6 FAILED: % readings survived their device', v_cnt;
  END IF;
  RAISE NOTICE 'V6  ok  deleting a device cascades to its readings';

  -- ============================================================
  -- V7  THE INDEXES THE TWO READERS RELY ON EXIST. The primary key
  --     on (device_id, at) is the "latest N per device" scan; the
  --     prune deletes by time alone and needs the other axis.
  -- ============================================================
  PERFORM 1 FROM pg_indexes
   WHERE schemaname = 'farm' AND tablename = 'battery_readings'
     AND indexname = 'battery_readings_pkey'
     AND indexdef LIKE '%(device_id, at)%';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'V7 FAILED: no (device_id, at) primary key on farm.battery_readings';
  END IF;
  PERFORM 1 FROM pg_indexes
   WHERE schemaname = 'farm' AND tablename = 'battery_readings'
     AND indexname = 'battery_readings_at';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'V7 FAILED: no index on farm.battery_readings(at) for the prune';
  END IF;
  RAISE NOTICE 'V7  ok  the per-device key and the time index are in place';

  -- ============================================================
  -- V8  NO LEASE WAS TOUCHED. Nothing in this file is a job ending,
  --     a user-written deadline elapsing, or a human taking a
  --     device back.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.leases l
    JOIN farm.devices d ON d.id = l.device_id
   WHERE d.host_id = 'h16';
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'V8 FAILED: battery history created % lease row(s)', v_cnt;
  END IF;
  RAISE NOTICE 'V8  ok  no lease was touched';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v16 ASSERTIONS PASSED';
END $$;

ROLLBACK;
