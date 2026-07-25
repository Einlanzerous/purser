-- Revert SERV-54: drop 'stale' from the allowed account statuses.
--
-- Any stale rows must be resolved first or the constraint won't validate.
-- They're demoted to 'deprovisioned' rather than 'active': a stale row means
-- nothing was found upstream, so calling it active would re-assert access the
-- person demonstrably doesn't have, and would restore the exact skip-forever
-- bug this status exists to fix.

UPDATE account SET status = 'deprovisioned' WHERE status = 'stale';

ALTER TABLE account DROP CONSTRAINT IF EXISTS account_status_check;

ALTER TABLE account
    ADD CONSTRAINT account_status_check
    CHECK (status IN ('active', 'deprovisioned'));
