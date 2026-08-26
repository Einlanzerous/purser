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
  `provision-service`, `person add|list|show`, `audit`, `reconcile`, `migrate`,
  `version`). Composition root: `setup()` wires store + connectors + **both**
  orchestrators — `invite.Service` and, since PRSR-31, `spinup.Service` via
  `spinupRegistry`/`tunnelSet`.
- `internal/model/` — domain types (person, service, account, invite,
  provision_task, service_resource), 1:1 with the schema.
- `internal/connector/` — the `Connector` interface + `Registry` +
  `Unavailable` (registered-but-unconfigured) + `ErrPending`.
- `internal/connectors/{switchyard,cloudflare,lyceum,argosy}/` — per-service connectors.
  `cloudflare/` serves **both** axes: the Access `Connector` (person × service)
  and, on the spin-up axis, `DNSProvisioner` (PRSR-28, `dns.go`),
  `AccessProvisioner` (PRSR-29, `access.go`) and `TunnelProvisioner` (PRSR-30,
  `tunnel.go`) — over one shared API transport in `client.go`. Grouped by
  **upstream, not by axis**: one API, one token, one place the read-modify-write
  hazard is solved and the next person to meet it can see the precedent. The axes
  stay separate where it counts — `spinup` imports neither `connector` nor this
  package, and a `ServiceProvisioner` and a `Connector` remain two types sharing
  no fields. All four have separate Config structs on purpose — see the
  invariant below.
- `internal/invite/` — the orchestrator (`Run`) + credential-block renderer. This
  is where idempotency lives.
- `internal/spinup/` — the **second axis** (PRSR-27): `ServiceSpec`, the
  `ServiceProvisioner` interface, its own `Registry`/`Unavailable`/
  `ErrUnavailable`, and the `Ensure` orchestrator. Keyed on hostname, not on a
  person. It imports `internal/model` and nothing else of ours — deliberately
  not `internal/connector`, and not `internal/store`.
- `internal/placard/` — the launcher-mark resolver (PRSR-37). A client for
  Placard's `/api/services`, deliberately **not** under `internal/connectors/`:
  Placard is never an invite target and implements neither axis's interface.
  It picks a URL and never decides one — see the invariant below.
- `internal/store/` — pgx pool, embedded migrator, repo queries.
- `internal/delivery/` — SMTP sender (email delivery).
- `internal/api/` — thin HTTP surface.
- `migrations/` — `NNNN_name.up.sql` / `.down.sql`, embedded, auto-applied on boot.

## Conventions (match the construct-server house style)

- Go 1.26, `pgx/v5`, `google/uuid`. No ORM. No external migration tool — the
  in-process migrator in `internal/store/migrate.go` applies embedded SQL.
- Config is env-only, `PURSER_`-prefixed, with a `DATABASE_URL` fallback
  (`internal/config`). No config files.
- Logs: stdlib `log`, which writes to **stderr** — nothing calls `SetOutput`.
  (This line said stdout until PRSR-31, where it mattered: the CLI's own summary
  also goes to stderr, which is what made a logged-and-detailed warning print
  twice on the same stream.) Health: `GET /healthz`. Port 4006.
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

### Build identity on `/healthz` (PRSR-32)

- **`/healthz` reports what was stamped into the binary, and never guesses.**
  Switchyard's delivery reconciler polls it and records the answer as the
  *observed* half of the estate's delivery ledger (the SWY-192 contract, rolled
  out by SERV-128). An observation is the half that is supposed to be
  trustworthy, so a plausible-looking version the build did not ship becomes a
  real row indistinguishable from a real deploy. An unstamped build says `dev`
  and an unknown commit says `null`; neither is ever inferred from `go.mod`, a
  VCS stamp, or the image tag.
- **Blank is not unset, so read through `version.Get()` — never the raw vars.**
  A Docker `ARG` that is declared but never passed expands to an *empty string*,
  and `-X ...Version=` links that empty string in **over** the `dev` default
  rather than leaving it alone (SWY-224). `Get()` is the only thing mapping
  blank back to `dev`/`null`, so a caller reading `version.Version` directly
  reports `""` where it means `dev`. The vars stay exported solely because
  `Get()`'s rule has to be exercised from sibling packages' tests. This was
  invisible while the Dockerfile still defaulted to the placeholder `docker`;
  emptying that default is what made the bypass reachable.
- **The version is bare semver, everywhere it is produced.** It is compared with
  *strict equality* against `org.opencontainers.image.version`, which
  docker/metadata-action stamps without the `v`. Report `v0.15.0` against a
  label of `0.15.0` and every deploy report is filed `claimed_not_confirmed`
  for ever — a permanent red row on the one page whose job is to be believed.
  That is why the Makefile strips the `v` off `git describe`, and why `Get()`
  reports verbatim instead of sanitising: a prefix is a bug in the *build*, and
  silently stripping it at the reporter hides it from the only place it shows.
- **`VERSION` is passed only on a tag build.** On a push to main
  `steps.meta.outputs.version` is the literal `latest` — a moving target, and
  storing it as an identity means comparing it against the image label and
  disagreeing for ever. `publish.yml` passes blank there instead, which maps to
  `dev`. `GIT_SHA` goes on **both** paths: a main build has a real commit and
  should say so, it just has no release to name. The sha is the full 40
  characters, because the cross-service comparison is an equality test and not
  a prefix match, and an absent one marshals to JSON `null` rather than `""` —
  "recorded no commit" is a different claim from "built at the empty commit".
