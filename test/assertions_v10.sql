-- Assertions for migration 00010: authorisation on lease re-attach.
--
-- farm.lease_acquire PHASE 1 re-attached on job_id ALONE and handed back
-- the SAME fence — the value that authorises writes to the handset. A job
-- id is published by three read endpoints, so any caller who had read one
-- could become a live lease's holder; the rightful holder's next renew
-- then matched nothing and it aborted a multi-hour run believing it had
-- been legitimately fenced.
--
-- The fix must hold TWO things at once, and every assertion below is
-- about one or the other:
--
--   the pod eviction still re-attaches at the same fence, because a
--   replacement pod is the most ordinary event in a Kubernetes control
--   plane and must never cost a device (DeviceFarmer/STF #663);
--
--   a lease ends when the job says so, when a user-written deadline
--   elapses, or when a human takes it back — NOTHING ELSE, and a failed
--   authorisation check is emphatically nothing else.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v10.sql

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host, three slots, three devices, two tenants.
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r1');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h01','r1','127.0.0.1:5037');
INSERT INTO farm.pools (id) VALUES ('default');
INSERT INTO farm.tenants (id) VALUES ('acme'), ('globex');
INSERT INTO farm.queues (id, tenant_id) VALUES ('q1','acme'), ('q2','globex');

SELECT farm.register_slot('h01','3-1.1','3-1',1,'hub',7,false,'R1-H1-P1');
SELECT farm.register_slot('h01','3-1.2','3-1',2,'hub',7,false,'R1-H1-P2');
SELECT farm.register_slot('h01','3-1.3','3-1',3,'hub',7,false,'R1-H1-P3');

-- Three schedulable handsets, adopted through the enrolment path so the
-- device_runtime rows exist the way the allocator expects them.
SELECT * FROM farm.resolve_device('h01','3-1.1', NULL, '\x01'::bytea, 'SER-1', 'default', '{}'::jsonb);
SELECT * FROM farm.resolve_device('h01','3-1.2', NULL, '\x02'::bytea, 'SER-2', 'default', '{}'::jsonb);
SELECT * FROM farm.resolve_device('h01','3-1.3', NULL, '\x03'::bytea, 'SER-3', 'default', '{}'::jsonb);
UPDATE farm.device_runtime SET adb_state = 'device', health = 'healthy';
UPDATE farm.slots SET state = 'active', rearm_at = now() - interval '1 minute';

DO $$
DECLARE
  a          record;
  b          record;
  v_job      uuid;
  v_job2     uuid;
  v_job3     uuid;
  v_lease3   uuid;
  v_fence3   bigint;
  v_lease    uuid;
  v_fence    bigint;
  v_dev      uuid;
  v_slot     bigint;
  v_inst     uuid := gen_random_uuid();
  v_inst2    uuid := gen_random_uuid();
  v_inst3    uuid := gen_random_uuid();
  v_state    text;
  v_bound    text;
  v_holdinst uuid;
  v_epoch    int;
  v_exp      timestamptz;
  v_floor    bigint;
  v_rearm    timestamptz;
  v_cnt      int;
  v_class    text;
  v_reason   text;
