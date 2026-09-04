-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- Operating devices generically and dynamically.
--
-- Everything before this migration assumed the fleet already existed.
-- This one is about a device that was plugged in five seconds ago and a
-- job that was written without knowing which device it would land on.
--
-- Two ideas carry it:
--   1. Identity is OBSERVED, then RESOLVED, then BRANDED. An ADB serial
--      is evidence, not an identifier — cheap OEMs ship duplicates.
--   2. A job is a list of typed steps against an abstract device. The
--      step vocabulary is closed and stored, so a job written today
--      still means the same thing when it resumes tomorrow.
-- =====================================================================

-- ---------------------------------------------------------------------
-- Identity. Every sighting is recorded before any conclusion is drawn,
-- so a wrong adoption can be explained afterwards instead of guessed at.
-- ---------------------------------------------------------------------

CREATE TABLE farm.identity_observations (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  observed_at   timestamptz NOT NULL DEFAULT now(),
  host_id       text   NOT NULL REFERENCES farm.hosts(id) ON DELETE CASCADE,
  host_epoch    bigint,
  usb_path      text   NOT NULL,
  adb_devpath   text   NOT NULL,
  transport_id  bigint,
  adb_serial    text,
  -- Read off the device itself. The strongest signal we have, because we
  -- are the ones who wrote it.
  farm_uid      text,
  hw_fingerprint bytea,
  manufacturer  text,
  model         text,
  product       text,
  device_codename text,
  android_release text,
  sdk_int       int,
  abis          text[],
  build_fingerprint text,
  -- How the device was matched to a row in farm.devices, or why it was not.
  resolution    text NOT NULL DEFAULT 'pending' CHECK (resolution IN
                ('pending','branded_uid','hw_fingerprint','serial_and_slot',
                 'adopted_new','ambiguous','conflict','unreadable')),
  device_id     uuid REFERENCES farm.devices(id) ON DELETE SET NULL,
  detail        jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX ident_obs_recent   ON farm.identity_observations (host_id, observed_at DESC);
CREATE INDEX ident_obs_device   ON farm.identity_observations (device_id, observed_at DESC);
CREATE INDEX ident_obs_conflict ON farm.identity_observations (observed_at DESC)
  WHERE resolution IN ('ambiguous','conflict','unreadable');

-- Which device sat in which slot, over time. A device that moves slots is
-- ordinary (a human re-cables a rack); a slot whose occupant changes while
-- a lease is live is not, and this table is how that is noticed.
CREATE TABLE farm.slot_occupancy (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  slot_id    bigint NOT NULL REFERENCES farm.slots(id)   ON DELETE CASCADE,
  device_id  uuid   NOT NULL REFERENCES farm.devices(id) ON DELETE CASCADE,
  since      timestamptz NOT NULL DEFAULT now(),
  until      timestamptz,
  reason     text,
  CHECK (until IS NULL OR until >= since)
);

CREATE UNIQUE INDEX slot_occ_live_slot   ON farm.slot_occupancy (slot_id)   WHERE until IS NULL;
CREATE UNIQUE INDEX slot_occ_live_device ON farm.slot_occupancy (device_id) WHERE until IS NULL;
CREATE INDEX slot_occ_hist ON farm.slot_occupancy (device_id, since DESC);

-- ---------------------------------------------------------------------
-- Durable per-device state. Survives reboot, re-plug and re-slotting,
-- because it is keyed on the device rather than on where it is sitting.
-- ---------------------------------------------------------------------

CREATE TABLE farm.profiles (
  id          text PRIMARY KEY,
  description text,
  -- Packages this profile owns. A 'medium' reset uninstalls everything
  -- that is NOT in this list, which is what makes reset generic: the farm
  -- does not need to know what a job installed.
  packages    text[] NOT NULL DEFAULT '{}',
  spec        jsonb  NOT NULL DEFAULT '{}'::jsonb,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE farm.device_state (
  device_id   uuid PRIMARY KEY REFERENCES farm.devices(id) ON DELETE CASCADE,
  profile_id  text REFERENCES farm.profiles(id) ON DELETE SET NULL,
  state       jsonb NOT NULL DEFAULT '{}'::jsonb,
  revision    bigint NOT NULL DEFAULT 1,
  -- The fence that last wrote this. A write carrying a lower fence is a
  -- write from a job that has already lost the device.
  written_by_fence bigint NOT NULL DEFAULT 0,
  applied_at  timestamptz,
  drifted     boolean NOT NULL DEFAULT false,
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX device_state_drift ON farm.device_state (drifted) WHERE drifted;

CREATE TABLE farm.device_state_revisions (
  device_id  uuid   NOT NULL REFERENCES farm.devices(id) ON DELETE CASCADE,
  revision   bigint NOT NULL,
  at         timestamptz NOT NULL DEFAULT now(),
  fence      bigint NOT NULL,
  state      jsonb  NOT NULL,
  PRIMARY KEY (device_id, revision)
);

-- ---------------------------------------------------------------------
-- Artifacts, content-addressed. A job names a sha256; the farm decides
-- whether that content already sits on the device.
-- ---------------------------------------------------------------------

CREATE TABLE farm.artifacts (
  sha256      text PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  kind        text NOT NULL CHECK (kind IN ('apk','file','script','bundle')),
  name        text NOT NULL,
  size_bytes  bigint NOT NULL CHECK (size_bytes >= 0),
  package     text,          -- apk only
  version_code bigint,       -- apk only
  url         text,          -- where the bytes live; NULL means inline/absent
  uploaded_by text,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX artifacts_pkg ON farm.artifacts (package, version_code DESC)
  WHERE package IS NOT NULL;

CREATE TABLE farm.device_artifacts (
  device_id   uuid NOT NULL REFERENCES farm.devices(id)  ON DELETE CASCADE,
  sha256      text NOT NULL REFERENCES farm.artifacts(sha256) ON DELETE CASCADE,
  state       text NOT NULL DEFAULT 'pending'
              CHECK (state IN ('pending','present','failed','removed')),
  installed_at timestamptz,
  detail      jsonb NOT NULL DEFAULT '{}'::jsonb,
  PRIMARY KEY (device_id, sha256)
);

CREATE INDEX device_artifacts_pending ON farm.device_artifacts (device_id)
  WHERE state = 'pending';

-- ---------------------------------------------------------------------
-- Job execution. THE step vocabulary is closed and lives here, not in a
-- comment, so a spec written today still means the same thing when it
-- resumes tomorrow.
-- ---------------------------------------------------------------------

CREATE TABLE farm.step_kinds (
  kind        text PRIMARY KEY,
  description text NOT NULL,
  -- Whether the step may be re-run after a crash without changing the
  -- outcome. A non-idempotent step must be checkpointed before it runs,
  -- or a resume repeats a side effect.
  idempotent  boolean NOT NULL,
  needs_artifact boolean NOT NULL DEFAULT false
);

INSERT INTO farm.step_kinds (kind, description, idempotent, needs_artifact) VALUES
  ('push',      'Copy an artifact to a path on the device.',                    true,  true),
  ('install',   'Install an APK artifact.',                                     true,  true),
  ('uninstall', 'Remove a package.',                                            true,  false),
  ('shell',     'Run a shell command and capture output and exit code.',        false, false),
  ('shell_detached', 'Start a long-running command under nohup setsid; the ' ||
                'device, not a socket, owns the result.',                       false, false),
  ('wait_for',  'Poll a shell probe until it succeeds or the timeout elapses.', true,  false),
  ('pull',      'Copy a file off the device.',                                  true,  false),
  ('assert',    'Fail the job unless a probe reports the expected value.',      true,  false),
  ('reset',     'Apply a reset tier: none, soft, medium or hard.',              true,  false),
  ('sleep',     'Wait a fixed duration.',                                       true,  false);

-- One row per step execution. jobs.checkpoint records where a resume
-- should pick up; this records what actually happened, which is what an
-- operator reads when a job fails at 3am.
CREATE TABLE farm.job_steps (
  job_id      uuid NOT NULL REFERENCES farm.jobs(id) ON DELETE CASCADE,
  attempt     int  NOT NULL DEFAULT 1,
  step_index  int  NOT NULL,
  step_id     text NOT NULL,
  kind        text NOT NULL REFERENCES farm.step_kinds(kind),
  state       text NOT NULL DEFAULT 'pending' CHECK (state IN
              ('pending','running','ok','failed','skipped','aborted')),
  started_at  timestamptz,
  finished_at timestamptz,
  exit_code   int,
  output      text,
  error       text,
  detail      jsonb NOT NULL DEFAULT '{}'::jsonb,
  PRIMARY KEY (job_id, attempt, step_index)
);

CREATE INDEX job_steps_live ON farm.job_steps (job_id, attempt, step_index)
  WHERE state IN ('pending','running');

-- One row per time a job was placed on a device. A job that has been
-- attempted on four devices and failed on all four is a job problem; the
-- same failure on one device four times is a device problem, and this is
-- the table that tells them apart.
CREATE TABLE farm.job_attempts (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  job_id      uuid NOT NULL REFERENCES farm.jobs(id) ON DELETE CASCADE,
  attempt     int  NOT NULL,
  device_id   uuid REFERENCES farm.devices(id) ON DELETE SET NULL,
  lease_id    uuid,
  fence       bigint,
  started_at  timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  outcome     text CHECK (outcome IN ('succeeded','failed','cancelled','abandoned')),
  error       text,
  UNIQUE (job_id, attempt)
);

CREATE INDEX job_attempts_device ON farm.job_attempts (device_id, started_at DESC);

-- Job columns the runner needs. Added here rather than in 00001 so the
-- lease core stays readable as the lease core.
ALTER TABLE farm.jobs
  ADD COLUMN attempt      int  NOT NULL DEFAULT 0,
  ADD COLUMN max_attempts int  NOT NULL DEFAULT 3 CHECK (max_attempts >= 1),
  ADD COLUMN profile_id   text REFERENCES farm.profiles(id) ON DELETE SET NULL,
  ADD COLUMN reset_tier   text NOT NULL DEFAULT 'soft'
             CHECK (reset_tier IN ('none','soft','medium','hard')),
  ADD COLUMN resumable    boolean NOT NULL DEFAULT true,
  ADD COLUMN error        text;

-- ---------------------------------------------------------------------
-- Adoption. One function, so the rule for "is this the device we think it
-- is" lives in exactly one place.
--
-- Resolution order, strongest evidence first:
--   1. branded farm_uid read off the device — we wrote it, so it is ours
--   2. hardware fingerprint
--   3. serial AND the same slot as last time (a serial alone is not enough)
--   4. otherwise adopt a new device
-- ---------------------------------------------------------------------

-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.resolve_device(
  p_host_id    text,
  p_usb_path   text,
  p_farm_uid   text,
  p_hw_fp      bytea,
  p_serial     text,
  p_pool_id    text DEFAULT 'default',
  p_props      jsonb DEFAULT '{}'::jsonb
) RETURNS TABLE (device_id uuid, resolution text, slot_id bigint)
LANGUAGE plpgsql AS $fn$
-- The OUT names shadow columns of farm.devices and farm.device_runtime, and
-- ON CONFLICT's target cannot be qualified, so bare names inside queries mean
-- the column. Assignment targets are still variables.
#variable_conflict use_column
DECLARE
  v_slot  bigint;
  v_dev   uuid;
  v_res   text;
  v_uid   text;
  v_cnt   int;
BEGIN
  SELECT s.id INTO v_slot FROM farm.slots s
   WHERE s.host_id = p_host_id AND s.usb_path = p_usb_path;
  IF v_slot IS NULL THEN
    RAISE EXCEPTION 'no slot registered at %/% — run topology discovery first',
      p_host_id, p_usb_path USING ERRCODE = 'no_data_found';
  END IF;

  -- 1. The device is carrying our brand.
  IF p_farm_uid IS NOT NULL AND p_farm_uid <> '' THEN
    SELECT d.id INTO v_dev FROM farm.devices d WHERE d.farm_uid = p_farm_uid;
    IF v_dev IS NOT NULL THEN
      v_res := 'branded_uid';
    END IF;
  END IF;

  -- 2. Hardware fingerprint.
  IF v_dev IS NULL AND p_hw_fp IS NOT NULL THEN
    SELECT count(*), min(d.id) INTO v_cnt, v_dev
      FROM farm.devices d WHERE d.hw_fingerprint = p_hw_fp;
    IF v_cnt = 1 THEN
      v_res := 'hw_fingerprint';
    ELSIF v_cnt > 1 THEN
      v_dev := NULL;
      v_res := 'ambiguous';
    ELSE
      v_dev := NULL;
    END IF;
  END IF;

  -- 3. Serial, but ONLY together with the slot it was last seen in.
  --    A serial on its own identifies nothing: clone serials are real.
  IF v_dev IS NULL AND p_serial IS NOT NULL AND p_serial <> '' THEN
    SELECT count(*) INTO v_cnt FROM farm.devices d WHERE d.adb_serial = p_serial;
    IF v_cnt = 1 THEN
      SELECT d.id INTO v_dev FROM farm.devices d
       WHERE d.adb_serial = p_serial AND d.current_slot_id = v_slot;
      IF v_dev IS NOT NULL THEN
        v_res := 'serial_and_slot';
      END IF;
    ELSIF v_cnt > 1 THEN
      v_res := 'ambiguous';
      UPDATE farm.devices d SET serial_ambiguous = true
       WHERE d.adb_serial = p_serial AND NOT d.serial_ambiguous;
    END IF;
  END IF;

  -- 4. Nothing matched: adopt it.
  IF v_dev IS NULL THEN
    v_uid := 'df-' || encode(gen_random_bytes(16), 'hex');
    INSERT INTO farm.devices (
      farm_uid, adb_serial, hw_fingerprint, pool_id, host_id, current_slot_id,
      manufacturer, model, product, device_codename,
      android_release, sdk_int, build_fingerprint)
    VALUES (
      v_uid, nullif(p_serial, ''), p_hw_fp, p_pool_id, p_host_id, v_slot,
      p_props->>'manufacturer', p_props->>'model', p_props->>'product',
      p_props->>'device_codename', p_props->>'android_release',
      nullif(p_props->>'sdk_int', '')::int, p_props->>'build_fingerprint')
    RETURNING id INTO v_dev;

    INSERT INTO farm.device_runtime (device_id, host_id, slot_id)
    VALUES (v_dev, p_host_id, v_slot)
    ON CONFLICT (device_id) DO NOTHING;

    v_res := 'adopted_new';

    -- Ambiguity is only visible AFTER the insert. When the second clone of a
    -- duplicated serial arrives, the count at the moment of its own lookup is
    -- still one, so neither device would ever be flagged and an operator would
    -- see two identical serials with nothing saying so. Re-check now and flag
    -- every device carrying this serial.
    IF p_serial IS NOT NULL AND p_serial <> '' THEN
      SELECT count(*) INTO v_cnt FROM farm.devices d WHERE d.adb_serial = p_serial;
      IF v_cnt > 1 THEN
        UPDATE farm.devices d SET serial_ambiguous = true
         WHERE d.adb_serial = p_serial AND NOT d.serial_ambiguous;
      END IF;
    END IF;
  ELSE
    -- Known device. Refresh what we observed and follow it if it moved.
    UPDATE farm.devices d
       SET host_id         = p_host_id,
           current_slot_id = v_slot,
           adb_serial      = COALESCE(nullif(p_serial, ''), d.adb_serial),
           hw_fingerprint  = COALESCE(p_hw_fp, d.hw_fingerprint),
           manufacturer    = COALESCE(p_props->>'manufacturer', d.manufacturer),
           model           = COALESCE(p_props->>'model', d.model),
           android_release = COALESCE(p_props->>'android_release', d.android_release),
           sdk_int         = COALESCE(nullif(p_props->>'sdk_int', '')::int, d.sdk_int),
           build_fingerprint = COALESCE(p_props->>'build_fingerprint', d.build_fingerprint),
           updated_at      = now()
     WHERE d.id = v_dev;
  END IF;

  -- Occupancy: close any stale tenancy, then record the current one.
  UPDATE farm.slot_occupancy o SET until = now(), reason = 'device moved'
   WHERE o.until IS NULL AND o.device_id = v_dev AND o.slot_id <> v_slot;
  UPDATE farm.slot_occupancy o SET until = now(), reason = 'slot reoccupied'
   WHERE o.until IS NULL AND o.slot_id = v_slot AND o.device_id <> v_dev;
  INSERT INTO farm.slot_occupancy (slot_id, device_id)
  SELECT v_slot, v_dev
   WHERE NOT EXISTS (SELECT 1 FROM farm.slot_occupancy o
                      WHERE o.until IS NULL AND o.slot_id = v_slot AND o.device_id = v_dev);

  device_id := v_dev; resolution := v_res; slot_id := v_slot;
  RETURN NEXT;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Register a slot from an observed USB position, creating the controller
-- and hub rows it hangs from. This is what makes topology dynamic: a host
-- reports what it sees and the schema grows to fit.
CREATE OR REPLACE FUNCTION farm.register_slot(
  p_host_id   text,
  p_usb_path  text,
  p_hub_path  text,
  p_port      int,
  p_hub_model text DEFAULT NULL,
  p_ports     int  DEFAULT 7,
  p_switchable boolean DEFAULT false,
  p_rack_slot text DEFAULT NULL
) RETURNS bigint
LANGUAGE plpgsql AS $fn$
DECLARE
  v_bus  int;
  v_ctl  bigint;
  v_hub  bigint;
  v_pd   bigint;
  v_slot bigint;
BEGIN
  v_bus := split_part(p_usb_path, '-', 1)::int;

  INSERT INTO farm.controllers (host_id, root_bus)
  VALUES (p_host_id, v_bus)
  ON CONFLICT (host_id, root_bus) DO UPDATE SET root_bus = EXCLUDED.root_bus
  RETURNING id INTO v_ctl;

  INSERT INTO farm.hubs (host_id, controller_id, usb_path, model, port_count, vbus_switchable)
  VALUES (p_host_id, v_ctl, p_hub_path, p_hub_model, p_ports, p_switchable)
  ON CONFLICT (host_id, usb_path) DO UPDATE
     SET model = COALESCE(EXCLUDED.model, farm.hubs.model),
         vbus_switchable = EXCLUDED.vbus_switchable
  RETURNING id INTO v_hub;

  -- A hub that cannot switch VBUS per port has ONE power domain: cycling
  -- any port cycles them all, and the ladder must know that before it acts.
  SELECT pd.id INTO v_pd FROM farm.power_domains pd
   WHERE pd.host_id = p_host_id
     AND pd.control_addr IS NOT DISTINCT FROM p_hub_path;
  IF v_pd IS NULL THEN
    INSERT INTO farm.power_domains (host_id, kind, control, control_addr)
    VALUES (p_host_id,
            CASE WHEN p_switchable THEN 'per_port' ELSE 'ganged' END,
            CASE WHEN p_switchable THEN 'uhubctl'  ELSE 'none'   END,
            p_hub_path)
    RETURNING id INTO v_pd;
  END IF;

  INSERT INTO farm.slots (host_id, hub_id, power_domain_id, port_number, usb_path,
                          topo_path, rack_slot)
  VALUES (p_host_id, v_hub, v_pd, p_port, p_usb_path,
          text2ltree(regexp_replace(p_host_id || '.' || p_usb_path, '[^A-Za-z0-9_.]', '_', 'g')),
          p_rack_slot)
  ON CONFLICT (host_id, usb_path) DO UPDATE
     SET hub_id = EXCLUDED.hub_id,
         power_domain_id = EXCLUDED.power_domain_id,
         rack_slot = COALESCE(EXCLUDED.rack_slot, farm.slots.rack_slot)
  RETURNING id INTO v_slot;

  RETURN v_slot;
END $fn$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Durable device state write, fenced. The guard is on the INSERT SOURCE
-- rows because ON CONFLICT ... WHERE does not guard the INSERT arm, so the
-- very first write for a device would otherwise accept any token, zero
-- included.
CREATE OR REPLACE FUNCTION farm.device_state_write(
  p_device_id uuid,
  p_fence     bigint,
  p_state     jsonb,
  p_profile   text DEFAULT NULL
) RETURNS bigint
LANGUAGE plpgsql AS $fn$
DECLARE
  v_rev bigint;
BEGIN
  IF p_fence < (SELECT d.fence_floor FROM farm.devices d WHERE d.id = p_device_id) THEN
    RAISE EXCEPTION 'device_state write for % rejected: fence % is below the floor',
      p_device_id, p_fence USING ERRCODE = 'check_violation';
  END IF;

  INSERT INTO farm.device_state (device_id, profile_id, state, revision, written_by_fence, applied_at)
  VALUES (p_device_id, p_profile, p_state, 1, p_fence, now())
  ON CONFLICT (device_id) DO UPDATE
     SET state            = EXCLUDED.state,
         profile_id       = COALESCE(EXCLUDED.profile_id, farm.device_state.profile_id),
         revision         = farm.device_state.revision + 1,
         written_by_fence = EXCLUDED.written_by_fence,
         applied_at       = now(),
         drifted          = false,
         updated_at       = now()
   WHERE EXCLUDED.written_by_fence >= farm.device_state.written_by_fence
  RETURNING revision INTO v_rev;

  IF v_rev IS NULL THEN
    RAISE EXCEPTION 'device_state write for % rejected: a newer fence holds it',
      p_device_id USING ERRCODE = 'check_violation';
  END IF;

  INSERT INTO farm.device_state_revisions (device_id, revision, fence, state)
  VALUES (p_device_id, v_rev, p_fence, p_state)
  ON CONFLICT (device_id, revision) DO NOTHING;

  RETURN v_rev;
END $fn$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS farm.device_state_write(uuid, bigint, jsonb, text);
DROP FUNCTION IF EXISTS farm.register_slot(text, text, text, int, text, int, boolean, text);
DROP FUNCTION IF EXISTS farm.resolve_device(text, text, text, bytea, text, text, jsonb);
ALTER TABLE farm.jobs
  DROP COLUMN IF EXISTS error,
  DROP COLUMN IF EXISTS resumable,
  DROP COLUMN IF EXISTS reset_tier,
  DROP COLUMN IF EXISTS profile_id,
  DROP COLUMN IF EXISTS max_attempts,
  DROP COLUMN IF EXISTS attempt;
DROP TABLE IF EXISTS farm.job_attempts;
DROP TABLE IF EXISTS farm.job_steps;
DROP TABLE IF EXISTS farm.step_kinds;
DROP TABLE IF EXISTS farm.device_artifacts;
DROP TABLE IF EXISTS farm.artifacts;
DROP TABLE IF EXISTS farm.device_state_revisions;
DROP TABLE IF EXISTS farm.device_state;
DROP TABLE IF EXISTS farm.profiles;
DROP TABLE IF EXISTS farm.slot_occupancy;
DROP TABLE IF EXISTS farm.identity_observations;
-- +goose StatementEnd
