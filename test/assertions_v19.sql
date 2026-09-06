-- Assertions for migration 00019_reattach_no_instance.sql: the handover log
-- names no holder_instance.
--
-- farm.lease_renew matches on the triple (id, fence, holder_instance).
-- lease_id and fence are published for every live lease by /api/v1/fleet and
-- by the event stream, so holder_instance is the entire secret — which is why
-- internal/api/leases.go refuses to select it into any listing.
--
-- 00009's lease_reattached row put it back in circulation: lease_id in the
-- row's own column, 'fence' in the detail, and both instance uuids beside it,
-- in a jsonb blob GET /api/v1/events returns verbatim. This file asserts the
-- ledger row no longer carries either, that it still answers the question it
-- was written to answer, and — because 00019 re-pastes a 230-line function to
-- change two lines of one jsonb — that the authorisation gates in the copied
-- body still fire.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v19.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host, two slots, two adopted handsets, two tenants.
-- --------------------------------------------------------------------
INSERT INTO farm.racks (id) VALUES ('r19');
INSERT INTO farm.hosts (id, rack_id, adb_endpoint) VALUES ('h19','r19','127.0.0.1:5037');
INSERT INTO farm.pools (id) VALUES ('default') ON CONFLICT (id) DO NOTHING;
INSERT INTO farm.tenants (id) VALUES ('acme19'), ('globex19');
INSERT INTO farm.queues (id, tenant_id) VALUES ('q19','acme19'), ('q19b','globex19');

SELECT farm.register_slot('h19','3-1.1','3-1',1,'hub',7,false,'R19-H1-P1');
SELECT farm.register_slot('h19','3-1.2','3-1',2,'hub',7,false,'R19-H1-P2');

SELECT * FROM farm.resolve_device('h19','3-1.1', NULL, '\x11'::bytea, 'SER-19-1', 'default', '{}'::jsonb);
SELECT * FROM farm.resolve_device('h19','3-1.2', NULL, '\x12'::bytea, 'SER-19-2', 'default', '{}'::jsonb);
UPDATE farm.device_runtime SET adb_state = 'device', health = 'healthy';
UPDATE farm.slots SET state = 'active', rearm_at = now() - interval '1 minute';

DO $$
DECLARE
  a        record;
  b        record;
  c        record;
  v_job    uuid;
  v_lease  uuid;
  v_fence  bigint;
  v_inst1  uuid := gen_random_uuid();
  v_inst2  uuid := gen_random_uuid();
  v_detail jsonb;
  v_live   uuid;
  v_cnt    int;
