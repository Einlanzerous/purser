# CLAUDE.md — Purser

Cross-service provisioning/invite service for the Construct. One command invites
a person into multiple services, mints credentials, grants Cloudflare Access
SSO, and returns a copy-pasteable credential block (or emails it). Single static
Go binary (CLI + thin HTTP API). Full design: `docs/architecture.md`.

Tracked in Switchyard under the **PRSR** project. It graduated from `SERV-33`;
old `SERV-*` keys still resolve as aliases, so read one in an older commit or
comment as historical. Open work lives on the board, not in this file.

Rationale, incident history and live-probe measurements for the rules below are
in `docs/person-axis-invariants.md`, `docs/spinup-invariants.md` and
`docs/build-identity.md`. Read the relevant one before changing the code a rule
guards.

## Layout

The tree is the directory tour. Only the rules the tree does not show:

- `internal/spinup/` is the **second axis** (hostname-keyed infrastructure).
  It imports `internal/model` and nothing else of ours: not `internal/connector`,
  not `internal/store`. `ServiceProvisioner` and `Connector` are two types that
  share no fields.
- `internal/connectors/cloudflare/` serves both axes (Access `Connector`, plus
  the `DNSProvisioner`, `AccessProvisioner` and `TunnelProvisioner` for
  spin-up) because it is grouped by upstream, not by axis: one API, one token,
  one client in `client.go`. Each connector keeps its own `Config` struct on
  purpose (see the `DNSConfig` rule below).
- `internal/placard/` is deliberately not under `connectors/`: Placard is never
  an invite target and implements neither axis's interface. It picks a logo URL
  and never decides one.
- `cmd/purser/` `setup()` is the composition root and wires both orchestrators,
  `invite.Service` and `spinup.Service` (via `spinupRegistry`/`tunnelSet`).
- `internal/invite/` is where person-axis idempotency lives.
- Migrations are `migrations/NNNN_name.{up,down}.sql`, embedded and applied on
  boot.

## Conventions

- Go 1.26, `pgx/v5`, `google/uuid`. No ORM, no external migration tool.
- Config is env-only, `PURSER_`-prefixed, with a `DATABASE_URL` fallback
  (`internal/config`). No config files. Port 4006, health at `GET /healthz`.
- Logs go through stdlib `log` to **stderr**, which is also where the CLI
  prints its summary, so a warning logged and also reported prints twice.
- Conventional commits and release-please per `PRINCIPLES.md`; image is
  `ghcr.io/einlanzerous/purser`.

## Invariants — don't break these

### Person axis

- **Idempotency is per (person x service).** `account` has `UNIQUE(person_id,
  service_id)`; the orchestrator skips active accounts and retries only failed
  ones. Bundles are only named service lists: no bundle-specific provisioning
  path. A bundle grants Switchyard project membership, never the instance role.
- **`Reconcile` never mutates.** No create, mint, rotate or revoke. A connector
  with no lookup returns `ErrReconcileUnsupported`; none may answer a reconcile
  it cannot verify, so all five refuse an emailless one.
- **Unverifiable is not absent.** `UpstreamUnknown` is distinct from
  `UpstreamNo`, and a failed connector call never marks records stale.
- **`person add` provisions nothing.** It writes one `person` row, no accounts,
  no credentials, no `Provision`.
- **An occupied email is a conflict, not an edit.** A person row is created only
  by `InsertPersonIfAbsent` and renamed only by `RenamePerson`; only `person add
  --rename` changes a name. `invite` reports a disagreement as
  `Result.NameConflict`. No name-setting upsert, no rename path in `invite`.
- **`invite` requires an email on every delivery method.** The address is the
  identity key and person id is what idempotency keys on. Never optional, never
  defaulted, never keyed on `--name`. Migration 0005 enforces it (`NOT VALID`).
- **A name mismatch blocks `--deliver email` but only warns on copy-paste**
  (`ErrNameConflictOnEmail`, 409 over HTTP). Do not unify the two behaviours:
  only one of the paths can be taken back.
- **`person.email` is unique case-insensitively** (migration 0003); every
  conflict target infers on `lower(email)`.
- **`offboard` previews; `invite` acts.** A dry run makes no connector call at
  all; `--apply` acts; `--email` is required and always one person, no bulk
  mode. Dry run and apply share one code path.
- **Only a successful `Deprovision` marks an account `deprovisioned`.**
  `failed` and `unavailable` leave it active so the next run retries.
  `revoked-not-recorded` (connector succeeded, write failed) is its own status
  because its advice is the opposite. `offboard` exits non-zero on
  `unavailable`.
- **The preview must not promise what `--apply` refuses.**
  `connector.CanDeprovision` answers from capability and config alone; a
  connector that knows it cannot act implements `RevokeChecker` and its
  `Deprovision` delegates to the same answer.
- **`Reconcile` must guard Switchyard's `findUser` name fallback.** `findUser`
  falls back to display name, which is fine on `Provision` but as an audit
  answer would record a person against a same-named stranger or mark a real
  account stale. That is why every connector refuses an emailless reconcile.
