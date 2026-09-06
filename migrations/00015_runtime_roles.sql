-- +goose Up

-- =====================================================================
-- THE ROLE FIREWALL, ASSUMED AT RUNTIME.
--
-- 00002_lease.sql created farm_reaper, farm_scheduler and farm_watchdog
-- and revoked from each the one table it must be structurally unable
-- to read: the reaper cannot see farm.device_runtime, the watchdog
-- cannot see farm.leases. That DDL was correct, and until now no
-- process assumed it. Every farmd role connected as the login user and
-- ran with that user's full grants; the only statements that executed
-- under a firewalled role were the three functions carrying their own
-- SET (farm.lease_reclaim, farm.device_park, farm.device_unpark). The
-- reaper's census query, or a future edit to its Go loop, could have
-- read health with nothing in the database to say no.
--
-- FARM_DB_ROLE (cmd/farmd, openPool) now makes a loop process execute
-- SET ROLE on every pooled connection, so the WHOLE process runs with
-- the role's privileges rather than three functions of it. SET ROLE has
-- two prerequisites, and this migration supplies both:
--
--   1. The login user must be a MEMBER of the role it assumes. 00002
--      granted farm_reaper to farm_scheduler and nothing to the login
--      user, so SET ROLE was refused for all three. The grant below is
--      the same idiom 00008 uses for farm_parker.
--
--   2. The role must hold every grant the loop needs to run WHOLE, not
--      only what its SQL functions touch. 00002 granted what
--      lease_reclaim and lease_acquire read; the Go loops around them
--      beat, arm, audit, take a census and update job state, and none
--      of that was covered. The list below was found by running each
--      loop's statements under SET ROLE against an empty database and
--      reading the 42501s; test/assertions_v15.sql keeps running them.
--      Each grant names the statement that needs it.
--
-- What this migration does NOT do is widen either firewall REVOKE. No
-- loop turned out to need its forbidden table, which is the finding
-- that makes the firewall worth assuming: the separation costs the
-- loops nothing they were actually using.
--
-- Nor does it guard any of these grants against a migrator applying the
-- same migration to another database at the same moment, and it
-- deliberately should not. Two different objects are involved and they
-- behave differently:
--
--   - A TABLE grant is per-database. It lives in the pg_class entry of
--     the table it is about, so 00002's REVOKE on farm.device_runtime
--     touches only the database being migrated. Another database's
--     farm_reaper keeps whatever that database granted it.
--   - A MEMBERSHIP grant (the line below, 00002:108, 00008:260) is
--     cluster-wide: pg_auth_members is a shared catalog. But Postgres
--     locks the role before it writes, so a second identical GRANT
--     waits, re-reads, and reports "already been granted" as a NOTICE.
--     Not an error, and never a moment in which the membership is gone.
--
-- Only CREATE ROLE races, because there is no object to lock yet, and
-- 00002 and 00008 guard it where they issue it — for every database
-- migrated from empty after that edit landed; a cluster whose roles
-- already exist has nothing left to race over. Turning any block here
-- into a REVOKE-then-GRANT to make it "idempotent" would MANUFACTURE
-- the window it claimed to close, and every revoke in reach is one the
-- #663 firewall depends on.
--
-- The Down below is the exception, and cannot be made into anything
-- else: REVOKE ... FROM current_user unmakes a cluster-wide membership,
-- so rolling 00015 back in one database stops every OTHER farm in the
-- cluster from assuming its runtime roles. Roll back one farm on a
-- shared cluster and you have stopped them all.
-- =====================================================================

-- +goose StatementBegin
DO $$
BEGIN
  -- Every loop beats. farm.component_beat is INSERT ... ON CONFLICT DO
  -- UPDATE, and the conflict check reads the existing row, so SELECT is
  -- part of the upsert's price even for a role that never queries the
  -- table; the reaper additionally reads min(beat_at) in reaper_arm.
  EXECUTE 'GRANT SELECT, INSERT, UPDATE ON farm.component_heartbeat TO farm_reaper, farm_scheduler, farm_watchdog';

  -- farm_reaper. 00002 gave it SELECT on reaper_state and
  -- control_plane_gap because lease_reclaim only reads them. reaper_arm,
  -- which the loop calls before its first sweep, writes both: the gap
  -- row is the record that a refund happened, and quiesce_until is the
  -- gate that stops a restored reaper from mass-reclaiming.
  EXECUTE 'GRANT INSERT ON farm.control_plane_gap TO farm_reaper';
  EXECUTE 'GRANT UPDATE ON farm.reaper_state     TO farm_reaper';
  -- lease_expire_max_runtime joins farm.jobs for the user-written
  -- max_runtime, the held/suspect census groups leases by pool through
  -- it, and the slot rearm census names the hub. Reads only: the reaper
  -- still writes nothing but leases, devices, slots and its own ledgers.
  EXECUTE 'GRANT SELECT ON farm.jobs, farm.hubs TO farm_reaper';

  -- farm_scheduler. Allocation itself was granted; the bookkeeping
  -- around a placement was not. Its unwind path — lease_release, which
  -- parks the slot for the rearm window — needs nothing here: 00002
  -- made farm_scheduler a member of farm_reaper, so it inherits the
  -- reaper's UPDATE on leases, devices and slots. Revoking that
  -- membership would break the scheduler's release, not only its
  -- reach into lease_reclaim.
  EXECUTE 'GRANT UPDATE ON farm.jobs TO farm_scheduler';                -- queued -> running after a placement
  EXECUTE 'GRANT SELECT ON farm.tenants, farm.queues TO farm_scheduler'; -- the per-tenant and per-queue caps
  EXECUTE 'GRANT SELECT ON farm.hosts TO farm_scheduler';               -- lease_acquire skips draining hosts

  -- farm_watchdog. Its slot and census queries name the hub for the
  -- usb_path label. Still no path to a lease.
  EXECUTE 'GRANT SELECT ON farm.hubs TO farm_watchdog';

  -- Membership. Without it every SET ROLE is refused and a process
  -- started with FARM_DB_ROLE does not start at all — which is the
  -- correct failure, and this is its fix.
  EXECUTE format('GRANT farm_reaper, farm_scheduler, farm_watchdog TO %I', current_user);
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  EXECUTE format('REVOKE farm_reaper, farm_scheduler, farm_watchdog FROM %I', current_user);
  EXECUTE 'REVOKE SELECT ON farm.hubs FROM farm_watchdog';
  EXECUTE 'REVOKE SELECT ON farm.hosts FROM farm_scheduler';
  EXECUTE 'REVOKE SELECT ON farm.tenants, farm.queues FROM farm_scheduler';
  EXECUTE 'REVOKE UPDATE ON farm.jobs FROM farm_scheduler';
  EXECUTE 'REVOKE SELECT ON farm.jobs, farm.hubs FROM farm_reaper';
  EXECUTE 'REVOKE UPDATE ON farm.reaper_state FROM farm_reaper';
  EXECUTE 'REVOKE INSERT ON farm.control_plane_gap FROM farm_reaper';
  EXECUTE 'REVOKE SELECT, INSERT, UPDATE ON farm.component_heartbeat FROM farm_reaper, farm_scheduler, farm_watchdog';
END $$;
-- +goose StatementEnd
