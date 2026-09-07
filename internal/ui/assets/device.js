/* The device sheet: everything an operator sees after clicking one device.
 *
 * Identity, physical position, health, lease, the operator actions, the ad-hoc
 * command box and the live screen. Extracted from app.js for the same reason
 * as fleet.js, and sharing the same scope on the same terms — read the note at
 * the top of fleet.js, which is the one place that argument is written down.
 *
 * This file owns screenSession, the one open screen panel per tab. That state
 * is a top-level let, which means it lives in the global lexical scope shared
 * by every classic script on the page. app.js may call into it freely; what
 * app.js must not do is read it at its own top level.
 *
 * # What this screen is for
 *
 * A person who clicks a device is asking two questions: WHAT IS THIS PHONE and
 * CAN I USE IT. The sheet answers both in its first two lines — a rack slot, a
 * model, and one sentence of plain English — and everything else is arranged
 * behind it.
 *
 * That is a reversal. What used to be here opened with farm_uid, a device
 * uuid, an adb serial and a JSON dump of labels; health was two screens down,
 * and eighteen of its thirty-one rows were raw column names with nothing
 * attached to explain them. Measured on the demo farm, #device-body was 2020
 * CSS pixels against a 658-pixel viewport — three screens for a device that is
 * healthy and free, more for one that is neither. The Overview panel is now
 * 729 against 729: one screen, no scroll at all.
 *
 * # Two rules that look like they contradict each other, and do not
 *
 * NOTHING IS RENAMED. `fence`, `admin_state`, `adb_devpath`, `next_ladder_tier`
 * are farm.leases.fence, farm.devices.admin_state, the ADB devpath and the
 * lowest unspent rung. A screen that says something else cannot be matched
 * against the database, against `ctl` output or against a log line — and being
 * matchable is the whole reason this product writes the words down.
 *
 * NOTHING IS LEFT UNEXPLAINED. So kv() prints the human label as the label and
 * the column name UNDERNEATH it, at --fs-xs in --txt-3. The operator reads
 * "Next recovery rung" and greps `next_ladder_tier`, and neither of them had
 * to give anything up.
 *
 * # Why tabs rather than one long scroll
 *
 * The Screen panel and the Command panel are each larger than a screen on
 * their own, and both are answers to questions nobody has yet when they open a
 * device. Behind a tab they cost nothing until they are asked for, and no
 * panel here exceeds one screen of scroll.
 */

/* ------------------------------------------------------------------ *
 * The sheet
 * ------------------------------------------------------------------ */

/* The six panels, in the order the questions arrive. Overview first because it
 * is the answer; Raw last because it is the appeal. */
const SHEET_TABS = ['overview', 'health', 'lease', 'screen', 'command', 'raw'];

/* sheetTabLabel is a switch rather than a table lookup on purpose: written this
 * way every key is a literal t('…') call, which is what
 * TestEveryKeyTheAppAsksForExists can see. A computed key is invisible to that
 * test and renders as the raw key on screen in every language when it is
 * misspelled. */
function sheetTabLabel(id) {
  switch (id) {
    case 'overview': return t('device.tab.overview');
    case 'health': return t('device.tab.health');
    case 'lease': return t('device.tab.lease');
    case 'screen': return t('device.tab.screen');
    case 'command': return t('device.tab.command');
    default: return t('device.tab.raw');
  }
}

/* The panel showing now. Reset to overview on every open: the first thing a
 * device says should not depend on what the last device was asked. */
let sheetTab = 'overview';

/* The device the sheet is describing. Kept so the sheet can be rebuilt without
 * a click — for a language switch, and for the moment after a footer action
 * changes the very thing the sheet is describing. */
let sheetDevice = null;

async function openDevice(d) {
  const dlg = $('#dlg-device');
  sheetTab = 'overview';
  sheetDevice = d;
  setSheetHead(d);
  $('#device-tabs').replaceChildren();
  const foot = $('#device-foot');
  foot.replaceChildren();
  foot.hidden = true;
  const body = $('#device-body');
  body.replaceChildren(emptyState(t('device.loading'),
    t('device.loadingDetail', { path: 'GET /api/v1/devices/' + shortId(d.id) })));
  if (!dlg.open) dlg.showModal();

  let full = d;
  try {
    const resp = await api.get('devices/' + encodeURIComponent(d.id));
    full = normDevice(pick(resp, 'device') || resp);
    if (!full.id) full.id = d.id;
  } catch (e) {
    banner('warn', t('device.fetchFailed', { err: errText(e) }), { key: 'devfetch' });
  }
  renderDeviceBody(full);
}

/* setSheetHead writes the two lines that answer "what is this phone".
 *
 * The title is the RACK SLOT, in sans, at --fs-xl. It used to be a monospace
 * run of the slot and the model glued together with two spaces, which read as
 * a log line rather than as the name of a thing. The slot is what an operator
 * says out loud, writes on a ticket and walks to; the uuid is not, and it has
 * moved to the Identifiers block at the bottom of Overview. */
function setSheetHead(d) {
  $('#device-title').textContent = d.rackSlot || d.usbPath || shortId(d.id) || t('state.none');

  const parts = [];
  const model = [d.manufacturer, d.model].filter(Boolean).join(' ');
  if (model) parts.push(model);
  /* "Android" is a product name and the release is server data; neither is
   * translated, here or anywhere else on this page. */
  if (d.android) parts.push('Android ' + d.android);
  const sub = $('#device-subtitle');
  sub.textContent = parts.length ? parts.join(' · ') : t('device.unnamedModel');
}

