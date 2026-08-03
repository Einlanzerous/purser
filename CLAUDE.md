# CLAUDE.md — Purser

Cross-service provisioning/invite service for the Construct. One command invites
a person into multiple services, mints credentials, grants Cloudflare Access
SSO, and returns a copy-pasteable credential block (or emails it). Single static
Go binary (CLI + thin HTTP API), sibling to the other construct-server Go
services. See `docs/architecture.md` for the full design (IDEA-14 reference).

Tracked in Switchyard under the **PRSR** project (epic `PRSR-1`). It graduated
there from `SERV-33`; the old `SERV-*` keys still resolve as aliases, so treat a
`SERV-` reference in an older commit or comment as historical.

## Layout

- `cmd/purser/` — entrypoint + subcommands (`serve`, `invite`, `person add`,
  `audit`, `reconcile`, `migrate`, `version`). Composition root: `setup()` wires
  store + connectors + orchestrator.
- `internal/model/` — domain types (person, service, account, invite,
  provision_task), 1:1 with the schema.
- `internal/connector/` — the `Connector` interface + `Registry` +
  `Unavailable` (registered-but-unconfigured) + `ErrPending`.
- `internal/connectors/{switchyard,cloudflare,lyceum,argosy}/` — per-service connectors.
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
  retries only failed ones. Keep it that way. Bundles (PRSR-12) rely on this —
  a bundle is only a named service list, so overlapping bundles and re-invites
  are safe without any bundle-specific logic. Don't give bundles their own
  provisioning path.
- **A bundle grants project access, not privilege.** The default `*:user` is a
  Switchyard *project membership* role; the instance role stays at its preset.
  Don't conflate the two axes.
- **`Reconcile` must never mutate.** No create, no mint, no rotate, no revoke —
  it answers "what does this person already have?" and nothing else. A version
  that repairs as a side effect can't be used to audit, because running it
  destroys the drift it's meant to report. A connector with no lookup endpoint
  returns `ErrReconcileUnsupported` rather than inferring absence: reporting
  "no" for a question you can't answer generates wrong records.
- **Never treat unverifiable as absent.** `UpstreamUnknown` is deliberately
  distinct from `UpstreamNo` throughout the audit, and a failed connector call
  must never mark records stale — a transient outage would otherwise wipe out
  everyone's access records.
- **`person add` provisions nothing.** The add writes a `person` row and nothing
  else — no `account` rows, no credentials, no `Provision`. It exists so the
  audit can see people onboarded outside Purser, and it stops being useful the
  moment it starts doing invite's job. (`--audit` then runs the ordinary
  read-only audit, which does call `Reconcile`; that's the preview, not the add.)
- **An occupied email is a conflict, not an edit.** The store used to carry an
  `UpsertPerson` doing `ON CONFLICT … DO UPDATE SET name`, so any command taking
  `--name` renamed whoever holds that address unless it refused first — `person
  add` uses `InsertPersonIfAbsent` and requires `--rename`, and `invite`'s
  `resolvePerson` keeps the stored name and reports the disagreement as
  `Result.NameConflict` (PRSR-20). `UpsertPerson` is now deleted (PRSR-23): a
  `person` row is created only by `InsertPersonIfAbsent` and renamed only by
  `RenamePerson`. **Only `person add --rename` may change a name.** Don't add a
  rename path to `invite`, and don't reintroduce a name-setting upsert — one
  command owning renames is what makes the guarantee checkable by grep.
- **`invite` requires an email, on every delivery method.** The address is the
  identity key and there is no second one. Without it the person row has no
  conflict target — `person_email_key` is partial on `email IS NOT NULL` — so
  each run recorded a *new* person id, and the person id is what idempotency is
  keyed on: `UNIQUE(person_id, service_id)` for the skip, and `InviteRef` for the
  upstream `Idempotency-Key`. The command that promises to retry only what failed
  therefore re-provisioned everything, fresh secret and all, once per run
  (PRSR-23). Don't make it optional again or default it, and don't key identity
  on `--name`: two people legitimately share a name, and merging them is worse
  than duplicating one. Migration 0005 says the same thing in the schema —
  `CHECK (email IS NOT NULL) NOT VALID`, so it binds new rows without having to
  decide the fate of pre-0005 emailless ones at boot. Those are stranded (no
  command can address a person with no address) and hand SQL is the only repair;
  the constraint permits it deliberately.