- **The Makefile's `dev` fallback lives inside the subshell, before the pipe.**
  `git describe ... | sed 's/^v//' || echo dev` never fires: `||` binds to the
  *pipeline*, whose status is `sed`'s, and `sed` exits 0 on empty input. An
  untagged checkout or a source tarball then linked a blank version.

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
- **The terminal catch-all rule stays last, and that is checked rather than
  assumed.** A tunnel's ingress list ends in a rule matching everything (no
  hostname, no path — typically `http_status:404`), which cloudflared requires. A
  rule appended *after* it is never matched and **nothing errors**: the route
  simply doesn't work, which is why this is asserted on the document before the
  PUT and again on the read-back, rather than trusted. The generalisation is the
  same bug one step out — a catch-all that isn't last has already killed every
  rule behind it, so inserting before the final rule there would land the new
  route in the dead tail. Both shapes are refused, not repaired: a document
  Purser doesn't understand is not one to rewrite on a guess. Anything walking
  ingress entries must also tolerate the missing hostname instead of tripping
  over it.
- **The dead tail is a *read*-path rule too, and that is the half that bites.**
  The write path refusing a malformed document isn't enough on its own, because
  `Inspect` is the only call a dry run makes and the status of every step is
  decided from it. A rule behind a catch-all reported as a working route gives
  the orchestrator `ok`/`adopt` — both `inPlace()` — so the DNS step unblocks and
  publishes a hostname in front of a tunnel that will 404 it, which is precisely
  the window `KindOrder` and `dependsOn` exist to close. So `findRoute` stops
  where cloudflared stops, at the first catch-all, and a rule behind one is not
  found. And the gate is `documentShape`, not just "is this rule dead": every
  answer other than *reachable and already correct* is one `--apply` would write
  from, so a document it will refuse must preview as `unknown` rather than as
  `create` or `update` — a plan is the first half of the apply, not a guess at it
  (PRSR-27), and a preview that promises what apply refuses is the same broken
  promise `connector.CanDeprovision` exists to stop making on `offboard`. A
  hostname that *is* served on a document broken elsewhere still reports in
  place, with the malformation named: that resource is already published, so
  withholding the line protects nobody.
- **The catch-all is not the only thing that shadows.** cloudflared matches
  ingress top-down, first match wins, and a rule's hostname may carry `*`
  wildcards — `*.zerogravity.industries` in front of a holding page is a
  *documented, deliberate* configuration, so this precondition is likelier than
  the malformed-document one, not rarer. So the walk stops at the first rule that
  would take the hostname (`scanRoute`/`shadows`), of which the terminal
  catch-all is just the case where the pattern is empty. Both paths read through
  that one helper, which is what keeps them agreeing: without it the read path
  reports a shadowed rule as a working route, and the write path inserts a new
  rule into the same dead region and then *confirms* it — a PUT lands, the
  read-back passes, DNS publishes, and the hostname serves the holding page with
  nothing erroring anywhere. **A new route is therefore inserted before the first
  rule that shadows it**, not before the terminal rule; most-specific-first is
  cloudflared's own idiom and the only position where the route works. And a
  wildcard is **never adopted as ours** even when it serves exactly what the spec
  asks: `isRoute` matches literally, because adopting it would have `Teardown`
  delete a rule standing in front of every other hostname in the zone.
- **`hostnameTakes` mirrors cloudflared's matcher; it does not approximate it.**
  It was a general `*`-glob first, and a glob is wrong in two directions at once.
  The rule, read from `Rule.Matches` in cloudflared's `ingress/rule.go` and
  `matchHost` in `ingress/ingress.go` rather than inferred: `""` and `"*"` match
  everything; otherwise exact equality; otherwise, **only** a leading `*.` is a
  wildcard, and it trims the `*` alone so the suffix tested keeps its dot. So
  `wiki.*` and `*.*.industries` are *literals* upstream and stop nothing — a glob
  treating them as patterns reports `create` for a hostname whose real rule is
  right below, inserts a duplicate in front of it, and calls a serving rule
  "never matched". And `*.zerogravity.industries` does **not** take the apex
  `zerogravity.industries`, so an apex route belongs in front of the terminal
  rule and works there. Both are pinned in `TestHostnameTakes` with the reason on
  each row; if that matcher is ever rewritten, check the source again rather than
  reasoning from the shape of the wildcard. cloudflared's own `isCatchAllRule` is
  `(Hostname == "" || Hostname == "*") && Path == ""` — the same predicate — and
  its validation rejects a non-last catch-all outright, so refusing that document
  is agreeing with the thing that has to serve it.
- **Check that the document is the one in force, not just who else wrote it.**
  `getConfig` refuses any tunnel whose `source` is not `cloudflare`. A
  *locally*-managed tunnel is configured by a YAML file on the origin machine, so
  the remote configuration this endpoint returns is not what it serves — and none
  of the four write guards can see that, because every one of them is about *who
  else wrote this document*. Left unchecked the entire sequence succeeds: the read
  reports "no ingress rule" from a document that is no evidence about what is
  served, the PUT is stored, the read-back finds the route, and DNS publishes a
  hostname the tunnel has never heard of, with nothing erroring anywhere. An
  **absent** `source` is refused too — "we could not tell" is not "it is fine",
  and this is the oldest invariant on the axis. PRSR-26 established that
  `construct-server` is remotely managed by hand, once; nothing re-asserted it at
  run time, converting a tunnel is a dashboard toggle, and PRSR-33 wires a second
  tunnel whose mode nobody has checked.
- **The ingress document is written back whole.** `PUT …/configurations`
  replaces it, so every key omitted is a setting the tunnel loses —
  `warp-routing`, the tunnel-wide `originRequest`, a per-rule `noTLSVerify`
  somebody set once by hand. Rules are held as raw JSON per key for exactly that
  reason: a field this build doesn't model is still one it can hand back
  byte-for-byte. Don't decode ingress into a tidy struct.
