'use strict';

/* device-farmer operator dashboard.
 *
 * Two rules shape every line below.
 *
 * 1. Nothing here invents data. Every number, name and timestamp on the
 *    screen came out of /api/v1. Where an endpoint returns nothing, the page
 *    says so and says what would have appeared there. A dashboard that
 *    guesses is worse than no dashboard, because an operator acts on it.
 *
 * 2. Nothing here decides that a lease is over. Deadlines are rendered from
 *    server-supplied instants and are display only: Postgres owns now(), and
 *    the browser clock has no standing. The UI never compares a local clock
 *    against a deadline to conclude anything, and never sends a client
 *    timestamp anywhere.
 */

/* ------------------------------------------------------------------ *
 * Small helpers
 * ------------------------------------------------------------------ */

const API_BASE = new URL('api/v1/', document.baseURI);

const $ = (sel, root) => (root || document).querySelector(sel);
const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));

/* el builds DOM nodes. Everything user- or server-supplied lands via
 * textContent or createTextNode, so there is no HTML injection path in this
 * app at all: no innerHTML is used anywhere. */
function el(tag, props, ...kids) {
  const n = document.createElement(tag);
  if (props) {
    for (const k of Object.keys(props)) {
      const v = props[k];
      if (v === null || v === undefined || v === false) continue;
      if (k === 'class') n.className = v;
      else if (k === 'text') n.textContent = v;
      else if (k === 'dataset') Object.assign(n.dataset, v);
      else if (k === 'style') Object.assign(n.style, v);
      else if (k.startsWith('on') && typeof v === 'function') n.addEventListener(k.slice(2), v);
      else if (v === true) n.setAttribute(k, '');
      else n.setAttribute(k, String(v));
    }
  }
  append(n, kids);
  return n;
}

function append(node, kids) {
  for (const kid of kids) {
    if (kid === null || kid === undefined || kid === false || kid === '') continue;
    if (Array.isArray(kid)) { append(node, kid); continue; }
    node.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
}

const camelCache = new Map();
function camel(s) {
  let v = camelCache.get(s);
  if (v === undefined) {
    v = s.replace(/_([a-z0-9])/g, (_, c) => c.toUpperCase());
    camelCache.set(s, v);
  }
  return v;
}

/* pick reads the first present key. The API is written by another hand; this
 * accepts snake_case (the column names in farm.v_fleet) and camelCase
 * equally rather than silently rendering blanks if a json tag differs. */
function pick(obj, ...keys) {
  if (!obj || typeof obj !== 'object') return undefined;
  for (const k of keys) {
    if (obj[k] !== undefined && obj[k] !== null) return obj[k];
    const c = camel(k);
    if (obj[c] !== undefined && obj[c] !== null) return obj[c];
  }
  return undefined;
}

/* listOf pulls an array out of a response that may be the array itself, or an
 * envelope naming it. */
function listOf(resp, ...keys) {
  if (Array.isArray(resp)) return resp;
  if (!resp || typeof resp !== 'object') return [];
  for (const k of keys) {
    const v = pick(resp, k);
    if (Array.isArray(v)) return v;
  }
  return [];
}

const nz = (v) => (v === undefined || v === null || v === '' ? null : v);
const shortId = (id) => (typeof id === 'string' && id.length > 12 ? id.slice(0, 8) : id || '');

function cmp(a, b) {
  return String(a === undefined || a === null ? '' : a)
    .localeCompare(String(b === undefined || b === null ? '' : b), undefined, { numeric: true, sensitivity: 'base' });
}

/* ------------------------------------------------------------------ *
 * Time. Rendered, never reasoned with.
 * ------------------------------------------------------------------ */

function parseTime(v) {
  if (v === null || v === undefined || v === '') return null;
  if (typeof v === 'number') return new Date(v > 1e11 ? v : v * 1000);
  const t = Date.parse(v);
  return Number.isNaN(t) ? null : new Date(t);
}

function fmtClock(v) {
  const d = parseTime(v);
  if (!d) return '';
  return d.toLocaleTimeString(undefined, { hour12: false });
}

function fmtAbs(v) {
  const d = parseTime(v);
  return d ? d.toISOString().replace('T', ' ').replace('.000Z', 'Z') : '';
}

function fmtRel(v) {
  const d = parseTime(v);
  if (!d) return '';
  const secs = Math.round((Date.now() - d.getTime()) / 1000);
  const ago = secs >= 0;
  const s = Math.abs(secs);
  let out;
  if (s < 45) out = s + 's';
  else if (s < 3600) out = Math.round(s / 60) + 'm';
  else if (s < 86400) out = Math.floor(s / 3600) + 'h' + (Math.round((s % 3600) / 60) || '') + (Math.round((s % 3600) / 60) ? 'm' : '');
  else out = Math.floor(s / 86400) + 'd' + (Math.floor((s % 86400) / 3600) || '') + (Math.floor((s % 86400) / 3600) ? 'h' : '');
  return ago ? out + ' ago' : 'in ' + out;
}

/* timeCell shows the relative distance, which is what an operator reads, with
 * the exact server instant on hover and in the accessible name. */
function timeCell(v, cls) {
  const d = parseTime(v);
  if (!d) return el('span', { class: 'chip chip-plain', title: 'not reported by the API' }, '—');
  return el('time', { class: cls || null, datetime: d.toISOString(), title: fmtAbs(v) + '  (' + d.toLocaleString() + ')' }, fmtRel(v));
}

/* fmtInterval accepts what Postgres intervals arrive as: seconds, an ISO8601
 * duration, or "HH:MM:SS". */
function fmtInterval(v) {
  if (v === null || v === undefined || v === '') return '—';
  if (typeof v === 'number') return fmtSecs(v);
  const s = String(v);
  let m = s.match(/^(\d+):(\d{2}):(\d{2})/);
  if (m) return fmtSecs(+m[1] * 3600 + +m[2] * 60 + +m[3]);
  m = s.match(/^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:([\d.]+)S)?)?$/);
  if (m) return fmtSecs((+m[1] || 0) * 86400 + (+m[2] || 0) * 3600 + (+m[3] || 0) * 60 + (+m[4] || 0));
  return s;
}

function fmtSecs(s) {
  s = Math.round(Number(s) || 0);
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm' + (s % 60 ? (s % 60) + 's' : '');
  if (s < 86400) return Math.floor(s / 3600) + 'h' + (Math.floor((s % 3600) / 60) ? Math.floor((s % 3600) / 60) + 'm' : '');
  return Math.floor(s / 86400) + 'd' + (Math.floor((s % 86400) / 3600) ? Math.floor((s % 86400) / 3600) + 'h' : '');
}

/* ------------------------------------------------------------------ *
 * Vocabulary. Colour is paired with a glyph and a word everywhere, so no
 * state on this page is legible only to someone who sees colour.
 * ------------------------------------------------------------------ */

const HEALTH_GLYPH = {
  healthy: '✓', booting: '↑', recovering: '↻', degraded: '▲', unauthorized: '⚠',
  offline: '✕', missing: '⊘', quarantined: '■', retired: '–', unknown: '?',
  // Out of service on purpose — a charge limiter holding a battery, or an
  // operator who said why. A pause glyph rather than a fault glyph, because
  // the entire point of the state is that this device is not broken.
  parked: '❙❙'
};

/* NOT_A_FAULT holds the health values that mean somebody DECIDED this device
 * is out of service, as opposed to something having broken: 'retired', and
 * 'parked' — a charge limiter holding a battery between 40% and 80%, or an
 * operator who took a handset out and recorded why.
 *
 * Every "how many are bad" number on this page goes through isFault, because
 * this page renders four of them — the fleet tab pip, the host header, the hub
 * card and the health filter — and they used to carry four copies of the same
 * predicate. They must also agree with farm.v_hub_health and with the API's
 * "unhealthy" pseudo-value in internal/api/fleet.go, or one panel reports four
 * failing devices while the panel beside it reports none. */
const NOT_A_FAULT = new Set(['healthy', 'retired', 'parked']);

function isFault(h) { return !!h && !NOT_A_FAULT.has(h); }

const OUTCOME_CLASS = {
  recovered: 'chip-healthy', no_change: 'chip-plain', failed: 'chip-offline',
  refused: 'chip-degraded', aborted: 'chip-unknown'
};

const JOB_CLASS = {
  queued: 'chip-plain', allocating: 'chip-booting', running: 'chip-held',
  succeeded: 'chip-healthy', failed: 'chip-offline', cancelled: 'chip-unknown'
};

const TARGET_CLASS = {
  pending: 'chip-plain', running: 'chip-held', ok: 'chip-healthy',
  error: 'chip-offline', skipped: 'chip-unknown'
};

function healthChip(h) {
  const key = h || 'unknown';
  const glyph = HEALTH_GLYPH[key] || '?';
  return el('span', { class: 'chip chip-' + key, title: 'device health: ' + key },
    el('span', { 'aria-hidden': 'true' }, glyph), key);
}

function leaseChips(d) {
  const out = [];
  const st = d.leaseState;
  if (!st || st === 'released' || st === 'expired') {
    out.push(el('span', { class: 'chip chip-free', title: 'no live lease on this device' },
      el('span', { 'aria-hidden': 'true' }, '○'), 'free'));
    return out;
  }
  const glyph = st === 'suspect' ? '◐' : '●';
  const title = st === 'suspect'
    ? 'suspect: no heartbeat from the holder. The device is NOT released and the job may be running fine.'
    : 'held: the holder is heartbeating';
  out.push(el('span', { class: 'chip chip-' + st, title }, el('span', { 'aria-hidden': 'true' }, glyph), st));
  if (d.protected) {
    out.push(el('span', { class: 'chip chip-protected', title: 'protected: the reaper will never reclaim this lease; only the job or a human ends it' },
      el('span', { 'aria-hidden': 'true' }, '★'), 'protected'));
  } else {
    out.push(el('span', { class: 'chip chip-plain', title: 'plain lease: reclaimable by the reaper after TTL + grace of holder silence' }, 'plain'));
  }
  return out;
}

function batteryEl(pct) {
  if (pct === null || pct === undefined) {
    return el('span', { class: 'batt', title: 'battery not reported' }, 'batt —');
  }
  const p = Math.max(0, Math.min(100, Number(pct)));
  const cls = p <= 15 ? 'batt low' : p <= 40 ? 'batt mid' : 'batt';
  const fill = el('span', { class: 'fill' });
  fill.style.width = p + '%';
  return el('span', { class: cls, title: 'battery ' + p + '%' },
    el('span', { class: 'meter', 'aria-hidden': 'true' }, fill), p + '%');
}

/* ------------------------------------------------------------------ *
 * Normalisers — one shape per resource, tolerant about field naming.
 * ------------------------------------------------------------------ */

function normDevice(raw) {
  // The API nests the live lease under "lease" and omits it entirely when the
  // device is free — a deliberate shape, so "no lease" cannot be misread as
  // "unreachable". A flat row (lease_state alongside health) is accepted too.
  const L = pick(raw, 'lease');
  return {
    raw,
    id: pick(raw, 'device_id', 'id'),
    farmUID: pick(raw, 'farm_uid'),
    serial: pick(raw, 'adb_serial', 'serial'),
    serialAmbiguous: pick(raw, 'serial_ambiguous') === true,
    model: pick(raw, 'model'),
    manufacturer: pick(raw, 'manufacturer'),
    android: pick(raw, 'android_release'),
    sdk: pick(raw, 'sdk_int'),
    pool: pick(raw, 'pool_id', 'pool'),
    adminState: pick(raw, 'admin_state'),
    labels: pick(raw, 'labels'),
    failureScore: pick(raw, 'failure_score'),
    slotID: pick(raw, 'slot_id'),
    rackSlot: pick(raw, 'rack_slot'),
    usbPath: pick(raw, 'usb_path'),
    devPath: pick(raw, 'adb_devpath'),
    slotState: pick(raw, 'slot_state'),
    hubID: pick(raw, 'hub_id'),
    hubPath: pick(raw, 'hub_path'),
    host: pick(raw, 'host_id', 'host'),
    hostAdminState: pick(raw, 'host_admin_state'),
    adbState: pick(raw, 'adb_state'),
    health: pick(raw, 'health') || 'unknown',
    healthSince: pick(raw, 'health_since'),
    battery: pick(raw, 'battery_pct'),
    batteryTempDC: pick(raw, 'battery_temp_dc'),
    consecBad: pick(raw, 'consec_bad'),
    ladderTier: pick(raw, 'ladder_tier'),
    lastSeen: pick(raw, 'last_seen_at'),
    leaseID: L ? pick(L, 'id', 'lease_id') : pick(raw, 'lease_id'),
    fence: L ? pick(L, 'fence') : pick(raw, 'fence'),
    leaseState: L ? (pick(L, 'state') || 'held') : pick(raw, 'lease_state'),
    protected: pick(L || raw, 'protected') === true,
    jobID: pick(L || raw, 'job_id'),
    tenant: pick(L || raw, 'tenant_id', 'tenant'),
    holder: pick(L || raw, 'holder'),
    acquiredAt: pick(L || raw, 'acquired_at'),
    expiresAt: pick(L || raw, 'expires_at'),
    reclaimableAt: pick(L || raw, 'reclaimable_at'),
    quarantineID: pick(raw, 'quarantine_id'),
    quarantineReason: pick(raw, 'quarantine_reason')
  };
}

