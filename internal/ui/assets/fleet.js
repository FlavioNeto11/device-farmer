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

/* The bucket devices the API returned with no host are grouped into. It is a
 * display string AND a grouping key AND the one value in that column that
 * names no host, so it exists once rather than three times — and everywhere
 * this page turns a group back into a request it has to be checked for:
 * ?host=(unassigned host) matches nothing, and there is no host to drain. */
const UNASSIGNED_HOST = '(unassigned host)';

/* ------------------------------------------------------------------ *
 * KEYED RECONCILIATION
 *
 * This grid is the only view on the page that repaints under a live event
 * stream, and it used to repaint by throwing itself away: renderFleet ended in
 * body.replaceChildren(frag), so every stream event, every poll and every
 * fifteen-second clock tick destroyed every tile on screen and built new ones.
 *
 * What that cost, measured against a running demo rather than guessed at: the
 * fleet body was rebuilt six times in twelve seconds; a tile that was focused
 * and then left alone for eight seconds was no longer in the document, with
 * document.activeElement fallen back to <body>; and browser automation could
 * not click a device at all, because the element it had just read went stale
 * before the click landed. A human moving a mouse toward a tile is in exactly
 * the same race, and a keyboard operator loses their place every few seconds.
 *
 * So nodes are kept and updated instead. Every reconciled node carries
 * data-key — the identity of the thing it stands for — and data-sig, a join of
 * exactly the fields it renders. Per render: same key and same signature and
 * nothing is touched at all; same key and a changed signature and the node's
 * contents are rewritten while the element itself survives, and with it focus,
 * hover, :active, the text selection and any automation handle.
 *
 * THE RISK THIS TRADES FOR IS WORSE THAN THE ONE IT REMOVES IF IT IS GOT
 * WRONG. A patch applied to the wrong node shows one device's data under
 * another device's name, which is how an operator power-cycles the wrong
 * handset — and unlike a rebuild, it looks completely fine. Two rules keep it
 * from happening, and neither is negotiable:
 *
 *   1. The key is the device id and nothing else. Never a rack slot, a serial
 *      or a usb path: those are labels a human can edit, duplicate or leave
 *      off, and two rows sharing a key is precisely the bug above.
 *   2. patch re-renders EVERY field the signature covers. It never tries to
 *      work out which one changed, because that is where the second copy of
 *      the tile's field list would live and the second copy is the one that
 *      goes stale.
 * ------------------------------------------------------------------ */

/* The children each container owns, by key. On the container rather than in
 * one flat table, so a whole host block dropping out of the DOM takes its hub
 * blocks, its grids and their entries with it: nothing here needs cleaning up
 * by hand, which is the failure mode a flat table would have. */
const keyedChildren = new WeakMap();

/* fleetSubject: the item a reconciled node currently stands for.
 *
 * A node now outlives the object it was first built from — that is the whole
 * point — so a handler that closed over the row it was created with would act
 * on whatever the device looked like the first time it was drawn. Every
 * handler on a reconciled node reads its subject from here instead, and
 * reconcile refreshes it on every pass, signature change or not. */
const fleetSubject = new WeakMap();

function subjectOf(node, fallback) {
  const v = fleetSubject.get(node);
  return v === undefined ? fallback : v;
}

/* signature joins the values a node renders into one comparable string.
 *
 * JSON rather than a join on some separator, because a separator has to be a
 * character no field can contain and there is no such character in a model
 * name typed by whoever assembled the rack. JSON also keeps the types apart:
 * the string "0" and the number 0 are different signatures, as they should be.
 *
 * undefined gets an object of its own, because undefined and null are
 * different answers here and JSON would collapse both to null. A lease fence
 * of undefined is WITHHELD — another tenant holds this device — and prints
 * "—", while a fence of 0 prints "f0". No scalar field can stringify to an
 * object, so the two can never be confused.
 *
 * THE LANGUAGE IS PART OF EVERY SIGNATURE, and it is here rather than in each
 * caller's list because forgetting it is silent and specific: switching to
 * Portuguese left every host head in English, because none of the numbers in
 * it had changed and the reconciler therefore had nothing to repaint. A
 * signature is a claim about what a node renders, and which dictionary was in
 * force is part of that whether the caller thought about it or not. */
