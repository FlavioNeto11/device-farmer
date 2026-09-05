# DeviceFarmerHubDown / DeviceFarmerHubDegraded

**Severity:** critical / warning · **Group:** `device-farmer.devices`

```promql
# HubDown
(sum by (host, hub) (farm_device_health{state="healthy"}) == 0)
  and (sum by (host, hub) (farm_device_health) > 1)                   for 10m

# HubDegraded
(sum by (host, hub) (farm_device_health{state=~"offline|unauthorized|missing|degraded"})
   / sum by (host, hub) (farm_device_health)) > 0.5
  and sum by (host, hub) (farm_device_health) > 1                     for 15m
```

## What fired

Several devices sharing one USB hub went unhealthy at the same time. `HubDown`
means none of them is healthy any more; `HubDegraded` means more than half.

## What it means

**Suspect the hub, its cable or its power domain. Not the phones.**

Handsets do not fail in unison. When a dozen devices on one hub go dark together
the cause is upstream of all of them: a cable seated badly, a hub that has lost
power, a power domain that tripped, a host whose ADB server died, or the host
itself.

This is why `farm_device_health` is aggregated per hub and zero-filled across
all ten health states rather than reported per device. Twelve simultaneous
device alerts are noise a human silences at 03:00; one hub alert is a person
walking to a rack. The zero-fill is what makes `== 0` work — a hub that dies has
its `healthy` count fall to a *visible* zero instead of its series simply
vanishing, and a vanished series matches nothing and fires nothing.

**No lease has ended.** Device health and lease liveness are separate clocks, on
purpose. A phone that has gone `offline` on ADB is still leased to whoever had
it, and its job may well still be running on the handset itself — the recovery
ladder will refuse to disturb it if the holder's `disruption_policy` says so.

## Where to walk

The alert carries `rack_slot` in the form `H3.1.4-P*`: the hub token is the
hub's USB path with `-` rewritten to `.`, which is how `internal/topo` builds
the real slot labels. The rack and shelf come from the host:

```sh
psql "$PGURL" -c "SELECT id, rack_id, rack_unit, adb_endpoint FROM farm.hosts WHERE id = '<host>'"
```

So `host=h01`, `hub=3-1.4`, `rack_id=R2`, `rack_unit=14` means rack 2, shelf 14,
the hub reached through USB path 3-1.4 — and the affected sockets are
`R2-U14-H3.1.4-P*`. The exact list:

```sh
farmd ctl fleet --host <host> --hub <hub>
```

## What is NOT wrong

- **A hub with one device on it.** Excluded by `> 1` on purpose. One dead phone
  is the recovery ladder's job, not a page.
- **Devices rebooting under their own tests.** A whole hub can legitimately go
  quiet for a minute when a cohort of jobs all call `adb reboot` at once. The
  10–15 minute `for:` is sized for that.
- **`state="recovering"`.** Excluded from the degraded ratio: the ladder is
  already working on it.
- **`state="booting"`.** Also excluded. A device doing what it was asked to do.
- **Transport blips.** `farm_adb_transport_blips_total` on this hub is
  correlation material for the dashboard and nothing else. It never justifies
  ending a lease, and it is not what fired this.

## Read the states before you walk

The mix tells you what kind of fault it is, and it changes where you go:

| Dominant state | Almost certainly | First move |
| --- | --- | --- |
| `missing` | The hub or its cable is gone; devices are not enumerating | Physical: reseat, check the hub's own power |
| `offline` | Enumerated but ADB cannot talk to them | Host-side: the adb server, or the hub |
| `unauthorized` | The host's ADB key changed | **Not hardware.** Re-authorise the host |
| `degraded` | Negotiated link speed collapsed, or battery gates | Cable quality, or a genuinely tired set of phones |

`unauthorized` across a whole hub after a host re-image is the classic
false-hardware-alarm: nothing is broken, the host just has a new RSA key and
every device is waiting for somebody to tap "always allow".