function normLease(raw) {
  return {
    raw,
    id: pick(raw, 'lease_id', 'id'),
    fence: pick(raw, 'fence'),
    state: pick(raw, 'state', 'lease_state') || 'unknown',
    protected: pick(raw, 'protected') === true,
    deviceID: pick(raw, 'device_id'),
    slotID: pick(raw, 'slot_id'),
    rackSlot: pick(raw, 'rack_slot'),
    jobID: pick(raw, 'job_id'),
    tenant: pick(raw, 'tenant_id', 'tenant'),
    queue: pick(raw, 'queue_id', 'queue'),
    holder: pick(raw, 'holder'),
    holderInstance: pick(raw, 'holder_instance'),
    policy: pick(raw, 'disruption_policy'),
    ttl: pick(raw, 'ttl_s', 'ttl'),
    grace: pick(raw, 'grace_s', 'grace'),
    acquiredAt: pick(raw, 'acquired_at'),
    heartbeatAt: pick(raw, 'heartbeat_at'),
    expiresAt: pick(raw, 'expires_at'),
    reclaimableAt: pick(raw, 'reclaimable_at'),
    witnessAt: pick(raw, 'witness_at'),
    witnessExtensions: pick(raw, 'witness_extensions'),
    releasedAt: pick(raw, 'released_at'),
    releaseReason: pick(raw, 'release_reason')
  };
}

function normJob(raw) {
  return {
    raw,
    id: pick(raw, 'job_id', 'id'),
    state: pick(raw, 'state') || 'unknown',
    pool: pick(raw, 'pool_id', 'pool'),
    queue: pick(raw, 'queue_id', 'queue'),
    tenant: pick(raw, 'tenant_id', 'tenant'),
    protected: pick(raw, 'protected') === true,
    policy: pick(raw, 'disruption_policy'),
    expected: pick(raw, 'expected_duration', 'expected_duration_s'),
    maxRuntime: pick(raw, 'max_runtime', 'max_runtime_s'),
    createdBy: pick(raw, 'created_by'),
    createdAt: pick(raw, 'created_at'),
    startedAt: pick(raw, 'started_at'),
    finishedAt: pick(raw, 'finished_at'),
    spec: pick(raw, 'spec')
  };
}

function normHost(raw) {
  return {
    raw,
    id: pick(raw, 'host_id', 'id'),
    rack: pick(raw, 'rack_id', 'rack'),
    rackUnit: pick(raw, 'rack_unit'),
    adminState: pick(raw, 'admin_state') || 'enabled',
    endpoint: pick(raw, 'adb_endpoint'),
    epoch: pick(raw, 'host_epoch'),
    agent: pick(raw, 'agent_version'),
    kernel: pick(raw, 'kernel_release'),
    lastSeen: pick(raw, 'last_seen_at'),
    hubs: listOf(raw && raw.hubs ? raw : {}, 'hubs')
  };
}

function normHub(raw) {
  return {
    raw,
    id: pick(raw, 'hub_id', 'id'),
    host: pick(raw, 'host_id', 'host'),
    path: pick(raw, 'usb_path', 'hub_path', 'path'),
    model: pick(raw, 'model'),
    vbus: pick(raw, 'vbus_switchable') === true,
    devices: pick(raw, 'devices'),
    healthy: pick(raw, 'healthy'),
    unhealthy: pick(raw, 'unhealthy'),
    correlated: pick(raw, 'correlated') === true,
    worstSince: pick(raw, 'worst_since'),
    slots: listOf(raw && raw.slots ? raw : {}, 'slots')
  };
}

function normTier(raw) {
  return {
    raw,
    tier: pick(raw, 'tier'),
    name: pick(raw, 'name'),
    description: pick(raw, 'description'),
    blast: pick(raw, 'blast_radius') || 'device',
    requires: pick(raw, 'requires_policy'),
    // farm.recovery_tiers.cooldown is an interval; the API flattens it to
    // whole seconds as cooldown_s. Reading only "cooldown" silently rendered
    // an em dash on every rung, which reads as "no cooldown" — the opposite of
    // what the ladder guarantees.
    cooldown: pick(raw, 'cooldown_s', 'cooldown'),
    maxPerHour: pick(raw, 'max_per_hour'),
    enabled: pick(raw, 'enabled') !== false
  };
}

function normAttempt(raw) {
  return {
    raw,
    id: pick(raw, 'id'),
    deviceID: pick(raw, 'device_id'),
    slotID: pick(raw, 'slot_id'),
    rackSlot: pick(raw, 'rack_slot'),
    hubID: pick(raw, 'hub_id'),
    host: pick(raw, 'host_id', 'host'),
    tier: pick(raw, 'tier'),
    tierName: pick(raw, 'tier_name', 'name'),
    startedAt: pick(raw, 'started_at'),
    finishedAt: pick(raw, 'finished_at'),
    outcome: pick(raw, 'outcome'),
    refusal: pick(raw, 'refusal'),
    detail: pick(raw, 'detail')
  };
}

function normQuarantine(raw) {
  return {
    raw,
    id: pick(raw, 'id', 'quarantine_id'),
    scope: pick(raw, 'scope') || 'device',
    deviceID: pick(raw, 'device_id'),
    slotID: pick(raw, 'slot_id'),
    hubID: pick(raw, 'hub_id'),
    host: pick(raw, 'host_id', 'host'),
    rackSlot: pick(raw, 'rack_slot'),
    reason: pick(raw, 'reason'),
    openedAt: pick(raw, 'opened_at'),
    closedAt: pick(raw, 'closed_at'),
    auto: pick(raw, 'auto') !== false
  };
}

function normRun(raw) {
  return {
    raw,
    id: pick(raw, 'run_id', 'id'),
    createdBy: pick(raw, 'created_by'),
    createdAt: pick(raw, 'created_at'),
    selector: pick(raw, 'selector'),
    command: pick(raw, 'command'),
    maxPerHub: pick(raw, 'max_per_hub'),
    // The run is created with timeout_ms and reported back as timeout_s (the
    // API flattens farm.bulk_runs.timeout, an interval). fmtInterval reads a
    // bare number as seconds, so milliseconds must never be fed to it here.
    timeout: pick(raw, 'timeout_s', 'timeout'),
    state: pick(raw, 'state') || 'unknown',
    finishedAt: pick(raw, 'finished_at'),
    // Per-state target counts, which the API reports as flat columns on the
    // run row. They are what makes progress visible without opening the run.
    // Named targetCount, not targets: the run-detail loader hangs the actual
    // target array off .targets and the two must not collide.
    targetCount: pick(raw, 'targets'),
    pending: pick(raw, 'pending'),
    running: pick(raw, 'running'),
    ok: pick(raw, 'ok'),
    errors: pick(raw, 'errors'),
    skipped: pick(raw, 'skipped')
  };
}

function normTarget(raw) {
  return {
    raw,
    deviceID: pick(raw, 'device_id'),
    rackSlot: pick(raw, 'rack_slot'),
    hubID: pick(raw, 'hub_id'),
    state: pick(raw, 'state') || 'pending',
    startedAt: pick(raw, 'started_at'),
    finishedAt: pick(raw, 'finished_at'),
    exitCode: pick(raw, 'exit_code'),
    output: pick(raw, 'output'),
    error: pick(raw, 'error')
  };
}

function normEvent(raw, source) {
  const action = pick(raw, 'kind', 'action');
  return {
    raw,
    // /api/v1/events merges farm.events and farm.audit_log into one array and
    // labels every row in its own `source` column. That label is the whole
    // point of the column: it separates "the machine did this" from "a named
    // human did this and typed a reason". Guessing it from which keys happen
    // to be present relabels every operator action as a machine event, so the
    // server's word wins and the fallback is only for a shape that omits it.
    source: pick(raw, 'source') || source || 'event',
    id: pick(raw, 'id'),
    at: pick(raw, 'at', 'created_at'),
    kind: action || '',
    actor: pick(raw, 'actor'),
    subject: pick(raw, 'subject'),
    reason: pick(raw, 'reason'),
    deviceID: pick(raw, 'device_id'),
    slotID: pick(raw, 'slot_id'),
    leaseID: pick(raw, 'lease_id'),
    jobID: pick(raw, 'job_id'),
    detail: pick(raw, 'detail')
  };
}

/* ------------------------------------------------------------------ *
 * State
 * ------------------------------------------------------------------ */

const VIEWS = ['fleet', 'leases', 'jobs', 'recovery', 'bulk', 'events', 'docs'];

const state = {
  view: 'fleet',
  q: '',
  filters: { host: '', hub: '', health: '', pool: '', lease: '', leaseState: '', jobState: '', eventKind: '', eventLimit: '250' },
  data: {
    fleet: null, counts: null, hosts: null, hubs: null, topology: null,
    leases: null, leaseCounts: null, protectedSuspect: 0, jobs: null,
    tiers: null, attempts: null, quarantines: null, bulk: null, bulkRun: null, events: null,
    capabilities: null, kinds: null
  },
  /* Per-resource "the server capped this response". A capped list that does
   * not say so is the worst thing this page can show: it looks like the whole
   * farm and is not, and an operator counts what is in front of them. */
  truncated: {},
  errors: {},
  loading: {},
  bulkRunID: null,
  conn: { mode: 'connecting', lastEvent: 0 }
};

/* Which resources each view needs. Used both when switching views and when a
 * live event says a resource changed. */
const VIEW_NEEDS = {
  fleet: ['fleet', 'hosts'],
  leases: ['leases', 'fleet'],
  jobs: ['jobs', 'fleet'],
  recovery: ['recovery', 'fleet'],
  bulk: ['bulk', 'bulkRun', 'fleet'],
  events: ['events'],
  // Docs reads what this deployment can actually do rather than describing
  // what the project can do. The two diverge the moment somebody deploys
  // without a host agent or forgets to set a token list.
  docs: ['capabilities', 'kinds', 'recovery']
};

const deviceIndex = new Map();   // device id -> normalised fleet row

/* ------------------------------------------------------------------ *
 * API client
 * ------------------------------------------------------------------ */

class ApiError extends Error {
  constructor(status, code, message, detail) {
    super(message || 'request failed');
    this.status = status;
    this.code = code || 'error';
    this.detail = detail;
  }
}

function apiURL(path, params) {
  const u = new URL(path, API_BASE);
  if (params) {
    for (const k of Object.keys(params)) {
      const v = params[k];
      if (v !== undefined && v !== null && v !== '') u.searchParams.set(k, String(v));
    }
  }
  return u;
}

/* The API authenticates with a bearer token. The token lives in sessionStorage
 * for this tab only and is attached as a header — never as a query parameter,
 * where it would land in access logs, in the URL bar and in anything the
 * operator pastes into a ticket. */
const TOKEN_KEY = 'device-farmer.api-token';

function readToken() {
  try { return sessionStorage.getItem(TOKEN_KEY) || ''; } catch (_) { return ''; }
}

function writeToken(v) {
  try {
    if (v) sessionStorage.setItem(TOKEN_KEY, v); else sessionStorage.removeItem(TOKEN_KEY);
  } catch (_) { /* private mode: the token simply lives in memory for this page */ }
  apiToken = v;
}

let apiToken = readToken();

async function request(method, path, { params, body } = {}) {
  const headers = { Accept: 'application/json' };
  if (apiToken) headers.Authorization = 'Bearer ' + apiToken;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  let res;
  try {
    res = await fetch(apiURL(path, params), {
      method,
      headers,
      cache: 'no-store',
      body: body === undefined ? undefined : JSON.stringify(body)
    });
  } catch (err) {
    throw new ApiError(0, 'unreachable', 'the control plane is unreachable from this browser: ' + err.message);
  }
  const text = await res.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch (_) {
      if (!res.ok) throw new ApiError(res.status, 'bad_response', text.slice(0, 500));
      throw new ApiError(res.status, 'bad_response', 'the response was not JSON', text.slice(0, 500));
    }
  }
  if (!res.ok) {
    const e = (data && data.error) || {};
    if (res.status === 401 || res.status === 403) noteAuthFailure(res.status, e.message);
    throw new ApiError(res.status, e.code || 'http_' + res.status, e.message || res.statusText || 'request failed', e.detail);
  }
  return data;
}

