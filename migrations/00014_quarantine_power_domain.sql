-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- A power-domain quarantine had no way to name its subject.
--
-- farm.quarantines.scope has permitted 'power_domain' since 00003, and
-- the recovery ladder's tier 4 ('port_power') is declared with exactly
-- that blast radius: on a ganged hub, cycling one port cycles seven. The
-- quarantine that ought to follow a ganged domain failing — "nothing on
-- this switch is schedulable until somebody looks" — could not be
-- written, because the table carries device_id, slot_id, hub_id and
-- host_id and nothing for a power domain. Every predicate that asks "is
-- this device covered?" therefore had a fallback arm for a row shape
-- that could not exist, and farm.v_fleet's arm compared q.slot_id to the
-- device's slot, which for a row with no slot_id is NULL — covering no
-- slotted device at all, and every UNSLOTTED device, the moment the
-- first such row was ever inserted.
--
-- On mixed hardware the choice was one device or the whole hub. This
-- adds the column, the one-open-row-per-domain index the other four
-- scopes already have, and a CHECK that the subject column named by
-- scope is actually present — so a row can no longer claim a scope it
-- cannot identify. The predicates in internal/recovery and internal/api
-- gain a real 'power_domain' arm in the same change, and 'slot', which
-- was equally unwritten, gets its first writer: POST /api/v1/quarantines.
-- =====================================================================

ALTER TABLE farm.quarantines
  ADD COLUMN IF NOT EXISTS power_domain_id bigint
    REFERENCES farm.power_domains(id) ON DELETE CASCADE;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN farm.quarantines.power_domain_id IS
  'Subject of a scope=power_domain row: every slot in the domain is covered. NULL for every other scope.';
-- +goose StatementEnd

-- +goose StatementBegin
-- One open quarantine per power domain, as for the other four scopes.
CREATE UNIQUE INDEX IF NOT EXISTS q_open_power_domain
  ON farm.quarantines (power_domain_id)
  WHERE closed_at IS NULL AND scope = 'power_domain';
-- +goose StatementEnd

-- +goose StatementBegin
-- The subject column that scope names must be present. Extra columns are
-- allowed and used: the ladder fills host_id on a device row so the row can
-- be reported without a slot lookup, and every predicate that reads this
-- table is driven by scope, not by which columns happen to be populated.
-- What this forbids is the row that names a scope and no subject — which
-- every coverage predicate would silently treat as covering nothing.
ALTER TABLE farm.quarantines
  DROP CONSTRAINT IF EXISTS quarantines_subject_matches_scope;
ALTER TABLE farm.quarantines
  ADD CONSTRAINT quarantines_subject_matches_scope CHECK (
    CASE scope
      WHEN 'device'       THEN device_id       IS NOT NULL
      WHEN 'slot'         THEN slot_id         IS NOT NULL
      WHEN 'power_domain' THEN power_domain_id IS NOT NULL
      WHEN 'hub'          THEN hub_id          IS NOT NULL
      WHEN 'host'         THEN host_id         IS NOT NULL
      ELSE false
    END);
-- +goose StatementEnd