function renderDeviceBody(d) {
  sheetDevice = d;
  setSheetHead(d);

  const panels = {
    overview: overviewPanel(d),
    health: healthPanel(d),
    lease: leasePanel(d),
    screen: el('div', { class: 'sheet-sec' },
      el('p', { class: 'dev-note' }, t('device.screenNote')),
      screenPanel(d)),
    command: el('div', { class: 'sheet-sec' }, execPanel(d)),
    raw: el('div', { class: 'sheet-sec' },
      el('p', { class: 'dev-note' }, t('device.rawNote')),
      el('pre', { class: 'out' }, JSON.stringify(d.raw, null, 2)))
  };

  $('#device-body').replaceChildren(...SHEET_TABS.map((id) => el('section', {
    class: 'sheet-panel', id: 'device-panel-' + id,
    role: 'tabpanel', 'aria-labelledby': 'device-tab-' + id,
    tabindex: '-1', hidden: true
  }, panels[id])));

  $('#device-tabs').replaceChildren(...SHEET_TABS.map((id) => el('button', {
    type: 'button', class: 'sheet-tab', role: 'tab',
    id: 'device-tab-' + id,
    'aria-controls': 'device-panel-' + id,
    'aria-selected': 'false',
    onclick: () => showSheetTab(id)
  }, sheetTabLabel(id))));

  showSheetTab(SHEET_TABS.includes(sheetTab) ? sheetTab : 'overview');

  /* The actions live in a footer that does not scroll away. An operator who has
   * read three panels to decide that a slot needs power-cycling should not have
   * to scroll back past them to say so. */
  const foot = $('#device-foot');
  const acts = sheetActions(d);
  foot.replaceChildren(...acts);
  foot.hidden = acts.length === 0;
}

/* showSheetTab shows exactly one panel.
 *
 * THE [hidden] TRAP. These panels are shown and hidden with the hidden
 * attribute, which is a user-agent rule that ANY author `display` beats
 * regardless of specificity. drawer.css therefore ships
 * `#dlg-device [hidden] { display: none !important }` in the same commit as the
 * `display:flex` on .sheet-panel. Without it all six panels paint at once —
 * which has shipped as a real bug twice in this codebase, once in this very
 * screen panel. */
function showSheetTab(id) {
  sheetTab = id;
  for (const t2 of SHEET_TABS) {
    const btn = $('#device-tab-' + t2);
    const panel = $('#device-panel-' + t2);
    const on = t2 === id;
    if (btn) btn.setAttribute('aria-selected', on ? 'true' : 'false');
    if (panel) panel.hidden = !on;
  }
  const body = $('#device-body');
  if (body) body.scrollTop = 0;
}

/* refreshSheet re-reads the open device and redraws the sheet.
 *
 * A LIVE SCREEN IS THE ONE THING IT WILL NOT DO THIS TO. Rebuilding the panels
 * replaces the canvas the decoder is painting into, and the session would keep
 * running against a node nobody can see and no button can stop — the handset
 * encoding for nobody that closeScreenOnDrawerClose exists to prevent. An
 * operator watching a picture is not the operator who just revoked a lease, so
 * the redraw is simply skipped while a session is open. */
async function refreshSheet() {
  const dlg = $('#dlg-device');
  if (!dlg || !dlg.open || !sheetDevice || !sheetDevice.id) return;
  if (screenSession) return;
  const id = sheetDevice.id;
  try {
    const resp = await api.get('devices/' + encodeURIComponent(id));
    const full = normDevice(pick(resp, 'device') || resp);
    if (!full.id) full.id = id;
    /* The sheet may have been closed, or moved to another device, while the
     * request was in flight. */
    if ($('#dlg-device').open && sheetDevice && sheetDevice.id === id) renderDeviceBody(full);
  } catch (_) {
    /* The sheet keeps the row it has. A failed re-read does not deserve a
     * banner of its own: the action that prompted it has already raised one. */
  }
}

/* A language switch rebuilds the sheet from the device it was opened with.
 *
 * applyTranslations has already flipped the two pieces of this sheet that live
 * in index.html — the close button and the tab strip's accessible name. Every
 * other string here is built in JS, so without this the sheet reads "Fechar"
 * beside "Overview / Health / Lease", which is worse than either language on
 * its own. The live-screen exception is refreshSheet's, for its reason. */
window.addEventListener('languagechange', () => {
  const dlg = $('#dlg-device');
  if (!dlg || !dlg.open || !sheetDevice || screenSession) return;
  try { renderDeviceBody(sheetDevice); } catch (_) { /* a redraw failure must not strand the switch */ }
});

/* ------------------------------------------------------------------ *
 * kv — a label a person reads, over the column name they will grep
 * ------------------------------------------------------------------ */

/* kv(label, value, {raw, term}).
 *
 * `label` is the human sentence fragment: "Failure score", not `failure score`.
 * `raw` is the database column, the API field or the ADB concept the label
 * stands for, printed underneath in mono at --fs-xs. Both, always, for every
 * row whose value an operator might have to find somewhere else.
 * `term` is the glossary hook — window.term(id) from terms.js, unit 10's file.
 * It is called through a guard because terms.js may not define it yet, and a
 * device sheet that throws is worse than one without a dotted underline. */
function kv(label, value, opts) {
  const o = opts || {};
  const dt = el('dt', null,
    glossed(o.term, label),
    o.raw ? el('span', { class: 'kv-raw' }, o.raw) : null);
  const blank = value === undefined || value === null || value === '';
  return [dt, el('dd', null, blank ? t('state.none') : value)];
}

/* glossed asks the glossary to mark a word, and settles for the plain word.
 *
 * There is exactly one glossary in this product and it is unit 10's. This is a
 * call into it, never a second implementation of it. */
function glossed(id, label) {
  if (!id) return label;
  try {
    if (typeof term === 'function') {
      const marked = term(id, label);
      if (marked instanceof Node) return marked;
      if (typeof marked === 'string' && marked) return marked;
    }
  } catch (_) { /* the glossary is an enhancement; the word survives without it */ }
  return label;
}

/* whenCell prints the distance an operator reads and the instant they grep.
 *
 * "38m ago" is the number that answers the question. The ISO instant beneath it
 * is the one that matches farm.devices.health_since in a psql session, and a
 * screen that showed only the first would be unmatchable against the row it
 * came from. */
function whenCell(v) {
  if (!parseTime(v)) return null;
  return el('span', { class: 'when' },
    timeCell(v),
    el('span', { class: 'when-abs' }, fmtAbs(v)));
}

