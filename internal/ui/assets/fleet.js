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
      /* "live" is every device somebody is on: held OR suspect. It exists
       * because "how much of the farm is busy right now" was the one question
       * the lease filter could not answer — `held` alone silently drops the
       * suspect leases, and a suspect lease is NOT a released one. The summary
       * strip's "In use" card counts leased = held + suspect, so it needs a
       * filter that means the same thing, or the card would send an operator
       * to a grid holding fewer devices than the number they clicked. */
      if (f.lease === 'live' && !live) return false;
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

/* ------------------------------------------------------------------ *
 * THE SUMMARY STRIP
 *
 * Five numbers above the grid that answer "is the farm OK?" before a single
 * device is read. Before it, the landing page of a control plane was an
 * inventory: fifty-six cards and a hundred and seventy-eight coloured chips,
 * with no line anywhere on it that said whether anything was wrong.
 *
 * # Two kinds of number, kept apart on purpose
 *
 * `state.data.counts` is what the SERVER counted, over the slice it was asked
 * for — the host, hub, health, pool and q filters go to the API, so the counts
 * narrow when those do. `fleetRows()` is what is ON THE SCREEN, which is the
 * server's rows minus the one filter this page applies by itself (lease).
 *
 * They are different numbers and the strip never mixes them: the five cards
 * are the server's, the note under them is the screen's. Conflating the two is
 * how an operator ends up acting on data that is not in front of them.
 *
 * # Clicking a card
 *
 * Each card touches ONLY its own axis — health for two of them, lease for two,
 * both for Total — so the number you clicked is the number you land on. A card
 * that is already applied clears itself, which is what makes the strip a
 * drill-down you can walk back out of rather than a one-way trip through
 * filters. setFilters() is the one path that applies them; the host and hub
 * links use it too, and a second one would fire a second /fleet request whose
 * response could land in either order.
 * ------------------------------------------------------------------ */

/* fleetServerCounts normalises the server's counts object, and computes the
 * same five numbers from the rows when a server has not sent one. The fallback
 * is not decoration: an older API, or any response that omits `counts`, would
 * otherwise render a strip of zeroes over a full grid — a page asserting the
 * farm is empty while showing fifty-six devices. */
function fleetServerCounts(all) {
  const c = state.data.counts;
  const n = (v) => (typeof v === 'number' && isFinite(v) ? v : 0);
  /* Every field is checked, not just the object. A `counts` that is present
   * but missing `leased` would otherwise take this branch and coerce to zero —
   * "In use 0, Free 0, Needs attention 0" printed over a full grid, which is
   * the exact failure this fallback exists to prevent, arriving through the
   * door the fallback left open. `health.offline` is NOT required: the server
   * builds that map from the rows it saw, so an absent key means none, and
   * demanding it would send a healthy farm down the slow path. */
  const has = (k) => c && typeof c[k] === 'number' && isFinite(c[k]);
  if (c && typeof c === 'object' && has('total') && has('unhealthy') && has('leased') && has('free')) {
    const health = (c.health && typeof c.health === 'object') ? c.health : {};
    return {
      total: n(c.total), unhealthy: n(c.unhealthy), leased: n(c.leased),
      free: n(c.free), offline: n(health.offline)
    };
  }
  const out = { total: 0, unhealthy: 0, leased: 0, free: 0, offline: 0 };
  for (const d of all || []) {
    out.total++;
    const h = d.health || 'unknown';
    if (isFault(h)) out.unhealthy++;
    if (h === 'offline') out.offline++;
    if (d.leaseState === 'held' || d.leaseState === 'suspect') out.leased++;
    else out.free++;
  }
  return out;
}

/* fleetCardSpecs is built per render rather than declared once, because every
 * label in it goes through t() and the language can change between two paints.
 * The keys are written out literally so the Go test that walks this file for
 * t('…') can see them; a computed key is invisible to it and renders as itself
 * on screen forever.
 *
 * `zeroIsGood` is what turns an inventory into an instrument. A card whose
 * count is zero because nothing is wrong says so with a tick and goes quiet,
 * instead of holding a red 0 that an eye still has to stop on. */