BEGIN
  -- ============================================================
  -- A1  Allocation BINDS the lease to the acquiring principal.
  --     holder is a pod name and changes on eviction; this is the
  --     identity that does not.
  -- ============================================================
  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id, expected_duration)
  VALUES ('acme','q1','default', interval '6 hours') RETURNING id INTO v_job;

  SELECT * INTO a FROM farm.lease_acquire(
    v_job, 'runner-pod-a', v_inst, 'acme', 'ci-bot@acme');
  IF a.lease_id IS NULL THEN RAISE EXCEPTION 'A1 FAILED: no lease granted'; END IF;
  IF a.reattached THEN RAISE EXCEPTION 'A1 FAILED: first acquire reported a reattach'; END IF;
  v_lease := a.lease_id; v_fence := a.fence; v_dev := a.device_id; v_slot := a.slot_id;

  SELECT holder_principal INTO v_bound FROM farm.leases WHERE id = v_lease;
  IF v_bound IS DISTINCT FROM 'ci-bot@acme' THEN
    RAISE EXCEPTION 'A1 FAILED: lease bound to % (expected ci-bot@acme)', v_bound;
  END IF;
  RAISE NOTICE 'A1  ok  allocation binds the lease to its principal (fence %)', v_fence;

  -- ============================================================
  -- A2  THE HEADLINE, HALF ONE: THE POD EVICTION STILL WORKS.
  --
  --     The replacement pod differs from its predecessor in both of
  --     the values that identify a PROCESS — a new pod name and a
  --     freshly minted holder_instance — and agrees on the one that
  --     identifies the WORKLOAD. It must re-attach to the same lease
  --     at the SAME fence: the job's own work may still be running
  --     detached on the phone, and bumping the fence would fence the
  --     job out of its own process. That is #663 by another road.
  -- ============================================================
  SELECT * INTO b FROM farm.lease_acquire(
    v_job, 'runner-pod-b', v_inst2, 'acme', 'ci-bot@acme');
  IF b.lease_id IS NULL THEN
    RAISE EXCEPTION 'A2 FAILED: the eviction re-attach was refused';
  END IF;
  IF b.lease_id <> v_lease THEN RAISE EXCEPTION 'A2 FAILED: a new lease was issued'; END IF;
  IF b.fence <> v_fence THEN
    RAISE EXCEPTION 'A2 FAILED: fence bumped on the eviction re-attach (% -> %)', v_fence, b.fence;
  END IF;
  IF NOT b.reattached THEN RAISE EXCEPTION 'A2 FAILED: reattached flag not set'; END IF;
  IF b.device_id <> v_dev THEN RAISE EXCEPTION 'A2 FAILED: re-attached to a different device'; END IF;
  RAISE NOTICE 'A2  ok  pod eviction re-attaches at the SAME fence (%), new pod and new instance', v_fence;

  -- The replacement is now the renewing process, which is the whole
  -- point of the re-attach: it presents the new holder_instance.
  SELECT holder_instance INTO v_holdinst FROM farm.leases WHERE id = v_lease;
  IF v_holdinst <> v_inst2 THEN
    RAISE EXCEPTION 'A2 FAILED: the replacement pod did not become the holder';
  END IF;
  RAISE NOTICE 'A2b ok  the replacement pod is the renewing instance';

  -- ============================================================
  -- A3  THE HEADLINE, HALF TWO: A STRANGER IS REFUSED.
  --
  --     Same job id, same tenant, a different credential. Today this
  --     succeeded and silently fenced out a running job.
  -- ============================================================
  SELECT holder_epoch INTO v_epoch FROM farm.leases WHERE id = v_lease;
  SELECT expires_at   INTO v_exp   FROM farm.leases WHERE id = v_lease;

  BEGIN
    PERFORM * FROM farm.lease_acquire(
      v_job, 'thief-pod', v_inst3, 'acme', 'someone-else@acme');
    RAISE EXCEPTION 'A3 FAILED: a stranger presenting only the job id took the lease';
  EXCEPTION WHEN SQLSTATE 'DF002' THEN
    RAISE NOTICE 'A3  ok  a re-attach by another principal is refused (DF002)';
  END;

  -- ============================================================
  -- A4  THE REFUSAL DID NOT END THE LEASE.
  --
  --     A lease ends when the job says so, when a user-written
  --     deadline elapses, or when a human takes it back. A failed
  --     authorisation check is none of the three, so every column the
  --     rightful holder depends on must be exactly where A2 left it —
  --     including holder_instance, which farm.lease_renew matches on
  --     and which is the value a takeover actually steals.
  -- ============================================================
  SELECT state, holder_instance, holder_epoch, expires_at, holder_principal
    INTO v_state, v_holdinst, v_cnt, v_exp, v_bound
    FROM farm.leases WHERE id = v_lease;

  IF v_state <> 'held' THEN
    RAISE EXCEPTION 'A4 FAILED: the refusal moved the lease to state %', v_state;
  END IF;
  IF v_holdinst <> v_inst2 THEN
    RAISE EXCEPTION 'A4 FAILED: the refusal changed holder_instance; the rightful holder is fenced';
  END IF;
  IF v_cnt <> v_epoch THEN
    RAISE EXCEPTION 'A4 FAILED: the refusal bumped holder_epoch (% -> %)', v_epoch, v_cnt;
  END IF;
  IF v_bound IS DISTINCT FROM 'ci-bot@acme' THEN
    RAISE EXCEPTION 'A4 FAILED: the refusal rebound the lease to %', v_bound;
  END IF;

  PERFORM 1 FROM farm.leases WHERE id = v_lease AND released_at IS NULL AND release_reason IS NULL;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'A4 FAILED: the refusal wrote an ending onto the lease';
  END IF;

  PERFORM 1 FROM farm.devices WHERE id = v_dev AND current_lease_id = v_lease;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'A4 FAILED: the refusal unbound the device from its lease';
  END IF;
  RAISE NOTICE 'A4  ok  the refusal ended nothing: state, holder_instance, epoch and binding intact';

  -- And the proof that matters to the holder: it can still renew, at
  -- the same fence, with the instance it acquired with.
  PERFORM * FROM farm.lease_renew(v_lease, v_fence, v_inst2);
  IF NOT FOUND THEN
    RAISE EXCEPTION 'A4 FAILED: the rightful holder was fenced by the refusal';
  END IF;
  RAISE NOTICE 'A4b ok  the rightful holder renews normally after the refusal';

  -- ============================================================
  -- A5  A caller confined to another tenant cannot even reach the
  --     lease. tenantScope() moved inside the transaction, so it also
  --     covers callers that never pass through the HTTP handler.
  -- ============================================================
  BEGIN
    PERFORM * FROM farm.lease_acquire(v_job, 'globex-pod', gen_random_uuid(), 'globex', 'ci-bot@globex');
    RAISE EXCEPTION 'A5 FAILED: a caller scoped to another tenant re-attached';
  EXCEPTION WHEN SQLSTATE 'DF001' THEN
    RAISE NOTICE 'A5  ok  a caller scoped to another tenant is refused at acquire';
  END;

  -- The same gate applies to ALLOCATION, not only to re-attach: a
  -- confined caller has no business allocating for a job it does not
  -- own either.
  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id) VALUES ('acme','q1','default')
  RETURNING id INTO v_job2;
  BEGIN
    PERFORM * FROM farm.lease_acquire(v_job2, 'globex-pod', gen_random_uuid(), 'globex', 'ci-bot@globex');
    RAISE EXCEPTION 'A5 FAILED: a caller scoped to another tenant allocated a device';
  EXCEPTION WHEN SQLSTATE 'DF001' THEN
    RAISE NOTICE 'A5b ok  the tenant gate covers allocation as well as re-attach';
  END;
  SELECT count(*) INTO v_cnt FROM farm.leases WHERE job_id = v_job2;
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'A5 FAILED: the refused allocation still created a lease';
  END IF;

  -- ============================================================
  -- A6  The control plane still re-attaches. This is the case the
  --     mechanism CANNOT close and the migration header says so: a
  --     caller presenting no principal is admitted, because SQL cannot
  --     tell the scheduler from a caller that omitted the argument.
  --     Asserted rather than left implicit, because tightening it
  --     would silently break the scheduler's re-attach of a queued job.
  -- ============================================================
  SELECT * INTO b FROM farm.lease_acquire(v_job, 'scheduler-1', gen_random_uuid());
  IF b.lease_id <> v_lease OR b.fence <> v_fence OR NOT b.reattached THEN
    RAISE EXCEPTION 'A6 FAILED: an unattributed control-plane re-attach was refused or moved the fence';
  END IF;
  RAISE NOTICE 'A6  ok  an unattributed control-plane re-attach still works at the same fence';

  -- ...and it is not silent. The ledger names it for what it is, so
  -- "who took my lease" is a query rather than an inference.
  SELECT detail->>'authorised' INTO v_class
    FROM farm.events WHERE kind = 'lease_reattached' AND lease_id = v_lease
   ORDER BY at DESC, id DESC LIMIT 1;
  IF v_class <> 'unattributed' THEN
    RAISE EXCEPTION 'A6 FAILED: an unattributed re-attach was logged as %', v_class;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.events
   WHERE kind = 'lease_reattached' AND lease_id = v_lease;
  IF v_cnt <> 2 THEN
    RAISE EXCEPTION 'A6 FAILED: expected 2 handover rows in the ledger, found %', v_cnt;
  END IF;
  RAISE NOTICE 'A6b ok  every handover is in the ledger, classified (% rows)', v_cnt;

  -- The binding survived it. An unattributed caller may re-attach; it
  -- may not take the lease away from the principal it belongs to.
  SELECT holder_principal INTO v_bound FROM farm.leases WHERE id = v_lease;
  IF v_bound IS DISTINCT FROM 'ci-bot@acme' THEN
    RAISE EXCEPTION 'A6 FAILED: an unattributed re-attach rebound the lease to %', v_bound;
  END IF;
  RAISE NOTICE 'A6c ok  an unattributed re-attach cannot rebind the lease';

  -- ============================================================
  -- A7  The binding is WRITE-ONCE for every writer, not just for
  --     lease_acquire. A binding one function respects is a binding
  --     the next writer silently loses.
  -- ============================================================
  BEGIN
    UPDATE farm.leases SET holder_principal = 'attacker' WHERE id = v_lease;
    RAISE EXCEPTION 'A7 FAILED: a direct UPDATE reassigned the holder principal';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'A7  ok  holder_principal cannot be reassigned by any writer';
  END;
  BEGIN
    UPDATE farm.leases SET holder_principal = NULL WHERE id = v_lease;
    RAISE EXCEPTION 'A7 FAILED: a direct UPDATE blanked the holder principal';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'A7b ok  holder_principal cannot be blanked back to unbound';
  END;

  -- ============================================================
  -- A8  A CONTROL-PLANE LEASE HAS AN OWNER, AND IT IS NOT THE
  --     CALLER. This is the attack that survives everything above if
  --     the reserved binding is dropped: in the shipped topology the
  --     scheduler places the job and the jobrunner runs it, neither
  --     holding an end-user credential, so a lease with no owner on
  --     the row would be re-attachable by any authenticated caller
  --     that had read a job id off a list endpoint.
  -- ============================================================
  SELECT * INTO a FROM farm.lease_acquire(v_job2, 'scheduler-1', gen_random_uuid());
  IF a.lease_id IS NULL THEN RAISE EXCEPTION 'A8 setup FAILED: no lease for job2'; END IF;
  SELECT holder_principal INTO v_bound FROM farm.leases WHERE id = a.lease_id;
  IF v_bound IS DISTINCT FROM 'system:control-plane' THEN
    RAISE EXCEPTION 'A8 FAILED: a control-plane allocation left the lease unowned (%)', v_bound;
  END IF;
  RAISE NOTICE 'A8  ok  a control-plane allocation binds the reserved principal';

  BEGIN
    PERFORM * FROM farm.lease_acquire(v_job2, 'thief-pod', gen_random_uuid(), 'acme', 'ci-bot@acme');
    RAISE EXCEPTION 'A8 FAILED: an authenticated caller re-attached a lease the scheduler placed';
  EXCEPTION WHEN SQLSTATE 'DF002' THEN
    RAISE NOTICE 'A8b ok  an identified caller cannot re-attach a control-plane lease';
  END;

  -- And the reserved name is not a password. A token whose subject
  -- happened to be it would BE the control plane to every check above.
  BEGIN
    PERFORM * FROM farm.lease_acquire(v_job2, 'thief-pod', gen_random_uuid(),
                                      'acme', 'system:control-plane');
    RAISE EXCEPTION 'A8 FAILED: a caller impersonated the control plane';
  EXCEPTION WHEN SQLSTATE 'DF003' THEN
    RAISE NOTICE 'A8c ok  the reserved control-plane principal may not be presented';
  END;

  -- The control plane itself still re-attaches its own lease.
  SELECT * INTO b FROM farm.lease_acquire(v_job2, 'jobrunner-1', gen_random_uuid());
  IF b.lease_id <> a.lease_id OR b.fence <> a.fence THEN
    RAISE EXCEPTION 'A8 FAILED: the control plane could not re-attach its own lease';
  END IF;
  RAISE NOTICE 'A8d ok  the control plane re-attaches its own lease at the same fence';

  -- ============================================================
  -- A8e A lease that PREDATES this column is adoptable exactly once.
  --     Nothing recorded the acquiring identity before holder_principal
  --     existed, so there is no source to backfill from; refusing these
  --     would cost a running job its device at upgrade time. Inserted
  --     directly, because farm.lease_acquire can no longer produce one.
  --     The population only shrinks — every new lease is bound.
  -- ============================================================
  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id) VALUES ('acme','q1','default')
  RETURNING id INTO v_job3;
  INSERT INTO farm.leases (device_id, slot_id, job_id, tenant_id, queue_id,
                           holder, holder_instance, ttl, grace, expires_at, reclaimable_at)
  SELECT d.id, d.current_slot_id, v_job3, 'acme', 'q1', 'pre-upgrade-pod', gen_random_uuid(),
         interval '15 minutes', interval '30 minutes',
         now() + interval '15 minutes', now() + interval '45 minutes'
    FROM farm.devices d WHERE d.current_lease_id IS NULL LIMIT 1
  RETURNING id, fence INTO v_lease3, v_fence3;
  IF v_lease3 IS NULL THEN RAISE EXCEPTION 'A8e setup FAILED: no free device'; END IF;

  SELECT * INTO b FROM farm.lease_acquire(v_job3, 'runner-pod-x', gen_random_uuid(),
                                          'acme', 'ci-bot@acme');
  IF b.lease_id <> v_lease3 THEN RAISE EXCEPTION 'A8e FAILED: adoption issued a new lease'; END IF;
  IF b.fence <> v_fence3 THEN RAISE EXCEPTION 'A8e FAILED: adoption bumped the fence'; END IF;
  SELECT holder_principal INTO v_bound FROM farm.leases WHERE id = v_lease3;
  IF v_bound IS DISTINCT FROM 'ci-bot@acme' THEN
    RAISE EXCEPTION 'A8e FAILED: the first identified holder did not adopt the lease (%)', v_bound;
  END IF;
  RAISE NOTICE 'A8e ok  a pre-migration lease is adopted once, at the same fence';

  BEGIN
    PERFORM * FROM farm.lease_acquire(v_job3, 'runner-pod-y', gen_random_uuid(), 'acme', 'other@acme');
    RAISE EXCEPTION 'A8 FAILED: a second principal took an already-adopted lease';
  EXCEPTION WHEN SQLSTATE 'DF002' THEN
    RAISE NOTICE 'A8f ok  once adopted, a different principal is refused';
  END;

  -- ============================================================
  -- A9  THE HUMAN PATH IS UNTOUCHED. An operator taking a device back
  --     is one of the three ways a lease may legitimately end, and it
  --     must keep working against a lease bound to somebody else —
  --     that is precisely when it is needed. This is the revoke that
  --     internal/api/leases.go performs, statement for statement.
  -- ============================================================
  UPDATE farm.leases
     SET state = 'released', released_at = now(), release_reason = 'operator_revoked'
   WHERE id = v_lease AND state IN ('held','suspect');
  IF NOT FOUND THEN RAISE EXCEPTION 'A9 FAILED: revoke could not close the lease'; END IF;

  UPDATE farm.devices SET fence_floor = nextval('farm.fence_seq'), updated_at = now()
   WHERE id = v_dev RETURNING fence_floor INTO v_floor;
  UPDATE farm.slots SET rearm_at = now() + interval '35 seconds' WHERE id = v_slot;
  INSERT INTO farm.audit_log (actor, action, subject, reason, detail)
  VALUES ('alice','lease.revoke','lease:'||v_lease::text,'assertion: human takes the device back',
          jsonb_build_object('revoked_fence', v_fence));

  SELECT state, release_reason INTO v_state, v_reason FROM farm.leases WHERE id = v_lease;
  IF v_state <> 'released' OR v_reason <> 'operator_revoked' THEN
    RAISE EXCEPTION 'A9 FAILED: revoke left the lease at %/%', v_state, v_reason;
  END IF;
  IF v_floor <= v_fence THEN
    RAISE EXCEPTION 'A9 FAILED: fence_floor % did not rise above the revoked fence %', v_floor, v_fence;
  END IF;
  SELECT rearm_at INTO v_rearm FROM farm.slots WHERE id = v_slot;
  IF v_rearm <= now() THEN
    RAISE EXCEPTION 'A9 FAILED: the slot was not quarantined after the revoke';
  END IF;
  PERFORM 1 FROM farm.devices WHERE id = v_dev AND current_lease_id IS NULL;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'A9 FAILED: the device is still bound to the revoked lease';
  END IF;
  RAISE NOTICE 'A9  ok  operator revoke still ends a bound lease, fences and quarantines';

  -- The ledger recorded it, and named the human rather than the fence.
  SELECT count(*) INTO v_cnt FROM farm.events
   WHERE kind = 'lease_ended' AND lease_id = v_lease
     AND detail->>'ended_by' = 'operator';
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'A9 FAILED: the revoke produced % ending rows in the ledger', v_cnt;
  END IF;
  RAISE NOTICE 'A9b ok  the revoke is in the ledger, classified as an operator ending';

  -- The device is back in the pool, so the revoke really did take it
  -- back rather than merely marking a row.
  UPDATE farm.slots SET rearm_at = now() - interval '1 second' WHERE id = v_slot;
  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id) VALUES ('acme','q1','default')
  RETURNING id INTO v_job;
  SELECT count(*) INTO v_cnt FROM farm.lease_acquire(v_job, 'next-pod', gen_random_uuid(), 'acme', 'other@acme');
  IF v_cnt <> 1 THEN
    RAISE EXCEPTION 'A9 FAILED: nothing could be allocated after the revoke';
  END IF;
  RAISE NOTICE 'A9c ok  the revoked device is allocatable again, by a different principal';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v10 ASSERTIONS PASSED';
END $$;

ROLLBACK;