/* ------------------------------------------------------------------ *
 * Overview — a name, a sentence, and where the thing is
 * ------------------------------------------------------------------ */

function overviewPanel(d) {
  const notes = [];
  if (d.leaseState === 'suspect') notes.push(t('device.say.suspectNote'));
  if (isHeld(d) && d.protected) notes.push(t('device.say.protectedNote'));

  return el('div', { class: 'sheet-sec' },
    el('p', { class: 'dev-say' }, deviceSentence(d)),
    el('div', { class: 'chips dev-chips' }, healthChip(d.health), leaseChips(d)),
    notes.length ? el('p', { class: 'dev-say-note' }, notes.join(' ')) : null,

    el('h3', { class: 'sheet-h' }, t('device.where')),
    el('p', { class: 'dev-where' }, whereSentence(d)),

    el('h3', { class: 'sheet-h' }, t('device.availability')),
    el('dl', { class: 'kv' },
      kv(t('device.f.pool'), d.pool, { raw: 'pool_id', term: 'pool' }),
      kv(t('device.f.adminState'), d.adminState, { raw: 'admin_state', term: 'admin_state' }),
      kv(t('device.f.android'), androidText(d), { raw: 'android_release / sdk_int' }),
      kv(t('device.f.battery'), batteryEl(d.battery), { raw: 'battery_pct' }),
      kv(t('device.f.lastSeen'), whenCell(d.lastSeen), { raw: 'last_seen_at' })),

    identifiersBlock(d));
}

function isHeld(d) { return d.leaseState === 'held' || d.leaseState === 'suspect'; }

/* deviceSentence is the most valuable string on this screen.
 *
 * It is one sentence of plain English that answers "can I use it", built from
 * the two facts that decide the answer and NEVER from one of them: health and
 * a lease are independent, and a page that conflated them would be wrong about
 * half the fleet. An offline device can still be held; a healthy one can be
 * idle. The health value and the holder are printed verbatim — they are a
 * column value and server data, and neither is translated. */
function deviceSentence(d) {
  if (!isHeld(d)) {
    const h = d.health || 'unknown';
    if (h === 'healthy') return t('device.say.freeHealthy');
    /* retired and parked are decisions, not faults. A charge-limited shelf must
     * not read as an incident here any more than it does on the grid. */
    if (NOT_A_FAULT.has(h)) return t('device.say.freeOffService', { health: h });
    return t('device.say.freeFault', { health: h });
  }

  /* A job id is NOT a holder and is never printed as one. farm.leases.holder
   * is who is using the device; job_id is what it is being used for, and a
   * sentence that put a truncated uuid where a name belongs would be inventing
   * a fact in the one string this screen exists to get right. */
  const who = nz(d.holder);
  const job = nz(d.jobID) ? shortId(d.jobID) : null;
  const since = sheetClock(d.acquiredAt);
  let head;
  if (who && since) head = t('device.say.heldBySince', { holder: who, since });
  else if (who) head = t('device.say.heldBy', { holder: who });
  else if (job && since) head = t('device.say.heldByJobSince', { job, since });
  else if (job) head = t('device.say.heldByJob', { job });
  else if (since) head = t('device.say.heldSince', { since });
  else head = t('device.say.held');

  /* fmtSecs, not fmtRel. fmtRel writes its own English affixes — "in 10m",
   * "6s ago" — which is right for a table cell and wrong inside a sentence:
   * the Portuguese read "Expira in 10m". A bare duration lets each language
   * supply its own preposition. */
  const at = parseTime(d.expiresAt);
  let tail;
  if (!at) tail = t('device.say.noExpiry');
  else {
    /* Rounded to the minute past a minute. "Expires in 9m56s" is a precision
     * this sentence does not have and does not need — the exact instant is two
     * rows down in the Lease panel, next to expires_at. */
    let secs = Math.abs(Math.round((at.getTime() - Date.now()) / 1000));
    if (secs >= 60) secs = Math.round(secs / 60) * 60;
    tail = at.getTime() > Date.now()
      ? t('device.say.expires', { when: fmtSecs(secs) })
      : t('device.say.expiryPassed', { when: fmtSecs(secs) });
  }

  return head + ' ' + tail;
}

/* sheetClock is a wall clock, not a timestamp: "since 14:02" is what a person
 * says. A lease acquired on another day gets the date too, because "since
 * 14:02" about yesterday afternoon is a sentence that lies. */
function sheetClock(v) {
  const at = parseTime(v);
  if (!at) return null;
  const now = new Date();
  const sameDay = at.getFullYear() === now.getFullYear()
    && at.getMonth() === now.getMonth()
    && at.getDate() === now.getDate();
  return sameDay
    ? at.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false })
    : at.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
}

/* whereSentence says where the handset physically is, in words.
 *
 * Every branch is written out rather than assembled from fragments, because a
 * missing host and a missing rack slot mean different things and each deserves
 * its own sentence. A farm with no host recorded for a device cannot reach it
 * at all, and that is worth saying rather than rendering as an em dash. */
function whereSentence(d) {
  const slot = nz(d.rackSlot);
  const host = nz(d.host);
  const hub = nz(d.hubPath) || nz(d.hubID);
  if (host && slot && hub) return t('device.where.full', { slot, host, hub });
  if (host && hub) return t('device.where.noSlot', { host, hub });
  if (host && slot) return t('device.where.noHub', { slot, host });
  if (host) return t('device.where.hostOnly', { host });
  /* No host is not a missing field like the others. A device this farm has no
   * host_id for is a device it cannot reach at all, so every branch below says
   * so — and none of them drops a hub or a slot that IS recorded, which would
   * make the sentence contradict the row it was built from. */
  if (slot && hub) return t('device.where.noHost', { slot, hub });
  if (slot) return t('device.where.slotOnly', { slot });
  if (hub) return t('device.where.hubOnly', { hub });
  return t('device.where.nothing');
}