/* An authentication failure is not a bug to be shown as a red table — it is a
 * missing credential with an obvious remedy, so say so once and offer the box
 * that fixes it. */
function noteAuthFailure(status, message) {
  banner('error',
    (status === 401 ? 'The API rejected this browser: no valid bearer token. ' : 'This token lacks the role for that action. ') +
    (message || '') + ' Operator actions additionally require the operator role.',
    { key: 'auth', action: { label: 'Set API token', run: openTokenDialog } });
}

const api = {
  get: (p, params) => request('GET', p, { params }),
  post: (p, body) => request('POST', p, { body: body === undefined ? {} : body })
};

/* ------------------------------------------------------------------ *
 * Loaders
 * ------------------------------------------------------------------ */

/* mark records the outcome of one load.
 *
 * A failed refetch must not wipe the screen. During an incident the control
 * plane is exactly what tends to blink, and an operator staring at the fleet
 * when a poll times out needs the last known picture plus an honest "this is
 * stale" — not an empty page. The rows stay, the banner and the connection
 * indicator carry the failure. */
/* beginLoad stamps a load with a sequence number and hands back the predicate
 * "is this still the newest load of this resource?".
 *
 * Several things refetch the same resource: the 5s poll, the 30s safety net,
 * the stream's coalesced refetch, the Refresh button, and every filter change.
 * Without ordering, a slow response from a request issued at T can land after
 * a fast one issued at T+3s and put three-second-old rows back on the screen —
 * silently, with the connection indicator still green. A response that is no
 * longer the newest is dropped rather than rendered. */
const loadSeq = new Map();

function beginLoad(key) {
  const n = (loadSeq.get(key) || 0) + 1;
  loadSeq.set(key, n);
  state.loading[key] = true;
  return () => loadSeq.get(key) === n;
}

function mark(key, err) {
  if (err) {
    state.errors[key] = err;
    if (err instanceof ApiError && (err.status === 0 || err.status >= 500)) {
      setConn('down', 'API unreachable');
      banner('error', 'The control plane is not answering: ' + err.message +
        ' The screen below is the last data received, not the current state of the farm.',
        { key: 'api-down' });
    }
  } else {
    delete state.errors[key];
    if (state.conn.mode === 'down') setConn(pollTimer ? 'polling' : 'live', pollTimer ? 'stream down — polling' : 'live');
    clearBanner('api-down');
  }
  state.loading[key] = false;
}

const loaders = {
  async fleet() {
    const current = beginLoad('fleet');
    try {
      const f = state.filters;
      const resp = await api.get('fleet', { host: f.host, hub: f.hub, health: f.health, pool: f.pool, q: state.q });
      if (!current()) return;
      const rows = listOf(resp, 'devices').map(normDevice);
      state.data.fleet = rows;
      state.data.counts = (resp && !Array.isArray(resp) && resp.counts) || null;
      state.truncated.fleet = !!pick(resp, 'truncated');
      // The fleet response folds in farm.v_hub_health for exactly the slice
      // the grid is showing, which is the authoritative per-hub arithmetic
      // behind the correlation banner.
      const hubRows = resp && !Array.isArray(resp) && Array.isArray(resp.hubs) ? resp.hubs.map(normHub) : null;
      if (hubRows && hubRows.length) state.data.hubs = hubRows;
      deviceIndex.clear();
      for (const d of rows) if (d.id) deviceIndex.set(String(d.id), d);
      mark('fleet');
    } catch (e) { if (!current()) return; mark('fleet', e); }
    render();
  },

  async hosts() {
    const current = beginLoad('hosts');
    try {
      const resp = await api.get('hosts');
      if (!current()) return;
      state.data.hosts = listOf(resp, 'hosts').map(normHost);
      mark('hosts');
    } catch (e) { if (!current()) return; mark('hosts', e); }
    render();
  },

  /* Topology carries farm.v_hub_health folded in, which is the authoritative
   * per-hub unhealthy count and worst_since behind the correlation banner. */
  async topology() {
    const current = beginLoad('topology');
    try {
      const resp = await api.get('topology');
      if (!current()) return;
      const hosts = listOf(resp, 'hosts');
      const hubs = [];
      for (const h of hosts) {
        const hostID = pick(h, 'host_id', 'id');
        for (const hub of listOf(h, 'hubs')) {
          const nh = normHub(hub);
          if (!nh.host) nh.host = hostID;
          hubs.push(nh);
        }
      }
      // Some deployments may return a flat hub list instead of a nested tree.
      if (!hubs.length) for (const hub of listOf(resp, 'hubs')) hubs.push(normHub(hub));
      state.data.topology = hosts;
      // The fleet response is the better source for hub health because it is
      // filtered to the same slice; topology only fills the gap.
      if (hubs.length && !(state.data.hubs || []).length) state.data.hubs = hubs;
      mark('topology');
    } catch (e) { if (!current()) return; mark('topology', e); }
    render();
  },

  async leases() {
    const current = beginLoad('leases');
    try {
      const resp = await api.get('leases', { state: state.filters.leaseState });
      if (!current()) return;
      state.data.leases = listOf(resp, 'leases').map(normLease);
      state.data.leaseCounts = (resp && !Array.isArray(resp) && resp.counts) || null;
      state.data.protectedSuspect = Number(pick(resp, 'protected_suspect') || 0);
      state.truncated.leases = !!pick(resp, 'truncated');
      mark('leases');
    } catch (e) { if (!current()) return; mark('leases', e); }
    render();
  },

  async jobs() {
    const current = beginLoad('jobs');
    try {
      const resp = await api.get('jobs', { state: state.filters.jobState });
      if (!current()) return;
      state.data.jobs = listOf(resp, 'jobs').map(normJob);
      state.truncated.jobs = !!pick(resp, 'truncated');
      mark('jobs');
    } catch (e) { if (!current()) return; mark('jobs', e); }
    render();
  },

  async recovery() {
    const current = beginLoad('recovery');
    try {
      const resp = await api.get('recovery');
      if (!current()) return;
      state.data.tiers = listOf(resp, 'tiers', 'recovery_tiers').map(normTier).sort((a, b) => (a.tier || 0) - (b.tier || 0));
      state.data.attempts = listOf(resp, 'attempts', 'recent', 'recovery_attempts').map(normAttempt);
      state.data.quarantines = listOf(resp, 'quarantines', 'open_quarantines').map(normQuarantine);
      mark('recovery');
    } catch (e) { if (!current()) return; mark('recovery', e); }
    render();
  },

  // What this deployment can actually do, observed rather than declared.
  async capabilities() {
    const current = beginLoad('capabilities');
    try {
      const resp = await api.get('capabilities');
      if (!current()) return;
      state.data.capabilities = resp;
      mark('capabilities');
    } catch (e) { if (!current()) return; mark('capabilities', e); }
    render();
  },

  // The step vocabulary, read from farm.step_kinds rather than hard-coded, so
  // the docs cannot drift from what this server will accept.
  async kinds() {
    const current = beginLoad('kinds');
    try {
      const resp = await api.get('specs/kinds');
      if (!current()) return;
      state.data.kinds = listOf(resp, 'kinds', 'step_kinds');
      mark('kinds');
    } catch (e) { if (!current()) return; mark('kinds', e); }
    render();
  },

  async bulk() {
    const current = beginLoad('bulk');
    try {
      const resp = await api.get('bulk');
      if (!current()) return;
      state.data.bulk = listOf(resp, 'runs', 'bulk_runs').map(normRun);
      if (!state.bulkRunID && state.data.bulk.length) state.bulkRunID = state.data.bulk[0].id;
      mark('bulk');
    } catch (e) { if (!current()) return; mark('bulk', e); }
    render();
    if (state.bulkRunID) loaders.bulkRun();
  },

  async bulkRun() {
    if (!state.bulkRunID) {
      state.data.bulkRun = null;
      delete state.errors.bulkRun;
      return;
    }
    const id = state.bulkRunID;
    const current = beginLoad('bulkRun');
    try {
      const resp = await api.get('bulk/' + encodeURIComponent(id));
      if (!current()) return;
      const run = normRun(pick(resp, 'run') || resp);
      run.targets = listOf(resp, 'targets', 'results').map(normTarget);
      if (!run.id) run.id = id;
      state.data.bulkRun = run;
      mark('bulkRun');
    } catch (e) { if (!current()) return; mark('bulkRun', e); }
    render();
  },

  async events() {
    const current = beginLoad('events');
    try {
      const resp = await api.get('events', { limit: state.filters.eventLimit });
      if (!current()) return;
      let rows;
      if (resp && !Array.isArray(resp) && Array.isArray(resp.audit)) {
        // A shape that returns the two logs as separate arrays: the array a row
        // came out of is then the only label there is.
        rows = (resp.events || []).map((r) => normEvent(r, 'event'))
          .concat(resp.audit.map((r) => normEvent(r, 'audit')));
      } else {
        rows = listOf(resp, 'events', 'entries', 'items').map((r) => normEvent(r));
      }
      rows.sort((a, b) => (parseTime(b.at) || 0) - (parseTime(a.at) || 0));
      state.data.events = rows;
      state.truncated.events = !!pick(resp, 'truncated');
      mark('events');
    } catch (e) { if (!current()) return; mark('events', e); }
    render();
  }
};

function loadFor(view) {
  for (const key of VIEW_NEEDS[view] || []) {
    if (key === 'bulkRun') continue;   // driven by the bulk loader and the run poller
    loaders[key]();
  }
}

/* refreshAll refetches everything the open view depends on, plus the fleet and
 * the physical inventory that every view labels its rows from. Each resource is
 * fetched once: issuing the same request twice doubles the load a dashboard
 * puts on a control plane that is, when this button gets pressed, usually
 * already having a bad day. */
function refreshAll() {
  const keys = new Set(['fleet', 'hosts', 'topology']);
  for (const k of VIEW_NEEDS[state.view] || []) keys.add(k);
  keys.delete('bulkRun');   // driven by the bulk loader and the run poller
  for (const k of keys) loaders[k]();
}

/* ------------------------------------------------------------------ *
 * Banners
 * ------------------------------------------------------------------ */

const bannerKeys = new Map();

function banner(level, text, opts) {
  const o = opts || {};
  const host = $('#banners');
  if (o.key) {
    const existing = bannerKeys.get(o.key);
    if (existing && existing.isConnected) existing.remove();
  }
  const glyph = level === 'error' ? '✕' : level === 'warn' ? '▲' : level === 'ok' ? '✓' : 'i';
  const node = el('div', { class: 'banner banner-' + level },
    el('span', { class: 'b-glyph', 'aria-hidden': 'true' }, glyph),
    el('span', { class: 'b-text' }, text, o.detail ? el('span', { class: 'mono' }, ' ' + o.detail) : null),
    o.action ? el('button', { class: 'mini ghost', onclick: o.action.run }, o.action.label) : null,
    el('button', {
      class: 'mini ghost', 'aria-label': 'Dismiss this message',
      onclick: () => node.remove()
    }, '×'));
  host.append(node);
  if (o.key) bannerKeys.set(o.key, node);
  if (level === 'ok' || level === 'info') setTimeout(() => node.remove(), 9000);
  while (host.children.length > 5) host.firstElementChild.remove();
  return node;
}

function clearBanner(key) {
  const node = bannerKeys.get(key);
  if (node && node.isConnected) node.remove();
  bannerKeys.delete(key);
}

function errText(e) {
  if (e instanceof ApiError) return e.code + ': ' + e.message;
  return String((e && e.message) || e);
}

/* ------------------------------------------------------------------ *
 * Generic pieces
 * ------------------------------------------------------------------ */

function table(cols, rows, opts) {
  const o = opts || {};
  const head = el('tr', null, cols.map((c) => el('th', { scope: 'col', class: c.cls || null }, c.label)));
  const body = el('tbody');
  for (const r of rows) {
    const tr = el('tr', { class: o.rowClass ? o.rowClass(r) : null });
    for (const c of cols) {
      const td = el('td', { class: c.cls || null });
      append(td, [c.cell(r)]);
      tr.append(td);
    }
    body.append(tr);
  }
  return el('table', null, el('thead', null, head), body);
}

function emptyState(title, detail) {
  return el('div', { class: 'empty' }, el('strong', null, title), detail);
}

