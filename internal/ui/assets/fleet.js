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

/* fleetNarrowedOnServer answers the one question an empty API response turns
 * on: did we ASK for a narrower farm than the whole of it?
 *
 * It deliberately lists what loaders.fleet() sends — host, hub, health, pool
 * and the search box — and deliberately omits `lease`, which is applied here in
 * the browser and never reaches /fleet. A lease filter therefore cannot be the
 * reason the server answered with nothing, and blaming it would tell an
 * operator with an empty rack that filters are hiding devices that do not
 * exist: the "Clear the filters" button would refetch, change nothing visible,
 * and hand them the conclusion that the dashboard is broken.
 *
 * The search box does count. It is not in the filter bar and does not look like
 * a filter, but it goes to the server as ?q= and empties the grid exactly as a
 * host filter does — and a Clear button that left it set would also appear to
 * do nothing. The client-side list is in fleetRows(); these two are different
 * lists on purpose, and each says which. */
function fleetNarrowedOnServer() {
  const f = state.filters;
  return !!(f.host || f.hub || f.health || f.pool || state.q.trim());
}

/* clearFleetFilters is what the Clear button does — literally: wire() binds
 * #f-clear to this function, so the toolbar button and the button inside an
 * empty grid cannot drift apart. Through setFilters() rather than five separate
 * calls, for the reason setFilters() documents: one fleet request, not five
 * racing ones. */
