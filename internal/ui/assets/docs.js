/* Docs — what this system does, how it works, and what it does not do yet.
 *
 * Two halves, deliberately different in kind.
 *
 * The CAPABILITY half is observed at request time from /api/v1/capabilities:
 * schema version from the migration table, roles from their own heartbeats,
 * auth mode from the Authenticator this server was actually built with. It
 * describes THIS deployment and it changes when the deployment changes.
 *
 * The REFERENCE half is written prose, split per area and fetched only when an
 * area is opened, because 550 KB of documentation has no business loading with
 * the fleet grid. Every example in it was executed against a running farm
 * before it shipped; a third of them were wrong until they were.
 *
 * This file owns the "docs" view and nothing else. It talks to app.js through
 * three globals it does not define: el, $ and state.
 */

(() => {
  'use strict';

  /* Loaded per area, kept for the session. An operator flipping between
   * Leases and Recovery while reading should not re-fetch either. */
  const cache = new Map();
  const pending = new Map();
  let meta = null;
  let metaErr = null;
  let openArea = null;
  let query = '';

  const DOCS_BASE = 'docs/';

  /* --------------------------------------------------------------- fetch */

  async function loadJSON(path) {
    const res = await fetch(DOCS_BASE + path, { headers: { accept: 'application/json' } });
    if (!res.ok) throw new Error(path + ': HTTP ' + res.status);
    return res.json();
  }

  function ensureMeta() {
    if (meta || metaErr || pending.has('index')) return;
    const p = loadJSON('index.json')
      .then((m) => { meta = m; })
      .catch((e) => { metaErr = e; })
      .finally(() => { pending.delete('index'); window.renderDocs && window.renderDocs(); });
    pending.set('index', p);
  }

  function ensureArea(area) {
    if (cache.has(area) || pending.has(area)) return;
    const p = loadJSON(area + '.json')
      .then((d) => { cache.set(area, d); })
      .catch((e) => { cache.set(area, { error: String(e && e.message || e) }); })
      .finally(() => { pending.delete(area); window.renderDocs && window.renderDocs(); });
    pending.set(area, p);
  }

  /* ---------------------------------------------------------- inline text */

  /* A deliberately small subset: `code` and **bold**. Anything richer would
   * mean a parser, and a parser in a page that renders operator-facing text is
   * a way to turn a documentation bug into an injection. Text is appended as
   * text nodes; nothing here ever touches innerHTML. */
  function inline(text) {
    const out = document.createDocumentFragment();
    const re = /`([^`]+)`|\*\*([^*]+)\*\*/g;
    let last = 0, m;
    while ((m = re.exec(text)) !== null) {
      if (m.index > last) out.append(document.createTextNode(text.slice(last, m.index)));
      if (m[1] !== undefined) out.append(el('code', null, m[1]));
      else out.append(el('strong', null, m[2]));
      last = re.lastIndex;
    }
    if (last < text.length) out.append(document.createTextNode(text.slice(last)));
    return out;
  }

  /* Paragraph breaks are the only block structure the content carries. */
  function prose(text) {
    const wrap = el('div', { class: 'doc-prose' });
    String(text || '').split(/\n{2,}/).forEach((para) => {
      const p = el('p', null);
      p.append(inline(para.trim()));
      wrap.append(p);
    });
    return wrap;
  }

  /* --------------------------------------------------------------- blocks */

  function codeBlock(ex) {
    const box = el('div', { class: 'doc-ex' });

    const head = el('div', { class: 'doc-ex-head' },
      el('span', { class: 'doc-ex-label' }, ex.label || 'Example'),
      el('span', { class: 'doc-ex-lang' }, ex.lang || 'text'));

    const copy = el('button', {
      class: 'mini ghost', type: 'button',
      onclick: async () => {
        try {
          await navigator.clipboard.writeText(ex.code);
          copy.textContent = 'copied';
          setTimeout(() => { copy.textContent = 'copy'; }, 1200);
        } catch {
          // Clipboard is gated on a secure context, and a farm reached over
          // plain http is an ordinary way to read this page. Say so instead of
          // failing silently under the cursor.
          copy.textContent = 'select it';
          setTimeout(() => { copy.textContent = 'copy'; }, 1600);
        }
      }
    }, 'copy');
    head.append(copy);
    box.append(head);

    box.append(el('pre', { class: 'doc-code' }, el('code', null, ex.code)));

    if (ex.output) {
      box.append(el('div', { class: 'doc-out-label' }, 'observed output'));
      box.append(el('pre', { class: 'doc-out' }, el('code', null, ex.output)));
    }
    if (ex.note) {
      const n = el('p', { class: 'doc-note' });
      n.append(inline(ex.note));
      box.append(n);
    }
    return box;
  }

  function tableBlock(t) {
    if (!t || !Array.isArray(t.columns) || !Array.isArray(t.rows)) return null;
    const wrap = el('div', { class: 'tscroll doc-table' });
    const table = el('table', null,
      el('thead', null, el('tr', null, ...t.columns.map((c) => el('th', null, c)))),
      el('tbody', null, ...t.rows.map((r) => el('tr', null,
        ...r.map((cell, i) => {
          const td = el('td', null);
          td.append(inline(String(cell)));
          if (i === 0) td.className = 'doc-cell-key';
          return td;
        })))));
    wrap.append(table);
    return wrap;
  }

  function gapBlock(g) {
    const state = String(g.status || '').toLowerCase();
    const label = {
      not_built: 'not built',
      partial: 'partial',
      linux_only: 'linux only',
      unverified: 'unverified'
    }[state] || state || 'unknown';

    return el('div', { class: 'doc-gap gap-' + state },
      el('div', { class: 'doc-gap-head' },
        el('span', { class: 'chip chip-' + (state === 'not_built' ? 'bad' : 'degraded') }, label)),
      (() => { const d = el('div', { class: 'doc-gap-body' }); d.append(inline(g.what)); return d; })(),
      (() => {
        const d = el('div', { class: 'doc-gap-cons' });
        d.append(el('span', { class: 'doc-gap-cons-label' }, 'What it costs you today: '));
        d.append(inline(g.consequence));
        return d;
      })());
  }

  function sectionBlock(s, idx) {
    const sec = el('section', { class: 'doc-section', id: 'doc-s-' + idx });

    sec.append(el('h3', { class: 'doc-h' }, s.heading));
    sec.append(prose(s.body));

    if (Array.isArray(s.bullets) && s.bullets.length) {
      sec.append(el('ul', { class: 'doc-bullets' },
        ...s.bullets.map((b) => { const li = el('li', null); li.append(inline(b)); return li; })));
    }
    const tb = tableBlock(s.table);
    if (tb) sec.append(tb);

    (s.examples || []).forEach((ex) => sec.append(codeBlock(ex)));

    if (s.source) {
      sec.append(el('div', { class: 'doc-src' },
        el('span', { class: 'doc-src-label' }, 'derived from '),
        el('code', null, s.source)));
    }
    return sec;
  }

  /* ------------------------------------------------------- capability half */

  function capabilityPanel() {
    const caps = state.data.capabilities;
    const wrap = el('div', { class: 'doc-caps' });

    if (!caps) {
      /* Two different nulls. Still loading is a moment; failed to load is a
       * state, and a panel that says "reading…" forever tells the same lie the
       * endpoint itself used to tell — it answered 200 with a schema of v0 and
       * an empty fleet rather than admitting it could not see. Say which of
       * the two this is, and name what must not be concluded from the gap. */
      const e = state.errors.capabilities;
      wrap.append(e
        ? el('div', { class: 'doc-warn' },
          el('span', { class: 'doc-warn-glyph', 'aria-hidden': 'true' }, '▲'),
          el('div', null,
            el('div', { class: 'doc-warn-title' },
              'What this deployment can do could not be observed'),
            el('div', null, String(e.message || e)),
            el('div', { class: 'doc-warn-fix' },
              Array.isArray(e.detail) && e.detail.length
                ? e.detail.map((p) => p.probe + ': ' + p.consequence).join(' · ')
                : 'Nothing is shown below, because nothing below would be a statement about the farm.')))
        : el('div', { class: 'doc-caps-loading' },
          'Reading what this deployment can actually do…'));
      return wrap;
    }

    const b = caps.build || {}, sc = caps.schema || {}, au = caps.auth || {};

    wrap.append(el('div', { class: 'doc-caps-strip' },
      chip('build', b.version || 'dev'),
      chip('platform', b.platform || '—'),
      chip('schema', 'v' + (sc.version || 0)),
      chip('uptime', fmtUptime(b.uptime_s)),
      chip('auth', au.mode || 'none', au.open ? 'bad' : 'good')));

    if (au.open) {
      wrap.append(el('div', { class: 'doc-warn' },
        el('span', { class: 'doc-warn-glyph', 'aria-hidden': 'true' }, '▲'),
        el('div', null,
          el('div', { class: 'doc-warn-title' }, 'Authentication is disabled on this listener'),
          el('div', null, au.consequence || ''),
          el('div', { class: 'doc-warn-fix' }, au.fix || ''))));
    }

    // Roles. A role that is not beating is not a cosmetic gap: the reaper's own
    // gap detection reads these same heartbeats.
    const roles = caps.roles || [];
    wrap.append(el('h3', { class: 'doc-h' }, 'Control-plane roles, right now'));
    wrap.append(el('div', { class: 'doc-roles' }, ...roles.map((r) => {
      const ago = r.last_beat_s === null || r.last_beat_s === undefined
        ? 'never beat' : r.last_beat_s + 's ago';
      return el('div', { class: 'doc-role ' + (r.running ? 'role-up' : 'role-down') },
        el('div', { class: 'doc-role-top' },
          el('span', { class: 'doc-role-dot', 'aria-hidden': 'true' }, r.running ? '●' : '○'),
          el('span', { class: 'doc-role-name mono' }, r.component),
          el('span', { class: 'doc-role-beat' }, ago)),
        el('div', { class: 'doc-role-meaning' }, r.meaning));
    })));

    // Features, with the honest state of each.
    const feats = caps.features || [];
    wrap.append(el('h3', { class: 'doc-h' }, 'What is enabled, and how'));
    wrap.append(el('div', { class: 'doc-feats' }, ...feats.map((f) => {
      const st = String(f.state || '');
      return el('div', { class: 'doc-feat feat-' + st },
        el('div', { class: 'doc-feat-top' },
          el('span', { class: 'doc-feat-name' }, f.name),
          el('span', { class: 'chip ' + featChip(st) }, st.replace(/_/g, ' '))),
        el('div', { class: 'doc-feat-how mono' }, f.how || ''),
        f.detail ? (() => {
          const d = el('div', { class: 'doc-feat-detail' }); d.append(inline(f.detail)); return d;
        })() : null);
    })));

    const limits = caps.limits || {};
    const keys = Object.keys(limits);
    if (keys.length) {
      wrap.append(el('h3', { class: 'doc-h' }, 'Effective limits'));
      wrap.append(el('div', { class: 'doc-limits' }, ...keys.map((k) =>
        el('div', { class: 'doc-limit' },
          el('span', { class: 'doc-limit-k' }, k.replace(/_/g, ' ')),
          el('span', { class: 'doc-limit-v mono' }, String(limits[k]))))));
    }
    return wrap;
  }

  /* Reuses the chip vocabulary the rest of the dashboard already speaks, so a
   * state means the same thing here as it does on the fleet grid. */
  function featChip(st) {
    if (st === 'enabled') return 'chip-ok';
    if (st === 'not_built' || st === 'unavailable') return 'chip-bad';
    if (st === 'unknown') return 'chip-unknown';
    return 'chip-degraded';
  }

  function chip(k, v, tone) {
    return el('span', { class: 'doc-chip' + (tone ? ' is-' + tone : '') },
      el('span', { class: 'doc-chip-k' }, k),
      el('span', { class: 'doc-chip-v mono' }, String(v)));
  }

  /* The ladder's cooldown arrives as whole seconds. 21600 is not a cooldown a
   * reader can weigh against an incident; 6h is. */
  function fmtSeconds(v) {
    const n = Number(v);
    if (!Number.isFinite(n) || n <= 0) return '—';
    if (n < 60) return n + 's';
    if (n < 3600) return (n % 60 ? (n / 60).toFixed(1) : n / 60) + 'm';
    return (n % 3600 ? (n / 3600).toFixed(1) : n / 3600) + 'h';
  }

  function fmtUptime(s) {
    s = Number(s) || 0;
    if (s < 60) return s + 's';
    if (s < 3600) return Math.floor(s / 60) + 'm';
    if (s < 86400) return Math.floor(s / 3600) + 'h ' + Math.floor((s % 3600) / 60) + 'm';
    return Math.floor(s / 86400) + 'd ' + Math.floor((s % 86400) / 3600) + 'h';
  }

  /* ----------------------------------------------------------- live tables */

  /* The step vocabulary and the recovery ladder are rendered from the DATABASE,
   * not from the prose, so this page cannot drift from what the server will
   * accept. If somebody adds a rung, it appears here without an edit. */
  function liveVocabulary() {
    const kinds = state.data.kinds;
    const tiers = state.data.tiers;
    const wrap = el('div', null);

    if (Array.isArray(kinds) && kinds.length) {
      wrap.append(el('h3', { class: 'doc-h' }, 'Step kinds this server accepts'));
      wrap.append(el('p', { class: 'doc-sub' },
        'Read from farm.step_kinds on every load. A step marked not idempotent is never ' +
        're-run by a resume, because repeating it would repeat its side effect.'));
      wrap.append(tableBlock({
        columns: ['kind', 'idempotent', 'needs artifact', 'what it does'],
        rows: kinds.map((k) => [
          '`' + (k.kind || '') + '`',
          k.idempotent ? 'yes' : '**no**',
          k.needs_artifact ? 'yes' : '—',
          k.description || ''
        ])
      }));
    }

    if (Array.isArray(tiers) && tiers.length) {
      wrap.append(el('h3', { class: 'doc-h' }, 'The recovery ladder on this farm'));
      wrap.append(el('p', { class: 'doc-sub' },
        'Read from farm.recovery_tiers. A rung whose blast radius exceeds what a live ' +
        "lease's disruption policy permits is refused and the refusal recorded, not " +
        'quietly downgraded.'));
      // Field names come from normTier() in app.js, not from the API payload:
      // the loader flattens blast_radius/requires_policy/cooldown_s before
      // anything renders them. Reading the API's names here produced an empty
      // blast-radius column and an empty pair of backticks where the policy
      // should be, which reads as "this rung disturbs nothing" — the opposite
      // of what tier 4 does.
      wrap.append(tableBlock({
        columns: ['tier', 'rung', 'blast radius', 'needs policy', 'cooldown', 'per hour', 'what it does'],
        rows: tiers.map((t) => [
          String(t.tier),
          '`' + (t.name || '') + '`',
          '`' + String(t.blast || 'device') + '`',
          '`' + String(t.requires || '') + '`',
          fmtSeconds(t.cooldown),
          String(t.maxPerHour === undefined || t.maxPerHour === null ? '' : t.maxPerHour),
          String(t.description || '')
        ])
      }));
    }
    return wrap;
  }

  /* ------------------------------------------------------------- searching */

  function matches(doc, q) {
    if (!q) return null;
    const needle = q.toLowerCase();
    const hits = [];
    (doc.sections || []).forEach((s, i) => {
      const hay = [s.heading, s.body, (s.bullets || []).join(' '),
      (s.examples || []).map((e) => e.label + ' ' + e.code).join(' ')].join(' ').toLowerCase();
      if (hay.includes(needle)) hits.push(i);
    });
    return hits;
  }

  /* ----------------------------------------------------------------- view */

  function renderDocs() {
    const body = $('#docs-body');
    if (!body) return;

    ensureMeta();

    const frag = document.createDocumentFragment();

    frag.append(el('div', { class: 'doc-intro' },
      el('h2', { class: 'doc-title' }, 'How this farm works'),
      el('p', { class: 'doc-lede' },
        'The top half is measured from this deployment right now. The rest is reference, ' +
        'and every example in it was executed against a running farm before it shipped.')));

    frag.append(capabilityPanel());
    frag.append(liveVocabulary());

    // Area navigation.
    frag.append(el('h3', { class: 'doc-h doc-h-major' }, 'Reference'));

    if (metaErr) {
      frag.append(el('div', { class: 'doc-warn' },
        el('span', { class: 'doc-warn-glyph', 'aria-hidden': 'true' }, '✕'),
        el('div', null,
          el('div', { class: 'doc-warn-title' }, 'The reference could not be loaded'),
          el('div', null, String(metaErr.message || metaErr)),
          el('div', { class: 'doc-warn-fix' },
            'The capability panel above is still accurate: it comes from the API, not from these files.'))));
      body.replaceChildren(frag);
      return;
    }

    if (!meta) {
      frag.append(el('div', { class: 'doc-caps-loading' }, 'Loading the reference…'));
      body.replaceChildren(frag);
      return;
    }

    const search = el('input', {
      class: 'doc-search', type: 'search', value: query,
      placeholder: 'Search the reference — try "fence", "shell_detached", "uhubctl"',
      'aria-label': 'Search the documentation',
      oninput: (e) => {
        query = e.target.value;
        // Searching implies reading everything, so pull what is missing.
        if (query) meta.areas.forEach((a) => ensureArea(a.area));
        renderDocs();
        const again = $('.doc-search');
        if (again) { again.focus(); again.setSelectionRange(again.value.length, again.value.length); }
      }
    });
    frag.append(search);

    const cards = el('div', { class: 'doc-areas' });
    meta.areas.forEach((a) => {
      const hits = query && cache.has(a.area) ? matches(cache.get(a.area), query) : null;
      const isOpen = openArea === a.area;
      const card = el('button', {
        type: 'button',
        class: 'doc-area' + (isOpen ? ' is-open' : '') +
          (hits && hits.length === 0 ? ' is-dim' : ''),
        'aria-expanded': isOpen ? 'true' : 'false',
        onclick: () => { openArea = isOpen ? null : a.area; if (openArea) ensureArea(openArea); renderDocs(); }
      },
        el('div', { class: 'doc-area-label' }, a.label),
        el('div', { class: 'doc-area-blurb' }, a.blurb),
        el('div', { class: 'doc-area-meta mono' },
          a.sections + ' sections · ' + a.examples + ' examples' +
          (a.gaps ? ' · ' + a.gaps + ' gaps' : '') +
          (hits ? '  —  ' + hits.length + ' match' + (hits.length === 1 ? '' : 'es') : '')));
      cards.append(card);
    });
    frag.append(cards);

    if (meta.built) {
      frag.append(el('p', { class: 'doc-provenance' },
        'Reference built by executing ' + meta.built.examples_run + ' examples against a ' +
        'running farm; ' + meta.built.examples_fixed + ' were wrong and were fixed, and ' +
        meta.built.claims_corrected + ' claims were corrected against the code they cite.'));
    }

    if (openArea) frag.append(areaBody(openArea));

    body.replaceChildren(frag);
  }

  function areaBody(area) {
    const wrap = el('div', { class: 'doc-body' });
    const doc = cache.get(area);

    if (!doc) {
      wrap.append(el('div', { class: 'doc-caps-loading' }, 'Loading…'));
      return wrap;
    }
    if (doc.error) {
      wrap.append(el('div', { class: 'doc-warn' },
        el('span', { class: 'doc-warn-glyph', 'aria-hidden': 'true' }, '✕'),
        el('div', null, el('div', { class: 'doc-warn-title' }, 'Could not load this area'),
          el('div', null, doc.error))));
      return wrap;
    }

    wrap.append(el('h2', { class: 'doc-area-title' }, doc.title));
    wrap.append(prose(doc.summary));

    const hits = query ? matches(doc, query) : null;
    const show = hits && hits.length ? new Set(hits) : null;

    if (hits && hits.length === 0) {
      wrap.append(el('div', { class: 'doc-empty' },
        'Nothing in this area matches “' + query + '”.'));
    }

    (doc.sections || []).forEach((s, i) => {
      if (show && !show.has(i)) return;
      wrap.append(sectionBlock(s, i));
    });

    if (!show && Array.isArray(doc.gaps) && doc.gaps.length) {
      wrap.append(el('h3', { class: 'doc-h doc-h-major' }, 'What is not built here'));
      wrap.append(el('p', { class: 'doc-sub' },
        'Listed because an operator finding an undocumented gap during an incident is the ' +
        'failure this section exists to prevent.'));
      wrap.append(el('div', { class: 'doc-gaps' }, ...doc.gaps.map(gapBlock)));
    }
    return wrap;
  }

  window.renderDocs = renderDocs;
})();
