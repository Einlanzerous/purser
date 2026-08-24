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

- `cmd/purser/` — entrypoint + subcommands (`serve`, `invite`, `offboard`,
  `person add|list|show`, `audit`, `reconcile`, `migrate`, `version`).
  Composition root: `setup()` wires store + connectors + orchestrator.
- `internal/model/` — domain types (person, service, account, invite,
  provision_task, service_resource), 1:1 with the schema.
- `internal/connector/` — the `Connector` interface + `Registry` +
  `Unavailable` (registered-but-unconfigured) + `ErrPending`.
- `internal/connectors/{switchyard,cloudflare,lyceum,argosy}/` — per-service connectors.
- `internal/invite/` — the orchestrator (`Run`) + credential-block renderer. This
  is where idempotency lives.
- `internal/spinup/` — the **second axis** (PRSR-27): `ServiceSpec`, the
  `ServiceProvisioner` interface, its own `Registry`/`Unavailable`/
  `ErrUnavailable`, and the `Ensure` orchestrator. Keyed on hostname, not on a
  person. It imports `internal/model` and nothing else of ours — deliberately
  not `internal/connector`, and not `internal/store`.
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
- **`offboard` previews; `invite` acts.** The one genuinely destructive command
  inverts every default the provisioning path takes (PRSR-17). A dry run makes
  **no connector call at all** — not a read-only one — and `--apply` is what acts;
  a duplicate grant is wasteful, but revoking the wrong person is not fixed by
  re-running. There is no bulk mode: `--email` is required and always one person,
  which is a stronger guard than the flag `reconcile --all` needs. Dry run and
  apply share one code path, so the preview is exactly what the apply does.
- **A revoke that didn't happen must never be recorded as one.** Only a
  successful `Deprovision` marks the account `deprovisioned`; `failed` and
  `unavailable` leave it **active** so the next run retries it. The lie outlives
  the error message — the audit, `person show`, and the next invite's idempotency
  skip all read that column and would report access as removed while it is live.
  For the same reason `offboard` exits non-zero on `unavailable`, the opposite of
  `invite`, where nothing was granted and waiting harms nobody. The inverse has
  its own state too: `revoked-not-recorded` means the connector succeeded and the
  write didn't, so access *is* gone and Purser's records are wrong — the opposite
  advice from `failed`, which is why they aren't one status.
- **The preview must not promise what `--apply` refuses.** Argosy's `Deprovision`
  is an unconditional refusal and so is every `Unavailable` stub, so a dry run
  that reported `revoke` for them would break the property that makes previewing
  worthwhile. `connector.CanDeprovision` answers from capability and config
  alone, contacting nothing; a connector that knows it can't act implements
  `RevokeChecker` and its `Deprovision` delegates to the same answer so the two
  cannot drift.
- **`ErrRevokeUnavailable` is the revoke-path twin of `ErrPending`.** Same
  bucket — match with `connector.IsUnavailable` — but "provisioning not yet
  available" is the wrong sentence on an offboard, and `ErrPending`'s text can't
  be reworded because migration 0004's backfill matches it as a literal prefix.
- **A 404 against a *recorded* `external_id` is a wrong record, not absent
  access.** Both Switchyard and Lyceum fall back to the email lookup and revoke
  what that finds. Treating it as "nothing to revoke" marks the account
  deprovisioned while the real user's access stays live, and the next run skips
  it — silent and permanent, unlike a failure. Lyceum has a second trap: its
  `Provision` 409 branch records the *email* in `external_id`, and its DELETE
  handler `ParseInt`s the path, so a non-numeric id must resolve by lookup
  instead.
- **`Deprovision` means revoke, not delete — except Lyceum.** Switchyard revokes
  tokens and keeps the user, because deleting would take their authored tickets
  with it; Cloudflare drops the email from the Access group. Lyceum's admin
  surface offers only `DELETE /admin/users/{id}`, so there the two collapse — say
  so rather than letting the interface's wording imply a reversibility it hasn't
  got. Argosy has no endpoint at all and returns `ErrRevokeUnavailable`, so a three-of-four
  offboard reports honestly instead of claiming what it didn't do.
- **Revoking Switchyard does not close its door.** Its tokens gate the API; the
  SSO login is gated by the *Cloudflare Access group*, so an offboard that skips
  `cloudflare` leaves a working sign-in behind while looking finished. The CLI
  says this outright when it happens. Don't remove that warning without removing
  the asymmetry that makes it true.