/* androidText keeps sdk_int on the screen.
 *
 * The API level is what a fleet is filtered on — "every sdk 33 handset" is a
 * real question and "every Android 13 handset" is the same question asked in a
 * form no selector accepts. The release alone would have left it reachable
 * only through the Raw tab. */
function androidText(d) {
  const rel = nz(d.android);
  const sdk = d.sdk === undefined || d.sdk === null || d.sdk === '' ? null : String(d.sdk);
  if (rel && sdk) return rel + ' (sdk ' + sdk + ')';
  return rel || (sdk ? 'sdk ' + sdk : null);
}

/* ------------------------------------------------------------------ *
 * Identifiers — demoted, kept, and copyable
 * ------------------------------------------------------------------ */

/* Every string this device is known by, collapsed.
 *
 * They are not deleted and they are not hidden: an operator who has to join
 * this row to a log line needs the uuid, and needs it byte-exact. They are
 * DEMOTED — one disclosure below the answer — and each carries a copy button,
 * because the failure mode of a uuid on a screen is somebody retyping it. */
function identifiersBlock(d) {
  const dl = el('dl', { class: 'kv' });

  append(dl, kv(t('device.f.farmUID'), identValue(d.farmUID), { raw: 'farm_uid' }));
  append(dl, kv(t('device.f.deviceID'), identValue(d.id), { raw: 'device_id' }));
  append(dl, kv(t('device.f.serial'), identValue(d.serial, d.serialAmbiguous
    ? el('span', { class: 'chip chip-degraded', title: t('device.serialAmbiguousWhy') },
      el('span', { 'aria-hidden': 'true' }, '▲'), t('device.serialAmbiguous'))
    : null), { raw: 'adb_serial' }));
  append(dl, kv(t('device.f.usbPath'), identValue(d.usbPath), { raw: 'usb_path' }));
  append(dl, kv(t('device.f.devpath'), identValue(d.devPath), { raw: 'adb_devpath', term: 'devpath' }));
  append(dl, kv(t('device.f.slotID'),
    identValue(d.slotID === undefined || d.slotID === null ? null : String(d.slotID)),
    { raw: 'slot_id', term: 'slot' }));
  /* labels used to be printed as JSON.stringify(d.labels) — a single
   * unwrappable line of braces and quotes that an operator had to parse by eye
   * to answer "is this an sdk 35 device". One chip per pair answers it by
   * looking. */
  append(dl, kv(t('device.f.labels'), labelChips(d.labels), { raw: 'labels' }));

  return el('details', { class: 'idents' },
    el('summary', null, t('device.identifiers')),
    el('p', { class: 'dev-note' }, t('device.identifiersNote')),
    dl);
}

/* identValue is the mono, dim, copyable rendering of a raw identifier.
 *
 * .ident is unit 1's class in the shared sheet. drawer.css carries a definition
 * of it for as long as that one is not there yet; see the note beside it. */
function identValue(value, extra) {
  const has = value !== undefined && value !== null && value !== '';
  return el('span', { class: 'ident-row' },
    el('span', { class: 'ident' }, has ? String(value) : t('state.none')),
    extra || null,
    has ? copyButton(String(value)) : null);
}

/* copyButton puts one identifier on the clipboard.
 *
 * The glyph changes as well as the word, and the word is announced: a control
 * whose only feedback is a colour is a control a colour-blind operator cannot
 * read, and one whose only feedback is visual is one a screen reader misses. */
function copyButton(value) {
  const mark = el('span', { 'aria-hidden': 'true' }, '⧉');
  const said = el('span', { class: 'sr-only', role: 'status' });
  const btn = el('button', {
    type: 'button', class: 'ghost mini copy-btn', title: t('device.copy'),
    'aria-label': t('device.copy')
  }, mark, said);

  let back = null;
  btn.addEventListener('click', async () => {
    let note;
    try {
      await navigator.clipboard.writeText(value);
      mark.textContent = '✓';
      note = t('device.copied');
    } catch (_) {
      mark.textContent = '✕';
      note = t('device.copyFailed');
    }
    said.textContent = note;
    btn.title = note;
    if (back) clearTimeout(back);
    back = setTimeout(() => {
      back = null;
      mark.textContent = '⧉';
      said.textContent = '';
      btn.title = t('device.copy');
    }, 2000);
  });
  return btn;
}

/* labelChips renders farm.devices.labels as one chip per pair.
 *
 * Keys and values are server data and are printed verbatim. A value that is not
 * a string is JSON-encoded rather than String()-ed, because String({}) is
 * "[object Object]" and that is a lie about what the column holds. */
function labelChips(labels) {
  if (!labels || typeof labels !== 'object' || Array.isArray(labels)) return null;
  const keys = Object.keys(labels).sort();
  if (!keys.length) return null;
  return el('div', { class: 'chips' }, keys.map((k) => {
    const v = labels[k];
    /* JSON-encoded rather than String()-ed, for everything that is not already
     * a string. String({}) is "[object Object]" and String(null) is "" — the
     * first is unreadable and the second is worse, because it renders a null
     * column identically to an empty one. */
    const text = v === undefined ? 'undefined' : (typeof v === 'string' ? v : JSON.stringify(v));
    return el('span', { class: 'chip chip-plain lbl', title: k + '=' + text },
      el('span', { class: 'lbl-k' }, k),
      el('span', { class: 'lbl-v' }, text));
  }));
}

/* ------------------------------------------------------------------ *
 * Health and lease
 * ------------------------------------------------------------------ */

