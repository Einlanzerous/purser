-- Revert PRSR-21: drop 'unavailable' from the allowed task statuses.
--
-- Folding these back into 'failed' is where they lived before the split, and it
-- is lossless in the direction that matters: last_error still carries the
-- ErrPending text, so re-applying the up migration reclassifies them.
--
-- The schema_migrations row has to go with them. The migrator only applies up
-- migrations and skips any version already recorded there (internal/store/
-- migrate.go), so a down script that leaves its row behind is a one-way door:
-- the next boot reports 0004 as applied, never restores the constraint, and the
-- running binary then writes an 'unavailable' the schema rejects on every
-- unconfigured connector. Deleting the row is what makes this reversible rather
-- than merely destructive.

UPDATE provision_task SET status = 'failed' WHERE status = 'unavailable';

ALTER TABLE provision_task DROP CONSTRAINT IF EXISTS provision_task_status_check;

ALTER TABLE provision_task
    ADD CONSTRAINT provision_task_status_check
    CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'skipped'));

DELETE FROM schema_migrations WHERE version = '0004';
