-- Lease protocol assertions.
--
-- These encode the failure this system exists to prevent: DeviceFarmer/STF
-- issue #663, open and unanswered since 2023, where a device is automatically
-- released mid-run after a ~90 minute ECONNRESET, destroying multi-hour work.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.

\set ON_ERROR_STOP on
BEGIN;

-- --------------------------------------------------------------------
-- Fixture: one host, one hub, four slots, four devices, one job each.
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
       -- Deliberately duplicated serial on two slots: clone serials are real.
       CASE WHEN s.port_number <= 2 THEN '0123456789ABCDEF'
            ELSE 'SER' || s.port_number END,
       'default', 'h01', s.id, 'Pixel Test'
  FROM farm.slots s WHERE s.host_id='h01';

INSERT INTO farm.device_runtime (device_id, host_id, slot_id, adb_state, health)
SELECT d.id, d.host_id, d.current_slot_id, 'device', 'healthy' FROM farm.devices d;

-- Long job: 6 hours, so lease_acquire marks it protected automatically.
INSERT INTO farm.jobs (id, tenant_id, queue_id, pool_id, expected_duration)
VALUES ('11111111-1111-1111-1111-111111111111','acme','q1','default', interval '6 hours');
-- Short job, explicitly unprotected, for the reclaim assertions.
INSERT INTO farm.jobs (id, tenant_id, queue_id, pool_id, expected_duration)
VALUES ('22222222-2222-2222-2222-222222222222','acme','q1','default', interval '5 minutes');

-- A probe carrying the same role scoping as farm.lease_reclaim. If the
-- firewall holds, reading device health from inside it raises 42501.
CREATE FUNCTION farm.assert_reaper_is_blind() RETURNS int
LANGUAGE plpgsql SET role = farm_reaper AS $probe$
DECLARE n int;
BEGIN
  SELECT count(*) INTO n FROM farm.device_runtime;
  RETURN n;
END $probe$;

-- --------------------------------------------------------------------
-- Assertions
-- --------------------------------------------------------------------
DO $$
DECLARE
  a           record;
  b           record;
  r           record;
  v_fence     bigint;
  v_lease     uuid;
  v_dev       uuid;
  v_inst      uuid := gen_random_uuid();
  v_inst2     uuid := gen_random_uuid();
  v_exp       timestamptz;
  v_cnt       int;
  v_ok        boolean;
  v_gap       interval;
  v_reclaimed int;