/* panelState renders the honest not-yet / failed / nothing-there states so no
 * view is ever silently blank. */
function panelState(key, rows, empty) {
  const e = state.errors[key];
  const have = Array.isArray(rows) && rows.length > 0;
  if (e && !have) {
    return emptyState('Could not load this from the API.',
      el('span', null, errText(e), e.detail ? el('span', { class: 'mono' }, ' ' + JSON.stringify(e.detail)) : null));
  }
  if (e) return null;   // stale rows beat a blank screen; the banner says so
  if (rows === null || rows === undefined) return emptyState('Loading from the API…', 'Nothing is drawn until the server answers.');
  if (!rows.length) return empty;
  return null;
}

function countChips(obj) {
  const out = [];
  if (!obj || typeof obj !== 'object') return out;
  for (const k of Object.keys(obj)) {
    const v = obj[k];
    if (v === null || typeof v === 'object') continue;
    out.push(el('span', { class: 'count' }, k.replace(/_/g, ' '), ' ', el('b', null, String(v))));
  }
  return out;
}

/* truncChip is the "you are not looking at all of it" badge. It is appended to
 * a view's counts whenever the API says it capped the response, because every
 * number beside it is then a number about a subset. */
function truncChip(key, narrow) {
  if (!state.truncated[key]) return null;
  return el('span', {
    class: 'chip chip-degraded',
    title: 'the server capped this response. ' + narrow +
      ' Everything counted here counts only the rows that came back, not the farm.'
  }, el('span', { 'aria-hidden': 'true' }, '▲'), 'truncated by the server');
}

function deviceLabel(deviceID, fallbackRack) {
  const d = deviceID ? deviceIndex.get(String(deviceID)) : null;
  const rack = fallbackRack || (d && (d.rackSlot || (d.usbPath ? 'usb ' + d.usbPath : null)));
  if (rack) {
    return el('span', null, el('span', { class: 'mono' }, rack),
      d && d.model ? el('span', { class: 'dim' }, ' ' + d.model) : null);
  }
  if (deviceID) return el('span', { class: 'mono', title: String(deviceID) }, shortId(deviceID));
  return el('span', { class: 'chip chip-plain' }, '—');
}

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
      'showing ', el('b', null, String(rows.length)), ' / ' + all.length));
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
  fillSelect($('#f-host'), 'all hosts',
    unique(rows.map((d) => d.host).filter(Boolean)).concat((state.data.hosts || []).map((h) => h.id).filter(Boolean)),
    state.filters.host);
  fillSelect($('#f-pool'), 'all pools', unique(rows.map((d) => d.pool).filter(Boolean)), state.filters.pool);

  const hubs = [];
  const seen = new Set();
  for (const d of rows) {
    const k = hubParamOf(d);
    if (!k || seen.has(k)) continue;
    seen.add(k);
    hubs.push({ value: k, label: (d.host ? d.host + ' · ' : '') + 'hub ' + (d.hubPath || k) });
  }
  hubs.sort((a, b) => cmp(a.label, b.label));
  fillSelect($('#f-hub'), 'all hubs', hubs, state.filters.hub);
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

/* ------------------------------------------------------------------ *
 * LEASES
 * ------------------------------------------------------------------ */

function renderLeases() {
  const body = $('#leases-body');
  const rows0 = state.data.leases;
  const q = state.q.trim().toLowerCase();
  const rows = (rows0 || []).filter((l) => {
    if (!q) return true;
    const d = l.deviceID ? deviceIndex.get(String(l.deviceID)) : null;
    const hay = [l.holder, l.jobID, l.tenant, l.queue, l.id, l.deviceID, l.fence, l.state,
      l.rackSlot || (d && d.rackSlot), d && d.model].join(' ').toLowerCase();
    return hay.includes(q);
  });

  const counts = $('#leases-counts');
  counts.replaceChildren();
  if (rows0) {
    const held = rows0.filter((l) => l.state === 'held').length;
    const suspect = rows0.filter((l) => l.state === 'suspect').length;
    const prot = rows0.filter((l) => l.protected && (l.state === 'held' || l.state === 'suspect')).length;
    append(counts, [
      el('span', { class: 'count' }, 'held ', el('b', null, String(held))),
      el('span', { class: 'count' }, 'suspect ', el('b', null, String(suspect))),
      el('span', { class: 'count' }, 'protected ', el('b', null, String(prot))),
      // A protected suspect lease is the one the reaper will never take: it
      // waits for a human. That number is worth its own badge.
      state.data.protectedSuspect
        ? el('span', { class: 'chip chip-protected', title: 'protected and suspect: the reaper will not reclaim these; a human is expected to look' },
          el('span', { 'aria-hidden': 'true' }, '★'), 'protected suspect ' + state.data.protectedSuspect)
        : null,
      el('span', { class: 'count' }, 'showing ', el('b', null, String(rows.length))),
      truncChip('leases', 'Pick a single lease state to see the rest.')
    ]);
  }

  const problem = panelState('leases', rows0, emptyState('No leases in this state.',
    'Every live lease appears here with its fence, holder, job, and the two server-computed instants that matter: expires_at (when it becomes suspect) and reclaimable_at (the earliest the reaper may act).'));
  if (problem) { body.replaceChildren(problem); return; }

  body.setAttribute('aria-busy', 'false');
  body.replaceChildren(table([
    {
      label: 'State', cell: (l) => {
        const chips = [el('span', { class: 'chip chip-' + (l.state === 'suspect' ? 'suspect' : l.state === 'held' ? 'held' : 'plain') },
          el('span', { 'aria-hidden': 'true' }, l.state === 'suspect' ? '◐' : l.state === 'held' ? '●' : '○'), l.state)];
        if (l.protected) chips.push(el('span', { class: 'chip chip-protected' }, el('span', { 'aria-hidden': 'true' }, '★'), 'protected'));
        if (l.state === 'suspect') {
          chips.push(el('span', { class: 'chip chip-plain', title: 'suspect is an alerting state only' }, 'holder not visible'));
        }
        return el('span', { class: 'chips' }, chips);
      }
    },
    { label: 'Fence', cls: 'num', cell: (l) => (l.fence === undefined ? '—' : String(l.fence)) },
    { label: 'Device', cell: (l) => deviceLabel(l.deviceID, l.rackSlot) },
    { label: 'Job', cls: 'mono', cell: (l) => el('span', { title: String(l.jobID || '') }, shortId(l.jobID) || '—') },
    { label: 'Tenant', cell: (l) => l.tenant || '—' },
    { label: 'Holder', cls: 'mono', cell: (l) => el('span', { class: 'trunc', title: (l.holder || '') + ' — audit only; the holder name confers no ownership' }, l.holder || '—') },
    { label: 'Acquired', cell: (l) => timeCell(l.acquiredAt) },
    { label: 'Heartbeat', cell: (l) => timeCell(l.heartbeatAt) },
    { label: 'Expires', cell: (l) => timeCell(l.expiresAt) },
    { label: 'Reclaimable', cell: (l) => timeCell(l.reclaimableAt) },
    { label: 'Witness', cell: (l) => (l.witnessAt ? timeCell(l.witnessAt) : el('span', { class: 'chip chip-plain' }, 'none')) },
    {
      label: '', cls: 'acts', cell: (l) => (l.state === 'held' || l.state === 'suspect')
        ? el('button', { class: 'mini danger', onclick: () => revokeLease(l) }, 'Revoke')
        : (l.releaseReason ? el('span', { class: 'chip chip-plain' }, l.releaseReason) : '')
    }
  ], rows, {
    rowClass: (l) => (l.state === 'suspect' ? 'row-suspect' : l.protected && l.state === 'held' ? 'row-protected' : l.state === 'held' ? 'row-held' : null)
  }));
}

/* ------------------------------------------------------------------ *
 * JOBS
 * ------------------------------------------------------------------ */

function renderJobs() {
  const body = $('#jobs-body');
  const rows0 = state.data.jobs;
  const q = state.q.trim().toLowerCase();
  const rows = (rows0 || []).filter((j) => {
    if (!q) return true;
    return [j.id, j.pool, j.queue, j.tenant, j.state, j.createdBy, j.spec ? JSON.stringify(j.spec) : '']
      .join(' ').toLowerCase().includes(q);
  });

  const counts = $('#jobs-counts');
  counts.replaceChildren();
  if (rows0) {
    const by = {};
    for (const j of rows0) by[j.state] = (by[j.state] || 0) + 1;
    append(counts, [countChips(by), el('span', { class: 'count' }, 'showing ', el('b', null, String(rows.length))),
      truncChip('jobs', 'Pick a single job state to see the rest.')]);
  }

  const problem = panelState('jobs', rows0, emptyState('No jobs.',
    'Queued and running jobs appear here as soon as one is submitted. Use the form on the right.'));
  if (problem) { body.replaceChildren(problem); return; }

  body.setAttribute('aria-busy', 'false');
  body.replaceChildren(table([
    { label: 'State', cell: (j) => el('span', { class: 'chip ' + (JOB_CLASS[j.state] || 'chip-plain') }, j.state) },
    { label: 'Job', cls: 'mono', cell: (j) => el('span', { title: String(j.id || '') }, shortId(j.id)) },
    { label: 'Pool', cell: (j) => j.pool || '—' },
    { label: 'Queue', cell: (j) => j.queue || '—' },
    { label: 'Tenant', cell: (j) => j.tenant || '—' },
    {
      label: 'Guards', cell: (j) => el('span', { class: 'chips' },
        j.protected ? el('span', { class: 'chip chip-protected' }, el('span', { 'aria-hidden': 'true' }, '★'), 'protected') : null,
        j.policy ? el('span', { class: 'chip chip-plain', title: 'disruption_policy' }, j.policy) : null)
    },
    { label: 'Max runtime', cell: (j) => el('span', { title: 'the only user-supplied clock that may end a lease automatically' }, fmtInterval(j.maxRuntime)) },
    { label: 'Expected', cell: (j) => fmtInterval(j.expected) },
    { label: 'Created', cell: (j) => timeCell(j.createdAt) },
    { label: 'Started', cell: (j) => (j.startedAt ? timeCell(j.startedAt) : '—') },
    { label: 'Finished', cell: (j) => (j.finishedAt ? timeCell(j.finishedAt) : '—') },
    { label: 'By', cell: (j) => j.createdBy || '—' },
    {
      label: '', cls: 'acts', cell: (j) => (['queued', 'allocating', 'running'].includes(j.state)
        ? el('button', { class: 'mini danger', onclick: () => cancelJob(j) }, 'Cancel')
        : '')
    }
  ], rows));
}

function readJobForm() {
  const num = (id) => {
    const v = $(id).value.trim();
    if (v === '') return undefined;
    const n = Number(v);
    return Number.isFinite(n) ? n : undefined;
  };
  const json = (id, label) => {
    const v = $(id).value.trim();
    if (v === '') return {};
    try { return JSON.parse(v); } catch (e) { throw new Error(label + ' is not valid JSON: ' + e.message); }
  };
  const body = {
    pool: $('#job-pool').value.trim(),
    queue: $('#job-queue').value.trim(),
    tenant: $('#job-tenant').value.trim(),
    spec: json('#job-spec', 'Spec'),
    selector: json('#job-selector', 'Selector'),
    protected: $('#job-protected').checked,
    disruption_policy: $('#job-policy').value
  };
  const expected = num('#job-expected'); if (expected !== undefined) body.expected_duration_s = expected;
  const maxrt = num('#job-maxrt'); if (maxrt !== undefined) body.max_runtime_s = maxrt;
  const ttl = num('#job-ttl'); if (ttl !== undefined) body.ttl_s = ttl;
  const grace = num('#job-grace'); if (grace !== undefined) body.grace_s = grace;
  const by = $('#job-by').value.trim(); if (by) body.created_by = by;
  return body;
}

/* ------------------------------------------------------------------ *
 * RECOVERY
 * ------------------------------------------------------------------ */

function renderRecovery() {
  renderTiers();
  renderAttempts();
  renderQuarantines();
}