BEGIN
  INSERT INTO farm.jobs (tenant_id, queue_id, pool_id)
  VALUES ('acme19','q19','default') RETURNING id INTO v_job;

  SELECT * INTO a FROM farm.lease_acquire(v_job, 'pod-1', v_inst1, 'acme19', 'ci@acme19');
  IF a.lease_id IS NULL THEN
    RAISE EXCEPTION 'A0 FAILED: the fixture allocated nothing, so nothing below is a test';
  END IF;
  v_lease := a.lease_id;
  v_fence := a.fence;

  -- ============================================================
  -- A1  The re-attach still works, at the same fence. 00019 re-pastes
  --     the function body, and a copy that broke the pod-eviction path
  --     would cost a device on every replacement pod — the loss 00009
  --     exists to prevent.
  -- ============================================================
  SELECT * INTO b FROM farm.lease_acquire(v_job, 'pod-2', v_inst2, 'acme19', 'ci@acme19');
  IF b.lease_id <> v_lease OR b.fence <> v_fence OR NOT b.reattached THEN
    RAISE EXCEPTION 'A1 FAILED: the re-attach moved the lease or the fence';
  END IF;
  SELECT holder_instance INTO v_live FROM farm.leases WHERE id = v_lease;
  IF v_live <> v_inst2 THEN
    RAISE EXCEPTION 'A1 FAILED: the re-attach did not install the new holder_instance';
  END IF;
  RAISE NOTICE 'A1  ok  a re-attach still adopts the lease at the same fence';

  SELECT detail INTO v_detail FROM farm.events
   WHERE kind = 'lease_reattached' AND lease_id = v_lease
   ORDER BY at DESC, id DESC LIMIT 1;
  IF v_detail IS NULL THEN
    RAISE EXCEPTION 'A2 FAILED: the handover was not recorded at all';
  END IF;

  -- ============================================================
  -- A2  Neither key exists. This is the leak itself: the row already
  --     carries lease_id and fence, so an instance uuid beside them is
  --     the whole renew triple in one record.
  -- ============================================================
  IF v_detail ?| ARRAY['prior_instance','new_instance'] THEN
    RAISE EXCEPTION 'A2 FAILED: the ledger row still names an instance key: %', v_detail;
  END IF;
  RAISE NOTICE 'A2  ok  the handover row carries neither prior_instance nor new_instance';

  -- ============================================================
  -- A3  And neither VALUE appears under any other name. Asserting on
  --     the keys alone would pass a rename; this asserts on the secret.
  -- ============================================================
  IF v_detail::text LIKE '%' || v_inst1::text || '%'
     OR v_detail::text LIKE '%' || v_inst2::text || '%' THEN
    RAISE EXCEPTION 'A3 FAILED: an instance uuid is in the ledger row under some other key: %', v_detail;
  END IF;
  RAISE NOTICE 'A3  ok  no instance uuid appears anywhere in the detail document';

  -- ============================================================
  -- A4  The row still answers "who took my lease". Removing the two
  --     keys must not cost the ledger its purpose, and the boolean that
  --     replaced them must be true when the instance really changed.
  -- ============================================================
  IF v_detail->>'authorised' <> 'principal_match'
     OR v_detail->>'prior_holder' <> 'pod-1'
     OR v_detail->>'new_holder' <> 'pod-2'
     OR v_detail->>'prior_principal' <> 'ci@acme19'
     OR (v_detail->>'fence')::bigint <> v_fence
     OR (v_detail->>'holder_epoch')::int <> 1 THEN
    RAISE EXCEPTION 'A4 FAILED: the handover row lost its diagnostic content: %', v_detail;
  END IF;
  IF (v_detail->>'instance_changed')::boolean IS NOT TRUE THEN
    RAISE EXCEPTION 'A4 FAILED: instance_changed is % after a genuine handover', v_detail->>'instance_changed';
  END IF;
  RAISE NOTICE 'A4  ok  the row still names both holders, the class and the epoch';

  -- ============================================================
  -- A5  The boolean is a fact, not a constant. A pod re-attaching with
  --     the instance it already holds is the case an operator must be
  --     able to tell apart from a real handover.
  -- ============================================================
  SELECT * INTO c FROM farm.lease_acquire(v_job, 'pod-2', v_inst2, 'acme19', 'ci@acme19');
  IF c.lease_id <> v_lease OR NOT c.reattached THEN
    RAISE EXCEPTION 'A5 FAILED: a same-instance re-attach was refused';
  END IF;
  SELECT detail INTO v_detail FROM farm.events
   WHERE kind = 'lease_reattached' AND lease_id = v_lease
   ORDER BY at DESC, id DESC LIMIT 1;
  IF (v_detail->>'instance_changed')::boolean IS NOT FALSE THEN
    RAISE EXCEPTION 'A5 FAILED: a same-instance re-attach reported instance_changed = %',
      v_detail->>'instance_changed';
  END IF;
  RAISE NOTICE 'A5  ok  instance_changed distinguishes a handover from a re-attach in place';

  -- ============================================================
  -- A6  The gates in the copied body still fire. 00019 re-pastes 230
  --     lines to change two of them; the cheapest proof that the copy
  --     is the function and not a mutation of it is that its refusals
  --     still refuse, and that a refusal still writes nothing.
  -- ============================================================
  BEGIN
    PERFORM * FROM farm.lease_acquire(v_job, 'globex-pod', gen_random_uuid(), 'globex19', 'ci@globex19');
    RAISE EXCEPTION 'A6 FAILED: a caller scoped to another tenant re-attached';
  EXCEPTION WHEN SQLSTATE 'DF001' THEN
    RAISE NOTICE 'A6  ok  the tenant gate still refuses';
  END;
  BEGIN
    PERFORM * FROM farm.lease_acquire(v_job, 'other-pod', gen_random_uuid(), 'acme19', 'thief@acme19');
    RAISE EXCEPTION 'A6 FAILED: a different principal in the same tenant re-attached';
  EXCEPTION WHEN SQLSTATE 'DF002' THEN
    RAISE NOTICE 'A6b ok  the principal gate still refuses';
  END;
  BEGIN
    PERFORM * FROM farm.lease_acquire(v_job, 'sneaky', gen_random_uuid(), NULL, 'system:control-plane');
    RAISE EXCEPTION 'A6 FAILED: the reserved principal was accepted from a caller';
  EXCEPTION WHEN SQLSTATE 'DF003' THEN
    RAISE NOTICE 'A6c ok  the reserved principal is still refused';
  END;

  SELECT holder_instance INTO v_live FROM farm.leases WHERE id = v_lease;
  IF v_live <> v_inst2 THEN
    RAISE EXCEPTION 'A6 FAILED: a refused re-attach moved the holder_instance';
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.events
   WHERE kind = 'lease_reattached' AND lease_id = v_lease;
  IF v_cnt <> 2 THEN
    RAISE EXCEPTION 'A6 FAILED: expected 2 handover rows, found % — a refusal wrote one', v_cnt;
  END IF;
  RAISE NOTICE 'A6d ok  a refused re-attach leaves the lease and the ledger untouched';

  -- ============================================================
  -- A7  Nothing else in the schema writes an instance uuid into the
  --     timeline. The API redaction is a deny-list naming two keys; it
  --     is only sufficient while no other writer invents a third.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.events ev
   WHERE ev.detail::text LIKE '%' || v_inst1::text || '%'
      OR ev.detail::text LIKE '%' || v_inst2::text || '%';
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'A7 FAILED: % event row(s) carry an instance uuid in their detail', v_cnt;
  END IF;
  SELECT count(*) INTO v_cnt FROM farm.audit_log au
   WHERE au.detail::text LIKE '%' || v_inst1::text || '%'
      OR au.detail::text LIKE '%' || v_inst2::text || '%';
  IF v_cnt <> 0 THEN
    RAISE EXCEPTION 'A7 FAILED: % audit row(s) carry an instance uuid in their detail', v_cnt;
  END IF;
  RAISE NOTICE 'A7  ok  no timeline or audit row anywhere carries a holder_instance';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v19 ASSERTIONS PASSED';
END $$;

ROLLBACK;
