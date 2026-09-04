-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- Where the cluster reaches a host's agent.
--
-- farm.hosts.adb_endpoint says where the host's ADB server listens. That
-- is enough for everything the control plane does THROUGH ADB — health,
-- shell, file transfer, the cheap recovery rungs.
--
-- It is not enough for the two rungs that ADB cannot perform. A USB
-- device reset and a VBUS power cycle happen on the physical machine,
-- through /dev/bus/usb and uhubctl, and only farmd-node can do them. The
-- ladder therefore needs a second address, and without this column it had
-- no way to find one: tiers 3 and 4 were refused on every deployment with
-- "no host agent is configured for this farm", which was true and
-- unfixable.
--
-- Nullable on purpose. A farm with no host agents is a legitimate
-- deployment — it simply cannot climb past tier 2, and the ladder says so
-- in the refusal rather than pretending the rung ran.
-- =====================================================================

ALTER TABLE farm.hosts
  ADD COLUMN node_endpoint text
    CHECK (node_endpoint IS NULL OR node_endpoint <> ''),
  -- Set by the agent itself on every registration, so an operator can see
  -- at a glance whether the version answering on that port is the one they
  -- rolled out.
  ADD COLUMN node_version text;

COMMENT ON COLUMN farm.hosts.node_endpoint IS
  'host:port of this host''s farmd-node agent, or NULL when the host runs no '
  'agent. Recovery tiers 3 and 4 are refused for hosts where this is NULL.';

-- The recovery ladder asks "which hosts can perform a hardware rung" on
-- every escalation, and that question is a scan of a small table with most
-- rows excluded.
CREATE INDEX hosts_with_agent ON farm.hosts (id) WHERE node_endpoint IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS farm.hosts_with_agent;
ALTER TABLE farm.hosts
  DROP COLUMN IF EXISTS node_version,
  DROP COLUMN IF EXISTS node_endpoint;
-- +goose StatementEnd