- **The lost update is the hazard, and one guard doesn't cover it.** There is no
  per-hostname write, so a stale read doesn't corrupt this service's route — it
  deletes somebody else's. `TunnelProvisioner.docMu` serializes the
  read-modify-write the way `groupMu` does for the Access group's email list, and
  `Ensure`/`Teardown` take their **own fresh read inside that lock** rather than
  building a write on the plan's `Inspect`, which ran outside it. The read-back
  then confirms our own route landed — and the configuration's `version` is
  checked to have moved by exactly one, because that is the only thing that can
  see a *different process* writing in between: confirming our own route always
  passes, since our write necessarily contains everything our own read did. A
  version jump is a warning on a step that succeeded, not a failure of it; what
  may have been lost is another service's route.
  **The `+1` is measured now, not assumed** (PRSR-38, on a disposable tunnel
  created and deleted for the purpose — neither live tunnel is a place to run
  an experiment). A content-changing PUT moves `version` by exactly one
  (0→1→2→3→4 across four writes), so our own route-adding write can never
  trip the guard and the false alarm this was worried about does not exist. The
  refinement: an **identical** PUT does not move it at all (2→2→2). That is the
  right behaviour rather than a gap — a writer who changed nothing lost nobody's
  route — but it does mean `version` counts *revisions*, not requests, so don't
  reach for it as a write counter. The PUT response also returns the new
  version directly, so the read-back is earning its keep by confirming the route
  landed, not by fetching the number.
- **Never treat unverifiable as absent**, here too. A failed `Inspect` is
  `unknown`, and `--apply` does not act on an unknown step: acting on a state
  that couldn't be read creates a second copy of something, or rebuilds a shared
  ingress document from a read that just failed. The DNS provisioner extends
  this past outright failure: **several records answering for one name, none of
  them the spec's, is also `unknown`** — a guess there changes whichever record
  wasn't this service's — and so is a lookup that comes back a *full page*,
  which means the name filter narrowed nothing and page one of the zone would
  otherwise read as "nothing here".
- **A direct spec pins the record's value and nothing else.** A tunnelled record
  must be proxied (SERV-45: an unproxied CNAME to `cfargotunnel.com` reaches
  nothing, since the tunnel is only reachable from Cloudflare's edge), so that is
  checked and an update turns it on, and its TTL is pinned to automatic because a
  proxied record requires it. A **direct** spec expresses neither, so
  `recordMatches` ignores both and an update carries the *existing* values
  across. `ttlAuto` and unproxied are **create-time defaults only** — applying
  them on an update would flip the traffic path of a running service and
  overwrite a TTL a human set, and since neither field is compared or printed,
  the plan the operator approved would not have mentioned either. A direct
  service that wants proxying is a spec field somebody adds.
- **`DNSConfig` is not `cloudflare.Config`.** Same package, same API client, two
  credentials sets: the zone id must stay out of the Access connector's
  readiness check, or `--to cloudflare` goes offline for every deployment that
  hasn't set `PURSER_CF_ZONE_ID`. An unconfigured DNS provisioner reports
  `spinup.ErrUnavailable` — one step of a spin-up, not a broken connector.
- **Don't assume every Cloudflare route sends the `{success, errors}`
  envelope, or sends a body at all.** `do()` decodes `success` into a `*bool`:
  absent plus a 2xx is success, and only an actual `false` is a failure. A plain
  bool made the zero value mean failure, which would report a deletion that
  *happened* as one that didn't — PRSR-17's lie backwards. An **empty** body is
  the same question one step out and has to be answered *before* the decode,
  since `json.Unmarshal` fails on `""` and would take the error branch: a 204
  would report as a failure through the door the `*bool` cannot cover.
  Both survived because the DNS delete is the first `DELETE` this client ever
  sent, and because the fake wrapped that response in an envelope — the second
  one was still there after the first was fixed, and it is the first test with
  the fake writing nothing. **When a fake models the shape you assumed, the
  suite asserts your model rather than the API.** PRSR-29/30 share this client:
  check each new route's real response instead of the fake's.
- **Absence is spelled per-product, so each resource type owns its own
  predicate.** `dnsRecordNotFound` lives in `dns.go`, not in the shared client,
  and is named for what it answers about: 81044 is *DNS's* code and means
  nothing to an Access application or a tunnel route. A general-looking
  `notFound()` in `client.go` would be reached for by the two provisioners that
  share the file and would answer `false` for ever — safe, but silently wrong.
  `client.go` exposes `errorCode(err)` and stops there.
  The code decides it and **a bare 404 is not enough**. `Teardown` reads the
  predicate as "already gone", so treating any 404 as absence risks reporting a
  deletion that never happened — and the next run reads the row as removed,
  which makes it silent and permanent, where the opposite error is a noisy retry
  on a record that was already gone. The asymmetry is the whole argument; it does
  not depend on knowing every 404 the API can emit. Only 81044 is listed, and
  only codes actually observed in a response may be added — a guessed one turns
  an unrelated failure into a reported deletion.
- **An Access application is updated by merge, never by rebuild.** Cloudflare
  refuses `PATCH` on `/access/apps/{id}`, so an update is a full-replacement
  `PUT`: whatever the body omits is deleted. `access.go` therefore holds the live
  application as a **map**, not a struct — `encoding/json` drops unknown keys on
  decode, so a struct round-trip would silently send back an object missing every
  field nobody modelled, including ones an operator set in the dashboard. On a
  gated app the field that goes is `policies`, and an update meaning to fix a
  logo would delete the rule that gates the service, which then stays up, keeps
  resolving, and admits everyone. Only spec-owned keys are written and only
  `id`/`uid`/`aud`/`created_at`/`updated_at` are stripped.
  **`policies` is appended to, never assigned** — that was the same bug by a
  second route, and worse for being invisible: an app already admitting the
  members group reports only its rotted logo as drift, so the plan says "fix a
  logo" while the apply deletes whoever else was allowed (a service token for an
  uptime monitor, a second group). The spec says this service is gated by the
  members group; it does not say the group is the only thing that may reach it,
  and the difference is somebody else's access. A policy list that cannot be
  read — Cloudflare is documented to return bare references for apps whose
  policies are managed separately — is left untouched and reported as a note,
  for the same reason an unfetchable logo is.
  **A policy's `id` is the load-bearing field, and which half of the body counts
  depends on `reusable`** (PRSR-40, measured 2026-08-26 on a disposable app and a
  disposable reusable policy shared by two disposable apps). A `reusable: true`
  policy in an application write is a **reference**: Cloudflare reads the id and
  ignores everything else. The probe sent one back with `name` rewritten and
  `decision` flipped to `deny`; the write returned 200 echoing the policy's real
  name and decision, the standalone policy's `updated_at` did not move, and the
  second app sharing it was untouched. So echoing the estate's `Standard` policy
  back cannot edit the gate on the six services that share it — the outcome
  PRSR-40 was filed to rule out is structurally impossible. The corollary is the
  thing to protect: **strip a policy's `id` and it stops being a reference**, so
  the app gets gated by a fresh private copy that no longer tracks the shared
  group, with nothing erroring and the plan still saying "fix a logo". The lever
  is `livePolicies` and the append through it — *not* `serverOwned`, which
  already lists `id` and is applied only to the top-level application map,
  never walking into the policy objects inside it. What invites the mistake is
  symmetry: a carried policy really does arrive with server-assigned
  `created_at`, `updated_at` and `uid`, so the obvious tidy-up is a policy-level
  strip modelled on `serverOwned`, and `id` goes in with them because at the
  callsite it looks like the same kind of field.
  `TestEnsure_AReusablePolicyIsCarriedByReferenceNotRewritten` is the guard, and
  it pins the pre-existing policy **by id** on both arms: counting the list is
  not enough, since a body holding only `membersPolicy` satisfies a count and is
  precisely the assign-instead-of-append failure.
  A `reusable: false` policy is the opposite — its body **is** honoured (the same
  probe flipped one to `deny` and the read-back confirmed it), so a gated update
  is a real write of that policy's content, safe only because `Ensure` takes its
  own fresh read immediately before building the body.
