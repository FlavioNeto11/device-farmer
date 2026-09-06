-- +goose Up
-- +goose StatementBegin

-- =====================================================================
-- One index, so that checking whether a finished job's state agrees with
-- its own step rows stays cheap forever.
--
-- internal/janitor gained a pass (sweepUnevidencedSuccess) over jobs that
-- are reported 'succeeded' while farm.job_steps says the work did not all
-- happen — JOB-10, the case where the API tells you a job succeeded when
-- its APK never landed. Every other pass in that loop is anchored to a row
-- that is still OPEN, and each of those has a partial index already:
-- jobs_live, job_steps_live, jobs_ready. Succeeded jobs are the opposite
-- shape. They accumulate for the life of the farm and nothing ever takes
-- them out of scope again, so the same pass written without an index is a
-- sequential scan of the whole job history every thirty seconds — slower
-- every week, and eventually slower than the janitor's own call timeout.
-- At that point the sweep does not degrade, it stops: the scan errors, the
-- error is counted, and the backstop silently closes nothing on exactly
-- the busy farm that produced the contradiction.
--
-- So the pass carries an upper bound as well as a lower one — it looks at
-- recently finished jobs only — and this index is what turns that bound
-- into a range scan.
--
-- The expression is COALESCE(finished_at, started_at, created_at) and not
-- plain finished_at, because finished_at is legitimately NULL for a while:
-- internal/runner writes the terminal state without a finish time and
-- internal/jobrunner's stampFinished fills it in afterwards. An index on
-- the bare column would silently exclude every job in that window, and
-- exclude forever any job whose supervisor never got to stamp it. It must
-- stay spelled exactly as internal/janitor's successClock constant spells
-- it: if the two drift, the planner stops using this index and says
-- nothing about it.
--
-- Partial on state = 'succeeded'. That is the only state the pass reads,
-- and it keeps the index to the finished-and-successful slice of the table
-- rather than a copy of every job that ever ran.
-- =====================================================================

CREATE INDEX IF NOT EXISTS jobs_recent_success
  ON farm.jobs ((COALESCE(finished_at, started_at, created_at)))
  WHERE state = 'succeeded';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS farm.jobs_recent_success;
-- +goose StatementEnd
