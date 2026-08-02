-- PRSR-21: allow provision_task.status = 'unavailable'.
--
-- A connector that is registered but can't provision — no token configured, or
-- an upstream with no provisioning API yet — returns connector.ErrPending. That
-- was recorded as 'failed' with a bool flag alongside it, so everything that
-- buckets by status read "not wired up yet" as "broke". It gets its own status
-- now. See internal/model/model.go for why it isn't called 'pending'.

ALTER TABLE provision_task DROP CONSTRAINT IF EXISTS provision_task_status_check;

ALTER TABLE provision_task
    ADD CONSTRAINT provision_task_status_check
    CHECK (status IN ('pending', 'running', 'unavailable', 'succeeded', 'failed', 'skipped'));

-- Reclassify the history. Without this, every task that was ever unavailable
-- stays on record as a failure, and the audit-by-status this split exists to
-- enable would read the past wrong forever.
--
-- The match is on connector.ErrPending's message, which is a package-level
-- sentinel wrapped with %w at the front of every error that carries it — so a
-- last_error starting with this text came from ErrPending and nothing else.
-- Rows that don't match keep 'failed': a task we can't positively identify as
-- unavailable is left where it is rather than guessed at.
UPDATE provision_task
   SET status = 'unavailable'
 WHERE status = 'failed'
   AND last_error LIKE 'connector: provisioning not yet available%';
