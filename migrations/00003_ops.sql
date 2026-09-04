-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- Operations: recovery, quarantine, bulk execution, operator audit.
--
-- Everything here is on the HEALTH side of the firewall. Nothing in this
-- file may release a lease. Recovery acts on behalf of a holder while
-- the holder keeps its device: the lease clock keeps ticking, the fence
-- is unchanged, and the job is told what is happening.
-- =====================================================================

-- ---------------------------------------------------------------------
-- The recovery ladder, cheapest first. Stored rather than hard-coded so
-- an operator can retune budgets without a redeploy, and so the UI can
-- render what the system will actually try.
-- ---------------------------------------------------------------------

CREATE TABLE farm.recovery_tiers (
  tier          int PRIMARY KEY,
  name          text NOT NULL UNIQUE,
  description   text NOT NULL,
  -- What else this tier disturbs. A tier whose blast radius exceeds the
  -- lease's disruption_policy is refused rather than downgraded.
  blast_radius  text NOT NULL CHECK (blast_radius IN
                ('device','power_domain','hub','host')),
  -- Minimum policy a live lease must carry for this tier to be allowed.
  requires_policy text NOT NULL DEFAULT 'allow_port_power_cycle'
                CHECK (requires_policy IN
                ('no_disruption','allow_soft_reset','allow_port_power_cycle')),
  cooldown      interval NOT NULL DEFAULT interval '2 minutes',
  max_per_hour  int NOT NULL DEFAULT 6,
  enabled       boolean NOT NULL DEFAULT true
);

INSERT INTO farm.recovery_tiers (tier, name, description, blast_radius, requires_policy, cooldown, max_per_hour) VALUES
  (0, 'observe',        'Do nothing for one debounce window. Most blips self-heal.',        'device',       'no_disruption',          interval '30 seconds', 60),
  (1, 'adb_reconnect',  'host-usb:<devpath>:reconnect — re-handshake one transport.',       'device',       'no_disruption',          interval '1 minute',   20),
  (2, 'transport_reset','host-usb:<devpath>:detach then :attach.',                          'device',       'allow_soft_reset',       interval '2 minutes',  10),
  (3, 'usb_reset',      'USBDEVFS_RESET on the device node. Re-enumerates one port.',       'device',       'allow_soft_reset',       interval '5 minutes',   6),
  (4, 'port_power',     'uhubctl power cycle. Disturbs the whole power domain if ganged.',  'power_domain', 'allow_port_power_cycle', interval '10 minutes',  4),
  (5, 'device_reboot',  'adb reboot. Costs boot time and any unsaved on-device state.',     'device',       'allow_port_power_cycle', interval '15 minutes',  3),
  (6, 'quarantine',     'Stop scheduling to this slot and page a human.',                   'device',       'no_disruption',          interval '1 hour',      2),
  (7, 'adb_restart',    'Restart the host adb server. SEVERS EVERY DEVICE ON THE HOST.',    'host',         'allow_port_power_cycle', interval '1 hour',      1),
  (8, 'host_drain',     'Drain the host: no new leases, wait out the live ones, then act.', 'host',         'no_disruption',          interval '6 hours',     1);

