-- Identity-resolution assertions for migration 00011_resolve_ambiguous.sql.
--
-- The defect these encode: farm.resolve_device knew how to notice that two
-- devices contest an identity, flagged the devices, and then told its caller
-- 'adopted_new' anyway — because the ambiguity was recorded in the same
-- variable the adoption rung unconditionally overwrites. internal/enroll
-- copies that answer into farm.identity_observations.resolution, which exists
-- so a wrong adoption can be explained afterwards, so the one record that
-- could explain it said nothing had gone wrong.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v11.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host, one hub, five registered slots, no devices.
--
-- Every device here is created BY farm.resolve_device, because the thing
-- under test is what that function concludes and reports. The two rows
-- inserted directly (V5) are the one case the function cannot reach on its
-- own: a fingerprint already shared by two devices.
--
-- farm.devices.farm_uid is CHECK (farm_uid ~ '^df-[0-9a-f]{32}$'), so those
-- uids are built from md5(), which is exactly 32 lowercase hex characters.
-- farm.devices carries a unique index devices_one_per_slot over
-- current_slot_id WHERE NOT NULL, so the directly-inserted rows sit in no
-- slot at all rather than competing for one.
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r11');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h11','r11','127.0.0.1:5037');
INSERT INTO farm.controllers (host_id, root_bus) VALUES ('h11', 3);
INSERT INTO farm.power_domains (host_id, kind, control)
  VALUES ('h11','per_port','uhubctl');
INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
  SELECT 'h11', c.id, '3-1', 7, true FROM farm.controllers c WHERE c.host_id='h11';
INSERT INTO farm.pools (id) VALUES ('default') ON CONFLICT (id) DO NOTHING;

INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number, usb_path,
                        topo_path, rack_slot)
SELECT 'h11', h.id, p.id, g, '3-1.' || g,
       ('h11.c3.p3_1.p3_1_' || g)::ltree, 'R11-U1-H1-P' || g
  FROM farm.hubs h, farm.power_domains p, generate_series(1,5) g
 WHERE h.host_id='h11' AND p.host_id='h11';

-- --------------------------------------------------------------------
-- Assertions
-- --------------------------------------------------------------------
DO $$
DECLARE
  r        record;
  v_dev1   uuid;
  v_dev2   uuid;
  v_dev3   uuid;
  v_uid1   text;
  v_cnt    int;
  v_fp     bytea := decode('deadbeefdeadbeefdeadbeefdeadbeef', 'hex');
  v_obs    bigint;