- **The `account` row is marked, never deleted**, and `deprovisioned_at`
  (migration 0006) is what makes "when was it taken away" durable. `status` +
  `updated_at` looked like enough and wasn't: `UpsertAccount` sets active and
  bumps `updated_at`, so the next invite erased both — and re-inviting someone is
  ordinary. The column is written on the transition into `deprovisioned` and
  never cleared, so an *active* row may still carry one; read `status` for the
  current state. Deleting the row instead would destroy the history the audit
  exists to read *and* silently re-arm provisioning, since the idempotency skip
  keys on an active account and a missing row means the opposite of a
  deprovisioned one.
- **Never persist a secret in plaintext.** `account.secret_hash` is sha256;
  plaintext lives only in the returned/emailed credential block.
- **The roster reads records, and cannot read a secret.** `person list` /
  `person show` (PRSR-24) read local tables only — `person`, `account` and
  `service`, plus `invite` for `show`'s history — and neither `Roster` nor
  `PersonDetail` references `s.registry`, because "who is on the roster" must
  not depend on every upstream being reachable, and that is the whole difference
  from `audit`. Don't give them a connector call. They read
  `store.AccountRecord`, which has no `secret_hash`/`secret_ref` field and comes
  from a query selecting neither column: credentials are shown once, at invite
  time, and `--json` serializes whatever the struct holds, so the guarantee is
  that the field doesn't exist rather than that no renderer prints it. Don't add
  one "just for the hash". `list` hides non-active accounts by default but
  reports `Hidden` so the omission is never silent — `--to lyceum` finding
  nobody has to mean "nobody has Lyceum", not "nobody has it any more" — and its
  service filter selects on the same accounts it displays. `show` filters
  nothing; a stale row is the point of the single-person view.
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

### The spin-up axis (`internal/spinup`, PRSR-27)

- **It is a second interface, not a widened first one.** Everything above is
  person-shaped — `Connector.Provision(Input{PersonName, Email, …})`, idempotent
  per (person × service). Spin-up provisions the infrastructure that makes a
  service *exist*, is keyed on hostname, and is idempotent per (hostname, kind).
  Bending `Connector` to serve both would mean widening `Input` until neither
  axis's fields mean anything. The two share an ethos and no types; `spinup`
  does not import `connector`, and its `ErrUnavailable` is its own sentinel
  because `ErrPending`'s wording is pinned by migration 0004's backfill.
- **`Ensure` previews; `--apply` acts.** The inverse of `invite`, matching
  `offboard`, and settled here so the DNS/Access/tunnel provisioners can't
  disagree. Two of the three steps are additive and idempotent, which argues for
  acting — but the third appends to a tunnel's ingress configuration, a
  read-modify-write of one document holding every *other* service's routes, and
  that is the mistake re-running doesn't fix. As with `offboard`, preview and
  apply are one code path: a single `Inspect` per step (read-only by contract)
  decides the plan, and `--apply` is that same decision plus the write.
- **DNS is applied last, and only if what it depends on landed.**
  `model.KindOrder` puts it last because it is the step that makes the hostname
  live; the other two are inert until something resolves. But ordering only
  closes the window when the earlier step *succeeded*, so `ServiceSpec.dependsOn`
  holds the DNS step (`StepBlocked`) when a prerequisite is failed, unavailable
  or unknown. Publishing anyway leaves a tunnelled service answering 502 until
  its route lands and — the reason this is an invariant and not a preference — a
  service meant to be gated reachable *ungated*, which is self-concealing in a
  way the 502 isn't. A **bookmark** Access app is deliberately not a
  prerequisite: it is a launcher tile in front of a service with its own login,
  so its absence costs an icon, not a gate. Blocking withholds changes, never the
  report — an already-correct record is already published, and an `adopt` writes
  a row without touching the edge.
- **An already-correct resource is adopted, not recreated.** Upstream matching
  the spec with no record of ours is `adopt`: `--apply` writes the row and makes
  no upstream call. Argosy's edge predates this axis, so the pilot is a service
  that already exists — a spin-up that can only recognise what it built itself is
  one nobody will point at production, and re-creating a live DNS record to
  obtain a row is the wrong way to learn its id.