const SIG_UNDEFINED = { undefinedValue: true };

function signature(parts) {
  return JSON.stringify([currentLang()].concat(parts.map((v) => (v === undefined ? SIG_UNDEFINED : v))));
}

/* reconcile brings container's children into line with items, keeping the
 * elements that are already right.
 *
 *   keyOf(item, i)  identity. Stable across renders or nothing below works.
 *   sigOf(item)     the fields the node renders, joined. See signature().
 *   create(item)    a new element for an item that has no node yet.
 *   patch(node,item) rewrite the node's contents in place. Every field.
 *                   Return false if it could NOT finish — the guard below
 *                   makes that possible — and the node keeps its old
 *                   signature so the next pass tries again. Stamping the new
 *                   signature over a half-applied patch would leave the node
 *                   permanently wrong and permanently considered up to date.
 *
 * opts.frozen defers STRUCTURAL change — insertion, removal, reordering —
 * and reports how much it deferred, while still patching content in place.
 * That is the interaction guard: content stays live, the ground does not move
 * under someone who is working in the grid right now.
 *
 * Returns { added, removed, moved, held }, counting DOM operations performed
 * and, in held, the ones a frozen pass declined to perform.
 */
function reconcile(container, items, keyOf, sigOf, create, patch, opts) {
  const frozen = !!(opts && opts.frozen);
  let known = keyedChildren.get(container);
  if (!known) { known = new Map(); keyedChildren.set(container, known); }

  const out = { added: 0, removed: 0, moved: 0, held: 0 };
  const want = [];
  const seen = new Set();
  const fresh = new Set();

  let i = -1;
  for (const item of items) {
    i += 1;
    const key = String(keyOf(item, i));
    /* Two items under one key would make one node stand for both, and the
     * second one's data would appear under the first one's name. Drop the
     * duplicate rather than render that: the caller's key is wrong, and
     * guessing which row was meant is not this function's business. */
    if (seen.has(key)) continue;
    seen.add(key);

    const sig = sigOf(item);
    let node = known.get(key);
    if (!node) {
      if (frozen) { out.held += 1; continue; }
      node = create(item);
      node.dataset.key = key;
      fleetSubject.set(node, item);
      node.dataset.sig = sig;
      known.set(key, node);
      fresh.add(node);
      out.added += 1;
    } else {
      // Always, signature change or not: the node's handlers read their
      // subject from here, and two rows can render identically while carrying
      // different lease fences into a confirmation dialog.
      fleetSubject.set(node, item);
      if (node.dataset.sig !== sig && patch(node, item) !== false) {
        node.dataset.sig = sig;
      }
    }
    want.push(node);
  }

  for (const [key, node] of Array.from(known)) {
    if (seen.has(key)) continue;
    if (frozen) { out.held += 1; continue; }
    known.delete(key);
    node.remove();
    out.removed += 1;
  }

  const owned = ownedChildren(container, known);

  if (frozen) {
    /* Nothing was inserted or removed, so the nodes that stayed are exactly
     * the ones wanted — count the positions that would have to move. */
    const stay = owned.filter((n) => seen.has(n.dataset.key));
    for (let n = 0; n < want.length; n += 1) if (stay[n] !== want[n]) out.held += 1;
    return out;
  }

  /* Order, walking backwards: each node is inserted before the one that must
   * follow it, which is a no-op for a node already in that position — so a
   * farm whose order has not changed moves nothing at all. Children this
   * reconciler does not own — a host head, the notice panel — are never
   * touched, because the tail starts at whatever currently follows the last
   * owned node rather than at the end of the container. */
  let tail = owned.length ? owned[owned.length - 1].nextSibling : null;
  for (let n = want.length - 1; n >= 0; n -= 1) {
    const node = want[n];
    if (node.parentNode !== container || node.nextSibling !== tail) {
      container.insertBefore(node, tail);
      if (!fresh.has(node)) out.moved += 1;
    }
    tail = node;
  }
  return out;
}