function renderTiers() {
  const host = $('#tiers-body');
  const tiers = state.data.tiers;
  const problem = panelState('recovery', tiers, emptyState('The ladder is empty.',
    'farm.recovery_tiers rows 0 through 8 appear here — name, blast radius, the lease disruption policy each rung requires, its cooldown and its hourly budget.'));
  if (problem) { host.replaceChildren(problem); return; }
  host.setAttribute('aria-busy', 'false');
  host.replaceChildren(...tiers.map((t) => el('div', { class: 'rung blast-' + t.blast + (t.enabled ? '' : ' disabled') },
    el('span', { class: 'tier' }, String(t.tier)),
    el('span', { class: 'rname' }, t.name || '—'),
    el('span', { class: 'rdesc' }, t.description || ''),
    el('span', { class: 'chip ' + (t.blast === 'device' ? 'chip-plain' : t.blast === 'power_domain' ? 'chip-degraded' : 'chip-offline'), title: 'blast radius' },
      el('span', { 'aria-hidden': 'true' }, t.blast === 'device' ? '·' : '▲'), t.blast),
    el('span', { class: 'chip chip-plain', title: 'a live lease must carry at least this disruption policy or the rung is refused' }, t.requires || '—'),
    el('span', { class: 'chip chip-plain', title: 'cooldown' }, 'cd ' + fmtInterval(t.cooldown)),
    el('span', { class: 'chip chip-plain', title: 'max attempts per hour' }, (t.maxPerHour !== undefined ? t.maxPerHour : '—') + '/h'),
    t.enabled ? null : el('span', { class: 'chip chip-offline' }, 'disabled'))));
}

function renderAttempts() {
  const host = $('#attempts-body');
  const rows0 = state.data.attempts;
  const q = state.q.trim().toLowerCase();
  const rows = (rows0 || []).filter((a) => !q || [a.host, a.tierName, a.outcome, a.refusal, a.rackSlot, a.deviceID,
    a.detail ? JSON.stringify(a.detail) : ''].join(' ').toLowerCase().includes(q));

  const problem = panelState('recovery', rows0, emptyState('No recovery attempts recorded.',
    'Each rung the watchdog climbs is logged here with its outcome, and a refused rung is shown with the reason it was refused.'));
  if (problem) { host.replaceChildren(problem); return; }
  host.setAttribute('aria-busy', 'false');
  host.replaceChildren(table([
    { label: 'Started', cell: (a) => timeCell(a.startedAt) },
    { label: 'Tier', cls: 'mono', cell: (a) => (a.tier !== undefined ? a.tier + ' ' + (a.tierName || tierName(a.tier)) : '—') },
    { label: 'Device', cell: (a) => deviceLabel(a.deviceID, a.rackSlot) },
    { label: 'Host', cell: (a) => a.host || '—' },
    {
      label: 'Outcome', cell: (a) => (a.outcome
        ? el('span', { class: 'chip ' + (OUTCOME_CLASS[a.outcome] || 'chip-plain') },
          el('span', { 'aria-hidden': 'true' }, a.outcome === 'recovered' ? '✓' : a.outcome === 'refused' ? '⊘' : a.outcome === 'failed' ? '✕' : '·'), a.outcome)
        : el('span', { class: 'chip chip-booting' }, el('span', { 'aria-hidden': 'true' }, '↻'), 'in flight'))
    },
    {
      label: 'Refusal / detail', cell: (a) => {
        if (a.refusal) return el('span', { class: 'mono', title: a.refusal }, a.refusal);
        if (a.detail && typeof a.detail === 'object' && Object.keys(a.detail).length) {
          const s = JSON.stringify(a.detail);
          return el('span', { class: 'mono trunc', title: s }, s);
        }
        return '—';
      }
    },
    {
      label: 'Took', cls: 'num', cell: (a) => {
        const s = parseTime(a.startedAt), f = parseTime(a.finishedAt);
        return s && f ? fmtSecs((f - s) / 1000) : '—';
      }
    }
  ], rows, { rowClass: (a) => (a.outcome === 'refused' ? 'row-refused' : null) }));
}

function tierName(n) {
  const t = (state.data.tiers || []).find((x) => x.tier === n);
  return t ? t.name : '';
}

/* quarantineSubject names what a quarantine actually covers.
 *
 * farm.quarantines.scope has five values — device, slot, power_domain, hub,
 * host — and the row carries only device_id, slot_id, hub_id and host_id.
 * A power_domain quarantine therefore has no id of its own, and printing one
 * anyway ("slot undefined") tells an operator to walk to a slot that is not
 * the thing being held out of service. Every branch below prints an id the row
 * really has, or says plainly that the API did not report one. */
function quarantineSubject(q) {
  switch (q.scope) {
    case 'device':
      return deviceLabel(q.deviceID, q.rackSlot);
    case 'host':
      return q.host ? el('span', { class: 'mono' }, String(q.host)) : unnamedSubject('host');
    case 'hub':
      return q.hubID !== undefined && q.hubID !== null
        ? el('span', { class: 'mono' }, 'hub ' + q.hubID) : unnamedSubject('hub');
    case 'slot':
      return q.slotID !== undefined && q.slotID !== null
        ? el('span', { class: 'mono' }, q.rackSlot ? q.rackSlot + ' (slot ' + q.slotID + ')' : 'slot ' + q.slotID)
        : unnamedSubject('slot');
    case 'power_domain': {
      // The power domain is identified by whatever the row does carry: the hub
      // it hangs off, or a slot inside it.
      const parts = [];
      if (q.hubID !== undefined && q.hubID !== null) parts.push('hub ' + q.hubID);
      if (q.slotID !== undefined && q.slotID !== null) parts.push('slot ' + q.slotID);
      if (q.host) parts.push('on ' + q.host);
      return parts.length
        ? el('span', { class: 'mono', title: 'a whole power domain, located by ' + parts.join(', ') }, 'power domain · ' + parts.join(' · '))
        : unnamedSubject('power domain');
    }
    default:
      return unnamedSubject(q.scope || 'unknown scope');
  }
}

function unnamedSubject(what) {
  return el('span', { class: 'chip chip-plain', title: 'the API reported no identifier for this ' + what },
    what + ', id not reported');
}

function renderQuarantines() {
  const host = $('#quarantines-body');
  const rows0 = state.data.quarantines;
  const problem = panelState('recovery', rows0, emptyState('No open quarantines.',
    'A quarantine appears here when the ladder stops scheduling to a device, slot, hub or host and asks for a human. Closing one is an audited action.'));
  if (problem) { host.replaceChildren(problem); return; }
  host.setAttribute('aria-busy', 'false');
  host.replaceChildren(table([
    { label: 'Scope', cell: (q) => el('span', { class: 'chip chip-quarantined' }, el('span', { 'aria-hidden': 'true' }, '■'), q.scope) },
    { label: 'Subject', cell: (q) => quarantineSubject(q) },
    { label: 'Reason', cell: (q) => el('span', { title: q.reason || '' }, q.reason || '—') },
    { label: 'Opened', cell: (q) => timeCell(q.openedAt) },
    { label: 'Source', cell: (q) => el('span', { class: 'chip chip-plain' }, q.auto ? 'automatic' : 'operator') },
    { label: '', cls: 'acts', cell: (q) => el('button', { class: 'mini', onclick: () => closeQuarantine(q) }, 'Close') }
  ], rows0));
}

/* ------------------------------------------------------------------ *
 * BULK
 * ------------------------------------------------------------------ */

/* farm.bulk_runs.state is running | done | cancelled. A cancelled run is not a
 * finished one and must not wear the same green chip as a run that completed,
 * so both places that show a run state go through here. */
function runStateChip(s) {
  const cls = s === 'running' ? 'chip-held' : s === 'done' ? 'chip-healthy' : s === 'cancelled' ? 'chip-unknown' : 'chip-plain';
  return el('span', { class: 'chip ' + cls }, s || 'unknown');
}

/* runProgress renders the per-state target counts the API already reports on
 * each run row, so progress is visible without opening the run. */
function runProgress(r) {
  if (r.targetCount === undefined || r.targetCount === null) return '—';
  const done = (Number(r.ok) || 0) + (Number(r.errors) || 0) + (Number(r.skipped) || 0);
  return el('span', { class: 'chips', title: 'ok / error / skipped, out of the targets the selector matched' },
    el('span', { class: 'count' }, String(done), ' / ', el('b', null, String(r.targetCount))),
    Number(r.errors) ? el('span', { class: 'chip chip-offline' }, 'error ' + r.errors) : null,
    Number(r.skipped) ? el('span', { class: 'chip chip-unknown' }, 'skipped ' + r.skipped) : null);
}

function renderBulk() {
  const runsHost = $('#bulk-runs');
  const rows0 = state.data.bulk;
  const problem = panelState('bulk', rows0, emptyState('No bulk runs yet.',
    'A run submitted on the right appears here, and its per-device results stream into the panel below as each target finishes.'));
  if (problem) { runsHost.replaceChildren(problem); }
  else {
    runsHost.setAttribute('aria-busy', 'false');
    runsHost.replaceChildren(table([
      {
        label: '', cls: 'acts', cell: (r) => el('button', {
          class: 'mini' + (String(r.id) === String(state.bulkRunID) ? ' primary' : ''),
          onclick: () => { state.bulkRunID = r.id; state.data.bulkRun = null; loaders.bulkRun(); render(); }
        }, String(r.id) === String(state.bulkRunID) ? 'Selected' : 'Open')
      },
      { label: 'State', cell: (r) => runStateChip(r.state) },
      { label: 'Progress', cell: (r) => runProgress(r) },
      { label: 'Command', cls: 'mono', cell: (r) => el('span', { class: 'trunc', title: r.command || '' }, r.command || '—') },
      { label: 'Selector', cls: 'mono', cell: (r) => el('span', { class: 'trunc', title: r.selector ? JSON.stringify(r.selector) : '' }, r.selector ? JSON.stringify(r.selector) : '—') },
      { label: 'Per hub', cls: 'num', cell: (r) => (r.maxPerHub !== undefined ? String(r.maxPerHub) : '—') },
      { label: 'Timeout', cls: 'num', cell: (r) => fmtInterval(r.timeout) },
      { label: 'By', cell: (r) => r.createdBy || '—' },
      { label: 'Started', cell: (r) => timeCell(r.createdAt) },
      { label: 'Finished', cell: (r) => (r.finishedAt ? timeCell(r.finishedAt) : '—') }
    ], rows0));
  }

  const detail = $('#bulk-detail');
  const run = state.data.bulkRun;
  if (state.errors.bulkRun) {
    detail.replaceChildren(emptyState('Could not load that run.', errText(state.errors.bulkRun)));
    return;
  }
  if (!state.bulkRunID) {
    detail.replaceChildren(emptyState('No run selected.', 'Pick a run above to watch its per-device results.'));
    return;
  }
  if (!run) { detail.replaceChildren(emptyState('Loading that run…', 'Per-device results appear as the API reports them.')); return; }

  const targets = run.targets || [];
  const by = {};
  for (const t of targets) by[t.state] = (by[t.state] || 0) + 1;

  const header = el('div', { class: 'toolbar' },
    el('span', { class: 'mono' }, run.command || ''),
    runStateChip(run.state),
    el('span', { class: 'grow' }),
    el('span', { class: 'counts' }, countChips(by),
      el('span', { class: 'count' }, 'targets ', el('b', null, String(targets.length)))));

  const body = targets.length ? table([
    { label: 'Device', cell: (t) => deviceLabel(t.deviceID, t.rackSlot) },
    { label: 'State', cell: (t) => el('span', { class: 'chip ' + (TARGET_CLASS[t.state] || 'chip-plain') }, t.state) },
    { label: 'Exit', cls: 'num', cell: (t) => (t.exitCode === undefined || t.exitCode === null ? '—' : String(t.exitCode)) },
    { label: 'Started', cell: (t) => (t.startedAt ? timeCell(t.startedAt) : '—') },
    { label: 'Took', cls: 'num', cell: (t) => { const s = parseTime(t.startedAt), f = parseTime(t.finishedAt); return s && f ? fmtSecs((f - s) / 1000) : '—'; } },
    {
      label: 'Output', cell: (t) => {
        const text = t.error ? t.error : t.output;
        if (!text) return el('span', { class: 'chip chip-plain' }, t.state === 'pending' ? 'not started' : 'no output');
        const d = el('details', null, el('summary', { class: 'mono trunc' }, String(text).split('\n')[0].slice(0, 80)),
          el('pre', { class: 'out' }, String(text)));
        return d;
      }
    }
  ], targets, { rowClass: (t) => (t.state === 'error' ? 'row-refused' : null) })
    : emptyState('This run has no targets yet.', 'The selector matched nothing, or the run has not expanded its target set.');

  detail.replaceChildren(header, body);
}

/* ------------------------------------------------------------------ *
 * EVENTS
 * ------------------------------------------------------------------ */

