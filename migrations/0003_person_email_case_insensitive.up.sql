-- PRSR-16: make person.email unique case-insensitively.
--
-- The original index was `ON person (email)` — case-SENSITIVE — while every
-- lookup in the store matches `lower(email) = lower($1)`. Those two disagree,
-- and the gap between them mints duplicate identities: a row inserted by hand
-- as 'Einlanzerous@Live.com' does not collide with the lowercased address the
-- code writes, so an upsert inserts a *second* person for the same human. The
-- audit then walks both and dutifully populates each one.
--
-- Email is the SSO join key (Cloudflare Access email OTP, Switchyard
-- users.email), and neither treats it as case-sensitive. Two rows differing
-- only in case are always the same person, so the index should say so.
--
-- Pre-flight: this index cannot be built if such duplicates already exist.
-- Check before deploying, and merge any hits by hand:
--
--   SELECT lower(email), count(*), array_agg(id)
--   FROM person WHERE email IS NOT NULL
--   GROUP BY lower(email) HAVING count(*) > 1;

DROP INDEX IF EXISTS person_email_key;

CREATE UNIQUE INDEX IF NOT EXISTS person_email_key
    ON person (lower(email)) WHERE email IS NOT NULL;
