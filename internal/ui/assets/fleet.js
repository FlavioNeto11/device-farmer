/* The Fleet view: the grid an operator lands on, the filters above it and the
 * card each device gets.
 *
 * Extracted from app.js because this is the view that changes most and the one
 * that repaints under a live event stream, so a change to how a device reads
 * should not put a reviewer in front of the docs typography or the job form.
 *
 * # How this file reaches app.js, and app.js reaches this file
 *
 * Both are classic scripts, so their top-level function declarations land in
 * one shared global scope: this file calls el(), t(), state and openDevice()
 * from app.js, and app.js calls renderFleet() from here. Nothing is exported
 * and nothing is imported. index.html loads this BEFORE app.js — not because
 * declarations need it, they are hoisted per script, but because app.js is the
 * only file that CALLS anything at load time.
 *
 * The one rule that keeps that working: nothing at this file's top level may
 * read a top-level const or let that app.js declares. Those live in the same
 * shared lexical scope but are in their temporal dead zone until app.js runs.
 * Inside a function body is always fine, because functions run later.
 */

/* ------------------------------------------------------------------ *
 * FLEET
 * ------------------------------------------------------------------ */

function fleetRows() {
  const rows = state.data.fleet || [];
  const f = state.filters;
  const q = state.q.trim().toLowerCase();
  return rows.filter((d) => {
    if (f.host && String(d.host || '') !== f.host) return false;
    if (f.hub && hubParamOf(d) !== f.hub) return false;
    if (f.health && !healthMatches(d, f.health)) return false;
    if (f.pool && String(d.pool || '') !== f.pool) return false;
    if (f.lease) {
      const live = d.leaseState === 'held' || d.leaseState === 'suspect';
      if (f.lease === 'free' && live) return false;
      if (f.lease === 'held' && d.leaseState !== 'held') return false;
      if (f.lease === 'suspect' && d.leaseState !== 'suspect') return false;
      if (f.lease === 'protected' && !(live && d.protected)) return false;
    }
    if (q) {
      const hay = [d.rackSlot, d.model, d.manufacturer, d.farmUID, d.serial, d.host, d.hubPath,
        d.usbPath, d.pool, d.holder, d.jobID, d.id, d.tenant, d.health, d.adbState,
        d.labels ? JSON.stringify(d.labels) : ''].join(' ').toLowerCase();
      if (!hay.includes(q)) return false;
    }
    return true;
  });
}

/* healthMatches applies the ?health= filter the same way the API does.
 *
 * "unhealthy" is not a health value: the API reads it as "every state except
 * healthy and the ones that are decisions rather than faults" — see
 * NOT_A_FAULT. Comparing it literally against a device's health matched
 * nothing, so a request that the server answered with rows — a shared
 * #/fleet?health=unhealthy link, or the filter below — painted an empty grid
 * over a full response. */
function healthMatches(d, want) {
  const h = d.health || 'unknown';
  if (want === 'unhealthy') return isFault(h);
  return h === want;
}

/* hubKeyOf groups devices onto one hub inside this page. Its "path:" and
 * "nohub" forms are local map keys and nothing else. */
function hubKeyOf(d) {
  if (d.hubID !== undefined && d.hubID !== null) return String(d.hubID);
  return d.hubPath ? 'path:' + d.hubPath : 'nohub';
}

/* hubParamOf is the value the API understands for ?hub=, which it matches
 * against hub_path OR hub_id::text — and against nothing else. The grouping
 * key above must never be sent: "path:3-1" matches neither column, so the
 * server would answer with an empty farm and the grid would agree with it. */
function hubParamOf(d) {
  if (d.hubID !== undefined && d.hubID !== null) return String(d.hubID);
  return d.hubPath ? String(d.hubPath) : '';
}

