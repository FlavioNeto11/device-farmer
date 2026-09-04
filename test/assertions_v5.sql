-- Assertions for migration 00005: four correctness defects in the core.
--
-- Each one is a code path that existed, was reachable, and did something
-- other than what its own API response claimed. These prove the fix and
-- would fail loudly if anybody restored the old behaviour.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v5.sql

\set ON_ERROR_STOP on
BEGIN;

INSERT INTO farm.racks (id) VALUES ('r1');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES
  ('h01','r1','127.0.0.1:5037'),
  ('h02','r1','127.0.0.1:5038');
INSERT INTO farm.pools (id) VALUES ('default');
INSERT INTO farm.tenants (id) VALUES ('acme');
INSERT INTO farm.queues (id, tenant_id) VALUES ('q1','acme');

SELECT farm.register_slot('h01','3-1.1','3-1',1,'hub',7,false,'R1-H1-P1');
SELECT farm.register_slot('h01','3-1.2','3-1',2,'hub',7,false,'R1-H1-P2');
SELECT farm.register_slot('h02','3-1.1','3-1',1,'hub',7,false,'R2-H1-P1');

DO $$
DECLARE
  v_dev   uuid;
  v_dev2  uuid;
  v_res   text;
  v_cnt   int;
  v_job   uuid;
  a       record;
  v_fence bigint;
  v_floor bigint;
  v_rearm timestamptz;
