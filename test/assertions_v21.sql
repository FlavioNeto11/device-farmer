-- Assertions for migration 00021_wait_for_handle.sql: the step vocabulary
-- describes the steps this build actually runs.
--
-- farm.step_kinds.description is not a comment. GET /api/v1/specs/kinds serves
-- it verbatim to any authenticated caller, `ctl kinds` prints it, and the Docs
-- tab renders it from the database rather than from prose so the page cannot
-- drift from what the server accepts. The project's stated position is that a
-- client should hard-code nothing and ask — which makes a wrong row here a
-- control plane answering a question about itself with something false, to
-- exactly the clients that did the right thing.
--
-- 00004 described wait_for as a shell probe and nothing else. It has two forms
-- now: jobspec.WaitFor also takes a handle naming a shell_detached step earlier
-- in the same spec, and internal/runner's execWaitForDetached judges THAT run's
-- published exit code rather than waiting for a file to appear. The distinction
-- is not cosmetic — the probe an author reaches for instead, `test -f
-- …/soak.result`, goes true the moment the wrapper publishes a status, 137 as
-- eagerly as 0, which is a killed four-hour soak reported as a green job.
--
-- This file exists so that the next person who changes what a kind DOES and
-- forgets the row it is described by fails a suite, rather than shipping the
-- lie to every client that asks.
--
-- It asserts on meaning, not on wording: each check names a term the
-- description cannot be true without, so a rewrite that keeps the facts passes
-- and one that drops a form does not.
--
-- Run:  psql -v ON_ERROR_STOP=1 -f test/assertions_v21.sql
-- Every assertion raises an exception on failure, so a clean run is the proof.

\set ON_ERROR_STOP on
BEGIN;

DO $$
DECLARE
  v_wait     text;
  v_detached text;
  v_cnt      int;
  v_kinds    text[];
  r          record;

  -- The MaxCell that cmdSpecKinds in internal/ctl/commands_spec.go gives this
  -- table. A6b reads it; see the reasoning there.
  c_clip     int := 96;