function healthPanel(d) {
  return el('div', { class: 'sheet-sec' },
    el('p', { class: 'dev-note' }, t('device.healthNote')),
    el('dl', { class: 'kv' },
      kv(t('device.f.health'), healthChip(d.health), { raw: 'health', term: 'health' }),
      kv(t('device.f.healthSince'), whenCell(d.healthSince), { raw: 'health_since' }),
      kv(t('device.f.adbState'), d.adbState, { raw: 'adb_state' }),
      kv(t('device.f.battery'), batteryEl(d.battery), { raw: 'battery_pct' }),
      kv(t('device.f.batteryTemp'),
        d.batteryTempDC !== undefined && d.batteryTempDC !== null
          ? (Number(d.batteryTempDC) / 10).toFixed(1) + ' °C' : null,
        { raw: 'battery_temp_dc' }),
      kv(t('device.f.slotState'), d.slotState, { raw: 'slot_state', term: 'slot' }),
      kv(t('device.f.consecBad'), d.consecBad, { raw: 'consec_bad' }),
      kv(t('device.f.nextRung'), d.nextLadderTier, { raw: 'next_ladder_tier', term: 'rung' }),
      kv(t('device.f.failureScore'), d.failureScore !== undefined && d.failureScore !== null
        ? String(d.failureScore) : null, { raw: 'failure_score' }),
      kv(t('device.f.lastSeen'), whenCell(d.lastSeen), { raw: 'last_seen_at' }),
      kv(t('device.f.quarantine'), d.quarantineID
        ? el('span', null, el('span', { class: 'ident' }, String(d.quarantineID)),
          d.quarantineReason ? ' — ' + d.quarantineReason : '')
        : t('device.noQuarantine'), { raw: 'quarantine_id', term: 'quarantine' })));
}

function leasePanel(d) {
  if (!isHeld(d)) {
    return el('div', { class: 'sheet-sec' },
      emptyState(t('device.noLease'), t('device.noLeaseNote')));
  }
  return el('div', { class: 'sheet-sec' },
    el('div', { class: 'chips dev-chips' }, leaseChips(d)),
    el('p', { class: 'dev-note' }, t('device.leaseNote')),
    el('dl', { class: 'kv' },
      kv(t('device.f.leaseState'), d.leaseState, { raw: 'state', term: 'lease' }),
      kv(t('device.f.leaseID'), d.leaseID ? identValue(d.leaseID) : null, { raw: 'lease_id' }),
      kv(t('device.f.fence'), d.fence !== undefined && d.fence !== null
        ? el('span', { class: 'ident' }, String(d.fence)) : null, { raw: 'fence', term: 'fence' }),
      kv(t('device.f.job'), d.jobID ? identValue(d.jobID) : null, { raw: 'job_id' }),
      kv(t('device.f.tenant'), d.tenant, { raw: 'tenant_id', term: 'tenant' }),
      kv(t('device.f.holder'), d.holder, { raw: 'holder', term: 'holder' }),
      kv(t('device.f.acquired'), whenCell(d.acquiredAt), { raw: 'acquired_at' }),
      kv(t('device.f.expires'), whenCell(d.expiresAt), { raw: 'expires_at' }),
      kv(t('device.f.reclaimable'), whenCell(d.reclaimableAt), { raw: 'reclaimable_at' })));
}

/* ------------------------------------------------------------------ *
 * The footer
 * ------------------------------------------------------------------ */

/* sheetActions builds the persistent footer.
 *
 * Every one of these goes through openConfirm, which names what it will disturb
 * and demands a typed reason for the audit log. That flow and revokeLease's
 * impact list are deliberately untouched here: they are the best destructive-
 * action wording in the product and this unit only moved where the buttons sit. */
function sheetActions(d) {
  return [
    isHeld(d) ? el('button', {
      class: 'danger', type: 'button',
      onclick: thenReread(() => revokeLease(normLease({
        id: d.leaseID, fence: d.fence, state: d.leaseState, protected: d.protected,
        device_id: d.id, rack_slot: d.rackSlot, job_id: d.jobID, holder: d.holder
      })))
    }, t('device.revoke')) : null,
    d.slotID !== undefined && d.slotID !== null
      ? el('button', { type: 'button', onclick: thenReread(() => powerSlot(d)) }, t('device.powerCycle')) : null,
    d.quarantineID ? el('button', {
      type: 'button',
      onclick: thenReread(() => closeQuarantine(normQuarantine({
        id: d.quarantineID, scope: 'device', device_id: d.id,
        rack_slot: d.rackSlot, reason: d.quarantineReason
      })))
    }, t('device.closeQuarantine')) : null,
    d.host ? el('button', {
      type: 'button',
      onclick: thenReread(() => drainHost(d.host, (state.data.fleet || []).filter((x) => x.host === d.host)))
    }, t('device.drainHost', { host: d.host })) : null
  ].filter(Boolean);
}

/* thenReread re-reads the device once the confirm dialog is done with it.
 *
 * wireConfirm — unit 7's code, untouched here — closes its dialog on success
 * and calls refreshAll(), which redraws the VIEW. It does not redraw this
 * sheet, and nothing else does either. Before the footer existed that barely
 * mattered; now the buttons never scroll away, so an operator who revoked a
 * lease would be left in front of "In use by X" and a live Revoke button
 * carrying a fence the server has already moved past — and clicking it again
 * is the easy path rather than the awkward one.
 *
 * The confirm dialog's own close event is the only hook this needs. It fires
 * on cancel too, where the re-read is merely redundant. */
function thenReread(run) {
  return () => {
    run();
    const c = $('#dlg-confirm');
    /* Only if the action actually opened it. An unconditional listener would
     * sit and wait for somebody else's confirm dialog. */
    if (c && c.open) c.addEventListener('close', () => { refreshSheet(); }, { once: true });
  };
}

/* ------------------------------------------------------------------ *
 * The live screen
 *
 * A device's picture, decoded in this tab, and a finger on it.
 *
 * WHAT THE PICTURE ENDING MEANS. Nothing about the lease. The proxy cuts a
 * screen mid-frame the instant a fence rises, a pod is replaced on every
 * deploy, and a handset's encoder dies for its own reasons — all of which
 * arrive here as a bare end of stream with no explanation attached. So this
 * panel says "the stream ended" and never, under any circumstance, infers that
 * the lease is over. It is not. farm.leases.release_reason has seven allowed
 * values and none of them is about a socket, and a dashboard that guessed
 * otherwise would be the STF #663 mistake reproduced in the one place an
 * operator would believe it.
 *
 * WHY fetch AND NOT EventSource. The token is a header, never a query
 * parameter (see the block above readToken). EventSource cannot send a header
 * and so needed the ticket; fetch can, and a ReadableStream feeding
 * VideoDecoder needs no ticket at all — which is why the ticket stays scoped to
 * exactly one route and keeps its "misrouted" counter meaningful.
 *
 * NO TRANSCODE ANYWHERE. The handset's encoder produces Annex-B H.264 and
 * WebCodecs takes Annex-B H.264. The bytes that reach this decoder are the
 * bytes the phone wrote; the api spliced them and did not look inside.
 * ------------------------------------------------------------------ */