/* The children of container that this reconciler owns, in document order. */
function ownedChildren(container, known) {
  const out = [];
  for (const n of Array.from(container.children)) {
    const k = n.dataset ? n.dataset.key : undefined;
    if (k !== undefined && known.get(k) === n) out.push(n);
  }
  return out;
}

function keyedChild(container, key) {
  const known = keyedChildren.get(container);
  return known ? known.get(String(key)) : undefined;
}

/* syncAttributes copies every attribute the freshly built node carries onto
 * the node being kept.
 *
 * Every attribute rather than a named list of the ones a tile carries today:
 * three units edit this file, and a tile that grows an attribute must not
 * become a tile whose one new attribute is silently never updated. data-key
 * and data-sig belong to the reconciler and are never on the fresh node. */
function syncAttributes(node, built) {
  for (const a of Array.from(built.attributes)) {
    if (node.getAttribute(a.name) !== a.value) node.setAttribute(a.name, a.value);
  }
  for (const a of Array.from(node.attributes)) {
    if (a.name === 'data-key' || a.name === 'data-sig') continue;
    if (!built.hasAttribute(a.name)) node.removeAttribute(a.name);
  }
}

/* ------------------------------------------------------------------ *
 * The interaction guard
 * ------------------------------------------------------------------ */

/* Consumed by the NEXT fleet render, which is then never held back.
 *
 * Set by the "update the grid" control, and by the controls inside the grid
 * whose whole purpose IS a structural change — narrowing to one hub is not
 * something to protect an operator from, it is what they just asked for, and
 * deferring it would answer the click with the same grid plus a banner. */
let fleetReleaseOnce = false;

/* The last time somebody typed or clicked inside the grid. See below. */
let fleetTouchedAt = 0;
let fleetWatchWired = false;

/* Past this much silence the guard lets go. An operator who tabbed into the
 * grid and then went to lunch is not mid-action, and a grid that has stopped
 * telling the truth about the farm because a tile still has focus is the
 * failure this whole change exists to avoid, arrived at from the other side.
 * Half a minute is long enough to read a tile and decide, short enough that
 * nobody is looking at a stale farm without knowing it. */
const FLEET_HOLD_MS = 30000;

function watchFleetInteraction() {
  if (fleetWatchWired) return;
  const body = $('#fleet-body');
  if (!body) return;
  fleetWatchWired = true;
  const touch = () => { fleetTouchedAt = Date.now(); };
  for (const ev of ['keydown', 'pointerdown', 'focusin']) body.addEventListener(ev, touch);
}

/* fleetHeldBack: is somebody working inside this grid right now?
 *
 * The device sheet being open is the easy half: the grid behind it is what the
 * operator came from and will return to.
 *
 * The other half is focus, and the first version of this got it wrong in a way
 * worth writing down. `#fleet-body.contains(document.activeElement)` is true
 * for a KEYBOARD operator part-way through something — and it is also true for
 * anybody who has ever clicked a tile, because a click leaves focus behind and
 * closing the device sheet puts it back. That froze the grid permanently for
 * every mouse user on the page: devices that left the farm never disappeared,
 * new ones never arrived, and every render raised a banner about it.
 *
 * :focus-visible is the browser's own answer to that exact distinction — it is
 * how it decides whether to draw a focus ring — so it is what decides here.
 * Plus a silence bound, because "focused" is not the same as "still working".
 *
 * Content is never frozen, only structure. A grid that shows yesterday's health
 * because somebody tabbed into it is a worse failure than one that moves. */
function fleetHeldBack() {
  if (fleetReleaseOnce) return false;
  const dlg = $('#dlg-device');
  if (dlg && dlg.open) return true;
  const body = $('#fleet-body');
  const a = document.activeElement;
  if (!a || !body || !body.contains(a)) return false;
  if (Date.now() - fleetTouchedAt > FLEET_HOLD_MS) return false;
  // A browser without :focus-visible holds the grid rather than moving it: the
  // conservative answer, and the one this had before the selector existed.
  try { return a.matches(':focus-visible'); } catch (_) { return true; }
}

