/* i18n.js — this dashboard in English and in Brazilian Portuguese.
 *
 * # What is translated, and what deliberately is not
 *
 * The INTERFACE is translated: every word this page writes itself — navigation,
 * column headings, buttons, empty states, the sentences that explain a refusal.
 *
 * Three things are NOT, and each for a reason a reader should be able to check:
 *
 *  1. IDENTIFIERS. `farm.leases`, `release_reason`, `usb:3-1.4`, `no_disruption`,
 *     `FARM_LEASE_TTL`, role names, error codes. These are what the schema, the
 *     API and the logs say. An operator matches them by eye against a psql
 *     session or a log line, and a translated identifier is one they cannot
 *     find. The status vocabulary an operator reads on a chip — `held`,
 *     `healthy`, `quarantined` — is in this category: the word is the value in
 *     the column, and the page prints the value.
 *
 *  2. DATA FROM THE SERVER. A holder's name, a job's spec, a device's model, the
 *     output of a shell command. The page renders what it was given.
 *
 *  3. THE `ctl` COMMAND LINE. It is not this page, and its output is grepped by
 *     scripts. Translating a CLI's output breaks the scripts written against it,
 *     silently, on the machines least likely to be watched.
 *
 * # How a string gets here
 *
 * `t('key')` in JS, or `data-i18n="key"` on an element in index.html. For an
 * attribute rather than the text, `data-i18n-attr="placeholder:key title:other"`.
 *
 * EVERY KEY MUST EXIST IN BOTH DICTIONARIES. `TestEveryStringExistsInBothLanguages`
 * fails otherwise, and it fails loudly rather than letting a Portuguese reader
 * meet an English word with no explanation — which is what a silent fallback
 * would produce, one string at a time, forever.
 *
 * # Where the choice lives
 *
 * In localStorage, per browser, because it is a reading preference and not a
 * property of the farm: two operators sharing a control plane can want different
 * languages, and neither should be able to change the other's. The first visit
 * takes a guess from the browser's own setting, which is the only evidence
 * available at that point.
 */

const LANG_KEY = 'device-farmer.lang';
const LANGS = ['en', 'pt'];

/* The dictionaries.
 *
 * Ordered as the page is: chrome first, then per view, then the pieces shared
 * across views. A key is a path — `fleet.filters.health` — so that a reader
 * looking at a string on screen can find it, and so that a whole view's strings
 * sit together when somebody adds a third language. */
