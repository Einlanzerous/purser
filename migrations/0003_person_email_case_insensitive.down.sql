-- Revert PRSR-16: return person.email to case-sensitive uniqueness.
--
-- Always succeeds: a case-insensitively unique set is case-sensitively unique
-- too. Note that reverting re-opens the duplicate-identity gap, since the
-- store's lookups still match on lower(email).

DROP INDEX IF EXISTS person_email_key;

CREATE UNIQUE INDEX IF NOT EXISTS person_email_key
    ON person (email) WHERE email IS NOT NULL;
