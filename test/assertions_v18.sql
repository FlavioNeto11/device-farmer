-- Assertions for migration 00018_lease_endings_grants.sql, and for the ledger
-- surface GET /api/v1/leases/endings serves out of farm.v_lease_endings.
--
-- The file number matches the migration it covers, as every suite here does.
--
-- The requirement is "the way a lease ended is recoverable from an event log
-- at 3am". 00007_lease_events.sql built the log and the view; 00018 decides
-- who may read it, and internal/api/lease_endings.go reads it over HTTP. Three
-- things therefore have to hold at once, and each is checked below:
--
--   * the view answers the question, for every class of ending — the job said
--     so, a human took it back, and the one nobody recorded a reason for;
--   * the API's vocabulary and the database's are the same set, so a filter
--     the operator types cannot silently match nothing;
--   * the roles that may read it are the ones that could already see the same
--     facts, and the health plane still cannot — a view is exactly the shape
--     that reopens the STF #663 firewall quietly.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v18.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.
--
-- On method: superusers bypass grants, and the development user is one, so a
-- check against current_user's own privileges would pass on a database where
-- the GRANT is missing and the REVOKE is imaginary. Every privilege check here
-- runs under SET ROLE, which applies the target role's privileges even to a
-- superuser session, and RESET ROLE follows each block.

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host with three slots and a device in each, three jobs,
-- and three leases inserted directly — farm.lease_acquire would work
-- equally well and is exercised elsewhere; what this file needs is three
-- live leases to END three different ways.
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r19');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h19','r19','127.0.0.1:5037');
INSERT INTO farm.hubs (host_id, usb_path, port_count) VALUES ('h19','3-1',7);
INSERT INTO farm.pools (id) VALUES ('default') ON CONFLICT (id) DO NOTHING;
INSERT INTO farm.tenants (id) VALUES ('acme19');
INSERT INTO farm.queues (id, tenant_id) VALUES ('q19','acme19');

INSERT INTO farm.slots (host_id, hub_id, port_number, usb_path, topo_path, rack_slot)
SELECT 'h19', h.id, g, '3-1.' || g, ('h19.p' || g)::ltree, 'R19-U1-H1-P' || g
  FROM farm.hubs h, generate_series(1,3) g WHERE h.host_id = 'h19';

INSERT INTO farm.devices (farm_uid, adb_serial, pool_id, host_id, current_slot_id, model)
SELECT 'df-' || lpad(md5('h19:' || s.usb_path), 32, '0'), 'SER19' || s.port_number,
       'default', 'h19', s.id, 'Pixel Test'
  FROM farm.slots s WHERE s.host_id = 'h19';

INSERT INTO farm.jobs (id, tenant_id, queue_id, pool_id)
VALUES ('19191919-1919-1919-1919-191919191901','acme19','q19','default'),
       ('19191919-1919-1919-1919-191919191902','acme19','q19','default'),
       ('19191919-1919-1919-1919-191919191903','acme19','q19','default');

-- Acquired an hour ago and beating until ten minutes ago, so held_seconds and
-- heartbeat_age_s are numbers with a shape worth asserting rather than zeros.
INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                         holder, holder_instance, ttl, grace,
                         acquired_at, heartbeat_at, expires_at, reclaimable_at)
SELECT d.id, d.current_slot_id, j.id, 'acme19', 'q19',
       'runner-19-' || s.port_number, gen_random_uuid(),
       interval '15 minutes', interval '30 minutes',
       now() - interval '1 hour', now() - interval '10 minutes',
       now() + interval '15 minutes', now() + interval '45 minutes'
  FROM farm.devices d
  JOIN farm.slots s ON s.id = d.current_slot_id
  JOIN LATERAL (
    SELECT id FROM farm.jobs
     WHERE id::text LIKE '19191919%'
     ORDER BY id OFFSET s.port_number - 1 LIMIT 1) j ON true
 WHERE d.host_id = 'h19';

-- --------------------------------------------------------------------
-- Assertions
-- --------------------------------------------------------------------
DO $$
DECLARE
  r        record;
  v_role   text;
  v_cnt    int;
  v_l1     uuid;
  v_l2     uuid;
  v_l3     uuid;
  v_words  text[];
  v_cols   text[];
  -- The five classes internal/api/lease_endings.go accepts in ?ended_by=.
  -- Sorted, because the comparison below is set equality and array_agg is
  -- given the same order.
  api_classes CONSTANT text[] := ARRAY['deadline','job','operator','reaper','unaccounted'];
  -- The columns internal/api/lease_endings.go selects by name. A rename in
  -- the view would break the route at runtime with a 500 an operator reads
  -- during an incident; here it is a failed assertion before the merge.
  api_columns CONSTANT text[] := ARRAY['ended_at','lease_id','device_id','slot_id','job_id',
                                       'tenant_id','fence','release_reason','ended_by',
                                       'held_seconds','heartbeat_age_s','holder',
                                       'protected','backfilled'];
