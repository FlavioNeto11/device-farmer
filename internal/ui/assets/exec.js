/* exec.js — building a command for a handset, and understanding it first.
 *
 * # Why this is its own file
 *
 * Not because app.js is long. Nobody has extracted renderFleet or the 430-line
 * screen panel out of it, and "it got long" has already lost that argument in
 * this tree several times.
 *
 * Because roughly half of what is below is a CATALOGUE WITH PROVENANCE, and the
 * people who should edit it are the people who add a probe to internal/watchdog
 * or a property to internal/enroll — not the people maintaining a dashboard.
 * Buried at line 2500 of app.js, between a quarantine handler and an H.264
 * decoder, a table like that is where a catalogue goes to rot. docs.js is the
 * precedent for the mechanics; assets/docs/*.json is the precedent for the
 * principle that content on its own cadence gets its own file.
 *
 * The seam is one function wide: this file exports window.commandBuilder(opts)
 * and reads globals app.js defines. That is a bigger coupling than docs.js's,
 * and it is the honest cost of the split — the alternative was duplicating
 * errText's error-code translation and impactList's markup.
 *
 * # The three facts this whole widget exists to make visible
 *
 * 1. THE COMMAND GOES TO THE DEVICE VERBATIM. adbwire.ShellService builds
 *    "shell,v2,raw:" + whatever you typed. Nothing quotes it, escapes it or
 *    validates it; the only mutation in the entire path is a TrimSpace in the
 *    handler. The handset's own /system/bin/sh does all the word-splitting, the
 *    ';', the '|', the '$( )' and the redirection.
 *
 *    The proof that this needed saying: the placeholder this box shipped with
 *    was `shell getprop ro.build.fingerprint`, which becomes
 *    `shell,v2,raw:shell getprop ...` — asking the phone to run a program called
 *    `shell`. It answers `not found`, exit 127. And on a simulated farm it hits
 *    the fake's catch-all and comes back as ONE BLANK LINE, EXIT 0, so the wrong
 *    advice looked like it had worked.
 *
 * 2. ON A FENCED FARM THIS ROUTE IS DEAD, FARM-WIDE. internal/api's
 *    refuseExecBehindTheFence answers 501 exec_not_admitted before it even looks
 *    the device up, because no pattern over an operator's command line can be
 *    made safe. Composing a whole command and then learning that is the specific
 *    frustration the pre-flight below exists to remove.
 *
 * 3. `exited` MUST BE READ BEFORE `exit_code`. adbwire leaves ExitCode at -1 and
 *    Exited at false when a shell stream ends with no exit frame, and the API
 *    returns that as HTTP 200. The old panel printed "exit_code ?" and called it
 *    a result. It is not a result: it means the status never arrived and the
 *    command may still be running on the phone.
 *
 * # What is translated here and what is not
 *
 * Labels and explanations go through t() and live in i18n.js, in both
 * dictionaries. COMMANDS NEVER DO — the same rule the documentation translation
 * followed for examples[].code, because a translated command does not run. Nor
 * do provenance paths, nor property names: they are identifiers an operator
 * greps for.
 */