const STRINGS = {
  en: {
    /* Header and navigation */
    'app.name': 'device-farmer',
    'app.skip': 'Skip to content',
    'nav.views': 'Views',
    'nav.fleet': 'Fleet',
    'nav.leases': 'Leases',
    'nav.jobs': 'Jobs',
    'nav.recovery': 'Recovery',
    'nav.bulk': 'Bulk',
    'nav.events': 'Events',
    'nav.docs': 'Docs',
    'header.searchLabel': 'Search devices, hosts, jobs and holders',
    'header.searchPlaceholder': 'Search  ( / )',
    'header.refresh': 'Refresh',
    'header.refreshTitle': 'Refetch every view now',
    'header.token': 'Token',
    'header.tokenTitle': 'Set the bearer token this browser sends to the API',
    'header.language': 'Language',
    'header.languageTitle': 'Read this dashboard in English or Portuguese',

    /* Connection state. These are about THIS page's connection to the API and
     * say nothing about any lease — see conn.downNote. */
    'conn.live': 'live',
    'conn.polling': 'polling',
    'conn.connecting': 'connecting',
    'conn.down': 'no connection',
    'conn.downNote': 'This page cannot reach the API. It says nothing about the farm: leases are held in PostgreSQL and are unaffected by a browser that lost its connection.',

    /* Filters, shared across views */
    'filter.host': 'Host',
    'filter.hub': 'Hub',
    'filter.health': 'Health',
    'filter.pool': 'Pool',
    'filter.lease': 'Lease',
    'filter.state': 'State',
    'filter.allHosts': 'all hosts',
    'filter.allHubs': 'all hubs',
    'filter.allPools': 'all pools',
    'filter.anyHealth': 'any health',
    'filter.anyLeaseState': 'any lease state',
    'filter.anyState': 'any state',
    'filter.clear': 'Clear',
    'filter.anyFault': 'not healthy (any fault)',

    /* Fleet */
    'count.total': 'total',
    'count.leased': 'leased',
    'count.free': 'free',
    'count.unhealthy': 'unhealthy',
    'count.quarantined': 'quarantined',
    'count.protected': 'protected',
    'count.devices': 'devices',
    'count.live': 'live',
    'count.expired': 'expired',
    'count.released': 'released',
    'count.running': 'running',
    'count.queued': 'queued',
    'count.failed': 'failed',
    'count.succeeded': 'succeeded',
    'fleet.total': 'total',
    'fleet.leased': 'leased',
    'fleet.free': 'free',
    'fleet.unhealthy': 'unhealthy',
    'fleet.quarantined': 'quarantined',
    'fleet.protected': 'protected',
    'fleet.showing': 'showing',
    'fleet.devices': 'devices',
    'fleet.liveLeases': 'live leases',
    'fleet.seen': 'seen',
    'fleet.drain': 'Drain',
    'fleet.showThatHub': 'Show that hub',
    'fleet.empty': 'No device matches these filters',
    'fleet.emptyDetail': 'Clear the filters, or widen them. Nothing is wrong with the farm.',

    /* Device drawer */
    'device.identity': 'Identity',
    'device.position': 'Physical position',
    'device.health': 'Health',
    'device.lease': 'Lease',
    'device.actions': 'Operator actions',
    'device.exec': 'Run one ADB command',
    'device.screen': 'Screen',
    'device.raw': 'Raw API row',
    'device.rawSummary': 'every field the API returned',
    'device.loading': 'Loading device…',
    'device.noLease': 'No live lease.',
    'device.noLeaseNote': 'This device is free. Health has nothing to do with that: an offline device can still be held, and a healthy one can be idle.',
    'device.close': 'Close',
    'device.run': 'Run',
    'device.powerCycle': 'Power-cycle slot',
    'device.drainHost': 'Drain host {host}',
    'device.refused': 'refused: {err}',

    /* The live screen */
    'screen.open': 'Open screen',
    'screen.close': 'Close screen',
    'screen.starting': 'Starting the server on the device…',
    'screen.streaming': 'Streaming {w}×{h}. Click and drag on the picture; the buttons below send keys.',
    'screen.ended': 'The stream ended ({why}). This says nothing about the lease: a screen session ends bytes, and the device is exactly as leased as it was before.',
    'screen.closedByOperator': 'closed by the operator',
    'screen.closedDrawer': 'the device drawer was closed',
    'screen.inputFailed': 'Input was not delivered: {err}',
    'screen.remedy': 'Remedy: {remedy}',
    'screen.noWebCodecs': 'This browser has no WebCodecs decoder, so a live screen cannot be shown here. The stream itself is fine: ctl device screen {id} --out screen.h264 records it.',
    'screen.key.back': 'Back',
    'screen.key.home': 'Home',
    'screen.key.recents': 'Recents',
    'screen.key.power': 'Power',

    /* Panel and table states */
    'state.loading': 'Loading…',
    'state.failed': 'Could not be loaded',
    'state.empty': 'Nothing here',
    'state.none': '—',

    /* Errors, by the code the API returns. The server also sends a message; it
     * is shown when a code has no entry here, so a new code degrades to English
     * prose rather than to nothing. */
    'error.unauthenticated': 'This API needs a credential. Set a token.',
    'error.forbidden': 'Your credential does not have the role this route needs.',
    'error.not_found': 'Not found.',
    'error.conflict': 'Refused: this would disturb something that is running.',
    'error.fenced': 'This lease is over. It is terminal — do not retry.',
    'error.transient': 'The database did not answer. The lease is untouched; retry.',
    'error.unavailable': 'This farm is not configured for that.',
    'error.timeout': 'Timed out.',
    'error.adb_error': 'The device did not answer. No lease was affected.',
    'error.bad_request': 'The request was refused as malformed.',
    'error.unreachable': 'The control plane is unreachable from this browser.',
    'error.internal': 'The control plane failed to answer.',
    'error.invalid_json': 'The request body is not valid JSON.',
    'error.check_violation': 'The database refused this: it violates a constraint the schema enforces.',
    'error.no_capacity': 'No device in this pool is free. Nothing failed; there is nothing to give yet.',
    'error.disruption_refused': 'Refused: this would disturb more than the live lease on that device permits. The refusal is recorded rather than quietly downgraded.',
    'error.host_agent': 'The host agent did not answer. No lease was affected.',
    'error.exec_not_admitted': 'This farm enforces the fence at the device, and an operator shell cannot be admitted through it. No safe pattern exists for an arbitrary command line.',
    'error.ui_not_mounted': 'This process serves the API but not the dashboard.',
    'error.client_closed': 'The client hung up before the answer was written.',

    /* Auth */
    'auth.needed': 'This API needs a credential.',
    'auth.setToken': 'Set API token',
    'auth.tokenTitle': 'API token',
    'auth.tokenNote': 'Stored in this tab only, and sent as a header — never in a URL.',
    'auth.save': 'Save',
    'auth.cancel': 'Cancel',

    /* Confirmations */
    'confirm.title': 'Confirm',
    'confirm.reason': 'Reason',
    'confirm.reasonNote': 'Recorded in farm.audit_log next to your name. Six weeks from now it is the only record of why.',
    'confirm.yes': 'Confirm',
    'confirm.no': 'Cancel',
  },

  pt: {
    /* Cabeçalho e navegação */
    'app.name': 'device-farmer',
    'app.skip': 'Ir para o conteúdo',
    'nav.views': 'Visões',
    'nav.fleet': 'Frota',
    'nav.leases': 'Leases',
    'nav.jobs': 'Jobs',
    'nav.recovery': 'Recuperação',
    'nav.bulk': 'Em massa',
    'nav.events': 'Eventos',
    'nav.docs': 'Docs',
    'header.searchLabel': 'Buscar dispositivos, hosts, jobs e holders',
    'header.searchPlaceholder': 'Buscar  ( / )',
    'header.refresh': 'Atualizar',
    'header.refreshTitle': 'Rebuscar todas as visões agora',
    'header.token': 'Token',
    'header.tokenTitle': 'Definir o bearer token que este navegador envia à API',
    'header.language': 'Idioma',
    'header.languageTitle': 'Ler este painel em inglês ou português',

    /* Estado da conexão. É sobre a conexão DESTA página com a API e não diz
     * nada sobre nenhuma lease — veja conn.downNote. */
    'conn.live': 'ao vivo',
    'conn.polling': 'consultando',
    'conn.connecting': 'conectando',
    'conn.down': 'sem conexão',
    'conn.downNote': 'Esta página não alcança a API. Isso não diz nada sobre a fazenda: as leases vivem no PostgreSQL e não são afetadas por um navegador que perdeu a conexão.',

    /* Filtros, comuns a várias visões */
    'filter.host': 'Host',
    'filter.hub': 'Hub',
    'filter.health': 'Saúde',
    'filter.pool': 'Pool',
    'filter.lease': 'Lease',
    'filter.state': 'Estado',
    'filter.allHosts': 'todos os hosts',
    'filter.allHubs': 'todos os hubs',
    'filter.allPools': 'todos os pools',
    'filter.anyHealth': 'qualquer saúde',
    'filter.anyLeaseState': 'qualquer estado de lease',
    'filter.anyState': 'qualquer estado',
    'filter.clear': 'Limpar',
    'filter.anyFault': 'com qualquer falha',

    /* Frota */
    'count.total': 'total',
    'count.leased': 'com lease',
    'count.free': 'livres',
    'count.unhealthy': 'com problema',
    'count.quarantined': 'em quarentena',
    'count.protected': 'protegidos',
    'count.devices': 'dispositivos',
    'count.live': 'vivas',
    'count.expired': 'expiradas',
    'count.released': 'liberadas',
    'count.running': 'rodando',
    'count.queued': 'na fila',
    'count.failed': 'falharam',
    'count.succeeded': 'sucesso',
    'fleet.total': 'total',
    'fleet.leased': 'com lease',
    'fleet.free': 'livres',
    'fleet.unhealthy': 'com problema',
    'fleet.quarantined': 'em quarentena',
    'fleet.protected': 'protegidos',
    'fleet.showing': 'mostrando',
    'fleet.devices': 'dispositivos',
    'fleet.liveLeases': 'leases vivas',
    'fleet.seen': 'visto',
    'fleet.drain': 'Drenar',
    'fleet.showThatHub': 'Mostrar esse hub',
    'fleet.empty': 'Nenhum dispositivo corresponde a estes filtros',
    'fleet.emptyDetail': 'Limpe os filtros, ou alargue-os. Não há nada errado com a fazenda.',

    /* Gaveta do dispositivo */
    'device.identity': 'Identidade',
    'device.position': 'Posição física',
    'device.health': 'Saúde',
    'device.lease': 'Lease',
    'device.actions': 'Ações de operador',
    'device.exec': 'Rodar um comando ADB',
    'device.screen': 'Tela',
    'device.raw': 'Linha crua da API',
    'device.rawSummary': 'todos os campos que a API devolveu',
    'device.loading': 'Carregando dispositivo…',
    'device.noLease': 'Nenhuma lease viva.',
    'device.noLeaseNote': 'Este dispositivo está livre. Saúde não tem relação com isso: um dispositivo offline pode estar com lease, e um saudável pode estar ocioso.',
    'device.close': 'Fechar',
    'device.run': 'Rodar',
    'device.powerCycle': 'Ciclar energia do slot',
    'device.drainHost': 'Drenar host {host}',
    'device.refused': 'recusado: {err}',

    /* A tela ao vivo */
    'screen.open': 'Abrir tela',
    'screen.close': 'Fechar tela',
    'screen.starting': 'Iniciando o servidor no dispositivo…',
    'screen.streaming': 'Transmitindo {w}×{h}. Clique e arraste na imagem; os botões abaixo enviam teclas.',
    'screen.ended': 'A transmissão terminou ({why}). Isso não diz nada sobre a lease: uma sessão de tela termina bytes, e o dispositivo continua exatamente tão alugado quanto estava.',
    'screen.closedByOperator': 'fechada pelo operador',
    'screen.closedDrawer': 'a gaveta do dispositivo foi fechada',
    'screen.inputFailed': 'O input não foi entregue: {err}',
    'screen.remedy': 'Como resolver: {remedy}',
    'screen.noWebCodecs': 'Este navegador não tem o decodificador WebCodecs, então a tela ao vivo não pode ser mostrada aqui. A transmissão em si está fina: ctl device screen {id} --out screen.h264 grava ela.',
    'screen.key.back': 'Voltar',
    'screen.key.home': 'Início',
    'screen.key.recents': 'Recentes',
    'screen.key.power': 'Ligar',

    /* Estados de painel e tabela */
    'state.loading': 'Carregando…',
    'state.failed': 'Não foi possível carregar',
    'state.empty': 'Nada aqui',
    'state.none': '—',

    /* Erros, pelo código que a API devolve. O servidor também manda uma
     * mensagem; ela aparece quando um código não tem entrada aqui, então um
     * código novo degrada para prosa em inglês em vez de para nada. */
    'error.unauthenticated': 'Esta API exige uma credencial. Defina um token.',
    'error.forbidden': 'Sua credencial não tem o papel que esta rota exige.',
    'error.not_found': 'Não encontrado.',
    'error.conflict': 'Recusado: isso perturbaria algo que está rodando.',
    'error.fenced': 'Esta lease acabou. É terminal — não tente de novo.',
    'error.transient': 'O banco não respondeu. A lease está intacta; tente de novo.',
    'error.unavailable': 'Esta fazenda não está configurada para isso.',
    'error.timeout': 'Tempo esgotado.',
    'error.adb_error': 'O dispositivo não respondeu. Nenhuma lease foi afetada.',
    'error.bad_request': 'A requisição foi recusada como malformada.',
    'error.unreachable': 'O control plane está inalcançável deste navegador.',
    'error.internal': 'O control plane falhou ao responder.',
    'error.invalid_json': 'O corpo da requisição não é um JSON válido.',
    'error.check_violation': 'O banco recusou isto: viola uma constraint que o schema impõe.',
    'error.no_capacity': 'Nenhum dispositivo deste pool está livre. Nada falhou; ainda não há o que entregar.',
    'error.disruption_refused': 'Recusado: isto perturbaria mais do que a lease viva nesse dispositivo permite. A recusa é registrada em vez de ser silenciosamente rebaixada.',
    'error.host_agent': 'O agente do host não respondeu. Nenhuma lease foi afetada.',
    'error.exec_not_admitted': 'Esta fazenda impõe a fence no dispositivo, e um shell de operador não pode ser admitido através dela. Não existe padrão seguro para uma linha de comando arbitrária.',
    'error.ui_not_mounted': 'Este processo serve a API, mas não o painel.',
    'error.client_closed': 'O cliente desligou antes de a resposta ser escrita.',

    /* Autenticação */
    'auth.needed': 'Esta API exige uma credencial.',
    'auth.setToken': 'Definir token da API',
    'auth.tokenTitle': 'Token da API',
    'auth.tokenNote': 'Guardado só nesta aba, e enviado como header — nunca numa URL.',
    'auth.save': 'Salvar',
    'auth.cancel': 'Cancelar',

    /* Confirmações */
    'confirm.title': 'Confirmar',
    'confirm.reason': 'Motivo',
    'confirm.reasonNote': 'Registrado em farm.audit_log ao lado do seu nome. Daqui a seis semanas é o único registro do porquê.',
    'confirm.yes': 'Confirmar',
    'confirm.no': 'Cancelar',
  },
};

