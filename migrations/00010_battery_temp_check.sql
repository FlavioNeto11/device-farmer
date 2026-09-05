-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- What the battery columns MEAN.
--
-- farm.device_runtime.battery_pct has carried a CHECK since 00001 and
-- battery_temp_dc has not, so until now the only thing saying what that
-- smallint held was its name. smallint accepts -32768..32767, which as
-- decidegrees is -3276.8 C .. 3276.7 C: every wrong unit a writer could
-- plausibly choose fits inside it. Millidegrees from a sysfs node — the
-- most likely mistake, since /sys/class/power_supply/*/temp is
-- millidegrees on some SoCs and decidegrees on others — would store
-- 29300 for a warm phone and be silently accepted, and 29300 is close
-- enough to a plausible number that nobody would notice until a charge
-- policy refused to charge the fleet.
--
-- This migration adds nothing to the health model. It writes the unit
-- into the schema, where the next writer will find it.
-- =====================================================================

ALTER TABLE farm.device_runtime
  ADD CONSTRAINT device_runtime_battery_temp_dc_check
  CHECK (battery_temp_dc IS NULL OR battery_temp_dc BETWEEN -400 AND 1500);

-- The bounds are deliberately wide: -40.0 C is the bottom of a lithium
-- cell's storage rating and 150.0 C is well past the temperature at
-- which any real device has shut itself down, so a reading inside them
-- is at worst surprising and never impossible. The constraint is a unit
-- check, not a health threshold. Deciding that 45.0 C is too hot to
-- charge is policy, it changes, and policy does not belong in a CHECK
-- that would need a migration to retune.
--
-- The one writer, internal/watchdog/battery.go, filters to exactly this
-- range before it writes, so this can never fire for a row it produced.
-- That is on purpose: its batch UPDATE carries a whole cycle's readings
-- in one statement, and a constraint violation there would discard
-- every OTHER device's reading along with the bad one. The database is
-- the backstop for a future writer, not the validator for this one.

COMMENT ON COLUMN farm.device_runtime.battery_temp_dc IS
  'Battery temperature in DECIDEGREES Celsius, as dumpsys battery reports it: '
  '293 is 29.3 C. NULL means never observed - not zero, not cold. Written by '
  'the watchdog''s battery reader; a device that cannot answer leaves the '
  'column alone.';

COMMENT ON COLUMN farm.device_runtime.battery_pct IS
  'Battery charge as a percentage, computed from dumpsys battery''s level '
  'against its scale rather than assuming a scale of 100. NULL means never '
  'observed.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE farm.device_runtime
  DROP CONSTRAINT IF EXISTS device_runtime_battery_temp_dc_check;
COMMENT ON COLUMN farm.device_runtime.battery_temp_dc IS NULL;
COMMENT ON COLUMN farm.device_runtime.battery_pct IS NULL;
-- +goose StatementEnd
