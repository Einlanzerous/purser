# CLAUDE.md — Purser

Cross-service provisioning/invite service for the Construct. One command invites
a person into multiple services, mints credentials, grants Cloudflare Access
SSO, and returns a copy-pasteable credential block (or emails it). Single static
Go binary (CLI + thin HTTP API), sibling to the other construct-server Go
services. See `docs/architecture.md` for the full design (IDEA-14 reference).

## Layout

- `cmd/purser/` — entrypoint + subcommands (`serve`, `invite`, `migrate`,
  `version`). Composition root: `setup()` wires store + connectors + orchestrator.
- `internal/model/` — domain types (person, service, account, invite,
  provision_task), 1:1 with the schema.
- `internal/connector/` — the `Connector` interface + `Registry` +
  `Unavailable` (registered-but-unconfigured) + `ErrPending`.
- `internal/connectors/{switchyard,cloudflare,argosy}/` — per-service connectors.
- `internal/invite/` — the orchestrator (`Run`) + credential-block renderer. This
  is where idempotency lives.
- `internal/store/` — pgx pool, embedded migrator, repo queries.
- `internal/delivery/` — SMTP sender (email delivery).
- `internal/api/` — thin HTTP surface.
- `migrations/` — `NNNN_name.up.sql` / `.down.sql`, embedded, auto-applied on boot.

## Conventions (match the construct-server house style)

- Go 1.26, `pgx/v5`, `google/uuid`. No ORM. No external migration tool — the
  in-process migrator in `internal/store/migrate.go` applies embedded SQL.
- Config is env-only, `PURSER_`-prefixed, with a `DATABASE_URL` fallback
  (`internal/config`). No config files.
- Logs: stdlib `log` to stdout. Health: `GET /healthz`. Port 4006.
- Release-please + GHCR image `ghcr.io/einlanzerous/purser`. Conventional
  commits.

## Invariants — don't break these

- **Idempotency is per (person × service).** `account` has `UNIQUE(person_id,
  service_id)`; the orchestrator skips services with an active account and
  retries only failed ones. Keep it that way.
- **Never persist a secret in plaintext.** `account.secret_hash` is sha256;
  plaintext lives only in the returned/emailed credential block.
- **Switchyard needs the email set** on user create — it's the SSO join key
  (`users.email`). Don't drop it.
- Connectors should treat "already exists" upstream as success (reconcile) so a
  failed-only retry is safe.
- Per-service failures must not abort the whole invite.

## Testing

- `make test` — unit tests (fake store + fake connectors for the orchestrator;
  httptest for the Switchyard/Cloudflare connectors; in-process SMTP for
  delivery). DB-backed store tests skip unless `PURSER_TEST_DATABASE_URL` is set.
- `make test-db` — spins a throwaway Postgres 16 and runs everything.
- Never point tests (or `purser migrate`) at the live shared `postgres` — use a
  throwaway container / the `_test` DB.

## Status

Phase 0+1 (schema + Switchyard connector) plus the owner-requested Cloudflare
Access connector and email/copy-paste delivery. Argosy connector is a
placeholder pending an upstream admin endpoint (ARGY ticket). Lyceum is future.