-- +goose StatementBegin
-- farm.v_fleet, verbatim from 00005 except for the power_domain arm of the
-- LATERAL, which now compares the column that exists. The previous arm,
-- `q.slot_id IS NOT DISTINCT FROM s.id`, was a placeholder for a row shape
-- that could not be written; with the column in place it would have covered
-- exactly the wrong set.
CREATE OR REPLACE VIEW farm.v_fleet AS
SELECT d.id                AS device_id,
       d.farm_uid, d.adb_serial, d.serial_ambiguous, d.model, d.manufacturer,
       d.android_release, d.sdk_int, d.pool_id, d.admin_state, d.labels,
       d.failure_score, d.fence_floor,
       s.id                AS slot_id,
       s.rack_slot, s.usb_path, s.adb_devpath,
       s.state             AS slot_state,
       s.rearm_at,
       hb.id               AS hub_id,
       hb.usb_path         AS hub_path,
       hb.vbus_switchable,
       ho.id               AS host_id,
       ho.adb_endpoint,
       ho.admin_state      AS host_admin_state,
       r.adb_state, r.health, r.health_since, r.battery_pct, r.battery_temp_dc,
       r.consec_bad, r.ladder_tier, r.last_seen_at,
       l.id                AS lease_id,
       l.fence, l.state    AS lease_state,
       l.protected, l.job_id, l.tenant_id, l.holder,
       l.acquired_at, l.expires_at, l.reclaimable_at,
       q.id                AS quarantine_id,
       q.reason            AS quarantine_reason,
       q.scope             AS quarantine_scope
  FROM farm.devices d
  LEFT JOIN farm.slots s          ON s.id = d.current_slot_id
  LEFT JOIN farm.hubs hb          ON hb.id = s.hub_id
  LEFT JOIN farm.hosts ho         ON ho.id = d.host_id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
  LEFT JOIN farm.leases l         ON l.id = d.current_lease_id
  LEFT JOIN LATERAL (
    -- The narrowest open quarantine that covers this device, whatever
    -- scope it was opened at.
    SELECT q.id, q.reason, q.scope
      FROM farm.quarantines q
     WHERE q.closed_at IS NULL
       AND (   (q.scope = 'device' AND q.device_id = d.id)
            OR (q.scope = 'slot'   AND q.slot_id   = s.id)
            OR (q.scope = 'hub'    AND q.hub_id    = hb.id)
            OR (q.scope = 'host'   AND q.host_id   = ho.id)
            OR (q.scope = 'power_domain' AND q.power_domain_id = s.power_domain_id))
     ORDER BY CASE q.scope WHEN 'device' THEN 0 WHEN 'slot' THEN 1
                           WHEN 'power_domain' THEN 2 WHEN 'hub' THEN 3
                           ELSE 4 END
     LIMIT 1
  ) q ON true;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The view references the column, so it goes back to its 00005 shape first;
-- dropping the column with the view in place would need CASCADE, and that
-- would take the fleet grid down with it.
CREATE OR REPLACE VIEW farm.v_fleet AS
SELECT d.id                AS device_id,
       d.farm_uid, d.adb_serial, d.serial_ambiguous, d.model, d.manufacturer,
       d.android_release, d.sdk_int, d.pool_id, d.admin_state, d.labels,
       d.failure_score, d.fence_floor,
       s.id                AS slot_id,
       s.rack_slot, s.usb_path, s.adb_devpath,
       s.state             AS slot_state,
       s.rearm_at,
       hb.id               AS hub_id,
       hb.usb_path         AS hub_path,
       hb.vbus_switchable,
       ho.id               AS host_id,
       ho.adb_endpoint,
       ho.admin_state      AS host_admin_state,
       r.adb_state, r.health, r.health_since, r.battery_pct, r.battery_temp_dc,
       r.consec_bad, r.ladder_tier, r.last_seen_at,
       l.id                AS lease_id,
       l.fence, l.state    AS lease_state,
       l.protected, l.job_id, l.tenant_id, l.holder,
       l.acquired_at, l.expires_at, l.reclaimable_at,
       q.id                AS quarantine_id,
       q.reason            AS quarantine_reason,
       q.scope             AS quarantine_scope
  FROM farm.devices d
  LEFT JOIN farm.slots s          ON s.id = d.current_slot_id
  LEFT JOIN farm.hubs hb          ON hb.id = s.hub_id
  LEFT JOIN farm.hosts ho         ON ho.id = d.host_id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
  LEFT JOIN farm.leases l         ON l.id = d.current_lease_id
  LEFT JOIN LATERAL (
    SELECT q.id, q.reason, q.scope
      FROM farm.quarantines q
     WHERE q.closed_at IS NULL
       AND (   (q.scope = 'device' AND q.device_id = d.id)
            OR (q.scope = 'slot'   AND q.slot_id   = s.id)
            OR (q.scope = 'hub'    AND q.hub_id    = hb.id)
            OR (q.scope = 'host'   AND q.host_id   = ho.id)
            OR (q.scope = 'power_domain' AND q.slot_id IS NOT DISTINCT FROM s.id))
     ORDER BY CASE q.scope WHEN 'device' THEN 0 WHEN 'slot' THEN 1
                           WHEN 'power_domain' THEN 2 WHEN 'hub' THEN 3
                           ELSE 4 END
     LIMIT 1
  ) q ON true;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE farm.quarantines DROP CONSTRAINT IF EXISTS quarantines_subject_matches_scope;
DROP INDEX IF EXISTS farm.q_open_power_domain;
ALTER TABLE farm.quarantines DROP COLUMN IF EXISTS power_domain_id;
-- +goose StatementEnd
