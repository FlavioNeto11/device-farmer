/* The glossary.
 *
 * Every domain word this product puts on screen — lease, fence, holder,
 * witness, rung, devpath — is also a column name, an API field or a value an
 * operator will meet in psql, in ctl output and in a log line. So none of them
 * is renamed: a screen that says something other than farm.leases.fence cannot
 * be matched against the database that says fence, and being matchable is the
 * whole reason this product writes the words down.
 *
 * The word stays. The explanation is attached to it.
 *
 * Owned by unit 10.
 *
 * # What this file is for
 *
 * `term('fence')` returns a button that reads "fence" with a dotted underline.
 * Clicking it opens one shared dialog carrying three things: the word, one
 * plain sentence, and the identifier the word maps to. A fourth control leads
 * into the Docs tab, which has had the long-form definition of every one of
 * these words all along and nothing on screen linking to it.
 *
 * The dotted underline is the point, and it is a shape rather than a colour so
 * that it survives a greyscale screenshot pasted into a ticket. A `title`
 * attribute is invisible until you hover, unreachable on a touch screen, and
 * gives a reader scanning the page no reason to believe an explanation exists.
 * The title is still set — hover is free and somebody is used to it — but it is
 * the second affordance here, not the first.
 *
 * # What a definition here has to do
 *
 * State what the thing IS, then correct the misconception the reader is about
 * to have. A gloss that only restates the word — which is what the old
 * `title: 'lease fence'` did — is the failure this file exists to fix. The
 * sentences live in i18n.js under `term.<id>.short`, because they are prose and
 * prose is translated; the identifiers live beside them under
 * `term.<id>.ident` and are byte-identical in both languages, which
 * TestNoTermIdentifierIsTranslated enforces.
 *
 * # The entries
 *
 * `area` and `heading` name a section of assets/docs/<area>.json. The heading
 * is written here in ENGLISH, always, because that file is the source of truth
 * on disk; docs.js turns it into a section index and then into whatever the
 * translated document calls that same section. TestEveryTermPointsAtADocsSection
 * ThatExists reads the JSON and fails if a heading has been renamed, because a
 * doc edit would otherwise turn every "read the full definition" into a dead
 * end silently.
 *
 * This file deliberately touches no view code. The call sites belong to unit 11.
 */