- **An application that serves a *path* is not that hostname's application**
  (PRSR-41). `domainHost` strips the path on purpose, so a bookmark's
  `https://argosy…/` compares equal to a self_hosted app's bare hostname — right
  for "which hostname is this app about", wrong for "is this the app this spec
  manages", and `findApp` answered the second question with the first by taking
  the first match. Two applications serve `switchyard.zerogravity.industries`:
  the service, and a path-scoped one on `/v1/external/github` whose only policy
  is **`decision: bypass`** — no Access authentication at all, correct for a
  webhook that authenticates by HMAC and safe *only* because it is confined to
  that path. The bypass sorts first, so it won; and since `desiredApp` writes
  `domain` from the spec, an `--apply` would have rewritten it to the bare
  hostname, **widening an unauthenticated bypass from one path to the whole
  service** while the real application sat untouched. The plan did describe it —
  that was the defence being relied on — as `update`, which is not a line that
  invites suspicion on a service you meant to update.
  So the match is `servesWholeHost` and nothing less: exactly one whole-host app
  is this spec's; **none** is a create, which is correct rather than merely safe
  because Access matches the more specific path first, so a hostname-wide gate in
  front of a narrower bypass is the shape this estate already runs (and the plan
  names the path-scoped applications it is landing in front of, because "correct
  because of a rule you have to know" is worth a line); **more than one** is
  `spinup.ErrRefused` naming them, which is `pickCandidate`'s reasoning exactly
  and `refused` rather than `unknown` because the read succeeded.
  **It reads all three spellings of what an application fronts** — `domain`,
  `destinations[].uri` and `self_hosted_domains`. This package already models all
  three (the bypass carries the path in each; `desiredApp` deletes the latter two
  by name on a bookmark conversion), so reading a subset is an enumeration gap
  rather than a judgement call. All seven live apps agree across all three
  (checked 2026-08-26, bypass included), which is an observation about today and
  not a guarantee — and this predicate gates a full-replacement PUT.
  **A path-carrying spelling disqualifies only when nothing names the host
  whole** — asked **per field**, not over the two lists pooled. The naive "any
  same-host path means not ours" is wrong the other way and worse: an app with
  `domain: H` and destinations `[H, H/admin]` really does front the whole host,
  so calling it somebody else's reports `create` and `--apply` stands up a
  **second** whole-host application while the real one sits untouched and
  unrecorded — the duplicate `listApps` exists to prevent. "Access prefers the
  more specific path" does not rescue that: it only helps when the loser is
  narrower, and there both are hostname-wide.
  Pooling the lists inverts the answer in exactly the state the third field was
  added for: a path-only `destinations` excused by a bare host in
  `self_hosted_domains` reads as ours, while `destinations` — the successor field
  — says it covers a path and nothing else. And that pair is the *only* state in
  which reading the second list changes anything, since while the fields agree
  `domain` alone decides; reading it in a way that does not survive the
  disagreement is reading it for nothing.
  **And when they disagree the answer is neither yes nor no — it is
  `spinup.ErrRefused`.** Which spelling is true depends on the field Access
  honours, which is what Purser does not know, and both answers are expensively
  wrong: "not ours" reports `create` and `--apply` stands up a **duplicate**
  whole-host application that nothing afterwards reports (the next run sees one
  whole-host app — ours — so the two-candidate refusal never fires), while "ours"
  writes the spec onto an application that may cover only a path and then lets
  DNS publish in front of it. The refusal is correct *without* knowing which
  field wins, which is what makes it an answer rather than a deferral, and
  `findApp` checks it before counting candidates — an application nobody can
  classify is exactly the one that might or might not belong in that count.
  **Selection is still on `domain`, deliberately.** `appsOn` picks candidates by
  it, so the three-field rule can disqualify an application and never discover
  one. `domain` is the only one of the three that every application has — a
  bookmark carries neither list — so one filter serves both shapes only by
  reading it, and it is what Cloudflare's dashboard shows the application as
  being about. The other two are a safety check on a candidate, not a wider net.
  **A third answer means every consumer has to decide about it.**
  `servesWholeHost` returns `hostWhole` / `hostPath` / `hostAmbiguous`, and the
  two callers want opposite things from the last one. `findApp` refuses, because
  both concrete answers are expensively wrong there. `confirmGone` must **error**,
  and tested `== hostWhole` — so ambiguous fell through into `hostPath`'s bucket
  ("was never this spec's") and the teardown reported success. That is the one
  place in the file where the undecidable answer must not resolve to absent:
  `hostPath` earns its pass because Purser *knows* the application was never this
  spec's, while `hostAmbiguous` is the state where it knows nothing, which is
  `Teardown`'s third documented outcome. PRSR-34's walk will read a `nil` there as
  licence to drop the `service_resource` row, after which the remaining
  application is tracked by nothing. So `!= hostPath`, not `== hostWhole` —
  erroring is the safe direction here in a way it is not in `findApp`, because a
  wrong error costs a noisy retry and a wrong `nil` costs a removal recorded over
  a live gate.
  **`confirmGone` asks about the id, then about the hostname**, and needs both.
  Narrowing `findApp` broke it: "nothing serves this hostname" stopped meaning
  "our application is gone" the moment a path-scoped remnant of *our own* app
  could hide from a hostname-shaped re-read — a failed DELETE would then report
  success over a live gate, which is the revoked-not-recorded lie. But an id
  check alone is not enough either, because a 404 against a recorded
  `external_id` is a wrong record rather than absent access: if our id is gone
  and a *whole-host* application still serves the name, the service is still
  gated. A remaining **path-scoped** app is neither, and is not an error — it was
  never this spec's.
  `desiredApp` still writes `domain` on an update. The power to widen came from
  *which application was selected*, not from writing the field, and that is what
  moved; base is now guaranteed whole-host, so the assignment is a no-op except
  on a type change, where a bookmark's scheme-carrying domain genuinely differs.
  Converting to a bookmark drops `destinations` and `self_hosted_domains`, which
  a live bookmark does not carry.
- **A logo has three outcomes, not two, and none of them fails the step.**
  Cloudflare stores any `logo_url` without validating it and the launcher falls
  back to the service's initials, so a wrong URL is indistinguishable from an
  unset one — one of six live apps had a working icon before this axis.
  `checkLogo` fetches it **as the sessionless public** (an Access-gated asset
  answers `200 text/html`, which a status-only check would pass) and returns
  `logoOK`, `logoBroken` — a definite non-image answer — or `logoUnknown` — a
  transport failure or 5xx, so **change nothing**. Collapsing
  unknown into broken clears a working icon every time a CDN blinks, and on
  `Inspect` it is a note rather than drift, because an update here is that
  full-replacement PUT. Never fatal either way: a gated app is a DNS
  prerequisite, so refusing it would leave a service unpublished over an icon.
- **A logo is a ref with three states, and only one of them removes an icon**
  (PRSR-37). `ServiceSpec.Logo` is `placard` | `none` | an https URL, defaulted
  to `placard` in `Normalized` — so an omitted `--logo` means *resolve it*, not
  *clear it*. That inverts the trap PRSR-38 found live: `resolveLogo("")` used to
  return `""` ("clearing is intended, not a fallback") and `desiredApp` writes
  the empty string rather than omitting the key, so a forgotten flag composed
  into an `--apply` that stripped a working tile — reported then as `update`,
  "has a logo (…), spec sets none". Clearing is still expressible and still
  correct; it now takes a keyword somebody typed.
  Defaulted in `Normalized` rather than at each surface for the reason `Mode`,
  `Access` and `Tunnel` are trimmed there: the CLI and the HTTP API must not
  disagree about what an omitted field means.
  **The plan must still name a clearing before it happens** — preview-by-default
  is the last thing between `--logo none` and a deleted icon — and the three
  answers stay distinguishable, which is what PRSR-38 asked for: "the spec asked
  for no icon" is drift, while "Placard has no mark for this slug" and "Placard
  could not be asked" are *notes* that change nothing. Collapsing the last two
  into the first would clear icons estate-wide on a registry blip; collapsing
  them into drift would have the plan promise a deletion `--apply` will not do.
  Pinned by `TestArgosy_AnOmittedLogoNoLongerClearsTheLiveOne`,
  `TestArgosy_LogoNoneStillClearsTheLiveOne` and
  `TestEnsure_AnUnresolvableLogoLeavesTheLiveOneAlone`.
- **`logoBroken` and `logoUnknown` differ about what to *write*, and not about
  what to keep.** Both leave an existing icon alone; only the note differs. The
  broken case used to discard `current` and return `""`, which `desiredApp`
  writes as an empty `logo_url` and PRSR-40 confirmed live really does remove the
  icon — so a spec naming a mark that 404s **cleared the working one already on
  the tile**, and the plan had said `update`, naming the new URL. The keep is
  conditioned on `current != want`, which is the whole of what it was reasoned
  about: where the live icon *is* the dead URL the spec asks for, `current` is
  not a tile to protect but the thing just proved broken, and writing it back
  makes the drift permanent — the plan reporting "set correctly but not a
  servable image" for ever while every `--apply` PUTs a gated application for a
  change that cannot happen. Clearing there converges. A spec asking
  for a different icon did not ask for this one to be removed, and losing a
  working tile is strictly worse than not gaining the new one. Only
  `spinup.LogoNone` clears, which is the same rule the ref itself encodes.
  **The plan fetches the candidate too**, in `logoDiff`'s `live != want` branch,
  and **unconditionally**. Without it the preview reports drift on a URL it never
  checked, so plan and apply disagree by construction — a preview is the first
  half of an apply, not a guess at it. Guarding the fetch on `live != ""` looks
  like an optimisation for the create path and is not one: `logoDiff` runs only
  when the application exists, so an empty `live` is an existing application with
  no icon yet — seven of the ten PRSR-38 audited. That case destroys nothing but
  never converges, printing `update` for a URL the apply then declines to write,
  for ever, with each `--apply` doing a full-replacement PUT for a change that
  will not happen. Reachable rather than
  theoretical: Placard reports a file `in_repo` from the repo's contents, and
  jsDelivr 404ing it (propagation lag after a rename) is a definite non-image
  answer, so it lands on `logoBroken` and not `logoUnknown`. Pinned by
  `TestEnsure_ABrokenSpecLogoDoesNotClearTheWorkingLiveOne`.
- **Placard picks the URL; the write-time fetch decides** (PRSR-37).
  `internal/placard` resolves a mark by service key, behind a one-method
  `cloudflare.LogoResolver` so the Access provisioner does not import a second
  upstream to decorate a tile. It answers *pick*, never *verify*: Placard's own
  per-file `check` is a periodic monitor carrying a `checked_at`, so a stale
  green is exactly how the silent failure returns, and `checkLogo` still runs at
  the moment of writing.
  Two live traps it exists for. **A working `logo_url` is not a correct one** —
  argosy's old URL answered `200 image/png` and was the 3.6:1 wordmark, which
  letterboxes to a sliver in a square tile, and no fetch check can tell that from
  the tile mark; Placard is the thing that knows which asset is which. And
  **`state: "missing"` still carries a fully populated `canonical_url`**, because
  Placard reports where a file *would* live — so reading the URL without reading
  the state writes a guaranteed 404, which is the exact condition switchyard's
  tile was in.
- **The Access teardown confirms absence by reading, not by an error code.**
  `dnsRecordNotFound` works because 81044 was observed; there is no observed
  Access equivalent, and the rule above forbids guessing one. So a failed delete
  re-reads the hostname: nothing there means it really is gone, something there
  means the gate is still up and the record is wrong, and a failed re-read means
  unverifiable — which is never absent. That is stronger than a code test, not a
  substitute for one: it asserts what `Teardown` actually claims, and it also
  catches a delete that failed at the transport after Cloudflare applied it.
- **Those two premises are observed now, and one of them was wrong** (PRSR-38,
  probed 2026-08-26). The sentence that stood here said not to harden either
  into fact without a probe; this is the probe. The DNS delete does **not**
  answer with a bare `{"result":{"id":…}}` — it carries the full envelope,
  `{"result":{"id":…},"success":true,"errors":[],"messages":[]}`. So `do()`'s
  `*bool` is defensive rather than load-bearing *on this route*; keep it, since
  it costs nothing and the route that omits an envelope is exactly the one
  nobody will think to check. The 404 half is confirmed in the shape that
  matters: deleting a well-formed id that is not there answers **404 with code
  81044**, which is the observation `dnsRecordNotFound` was waiting for and the
  licence to keep that code listed. What was *not* reproduced is a "could not
  route" 404 — a **malformed** id answers **405, code 10405** ("Method not
  allowed for this authentication scheme"), which is neither absence nor a clean
  failure, and is handled correctly for precisely the reason the predicate keys
  on the code rather than the status: 10405 is not 81044, so it can never read
  as "already gone". That asymmetry now has a live counter-example behind it
  instead of an argument.
- **Cloudflare appends the zone to a name it doesn't recognise.** A hostname from
  another domain becomes `svc.example.org.zerogravity.industries` with no error
  anywhere. `ServiceSpec` can't catch it (it validates the shape of a hostname,
  not which zone the token points at), so the *created* record's name is checked
  against what was asked for, and a mismatch deletes it. Cleanup runs on the
  create path only: on an update the record predates Purser, and removing it
  would destroy something nobody asked to have removed.
  **The reason this file used to give for checking it afterwards was wrong**
  (PRSR-38). It said a Zone→DNS→Edit token "can't read the zone object to find
  out first"; the production token answers `GET /zones` with exactly
  `["zerogravity.industries"]`. A pre-flight against the token's own zone is
  available and would refuse an out-of-zone hostname in the *plan*, before
  anything exists to delete — PRSR-39. The create-then-delete stays as the
  backstop either way: it catches a normalisation surprise a pre-flight cannot
  predict, and it is the half that has actually been reasoned about.
