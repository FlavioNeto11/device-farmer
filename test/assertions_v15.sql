-- Runtime-role assertions for migration 00015_runtime_roles.sql.
--
-- The defect these encode: the Postgres role firewall of 00002_lease.sql was
-- correct DDL that no process assumed. A reaper started with
-- FARM_DB_ROLE=farm_reaper now runs its WHOLE loop under that role, so two
-- things must be true at once — the loop's every statement still works, and
-- the one table the role must not see is still unreadable. Each role below
-- is exercised both ways.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v15.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.
--
-- On method: superusers bypass grants, and the development user is one, so
-- a check against current_user's own privileges would pass on a database
-- where the grants are missing. Every check here therefore runs under
-- SET ROLE, which applies the target role's privileges even to a superuser
-- session. RESET ROLE follows each block, and the file ends with one more.

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host, one hub, three slots with a device each, three
-- short jobs. Job 1 already holds a lease that is overdue in every way
-- the reaper checks — heartbeat two hours old, TTL and grace both
-- elapsed — inserted directly because deadlines only move forwards
-- and farm.lease_acquire cannot produce one in the past.
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r15');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h15','r15','127.0.0.1:5037');
INSERT INTO farm.controllers (host_id, root_bus) VALUES ('h15', 3);
INSERT INTO farm.power_domains (host_id, kind, control)
  VALUES ('h15','per_port','uhubctl');
INSERT INTO farm.hubs (host_id, controller_id, usb_path, port_count, vbus_switchable)
  SELECT 'h15', c.id, '3-1', 7, true FROM farm.controllers c WHERE c.host_id='h15';
INSERT INTO farm.pools (id) VALUES ('default') ON CONFLICT (id) DO NOTHING;
INSERT INTO farm.tenants (id) VALUES ('acme15');
INSERT INTO farm.queues (id, tenant_id) VALUES ('q15','acme15');

INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number, usb_path,
                        topo_path, rack_slot)
SELECT 'h15', h.id, p.id, g, '3-1.' || g,
       ('h15.c3.p3_1.p3_1_' || g)::ltree, 'R15-U1-H1-P' || g
  FROM farm.hubs h, farm.power_domains p, generate_series(1,3) g
 WHERE h.host_id='h15' AND p.host_id='h15';

INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id, model)
SELECT 'df-' || lpad(md5('h15:' || s.usb_path), 32, '0'), 'SER15' || s.port_number,
       'default', 'h15', s.id, 'Pixel Test'
  FROM farm.slots s WHERE s.host_id='h15';

INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health)
SELECT d.id, d.host_id, d.current_slot_id, 'device', 'healthy'
  FROM farm.devices d WHERE d.host_id='h15';

INSERT INTO farm.jobs (id, tenant_id, queue_id, pool_id, expected_duration)
VALUES ('15151515-1515-1515-1515-151515151501','acme15','q15','default', interval '5 minutes'),
       ('15151515-1515-1515-1515-151515151502','acme15','q15','default', interval '5 minutes'),
       ('15151515-1515-1515-1515-151515151503','acme15','q15','default', interval '5 minutes');

INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                         holder, holder_instance, ttl, grace,
                         heartbeat_at, expires_at, reclaimable_at)
SELECT d.id, d.current_slot_id, '15151515-1515-1515-1515-151515151501', 'acme15', 'q15',
       'dead-pod', gen_random_uuid(), interval '15 minutes', interval '30 minutes',
       now() - interval '2 hours', now() - interval '1 hour', now() - interval '30 minutes'
  FROM farm.devices d WHERE d.host_id='h15' ORDER BY d.current_slot_id LIMIT 1;
UPDATE farm.jobs SET state = 'running'
 WHERE id = '15151515-1515-1515-1515-151515151501';

