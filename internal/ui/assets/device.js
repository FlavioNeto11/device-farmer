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
 */

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
    kv('consecutive bad', d.consecBad), kv('next recovery rung', d.nextLadderTier),
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

  /* The command box is exec.js's, and it is a whole widget rather than an input
   * and a button. What used to be here posted a fixed 30-second timeout and
   * rendered `'exit_code ' + code`, which is wrong in a way worth remembering:
   * the API returns exit_code -1 with exited:false as a 200 when the shell
   * stream carried no exit frame, and that is not a result — it means the
   * command may still be running on the phone. See exec.js's header. */
  const execBox = execPanel(d);

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
    el('h3', { class: 'section-h' }, t('device.identity')), identity,
    el('h3', { class: 'section-h' }, t('device.position')), position,
    el('h3', { class: 'section-h' }, t('device.health')), health,
    el('h3', { class: 'section-h' }, t('device.lease')), lease,
    el('h3', { class: 'section-h' }, t('device.actions')), actions,
    el('h3', { class: 'section-h' }, t('exec.title')),
    execBox,
    el('h3', { class: 'section-h' }, t('device.screen')), screenPanel(d),
    el('h3', { class: 'section-h' }, t('device.raw')),
    el('details', null, el('summary', null, t('device.rawSummary')),
      el('pre', { class: 'out' }, JSON.stringify(d.raw, null, 2))));
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