function clearFleetFilters() {
  state.q = '';
  /* Clearing the filters is a structural change to the grid and it is the one
   * the operator just asked for. Without this release the button — which lives
   * INSIDE #fleet-body, in the empty panel — takes focus, the interaction guard
   * sees focus in the grid, and the answer to "show me everything again" is the
   * same empty grid plus a banner saying some devices changed. */
  fleetReleaseOnce = true;
  setFilters({ host: '', hub: '', health: '', pool: '', lease: '' });
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
 *
 * It is generic on purpose. The card grid keys host blocks, hub blocks and
 * tiles with it; the table keys its <tr> rows with it. One diff, one set of
 * rules, one place to get right.
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

/* clearKeyed empties a container AND forgets what it owned.
 *
 * Emptying alone would leave the key table pointing at detached nodes, which
 * the next pass would silently re-attach — the hidden pane's five hundred rows
 * coming back from the dead the moment somebody switched modes. The pane
 * elements themselves are never replaced; only their contents go. */
function clearKeyed(container) {
  if (!container.firstChild && !keyedChildren.has(container)) return;
  keyedChildren.delete(container);
  container.replaceChildren();
}

/* syncAttributes copies every attribute the freshly built node carries onto
 * the node being kept.
 *
 * Every attribute rather than a named list of the ones a tile carries today:
 * four units edit this file, and a tile that grows an attribute must not
 * become a tile whose one new attribute is silently never updated. That is how
 * the aria-label unit 3 builds from the same pieces as the visible card stays
 * in step with the card. data-key and data-sig belong to the reconciler and
 * are never on the fresh node. */
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
 * whose whole purpose IS a structural change — narrowing to one hub, sorting a
 * column, clearing the filters. None of those is something to protect an
 * operator from; each is what they just asked for, and deferring it would
 * answer the click with the same grid plus a banner. */
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
let heldDismissed = false;

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
 * full strip is most of how the two are told apart — a dismissal leaves room
 * behind it, an eviction does not — but only at the instant of dismissal, so
 * an actual dismissal is remembered as well. See below for what that costs
 * when it is left to the room test alone. */
function announceHeld(n) {
  /* A modal <dialog> makes the rest of the page inert: while the device sheet
   * is open the banner strip cannot be read by a screen reader and the control
   * it offers cannot be pressed. So nothing is said — the grid behind the sheet
   * is not being read either — and the first render after it closes says it if
   * it is still true. */
  const dlg = $('#dlg-device');
  if (dlg && dlg.open) return;

  const strip = $('#banners');
  const evicted = heldNote && !heldNote.isConnected && !heldDismissed &&
    strip && strip.children.length >= 5;
  if (heldNoteCount === n && !evicted) return;
  heldNoteCount = n;
  if (!n) { clearBanner('fleet-held'); heldNote = null; heldDismissed = false; return; }
  heldDismissed = false;
  heldNote = banner('warn', t('fleet.held', { n: String(n) }), {
    key: 'fleet-held',
    action: { label: t('fleet.heldApply'), run: applyHeldFleetChanges }
  });
  /* A click anywhere in this banner is the operator dealing with it — the
   * control that applies the change, or the × beside it. Either way it must not
   * come back on its own, and the room test alone cannot tell: an alert burst
   * refills the strip within seconds, at which point a DISMISSED notice also
   * satisfies "no room behind it" and is re-raised, render after render, each
   * new copy evicted by the next alert and put back by the one after. That is
   * the live-region churn this whole function exists to prevent, arrived at
   * from the other side and at the moment the strip is busiest. */
  if (!heldNote.dataset.heldWired) {
    heldNote.dataset.heldWired = '1';
    heldNote.addEventListener('click', () => { heldDismissed = true; });
  }
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
  const first = $('.tile, .rowlink', $('#fleet-body'));
  if (first) first.focus();
}

/* heldDeviceCount counts, in devices, the difference between what is drawn and
 * what the data says — which is what the guard is holding back. It reads the
 * DOM rather than the reconcilers' own tallies because "devices" is the unit
 * the sentence is written in, and a held-back host block is several of them.
 *
 * `want` is the keys the render asked for, in order; `drawn` is the nodes that
 * actually carry a device — tiles in the card grid, rows in the table. Both
 * modes key on the same deviceKey, so one function answers for both.
 *
 * When the two sets agree, the difference is order: count the positions that
 * do not match, so a device that would move is still news. */
function heldDeviceCount(want, drawn) {
  const have = drawn.map((n) => n.dataset.key || '');
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

/* ------------------------------------------------------------------ *
 * Grouping: the rows, arranged as they are drawn
 * ------------------------------------------------------------------ */

/* fleetGroups turns the filtered rows into the host → hub → device tree the
 * card grid draws, with every number each level shows computed once.
 * Separating it from the drawing is what lets a signature be compared without
 * building a node to compare it against. */
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

/* deviceKey: the device id and nothing else — see the two rules above. It is
 * the key in BOTH modes, because a tile and a row stand for the same handset
 * and switching modes must not renumber the farm.
 *
 * A row the API returned without an id cannot be reconciled at all, so it is
 * keyed by its position instead and repaints the way the whole grid used to.
 * That is the honest fallback: the alternatives are dropping it, or letting
 * every id-less row in the container share one node. */
function deviceKey(d, index) {
  const id = d.id;
  return id === undefined || id === null || id === '' ? 'pos:' + index : 'id:' + String(id);
}

/* deviceSig covers EXACTLY the fields deviceTile renders. It is a contract
 * between two functions in one file, and both halves have to be edited
 * together: a field the tile shows and this omits freezes on screen, and a
 * field here that the tile does not show repaints for nothing.
 *
 * The list is not the same one this file shipped with. Unit 3's two-chip
 * budget took `fence` and the raw `adb_state` off the tile face — both moved
 * to the device sheet — and put `holder` on it, in the one sentence under the
 * chips that says who to go and ask. Signing a field the tile no longer draws
 * would repaint fifty-six tiles for a lease fence nobody can see; not signing
 * `holder` would freeze a name on screen after the lease moved.
 *
 * TestTheTileSignatureCoversEveryFieldTheTileRenders in internal/ui derives
 * this list from deviceTile itself — following the helpers the tile hands the
 * whole row to — and fails in both directions, because "remember to update the
 * other one" is not a mechanism. */
function deviceSig(d) {
  return signature([
    d.rackSlot, d.usbPath, d.id,
    d.manufacturer, d.model, d.android, d.serial,
    d.health,
    d.leaseState, d.protected, d.holder,
    d.quarantineID, d.quarantineReason,
    d.serialAmbiguous, d.adminState,
    d.battery
  ]);
}

/* The table row shows a different set: no battery percentage in words, no
 * holder, but the host, the hub path, the host's admin_state — a drained host
 * takes every device on it out of the pool and a table has no host header to
 * say so — and a last-seen clock.
 *
 * lastSeen is signed as BOTH the timestamp and the rendered distance. The cell
 * prints "4m ago", which goes stale with no change to the data behind it, so a
 * signature over the timestamp alone would stop that clock the moment the row
 * was first drawn.
 *
 * TestTheTableRowSignatureCoversEveryFieldTheRowRenders holds this list to the
 * columns the same way. */
function fleetRowSig(d) {
  return signature([
    d.rackSlot, d.usbPath, d.id,
    d.manufacturer, d.model, d.android, d.serial, d.serialAmbiguous,
    d.host, d.hubPath,
    d.health, d.quarantineID, d.quarantineReason,
    d.leaseState, d.protected, d.adminState, d.hostAdminState,
    d.battery,
    d.lastSeen, fmtRel(d.lastSeen)
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

/* The not-yet / failed / nothing-here panel, kept as a sibling of the two
 * panes. It is tracked in a variable rather than found by class because the
 * one thing this file must never do again is decide what the fleet body
 * contains by replacing all of it.
 *
 * It is a sibling of BOTH panes and not a child of either, because
 * nothingToShow() is one decision for both modes — "this farm has no devices"
 * and "the filters hid all of them" have opposite next actions — and one
 * decision drawn in one place cannot drift into two. */
let fleetNotice = null;

function fleetNoticeTo(body, node) {
  if (!node) {
    if (fleetNotice && fleetNotice.isConnected) fleetNotice.remove();
    fleetNotice = null;
    return;
  }
  // Same words as last time: leave the node alone, so a polite live region is
  // not told "Loading from the API…" once per event, and a <details> somebody
  // just opened is not re-collapsed under them.
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

/* fleetDrawn: the nodes currently on screen that stand for one device, in
 * whichever mode is being drawn — tiles in the card grid, rows in the table.
 * Two decisions read it and they must read the same thing: whether there is
 * anything for the interaction guard to protect, and whether the grid has
 * anything on it at all. */
function fleetDrawn(mode, boxes) {
  return mode === 'table' ? $$('tbody tr', boxes.table) : $$('.tile', boxes.cards);
}

function renderFleet() {
  const body = $('#fleet-body');
  const alerts = $('#fleet-alerts');
  body.setAttribute('aria-busy', state.loading.fleet ? 'true' : 'false');
  watchFleetInteraction();

  const all = state.data.fleet;
  const rows = fleetRows();

  // The five server-computed numbers live in the summary strip above the grid,
  // where they are large enough to read from a doorway and each one can be
  // clicked. countChips() rendered the same six values here as six grey pills
  // in the corner of a toolbar; keeping both would put the same arithmetic on
  // the page twice, differently worded, which is how two numbers that must
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

  /* Cards or table, decided and painted on the switch BEFORE anything below
   * can return early. The loading and the failed states own the whole of
   * #fleet-body, and they are the states the page spends its first seconds in
   * — a switch wired only on the happy path is a switch that does nothing
   * until the first response lands, and nothing at all on a farm whose fleet
   * endpoint is refusing. */
  const mode = fleetMode(all);
  syncFleetModeControl(mode);

  /* Both panes always exist and are never replaced — they are the containers
   * the reconcilers key their children on, and a container that is swapped out
   * takes every kept node with it. Exactly one is visible, and only the
   * visible one is ever filled: a hidden table of five hundred rows is five
   * hundred rows of work nobody asked for, on a view that repaints under a
   * live event stream. */
  const boxes = fleetModeBoxes(body);

  /* A farm with no devices and a filter that hides every device read
   * identically — "No devices match" — and they are not the same situation at
   * all. The first is answered by a person walking to the rack; the second by
   * one button. Telling a new operator that nothing matches, when nothing has
   * ever been registered, sends them looking for a filter that is not set. */
  const problem = panelState('fleet', all, fleetNarrowedOnServer()
    ? emptyState(t('empty.fleet.filtered'),
      [t('empty.fleet.filteredDetail'), provenance(t('empty.fleet.where'))],
      emptyAction(t('empty.clearFilters'), () => clearFleetFilters()))
    : emptyState(t('empty.fleet.none'),
      [t('empty.fleet.noneDetail'), provenance(t('empty.fleet.where'))],
      emptyAction(t('empty.openDocs'), () => setView('docs'))));

  /* The guard never holds a grid back behind a panel that says the grid could
   * not be loaded: those two states contradict each other on screen, and the
   * only way to reach one with tiles still drawn is a farm that has just gone
   * from rows to none. Let that one through.
   *
   * And NOTHING DRAWN IS NOTHING TO PROTECT. A frozen pass defers creation as
   * well as removal, so freezing a pass that is drawing into an empty pane
   * produces an empty pane — and the page then prints "No devices match" over a
   * farm with fifty-six devices in it. That is reachable without anything
   * unusual: the fleet fetch fails, the loading panel takes the whole body, the
   * operator tabs to its Retry button and presses it, and the good response
   * lands with focus still :focus-visible inside #fleet-body. A mode flip
   * reaches it too — fleetMode re-derives cards-or-table from the farm's size
   * on every unfiltered response, so a farm hovering either side of forty can
   * change panes with nobody touching anything. */
  const frozen = !problem && fleetDrawn(mode, boxes).length > 0 && fleetHeldBack();
  fleetReleaseOnce = false;
  fleetPass.frozen = frozen;
  fleetPass.held = 0;

  if (problem) {
    clearKeyed(boxes.cards);
    clearKeyed(boxes.table);
    boxes.cards.hidden = true;
    boxes.table.hidden = true;
    fleetNoticeTo(body, problem);
    announceHeld(0);
    alerts.replaceChildren();
    return;
  }

  // The keys this render asked for, in drawing order, so the held-back count
  // below can be stated in devices rather than in DOM operations.
  const want = [];
  let held = 0;

  if (mode === 'table') {
    clearKeyed(boxes.cards);
    held += fleetTableInto(boxes.table, rows, want);
    /* The in-context hub-correlation box below is not drawn here, and it is
     * worth being exact about what that costs. The box belongs beside the hub
     * it accuses, and a table sorted by battery has no hub to stand beside.
     * The correlation the SERVER found still reaches the reader: it comes off
     * the alert stream with the hub id on it and fills the banner region at
     * the top of the page, which is above this table too.
     *
     * What is not reproduced is the client-side fallback — the ratio test that
     * fires when the server did not flag the hub itself. In table mode that
     * hub is visible only as several unhealthy rows sharing a hub path, which
     * is a thing an operator can sort by and not a thing the page says out
     * loud. That is a real gap and it is written down here rather than papered
     * over. */
  } else {
    clearKeyed(boxes.table);
    held += renderFleetCards(boxes.cards, fleetGroups(rows), frozen, want);
  }

  held += fleetPass.held;

  /* What is actually on screen decides which pane is shown and whether the
   * "nothing to show" panel is drawn — not `rows.length`. A frozen pass keeps
   * tiles the data no longer contains, and a grid full of tiles under a panel
   * saying no devices match is the page contradicting itself. */
  const drawn = fleetDrawn(mode, boxes);
  boxes.cards.hidden = mode !== 'cards' || !drawn.length;
  boxes.table.hidden = mode !== 'table' || !drawn.length;
  fleetNoticeTo(body, drawn.length ? null : nothingToShow(all));

  /* The sentence counts devices, so it is only raised when the difference can
   * be stated in devices. A held-back correlation box is a real deferral and
   * is counted above, but "0 devices changed" would be a worse thing to say
   * than nothing; the guard lets go on its own within FLEET_HOLD_MS. */
  announceHeld(held ? heldDeviceCount(want, drawn) : 0);

  // Alerts inside the view are rendered content; the announcement goes to the
  // aria-live banner region once per new correlation, not on every repaint.
  alerts.replaceChildren();
}

/* renderFleetCards reconciles the host → hub → tile tree into the cards pane
 * and returns how much a frozen pass held back. */
function renderFleetCards(pane, hosts, frozen, want) {
  let held = reconcile(pane, hosts, hostKey, hostSig, createHostBlock, patchHostBlock, { frozen }).held;

  for (const h of hosts) {
    /* Every device this render asked for, counted BEFORE the two guards below.
     * A host block a frozen pass declined to create is the case where saying
     * nothing is worst — a host registering with twenty-four devices on it is
     * twenty-four devices the operator cannot see — and counting these after
     * the `continue` made the want set match the DOM exactly, which reads as
     * "nothing is being held back" and silently cleared the banner.
     *
     * The index is the device's position ON ITS HUB, because that is the index
     * the grid's own reconciler keys an id-less row by; a running total across
     * hubs would compare two different keys for one tile. */
    for (const hub of h.hubs) hub.devices.forEach((d, i) => want.push(deviceKey(d, i)));

    const block = keyedChild(pane, hostKey(h));
    if (!block) continue;                       // held back; its hubs are not drawn yet
    held += reconcile(block, h.hubs, hubKey, hubSig, createHubBlock, patchHubBlock, { frozen }).held;
    for (const hub of h.hubs) {
      const hubBlock = keyedChild(block, hubKey(hub));
      if (!hubBlock) continue;
      held += reconcile($('.grid', hubBlock), hub.devices, deviceKey, deviceSig,
        deviceTile, patchTile, { frozen }).held;
    }
  }
  return held;
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
   * be refused. Nothing sets display on a button, so [hidden] alone is enough
   * here — see the note in fleet.css for when it is not. */
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
 * ONLY ONE OF THE FOUR CASES IS EVER DEFERRED, and getting that wrong was
 * worse than not deferring at all. The first version deferred every change of
 * visibility and returned before writing the box's words, which meant a hub
 * that went bad while somebody was working in the grid raised no alarm at all —
 * silently, because a deferral counted in boxes cannot be announced by a
 * sentence written in devices — and a hub that RECOVERED kept its accusation on
 * screen. So:
 *
 *   - appearing is never deferred. It moves the tiles below it down by one box,
 *     which is exactly the thing the guard exists to prevent, and it is still
 *     the right trade: the box is an alarm about those same tiles, and an alarm
 *     nobody is shown is not a smaller problem than a grid that shifted.
 *   - disappearing is deferred ONLY while the focus is inside the box, which is
 *     the whole reason any of this is structural: hiding an element blurs
 *     whatever it contains. That case is one operator with their hand on this
 *     hub's own button at the moment the hub recovers; they can see the box
 *     they are standing on, so nothing needs announcing, and the next pass
 *     hides it as soon as they move.
 *   - the WORDS are rewritten on every pass a correlated box is drawn on, so a
 *     box that is up and staying up says what is true now. The one box that is
 *     up and on its way down keeps its last sentence rather than being handed
 *     the numbers of a hub that has recovered.
 */
function patchCorrelation(block, item) {
  const box = $('.correlate', block);
  const show = !!item.correlated;
  let deferred = false;
  if (box.hidden === show) {
    if (!show && fleetPass.frozen && box.contains(document.activeElement)) {
      fleetPass.held += 1;
      deferred = true;
    } else {
      box.hidden = !show;
    }
  }
  /* A box left on screen by the one deferral above keeps the sentence it was
   * last given, which was true when it was written. It is NOT rewritten from
   * this item: the hub is no longer correlated, so "1 of 7 devices on hub 3-1
   * unhealthy — suspect the hub, not the phones" would be an accusation the
   * data has already withdrawn. Returning false leaves the hub's signature
   * unstamped, so the next pass hides it. */
  if (box.hidden || !show) return !deferred;

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

  /* subjectOf, not d. This element is kept across renders — that is the whole
   * point of the reconciler — so the row it was first built from is the one row
   * it must never open. Two devices can render identically and carry different
   * ids into the sheet. */
  const tile = el('button', {
    type: 'button',
    class: 'tile h-' + cond,
    'aria-label': name,
    onclick: () => openDevice(subjectOf(tile, d))
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
 * syncAttributes copies EVERY attribute rather than a named list, which is what
 * keeps the aria-label in step: unit 3 built it from the same pieces as the
 * visible card so the two cannot say different things, and a patch that
 * refreshed the card while leaving the label alone would break exactly that.
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
 * THE FLEET AS A TABLE
 *
 * A card grid is a map of a rack you can walk. Forty tiles is a wall of
 * handsets an operator can point at, and where a tile sits on screen is where
 * a phone sits in the room. Past a certain size that stops being true: the
 * grid wraps into a shape with no relation to the room, and the map becomes a
 * wall. A table does not pretend to be a map. It has one row per device, it
 * sorts, and it is what a business reads.
 *
 * Neither mode is the real one. The grid is how you find a device you can see;
 * the table is how you find a device among five hundred you cannot.
 * ------------------------------------------------------------------ */

const FLEET_MODES = ['cards', 'table'];

/* Where the grid stops being a map. Forty is roughly where a comfortable-
 * density grid stops fitting on one screen — and it is only a DEFAULT: the
 * moment a reader picks a mode, their choice is what fleetMode returns, at
 * every size, forever. */
const FLEET_TABLE_ABOVE = 40;

/* The choice lives in localStorage, per browser, for the same reason the
 * language and the density do (see i18n.js): it is a reading preference and
 * not a property of the farm. Two operators sharing one control plane can want
 * different modes and neither may change the other's.
 *
 * It is deliberately NOT in the URL hash either, which is the same argument
 * one step further out: buildHash() makes a link somebody pastes into a
 * ticket, and a pasted link that silently re-modes a colleague's dashboard is
 * exactly the thing storing this per browser is meant to prevent. The hash
 * carries what the OTHER reader needs — the filters and the search — and
 * nothing about how this one likes to read. */
const FLEET_MODE_KEY = 'device-farmer.fleetMode';

/* null means "never chosen", which is not the same as either mode: it is what
 * lets the size rule below decide, and what a stored choice replaces. */
let fleetModeChoice = readFleetMode();

function readFleetMode() {
  try {
    const saved = localStorage.getItem(FLEET_MODE_KEY);
    if (FLEET_MODES.includes(saved)) return saved;
  } catch (_) { /* private mode: no stored choice, so size decides */ }
  return null;
}

/* The size rule's answer, once something has been able to measure the farm.
 * null until then. */
let fleetModeDefault = null;

/* fleetIsFiltered: is the fleet the page is holding the whole farm, or a slice
 * of it? The host, hub, health, pool and search filters are all applied by the
 * SERVER — state.data.fleet is its answer, not the farm — so none of them may
 * be in effect when the size rule measures. */
function fleetIsFiltered() {
  const f = state.filters;
  return !!(state.q.trim() || f.host || f.hub || f.health || f.pool || f.lease);
}

/* fleetMode answers with the mode to draw: a stored choice if the reader has
 * made one, and otherwise the size rule.
 *
 * The size is re-measured only from an UNFILTERED answer, and this is the
 * whole reason the function is shaped this way. Measuring whatever the server
 * last returned meant that narrowing the health filter to the seven
 * quarantined devices threw an operator out of the table and into the card
 * grid, and clearing the filter threw them back. A default that moves while
 * somebody is working is not a default.
 *
 * `fleet` is null until the first response, which is why it is passed whole
 * rather than as a length: an empty farm is a farm with no devices in it, and
 * a farm nobody has heard from yet is not. */
function fleetMode(fleet) {
  if (fleetModeChoice) return fleetModeChoice;
  if (Array.isArray(fleet) && !fleetIsFiltered()) {
    fleetModeDefault = fleet.length > FLEET_TABLE_ABOVE ? 'table' : 'cards';
  }
  return fleetModeDefault || 'cards';
}

function setFleetMode(next) {
  if (!FLEET_MODES.includes(next) || next === fleetModeChoice) return;
  fleetModeChoice = next;
  try { localStorage.setItem(FLEET_MODE_KEY, next); } catch (_) { /* not fatal */ }
  render();
}

/* fleetModeBoxes returns the two containers, making them if they are not there.
 *
 * Made ONCE and never replaced. They used to be rebuilt whenever the API had
 * not answered yet or answered with nothing, because the loading and error
 * states owned the whole box — and that is now the one thing this file must
 * not do: these two elements are the containers the reconcilers key their
 * children on, so swapping either of them out throws away every kept tile and
 * every kept row along with it. The empty and failed states are a THIRD
 * sibling instead (see fleetNoticeTo), and the panes are emptied and hidden
 * under it rather than replaced. */
function fleetModeBoxes(body) {
  let cards = $('#fleet-cards', body);
  let tbl = $('#fleet-table', body);
  if (!cards) {
    cards = el('div', { id: 'fleet-cards', class: 'fleet-pane' });
    body.append(cards);
  }
  if (!tbl) {
    tbl = el('div', { id: 'fleet-table', class: 'fleet-pane' });
    body.append(tbl);
  }
  return { cards: cards, table: tbl };
}

/* syncFleetModeControl paints the toolbar's two-state switch, and wires it
 * once. The listener sits on the group rather than on the buttons so that it
 * survives every repaint of the view below it. */
function syncFleetModeControl(mode) {
  const group = $('#fleet-mode');
  if (!group) return;
  if (!group.dataset.wired) {
    group.dataset.wired = '1';
    group.addEventListener('click', (ev) => {
      const btn = ev.target.closest('button[data-mode]');
      if (btn) setFleetMode(btn.dataset.mode);
    });
  }
  for (const btn of $$('button[data-mode]', group)) {
    btn.setAttribute('aria-pressed', btn.dataset.mode === mode ? 'true' : 'false');
  }
}

/* ---------------------------- sorting ---------------------------- */

/* Which column the table is ordered by, and which way. It is not stored: the
 * mode is how somebody reads, a sort is what they are looking for right now,
 * and a sort that outlived the question it answered is a table that opens in
 * an order nobody asked for. */
let fleetSort = { key: 'slot', dir: 'asc' };

function setFleetSort(key) {
  fleetSort = fleetSort.key === key
    ? { key: key, dir: fleetSort.dir === 'asc' ? 'desc' : 'asc' }
    : { key: key, dir: 'asc' };
  /* Reordering the rows is a structural change, and the header button that
   * asked for it lives inside #fleet-body — so without this release the
   * interaction guard would see focus in the grid and answer a click on
   * "Battery" with the same order plus a banner. Sorting is the one thing a
   * reader presses a column header to get. */
  fleetReleaseOnce = true;
  render();
}

/* HEALTH_RANK orders the Condition column and does nothing else.
 *
 * Ascending is calmest first, so one click puts the devices that are fine at
 * the top and a second click puts the ones somebody has to walk to. This is a
 * reading order and not a severity the server would recognise: there is no
 * rank column in farm.v_fleet, and this number is never sent anywhere. The
 * only judgement in it is that the two states which mean somebody DECIDED a
 * handset is out of service — parked and retired — sit next to healthy rather
 * than among the faults, which is the same call NOT_A_FAULT makes in app.js. */
const HEALTH_RANK = {
  healthy: 0, parked: 1, retired: 2, booting: 3, recovering: 4,
  degraded: 5, unauthorized: 6, quarantined: 7, missing: 8, offline: 9, unknown: 10
};

function healthRank(h) {
  const r = HEALTH_RANK[h || 'unknown'];
  // A health value this page has never heard of sorts after every one it has.
  return r === undefined ? 99 : r;
}

/* free < held < protected < suspect, and then the same four again for a device
 * that is administratively out. Suspect goes last of the four because it is
 * the one an operator is hunting for: the control plane has not heard from the
 * holder, and — the axiom the Leases view states — the lease is not over.
 *
 * The +4 puts every device a scheduler could still be given above every device
 * it could not, which is the question this column is asked. A drained device
 * holds no lease and is not free either. */
function availRank(d) {
  const out = drainedState(d) ? 4 : 0;
  const st = d.leaseState;
  if (!st || st === 'released' || st === 'expired') return out;
  if (st === 'suspect') return out + 3;
  return out + (d.protected ? 2 : 1);
}

function deviceModelName(d) {
  return [d.manufacturer, d.model].filter(Boolean).join(' ');
}

/* The five columns whose order is a comparison between two rows. */
const FLEET_SORTS = {
  slot: (a, b) => cmp(a.rackSlot || a.usbPath, b.rackSlot || b.usbPath),
  model: (a, b) => cmp(deviceModelName(a), deviceModelName(b)) || cmp(a.android, b.android),
  where: (a, b) => cmp(a.host, b.host) || cmp(a.hubPath, b.hubPath) ||
    cmp(a.rackSlot || a.usbPath, b.rackSlot || b.usbPath),
  condition: (a, b) => healthRank(a.health) - healthRank(b.health) || cmp(a.health, b.health),
  availability: (a, b) => availRank(a) - availRank(b)
};

/* And the two whose order is a number the row either has or does not.
 *
 * They are separated because both problems live here. A comparison sort asks
 * each row for its value about log2(n) times, so Date.parse on five hundred
 * rows was twenty thousand parses per repaint on the one view that repaints
 * under a live event stream; computing the value once per row per render
 * removes all of it. And a value the API did not fill is not a small number
 * and not a large one, so it is null rather than NaN or zero — see fleetSorted
 * for what that buys. */
const FLEET_NUMBERS = {
  battery: (d) => {
    if (d.battery === null || d.battery === undefined) return null;
    const n = Number(d.battery);
    return Number.isNaN(n) ? null : n;
  },
  lastSeen: (d) => {
    const at = parseTime(d.lastSeen);
    return at ? at.getTime() : null;
  }
};

function fleetSorted(rows) {
  const dir = fleetSort.dir === 'desc' ? -1 : 1;
  const value = FLEET_NUMBERS[fleetSort.key];

  // slice(): fleetRows() hands back an array the render pipeline still owns.
  if (value) {
    const v = new Map(rows.map((d) => [d, value(d)]));
    return rows.slice().sort((a, b) => {
      const va = v.get(a), vb = v.get(b);
      /* A row with nothing to compare sinks to the bottom in BOTH directions.
       * "—" is not an answer to "which battery is lowest", and reversing the
       * order must not promote it to one. */
      if (va === null || vb === null) return va === vb ? 0 : va === null ? 1 : -1;
      return dir * (va - vb);
    });
  }

  const by = FLEET_SORTS[fleetSort.key] || FLEET_SORTS.slot;
  return rows.slice().sort((a, b) => dir * by(a, b));
}

/* ----------------------------- the table ----------------------------- */

/* fleetTable takes rows and returns one node. It re-filters nothing —
 * fleetRows() has already applied every filter and the header search — and it
 * holds no reference to anything outside itself, so the caller decides where
 * it goes and when it is replaced. */
/* nothingToShow is the panel BOTH modes draw when the filters hid everything.
 *
 * One function rather than one per mode, because the two answers it chooses
 * between are the reason this exists: a farm that has no devices and a farm
 * whose filters are hiding all of them read identically as "no devices match",
 * and they have opposite next actions. A second copy of that decision is a
 * second place for the two to drift back together.
 *
 * The empty state ABOVE this, on panelState, answers the case where the API
 * returned nothing at all. This one answers the case where it returned devices
 * and the client-side filters removed every one. */
function nothingToShow(all) {
  const n = all ? all.length : 0;
  if (!n) {
    return emptyState(t('empty.fleet.none'),
      [t('empty.fleet.noneDetail'), provenance(t('empty.fleet.where'))],
      emptyAction(t('empty.openDocs'), () => setView('docs')));
  }
  return emptyState(t('empty.fleet.filtered'),
    t('empty.fleet.hiddenDetail', { n: n }),
    emptyAction(t('empty.clearFilters'), () => clearFleetFilters()));
}

/* The seven columns, built per call rather than declared once, because every
 * label goes through t() and the language can change between two paints.
 *
 * Each cell is written as `(d) => cell(d)` rather than as the bare function
 * name, and that is not style. TestTheTableRowSignatureCoversEveryFieldTheRow-
 * Renders derives the row's field list by following the helpers a row is handed
 * whole to, and a bare `cell: slotCell` is a reference this file passes along
 * rather than a call it makes — invisible to that walk, and therefore a column
 * whose fields nobody would check against fleetRowSig. */
function fleetColumns() {
  return [
    { label: t('fleet.col.slot'), sort: 'slot', cls: 'f-slot', cell: (d) => slotCell(d) },
    { label: t('fleet.col.model'), sort: 'model', cls: 'f-model', cell: (d) => modelCell(d) },
    { label: t('fleet.col.where'), sort: 'where', cls: 'f-where', cell: (d) => whereCell(d) },
    { label: t('fleet.col.condition'), sort: 'condition', cls: 'f-cond', cell: (d) => conditionCell(d) },
    { label: t('fleet.col.availability'), sort: 'availability', cls: 'f-avail', cell: (d) => availabilityCell(d) },
    { label: t('fleet.col.battery'), sort: 'battery', cls: 'f-batt', cell: (d) => batteryEl(d.battery) },
    { label: t('fleet.col.lastSeen'), sort: 'lastSeen', cls: 'f-seen', cell: (d) => timeCell(d.lastSeen) }
  ];
}

function fleetTable(rows) {
  /* .tscroll is not optional and never has been: html and body clip sideways
   * overflow, so a wide thing without its own scroller is a wide thing nobody
   * can reach. The seven columns above are chosen to fit a 1280px laptop
   * without using it — see fleet.css, which relaxes the 900px floor that the
   * global table rule sets for the denser views. */
  return el('div', { class: 'tscroll fleet-tbl' },
    table(fleetColumns(), fleetSorted(rows), {
      // subjectOf, not the row this <tr> was built from: rows are kept across
      // renders here exactly as tiles are, so the object a handler closes over
      // is a snapshot and the element outlives it.
      onRowClick: (d, ev) => openDevice(subjectOf(ev.currentTarget, d)),
      sortKey: fleetSort.key,
      sortDir: fleetSort.dir,
      onSort: setFleetSort
    }));
}

/* ------------------- the table, reconciled -------------------- *
 *
 * The grid keeps its tiles and the table keeps its rows, for the same reasons
 * and through the same diff. A table is where a five-hundred-device farm is
 * actually read, so it is the mode where a rebuild costs most: five hundred
 * <tr> elements thrown away twice a second, a sort header that loses focus
 * mid-press, and a row that goes stale between the read and the click.
 *
 * Two layers. The SHELL — the scroller, the header row and an empty tbody — is
 * built by fleetTable itself and rebuilt only when the header changes, which is
 * the sort column, its direction, or the language its labels are written in.
 * The ROWS are reconciled into the tbody on deviceKey, the same key a tile uses,
 * so switching modes does not renumber the farm.
 * ------------------------------------------------------------------ */

/* fleetRowNode builds one <tr>, through the same tableRow() that fills every
 * other table on this page rather than beside it. A hand-written row would be a
 * second copy of the column list and of what table() does with it — the click
 * handling, the cell classes, the order — and the second copy is the one that
 * goes stale.
 *
 * `cols` is passed in so a five-hundred-row pass builds the column list once
 * instead of five hundred times; it defaults for the single-row callers. */
function fleetRowNode(d, cols) {
  return tableRow(cols || fleetColumns(), d, {
    // subjectOf, not the row this <tr> was built from: rows are kept across
    // renders here exactly as tiles are, so the object a handler closes over is
    // a snapshot and the element outlives it.
    onRowClick: (row, ev) => openDevice(subjectOf(ev.currentTarget, row))
  });
}

/* patchFleetRow: the row's cells rewritten from the same builder, every column,
 * never a guess at which one changed. Same argument as patchTile.
 *
 * THE CELLS, NOT THE ROW. patchTile can replace a tile's children outright
 * because the focusable element there is the tile itself; a table row is the
 * other way round — the <tr> announces itself as nothing, and the row's ONE
 * keyboard control is a button inside its first cell. Replacing the row's
 * children would detach that button on every repaint, which in this mode is
 * every fifteen seconds at the latest, because fleetRowSig signs the rendered
 * "4m ago" so the clock in the last column cannot freeze. That is the original
 * defect, reintroduced in the mode a farm of more than forty devices opens in.
 * So the cells are patched one at a time and the button is kept. */
function patchFleetRow(node, d, cols) {
  const built = fleetRowNode(d, cols);
  syncAttributes(node, built);
  const kept = Array.from(node.children);
  const fresh = Array.from(built.children);
  // A row whose column count changed is not a row this can patch cell by cell;
  // the shell is rebuilt on a header change, so this is the belt to that brace.
  if (kept.length !== fresh.length) { node.replaceChildren(...built.childNodes); return; }
  for (let i = 0; i < kept.length; i += 1) patchFleetCell(kept[i], fresh[i]);
}

/* One <td>. Its contents are rewritten wholesale, EXCEPT when the cell is
 * exactly the row's link button in both the kept row and the freshly built one
 * — then the button survives and only its contents and attributes are written
 * over, so focus, hover, an in-flight click and any automation handle stay with
 * it. Same trade as .facts in the host head, for the same reason. */
function patchFleetCell(kept, built) {
  const a = kept.firstElementChild;
  const b = built.firstElementChild;
  if (a && b && kept.children.length === 1 && built.children.length === 1 &&
    a.classList.contains('rowlink') && b.classList.contains('rowlink')) {
    syncAttributes(a, b);
    a.replaceChildren(...b.childNodes);
    return;
  }
  syncAttributes(kept, built);
  kept.replaceChildren(...built.childNodes);
}

/* fleetTableInto draws the table into `pane`, keeping what is already right,
 * and appends the keys it asked for to `want`. Returns what a frozen pass held
 * back. */
function fleetTableInto(pane, rows, want) {
  const sorted = fleetSorted(rows);
  sorted.forEach((d, i) => want.push(deviceKey(d, i)));

  /* The shell carries its own signature. Sort key, direction and language are
   * everything the header renders; a change to any of them rewrites the header
   * and nothing else, and the rows below are reconciled into the new tbody on
   * the next line rather than rebuilt. */
  const shell = signature([fleetSort.key, fleetSort.dir]);
  let scroll = $('.fleet-tbl', pane);
  let rebuilt = false;
  if (!scroll || scroll.dataset.shell !== shell) {
    scroll = fleetTable([]);
    scroll.dataset.shell = shell;
    clearKeyed(pane);
    pane.append(scroll);
    rebuilt = true;
  }

  const cols = fleetColumns();
  return reconcile($('tbody', scroll), sorted, deviceKey, fleetRowSig,
    (d) => fleetRowNode(d, cols), (node, d) => patchFleetRow(node, d, cols),
    /* A rebuilt shell has an empty body, and a frozen pass creates nothing:
     * freezing this one would draw a header over no rows and let the caller
     * conclude the farm is empty. There is nothing to hold still either —
     * whatever the guard was protecting went with the old table. */
    { frozen: fleetPass.frozen && !rebuilt }).held;
}

/* The first cell is the row's keyboard control, and the only one: a table of
 * five hundred devices with seven tab stops per row is a table nobody reaches
 * the bottom of. Its accessible name is the device, not the word in the cell,
 * so what a screen reader announces is what a sighted reader clicks. */
function slotCell(d) {
  const label = d.rackSlot || (d.usbPath ? 'usb ' + d.usbPath : null);
  return el('button', {
    type: 'button',
    class: 'rowlink',
    'aria-label': t('fleet.openDevice', { device: fleetRowName(d) }),
    /* The row this button sits in outlives the object the button was built
     * from — rows are reconciled, not rebuilt — so it asks the row what device
     * it currently stands for instead of trusting the one it closed over. The
     * <tr> is what carries the subject; this button is replaced whenever the
     * row's signature changes, and between those it must not go stale. */
    onclick: (ev) => openDevice(subjectOf(ev.currentTarget.closest('tr'), d))
  }, label
    ? el('span', { class: 'mono' }, label)
    : el('span', { class: 'unslotted', title: t('fleet.unslottedWhy') }, t('fleet.unslotted')));
}

/* The name a screen reader hears in place of a row of cells. Position first,
 * because the position is what an operator walks to. Nothing here is
 * translated: it is a rack slot, a manufacturer and a model, all of them
 * server data. */
function fleetRowName(d) {
  return [d.rackSlot || (d.usbPath ? 'usb ' + d.usbPath : null) || shortId(d.id), deviceModelName(d)]
    .filter(Boolean).join(' — ');
}

/* The model, and the one identity fact that changes how a device must be
 * addressed: an ADB serial that is not unique in this farm. That belongs here
 * rather than under Condition, because nothing about the handset is wrong —
 * two of them answer to the same name, and a command sent by serial could
 * reach either one. */
function modelCell(d) {
  const name = deviceModelName(d);
  return el('span', { class: 'chips' },
    el('span', {
      class: 'trunc',
      title: [name, d.android ? 'Android ' + d.android : null, d.serial || null].filter(Boolean).join('  ')
    }, name || t('fleet.unknownModel'),
      d.android ? el('span', { class: 'dim' }, ' · ' + d.android) : null),
    d.serialAmbiguous
      ? el('span', { class: 'chip chip-degraded', title: t('fleet.dupSerialWhy') },
        el('span', { 'aria-hidden': 'true' }, '⚠'), t('fleet.dupSerial'))
      : null);
}

/* Host and hub in one column. They are one answer — which machine, which port
 * tree — and the pair is what the correlated-failure story is told in.
 *
 * The separator is its own element: inside an inline-flex box a leading space
 * in a text node sits at the start of a line box and is dropped, so
 * "h01 · 3-1" rendered as "h01· 3-1" when it was glued to the hub path. */
function whereCell(d) {
  return el('span', { class: 'f-where-in' },
    el('span', { class: 'mono trunc', title: String(d.host || '') }, d.host || t('fleet.noHost')),
    el('span', { class: 'dim', 'aria-hidden': 'true' }, '·'),
    el('span', { class: 'dim mono' }, d.hubPath || t('fleet.noHub')));
}

/* Condition: the health the watchdog decided, plus an open quarantine record
 * when the health value does not already say so. A device can carry a
 * quarantine that its current health has moved on from, and a quarantine is a
 * standing decision about a handset — losing it on the mode that a large farm
 * opens in would be losing it. */
function conditionCell(d) {
  return el('span', { class: 'chips' },
    healthChip(d.health),
    d.quarantineID && d.health !== 'quarantined'
      ? el('span', { class: 'chip chip-quarantined', title: d.quarantineReason || t('fleet.openQuarantine') },
        el('span', { 'aria-hidden': 'true' }, '■'), 'quarantined')
      : null);
}

/* Availability, which is the whole question of whether a job can have this
 * device: the lease, and the administrative state of the device itself. A
 * drained device holds no lease and is still not available, so a table that
 * showed only the lease would call it free.
 *
 * leaseChips() also returns a "plain" chip beside a held lease, meaning the
 * reaper may reclaim it after TTL plus grace. That is the DEFAULT, and on a
 * table of five hundred rows it is a word on every held row that tells an
 * operator nothing they had not already assumed. The exception — protected —
 * still comes through, because that one is a decision somebody made. */
function availabilityCell(d) {
  const out = drainedState(d);
  return el('span', { class: 'chips' },
    leaseChips(d).filter((c) => !c.classList.contains('chip-plain')),
    out
      ? el('span', { class: 'chip chip-drain', title: out.why },
        el('span', { 'aria-hidden': 'true' }, '⏸'), out.state)
      : null);
}

/* drainedState: is somebody holding this device out of the pool, and at which
 * level? The device's own admin_state first, then the host's — a drained host
 * takes every device on it out with it.
 *
 * The card grid says the host half in the host header, above the tiles. A
 * table has no host header, so without this a whole drained host would read as
 * twenty-eight free devices in the mode a large farm opens in. The value
 * printed is the column's, untranslated, because it is what admin_state says. */
function drainedState(d) {
  if (d.adminState && d.adminState !== 'enabled') {
    return { state: d.adminState, why: 'device admin_state' };
  }
  if (d.hostAdminState && d.hostAdminState !== 'enabled') {
    return { state: d.hostAdminState, why: 'host_admin_state — the host this device is on is out of the pool' };
  }
  return null;
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