-- --------------------------------------------------------------------
-- Assertions
-- --------------------------------------------------------------------
DO $$
DECLARE
  acq         record;
  v_role      text;
  v_owner     text;
  v_cnt       int;
  v_gap       interval;
  v_overdue   uuid;
  v_reclaimed uuid;
  v_lease     uuid;
  v_fence     bigint;
  v_dev       uuid;
  v_slot      bigint;
  v_ok        boolean;
  v_ended     int;
BEGIN
  SELECT id INTO v_overdue FROM farm.leases
   WHERE job_id = '15151515-1515-1515-1515-151515151501';

  -- ============================================================
  -- M1  MEMBERSHIP. The user migrations run as — the owner of
  --     schema farm — can SET ROLE to each firewalled role. Read
  --     from pg_auth_members rather than pg_has_role(), which
  --     answers true for a superuser whether or not a GRANT exists.
  -- ============================================================
  SELECT r.rolname INTO v_owner
    FROM pg_namespace n JOIN pg_roles r ON r.oid = n.nspowner
   WHERE n.nspname = 'farm';
  FOREACH v_role IN ARRAY ARRAY['farm_reaper','farm_scheduler','farm_watchdog'] LOOP
    PERFORM 1
       FROM pg_auth_members m
       JOIN pg_roles g ON g.oid = m.roleid
       JOIN pg_roles u ON u.oid = m.member
      WHERE g.rolname = v_role AND u.rolname = v_owner;
    IF NOT FOUND THEN
      RAISE EXCEPTION 'M1 FAILED: % is not a member of %, so SET ROLE would be refused', v_owner, v_role;
    END IF;
  END LOOP;
  RAISE NOTICE 'M1  ok  % may assume all three runtime roles', v_owner;

  -- ============================================================
  -- THE REAPER, whole, as farm_reaper.
  -- ============================================================
  SET ROLE farm_reaper;

  -- R1 The firewall. Health is not merely unread by this role — it
  --    is unreadable, for every statement the process will ever run.
  BEGIN
    SELECT count(*) INTO v_cnt FROM farm.device_runtime;
    RAISE EXCEPTION 'R1 FAILED: farm_reaper read % device_runtime row(s)', v_cnt;
  EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'R1  ok  farm_reaper cannot read farm.device_runtime';
  END;

  -- R2 The heartbeat. A reaper that cannot beat is a control-plane
  --    gap that never ends.
  PERFORM farm.component_beat('reaper');
  PERFORM farm.component_beat('api');
  PERFORM farm.component_beat('scheduler');
  PERFORM farm.component_beat('jobrunner');
  RAISE NOTICE 'R2  ok  farm_reaper beats';

  -- R3 Arming on fresh beats: reads the heartbeats, finds no gap, and
  --    writes reaper_state. The gap path is R7, after the reclaim:
  --    lease_reclaim never reclaims across a gap that ended after the
  --    lease's last heartbeat, and in one transaction every gap ends
  --    at now().
  SELECT farm.reaper_arm(ARRAY['reaper','api','scheduler','jobrunner'], interval '60 seconds')
    INTO v_gap;
  IF v_gap <> interval '0' THEN
    RAISE EXCEPTION 'R3 FAILED: fresh beats produced a gap of %', v_gap;
  END IF;
  PERFORM 1 FROM farm.reaper_state WHERE armed_at > now() - interval '1 minute';
  IF NOT FOUND THEN RAISE EXCEPTION 'R3 FAILED: reaper_arm did not record armed_at'; END IF;
  RAISE NOTICE 'R3  ok  farm_reaper arms and writes reaper_state';

  -- R4 The suspect sweep and the max-runtime sweep, which joins jobs.
  SELECT count(*) INTO v_cnt FROM farm.lease_mark_suspect(500);
  IF v_cnt <> 1 THEN RAISE EXCEPTION 'R4 FAILED: expected 1 lease marked suspect, got %', v_cnt; END IF;
  PERFORM * FROM farm.lease_expire_max_runtime(100, interval '35 seconds');
  RAISE NOTICE 'R4  ok  farm_reaper sweeps suspect and max-runtime';

  -- R5 Reclaim. reaper_arm just quiesced the reaper for the longest
  --    TTL it could have missed; open the gate the way P10 does and
  --    take the overdue lease back under the role.
  RESET ROLE;
  UPDATE farm.reaper_state SET quiesce_until = now() - interval '1 second';
  SET ROLE farm_reaper;
  SELECT lease_id INTO v_reclaimed FROM farm.lease_reclaim(100, interval '35 seconds');
  IF v_reclaimed IS DISTINCT FROM v_overdue THEN
    RAISE EXCEPTION 'R5 FAILED: expected to reclaim %, got %', v_overdue, v_reclaimed;
  END IF;
  PERFORM 1 FROM farm.leases WHERE id = v_overdue
     AND state = 'expired' AND release_reason = 'holder_expired';
  IF NOT FOUND THEN RAISE EXCEPTION 'R5 FAILED: the reclaimed lease is not expired/holder_expired'; END IF;
  RAISE NOTICE 'R5  ok  farm_reaper reclaims the overdue lease';

  -- R6 The audit row the Go loop writes for every reclaim, and the
  --    census queries it answers /metrics with.
  INSERT INTO farm.events (kind, device_id, slot_id, lease_id, job_id, actor, detail)
  SELECT 'lease_reclaimed', l.device_id, l.slot_id, l.id, l.job_id, 'reaper',
         jsonb_build_object('release_reason', 'holder_expired')
    FROM farm.leases l WHERE l.id = v_overdue;
  PERFORM j.pool_id, l.tenant_id, count(*)
     FROM farm.leases l JOIN farm.jobs j ON j.id = l.job_id
    WHERE l.state = 'held' GROUP BY 1, 2;
  PERFORM s.host_id, hb.usb_path, count(*)
     FROM farm.slots s JOIN farm.hubs hb ON hb.id = s.hub_id
    WHERE s.rearm_at > now() GROUP BY 1, 2;
  RAISE NOTICE 'R6  ok  farm_reaper audits and takes its census';

  -- R7 The refund path. With the api's last beat five minutes old,
  --    reaper_arm must INSERT the gap row and push quiesce_until — the
  --    two ledger writes 00002 never granted, on the one arm a restored
  --    reaper needs most. now() is fixed for the transaction, so the
  --    gap is exactly five minutes.
  UPDATE farm.component_heartbeat SET beat_at = now() - interval '5 minutes'
   WHERE component = 'api';
  SELECT farm.reaper_arm(ARRAY['reaper','api','scheduler','jobrunner'], interval '60 seconds')
    INTO v_gap;
  IF v_gap <> interval '5 minutes' THEN
    RAISE EXCEPTION 'R7 FAILED: a five-minute-old api beat produced a gap of %', v_gap;
  END IF;
  PERFORM 1 FROM farm.control_plane_gap
    WHERE component = 'api' AND ended_at - started_at = v_gap;
  IF NOT FOUND THEN RAISE EXCEPTION 'R7 FAILED: reaper_arm recorded no control_plane_gap row for api'; END IF;
  PERFORM 1 FROM farm.reaper_state WHERE quiesce_until > now();
  IF NOT FOUND THEN RAISE EXCEPTION 'R7 FAILED: reaper_arm did not quiesce after a gap'; END IF;
  RAISE NOTICE 'R7  ok  farm_reaper records a control-plane gap and quiesces';

  RESET ROLE;

  -- ============================================================
  -- THE SCHEDULER, whole, as farm_scheduler.
  -- ============================================================
  SET ROLE farm_scheduler;

  -- S1 It beats, and it reads health — allocation must.
  PERFORM farm.component_beat('scheduler');
  SELECT count(*) INTO v_cnt FROM farm.device_runtime;
  IF v_cnt < 3 THEN RAISE EXCEPTION 'S1 FAILED: farm_scheduler sees % runtime rows', v_cnt; END IF;
  RAISE NOTICE 'S1  ok  farm_scheduler beats and reads health';

  -- S2 The queue poll: jobs against their tenant and queue caps.
  SELECT count(*) INTO v_cnt
    FROM farm.jobs j
    JOIN farm.queues  q ON q.id = j.queue_id
    JOIN farm.tenants t ON t.id = j.tenant_id
    LEFT JOIN (SELECT l.tenant_id, count(*) AS n FROM farm.leases l
                WHERE l.state IN ('held','suspect') GROUP BY 1) bt ON bt.tenant_id = j.tenant_id
   WHERE j.state = 'queued';
  IF v_cnt <> 2 THEN RAISE EXCEPTION 'S2 FAILED: expected 2 queued candidates, got %', v_cnt; END IF;
  RAISE NOTICE 'S2  ok  farm_scheduler polls the queue';

  -- S3 A placement and its bookkeeping.
  SELECT * INTO acq
    FROM farm.lease_acquire('15151515-1515-1515-1515-151515151502', 'scheduler-1', gen_random_uuid());
  IF acq.lease_id IS NULL THEN RAISE EXCEPTION 'S3 FAILED: lease_acquire placed nothing'; END IF;
  v_lease := acq.lease_id; v_fence := acq.fence;
  UPDATE farm.jobs SET state = 'running', started_at = COALESCE(started_at, now())
   WHERE id = '15151515-1515-1515-1515-151515151502' AND state IN ('queued','allocating');
  IF NOT FOUND THEN RAISE EXCEPTION 'S3 FAILED: the placed job did not move to running'; END IF;
  RAISE NOTICE 'S3  ok  farm_scheduler allocates and marks the job running';

  -- S4 The unwind path: the scheduler's one release, which parks the
  --    slot for the rearm window.
  SELECT farm.lease_release(v_lease, v_fence, 'completed', interval '35 seconds') INTO v_ok;
  IF NOT v_ok THEN RAISE EXCEPTION 'S4 FAILED: lease_release refused the scheduler'; END IF;
  RAISE NOTICE 'S4  ok  farm_scheduler unwinds a lease';

  -- S5 The function-level firewall still stacks on the session one:
  --    lease_reclaim called from a farm_scheduler session runs as
  --    farm_reaper (00002's membership chain) and raises nothing.
  PERFORM * FROM farm.lease_reclaim(100, interval '35 seconds');
  RAISE NOTICE 'S5  ok  lease_reclaim still assumes farm_reaper from a farm_scheduler session';

  RESET ROLE;

  -- ============================================================
  -- THE WATCHDOG, whole, as farm_watchdog.
  -- ============================================================
  SELECT count(*) INTO v_ended FROM farm.leases
   WHERE tenant_id = 'acme15' AND state IN ('expired','released');
  SET ROLE farm_watchdog;

  -- W1 The firewall. The health plane has no path to a lease.
  BEGIN
    SELECT count(*) INTO v_cnt FROM farm.leases;
    RAISE EXCEPTION 'W1 FAILED: farm_watchdog read % lease row(s)', v_cnt;
  EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'W1  ok  farm_watchdog cannot read farm.leases';
  END;

  -- W2 It beats under its per-host key and reads its host and slots.
  PERFORM farm.component_beat('watchdog:h15');
  PERFORM h.id, h.adb_endpoint, h.host_epoch FROM farm.hosts h WHERE h.admin_state <> 'disabled';
  SELECT d.id, s.id INTO v_dev, v_slot
    FROM farm.slots s
    JOIN farm.devices d ON d.current_slot_id = s.id
    LEFT JOIN farm.hubs hb ON hb.id = s.hub_id
   WHERE s.host_id = 'h15' AND d.admin_state <> 'retired'
   ORDER BY s.id DESC LIMIT 1;
  IF v_dev IS NULL THEN RAISE EXCEPTION 'W2 FAILED: the slot query returned no device'; END IF;
  RAISE NOTICE 'W2  ok  farm_watchdog beats and reads host, slots and hubs';

  -- W3 The reconcile write, the shape internal/watchdog runs it in:
  --    a CTE over device_runtime feeding an UPDATE of the same row.
  WITH o AS (
    SELECT v_dev AS device_id, 'h15'::text AS host_id, v_slot AS slot_id,
           'device'::text AS adb_state, 'healthy'::text AS candidate, false AS bad,
           7::bigint AS transport_id, 1::bigint AS host_epoch
  ), c AS (
    SELECT o.*, r.health AS cur_health,
           LEAST(3::numeric, r.flap_credits
                 + EXTRACT(EPOCH FROM (now() - r.flap_updated_at)) / 60.0 * 0.5) AS credits,
           CASE WHEN o.bad THEN r.consec_bad + 1 ELSE 0 END AS consec_bad,
           CASE WHEN o.bad THEN 0 ELSE r.consec_good + 1 END AS consec_good
      FROM farm.device_runtime r JOIN o ON o.device_id = r.device_id
  )
  UPDATE farm.device_runtime r
     SET adb_state = c.adb_state, host_id = c.host_id, slot_id = c.slot_id,
         transport_id = c.transport_id, host_epoch = c.host_epoch,
         consec_bad = c.consec_bad, consec_good = c.consec_good,
         health = c.candidate,
         health_since = CASE WHEN c.candidate <> r.health THEN now() ELSE r.health_since END,
         flap_credits = c.credits, flap_updated_at = now(),
         last_seen_at = now(), updated_at = now()
    FROM c WHERE r.device_id = c.device_id;
  IF NOT FOUND THEN RAISE EXCEPTION 'W3 FAILED: the reconcile write touched no row'; END IF;
  RAISE NOTICE 'W3  ok  farm_watchdog reconciles device_runtime';

  -- W4 The first-sight insert, the last_seen refresh and the battery
  --    columns 00010 constrains.
  INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health)
  VALUES (v_dev, 'h15', v_slot, 'unknown', 'unknown')
  ON CONFLICT (device_id) DO NOTHING;
  UPDATE farm.device_runtime SET last_seen_at = now(), updated_at = now()
   WHERE device_id = ANY(ARRAY[v_dev::text]::uuid[]);
  UPDATE farm.device_runtime r
     SET battery_pct = 50, battery_temp_dc = 300, updated_at = now()
   WHERE r.device_id = v_dev;
  IF NOT FOUND THEN RAISE EXCEPTION 'W4 FAILED: the battery write touched no row'; END IF;
  RAISE NOTICE 'W4  ok  farm_watchdog ensures, touches and writes battery';

  -- W5 The health census behind farm_device_health.
  PERFORM COALESCE(s.host_id, r.host_id, ''), COALESCE(hb.usb_path, ''), r.health, count(*)
     FROM farm.device_runtime r
     LEFT JOIN farm.devices d ON d.id = r.device_id
     LEFT JOIN farm.slots   s ON s.id = COALESCE(d.current_slot_id, r.slot_id)
     LEFT JOIN farm.hubs   hb ON hb.id = s.hub_id
    GROUP BY 1, 2, 3;
  RAISE NOTICE 'W5  ok  farm_watchdog takes its census';

  RESET ROLE;

  -- ============================================================
  -- Z1  NO LEASE ENDED ON THE HEALTH PLANE. Exactly two of this
  --     fixture's leases ended in this file — one reclaimed by the
  --     reaper, one released by the scheduler — and the watchdog's
  --     whole pass changed that count by zero.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.leases
   WHERE tenant_id = 'acme15' AND state IN ('expired','released');
  IF v_cnt <> v_ended OR v_cnt <> 2 THEN
    RAISE EXCEPTION 'Z1 FAILED: % lease(s) ended, % before the watchdog ran', v_cnt, v_ended;
  END IF;
  RAISE NOTICE 'Z1  ok  the watchdog pass ended no lease';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v15 ASSERTIONS PASSED';
END $$;

RESET ROLE;
ROLLBACK;