let heldNoteCount = 0;
let heldNote = null;

/* announceHeld says out loud what the guard is holding back, in the page's
 * existing aria-live region, and offers the control that applies it.
 *
 * Only when the NUMBER changes. Re-raising the same sentence on every stream
 * event would make a polite live region announce itself several times a second
 * to a screen reader, which is a worse interruption than the one the guard
 * exists to prevent — and it would also put back a banner the operator had
 * just dismissed, on the next event, forever.
 *
 * The exception is a banner that was not dismissed but PUSHED OUT: the strip
 * holds five and drops the oldest, and an alert burst is exactly the moment
 * this notice is most likely to be evicted and least likely to be missed. A
 * full strip is how the two are told apart — a dismissal leaves room behind
 * it, an eviction does not. */
function announceHeld(n) {
  const strip = $('#banners');
  const evicted = heldNote && !heldNote.isConnected && strip && strip.children.length >= 5;
  if (heldNoteCount === n && !evicted) return;
  heldNoteCount = n;
  if (!n) { clearBanner('fleet-held'); heldNote = null; return; }
  heldNote = banner('warn', t('fleet.held', { n: String(n) }), {
    key: 'fleet-held',
    action: { label: t('fleet.heldApply'), run: applyHeldFleetChanges }
  });
}

function applyHeldFleetChanges() {
  /* Not a plain renderFleet() call: the operator pressed a button, which moved
   * focus to the banner and away from the grid, and the render must happen
   * with the guard released whatever focus does next. */
  fleetReleaseOnce = true;
  renderFleet();                 // consumes the release and clears it
  clearBanner('fleet-held');
  heldNote = null;
  heldNoteCount = 0;
  /* The button that was just pressed went with the banner. Put the keyboard on
   * the grid it was asked to update rather than at the top of the document —
   * unless the device sheet is open, in which case the browser will refuse to
   * focus anything behind it and is right to. */
  const first = $('.tile', $('#fleet-body'));
  if (first) first.focus();
}

/* heldDeviceCount counts, in devices, the difference between what is drawn and
 * what the data says — which is what the guard is holding back. It reads the
 * DOM rather than the reconcilers' own tallies because "devices" is the unit
 * the sentence is written in, and a held-back host block is several of them.
 *
 * When the two sets agree, the difference is order: count the positions that
 * do not match, so a device that would move is still news. */
function heldDeviceCount(hosts) {
  const want = [];
  // The index is the device's position ON ITS HUB, because that is the index
  // the grid's own reconciler keys an id-less row by; a running total across
  // hubs would compare two different keys for the same tile.
  for (const h of hosts) {
    for (const hub of h.hubs) hub.devices.forEach((d, i) => want.push(deviceKey(d, i)));
  }
  const have = $$('.tile', $('#fleet-body')).map((n) => n.dataset.key || '');
  const wantSet = new Set(want);
  const haveSet = new Set(have);
  let n = 0;
  for (const k of wantSet) if (!haveSet.has(k)) n += 1;
  for (const k of haveSet) if (!wantSet.has(k)) n += 1;
  if (n === 0) {
    for (let i = 0; i < want.length; i += 1) if (want[i] !== have[i]) n += 1;
  }
  return n;
}

/* ------------------------------------------------------------------ *
 * Grouping: the rows, arranged as they are drawn
 * ------------------------------------------------------------------ */

/* fleetGroups turns the filtered rows into the host → hub → device tree the
 * grid draws, with every number each level shows computed once. Separating it
 * from the drawing is what lets a signature be compared without building a
 * node to compare against. */
