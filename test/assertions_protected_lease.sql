-- Assertions for the protection guarantee: a protected lease is never
-- auto-reclaimed. It is HELD, and a human is paged.
--
-- The guarantee lives in one line of farm.lease_reclaim's candidate CTE
-- (migrations/00012_reaper_arm_unbeaten.sql, `AND l.protected = false`),
-- and until this file existed no SQL suite would have failed if that line
-- were deleted. The Go side does catch it — internal/lease/store_test.go and
-- internal/reaper/reaper_test.go both fail on that deletion, and CI runs
-- them against a database — so what was missing is narrower than "nobody was
-- watching": the guarantee had no statement in the layer it is written in.
-- The SQL suites are what runs when there is no Go binary in the picture,
-- against a schema applied from empty by `make assertions`, by
-- scripts/linux-acceptance.sh, and by a psql session at 3am.
--
-- P10 in test/assertions.sql read as that statement and was not one. That
-- suite is a single transaction, so now() is frozen, and by the time P10
-- ran, the lease it inspected had been renewed past that instant.
-- farm.lease_mark_suspect selects on expires_at < now(), so it matched
-- nothing; the lease never reached 'suspect'; the reclaim CTE requires
-- 'suspect' and matched nothing either. P10 then accepted a lease that no
-- code path had any reason to touch, and deleting the protection line left
-- it green. P10 has been rewritten to assert only what it can reach and to
-- name this file for the rest.
--
-- Two properties make the state P10 cannot reach constructible here:
-- farm.trg_leases_guard is BEFORE UPDATE only, so an INSERT may place its
-- deadlines in the past; and reclamation is a pure function of the lease
-- row plus the reaper gates, so a sibling lease differing in one column is
-- a controlled experiment.
--
-- Hence the shape of this file: TWO leases, identical in every field the
-- reaper reads, on two devices, at the same frozen instant — one protected,
-- one not. The unprotected one is the positive control and it is what makes
-- this file a measurement. Without it, "the protected lease survived" is
-- also what an empty sweep, a shut gate, a typo in the fixture, or a reaper
-- that reclaims nothing at all would report.
--
-- Both sweeps are farm-wide, so every count below is filtered down to these
-- two leases: a suite that asserted farm-wide totals would fail against any
-- database that already had overdue leases in it, which is most of them.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v18.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host, two slots, two devices adopted through
-- farm.resolve_device, one tenant. The leases are inserted directly
-- rather than acquired: farm.lease_acquire stamps deadlines from now(),
-- and a lease that is due for reclamation has them in the past.
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r18');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h18','r18','127.0.0.1:5037');
INSERT INTO farm.pools (id) VALUES ('default') ON CONFLICT (id) DO NOTHING;
INSERT INTO farm.tenants (id) VALUES ('acme18');
INSERT INTO farm.queues (id, tenant_id) VALUES ('q18','acme18');

SELECT farm.register_slot('h18','3-1.1','3-1',1,'Test Hub',7,true,'R18-U1-H1-P1');
SELECT farm.register_slot('h18','3-1.2','3-1',2,'Test Hub',7,true,'R18-U1-H1-P2');
SELECT * FROM farm.resolve_device('h18','3-1.1', NULL, '\xd1'::bytea, 'SER-D1', 'default', '{}'::jsonb);
SELECT * FROM farm.resolve_device('h18','3-1.2', NULL, '\xd2'::bytea, 'SER-D2', 'default', '{}'::jsonb);

-- Both devices healthy and both slots out of quarantine. Neither fact is
-- an input to reclamation — health is unreadable to the reaper role — but
-- a fixture that left them unset would leave the reader wondering.
UPDATE farm.device_runtime SET adb_state = 'device', health = 'healthy'
 WHERE host_id = 'h18';
UPDATE farm.slots SET state = 'active', rearm_at = now() - interval '1 minute'
 WHERE host_id = 'h18';