BEGIN
  -- ============================================================
  -- V1  The hardware-fingerprint rung EXECUTES.
  --     It called min(uuid), which PostgreSQL has no aggregate for, so
  --     the second-strongest identity signal threw the moment two
  --     devices shared a fingerprint — exactly the case it adjudicates.
  -- ============================================================
  SELECT device_id INTO v_dev FROM farm.resolve_device(
    'h01','3-1.1', NULL, '\x1122'::bytea, 'SER-A', 'default', '{}'::jsonb);
  SELECT resolution INTO v_res FROM farm.resolve_device(
    'h01','3-1.1', NULL, '\x1122'::bytea, 'SER-A', 'default', '{}'::jsonb);
  IF v_res <> 'hw_fingerprint' THEN
    RAISE EXCEPTION 'V1 FAILED: fingerprint rung resolved as % (expected hw_fingerprint)', v_res;
  END IF;
  RAISE NOTICE 'V1  ok  fingerprint rung resolves (was: ERROR function min(uuid) does not exist)';

  -- A fingerprint claimed by two devices must identify neither: not a
  -- crash, and not an arbitrary pick between them. Adopt a second device
  -- on the other host, then make it collide.
  SELECT device_id INTO v_dev2 FROM farm.resolve_device(
    'h02','3-1.1', NULL, '\x3344'::bytea, 'SER-B', 'default', '{}'::jsonb);
  UPDATE farm.devices SET hw_fingerprint = '\x1122'::bytea WHERE id = v_dev2;

  SELECT resolution INTO v_res FROM farm.resolve_device(
    'h01','3-1.2', NULL, '\x1122'::bytea, NULL, 'default', '{}'::jsonb);
  IF v_res <> 'adopted_new' THEN
    RAISE EXCEPTION 'V1 FAILED: a fingerprint claimed by two devices resolved to one (%)', v_res;
  END IF;
  RAISE NOTICE 'V1b ok  a fingerprint claimed by two devices identifies neither';

  -- ============================================================
  -- V2  An unknown selector key is REFUSED at acquire.
  --     Silently ignoring a constraint is how a job runs on hardware its
  --     author excluded on purpose.
  -- ============================================================
  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, selector)
  VALUES ('acme','q1','default','{"gpu":"adreno"}'::jsonb) RETURNING id INTO v_job;
  BEGIN
    PERFORM * FROM farm.lease_acquire(v_job, 'h', gen_random_uuid());
    RAISE EXCEPTION 'V2 FAILED: an unknown selector key was accepted';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'V2  ok  unknown selector key refused with the supported list';
  END;

  -- ============================================================
  -- V3  A selector that matches nothing yields NO CAPACITY, and one that
  --     matches yields that device. The column was stored, validated,
  --     echoed by the API and never read by any allocator.
  -- ============================================================
  UPDATE farm.devices SET sdk_int = 29, model = 'Pixel 4a', abis = ARRAY['arm64-v8a'];
  UPDATE farm.device_runtime SET adb_state='device', health='healthy';
  UPDATE farm.slots SET state='active', rearm_at = now() - interval '1 minute';

  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, selector)
  VALUES ('acme','q1','default','{"sdk_min":33}'::jsonb) RETURNING id INTO v_job;
  SELECT count(*) INTO v_cnt FROM farm.lease_acquire(v_job, 'h', gen_random_uuid());
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'V3 FAILED: sdk_min 33 was satisfied by an sdk 29 fleet';
  END IF;
  RAISE NOTICE 'V3  ok  sdk_min excludes a fleet that cannot satisfy it';

  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, selector)
  VALUES ('acme','q1','default','{"sdk_min":28,"abi":"arm64-v8a"}'::jsonb) RETURNING id INTO v_job;
  SELECT count(*) INTO v_cnt FROM farm.lease_acquire(v_job, 'h', gen_random_uuid());
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'V3 FAILED: a satisfiable selector allocated nothing';
  END IF;
  RAISE NOTICE 'V3b ok  a satisfiable selector allocates';

  -- An unknown sdk_int must NOT satisfy a bound.
  UPDATE farm.devices SET sdk_int = NULL;
  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, selector)
  VALUES ('acme','q1','default','{"sdk_min":1}'::jsonb) RETURNING id INTO v_job;
  SELECT count(*) INTO v_cnt FROM farm.lease_acquire(v_job, 'h', gen_random_uuid());
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'V3 FAILED: a device with an unread sdk_int satisfied sdk_min';
  END IF;
  RAISE NOTICE 'V3c ok  an unread sdk_int cannot satisfy a version bound';
  UPDATE farm.devices SET sdk_int = 33;

  -- ============================================================
  -- V4  DRAINING A HOST ACTUALLY STOPS PLACEMENT ON IT.
  --     The API answered "no new leases will be placed on this host" and
  --     the allocator filtered the DEVICE's admin_state, never the
  --     host's, so the sentence was false.
  -- ============================================================
  DELETE FROM farm.leases;
  UPDATE farm.devices SET current_lease_id = NULL;
  UPDATE farm.hosts SET admin_state = 'draining';

  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id) VALUES ('acme','q1','default')
  RETURNING id INTO v_job;
  SELECT count(*) INTO v_cnt FROM farm.lease_acquire(v_job, 'h', gen_random_uuid());
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'V4 FAILED: a draining host was given new work';
  END IF;
  RAISE NOTICE 'V4  ok  a draining host takes no new leases';

  UPDATE farm.hosts SET admin_state = 'enabled';
  SELECT count(*) INTO v_cnt FROM farm.lease_acquire(v_job, 'h', gen_random_uuid());
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'V4 FAILED: undrained host still refuses work';
  END IF;
  RAISE NOTICE 'V4b ok  undraining restores placement';

  -- ============================================================
  -- V5  A max_runtime expiry FENCES AND QUARANTINES like every other
  --     ending. It did neither, so the device went back into the pool
  --     with its floor still at or below the fence the previous holder
  --     was carrying.
  -- ============================================================
  DELETE FROM farm.leases;
  UPDATE farm.devices SET current_lease_id = NULL;
  UPDATE farm.slots SET rearm_at = now() - interval '1 minute';

  -- The deadline is moved on the JOB, not on the lease. farm.leases
  -- guards its own identity — acquired_at included — and that trigger
  -- refused this test's first draft, which is the trigger doing its job:
  -- a lease that could be backdated is a lease whose whole timeline can
  -- be rewritten by anything holding a connection.
  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, max_runtime)
  VALUES ('acme','q1','default', interval '1 hour') RETURNING id INTO v_job;
  SELECT * INTO a FROM farm.lease_acquire(v_job, 'h', gen_random_uuid());
  IF a.lease_id IS NULL THEN RAISE EXCEPTION 'V5 setup FAILED: no lease'; END IF;
  v_fence := a.fence; v_dev := a.device_id;

  UPDATE farm.jobs SET max_runtime = interval '0' WHERE id = v_job;
  PERFORM farm.lease_expire_max_runtime(10, interval '35 seconds');

  SELECT fence_floor INTO v_floor FROM farm.devices WHERE id = v_dev;
  IF v_floor <= v_fence THEN
    RAISE EXCEPTION 'V5 FAILED: fence_floor % did not rise above the expired fence %',
      v_floor, v_fence;
  END IF;
  RAISE NOTICE 'V5  ok  max_runtime raises fence_floor (% -> %)', v_fence, v_floor;

  SELECT s.rearm_at INTO v_rearm FROM farm.slots s
    JOIN farm.devices d ON d.current_slot_id = s.id WHERE d.id = v_dev;
  IF v_rearm <= now() THEN
    RAISE EXCEPTION 'V5 FAILED: slot was not rearm-quarantined after max_runtime';
  END IF;
  RAISE NOTICE 'V5b ok  max_runtime quarantines the slot, like release and reclaim';

  -- ============================================================
  -- V6  A hub-scoped quarantine is visible on the fleet grid.
  --     v_fleet joined on q.device_id only, so the scope the correlation
  --     logic actually opens when a hub sheds its devices was invisible
  --     on the very view an operator stares at during that incident.
  -- ============================================================
  INSERT INTO farm.quarantines (scope, hub_id, host_id, reason)
  SELECT 'hub', hb.id, 'h01', 'assertion: hub shedding devices'
    FROM farm.hubs hb WHERE hb.host_id = 'h01' LIMIT 1;

  SELECT count(*) INTO v_cnt FROM farm.v_fleet f
   WHERE f.host_id = 'h01' AND f.quarantine_id IS NOT NULL
     AND f.quarantine_scope = 'hub';
  IF v_cnt = 0 THEN
    RAISE EXCEPTION 'V6 FAILED: a hub-scoped quarantine is invisible on v_fleet';
  END IF;
  RAISE NOTICE 'V6  ok  hub-scoped quarantine reaches the fleet grid (% devices)', v_cnt;

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v5 ASSERTIONS PASSED';
END $$;

ROLLBACK;