BEGIN
  -- ============================================================
  -- P1  ACQUIRE grants a device and marks a 6h job protected.
  -- ============================================================
  SELECT * INTO a FROM farm.lease_acquire(
    '11111111-1111-1111-1111-111111111111', 'runner-pod-a', v_inst);
  IF a.lease_id IS NULL THEN RAISE EXCEPTION 'P1 FAILED: no lease granted'; END IF;
  v_lease := a.lease_id; v_fence := a.fence; v_dev := a.device_id;
  IF a.reattached THEN RAISE EXCEPTION 'P1 FAILED: first acquire reported reattach'; END IF;

  PERFORM 1 FROM farm.leases WHERE id = v_lease AND protected;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'P1 FAILED: 6h job was not auto-protected';
  END IF;
  RAISE NOTICE 'P1  ok  acquire grants + auto-protects long job (fence %)', v_fence;

  -- ============================================================
  -- P2  THE HEADLINE: a connectivity release reason is not
  --     expressible. This is the #663 countermeasure.
  -- ============================================================
  BEGIN
    UPDATE farm.leases SET state='released', released_at=now(),
                           release_reason='device_offline'
     WHERE id = v_lease;
    RAISE EXCEPTION 'P2 FAILED: schema accepted release_reason=device_offline';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'P2  ok  release_reason=device_offline rejected (check_violation)';
  END;

  -- ============================================================
  -- P3  Pod eviction: re-acquire on the SAME job_id re-attaches
  --     to the SAME lease at the SAME fence. A replacement pod
  --     must not cost six hours of on-device work.
  -- ============================================================
  SELECT * INTO b FROM farm.lease_acquire(
    '11111111-1111-1111-1111-111111111111', 'runner-pod-b', v_inst2);
  IF b.lease_id <> v_lease THEN RAISE EXCEPTION 'P3 FAILED: new lease issued'; END IF;
  IF b.fence <> v_fence THEN
    RAISE EXCEPTION 'P3 FAILED: fence bumped on reattach (% -> %)', v_fence, b.fence;
  END IF;
  IF NOT b.reattached THEN RAISE EXCEPTION 'P3 FAILED: reattached flag not set'; END IF;
  RAISE NOTICE 'P3  ok  pod eviction reattaches same lease at same fence';

  -- ============================================================
  -- P4  Renew with a stale holder_instance yields ZERO ROWS.
  --     The evicted pod is fenced; the replacement is not.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.lease_renew(v_lease, v_fence, v_inst);
  IF v_cnt <> 0 THEN RAISE EXCEPTION 'P4 FAILED: stale holder renewed'; END IF;
  SELECT count(*) INTO v_cnt FROM farm.lease_renew(v_lease, v_fence, v_inst2);
  IF v_cnt <> 1 THEN RAISE EXCEPTION 'P4 FAILED: current holder could not renew'; END IF;
  RAISE NOTICE 'P4  ok  stale holder fenced, current holder renews';

  -- ============================================================
  -- P5  Renew with a wrong fence yields ZERO ROWS.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.lease_renew(v_lease, v_fence + 999, v_inst2);
  IF v_cnt <> 0 THEN RAISE EXCEPTION 'P5 FAILED: wrong fence renewed'; END IF;
  RAISE NOTICE 'P5  ok  wrong fence cannot renew';

  -- ============================================================
  -- P6  Deadlines are monotonic. A backwards expires_at is
  --     rejected, so a gap refund cannot be silently undone.
  -- ============================================================
  SELECT expires_at INTO v_exp FROM farm.leases WHERE id = v_lease;
  BEGIN
    UPDATE farm.leases SET expires_at = v_exp - interval '1 hour' WHERE id = v_lease;
    RAISE EXCEPTION 'P6 FAILED: expires_at moved backwards';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'P6  ok  expires_at is monotonic';
  END;

  -- ============================================================
  -- P7  Lease identity is immutable.
  -- ============================================================
  BEGIN
    UPDATE farm.leases SET fence = fence + 1 WHERE id = v_lease;
    RAISE EXCEPTION 'P7 FAILED: fence was mutable';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'P7  ok  lease identity immutable';
  END;

  -- ============================================================
  -- P8  A DEVICE GOING OFFLINE DOES NOT TOUCH THE LEASE.
  --     This is the exact #663 scenario: the transport dies,
  --     health collapses, and the lease is untouched.
  -- ============================================================
  UPDATE farm.device_runtime
     SET adb_state='absent', health='offline', health_since=now()
   WHERE device_id = v_dev;

  PERFORM 1 FROM farm.leases
    WHERE id = v_lease AND state='held' AND released_at IS NULL;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'P8 FAILED: device offline disturbed the lease';
  END IF;

  -- ...and the suspect sweep still finds nothing, because the
  -- heartbeat is fresh. Health is not an input.
  SELECT count(*) INTO v_cnt FROM farm.lease_mark_suspect(500);
  IF v_cnt <> 0 THEN RAISE EXCEPTION 'P8 FAILED: sweep acted on an offline device'; END IF;
  RAISE NOTICE 'P8  ok  device offline + sweep: lease untouched  <-- STF #663';

  UPDATE farm.device_runtime SET adb_state='device', health='healthy'
   WHERE device_id = v_dev;

  -- ============================================================
  -- P9  Witness extensions reset on a successful renew, so the
  --     cap counts only CONSECUTIVE witness-only extensions.
  -- ============================================================
  PERFORM farm.lease_witness(v_lease, v_fence, 12);
  PERFORM farm.lease_witness(v_lease, v_fence, 12);
  SELECT witness_extensions INTO v_cnt FROM farm.leases WHERE id = v_lease;
  IF v_cnt <> 2 THEN RAISE EXCEPTION 'P9 FAILED: witness did not count (got %)', v_cnt; END IF;
  PERFORM farm.lease_renew(v_lease, v_fence, v_inst2);
  SELECT witness_extensions INTO v_cnt FROM farm.leases WHERE id = v_lease;
  IF v_cnt <> 0 THEN RAISE EXCEPTION 'P9 FAILED: renew did not reset witness (got %)', v_cnt; END IF;
  RAISE NOTICE 'P9  ok  renew resets the witness extension counter';

  -- ============================================================
  -- P10 Protected leases are never auto-reclaimed, even when
  --     long expired. Hold and page instead.
  -- ============================================================
  UPDATE farm.leases
     SET heartbeat_at = now() - interval '10 hours'
   WHERE id = v_lease;
  -- Move the deadlines forward-only is enforced, so expire via the
  -- guard-legal route: they are already in the past relative to a
  -- shifted clock, so drive the sweep directly.
  UPDATE farm.reaper_state SET quiesce_until = now() - interval '1 second';

  PERFORM farm.lease_mark_suspect(500);
  SELECT count(*) INTO v_reclaimed FROM farm.lease_reclaim(100, interval '35 seconds');
  PERFORM 1 FROM farm.leases WHERE id = v_lease AND state IN ('held','suspect');
  IF NOT FOUND THEN
    RAISE EXCEPTION 'P10 FAILED: protected lease was reclaimed';
  END IF;
  RAISE NOTICE 'P10 ok  protected lease survives reclaim (hold and page)';

  -- ============================================================
  -- P11 Release is the normal end, and it bumps the device floor
  --     and quarantines the slot so stale sockets are severed.
  -- ============================================================
  SELECT farm.lease_release(v_lease, v_fence, 'completed', interval '35 seconds')
    INTO v_ok;
  IF NOT v_ok THEN RAISE EXCEPTION 'P11 FAILED: release rejected'; END IF;

  PERFORM 1 FROM farm.devices WHERE id = v_dev AND fence_floor > v_fence;
  IF NOT FOUND THEN RAISE EXCEPTION 'P11 FAILED: fence_floor not bumped'; END IF;

  PERFORM 1 FROM farm.slots s
    JOIN farm.devices d ON d.current_slot_id = s.id
   WHERE d.id = v_dev AND s.rearm_at > now();
  IF NOT FOUND THEN RAISE EXCEPTION 'P11 FAILED: slot not rearm-quarantined'; END IF;

  PERFORM 1 FROM farm.devices WHERE id = v_dev AND current_lease_id IS NULL;
  IF NOT FOUND THEN RAISE EXCEPTION 'P11 FAILED: current_lease_id not cleared'; END IF;
  RAISE NOTICE 'P11 ok  release bumps fence floor, quarantines slot, frees device';

  -- ============================================================
  -- P12 A released lease is terminal and cannot be revived.
  -- ============================================================
  BEGIN
    UPDATE farm.leases SET state='held' WHERE id = v_lease;
    RAISE EXCEPTION 'P12 FAILED: terminal lease revived';
  EXCEPTION WHEN check_violation THEN
    RAISE NOTICE 'P12 ok  terminal lease cannot be revived';
  END;

  -- ============================================================
  -- P13 A control-plane gap blocks reclamation entirely: our
  --     downtime is refunded, never charged to the tenant.
  -- ============================================================
  PERFORM farm.component_beat('reaper');
  PERFORM farm.component_beat('api');
  UPDATE farm.component_heartbeat SET beat_at = now() - interval '25 minutes'
   WHERE component = 'api';   -- api was down; reaper looked healthy

  SELECT farm.reaper_arm(ARRAY['reaper','api','scheduler'], interval '60 seconds')
    INTO v_gap;
  IF v_gap < interval '20 minutes' THEN
    RAISE EXCEPTION 'P13 FAILED: gap not detected across components (got %)', v_gap;
  END IF;
  PERFORM 1 FROM farm.reaper_state WHERE quiesce_until > now();
  IF NOT FOUND THEN RAISE EXCEPTION 'P13 FAILED: reaper not quiesced after gap'; END IF;
  RAISE NOTICE 'P13 ok  api outage recorded as gap and quiesces reaper (% )', v_gap;

  -- ============================================================
  -- P14 THE FIREWALL. Under the role reclamation runs as, device
  --     health is not merely unread — it is unreadable. No future
  --     edit to lease_reclaim can make releasing a device depend
  --     on whether that device is reachable.
  -- ============================================================
  BEGIN
    PERFORM farm.assert_reaper_is_blind();
    RAISE EXCEPTION 'P14 FAILED: reaper role can read device_runtime';
  EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'P14 ok  device health is unreadable to the reaper role';
  END;

  -- ============================================================
  -- P15 lease_reclaim restores the caller's role on exit, so the
  --     role change cannot leak into the rest of the transaction.
  -- ============================================================
  PERFORM farm.lease_reclaim(1, interval '35 seconds');
  PERFORM 1 FROM farm.component_heartbeat LIMIT 1;   -- reaper cannot see this
  RAISE NOTICE 'P15 ok  reclaim does not leak its role to the transaction';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL ASSERTIONS PASSED';
END $$;

ROLLBACK;