-- The reaper's gates, all open: the kill switch on, the quiesce window
-- long closed, no standing refusal from farm.reaper_arm (the gate added by
-- 00012). Any one of these left shut would stop the sweep before it ever
-- looked at a lease, and the positive control below is what proves they
-- are open at the instant that matters.
DELETE FROM farm.control_plane_gap;
UPDATE farm.reaper_state
   SET enabled         = true,
       quiesce_until   = now() - interval '1 hour',
       armed_at        = now() - interval '1 minute',
       last_refusal    = NULL,
       last_refusal_at = NULL;

-- --------------------------------------------------------------------
-- Assertions
-- --------------------------------------------------------------------
DO $$
DECLARE
  r          record;
  v_job_c    uuid;
  v_job_p    uuid;
  v_dev_c    uuid;
  v_dev_p    uuid;
  v_slot_c   bigint;
  v_slot_p   bigint;
  v_lease_c  uuid;
  v_lease_p  uuid;
  v_fence_p  bigint;
  v_floor_c  bigint;
  v_fence_c  bigint;
  v_cnt      int;
  v_hit_c    int;
  v_hit_p    int;
BEGIN
  SELECT d.id, d.current_slot_id INTO v_dev_c, v_slot_c
    FROM farm.devices d WHERE d.host_id = 'h18' ORDER BY d.farm_uid LIMIT 1;
  SELECT d.id, d.current_slot_id INTO v_dev_p, v_slot_p
    FROM farm.devices d WHERE d.host_id = 'h18' AND d.id <> v_dev_c LIMIT 1;
  IF v_dev_p IS NULL THEN
    RAISE EXCEPTION 'fixture: two adopted devices expected on h18';
  END IF;
  SELECT fence_floor INTO v_floor_c FROM farm.devices WHERE id = v_dev_c;

  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, expected_duration)
  VALUES ('acme18','q18','default','running', interval '5 minutes')
  RETURNING id INTO v_job_c;
  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, state, protected, expected_duration)
  VALUES ('acme18','q18','default','running', true, interval '6 hours')
  RETURNING id INTO v_job_p;

  -- Two holders that went silent eight hours ago and never came back. Every
  -- column farm.lease_reclaim reads is equal across the pair — same ttl,
  -- same grace, same deadlines, no witness, no control-plane gap over
  -- either heartbeat. The only difference in the farm is `protected`.
  INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                           holder, holder_instance, state, protected, ttl, grace,
                           acquired_at, heartbeat_at, expires_at, reclaimable_at)
  VALUES (v_dev_c, v_slot_c, v_job_c, 'acme18', 'q18',
          'runner-short', gen_random_uuid(), 'held', false,
          interval '15 minutes', interval '30 minutes',
          now() - interval '8 hours', now() - interval '8 hours',
          now() - interval '2 hours', now() - interval '90 minutes')
  RETURNING id, fence INTO v_lease_c, v_fence_c;

  INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                           holder, holder_instance, state, protected, ttl, grace,
                           acquired_at, heartbeat_at, expires_at, reclaimable_at)
  VALUES (v_dev_p, v_slot_p, v_job_p, 'acme18', 'q18',
          'runner-long', gen_random_uuid(), 'held', true,
          interval '15 minutes', interval '30 minutes',
          now() - interval '8 hours', now() - interval '8 hours',
          now() - interval '2 hours', now() - interval '90 minutes')
  RETURNING id, fence INTO v_lease_p, v_fence_p;

  -- ============================================================
  -- H1  The sweep PAGES. It reports both leases and it reports the
  --     protected one AS protected. That boolean is the page: the
  --     reaper's sweep loop branches on it to log "held indefinitely
  --     until a human acts" and to count the suspect under the
  --     protected label. Marking a lease suspect releases nothing; it
  --     is the notification, and for a protected lease it is the
  --     whole of the automatic response.
  -- ============================================================
  SELECT count(*) FILTER (WHERE lease_id = v_lease_c AND NOT protected),
         count(*) FILTER (WHERE lease_id = v_lease_p AND protected)
    INTO v_hit_c, v_hit_p
    FROM farm.lease_mark_suspect(500);
  IF v_hit_c <> 1 OR v_hit_p <> 1 THEN
    RAISE EXCEPTION 'H1 FAILED: sweep reported control % and protected % row(s), expected one each, flagged truthfully',
      v_hit_c, v_hit_p;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.leases
   WHERE id IN (v_lease_c, v_lease_p) AND state = 'suspect' AND released_at IS NULL;
  IF v_cnt <> 2 THEN
    RAISE EXCEPTION 'H1 FAILED: the sweep did not leave both leases suspect and unreleased';
  END IF;
  RAISE NOTICE 'H1  ok  the sweep names the protected lease as protected and releases nothing';

  -- ============================================================
  -- H2  THE HEADLINE. One reclaim call sees two leases that are
  --     equally overdue, equally silent, and equally reclaimable in
  --     every respect but one. It takes the unprotected one and
  --     leaves the protected one where it is.
  --
  --     The control is asserted FIRST and deliberately: it is what
  --     turns "the protected lease survived" from a tautology into a
  --     measurement. If a future gate, a fixture slip, or a shut kill
  --     switch stops the reaper reaching either lease, this line
  --     fails rather than the file passing on a sweep that did
  --     nothing at all.
  -- ============================================================
  SELECT count(*) FILTER (WHERE lease_id = v_lease_c),
         count(*) FILTER (WHERE lease_id = v_lease_p)
    INTO v_hit_c, v_hit_p
    FROM farm.lease_reclaim(100, interval '35 seconds');
  IF v_hit_c <> 1 THEN
    RAISE EXCEPTION 'H2 FAILED: the unprotected control was NOT reclaimed, so nothing below means anything';
  END IF;
  IF v_hit_p <> 0 THEN
    RAISE EXCEPTION 'H2 FAILED: the PROTECTED lease was reclaimed';
  END IF;
  RAISE NOTICE 'H2  ok  same instant, same deadlines: control reclaimed, protected held';

  -- ============================================================
  -- H3  The protected lease is UNTOUCHED, not merely unreleased. Its
  --     fence still stands, so the holder's ongoing work is still
  --     authorised, and its slot was not rearm-quarantined: severing
  --     the socket is exactly the harm the protection exists to
  --     prevent, and doing it while declining to release the row
  --     would be the same loss under a different name.
  -- ============================================================
  SELECT * INTO r FROM farm.leases WHERE id = v_lease_p;
  IF r.state <> 'suspect' OR r.released_at IS NOT NULL OR r.release_reason IS NOT NULL THEN
    RAISE EXCEPTION 'H3 FAILED: protected lease is now state %, released %, reason %',
      r.state, r.released_at, r.release_reason;
  END IF;
  IF r.fence <> v_fence_p OR NOT r.protected THEN
    RAISE EXCEPTION 'H3 FAILED: protected lease changed identity (fence % -> %, protected %)',
      v_fence_p, r.fence, r.protected;
  END IF;
  PERFORM 1 FROM farm.devices WHERE id = v_dev_p AND current_lease_id = v_lease_p;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'H3 FAILED: the device stopped pointing at the protected lease';
  END IF;
  PERFORM 1 FROM farm.slots WHERE id = v_slot_p AND rearm_at > now();
  IF FOUND THEN
    RAISE EXCEPTION 'H3 FAILED: the protected holder''s slot was rearm-quarantined';
  END IF;
  RAISE NOTICE 'H3  ok  the protected lease keeps its fence, its device and its socket';

  -- ============================================================
  -- H4  The control got the FULL reclamation, which is what makes it
  --     a control: the whole mechanism ran, at this instant, on the
  --     sibling row. Terminal state, reaper's release reason, fence
  --     floor raised past the dead holder's fence, device freed, slot
  --     quarantined for longer than the proxy's self-fence.
  -- ============================================================
  SELECT * INTO r FROM farm.leases WHERE id = v_lease_c;
  IF r.state <> 'expired' OR r.release_reason <> 'holder_expired' OR r.released_at IS NULL THEN
    RAISE EXCEPTION 'H4 FAILED: control ended as state %, reason %', r.state, r.release_reason;
  END IF;
  PERFORM 1 FROM farm.devices
   WHERE id = v_dev_c AND current_lease_id IS NULL
     AND fence_floor > v_fence_c AND fence_floor > v_floor_c;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'H4 FAILED: control device not freed and fenced above %', v_fence_c;
  END IF;
  PERFORM 1 FROM farm.slots WHERE id = v_slot_c AND rearm_at > now();
  IF NOT FOUND THEN
    RAISE EXCEPTION 'H4 FAILED: control slot not rearm-quarantined';
  END IF;
  RAISE NOTICE 'H4  ok  the control got the whole reclamation: fenced, freed, quarantined';

  -- ============================================================
  -- H5  The audit trail agrees. farm.trg_leases_ledger writes one
  --     lease_ended row per ending, attributed to the class of ender;
  --     the control's says 'reaper'. The protected lease has no row,
  --     because it did not end. An incident review that reads the
  --     ledger sees the same story this file just asserted.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.events
   WHERE kind = 'lease_ended' AND lease_id = v_lease_p;
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'H5 FAILED: the protected lease has an ending in the ledger';
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.events
   WHERE kind = 'lease_ended' AND lease_id = v_lease_c
     AND actor = 'reaper' AND detail ->> 'release_reason' = 'holder_expired';
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'H5 FAILED: the control has % reaper ending(s) in the ledger', v_cnt;
  END IF;
  RAISE NOTICE 'H5  ok  one ledger ending, attributed to the reaper, for the control only';

  -- ============================================================
  -- H6  HELD, not deferred. The reaper does not come back for it on
  --     the next tick and finish the job: it has been past its
  --     reclaimable_at for ninety minutes and silent for eight hours,
  --     and further passes still take nothing. This is what "hold and
  --     page" costs — the device stays out of the pool until a human
  --     looks, and that is the intended price.
  -- ============================================================
  FOR pass IN 1..2 LOOP
    SELECT count(*) FILTER (WHERE lease_id IN (v_lease_c, v_lease_p))
      INTO v_hit_p
      FROM farm.lease_reclaim(100, interval '35 seconds');
    IF v_hit_p <> 0 THEN
      RAISE EXCEPTION 'H6 FAILED: reclaim pass % took % more lease(s)', pass, v_hit_p;
    END IF;
  END LOOP;
  PERFORM 1 FROM farm.leases WHERE id = v_lease_p AND state = 'suspect';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'H6 FAILED: the protected lease did not survive the later passes';
  END IF;
  RAISE NOTICE 'H6  ok  repeated passes never come back for it';

  -- ============================================================
  -- H7  The last thing that could be wrong: that something else in
  --     this row — its holder name, its device, its job — was doing
  --     the holding, and `protected` merely came along. Clear the one
  --     column, change nothing else, and the same lease at the same
  --     instant is taken by the same call.
  --
  --     Removing protection is an operator act, and the reason it is
  --     spelled as a bare UPDATE here rather than through the
  --     operator surface is that the surface would end the lease
  --     itself, leaving nothing for the reaper to prove a point with.
  -- ============================================================
  UPDATE farm.leases SET protected = false WHERE id = v_lease_p;
  SELECT count(*) FILTER (WHERE lease_id = v_lease_p) INTO v_cnt
    FROM farm.lease_reclaim(100, interval '35 seconds');
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'H7 FAILED: unprotected, the held-back lease was still not reclaimed (% rows)', v_cnt;
  END IF;
  SELECT * INTO r FROM farm.leases WHERE id = v_lease_p;
  IF r.state <> 'expired' THEN
    RAISE EXCEPTION 'H7 FAILED: lease is % after the unprotected reclaim', r.state;
  END IF;
  RAISE NOTICE 'H7  ok  protection alone was holding it: cleared, it is reclaimed at once';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v18 ASSERTIONS PASSED';
END $$;

ROLLBACK;
