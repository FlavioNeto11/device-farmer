-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- farm.v_hub_health.unhealthy disagreed with the hub-fault quorum.
--
-- Two queries answer "how many devices on this hub are broken", and until
-- this migration they answered differently for the same hub at the same
-- instant:
--
--   the ladder's quorum   internal/recovery/ladder.go, UnhealthyStates
--                         health IN ('offline','unauthorized','missing','degraded')
--   this view             health NOT IN ('healthy','retired','parked')
--
-- The view is a deny-list, so it also counts 'unknown', 'recovering',
-- 'quarantined' and 'booting'. Verified live: a hub whose seven devices
-- were all quarantined read 7/7 unhealthy on the fleet banner while the
-- correlate query read 0/7 — and it is the banner an operator stares at
-- during an incident. Any alert threshold written against one number
-- fired, or failed to fire, on the other.
--
-- The quorum's allow-list is the one that is right, and this view now
-- carries it. Each excluded value is excluded for a specific loop it
-- would otherwise create — the ladder's own doc comment on UnhealthyStates
-- is the authority, and the two that matter most for a VIEW are:
--
--   'unknown'      is not an observation. internal/recovery's
--                  reconcileQuarantines writes it to EVERY device on a
--                  hub in ONE statement the moment an operator closes
--                  that hub's quarantine, so all of them carry the same
--                  health_since. The spread between first and last is
--                  zero, a quorum computed over it is unanimous by
--                  construction, and a banner that counted it would show
--                  the hub the operator just cleared as 100% unhealthy —
--                  and the ladder, had it agreed, would re-open the
--                  quarantine about a debounce window later, forever.
--   'quarantined'  is the ladder's own bookkeeping. A quarantined hub is
--                  reported by farm.quarantines, not by counting its
--                  devices as freshly broken every refresh.
--
-- 'recovering' is this loop's induced state; 'booting' is transient and
-- a mass reboot is not a hub fault; 'parked' and 'retired' are decisions.
--
-- worst_since moves with it: it is "when the fault evidence started", and
-- dating a hub fault from a quarantine the ladder itself opened, or from
-- a device nobody has looked at, is the same mistake in a timestamp.
--
-- The list here is pinned to recovery.UnhealthyStates by a Go test that
-- reads this file, and test/assertions_v13.sql checks the view against
-- every value farm.device_runtime.health may hold. Change all three or
-- none.
-- =====================================================================
CREATE OR REPLACE VIEW farm.v_hub_health AS
SELECT h.id                AS hub_id,
       h.host_id,
       h.usb_path,
       h.model,
       h.vbus_switchable,
       count(d.id)                                                        AS devices,
       count(*) FILTER (WHERE r.health = 'healthy')                       AS healthy,
       count(*) FILTER (WHERE r.health IN ('offline','unauthorized','missing','degraded'))
                                                                          AS unhealthy,
       max(r.health_since) FILTER (WHERE r.health IN ('offline','unauthorized','missing','degraded'))
                                                                          AS worst_since
  FROM farm.hubs h
  LEFT JOIN farm.slots s          ON s.hub_id = h.id
  LEFT JOIN farm.devices d        ON d.current_slot_id = s.id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
 GROUP BY h.id, h.host_id, h.usb_path, h.model, h.vbus_switchable;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON VIEW farm.v_hub_health IS
  'Per-hub device counts for the correlation banner. unhealthy counts ONLY positive fault evidence '
  '(offline, unauthorized, missing, degraded) — the same predicate as the recovery ladder''s hub-fault '
  'quorum — so the banner and the quarantine reason agree. unknown, recovering, quarantined, booting, '
  'parked and retired are not evidence and are not counted.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The definition 00008 left in place: healthy/retired/parked excluded, everything
-- else counted. Rolling back restores the disagreement with the ladder; it does
-- not restore anything the ladder relies on.
CREATE OR REPLACE VIEW farm.v_hub_health AS
SELECT h.id                AS hub_id,
       h.host_id,
       h.usb_path,
       h.model,
       h.vbus_switchable,
       count(d.id)                                                            AS devices,
       count(*) FILTER (WHERE r.health = 'healthy')                           AS healthy,
       count(*) FILTER (WHERE r.health NOT IN ('healthy','retired','parked')) AS unhealthy,
       max(r.health_since) FILTER (WHERE r.health NOT IN ('healthy','parked')) AS worst_since
  FROM farm.hubs h
  LEFT JOIN farm.slots s          ON s.hub_id = h.id
  LEFT JOIN farm.devices d        ON d.current_slot_id = s.id
  LEFT JOIN farm.device_runtime r ON r.device_id = d.id
 GROUP BY h.id, h.host_id, h.usb_path, h.model, h.vbus_switchable;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON VIEW farm.v_hub_health IS NULL;
-- +goose StatementEnd
