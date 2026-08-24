-- The service spin-up axis's record of what it created at the edge (PRSR-27).
--
-- Everything before this migration is person-shaped: person / account / invite /
-- provision_task, keyed UNIQUE(person_id, service_id). This table belongs to the
-- *other* axis — provisioning the infrastructure for a service to exist — and is
-- keyed on hostname. The two are deliberately not joined anywhere.
--
-- service_key is the spun-up service's name and is **not** a foreign key to
-- service(key), which is a different thing entirely: that table lists the target
-- systems Purser can invite people *into*, seeded from the connector registry on
-- boot. The services this axis stands up (interlock, cook_book, centrifuge) have
-- no connector and must not need one — requiring a row over there would make
-- "can Purser invite someone to it" a precondition for "can Purser deploy it".
CREATE TABLE IF NOT EXISTS service_resource (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_key TEXT NOT NULL,
    hostname    TEXT NOT NULL,
    kind        TEXT NOT NULL
                CHECK (kind IN ('tunnel_route', 'access_app', 'dns_record')),
    -- The id of the object itself, when it has one. Empty is legitimate for a
    -- tunnel route: the ingress configuration is one document per tunnel and its
    -- rules carry no ids, so a route is identified by (parent_id, hostname).
    -- Read an empty external_id as "this kind has none", not "we lost it".
    external_id TEXT NOT NULL DEFAULT '',
    -- The container the resource was written into: the zone for a DNS record,
    -- the account for an Access app, the *tunnel* for an ingress route.
    --
    -- Recorded rather than re-read from config because the tunnel is a
    -- per-spec choice, not a global (PRSR-33): the account already has two, and a
    -- teardown that resolved the tunnel from today's config would edit the wrong
    -- shared document if the spec's default ever moved — silently unrouting
    -- whatever else lives in it.
    parent_id   TEXT NOT NULL DEFAULT '',
    -- Only two states today. A resource-level 'stale' (upstream lost it) would
    -- mirror account.status, but nothing writes one yet: unlike the person axis,
    -- the spin-up path decides what to do by *reading upstream*, not by reading
    -- this row, so there is no provisioning to re-arm by marking it. Adding it
    -- later is a migration, which is the point of the CHECK.
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'removed')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- When it was torn down. Stamped on the transition into 'removed' and never
    -- cleared, so a hostname that is stood back up keeps the history of having
    -- been taken down — account.deprovisioned_at (migration 0006) exists for
    -- exactly this reason, and learned it by losing the date to a re-invite.
    removed_at  TIMESTAMPTZ
);

-- One resource per (hostname, kind) — the idempotency key of this axis, the way
-- (person_id, service_id) is the person axis's.
--
-- Case-insensitive because hostnames are: DNS does not distinguish
-- Argosy.zerogravity.industries from argosy.zerogravity.industries, so an index
-- that did would let the same hostname be recorded twice and torn down once.
-- person_email_key had precisely this bug (migration 0003) and it inserted
-- duplicate identities; the conflict target in the store must infer on
-- lower(hostname) to match.
--
-- Deliberately not filtered on status: a removed row keeps the slot, so standing
-- the hostname back up reuses it and flips the status. That mirrors `account`,
-- which is marked and never deleted, and for the same reason — deleting the row
-- would destroy the record of what once existed here.
CREATE UNIQUE INDEX IF NOT EXISTS service_resource_hostname_kind_key
    ON service_resource (lower(hostname), kind);

-- "What does this service hold at the edge?" — the teardown and report query.
CREATE INDEX IF NOT EXISTS service_resource_service_idx
    ON service_resource (service_key);