- **No connector may answer a reconcile it can't verify.** All four refuse an
  emailless `Reconcile`. Switchyard's `findUser` still falls back to matching on
  display name — fine on the Provision path, where Switchyard itself reported the
  conflict, but as an *audit* answer it would record a person against a
  same-named stranger, or mark a real account stale and re-arm provisioning for a
  second token. Requiring the address on `invite` is what left that fallback
  reachable only from the audit, so `Reconcile` guards it there (PRSR-23).
- **A name mismatch blocks `--deliver email`, not copy-paste.** It's the only
  evidence that a mistyped `--email` landed on a *different existing person*, and
  email mails them working credentials before any warning can be read — so Run
  returns `ErrNameConflictOnEmail` before writing an invite row or provisioning
  (409 over HTTP). Copy-paste warns and proceeds; the operator is the gate there.
  Don't "simplify" these into one behaviour: the asymmetry is the point, because
  only one of the two paths can be taken back.
- **`person.email` is unique case-insensitively** (migration 0003), and every
  conflict target must infer on `lower(email)`. The index and the store's
  lookups disagreed once; the gap inserted duplicate identities for the same
  human, which the audit then populated twice.
- **Never persist a secret in plaintext.** `account.secret_hash` is sha256;
  plaintext lives only in the returned/emailed credential block.
- **The credential block is the recipient's; the operator note is the
  operator's.** `Result.CredentialBlock` is the only thing `--deliver email` may
  ever send, and `RenderCredentialBlock` must stay free of operator-facing
  content. The failure list was once a trailing section of the block, so a single
  failed connector mailed an invitee raw `err.Error()` text under a heading
  saying it wasn't for them (PRSR-19). Don't re-merge them or filter the rendered
  string — the split is at the source so the emailer has nothing to get wrong.
- **An invite that provisioned nothing sends no email.** With the operator note
  split out, an all-failed invite's block is a greeting and nothing else, so
  mailing it announces access that wasn't granted — and marks the invite
  delivered, so "did they get it?" answers yes. `deliverable()` gates the send;
  `Delivered` stays false and the CLI says so. A *partial* failure still sends.
- **`unavailable` is a task status, not a flavour of `failed`.** A connector
  returning `connector.ErrPending` — registered but unconfigured, or with no
  upstream provisioning API — records `TaskUnavailable` (PRSR-21). It used to be
  `TaskFailed` plus a `Pending bool`, and every consumer that buckets by status
  then had to remember the bool: the operator note filed it under "what failed"
  and then labelled the line `(pending)`. Don't reintroduce a modifier alongside
  the status, and don't reuse `TaskPending` for it — that's the *queued* state,
  and "hasn't run yet" vs "can't be run" is the collision that caused this. The
  split is only ever consulted for the difference, so let it switch on a status:
  the note groups the two under separate headings, the CLI marks them `…` and
  `✗`. `provision_task.status` is CHECK-constrained (not free text) — a new
  status needs a migration.
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
Access connector and email/copy-paste delivery. All four connectors are live:
`switchyard`, `cloudflare`, `lyceum` (PRSR-10) and `argosy` (PRSR-13) — each of
the three token-gated ones registers Unavailable when its token is unset.

Also shipped: onboarding bundles + the launcher-led credential block (PRSR-12),
`audit` / `reconcile` (PRSR-15), which retired 15 (person × service) pairs of
real drift with zero upstream mutation, `person add` (PRSR-16), the roster entry
point that provisions nothing, the recipient/operator split in the invite result
(PRSR-19), the end of `invite`'s silent rename (PRSR-20), which also made a
name mismatch fatal on the email path, `TaskUnavailable` (PRSR-21), which
stopped a not-yet-configured connector counting as a breakage, and a required
`--email` on `invite` (PRSR-23), which closed the emailless path that minted a
new person — and so a new idempotency key — on every run.

Open, in rough priority order: `Deprovision` is unimplemented on every connector
but Cloudflare (PRSR-17) — so `stale` can re-arm provisioning but cannot revoke;
nothing runs the audit on a schedule (PRSR-18); Purser still authenticates to
Switchyard as the instance bootstrap token rather than a dedicated one (PRSR-3);
and service spin-up is a separate axis, not started (PRSR-11).
