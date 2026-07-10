-- Purser initial schema (IDEA-14 / Phase 0+1).
--
-- One action invites a person into one or more services; each service is
-- hidden behind a connector. These tables are the spine: who we invited
-- (person), where (service), the durable access record (account), the run that
-- created it (invite), and the per-service unit of work (provision_task).

-- People we provision access for. Email is the SSO join key (Cloudflare Access
-- email OTP + Switchyard SSO), so it is unique when present.
CREATE TABLE IF NOT EXISTS person (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    email      TEXT,
    type       TEXT NOT NULL DEFAULT 'human' CHECK (type IN ('human', 'agent')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Unique email among people that have one (partial index skips NULLs).
CREATE UNIQUE INDEX IF NOT EXISTS person_email_key
    ON person (email) WHERE email IS NOT NULL;

-- Target systems Purser can provision into. Seeded from the connector registry
-- on boot; key always matches a registered connector.
CREATE TABLE IF NOT EXISTS service (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key          TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The durable "person P has access to service S" record. The (person_id,
-- service_id) pair is unique — it is the idempotency key for provisioning.
-- Secrets are never stored plaintext: secret_hash is sha256 of the delivered
-- one-time credential; secret_ref is reserved for a future vault reference.
CREATE TABLE IF NOT EXISTS account (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id   UUID NOT NULL REFERENCES person (id) ON DELETE CASCADE,
    service_id  UUID NOT NULL REFERENCES service (id) ON DELETE CASCADE,
    external_id TEXT NOT NULL DEFAULT '',
    username    TEXT NOT NULL DEFAULT '',
    secret_hash TEXT NOT NULL DEFAULT '',
    secret_ref  TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'deprovisioned')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (person_id, service_id)
);

-- A provisioning run for a person across one or more services.
CREATE TABLE IF NOT EXISTS invite (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id    UUID NOT NULL REFERENCES person (id) ON DELETE CASCADE,
    delivery     TEXT NOT NULL DEFAULT 'copypaste'
                 CHECK (delivery IN ('copypaste', 'email')),
    role         TEXT NOT NULL DEFAULT '',
    delivered_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One service's slice of an invite. Tracks attempts + last_error so a re-run
-- retries only what failed. account_id is set once the task succeeds.
CREATE TABLE IF NOT EXISTS provision_task (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    invite_id  UUID NOT NULL REFERENCES invite (id) ON DELETE CASCADE,
    person_id  UUID NOT NULL REFERENCES person (id) ON DELETE CASCADE,
    service_id UUID NOT NULL REFERENCES service (id) ON DELETE CASCADE,
    account_id UUID REFERENCES account (id) ON DELETE SET NULL,
    status     TEXT NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'skipped')),
    attempts   INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (invite_id, service_id)
);

CREATE INDEX IF NOT EXISTS provision_task_invite_idx ON provision_task (invite_id);