function fleetCardSpecs() {
  const f = state.filters;
  return [
    {
      key: 'attention', tone: 'cri', glyph: '▲', zeroIsGood: true,
      value: (c) => c.unhealthy,
      label: t('fleet.sum.attention'), sub: t('fleet.sum.attentionSub'), help: t('fleet.sum.attentionHelp'),
      active: f.health === 'unhealthy',
      apply: { health: 'unhealthy' }, clear: { health: '' }
    },
    {
      key: 'inuse', tone: 'inf', glyph: '●',
      value: (c) => c.leased,
      label: t('fleet.sum.inUse'), sub: t('fleet.sum.inUseSub'), help: t('fleet.sum.inUseHelp'),
      active: f.lease === 'live',
      apply: { lease: 'live' }, clear: { lease: '' }
    },
    {
      key: 'free', tone: 'pos', glyph: '○',
      value: (c) => c.free,
      label: t('fleet.sum.free'), sub: t('fleet.sum.freeSub'), help: t('fleet.sum.freeHelp'),
      active: f.lease === 'free',
      apply: { lease: 'free' }, clear: { lease: '' }
    },
    {
      key: 'offline', tone: 'cri', glyph: '✕', zeroIsGood: true,
      value: (c) => c.offline,
      label: t('fleet.sum.offline'), sub: t('fleet.sum.offlineSub'), help: t('fleet.sum.offlineHelp'),
      active: f.health === 'offline',
      apply: { health: 'offline' }, clear: { health: '' }
    },
    {
      key: 'total', tone: 'quiet', glyph: null,
      value: (c) => c.total,
      label: t('fleet.sum.total'), sub: t('fleet.sum.totalSub'), help: t('fleet.sum.totalHelp'),
      active: !f.health && !f.lease,
      apply: { health: '', lease: '' }, clear: { health: '', lease: '' }
    }
  ];
}

/* The five buttons, kept across repaints.
 *
 * The Fleet redraws roughly every two seconds under the event stream. If this
 * strip were rebuilt each time, every one of those redraws would detach a
 * focused card — an operator tabbing to "Needs attention" would be thrown back
 * to the top of the document before they could press it, twice a minute, with
 * nothing on screen to explain why. So the nodes are made once and their text
 * is written over; only the numbers move.
 *
 * The click handler is attached once and looks its spec up FRESH, rather than
 * closing over the one that built it. A closure would be reading a filter state
 * from whenever the button happened to be created, which is the same class of
 * bug as a stale element reference and just as quiet. */
const fleetCardNodes = new Map();

function fleetCardNode(key) {
  let node = fleetCardNodes.get(key);
  if (node) return node;
  node = el('button', { type: 'button', class: 'sum-card' },
    el('span', { class: 'sum-n' },
      el('span', { class: 'sum-g', 'aria-hidden': 'true' }),
      el('b', null, '')),
    el('span', { class: 'sum-label' }),
    el('span', { class: 'sum-sub' }));
  node.addEventListener('click', () => {
    const spec = fleetCardSpecs().find((s) => s.key === key);
    if (spec) setFilters(spec.active ? spec.clear : spec.apply);
  });
  fleetCardNodes.set(key, node);
  return node;
}

function renderFleetSummary(all, rows) {
  const strip = $('#fleet-summary');
  if (!strip) return;

  // Nothing has been loaded, or the load failed and left us with nothing: a
  // strip of zeroes would be an assertion about a farm we have not read.
  if (!Array.isArray(all)) { strip.hidden = true; return; }
  strip.hidden = false;

  let cards = $('.sum-cards', strip);
  let note = $('.sum-note', strip);
  if (!cards || !note) {
    cards = el('div', { class: 'sum-cards' });
    note = el('p', { class: 'sum-note' });
    strip.replaceChildren(cards, note);
  }

  const c = fleetServerCounts(all);
  for (const spec of fleetCardSpecs()) {
    const node = fleetCardNode(spec.key);
    if (node.parentNode !== cards) cards.append(node);
    paintFleetCard(node, spec, c);
  }

  /* The note is the one place on this page where the two kinds of number are
   * told apart in words, so it says three separate things and never merges
   * them into one hedge:
   *
   *  - `served`: a filter the SERVER applied. The counts above narrowed with
   *    it, so they are about a slice and must not be read as the farm.
   *  - `screened`: the page filtered further by itself. `lease` never reaches
   *    the API, so the cards stay whole-farm while the grid does not — and
   *    calling that "a filter is on: these numbers count that slice" would be
   *    a false confession, telling an operator the five numbers had narrowed
   *    when they had not.
   *  - `capped`: the server stopped at its limit. Then the counts describe the
   *    rows that came back and nothing else — an all-clear computed over the
   *    first thousand of twelve hundred devices is the worst sentence this
   *    page could print. truncChip says the same thing in the toolbar; it did
   *    sit beside these numbers before they moved up here. */
  const f = state.filters;
  const served = !!(f.host || f.hub || f.health || f.pool || state.q.trim());
  const screened = rows.length !== c.total;
  const capped = !!state.truncated.fleet;

  const words = [];
  if (!served && !screened && !capped && c.unhealthy === 0) words.push(t('fleet.sum.allClear'));
  if (capped) words.push(t('fleet.sum.truncated'));
  else words.push(served ? t('fleet.sum.filtered', { served: String(c.total) }) : t('fleet.sum.wholeFarm'));
  if (screened) words.push(t('fleet.sum.onScreen', { shown: String(rows.length) }));

  note.className = 'sum-note' + (capped ? ' capped' : '');
  note.replaceChildren();
  // A glyph as well as the colour, for the same reason every chip on this page
  // carries one: the sentence has to survive a greyscale screenshot.
  append(note, [capped ? el('span', { 'aria-hidden': 'true' }, '▲ ') : null, words.join(' ')]);
}