function fleetGroups(rows) {
  const byHost = new Map();
  for (const d of rows) {
    const host = d.host || UNASSIGNED_HOST;
    let hubs = byHost.get(host);
    if (!hubs) { hubs = new Map(); byHost.set(host, hubs); }
    const hk = hubKeyOf(d);
    if (!hubs.has(hk)) hubs.set(hk, []);
    hubs.get(hk).push(d);
  }

  const hostMeta = new Map((state.data.hosts || []).map((h) => [String(h.id), h]));
  const hubMeta = new Map((state.data.hubs || [])
    .filter((h) => h.id !== undefined && h.id !== null).map((h) => [String(h.id), h]));

  const out = [];
  for (const host of Array.from(byHost.keys()).sort(cmp)) {
    const hubs = byHost.get(host);
    const hubKeys = Array.from(hubs.keys()).sort((a, b) => {
      const da = hubs.get(a)[0], db = hubs.get(b)[0];
      return cmp(da.hubPath || a, db.hubPath || b);
    });
    const hubItems = hubKeys.map((hk) => hubGroup(host, hk, hubs.get(hk), hubMeta.get(hk)));

    const hostDevices = [];
    for (const hub of hubItems) for (const d of hub.devices) hostDevices.push(d);

    const meta = hostMeta.get(String(host));
    const adminState = (meta && meta.adminState) ||
      (hostDevices[0] && hostDevices[0].hostAdminState) || 'enabled';

    out.push({
      host: host,
      adminState: adminState,
      count: hostDevices.length,
      unhealthy: hostDevices.filter((d) => isFault(d.health)).length,
      live: hostDevices.filter((d) => d.leaseState === 'held' || d.leaseState === 'suspect').length,
      // The RENDERED relative time, not the timestamp: "4m ago" goes stale on
      // its own, and a signature over the timestamp would freeze it there.
      seen: meta && meta.lastSeen ? fmtRel(meta.lastSeen) : null,
      rows: hostDevices,
      hubs: hubItems
    });
  }
  return out;
}

function hubGroup(host, hk, list, meta) {
  const devices = list.slice().sort((a, b) => cmp(a.rackSlot || a.usbPath, b.rackSlot || b.usbPath));
  const first = devices[0];
  const hubPath = first.hubPath || (hk === 'nohub' ? null : hk);

  // Prefer the server's v_hub_health numbers (they count every device on the
  // hub, not just the ones passing the current filter).
  const total = meta && meta.devices !== undefined ? Number(meta.devices) : devices.length;
  const unhealthyList = devices.filter((d) => isFault(d.health));
  const unhealthy = meta && meta.unhealthy !== undefined ? Number(meta.unhealthy) : unhealthyList.length;
  let since = meta && meta.worstSince ? meta.worstSince : null;
  if (!since) {
    for (const d of unhealthyList) {
      const ts = parseTime(d.healthSince);
      if (ts && (!since || ts > parseTime(since))) since = d.healthSince;
    }
  }

  // The server sets `correlated` on a hub with more than one unhealthy device;
  // the ratio test is the fallback when it does not.
  const correlated = unhealthy >= 2 && total > 0 &&
    ((meta && meta.correlated) || unhealthy / total >= 0.4);

  return {
    key: hk,
    host: host,
    hubPath: hubPath,
    hubParam: hubParamOf(first),
    model: meta && meta.model ? meta.model : null,
    vbus: !!(meta && meta.vbus),
    count: devices.length,
    total: total,
    unhealthy: unhealthy,
    since: since,
    correlated: correlated,
    severe: correlated && unhealthy / total >= 0.75,
    devices: devices
  };
}

/* ------------------------------------------------------------------ *
 * Keys and signatures
 * ------------------------------------------------------------------ */

/* deviceKey: the device id and nothing else — see the two rules above.
 *
 * A row the API returned without an id cannot be reconciled at all, so it is
 * keyed by its position on its hub instead and repaints the way the whole grid
 * used to. That is the honest fallback: the alternatives are dropping it, or
 * letting every id-less row on the hub share one tile. */
function deviceKey(d, index) {
  const id = d.id;
  return id === undefined || id === null || id === '' ? 'pos:' + index : 'id:' + String(id);
}