/* SCREEN_MAX_PACKET mirrors internal/scrcpy's cap, and it is here for the same
 * reason it is there: the length that would size this allocation was chosen by
 * a handset. Four mebibytes is more than an order of magnitude above the
 * largest frame a phone's hardware encoder produces, so a stream that reaches
 * it is not a large frame — it is a desynchronised parse or a wedged encoder,
 * and the only correct thing to do with the connection is drop it. Without
 * this, a confused phone can make this tab allocate four gibibytes. */
const SCREEN_MAX_PACKET = 4 * 1024 * 1024;

/* Android KeyEvent constants. Not a whitelist — the API accepts any keycode —
 * just the four an operator reaches for when a phone is misbehaving. */
const SCREEN_KEYS = [
  { labelKey: 'screen.key.back', code: 4 },
  { labelKey: 'screen.key.home', code: 3 },
  { labelKey: 'screen.key.recents', code: 187 },
  { labelKey: 'screen.key.power', code: 26 }
];

/* screenSession is the one open panel. One at a time in this tab, because the
 * server allows one session per device and a second panel would only discover
 * that as a 409 after spending a round trip. */
let screenSession = null;

/* screenPanel builds the Screen section of the device drawer.
 *
 * It does NOT open a stream. A session costs three ADB transports and a
 * hardware encoder on a phone, so it starts when a human asks for it and not
 * when a drawer is opened to read a battery percentage. */
function screenPanel(d) {
  const canvas = el('canvas', { class: 'screen-canvas', width: 1, height: 1, hidden: true });
  const status = el('div', { class: 'screen-status' });
  const keys = el('div', { class: 'screen-keys', hidden: true });
  const openBtn = el('button', { class: 'ghost', type: 'button' }, t('screen.open'));
  const closeBtn = el('button', { class: 'ghost', type: 'button', hidden: true }, t('screen.close'));

  const say = (level, text) => {
    status.replaceChildren(el('span', { class: 'screen-note screen-' + level }, text));
  };

  if (typeof window.VideoDecoder !== 'function') {
    /* Said plainly rather than left as a blank rectangle. WebCodecs is the
     * decoder; without it there is no fallback that does not mean transcoding
     * on the control plane, which this feature deliberately does not do. */
    say('warn', t('screen.noWebCodecs', { id: shortId(d.id) }));
    return el('div', { class: 'screen-panel' }, status);
  }

  for (const k of SCREEN_KEYS) {
    keys.append(el('button', {
      class: 'ghost', type: 'button',
      onclick: () => {
        if (!screenSession) return;
        screenSession.send([
          { type: 'key', action: 'down', keycode: k.code },
          { type: 'key', action: 'up', keycode: k.code }
        ]);
      }
    }, t(k.labelKey)));
  }

  const stopped = () => {
    canvas.hidden = true;
    keys.hidden = true;
    closeBtn.hidden = true;
    openBtn.hidden = false;
    openBtn.disabled = false;
    screenSession = null;
  };

  openBtn.addEventListener('click', async () => {
    openBtn.disabled = true;
    say('info', t('screen.starting'));
    try {
      screenSession = await openScreenStream(d, canvas, say, stopped);
      canvas.hidden = false;
      keys.hidden = false;
      openBtn.hidden = true;
      closeBtn.hidden = false;
    } catch (e) {
      openBtn.disabled = false;
      say('error', errText(e));
      /* A refusal carries its remedy in detail.remedy. That is the difference
       * between an operator setting two environment variables and an operator
       * filing a ticket. */
      if (e && e.detail && e.detail.remedy) {
        status.append(el('div', { class: 'screen-remedy' }, t('screen.remedy', { remedy: e.detail.remedy })));
      }
    }
  });

  closeBtn.addEventListener('click', () => {
    if (screenSession) screenSession.stop(t('screen.closedByOperator'));
  });

  wireScreenInput(canvas, () => screenSession);

  return el('div', { class: 'screen-panel' },
    el('div', { class: 'screen-controls' }, openBtn, closeBtn, keys),
    status, canvas);
}

/* openScreenStream fetches the stream, decodes it, and paints it.
 *
 * Returns a handle with send() and stop(). onEnd runs exactly once, whatever
 * ended it. */
async function openScreenStream(d, canvas, say, onEnd) {
  const ctrl = new AbortController();
  const headers = { Accept: 'application/vnd.device-farmer.screen' };
  if (apiToken) headers.Authorization = 'Bearer ' + apiToken;

  let res;
  try {
    res = await fetch(apiURL('devices/' + encodeURIComponent(d.id) + '/screen'), {
      headers, cache: 'no-store', signal: ctrl.signal
    });
  } catch (err) {
    throw new ApiError(0, 'unreachable',
      'the control plane is unreachable from this browser: ' + err.message);
  }
  if (!res.ok) {
    /* The refusal body is JSON even though the success body is not. */
    let data = null;
    try { data = JSON.parse(await res.text()); } catch (_) { /* leave it null */ }
    const e = (data && data.error) || {};
    if (res.status === 401 || res.status === 403) noteAuthFailure(res.status, e.message);
    throw new ApiError(res.status, e.code || 'http_' + res.status,
      e.message || res.statusText || 'the screen was refused', e.detail);
  }

  const session = res.headers.get('X-Screen-Session') || '';
  if (!session) {
    throw new ApiError(0, 'bad_response',
      'the stream carries no session id, so input would have nothing to address');
  }

  let ended = false;
  const end = (why) => {
    if (ended) return;
    ended = true;
    ctrl.abort();
    /* NEVER "the lease ended". See the block at the top of this section. */
    say('info', t('screen.ended', { why }));
    onEnd();
  };

  const handle = {
    session,
    width: 0,
    height: 0,
    stop: (why) => end(why || 'closed'),
    send: async (events) => {
      if (ended) return;
      try {
        await api.post('devices/' + encodeURIComponent(d.id) + '/input', { session, events });
      } catch (e) {
        /* Input failing is not the stream failing, and it is certainly not the
         * lease failing. Say it once and keep the picture. */
        say('warn', t('screen.inputFailed', { err: errText(e) }));
      }
    }
  };

  pumpScreen(res.body, canvas, handle, say, end).catch((err) => {
    end(err && err.name === 'AbortError' ? 'closed' : (err.message || String(err)));
  });
  return handle;
}