BEGIN
  -- ============================================================
  -- V1  A serial nothing else carries is a clean adoption, and
  --     still says so. The fix must not turn every adoption into
  --     a report of ambiguity.
  -- ============================================================
  SELECT * INTO r FROM farm.resolve_device('h11', '3-1.1', NULL, NULL, 'SER-DUP');
  IF r.device_id IS NULL THEN
    RAISE EXCEPTION 'V1 FAILED: the first sighting adopted nothing';
  END IF;
  IF r.resolution <> 'adopted_new' THEN
    RAISE EXCEPTION 'V1 FAILED: first sighting of an unseen serial resolved as %', r.resolution;
  END IF;
  v_dev1 := r.device_id;
  SELECT farm_uid INTO v_uid1 FROM farm.devices WHERE id = v_dev1;
  RAISE NOTICE 'V1  ok  an uncontested serial still adopts as adopted_new';

  -- ============================================================
  -- V2  THE HEADLINE. The same serial in a second slot is a
  --     duplicate-serial collision, discovered by the re-check
  --     that runs AFTER the insert — the common path, because at
  --     the moment of this device's own lookup only one row
  --     carried the serial. Both devices are flagged, and the
  --     CALLER IS TOLD. Before 00011 this returned 'adopted_new'
  --     and the collision left the function unmentioned.
  -- ============================================================
  SELECT * INTO r FROM farm.resolve_device('h11', '3-1.2', NULL, NULL, 'SER-DUP');
  IF r.resolution = 'adopted_new' THEN
    RAISE EXCEPTION 'V2 FAILED: a duplicate-serial adoption was reported as a clean one';
  END IF;
  IF r.resolution <> 'ambiguous' THEN
    RAISE EXCEPTION 'V2 FAILED: expected ambiguous, got %', r.resolution;
  END IF;
  -- The adoption still happened: a physically present handset must get a
  -- row, or a clone serial would make a device permanently unusable.
  IF r.device_id IS NULL THEN
    RAISE EXCEPTION 'V2 FAILED: ambiguity was reported by refusing to adopt the device';
  END IF;
  v_dev2 := r.device_id;
  SELECT count(*) INTO v_cnt FROM farm.devices
   WHERE adb_serial = 'SER-DUP' AND serial_ambiguous;
  IF v_cnt <> 2 THEN
    RAISE EXCEPTION 'V2 FAILED: % of 2 colliding devices carry serial_ambiguous', v_cnt;
  END IF;
  RAISE NOTICE 'V2  ok  the second clone adopts, flags both rows, and reports ambiguous';

  -- ============================================================
  -- V3  THE RUNG NAMED IN THE DEFECT. A third sighting reaches
  --     rung 3 with the serial ALREADY duplicated, so the rung
  --     itself sets ambiguity — and rung 4 used to overwrite that
  --     with 'adopted_new' on its way past.
  -- ============================================================
  SELECT * INTO r FROM farm.resolve_device('h11', '3-1.3', NULL, NULL, 'SER-DUP');
  IF r.resolution <> 'ambiguous' THEN
    RAISE EXCEPTION 'V3 FAILED: rung 3 ambiguity was overwritten, got %', r.resolution;
  END IF;
  IF r.device_id IS NULL THEN
    RAISE EXCEPTION 'V3 FAILED: rung 3 ambiguity adopted nothing';
  END IF;
  v_dev3 := r.device_id;
  RAISE NOTICE 'V3  ok  ambiguity raised at the serial rung survives the adoption rung';

  -- ============================================================
  -- V4  A POSITIVE IDENTIFICATION STILL WINS. The brand we wrote
  --     to the device outranks a contested serial: ambiguity at a
  --     weaker rung is not a verdict, and reporting it here would
  --     throw away a real identification.
  -- ============================================================
  SELECT * INTO r FROM farm.resolve_device('h11', '3-1.1', v_uid1, NULL, 'SER-DUP');
  IF r.resolution <> 'branded_uid' THEN
    RAISE EXCEPTION 'V4 FAILED: a branded device with a duplicated serial resolved as %', r.resolution;
  END IF;
  IF r.device_id <> v_dev1 THEN
    RAISE EXCEPTION 'V4 FAILED: the brand named the wrong device';
  END IF;
  RAISE NOTICE 'V4  ok  a branded uid outranks a duplicated serial';

  -- ============================================================
  -- V5  THE FINGERPRINT RUNG. Two devices sharing one hardware
  --     fingerprint is the case rung 2 exists to adjudicate. It
  --     must not raise (min(uuid) does not exist in PostgreSQL,
  --     which is what 00005 fixed and this migration carries
  --     forward), and it must not report the adoption that
  --     follows as a clean one. There is no fingerprint_ambiguous
  --     column, so this answer is the ONLY record that it
  --     happened.
  -- ============================================================
  INSERT INTO farm.devices (farm_uid, adb_serial, hw_fingerprint, pool_id, host_id)
  VALUES ('df-' || md5('twin-a'), NULL, v_fp, 'default', 'h11'),
         ('df-' || md5('twin-b'), NULL, v_fp, 'default', 'h11');

  SELECT * INTO r FROM farm.resolve_device('h11', '3-1.4', NULL, v_fp, NULL);
  IF r.resolution <> 'ambiguous' THEN
    RAISE EXCEPTION 'V5 FAILED: a contested fingerprint resolved as %', r.resolution;
  END IF;
  IF r.device_id IS NULL THEN
    RAISE EXCEPTION 'V5 FAILED: a contested fingerprint adopted nothing';
  END IF;
  RAISE NOTICE 'V5  ok  a fingerprint shared by two devices resolves ambiguous, not adopted_new';

  -- ============================================================
  -- V6  An ambiguous adoption is a USABLE device, not a stub: it
  --     holds the slot, it has a runtime row, and the occupancy
  --     history says which slot it took. An answer of 'ambiguous'
  --     describes the evidence, never a half-finished write.
  -- ============================================================
  PERFORM 1 FROM farm.devices WHERE id = v_dev3 AND current_slot_id IS NOT NULL;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'V6 FAILED: an ambiguously adopted device holds no slot';
  END IF;
  PERFORM 1 FROM farm.device_runtime WHERE device_id = v_dev3;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'V6 FAILED: an ambiguously adopted device has no runtime row';
  END IF;
  PERFORM 1 FROM farm.slot_occupancy o
    JOIN farm.devices d ON d.id = o.device_id AND d.current_slot_id = o.slot_id
   WHERE o.device_id = v_dev3 AND o.until IS NULL;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'V6 FAILED: an ambiguously adopted device has no open occupancy';
  END IF;
  RAISE NOTICE 'V6  ok  an ambiguous adoption is a complete, usable device row';

  -- ============================================================
  -- V7  The answer must be STORABLE where its caller puts it.
  --     internal/enroll writes this value straight into
  --     farm.identity_observations.resolution, whose CHECK
  --     constraint is a closed vocabulary; a resolution the
  --     function can return but the column cannot hold would be a
  --     23514 in the enrollment loop rather than a bug report.
  -- ============================================================
  SELECT * INTO r FROM farm.resolve_device('h11', '3-1.5', NULL, NULL, 'SER-DUP');
  INSERT INTO farm.identity_observations
    (host_id, usb_path, adb_devpath, adb_serial, resolution, device_id)
  VALUES ('h11', '3-1.5', 'usb:3-1.5', 'SER-DUP', r.resolution, r.device_id)
  RETURNING id INTO v_obs;
  IF v_obs IS NULL THEN
    RAISE EXCEPTION 'V7 FAILED: the observation was not recorded';
  END IF;
  -- And it lands in the partial index built to find exactly these.
  PERFORM 1 FROM farm.identity_observations
   WHERE id = v_obs AND resolution IN ('ambiguous','conflict','unreadable');
  IF NOT FOUND THEN
    RAISE EXCEPTION 'V7 FAILED: the observation is invisible to the conflict index';
  END IF;
  RAISE NOTICE 'V7  ok  the answer is storable and findable in identity_observations';

  -- ============================================================
  -- V8  NO LEASE WAS TOUCHED. Resolution is identity work; it
  --     ends nothing. A lease ends when the job says so, when a
  --     deadline a human wrote elapses, or when a human takes it
  --     back — and nothing in this file is any of those.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.leases l
    JOIN farm.devices d ON d.id = l.device_id
   WHERE d.host_id = 'h11';
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'V8 FAILED: identity resolution created % lease row(s)', v_cnt;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.devices
   WHERE host_id = 'h11' AND current_lease_id IS NOT NULL;
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'V8 FAILED: identity resolution attached a lease to a device';
  END IF;
  RAISE NOTICE 'V8  ok  resolution touched no lease';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL 00011 ASSERTIONS PASSED';
END $$;

ROLLBACK;
