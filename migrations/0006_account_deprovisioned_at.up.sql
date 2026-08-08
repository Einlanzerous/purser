-- Record *when* access was taken away, durably (PRSR-17).
--
-- account.status plus updated_at looked like enough, and isn't: UpsertAccount
-- sets status='active' and bumps updated_at, so the very next invite for that
-- person and service erases both. "What did they hold, and when was it taken
-- away?" — the question the deprovisioned row exists to answer — therefore did
-- not survive a re-invite, which is an ordinary thing to do.
--
-- This column is written only by a successful revoke and is never cleared: a
-- re-provisioned account keeps the date it was last revoked, because that is
-- history and history doesn't stop having happened. A NULL means "never
-- deprovisioned", not "not deprovisioned right now" — read status for the
-- latter.
ALTER TABLE account ADD COLUMN IF NOT EXISTS deprovisioned_at TIMESTAMPTZ;

-- Backfill what can be known: rows already sitting at 'deprovisioned' were put
-- there before this column existed, and updated_at is the best available
-- evidence of when. Scoped to that status so an active row is never stamped.
UPDATE account
   SET deprovisioned_at = updated_at
 WHERE status = 'deprovisioned' AND deprovisioned_at IS NULL;
