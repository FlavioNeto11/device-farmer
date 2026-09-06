-- +goose Up

-- =====================================================================
-- WHO MAY READ THE LEDGER OF ENDINGS.
--
-- 00007_lease_events.sql built farm.v_lease_endings — one row per
-- lease ending, with the reason, the class of ender, and how long the
-- device was held — and granted it to nobody. That was invisible
-- because the API connects as the schema owner, and an owner needs no
-- grant. GET /api/v1/leases/endings now serves that view to an operator
-- at 3am, so the question "which role may read this" has an answer that
-- matters: a deployment that narrows the API's role — the possibility
-- 00007's own header names, and the one FARM_DB_ROLE already
-- implements for the three loop processes — would find the one route
-- an incident review reaches for answering 42501.
--
-- 00015_runtime_roles.sql granted what the loops needed table by table.
-- A view is not a table and inherits nothing from those grants: SELECT
-- on farm.events does not carry to farm.v_lease_endings, and SELECT on
-- farm.v_lease_endings does not carry to farm.events. That second half
-- is the useful one, and it is why this file grants a VIEW rather than
-- widening anything. The view is created by the schema owner and is not
-- security_invoker, so a reader of it is checked against the view and
-- the owner's rights are used for farm.events underneath: the grant
-- below hands out exactly the lease_ended rows and exactly the fourteen
-- columns 00007 chose, and no path at all to the rest of the timeline.
--
-- WHO GETS IT
--
-- farm_reaper. It already holds SELECT on farm.leases whole
-- (00002_lease.sql), and a released lease keeps its row: release_reason,
-- holder, tenant, heartbeat and deadlines are all readable to it today,
-- for live leases as well as ended ones. The view is a strictly narrower
-- projection of facts this role can already see, so the grant confers no
-- new visibility — it means the loop that ends leases without a human
-- can read back the record of its own endings through the same surface
-- the operator does, rather than through a second query that could
-- disagree with it.
--
-- farm_scheduler receives it by inheritance: 00002_lease.sql makes it a
-- member of farm_reaper, and the role is INHERIT. Naming it here as
-- well would be a second grant to revoke later and a second place to
-- forget. It too reads farm.leases whole, so the same argument holds.
--
-- WHO DOES NOT, AND WHY THAT IS THE POINT
--
-- farm_watchdog and farm_parker carry REVOKE ALL ON farm.leases
-- (00002_lease.sql:110, 00008_parked.sql:254). That REVOKE is the
-- STF #663 firewall stated in DDL: the health plane and the parking
-- path must be unable to learn anything about an allocation, so that no
-- future edit to them can be written to end one. A view is precisely the
-- shape that reopens such a firewall quietly — it is a different object
-- with its own grant, it reads its base table with the owner's rights,
-- and "it's only a view" is how it gets approved. Granting it here would
-- hand those two roles who held what, for how long, and why it ended:
-- the whole of what the REVOKE took away, minus the live rows.
--
-- So they are named and refused. The REVOKE is a no-op against roles
-- that were never granted anything, and it is written anyway for the
-- reason 00002 writes the same idiom: the intent has to be somewhere a
-- reader of the schema can find it. What actually keeps it true is
-- test/assertions_v19.sql, which fails if either role can read this
-- view — including via a later GRANT ... ON ALL TABLES IN SCHEMA farm,
-- which in Postgres includes views and would otherwise open this door
-- with nothing to notice it.
--
-- Nothing here can end a lease. Every statement in this file is a
-- privilege on a read-only view of an append-only ledger.
-- =====================================================================

-- +goose StatementBegin
DO $$
BEGIN
  -- The reaper, and farm_scheduler through its membership of it.
  EXECUTE 'GRANT SELECT ON farm.v_lease_endings TO farm_reaper';

  -- The health plane and the parking path. See above: this is the
  -- firewall, restated for the object that could route around it.
  EXECUTE 'REVOKE ALL ON farm.v_lease_endings FROM farm_watchdog, farm_parker';
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  EXECUTE 'REVOKE SELECT ON farm.v_lease_endings FROM farm_reaper';
END $$;
-- +goose StatementEnd