CREATE TABLE farm.recovery_attempts (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  device_id   uuid   REFERENCES farm.devices(id) ON DELETE SET NULL,
  slot_id     bigint REFERENCES farm.slots(id)   ON DELETE SET NULL,
  hub_id      bigint REFERENCES farm.hubs(id)    ON DELETE SET NULL,
  host_id     text   REFERENCES farm.hosts(id)   ON DELETE SET NULL,
  tier        int NOT NULL REFERENCES farm.recovery_tiers(tier),
  started_at  timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  outcome     text CHECK (outcome IN ('recovered','no_change','failed','refused','aborted')),
  -- Populated when a tier was refused, so the UI can explain the refusal
  -- instead of showing a gap: "tier 4 refused, lease abc is no_disruption".
  refusal     text,
  detail      jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX recovery_device  ON farm.recovery_attempts (device_id, started_at DESC);
CREATE INDEX recovery_slot    ON farm.recovery_attempts (slot_id, started_at DESC);
CREATE INDEX recovery_hub     ON farm.recovery_attempts (hub_id, started_at DESC);
CREATE INDEX recovery_open    ON farm.recovery_attempts (started_at DESC)
  WHERE finished_at IS NULL;

-- ---------------------------------------------------------------------
-- Quarantine. Scoped to whatever level the evidence supports: when six
-- devices on one hub fail within a minute, the hub is quarantined once
-- rather than six devices independently.
-- ---------------------------------------------------------------------

CREATE TABLE farm.quarantines (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  scope       text NOT NULL CHECK (scope IN ('device','slot','power_domain','hub','host')),
  device_id   uuid   REFERENCES farm.devices(id) ON DELETE CASCADE,
  slot_id     bigint REFERENCES farm.slots(id)   ON DELETE CASCADE,
  hub_id      bigint REFERENCES farm.hubs(id)    ON DELETE CASCADE,
  host_id     text   REFERENCES farm.hosts(id)   ON DELETE CASCADE,
  reason      text NOT NULL,
  opened_at   timestamptz NOT NULL DEFAULT now(),
  closed_at   timestamptz,
  closed_by   text,
  auto        boolean NOT NULL DEFAULT true,
  CHECK (closed_at IS NULL OR closed_at >= opened_at)
);

-- One open quarantine per subject, per scope.
CREATE UNIQUE INDEX q_open_device ON farm.quarantines (device_id) WHERE closed_at IS NULL AND scope = 'device';
CREATE UNIQUE INDEX q_open_slot   ON farm.quarantines (slot_id)   WHERE closed_at IS NULL AND scope = 'slot';
CREATE UNIQUE INDEX q_open_hub    ON farm.quarantines (hub_id)    WHERE closed_at IS NULL AND scope = 'hub';
CREATE UNIQUE INDEX q_open_host   ON farm.quarantines (host_id)   WHERE closed_at IS NULL AND scope = 'host';
CREATE INDEX q_open_any ON farm.quarantines (opened_at DESC) WHERE closed_at IS NULL;

-- ---------------------------------------------------------------------
-- Bulk ADB execution across a selector.
-- ---------------------------------------------------------------------

CREATE TABLE farm.bulk_runs (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  created_by   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  selector     jsonb NOT NULL DEFAULT '{}'::jsonb,
  command      text NOT NULL,
  -- Bulk work must not stampede a hub: a cap per hub keeps a 60-device
  -- command from browning out one power domain.
  max_per_hub  int NOT NULL DEFAULT 4 CHECK (max_per_hub > 0),
  timeout      interval NOT NULL DEFAULT interval '60 seconds',
  state        text NOT NULL DEFAULT 'running'
               CHECK (state IN ('running','done','cancelled')),
  finished_at  timestamptz
);

CREATE TABLE farm.bulk_targets (
  run_id      uuid   NOT NULL REFERENCES farm.bulk_runs(id) ON DELETE CASCADE,
  device_id   uuid   NOT NULL REFERENCES farm.devices(id) ON DELETE CASCADE,
  hub_id      bigint REFERENCES farm.hubs(id) ON DELETE SET NULL,
  state       text NOT NULL DEFAULT 'pending'
              CHECK (state IN ('pending','running','ok','error','skipped')),
  started_at  timestamptz,
  finished_at timestamptz,
  exit_code   int,
  output      text,
  error       text,
  PRIMARY KEY (run_id, device_id)
);

CREATE INDEX bulk_targets_claim ON farm.bulk_targets (run_id, hub_id)
  WHERE state = 'pending';

-- ---------------------------------------------------------------------
-- Operator audit. Every destructive action names a human.
-- ---------------------------------------------------------------------

CREATE TABLE farm.audit_log (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at         timestamptz NOT NULL DEFAULT now(),
  actor      text NOT NULL,
  action     text NOT NULL,
  subject    text NOT NULL,
  reason     text,
  detail     jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX audit_at ON farm.audit_log (at DESC);

-- ---------------------------------------------------------------------
-- Blast-radius correlation.
--
-- Devices that fail together usually share a hub. This view is what turns
-- "six unrelated device alerts" into one hub alert, and it is the query
-- behind the fleet grid's correlation banner.
-- ---------------------------------------------------------------------

CREATE OR REPLACE VIEW farm.v_hub_health AS
SELECT h.id                AS hub_id,
       h.host_id,
       h.usb_path,
       h.model,
       h.vbus_switchable,
       count(d.id)                                              AS devices,
       count(*) FILTER (WHERE r.health = 'healthy')             AS healthy,
       count(*) FILTER (WHERE r.health NOT IN ('healthy','retired')) AS unhealthy,
       max(r.health_since) FILTER (WHERE r.health <> 'healthy')  AS worst_since
  FROM farm.hubs h
  LEFT JOIN farm.slots s          ON s.hub_id = h.id
  LEFT JOIN farm.devices d        ON d.current_slot_id = s.id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
 GROUP BY h.id, h.host_id, h.usb_path, h.model, h.vbus_switchable;

-- The fleet grid in one query: physical position, identity, health and
-- allocation joined so the UI never has to stitch them client-side.
CREATE OR REPLACE VIEW farm.v_fleet AS
SELECT d.id                AS device_id,
       d.farm_uid,
       d.adb_serial,
       d.serial_ambiguous,
       d.model,
       d.manufacturer,
       d.android_release,
       d.sdk_int,
       d.pool_id,
       d.admin_state,
       d.labels,
       d.failure_score,
       d.fence_floor,
       s.id                AS slot_id,
       s.rack_slot,
       s.usb_path,
       s.adb_devpath,
       s.state             AS slot_state,
       s.rearm_at,
       hb.id               AS hub_id,
       hb.usb_path         AS hub_path,
       hb.vbus_switchable,
       ho.id               AS host_id,
       ho.adb_endpoint,
       ho.admin_state      AS host_admin_state,
       r.adb_state,
       r.health,
       r.health_since,
       r.battery_pct,
       r.battery_temp_dc,
       r.consec_bad,
       r.ladder_tier,
       r.last_seen_at,
       l.id                AS lease_id,
       l.fence,
       l.state             AS lease_state,
       l.protected,
       l.job_id,
       l.tenant_id,
       l.holder,
       l.acquired_at,
       l.expires_at,
       l.reclaimable_at,
       q.id                AS quarantine_id,
       q.reason            AS quarantine_reason
  FROM farm.devices d
  LEFT JOIN farm.slots s          ON s.id = d.current_slot_id
  LEFT JOIN farm.hubs hb          ON hb.id = s.hub_id
  LEFT JOIN farm.hosts ho         ON ho.id = d.host_id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
  LEFT JOIN farm.leases l         ON l.id = d.current_lease_id
  LEFT JOIN farm.quarantines q    ON q.device_id = d.id AND q.closed_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP VIEW IF EXISTS farm.v_fleet;
DROP VIEW IF EXISTS farm.v_hub_health;
DROP TABLE IF EXISTS farm.audit_log;
DROP TABLE IF EXISTS farm.bulk_targets;
DROP TABLE IF EXISTS farm.bulk_runs;
DROP TABLE IF EXISTS farm.quarantines;
DROP TABLE IF EXISTS farm.recovery_attempts;
DROP TABLE IF EXISTS farm.recovery_tiers;
-- +goose StatementEnd