/* pumpScreen reads the framing and feeds the decoder.
 *
 * The layout is scrcpy's, verified against app/src/demuxer.c: four bytes of
 * codec id, then twelve-byte headers. A header whose first byte has 0x80 set is
 * a SESSION header carrying a new width and height — it is not a one-time
 * preamble, it arrives again on every rotation, and a reader that assumed
 * packets forever after the first would parse a rotation's width as a timestamp
 * and its height as a payload length. The height of a phone is a plausible
 * number of bytes, so that failure would not even be loud. */
async function pumpScreen(body, canvas, handle, say, end) {
  const reader = body.getReader();
  let buf = new Uint8Array(0);

  const need = async (n) => {
    while (buf.length < n) {
      const { value, done } = await reader.read();
      if (done) return null;
      const merged = new Uint8Array(buf.length + value.length);
      merged.set(buf, 0);
      merged.set(value, buf.length);
      buf = merged;
    }
    const out = buf.slice(0, n);
    buf = buf.subarray(n);
    return out;
  };

  const codecBytes = await need(4);
  if (!codecBytes) { end('the server produced nothing'); return; }
  const codecName = String.fromCharCode.apply(null, codecBytes).replace(/\0/g, '');
  if (codecName !== 'h264') {
    end('the stream is ' + codecName + ', which this panel cannot decode');
    return;
  }

  const ctx = canvas.getContext('2d');
  let decoder = null;
  let configBytes = null;   /* SPS and PPS, prepended to each key frame */

  const closeDecoder = () => {
    if (decoder && decoder.state !== 'closed') {
      try { decoder.close(); } catch (_) { /* already gone */ }
    }
    decoder = null;
  };

  for (;;) {
    const hdr = await need(12);
    if (!hdr) { closeDecoder(); end('the device closed the stream'); return; }
    const view = new DataView(hdr.buffer, hdr.byteOffset, 12);

    if (hdr[0] & 0x80) {
      /* A session header: the video size, now. Every coordinate this panel
       * sends is in this space, so it is stored rather than merely displayed. */
      handle.width = view.getUint32(4);
      handle.height = view.getUint32(8);
      canvas.width = handle.width;
      canvas.height = handle.height;
      /* A rotation invalidates the decoder's configuration, so it is rebuilt
       * from the parameter sets the next key frame carries. */
      closeDecoder();
      say('ok', t('screen.streaming', { w: handle.width, h: handle.height }));
      continue;
    }

    const meta = view.getBigUint64(0);
    const isConfig = (meta & (1n << 62n)) !== 0n;
    const isKey = (meta & (1n << 61n)) !== 0n;
    const pts = Number(meta & ((1n << 61n) - 1n));
    const len = view.getUint32(8);

    if (len === 0) { closeDecoder(); end('the device sent an empty packet'); return; }
    if (len > SCREEN_MAX_PACKET) {
      /* Checked BEFORE the read that would size an allocation from it. */
      closeDecoder();
      end('the device declared a ' + len + '-byte frame, past the ' + SCREEN_MAX_PACKET +
        '-byte cap; the stream is desynchronised or the encoder is wedged');
      return;
    }

    const payload = await need(len);
    if (!payload) { closeDecoder(); end('the stream was cut mid-frame'); return; }

    if (isConfig) {
      /* SPS and PPS. Kept and prepended to each key frame rather than fed on
       * their own: a decoder that joins at a later key frame needs them again,
       * and duplicate parameter sets in an Annex-B stream are legal and
       * ignored. */
      configBytes = payload;
      if (!decoder) decoder = buildScreenDecoder(configBytes, ctx, canvas, end);
      continue;
    }

    if (!decoder) {
      if (!configBytes) continue;  /* nothing to configure with yet */
      decoder = buildScreenDecoder(configBytes, ctx, canvas, end);
      if (!decoder) return;
    }
    if (decoder.state !== 'configured') continue;

    let chunkBytes = payload;
    if (isKey && configBytes) {
      chunkBytes = new Uint8Array(configBytes.length + payload.length);
      chunkBytes.set(configBytes, 0);
      chunkBytes.set(payload, configBytes.length);
    }
    try {
      decoder.decode(new EncodedVideoChunk({
        type: isKey ? 'key' : 'delta',
        timestamp: pts,
        data: chunkBytes
      }));
    } catch (err) {
      closeDecoder();
      end('the decoder refused a frame: ' + err.message);
      return;
    }
  }
}

/* buildScreenDecoder configures a VideoDecoder from the stream's own parameter
 * sets.
 *
 * The codec string is READ FROM THE SPS rather than hard-coded, because it has
 * to describe the phone that is actually streaming: profile_idc, the constraint
 * flags byte and level_idc, in hex, are the three bytes after the SPS NAL
 * header. A hard-coded avc1.42E01E works until the first device that encodes at
 * a different level, and then fails as a decoder that will not configure — on
 * that model only, which is the worst kind of bug to be told about. */