- **`ErrRevokeUnavailable` is the revoke twin of `ErrPending`.** Match with
  `connector.IsUnavailable`. Never reword `ErrPending`: migration 0004's
  backfill matches its text as a literal prefix.
- **A 404 on a recorded `external_id` is a wrong record, not absent access.**
  Switchyard and Lyceum fall back to email lookup and revoke what that finds.
  Lyceum's DELETE needs a numeric id, so a non-numeric `external_id` resolves
  by lookup.
- **`Deprovision` means revoke, not delete,** except Lyceum, whose only endpoint
  is `DELETE`. Argosy has no endpoint and returns `ErrRevokeUnavailable`.
- **Revoking Switchyard does not close SSO;** the Cloudflare Access group does.
  The CLI warns when an offboard skips `cloudflare`; keep the warning while the
  asymmetry exists.
- **The `account` row is marked, never deleted.** `deprovisioned_at` is written
  on the transition and never cleared (an active row may carry one; read
  `status`). Deleting the row would erase history and re-arm provisioning.
- **Never persist a secret in plaintext.** `account.secret_hash` is sha256;
  plaintext exists only in the returned or emailed credential block.
- **The roster reads local records only.** `person list` / `person show` make no
  connector call (`Roster`, `PersonDetail` never touch `s.registry`) and read
  `store.AccountRecord`, which has no secret field. Do not add one. `list`
  reports `Hidden` rather than omitting non-active accounts silently.
- **The credential block is the recipient's; the operator note is the
  operator's.** `--deliver email` sends only `Result.CredentialBlock`, which
  must stay free of operator-facing content (error text). Split at the source;
  do not filter the rendered string.
- **An invite that provisioned nothing sends no email** (`deliverable()`);
  `Delivered` stays false. A partial failure still sends.
- **`unavailable` is a task status, not a flavour of `failed`.** A connector
  returning `connector.ErrPending` records `TaskUnavailable`. No modifier field
  beside the status; do not reuse `TaskPending` (queued). `provision_task.status`
  is CHECK-constrained, so a new status needs a migration.
- **Switchyard user create needs the email set** (the SSO join key).
- **Connectors treat "already exists" upstream as success,** and a per-service
  failure never aborts the whole invite.

### Build identity

`/healthz` reports what was stamped in and never guesses; the generic rules are
`PRINCIPLES.md` section 4. Purser-specific:

- **The version is bare semver, compared with strict equality** against the image
  label `org.opencontainers.image.version` (PRSR-32; docker/metadata-action
  stamps it without the `v`). Any disagreement is a permanently unresolvable row
  in the delivery ledger. The Makefile strips the `v`; `Get()` reports verbatim.
- **Read the version through `version.Get()`, never the raw vars.** A declared
  but unpassed Docker `ARG` links an empty string over the `dev` default; only
  `Get()` maps blank back to `dev`/`null`.
- **`publish.yml` stamps `VERSION` from its `ver` step's `stamp` output only,**
  never `steps.meta.outputs.version` (it can be `latest` or `sha-<short>`).
  Adding any `docker/metadata-action` tag entry with priority above 900 breaks
  label/binary agreement on the release path.
- **`:latest` has exactly one writer, the main build** (`flavor: latest=false`,
  SERV-108), so dev reads `version=latest` / `/healthz: dev` by design. Check
  the open decision on SERV-108 before changing the `enable=` conditions.
- **The Makefile's `dev` fallback goes inside the subshell, before the pipe:**
  `||` binds to the pipeline and `sed` exits 0 on empty input.

### Spin-up axis (`internal/spinup`)

- **`Ensure` previews; `--apply` acts** (and `--prune` additionally for
  removals). Preview and apply are one code path: one read-only `Inspect` per
  step decides the plan. `Teardown` is stricter: a dry run makes no provisioner
  call at all, works from recorded rows, and takes no spec. It returns a
  `spinup.Removal`, not a bare `error`, so the tunnel's concurrent-write
  `Warning` reaches the operator instead of only a server log.
- **DNS applies last and comes off first.** `model.KindOrder` is the apply order;
  `TeardownOrder()` is derived from it, never a second list. `dependsOn` /
  `teardownDependsOn` hold DNS (`blocked`) when a prerequisite did not land,
  because publishing before the gate exists exposes a service ungated.
  A bookmark Access app is not a DNS prerequisite going up.
- **The teardown preview must not promise what `--apply` refuses.**
  `spinup.TeardownChecker` / `CanTeardown` is the twin of `RevokeChecker`: it
  answers from capability and config alone, contacts nothing, asks about the
  empty spec a teardown carries, and `Teardown` delegates to the same answer.
- **A resource row exists only for a resource that exists,** so a failed step
  records nothing and `Teardown` targets recorded ids, never hostnames.
  `applied-not-recorded`, `removed-not-recorded` and `pruned-not-recorded` are
  their own statuses. Only a successful removal marks a row removed; both
  `removed_at` and `deprovisioned_at` stamp on every transition into the status.
- **An already-correct resource is adopted, not recreated** (row written, no
  upstream call).
