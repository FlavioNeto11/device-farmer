-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- farm.resolve_device could not report that identity was CONTESTED.
--
-- The resolution ladder has always known how to notice a collision. Rung
-- 2 sets 'ambiguous' when a hardware fingerprint matches more than one
-- device; rung 3 does the same for a duplicated ADB serial, and writes
-- farm.devices.serial_ambiguous on every row carrying that serial. But
-- neither rung can name a device — that is what ambiguous MEANS — so
-- v_dev stays NULL, rung 4 fires on `IF v_dev IS NULL`, adopts, and
-- overwrites the answer with an unconditional 'adopted_new'.
--
-- The word therefore never left the function. The caller was told a
-- clean adoption had happened, on evidence that had just been found to
-- contradict itself.
--
-- That lie is written down. internal/enroll copies this return value
-- into farm.identity_observations.resolution, and that table exists for
-- exactly one purpose, stated in 00004: "Every sighting is recorded
-- before any conclusion is drawn, so a wrong adoption can be explained
-- afterwards instead of guessed at." A wrong adoption recorded as
-- 'adopted_new' cannot be explained afterwards — the row says the fleet
-- had never seen this device, when in fact it holds rows that collide
-- with it. The partial index ident_obs_conflict, which indexes exactly
-- ('ambiguous','conflict','unreadable') so a human can find these,
-- matched nothing for the case it was built for.
--
-- The fix carries the ambiguity forward in its own variable instead of
-- in v_res, which any later rung is free to overwrite:
--
--   * A rung that POSITIVELY IDENTIFIES the device still wins and still
--     reports its own name. Ambiguity at a weaker rung is not a verdict;
--     if the branded uid or the serial-and-slot pair then names one
--     device, the ladder decided, and saying 'ambiguous' would throw
--     away a real identification.
--   * A rung that could not decide, followed by an ADOPTION, reports
--     'ambiguous'. The adoption still happens and the device_id is still
--     returned: the handset is physically present, it needs a row, a
--     slot occupancy and a brand, and refusing to adopt would leave
--     every duplicate-serial device permanently unusable. What changes
--     is only that the caller is told the truth about the evidence.
--
-- The post-insert re-check counts too, and it is the common path: when
-- the SECOND clone of a duplicated serial arrives, the count at the
-- moment of its own lookup is still one, so rung 3 sees nothing and the
-- collision only becomes visible after the INSERT. That branch already
-- flagged both devices; now it also reports what it found.
--
-- THE INVARIANT CALLERS MAY RELY ON: 'ambiguous' is returned ONLY from
-- the adoption branch. Every rung that can detect a collision names no
-- device by definition, so control always reaches rung 4 and the row is
-- always created. An ambiguous answer therefore describes the EVIDENCE,
-- never a half-finished write, and it always arrives with a device_id.
-- internal/enroll depends on this to raise device_adopted (with
-- "ambiguous": true) and to count farm_enroll_adopted_total for a
-- contested adoption, so a tray of clone-serial handsets still shows up
-- on the timeline as devices joining the fleet. Any future rung that
-- wants to report ambiguity WITHOUT adopting must give that case its own
-- word rather than reusing this one.
--
-- Two more things worth stating, since this changes an answer the caller
-- branches on:
--
--   * 'ambiguous' is already in the CHECK constraint on
--     farm.identity_observations.resolution and already a constant in
--     internal/enroll, so nothing downstream needs to learn a new word.
--     A fingerprint collision has no flag column of its own, so for that
--     rung the observation row is the ONLY record that it happened;
--     losing it to 'adopted_new' lost it entirely.
--   * The sighting is now also counted as
--     farm_enroll_resolutions_total{resolution="ambiguous"}, whose help
--     text already says ambiguous "needs a human", and it lands in the
--     ident_obs_conflict index WITH its device_id. A duplicated serial
--     additionally flags every colliding row, which /api/v1/fleet and
--     the watchdog's ambiguous_serials gauge already surface.
--
-- Not changed here, deliberately: WHICH device each rung matches. Rung 3
-- still refuses to identify anything when a serial is duplicated, even
-- though devices_one_per_slot means serial-plus-slot could still pick
-- out at most one row. That is a change to what counts as proof of
-- identity and belongs in its own review; this migration only stops the
-- function misreporting the conclusion it already reached.
--
-- This supersedes the definition in 00005_correctness.sql and carries
-- its fix forward unchanged: the fingerprint rung counts first and
-- fetches second, because min(uuid) does not exist in PostgreSQL and
-- calling it made the whole rung throw at the moment it was needed.
-- =====================================================================
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION farm.resolve_device(
  p_host_id    text,
  p_usb_path   text,
  p_farm_uid   text,
  p_hw_fp      bytea,
  p_serial     text,
  p_pool_id    text DEFAULT 'default',
  p_props      jsonb DEFAULT '{}'::jsonb
) RETURNS TABLE (device_id uuid, resolution text, slot_id bigint)
LANGUAGE plpgsql AS $fn$
-- The OUT names shadow columns of farm.devices and farm.device_runtime, and
-- ON CONFLICT's target cannot be qualified, so bare names inside queries mean
-- the column. Assignment targets are still variables.
#variable_conflict use_column
DECLARE
  v_slot  bigint;
  v_dev   uuid;
  v_res   text;
  v_uid   text;
  v_cnt   int;
  -- Ambiguity lives here rather than in v_res because v_res is what each
  -- rung overwrites as it goes. A rung that finds a collision has not
  -- decided anything, so it must not be the last writer of the answer: it
  -- records the fact, and the adoption branch reads it back.
  v_amb   boolean := false;