- **A refusal is not a failed read** (PRSR-31). `StepUnknown` covers a read that
  did not complete — re-running is the whole fix. `StepRefused` covers a read
  that *succeeded* and came back with something no provisioner may write to: a
  tunnel whose catch-all is not last, or one that is locally managed. Both
  decline to act and they want opposite things from an operator, so the
  difference is a **status**, not the `Err` string — putting it there was the
  `TaskFailed` + `Pending bool` shape PRSR-21 removed from the person axis, where
  every consumer bucketing by status had to remember a second field existed.
  Provisioners signal it with `spinup.ErrRefused`, matched via
  `spinup.IsRefused`, exactly as they already signal `ErrUnavailable`. The
  sentinel rides on `documentShape` and `checkTunnelSource` and **not** on
  `terminalIndex`: that helper's other callers (`assertWritable`, `confirmRoute`,
  `Teardown`) ask it about a document *this run just built or read back*, where a
  failure is our own arithmetic or a concurrent writer — a breakage, not
  something upstream to go and fix. Keep `unavailable` distinct from both: it is
  Purser's own missing credential, fixed here rather than there.
  **The line runs through the DNS provisioner too, and both sides of it are
  live.** `pickCandidate` — several records answer for this name and none is the
  spec's — is `refused`: the read succeeded, and its own message says to resolve
  it in the dashboard. `records()`'s full-page refusal stays `unknown`: the
  filter narrowed nothing, so the answer really was not read and a re-run is the
  fix. Getting that backwards prints "could not be read … re-run" at somebody
  whose zone needs editing, which is the sentence this split exists to stop.
