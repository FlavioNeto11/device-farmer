-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- THE VOCABULARY DESCRIBES ITSELF HONESTLY AGAIN
--
-- farm.step_kinds is the closed step vocabulary, and its description
-- column is not decoration. GET /api/v1/specs/kinds serves it verbatim,
-- `ctl kinds` prints it, and the Docs tab renders it FROM THE DATABASE
-- rather than from prose precisely so the page cannot drift from what
-- the server will accept. internal/api/router.go leaves that endpoint
-- unprivileged for the same reason: the project wants clients that
-- hard-code nothing and ask.
--
-- So a wrong row here is not a stale comment. It is the control plane
-- answering a question about itself, on request, with something false —
-- and the clients most damaged by it are exactly the ones that did the
-- right thing and asked.
--
-- Two rows had drifted, both in the same round of runner work.
--
-- WAIT_FOR said "Poll a shell probe until it succeeds or the timeout
-- elapses." It has two forms now. jobspec.WaitFor also takes a handle
-- naming a shell_detached step declared earlier in the same spec, and
-- internal/runner's execWaitForDetached then polls THAT run's published
-- status and judges its exit code, instead of waiting for a file to
-- appear. The forms differ in the one thing an author is choosing
-- between — what the step is allowed to conclude when the wait ends —
-- so naming only one of them sends the author to the form that cannot
-- say what they meant. Worse, the probe-only reading is how the bug the
-- handle form exists to fix gets written by hand: the obvious probe for
-- a detached run is `test -f …/soak.result`, and that becomes true the
-- instant the wrapper publishes a status, 137 as eagerly as 0. A soak
-- that started cleanly and was killed four hours later produced a green
-- wait_for and a green job, with the failure sitting in a file on the
-- phone that nothing ever read.
--
-- SHELL_DETACHED described the launch and nothing else. Its exit status
-- is now read and judged twice over: once immediately after the launch,
-- so a command that dies on its first line fails there rather than
-- leaving a wait_for to burn a timeout written for a six-hour soak, and
-- again when a resume re-attaches by handle, so a run that finished
-- while the job was away is judged rather than merely noticed. A client
-- reading the old row would expect a step that cannot fail on the
-- command's own verdict. It can, and that is the point of it.
--
-- THE SUCCESS SET IS NAMED WHERE IT ACTUALLY LIVES
--
-- Both new rows say default_expect_exit, the SPEC-level key, and not
-- expect_exit. That distinction is the whole reason to name it at all.
-- expect_exit is a field of the shell payload and of nothing else;
-- jobspec decodes payloads with DisallowUnknownFields, so an author who
-- believed this row and wrote expect_exit into a wait_for or a
-- shell_detached step would have the entire spec refused at parse time.
-- runner.detachedExpectExit reads spec.ExpectExit(jobspec.Shell{}) —
-- an EMPTY shell, deliberately — which resolves to the spec's
-- default_expect_exit, or {0} when it is absent. A detached run has no
-- success set of its own, and saying so is half of what these rows are
-- for.
--
-- WHAT IS NOT CHANGED
--
-- The flags. idempotent and needs_artifact are behaviour, not prose:
-- they decide whether a resume may re-run a step, they are mirrored in
-- internal/jobspec's kindTable, and TestKindTableMatchesMigration pins
-- that mirror against the INSERT in 00004. Nothing here touches them,
-- which is the whole reason the description was put in a column that
-- can be corrected without a release while the flags were not.
--
-- The other eight rows were re-read against their executors in
-- internal/runner/steps.go and deliberately left alone. Several are
-- thin — 'Copy a file off the device.' does not mention that the bytes
-- land in the content-addressed store under a name the spec chose — but
-- thin is not false, and a table rewritten for style is a table whose
-- next reader cannot tell which edits were corrections.
--
-- WHY THESE TWO SENTENCES ARE IN THIS ORDER
--
-- `ctl kinds` prints these in a table cell and CLIPS at 96 characters
-- with an ellipsis; widening that column past 200 would trade a clip
-- for a wrap, which is the grid the clip exists to protect. Both rows
-- are therefore written so the clipped form still carries the thing
-- that was previously false — wait_for names both of its forms inside
-- 96 characters, shell_detached says the status is judged inside 96 —
-- and the remainder reaches `ctl kinds -o json`, the Docs tab and the
-- endpoint itself in full.
--
-- The UPDATEs are counted. A typo in a WHERE clause updates no rows and
-- succeeds, and a migration that silently corrected nothing would leave
-- the farm publishing the old answer with a version number that claims
-- otherwise.
-- =====================================================================

DO $$
DECLARE
  n int;
BEGIN
  UPDATE farm.step_kinds
     SET description = 'Poll a shell probe until it exits zero, or name the handle of an '
                    || 'earlier shell_detached step and judge the status it publishes '
                    || 'against default_expect_exit.'
   WHERE kind = 'wait_for';
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'expected exactly one wait_for row in farm.step_kinds, updated %', n;
  END IF;

  UPDATE farm.step_kinds
     SET description = 'Start a long-running command and return at once, judging any status '
                    || 'it has already published; the device, not a socket, owns the '
                    || 'result, and a resume judges it again.'
   WHERE kind = 'shell_detached';
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'expected exactly one shell_detached row in farm.step_kinds, updated %', n;
  END IF;
END $$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Back to the text 00004 seeded. A down migration that left the
-- corrected prose in place would make "roll back to v20" mean two
-- different schemas depending on which way the farm arrived at it, and
-- this column is read by clients — it is not a comment nobody queries.
--
-- Counted for the reason the Up gives, and for one more: a rollback is
-- run by somebody who already has a problem, and a restore that
-- silently restored nothing would leave them reading a description
-- from a schema version that no longer exists.

DO $$
DECLARE
  n int;
BEGIN
  UPDATE farm.step_kinds
     SET description = 'Poll a shell probe until it succeeds or the timeout elapses.'
   WHERE kind = 'wait_for';
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'expected exactly one wait_for row in farm.step_kinds, restored %', n;
  END IF;

  UPDATE farm.step_kinds
     SET description = 'Start a long-running command under nohup setsid; the '
                    || 'device, not a socket, owns the result.'
   WHERE kind = 'shell_detached';
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'expected exactly one shell_detached row in farm.step_kinds, restored %', n;
  END IF;
END $$;

-- +goose StatementEnd