/* deviceSig covers EXACTLY the fields deviceTile renders. It is a contract
 * between two functions in one file, and both halves have to be edited
 * together: a field the tile shows and this omits freezes on screen, and a
 * field here that the tile does not show repaints for nothing.
 *
 * TestTheTileSignatureCoversEveryFieldTheTileRenders in internal/ui reads both
 * function bodies and fails when they disagree, because "remember to update
 * the other one" is not a mechanism. */
function deviceSig(d) {
  return signature([
    d.rackSlot, d.usbPath, d.id,
    d.manufacturer, d.model, d.android, d.serial,
    d.health,
    d.leaseState, d.protected, d.fence,
    d.quarantineID, d.quarantineReason,
    d.serialAmbiguous, d.adminState,
    d.battery, d.adbState
  ]);
}

function hostKey(h) { return String(h.host); }

function hostSig(h) {
  return signature([h.host, h.adminState, h.count, h.unhealthy, h.live, h.seen]);
}

function hubKey(h) { return String(h.key); }

function hubSig(h) {
  return signature([h.hubPath, h.model, h.count, h.vbus, h.host,
    h.correlated, h.severe, h.unhealthy, h.total,
    // The rendered forms, for the same reason the host's "seen" is: these are
    // clocks, and a signature over the timestamp would stop them.
    h.since ? fmtClock(h.since) : null,
    h.since ? fmtAbs(h.since) : null,
    h.since ? fmtRel(h.since) : null]);
}

/* ------------------------------------------------------------------ *
 * Render
 * ------------------------------------------------------------------ */

/* The not-yet / failed / nothing-here panel, kept as a sibling of the host
 * blocks. It is tracked in a variable rather than found by class because the
 * one thing this file must never do again is decide what the fleet body
 * contains by replacing all of it. */
let fleetNotice = null;

function fleetNoticeTo(body, node) {
  if (!node) {
    if (fleetNotice && fleetNotice.isConnected) fleetNotice.remove();
    fleetNotice = null;
    return;
  }
  // Same words as last time: leave the node alone, so a polite live region is
  // not told "Loading from the API…" once per event.
  if (fleetNotice && fleetNotice.isConnected && fleetNotice.textContent === node.textContent) return;
  if (fleetNotice && fleetNotice.isConnected) fleetNotice.remove();
  fleetNotice = node;
  body.append(node);
}

/* The state of the pass currently running.
 *
 * A patch function is called whether or not the pass is frozen, because
 * content must stay live either way — but one of them, patchCorrelation, has a
 * structural half: the correlation box appears and disappears, and it carries
 * a button. It reads its permission from here rather than from a parameter
 * threaded through reconcile, which would put "are we frozen" in the signature
 * of every patch on the page for the sake of the one that needs it. */
const fleetPass = { frozen: false, held: 0 };

function renderFleet() {
  const body = $('#fleet-body');
  const alerts = $('#fleet-alerts');
  body.setAttribute('aria-busy', state.loading.fleet ? 'true' : 'false');
  watchFleetInteraction();

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

  /* The guard never holds a grid back behind a panel that says the grid could
   * not be loaded: those two states contradict each other on screen, and the
   * only way to reach one with tiles still drawn is a farm that has just gone
   * from rows to none. Let that one through. */
  const frozen = !problem && fleetHeldBack();
  fleetReleaseOnce = false;
  fleetPass.frozen = frozen;
  fleetPass.held = 0;

  const hosts = problem ? [] : fleetGroups(rows);

  fleetNoticeTo(body, problem || (rows.length ? null : emptyState(
    'No devices match these filters.',
    'The fleet has ' + (all ? all.length : 0) + ' devices. Clear the filters to see them.')));

  let held = reconcile(body, hosts, hostKey, hostSig, createHostBlock, patchHostBlock, { frozen }).held;

  for (const h of hosts) {
    const block = keyedChild(body, hostKey(h));
    if (!block) continue;                       // held back; its hubs are not drawn yet
    held += reconcile(block, h.hubs, hubKey, hubSig, createHubBlock, patchHubBlock, { frozen }).held;
    for (const hub of h.hubs) {
      const hubBlock = keyedChild(block, hubKey(hub));
      if (!hubBlock) continue;
      held += reconcile($('.grid', hubBlock), hub.devices, deviceKey, deviceSig,
        deviceTile, patchTile, { frozen }).held;
    }
  }

  held += fleetPass.held;

  /* The sentence counts devices, so it is only raised when the difference can
   * be stated in devices. A held-back correlation box is a real deferral and
   * is counted above, but "0 devices changed" would be a worse thing to say
   * than nothing; the guard lets go on its own within FLEET_HOLD_MS. */
  announceHeld(held ? heldDeviceCount(hosts) : 0);

  // Alerts inside the view are rendered content; the announcement goes to the
  // aria-live banner region once per new correlation, not on every repaint.
  alerts.replaceChildren();
}