function buildScreenDecoder(configBytes, ctx, canvas, end) {
  const sps = findNAL(configBytes, 7);
  if (!sps || sps.length < 4) {
    end('the stream carried no usable parameter set, so no decoder can be configured for it');
    return null;
  }
  const hex = (b) => b.toString(16).padStart(2, '0');
  const codec = 'avc1.' + hex(sps[1]) + hex(sps[2]) + hex(sps[3]);

  const decoder = new VideoDecoder({
    output: (frame) => {
      try {
        ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
      } finally {
        /* Closed in a finally, always. A VideoFrame holds a decoder buffer, and
         * a browser that runs out of them stops decoding rather than throwing:
         * the picture would simply freeze with nothing in the console. */
        frame.close();
      }
    },
    error: (err) => end('the decoder failed: ' + err.message)
  });
  try {
    decoder.configure({ codec, optimizeForLatency: true });
  } catch (err) {
    end('this browser cannot decode ' + codec + ': ' + err.message);
    return null;
  }
  return decoder;
}

/* findNAL returns the first Annex-B NAL unit of the given type, without its
 * start code.
 *
 * Both three- and four-byte start codes occur in one stream — encoders commonly
 * write four before parameter sets and three before slices — so a scanner that
 * knew only one would miss exactly the SPS it is looking for. */
function findNAL(bytes, want) {
  let i = 0;
  let start = -1;
  let type = -1;
  const take = (endAt) => (start >= 0 && type === want) ? bytes.subarray(start, endAt) : null;
  while (i + 3 < bytes.length) {
    const four = bytes[i] === 0 && bytes[i + 1] === 0 && bytes[i + 2] === 0 && bytes[i + 3] === 1;
    const three = bytes[i] === 0 && bytes[i + 1] === 0 && bytes[i + 2] === 1;
    if (four || three) {
      const found = take(i);
      if (found) return found;
      i += four ? 4 : 3;
      start = i;
      type = bytes[i] & 0x1f;
      continue;
    }
    i++;
  }
  return take(bytes.length);
}

/* wireScreenInput turns pointer events on the canvas into input messages.
 *
 * COORDINATES ARE IN VIDEO SPACE, and the conversion below is the whole reason
 * this function exists rather than the events being sent raw. The canvas is
 * displayed at whatever size the drawer gives it; the frame is whatever the
 * encoder produced after max_size scaling and rotation; the device's own panel
 * is a third thing again. A 1080x2400 phone streamed at max_size 1024 is
 * 460x1024 and displayed at maybe 260 CSS pixels wide, so there are three
 * coordinate spaces in play and only one of them means anything to the device.
 *
 * The API refuses a coordinate outside the frame rather than clamping it, which
 * is what catches a caller that got this wrong. */
function wireScreenInput(canvas, current) {
  const at = (ev) => {
    const s = current();
    if (!s || !s.width || !s.height) return null;
    const r = canvas.getBoundingClientRect();
    if (!r.width || !r.height) return null;
    const x = Math.round((ev.clientX - r.left) / r.width * s.width);
    const y = Math.round((ev.clientY - r.top) / r.height * s.height);
    /* Clamped here rather than sent and refused: a pointer released one pixel
     * off the edge is an ordinary gesture, and a refusal there would discard
     * the whole batch — which is the batch carrying the UP, so the phone would
     * be left with a finger down that nothing lifts. */
    return {
      x: Math.min(Math.max(x, 0), s.width - 1),
      y: Math.min(Math.max(y, 0), s.height - 1)
    };
  };

  let down = false;
  let pending = null;
  let flushing = false;

  /* Moves are coalesced to the latest position. A drag fires pointermove far
   * faster than a phone consumes input, and sending every one would spend the
   * batch limit on positions the finger has already left. */
  const flush = async () => {
    if (flushing || !pending) return;
    flushing = true;
    const ev = pending;
    pending = null;
    const s = current();
    if (s) await s.send([ev]);
    flushing = false;
    if (pending) flush();
  };

  canvas.addEventListener('pointerdown', (e) => {
    const p = at(e);
    if (!p) return;
    down = true;
    try { canvas.setPointerCapture(e.pointerId); } catch (_) { /* not capturable */ }
    const s = current();
    if (s) s.send([{ type: 'touch', action: 'down', x: p.x, y: p.y, pressure: 1 }]);
  });

  canvas.addEventListener('pointermove', (e) => {
    if (!down) return;
    const p = at(e);
    if (!p) return;
    pending = { type: 'touch', action: 'move', x: p.x, y: p.y, pressure: 1 };
    flush();
  });

  /* pointerup AND pointercancel, because a cancelled pointer — the browser
   * taking over for a scroll gesture, the window losing focus — is a finger
   * this page will never hear about again. Without the UP, the phone keeps the
   * pointer down and behaves as though somebody is still holding the screen. */
  for (const kind of ['pointerup', 'pointercancel']) {
    canvas.addEventListener(kind, (e) => {
      if (!down) return;
      down = false;
      pending = null;
      const p = at(e);
      const s = current();
      if (s && p) s.send([{ type: 'touch', action: 'up', x: p.x, y: p.y }]);
    });
  }

  canvas.addEventListener('wheel', (e) => {
    const p = at(e);
    const s = current();
    if (!p || !s) return;
    e.preventDefault();
    /* deltaY is positive when the content scrolls down and scrcpy's vertical
     * scroll is positive upward, so the sign is inverted here — at the edge
     * that knows about browsers, rather than in the API. */
    s.send([{ type: 'scroll', x: p.x, y: p.y, h: 0, v: -Math.sign(e.deltaY) }]);
  }, { passive: false });
}

/* closeScreenOnDrawerClose stops a session when the drawer closes.
 *
 * Without it the fetch keeps running against a canvas nobody can see: the
 * handset keeps encoding, the api keeps holding three transports, and the
 * session occupies one of the process's few slots until something else times
 * out. The drawer closing is the clearest possible statement that nobody is
 * watching. */
function closeScreenOnDrawerClose() {
  const dlg = $('#dlg-device');
  if (!dlg) return;
  dlg.addEventListener('close', () => {
    if (screenSession) screenSession.stop(t('screen.closedDrawer'));
  });
}