function renderFleet() {
  const body = $('#fleet-body');
  const alerts = $('#fleet-alerts');
  body.setAttribute('aria-busy', state.loading.fleet ? 'true' : 'false');

  const all = state.data.fleet;
  const rows = fleetRows();

  // Counts: whatever the server computed, plus what is on screen after
  // filtering, so the two are never confused with each other.
  const counts = $('#fleet-counts');
  counts.replaceChildren();
  append(counts, [countChips(state.data.counts)]);
  if (all) {
    counts.append(el('span', { class: 'count', title: 'rows currently rendered after filters' },
      t('fleet.showing') + ' ', el('b', null, String(rows.length)), ' / ' + all.length));
  }
  append(counts, [truncChip('fleet', 'Narrow the host, hub or pool filter to see the rest.')]);

  refreshFilterOptions(all || []);

  const problem = panelState('fleet', all, emptyState(
    'No devices match.',
    'This grid shows every device in farm.v_fleet grouped by host and then by hub — rack slot, model, health, lease and battery. Clear the filters, or check that the watchdog has registered devices.'));
  if (problem) { body.replaceChildren(problem); alerts.replaceChildren(); return; }

  // Group by host, then by hub: the physical failure unit, in the order a
  // human walks the room.
  const byHost = new Map();
  for (const d of rows) {
    const host = d.host || '(unassigned host)';
    if (!byHost.has(host)) byHost.set(host, new Map());
    const hubs = byHost.get(host);
    const hk = hubKeyOf(d);
    if (!hubs.has(hk)) hubs.set(hk, []);
    hubs.get(hk).push(d);
  }

  const hostMeta = new Map((state.data.hosts || []).map((h) => [String(h.id), h]));
  const hubMeta = new Map((state.data.hubs || []).filter((h) => h.id !== undefined && h.id !== null).map((h) => [String(h.id), h]));

  const frag = document.createDocumentFragment();

  for (const host of Array.from(byHost.keys()).sort(cmp)) {
    const hubs = byHost.get(host);
    const hostDevices = rows.filter((d) => (d.host || '(unassigned host)') === host);
    const meta = hostMeta.get(String(host));
    const adminState = (meta && meta.adminState) || (hostDevices[0] && hostDevices[0].hostAdminState) || 'enabled';
    const live = hostDevices.filter((d) => d.leaseState === 'held' || d.leaseState === 'suspect');
    const bad = hostDevices.filter((d) => isFault(d.health));

    const head = el('div', { class: 'host-head' },
      el('span', { class: 'host-name' }, host),
      adminState !== 'enabled'
        ? el('span', { class: 'chip chip-drain', title: 'host admin_state' }, el('span', { 'aria-hidden': 'true' }, '⏸'), adminState)
        : el('span', { class: 'chip chip-ok', title: 'host admin_state' }, el('span', { 'aria-hidden': 'true' }, '✓'), 'enabled'),
      el('span', { class: 'count' }, 'devices ', el('b', null, String(hostDevices.length))),
      el('span', { class: 'count' }, 'unhealthy ', el('b', null, String(bad.length))),
      el('span', { class: 'count' }, 'live leases ', el('b', null, String(live.length))),
      meta && meta.lastSeen ? el('span', { class: 'count' }, 'seen ', el('b', null, fmtRel(meta.lastSeen))) : null,
      el('span', { class: 'host-actions' },
        adminState === 'draining' || adminState === 'disabled'
          ? el('button', { class: 'mini', onclick: () => undrainHost(host, hostDevices) }, 'Undrain')
          : el('button', { class: 'mini', onclick: () => drainHost(host, hostDevices) }, 'Drain')));

    const block = el('div', { class: 'host-block' }, head);

    const hubKeys = Array.from(hubs.keys()).sort((a, b) => {
      const da = hubs.get(a)[0], db = hubs.get(b)[0];
      return cmp(da.hubPath || a, db.hubPath || b);
    });

    for (const hk of hubKeys) {
      const devices = hubs.get(hk).slice().sort((a, b) => cmp(a.rackSlot || a.usbPath, b.rackSlot || b.usbPath));
      const first = devices[0];
      const hubPath = first.hubPath || (hk === 'nohub' ? null : hk);
      const meta2 = hubMeta.get(hk);

      // Prefer the server's v_hub_health numbers (they count every device on
      // the hub, not just the ones passing the current filter).
      const total = meta2 && meta2.devices !== undefined ? Number(meta2.devices) : devices.length;
      const unhealthyList = devices.filter((d) => isFault(d.health));
      const unhealthy = meta2 && meta2.unhealthy !== undefined ? Number(meta2.unhealthy) : unhealthyList.length;
      let since = meta2 && meta2.worstSince ? meta2.worstSince : null;
      if (!since) {
        for (const d of unhealthyList) {
          const t = parseTime(d.healthSince);
          if (t && (!since || t > parseTime(since))) since = d.healthSince;
        }
      }

      const hubHead = el('div', { class: 'hub-head' },
        el('span', { class: 'hub-name' }, hubPath ? 'hub ' + hubPath : 'no hub recorded'),
        meta2 && meta2.model ? el('span', null, meta2.model) : null,
        el('span', null, devices.length + (devices.length === 1 ? ' device' : ' devices')),
        meta2 && meta2.vbus ? el('span', { class: 'chip chip-plain', title: 'this hub can switch VBUS per port' }, 'switchable') : null);

      const hubBlock = el('div', { class: 'hub-block' }, hubHead);

      // The correlation banner: several devices failing on one hub is one hub
      // fault, not several phone faults. Saying so out loud is the difference
      // between an operator replacing five phones and an operator replacing
      // one hub.
      // The server sets `correlated` on a hub with more than one unhealthy
      // device; the ratio test is the fallback when it does not.
      if (unhealthy >= 2 && total > 0 && ((meta2 && meta2.correlated) || unhealthy / total >= 0.4)) {
        const severe = unhealthy / total >= 0.75;
        const line = unhealthy + ' of ' + total + ' devices on hub ' + (hubPath || '(unknown)') +
          ' unhealthy' + (since ? ' since ' + fmtClock(since) : '') + ' — suspect the hub, not the phones.';
        hubBlock.append(el('div', { class: 'correlate' + (severe ? ' correlate-bad' : '') },
          el('span', { 'aria-hidden': 'true' }, '▲'),
          el('span', null, line,
            el('span', { class: 'c-sub' },
              'Blast radius is this hub, on host ' + host + '. Leases on these devices are untouched and their clocks keep running.'),
            el('span', { class: 'c-sub' },
              since ? 'Worst health_since: ' + fmtAbs(since) + ' (' + fmtRel(since) + ').' : 'The API reported no health_since for these devices.')),
          el('button', {
            class: 'mini ghost',
            onclick: () => setFilters({ host: host === '(unassigned host)' ? '' : host, hub: hubParamOf(first) })
          }, 'Focus this hub')));

        // No top-level banner is raised from here. Hub correlation is also
        // computed server-side from farm.v_hub_health and arrives on the alert
        // stream, which carries the hub id and can offer a jump action. Raising
        // it here as well produced two banners per failing hub, worded slightly
        // differently, which is precisely how a real correlated failure gets
        // lost in its own noise. The in-context box above the affected devices
        // is the better placement and stays.
      }

      const grid = el('div', { class: 'grid' });
      for (const d of devices) grid.append(deviceTile(d));
      hubBlock.append(grid);
      block.append(hubBlock);
    }

    frag.append(block);
  }

  if (!rows.length) {
    frag.append(emptyState('No devices match these filters.',
      'The fleet has ' + (all ? all.length : 0) + ' devices. Clear the filters to see them.'));
  }

  body.replaceChildren(frag);

  // Alerts inside the view are rendered content; the announcement goes to the
  // aria-live banner region once per new correlation, not on every repaint.
  alerts.replaceChildren();
}