- **A warning is its own field, printed once per surface.** Trouble *around* a
  step that succeeded travels as `Resource.Warning` → `StepFinding.Warning`, not
  as a clause appended to `Detail`. The two are read by different people: Detail
  describes this resource and a surface may truncate it, while the tunnel's
  concurrent-write note says another service's ingress route may have been
  dropped from the shared document — which a caller must be able to find without
  pattern-matching a substring out of a description.
  It is **not logged from `Ensure`**, because `log` and the CLI's own summary
  both write to stderr and one event then read as two; it *is* logged by the API
  layer, where there is no double-print to avoid and a caller that ignores the
  field would leave no trace of it anywhere — the same argument `handleSpinup`
  already makes for `applied-not-recorded`. Dropping the log outright (PRSR-31's
  first attempt) fixed the CLI and blinded `purser serve`: a 200 with sensible
  counts reveals nothing, and the damage is somebody *else's* outage.
  `Teardown` still logs its own — nothing carries a `Resource` back from there.
- **"Nothing pending" is not "the edge is up."** `Result.Pending()` counts only
  what `--apply` would act on, and deliberately excludes `unavailable`,
  `refused`, `unknown` and `blocked` — none of them is fixed by the flag. So
  neither `pending` nor `changed` is a verdict: an apply against an unconfigured
  deployment reports `0` and `0`, which is identical to an edge that was already
  correct. `Result.NeedsAttention()` is the verdict, and it lives on the result
  rather than in a renderer so the CLI's exit code and the HTTP response cannot
  drift about what counts as fine. Reading the pending count as success reported
  "the edge already matches this spec" over a plan whose DNS step was
  unavailable — a hostname that does not resolve, signed off as a service that
  is up. Found by running the binary, not by reading it, which is the argument
  for running it.