BEGIN
  SELECT id INTO v_l1 FROM farm.leases WHERE job_id = '19191919-1919-1919-1919-191919191901';
  SELECT id INTO v_l2 FROM farm.leases WHERE job_id = '19191919-1919-1919-1919-191919191902';
  SELECT id INTO v_l3 FROM farm.leases WHERE job_id = '19191919-1919-1919-1919-191919191903';
  IF v_l1 IS NULL OR v_l2 IS NULL OR v_l3 IS NULL THEN
    RAISE EXCEPTION 'fixture FAILED: the three leases were not created';
  END IF;

  -- ============================================================
  -- E1  THE VIEW ANSWERS THE QUESTION. One UPDATE writes state
  --     and release_reason together, which is what all four
  --     ending paths do and what the trigger fires on. Nothing
  --     here inserts into farm.events: if a row does not appear,
  --     the ledger did not record an ending.
  -- ============================================================
  UPDATE farm.leases SET state = 'released', release_reason = 'completed', released_at = now()
   WHERE id = v_l1;

  SELECT count(*) INTO v_cnt FROM farm.v_lease_endings WHERE lease_id = v_l1;
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'E1 FAILED: % ledger rows for a lease that ended once', v_cnt;
  END IF;

  SELECT * INTO r FROM farm.v_lease_endings WHERE lease_id = v_l1;
  IF r.release_reason <> 'completed' OR r.ended_by <> 'job' THEN
    RAISE EXCEPTION 'E1 FAILED: the job''s own release reads as %/%', r.release_reason, r.ended_by;
  END IF;
  IF r.tenant_id <> 'acme19' OR r.holder IS NULL OR r.fence IS NULL OR r.job_id IS NULL THEN
    RAISE EXCEPTION 'E1 FAILED: the ending is missing identity an incident review reads (tenant %, holder %, fence %, job %)',
      r.tenant_id, r.holder, r.fence, r.job_id;
  END IF;
  -- The lease was acquired an hour ago and last beat ten minutes ago. Both
  -- numbers are what separate "the holder said stop" from "the holder went
  -- silent", so both have to be there and be the right size.
  IF r.held_seconds < 3500 OR r.held_seconds > 3700 THEN
    RAISE EXCEPTION 'E1 FAILED: held_seconds is %, want about 3600', r.held_seconds;
  END IF;
  IF r.heartbeat_age_s < 500 OR r.heartbeat_age_s > 700 THEN
    RAISE EXCEPTION 'E1 FAILED: heartbeat_age_s is %, want about 600', r.heartbeat_age_s;
  END IF;
  IF r.backfilled THEN
    RAISE EXCEPTION 'E1 FAILED: a row written at the time claims to be a reconstruction';
  END IF;
  RAISE NOTICE 'E1  ok  the view answers "how did this lease end" for a job-ended lease';

  -- ============================================================
  -- E2  A HUMAN TAKING IT BACK reads as a different class. The
  --     three ways a lease may legitimately end have to be
  --     distinguishable from each other, or the ledger records
  --     that something happened and not what.
  -- ============================================================
  UPDATE farm.leases SET state = 'released', release_reason = 'operator_revoked', released_at = now()
   WHERE id = v_l2;
  SELECT * INTO r FROM farm.v_lease_endings WHERE lease_id = v_l2;
  IF r.ended_by <> 'operator' OR r.release_reason <> 'operator_revoked' THEN
    RAISE EXCEPTION 'E2 FAILED: an operator revoke reads as %/%', r.release_reason, r.ended_by;
  END IF;
  RAISE NOTICE 'E2  ok  an operator revoke is classified as one';

  -- ============================================================
  -- E3  AN ENDING NOBODY ACCOUNTED FOR is named, not blanked.
  --     release_reason is nullable, so a caller CAN close a lease
  --     without saying why — the exact failure this project
  --     exists to prevent. The ledger must report that loudly
  --     rather than leaving an empty cell that reads as a display
  --     bug. GET /api/v1/leases/endings counts these at the top
  --     level for the same reason.
  -- ============================================================
  UPDATE farm.leases SET state = 'expired', released_at = now() WHERE id = v_l3;
  SELECT * INTO r FROM farm.v_lease_endings WHERE lease_id = v_l3;
  IF r.release_reason IS NOT NULL OR r.ended_by <> 'unaccounted' THEN
    RAISE EXCEPTION 'E3 FAILED: an ending with no reason reads as %/%', r.release_reason, r.ended_by;
  END IF;
  RAISE NOTICE 'E3  ok  an ending with no recorded reason is called unaccounted';

  -- ============================================================
  -- E4  THE VOCABULARY THE API ACCEPTS IS THE ONE THE DATABASE
  --     PRODUCES. internal/api/lease_endings.go refuses an
  --     ?ended_by= outside its five words, because answering a
  --     typo with an empty list reads as "nothing ended" at 3am.
  --     That refusal is only safe while the two lists agree: a
  --     sixth class added to farm.lease_ended_by would become
  --     unaskable through the API, silently. This is where that
  --     is caught.
  -- ============================================================
  SELECT array_agg(DISTINCT farm.lease_ended_by(reason) ORDER BY farm.lease_ended_by(reason))
    INTO v_words
    FROM unnest(ARRAY['completed','failed','job_cancelled','max_runtime',
                      'operator_revoked','holder_expired','device_retired',
                      NULL]::text[]) AS reason;
  IF v_words <> api_classes THEN
    RAISE EXCEPTION 'E4 FAILED: farm.lease_ended_by produces %, the API accepts %; update endedByClasses in internal/api/lease_endings.go',
      v_words, api_classes;
  END IF;
  RAISE NOTICE 'E4  ok  the five classes are the same set in SQL and in the API';

  -- ============================================================
  -- E5  THE COLUMNS THE ROUTE SELECTS EXIST. Containment, not
  --     equality: adding a column to the view is additive and
  --     harmless, renaming one breaks the route at runtime.
  -- ============================================================
  SELECT array_agg(attname ORDER BY attname) INTO v_cols
    FROM (SELECT attname::text AS attname
            FROM pg_attribute
           WHERE attrelid = 'farm.v_lease_endings'::regclass
             AND attnum > 0 AND NOT attisdropped) a;
  IF NOT (v_cols @> api_columns) THEN
    RAISE EXCEPTION 'E5 FAILED: farm.v_lease_endings has % and the route reads %', v_cols, api_columns;
  END IF;
  RAISE NOTICE 'E5  ok  the view carries every column the route reads';

  -- ============================================================
  -- E6  THE INDEX THAT BOUNDS THE LISTING EXISTS.
  --     GET /api/v1/leases/endings asks for the newest N endings
  --     in ended_at order, and its comment claims that costs what
  --     N costs rather than what the farm's history costs. What
  --     makes that true is events_kind_time (kind, at DESC) from
  --     00001_core.sql: the view pins kind to a constant, so the
  --     route's ORDER BY is that index's own order. Drop the
  --     index and nothing breaks — the route just quietly becomes
  --     a scan of the whole timeline, discovered during the
  --     incident it was built for. Asserted structurally rather
  --     than by reading a plan, because on a database of a few
  --     rows the planner will pick something else and be right.
  -- ============================================================
  SELECT count(*) INTO v_cnt
    FROM pg_index i
    JOIN pg_class c ON c.oid = i.indexrelid
   WHERE i.indrelid = 'farm.events'::regclass
     AND c.relname = 'events_kind_time'
     AND pg_get_indexdef(i.indexrelid) LIKE '%(kind, at DESC)%';
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'E6 FAILED: farm.events has no events_kind_time (kind, at DESC); the endings listing is now a scan of the whole timeline';
  END IF;
  RAISE NOTICE 'E6  ok  the endings listing is bounded by events_kind_time';

  -- ============================================================
  -- E7  MEMBERSHIP. The user migrations run as can assume each
  --     role below; without it every SET ROLE that follows would
  --     fail for a reason unrelated to the grant being tested.
  --     Read from pg_auth_members rather than pg_has_role(),
  --     which answers true for a superuser whether or not a
  --     GRANT exists.
  -- ============================================================
  FOREACH v_role IN ARRAY ARRAY['farm_reaper','farm_scheduler','farm_watchdog','farm_parker'] LOOP
    PERFORM 1
       FROM pg_auth_members m
       JOIN pg_roles g ON g.oid = m.roleid
       JOIN pg_roles u ON u.oid = m.member
       JOIN pg_namespace n ON n.nspname = 'farm' AND n.nspowner = u.oid
      WHERE g.rolname = v_role;
    IF NOT FOUND THEN
      RAISE EXCEPTION 'E7 FAILED: the owner of schema farm is not a member of %, so SET ROLE would be refused', v_role;
    END IF;
  END LOOP;
  RAISE NOTICE 'E7  ok  the schema owner may assume every role this file tests';

  -- ============================================================
  -- E8  THE REAPER MAY READ THE LEDGER, and still may not read
  --     the timeline it lives in. That second half is the point
  --     of granting a VIEW: farm_reaper holds INSERT on
  --     farm.events and no SELECT (00002_lease.sql), and 00018
  --     did not change that. The view is a capability narrower
  --     than the table, not a way around it.
  -- ============================================================
  SET ROLE farm_reaper;
  SELECT count(*) INTO v_cnt FROM farm.v_lease_endings;
  IF v_cnt < 3 THEN
    RAISE EXCEPTION 'E8 FAILED: farm_reaper sees % endings, want at least the three above', v_cnt;
  END IF;
  BEGIN
    SELECT count(*) INTO v_cnt FROM farm.events;
    RAISE EXCEPTION 'E8 FAILED: the grant on the view widened farm_reaper to the whole timeline';
  EXCEPTION WHEN insufficient_privilege THEN
    NULL;
  END;
  RESET ROLE;
  RAISE NOTICE 'E8  ok  farm_reaper reads the endings and not the timeline';

  -- ============================================================
  -- E9  THE SCHEDULER INHERITS IT. 00002_lease.sql makes
  --     farm_scheduler a member of farm_reaper, so 00018 grants
  --     it nothing directly and it can still read. Asserted so
  --     that revoking that membership does not quietly remove a
  --     read somebody assumed was granted.
  -- ============================================================
  SET ROLE farm_scheduler;
  SELECT count(*) INTO v_cnt FROM farm.v_lease_endings;
  RESET ROLE;
  RAISE NOTICE 'E9  ok  farm_scheduler inherits the read through farm_reaper (% endings)', v_cnt;

  -- ============================================================
  -- E10 THE HEALTH PLANE AND THE PARKING PATH ARE REFUSED. Both
  --     carry REVOKE ALL ON farm.leases — the STF #663 firewall
  --     in DDL — and this view would hand back what that REVOKE
  --     took away: who held what, for how long, and why it
  --     ended. A view is read with its OWNER's rights on
  --     farm.events, so nothing but the absence of a grant stops
  --     it, and nothing but this assertion notices if a later
  --     GRANT ... ON ALL TABLES IN SCHEMA farm supplies one —
  --     which in Postgres includes views.
  -- ============================================================
  FOREACH v_role IN ARRAY ARRAY['farm_watchdog','farm_parker'] LOOP
    BEGIN
      EXECUTE format('SET ROLE %I', v_role);
      EXECUTE 'SELECT count(*) FROM farm.v_lease_endings' INTO v_cnt;
      RESET ROLE;
      RAISE EXCEPTION 'E10 FAILED: % can read the lease endings; the #663 firewall has a view-shaped hole in it', v_role;
    EXCEPTION WHEN insufficient_privilege THEN
      RESET ROLE;
    END;
  END LOOP;
  RAISE NOTICE 'E10 ok  neither the health plane nor the parking path can read an ending';

  -- ============================================================
  -- E11 NOTHING HERE ENDED A LEASE THAT SHOULD NOT HAVE. Three
  --     leases existed, three were ended deliberately by this
  --     file, and the ledger holds exactly three rows for them.
  --     A fourth would mean something in the read path wrote.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.v_lease_endings
   WHERE lease_id IN (v_l1, v_l2, v_l3);
  IF v_cnt <> 3 THEN
    RAISE EXCEPTION 'E11 FAILED: % ledger rows for three endings', v_cnt;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.leases l
    JOIN farm.devices d ON d.id = l.device_id
   WHERE d.host_id = 'h19' AND l.state IN ('held','suspect');
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'E11 FAILED: % fixture leases are still live', v_cnt;
  END IF;
  RAISE NOTICE 'E11 ok  three endings, three ledger rows, nothing else touched';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v18 ASSERTIONS PASSED';
END $$;

RESET ROLE;
ROLLBACK;
