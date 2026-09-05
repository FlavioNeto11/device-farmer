-- Assertions for migration 00012: farm.reaper_arm and the component that
-- has never beaten (LEASE-05's remaining hole).
--
-- farm.reaper_arm computed the control-plane gap as min(beat_at) over the
-- rows in farm.component_heartbeat whose name it was given. That table has
-- no seed rows — a component gets one the first time it beats — so a
-- watched name that had NEVER beaten was simply absent from the minimum.
-- The gap read small, nothing was refunded, and TTL+grace ran against
-- leases whose holder had never once been given a chance to renew. With no
-- rows at all, the refund was skipped entirely while armed_at and the
-- quiesce window were still stamped as though the arm had meant something.
--
-- The decision under test: such a name makes the reaper REFUSE TO ARM,
-- loudly, and nothing is reclaimed until every watched component has
-- beaten and an arm succeeds. Not an infinite refund (which would silently
-- disable the reaper forever) and not a shrug (which is the bug).
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v12.sql

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host, two slots, two devices, one tenant. The lease is
-- inserted directly, because farm.trg_leases_guard forbids moving a
-- deadline backwards and a reclaimable lease has its deadlines in the
-- past by definition.
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r12');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h12','r12','127.0.0.1:5037');
INSERT INTO farm.pools (id) VALUES ('default') ON CONFLICT (id) DO NOTHING;
INSERT INTO farm.tenants (id) VALUES ('acme');
INSERT INTO farm.queues (id, tenant_id) VALUES ('q12','acme');

SELECT farm.register_slot('h12','3-1.1','3-1',1,'hub',7,false,'R12-H1-P1');
SELECT farm.register_slot('h12','3-1.2','3-1',2,'hub',7,false,'R12-H1-P2');
SELECT * FROM farm.resolve_device('h12','3-1.1', NULL, '\xc1'::bytea, 'SER-C1', 'default', '{}'::jsonb);
SELECT * FROM farm.resolve_device('h12','3-1.2', NULL, '\xc2'::bytea, 'SER-C2', 'default', '{}'::jsonb);
UPDATE farm.device_runtime SET adb_state = 'device', health = 'healthy';
UPDATE farm.slots SET state = 'active', rearm_at = now() - interval '1 minute';

-- A clean slate for the state under test. The suite runs against a
-- database migrated from empty, so these are no-ops there; they make the
-- file honest against any other.
DELETE FROM farm.component_heartbeat;
DELETE FROM farm.control_plane_gap;
UPDATE farm.reaper_state
   SET enabled = true, quiesce_until = now() - interval '1 hour',
       armed_at = now() - interval '1 hour',
       last_refusal = NULL, last_refusal_at = NULL;

DO $$
DECLARE
  a            record;
  v_job        uuid;
  v_lease      uuid;
  v_dev        uuid;
  v_armed_at   timestamptz;
  v_quiesce    timestamptz;
  v_expires    timestamptz;
  v_reclaim    timestamptz;
  v_refusal    text;
  v_refused_at timestamptz;
  v_state      text;
  v_cnt        int;
  v_comp       text;
  v_watch      text[] := ARRAY['reaper','api','scheduler','ghost'];