/* ------------------------------------------------------------------ *
 * The host block
 *
 * A head and its hub blocks. The head's volatile half lives in .facts and is
 * rewritten wholesale; the button beside it is never detached, because
 * detaching a focused button blurs it and this head is repainted whenever a
 * device on the host changes health.
 * ------------------------------------------------------------------ */

function createHostBlock(item) {
  const block = el('div', { class: 'host-block' },
    el('div', { class: 'host-head' },
      el('span', { class: 'facts' }),
      el('span', { class: 'host-actions' },
        el('button', {
          class: 'mini',
          onclick: () => {
            const h = subjectOf(block, item);
            if (h.host === UNASSIGNED_HOST) return;
            if (h.adminState === 'draining' || h.adminState === 'disabled') undrainHost(h.host, h.rows);
            else drainHost(h.host, h.rows);
          }
        }))));
  patchHostBlock(block, item);
  return block;
}

function patchHostBlock(block, item) {
  const head = $('.host-head', block);
  const facts = $('.facts', head);
  facts.replaceChildren();
  append(facts, [
    el('span', { class: 'host-name' }, item.host),
    item.adminState !== 'enabled'
      ? el('span', { class: 'chip chip-drain', title: 'host admin_state' },
        el('span', { 'aria-hidden': 'true' }, '⏸'), item.adminState)
      : el('span', { class: 'chip chip-ok', title: 'host admin_state' },
        el('span', { 'aria-hidden': 'true' }, '✓'), 'enabled'),
    el('span', { class: 'count' }, t('fleet.devices') + ' ', el('b', null, String(item.count))),
    el('span', { class: 'count' }, t('fleet.unhealthy') + ' ', el('b', null, String(item.unhealthy))),
    el('span', { class: 'count' }, t('fleet.liveLeases') + ' ', el('b', null, String(item.live))),
    item.seen ? el('span', { class: 'count' }, t('fleet.seen') + ' ', el('b', null, item.seen)) : null
  ]);

  const drained = item.adminState === 'draining' || item.adminState === 'disabled';
  const btn = $('.host-actions button', head);
  btn.textContent = drained ? t('fleet.undrain') : t('fleet.drain');
  /* There is no host behind the unassigned bucket, so there is nothing to
   * drain: POST hosts/(unassigned host)/drain would take a confirmation from
   * an operator and answer it with a 404. Buttons on this page do not exist to
   * be refused. Nothing here sets display on a button, so [hidden] alone is
   * enough — see the note in fleet.css for when it is not. */
  btn.hidden = item.host === UNASSIGNED_HOST;
}

/* ------------------------------------------------------------------ *
 * The hub block
 * ------------------------------------------------------------------ */

/* The correlation box is built once and hidden, never inserted and removed.
 * It carries a button, and a button that leaves the document takes the focus
 * on it with it — which is the defect this whole file is about. */
function createHubBlock(item) {
  const block = el('div', { class: 'hub-block' },
    el('div', { class: 'hub-head' }),
    el('div', { class: 'correlate', hidden: true },
      el('span', { class: 'facts' }),
      el('button', {
        class: 'mini ghost',
        onclick: () => {
          const h = subjectOf(block, item);
          /* Narrowing to one hub IS a structural change, and it is the one the
           * operator just asked for. Without this release the click focuses
           * this button, the guard sees focus inside the grid, and the answer
           * to "show me only this hub" is the same grid plus a banner saying
           * some devices changed. */
          fleetReleaseOnce = true;
          setFilters({ host: h.host === UNASSIGNED_HOST ? '' : h.host, hub: h.hubParam });
        }
      }, 'Focus this hub')),
    el('div', { class: 'grid' }));
  patchHubBlock(block, item);
  return block;
}