function deviceTile(d) {
  const rack = d.rackSlot
    ? el('span', null, d.rackSlot)
    : el('span', { class: 'unslotted', title: 'this device has no rack_slot label; a human cannot be told where to walk' },
      d.usbPath ? 'usb ' + d.usbPath : 'unslotted');

  const name = [d.rackSlot || d.usbPath || shortId(d.id), d.model || 'unknown model',
    'health ' + (d.health || 'unknown'),
    d.leaseState === 'held' || d.leaseState === 'suspect'
      ? 'lease ' + d.leaseState + (d.protected ? ' protected' : '') : 'no lease'].join(', ');

  const tile = el('button', {
    type: 'button',
    class: 'tile h-' + (d.health || 'unknown'),
    'aria-label': name,
    title: (d.manufacturer ? d.manufacturer + ' ' : '') + (d.model || '') +
      (d.android ? '  Android ' + d.android : '') + (d.serial ? '  serial ' + d.serial : ''),
    onclick: () => openDevice(d)
  },
    el('span', { class: 'slot' }, rack),
    el('span', { class: 'model' },
      [d.manufacturer, d.model].filter(Boolean).join(' ') || 'unknown model',
      d.android ? ' · ' + d.android : ''),
    el('span', { class: 'chips' },
      healthChip(d.health),
      leaseChips(d),
      d.quarantineID ? el('span', { class: 'chip chip-quarantined', title: d.quarantineReason || 'open quarantine' }, el('span', { 'aria-hidden': 'true' }, '■'), 'quarantined') : null,
      d.serialAmbiguous ? el('span', { class: 'chip chip-degraded', title: 'this ADB serial is not unique in the farm; address it by devpath only' }, 'dup serial') : null,
      d.adminState && d.adminState !== 'enabled' ? el('span', { class: 'chip chip-plain' }, d.adminState) : null),
    el('span', { class: 'foot' },
      batteryEl(d.battery),
      el('span', { title: 'adb_state' }, d.adbState || 'adb ?'),
      // The fence is null for a tenant looking at another tenant's lease: the
      // API withholds it rather than omitting it, so "—" here means "not
      // yours", never "unknown".
      d.leaseState === 'held' || d.leaseState === 'suspect'
        ? el('span', { class: 'mono', title: d.fence === undefined ? 'lease fence: withheld, another tenant holds this device' : 'lease fence' },
          d.fence === undefined ? '—' : 'f' + d.fence)
        : null));
  return tile;
}