- **Ownership is checked on rows, whole-run, before any `Inspect`.** Teardown
  requires `--service` and `--hostname` to agree; an `Ensure` or prune that finds
  an active row recorded to another service refuses the whole run
  (`ErrHostnameNotThisService`, exit 2 / 409). A hostname moves only with
  `--reassign-from KEY`. Never refuse per kind: a partial run leaves the
  hostname half-owned. A *removed* row from a former owner does not refuse a
  teardown. `--reassign-from` naming the spec's own service is
  `ErrReassignFromSelf` (400). `rebindOrphan` moves orphans (a row write,
  nothing upstream), and `Pending` counts an `orphaned` line carrying a rebind.
- **`--prune` runs after the whole additive pass and is held back when any step
  the spec calls for did not land** (`unmetPruneDeps`).
- **Unverifiable is not absent, here too.** A failed `Inspect` is `unknown` and
  `--apply` does not act on it. `refused` (read succeeded, upstream state not
  writable, fix it there) is a different status from `unknown` (read failed,
  re-run) and from `unavailable` (Purser's own missing credential). In DNS,
  `pickCandidate` (several records answer, none is the spec's) is `refused`;
  `records()`'s full-page lookup is `unknown`.
- **Tunnel ingress is one shared document written back whole.** Hold rules as
  raw JSON, never a tidy struct. Insert before the first rule that shadows the
  hostname (`scanRoute`), never adopt a wildcard, keep the catch-all last, and
  refuse a malformed document, a non-`cloudflare` or absent `source`, on the
  read path and the teardown path as well as the write path.
  `hostnameTakes` mirrors cloudflared's matcher; re-read its source before
  changing it. `TunnelProvisioner.docMu` serializes the read-modify-write with a
  fresh read inside the lock, and the config `version` must move by exactly one.
  `Ensure` and `Teardown` must agree on a rule shadowed by a wildcard or
  catch-all: `withoutRoute` leaves it alone and names it, and `removed == 0 &&
  behind > 0` is refused, not read as success.
- **The tunnel is a spec field.** `TunnelSet` resolves it once per run; `dev`
  refuses (`ErrTunnelUnconfigured`, a 400), never falls back to prod.
- **An Access app is updated by merge, never rebuilt.** It is held as a map,
  `policies` is appended to never assigned, and a policy's `id` is never
  stripped (a `reusable` policy is a reference). Anything else deletes the gate
  or somebody else's access.
- **`findApp` selects exactly one whole-host app (`servesWholeHost`).** A
  path-scoped app, such as the `bypass` webhook one, is not that hostname's app.
  Ambiguity is `ErrRefused`, and `confirmGone` treats ambiguous as an error,
  never as absent; it asks about the id, then the hostname, and needs both. Selection stays on `domain`.
- **Absence is spelled per product and decided by code.** `dnsRecordNotFound`
  (81044) lives in `dns.go`; a bare 404 is not absence; add a code only after
  observing it. The Access teardown re-reads rather than trusting 11021.
- **Cloudflare `do()` does not assume the `{success, errors}` envelope or a
  body:** absent `success` on a 2xx is success, empty body is checked first.
- **A logo is a ref (`placard` | `none` | https URL), default `placard`** in
  `Normalized`. Only `none` clears an icon; an unresolvable, broken or unknown
  logo leaves the live one alone (a note, never drift, never fatal), except
  where the live icon *is* the dead URL the spec asks for: clear it, or the drift
  is permanent and every `--apply` does a full-replacement PUT (`current !=
  want`). `checkLogo` fetches sessionless at write time; the plan fetches the
  candidate too, unconditionally, never guarded on `live != ""`.
  Aspect ratio outside 0.8-1.25 is a note only. Placard picks, never verifies.
- **A direct DNS spec pins the value only.** TTL and proxied are create-time
  defaults; an update carries the existing values across.
- **`DNSConfig` is not `cloudflare.Config`.** The zone id stays out of the Access
  connector's readiness check. The zone preflight runs first; the
  create-then-delete `wrongName` check stays as the backstop; its cleanup
  (`removeStray`) runs on the create path only, because on an update the record
  predates Purser and nobody asked for it to be removed.
- **`Result.NeedsAttention()` is the verdict;** `Pending()` counts only what
  `--apply` would act on and is not a success signal. `orphaned` does not count
  toward `NeedsAttention`; `applied-not-recorded`, `removed-not-recorded` and
  `pruned-not-recorded` do. Warnings travel in
  `Resource.Warning` / `Removal.Warning`, logged once per surface.
- **`Normalized()` trims every padded field first,** before anything that reads
  it. `Key` and `Hostname` are not case-folded. A spec is flags, not a file.
  `spinupRegistry` registers all three provisioners unconditionally.
- **Fakes model your assumptions, not the API.** Check each new Cloudflare
  route's real response; run the binary against the real API before trusting a
  green suite on this axis.

## Testing

- `make test` runs unit tests (fake store and connectors, httptest for HTTP
  upstreams, in-process SMTP). DB-backed store tests skip unless
  `PURSER_TEST_DATABASE_URL` is set.
- `make test-db` spins a throwaway Postgres 16 and runs everything.
- Never point tests or `purser migrate` at the live shared `postgres`; use a
  throwaway container or the `_test` DB.
