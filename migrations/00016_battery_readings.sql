-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- Battery HISTORY, so that a cell can be seen changing.
--
-- farm.device_runtime.battery_pct and .battery_temp_dc are a two-column
-- overwrite: every reading replaces the last, and the fleet remembers
-- one number per device. That is the right shape for "how full is it"
-- and useless for the only battery question that is about safety, which
-- is a RATE — a cell heating at two degrees a minute is on its way to
-- thermal runaway at every absolute temperature it passes through, and
-- a cell that loses charge while sitting on a powered port cannot hold
-- what it is given. Neither is visible in a column that has forgotten
-- the previous minute.
--
-- The physical research behind this table is in docs/siting.md §2:
-- clean-agent suppression does not stop a lithium event, and the
-- mitigations that do are containment, spacing, charge limiting and
-- early detection. This is the early-detection half. It does not act
-- on anything — the reader in internal/watchdog/swell.go raises an
-- event and a gauge, and a human walks to the rack_slot.
--
-- One row per reading per device, keyed by the server's clock. The CHECKs
-- are the same ones the runtime columns carry (00001 for pct, 00010 for
-- temperature): the same unit, stated in the same place, so a writer
-- that satisfies one satisfies the other. The third CHECK says that a
-- row with nothing in it is not a reading — a device that answered with
-- neither value produced no observation, and the honest record of that
-- is no row, exactly as it is no column change.
-- =====================================================================

CREATE TABLE farm.battery_readings (
  device_id  uuid        NOT NULL REFERENCES farm.devices(id) ON DELETE CASCADE,
  at         timestamptz NOT NULL DEFAULT now(),
  pct        smallint    CHECK (pct IS NULL OR pct BETWEEN 0 AND 100),
  temp_dc    smallint    CHECK (temp_dc IS NULL OR temp_dc BETWEEN -400 AND 1500),
  CONSTRAINT battery_readings_not_empty CHECK (pct IS NOT NULL OR temp_dc IS NOT NULL),
  PRIMARY KEY (device_id, at)
);

-- The primary key IS the "latest N per device" index: a btree on
-- (device_id, at) answers `WHERE device_id = $1 ORDER BY at DESC LIMIT n`
-- with one backward range scan, and a second index on the same two
-- columns in the other direction would cost every INSERT a second
-- write for no plan Postgres cannot already make.
--
-- The prune below deletes by time alone, across every device, so it
-- needs the other axis.
CREATE INDEX battery_readings_at ON farm.battery_readings (at);

COMMENT ON TABLE farm.battery_readings IS
  'One row per battery observation per device, written by the watchdog''s '
  'battery reader in the same statement that updates farm.device_runtime. '
  'Read by the swell detector for rates; pruned by farm.battery_readings_prune.';
COMMENT ON COLUMN farm.battery_readings.at IS
  'The server''s now() at the moment the batch was written. Never a client clock.';
COMMENT ON COLUMN farm.battery_readings.temp_dc IS
  'Decidegrees Celsius, as dumpsys battery reports it: 293 is 29.3 C.';

-- +goose StatementEnd

-- +goose StatementBegin

-- ---------------------------------------------------------------------
-- Retention.
--
-- A minute-cadence reading on a sixty-device fleet is under a hundred
-- thousand rows a day. Seven days keeps a week of "what did this cell do
-- before it was pulled" — the question an operator asks after the walk
-- to the rack, not before — and is short enough that the table never
-- needs a partition scheme. The watchdog calls this once an hour.
--
-- The floor is the detector's own window, not a taste: swell.go reads
-- the last thirty minutes of readings and suppresses a repeat of the
-- same anomaly for an hour, so a keep interval below an hour would
-- delete the evidence an open alert is standing on. A retention that
-- can blind the reader is refused rather than honoured.
-- ---------------------------------------------------------------------
DROP FUNCTION IF EXISTS farm.battery_readings_prune(interval);

CREATE FUNCTION farm.battery_readings_prune(p_keep interval DEFAULT interval '7 days')
RETURNS bigint
LANGUAGE plpgsql AS $fn$
DECLARE
  v_deleted bigint;
BEGIN
  IF p_keep IS NULL OR p_keep < interval '1 hour' THEN
    RAISE EXCEPTION 'battery_readings_prune: p_keep (%) must be at least one hour — '
                    'the swell detector reads the last 30 minutes and holds an anomaly for 60',
                    p_keep
      USING ERRCODE = 'invalid_parameter_value';
  END IF;

  DELETE FROM farm.battery_readings WHERE at < now() - p_keep;
  GET DIAGNOSTICS v_deleted = ROW_COUNT;
  RETURN v_deleted;
END $fn$;

COMMENT ON FUNCTION farm.battery_readings_prune(interval) IS
  'Deletes readings older than p_keep and returns how many. Refuses a keep '
  'below one hour, the swell detector''s evidence window.';

-- +goose StatementEnd

-- +goose StatementBegin

-- ---------------------------------------------------------------------
-- The ledger row must name a position.
--
-- A battery_anomaly event is read by a person who is about to walk to a
-- shelf. farm.events.detail is free-form jsonb for every other kind and
-- stays so; for this one kind the schema refuses a row whose detail
-- carries no rack_slot, because an anomaly that names a uuid and no
-- position is a page nobody can act on — and it is exactly the row a
-- refactor produces silently, by dropping one key from a map. The
-- writer (internal/watchdog/swell.go) derives a position from host, hub
-- and port when farm.slots.rack_slot is unlabelled, the way
-- topo.Labeler does for a host with no rack coordinates, so this CHECK
-- never fires for it. It is there for the writer after the next one.
-- ---------------------------------------------------------------------
ALTER TABLE farm.events
  ADD CONSTRAINT events_battery_anomaly_names_a_position
  CHECK (kind <> 'battery_anomaly' OR COALESCE(detail->>'rack_slot', '') <> '');

-- +goose StatementEnd

-- +goose StatementBegin

-- ---------------------------------------------------------------------
-- Privileges.
--
-- The watchdog role gains its own history table, the function that
-- trims it, and INSERT on farm.events — the ledger every other loop
-- already writes to, and the one place a "walk to R2-U14-H3.1.4-P5 now"
-- can be found afterwards. None of the three is fleet state: the
-- package rule that the watchdog writes exactly one table of fleet
-- state (farm.device_runtime) is unchanged, and so is the one that
-- matters most, which is re-asserted here rather than assumed: this
-- role has no privilege of any kind on farm.leases. A hot battery is a
-- reason for a human to walk, never a reason for the control plane to
-- take a device away from the job holding it.
-- ---------------------------------------------------------------------
DO $$
BEGIN
  EXECUTE 'GRANT SELECT, INSERT, DELETE ON farm.battery_readings TO farm_watchdog';
  EXECUTE 'GRANT EXECUTE ON FUNCTION farm.battery_readings_prune(interval) TO farm_watchdog';
  EXECUTE 'GRANT INSERT ON farm.events TO farm_watchdog';
  EXECUTE 'REVOKE ALL ON farm.leases FROM farm_watchdog';
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  EXECUTE 'REVOKE INSERT ON farm.events FROM farm_watchdog';
END $$;
ALTER TABLE farm.events DROP CONSTRAINT IF EXISTS events_battery_anomaly_names_a_position;
DROP FUNCTION IF EXISTS farm.battery_readings_prune(interval);
DROP TABLE IF EXISTS farm.battery_readings;
-- +goose StatementEnd
