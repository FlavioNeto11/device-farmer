-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- device-farmer core schema
--
-- Organising principle: the SLOT (a position in the USB tree) is the
-- primary physical object, not the device. Devices come and go from
-- slots; slots are what you can power-cycle and what a human can find
-- in a rack.
--
-- Second principle: ALLOCATION state (farm.leases) and HEALTH state
-- (farm.device_runtime) are orthogonal. They are separated here by
-- table, by function, and by Postgres ROLE, so that "release the device
-- because the transport dropped" is not merely discouraged but
-- unrepresentable. See 00002_lease.sql.
-- =====================================================================

-- ---------------------------------------------------------------------
-- The version floor, stated once and enforced before anything is created.
--
-- 14 is not a guess. farm.lease_acquire and the reaper's sweeps are written
-- as one statement each around a CTE that UPDATEs and RETURNs, and they take
-- their rows with FOR UPDATE SKIP LOCKED; the partial unique indexes that make
-- "one live lease per device" a schema fact, the GENERATED columns, and the
-- ltree ancestry all predate 14, but 14 is the oldest release still receiving
-- fixes when this line was written, and running a farm's allocator on an
-- unpatched database is not a saving.
--
-- Refused here rather than reported later: a schema that half-applies against
-- an old server leaves an operator reading an error about a missing operator
-- class, three migrations from the cause.
DO $version$
BEGIN
  IF current_setting('server_version_num')::int < 140000 THEN
    RAISE EXCEPTION
      'device-farmer needs PostgreSQL 14 or newer; this server is %. '
      'The compose file and the Helm chart both pin 17, which is what the '
      'assertion suites and CI run against.', current_setting('server_version');
  END IF;
END
$version$;

CREATE SCHEMA IF NOT EXISTS farm;
CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- gen_random_uuid
CREATE EXTENSION IF NOT EXISTS ltree;     -- USB ancestry as a prefix relation

-- ---------------------------------------------------------------------
-- Physical topology: rack -> host -> controller -> hub -> slot
-- Modelled explicitly so an alert names a rack position, not a serial.
-- Devices that fail together almost always share a hub or a power domain.
-- ---------------------------------------------------------------------