function renderEvents() {
  const host = $('#events-body');
  const rows0 = state.data.events;
  const q = state.q.trim().toLowerCase();
  const kindFilter = state.filters.eventKind.trim().toLowerCase();
  const rows = (rows0 || []).filter((e) => {
    if (kindFilter && !String(e.kind || '').toLowerCase().includes(kindFilter)) return false;
    if (!q) return true;
    return [e.kind, e.actor, e.subject, e.reason, e.deviceID, e.jobID, e.leaseID,
      e.detail ? JSON.stringify(e.detail) : ''].join(' ').toLowerCase().includes(q);
  });

  const counts = $('#events-counts');
  counts.replaceChildren();
  if (rows0) {
    append(counts, [
      el('span', { class: 'count' }, 'showing ', el('b', null, String(rows.length)), ' / ' + rows0.length),
      truncChip('events', 'This is the newest page only; raise Show to reach further back.')
    ]);
  }

  const problem = panelState('events', rows0, emptyState('No events.',
    'farm.events and farm.audit_log are merged here newest first: every lease transition, every recovery attempt and every operator action with the human who typed the reason.'));
  if (problem) { host.replaceChildren(problem); return; }
  host.setAttribute('aria-busy', 'false');
  host.replaceChildren(table([
    { label: 'When', cell: (e) => timeCell(e.at) },
    { label: 'Source', cell: (e) => el('span', { class: 'chip ' + (e.source === 'audit' ? 'chip-protected' : 'chip-plain') }, e.source) },
    { label: 'Kind', cls: 'mono', cell: (e) => e.kind || '—' },
    { label: 'Actor', cell: (e) => e.actor || '—' },
    { label: 'Subject', cell: (e) => (e.subject ? el('span', { class: 'mono trunc', title: e.subject }, e.subject) : deviceLabel(e.deviceID)) },
    { label: 'Reason', cell: (e) => (e.reason ? el('span', { title: e.reason }, e.reason) : '—') },
    { label: 'Job', cls: 'mono', cell: (e) => (e.jobID ? el('span', { title: String(e.jobID) }, shortId(e.jobID)) : '—') },
    {
      label: 'Detail', cell: (e) => {
        if (!e.detail || (typeof e.detail === 'object' && !Object.keys(e.detail).length)) return '—';
        const s = typeof e.detail === 'string' ? e.detail : JSON.stringify(e.detail);
        return el('details', null, el('summary', { class: 'mono trunc' }, s.slice(0, 70)), el('pre', { class: 'out' }, s));
      }
    }
  ], rows));
}

/* ------------------------------------------------------------------ *
 * Operator actions. Every one of them: a typed reason, a confirm step that
 * names exactly what will be disturbed, and the server's own words when the
 * server says no.
 * ------------------------------------------------------------------ */

let pendingConfirm = null;

function impactList(subject, lines) {
  return el('div', null,
    el('div', null, 'Subject: ', el('span', { class: 'subject' }, subject)),
    el('ul', null, lines.map((l) => el('li', null, l))));
}

function openConfirm(spec) {
  pendingConfirm = spec;
  $('#confirm-title').textContent = spec.title;
  $('#confirm-impact').replaceChildren(spec.impact);
  const reason = $('#confirm-reason');
  reason.value = '';
  const err = $('#confirm-error');
  err.hidden = true;
  err.replaceChildren();
  const ok = $('#confirm-ok');
  ok.textContent = spec.confirmLabel || 'Confirm';
  ok.className = spec.safe ? 'primary' : 'danger';
  ok.disabled = false;
  $('#confirm-cancel').disabled = false;
  $('#dlg-confirm').showModal();
  reason.focus();
}

function showConfirmError(e) {
  const err = $('#confirm-error');
  const title = e instanceof ApiError && e.status === 409
    ? 'The server refused this action:'
    : 'The server rejected this action:';
  err.replaceChildren(
    el('strong', null, title), ' ',
    el('span', null, e instanceof ApiError ? e.message : String(e && e.message ? e.message : e)),
    e instanceof ApiError && e.code ? el('span', { class: 'fe-detail' }, 'code: ' + e.code + (e.status ? '  http ' + e.status : '')) : null,
    e instanceof ApiError && e.detail !== undefined
      ? el('span', { class: 'fe-detail' }, typeof e.detail === 'string' ? e.detail : JSON.stringify(e.detail, null, 2))
      : null);
  err.hidden = false;
}

function wireConfirm() {
  $('#confirm-form').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!pendingConfirm) return;
    const reason = $('#confirm-reason').value.trim();
    if (!reason) {
      showConfirmError(new Error('A reason is required. It is written to farm.audit_log next to your name.'));
      return;
    }
    const ok = $('#confirm-ok'), cancel = $('#confirm-cancel');
    ok.disabled = true; cancel.disabled = true;
    try {
      await pendingConfirm.run(reason);
      const done = pendingConfirm.done;
      $('#dlg-confirm').close();
      pendingConfirm = null;
      banner('ok', done);
      refreshAll();
    } catch (e) {
      showConfirmError(e);
    } finally {
      ok.disabled = false; cancel.disabled = false;
    }
  });
  $('#confirm-cancel').addEventListener('click', () => { $('#dlg-confirm').close(); pendingConfirm = null; });
  $('#dlg-confirm').addEventListener('close', () => { pendingConfirm = null; });
}

function openTokenDialog() {
  const input = $('#token-input');
  input.value = apiToken;
  $('#token-note').textContent = apiToken
    ? 'A token is set for this tab. The live stream cannot carry it (EventSource sends no headers), so a token-protected deployment updates by polling.'
    : 'No token is set for this tab.';
  $('#dlg-token').showModal();
  input.focus();
}

function wireToken() {
  $('#token-btn').addEventListener('click', openTokenDialog);
  $('#token-cancel').addEventListener('click', () => $('#dlg-token').close());
  $('#token-clear').addEventListener('click', () => {
    writeToken('');
    $('#token-input').value = '';
    $('#dlg-token').close();
    banner('info', 'API token cleared for this tab.');
    restartStream();
    refreshAll();
  });
  $('#token-form').addEventListener('submit', (ev) => {
    ev.preventDefault();
    writeToken($('#token-input').value.trim());
    $('#dlg-token').close();
    banner('ok', apiToken ? 'Token saved for this tab.' : 'Token cleared for this tab.');
    restartStream();
    refreshAll();
  });
}

function drainHost(host, devices) {
  const live = devices.filter((d) => d.leaseState === 'held' || d.leaseState === 'suspect');
  openConfirm({
    title: 'Drain host ' + host,
    impact: impactList(host, [
      'No new lease will be placed on this host.',
      live.length + ' live lease' + (live.length === 1 ? '' : 's') + ' on this host keep their devices and keep running. Draining never ends a lease.',
      devices.length + ' device' + (devices.length === 1 ? '' : 's') + ' stop being schedulable once their current work finishes.',
      live.length
        ? 'Live now: ' + live.map((d) => (d.rackSlot || d.usbPath || shortId(d.id)) + ' (fence ' + d.fence + ')').join(', ')
        : 'Nothing is running on this host right now.'
    ]),
    confirmLabel: 'Drain host',
    done: 'Host ' + host + ' is draining.',
    run: (reason) => api.post('hosts/' + encodeURIComponent(host) + '/drain', { reason })
  });
}

function undrainHost(host, devices) {
  openConfirm({
    title: 'Undrain host ' + host,
    safe: true,
    impact: impactList(host, [
      'This host becomes schedulable again.',
      devices.length + ' device' + (devices.length === 1 ? '' : 's') + ' on it return to the allocation pool as their health allows.'
    ]),
    confirmLabel: 'Undrain host',
    done: 'Host ' + host + ' is enabled again.',
    run: (reason) => api.post('hosts/' + encodeURIComponent(host) + '/undrain', { reason })
  });
}

function revokeLease(l) {
  const d = l.deviceID ? deviceIndex.get(String(l.deviceID)) : null;
  const where = l.rackSlot || (d && (d.rackSlot || d.usbPath)) || shortId(l.deviceID);
  const lines = [
    'Lease fence ' + l.fence + ' on ' + where + ' ends now, with release_reason operator_revoked.',
    'Job ' + shortId(l.jobID) + ' loses this device immediately. Whatever it is doing on the phone is not finished for it.',
    'Holder ' + (l.holder || 'unknown') + ' is fenced out at the device: its next renew fails with 410 and its open sockets are refused.',
    'The device becomes allocatable again only after its slot re-arms.'
  ];
  if (l.protected) lines.unshift('This lease is PROTECTED — the reaper would never take it. Revoking is a human overriding that protection.');
  if (l.state === 'suspect') lines.push('This lease is suspect, which means only that we cannot see the holder. It is not evidence that the job died or that the device is broken.');
  openConfirm({
    title: 'Revoke lease ' + shortId(l.id),
    impact: impactList('fence ' + l.fence + ' · ' + where, lines),
    confirmLabel: 'Revoke this lease',
    done: 'Lease fence ' + l.fence + ' revoked.',
    run: (reason) => api.post('leases/' + encodeURIComponent(l.id) + '/revoke', { reason })
  });
}

function powerSlot(d) {
  const sameHub = (state.data.fleet || []).filter((x) => x.slotID !== d.slotID && hubKeyOf(x) === hubKeyOf(d));
  const liveOnHub = sameHub.filter((x) => x.leaseState === 'held' || x.leaseState === 'suspect');
  openConfirm({
    title: 'Power-cycle slot ' + (d.rackSlot || d.usbPath || d.slotID),
    impact: impactList((d.rackSlot || 'slot ' + d.slotID) + ' · ' + (d.usbPath || ''), [
      'VBUS is cut and restored for this slot’s power domain.',
      'If this hub switches power per port, only this device is disturbed. If the domain is ganged, every device in it goes down with it.',
      'Worst case on this hub: ' + sameHub.length + ' other device' + (sameHub.length === 1 ? '' : 's') +
      ', of which ' + liveOnHub.length + ' currently hold a live lease.',
      'The server checks the real power domain and refuses this if any live lease in it forbids the disruption. If it refuses, the reason appears here.'
    ]),
    confirmLabel: 'Cut power to this slot',
    done: 'Power cycle requested for slot ' + (d.rackSlot || d.slotID) + '.',
    run: (reason) => api.post('slots/' + encodeURIComponent(d.slotID) + '/power', { reason })
  });
}

function closeQuarantine(q) {
  openConfirm({
    title: 'Close quarantine ' + q.id,
    safe: true,
    impact: impactList(q.scope + ' ' + (q.rackSlot || q.deviceID || q.host || q.hubID || q.slotID || ''), [
      'The quarantine opened ' + fmtRel(q.openedAt) + ' is marked closed with your name on it.',
      'Scheduling resumes to this ' + q.scope + ' as soon as its health allows.',
      'Reason it was opened: ' + (q.reason || 'not recorded') + '.',
      'Close it because the fault is fixed, not to clear the screen — the watchdog will simply open it again.'
    ]),
    confirmLabel: 'Close quarantine',
    done: 'Quarantine ' + q.id + ' closed.',
    run: (reason) => api.post('quarantines/' + encodeURIComponent(q.id) + '/close', { reason })
  });
}

function cancelJob(j) {
  openConfirm({
    title: 'Cancel job ' + shortId(j.id),
    impact: impactList(String(j.id), [
      'The job is cancelled in state ' + j.state + '.',
      'Its lease, if it holds one, ends with release_reason job_cancelled — a deliberate ending, recorded as one.',
      'Work already done on the device is not recovered.'
    ]),
    confirmLabel: 'Cancel job',
    done: 'Job ' + shortId(j.id) + ' cancelled.',
    run: (reason) => api.post('jobs/' + encodeURIComponent(j.id) + '/cancel', { reason })
  });
}

/* ------------------------------------------------------------------ *
 * Device dialog
 * ------------------------------------------------------------------ */

async function openDevice(d) {
  const dlg = $('#dlg-device');
  $('#device-title').textContent = (d.rackSlot || d.usbPath || shortId(d.id)) + '  ' + (d.model || '');
  const body = $('#device-body');
  body.replaceChildren(emptyState('Loading device…', 'Fetching /api/v1/devices/' + shortId(d.id)));
  if (!dlg.open) dlg.showModal();

  let full = d;
  try {
    const resp = await api.get('devices/' + encodeURIComponent(d.id));
    full = normDevice(pick(resp, 'device') || resp);
    if (!full.id) full.id = d.id;
  } catch (e) {
    banner('warn', 'Device detail could not be fetched (' + errText(e) + '); showing the fleet row instead.', { key: 'devfetch' });
  }
  renderDeviceBody(full);
}