(() => {
  'use strict';

  /* One line per entry, in the order a new operator meets them: the lease
     vocabulary first, then the work that asks for a lease, then the hardware
     underneath, then the two states a human acts on. */
  const TERMS = [
    { id: 'fence', area: 'lease', heading: 'The fence: what it protects, and where it is not checked' },
    { id: 'witness', area: 'lease', heading: 'Witness extensions and their cap' },
    { id: 'holder', area: 'lease', heading: 'What a lease is' },
    { id: 'suspect', area: 'lease', heading: 'held -> suspect -> held: suspect does NOT mean broken' },
    { id: 'protected', area: 'lease', heading: 'Protected leases and \'hold and page\'' },
    { id: 'guards', area: 'lease', heading: 'The guard triggers' },
    { id: 'tenant', area: 'surface', heading: 'Every route, with the role it needs' },
    { id: 'pool', area: 'jobs', heading: 'Validating before you file a job' },
    { id: 'queue', area: 'jobs', heading: 'Validating before you file a job' },
    { id: 'disruptionPolicy', area: 'recovery', heading: 'Why a rung is refused rather than downgraded' },
    { id: 'rung', area: 'recovery', heading: 'The ladder, rung by rung' },
    { id: 'blastRadius', area: 'recovery', heading: 'The agent is the last line on blast radius' },
    { id: 'quarantine', area: 'recovery', heading: 'Quarantine: scope, what actually stops allocation, and getting back out' },
    { id: 'drain', area: 'recovery', heading: 'Operator actions and what they cost' },
    { id: 'devpath', area: 'devices', heading: 'How USB topology becomes slots, hubs, controllers and power domains' },
    { id: 'adminState', area: 'devices', heading: 'The device state machine' },
    { id: 'slotState', area: 'devices', heading: 'Slot lifecycle: marked, never deleted' }
  ];

  const byID = new Map(TERMS.map((e) => [e.id, e]));

  /* i18n.js is loaded before this file and defines t() in the shared lexical
     scope of classic scripts. It is read through a shim for the same reason
     docs.js reads it through one: a glossary that throws ReferenceError because
     a sibling file has not merged yet is a worse failure than a glossary that
     is briefly English-only. */
  function tr(key) {
    if (typeof t === 'function') {
      try {
        const s = t(key);
        if (typeof s === 'string' && s !== '') return s;
      } catch { /* an i18n layer mid-initialisation is not a reason to stop */ }
    }
    return key;
  }

  const shortOf = (id) => tr('term.' + id + '.short');
  const identOf = (id) => tr('term.' + id + '.ident');

  /* ------------------------------------------------------------ the button */

  let openerEl = null;
  let currentID = null;
  let wired = false;

  /* Set for the one path that deliberately does NOT restore focus to the word:
     following the deep link into Docs. See the close handler. */
  let leaving = false;

  /* term(id, label) — the word, glossed.
   *
   * `label` exists because the word on screen is not always the dictionary id:
   * the Jobs table heads a column "Guards", a device card writes the fence as
   * `f7`, and the id has to stay a stable key either way.
   *
   * An unknown id returns the plain word rather than throwing. This is called
   * from render paths that repaint under a live event stream, and a glossary
   * that can take a view down is worse than a word without a gloss. */
  function term(id, label) {
    const word = label === undefined || label === null || label === '' ? id : String(label);
    if (!byID.has(id)) return document.createTextNode(word);

    return el('button', {
      type: 'button',
      class: 'term',
      'data-term': id,
      'aria-haspopup': 'dialog',
      title: shortOf(id),
      onclick: (ev) => {
        ev.preventDefault();
        ev.stopPropagation();
        openTerm(id, ev.currentTarget);
      }
    }, word);
  }

  /* ------------------------------------------------------------ the dialog */

  /* One shared <dialog>, in index.html, rather than a popover or an anchored
     panel: `popover` and CSS anchor positioning are not supported everywhere
     this dashboard is opened and there is no polyfill under the no-dependency
     rule. A modal dialog is boring and works in every engine. */
  function wire() {
    if (wired) return;
    const dlg = $('#dlg-term');
    if (!dlg) return;
    wired = true;

    const close = $('#term-close');
    if (close) close.addEventListener('click', () => dlg.close());

    const more = $('#term-more');
    if (more) {
      more.addEventListener('click', () => {
        const entry = byID.get(currentID);
        if (!entry || typeof window.openDocs !== 'function') return;

        /* EVERY open dialog closes, not just this one.
         *
         * A word is glossed inside the device sheet as readily as on a table
         * row, and that sheet is a MODAL — opened with showModal(). Switching
         * the view behind a modal changes a page nobody can reach: the reader
         * presses "read the full definition", sees the device sheet unchanged,
         * and finds the Docs page only if they think to close the sheet by
         * hand. Closing them is also the honest reading of the click: they
         * asked to leave for another page.
         *
         * Each dialog's own close handler still runs — device.js stops a live
         * screen session on it, app.js drops a pending confirmation — so this
         * is a departure, not a teardown that skips their cleanup. */
        leaving = true;
        for (const d of document.querySelectorAll('dialog[open]')) d.close();

        /* Focus is NOT restored to the word here, and the `leaving` flag is
         * what stops the close handler from trying.
         *
         * Two reasons, and the first is a fact about the platform rather than a
         * preference: close() fires its `close` event from a queued task, so
         * that handler runs AFTER the synchronous part of openDocs below has
         * already hidden the view the word was in. Restoring focus then either
         * yanks the reader back into a dialog they just left or silently does
         * nothing on a hidden element. The second is that the reader asked to
         * go somewhere: setView focuses the view they asked for, which is
         * where a screen reader should start reading. */
        window.openDocs(entry.area, entry.heading);
      });
    }

    /* Escape, and every other key, stops here.
     *
     * <dialog> closes itself on Escape, but app.js also listens for keys on the
     * document: it closes the topmost of its own three dialogs on Escape, and
     * it treats 1-7 as view hotkeys. Neither knows about this dialog, so
     * without this handler pressing Escape over a glossary opened from the
     * device sheet would close the SHEET, and pressing 3 would switch the view
     * behind it. Stopping propagation at the dialog is the fix that needs no
     * edit to app.js. */
    dlg.addEventListener('keydown', (ev) => {
      ev.stopPropagation();
      if (ev.key === 'Escape') {
        ev.preventDefault();
        dlg.close();
      }
    });

    /* Focus returns to the word that was clicked.
     *
     * The platform restores focus for a modal dialog on its own, but only to
     * whatever was focused when showModal() ran, and only while that element is
     * still focusable. Two things here defeat that. Every view repaints under a
     * live event stream, so the button may have been replaced while the dialog
     * was open — hence the fallback to the same term wherever it now is. And a
     * click does not focus a <button> in every engine (Safari notably does not),
     * so the element the platform would restore to can be the body.
     *
     * The restore is deferred a frame so it lands AFTER the platform's own,
     * rather than being overwritten by it. Where the platform got it right this
     * focuses the element that already has focus, which costs nothing. */
    dlg.addEventListener('close', () => {
      const back = openerEl && openerEl.isConnected
        ? openerEl
        : (currentID ? document.querySelector('.term[data-term="' + currentID + '"]') : null);
      openerEl = null;
      if (leaving) { leaving = false; return; }
      if (back && typeof back.focus === 'function') requestAnimationFrame(() => back.focus());
    });
  }

  function fillDialog(id) {
    const word = $('#term-title');
    const short = $('#term-short');
    const ident = $('#term-ident');
    const more = $('#term-more');
    const entry = byID.get(id);

    // The heading is the word itself, and it is the dialog's accessible name
    // through aria-labelledby. A dialog announced as "Glossary" tells a screen
    // reader user nothing about which word they just opened.
    if (word) word.textContent = wordOf(id);
    if (short) short.textContent = shortOf(id);
    if (ident) ident.textContent = identOf(id);
    // Docs is a separate script. If it failed to load there is nothing to link
    // to, and a control that does nothing is worse than no control.
    if (more) more.hidden = !(entry && typeof window.openDocs === 'function');
  }

  /* The word as prose. The ids are camelCase because they are dictionary keys;
     three of them name two words and the heading has to read as English (and
     as Portuguese — these three are spelled the same in both). */
  const WORDS = {
    blastRadius: 'blast radius',
    adminState: 'admin_state',
    slotState: 'slot state',
    disruptionPolicy: 'disruption policy'
  };
  function wordOf(id) { return WORDS[id] || id; }

  function openTerm(id, opener) {
    wire();
    const dlg = $('#dlg-term');
    if (!dlg) return;
    // Cleared here as well as in the close handler: the flag belongs to one
    // departure, and a departure that somehow never reached that handler must
    // not silently swallow the focus restore of the next word opened.
    leaving = false;
    currentID = id;
    openerEl = opener || document.activeElement;
    fillDialog(id);
    if (!dlg.open) dlg.showModal();
    // Into the dialog, not onto its first button: the dialog carries the word
    // as its accessible name and the sentence as its content, and that is what
    // a reader who just asked "what is this word" needs read to them first.
    dlg.focus();
  }

  /* A language switch while the dialog is open. The chrome around it is
     data-i18n and i18n.js repaints that on its own; the three fields below are
     written by this file and would otherwise sit in the previous language until
     the reader closed and reopened. */
  window.addEventListener('languagechange', () => {
    const dlg = $('#dlg-term');
    if (dlg && dlg.open && currentID) fillDialog(currentID);
    // The titles on every term already on the page are the same string.
    for (const b of document.querySelectorAll('.term[data-term]')) {
      const id = b.getAttribute('data-term');
      if (byID.has(id)) b.title = shortOf(id);
    }
  });

  window.term = term;
  window.glossary = TERMS;    /* for tests and for the console */
})();