function paintFleetCard(node, spec, c) {
  const n = spec.value(c);
  // A zero that is good news says so and goes quiet. A red 0 is still
  // something an eye has to stop on and decide about.
  const quiet = spec.zeroIsGood && n === 0;
  node.className = 'sum-card tone-' + (quiet ? 'quiet' : spec.tone);
  node.setAttribute('aria-pressed', spec.active ? 'true' : 'false');
  node.title = spec.help;
  $('.sum-g', node).textContent = quiet ? '✓' : (spec.glyph || '');
  $('.sum-n b', node).textContent = String(n);
  $('.sum-label', node).textContent = spec.label;
  $('.sum-sub', node).textContent = spec.sub;
}

function renderFleet() {
  const body = $('#fleet-body');
  const alerts = $('#fleet-alerts');
  body.setAttribute('aria-busy', state.loading.fleet ? 'true' : 'false');

  const all = state.data.fleet;
  const rows = fleetRows();

  // The five server-computed numbers now live in the summary strip above the
  // grid, where they are large enough to read from a doorway and each one can
  // be clicked. countChips() rendered the same six values here as six grey
  // pills in the corner of a toolbar; keeping both would put the same arithmetic
  // on the page twice, differently worded, which is how two numbers that must
  // agree start disagreeing.
  const counts = $('#fleet-counts');
  counts.replaceChildren();
  renderFleetSummary(all, rows);
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

/* ------------------------------------------------------------------ *
 * THE DEVICE CARD
 *
 * # The chip budget: two per device, hard
 *
 * A device states its worst state ONCE. One condition chip, one availability
 * chip, and everything else collapses into a single ⚠ that opens the sheet.
 *
 * The rule exists because the grid used to spend up to five chips on one
 * device and repeat itself doing it: a quarantined handset carried a health
 * chip reading "quarantined", a quarantine chip reading "quarantined" and an
 * admin_state chip reading "quarantined", three times in one card, in three
 * colours. Fifty-six devices came to a hundred and seventy-eight chips, and
 * the count GREW with the incident — the worse the farm got, the more
 * identical words there were to read. Two per device is a constant, so a bad
 * day now looks different from a good one instead of merely looking louder.
 *
 * Nothing is deleted. Everything demoted is on the ⚠, in its aria-label and in
 * the device sheet; the fence string and the raw adb_state moved to the sheet
 * outright, because neither is a thing you scan a wall of cards for.
 * ------------------------------------------------------------------ */

/* CONDITION_RANK is the precedence a device's ONE condition chip is chosen by,
 * lowest first.
 *
 * Only one comparison in this file consults it, and only one CAN: health is a
 * single column, so the sole contest is between an open quarantine record and
 * whatever health says. Quarantine sits at 0 and therefore always wins today —
 * which is the intended answer, because quarantine is the state that stops the
 * scheduler, and it is an answer this table can be edited to change rather than
 * a constant buried in an `if`.
 *
 * The rest of the order is here to be read, not executed: it is the sequence a
 * reviewer needs when deciding where a NEW state belongs, and where a future
 * second source of condition would slot in. parked and retired sit below the
 * faults and above healthy because they are decisions somebody made rather
 * than breakage — the distinction NOT_A_FAULT draws in app.js and style.css
 * records for the grey chip. */
const CONDITION_RANK = {
  quarantined: 0, offline: 1, missing: 1, unauthorized: 2, degraded: 3,
  unknown: 4, recovering: 5, booting: 5, parked: 6, retired: 6, healthy: 7
};

function deviceCondition(d) {
  const h = d.health || 'unknown';
  if (!d.quarantineID) return h;
  const hr = CONDITION_RANK[h];
  return (hr === undefined || CONDITION_RANK.quarantined <= hr) ? 'quarantined' : h;
}

/* conditionHelp is the plain-English sentence behind a one-word value. The
 * value itself is never translated — `degraded` is what the health column
 * says and what an operator greps a log for — so the explanation goes NEXT to
 * it, in the tooltip and in the device sheet, and the word stays put.
 *
 * A switch of literal keys rather than a lookup table: t() calls with a
 * computed key are invisible to TestEveryKeyTheAppAsksForExists, and a key it
 * cannot see is one that renders as itself, in every language, forever. */
function conditionHelp(c) {
  switch (c) {
    case 'healthy': return t('fleet.cond.healthy');
    case 'degraded': return t('fleet.cond.degraded');
    case 'offline': return t('fleet.cond.offline');
    case 'missing': return t('fleet.cond.missing');
    case 'unauthorized': return t('fleet.cond.unauthorized');
    case 'recovering': return t('fleet.cond.recovering');
    case 'booting': return t('fleet.cond.booting');
    case 'quarantined': return t('fleet.cond.quarantined');
    case 'parked': return t('fleet.cond.parked');
    case 'retired': return t('fleet.cond.retired');
    default: return t('fleet.cond.unknown');
  }
}

function conditionChip(cond) {
  return el('span', { class: 'chip chip-' + cond, title: conditionHelp(cond) },
    el('span', { 'aria-hidden': 'true' }, HEALTH_GLYPH[cond] || '?'), cond);
}

/* availabilityOf collapses the lease into the one thing an operator wants off
 * a wall of cards: can I take this device, and if not, who has it.
 *
 * `protected` and `suspect` are both properties OF a live lease rather than
 * alternatives to it, so they rank above plain `held` here — a lease nothing
 * but a human can end, and a lease whose holder stopped answering, are the two
 * that change what you do next. */
function availabilityOf(d) {
  const st = d.leaseState;
  if (st !== 'held' && st !== 'suspect') return 'free';
  if (st === 'suspect') return 'suspect';
  return d.protected ? 'protected' : 'held';
}

const AVAIL_GLYPH = { free: '○', held: '●', suspect: '◐', protected: '★' };

/* Every one of these four reads differently in greyscale: a different glyph and
 * a different word, never a different colour alone. "In use · suspect" keeps
 * the raw lease-state value on the face precisely because it is the state an
 * operator must not mistake for a healthy one — the device is NOT released and
 * the job may be running fine. */
function availabilityWord(a) {
  switch (a) {
    case 'free': return t('fleet.avail.free');
    case 'suspect': return t('fleet.avail.suspect');
    case 'protected': return t('fleet.avail.protected');
    default: return t('fleet.avail.inUse');
  }
}

function availabilityHelp(d, a) {
  switch (a) {
    case 'free': return t('fleet.avail.freeHelp');
    // A suspect lease that is also protected is the combination the leases
    // view counts on its own; the tooltip says both, because the chip cannot.
    case 'suspect': return d.protected
      ? t('fleet.avail.suspectHelp') + ' ' + t('fleet.avail.protectedHelp')
      : t('fleet.avail.suspectHelp');
    case 'protected': return t('fleet.avail.protectedHelp');
    default: return t('fleet.avail.heldHelp');
  }
}

function availabilityChip(d, a) {
  return el('span', { class: 'chip chip-' + a, title: availabilityHelp(d, a) },
    el('span', { 'aria-hidden': 'true' }, AVAIL_GLYPH[a]), availabilityWord(a));
}

/* The one short sentence under the chips. It carries the holder, which is the
 * half of "in use" that tells you who to go and ask, and it deliberately does
 * not repeat the chip's own word.
 *
 * It is also where a protected lease that has gone suspect is stated. The chip
 * can only hold one of the two and suspect is the one that changes with the
 * clock, so protection — the fact that decides whether this device comes back
 * on its own or waits for a human — is written out here in full rather than
 * left to the colour of a chip it lost. A suspect plain lease and a suspect
 * protected lease have opposite next actions and must never look alike. */
function availabilityLine(d, a) {
  const who = nz(d.holder) === null ? t('fleet.avail.someoneElse') : String(d.holder);
  switch (a) {
    case 'free': return t('fleet.avail.freeLine');
    case 'suspect': return d.protected
      ? t('fleet.avail.suspectProtectedLine', { holder: who })
      : t('fleet.avail.suspectLine', { holder: who });
    case 'protected': return t('fleet.avail.protectedLine', { holder: who });
    default: return t('fleet.avail.heldLine', { holder: who });
  }
}

/* tileFlags is everything true of this device that did not earn one of the two
 * chips. It is demotion, not deletion: the list is the ⚠'s accessible name, its
 * tooltip, and part of the tile's own aria-label, and the sheet has all of it
 * in full. */
function tileFlags(d, cond) {
  const out = [];
  const health = d.health || 'unknown';
  if (d.quarantineID && nz(d.quarantineReason) !== null) {
    out.push(t('fleet.flag.quarantine', { reason: String(d.quarantineReason) }));
  }
  // The condition chip is showing something other than what health says —
  // quarantine outranked it. Name the value it covered up.
  if (cond !== health) out.push(t('fleet.flag.health', { health: health }));
  if (d.serialAmbiguous) out.push(t('fleet.flag.dupSerial'));
  if (nz(d.adminState) !== null && d.adminState !== 'enabled') {
    out.push(t('fleet.flag.adminState', { state: String(d.adminState) }));
  }
  return out;
}

/* A span and not a button: the tile IS a button, and a button inside a button
 * is invalid HTML that browsers resolve by dropping one of them. role="img"
 * with an aria-label is how a glyph gets a name without becoming a second
 * control — and because the tile carries its own aria-label, which suppresses
 * everything inside it for a screen reader, the same text is folded into the
 * tile's label too. */
function tileFlag(flags) {
  if (!flags.length) return null;
  const label = t('fleet.flag.label') + ' ' + flags.join('; ') + '. ' + t('fleet.flag.open');
  return el('span', { class: 'tile-flag', role: 'img', 'aria-label': label, title: label }, '⚠');
}

function deviceTile(d) {
  const cond = deviceCondition(d);
  const avail = availabilityOf(d);
  const flags = tileFlags(d, cond);

  const modelText = [d.manufacturer, d.model].filter(Boolean).join(' ') || t('fleet.tile.unknownModel');
  const slotText = nz(d.rackSlot) === null
    ? (nz(d.usbPath) === null ? shortId(d.id) : 'usb ' + d.usbPath)
    : String(d.rackSlot);

  /* The aria-label is the oldest human sentence on this page and it stays. It
   * used to be the ONLY one — the card beside it was chips and monospace — and
   * now the card says nearly the same thing, so the two are written from the
   * same pieces and cannot drift apart.
   *
   * Battery is in here and only in here for a screen reader: an element with an
   * aria-label suppresses everything inside it, so the meter's own title is
   * unreachable from the tile. */
  const pct = (d.battery === null || d.battery === undefined) ? null : Math.round(Number(d.battery));
  const name = [
    slotText,
    modelText,
    d.android ? 'Android ' + d.android : null,
    'health ' + cond,
    avail === 'free' ? 'no lease' : 'lease ' + d.leaseState + (d.protected ? ' protected' : ''),
    pct === null ? 'battery not reported' : 'battery ' + pct + '%'
  ].concat(flags).filter(Boolean).join(', ');

  return el('button', {
    type: 'button',
    class: 'tile h-' + cond,
    'aria-label': name,
    onclick: () => openDevice(d)
  },
    el('span', { class: 'slot' }, nz(d.rackSlot) === null
      ? el('span', { class: 'unslotted', title: t('fleet.tile.unslottedHelp') }, slotText)
      : slotText),
    el('span', {
      class: 'model',
      title: modelText + (d.android ? ' · Android ' + d.android : '') + (d.serial ? ' · ' + d.serial : '')
    }, modelText, d.android ? ' · Android ' + d.android : ''),
    el('span', { class: 'chips' }, conditionChip(cond), availabilityChip(d, avail), tileFlag(flags)),
    el('span', { class: 'avail' }, availabilityLine(d, avail)),
    /* The meter stays exactly as it was: batteryEl sets element.style.width
     * through the CSSOM, which the Content-Security-Policy permits and an
     * inline style attribute would not. It is labelled now, because a bare
     * "44%" beside a bar is a number whose unit the reader has to guess.
     *
     * When nothing has been reported there is nothing to meter, so the meter
     * is not drawn at all — batteryEl's own "batt —" under a "Battery" label
     * would read "Battery batt —", which is the abbreviation and the word for
     * the same thing, twice. */
    el('span', { class: 'foot' },
      el('span', { class: 'foot-k' }, t('fleet.tile.battery')),
      pct === null
        ? el('span', { class: 'batt', title: t('fleet.tile.noBattery') }, '—')
        : batteryEl(d.battery)));
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