(() => {
  'use strict';

  /* ------------------------------------------------------------ provenance */

  /* PROBE_PROPS is the list internal/enroll asks every handset for, in its
   * order. It is a FOURTH copy of that list — the Go constant, the built probe
   * command, the documentation and now this — and the only thing that makes a
   * fourth copy acceptable is TestTheGetpropChoicesAreTheEnrollersOwnProperties,
   * which fails the build when the two disagree. Do not add a property here
   * without adding it there. */
  const PROBE_PROPS = [
    'ro.product.manufacturer',
    'ro.product.model',
    'ro.product.name',
    'ro.product.device',
    'ro.build.version.release',
    'ro.build.version.sdk',
    'ro.product.cpu.abilist',
    'ro.build.fingerprint',
    'ro.hardware',
    'ro.boot.serialno',
    'ro.serialno',
  ];

  /* The catalogue, in two tiers, and the tier is the point.
   *
   * VERIFIED entries carry a `source`: a file in this repository that sends this
   * exact command to a real handset. That is a claim the build checks —
   * TestEveryVerifiedCommandIsOneThisRepoRuns reads the named file and looks for
   * the literal — so `source` is evidence rather than decoration.
   *
   * UNVERIFIED entries carry no source and say so on screen. They are ordinary
   * Android commands that this project has never run: useful, widely known, and
   * NOT covered by anything anybody here has observed. On a simulated farm most
   * of them come back as one blank line with exit 0, because the fake scripts
   * only what this project actually uses.
   *
   * Keeping the two visibly apart is the whole reason the catalogue is worth
   * having. A list that mixed them would make "we run this every minute on every
   * device" and "this generally exists on Android" look like the same kind of
   * statement.
   *
   * effect is 'read' or 'writes' and exists ONLY here. A command you typed gets
   * no classification at all — see the note in the composer.
   */
  const CATALOGUE = [
    /* ---- verified: this repository sends these ---- */
    {
      id: 'getprop', group: 'identity', effect: 'read',
      template: 'getprop {prop}',
      params: [{ name: 'prop', kind: 'choice', choices: PROBE_PROPS, def: 'ro.build.fingerprint' }],
      source: 'internal/enroll/identity.go',
    },
    {
      id: 'brandRead', group: 'identity', effect: 'read',
      template: 'cat /data/local/tmp/.farm/uid',
      params: [],
      /* The DOCUMENTATION, not internal/enroll — and the difference is the kind
       * of thing this tier exists to be honest about. The enroller does not send
       * this: it sends a guarded form that exits 44 when the file is absent, so
       * that "no brand" is distinguishable from "read failed". The bare cat is
       * what the documentation shows for reading a brand BY HAND, which is
       * exactly what an operator is doing here. Citing brand.go would have been
       * a plausible-looking lie, and the build caught it. */
      source: 'internal/ui/assets/docs/devices.json',
    },
    {
      id: 'battery', group: 'health', effect: 'read',
      template: 'dumpsys battery',
      params: [],
      source: 'internal/watchdog/battery.go',
    },
    {
      id: 'batteryLevel', group: 'health', effect: 'read',
      template: 'dumpsys battery | grep level',
      params: [],
      source: 'internal/demo/demo.go',
    },
    {
      id: 'idle', group: 'health', effect: 'read',
      template: 'dumpsys deviceidle get deep',
      params: [],
      source: 'internal/ui/assets/docs/jobs.json',
    },
    {
      id: 'packages', group: 'packages', effect: 'read',
      template: 'pm list packages -3',
      params: [],
      source: 'internal/demo/demo.go',
    },
    {
      id: 'logcat', group: 'logs', effect: 'read',
      template: 'logcat -d -t {n}',
      params: [{ name: 'n', kind: 'int', min: 1, max: 1000, def: 5 }],
      source: 'internal/demo/demo.go',
    },
    {
      id: 'bootCompleted', group: 'health', effect: 'read',
      template: 'getprop sys.boot_completed',
      params: [],
      source: 'internal/jobspec/validate.go',
    },

    /* ---- unverified: ordinary Android, never run by this project ---- */
    { id: 'df', group: 'storage', effect: 'read', template: 'df -h', params: [], source: '' },
    { id: 'ps', group: 'health', effect: 'read', template: 'ps -A', params: [], source: '' },
    { id: 'uptime', group: 'health', effect: 'read', template: 'uptime', params: [], source: '' },
    { id: 'wmSize', group: 'identity', effect: 'read', template: 'wm size', params: [], source: '' },
    { id: 'wmDensity', group: 'identity', effect: 'read', template: 'wm density', params: [], source: '' },
    { id: 'dumpsysWindow', group: 'identity', effect: 'read', template: 'dumpsys window displays', params: [], source: '' },
    { id: 'meminfo', group: 'health', effect: 'read', template: 'dumpsys meminfo', params: [], source: '' },
    { id: 'thermal', group: 'health', effect: 'read', template: 'dumpsys thermalservice', params: [], source: '' },
    { id: 'wifi', group: 'health', effect: 'read', template: 'dumpsys wifi | head -40', params: [], source: '' },
    { id: 'packagesAll', group: 'packages', effect: 'read', template: 'pm list packages', params: [], source: '' },
    { id: 'packagePath', group: 'packages', effect: 'read', template: 'pm path {pkg}', params: [{ name: 'pkg', kind: 'choice', choices: ['com.android.settings', 'com.android.chrome', 'com.acme.app'], def: 'com.android.settings' }], source: '' },
    { id: 'dumpsysPackage', group: 'packages', effect: 'read', template: 'dumpsys package {pkg} | head -60', params: [{ name: 'pkg', kind: 'choice', choices: ['com.android.settings', 'com.android.chrome', 'com.acme.app'], def: 'com.android.settings' }], source: '' },
    { id: 'logcatCrash', group: 'logs', effect: 'read', template: 'logcat -d -b crash -t {n}', params: [{ name: 'n', kind: 'int', min: 1, max: 1000, def: 50 }], source: '' },
    { id: 'settingsGet', group: 'identity', effect: 'read', template: 'settings get {ns} {key}', params: [{ name: 'ns', kind: 'choice', choices: ['global', 'system', 'secure'], def: 'global' }, { name: 'key', kind: 'choice', choices: ['airplane_mode_on', 'development_settings_enabled', 'stay_on_while_plugged_in', 'adb_enabled'], def: 'adb_enabled' }], source: '' },
    { id: 'lsTmp', group: 'storage', effect: 'read', template: 'ls -la /data/local/tmp', params: [], source: '' },
    { id: 'topOnce', group: 'health', effect: 'read', template: 'top -n 1 -b | head -20', params: [], source: '' },
  ];

  /* The group order on screen. Declared rather than derived, so a new entry does
   * not silently reorder the page. */
  const GROUPS = ['identity', 'health', 'packages', 'logs', 'storage'];

  /* The default. dumpsys battery earns the slot on its own terms — read-only,
   * present on every handset, and the command internal/watchdog itself sends
   * once a minute to every attached device. That it also happens to be scripted
   * by the demo's fake, so a first click on a simulated farm returns a real
   * dump instead of a blank line, is a welcome accident and NOT the reason: an
   * operator-facing catalogue ordered by a property of a test fixture would be
   * coupled to that fixture invisibly. TestTheDefaultIsOneTheDemoAnswers pins
   * the accident so it stays true, and pins nothing else. */
  const DEFAULT_ID = 'battery';

  /* The name of the capability row the API publishes for fence enforcement.
   *
   * Matched by an English prose name, which is fragile-looking and is in fact
   * checked: TestTheFenceFeatureNameTheDashboardMatchesIsTheOneTheAPISends
   * compares this literal against the Go one. internal/api/capabilities_test.go
   * already carried a comment saying "the dashboard finds the row by name";
   * until now that described an intention. */
  const FENCE_FEATURE = 'Fence enforcement at the resource';

  /* The API's own bounds. The floor is the interesting one: internal/api clamps
   * timeout_ms from ABOVE and has no floor at all, so timeout_ms:1 is accepted
   * as one millisecond and comes back as a 502 that reads like a broken
   * handset. The UI imposes the floor the API does not. */
  const TIMEOUT_MIN = 1000;
  const TIMEOUT_MAX = 300000;
  const TIMEOUT_DEF = 30000;

  const WIRE_PREFIX = 'shell,v2,raw:';

  /* --------------------------------------------------------------- helpers */

  function entry(id) { return CATALOGUE.find((c) => c.id === id) || null; }

  /* render substitutes {name} with split/join — the same primitive t() uses.
   * No regex, and deliberately no quoting: nothing anywhere in this path quotes
   * anything, and a UI that quoted would teach a model that is false at every
   * other layer. That is also why there is no free-text parameter kind. */
  function renderTemplate(c, values) {
    let out = c.template;
    for (const p of c.params) {
      out = out.split('{' + p.name + '}').join(String(values[p.name]));
    }
    return out;
  }

  function defaults(c) {
    const v = {};
    for (const p of c.params) v[p.name] = p.def;
    return v;
  }

  function clampTimeout(ms) { return Math.min(Math.max(ms, TIMEOUT_MIN), TIMEOUT_MAX); }

  /* ------------------------------------------------------- fence capability */

  /* fenceState answers one question — will this farm refuse every exec — and it
   * is cached for the life of the tab.
   *
   * Cached because handleCapabilities runs three database probes including
   * several count(*)s over farm.devices and farm.jobs, and the drawer can be
   * opened many times a minute. Fencedness is a property of the api PROCESS: it
   * is cfg.FenceClient.Enabled(), read by capabilities.go and by the exec
   * handler as the same expression, and it changes only on redeploy.
   *
   * Three states, and the third is not optional. capabilities answers 503 by
   * design when a probe fails — so that a report which could not be taken is not
   * published as a report — and this must then say "cannot be known from here"
   * rather than "fine". A pre-flight that guessed would be the thing app.js's
   * header forbids. */
  let fenceCache = null;   // null = not asked, 'on' | 'off' | 'unknown'
  let fenceLatched = false; // set once the API has actually answered 501

  async function fenceState() {
    if (fenceLatched) return 'on';
    if (fenceCache) return fenceCache;
    try {
      const caps = await api.get('capabilities');
      const feats = pick(caps, 'features') || [];
      const row = feats.find((f) => pick(f, 'name') === FENCE_FEATURE);
      fenceCache = row && pick(row, 'state') === 'enabled' ? 'on' : 'off';
    } catch (_) {
      /* Not cached: a 503 is transient by construction and the next drawer may
       * get a real answer. */
      return 'unknown';
    }
    return fenceCache;
  }

  /* latchFence records that the API refused for real. It outranks the
   * capability document because it is what happened rather than what a report
   * predicted, and it is the honest fallback when capabilities is unavailable. */
  function latchFence() { fenceLatched = true; fenceCache = 'on'; }

  /* ------------------------------------------------------------ the chooser */

  /* The catalogue was twenty-four buttons under ten headings, inline, above the
   * one field the panel exists for: two and a half screens of wall before the
   * command line. And because both tiers carry the same groups, IDENTITY and
   * HEALTH each appeared twice — so the wall read as one duplicated list rather
   * than as two claims of different standing.
   *
   * It is a dialog now, and the tier is a two-option segmented control that
   * renders ONE tier at a time. That is what fixes the repeated headings: not a
   * rename, just never showing both at once. Twenty-four buttons behind a
   * filter is a chooser; twenty-four buttons in front of the field is a wall.
   *
   * The dialog element itself lives in index.html, empty, and is filled here —
   * the same division docs.js uses. */
  const CHOOSER_DIALOG_ID = 'dlg-cmd';

  /* openChooser(cat, groups, onPick) is deliberately self-contained: it takes
   * the catalogue and a callback and knows nothing about the panel around it,
   * so it drops into any host that has a command field to fill.
   *
   * It rebuilds its contents on every open rather than once. Twenty-four rows
   * cost nothing to build, and the alternative is a retranslate() that has to
   * remember the filter text and the selected tier — a language switch would
   * otherwise leave a dialog reading in the language it was first opened in. */
  function openChooser(cat, groups, onPick) {
    const dlg = document.getElementById(CHOOSER_DIALOG_ID);
    if (!dlg) return false;

    let verifiedTier = true;

    const filter = el('input', {
      type: 'text', class: 'cmd-filter', spellcheck: 'false', autocomplete: 'off',
      placeholder: t('exec.chooser.filter'), 'aria-label': t('exec.chooser.filter'),
    });

    const countV = el('span', { class: 'cmd-seg-n' });
    const countU = el('span', { class: 'cmd-seg-n' });
    const segV = el('button', { class: 'cmd-seg', type: 'button', 'aria-pressed': 'true' },
      el('span', { 'aria-hidden': 'true' }, '✓'), ' ', t('exec.tier.verified'), ' ', countV);
    const segU = el('button', { class: 'cmd-seg', type: 'button', 'aria-pressed': 'false' },
      el('span', { 'aria-hidden': 'true' }, '?'), ' ', t('exec.tier.unverified'), ' ', countU);

    const paneV = el('div', { class: 'cmd-tier' });
    const paneU = el('div', { class: 'cmd-tier' });
    const empty = el('p', { class: 'cmd-chooser-empty', hidden: true });

    /* Matches command text, label, explanation, group and provenance path, on
     * every word typed. The command text is in there because it is what an
     * operator who already knows Android searches for: somebody looking for
     * `logcat` does not know this catalogue calls it "Last N log lines". */
    function matches(c, words) {
      if (!words.length) return true;
      const hay = [
        c.template, c.source,
        t('exec.cat.' + c.id + '.label'), t('exec.cat.' + c.id + '.why'),
        t('exec.group.' + c.group),
      ].join(' ').toLowerCase();
      return words.every((w) => hay.includes(w));
    }

    function pickRow(c) {
      return el('button', {
        class: 'cmd-pick', type: 'button',
        onclick: () => { dlg.close(); onPick(c); },
      },
        el('span', { class: 'cmd-pick-label' }, t('exec.cat.' + c.id + '.label')),
        c.source
          ? el('span', { class: 'cmd-source', title: t('exec.verifiedWhy') },
            el('span', { 'aria-hidden': 'true' }, '✓'), ' ', c.source)
          : null,
        /* The command, verbatim and in monospace. Never through t(): a
         * translated command does not run, and this is the one string in the
         * row an operator may be reading to decide. */
        el('code', { class: 'cmd-pick-cmd' }, c.template),
        el('span', { class: 'cmd-pick-why' }, t('exec.cat.' + c.id + '.why')));
    }

    /* fill returns how many entries this tier has under the current filter,
     * which is what the segmented control counts and what decides the empty
     * note — including its most useful case, "it is in the other tier". */
    function fill(pane, verified, words) {
      pane.replaceChildren(el('p', { class: 'cmd-tier-note' },
        t(verified ? 'exec.tier.verifiedNote' : 'exec.tier.unverifiedNote')));
      let n = 0;
      for (const g of groups) {
        const items = cat.filter((c) => c.group === g && !!c.source === verified && matches(c, words));
        if (!items.length) continue;
        n += items.length;
        const list = el('div', { class: 'cmd-group-items' });
        for (const c of items) list.append(pickRow(c));
        pane.append(el('div', { class: 'cmd-group' },
          el('div', { class: 'cmd-group-name' }, t('exec.group.' + g)), list));
      }
      return n;
    }

    function repaint() {
      const words = filter.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
      const nv = fill(paneV, true, words);
      const nu = fill(paneU, false, words);
      countV.textContent = String(nv);
      countU.textContent = String(nu);
      segV.setAttribute('aria-pressed', verifiedTier ? 'true' : 'false');
      segU.setAttribute('aria-pressed', verifiedTier ? 'false' : 'true');
      /* One tier at a time. .cmd-tier is given a display below, so the
       * stylesheet has to opt back into [hidden] for it — an author display
       * beats the user agent's [hidden] rule at any specificity, which this
       * tree has shipped as a real bug twice. */
      paneV.hidden = !verifiedTier;
      paneU.hidden = verifiedTier;
      const here = verifiedTier ? nv : nu;
      const there = verifiedTier ? nu : nv;
      empty.hidden = here > 0;
      empty.replaceChildren(t('exec.chooser.none'),
        there ? ' ' + t('exec.chooser.otherTier', { n: String(there) }) : '');
    }

    segV.addEventListener('click', () => { verifiedTier = true; repaint(); });
    segU.addEventListener('click', () => { verifiedTier = false; repaint(); });
    filter.addEventListener('input', repaint);

    const close = el('button', { class: 'ghost', type: 'button', onclick: () => dlg.close() },
      t('exec.chooser.close'));

    dlg.replaceChildren(
      el('div', { class: 'dlg-head' },
        el('h2', { id: 'cmd-chooser-title' }, t('exec.catalogue')), close),
      el('div', { class: 'cmd-chooser' },
        filter,
        el('div', { class: 'cmd-segs', role: 'group', 'aria-label': t('exec.chooser.tiers') },
          segV, segU),
        paneV, paneU, empty));

    repaint();
    /* showModal() on an already-open dialog throws, and the panel's own button
     * cannot be reached while it is open — but window.openCommandChooser can be
     * called by anything, and a thrown InvalidStateError there would take the
     * caller's click handler down with it. */
    if (!dlg.open) dlg.showModal();
    filter.focus();
    return true;
  }

  /* ------------------------------------------------------------- the widget */

  /* commandBuilder(opts) returns { node, command(), timeoutMS(), … }.
   *
   * opts:
   *   device   the normalised fleet row, or null for a fleet-wide form (bulk)
   *   onRun    async (command, timeoutMS) => void, called when Enter is pressed
   *            in the command field. Absent for the bulk form, where Enter
   *            belongs to the host <form> whose submit button owns the run, and
   *            where a widget that ran the command itself would run it twice.
   *            The Run BUTTON is never the widget's: in the drawer it must sit
   *            below the force-and-reason consent, and only the host knows that
   *            order.
   *   compact  true for the bulk form, which has its own submit and its own
   *            fields. The widget then drops the panel chrome and renders the
   *            chooser, the command field, the wire line, a FLEET-WIDE
   *            pre-flight and the timeout — and nothing else.
   *   timeout  the timeout field's starting value in ms, clamped to the field's
   *            own bounds. Bulk passes 60000 because the input this widget
   *            replaced shipped that default, and halving a fleet-wide timeout
   *            in passing is how a run that used to finish starts reporting a
   *            timeout on every one of fifty-six targets.
   *
   * Every option above is read below, and that sentence is here because it was
   * not true: `compact` and `onRun` were documented and never implemented, so
   * this comment described an API the file did not have while the bulk form
   * went on shipping a bare <input> with none of it.
   * TestTheCommandBuilderOptionsAreRead is what keeps it true.
   */
  function commandBuilder(opts) {
    const o = opts || {};
    const device = o.device || null;
    const compact = !!o.compact;

    let selected = entry(DEFAULT_ID);
    let values = defaults(selected);

    /* ---- the composer: one text field, one truth ---- */
    const input = el('input', {
      type: 'text', class: 'cmd-input', spellcheck: 'false', autocomplete: 'off',
      placeholder: 'getprop ro.build.fingerprint',
      'aria-label': t('exec.commandLabel'),
    });
    input.value = renderTemplate(selected, values);

    /* Enter is the widget's only submission gesture, because the field is the
     * widget's. The button is not: see onRun above. */
    input.addEventListener('keydown', (ev) => {
      if (ev.key !== 'Enter' || typeof o.onRun !== 'function') return;
      ev.preventDefault();
      o.onRun(input.value.trim(), timeoutMS());
    });

    const params = el('div', { class: 'cmd-params' });
    const wire = el('code', { class: 'cmd-wire-line' });
    const chosen = el('div', { class: 'cmd-chosen' });
    const preflight = el('ul', { class: 'cmd-preflight' });

    const paint = () => {
      wire.textContent = WIRE_PREFIX + input.value;
      renderChosen();
      renderPreflight();
    };

    /* Editing the text drops the selection AND the classification with it. One
     * field, one truth: a command that no longer matches the entry it came from
     * must not keep wearing that entry's read-only badge. */
    input.addEventListener('input', () => {
      if (selected && input.value !== renderTemplate(selected, values)) {
        selected = null;
        params.replaceChildren();
      }
      paint();
    });

    function choose(c) {
      selected = c;
      values = defaults(c);
      input.value = renderTemplate(c, values);
      renderParams();
      paint();
    }

    /* ---- parameters: choice and int, and nothing else ----
     *
     * There is deliberately no free-text parameter kind. A text parameter is
     * indistinguishable from typing the command, and it pulls the UI toward
     * quoting the value — and the moment this UI quotes anything it does
     * something no other layer in this system does. A select and a number input
     * raise no quoting question at all. */
    function renderParams() {
      params.replaceChildren();
      if (!selected || !selected.params.length) return;
      for (const p of selected.params) {
        let field;
        if (p.kind === 'choice') {
          field = el('select', { class: 'cmd-param' });
          for (const v of p.choices) {
            const op = el('option', { value: v }, v);
            if (v === values[p.name]) op.selected = true;
            field.append(op);
          }
          field.addEventListener('change', () => {
            values[p.name] = field.value;
            input.value = renderTemplate(selected, values);
            paint();
          });
        } else {
          field = el('input', {
            class: 'cmd-param', type: 'number',
            min: String(p.min), max: String(p.max), step: '1',
          });
          field.value = String(values[p.name]);
          field.addEventListener('input', () => {
            const n = Number(field.value);
            if (!Number.isFinite(n)) return;
            values[p.name] = Math.min(Math.max(Math.round(n), p.min), p.max);
            input.value = renderTemplate(selected, values);
            paint();
          });
          /* And the field is corrected to what was actually used, on the way
           * out. Clamping on every keystroke would fight the typist — 1 is on
           * the way to 10 — but leaving 2000 in a box whose command reads
           * `-t 1000` shows the operator two different numbers and calls both
           * of them the request. */
          field.addEventListener('change', () => { field.value = String(values[p.name]); });
        }
        params.append(el('label', { class: 'cmd-param-wrap' },
          el('span', null, p.name), field));
      }
    }

    /* ---- what the chosen entry is, and what it is not ---- */
    function renderChosen() {
      chosen.replaceChildren();
      if (!selected) {
        /* No classification for a typed command, and the reason said ONCE.
         * Implemented as absence rather than as a branch: effect is a property
         * of a catalogue entry and there is no entry here. */
        chosen.append(el('span', { class: 'cmd-note' }, t('exec.freeform')));
        return;
      }
      const verified = !!selected.source;
      chosen.append(
        el('span', {
          class: 'chip ' + (selected.effect === 'read' ? 'chip-healthy' : 'chip-degraded'),
          title: t(selected.effect === 'read' ? 'exec.effect.readWhy' : 'exec.effect.writesWhy'),
        },
          el('span', { 'aria-hidden': 'true' }, selected.effect === 'read' ? '○' : '▲'),
          t(selected.effect === 'read' ? 'exec.effect.read' : 'exec.effect.writes')),
        el('span', { class: 'cmd-why' }, t('exec.cat.' + selected.id + '.why')),
        verified
          ? el('span', { class: 'cmd-source', title: t('exec.verifiedWhy') },
            el('span', { 'aria-hidden': 'true' }, '✓'), ' ', selected.source)
          : el('span', { class: 'cmd-unverified', title: t('exec.unverifiedWhy') },
            el('span', { 'aria-hidden': 'true' }, '?'), ' ', t('exec.unverified')));
    }

    /* ---- the pre-flight ----
     *
     * A list, in the order internal/api evaluates its refusals, of what will
     * stop this request — each row traceable to a field already fetched, or
     * explicitly unknowable.
     *
     * Deliberately NOT a box diagram of browser → api → proxy → adb → device.
     * This page cannot see whether a host runs a fence proxy — the capability's
     * own detail says so — cannot see the ADB server, and cannot see the
     * handset's shell. Four boxes of which three are guesses is exactly what
     * app.js's header forbids: a dashboard that guesses is worse than no
     * dashboard, because an operator acts on it. */
    function row(state, label, evidence) {
      const glyph = state === 'blocked' ? '✕' : state === 'unknown' ? '?' : '✓';
      return el('li', { class: 'cmd-pf cmd-pf-' + state },
        el('span', { class: 'cmd-pf-glyph', 'aria-hidden': 'true' }, glyph),
        el('span', { class: 'cmd-pf-label' }, label),
        el('span', { class: 'cmd-pf-evidence' }, evidence));
    }

    let fence = 'unknown';
    function renderPreflight() {
      preflight.replaceChildren();

      preflight.append(input.value.trim()
        ? row('ok', t('exec.pf.command'), t('exec.pf.commandOk'))
        : row('blocked', t('exec.pf.command'), t('exec.pf.commandEmpty')));

      /* A fenced farm reads differently fleet-wide, and the difference matters
       * because it decides what the operator will SEE. refuseExecBehindTheFence
       * guards the single-device route only: a bulk run is never refused up
       * front, it starts, and every target answers with a transport failure of
       * its own. Fifty-six red rows and one 501 are the same fence. */
      preflight.append(
        fence === 'on' ? row('blocked', t('exec.pf.fence'),
          compact ? t('exec.bulk.fenceOn') : t('exec.pf.fenceOn'))
          : fence === 'off' ? row('ok', t('exec.pf.fence'), t('exec.pf.fenceOff'))
            : row('unknown', t('exec.pf.fence'), t('exec.pf.fenceUnknown')));

      /* Device rows only when there is a device. The bulk form addresses a
       * selector, and claiming anything about "the device" there would be the
       * guessing failure again. */
      if (!device) {
        preflight.append(row('unknown', t('exec.pf.targets'),
          compact ? t('exec.bulk.targets') : t('exec.pf.targetsBulk')));
        preflight.append(row('unknown', t('exec.pf.answer'),
          compact ? t('exec.bulk.answer') : t('exec.pf.answerUnknown')));
        return;
      }

      preflight.append(device.devPath
        ? row('ok', t('exec.pf.slot'), device.devPath)
        : row('blocked', t('exec.pf.slot'), t('exec.pf.slotNone')));

      preflight.append(device.adbEndpoint
        ? row('ok', t('exec.pf.endpoint'), device.adbEndpoint)
        : row('blocked', t('exec.pf.endpoint'), t('exec.pf.endpointNone')));

      const held = device.leaseState && device.leaseState !== 'released';
      preflight.append(held
        ? row('blocked', t('exec.pf.lease'),
          t('exec.pf.leaseHeld', { holder: device.holder || '—', job: shortId(device.jobID) }))
        : row('ok', t('exec.pf.lease'), t('exec.pf.leaseFree')));

      preflight.append(row('unknown', t('exec.pf.answer'), t('exec.pf.answerUnknown')));
    }

    /* ---- the way into the catalogue ----
     *
     * One button. The catalogue itself is in the dialog above, which is the
     * whole point of this unit: what used to sit here was every entry at once,
     * in front of the field. */
    const pickBtn = el('button', { class: 'cmd-open-chooser', type: 'button' },
      t('exec.chooser.open'));
    pickBtn.addEventListener('click', () => openChooser(CATALOGUE, GROUPS, choose));

    const timeout = el('input', {
      class: 'cmd-timeout', type: 'number',
      min: String(TIMEOUT_MIN), max: String(TIMEOUT_MAX), step: '1000',
    });
    timeout.value = String(clampTimeout(Number(o.timeout) || TIMEOUT_DEF));

    /* Declared rather than assigned, so the Enter handler above — which is
     * wired before this line runs — can close over it. */
    function timeoutMS() { return clampTimeout(Number(timeout.value) || TIMEOUT_DEF); }

    /* The static chrome is held by reference rather than built inline, because
     * retranslate() has to reach every one of these. The bulk form is built
     * once at first paint and never rebuilt — unlike the drawer, which a
     * language switch reopens from the fleet row — so a heading this function
     * cannot reach is a heading that stays in the old language for the life of
     * the tab. */
    const wireHead = el('div', { class: 'cmd-wire-head' }, t('exec.wire'));
    const wireNote = el('p', { class: 'cmd-note' }, t('exec.wireNote'));
    const pfHead = el('div', { class: 'cmd-wire-head' },
      compact ? t('exec.bulk.preflight') : t('exec.preflight'));
    /* The one sentence that says what compact mode is FOR: in the drawer a
     * wrong command reaches one handset; here it reaches every device the
     * selector matched. */
    const bulkNote = compact ? el('p', { class: 'cmd-note' }, t('exec.bulk.note')) : null;
    const timeoutLabel = el('span', null, t('exec.timeout', { max: TIMEOUT_MAX / 1000 }));

    const node = el('div', { class: 'cmd-builder' + (compact ? ' cmd-compact' : '') },
      el('div', { class: 'cmd-choose-row' }, pickBtn),
      params,
      el('div', { class: 'cmd-row' }, input),
      chosen,
      el('div', { class: 'cmd-wire' }, wireHead, wire, wireNote),
      el('div', { class: 'cmd-preflight-wrap' }, pfHead, preflight, bulkNote),
      el('label', { class: 'cmd-timeout-wrap' }, timeoutLabel, timeout));

    function retranslate() {
      pickBtn.textContent = t('exec.chooser.open');
      input.setAttribute('aria-label', t('exec.commandLabel'));
      wireHead.textContent = t('exec.wire');
      wireNote.textContent = t('exec.wireNote');
      pfHead.textContent = compact ? t('exec.bulk.preflight') : t('exec.preflight');
      if (bulkNote) bulkNote.textContent = t('exec.bulk.note');
      timeoutLabel.textContent = t('exec.timeout', { max: TIMEOUT_MAX / 1000 });
      renderParams();
      paint();
    }

    renderParams();
    paint();

    /* Only the compact instance listens for itself. The drawer builds a fresh
     * builder on every open and execPanel already drives its retranslate, so a
     * listener here would accumulate one per open for no gain. */
    if (compact) {
      window.addEventListener('languagechange', () => {
        if (!node.isConnected) return;
        try { retranslate(); } catch (_) { /* a stale widget must not strand the switch */ }
      });
    }

    /* The capability read is lazy and fire-and-forget: the panel renders
     * immediately with the fence row unknown, and corrects itself when the
     * answer lands. Blocking the whole widget on a database-backed report to
     * draw one line would be the wrong trade. */
    fenceState().then((s) => { fence = s; renderPreflight(); }).catch(() => { });

    return {
      node,
      command: () => input.value.trim(),
      timeoutMS,
      input,
      refreshFence: () => fenceState().then((s) => { fence = s; renderPreflight(); }),
      latchFence: () => { latchFence(); fence = 'on'; renderPreflight(); },
      retranslate,
    };
  }

  /* --------------------------------------------------------- the run panel */

  /* execPanel(d) is the whole "Run one ADB command" section of the device
   * drawer: the builder above, a Run button, the force+reason step, and a
   * result panel that tells the truth about what came back. */
  function execPanel(d) {
    /* onRun is Enter in the command field. run() is a hoisted declaration
     * below, so the closure is valid here. */
    const builder = commandBuilder({ device: d, onRun: () => run() });

    const out = el('pre', { class: 'out', hidden: true });
    const verdict = el('div', { class: 'cmd-verdict', hidden: true });
    const err = el('div', { class: 'form-error', hidden: true });

    /* The force step is REVEALED, not offered. It appears when the fleet row
     * already shows a lease, and it appears again when the server answers 409 —
     * which is the authority, because the fleet row can be five seconds stale
     * and a lease acquired three seconds ago is invisible to it.
     *
     * It deliberately does NOT go through openConfirm, and the next reviewer
     * will ask why: wireConfirm closes its dialog on success, raises an ok
     * banner and calls refreshAll(). A banner is not a container for four
     * kilobytes of logcat, and a getprop does not justify refetching the whole
     * fleet. So the consent lives in the panel, reusing impactList for the
     * wording and the .form-error pattern for the refusal. */
    const reason = el('input', {
      type: 'text', class: 'cmd-reason', maxlength: '240',
      placeholder: t('exec.reasonPlaceholder'),
    });
    const impactBox = el('div', { class: 'cmd-force-impact' });
    /* Held rather than inlined, because this label is no longer the word
     * "Reason": it now states the server's condition — a reason is required
     * BECAUSE this device is leased — and that sentence has to follow a
     * language switch like every other sentence on the panel. */
    const reasonLabel = el('span', null, t('exec.force.reason'));
    const forceWrap = el('div', { class: 'cmd-force', hidden: true },
      el('div', { class: 'cmd-force-head' },
        el('span', { 'aria-hidden': 'true' }, '▲'), ' ', t('exec.force.head')),
      impactBox,
      el('label', { class: 'cmd-reason-wrap' }, reasonLabel, reason),
      el('p', { class: 'cmd-note' }, t('exec.force.audit')));
    window.addEventListener('languagechange', () => {
      if (!forceWrap.isConnected) return;
      try { reasonLabel.textContent = t('exec.force.reason'); } catch (_) { /* a stale panel must not strand the switch */ }
    });

    let forcing = false;
    function revealForce(detail) {
      forcing = true;
      forceWrap.hidden = false;
      const lines = [];
      if (detail) {
        if (detail.holder) lines.push(t('exec.force.holder', { holder: detail.holder }));
        if (detail.job_id) lines.push(t('exec.force.job', { job: shortId(detail.job_id) }));
        if (detail.tenant_id) lines.push(t('exec.force.tenant', { tenant: detail.tenant_id }));
        if (detail.protected) lines.push(t('exec.force.protected'));
      }
      lines.push(t('exec.force.noSignal'));
      impactBox.replaceChildren(
        impactList(d.rackSlot || d.usbPath || shortId(d.id), lines));
    }

    /* forceReasonMissing is what a silent focus jump used to be.
     *
     * run() answered an empty reason by moving the caret into this field and
     * nothing else — no message, no sound, no change on screen — which reads as
     * a Run button that does not work. The message names the condition the
     * server actually applies: internal/api/fleet.go asks for a reason when
     * d.Lease != nil, so it is required BECAUSE this device is leased and not
     * because every command needs one. */
    function forceReasonMissing() {
      verdict.hidden = true;
      out.hidden = true;
      // Announced, not just shown. The caret lands in the reason field a moment
      // later, and a reader who cannot see the box would otherwise be moved
      // there with no explanation at all — which is the same silence, wearing a
      // message.
      err.setAttribute('role', 'alert');
      err.hidden = false;
      err.replaceChildren(
        el('span', { 'aria-hidden': 'true' }, '▲'), ' ',
        el('strong', null, t('exec.force.reasonMissingHead')), ' ',
        el('span', null, t('exec.force.reasonMissing')));
    }
    /* Revealed from the fleet row when it already shows a lease, and revealed
     * again by a 409 — which is the authority, because this row can be five
     * seconds old and a lease acquired three seconds ago is invisible to it.
     *
     * The row's own lease fields are passed through so the first reveal names
     * WHO holds the device, rather than waiting for the server to say it. An
     * impact list that only warned in the abstract, when the holder's name was
     * already on screen two sections above, would be asking for consent while
     * withholding the one fact the decision turns on. */
    if (d.leaseState && d.leaseState !== 'released') {
      revealForce({ holder: d.holder, job_id: d.jobID, tenant_id: d.tenant, protected: d.protected });
    }

    /* ---- the result, in the order the truth arrives ---- */
    function renderResult(resp) {
      err.hidden = true;
      verdict.hidden = false;
      const exited = pick(resp, 'exited');
      const code = pick(resp, 'exit_code');
      const stdout = pick(resp, 'output') || '';
      const stderr = pick(resp, 'stderr') || '';
      const truncated = pick(resp, 'truncated');
      const ms = pick(resp, 'duration_ms');

      const bits = [];

      /* exited FIRST. adbwire leaves exit_code at -1 when a shell stream ends
       * with no exit frame, and the API returns that as a 200 — so a panel that
       * led with the number would print "exit_code -1" for a command that may
       * still be running on the phone. It is not a failure and it is not a
       * success; it is an unknown, and ctl already words it that way. */
      if (exited === false) {
        bits.push(el('div', { class: 'cmd-v cmd-v-unknown' },
          el('span', { 'aria-hidden': 'true' }, '?'), ' ', t('exec.result.neverExited')));
      } else if (code === 0) {
        bits.push(el('div', { class: 'cmd-v cmd-v-ok' },
          el('span', { 'aria-hidden': 'true' }, '✓'), ' ', t('exec.result.exited0')));
      } else {
        bits.push(el('div', { class: 'cmd-v cmd-v-bad' },
          el('span', { 'aria-hidden': 'true' }, '✕'), ' ',
          t('exec.result.exitedN', { code: String(code) })));
      }

      /* A command that exited 0 and printed nothing is its own outcome, and
       * saying so is what makes a simulated farm legible: the fake answers any
       * command it does not script with exactly this, so without this line an
       * unrecognised command looks like a quiet success. */
      if (exited !== false && !stdout.trim() && !stderr.trim()) {
        bits.push(el('div', { class: 'cmd-v cmd-v-note' }, t('exec.result.silent')));
      }
      if (truncated) {
        bits.push(el('div', { class: 'cmd-v cmd-v-warn' },
          el('span', { 'aria-hidden': 'true' }, '▲'), ' ', t('exec.result.truncated')));
      }
      if (ms !== undefined && ms !== null) {
        bits.push(el('div', { class: 'cmd-v cmd-v-note' }, t('exec.result.took', { ms: String(ms) })));
      }
      verdict.replaceChildren(...bits);

      out.hidden = false;
      out.replaceChildren();
      if (stdout) out.append(stdout);
      if (stderr) {
        out.append(el('div', { class: 'cmd-stderr-head' }, t('exec.result.stderr')));
        out.append(stderr);
      }
      if (!stdout && !stderr) out.append(t('exec.result.nothing'));
    }

    function renderRefusal(e) {
      verdict.hidden = true;
      out.hidden = true;
      err.hidden = false;
      err.replaceChildren(
        el('strong', null, t('exec.refused') + ' '), errText(e),
        e instanceof ApiError && e.detail !== undefined
          ? el('span', { class: 'fe-detail' },
            typeof e.detail === 'string' ? e.detail : JSON.stringify(e.detail, null, 2))
          : null);
    }

    const runBtn = el('button', { class: 'primary', type: 'button' }, t('exec.run'));

    async function run() {
      const cmd = builder.command();
      if (!cmd) { builder.input.focus(); return; }
      if (forcing && !reason.value.trim()) { forceReasonMissing(); reason.focus(); return; }

      runBtn.disabled = true;
      verdict.hidden = true;
      err.hidden = true;
      out.hidden = false;
      out.textContent = t('exec.running');

      const body = { command: cmd, timeout_ms: builder.timeoutMS() };
      if (forcing) { body.force = true; body.reason = reason.value.trim(); }

      /* NO AbortController, and that is deliberate. internal/api derives the
       * exec context from the request's own, so aborting this fetch would
       * cancel the server context and kill a half-executed command on a phone.
       * A screen left running is a handset encoding for nobody; a command left
       * running is work already begun. If the drawer closes the promise is
       * kept, and the outcome arrives as a banner instead of nowhere — which is
       * what happened before: the command ran, the audit row was written, and
       * the operator saw nothing at all. */
      const where = d.rackSlot || d.usbPath || shortId(d.id);
      try {
        const resp = await api.post('devices/' + encodeURIComponent(d.id) + '/exec', body);
        if (out.isConnected) {
          renderResult(resp);
        } else {
          banner(pick(resp, 'exited') === false ? 'warn' : 'ok',
            t('exec.landedAfterClose', { where: where }));
        }
      } catch (e) {
        if (e instanceof ApiError && e.code === 'exec_not_admitted') builder.latchFence();
        if (!out.isConnected) {
          banner('warn', t('exec.landedAfterClose', { where: where }), { detail: errText(e) });
        } else if (e instanceof ApiError && e.status === 409 && !forcing) {
          renderRefusal(e);
          revealForce(e.detail && typeof e.detail === 'object' ? e.detail : null);
        } else {
          renderRefusal(e);
        }
      } finally {
        runBtn.disabled = false;
      }
    }

    runBtn.addEventListener('click', run);

    /* The drawer is not re-rendered on a language switch — render() dispatches
     * on state.view and never touches it — so the panel re-translates its own
     * chrome and leaves the typed command, the parameters and the bytes in the
     * output alone. An already-rendered interpolated sentence keeps its old
     * language until the next run; that residue is bounded, and stating it is
     * better than a re-render that would throw away a result somebody is
     * reading. */
    window.addEventListener('languagechange', () => {
      if (!out.isConnected) return;
      try {
        builder.retranslate();
        runBtn.textContent = t('exec.run');
        reason.placeholder = t('exec.reasonPlaceholder');
      } catch (_) { /* a stale panel must not strand the switch */ }
    });

    return el('div', { class: 'cmd-panel' },
      builder.node, forceWrap,
      el('div', { class: 'cmd-actions' }, runBtn),
      err, verdict, out);
  }

  window.execPanel = execPanel;

  window.commandBuilder = commandBuilder;
  /* The seam a host that is not this file uses to reach the catalogue: one
   * call, a callback, and no knowledge of how the dialog is built. */
  window.openCommandChooser = (onPick) => openChooser(CATALOGUE, GROUPS, onPick);
  window.execCatalogue = CATALOGUE;      /* for tests and for the console */
  window.execProbeProps = PROBE_PROPS;
})();