- **A spec is an argument, not configuration.** `provision-service` takes flags
  and there is no spec file: config here is env-only by house convention, and a
  spec is written rarely and read carefully — the same reason a tunnelled spec
  must name its tunnel rather than defaulting to one. `spinupRegistry` registers
  all three provisioners *unconditionally*, unlike `buildRegistry` on the person
  axis: each reports the env var it is missing, which a generic `Unavailable`
  stand-in could not, and registering them keeps the kind known so a step can
  never be quietly absent from a report.
- **`Normalized()` trims every field a surface might pad, and does it *first*.**
  The order is load-bearing, not tidy: `Mode` decides whether `Upstream` is
  case-folded, so trimming it afterwards left `"direct "` skipping that fold —
  and since `validHostname` assumes what `Normalized` produced, an upstream like
  `Origin.Example.Com` was then refused outright on one spelling of the spec and
  accepted on the other. Anything added here goes above its readers.
  `Mode`, `Access` and `Tunnel` are compared against constants, so a
  stray space is a refusal — and it was a refusal on the HTTP path only, because
  the CLI trimmed them itself and `Normalized` did not. One surface accepting
  `"direct "` while the other answers `unknown mode "direct "` is a difference
  nobody would guess at. Trimmed, but deliberately **not** case-folded: `Key` and
  `Hostname` are identity keys where two spellings would split a service in half,
  whereas these three are a closed set, and accepting `"Direct"` would widen what
  a spec may say without anybody deciding it should.
- **`spinup.ErrTunnelUnconfigured` exists so the HTTP surface need not match on
  a message.** `Validate` has already rejected refs that are not names, so it is
  only ever a legal ref nobody has wired (`dev`, until PRSR-33) — the caller's to
  fix, so a 400 rather than a 500. It must never fall back to prod: that would
  have a dev spin-up rewrite the production tunnel's shared ingress document.

## Testing

- `make test` — unit tests (fake store + fake connectors for the orchestrator;
  httptest for the Switchyard/Cloudflare connectors and the DNS provisioner —
  `dns_test.go` runs a stateful fake zone, so create → read back → delete is a
  test rather than a manual probe; in-process SMTP for delivery). DB-backed store
  tests skip unless `PURSER_TEST_DATABASE_URL` is set.
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

Purser also carries the SWY-192 build-identity contract as of v0.15.0
(PRSR-32): `/healthz` reports `version` + `sha`, so it probes as a corroborated
deploy on the delivery matrix instead of `no_version`. Verified against the
published images rather than only in tests — `:0.15.0` answers
`"version":"0.15.0"` with the full 40-char sha, both matching that image's
`org.opencontainers.image.version` / `.revision` labels exactly, and `:latest`
answers `"version":"dev"` rather than leaking the literal `latest` its own label
carries.

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

PRSR-28 and PRSR-29 landed two of the three provisioners.
`cloudflare.DNSProvisioner` does both record shapes, idempotent by
lookup-then-write, recording `dns_record_id` so `Teardown` targets an id rather
than re-resolving a name. `cloudflare.AccessProvisioner` does the gated
`self_hosted` app and the `bookmark` tile, verifies a logo before writing it,
and merges rather than replaces on update.

PRSR-27 landed the foundation: `internal/spinup` (spec, `ServiceProvisioner`,
registry, `Ensure` orchestrator) and `service_resource` (migration 0007). It
settled the two questions the connectors were waiting on — preview by default,
and the tunnel as a spec field — and it deliberately stops short of the three
provisioners and the CLI. `Teardown` is on the interface, because the resource
table exists to give it concrete ids to target, but nothing orchestrates a
teardown yet: its ordering and its "is this hostname still someone's?" question
are **PRSR-34**. That has a key rather than a comment on a closed ticket for a
reason — twice now this project has lost the remaining half of a piece of work
by closing the ticket that described it (see PRSR-25, below) — so don't delete
that interface method as dead code; it is waiting on a walk, not unused.

PRSR-30 completes the three: `TunnelProvisioner` in `tunnel.go`, the ingress
route. It is the step with the blast radius — one shared document per tunnel
holding every hostname on it — so the invariants above are where the work went:
insert before the terminal catch-all and assert it stayed last, write every key
back verbatim, and guard the read-modify-write with a mutex, a fresh read inside
it, a read-back, and a version check for the writer the read-back cannot see.
The read path refuses the same documents the write path does, which review
caught as the half that actually reaches production. It also refuses a tunnel
that is not remotely managed, because the configuration this endpoint returns is
then not the one being served, and every other guard in the file is about *who
else wrote it* rather than whether it is live.