function refreshFilterOptions(rows) {
  /* The "all …" option is rebuilt here rather than left to the data-i18n in
   * index.html, because this function REPLACES the select's options with what
   * the API returned — which throws away the marked-up placeholder along with
   * them. Without this the three filters were the only English left on a
   * Portuguese page, and only after the first fleet load, which is exactly the
   * kind of half-translation that looks like a bug in the language switch. */
  fillSelect($('#f-host'), t('filter.allHosts'),
    unique(rows.map((d) => d.host).filter(Boolean)).concat((state.data.hosts || []).map((h) => h.id).filter(Boolean)),
    state.filters.host);
  fillSelect($('#f-pool'), t('filter.allPools'), unique(rows.map((d) => d.pool).filter(Boolean)), state.filters.pool);

  const hubs = [];
  const seen = new Set();
  for (const d of rows) {
    const k = hubParamOf(d);
    if (!k || seen.has(k)) continue;
    seen.add(k);
    hubs.push({ value: k, label: (d.host ? d.host + ' · ' : '') + 'hub ' + (d.hubPath || k) });
  }
  hubs.sort((a, b) => cmp(a.label, b.label));
  fillSelect($('#f-hub'), t('filter.allHubs'), hubs, state.filters.hub);
}

function unique(list) {
  return Array.from(new Set(list.map(String))).sort(cmp);
}

function fillSelect(sel, allLabel, items, current) {
  const opts = items.map((i) => (typeof i === 'object' ? i : { value: String(i), label: String(i) }));
  const sig = allLabel + '|' + opts.map((o) => o.value).join(',');
  if (sel.dataset.sig === sig) { if (sel.value !== (current || '')) sel.value = current || ''; return; }
  sel.dataset.sig = sig;
  sel.replaceChildren(el('option', { value: '' }, allLabel),
    ...opts.map((o) => el('option', { value: o.value }, o.label)));
  sel.value = current || '';
}