CREATE TABLE farm.racks (
  id          text PRIMARY KEY,
  location    text,
  notes       text,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE farm.hosts (
  id            text PRIMARY KEY,
  rack_id       text REFERENCES farm.racks(id) ON DELETE SET NULL,
  rack_unit     int,
  -- host_epoch increments on every adb server restart. A transport_id is
  -- meaningless without the epoch it was minted in: adb reuses small
  -- integers, so (epoch, transport_id) is the only stable pair.
  host_epoch    bigint NOT NULL DEFAULT 1,
  adb_endpoint  text NOT NULL,
  admin_state   text NOT NULL DEFAULT 'enabled'
                CHECK (admin_state IN ('enabled','draining','disabled')),
  kernel_release text,
  agent_version text,
  last_seen_at  timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE farm.controllers (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  host_id    text NOT NULL REFERENCES farm.hosts(id) ON DELETE CASCADE,
  pci_addr   text,
  kind       text,
  root_bus   int NOT NULL,
  UNIQUE (host_id, root_bus)
);

-- power_domains model what a single power switch actually controls. On a
-- hub without per-port switching, every port shares one domain, so
-- "power-cycle this device" is really "power-cycle these seven devices"
-- and must be refused while any of them holds a live lease.
CREATE TABLE farm.power_domains (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  host_id       text NOT NULL REFERENCES farm.hosts(id) ON DELETE CASCADE,
  kind          text NOT NULL CHECK (kind IN ('per_port','ganged','none')),
  control       text NOT NULL CHECK (control IN ('uhubctl','smarthub','pdu','none')),
  control_addr  text,
  notes         text
);

CREATE TABLE farm.hubs (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  host_id        text NOT NULL REFERENCES farm.hosts(id) ON DELETE CASCADE,
  controller_id  bigint REFERENCES farm.controllers(id) ON DELETE SET NULL,
  parent_hub_id  bigint REFERENCES farm.hubs(id) ON DELETE SET NULL,
  usb_path       text NOT NULL CHECK (usb_path ~ '^[0-9]+-[0-9]+(\.[0-9]+)*$'),
  model          text,
  port_count     int NOT NULL CHECK (port_count BETWEEN 1 AND 32),
  vbus_switchable boolean NOT NULL DEFAULT false,
  UNIQUE (host_id, usb_path)
);

CREATE TABLE farm.slots (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  host_id         text   NOT NULL REFERENCES farm.hosts(id)  ON DELETE RESTRICT,
  hub_id          bigint NOT NULL REFERENCES farm.hubs(id)   ON DELETE RESTRICT,
  power_domain_id bigint REFERENCES farm.power_domains(id),
  port_number     int  NOT NULL CHECK (port_number BETWEEN 1 AND 32),
  usb_path        text NOT NULL CHECK (usb_path ~ '^[0-9]+-[0-9]+(\.[0-9]+)*$'),

  -- BLOCKER 2 FIX: every ADB call that targets a physical position uses
  -- this, never a serial. adb parses "host-usb:<devpath>:<cmd>" the same
  -- way it parses "host-serial:", and duplicate OEM serials are real
  -- (STF's README documents a device with serial "0123456789ABCDEF").
  -- Addressing recovery by serial would let a reset land on a healthy
  -- device holding a live 6-hour lease.
  adb_devpath     text GENERATED ALWAYS AS ('usb:' || usb_path) STORED,

  -- ltree of the USB ancestry, e.g. h01.c3.p3_1.p3_1_4 — a GiST prefix
  -- query answers "everything downstream of this hub" in one index scan.
  topo_path       ltree NOT NULL,

  rack_slot       text,          -- human-facing label: "R1-U14-H2-P3"
  state           text NOT NULL DEFAULT 'active'
                  CHECK (state IN ('active','disabled','maintenance')),
  -- After a reclaim the slot is briefly unschedulable so the previous
  -- holder's sockets are certainly severed. MUST exceed the node proxy's
  -- self-fence timeout; asserted at startup.
  rearm_at        timestamptz NOT NULL DEFAULT now(),
  created_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (host_id, usb_path)
);

CREATE INDEX slots_ltree     ON farm.slots USING gist (topo_path);
CREATE INDEX slots_hub       ON farm.slots (hub_id);
CREATE INDEX slots_power_dom ON farm.slots (power_domain_id);

CREATE TABLE farm.pools (
  id          text PRIMARY KEY,
  description text,
  created_at  timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------
-- Devices. Identity is a branded farm_uid written to the device itself,
-- because the ADB serial is an OBSERVATION and is not unique.
-- ---------------------------------------------------------------------

CREATE TABLE farm.devices (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  farm_uid          text NOT NULL UNIQUE CHECK (farm_uid ~ '^df-[0-9a-f]{32}$'),
  adb_serial        text,                     -- observed, deliberately NOT unique
  serial_ambiguous  boolean NOT NULL DEFAULT false,
  hw_fingerprint    bytea,
  manufacturer      text,
  model             text,
  product           text,
  device_codename   text,
  android_release   text,
  sdk_int           int CHECK (sdk_int BETWEEN 1 AND 100),
  abis              text[] NOT NULL DEFAULT '{}',
  build_fingerprint text,
  current_slot_id   bigint REFERENCES farm.slots(id) ON DELETE SET NULL,
  host_id           text   REFERENCES farm.hosts(id) ON DELETE SET NULL,
  pool_id           text   NOT NULL REFERENCES farm.pools(id) ON DELETE RESTRICT,
  labels            jsonb  NOT NULL DEFAULT '{}'::jsonb,
  admin_state       text   NOT NULL DEFAULT 'enabled'
                    CHECK (admin_state IN ('enabled','disabled','quarantined','retired')),

  -- Denormalised pointer to the live lease, maintained by trigger. Turns
  -- the hot allocation predicate into an index probe instead of an
  -- anti-join against a growing lease history.
  current_lease_id  uuid,

  -- Monotonic floor: any fence at or below this is stale and must be
  -- refused at the resource, not merely in the database.
  fence_floor       bigint NOT NULL DEFAULT 0,

  failure_score     numeric(8,3) NOT NULL DEFAULT 0,
  failure_score_at  timestamptz NOT NULL DEFAULT now(),
  last_released_at  timestamptz,
  first_seen_at     timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX devices_one_per_slot ON farm.devices (current_slot_id)
  WHERE current_slot_id IS NOT NULL;
CREATE INDEX devices_serial     ON farm.devices (adb_serial);
CREATE INDEX devices_labels_gin ON farm.devices USING gin (labels);

-- The hot allocation index: free, enabled devices ordered by flakiness.
CREATE INDEX devices_free ON farm.devices (pool_id, failure_score, last_released_at)
  WHERE current_lease_id IS NULL AND admin_state = 'enabled';

-- ---------------------------------------------------------------------
-- Health. Written ONLY by the watchdog. Read by the allocator.
-- NEVER read by the reaper — enforced below by a Postgres role, not by
-- a code-review convention.
-- ---------------------------------------------------------------------

CREATE TABLE farm.device_runtime (
  device_id     uuid PRIMARY KEY REFERENCES farm.devices(id) ON DELETE CASCADE,
  host_id       text   REFERENCES farm.hosts(id) ON DELETE SET NULL,
  slot_id       bigint REFERENCES farm.slots(id) ON DELETE SET NULL,
  adb_state     text NOT NULL DEFAULT 'absent' CHECK (adb_state IN
                ('device','offline','unauthorized','authorizing','connecting',
                 'detached','no_permissions','bootloader','recovery','sideload',
                 'rescue','host','absent','unknown')),
  health        text NOT NULL DEFAULT 'unknown' CHECK (health IN
                ('unknown','booting','healthy','degraded','offline','unauthorized',
                 'missing','recovering','quarantined','retired')),
  health_since  timestamptz NOT NULL DEFAULT now(),
  transport_id  bigint,
  host_epoch    bigint,
  negotiated_mbps int,
  max_mbps        int,
  consec_bad    int NOT NULL DEFAULT 0,
  consec_good   int NOT NULL DEFAULT 0,
  -- Token bucket, not a raw counter: prevents a device flapping between
  -- healthy and quarantined on every transient blip.
  flap_credits  numeric(6,2) NOT NULL DEFAULT 10.0,
  flap_updated_at timestamptz NOT NULL DEFAULT now(),
  ladder_tier   int NOT NULL DEFAULT 0,
  suppress_until timestamptz,          -- set during an induced reset
  battery_pct   smallint CHECK (battery_pct BETWEEN 0 AND 100),
  battery_temp_dc smallint,
  charge_gate   text CHECK (charge_gate IN ('on','off','unknown')),
  boot_completed boolean,
  last_seen_at  timestamptz,
  updated_at    timestamptz NOT NULL DEFAULT now()
) WITH (fillfactor = 70);   -- updated constantly; leave room for HOT updates

CREATE INDEX device_runtime_ready ON farm.device_runtime (device_id)
  WHERE adb_state = 'device' AND health = 'healthy';
CREATE INDEX device_runtime_bad ON farm.device_runtime (health, health_since)
  WHERE health NOT IN ('healthy','retired');

-- ---------------------------------------------------------------------
-- Tenancy and work
-- ---------------------------------------------------------------------

CREATE TABLE farm.tenants (
  id          text PRIMARY KEY,
  name        text,
  max_devices int NOT NULL DEFAULT 0 CHECK (max_devices >= 0),  -- 0 = unlimited
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE farm.queues (
  id          text PRIMARY KEY,
  tenant_id   text NOT NULL REFERENCES farm.tenants(id) ON DELETE CASCADE,
  priority    int  NOT NULL DEFAULT 100,
  max_devices int  NOT NULL DEFAULT 0 CHECK (max_devices >= 0),
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE farm.jobs (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         text NOT NULL REFERENCES farm.tenants(id),
  queue_id          text NOT NULL REFERENCES farm.queues(id),
  pool_id           text NOT NULL REFERENCES farm.pools(id),
  state             text NOT NULL DEFAULT 'queued' CHECK (state IN
                    ('queued','allocating','running','succeeded','failed','cancelled')),
  spec              jsonb NOT NULL DEFAULT '{}'::jsonb,
  checkpoint        jsonb NOT NULL DEFAULT '{}'::jsonb,
  selector          jsonb NOT NULL DEFAULT '{}'::jsonb,
  cohort_hash       text,
  pin_device        uuid REFERENCES farm.devices(id),
  protected         boolean NOT NULL DEFAULT false,
  disruption_policy text NOT NULL DEFAULT 'allow_port_power_cycle'
                    CHECK (disruption_policy IN
                    ('allow_port_power_cycle','allow_soft_reset','no_disruption')),
  expected_duration interval,
  -- The ONLY user-supplied clock that may end a lease automatically.
  max_runtime       interval,
  ttl               interval NOT NULL DEFAULT interval '15 minutes'
                    CHECK (ttl   >= interval '10 minutes'),
  grace             interval NOT NULL DEFAULT interval '30 minutes'
                    CHECK (grace >= interval '5 minutes'),
  created_by        text,
  created_at        timestamptz NOT NULL DEFAULT now(),
  started_at        timestamptz,
  finished_at       timestamptz
);

CREATE INDEX jobs_ready ON farm.jobs (queue_id, created_at) WHERE state = 'queued';
CREATE INDEX jobs_live  ON farm.jobs (state) WHERE state IN ('allocating','running');

-- ---------------------------------------------------------------------
-- Leases. The heart of the system.
-- ---------------------------------------------------------------------

CREATE SEQUENCE farm.fence_seq AS bigint START 1 INCREMENT 1 CACHE 1 NO CYCLE;

CREATE TABLE farm.leases (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  fence           bigint NOT NULL DEFAULT nextval('farm.fence_seq') UNIQUE,
  device_id       uuid   NOT NULL REFERENCES farm.devices(id) ON DELETE RESTRICT,
  slot_id         bigint REFERENCES farm.slots(id),
  job_id          uuid   NOT NULL REFERENCES farm.jobs(id) ON DELETE RESTRICT,
  tenant_id       text   NOT NULL REFERENCES farm.tenants(id),
  queue_id        text   NOT NULL REFERENCES farm.queues(id),

  -- holder is a pod name and is AUDIT ONLY. A pod eviction is the most
  -- common event in a Kubernetes control plane; it is not evidence of
  -- death and must never cost a device. Ownership is keyed on job_id.
  holder          text NOT NULL,
  holder_instance uuid NOT NULL,
  holder_epoch    int  NOT NULL DEFAULT 0,

  state           text NOT NULL DEFAULT 'held'
                  CHECK (state IN ('held','suspect','expired','released')),
  protected       boolean NOT NULL DEFAULT false,
  disruption_policy text NOT NULL DEFAULT 'allow_port_power_cycle',

  ttl             interval NOT NULL CHECK (ttl   >= interval '10 minutes'),
  grace           interval NOT NULL CHECK (grace >= interval '5 minutes'),

  acquired_at     timestamptz NOT NULL DEFAULT now(),
  heartbeat_at    timestamptz NOT NULL DEFAULT now(),
  heartbeat_seq   bigint NOT NULL DEFAULT 0,

  -- Plain columns, not GENERATED: timestamptz + interval is STABLE, not
  -- IMMUTABLE, so a generation expression is rejected.
  expires_at      timestamptz NOT NULL,
  reclaimable_at  timestamptz NOT NULL,

  witness_at         timestamptz,
  witness_extensions int NOT NULL DEFAULT 0,

  released_at     timestamptz,

  -- ===============================================================
  -- THE #663 COUNTERMEASURE.
  -- There is no connectivity value in this domain. A socket error, a
  -- probe timeout, or a device going offline cannot be written here,
  -- so "released because the transport dropped" raises check_violation
  -- rather than silently destroying six hours of work.
  -- ===============================================================
  release_reason  text CHECK (release_reason IN
                  ('completed','failed','job_cancelled','max_runtime',
                   'operator_revoked','holder_expired','device_retired')),

  CHECK (reclaimable_at >= expires_at)
);

-- The partial unique index — not the row lock — is what actually
-- prevents a double grant. Under READ COMMITTED, EvalPlanQual re-checks
-- quals only on the locked relation, so a subquery against leases still
-- sees the original snapshot.
CREATE UNIQUE INDEX leases_one_live_per_device ON farm.leases (device_id)
  WHERE state IN ('held','suspect');
CREATE UNIQUE INDEX leases_one_live_per_job ON farm.leases (job_id)
  WHERE state IN ('held','suspect');
CREATE INDEX leases_suspect_scan ON farm.leases (expires_at)
  WHERE state = 'held';
CREATE INDEX leases_reclaim_scan ON farm.leases (reclaimable_at)
  WHERE state = 'suspect';
CREATE INDEX leases_device_hist ON farm.leases (device_id, acquired_at DESC);

ALTER TABLE farm.devices
  ADD CONSTRAINT devices_current_lease_fk
  FOREIGN KEY (current_lease_id) REFERENCES farm.leases(id) ON DELETE SET NULL;

-- ---------------------------------------------------------------------
-- Control-plane liveness. Our downtime is refunded to tenants, never
-- charged to them as lease budget.
-- ---------------------------------------------------------------------

CREATE TABLE farm.component_heartbeat (
  component text PRIMARY KEY,
  beat_at   timestamptz NOT NULL
);

CREATE TABLE farm.control_plane_gap (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  component  text NOT NULL,
  started_at timestamptz NOT NULL,
  ended_at   timestamptz NOT NULL,
  CHECK (ended_at > started_at)
);

CREATE INDEX cpg_recent ON farm.control_plane_gap (ended_at DESC);

CREATE TABLE farm.reaper_state (
  singleton     boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  quiesce_until timestamptz NOT NULL DEFAULT now(),
  armed_at      timestamptz NOT NULL DEFAULT now(),
  enabled       boolean NOT NULL DEFAULT true
);

INSERT INTO farm.reaper_state (singleton) VALUES (true);

-- ---------------------------------------------------------------------
-- Audit
-- ---------------------------------------------------------------------

CREATE TABLE farm.events (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at         timestamptz NOT NULL DEFAULT now(),
  kind       text NOT NULL,
  device_id  uuid,
  slot_id    bigint,
  lease_id   uuid,
  job_id     uuid,
  actor      text,
  detail     jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX events_device_time ON farm.events (device_id, at DESC);
CREATE INDEX events_slot_time   ON farm.events (slot_id, at DESC);
CREATE INDEX events_kind_time   ON farm.events (kind, at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP SCHEMA IF EXISTS farm CASCADE;
-- +goose StatementEnd