BEGIN
  SELECT description INTO v_wait     FROM farm.step_kinds WHERE kind = 'wait_for';
  SELECT description INTO v_detached FROM farm.step_kinds WHERE kind = 'shell_detached';
  IF v_wait IS NULL OR v_detached IS NULL THEN
    RAISE EXCEPTION 'A0 FAILED: farm.step_kinds is missing wait_for or shell_detached, so nothing below is a test';
  END IF;

  -- ============================================================
  -- A1  wait_for names BOTH forms. This is the assertion the file was
  --     written for: the row said "probe" and only "probe" while the
  --     runner had grown a second form with a different verdict, and a
  --     client that asks the server was told the wrong thing.
  -- ============================================================
  IF v_wait !~* 'probe' THEN
    RAISE EXCEPTION 'A1 FAILED: wait_for no longer names its probe form: %', v_wait;
  END IF;
  IF v_wait !~* 'handle' THEN
    RAISE EXCEPTION 'A1 FAILED: wait_for does not mention the handle form, so the server publishes a description of a step it does not have: %', v_wait;
  END IF;
  RAISE NOTICE 'A1  ok  wait_for names both the probe form and the handle form';

  -- ============================================================
  -- A2  And says which step a handle belongs to. "handle" alone leaves
  --     an author guessing where one comes from; the answer is a
  --     shell_detached step declared earlier in the same spec, and
  --     jobspec.Validate refuses every other reading.
  -- ============================================================
  IF v_wait !~* 'shell_detached' THEN
    RAISE EXCEPTION 'A2 FAILED: wait_for names a handle without naming shell_detached, the only step that declares one: %', v_wait;
  END IF;
  RAISE NOTICE 'A2  ok  wait_for says a handle names a shell_detached step';

  -- ============================================================
  -- A3  And says what the handle form JUDGES, against the key that
  --     really decides it. This is the whole difference between the two
  --     forms: the probe form concludes something about its own probe,
  --     the handle form concludes something about the detached command.
  --     The success set it is measured by is the SPEC-level
  --     default_expect_exit — runner.detachedExpectExit resolves
  --     spec.ExpectExit(jobspec.Shell{}) with an empty shell, so a
  --     detached run has no success set of its own.
  --
  --     The bare key expect_exit belongs to the shell payload and to
  --     nothing else, and jobspec decodes with DisallowUnknownFields:
  --     an author who read it here and wrote it into a wait_for step
  --     would have the whole spec refused at parse time. A description
  --     that named it would be a new lie in the row written to end one,
  --     so it is refused here in both rows.
  -- ============================================================
  IF v_wait !~* 'judge' THEN
    RAISE EXCEPTION 'A3 FAILED: wait_for does not say the handle form judges anything: %', v_wait;
  END IF;
  IF v_wait !~* 'default_expect_exit' THEN
    RAISE EXCEPTION 'A3 FAILED: wait_for does not say what the published status is judged against: %', v_wait;
  END IF;
  IF v_wait ~* '(^|[^_[:alnum:]])expect_exit' OR v_detached ~* '(^|[^_[:alnum:]])expect_exit' THEN
    RAISE EXCEPTION 'A3 FAILED: a row offers expect_exit, which only the shell payload has; a spec that took the offer is refused at parse time';
  END IF;
  RAISE NOTICE 'A3  ok  wait_for judges the published status against default_expect_exit';

  -- ============================================================
  -- A4  The probe-only sentence is gone. Asserting on the terms above
  --     alone would pass a row that had somehow acquired both texts;
  --     this pins that the false one is not still being served.
  -- ============================================================
  IF v_wait = 'Poll a shell probe until it succeeds or the timeout elapses.' THEN
    RAISE EXCEPTION 'A4 FAILED: wait_for still carries the probe-only description 00004 seeded';
  END IF;
  RAISE NOTICE 'A4  ok  the probe-only description is no longer served';

  -- ============================================================
  -- A5  shell_detached says its exit status is judged. The launch used
  --     to be the whole story; it is not. execShellDetached probes once
  --     immediately after starting, so a command that dies on its first
  --     line fails there instead of leaving a wait_for to burn a
  --     six-hour timeout, and reattachDetached judges a run that
  --     finished while the job was away. A client reading the old row
  --     would expect a step that cannot fail on the command's verdict.
  -- ============================================================
  IF v_detached !~* 'status' THEN
    RAISE EXCEPTION 'A5 FAILED: shell_detached does not mention the status its command publishes: %', v_detached;
  END IF;
  IF v_detached !~* 'judge' THEN
    RAISE EXCEPTION 'A5 FAILED: shell_detached does not say that status is judged, so the row describes a step that cannot fail on its command''s own verdict: %', v_detached;
  END IF;
  IF v_detached = 'Start a long-running command under nohup setsid; the device, not a socket, owns the result.' THEN
    RAISE EXCEPTION 'A5 FAILED: shell_detached still carries the launch-only description 00004 seeded';
  END IF;
  RAISE NOTICE 'A5  ok  shell_detached says the status it publishes is judged';

  -- ============================================================
  -- A6  Every kind carries a real description. The column is NOT NULL,
  --     which stops nothing: '' and 'TODO' satisfy it and are what an
  --     endpoint would then publish as the meaning of a step.
  -- ============================================================
  FOR r IN SELECT kind, description FROM farm.step_kinds ORDER BY kind LOOP
    IF btrim(r.description) = '' THEN
      RAISE EXCEPTION 'A6 FAILED: step kind % has an empty description, and the API serves it as the meaning of the step', r.kind;
    END IF;
    IF r.description ~* '(TODO|TBD|FIXME|XXX)' THEN
      RAISE EXCEPTION 'A6 FAILED: step kind % is described by a placeholder: %', r.kind, r.description;
    END IF;
  END LOOP;
  RAISE NOTICE 'A6  ok  every kind carries a non-placeholder description';

  -- ============================================================
  -- A6b The CLIPPED form still carries the correction.
  --
  --     `ctl kinds` prints these in a table cell and clips at c_clip
  --     with an ellipsis, so the first c_clip characters are the whole
  --     answer for one of the three clients that read this column.
  --     Widening that column past 200 would trade the clip for a wrap
  --     and destroy the grid the clip exists to protect, so the
  --     discipline is on the prose instead: say the thing that was
  --     false first. A row that pushed the handle form past the clip
  --     would be truthful in the database and, at the CLI, exactly as
  --     misleading as the row 00021 replaced.
  --
  --     Conservative on purpose: if the CLI ever widens its column this
  --     assertion merely asks for more than it must.
  -- ============================================================
  IF left(v_wait, c_clip) !~* 'probe' OR left(v_wait, c_clip) !~* 'handle' THEN
    RAISE EXCEPTION 'A6b FAILED: wait_for loses one of its two forms in the first % characters, which is all ctl kinds prints: %', c_clip, left(v_wait, c_clip);
  END IF;
  IF left(v_detached, c_clip) !~* 'judg' THEN
    RAISE EXCEPTION 'A6b FAILED: shell_detached loses the judgement in the first % characters, which is all ctl kinds prints: %', c_clip, left(v_detached, c_clip);
  END IF;
  RAISE NOTICE 'A6b ok  both corrections survive the clip ctl kinds applies';

  -- ============================================================
  -- A7  The vocabulary is still exactly the ten kinds this file has
  --     checked. It is closed — internal/jobspec mirrors it and
  --     TestKindTableMatchesMigration pins the mirror — so an eleventh
  --     row is a deliberate act, and this is where its author is told
  --     that a new kind arrives with a description or not at all.
  -- ============================================================
  SELECT count(*), array_agg(kind ORDER BY kind) INTO v_cnt, v_kinds FROM farm.step_kinds;
  IF v_cnt <> 10 OR v_kinds <> ARRAY[
       'assert','install','pull','push','reset','shell','shell_detached','sleep','uninstall','wait_for'
     ]::text[] THEN
    RAISE EXCEPTION 'A7 FAILED: the step vocabulary is now %, which this suite has not read against the code that implements it; describe the new kind and extend these assertions', v_kinds;
  END IF;
  RAISE NOTICE 'A7  ok  the vocabulary is the ten kinds these assertions cover';

  -- ============================================================
  -- A8  The behavioural flags were NOT moved. 00021 corrects prose, and
  --     prose lives in the database exactly because it can be corrected
  --     without a release — idempotent and needs_artifact cannot, since
  --     they decide whether a resume may re-run a step. A migration
  --     that edited a description and a flag together would be a
  --     resume repeating a side effect, shipped as a documentation fix.
  -- ============================================================
  SELECT count(*) INTO v_cnt FROM farm.step_kinds
   WHERE (kind = 'wait_for'       AND (idempotent, needs_artifact) = (true,  false))
      OR (kind = 'shell_detached' AND (idempotent, needs_artifact) = (false, false));
  IF v_cnt <> 2 THEN
    RAISE EXCEPTION 'A8 FAILED: the flags on wait_for or shell_detached moved; internal/jobspec still mirrors the old ones';
  END IF;
  RAISE NOTICE 'A8  ok  wait_for and shell_detached keep the flags 00004 gave them';

  RAISE NOTICE '--------------------------------------------------';
  RAISE NOTICE 'ALL v21 ASSERTIONS PASSED';
END $$;

ROLLBACK;
