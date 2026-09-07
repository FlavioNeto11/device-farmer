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
 *
 * # Reading a long page
 *
 * These pages are long — lease.json alone is 17 sections and 39 examples — and
 * until recently there was no way to see what was in one, no way to jump to a
 * section, and no way back to the top. Worse, opening an area appended it
 * BELOW the grid of cards and left the scroll position alone, so choosing
 * something left the reader looking at exactly what they had been looking at
 * before. Three things address that and they are all here: a table of contents
 * with a reading position, a scroll into the chosen area, and a back-to-top.
 *
 * # Language
 *
 * Reference content is per language on disk: assets/docs/<area>.json is
 * English, assets/docs/<area>.pt.json is Portuguese. Translation is being done
 * area by area, so at any moment a real farm has some areas translated and
 * some not. A missing translation must therefore be an ordinary, expected
 * state that costs the reader nothing but a sentence of explanation — never a
 * broken page, and never English silently passed off as the translation.
 * fetchDoc() below is where that happens.
 */

(() => {
  'use strict';

  /* ------------------------------------------------------------- language */

  /* i18n.js defines t() and currentLang() and is loaded before this file.
   *
   * Both are read through the two shims below rather than called directly, for
   * one reason: the language switch and these docs were built at the same time
   * by different hands, and a docs view that throws ReferenceError because the
   * switcher has not merged yet is a worse failure than a docs view that is
   * briefly English-only. The shims cost two function calls per string and
   * remove an ordering dependency between two files entirely.
   *
   * STR below is the SAME text that belongs in i18n.js's two dictionaries — it
   * is written as two flat objects, `en` and `pt`, key for key, so that it can
   * be lifted into them unchanged. It is the fallback for the window in which
   * a key has not been added there yet, not a second source of truth. When t()
   * answers with the key it was given — the usual way an i18n layer reports a
   * miss — STR answers instead. */
  function lang() {
    try {
      if (typeof window.currentLang === 'function') {
        const l = window.currentLang();
        if (l === 'pt' || l === 'en') return l;
      }
    } catch { /* a switcher mid-initialisation is not a reason to stop rendering */ }
    return 'en';
  }

  /* Interpolation is {name}. It is applied here even to a string t() returned,
   * because a dictionary that carries the placeholders but does not expand
   * them would otherwise put a literal "{n}" on the page. */
  function fill(s, vars) {
    if (!vars) return s;
    return s.replace(/\{(\w+)\}/g, (whole, k) =>
      (vars[k] === undefined || vars[k] === null ? whole : String(vars[k])));
  }

  function tr(key, vars) {
    let s = null;
    if (typeof window.t === 'function') {
      try { s = window.t(key, vars); } catch { s = null; }
    }
    if (typeof s !== 'string' || s === '' || s === key) {
      const l = STR[lang()] || STR.en;
      s = l[key] !== undefined ? l[key] : (STR.en[key] !== undefined ? STR.en[key] : key);
    }
    return fill(s, vars);
  }

  const STR = {
    en: {
      'docs.role.api': 'serves this page and the HTTP API',
      'docs.role.scheduler': 'matches queued jobs to free devices; without it nothing is ever placed',
      'docs.role.jobrunner': 'runs job specs on leased devices; without it jobs sit in running, holding a device each',
      'docs.role.reaper': 'the only automatic release path; without it an abandoned lease is never reclaimed',
      'docs.role.recovery': 'the recovery ladder; without it a stuck device stays stuck until a human acts',
      'docs.role.watchdog': 'device health; without it the fleet view goes stale and quarantine never fires',
      'docs.role.node': 'host agent on a USB host; without it recovery tiers 3 and 4 are refused',
      'docs.role.enroll': 'adopts newly plugged devices; without it a new handset never joins the fleet',
      'docs.title': 'How this farm works',
      'docs.lede': 'The top half is measured from this deployment right now. The rest is reference, ' +
        'and every example in it was executed against a running farm before it shipped.',
      'docs.capsLoading': 'Reading what this deployment can actually do…',
      'docs.capsFailTitle': 'What this deployment can do could not be observed',
      'docs.capsFailFix': 'Nothing is shown below, because nothing below would be a statement about the farm.',
      'docs.authOpenTitle': 'Authentication is disabled on this listener',
      'docs.rolesH': 'Control-plane roles, right now',
      'docs.neverBeat': 'never beat',
      'docs.secondsAgo': '{s}s ago',
      'docs.featsH': 'What is enabled, and how',
      'docs.limitsH': 'Effective limits',
      'docs.chipBuild': 'build',
      'docs.chipPlatform': 'platform',
      'docs.chipSchema': 'schema',
      'docs.chipUptime': 'uptime',
      'docs.chipAuth': 'auth',
      'docs.kindsH': 'Step kinds this server accepts',
      'docs.kindsSub': 'Read from farm.step_kinds on every load. A step marked not idempotent is never ' +
        're-run by a resume, because repeating it would repeat its side effect.',
      'docs.colKind': 'kind',
      'docs.colIdempotent': 'idempotent',
      'docs.colNeedsArtifact': 'needs artifact',
      'docs.colDoes': 'what it does',
      'docs.yes': 'yes',
      'docs.no': 'no',
      'docs.ladderH': 'The recovery ladder on this farm',
      'docs.ladderSub': 'Read from farm.recovery_tiers. A rung whose blast radius exceeds what a live ' +
        "lease's disruption policy permits is refused and the refusal recorded, not quietly downgraded.",
      'docs.colTier': 'tier',
      'docs.colRung': 'rung',
      'docs.colBlast': 'blast radius',
      'docs.colNeedsPolicy': 'needs policy',
      'docs.colCooldown': 'cooldown',
      'docs.colPerHour': 'per hour',
      'docs.referenceH': 'Reference',
      'docs.metaFailTitle': 'The reference could not be loaded',
      'docs.metaFailFix': 'The capability panel above is still accurate: it comes from the API, not from these files.',
      'docs.metaLoading': 'Loading the reference…',
      'docs.searchPlaceholder': 'Search the reference — try "fence", "shell_detached", "uhubctl"',
      'docs.searchLabel': 'Search the documentation',
      'docs.sectionsWord': 'sections',
      'docs.examplesWord': 'examples',
      'docs.gapsWord': 'gaps',
      'docs.match': 'match',
      'docs.matches': 'matches',
      'docs.provenance': 'Reference built by executing {run} examples against a running farm; ' +
        '{fixed} were wrong and were fixed, and {corrected} claims were corrected against the code they cite.',
      'docs.loading': 'Loading…',
      'docs.areaFailTitle': 'Could not load this area',
      'docs.areaFailHint': 'A restarting api, a dropped connection — nothing about this page has to be ' +
        'wrong for the fetch to fail.',
      'docs.retry': 'Try again',
      'docs.noMatch': 'Nothing in this area matches “{q}”.',
      'docs.gapsH': 'What is not built here',
      'docs.gapsSub': 'Listed because an operator finding an undocumented gap during an incident is the ' +
        'failure this section exists to prevent.',
      'docs.gapNotBuilt': 'not built',
      'docs.gapPartial': 'partial',
      'docs.gapLinuxOnly': 'linux only',
      'docs.gapUnverified': 'unverified',
      'docs.gapUnknown': 'unknown',
      'docs.gapCost': 'What it costs you today: ',
      'docs.derivedFrom': 'derived from ',
      'docs.example': 'Example',
      'docs.copy': 'copy',
      'docs.copied': 'copied',
      'docs.copyManual': 'select it',
      'docs.observedOutput': 'observed output',
      'docs.toc': 'On this page',
      'docs.backToTop': 'Back to top',
      'docs.untranslatedTitle': 'This area is not translated yet',
      'docs.untranslatedBody': 'It is being shown in English. Nothing on it is wrong — only the language is.',
      'docs.untranslatedIndexTitle': 'The list of areas is not translated yet',
      'docs.untranslatedIndexBody': 'The names and descriptions of the areas below are shown in English. ' +
        'An area can still be translated even when this list is not.'
    },
    pt: {
      'docs.role.api': 'serve esta página e a API HTTP',
      'docs.role.scheduler': 'casa jobs na fila com dispositivos livres; sem ele nada é jamais alocado',
      'docs.role.jobrunner': 'roda specs de job em dispositivos com lease; sem ele os jobs ficam em running, cada um segurando um dispositivo',
      'docs.role.reaper': 'o único caminho automático de liberação; sem ele uma lease abandonada nunca é retomada',
      'docs.role.recovery': 'a escada de recuperação; sem ela um dispositivo travado continua travado até um humano agir',
      'docs.role.watchdog': 'saúde dos dispositivos; sem ele a visão da frota fica velha e a quarentena nunca dispara',
      'docs.role.node': 'agente no host USB; sem ele os tiers 3 e 4 da recuperação são recusados',
      'docs.role.enroll': 'adota dispositivos recém-plugados; sem ele um aparelho novo nunca entra na frota',
      'docs.title': 'Como esta fazenda funciona',
      'docs.lede': 'A metade de cima é medida deste deployment agora. O resto é referência, ' +
        'e todo exemplo nela foi executado contra uma fazenda em operação antes de ser publicado.',
      'docs.capsLoading': 'Lendo o que este deployment realmente faz…',
      'docs.capsFailTitle': 'Não foi possível observar o que este deployment faz',
      'docs.capsFailFix': 'Nada é exibido abaixo, porque nada abaixo seria uma afirmação sobre esta fazenda.',
      'docs.authOpenTitle': 'A autenticação está desligada neste listener',
      'docs.rolesH': 'Papéis do control plane, agora',
      'docs.neverBeat': 'nunca bateu',
      'docs.secondsAgo': 'há {s}s',
      'docs.featsH': 'O que está habilitado, e como',
      'docs.limitsH': 'Limites efetivos',
      'docs.chipBuild': 'build',
      'docs.chipPlatform': 'plataforma',
      'docs.chipSchema': 'schema',
      'docs.chipUptime': 'no ar há',
      'docs.chipAuth': 'autenticação',
      'docs.kindsH': 'Tipos de passo que este servidor aceita',
      'docs.kindsSub': 'Lido de farm.step_kinds a cada carregamento. Um passo marcado como não idempotente ' +
        'nunca é reexecutado por um resume, porque repeti-lo repetiria seu efeito colateral.',
      'docs.colKind': 'tipo',
      'docs.colIdempotent': 'idempotente',
      'docs.colNeedsArtifact': 'exige artefato',
      'docs.colDoes': 'o que faz',
      'docs.yes': 'sim',
      'docs.no': 'não',
      'docs.ladderH': 'A escada de recuperação desta fazenda',
      'docs.ladderSub': 'Lido de farm.recovery_tiers. Um degrau cujo raio de impacto excede o que a política ' +
        'de disrupção de um lease vivo permite é recusado, e a recusa é registrada — não rebaixado em silêncio.',
      'docs.colTier': 'nível',
      'docs.colRung': 'degrau',
      'docs.colBlast': 'raio de impacto',
      'docs.colNeedsPolicy': 'exige política',
      'docs.colCooldown': 'intervalo',
      'docs.colPerHour': 'por hora',
      'docs.referenceH': 'Referência',
      'docs.metaFailTitle': 'Não foi possível carregar a referência',
      'docs.metaFailFix': 'O painel de capacidades acima continua correto: ele vem da API, não destes arquivos.',
      'docs.metaLoading': 'Carregando a referência…',
      'docs.searchPlaceholder': 'Busque na referência — tente "fence", "shell_detached", "uhubctl"',
      'docs.searchLabel': 'Buscar na documentação',
      'docs.sectionsWord': 'seções',
      'docs.examplesWord': 'exemplos',
      'docs.gapsWord': 'lacunas',
      'docs.match': 'resultado',
      'docs.matches': 'resultados',
      'docs.provenance': 'Referência construída executando {run} exemplos contra uma fazenda em operação; ' +
        '{fixed} estavam errados e foram corrigidos, e {corrected} afirmações foram corrigidas contra o ' +
        'código que citam.',
      'docs.loading': 'Carregando…',
      'docs.areaFailTitle': 'Não foi possível carregar esta área',
      'docs.areaFailHint': 'Uma api reiniciando, uma conexão perdida — nada nesta página precisa estar ' +
        'errado para a busca falhar.',
      'docs.retry': 'Tentar de novo',
      'docs.noMatch': 'Nada nesta área corresponde a “{q}”.',
      'docs.gapsH': 'O que não está construído aqui',
      'docs.gapsSub': 'Listado porque um operador descobrir uma lacuna não documentada durante um incidente ' +
        'é exatamente a falha que esta seção existe para evitar.',
      'docs.gapNotBuilt': 'não construído',
      'docs.gapPartial': 'parcial',
      'docs.gapLinuxOnly': 'só no linux',
      'docs.gapUnverified': 'não verificado',
      'docs.gapUnknown': 'desconhecido',
      'docs.gapCost': 'O que isso te custa hoje: ',
      'docs.derivedFrom': 'derivado de ',
      'docs.example': 'Exemplo',
      'docs.copy': 'copiar',
      'docs.copied': 'copiado',
      'docs.copyManual': 'selecione',
      'docs.observedOutput': 'saída observada',
      'docs.toc': 'Nesta página',
      'docs.backToTop': 'Voltar ao topo',
      'docs.untranslatedTitle': 'Esta área ainda não foi traduzida',
      'docs.untranslatedBody': 'Ela está sendo exibida em inglês. Nada nela está errado — apenas o idioma.',
      'docs.untranslatedIndexTitle': 'A lista de áreas ainda não foi traduzida',
      'docs.untranslatedIndexBody': 'Os nomes e as descrições das áreas abaixo estão sendo exibidos em inglês. ' +
        'Uma área pode estar traduzida mesmo quando esta lista não está.'
    }
  };

  /* ---------------------------------------------------------------- state */

  /* Loaded per area, kept for the session. An operator flipping between
   * Leases and Recovery while reading should not re-fetch either.
   *
   * Keyed by area AND language. It used to be keyed by area alone, which was
   * correct exactly until the reference gained a second language: switching to
   * Portuguese then served the English copy straight out of this map, with no
   * fetch to notice and nothing on screen to admit it. */
  const cache = new Map();
  const pending = new Map();
  let meta = null;
  let metaErr = null;
  let metaLang = null;
  let openArea = null;
  let query = '';

  /* One-shot: set when a card is chosen, consumed when that area's content is
   * actually on the page. It has to survive the render that happens while the
   * fetch is still in flight, and it must NOT survive into the next periodic
   * re-render, or the page would yank itself downward every few seconds. */
  let pendingScroll = false;
  let tocOpen = true;
  let spy = null;
  let scrollWired = false;

  const DOCS_BASE = 'docs/';
  const INDEX = 'index';

  /* NUL cannot appear in an area name or a language tag, so it cannot make two
   * different pairs collide into one key. */
  function ck(area, l) { return area + '\u0000' + l; }

  /* --------------------------------------------------------------- fetch */

  async function loadJSON(path) {
    const res = await fetch(DOCS_BASE + path, { headers: { accept: 'application/json' } });
    if (!res.ok) throw new Error(path + ': HTTP ' + res.status);
    return res.json();
  }

  /* The translation fallback, and the only place it happens.
   *
   * Eight areas are being translated in parallel, so "<area>.pt.json is not
   * there yet" is the normal state of a farm mid-translation, not a fault. Any
   * failure at all — a 404 from a file nobody has written, a half-written file
   * that will not parse, a proxy answering HTML — falls through to the English
   * file and marks the result. The alternatives are both worse: failing the
   * page punishes the reader for a translator's queue, and rendering English
   * without saying so tells them this IS the Portuguese text. */
  async function fetchDoc(name, l) {
    if (l && l !== 'en') {
      try {
        const d = await loadJSON(name + '.' + l + '.json');
        if (d && typeof d === 'object' && !Array.isArray(d)) return d;
      } catch { /* fall through to English, deliberately, for any reason */ }
      const d = await loadJSON(name + '.json');
      if (d && typeof d === 'object') d.englishFallback = true;
      return d;
    }
    return loadJSON(name + '.json');
  }

  function ensureMeta() {
    const l = lang();
    if (metaLang !== l) { meta = null; metaErr = null; metaLang = l; }
    if (meta || metaErr) return;
    const key = ck(INDEX, l);
    if (pending.has(key)) return;
    const p = fetchDoc(INDEX, l)
      /* The language may have changed while this was in flight; the answer
       * belongs to the language that asked for it and to no other. */
      .then((m) => { if (metaLang === l) meta = m; })
      .catch((e) => { if (metaLang === l) metaErr = e; })
      .finally(() => { pending.delete(key); window.renderDocs && window.renderDocs(); });
    pending.set(key, p);
  }

  function ensureArea(area) {
    const l = lang();
    const key = ck(area, l);
    if (cache.has(key) || pending.has(key)) return;
    const p = fetchDoc(area, l)
      .then((d) => { cache.set(key, d); })
      /* A failure is cached so the guard above does not refetch on every
       * render, and marked `retryable` so it can be thrown away deliberately.
       * Without that flag a page loaded while the api was restarting kept its
       * error for the life of the tab: the area is in `cache`, so ensureArea
       * returns on the first line and nothing ever asks again. */
      .catch((e) => { cache.set(key, { error: String(e && e.message || e), retryable: true }); })
      .finally(() => { pending.delete(key); window.renderDocs && window.renderDocs(); });
    pending.set(key, p);
  }

  /* retryArea drops a cached failure and asks again. Only a failure: dropping
   * a loaded area would refetch a document that is already correct. */
  function retryArea(area) {
    const key = ck(area, lang());
    const doc = cache.get(key);
    if (!doc || !doc.retryable) return;
    cache.delete(key);
    ensureArea(area);
    window.renderDocs && window.renderDocs();
  }

  /* The switcher dispatches this on window with detail.lang. currentLang() is
   * the authority either way, so the detail is not read: an event that arrives
   * without one — the platform fires a `languagechange` of its own when the
   * browser's language preferences change — still lands on the right answer.
   * Nothing is evicted from the cache; the new language simply misses it. */
  window.addEventListener('languagechange', () => {
    window.renderDocs && window.renderDocs();
  });

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
      el('span', { class: 'doc-ex-label' }, ex.label || tr('docs.example')),
      el('span', { class: 'doc-ex-lang' }, ex.lang || 'text'));

    const copy = el('button', {
      class: 'mini ghost', type: 'button',
      onclick: async () => {
        try {
          await navigator.clipboard.writeText(ex.code);
          copy.textContent = tr('docs.copied');
          setTimeout(() => { copy.textContent = tr('docs.copy'); }, 1200);
        } catch {
          // Clipboard is gated on a secure context, and a farm reached over
          // plain http is an ordinary way to read this page. Say so instead of
          // failing silently under the cursor.
          copy.textContent = tr('docs.copyManual');
          setTimeout(() => { copy.textContent = tr('docs.copy'); }, 1600);
        }
      }
    }, tr('docs.copy'));
    head.append(copy);
    box.append(head);

    box.append(el('pre', { class: 'doc-code' }, el('code', null, ex.code)));

    if (ex.output) {
      box.append(el('div', { class: 'doc-out-label' }, tr('docs.observedOutput')));
      box.append(el('pre', { class: 'doc-out' }, el('code', null, ex.output)));
    }
    if (ex.note) {
      const n = el('p', { class: 'doc-note' });
      n.append(inline(ex.note));
      box.append(n);
    }
    return box;
  }

  /* A table is the one block on this page whose width is written by whoever
   * wrote the content, not by the layout. It gets its own scroll box for that
   * reason — see the .doc-table comment in style.css for the 6888px-wide
   * document this used to produce. */
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
      not_built: tr('docs.gapNotBuilt'),
      partial: tr('docs.gapPartial'),
      linux_only: tr('docs.gapLinuxOnly'),
      unverified: tr('docs.gapUnverified')
    }[state] || state || tr('docs.gapUnknown');

    return el('div', { class: 'doc-gap gap-' + state },
      el('div', { class: 'doc-gap-head' },
        el('span', { class: 'chip chip-' + (state === 'not_built' ? 'bad' : 'degraded') }, label)),
      (() => { const d = el('div', { class: 'doc-gap-body' }); d.append(inline(g.what)); return d; })(),
      (() => {
        const d = el('div', { class: 'doc-gap-cons' });
        d.append(el('span', { class: 'doc-gap-cons-label' }, tr('docs.gapCost')));
        d.append(inline(g.consequence));
        return d;
      })());
  }

  /* The one thing a reader is owed when a translation is missing: which part
   * of the page they are looking at is English, said in the language they
   * asked for. Two callers — the list of areas and an area itself fall back
   * independently, and a farm mid-translation will often have one without the
   * other, so a single note at the top would be wrong half the time. */
  function langNote(titleKey, bodyKey) {
    return el('div', { class: 'doc-lang-note' },
      el('span', { class: 'doc-lang-glyph', 'aria-hidden': 'true' }, 'ⓘ'),
      el('div', null,
        el('div', { class: 'doc-lang-title' }, tr(titleKey)),
        el('div', null, tr(bodyKey))));
  }

  function sectionID(idx) { return 'doc-s-' + idx; }
  const GAPS_ID = 'doc-s-gaps';

  function sectionBlock(s, idx) {
    const sec = el('section', { class: 'doc-section', id: sectionID(idx) });

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
        el('span', { class: 'doc-src-label' }, tr('docs.derivedFrom')),
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
            el('div', { class: 'doc-warn-title' }, tr('docs.capsFailTitle')),
            el('div', null, String(e.message || e)),
            el('div', { class: 'doc-warn-fix' },
              Array.isArray(e.detail) && e.detail.length
                ? e.detail.map((p) => p.probe + ': ' + p.consequence).join(' · ')
                : tr('docs.capsFailFix'))))
        : el('div', { class: 'doc-caps-loading' }, tr('docs.capsLoading')));
      return wrap;
    }

    const b = caps.build || {}, sc = caps.schema || {}, au = caps.auth || {};

    wrap.append(el('div', { class: 'doc-caps-strip' },
      chip(tr('docs.chipBuild'), b.version || 'dev'),
      chip(tr('docs.chipPlatform'), b.platform || '—'),
      chip(tr('docs.chipSchema'), 'v' + (sc.version || 0)),
      chip(tr('docs.chipUptime'), fmtUptime(b.uptime_s)),
      chip(tr('docs.chipAuth'), au.mode || 'none', au.open ? 'bad' : 'good')));

    if (au.open) {
      wrap.append(el('div', { class: 'doc-warn' },
        el('span', { class: 'doc-warn-glyph', 'aria-hidden': 'true' }, '▲'),
        el('div', null,
          el('div', { class: 'doc-warn-title' }, tr('docs.authOpenTitle')),
          el('div', null, au.consequence || ''),
          el('div', { class: 'doc-warn-fix' }, au.fix || ''))));
    }

    // Roles. A role that is not beating is not a cosmetic gap: the reaper's own
    // gap detection reads these same heartbeats.
    const roles = caps.roles || [];
    wrap.append(el('h3', { class: 'doc-h' }, tr('docs.rolesH')));
    wrap.append(el('div', { class: 'doc-roles' }, ...roles.map((r) => {
      const ago = r.last_beat_s === null || r.last_beat_s === undefined
        ? tr('docs.neverBeat') : tr('docs.secondsAgo', { s: r.last_beat_s });
      return el('div', { class: 'doc-role ' + (r.running ? 'role-up' : 'role-down') },
        el('div', { class: 'doc-role-top' },
          el('span', { class: 'doc-role-dot', 'aria-hidden': 'true' }, r.running ? '●' : '○'),
          el('span', { class: 'doc-role-name mono' }, r.component),
          el('span', { class: 'doc-role-beat' }, ago)),
        /* The meaning is prose the API sends, and prose the API sends is
         * normally left alone — it is data, and the page renders what it was
         * given. This one is the exception, for the same reason the error codes
         * are: it is a sentence written FOR A HUMAN READING THIS PAGE, and it is
         * keyed by something that is not prose at all. The component name is an
         * identifier, stable, and the same string the process is called in every
         * log — so it bridges to a translation the way an error code does.
         *
         * A component with no entry keeps the server's English. That is the
         * right fallback: a role added to internal/api/capabilities.go tomorrow
         * appears here immediately, described, in English, rather than
         * disappearing because nobody had written a word for it yet. */
        el('div', { class: 'doc-role-meaning' },
          tr('docs.role.' + r.component) === 'docs.role.' + r.component
            ? r.meaning
            : tr('docs.role.' + r.component)));
    })));

    // Features, with the honest state of each.
    const feats = caps.features || [];
    wrap.append(el('h3', { class: 'doc-h' }, tr('docs.featsH')));
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
      wrap.append(el('h3', { class: 'doc-h' }, tr('docs.limitsH')));
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
      wrap.append(el('h3', { class: 'doc-h' }, tr('docs.kindsH')));
      wrap.append(el('p', { class: 'doc-sub' }, tr('docs.kindsSub')));
      wrap.append(tableBlock({
        columns: [tr('docs.colKind'), tr('docs.colIdempotent'),
          tr('docs.colNeedsArtifact'), tr('docs.colDoes')],
        rows: kinds.map((k) => [
          '`' + (k.kind || '') + '`',
          k.idempotent ? tr('docs.yes') : '**' + tr('docs.no') + '**',
          k.needs_artifact ? tr('docs.yes') : '—',
          k.description || ''
        ])
      }));
    }

    if (Array.isArray(tiers) && tiers.length) {
      wrap.append(el('h3', { class: 'doc-h' }, tr('docs.ladderH')));
      wrap.append(el('p', { class: 'doc-sub' }, tr('docs.ladderSub')));
      // Field names come from normTier() in app.js, not from the API payload:
      // the loader flattens blast_radius/requires_policy/cooldown_s before
      // anything renders them. Reading the API's names here produced an empty
      // blast-radius column and an empty pair of backticks where the policy
      // should be, which reads as "this rung disturbs nothing" — the opposite
      // of what tier 4 does.
      wrap.append(tableBlock({
        columns: [tr('docs.colTier'), tr('docs.colRung'), tr('docs.colBlast'),
          tr('docs.colNeedsPolicy'), tr('docs.colCooldown'), tr('docs.colPerHour'),
          tr('docs.colDoes')],
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

  /* ------------------------------------------------------------ navigation */

  function reducedMotion() {
    return typeof matchMedia === 'function'
      && matchMedia('(prefers-reduced-motion: reduce)').matches;
  }

  function goTo(node) {
    if (!node) return;
    node.scrollIntoView({ behavior: reducedMotion() ? 'auto' : 'smooth', block: 'start' });
  }

  /* The table of contents. Built from the sections that are actually on the
   * page — under a search only the matching ones are rendered, and a contents
   * listing seventeen entries where seven exist would send the reader to
   * headings that are not there.
   *
   * The entries are buttons, not links. The app routes on location.hash, so an
   * <a href="#doc-s-4"> would rewrite the route and land the reader on the
   * Fleet grid. */
  function tocBlock(entries) {
    const box = el('details', { class: 'doc-toc-box', open: tocOpen });
    // Sections, not entries: the gaps block is listed too, and counting it as
    // a section made a seventeen-section page announce eighteen.
    const sections = entries.filter((e) => e.id !== GAPS_ID).length;
    const sum = el('summary', { class: 'doc-toc-h' },
      tr('docs.toc'),
      ' ',
      el('span', { class: 'doc-toc-count' },
        '(' + sections + ' ' + tr('docs.sectionsWord') + ')'));
    box.append(sum);
    box.addEventListener('toggle', () => { tocOpen = box.open; });

    const list = el('ol', { class: 'doc-toc-list' });
    entries.forEach((e) => {
      const link = el('button', {
        type: 'button', class: 'doc-toc-link', 'data-doc-target': e.id,
        onclick: () => goTo(document.getElementById(e.id))
      },
        el('span', { class: 'doc-toc-mark', 'aria-hidden': 'true' }, '▸'),
        el('span', null, e.label));
      list.append(el('li', null, link));
    });
    box.append(list);

    return el('nav', { class: 'doc-toc', 'aria-label': tr('docs.toc') }, box);
  }

  /* Which section the reader is in. The observer is rebuilt on every render
   * because every render replaces the nodes it was watching; an observer left
   * pointing at detached sections reports nothing and the marker freezes.
   *
   * The bottom margin is the point of the rootMargin: without it every section
   * from the viewport top to the page bottom counts as visible and the marker
   * sticks to whichever happens to be first. -55% narrows "you are here" to
   * the upper part of the window, where a reader's eye actually is. */
  /* Where the "you are here" band starts, in pixels from the top of the
   * window: below the sticky header, which is 44px tall unaligned and taller
   * when it wraps. Used twice — for the observer's root margin and for the
   * "already gone past" fallback — and they have to be the same number. */
  const SPY_TOP = 72;

  function wireSpy(ids) {
    if (spy) { spy.disconnect(); spy = null; }
    if (!ids.length || typeof IntersectionObserver !== 'function') return;

    const seen = new Set();
    spy = new IntersectionObserver((entries) => {
      for (const e of entries) {
        if (e.isIntersecting) seen.add(e.target.id);
        else seen.delete(e.target.id);
      }
      let current = ids.find((id) => seen.has(id));
      if (!current) {
        // Nothing in the band: a long code block spans it, or the reader has
        // just landed at the top of an area. The answer is the last heading
        // they have already gone past. Keeping the previous answer instead
        // would be equivalent right up until the next redraw — this view is
        // rebuilt on every SSE tick, and a fresh observer has no previous
        // answer to keep, so the marker used to vanish while somebody read.
        for (const id of ids) {
          const n = document.getElementById(id);
          if (n && n.getBoundingClientRect().top < SPY_TOP) current = id;
        }
      }
      if (current) markCurrent(current);
    }, { rootMargin: '-' + SPY_TOP + 'px 0px -55% 0px', threshold: 0 });

    ids.forEach((id) => {
      const n = document.getElementById(id);
      if (n) spy.observe(n);
    });
  }

  function markCurrent(id) {
    const links = document.querySelectorAll('.doc-toc-link');
    for (const link of links) {
      const on = link.getAttribute('data-doc-target') === id;
      link.classList.toggle('is-current', on);
      if (on) link.setAttribute('aria-current', 'true');
      else link.removeAttribute('aria-current');
    }
  }

  /* One listener for the life of the page rather than one per render: the
   * button is replaced on every render, the scroll position is not. */
  function wireBackToTop() {
    if (scrollWired) return;
    scrollWired = true;
    window.addEventListener('scroll', () => {
      const b = $('.doc-totop');
      if (b) b.hidden = window.scrollY < 600;
    }, { passive: true });
  }

  function backToTop() {
    const b = el('button', {
      class: 'doc-totop', type: 'button',
      onclick: () => window.scrollTo({ top: 0, behavior: reducedMotion() ? 'auto' : 'smooth' })
    },
      el('span', { 'aria-hidden': 'true' }, '↑'),
      tr('docs.backToTop'));
    // Rendered mid-page — an SSE tick redraws this view while the operator is
    // reading — so its initial state is read from where the page actually is,
    // not assumed to be the top.
    b.hidden = window.scrollY < 600;
    return b;
  }

  /* ----------------------------------------------------------------- view */

  function renderDocs() {
    const body = $('#docs-body');
    if (!body) return;

    ensureMeta();

    const frag = document.createDocumentFragment();

    frag.append(el('div', { class: 'doc-intro doc-read' },
      el('h2', { class: 'doc-title' }, tr('docs.title')),
      el('p', { class: 'doc-lede' }, tr('docs.lede'))));

    frag.append(capabilityPanel());
    frag.append(liveVocabulary());

    // Area navigation.
    frag.append(el('h3', { class: 'doc-h doc-h-major' }, tr('docs.referenceH')));

    // index.json falls back to English on its own, and every area label and
    // blurb below comes out of it. Saying so is the same debt the per-area
    // note pays: English on the page is fine, English pretending to be the
    // translation is not.
    if (meta && meta.englishFallback) {
      frag.append(langNote('docs.untranslatedIndexTitle', 'docs.untranslatedIndexBody'));
    }

    if (metaErr) {
      frag.append(el('div', { class: 'doc-warn' },
        el('span', { class: 'doc-warn-glyph', 'aria-hidden': 'true' }, '✕'),
        el('div', null,
          el('div', { class: 'doc-warn-title' }, tr('docs.metaFailTitle')),
          el('div', null, String(metaErr.message || metaErr)),
          el('div', { class: 'doc-warn-fix' }, tr('docs.metaFailFix')))));
      body.replaceChildren(frag);
      return;
    }

    if (!meta) {
      frag.append(el('div', { class: 'doc-caps-loading' }, tr('docs.metaLoading')));
      body.replaceChildren(frag);
      return;
    }

    const search = el('input', {
      class: 'doc-search', type: 'search', value: query,
      placeholder: tr('docs.searchPlaceholder'),
      'aria-label': tr('docs.searchLabel'),
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
      const key = ck(a.area, lang());
      const hits = query && cache.has(key) ? matches(cache.get(key), query) : null;
      const isOpen = openArea === a.area;
      const card = el('button', {
        type: 'button',
        class: 'doc-area' + (isOpen ? ' is-open' : '') +
          (hits && hits.length === 0 ? ' is-dim' : ''),
        'aria-expanded': isOpen ? 'true' : 'false',
        onclick: () => {
          openArea = isOpen ? null : a.area;
          // Choosing an area used to load it below the grid and leave the
          // scroll alone, so the reader's answer to "take me to Leases" was
          // the same screenful of cards they had just been looking at.
          if (openArea) pendingScroll = true;
          renderDocs();
        }
      },
        el('div', { class: 'doc-area-label' },
          el('span', { class: 'doc-area-mark', 'aria-hidden': 'true' }, '▸'),
          a.label),
        el('div', { class: 'doc-area-blurb' }, a.blurb),
        el('div', { class: 'doc-area-meta mono' },
          a.sections + ' ' + tr('docs.sectionsWord') +
          ' · ' + a.examples + ' ' + tr('docs.examplesWord') +
          (a.gaps ? ' · ' + a.gaps + ' ' + tr('docs.gapsWord') : '') +
          (hits ? '  —  ' + hits.length + ' ' +
            tr(hits.length === 1 ? 'docs.match' : 'docs.matches') : '')));
      cards.append(card);
    });
    frag.append(cards);

    if (meta.built) {
      frag.append(el('p', { class: 'doc-provenance' }, tr('docs.provenance', {
        run: meta.built.examples_run,
        fixed: meta.built.examples_fixed,
        corrected: meta.built.claims_corrected
      })));
    }

    const anchors = [];
    if (openArea) {
      // Asked for on every render, not only on the click that opened the area.
      // The card's handler cannot be the only caller: a language switch changes
      // the cache key under an area that is already open, and with the fetch
      // living in the handler the page sat on "Loading…" for the rest of the
      // session. ensureArea is guarded by both the cache and the in-flight map,
      // so asking again costs a Map lookup.
      ensureArea(openArea);
      frag.append(areaBody(openArea, anchors));
    }

    frag.append(backToTop());

    body.replaceChildren(frag);
    wireBackToTop();
    wireSpy(anchors.map((a) => a.id));

    // Consumed only once the area has settled — loaded, or failed and said so.
    // The click fires a render while the fetch is still in flight, and moving
    // the reader to a "Loading…" line and stopping there takes them nowhere;
    // moving them to the failure, on the other hand, is the answer they asked
    // for. Both of those are "the area is in the cache".
    if (pendingScroll && openArea && cache.has(ck(openArea, lang()))) {
      pendingScroll = false;
      goTo($('.doc-body'));
    }
  }

  function areaBody(area, anchors) {
    const wrap = el('div', { class: 'doc-body' });
    const doc = cache.get(ck(area, lang()));

    if (!doc) {
      wrap.append(el('div', { class: 'doc-caps-loading' }, tr('docs.loading')));
      return wrap;
    }
    if (doc.error) {
      const again = el('button', { class: 'doc-retry', type: 'button' }, tr('docs.retry'));
      again.addEventListener('click', () => retryArea(area));
      wrap.append(el('div', { class: 'doc-warn' },
        el('span', { class: 'doc-warn-glyph', 'aria-hidden': 'true' }, '✕'),
        el('div', null, el('div', { class: 'doc-warn-title' }, tr('docs.areaFailTitle')),
          el('div', null, doc.error),
          el('div', { class: 'doc-warn-hint' }, tr('docs.areaFailHint')),
          again)));
      return wrap;
    }

    const col = el('div', { class: 'doc-doc doc-read' });

    if (doc.englishFallback) {
      col.append(langNote('docs.untranslatedTitle', 'docs.untranslatedBody'));
    }

    col.append(el('h2', { class: 'doc-area-title' }, doc.title));
    col.append(prose(doc.summary));

    const hits = query ? matches(doc, query) : null;
    const show = hits && hits.length ? new Set(hits) : null;

    if (hits && hits.length === 0) {
      col.append(el('div', { class: 'doc-empty' }, tr('docs.noMatch', { q: query })));
    }

    (doc.sections || []).forEach((s, i) => {
      if (show && !show.has(i)) return;
      col.append(sectionBlock(s, i));
      anchors.push({ id: sectionID(i), label: s.heading });
    });

    if (!show && Array.isArray(doc.gaps) && doc.gaps.length) {
      col.append(el('h3', { class: 'doc-h doc-h-major doc-anchor', id: GAPS_ID }, tr('docs.gapsH')));
      col.append(el('p', { class: 'doc-sub' }, tr('docs.gapsSub')));
      col.append(el('div', { class: 'doc-gaps' }, ...doc.gaps.map(gapBlock)));
      anchors.push({ id: GAPS_ID, label: tr('docs.gapsH') });
    }

    // Two columns only when there are two columns. The sidebar template used
    // to be unconditional, so the one-child states — "Loading…", and the
    // failure panel with its retry button — were auto-placed into the 15rem
    // TOC track and rendered as a sliver against an empty row.
    if (anchors.length) {
      wrap.classList.add('is-split');
      wrap.append(tocBlock(anchors));
    }
    wrap.append(col);
    return wrap;
  }

  window.renderDocs = renderDocs;
})();