function renderDeviceBody(d) {
  const body = $('#device-body');
  const kv = (label, value) => [el('dt', null, label), el('dd', null, value === undefined || value === null || value === '' ? '—' : value)];

  const identity = el('dl', { class: 'kv' },
    kv('farm_uid', d.farmUID), kv('device id', d.id),
    kv('adb serial', el('span', null, d.serial || '—', d.serialAmbiguous ? el('span', { class: 'chip chip-degraded' }, 'not unique') : null)),
    kv('model', [d.manufacturer, d.model].filter(Boolean).join(' ')),
    kv('android', (d.android || '') + (d.sdk ? ' (sdk ' + d.sdk + ')' : '')),
    kv('pool', d.pool), kv('admin_state', d.adminState),
    kv('failure score', d.failureScore !== undefined ? String(d.failureScore) : '—'),
    kv('labels', d.labels ? JSON.stringify(d.labels) : '—'));

  const position = el('dl', { class: 'kv' },
    kv('rack slot', d.rackSlot || el('span', { class: 'stale' }, 'no rack_slot recorded')),
    kv('host', d.host), kv('hub', d.hubPath || d.hubID), kv('usb path', d.usbPath),
    kv('adb devpath', d.devPath), kv('slot id', d.slotID), kv('slot state', d.slotState));

  const health = el('dl', { class: 'kv' },
    kv('health', healthChip(d.health)),
    kv('since', d.healthSince ? el('span', null, fmtAbs(d.healthSince), ' (', fmtRel(d.healthSince), ')') : '—'),
    kv('adb_state', d.adbState), kv('battery', batteryEl(d.battery)),
    kv('battery temp', d.batteryTempDC !== undefined ? (Number(d.batteryTempDC) / 10).toFixed(1) + ' °C' : '—'),
    kv('consecutive bad', d.consecBad), kv('ladder tier', d.ladderTier),
    kv('last seen', d.lastSeen ? fmtRel(d.lastSeen) : '—'),
    kv('quarantine', d.quarantineID ? String(d.quarantineID) + ' — ' + (d.quarantineReason || '') : 'none'));

  const hasLease = d.leaseState === 'held' || d.leaseState === 'suspect';
  const lease = hasLease
    ? el('dl', { class: 'kv' },
      kv('state', el('span', { class: 'chips' }, leaseChips(d))),
      kv('fence', d.fence), kv('job', d.jobID), kv('tenant', d.tenant), kv('holder', d.holder),
      kv('acquired', d.acquiredAt ? fmtAbs(d.acquiredAt) + ' (' + fmtRel(d.acquiredAt) + ')' : '—'),
      kv('expires', d.expiresAt ? fmtAbs(d.expiresAt) + ' (' + fmtRel(d.expiresAt) + ')' : '—'),
      kv('reclaimable', d.reclaimableAt ? fmtAbs(d.reclaimableAt) + ' (' + fmtRel(d.reclaimableAt) + ')' : '—'))
    : emptyState('No live lease.', 'This device is free. Health has nothing to do with that: an offline device can still be held, and a healthy one can be idle.');

  const execInput = el('input', { type: 'text', placeholder: 'shell getprop ro.build.fingerprint', 'aria-label': 'ADB command to run on this device' });
  const execOut = el('pre', { class: 'out', hidden: true });
  const execBtn = el('button', {
    class: 'primary', onclick: async () => {
      const cmd = execInput.value.trim();
      if (!cmd) return;
      execBtn.disabled = true;
      execOut.hidden = false;
      execOut.textContent = 'running…';
      try {
        const resp = await api.post('devices/' + encodeURIComponent(d.id) + '/exec', { command: cmd, timeout_ms: 30000 });
        const code = pick(resp, 'exit_code');
        const stderr = pick(resp, 'stderr');
        execOut.textContent = 'exit_code ' + (code === undefined ? '?' : code) + '\n\n' +
          (pick(resp, 'output') || '(no output)') + (stderr ? '\n\n--- stderr ---\n' + stderr : '');
      } catch (e) {
        execOut.textContent = 'refused: ' + errText(e) +
          (e instanceof ApiError && e.detail !== undefined ? '\n' + JSON.stringify(e.detail, null, 2) : '');
      } finally {
        execBtn.disabled = false;
      }
    }
  }, 'Run');

  const actions = el('div', { class: 'actions' },
    hasLease ? el('button', {
      class: 'danger',
      onclick: () => revokeLease(normLease({
        id: d.leaseID, fence: d.fence, state: d.leaseState, protected: d.protected,
        device_id: d.id, rack_slot: d.rackSlot, job_id: d.jobID, holder: d.holder
      }))
    }, 'Revoke lease') : null,
    d.slotID !== undefined && d.slotID !== null ? el('button', { onclick: () => powerSlot(d) }, 'Power-cycle slot') : null,
    d.quarantineID ? el('button', { onclick: () => closeQuarantine(normQuarantine({ id: d.quarantineID, scope: 'device', device_id: d.id, rack_slot: d.rackSlot, reason: d.quarantineReason })) }, 'Close quarantine') : null,
    d.host ? el('button', {
      onclick: () => drainHost(d.host, (state.data.fleet || []).filter((x) => x.host === d.host))
    }, 'Drain host ' + d.host) : null);

  body.replaceChildren(
    el('h3', { class: 'section-h' }, 'Identity'), identity,
    el('h3', { class: 'section-h' }, 'Physical position'), position,
    el('h3', { class: 'section-h' }, 'Health'), health,
    el('h3', { class: 'section-h' }, 'Lease'), lease,
    el('h3', { class: 'section-h' }, 'Operator actions'), actions,
    el('h3', { class: 'section-h' }, 'Run one ADB command'),
    el('div', { class: 'exec-row' }, execInput, execBtn), execOut,
    el('h3', { class: 'section-h' }, 'Raw API row'),
    el('details', null, el('summary', null, 'every field the API returned'),
      el('pre', { class: 'out' }, JSON.stringify(d.raw, null, 2))));
}

/* ------------------------------------------------------------------ *
 * Live updates
 * ------------------------------------------------------------------ */

let es = null;
let esFailures = 0;
let esRetry = null;
let pollTimer = null;

function setConn(mode, text) {
  state.conn.mode = mode;
  const box = $('#conn');
  box.className = 'conn conn-' + (mode === 'live' ? 'live' : mode === 'polling' ? 'polling' : mode === 'down' ? 'down' : 'connecting');
  $('#conn-text').textContent = text || mode;
  box.title = mode === 'live'
    ? 'Server-sent events are connected; the page updates as the control plane changes.'
    : mode === 'polling'
      ? 'The event stream is down. The page is refetching every 5 seconds instead.'
      : mode === 'down'
        ? 'The control plane is not answering this browser.'
        : 'Opening the event stream…';
}

function startPolling() {
  if (pollTimer) return;
  pollTimer = setInterval(() => { loadFor(state.view); if (state.view !== 'fleet') loaders.fleet(); }, 5000);
}

function stopPolling() {
  if (!pollTimer) return;
  clearInterval(pollTimer);
  pollTimer = null;
}

function connectStream() {
  if (!('EventSource' in window)) {
    setConn('polling', 'no SSE — polling');
    startPolling();
    return;
  }
  // Never leave an old stream open behind a new one: an orphaned EventSource
  // keeps a connection slot on the control plane and keeps delivering events
  // into handlers nothing will ever close.
  if (es) { try { es.close(); } catch (_) { /* already closed */ } es = null; }
  try {
    es = new EventSource(apiURL('stream').toString());
  } catch (err) {
    onStreamFailure();
    return;
  }
  setConn('connecting');
  es.addEventListener('open', () => {
    esFailures = 0;
    setConn('live');
    stopPolling();
    refreshAll();
  });
  for (const name of ['fleet', 'lease', 'recovery', 'job', 'alert']) {
    es.addEventListener(name, (ev) => onStreamEvent(name, ev));
  }
  es.addEventListener('message', (ev) => onStreamEvent('message', ev));
  es.addEventListener('error', () => {
    // EventSource retries by itself while CONNECTING; only a CLOSED stream
    // needs us to rebuild it.
    if (es && es.readyState === EventSource.CLOSED) {
      try { es.close(); } catch (_) { /* already closed */ }
      es = null;
      onStreamFailure();
    } else {
      onStreamFailure(true);
    }
  });
}

function restartStream() {
  if (esRetry) { clearTimeout(esRetry); esRetry = null; }
  if (es) { try { es.close(); } catch (_) { /* already closed */ } es = null; }
  esFailures = 0;
  connectStream();
}

function onStreamFailure(retrying) {
  esFailures += 1;
  if (esFailures >= 2) {
    startPolling();
    // "API unreachable" outranks "stream down": when every request is failing,
    // saying only that the stream is down understates it.
    if (state.conn.mode !== 'down') setConn('polling', 'stream down — polling');
  } else if (state.conn.mode !== 'down') {
    setConn('connecting', 'reconnecting');
  }
  if (retrying) return;                       // the browser is already retrying
  if (esRetry) return;
  const wait = Math.min(30000, 1000 * Math.pow(2, Math.min(esFailures, 5)));
  esRetry = setTimeout(() => { esRetry = null; connectStream(); }, wait);
}

function onStreamEvent(name, ev) {
  state.conn.lastEvent = Date.now();
  if (state.conn.mode !== 'live') { setConn('live'); stopPolling(); }
  let payload = null;
  if (ev && typeof ev.data === 'string' && ev.data.length) {
    try { payload = JSON.parse(ev.data); } catch (_) { payload = { message: ev.data }; }
  }
  switch (name) {
    case 'alert': {
      // The stream sends alerts as {"alerts":[{kind,message,...}]} — a batch,
      // because a hub failing produces one alert about the hub rather than one
      // per phone. Reading payload.message off the envelope found nothing and
      // showed a placeholder, throwing away the only sentence that said what
      // had happened. Each alert is rendered in the server's own words.
      const list = payload && Array.isArray(payload.alerts) ? payload.alerts : payload ? [payload] : [];
      if (!list.length) {
        banner('warn', 'The control plane raised an alert this browser could not read: ' +
          (ev && ev.data ? String(ev.data).slice(0, 300) : 'no payload'), { key: 'alert-unreadable' });
      }
      for (const a of list) renderAlert(a);
      markDirty('fleet');
      break;
    }
    case 'fleet': markDirty('fleet', 'topology'); break;
    case 'lease': markDirty('leases', 'fleet'); break;
    case 'recovery': markDirty('recovery', 'fleet', 'bulkRun'); break;
    case 'job': markDirty('jobs', 'leases'); break;
    default: markDirty('fleet', 'leases', 'jobs', 'recovery', 'bulk', 'bulkRun', 'events'); break;
  }
}

/* renderAlert shows one server alert. The text is the server's; only the
 * severity is chosen here, and only from the server's own `kind`, so nothing
 * on screen says more than the control plane said. */
function renderAlert(a) {
  if (!a || typeof a !== 'object') {
    banner('warn', 'alert: ' + String(a), { key: 'alert-scalar' });
    return;
  }
  const kind = String(pick(a, 'kind') || '');
  const msg = pick(a, 'message', 'text', 'detail');
  const text = typeof msg === 'string' && msg
    ? msg
    // No message field: print the alert itself rather than inventing a sentence.
    : (kind ? kind + ': ' : '') + JSON.stringify(a);

  const declared = String(pick(a, 'level') || '');
  const level = declared
    ? (declared === 'error' || declared === 'critical' ? 'error' : declared === 'info' ? 'info' : 'warn')
    : /_recovered$|_cleared$|_resolved$/.test(kind) ? 'info' : 'warn';

  // One banner per subject, so a hub that flaps replaces its own line instead
  // of stacking a column of near-identical warnings over the fleet.
  const subject = pick(a, 'hub_id', 'gap_id', 'lease_id', 'device_id', 'host_id');
  const key = 'alert:' + (kind || 'unkinded') + (subject === undefined ? '' : ':' + subject);

  banner(level, text, {
    key,
    action: kind === 'hub_correlation' && pick(a, 'host_id') !== undefined
      ? {
        label: 'Show that hub',
        run: () => {
          setView('fleet');
          const hub = pick(a, 'hub_id', 'usb_path');
          setFilters({ host: String(pick(a, 'host_id')), hub: hub === undefined ? '' : String(hub) });
        }
      }
      : null
  });
}

const dirty = new Set();
let dirtyTimer = null;

/* markDirty coalesces a burst of events into one refetch per resource. A hub
 * going down produces dozens of events in a second; refetching once is the
 * difference between a dashboard and a self-inflicted load test. */
function markDirty(...names) {
  for (const n of names) dirty.add(n);
  if (dirtyTimer) return;
  dirtyTimer = setTimeout(() => {
    dirtyTimer = null;
    const needs = VIEW_NEEDS[state.view] || [];
    const wanted = Array.from(dirty);
    dirty.clear();
    for (const n of wanted) {
      if (n === 'bulkRun') { if (state.view === 'bulk') loaders.bulkRun(); continue; }
      if (needs.includes(n) || n === 'fleet') loaders[n] && loaders[n]();
    }
  }, 400);
}