PRSR-31 wired all three into `setup()` and gave the axis its entry points:
`purser provision-service` (flags, plans by default, `--apply` acts) and `POST
/v1/spinups`. It settled the two questions PRSR-30's review deferred — `refused`
split from `unknown`, and the concurrent-write warning reported once — and it
added `spinup_argosy_test.go`, which runs all three real provisioners through
the real orchestrator against an already-up edge and asserts three no-ops with
zero upstream writes — against a fake, which is the caveat **PRSR-38 retired on
2026-08-26 by running the binary against the real Cloudflare API for the first
time in this project's history.** The three runs the ticket asked for all pass:
plan → `adopt`/`adopt`/`skipped`, `--apply` → two rows and *zero* upstream
calls, re-plan → `ok`/`ok`/`skipped` with `Pending()==0`. "An adopt is a row and
nothing else" is now checked against Cloudflare's own `modified_on` and
`updated_at`, which did not move, rather than against Purser's self-report.

**What the first run actually found is the part worth keeping.** The Access step
came back `update`, not `adopt`: the live tile carries a logo and the ticket's
spec named none, so `--apply` would have cleared a working icon. The cause was
`liveBookmark()` — a fixture built to match the spec rather than the API, so the
suite asserted `adopt` on a shape Cloudflare does not return. That is the
"a fake models the shape you assumed" trap reached through fixture *data*
instead of a wire shape, and it is the argument for this ticket existing at all:
five green tests could not see it, and one live plan could. The fixture is now
the observed response, `tags` and `policies` included.

PRSR-38 was **adopt-only by construction** — the plan wrote nothing, `--apply`
produced two rows and zero upstream calls, the re-run was `ok`/`ok` — so what it
established was a claim about the **read** paths: `Inspect`, the matchers, the
reconcile logic and the statuses they produce. Its own probes were **raw API
calls**, confirming Cloudflare's behaviour rather than the bodies `desiredApp`
and `putConfig` construct; confirming that a PUT bumps a version is not
confirming that the document we would PUT is one Cloudflare accepts.

**PRSR-40 closed that gap for Access, and only for Access** (2026-08-26). It ran
the real `AccessProvisioner` — not curl — against the live API on disposable
hostnames, so what went on the wire was `desiredApp`'s own body. **Every write
verb in `access.go` has now executed**, on both application shapes:

- **gated create** — the inline policy is accepted, and comes back with a fresh
  id, `reusable: false`, `precedence: 1`. So a new gated service gets its own
  private gate rather than joining the estate's shared `Standard`.
- **gated update, both branches** — carry-through when the group is already
  admitted, and the **append** when it is not (the foreign policy survived at
  precedence 1, the members policy landed at 2).
- **bookmark create and update** — a materially different body, not a variant of
  the gated one: `type: bookmark`, a `domain` carrying a **scheme**, and
  `policies` **assigned** to `[]any{}` rather than appended to. Cloudflare accepts
  the empty list and echoes it back as `[]`, `tags` survives, and the response
  key set is far smaller than a `self_hosted` app's — no `destinations`,
  `self_hosted_domains` or `session_duration`. Added after review pointed out the
  residual caveat below was still exhaustive and still omitted this one.
- **the logo clear** — `logo_url: ""` really does remove it (the key is absent on
  read-back), and the plan named the clearing first.
- **`Teardown`** — including a second teardown of an already-gone app, where
  `confirmGone`'s re-read correctly reported success rather than an error.

The estate was byte-identical to its pre-probe snapshot afterwards, `updated_at`
included; all three disposable apps and the disposable policy were deleted.

So the caveat is now genuinely narrower: **the DNS write verbs and the tunnel
write verbs have never executed against Cloudflare.** `DNSProvisioner`'s
create/update/delete and `TunnelProvisioner.putConfig` are still fake-only —
PRSR-38 probed the DNS delete with raw curl, which is not the same as running
the provisioner. The tunnel is the one that matters most and is the worst
candidate for a casual probe, since its write is a read-modify-write of a
document holding every other service's routes.

**PRSR-37** made the launcher icon a resolved fact rather than a typed path.
`ServiceSpec.Logo` is a ref (`placard` | `none` | url) defaulting to `placard`,
`internal/placard` resolves a mark by service key behind a one-method
`LogoResolver`, and every answer other than "here is the mark" leaves the tile
alone. Verified by running the binary against live Placard and live Cloudflare in
plan mode: switchyard resolves to
`…/placard@main/switchyard/switchyard-mark-light.png` against a stored URL that
is a live 404, argosy reports the repoint from its 3.6:1 wordmark to the tile
mark with both URLs named, and chronicle — which Placard has never heard of —
reports `adopt` with a note rather than drift or a failure. It is also what
turned up PRSR-41.

**PRSR-33** wires the `dev` tunnel ref
that PRSR-27 left resolving to a refusal, and now also owns whether "dev" is one
spec field driving both the tunnel and Placard's `-dev` mark. **PRSR-34** holds
the `Teardown` walk — its orchestration, that is; the Access provisioner's own
`Teardown` has now run live (PRSR-40), so what is left there is the ordering and
the "is this hostname still someone's?" question. **PRSR-39** is the zone
pre-flight that PRSR-38's probe showed was available all along. **PRSR-36**,
**PRSR-37** and **PRSR-40** are all closed.

**PRSR-41 was found by running the binary** — the third time on this axis that
has caught something reading could not (PRSR-31's outcome line, PRSR-38's
fixture, and now this), and it is fixed. See the invariant above.

**PRSR-36** asked for two Cloudflare response shapes that PRSR-28's fixes were
reasoning about from the published schema rather than from a response anybody
had seen. **PRSR-38 observed both** — see the DNS bullet above: the delete
*does* send the envelope (so that premise was wrong and the code was defensively
right anyway), 81044 arrives with a 404 as hoped, and a malformed id answers
405/10405 rather than any "could not route" 404. The reason it blocked PRSR-34
is discharged with it: `Teardown` is where a wrong absence-predicate starts
marking rows removed for records that still resolve, and the predicate is now
keyed on a code somebody has actually seen.

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