BEGIN
  SELECT s.id INTO v_slot FROM farm.slots s
   WHERE s.host_id = p_host_id AND s.usb_path = p_usb_path;
  IF v_slot IS NULL THEN
    RAISE EXCEPTION 'no slot registered at %/% — run topology discovery first',
      p_host_id, p_usb_path USING ERRCODE = 'no_data_found';
  END IF;

  -- 1. The device is carrying our brand. We wrote it, so it is ours.
  IF p_farm_uid IS NOT NULL AND p_farm_uid <> '' THEN
    SELECT d.id INTO v_dev FROM farm.devices d WHERE d.farm_uid = p_farm_uid;
    IF v_dev IS NOT NULL THEN
      v_res := 'branded_uid';
    END IF;
  END IF;

  -- 2. Hardware fingerprint. Count first, then fetch: a fingerprint that
  --    matches more than one device identifies nothing, and one that
  --    matches exactly one needs no aggregate to pick it out.
  IF v_dev IS NULL AND p_hw_fp IS NOT NULL THEN
    SELECT count(*) INTO v_cnt FROM farm.devices d WHERE d.hw_fingerprint = p_hw_fp;
    IF v_cnt = 1 THEN
      SELECT d.id INTO v_dev FROM farm.devices d WHERE d.hw_fingerprint = p_hw_fp;
      v_res := 'hw_fingerprint';
    ELSIF v_cnt > 1 THEN
      -- Two devices claiming one fingerprint is a data problem worth
      -- recording, not a coin flip between them. There is no
      -- fingerprint_ambiguous column, so the observation row this answer
      -- becomes is the only place it is written down.
      v_amb := true;
    END IF;
  END IF;

  -- 3. Serial, but ONLY together with the slot it was last seen in.
  --    A serial on its own identifies nothing: clone serials are real.
  IF v_dev IS NULL AND p_serial IS NOT NULL AND p_serial <> '' THEN
    SELECT count(*) INTO v_cnt FROM farm.devices d WHERE d.adb_serial = p_serial;
    IF v_cnt = 1 THEN
      SELECT d.id INTO v_dev FROM farm.devices d
       WHERE d.adb_serial = p_serial AND d.current_slot_id = v_slot;
      IF v_dev IS NOT NULL THEN
        v_res := 'serial_and_slot';
      END IF;
    ELSIF v_cnt > 1 THEN
      v_amb := true;
      UPDATE farm.devices d SET serial_ambiguous = true
       WHERE d.adb_serial = p_serial AND NOT d.serial_ambiguous;
    END IF;
  END IF;

  -- 4. Nothing matched: adopt it.
  IF v_dev IS NULL THEN
    v_uid := 'df-' || encode(gen_random_bytes(16), 'hex');
    INSERT INTO farm.devices (
      farm_uid, adb_serial, hw_fingerprint, pool_id, host_id, current_slot_id,
      manufacturer, model, product, device_codename,
      android_release, sdk_int, build_fingerprint)
    VALUES (
      v_uid, nullif(p_serial, ''), p_hw_fp, p_pool_id, p_host_id, v_slot,
      p_props->>'manufacturer', p_props->>'model', p_props->>'product',
      p_props->>'device_codename', p_props->>'android_release',
      nullif(p_props->>'sdk_int', '')::int, p_props->>'build_fingerprint')
    RETURNING id INTO v_dev;

    INSERT INTO farm.device_runtime (device_id, host_id, slot_id)
    VALUES (v_dev, p_host_id, v_slot)
    ON CONFLICT (device_id) DO NOTHING;

    -- Ambiguity is only visible AFTER the insert. When the second clone of
    -- a duplicated serial arrives, the count at the moment of its own
    -- lookup is still one, so neither device would ever be flagged.
    IF p_serial IS NOT NULL AND p_serial <> '' THEN
      SELECT count(*) INTO v_cnt FROM farm.devices d WHERE d.adb_serial = p_serial;
      IF v_cnt > 1 THEN
        v_amb := true;
        UPDATE farm.devices d SET serial_ambiguous = true
         WHERE d.adb_serial = p_serial AND NOT d.serial_ambiguous;
      END IF;
    END IF;

    -- The row exists either way; the only question is what the caller is
    -- told about the evidence it was created on.
    IF v_amb THEN
      v_res := 'ambiguous';
    ELSE
      v_res := 'adopted_new';
    END IF;
  ELSE
    -- Known device. Refresh what we observed and follow it if it moved.
    UPDATE farm.devices d
       SET host_id         = p_host_id,
           current_slot_id = v_slot,
           adb_serial      = COALESCE(nullif(p_serial, ''), d.adb_serial),
           hw_fingerprint  = COALESCE(p_hw_fp, d.hw_fingerprint),
           manufacturer    = COALESCE(p_props->>'manufacturer', d.manufacturer),
           model           = COALESCE(p_props->>'model', d.model),
           android_release = COALESCE(p_props->>'android_release', d.android_release),
           sdk_int         = COALESCE(nullif(p_props->>'sdk_int', '')::int, d.sdk_int),
           build_fingerprint = COALESCE(p_props->>'build_fingerprint', d.build_fingerprint),
           updated_at      = now()
     WHERE d.id = v_dev;
  END IF;

  -- Occupancy: close any stale tenancy, then record the current one.
  UPDATE farm.slot_occupancy o SET until = now(), reason = 'device moved'
   WHERE o.until IS NULL AND o.device_id = v_dev AND o.slot_id <> v_slot;
  UPDATE farm.slot_occupancy o SET until = now(), reason = 'slot reoccupied'
   WHERE o.until IS NULL AND o.slot_id = v_slot AND o.device_id <> v_dev;
  INSERT INTO farm.slot_occupancy (slot_id, device_id)
  SELECT v_slot, v_dev
   WHERE NOT EXISTS (SELECT 1 FROM farm.slot_occupancy o
                      WHERE o.until IS NULL AND o.slot_id = v_slot AND o.device_id = v_dev);

  device_id := v_dev; resolution := v_res; slot_id := v_slot;
  RETURN NEXT;
END $fn$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The previous definitions live in 00004 and 00005; rolling back would mean
-- re-running one of those files' function bodies, which goose cannot do for a
-- CREATE OR REPLACE. Down is a deliberate no-op: this is a bug fix, and
-- reverting it restores a function that tells its caller a contested identity
-- was a clean adoption.
SELECT 1;
-- +goose StatementEnd