BEGIN
  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state)
  VALUES ('acme','q12','default','running') RETURNING id INTO v_job;

  -- Reclaimable right now: suspect, silent for two hours, reclaimable_at
  -- half an hour ago, unprotected, no witness, no gap over its silence.
  INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                           holder, holder_instance, state, ttl, grace,
                           acquired_at, heartbeat_at, expires_at, reclaimable_at)
  SELECT d.id, d.current_slot_id, v_job, 'acme', 'q12',
         'dead-pod', gen_random_uuid(), 'suspect',
         interval '15 minutes', interval '30 minutes',
         now() - interval '2 hours', now() - interval '2 hours',
         now() - interval '1 hour',  now() - interval '30 minutes'
    FROM farm.devices d WHERE d.host_id = 'h12' ORDER BY d.farm_uid LIMIT 1
  RETURNING id, device_id INTO v_lease, v_dev;

  SELECT armed_at, quiesce_until INTO v_armed_at, v_quiesce FROM farm.reaper_state;

  -- ============================================================
  -- A1  NO watched component has ever beaten. The old function's
  --     worst case: v_prev NULL, refund skipped, quiesce stamped
  --     anyway. Now: refused, every name listed, nothing stamped.
  -- ============================================================
  SELECT * INTO a FROM farm.reaper_arm(v_watch, interval '60 seconds');
  IF a.armed THEN RAISE EXCEPTION 'A1 FAILED: armed with no heartbeat row at all'; END IF;
  IF a.unbeaten IS DISTINCT FROM ARRAY['api','ghost','reaper','scheduler'] THEN
    RAISE EXCEPTION 'A1 FAILED: unbeaten = % (expected every watched name, sorted)', a.unbeaten;
  END IF;
  IF a.gap <> interval '0' THEN RAISE EXCEPTION 'A1 FAILED: a refusal refunded %', a.gap; END IF;

  SELECT armed_at, quiesce_until, last_refusal, last_refusal_at
    INTO v_armed_at, v_quiesce, v_refusal, v_refused_at
    FROM farm.reaper_state;
  IF v_armed_at <> now() - interval '1 hour' OR v_quiesce <> now() - interval '1 hour' THEN
    RAISE EXCEPTION 'A1 FAILED: a refusal moved armed_at/quiesce_until (%, %)', v_armed_at, v_quiesce;
  END IF;
  IF v_refusal IS NULL OR v_refused_at IS NULL THEN
    RAISE EXCEPTION 'A1 FAILED: the refusal was not recorded in farm.reaper_state';
  END IF;
  RAISE NOTICE 'A1  ok  with no heartbeat rows at all the arm refuses and stamps nothing';

  -- now() is one instant for this whole transaction, so "was the stamp
  -- rewritten" cannot be read off the clock. Backdate it by hand: a call
  -- that rewrites it puts now() back; one that leaves it alone does not.
  UPDATE farm.reaper_state SET last_refusal_at = now() - interval '1 hour';

  -- ============================================================
  -- A2  THE HEADLINE. Every real component has beaten; one watched
  --     name never has. The arm refuses and NAMES it, and the
  --     reclaimable lease is NOT reclaimed — with the kill switch on
  --     and the quiesce window long closed, the refusal is the only
  --     thing standing between this lease and a sweep.
  -- ============================================================
  PERFORM farm.component_beat('reaper');
  PERFORM farm.component_beat('api');
  PERFORM farm.component_beat('scheduler');

  SELECT * INTO a FROM farm.reaper_arm(v_watch, interval '60 seconds');
  IF a.armed THEN RAISE EXCEPTION 'A2 FAILED: armed with ghost unbeaten'; END IF;
  IF a.unbeaten IS DISTINCT FROM ARRAY['ghost'] THEN
    RAISE EXCEPTION 'A2 FAILED: unbeaten = % (expected {ghost})', a.unbeaten;
  END IF;

  SELECT last_refusal, last_refusal_at INTO v_refusal, v_refused_at FROM farm.reaper_state;
  IF v_refusal NOT LIKE '%ghost%' THEN
    RAISE EXCEPTION 'A2 FAILED: the recorded refusal does not name ghost: %', v_refusal;
  END IF;
  -- A DIFFERENT refusal from A1's: the stamp moves to now, because this
  -- one began now.
  IF v_refused_at <> now() THEN
    RAISE EXCEPTION 'A2 FAILED: last_refusal_at = % after the refusal changed (expected now())', v_refused_at;
  END IF;
  RAISE NOTICE 'A2  ok  a watched component that never beat is refused by name';

  SELECT count(*) INTO v_cnt FROM farm.lease_reclaim(100, interval '35 seconds');
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'A2 FAILED: lease_reclaim took % lease(s) while the arm stood refused', v_cnt;
  END IF;
  SELECT state INTO v_state FROM farm.leases WHERE id = v_lease;
  IF v_state <> 'suspect' THEN
    RAISE EXCEPTION 'A2 FAILED: the lease is % after a refused arm; its holder never had a chance to renew', v_state;
  END IF;
  PERFORM 1 FROM farm.devices WHERE id = v_dev AND current_lease_id = v_lease;
  IF NOT FOUND THEN RAISE EXCEPTION 'A2 FAILED: the device was unbound from its lease'; END IF;
  RAISE NOTICE 'A2b ok  nothing is reclaimed while the refusal stands';

  -- The ledger has one row per CHANGE of refusal, not per attempt: A1's
  -- refusal named four components, A2's names one, so two rows so far —
  -- and a repeat of A2 adds none. The stamp follows the same rule: the
  -- reaper retries every ten seconds, and a retry that rewrote it would
  -- turn "when this refusal began" into "the last time we looked".
  UPDATE farm.reaper_state SET last_refusal_at = now() - interval '1 hour';
  SELECT * INTO a FROM farm.reaper_arm(v_watch, interval '60 seconds');
  IF a.armed THEN RAISE EXCEPTION 'A2 FAILED: the repeated arm did not refuse'; END IF;
  SELECT count(*) INTO v_cnt FROM farm.events WHERE kind = 'reaper_arm_refused';
  IF v_cnt <> 2 THEN
    RAISE EXCEPTION 'A2 FAILED: % reaper_arm_refused rows after three refusals of two kinds (expected 2)', v_cnt;
  END IF;
  SELECT detail->'unbeaten' INTO v_comp FROM farm.events
   WHERE kind = 'reaper_arm_refused' ORDER BY id DESC LIMIT 1;
  IF v_comp <> '["ghost"]' THEN
    RAISE EXCEPTION 'A2 FAILED: the ledger row does not name ghost: %', v_comp;
  END IF;
  SELECT last_refusal_at INTO v_refused_at FROM farm.reaper_state;
  IF v_refused_at <> now() - interval '1 hour' THEN
    RAISE EXCEPTION 'A2 FAILED: a repeat of the same refusal rewrote last_refusal_at to % (expected the hour-old stamp)', v_refused_at;
  END IF;
  RAISE NOTICE 'A2c ok  the ledger and the stamp record each refusal once, naming the component';

  -- ============================================================
  -- A3  The ghost beats. The arm succeeds, the refusal clears, and
  --     the refund math for the components that DID beat is exactly
  --     what it always was: api went quiet 25 minutes ago, so 25
  --     minutes are refunded to every live lease and the gap names
  --     api. The ghost's brand-new row is not the oldest and changes
  --     nothing.
  -- ============================================================
  UPDATE farm.component_heartbeat SET beat_at = now() - interval '25 minutes'
   WHERE component = 'api';
  PERFORM farm.component_beat('ghost');
  SELECT expires_at, reclaimable_at INTO v_expires, v_reclaim FROM farm.leases WHERE id = v_lease;

  SELECT * INTO a FROM farm.reaper_arm(v_watch, interval '60 seconds');
  IF NOT a.armed THEN RAISE EXCEPTION 'A3 FAILED: refused after every watched component beat (%)', a.unbeaten; END IF;
  IF a.unbeaten IS NOT NULL THEN RAISE EXCEPTION 'A3 FAILED: unbeaten = % on a successful arm', a.unbeaten; END IF;
  IF a.gap < interval '24 minutes' OR a.gap > interval '26 minutes' THEN
    RAISE EXCEPTION 'A3 FAILED: gap = % (expected the 25-minute api outage)', a.gap;
  END IF;
  RAISE NOTICE 'A3  ok  once every component has beaten the arm succeeds (gap %)', a.gap;

  SELECT component INTO v_comp FROM farm.control_plane_gap ORDER BY id DESC LIMIT 1;
  IF v_comp <> 'api' THEN
    RAISE EXCEPTION 'A3 FAILED: the gap names % (expected api, the oldest beat)', v_comp;
  END IF;
  PERFORM 1 FROM farm.leases WHERE id = v_lease
     AND expires_at = v_expires + a.gap AND reclaimable_at = v_reclaim + a.gap;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'A3 FAILED: the refund was not added to the live lease''s deadlines';
  END IF;
  RAISE NOTICE 'A3b ok  the refund math for the components that beat is unchanged';

  SELECT armed_at, quiesce_until, last_refusal, last_refusal_at
    INTO v_armed_at, v_quiesce, v_refusal, v_refused_at
    FROM farm.reaper_state;
  IF v_refusal IS NOT NULL OR v_refused_at IS NOT NULL THEN
    RAISE EXCEPTION 'A3 FAILED: the refusal still stands after a successful arm (%)', v_refusal;
  END IF;
  IF v_armed_at <> now() OR v_quiesce <= now() THEN
    RAISE EXCEPTION 'A3 FAILED: the successful arm did not stamp armed_at/quiesce (%, %)', v_armed_at, v_quiesce;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.events WHERE kind = 'reaper_armed';
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'A3 FAILED: % reaper_armed rows (expected 1: the arm that cleared the refusal)', v_cnt;
  END IF;
  RAISE NOTICE 'A3c ok  the refusal is cleared, the reaper is quiesced, and the ledger says so';

  -- ============================================================
  -- A4  The quiesce gate the arm set still holds — restoration rule
  --     unchanged — and once it opens, the same lease IS reclaimed.
  --     Without this the "nothing reclaimed" verdicts above would
  --     pass against a lease that could never be reclaimed at all.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.lease_reclaim(100, interval '35 seconds');
  IF v_cnt <> 0 THEN RAISE EXCEPTION 'A4 FAILED: reclaimed % inside the quiesce window', v_cnt; END IF;

  UPDATE farm.reaper_state SET quiesce_until = now() - interval '1 second';
  DELETE FROM farm.control_plane_gap;   -- the gap over its silence shields it; lift that too
  SELECT count(*) INTO v_cnt FROM farm.lease_reclaim(100, interval '35 seconds');
  IF v_cnt <> 1 THEN RAISE EXCEPTION 'A4 FAILED: reclaimed % with the gate open (expected 1)', v_cnt; END IF;
  SELECT state INTO v_state FROM farm.leases WHERE id = v_lease;
  IF v_state <> 'expired' THEN RAISE EXCEPTION 'A4 FAILED: lease is % after reclaim', v_state; END IF;
  RAISE NOTICE 'A4  ok  with the gate open the lease is reclaimed: the refusal was what held it';

  -- ============================================================
  -- A5  The gate is enforced in farm.lease_reclaim itself, for any
  --     caller. A refusal recorded now shuts it again with everything
  --     else open, and a direct UPDATE that clears the refusal cannot
  --     leave the pair half-set.
  -- ============================================================
  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state)
  VALUES ('acme','q12','default','running') RETURNING id INTO v_job;
  INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                           holder, holder_instance, state, ttl, grace,
                           acquired_at, heartbeat_at, expires_at, reclaimable_at)
  SELECT d.id, d.current_slot_id, v_job, 'acme', 'q12',
         'dead-pod-2', gen_random_uuid(), 'suspect',
         interval '15 minutes', interval '30 minutes',
         now() - interval '2 hours', now() - interval '2 hours',
         now() - interval '1 hour',  now() - interval '30 minutes'
    FROM farm.devices d WHERE d.host_id = 'h12' AND d.current_lease_id IS NULL LIMIT 1
  RETURNING id INTO v_lease;

  DELETE FROM farm.component_heartbeat WHERE component = 'ghost';
  SELECT * INTO a FROM farm.reaper_arm(v_watch, interval '60 seconds');
  IF a.armed THEN RAISE EXCEPTION 'A5 FAILED: armed after ghost''s row was removed'; END IF;
  UPDATE farm.reaper_state SET quiesce_until = now() - interval '1 second';
  SELECT count(*) INTO v_cnt FROM farm.lease_reclaim(100, interval '35 seconds');
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'A5 FAILED: lease_reclaim took % lease(s) with only the refusal gating it', v_cnt;
  END IF;
  RAISE NOTICE 'A5  ok  the refusal alone shuts farm.lease_reclaim, whoever recorded it';

  BEGIN
    UPDATE farm.reaper_state SET last_refusal = NULL;
    RAISE EXCEPTION 'A5 FAILED: last_refusal was cleared while last_refusal_at stood';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'A5b ok  the refusal and its timestamp are set and cleared together';
  END;

  -- ============================================================
  -- The mirror hazard, stated and deliberately NOT changed here: a
  -- component that beat once and was then scaled to zero leaves a
  -- STALE row. That row is not "unbeaten" — it is the oldest beat —
  -- so every arm refunds now() - beat_at to every live lease, for as
  -- long as the row exists. That is the safe direction (nothing is
  -- reclaimed that should not be) and it is documented on
  -- config.DefaultReaperComponents: remove the name from the list
  -- when the component leaves the farm, and delete its row. It is a
  -- different defect from the one this migration closes, and it is
  -- left exactly as it was.
  -- ============================================================

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v12 ASSERTIONS PASSED';
END $$;

ROLLBACK;
