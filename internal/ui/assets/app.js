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
  // The unit letters are not translated — s, m, h, d are read as symbols and a
  // translated one would not line up down a column. The direction is a word,
  // and a word that says "ago" to a Portuguese reader is a word they have to
  // stop and translate on a page that is otherwise theirs.
  return ago ? t('time.ago', { v: out }) : t('time.in', { v: out });
}

/* timeCell shows the relative distance, which is what an operator reads, with
 * the exact server instant on hover and in the accessible name. */
function timeCell(v, cls) {
  const d = parseTime(v);
  if (!d) return el('span', { class: 'chip chip-plain', title: t('word.notReported') }, '—');
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

/* farm.job_steps.state. 'skipped' and 'aborted' are deliberately not the same
 * colour as 'failed': a step that never ran did not break anything, and a panel
 * that paints the whole tail of a failed attempt red hides the one row that
 * actually stopped the job. */
const STEP_CLASS = {
  pending: 'chip-plain', running: 'chip-held', ok: 'chip-healthy',
  failed: 'chip-offline', skipped: 'chip-unknown', aborted: 'chip-degraded'
};

/* A step is a failure worth surfacing, as opposed to one that merely did not
 * run. This is the predicate the panel highlights on and the one the counts
 * beside the job use; they must agree, or the row and the tally disagree about
 * the same attempt. */
function stepFailed(s) { return s.state === 'failed' || s.state === 'aborted'; }

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
    /* The exec pre-flight reads this: a host with no recorded ADB endpoint is a
     * 409 the panel can predict from a row it already has, rather than a
     * refusal an operator meets after composing a command. */
    adbEndpoint: pick(raw, 'adb_endpoint'),
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
    // The lowest rung NOT yet spent. The old name is kept as a fallback so
    // this dashboard still reads a server that has not been updated.
    nextLadderTier: pick(raw, 'next_ladder_tier', 'ladder_tier'),
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

/* normStep is one row of farm.job_steps as GET /jobs/{id}/steps renders it.
 *
 * The three *_chars / *_truncated / *_omitted triples are carried rather than
 * flattened into the text, because they are the difference between a step that
 * printed nothing and one whose log the server could not afford to send. A
 * panel that dropped them would render both as an empty cell, which is the one
 * thing this endpoint was careful not to do. */
function normStep(raw) {
  return {
    raw,
    attempt: pick(raw, 'attempt'),
    index: pick(raw, 'step_index'),
    id: pick(raw, 'step_id', 'id'),
    kind: pick(raw, 'kind'),
    state: pick(raw, 'state') || 'unknown',
    startedAt: pick(raw, 'started_at'),
    finishedAt: pick(raw, 'finished_at'),
    // Computed by Postgres against now(), so a running step reports how long
    // it HAS been running. The browser clock never enters this number.
    durationS: pick(raw, 'duration_s'),
    exitCode: pick(raw, 'exit_code'),
    output: pick(raw, 'output'),
    outputChars: pick(raw, 'output_chars'),
    outputTruncated: pick(raw, 'output_truncated') === true,
    outputOmitted: pick(raw, 'output_omitted') === true,
    error: pick(raw, 'error'),
    errorChars: pick(raw, 'error_chars'),
    errorTruncated: pick(raw, 'error_truncated') === true,
    errorOmitted: pick(raw, 'error_omitted') === true,
    detail: pick(raw, 'detail'),
    detailOmitted: pick(raw, 'detail_omitted') === true
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
  filters: {
    host: '', hub: '', health: '', pool: '', lease: '', leaseState: '', jobState: '',
    // Which attempt the step panel is showing: '' is the newest attempt that
    // ran, 'all' is every placement, a number pins one. It is the server's own
    // ?attempt= vocabulary, passed through rather than reinvented here.
    stepAttempt: '',
    eventKind: '', eventLimit: '250'
  },
  data: {
    fleet: null, counts: null, hosts: null, hubs: null, topology: null,
    leases: null, leaseCounts: null, protectedSuspect: 0, jobs: null,
    tiers: null, attempts: null, quarantines: null, bulk: null, bulkRun: null, events: null,
    capabilities: null, kinds: null, jobSteps: null
  },
  /* Per-resource "the server capped this response". A capped list that does
   * not say so is the worst thing this page can show: it looks like the whole
   * farm and is not, and an operator counts what is in front of them. */
  truncated: {},
  errors: {},
  loading: {},
  bulkRunID: null,
  jobStepsID: null,
  conn: { mode: 'connecting', lastEvent: 0 }
};

/* liveSteps is which step each job is on, as the event stream last reported it:
 * job id -> {attempt, index, id, kind, state}.
 *
 * It is fed from the "job" frames and never from a fetch, because that is the
 * point — the jobs list is refetched at most once per coalescing window, and a
 * step transition should be visible before that. It is rebuilt from scratch on
 * every snapshot frame, which arrives on connect and on the server's periodic
 * resync, so a job that has aged out of the stream's window cannot leave a
 * stale row here forever. */
const liveSteps = new Map();

/* Which resources each view needs. Used both when switching views and when a
 * live event says a resource changed. */
const VIEW_NEEDS = {
  fleet: ['fleet', 'hosts'],
  leases: ['leases', 'fleet'],
  jobs: ['jobs', 'jobSteps', 'fleet'],
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
 * operator pastes into a ticket.
 *
 * That rule has exactly one consequence worth naming here, because it is the
 * one that looks like a reason to break it: EventSource cannot send a header,
 * so the live stream cannot present this token at all. It is not presented
 * anyway. connectStream mints a short-lived single-use ticket over a request
 * that CAN carry the header, and puts that in the stream's URL instead — a
 * capability worth one read-only connection for a few seconds, rather than the
 * credential that can revoke a lease. internal/api/stream_ticket.go is where
 * that trade is argued in full. */
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

  /* The step log of one job. Nothing is fetched until an operator picks a job:
   * this is the only read on the page whose response can carry megabytes of
   * somebody's shell output, and issuing it for every job in the table on every
   * poll would be a self-inflicted load test on a control plane that is, when
   * this panel gets opened, usually already having a bad day. */
  async jobSteps() {
    if (!state.jobStepsID) {
      state.data.jobSteps = null;
      delete state.errors.jobSteps;
      return;
    }
    const id = state.jobStepsID;
    const current = beginLoad('jobSteps');
    try {
      const resp = await api.get('jobs/' + encodeURIComponent(id) + '/steps',
        { attempt: state.filters.stepAttempt });
      if (!current()) return;
      const steps = listOf(resp, 'steps').map(normStep);
      state.data.jobSteps = {
        jobID: pick(resp, 'job_id') || id,
        jobState: pick(resp, 'job_state'),
        attempt: pick(resp, 'attempt'),
        maxAttempts: pick(resp, 'max_attempts'),
        attemptsWithSteps: listOf(resp, 'attempts_with_steps'),
        scope: pick(resp, 'scope'),
        states: (resp && !Array.isArray(resp) && resp.states) || null,
        // logs_omitted is not a detail: it is the server saying the steps are
        // all here and some of their text is not, which is the difference
        // between a quiet step and one whose log did not fit.
        logsOmitted: Number(pick(resp, 'logs_omitted') || 0),
        steps
      };
      state.truncated.jobSteps = !!pick(resp, 'truncated');
      mark('jobSteps');
    } catch (e) { if (!current()) return; mark('jobSteps', e); }
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

  /* THE SAME SENTENCE TWICE IS NOT TWO PIECES OF NEWS.
   *
   * Only a caller that passed a key was ever deduplicated, and the busiest
   * source of banners — the event stream — passes none: a farm refusing the
   * same tier-4 power cycle on three devices raised three identical rows. With
   * a cap of five that filled the strip, pushed every view below the fold, and
   * read as a broken page rather than as one recurring fact. It was the first
   * thing visible on this dashboard and it is what "everything is hard to get
   * to" looked like.
   *
   * So an identical message at the same level increments a count instead. The
   * count is the honest rendering: three refusals DID happen, and "×3" says so
   * in one row. The banner is moved to the end as it recurs, because a thing
   * that just happened again is news about now. */
  const sameText = String(text);
  for (const prev of host.children) {
    if (prev.dataset.level !== level || prev.dataset.text !== sameText) continue;
    const n = (Number(prev.dataset.count) || 1) + 1;
    prev.dataset.count = String(n);
    let tally = prev.querySelector('.b-tally');
    if (!tally) {
      tally = el('span', { class: 'b-tally' });
      prev.querySelector('.b-text').after(tally);
    }
    tally.textContent = '×' + n;
    host.append(prev);
    return prev;
  }
  const glyph = level === 'error' ? '✕' : level === 'warn' ? '▲' : level === 'ok' ? '✓' : 'i';
  const node = el('div', {
    class: 'banner banner-' + level,
    dataset: { level: level, text: sameText, count: '1' }
  },
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

/* errText renders a failure for a human.
 *
 * The API answers in English, always, because its messages are a contract that
 * scripts and `ctl` read too. So a Portuguese reader would otherwise meet an
 * English sentence at exactly the moment something went wrong — the worst
 * possible moment to be handed a language you do not read.
 *
 * The bridge is the CODE, which the envelope already carries and which is a
 * machine token rather than prose. A code this page knows gets a translated
 * sentence; a code it does not know keeps the server's own words, so a route
 * added tomorrow degrades to English prose rather than to nothing.
 *
 * THE CODE IS ALWAYS SHOWN, in both languages. It is what an operator quotes in
 * a ticket, greps for in a log, and matches against the API reference — and a
 * translation that hid it would make this page the only surface in the system
 * that cannot be cross-referenced with any other. */
function errText(e) {
  if (!(e instanceof ApiError)) return String((e && e.message) || e);
  const key = 'error.' + e.code;
  const known = t(key);
  return e.code + ': ' + (known === key ? e.message : known);
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

/* cstack and csub are how four tables lost twenty-two columns between them
 * without losing a single value.
 *
 * The Jobs table was fifteen columns and wanted 1182px inside an 897px panel on
 * an ordinary 1280px laptop: Created, Started, Finished, By and the Cancel
 * button were reachable only by scrolling a table sideways inside a page that
 * does not scroll, which is a fine way to hide five columns from anybody who
 * does not already know they are there.
 *
 * The observation that fixes it is that half of those columns were never
 * scanned down. Nobody reads the "created by" column of a job table; they read
 * it about ONE job, the one they have already found. A value like that does not
 * need a column of its own — it needs to be next to the thing that identifies
 * the row. So the identifier goes on the first line and its qualifiers go
 * underneath, each with its own name so the second line is never a bare value
 * whose meaning has to be guessed from its shape.
 *
 * The rule for which is which: if you would sort or filter by it, it is a
 * column. If you would only read it after you had found the row, it is a sub. */
function cstack(primary, ...subs) {
  return el('div', { class: 'cstack' }, primary, subs);
}

function csub(label, value, title) {
  if (value === null || value === undefined || value === '') return null;
  // The value is wrapped so it — and never the label — is the part that
  // ellipsises when the column is tight, and so that a cut value still says
  // what it was on hover.
  return el('span', { class: 'csub', title: title || (typeof value === 'string' ? value : null) },
    el('span', { class: 'clab' }, label), el('span', { class: 'cval' }, value));
}

/* panelState renders the honest not-yet / failed / nothing-there states so no
 * view is ever silently blank. */
function panelState(key, rows, empty) {
  const e = state.errors[key];
  const have = Array.isArray(rows) && rows.length > 0;
  if (e && !have) {
    return emptyState(t('panel.failed'),
      el('span', null, errText(e), e.detail ? el('span', { class: 'mono' }, ' ' + JSON.stringify(e.detail)) : null));
  }
  if (e) return null;   // stale rows beat a blank screen; the banner says so
  if (rows === null || rows === undefined) return emptyState(t('panel.loading'), t('panel.loadingDetail'));
  if (!rows.length) return empty;
  return null;
}

function countChips(obj) {
  const out = [];
  if (!obj || typeof obj !== 'object') return out;
  for (const k of Object.keys(obj)) {
    const v = obj[k];
    if (v === null || typeof v === 'object') continue;
    /* The key names a count the API computed — total, leased, free, unhealthy.
     * It is looked up as a translatable label and falls back to the key itself
     * with underscores opened out, which is what it always was.
     *
     * The fallback matters more than the lookup: this loop renders WHATEVER the
     * server put in the object, so a counter added to the API tomorrow appears
     * here before anybody has written a word for it. Reading `charge_parked` in
     * an English page is worse than reading "estacionados por carga" and far
     * better than the counter vanishing because no translation existed. */
    const label = t('count.' + k);
    out.push(el('span', { class: 'count' },
      label === 'count.' + k ? k.replace(/_/g, ' ') : label,
      ' ', el('b', null, String(v))));
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
    title: t('trunc.title') + ' ' + narrow + ' ' + t('trunc.tail')
  }, el('span', { 'aria-hidden': 'true' }, '▲'), t('trunc.chip'));
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
        ? el('span', { class: 'chip chip-protected', title: t('leases.protectedSuspectTitle') },
          el('span', { 'aria-hidden': 'true' }, '★'), t('leases.protectedSuspect') + ' ' + state.data.protectedSuspect)
        : null,
      el('span', { class: 'count' }, t('fleet.showing') + ' ', el('b', null, String(rows.length))),
      truncChip('leases', t('leases.narrow'))
    ]);
  }

  const problem = panelState('leases', rows0, emptyState(t('leases.empty'), t('leases.emptyDetail')));
  if (problem) { body.replaceChildren(problem); return; }

  body.setAttribute('aria-busy', 'false');
  /* NINE COLUMNS, DOWN FROM TWELVE.
   *
   * Five of the twelve were instants — acquired, heartbeat, expires,
   * reclaimable, witness — laid out as five columns of "3m ago" that an
   * operator reads in pairs: when did this start and is the holder still
   * talking; when does it turn suspect and when may the reaper act. So they
   * are two cells of a pair each, plus witness, which is the odd one because
   * most leases have none. Tenant joins the holder it belongs to and the queue
   * joins the job. Nothing left the page. */
  body.replaceChildren(table([
    {
      label: t('col.state'), cell: (l) => {
        const chips = [el('span', { class: 'chip chip-' + (l.state === 'suspect' ? 'suspect' : l.state === 'held' ? 'held' : 'plain') },
          el('span', { 'aria-hidden': 'true' }, l.state === 'suspect' ? '◐' : l.state === 'held' ? '●' : '○'), l.state)];
        if (l.protected) chips.push(el('span', { class: 'chip chip-protected' }, el('span', { 'aria-hidden': 'true' }, '★'), 'protected'));
        if (l.state === 'suspect') {
          chips.push(el('span', { class: 'chip chip-plain', title: t('leases.suspectAlertOnly') }, t('leases.holderNotVisible')));
        }
        return el('span', { class: 'chips' }, chips);
      }
    },
    { label: t('col.fence'), cls: 'num', cell: (l) => (l.fence === undefined ? '—' : String(l.fence)) },
    { label: t('col.device'), cell: (l) => deviceLabel(l.deviceID, l.rackSlot) },
    {
      label: t('col.job'), cell: (l) => cstack(
        el('span', { class: 'mono', title: String(l.jobID || '') }, shortId(l.jobID) || '—'),
        csub(t('sub.queue'), l.queue || null))
    },
    {
      label: t('col.holder'), cell: (l) => cstack(
        el('span', { class: 'mono trunc', title: (l.holder || '') + ' — ' + t('leases.holderTitle') }, l.holder || '—'),
        csub(t('sub.tenant'), l.tenant || null))
    },
    {
      label: t('col.acquired'), cell: (l) => cstack(
        timeCell(l.acquiredAt),
        csub(t('sub.heartbeat'), timeCell(l.heartbeatAt)))
    },
    {
      label: t('col.expires'), cell: (l) => cstack(
        timeCell(l.expiresAt),
        csub(t('sub.reclaimable'), timeCell(l.reclaimableAt), t('leases.reclaimableTitle')))
    },
    { label: t('col.witness'), cell: (l) => (l.witnessAt ? timeCell(l.witnessAt) : el('span', { class: 'chip chip-plain' }, t('word.none'))) },
    {
      label: t('col.actions'), cls: 'acts', cell: (l) => (l.state === 'held' || l.state === 'suspect')
        ? el('button', { class: 'mini danger', onclick: () => revokeLease(l) }, t('act.revoke'))
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
    append(counts, [countChips(by), el('span', { class: 'count' }, t('fleet.showing') + ' ', el('b', null, String(rows.length))),
      truncChip('jobs', t('jobs.narrow'))]);
  }

  const problem = panelState('jobs', rows0, emptyState(t('jobs.empty'), t('jobs.emptyDetail')));
  if (problem) { body.replaceChildren(problem); renderJobSteps(); return; }

  body.setAttribute('aria-busy', 'false');
  /* EIGHT COLUMNS, DOWN FROM FIFTEEN — and this is the table the whole
   * min-width argument in style.css was written about. At 1280px it wanted
   * 1182px inside an 897px panel and hid its last five columns behind a
   * sideways scrollbar.
   *
   * What went: `queue` under the pool it belongs to, `created_by` under the job
   * id it belongs to, `expected_duration` under the max runtime it is compared
   * against, `started_at` and `finished_at` under the `created_at` they follow,
   * the two guard chips under the state they guard, and the two action buttons
   * into one cell. Fifteen values, still fifteen values, eight headings. */
  body.replaceChildren(table([
    {
      /* The disruption policy is a labelled sub rather than a fifth chip.
       * `allow_port_power_cycle` in a chip is 130px that will not shrink —
       * a chip is `white-space: nowrap` by design — and one such value on
       * every row set the whole table's minimum width single-handed. As a
       * sub it keeps the value verbatim, gains the word POLICY in front of
       * it, and yields to an ellipsis with its full text on hover when the
       * column is tight. `protected` stays a chip: it is short, it is rare,
       * and it is the one thing on the row worth a colour. */
      label: t('col.state'), cell: (j) => cstack(
        el('span', { class: 'chips' },
          el('span', { class: 'chip ' + (JOB_CLASS[j.state] || 'chip-plain') }, j.state),
          j.protected ? el('span', { class: 'chip chip-protected', title: t('jobs.protectedTitle') }, el('span', { 'aria-hidden': 'true' }, '★'), 'protected') : null),
        csub(t('sub.policy'), j.policy || null, 'disruption_policy'))
    },
    { label: t('col.step'), cell: (j) => liveStepCell(j) },
    {
      label: t('col.job'), cell: (j) => cstack(
        el('span', { class: 'mono', title: String(j.id || '') }, shortId(j.id)),
        csub(t('sub.by'), j.createdBy || null))
    },
    {
      label: t('col.pool'), cell: (j) => cstack(
        j.pool || '—',
        csub(t('sub.queue'), j.queue || null))
    },
    { label: t('col.tenant'), cell: (j) => j.tenant || '—' },
    {
      label: t('col.limits'), cell: (j) => cstack(
        el('span', { title: t('jobs.maxRuntimeTitle') }, fmtInterval(j.maxRuntime)),
        csub(t('sub.expected'), j.expected === undefined || j.expected === null ? null : fmtInterval(j.expected), t('jobs.expectedTitle')))
    },
    {
      /* All three instants, not the newest one.
       *
       * This cell first showed `finished` when there was one and `started`
       * otherwise, which loses `started_at` entirely for every job that has
       * ended — and `started_at` is the only thing that says how long a job sat
       * in the queue before it was placed, and the only way to derive how long
       * it actually ran. It is rendered nowhere else in this app. A row grows
       * to three lines when a job has finished, which is the cost of the claim
       * this redesign makes: nothing the API returns stopped being on screen. */
      label: t('col.created'), cell: (j) => cstack(
        timeCell(j.createdAt),
        j.startedAt ? csub(t('sub.started'), timeCell(j.startedAt)) : null,
        j.finishedAt ? csub(t('sub.finished'), timeCell(j.finishedAt)) : null)
    },
    {
      label: t('col.actions'), cls: 'acts', cell: (j) => el('span', null,
        el('button', {
          class: 'mini' + (String(j.id) === String(state.jobStepsID) ? ' primary' : ''),
          onclick: () => selectJobSteps(j.id)
        }, String(j.id) === String(state.jobStepsID) ? t('act.selected') : t('act.steps')),
        ['queued', 'allocating', 'running'].includes(j.state)
          ? el('button', { class: 'mini danger', onclick: () => cancelJob(j) }, t('act.cancel'))
          : null)
    }
  ], rows, { rowClass: (j) => (String(j.id) === String(state.jobStepsID) ? 'row-held' : null) }));

  renderJobSteps();
}

/* selectJobSteps opens one job in the panel below. The pinned attempt is
 * cleared with the selection: ?attempt=3 means something different on the next
 * job, and carrying it over would answer a question nobody asked. */
function selectJobSteps(id) {
  state.jobStepsID = id ? String(id) : null;
  state.filters.stepAttempt = '';
  state.data.jobSteps = null;
  // The expanded-log keys are attempt/index pairs of the job being left.
  openLogs.clear();
  delete state.errors.jobSteps;
  syncHash();
  loaders.jobSteps();
  render();
}

/* liveStepCell is which step the job is on according to the event stream.
 *
 * The job list itself does not carry a step — it is farm.jobs, which knows
 * only that the job is running — so this reads the digest the stream delivers
 * on every step transition. When no frame has arrived for this job the cell
 * says so rather than showing an em dash that would read as "no steps". */
function liveStepCell(j) {
  const s = liveSteps.get(String(j.id));
  if (!s) {
    // No frame for this job. Which of the two reasons it is matters: a browser
    // that cannot open the stream at all shows an em dash on every row, and an
    // operator is owed the reason rather than left to conclude the jobs have
    // no steps.
    return el('span', {
      class: 'chip chip-plain',
      title: state.conn.mode === 'live' ? t('step.noFrame') : t('step.notLive')
    }, '—');
  }
  if (s.index === null || s.index === undefined || s.index < 0) {
    // -1 is the stream's own spelling of "no step of this attempt has started".
    const done = ['succeeded', 'failed', 'cancelled'].includes(j.state);
    return el('span', {
      class: 'chip chip-plain',
      title: done ? t('step.noneRanTitle') : t('step.notStartedTitle')
    }, done ? t('step.noneRan') : t('step.notStarted'));
  }
  return el('span', { class: 'chips' },
    el('span', {
      class: 'chip ' + (STEP_CLASS[s.state] || 'chip-plain'),
      title: t('step.frameTitle', { index: s.index, id: s.id, kind: s.kind || 'step', state: s.state, attempt: s.attempt })
    }, String(s.index), ' ', s.state),
    el('span', { class: 'mono trunc', title: s.id || '' }, s.id || ''));
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
    try { return JSON.parse(v); } catch (e) { throw new Error(t('form.badJSON', { field: label }) + ' ' + e.message); }
  };
  const body = {
    pool: $('#job-pool').value.trim(),
    queue: $('#job-queue').value.trim(),
    tenant: $('#job-tenant').value.trim(),
    spec: json('#job-spec', 'spec'),
    selector: json('#job-selector', 'selector'),
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
 * JOB STEPS
 *
 * "Which step failed, and what did it print" — answered on the page rather
 * than in a terminal. Everything below renders GET /jobs/{id}/steps exactly as
 * the server sent it, including the three ways that response admits it is not
 * complete: a truncated step list, a log cut at ?output_chars, and a log
 * dropped whole for the response budget. Rendering any of those as an empty
 * cell would tell an operator the step was quiet when it was not.
 * ------------------------------------------------------------------ */

/* stepTook renders how long a step took, or has been taking.
 *
 * Milliseconds below ten seconds, because an adb shell command on a healthy
 * phone finishes in a few hundred of them: whole seconds renders every one of
 * those as "0s" and erases the only number in the row that separates a fast
 * device from one that took twenty seconds to answer. */
function stepTook(s) {
  if (s.durationS === null || s.durationS === undefined) return '—';
  const ms = Number(s.durationS) * 1000;
  const out = ms < 10000 ? Math.round(ms) + 'ms' : fmtSecs(ms / 1000);
  // Server-side now() minus started_at: this step has not finished and the
  // number is still climbing.
  return s.finishedAt ? out : out + '…';
}

/* charNote is the true size of a log beside the piece of it that was sent, so
 * a cut log can never be mistaken for a complete one. */
function charNote(truncated, chars, rendered) {
  const n = Number(chars) || 0;
  if (!n) return '';
  if (!truncated) return t('log.chars', { n: n.toLocaleString() });
  return t('log.charsCut', { shown: Array.from(rendered).length.toLocaleString(), n: n.toLocaleString() });
}

/* openLogs is which log blocks the operator has expanded, keyed by
 * attempt/index/label.
 *
 * The panel is rebuilt from scratch on every refetch, and a running job's step
 * rows change while somebody is reading one. Without this, the log they opened
 * re-collapses under them every time the runner moves — which would defeat the
 * only thing this panel is for. Cleared when another job is selected, because
 * the keys belong to that job's rows. */
const openLogs = new Set();

/* logDetails is one collapsed block: a label, a one-line preview, and the whole
 * of what arrived inside. */
function logDetails(key, label, bad, summary, note, body) {
  return el('details', {
    open: openLogs.has(key) || null,
    ontoggle: (e) => { if (e.target.open) openLogs.add(key); else openLogs.delete(key); }
  },
  el('summary', null,
    el('span', { class: 'chip ' + (bad ? 'chip-offline' : 'chip-plain') }, label),
    el('span', { class: 'mono trunc-wide' }, summary),
    note ? el('span', { class: 'dim' }, note) : null),
  el('pre', { class: 'out' }, body));
}

/* logBlock is a stored log — an output or an error — previewed by its first
 * line and labelled with its true size. */
function logBlock(key, label, text, chars, truncated, bad) {
  const body = String(text);
  const firstLine = body.split('\n')[0].slice(0, 100);
  const summary = firstLine.trim() === ''
    ? t('log.whitespaceOnly', { n: (Number(chars) || 0).toLocaleString() })
    : firstLine;
  return logDetails(key, label, bad, summary, charNote(truncated, chars, body), body);
}

/* omittedChip is a log the server dropped for its size budget. The step is
 * still on screen; only its text is gone, and the remedy is one request. */
function omittedChip(label, chars, attempt) {
  const stored = Number(chars) || 0;
  return el('span', {
    class: 'chip chip-degraded',
    title: t('log.omittedTitle', { field: label, attempt })
  }, el('span', { 'aria-hidden': 'true' }, '▲'),
    t('log.omitted', { field: label }) + (stored ? ' ' + t('log.storedChars', { n: stored.toLocaleString() }) : ''));
}

function stepLogCell(s) {
  const parts = [];
  const key = (label) => s.attempt + '/' + s.index + '/' + label;
  // The error first: it is the field that says WHY the step stopped, and it is
  // the one the server protects with its own share of the response budget.
  if (s.error) parts.push(logBlock(key('error'), 'error', s.error, s.errorChars, s.errorTruncated, true));
  else if (s.errorOmitted) parts.push(omittedChip('error', s.errorChars, s.attempt));
  if (s.output) parts.push(logBlock(key('output'), 'output', s.output, s.outputChars, s.outputTruncated, false));
  else if (s.outputOmitted) parts.push(omittedChip('output', s.outputChars, s.attempt));

  const detail = s.detail && typeof s.detail === 'object' ? s.detail : null;
  if (detail && Object.keys(detail).length) {
    // Compact on one line as the preview, indented in the body: the preview is
    // there to be scanned, and pretty-printed JSON begins with a brace.
    parts.push(logDetails(key('detail'), 'detail', false,
      JSON.stringify(detail), '', JSON.stringify(detail, null, 2)));
  } else if (s.detailOmitted) {
    parts.push(omittedChip('detail', 0, s.attempt));
  }

  if (!parts.length) {
    return el('span', {
      class: 'chip chip-plain',
      title: s.state === 'pending' ? t('log.notRunTitle') : t('log.nothingStoredTitle')
    }, s.state === 'pending' ? t('step.notStarted') : t('log.nothingStored'));
  }
  return el('div', { class: 'steplogs' }, parts);
}

/* renderJobSteps draws the panel under the jobs table. */
function renderJobSteps() {
  const host = $('#job-steps');
  if (!host) return;

  if (!state.jobStepsID) {
    host.replaceChildren(emptyState(t('steps.none'), t('steps.noneDetail')));
    return;
  }
  if (state.errors.jobSteps && !state.data.jobSteps) {
    host.replaceChildren(emptyState(t('steps.failed'), errText(state.errors.jobSteps)));
    return;
  }
  const data = state.data.jobSteps;
  if (!data) {
    host.replaceChildren(emptyState(t('steps.loading'),
      'GET /api/v1/jobs/' + shortId(state.jobStepsID) + '/steps'));
    return;
  }

  const scope = el('select', {
    'aria-label': t('steps.whichAttempt'),
    onchange: (e) => {
      state.filters.stepAttempt = e.target.value;
      state.data.jobSteps = null;
      syncHash();
      loaders.jobSteps();
      render();
    }
  });
  // 'all' is offered even when only one attempt has steps, because the point
  // of the option is that a retry writes a NEW set of rows: comparing them is
  // how a job problem is told apart from a device problem.
  const pinned = state.filters.stepAttempt || '';
  const known = (data.attemptsWithSteps || []).map((n) => String(n));
  append(scope, [
    el('option', { value: '' }, t('steps.newest')),
    el('option', { value: 'all' }, t('steps.every')),
    known.map((n) => el('option', { value: n }, t('steps.attemptN', { n }))),
    // A pinned attempt with no rows — ?attempt=7 pasted from a link, or a
    // number typed into the URL — still needs an option, or assigning it below
    // silently snaps the control back to "newest" while the panel goes on
    // rendering the empty attempt 7. The two would then disagree, and the
    // operator could not click their way out: the control already reads
    // "newest", so choosing it fires no change event.
    pinned && pinned !== 'all' && !known.includes(pinned)
      ? el('option', { value: pinned }, t('steps.attemptNoSteps', { n: pinned }))
      : null
  ]);
  scope.value = pinned;

  const failed = (data.steps || []).filter(stepFailed).length;
  const header = el('div', { class: 'toolbar' },
    el('span', { class: 'mono', title: String(data.jobID || '') }, shortId(data.jobID)),
    el('span', { class: 'chip ' + (JOB_CLASS[data.jobState] || 'chip-plain') }, data.jobState || 'unknown'),
    el('span', {
      class: 'count',
      title: t('steps.attemptCountTitle')
    }, t('steps.attempt') + ' ', el('b', null, String(data.attempt === undefined ? '?' : data.attempt)),
      ' ' + t('steps.of') + ' ', String(data.maxAttempts === undefined ? '?' : data.maxAttempts)),
    el('label', null, t('steps.showing') + ' ', scope),
    el('span', { class: 'grow' }),
    el('span', { class: 'counts' },
      countChips(data.states),
      failed ? el('span', { class: 'chip chip-offline', title: t('steps.failedTitle') },
        el('span', { 'aria-hidden': 'true' }, '✕'), t('steps.nFailed', { n: failed })) : null,
      data.logsOmitted
        ? el('span', {
          class: 'chip chip-degraded',
          title: t('steps.logsOmittedTitle', { n: data.logsOmitted })
        }, el('span', { 'aria-hidden': 'true' }, '▲'), t('steps.logsOmitted', { n: data.logsOmitted }))
        : null,
      truncChip('jobSteps', t('steps.narrow'))));

  const empty = emptyState(t('steps.empty'),
    (data.attemptsWithSteps || []).length
      ? t('steps.emptyOther', { list: (data.attemptsWithSteps || []).join(', ') })
      : t('steps.emptyNone'));
  const problem = panelState('jobSteps', data.steps, empty);
  if (problem) { host.replaceChildren(header, problem); return; }

  /* Six columns, down from nine. `kind` sits under the step id it describes,
   * and the exit code under the state it explains — a NULL exit code on a
   * finished step is not a zero, so it still says so in words. */
  host.replaceChildren(header, table([
    {
      label: t('col.attempt'), cls: 'num', cell: (s) => el('span', {
        title: t('steps.attemptColTitle')
      }, String(s.attempt === undefined ? '—' : s.attempt))
    },
    { label: t('col.index'), cls: 'num', cell: (s) => String(s.index === undefined ? '—' : s.index) },
    {
      label: t('col.step'), cell: (s) => cstack(
        el('span', { class: 'mono', title: String(s.id || '') }, s.id || '—'),
        csub(t('sub.kind'), s.kind || 'unknown'))
    },
    {
      label: t('col.state'), cell: (s) => cstack(
        el('span', { class: 'chip ' + (STEP_CLASS[s.state] || 'chip-plain') }, s.state),
        // A NULL exit code on a finished step is not a zero: the step never got
        // one, which is a different fact and usually the more interesting one.
        csub(t('sub.exit'),
          s.exitCode === undefined || s.exitCode === null ? t('steps.noExit') : String(s.exitCode),
          s.exitCode === undefined || s.exitCode === null ? t('steps.noExitTitle') : null))
    },
    {
      // stepTook returns an em dash rather than null for a step with no
      // duration, and csub only suppresses null — so passing it straight
      // through printed "TOOK —" under every pending step.
      label: t('col.started'), cell: (s) => cstack(
        s.startedAt ? timeCell(s.startedAt) : '—',
        csub(t('sub.took'), s.durationS === null || s.durationS === undefined ? null : stepTook(s)))
    },
    { label: t('col.log'), cell: (s) => stepLogCell(s) }
  ], data.steps, {
    rowClass: (s) => (stepFailed(s) ? 'row-refused' : s.state === 'running' ? 'row-held' : null)
  }));
}

/* ------------------------------------------------------------------ *
 * RECOVERY
 * ------------------------------------------------------------------ */

function renderRecovery() {
  renderTiers();
  renderAttempts();
  renderQuarantines();
}

/* THE LADDER READS AS ONE THING NOW.
 *
 * It was a seven-column grid: tier number, name, description, then four chips
 * in four unlabelled tracks. The four were `power_domain`, `allow_soft_reset`,
 * `cd 5m` and `6/h` — the same four on every row, in the same order, with
 * nothing on screen saying which was the cooldown and which the hourly budget.
 * A reader learned that by hovering, one badge at a time, or not at all.
 *
 * Recovery CLIMBS: rung 0 is tried before rung 1, and the whole point of the
 * page is that an operator can see how far up the escalation the watchdog is
 * allowed to go and what each step would cost. Nine unrelated cards do not say
 * that. One rail with numbered rungs on it, a sentence per rung, and the four
 * numbers labelled underneath, does.
 *
 * The `.rung.blast-*` class names keep the snake_case values from
 * farm.recovery_tiers.blast_radius. They are how the CSS finds the rung and
 * how a reader matches a row against the column; renaming them would be churn
 * that helps nobody. */
function renderTiers() {
  const host = $('#tiers-body');
  const tiers = state.data.tiers;
  const problem = panelState('recovery', tiers, emptyState(t('recovery.ladderEmpty'), t('recovery.ladderEmptyDetail')));
  if (problem) { host.replaceChildren(problem); return; }
  host.setAttribute('aria-busy', 'false');
  host.replaceChildren(...tiers.map((tr) => el('div', { class: 'rung blast-' + tr.blast + (tr.enabled ? '' : ' disabled') },
    el('div', { class: 'rung-step' }, el('span', { class: 'tier' }, String(tr.tier))),
    el('div', { class: 'rung-main' },
      el('div', { class: 'rung-head' },
        el('span', { class: 'rname' }, tr.name || '—'),
        el('span', {
          class: 'chip ' + (tr.blast === 'device' ? 'chip-plain' : tr.blast === 'power_domain' ? 'chip-degraded' : 'chip-offline'),
          title: t('recovery.blastTitle')
        }, el('span', { 'aria-hidden': 'true' }, tr.blast === 'device' ? '·' : '▲'), tr.blast),
        tr.enabled ? null : el('span', { class: 'chip chip-offline', title: t('recovery.disabledTitle') },
          el('span', { 'aria-hidden': 'true' }, '⊘'), t('recovery.disabled'))),
      tr.description ? el('p', { class: 'rdesc' }, tr.description) : null,
      el('dl', { class: 'rung-meta' },
        el('div', { title: t('recovery.requiresTitle') },
          el('dt', null, t('recovery.requires')), el('dd', null, tr.requires || '—')),
        el('div', { title: t('recovery.cooldownTitle') },
          el('dt', null, t('recovery.cooldown')), el('dd', null, fmtInterval(tr.cooldown))),
        el('div', { title: t('recovery.budgetTitle') },
          el('dt', null, t('recovery.budget')),
          el('dd', null, t('recovery.perHour', { n: tr.maxPerHour !== undefined ? tr.maxPerHour : '—' }))))))));
}

function renderAttempts() {
  const host = $('#attempts-body');
  const rows0 = state.data.attempts;
  const q = state.q.trim().toLowerCase();
  const rows = (rows0 || []).filter((a) => !q || [a.host, a.tierName, a.outcome, a.refusal, a.rackSlot, a.deviceID,
    a.detail ? JSON.stringify(a.detail) : ''].join(' ').toLowerCase().includes(q));

  const problem = panelState('recovery', rows0, emptyState(t('recovery.attemptsEmpty'), t('recovery.attemptsEmptyDetail')));
  if (problem) { host.replaceChildren(problem); return; }
  host.setAttribute('aria-busy', 'false');
  host.replaceChildren(table([
    {
      label: t('col.started'), cell: (a) => {
        const s = parseTime(a.startedAt), f = parseTime(a.finishedAt);
        return cstack(timeCell(a.startedAt), csub(t('sub.took'), s && f ? fmtSecs((f - s) / 1000) : null));
      }
    },
    {
      label: t('col.tier'), cell: (a) => cstack(
        el('span', { class: 'mono' }, a.tier !== undefined ? String(a.tier) : '—'),
        csub(t('sub.rung'), a.tierName || tierName(a.tier) || null))
    },
    {
      label: t('col.device'), cell: (a) => cstack(
        deviceLabel(a.deviceID, a.rackSlot),
        csub(t('sub.host'), a.host || null))
    },
    {
      label: t('col.outcome'), cell: (a) => (a.outcome
        ? el('span', { class: 'chip ' + (OUTCOME_CLASS[a.outcome] || 'chip-plain') },
          el('span', { 'aria-hidden': 'true' }, a.outcome === 'recovered' ? '✓' : a.outcome === 'refused' ? '⊘' : a.outcome === 'failed' ? '✕' : '·'), a.outcome)
        : el('span', { class: 'chip chip-booting' }, el('span', { 'aria-hidden': 'true' }, '↻'), t('recovery.inFlight')))
    },
    {
      label: t('col.refusal'), cell: (a) => {
        if (a.refusal) return el('span', { class: 'mono', title: a.refusal }, a.refusal);
        if (a.detail && typeof a.detail === 'object' && Object.keys(a.detail).length) {
          const s = JSON.stringify(a.detail);
          return el('span', { class: 'mono trunc', title: s }, s);
        }
        return '—';
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
        ? el('span', { class: 'mono', title: t('quarantine.domainTitle', { by: parts.join(', ') }) }, 'power_domain · ' + parts.join(' · '))
        : unnamedSubject('power_domain');
    }
    default:
      return unnamedSubject(q.scope || 'unknown');
  }
}

function unnamedSubject(what) {
  return el('span', { class: 'chip chip-plain', title: t('quarantine.unnamedTitle', { what }) },
    t('quarantine.unnamed', { what }));
}

function renderQuarantines() {
  const host = $('#quarantines-body');
  const rows0 = state.data.quarantines;
  const problem = panelState('recovery', rows0, emptyState(t('recovery.quarantinesEmpty'), t('recovery.quarantinesEmptyDetail')));
  if (problem) { host.replaceChildren(problem); return; }
  host.setAttribute('aria-busy', 'false');
  host.replaceChildren(table([
    { label: t('col.scope'), cell: (q) => el('span', { class: 'chip chip-quarantined' }, el('span', { 'aria-hidden': 'true' }, '■'), q.scope) },
    { label: t('col.subject'), cell: (q) => quarantineSubject(q) },
    { label: t('col.reason'), cell: (q) => el('span', { title: q.reason || '' }, q.reason || '—') },
    {
      label: t('col.opened'), cell: (q) => cstack(
        timeCell(q.openedAt),
        csub(t('sub.source'), q.auto ? t('quarantine.automatic') : t('quarantine.operator')))
    },
    { label: t('col.actions'), cls: 'acts', cell: (q) => el('button', { class: 'mini', onclick: () => closeQuarantine(q) }, t('act.close')) }
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
  return el('span', { class: 'chips', title: t('bulk.progressTitle') },
    el('span', { class: 'count' }, String(done), ' / ', el('b', null, String(r.targetCount))),
    Number(r.errors) ? el('span', { class: 'chip chip-offline' }, 'error ' + r.errors) : null,
    Number(r.skipped) ? el('span', { class: 'chip chip-unknown' }, 'skipped ' + r.skipped) : null);
}

function renderBulk() {
  const runsHost = $('#bulk-runs');
  const rows0 = state.data.bulk;
  const problem = panelState('bulk', rows0, emptyState(t('bulk.empty'), t('bulk.emptyDetail')));
  if (problem) { runsHost.replaceChildren(problem); }
  else {
    runsHost.setAttribute('aria-busy', 'false');
    /* Seven columns, down from ten: the selector under the command it runs, the
     * timeout under the concurrency limit it pairs with, and the finish under
     * the start. */
    runsHost.replaceChildren(table([
      { label: t('col.state'), cell: (r) => runStateChip(r.state) },
      { label: t('col.progress'), cell: (r) => runProgress(r) },
      {
        label: t('col.command'), cell: (r) => cstack(
          el('span', { class: 'mono trunc', title: r.command || '' }, r.command || '—'),
          csub(t('sub.selector'), r.selector
            ? el('span', { class: 'mono trunc', title: JSON.stringify(r.selector) }, JSON.stringify(r.selector))
            : null))
      },
      {
        label: t('col.limits'), cell: (r) => cstack(
          t('bulk.perHub', { n: r.maxPerHub !== undefined ? r.maxPerHub : '—' }),
          csub(t('sub.timeout'), fmtInterval(r.timeout)))
      },
      {
        label: t('col.started'), cell: (r) => cstack(
          timeCell(r.createdAt),
          r.finishedAt ? csub(t('sub.finished'), timeCell(r.finishedAt)) : null)
      },
      { label: t('col.by'), cell: (r) => r.createdBy || '—' },
      {
        label: t('col.actions'), cls: 'acts', cell: (r) => el('button', {
          class: 'mini' + (String(r.id) === String(state.bulkRunID) ? ' primary' : ''),
          onclick: () => { state.bulkRunID = r.id; state.data.bulkRun = null; loaders.bulkRun(); render(); }
        }, String(r.id) === String(state.bulkRunID) ? t('act.selected') : t('act.open'))
      }
    ], rows0));
  }

  const detail = $('#bulk-detail');
  const run = state.data.bulkRun;
  if (state.errors.bulkRun) {
    detail.replaceChildren(emptyState(t('bulk.runFailed'), errText(state.errors.bulkRun)));
    return;
  }
  if (!state.bulkRunID) {
    detail.replaceChildren(emptyState(t('bulk.noRun'), t('bulk.noRunDetail')));
    return;
  }
  if (!run) { detail.replaceChildren(emptyState(t('bulk.loadingRun'), t('bulk.loadingRunDetail'))); return; }

  const targets = run.targets || [];
  const by = {};
  for (const tg of targets) by[tg.state] = (by[tg.state] || 0) + 1;

  const header = el('div', { class: 'toolbar' },
    el('span', { class: 'mono' }, run.command || ''),
    runStateChip(run.state),
    el('span', { class: 'grow' }),
    el('span', { class: 'counts' }, countChips(by),
      el('span', { class: 'count' }, t('bulk.targets') + ' ', el('b', null, String(targets.length)))));

  const body = targets.length ? table([
    { label: t('col.device'), cell: (tg) => deviceLabel(tg.deviceID, tg.rackSlot) },
    {
      label: t('col.state'), cell: (tg) => cstack(
        el('span', { class: 'chip ' + (TARGET_CLASS[tg.state] || 'chip-plain') }, tg.state),
        csub(t('sub.exit'), tg.exitCode === undefined || tg.exitCode === null ? null : String(tg.exitCode)))
    },
    {
      label: t('col.started'), cell: (tg) => cstack(
        tg.startedAt ? timeCell(tg.startedAt) : '—',
        csub(t('sub.took'), (() => { const s = parseTime(tg.startedAt), f = parseTime(tg.finishedAt); return s && f ? fmtSecs((f - s) / 1000) : null; })()))
    },
    {
      label: t('col.output'), cell: (tg) => {
        const text = tg.error ? tg.error : tg.output;
        if (!text) return el('span', { class: 'chip chip-plain' }, tg.state === 'pending' ? t('step.notStarted') : t('bulk.noOutput'));
        return el('details', null, el('summary', { class: 'mono trunc' }, String(text).split('\n')[0].slice(0, 80)),
          el('pre', { class: 'out' }, String(text)));
      }
    }
  ], targets, { rowClass: (tg) => (tg.state === 'error' ? 'row-refused' : null) })
    : emptyState(t('bulk.noTargets'), t('bulk.noTargetsDetail'));

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
      el('span', { class: 'count' }, t('fleet.showing') + ' ', el('b', null, String(rows.length)), ' / ' + rows0.length),
      truncChip('events', t('events.narrow'))
    ]);
  }

  const problem = panelState('events', rows0, emptyState(t('events.empty'), t('events.emptyDetail')));
  if (problem) { host.replaceChildren(problem); return; }
  host.setAttribute('aria-busy', 'false');
  host.replaceChildren(table([
    { label: t('col.when'), cell: (e) => timeCell(e.at) },
    { label: t('col.source'), cell: (e) => el('span', { class: 'chip ' + (e.source === 'audit' ? 'chip-protected' : 'chip-plain') }, e.source) },
    { label: t('col.kind'), cls: 'mono', cell: (e) => e.kind || '—' },
    { label: t('col.actor'), cell: (e) => e.actor || '—' },
    {
      label: t('col.subject'), cell: (e) => cstack(
        e.subject ? el('span', { class: 'mono trunc', title: e.subject }, e.subject) : deviceLabel(e.deviceID),
        csub(t('sub.job'), e.jobID ? el('span', { class: 'mono', title: String(e.jobID) }, shortId(e.jobID)) : null))
    },
    { label: t('col.reason'), cell: (e) => (e.reason ? el('span', { title: e.reason }, e.reason) : '—') },
    {
      label: t('col.detail'), cell: (e) => {
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
    el('div', null, t('confirm.subject') + ' ', el('span', { class: 'subject' }, subject)),
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
    const label = ok.textContent;
    ok.disabled = true; cancel.disabled = true;
    // An action that blocks on hardware says so on the button while it waits,
    // rather than looking like a click that did nothing.
    if (pendingConfirm.busyLabel) ok.textContent = pendingConfirm.busyLabel;
    try {
      const res = await pendingConfirm.run(reason);
      // done is a sentence, or a function of the server's answer for actions
      // whose outcome is only known once the server has acted.
      const done = typeof pendingConfirm.done === 'function' ? pendingConfirm.done(res) : pendingConfirm.done;
      $('#dlg-confirm').close();
      pendingConfirm = null;
      banner('ok', done);
      refreshAll();
    } catch (e) {
      showConfirmError(e);
    } finally {
      ok.disabled = false; cancel.disabled = false; ok.textContent = label;
    }
  });
  $('#confirm-cancel').addEventListener('click', () => { $('#dlg-confirm').close(); pendingConfirm = null; });
  $('#dlg-confirm').addEventListener('close', () => { pendingConfirm = null; });
}

function openTokenDialog() {
  const input = $('#token-input');
  input.value = apiToken;
  $('#token-note').textContent = apiToken
    ? 'A token is set for this tab. It is sent as a header on every request, including the one that mints the ' +
      'single-use ticket the live stream opens with — EventSource sends no headers of its own.'
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
      'The server checks the real power domain and refuses this if any live lease in it forbids the disruption. If it refuses, the reason appears here.',
      'The cycle happens while you wait: the host agent cuts VBUS, lets the port settle, restores it and waits for the port to enumerate again, which can take a couple of minutes. The answer is the agent’s outcome, written on the recovery attempt it closes.'
    ]),
    confirmLabel: 'Cut power to this slot',
    busyLabel: 'Cycling power — waiting for the host agent…',
    done: (res) => powerCycleOutcome(d, res),
    run: (reason) => api.post('slots/' + encodeURIComponent(d.slotID) + '/power', { reason })
  });
}

/* The 200 body of POST /slots/{id}/power is the outcome of a cycle that has
 * already happened, not an acknowledgement of a request, and the banner says
 * what the server said: which attempt closed, as what, and how long the port
 * took. A row the janitor had already closed is called out rather than
 * claimed. */
function powerCycleOutcome(d, res) {
  const r = res || {};
  const where = d.rackSlot || d.usbPath || ('slot ' + d.slotID);
  const secs = r.elapsed_ms !== undefined ? Math.round(r.elapsed_ms / 1000) + 's' : null;
  const port = 'VBUS was cut and restored for ' + where + (secs ? ' in ' + secs : '') +
    '; the agent saw the port enumerate again. ADB health is the watchdog’s to confirm.';
  if (r.attempt_id === undefined) return port;
  const row = r.closed === false
    ? ' Recovery attempt ' + r.attempt_id + ' had already been closed before this outcome arrived; the audit log carries it.'
    : ' Recovery attempt ' + r.attempt_id + ' is closed as ' + (r.outcome || 'recovered') + '.';
  return port + row;
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
 * Live updates
 * ------------------------------------------------------------------ */

let es = null;
let esFailures = 0;
let esRetry = null;
let pollTimer = null;

/* esGeneration is which connect attempt owns `es`.
 *
 * Opening the stream is no longer synchronous: a ticket has to be minted
 * first, and anything that happens while that request is in flight — the
 * operator saving a token, a retry firing — starts a newer attempt. The
 * generation is checked after every await, so an older attempt that finally
 * gets its ticket drops it instead of installing a second EventSource behind
 * the current one. */
let esGeneration = 0;

/* A ticket is single-use and lives in the memory of the api replica that
 * minted it, so a redeem that lands on a different replica fails once and
 * succeeds on the next try with a fresh one. That is the ordinary case on a
 * farm running more than one replica, and it must not look like an outage: a
 * stream that never opened is retried at once, a few times, before the page
 * starts announcing that it is polling.
 *
 * Five, not two, because the cost is asymmetric. Each attempt is an
 * independent draw — one in N replicas — so five turns a 25% chance of a
 * visible flip to "polling" at two replicas into under 2%, while the farm
 * where the retries are wasted is the one whose mint is ALSO failing, and
 * there the loop never starts: a failed mint goes straight to the backoff
 * below without spending any of this. */
const STREAM_TICKET_RETRIES = 5;
let ticketRetries = 0;

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

/* connectStream opens the event stream, authenticated.
 *
 * The API is header-only and EventSource sends no headers, which is why every
 * token-protected farm used to watch its dashboard fall back to polling within
 * two failed connects. So the credential does not travel on this request at
 * all: an ordinary POST — a fetch, with the Authorization header on it — mints
 * a short-lived single-use ticket for this caller's identity, and the ticket is
 * what the EventSource URL carries. The token stays in a header, which is the
 * rule stated next to readToken and the one thing this must not break.
 *
 * The server names the query parameter in its own answer rather than this page
 * hard-coding it, so the two cannot drift apart.
 *
 * Taking over reconnection is the price. A ticket is spent the moment it is
 * redeemed, so the browser's own retry — which replays the same URL — would
 * present a ticket this connection already burned. Every error therefore
 * rebuilds the stream around a freshly minted one, with the backoff below. */
async function connectStream() {
  if (!('EventSource' in window)) {
    setConn('polling', 'no SSE — polling');
    startPolling();
    return;
  }
  // Never leave an old stream open behind a new one: an orphaned EventSource
  // keeps a connection slot on the control plane and keeps delivering events
  // into handlers nothing will ever close.
  const gen = ++esGeneration;
  if (es) { try { es.close(); } catch (_) { /* already closed */ } es = null; }
  setConn('connecting');

  let ticket, param;
  try {
    // A failed mint is a failed stream and nothing more: request() has already
    // raised the banner that offers the token box on a 401, and the poll below
    // keeps the screen truthful meanwhile.
    const minted = await api.post('stream/ticket');
    ticket = pick(minted, 'ticket');
    param = pick(minted, 'param') || 'ticket';
    if (!ticket) throw new Error('the control plane minted no stream ticket');
  } catch (err) {
    if (gen !== esGeneration) return;
    onStreamFailure();
    return;
  }
  if (gen !== esGeneration) return;

  let opened = false;
  try {
    es = new EventSource(apiURL('stream', { [param]: ticket }).toString());
  } catch (err) {
    onStreamFailure();
    return;
  }
  // Named, because every handler below must act on the EventSource IT was
  // attached to rather than on whatever `es` points at by the time it fires.
  const source = es;
  source.addEventListener('open', () => {
    opened = true;
    esFailures = 0;
    ticketRetries = 0;
    setConn('live');
    stopPolling();
    refreshAll();
  });
  for (const name of ['fleet', 'lease', 'recovery', 'job', 'alert']) {
    source.addEventListener(name, (ev) => onStreamEvent(name, ev));
  }
  source.addEventListener('message', (ev) => onStreamEvent('message', ev));
  source.addEventListener('error', () => {
    // A newer attempt already owns the stream; this one is a ghost.
    if (gen !== esGeneration) { try { source.close(); } catch (_) { /* already closed */ } return; }
    try { source.close(); } catch (_) { /* already closed */ }
    es = null;
    // Never opened, and a ticket was minted for it: the likely reason is that
    // the redeem reached a different api replica from the mint, which the next
    // ticket fixes. Retry at once rather than declaring the stream down.
    if (!opened && ticketRetries < STREAM_TICKET_RETRIES) {
      ticketRetries += 1;
      connectStream();
      return;
    }
    onStreamFailure();
  });
}

function restartStream() {
  if (esRetry) { clearTimeout(esRetry); esRetry = null; }
  esGeneration += 1;
  if (es) { try { es.close(); } catch (_) { /* already closed */ } es = null; }
  esFailures = 0;
  ticketRetries = 0;
  connectStream();
}

function onStreamFailure() {
  esFailures += 1;
  // Each backoff cycle gets its own budget of immediate ticket retries: the
  // replica mismatch they exist for is a coin toss per attempt, not a state
  // the farm stays in, so spending the budget once must not disarm it for the
  // rest of the page's life.
  ticketRetries = 0;
  if (esFailures >= 2) {
    startPolling();
    // "API unreachable" outranks "stream down": when every request is failing,
    // saying only that the stream is down understates it.
    if (state.conn.mode !== 'down') setConn('polling', 'stream down — polling');
  } else if (state.conn.mode !== 'down') {
    setConn('connecting', 'reconnecting');
  }
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
    case 'job': {
      // The job frame carries which step each job is on, so the Step column
      // moves with the runner and costs nothing. A refetch is asked for only
      // when the job itself changed, and the step log below only when the job
      // it is showing moved — the digest is a pointer at one step, and reading
      // what that step printed is the one part that needs a request.
      const ch = takeJobSteps(payload);
      if (ch.jobsChanged) markDirty('jobs', 'leases');
      if (ch.selectedChanged) markDirty('jobSteps');
      if (ch.stepMoved && state.view === 'jobs') render();
      break;
    }
    default: markDirty('fleet', 'leases', 'jobs', 'jobSteps', 'recovery', 'bulk', 'bulkRun', 'events'); break;
  }
}

/* takeJobSteps reads the step out of a "job" frame into liveSteps, and reports
 * what kind of change it was.
 *
 * The distinction is the whole reason this function returns anything. Before
 * the digest carried a step, a job frame arrived on a state transition — four
 * or five times in a job's life — and refetching /jobs and /leases on each was
 * free. It now also arrives on every step transition, which for a thirty-step
 * spec is thirty more frames, one every couple of seconds. Refetching the job
 * list, the lease list and up to two megabytes of step log on each of those
 * would be a self-inflicted load test on a control plane that is, when this
 * page is open, usually already having a bad day.
 *
 * So a frame that only moved a step moves the Step column and nothing else:
 * the digest IS the answer to "which step", and no request is needed to render
 * it. The step log below is refetched only when the job it is showing moved.
 *
 * A snapshot frame carries every job the stream is watching, so it REPLACES
 * the map: a job that has aged out of the server's window must not leave a
 * step behind that this page would go on rendering as current. Its arrival is
 * treated as a full change, which is the resync cadence the page already had. */
function takeJobSteps(payload) {
  const out = { jobsChanged: false, selectedChanged: false, stepMoved: false };
  if (!payload || typeof payload !== 'object') return out;
  const snapshot = payload.snapshot === true;
  const rows = listOf(payload, snapshot ? 'jobs' : 'changed');
  if (snapshot) {
    liveSteps.clear();
    out.jobsChanged = true;
    out.selectedChanged = !!state.jobStepsID;
  }
  for (const raw of rows) {
    const id = pick(raw, 'job_id', 'id');
    if (!id) continue;
    const key = String(id);
    // step_index is -1 when nothing of this attempt has started, and 0 is a
    // real step, so the value is read directly rather than through pick's
    // "first key that is not null" — which is right for names and wrong here.
    const idx = raw.step_index === undefined ? pick(raw, 'stepIndex') : raw.step_index;
    const next = {
      jobState: pick(raw, 'state') || '',
      attempt: pick(raw, 'attempt'),
      index: idx === undefined || idx === null ? null : Number(idx),
      id: pick(raw, 'step_id') || '',
      kind: pick(raw, 'step_kind') || '',
      state: pick(raw, 'step_state') || ''
    };
    const prev = liveSteps.get(key);
    liveSteps.set(key, next);
    if (snapshot) continue;

    // A job this page has never seen, a state change or a retry is a change to
    // the job itself; anything else in the frame moved only the step.
    const known = prev && prev.jobState === next.jobState && prev.attempt === next.attempt;
    if (!known) out.jobsChanged = true;
    else out.stepMoved = true;
    if (key === String(state.jobStepsID)) out.selectedChanged = true;
  }
  return out;
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
  // The selected job and the attempt within it travel in the URL, so the
  // answer to "which step failed" is a link somebody can paste into a ticket.
  if (state.view === 'jobs' && state.jobStepsID) p.set('job', String(state.jobStepsID));
  if (state.view === 'jobs' && f.stepAttempt) p.set('attempt', f.stepAttempt);
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
  if (view === 'jobs') {
    f.jobState = p.get('state') || '';
    const job = p.get('job') || '';
    const attempt = p.get('attempt') || '';
    if (job !== (state.jobStepsID || '') || attempt !== f.stepAttempt) state.data.jobSteps = null;
    state.jobStepsID = job || null;
    f.stepAttempt = attempt;
  }
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
      err.replaceChildren(el('strong', null, t('jobs.cannotSubmit') + ' '), String(e.message));
      err.hidden = false;
      return;
    }
    const btn = $('#job-form button[type="submit"]');
    btn.disabled = true;
    try {
      const resp = await api.post('jobs', body);
      const id = pick(resp, 'job_id', 'id') || (pick(resp, 'job') ? pick(pick(resp, 'job'), 'id') : null);
      banner('ok', t('jobs.submitted') + (id ? ' — ' + shortId(id) : '') + '.');
      loaders.jobs();
    } catch (e) {
      err.replaceChildren(el('strong', null, t('jobs.rejected') + ' '), errText(e),
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
      err.replaceChildren(el('strong', null, t('bulk.badSelector') + ' '), String(e.message));
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
      banner('ok', t('bulk.started') + (id ? ' — ' + shortId(id) : '') + '. ' + t('bulk.startedDetail'));
      loaders.bulk();
    } catch (e) {
      err.replaceChildren(el('strong', null, t('bulk.rejected') + ' '), errText(e),
        e instanceof ApiError && e.detail !== undefined
          ? el('span', { class: 'fe-detail' }, typeof e.detail === 'string' ? e.detail : JSON.stringify(e.detail, null, 2))
          : null);
      err.hidden = false;
    } finally {
      btn.disabled = false;
    }
  });

  $('#device-close').addEventListener('click', () => $('#dlg-device').close());
  // A screen left running behind a closed drawer is a handset still encoding
  // for nobody. See closeScreenOnDrawerClose.
  closeScreenOnDrawerClose();

  wireFormPanels();
  wireLanguage();
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

/* wireFormPanels drives the two disclosure forms — Submit a job, Run a
 * command — that used to be permanent sidebars beside their tables.
 *
 * THE `[hidden]` TRAP LIVES HERE. `.formpanel-body` sets `display: grid`, and
 * an author `display` beats the user agent's `[hidden] { display: none }` at
 * any specificity. Without the global `[hidden] { display: none !important }`
 * at the top of style.css this form would be on screen from the first paint
 * with `aria-expanded="false"` beside it — a control that says closed over a
 * form that is open. That is the third time this stylesheet has met that trap
 * and the first time it was closed for the whole page rather than patched
 * for one component.
 *
 * The button carries `aria-expanded` and `aria-controls`, so a screen reader
 * is told the same thing the glyph says. The glyph is + and −, which survive
 * a greyscale screenshot; nothing here is signalled by colour alone. */
function wireFormPanels() {
  for (const btn of $$('.formpanel-toggle')) {
    const form = $('#' + btn.dataset.formpanel);
    if (!form) continue;
    const glyph = $('.fp-glyph', btn);
    const paint = () => {
      const open = btn.getAttribute('aria-expanded') === 'true';
      form.hidden = !open;
      if (glyph) glyph.textContent = open ? '−' : '+';
    };
    paint();
    btn.addEventListener('click', () => {
      const open = btn.getAttribute('aria-expanded') === 'true';
      btn.setAttribute('aria-expanded', open ? 'false' : 'true');
      paint();
      // Opening puts the caret in the first field. Somebody who clicked
      // "Submit a job" is about to type a pool name, not to read a legend.
      if (!open) {
        const first = form.querySelector('input, textarea, select');
        if (first) first.focus();
      }
    });
  }
}

/* wireLanguage drives the header's language switch.
 *
 * The button shows the language it would switch TO, not the one in use. That is
 * the only label a reader who cannot read the current language can still act on
 * — the point of the control is to escape a language you do not understand, and
 * a button labelled in that language would be part of the problem.
 *
 * Switching re-renders the whole view rather than reloading the page: a reload
 * would drop the event stream, the filters, and any drawer that was open, which
 * turns "I wanted to read this in Portuguese" into "I lost my place". */
function wireLanguage() {
  const btn = $('#lang-btn');
  if (!btn) return;

  const paint = () => { btn.textContent = currentLang() === 'pt' ? 'EN' : 'PT'; };
  paint();

  btn.addEventListener('click', () => {
    setLang(currentLang() === 'pt' ? 'en' : 'pt');
  });

  window.addEventListener('languagechange', () => {
    paint();
    /* Everything this app draws itself is redrawn. applyTranslations has already
     * handled the static HTML; render() handles the DOM built in JS, which is
     * most of it. A drawer that is open is rebuilt from the row it was opened
     * with, so its headings follow too — except a live screen, which is
     * deliberately left alone: re-rendering it would tear down a session and an
     * encoder on a phone to change a heading. */
    try { render(); } catch (_) { /* a redraw failure must not strand the switch */ }
  });
}
