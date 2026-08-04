-- Revert PRSR-23: allow a person row with no email again.
--
-- Dropping the constraint is lossless — it never rewrote a row, only refused
-- new ones — so nothing needs restoring alongside it.
--
-- The schema_migrations row has to go with it. The migrator only applies up
-- migrations and skips any version already recorded there (internal/store/
-- migrate.go), so a down script that leaves its row behind is a one-way door:
-- the next boot reports 0005 as applied and never re-adds the constraint.

ALTER TABLE person DROP CONSTRAINT IF EXISTS person_email_required;

DELETE FROM schema_migrations WHERE version = '0005';