## First three checks

**1. Confirm the correlation and see the states.**

```sh
psql "$PGURL" -c "
SELECT h.host_id, h.usb_path, h.model, h.vbus_switchable,
       hh.devices, hh.healthy, hh.unhealthy, hh.worst_since
  FROM farm.v_hub_health hh JOIN farm.hubs h ON h.id = hh.hub_id
 ORDER BY hh.unhealthy DESC, hh.host_id"
psql "$PGURL" -c "
SELECT rack_slot, farm_uid, adb_state, health, health_since, consec_bad, ladder_tier,
       lease_state, job_id
  FROM farm.v_fleet
 WHERE host_id = '<host>' AND hub_path = '<hub>'
 ORDER BY rack_slot"
```

`worst_since` is when it started. If every device's `health_since` is within a
few seconds of the others, it is one upstream event; if they are spread over
hours, it is a hub wearing out rather than a hub that fell over.

**2. Is the host itself alive, and is its ADB server up?**

```sh
curl -s "$FARM_API_URL/api/v1/capabilities" | jq '.roles'
farmd ctl hosts
psql "$PGURL" -c "
SELECT id, adb_endpoint, node_endpoint, host_epoch, admin_state,
       now() - last_seen_at AS unseen FROM farm.hosts WHERE id = '<host>'"
```

If **every** hub on the host is unhealthy, the fault is the host or its ADB
server, not this hub. A bumped `host_epoch` means the adb server restarted,
which severs every transport on the machine at once.

**3. Is it a power domain rather than a hub?** Sockets sharing a ganged power
domain fail together even when they are on different hubs.

```sh
psql "$PGURL" -c "
SELECT pd.id, pd.kind, pd.control, pd.control_addr, count(s.id) AS slots
  FROM farm.power_domains pd LEFT JOIN farm.slots s ON s.power_domain_id = pd.id
 WHERE pd.host_id = '<host>' GROUP BY pd.id ORDER BY pd.id"
psql "$PGURL" -c "
SELECT s.power_domain_id, count(*) FILTER (WHERE r.health <> 'healthy') AS unhealthy, count(*) AS total
  FROM farm.slots s JOIN farm.device_runtime r ON r.slot_id = s.id
 WHERE s.host_id = '<host>' GROUP BY s.power_domain_id ORDER BY 2 DESC"
```

A power domain that is entirely unhealthy while its hub is only partly so is a
tripped supply, not a data problem.

## What to do about it

**Stop new work landing there before you touch anything.** Draining takes
nothing away from anyone: it stops *new* placement and lets live leases run out
untouched.

```sh
farmd ctl host drain <host> --reason "hub <hub> correlated failure, investigating"
```

There is no hub-level drain in `ctl`; the recovery ladder opens hub-scoped
quarantines itself when it sees correlated failure, and you can see them with
`farmd ctl recovery`.

Then do the physical work. When it is fixed, `farmd ctl host undrain <host>` and
close any quarantine the ladder opened — devices go back to `unknown` health and
the watchdog re-observes them from scratch, which is deliberate: nobody has
looked at them since, and `healthy` would be an assumption the allocator acts on.

## When to escalate

- **Live leases are still held on the affected slots.** Their jobs may still be
  running on the handsets. Do **not** power-cycle the hub to "clear" it — that
  destroys work the ladder is deliberately refusing to destroy. Check first:
  ```sh
  psql "$PGURL" -c "
  SELECT rack_slot, lease_id, job_id, tenant_id, lease_state, protected
    FROM farm.v_fleet WHERE host_id='<host>' AND hub_path='<hub>' AND lease_id IS NOT NULL"
  ```
- **Several hubs on several hosts at once.** That is a rack-level power or
  network event, not a hub.
- **It clears and comes back on a cycle.** A marginal cable or a hub near its
  current limit. Replace the hardware; the alert is doing its job and will keep
  doing it.