/* pickInitialLang guesses once, from the only evidence there is.
 *
 * localStorage first, because a choice already made outranks a guess. Then the
 * browser's own languages: a reader whose browser is set to pt-BR is told, by
 * their own configuration, which language they read. Anything else is English,
 * because English is what every string in this file is guaranteed to have. */
function pickInitialLang() {
  try {
    const saved = localStorage.getItem(LANG_KEY);
    if (LANGS.includes(saved)) return saved;
  } catch (_) { /* private mode: fall through to the browser's setting */ }

  const wanted = navigator.languages || [navigator.language || ''];
  for (const tag of wanted) {
    const base = String(tag).toLowerCase().split('-')[0];
    if (LANGS.includes(base)) return base;
  }
  return 'en';
}

let lang = pickInitialLang();

/* currentLang is what everything else reads. */
function currentLang() { return lang; }

/* setLang switches the page.
 *
 * It re-applies every marked element, sets the document's own lang attribute —
 * which is what a screen reader uses to choose a voice, and the one part of this
 * that is not cosmetic — and announces the change so that views holding their
 * own rendered DOM can redraw. docs.js listens for it to refetch content in the
 * new language. */
function setLang(next) {
  if (!LANGS.includes(next) || next === lang) return;
  lang = next;
  try { localStorage.setItem(LANG_KEY, next); } catch (_) { /* not fatal */ }
  document.documentElement.lang = next === 'pt' ? 'pt-BR' : 'en';
  applyTranslations(document);
  window.dispatchEvent(new CustomEvent('languagechange', { detail: { lang: next } }));
}

