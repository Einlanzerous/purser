-- PRSR-23: require person.email on every new row.
--
-- The address is a person's identity key: it is what the audit looks them up
-- by, what Switchyard joins SSO on, and the conflict target that makes an
-- invite idempotent. person_email_key is partial on `email IS NOT NULL`, so a
-- row without one collides with nothing — including a previous run of the same
-- invite — and every re-run therefore minted a new person id, which is what
-- UNIQUE(person_id, service_id) and the connectors' Idempotency-Key both key
-- on. `invite` now requires the address, but a guard in Go is only a guard for
-- callers that go through Go; this is the same reasoning as 0003, where a
-- check in AddPerson would have left `invite` minting the duplicates instead.
--
-- NOT VALID is the point, not a shortcut. It enforces the rule on every insert
-- and every update from here on, while leaving rows the old path already wrote
-- exactly where they are:
--
--   * SET NOT NULL would have to rewrite or reject them at boot, inside a
--     migration that takes the whole service down if it guesses wrong about
--     what they should say — and there is nothing to backfill them *with*.
--   * Those rows are stranded either way: no command can address a person who
--     has no address, so repairing one means hand SQL, and this constraint
--     deliberately still permits that (UPDATE person SET email = … satisfies
--     it).
--
-- To adopt them later, once each has been given a real address:
--
--   ALTER TABLE person VALIDATE CONSTRAINT person_email_required;
--
-- which scans the table and fails loudly, out of band, if any remain.

ALTER TABLE person DROP CONSTRAINT IF EXISTS person_email_required;

ALTER TABLE person
    ADD CONSTRAINT person_email_required
    CHECK (email IS NOT NULL) NOT VALID;