/* ------------------------------------------------------------------ *
 * Routing, filters, keyboard
 * ------------------------------------------------------------------ */

let writingHash = false;

function buildHash() {
  const p = new URLSearchParams();
  if (state.q) p.set('q', state.q);
  const f = state.filters;
  if (state.view === 'fleet') {
    if (f.host) p.set('host', f.host);
    if (f.hub) p.set('hub', f.hub);
    if (f.health) p.set('health', f.health);
    if (f.pool) p.set('pool', f.pool);
    if (f.lease) p.set('lease', f.lease);
  }
  if (state.view === 'leases' && f.leaseState) p.set('state', f.leaseState);
  if (state.view === 'jobs' && f.jobState) p.set('state', f.jobState);
  if (state.view === 'bulk' && state.bulkRunID) p.set('run', String(state.bulkRunID));
  const qs = p.toString();
  return '/' + state.view + (qs ? '?' + qs : '');
}

function syncHash() {
  const h = buildHash();
  if ('#' + h === location.hash) return;
  writingHash = true;
  location.hash = h;
}

function applyHash() {
  const raw = location.hash.replace(/^#\/?/, '');
  const [path, qs] = raw.split('?');
  const view = VIEWS.includes(path) ? path : 'fleet';
  const p = new URLSearchParams(qs || '');
  state.view = view;
  state.q = p.get('q') || '';
  const f = state.filters;
  f.host = p.get('host') || '';
  f.hub = p.get('hub') || '';
  f.health = p.get('health') || '';
  f.pool = p.get('pool') || '';
  f.lease = p.get('lease') || '';
  if (view === 'leases') f.leaseState = p.get('state') || '';
  if (view === 'jobs') f.jobState = p.get('state') || '';
  if (view === 'bulk' && p.get('run')) state.bulkRunID = p.get('run');
  syncControls();
  showView(view);
  loadFor(view);
  render();
}

function syncControls() {
  $('#q').value = state.q;
  $('#f-health').value = state.filters.health;
  $('#f-lease').value = state.filters.lease;
  $('#l-state').value = state.filters.leaseState;
  $('#j-state').value = state.filters.jobState;
  $('#e-limit').value = state.filters.eventLimit;
  $('#e-kind').value = state.filters.eventKind;
}

function showView(view) {
  for (const v of VIEWS) {
    const sec = $('#view-' + v);
    const tab = $('#tab-' + v);
    const on = v === view;
    sec.hidden = !on;
    tab.setAttribute('aria-selected', on ? 'true' : 'false');
  }
}

function setView(view) {
  if (state.view === view) return;
  state.view = view;
  showView(view);
  syncHash();
  loadFor(view);
  render();
  const sec = $('#view-' + view);
  if (sec) sec.focus();
}

function setFilter(name, value) {
  setFilters({ [name]: value });
}

/* setFilters applies several filters as one change. Setting them one at a time
 * fired one /fleet request per filter, and the responses could land in either
 * order — the grid would settle on whichever the server answered last, which
 * is not necessarily the one the operator asked for. */
function setFilters(patch) {
  Object.assign(state.filters, patch);
  syncControls();
  $('#f-host').value = state.filters.host;
  $('#f-hub').value = state.filters.hub;
  $('#f-pool').value = state.filters.pool;
  syncHash();
  loaders.fleet();
  render();
}

/* ------------------------------------------------------------------ *
 * Render entry point
 * ------------------------------------------------------------------ */

let renderQueued = false;

function render() {
  if (renderQueued) return;
  renderQueued = true;
  requestAnimationFrame(() => {
    renderQueued = false;
    try {
      switch (state.view) {
        case 'fleet': renderFleet(); break;
        case 'leases': renderLeases(); break;
        case 'jobs': renderJobs(); break;
        case 'recovery': renderRecovery(); break;
        case 'bulk': renderBulk(); break;
        case 'events': renderEvents(); break;
        case 'docs': renderDocs(); break;
      }
      renderTabPips();
    } catch (e) {
      banner('error', 'The dashboard failed to draw this view: ' + String(e && e.message ? e.message : e), { key: 'render' });
    }
  });
}

/* The tab pips carry the two numbers worth interrupting for: devices that are
 * not healthy, and leases we cannot see the holder of. */
function renderTabPips() {
  const fleet = state.data.fleet || [];
  const bad = fleet.filter((d) => isFault(d.health)).length;
  setPip('#tab-fleet', bad, bad ? 'chip-degraded' : null, bad + ' devices not healthy');
  const leases = state.data.leases || [];
  const suspect = leases.filter((l) => l.state === 'suspect').length;
  setPip('#tab-leases', suspect, suspect ? 'chip-suspect' : null, suspect + ' suspect leases');
  const q = (state.data.quarantines || []).length;
  setPip('#tab-recovery', q, q ? 'chip-quarantined' : null, q + ' open quarantines');
}

function setPip(sel, n, cls, title) {
  const tab = $(sel);
  if (!tab) return;
  let pip = $('.pip', tab);
  if (!n) { if (pip) pip.remove(); return; }
  if (!pip) { pip = el('span', { class: 'pip' }); tab.append(pip); }
  pip.className = 'pip chip ' + (cls || 'chip-plain');
  pip.textContent = String(n);
  pip.title = title;
}

/* ------------------------------------------------------------------ *
 * Wiring
 * ------------------------------------------------------------------ */

function debounce(fn, ms) {
  let t = null;
  return (...args) => { if (t) clearTimeout(t); t = setTimeout(() => { t = null; fn(...args); }, ms); };
}

function wire() {
  for (const tab of $$('.tab')) {
    tab.addEventListener('click', () => setView(tab.dataset.view));
  }

  const onSearch = debounce(() => { syncHash(); loaders.fleet(); render(); }, 220);
  $('#q').addEventListener('input', (ev) => { state.q = ev.target.value; onSearch(); });

  $('#refresh').addEventListener('click', () => { refreshAll(); banner('info', 'Refetching every view from the API.'); });

  $('#f-host').addEventListener('change', (e) => setFilter('host', e.target.value));
  $('#f-hub').addEventListener('change', (e) => setFilter('hub', e.target.value));
  $('#f-health').addEventListener('change', (e) => setFilter('health', e.target.value));
  $('#f-pool').addEventListener('change', (e) => setFilter('pool', e.target.value));
  $('#f-lease').addEventListener('change', (e) => { state.filters.lease = e.target.value; syncHash(); render(); });
  $('#f-clear').addEventListener('click', () => {
    Object.assign(state.filters, { host: '', hub: '', health: '', pool: '', lease: '' });
    state.q = '';
    syncControls();
    $('#f-host').value = ''; $('#f-hub').value = ''; $('#f-pool').value = '';
    syncHash(); loaders.fleet(); render();
  });

  $('#l-state').addEventListener('change', (e) => { state.filters.leaseState = e.target.value; syncHash(); loaders.leases(); });
  $('#j-state').addEventListener('change', (e) => { state.filters.jobState = e.target.value; syncHash(); loaders.jobs(); });
  $('#e-limit').addEventListener('change', (e) => { state.filters.eventLimit = e.target.value; loaders.events(); });
  $('#e-kind').addEventListener('input', debounce((e) => { state.filters.eventKind = e.target.value; render(); }, 200));

  $('#job-form').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const err = $('#job-form-error');
    err.hidden = true; err.replaceChildren();
    let body;
    try { body = readJobForm(); } catch (e) {
      err.replaceChildren(el('strong', null, 'Cannot submit: '), String(e.message));
      err.hidden = false;
      return;
    }
    const btn = $('#job-form button[type="submit"]');
    btn.disabled = true;
    try {
      const resp = await api.post('jobs', body);
      const id = pick(resp, 'job_id', 'id') || (pick(resp, 'job') ? pick(pick(resp, 'job'), 'id') : null);
      banner('ok', 'Job submitted' + (id ? ' — ' + shortId(id) : '') + '.');
      loaders.jobs();
    } catch (e) {
      err.replaceChildren(el('strong', null, 'The server rejected this job: '), errText(e),
        e instanceof ApiError && e.detail !== undefined
          ? el('span', { class: 'fe-detail' }, typeof e.detail === 'string' ? e.detail : JSON.stringify(e.detail, null, 2))
          : null);
      err.hidden = false;
    } finally {
      btn.disabled = false;
    }
  });

  $('#bulk-form').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const err = $('#bulk-form-error');
    err.hidden = true; err.replaceChildren();
    let selector;
    try { selector = JSON.parse($('#bulk-selector').value.trim() || '{}'); } catch (e) {
      err.replaceChildren(el('strong', null, 'Selector is not valid JSON: '), String(e.message));
      err.hidden = false;
      return;
    }
    const body = {
      selector,
      command: $('#bulk-command').value.trim(),
      max_per_hub: Number($('#bulk-max').value) || 4,
      timeout_ms: Number($('#bulk-timeout').value) || 60000
    };
    const btn = $('#bulk-form button[type="submit"]');
    btn.disabled = true;
    try {
      const resp = await api.post('bulk', body);
      const id = pick(resp, 'run_id', 'id') || (pick(resp, 'run') ? pick(pick(resp, 'run'), 'id') : null);
      if (id) { state.bulkRunID = id; state.data.bulkRun = null; syncHash(); }
      banner('ok', 'Bulk run started' + (id ? ' — ' + shortId(id) : '') + '. Results appear below as each device answers.');
      loaders.bulk();
    } catch (e) {
      err.replaceChildren(el('strong', null, 'The server rejected this run: '), errText(e),
        e instanceof ApiError && e.detail !== undefined
          ? el('span', { class: 'fe-detail' }, typeof e.detail === 'string' ? e.detail : JSON.stringify(e.detail, null, 2))
          : null);
      err.hidden = false;
    } finally {
      btn.disabled = false;
    }
  });

  $('#device-close').addEventListener('click', () => $('#dlg-device').close());

  wireConfirm();
  wireToken();

  document.addEventListener('keydown', (ev) => {
    const t = ev.target;
    const typing = t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable);
    if (ev.key === 'Escape') {
      // <dialog> is supposed to close itself on Escape, but the close signal
      // does not always reach it (an embedded webview, a page that has already
      // consumed the key). An operator pressing Escape on a confirm dialog and
      // having nothing happen is not acceptable, so close it here. Topmost
      // first: confirm and token are opened over the device dialog.
      const stack = ['#dlg-confirm', '#dlg-token', '#dlg-device'];
      for (const sel of stack) {
        const dlg = $(sel);
        if (dlg && dlg.open) { ev.preventDefault(); dlg.close(); return; }
      }
      if (typing && t.id === 'q') { t.value = ''; state.q = ''; syncHash(); loaders.fleet(); render(); t.blur(); }
      return;
    }
    if (typing || ev.metaKey || ev.ctrlKey || ev.altKey) return;
    if ($('#dlg-confirm').open || $('#dlg-device').open || $('#dlg-token').open) return;
    if (ev.key === '/') { ev.preventDefault(); $('#q').focus(); $('#q').select(); return; }
    const n = Number(ev.key);
    if (n >= 1 && n <= VIEWS.length) { ev.preventDefault(); setView(VIEWS[n - 1]); }
  });

  window.addEventListener('hashchange', () => {
    if (writingHash) { writingHash = false; return; }
    applyHash();
  });
}

/* ------------------------------------------------------------------ *
 * Boot
 * ------------------------------------------------------------------ */

function boot() {
  wire();
  applyHash();
  loaders.fleet();
  loaders.hosts();
  loaders.topology();
  connectStream();

  // A bulk run in flight is the one place an operator watches a progress
  // number move, so poll its detail regardless of the stream.
  setInterval(() => {
    const run = state.data.bulkRun;
    if (state.view === 'bulk' && run && run.state === 'running') loaders.bulkRun();
  }, 2000);

  // Safety net: even with a healthy stream, refetch the open view now and
  // then, so a missed event cannot leave a stale screen in front of someone
  // making a decision.
  setInterval(() => { loadFor(state.view); }, 30000);

  // Relative times ("4m ago") are re-rendered on their own cadence, but not
  // while someone is reading: a repaint collapses an expanded output row and
  // an operator who just opened a stack trace should not lose it to a clock.
  setInterval(() => {
    if ($('#dlg-confirm').open || $('#dlg-device').open || $('#dlg-token').open) return;
    if ($('#main details[open]')) return;
    render();
  }, 15000);
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', boot);
} else {
  boot();
}