/* t looks a key up in the current language.
 *
 * A MISSING KEY RETURNS THE KEY ITSELF, loudly and visibly, rather than falling
 * back to English. A silent English fallback is how a half-translated interface
 * survives for years: nobody can tell a deliberate English word from a forgotten
 * one. `nav.fleet` on screen is ugly and gets fixed;
 * TestEveryStringExistsInBothLanguages is what stops it reaching a reader at
 * all.
 *
 * vars interpolate as {name}. They are substituted with String(), never parsed,
 * so a value that happens to contain braces cannot introduce a second
 * substitution. */
function t(key, vars) {
  const table = STRINGS[lang] || STRINGS.en;
  let s = table[key];
  if (s === undefined) return key;
  if (vars) {
    for (const k of Object.keys(vars)) {
      s = s.split('{' + k + '}').join(String(vars[k]));
    }
  }
  return s;
}

/* applyTranslations fills in every element marked in the HTML.
 *
 * `data-i18n` sets textContent. `data-i18n-attr` sets attributes, as
 * space-separated `attribute:key` pairs — which is how a placeholder, a title
 * and an aria-label get translated without a wrapper element each.
 *
 * textContent rather than innerHTML, here as everywhere in this app: a
 * dictionary is a file in this repository today, and the day somebody loads one
 * from anywhere else is the day innerHTML becomes a script injection. */
function applyTranslations(root) {
  const scope = root || document;
  for (const el of scope.querySelectorAll('[data-i18n]')) {
    el.textContent = t(el.getAttribute('data-i18n'));
  }
  for (const el of scope.querySelectorAll('[data-i18n-attr]')) {
    for (const pair of el.getAttribute('data-i18n-attr').split(/\s+/)) {
      const at = pair.indexOf(':');
      if (at <= 0) continue;
      el.setAttribute(pair.slice(0, at), t(pair.slice(at + 1)));
    }
  }
}

/* The keys, for the guard test and for anybody adding a language. */
function i18nKeys(which) { return Object.keys(STRINGS[which] || {}); }

/* Applied as early as the parser reaches this file, so the first paint is
 * already in the right language rather than flashing English and correcting
 * itself. The DOM below this script does not exist yet, so a second pass runs
 * on DOMContentLoaded. */
document.documentElement.lang = lang === 'pt' ? 'pt-BR' : 'en';
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', () => applyTranslations(document));
} else {
  applyTranslations(document);
}