function patchHubBlock(block, item) {
  const head = $('.hub-head', block);
  head.replaceChildren();
  append(head, [
    el('span', { class: 'hub-name' }, item.hubPath ? 'hub ' + item.hubPath : 'no hub recorded'),
    item.model ? el('span', null, item.model) : null,
    el('span', null, item.count + (item.count === 1 ? ' device' : ' devices')),
    item.vbus
      ? el('span', { class: 'chip chip-plain', title: 'this hub can switch VBUS per port' }, 'switchable')
      : null
  ]);
  return patchCorrelation(block, item);
}

/* The correlation banner: several devices failing on one hub is one hub fault,
 * not several phone faults. Saying so out loud is the difference between an
 * operator replacing five phones and an operator replacing one hub.
 *
 * No top-level banner is raised from here. Hub correlation is also computed
 * server-side from farm.v_hub_health and arrives on the alert stream, which
 * carries the hub id and can offer a jump action. Raising it here as well
 * produced two banners per failing hub, worded slightly differently, which is
 * precisely how a real correlated failure gets lost in its own noise. The
 * in-context box above the affected devices is the better placement and stays.
 *
 * Showing or hiding the box is structural — it carries a button, and a hidden
 * element cannot hold focus — so a frozen pass defers it and returns false,
 * which leaves the hub's signature unstamped so the next pass tries again. Its
 * WORDS are never deferred: a box that is on screen says what is true now.
 */
function patchCorrelation(block, item) {
  const box = $('.correlate', block);
  const show = !!item.correlated;
  if (box.hidden === show) {
    if (fleetPass.frozen) { fleetPass.held += 1; return false; }
    box.hidden = !show;
  }
  if (!show) return true;

  box.className = 'correlate' + (item.severe ? ' correlate-bad' : '');

  const line = item.unhealthy + ' of ' + item.total + ' devices on hub ' +
    (item.hubPath || '(unknown)') + ' unhealthy' +
    (item.since ? ' since ' + fmtClock(item.since) : '') + ' — suspect the hub, not the phones.';

  const facts = $('.facts', box);
  facts.replaceChildren();
  append(facts, [
    el('span', { 'aria-hidden': 'true' }, '▲'),
    el('span', null, line,
      el('span', { class: 'c-sub' },
        'Blast radius is this hub, on host ' + item.host +
        '. Leases on these devices are untouched and their clocks keep running.'),
      el('span', { class: 'c-sub' },
        item.since
          ? 'Worst health_since: ' + fmtAbs(item.since) + ' (' + fmtRel(item.since) + ').'
          : 'The API reported no health_since for these devices.'))
  ]);
  return true;
}

/* ------------------------------------------------------------------ *
 * The device tile
 * ------------------------------------------------------------------ */

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
    // subjectOf, not d: this element is kept across renders, so the row it was
    // first built from is the one row it must not open.
    onclick: () => openDevice(subjectOf(tile, d))
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

/* patchTile rewrites a tile from the tile builder itself.
 *
 * Deliberately not a set of targeted edits. Working out which span holds the
 * changed field would be a second, silent copy of deviceTile's layout, and the
 * copy is the one that goes wrong — quietly, on one field, showing an old
 * value beside five fresh ones. Building the tile again and moving its
 * children across costs a few microseconds and cannot drift.
 *
 * The <button> itself is what survives, and it is the whole point: focus,
 * hover, :active, an in-flight click and any automation reference are all
 * properties of that element. Its children are not focusable. */
function patchTile(node, d) {
  const built = deviceTile(d);
  syncAttributes(node, built);
  node.replaceChildren(...built.childNodes);
}

/* ------------------------------------------------------------------ *
 * Filters
 * ------------------------------------------------------------------ */

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
