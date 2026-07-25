-- SERV-54: allow account.status = 'stale'.
--
-- Stale means Purser holds a record but a Reconcile found nothing upstream —
-- drift Purser didn't cause. It is deliberately distinct from 'deprovisioned',
-- which is Purser removing access on purpose.
--
-- This matters because the orchestrator's idempotency skip keys on 'active': a
-- row stuck active with no upstream account can never be re-provisioned by any
-- invite. Marking it stale re-arms provisioning for that person × service.

ALTER TABLE account DROP CONSTRAINT IF EXISTS account_status_check;

ALTER TABLE account
    ADD CONSTRAINT account_status_check
    CHECK (status IN ('active', 'deprovisioned', 'stale'));