- **A resource row exists only for a resource that exists.** A failed step
  records nothing, so "no row" means "we put nothing here", never "we tried".
  That is what lets `Teardown` target recorded ids instead of guessing by
  hostname — deleting a record someone made by hand is not fixed by re-running.
  The inverse gets its own status: `applied-not-recorded` means the edge changed
  and the write didn't, the opposite advice from `failed` (compare
  `revoked-not-recorded`, PRSR-17). And `service_resource.hostname` is unique
  case-insensitively, for the same reason `person.email` is (migration 0003).
- **The tunnel is a spec field, not a global** (PRSR-33). The account has two
  healthy tunnels; a dev instance is the same shape pointed at a different one.
  Specs name a ref (`prod` | `dev`) and `TunnelSet` resolves it to an id once per
  run, before any step, so the ingress route and the DNS record cannot end up
  describing different tunnels. Only `prod` is wired; `dev` resolves to a
  refusal rather than falling back — which is the entire point of a named ref.
- **Never treat unverifiable as absent**, here too. A failed `Inspect` is
  `unknown`, and `--apply` does not act on an unknown step: acting on a state
  that couldn't be read creates a second copy of something, or rebuilds a shared
  ingress document from a read that just failed.

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
stopped a not-yet-configured connector counting as a breakage, a required
`--email` on `invite` (PRSR-23), which closed the emailless path that minted a
new person — and so a new idempotency key — on every run, and the read-only
roster commands `person list` / `person show` (PRSR-24), which answer "who has
what" from local records so nobody has to reach for psql, and `purser offboard`
(PRSR-17), which gave Purser the revoke half it had never had — three of four
connectors revoking, Argosy reporting `unavailable` until it has an endpoint, and
`deprovisioned` finally a status something writes.

**Service spin-up (epic PRSR-22) is in progress**, and its prerequisites are
done. PRSR-11 closed both halves: the CF token carries Zone→DNS→Edit (scoped to
`zerogravity.industries`) and Account→Cloudflare Tunnel→Edit, both probed
against the live API, and `PURSER_CF_ZONE_ID` / `PURSER_CF_TUNNEL_ID` ship
through `internal/config`, `.env.example` and the deploy compose as of v0.14.0.
Edit subsumes Read in Cloudflare's model, so there is no separate read scope to
grant: keeping `Reconcile` read-only is a constraint on the code, not on the
token. PRSR-26 closed done — the tunnel is remotely-managed (`source:
"cloudflare"`), so the tunnel connector is an ordinary API client and no tunnel
migration is needed.

PRSR-27 landed the foundation: `internal/spinup` (spec, `ServiceProvisioner`,
registry, `Ensure` orchestrator) and `service_resource` (migration 0007). It
settled the two questions the connectors were waiting on — preview by default,
and the tunnel as a spec field — and it deliberately stops short of the three
provisioners and the CLI. `Teardown` is on the interface, because the resource
table exists to give it concrete ids to target, but nothing orchestrates a
teardown yet: its ordering and its "is this hostname still someone's?" question
belong with the command that needs them.

Next on that axis, and independent of each other now: **PRSR-28** (DNS record),
**PRSR-29** (Access application — `self_hosted` + policy vs `bookmark`, and a
`logo_url` that must be verified reachable before it is written) and **PRSR-30**
(tunnel ingress route — insert before the terminal catch-all rule, and guard the
shared document the way the Access connector guards the group's email list).
Then **PRSR-31** — the `provision-service` CLI/HTTP surface and Argosy end to
end, on the direct path, so it needs PRSR-28 and PRSR-29 but not PRSR-30.
**PRSR-33** wires the `dev` tunnel ref that PRSR-27 left resolving to a refusal.

Also open: nothing runs the audit on a schedule (PRSR-18). Argosy has no delete
or disable endpoint, so it is the one service `offboard` cannot close (ARGY
ticket pending).

PRSR-25 — the dedicated Switchyard provisioning token — reads as closed/done on
the board, but its own last comment (2026-08-15) records step 2, minting the
token, as still blocked on SERV-49, which is still in Backlog: the assistant's
MCP token holds no `users:manage`. The `purser` agent user itself has existed
since 2026-08-01. Check what `PURSER_SWITCHYARD_TOKEN` actually holds before
repeating either answer — the board and the thread disagree, and this file has
been wrong about it in both directions.
