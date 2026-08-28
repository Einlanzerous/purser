# Purser — architecture & design

Purser is the Construct's cross-service provisioning/invite service. One action
invites a person into multiple ecosystem services at once, mints starter
credentials, grants Cloudflare Access SSO, and hands back a copy-pasteable
credential block (or emails it).

This is the canonical design reference (the "design doc — link TBD" from
IDEA-14).

## Problem

The Construct's apps do not share a user model:

- **Switchyard** (Jira replacement) has its own `users` table + API tokens, and
  logs people in via Cloudflare Access SSO (email → `users.email`).
- **Argosy** (media server) has accounts (email + password, post-ARGY-159) →
  profiles → device bearer tokens, on the *direct* (non-tunnelled) path — its own
  login, no Cloudflare Access.
- **Lyceum** (ebooks) has per-user accounts (LYCM-804) and sits behind the same
  Access gate as Switchyard, matching the verified email to its own user record.

Onboarding a person means touching each system by hand, plus adding their email
to the Cloudflare Access gate. Purser collapses that into one command.

## Shape

A single static Go binary that is both a CLI and a thin HTTP API, a sibling to
the other construct-server Go services. Shared Postgres 16 (`purser` DB + role),
runs on `construct_net` behind Tailscale/Cloudflare.

```
purser invite --name "Ada Lovelace" --email ada@example.com \
    --to switchyard,cloudflare --deliver copypaste
```

### Connectors

Each service hides its own user model behind a `Connector`:

```go
Provision(ctx, Input) (Result, error)             // create/ensure the account, return a one-time secret
Reconcile(ctx, Input) (ReconcileResult, error)    // READ-ONLY: does this person already have access?
Deprovision(ctx, Input) error                     // REVOKE access — not delete (PRSR-17)
```

**`Reconcile` must never mutate.** No create, no mint, no rotate, no revoke — it
answers "what does this person already have?" and nothing else. That constraint
is the whole point: a version that repairs as a side effect cannot be used to
audit, because running it destroys the drift it exists to report. It is what
makes the audit safe to run against real people at any time.

A connector whose upstream has no lookup endpoint returns
`ErrReconcileUnsupported` rather than inferring absence. Reporting "no" for a
question you cannot answer would claim people lack access they demonstrably
have — manufacturing exactly the drift the audit exists to find. Nothing is in
that state today (all four have lookups), but the contract keeps a future
connector honest.

| Connector    | What it does                                                        | Status |
|--------------|---------------------------------------------------------------------|--------|
| `switchyard` | `POST /v1/users` (email set) → `POST /v1/users/{id}/tokens`         | ✅ live |
| `cloudflare` | Adds the email to a shared Access group (email-OTP SSO gate)         | ✅ live when a CF API token is configured; otherwise prints the manual dashboard step |
| `lyceum`     | `POST /admin/users` (email set) → single-use 7-day `lyc_` invite      | ✅ live when an owner session token is configured and `LYCEUM_AUTH=true`; otherwise registers Unavailable |
| `argosy`     | `POST /api/v1/admin/accounts` (email login) → one-time password       | ✅ live when the provisioning token matches argosy's `ARGOSY_PROVISION_TOKEN`; otherwise registers Unavailable |

Switchyard is the account inside the app; Cloudflare Access is the SSO gate in
front of it. A typical human invite targets **both**: Cloudflare grants the
email-OTP login, Switchyard creates the account it maps to.

### The two identities, and why email is the join key

Switchyard's SSO endpoint (`POST /v1/auth/sso/cloudflare`, shipped in SWY-161)
verifies the `Cf-Access-Jwt-Assertion` JWT and matches the verified email to
`users.email` — it **never auto-provisions**. So the Switchyard user must exist
*with the email set* before SSO login works, and the email must be allowed
through Cloudflare Access. Purser does exactly these two things.

### Cloudflare Access reality (SERV-17 / SERV-25)

The Zero Gravity edge uses Cloudflare's **built-in email one-time-PIN IdP** with
**Allow-by-email** policies, team domain
`zero-gravity-industries.cloudflareaccess.com`.

Grants are **programmatic as of PRSR-4**: an Access-scoped API token and the
shared group `zerogravity-members` exist, and every tunnelled app's policy
references that group — so one grant covers all of them and Purser has a single
place to add or remove people. `purser invite --to cloudflare` adds the email to
the group idempotently. With the token absent the connector still degrades to
printing the exact manual dashboard step, which is what keeps a partial config
safe rather than half-provisioning someone.

## Data model

`migrations/0001_init.up.sql`:

- `person` — who we invite (email unique when present; the SSO join key).
- `service` — target systems, seeded from the connector registry on boot.
- `account` — durable "person P has access to service S"; **unique (person,
  service)** — the idempotency key. Secrets are never stored plaintext, only a
  sha256 hash (`secret_ref` is reserved for a future vault). `status` is
  `active | stale | deprovisioned`; `stale` was added by `0002` (see
  [Audit & reconcile](#audit--reconcile)).
- `invite` — one provisioning run for a person across services.
- `provision_task` — one service's slice of an invite; tracks attempts +
  last_error so a re-run retries only what failed. `status` is `pending |
  running | succeeded | skipped | failed | unavailable`; `unavailable` was added
  by `0004` (see [Task status](#task-status)).

The spin-up axis has one table of its own, added by `0007` and joined to none of
the above (see [Service spin-up](#service-spin-up-epic-prsr-22)):

- `service_resource` — an edge object Purser created for a service, and the
  coordinates to find it again: `kind` (`tunnel_route | access_app |
  dns_record`), `external_id`, and the `parent_id` of the zone/account/tunnel it
  lives in. **Unique on (lower(hostname), kind)** — this axis's idempotency key,
  case-folded because hostnames are. `service_key` is *not* a foreign key to
  `service`: a service being stood up need never be an invite target.

## Idempotency

Re-running the same invite is safe and **retries only failed services**: a
service with an active `account` row (upstream id present) is *skipped* — no
duplicate upstream user, no fresh secret — while a previously-failed service is
retried. Per-service connector failures never abort the whole invite; they are
recorded and surfaced in the operator note — a field of its own, separate from
the credential block, because only the block is ever emailed (PRSR-19).

## Task status

`provision_task.status` distinguishes two ways of not being provisioned:

| status        | meaning                                              | retry helps        |
| ------------- | ---------------------------------------------------- | ------------------ |
| `failed`      | the connector tried and something broke               | yes                |
| `unavailable` | the connector never tried — `connector.ErrPending`    | only after a fix   |

`ErrPending` means a connector is registered but not in a position to provision:
its token isn't configured, or its upstream has no provisioning API yet. Both
states are retryable and neither writes an `account` row, which is why they were
one state — `failed` carrying a `Pending bool` — until PRSR-21.

The reason to split them is that the difference is *all* they are ever consulted
for. The operator note groups them under separate headings ("worth a retry once
fixed" against "a retry changes nothing until these are configured"), the CLI
marks them `✗` and `…`, and the HTTP outcome reports them as distinct `status`
values. Every one of those consumers had to either special-case the bool or
accidentally get the right answer by lumping them together; a status they can
switch on costs the next consumer nothing.

It is *not* called `pending`: that name is taken by the queued state ("created,
not yet run"), and one word meaning both "hasn't run yet" and "can't be run" is
the collision that put this on a bool in the first place. `unavailable` matches
`connector.Unavailable`, which is already the term for a registered-but-
unconfigured connector.

Idempotency is unaffected — the re-run skip keys on an **active account**, never
on task status — so an unavailable service is retried by the next invite exactly
as a failed one is. Migration `0004` adds the status and backfills history:
`failed` rows whose `last_error` begins with `ErrPending`'s message become
`unavailable`, and rows that can't be positively identified stay `failed` rather
than being guessed at.

## Onboarding bundles

The dominant invite is not "here's access to app X" but "welcome to the family."
A **bundle** is a named service set, so that's one flag or none:

| Bundle | Services | For |
|---|---|---|
| `media` (default) | `cloudflare`, `lyceum`, `argosy` | Household members who just want to watch and read |
| `all` | `cloudflare`, `switchyard`, `lyceum`, `argosy` | People who'd use Switchyard too |

Cloudflare is in both because Lyceum sits behind the Access gate: the app account
without the Access entry strands the person at the edge. `media` is the default
deliberately — it's the smaller grant, so an unqualified invite can't hand out
Switchyard by accident.

**A bundle is only a named list**, expanded into the existing per-service
orchestration. It introduces no new provisioning path and no new idempotency
rules, which is what makes overlapping bundles, a bundle overlapping an explicit
`--to`, and re-inviting someone who already has half the set all safe by
construction. Giving bundles their own provisioning path would forfeit that.

A bundle grants `*:user` — Switchyard's **project membership** role, not the
instance role, which stays at its preset. Those are independent axes: a bundle
widens what you can see, it does not escalate privilege.

Bundles are env-configured (`PURSER_BUNDLE_*`). Defining any bundle replaces the
built-ins wholesale rather than merging, so a partial override can't silently
inherit half a default set.

## Audit & reconcile

`account` rows record what **Purser provisioned** — not who actually has access.
Those diverge whenever someone is set up by hand or an upstream account is
deleted. `purser audit` compares the two through each connector's read-only
`Reconcile`; `purser reconcile` applies the same findings. One code path, so the
dry run is exactly what the repair does.

| situation | action |
|---|---|
| upstream has it, Purser doesn't | write an `account` row (no secret — Purser never learned theirs) |
| Purser has it, upstream doesn't | mark the row `stale` |

`stale` matters more than it looks. Idempotency keys on **active**, so a row left
active with no upstream account means the orchestrator skips that person forever
and no invite can ever fix them. Marking it stale re-arms provisioning.

Three guardrails, because an audit that damages records is worse than no audit:

- **Never treat unverifiable as absent.** `UpstreamUnknown` is deliberately
  distinct from `UpstreamNo` throughout.
- **A failed connector call never marks anything stale** — a transient outage
  would otherwise wipe out everyone's access records.
- **Rows already in agreement are never rewritten**, preserving `secret_hash` on
  genuinely Purser-provisioned accounts.

Why this is not just a re-invite: re-inviting someone who already has Switchyard
mints them a *second* API token. Reconcile mints nothing and contacts nobody.

### Getting people into the roster (PRSR-16)

The audit walks the `person` table, so it can only ask about people Purser
already knows. Anyone onboarded outside Purser is simply omitted — and the
report says "0 errors" while being silently incomplete, which is the failure
mode the audit exists to prevent.

`purser person add --name … --email …` writes that row and nothing else: no
`account` rows, no connector calls, no credentials. It makes someone
*auditable* without claiming to have provisioned them, which is what neither
`invite` (it would try to create accounts they may already hold) nor hand-written
SQL (a typo'd email mints a duplicate identity that reconcile then dutifully
populates) can do. `--audit` follows the add with the same read-only preview
`purser audit --email …` prints.

An email that already exists is a **conflict, not an edit**. The store used to
carry an `UpsertPerson` doing `ON CONFLICT … DO UPDATE SET name`, so any command
taking `--name` renamed whoever held that address without ever saying so. `add`
uses `InsertPersonIfAbsent`, which never touches an occupied row, and `--rename` is
the explicit opt-in — served by a single `UPDATE … RETURNING` that reports the
name it replaced, so the command cannot announce a rename the database didn't
perform. The same rule covers `--type`: omitted, it leaves an existing person
alone; supplied and disagreeing, it is refused rather than silently dropped.

`invite` is held to the same rule *for names* (PRSR-20). It used to call
`UpsertPerson` directly, so a mistyped `--name` on a re-invite — an ordinary
thing to do, since invites are idempotent per (person × service) — renamed
whoever held that address. It now keeps the stored name and reports the
disagreement as `Result.NameConflict`: a warning on stderr, `name_conflict` over
HTTP, and a server-side log line so a caller that ignores the field still leaves
a record. Renaming stays exclusively with `person add --rename`.

`UpsertPerson` itself is **gone** (PRSR-23). PRSR-20 moved the addressed path off
it; requiring `--email` below removed its last caller, and it is deleted rather
than left for the next one. A `person` row is now created only by
`InsertPersonIfAbsent` and renamed only by `RenamePerson`, so the rule that one
command owns renames is checkable by grepping the store rather than by trusting
each new call site.

**The two delivery paths resolve that signal differently.** Copy-paste warns:
the operator is the gate, nothing has left the building, and failing the
provision over a name mismatch punishes the wrong action. Email **refuses**,
before writing an invite row or provisioning anything. A name mismatch is the
only evidence that a mistyped `--email` landed on a *different existing person*,
and email delivery would mail that person working credentials before the
operator could read a warning about it. Refusing costs a re-run; not refusing
costs a credential sent to the wrong human. Over HTTP that refusal is a `409`
carrying the disagreeing names, not a generic 500.

The comparison ignores differences nobody can see — surrounding and repeated
whitespace — but not case: a doubled space would otherwise block delivery
forever, while `ada lovelace` against `Ada Lovelace` is visible in the warning
and worth an operator's decision.

Unlike `person add`, `invite` does **not** compare `--type`. It never asserts
what kind of identity this is; it passes `human` only as the default for a row
that may not exist yet, and an existing row keeps its own type untouched.

Email is required by **both** commands, though the schema allows the column to be
null: it is the conflict target that makes them idempotent and the key the audit
looks people up by, so a row without one could be added twice and reconciled
never. `person add` has required it since PRSR-16, including for `--type agent`.

`invite` requires it as of PRSR-23, which is a behaviour change to a shipped
command and worth the reasoning. An emailless invite took a separate branch, and
because `person_email_key` is partial on `email IS NOT NULL`, the row it wrote
could not collide with anything — including the row the previous run of the same
invite wrote. Each run therefore recorded a *new person id*, and that id is what
every downstream guarantee keys on: `UNIQUE(person_id, service_id)`, which is how
a re-run skips what is already provisioned, and `InviteRef`
(`purser-{person.ID}-{connector}`), which is stable per (person × service) so an
upstream `Idempotency-Key` dedupes across re-runs. So the one command that
promises "re-running retries only what failed" did the opposite: a fresh upstream
user and a fresh secret per run, plus one more person for the audit to walk each
time. The alternative to requiring the address would be inventing a second
identity key (a `--handle` with its own unique index); nothing needed one, and
keying on `--name` instead is worse than the bug — two people legitimately share
a name, and collapsing them is less recoverable than duplicating one.

The rule is in the schema too, as migration 0005: `CHECK (email IS NOT NULL)`,
declared `NOT VALID`. A guard in Go only guards callers that come through Go,
which is the lesson of 0003 — a check in `AddPerson` would have left `invite`
minting the duplicates instead. `NOT VALID` is doing work here rather than
deferring it: the constraint binds every insert and update from now on, while
rows the old path already wrote stay exactly as they are. `SET NOT NULL` is what
could not be deployed, since it would have to rewrite or reject those rows at
boot and there is nothing to backfill them *with*.

Those rows are stranded, which is worth saying plainly. No command can address a
person who has no address: `person add` and `invite` both key on the email, so
either creates a *second* row rather than finding them, and `RenamePerson`
matches `lower(email)` and so never matches NULL. That was equally true before
this change — `UpsertPerson` given an address didn't match a NULL-email row
either — so nothing here regresses, but nothing here repairs it. Repair means
hand SQL, which 0005 deliberately still permits (`UPDATE person SET email = …`
satisfies the constraint). Once none remain,
`ALTER TABLE person VALIDATE CONSTRAINT person_email_required` adopts the rest.

The audit is safe to run with them in the roster, but that took a fix of its own.
All four connectors now refuse an emailless `Reconcile`, so the audit files the
question as an error and writes nothing. Switchyard did not: `findUser` falls
back to matching on display name when there is no email, so `reconcile --all`
would have recorded such a person against whoever upstream happens to share their
name — or, on a miss, marked a real account stale and re-armed provisioning for a
second token. Requiring the address everywhere else is precisely what left that
fallback reachable only from the audit, where guessing is least acceptable.

**Emails are unique case-insensitively** (migration 0003). They weren't
originally: `person_email_key` indexed the bare column while every store lookup
matched `lower(email)`. A row entered by hand as `Ada@Example.com` therefore did
not collide with the lowercased address the code writes, and the insert added
a *second* person for the same human — precisely the duplicate identity this
command exists to prevent, and one the audit would then walk and populate twice.
That had to be fixed in the schema: a guard in `AddPerson` would have left
`invite` minting the duplicates instead.

Deploying 0003 requires that no such pair already exists, since the index cannot
be built over one. It fails loudly and transactionally if so — the migration
carries the query that finds them.

### Reading the roster back (PRSR-24)

`person add` writes people in; `person list` and `person show` read them out.
Until they existed the only way to ask what Purser held was psql — which needs
schema knowledge, bypasses every invariant the CLI enforces, and sits one typo
away from an `UPDATE` against live provisioning records. Provisioning one person
by matching what two others already had took four hand-written joins across
`person`, `account`, `service` and `invite`, for a question Purser is the system
of record for.

**`audit` is not this.** It answers a different question — records *versus
upstream* — so it needs a connector call per (person × service) and is only as
available as the connectors are. Asking who is on the roster shouldn't require
every upstream service to be reachable, or cost a reconcile sweep. So the roster
reads local tables and stops — `person`, `account` and `service`, plus `invite`
for `show`'s history: neither `Roster` nor `PersonDetail` so much as references
the connector registry, and a test asserts both `Provision` and `Reconcile` stay
at zero calls.

**No secret reaches either command, structurally.** They read
`store.AccountRecord` — an account joined to its service, with no `secret_hash`
and no `secret_ref` field, from a query that selects neither column. Credentials
are shown exactly once, at invite time; a roster view that re-surfaced even the
hash would weaken that, and `--json` serializes whatever the struct holds. Making
it unrepresentable is what keeps the property answerable by reading one type,
rather than by auditing every renderer and DTO that will ever wrap it.

**The default filter is never silent.** `list` shows active accounts, because a
stale row is exactly what someone does *not* have and listing it under "services"
would misreport access. But an omission that can't be seen produces the same
class of wrong answer: `--to lyceum` returning nobody must mean "nobody has
Lyceum", not "nobody has Lyceum any more". So the hidden count comes back with
the result and the CLI prints it. The service filter also selects on the same
accounts the output shows — filtering on one set and displaying another would
return people with an empty services column and no way to tell why.

`show` withholds nothing by contrast: it is the single-person view, where a
deprovisioned or stale row is the interesting part, and each carries its status
so none of them reads as access still held.

A person with **no** accounts is still on the roster. That row is what `person
add` writes, so an account-driven listing would be blind to exactly the people
that command exists to record.

Switchyard-side detail — instance role, project memberships — also had to come
from the Switchyard database by hand, and is *not* here. Surfacing it belongs to
the switchyard connector's `Reconcile`, not to a local roster command.

## Offboarding (PRSR-17)

`invite` grants access across the stack in one command. Until PRSR-17 nothing
took it back: `Deprovision` was declared on the interface and had no caller, so
removing someone meant four manual deletes plus hand-editing `account` rows.
`model.AccountDeprovisioned` was a status the schema permitted and nothing wrote.

`purser offboard --email …` is the other half. Its unit of work is the `account`
row, not the connector list — it acts on what Purser recorded this person as
holding, so a service they never had is never called. That is the difference from
`audit`, which asks every connector about everyone; here a needless call is a
needless mutation.

### Every default is inverted

This is the one genuinely destructive operation, so it takes the opposite
defaults from the provisioning path:

| | `invite` | `offboard` |
|---|---|---|
| default | acts | previews; `--apply` acts |
| scope | one person | one person, and no bulk mode exists |
| `unavailable` | benign — nothing was granted | non-zero exit — access is still live |

A dry run makes **no connector call at all**, not merely no mutating one. Dry run
and apply share a code path, so the preview is exactly what the apply does — the
same property `audit`/`reconcile` have, for the same reason.

The asymmetry on the first row is the whole argument: granting access twice is
wasteful, and revoking the wrong person is not undone by running the command
again.

### Revoke, not delete

Where an upstream distinguishes them, `Deprovision` takes away the way in and
leaves the account standing. Revoking is reversible, it preserves authorship, and
"they can't get in" is the actual requirement.

| service | operation | note |
|---|---|---|
| `switchyard` | revoke every live API token | the user and their tickets stay; deleting would orphan authored work |
| `cloudflare` | remove the email from the Access group | already a pure access grant |
| `lyceum` | `DELETE /admin/users/{id}` | **the exception** — its admin API has no disable |
| `argosy` | none | no delete, disable, or token invalidation exists |

Lyceum is documented as deleting rather than being quietly folded in, because the
interface's gentler wording would otherwise imply a reversibility it hasn't got.

Argosy returns `ErrRevokeUnavailable`, bucketed exactly like `ErrPending`. That
is PRSR-21 earning its keep: a three-of-four offboard reports "three revoked, one
still open and needing a hand" instead of either looking broken or — far worse —
claiming success. It is also why this shipped without waiting on an ARGY endpoint.

The separate sentinel exists only so the *sentence* is right: `ErrPending` reads
"provisioning not yet available", which is nonsense on an offboard, and its text
cannot be reworded because migration `0004`'s backfill matches it as a literal
prefix. Callers bucket with `connector.IsUnavailable`, which accepts either.

A connector that cannot revoke also says so *without being called*, via the
optional `RevokeChecker` interface. The offboard preview needs that: Argosy's
`Deprovision` is an unconditional refusal and so is every `Unavailable` stub, so
a dry run reporting "revoke" for them would promise what `--apply` declines —
breaking the one property that makes previewing worth doing. Each such connector's
`Deprovision` delegates to its own `CanDeprovision`, so the two cannot drift.

### Two things that must not collapse

**A revoke that didn't happen must never be recorded as one.** Only a successful
`Deprovision` marks the row `deprovisioned`; `failed` and `unavailable` leave it
`active` so the next run retries. The error message scrolls away; the column is
read forever after by the audit, by `person show`, and by the next invite's
idempotency skip. Recording a revoke that didn't happen tells all three that
access was removed while it is live.

The inverse is its own state, `revoked-not-recorded`: the connector succeeded and
the database write didn't. Access *is* gone and Purser's records are wrong, which
is the opposite of a failure and needs the opposite advice — so they are not one
status, and a write failure mid-run no longer aborts the report. Aborting used to
discard every finding accumulated so far, leaving the operator one error line and
no idea what had already been taken away.

The same rule reaches into the connectors. A 404 against the `external_id` Purser
*recorded* means the record is wrong, not that access is absent; both Switchyard
and Lyceum fall back to the email lookup and revoke what that finds. Reporting
success there would mark the row `deprovisioned` while the real account stays
live, and the next run would skip it — silent and permanent, where a failure at
least announces itself. Lyceum carries a second trap: `Provision`'s 409 branch
records the *email* in `external_id`, and its DELETE handler `ParseInt`s the path
segment, so anyone who already had a Lyceum account when they were invited was
permanently un-offboardable until a non-numeric id was made to resolve by lookup.

**Revoking Switchyard does not close Switchyard's door.** Its tokens gate the
API. The sign-in is gated by the *Cloudflare Access group*, which is a different
connector — so an offboard scoped to `switchyard` alone leaves a working login
behind while reading as finished. The CLI says so explicitly when it detects that
shape. The fix is not to merge the two connectors: they are genuinely separate
grants, and the invite path depends on that separation too.

### The record is kept

The `account` row is marked, never deleted. Deleting it would destroy the history
the audit exists to read, and would silently re-arm provisioning — the
orchestrator's skip keys on an *active* account, so a missing row and a
deprovisioned one mean opposite things to the next invite. Marking it is also
what finally makes `deprovisioned` a state something writes, and `person list`
hides it by default while reporting the count.

Marking alone turned out not to be enough. `status` plus `updated_at` is erased
by the next `UpsertAccount` — which sets active and bumps the timestamp — so a
re-invite, an ordinary thing to do, destroyed the answer to "when was it taken
away". Migration `0006` adds `deprovisioned_at`, written on the transition into
`deprovisioned` and never cleared. A re-provisioned account keeps the date it was
last revoked, because that is history and history doesn't stop having happened;
read `status` for the current state and this for the last revocation. `person
show` renders it as a REVOKED column.

## Delivery

The credential block is plain text (pastes cleanly into any chat platform).
`--deliver copypaste` (default) returns it for the operator to paste;
`--deliver email` sends it over SMTP to the person. One-time secrets appear once
and are never retrievable afterward.

An invite that provisioned *nothing* sends no email at all: there is nothing to
tell the recipient, and a greeting on its own announces access that wasn't
granted. The invite stays undelivered, the operator is told why, and the next run
retries only what failed. The gate asks whether anything succeeded rather than
whether anything failed, which is how an all-`unavailable` invite lands on the
same answer without being special-cased: a connector nobody configured granted
exactly as much access as one that broke.

The block **leads with Cloudflare's App Launcher** — the one page listing every
Access-gated app a person can reach — and then gives per-service detail. That
page is Cloudflare's, rendered at the team domain, so Purser gains no public
surface and stays internal-only.

The link is conditional: the launcher lists *Access* apps, so it appears only for
invites that actually granted Access. An Argosy-only invitee would otherwise be
sent to an empty page, and a *failed* Access grant doesn't count either — a link
that rejects them reads as a broken invite. Neither does an `unavailable` one:
the recipient can't act on why a door is shut, only that it is. Direct-path apps
deliver their secrets inline in the same block, which is precisely what the
launcher cannot carry through.

## Security notes

- Secrets are delivered once and persisted only as a hash.
- The HTTP API is bearer-token protected (`PURSER_API_TOKEN`); it also relies on
  `construct_net`/Tailscale isolation.
- Purser holds an admin-capable Switchyard token and (when configured) a
  Cloudflare Access-edit API token — treat the `.env` as sensitive.

## Phasing

Tracked under the **PRSR** project (graduated from SERV-33 / IDEA-14).

**Delivered:**

- **Phase 0+1:** spine — schema, connector interface, Switchyard connector,
  idempotent invites, credential block. Extended per the owner's ask with the
  Cloudflare Access connector and email/copy-paste delivery.
- **All four connectors live** — `switchyard`, `cloudflare`, `lyceum` (PRSR-6 /
  PRSR-10), `argosy` (PRSR-13). Each token-gated one registers Unavailable when
  its credential is unset.
- **Permissions:** project memberships at invite time (PRSR-7), explicit
  `--instance-role` + `--scopes` (PRSR-9).
- **Onboarding bundles** (PRSR-12) and the launcher-led credential block.
- **Audit & reconcile** (PRSR-15), which retired 15 (person × service) pairs of
  real drift with zero upstream mutation (PRSR-14).
- **`purser person add`** (PRSR-16) — a roster entry point that provisions
  nothing, so people onboarded outside Purser stop being invisible to the audit.
- **Identity guarantees on `invite`** — it no longer renames silently (PRSR-20),
  and no longer accepts a person with no email (PRSR-23). Those were the two
  ways an ordinary re-invite could stop meaning what it says.
- **`purser person list` / `person show`** (PRSR-24) — the roster read back out
  of local records, so asking what someone already holds no longer means psql
  against live provisioning tables.
- **`purser offboard`** (PRSR-17) — the revoke half Purser never had. Three of
  four connectors revoke; Argosy reports `unavailable` until it has an endpoint;
  `deprovisioned` is finally a status something writes.

**Open:**

- **PRSR-18 — run the audit on a schedule.** It exists and nothing triggers it,
  so drift will reaccumulate and be found the same way it was last time: by
  accident.
- **Argosy cannot be offboarded.** Its admin API has create and lookup but no
  delete, disable, or token-invalidation. `offboard` reports it `unavailable`
  rather than skipping it; an ARGY ticket is pending, same shape as ARGY-163.
- **PRSR-25 — a dedicated Switchyard provisioning token.** Closed as done on the
  board, while its own last comment records the token mint still blocked on
  SERV-49 (no `users:manage` on the assistant's MCP token). The `purser` agent
  user exists; whether `PURSER_SWITCHYARD_TOKEN` is still the instance bootstrap
  token is worth checking rather than asserting.
- **PRSR-22 — service spin-up** (below). Its prerequisites, its foundation, all
  three provisioners — DNS (PRSR-28), Access (PRSR-29), the tunnel ingress route
  (PRSR-30) — and the `provision-service` command that runs them (PRSR-31) have
  landed. What remains is pointing it at the live API for the first time.

## Service spin-up (epic PRSR-22)

Everything above provisions **people into existing services** (the person ×
service axis). This is the other one: standing up the Cloudflare edge for a
service in one command — DNS record, tunnel ingress route, Access application —
so bringing `interlock`, `cook_book`, `centrifuge` online stops being a manual
dashboard operation.

It is a **different axis**, keyed on hostname rather than on (person × service),
and it does not extend the person-shaped `Connector` — bending one interface to
carry both would mean widening `Input` until neither axis's fields mean
anything. It lives in `internal/spinup`, which imports `internal/model` and
nothing else of ours.

### What has landed

**Prerequisites.** PRSR-11 provisioned the token scopes (**Zone → DNS → Edit**
scoped to the one zone, **Account → Cloudflare Tunnel → Edit**, both probed
against the live API) and shipped `PURSER_CF_ZONE_ID` / `PURSER_CF_TUNNEL_ID`.
PRSR-26 answered the tunnel question: it is **remotely-managed**
(`source: "cloudflare"`), so ingress routes are settable over the API and no
tunnel migration is needed.

**Foundation (PRSR-27).** `ServiceSpec`, the `ServiceProvisioner` interface, its
registry, and the `Ensure` orchestrator, plus `service_resource` (migration
0007). Two decisions were settled here so the three provisioners cannot
disagree:

- **`Ensure` previews by default; `--apply` acts** — `offboard`'s posture, not
  `invite`'s. Creation is additive and idempotent, which argues for acting, but
  the tunnel step is a read-modify-write of one document holding every *other*
  service's routes. Preview and apply are one code path: a single read-only
  `Inspect` per step decides the plan, and `--apply` is that decision plus the
  write.
- **The tunnel is a spec field, not a global** (PRSR-33) — the account has two
  healthy tunnels, and a dev instance of a service is the same shape pointed at
  a different one. Specs name a ref (`prod` | `dev`); `TunnelSet` resolves it to
  an id once per run, before any step, so the ingress route and the DNS record
  cannot describe different tunnels. Only `prod` is wired.

**The ingress route (PRSR-30).** `TunnelProvisioner`, in
`internal/connectors/cloudflare/tunnel.go` — grouped with the Access connector by
*upstream* rather than by axis, over the shared transport in `client.go` that
PRSR-28's DNS provisioner and PRSR-29's Access provisioner use too. PRSR-26 is
what made it ordinary: the tunnel is remotely-managed, so `GET`/`PUT
/accounts/{acct}/cfd_tunnel/{id}/configurations` is the whole mechanism.

It is the step with the blast radius, and it has two distinct failure modes that
the other two provisioners do not:

- **The document is shared.** There is no per-hostname write — one document per
  tunnel holds every hostname on it — so a stale read doesn't damage this
  service's route, it deletes somebody else's. Four guards, covering different
  windows: a mutex (`docMu`, the same shape as the Access group's `groupMu`); a
  fresh read taken *inside* it rather than reuse of the plan's `Inspect`, which
  ran outside; every unmodelled key written back byte-for-byte, because a PUT
  replaces the document and what it omits the tunnel loses; and a check that the
  configuration's `version` moved by exactly one. That last one is the only guard
  that can see a *second process* writing in between — confirming our own route
  always passes, since our write necessarily contains everything our own read
  did. A version jump warns on a step that succeeded; what may have been lost is
  another service's route.
- **The catch-all must stay last.** cloudflared requires a terminal rule matching
  everything. A rule appended after it never matches and **nothing errors** — the
  route just doesn't work — so a new rule is inserted *before* it, and the
  position is asserted on the built document and again on the read-back. A
  document that doesn't have that shape (no catch-all, or one that isn't last and
  has therefore already killed the rules behind it) is refused rather than
  rewritten on a guess.

  **The read path refuses the same documents**, which is the half that reaches
  production: `Inspect` is the only call a dry run makes and every step's status
  comes from it, so a rule behind a catch-all reported as a working route gives
  `ok`/`adopt` — both `inPlace()` — and the DNS step then publishes a hostname in
  front of a tunnel that will 404 it. `findRoute` therefore stops where
  cloudflared stops, and every answer other than *reachable and already correct*
  is gated on `documentShape`: what `--apply` will refuse must preview as
  `unknown`, never as `create` or `update`. A hostname that is genuinely served
  on a document broken elsewhere still reports in place, with the malformation
  named.

  **And the catch-all is not the only thing that shadows.** A wildcard hostname
  rule takes everything under it, and a holding page on `*.zerogravity.industries`
  is a deliberate configuration rather than a broken one — so the walk stops at
  the first rule that would take the hostname, of which the terminal catch-all is
  the empty-pattern case. A new route goes in front of that rule rather than in
  front of the terminal one (most-specific-first, cloudflared's own idiom), and a
  wildcard is never adopted as Purser's own route: `Teardown` would then delete a
  rule standing in front of the whole zone.

  `hostnameTakes` mirrors cloudflared's matcher rather than approximating it —
  `""`/`"*"` match everything, otherwise exact, otherwise only a leading `*.` is
  a wildcard and the suffix it tests keeps its dot. Read from `ingress/rule.go`
  and `ingress/ingress.go`, because the general glob it replaced was wrong in
  both directions: `wiki.*` is a literal upstream and stopped a walk it should
  not have, and the apex question (`*.zone` does *not* take `zone`) decides
  whether an apex route is published or dead.

- **Is this the document the tunnel serves?** `getConfig` refuses any tunnel
  whose `source` is not `cloudflare`. A locally-managed tunnel runs a YAML file
  on the origin machine, so the remote configuration is not in force — and none
  of the four guards above can see that, since each is about *who else wrote this
  document*. Unchecked, the whole sequence succeeds silently and DNS publishes a
  hostname the tunnel has never heard of. An absent `source` is refused as well.
  PRSR-26 verified `construct-server` by hand, once; nothing re-asserted it at
  run time, and PRSR-33 adds a tunnel whose mode nobody has checked.

**The command (PRSR-31).** `purser provision-service`, plus `POST /v1/spinups`,
plus the wiring: `setup()` now builds a `spinup.Service` beside the invite one,
registers all three provisioners, and resolves `prod` from
`PURSER_CF_TUNNEL_ID`. The spec is flags rather than a file — config here is
env-only by house convention, and a spec is an *argument*, written rarely and
read carefully, which is the same reason a tunnelled spec must name its tunnel
instead of defaulting to one.

All three provisioners are registered even with no credentials, which is the
opposite of what `buildRegistry` does on the person axis and is deliberate: each
one reports `spinup.ErrUnavailable` naming the variable it is missing, which a
generic `Unavailable` stand-in could not. It also keeps the *kind* known — the
registry panics on a kind outside `KindOrder` precisely so a step can never be
quietly absent from a report.

Two questions PRSR-30's review deferred here were settled rather than carried:

- **`refused` is now a status of its own**, split from `unknown`. Both decline
  to act; they differ in what the operator should do next. A failed read says
  "run it again"; a document nobody may write to says "go and fix the tunnel,
  because running it again will print this until you do". The difference used to
  live in the `Err` string, which is a second field a reader has to know to
  consult — the exact shape PRSR-21 removed from the person axis when it deleted
  `TaskFailed` + `Pending bool`. Provisioners signal it with a
  `spinup.ErrRefused` sentinel, the way they already signal `ErrUnavailable`; it
  rides on `documentShape` and `checkTunnelSource`, not on `terminalIndex`,
  because `terminalIndex`'s other callers ask it about a document *this run just
  built* — a failure there is our own arithmetic or a concurrent writer, which is
  a breakage rather than something to go and fix upstream.
- **The concurrent-write warning is reported once.** It was logged *and* appended
  to the step's `Detail`; since `log` writes to stdout beside the report, one
  event read as two on the CLI. It is the one message whose whole value is being
  believed when it fires, and a warning printed twice is one a reader starts
  discounting. `Teardown` still logs its own — nothing carries a detail back from
  there.

One thing running it caught that reading it did not: the closing summary line
said "the edge already matches this spec" whenever `Pending()` was zero — and
`Pending()` deliberately excludes `unavailable`, `refused`, `unknown` and
`blocked`, because `--apply` fixes none of them. So a plan whose DNS step was
unavailable, on a hostname that does not resolve at all, signed off as a service
that was up.

### The tunnelled/direct split reaches three steps

The epic described this as a DNS-and-tunnel distinction. It also reaches Access,
which is what makes it a spec-level decision rather than a per-connector one:

| step | tunnelled | direct |
|---|---|---|
| tunnel ingress | append a hostname rule | **skipped entirely** |
| Access | `self_hosted` app + policy → `zerogravity-members` | **`bookmark` app**, no policy |
| DNS | proxied CNAME → `<tunnel-id>.cfargotunnel.com` | A/AAAA (or CNAME) → the static endpoint |

A bookmark is a *different application type*, not a gated app minus its policy,
so a spec that could only emit `self_hosted` could not describe the direct path
at all — and Argosy, the pilot, is on it.

**DNS is applied last**, which is the order of that table. It is the step that
makes the hostname live; the other two are inert until something resolves.
Publishing the record first leaves a tunnelled service answering 502 until its
route lands, and a service meant to be gated reachable *ungated* until its
Access app exists — and only one of those two announces itself.

Ordering alone doesn't close that window, though: it only helps if the earlier
step actually landed. So DNS also **depends** on the steps in front of it, and is
held (`blocked`) when a prerequisite failed, was unavailable, or couldn't be
read. A *bookmark* Access app is not a prerequisite — it is a launcher tile in
front of a service with its own login, so its absence costs an icon, not a gate.
Blocking withholds changes, not the report: a record that already matches is
already published, so it is still reported and still adopted.

**The DNS record (PRSR-28).** `cloudflare.DNSProvisioner`, in the same package
as the Access connector because it speaks to the same API through the same
transport — `do()` already took a free-form path, so `/zones/{zone}/…` works as
well as `/accounts/{acct}/…`, and it moved to a small `client` type two callers
can hold. It does **not** share `cloudflare.Config`: the zone coordinates stay
out of the Access connector's readiness check, or `--to cloudflare` goes offline
for every deployment that hasn't set them.

What it decides, and why:

- **The two shapes.** Tunnelled is a **proxied** CNAME to
  `<tunnel-id>.cfargotunnel.com` — the orange cloud is not a preference
  (SERV-45), since the tunnel is reachable only from Cloudflare's edge and an
  unproxied record resolves to something nothing can connect to. Direct is an
  A/AAAA/CNAME chosen from the upstream's own shape, created **DNS-only**.
- **Proxying is compared only when the spec requires it.** A direct spec says
  nothing about the orange cloud, so neither does the match — and an update on
  that path preserves whatever is set. Reporting it as drift would have `--apply`
  flip the traffic path of a service that is already running.
- **Ambiguity is refused, not guessed.** Several records answer for a name and
  none matches the spec → the step is `unknown` and `--apply` does not act. A
  dual-stack A + AAAA is *not* ambiguous: the spec claims one record, not the
  name.
- **A full page of results is refused too.** The name filter goes to the API and
  the exact match is re-applied locally, but a hundred records back means the
  filter narrowed nothing — and reading page one of the zone as "nothing here"
  creates a duplicate. Unreadable is not absent, here as everywhere.
- **A hostname outside the zone is refused before the write, and caught again
  after it.** Cloudflare treats such a name as *relative* and silently appends
  the zone, producing `svc.example.org.zerogravity.industries`. `ServiceSpec`
  can't catch it — it validates the shape of a hostname, not which zone the token
  points at.

  This paragraph used to justify checking *only afterwards* by saying a token
  scoped to Zone → DNS → Edit "cannot read the zone object to find out
  beforehand". That is false: PRSR-38 found the production token answers
  `GET /zones/{zone_id}` with `name: zerogravity.industries, status: active` —
  the object read `preflight` actually makes, the list endpoint answering as well
  — and **PRSR-39** built the pre-flight on it. `preflight` resolves `PURSER_CF_ZONE_ID` to the zone name
  and refuses an out-of-zone hostname on both `Inspect` and `Ensure`, ahead of the
  record lookup, so the operator sees it in the plan and nothing is ever written.
  It is `refused` rather than `unknown` — the read succeeded, so re-running
  changes nothing and only the refusal's message names the actual fix.

  The create-then-delete stays behind it, because the two answer different
  questions: the pre-flight asks what the *spec* said, `wrongName` asks what
  *Cloudflare did*. It is therefore still the guard for a normalisation surprise
  a pre-flight cannot predict, still the half exercised live (PRSR-42 ran it end
  to end), and the only guard at all in the one state the pre-flight waves
  through — a zone that could not be read, which is never evidence about the
  hostname. Usually the same failure takes the record lookup with it and the step
  reports `unknown` on its own, but that is a tendency rather than a property: a
  failure specific to `GET /zones/{id}` leaves the pre-flight silently inert, and
  the backstop rather than any self-healing is what makes that acceptable. Only
  successful zone reads are memoised (one timeout must not disable the check for
  a whole `purser serve`), a 200 naming no zone counts as a failed read rather
  than as a zone everything is inside, and the zone name is derived from the id
  rather than configured beside it, since two settings that can disagree merely
  relocate the mismatch.
- **Teardown targets the recorded id** and refuses without one, confirms the id
  still names this hostname before deleting, and treats an already-absent record
  as success — where "absent" means Cloudflare's record code (81044) and *not* a
  bare 404. A teardown that reported a deletion it never performed would leave
  the row marked removed for a record that still resolves, which is silent and
  survives a re-run; the opposite error is a visible retry.
- **The envelope is not assumed to be universal.** `do()` reads an absent
  `success` field on a 2xx as success rather than as failure, because a plain
  bool made the zero value mean failure. PRSR-29 and PRSR-30 share the client;
  check each new route's real response shape rather than the fake's — the fake
  wrapping the DNS delete in an envelope is how this got as far as review.

Two premises those rested on came from Cloudflare's published schema. **PRSR-38
probed them on 2026-08-26** and one was wrong:

| premise | observed |
|---|---|
| `DELETE /zones/{zone}/dns_records/{id}` answers with a bare `{"result":{"id":…}}` | **False.** It sends the full envelope: `{"result":{"id":…},"success":true,"errors":[],"messages":[]}` |
| a "could not route" error is a 404 | **Not reproduced.** A well-formed id that is absent gives **404 / 81044**; a *malformed* id gives **405 / 10405** |

Both fixes held under the real answer, which was the point of writing them the
safe way: the `*bool` is simply defensive on a route that does send an envelope,
and keying absence on 81044 rather than on the status is what stops the 405 from
ever reading as "already gone". Keep the `*bool` anyway — the route that omits
an envelope is the one nobody will think to re-check.

### What is left

- ~~**PRSR-38 — Argosy end to end, against the live API.**~~ **Done, 2026-08-26.**
  The axis has now contacted Cloudflare. All three runs behaved: plan →
  `adopt`/`adopt`/`skipped`, `--apply` → two rows with zero upstream calls,
  re-plan → `ok`/`ok`/`skipped`, `Pending()==0`, exit 0 throughout. The no-write
  claim is verified against Cloudflare's own `modified_on` / `updated_at`, not
  against Purser's summary of itself.

  **What that does not establish**: the exercise was adopt-only, so no write
  verb this axis owns ran against Cloudflare, and the premises below were
  settled by **raw API calls**, which confirm Cloudflare and not the bodies
  `desiredApp`/`putConfig` build. The read paths are what this run verified.
  PRSR-40 has since closed that gap for Access; see below.

  Every premise that was true only because a fixture said so is now measured:
  the tunnel `version` moves by **exactly one per content-changing PUT** (and
  not at all for an identical one); the DNS delete **does** carry the
  `{success, errors}` envelope, contradicting PRSR-36's reading of the schema;
  81044 arrives with a 404 while a malformed id answers 405/10405; a bookmark's
  live JSON carries `tags` and `policies`, neither of which this package models;
  and policies come back as **full objects with `decision`**, not bare
  references — the estate's is `reusable: true` and shared by six apps, and
  `groupPolicy` reads it correctly.

  The exercise earned its keep on the first plan, which reported `update` rather
  than `adopt`: the live tile carries a logo the fixture did not, so an `--apply`
  would have stripped a working icon. Five green tests could not see it because
  the fixture had been built to match the spec instead of the API.
- ~~**PRSR-40 — do the Access write verbs work?**~~ **Done, 2026-08-26.** It ran
  the real `AccessProvisioner` against the live API on a disposable hostname, so
  the bytes on the wire were `desiredApp`'s own rather than curl's approximation
  of them — which is precisely what PRSR-38 could not claim.

  **Every write verb in `access.go` has now executed**, on both application
  shapes: the gated **create**, the full-replacement **update** on both of its
  branches, the **bookmark** create and update, the logo **clear**, and
  **`Teardown`** (twice, the second on an already-gone app, where `confirmGone`'s
  re-read correctly reported success rather than an error). The estate was
  byte-identical to its pre-probe snapshot afterwards, `updated_at` included.

  The bookmark was added after review caught the residual caveat below still
  reading as exhaustive while omitting it — the same failure the paragraph it
  replaced had opened by admitting. It is a materially different body, not a
  variant of the gated one: `type: bookmark`, a `domain` carrying a **scheme**,
  and `policies` **assigned** to an empty list rather than appended to.
  Cloudflare accepts the empty list and echoes it back as `[]`, `tags` survives,
  and the response key set is far smaller than a `self_hosted` app's — no
  `destinations`, `self_hosted_domains` or `session_duration`.

  The question it was filed for — whether echoing a `reusable: true` policy back
  inline edits the shared object — is answered **no, structurally**:

  | policy | what an application write does with its body |
  |---|---|
  | `reusable: true` | **ignored.** Only `id` is read; it is a reference. A probe sending `name: "MUTATED BY PROBE"` and `decision: "deny"` got a 200 echoing the policy's real name and `allow`, with the standalone policy and a second app sharing it both untouched |
  | `reusable: false` | **honoured.** The same probe flipped one to `deny` and the read-back confirmed it |

  So the estate's `Standard` policy cannot be edited by one service's logo fix,
  and the field that makes that true is the `id`. Strip it and the policy stops
  being a reference: Cloudflare would mint a private copy, the app would be gated
  by something that no longer tracks the shared group, nothing would error, and
  the plan would still say "fix a logo".

  The lever is `livePolicies`, not `serverOwned` — which already lists `id`, and
  is applied only to the top-level application map rather than to the policy
  objects inside it. The invitation is symmetry: a carried policy arrives with
  server-assigned `created_at`, `updated_at` and `uid`, so the obvious tidy-up is
  a policy-level strip modelled on `serverOwned`, with `id` swept in alongside
  them. `TestEnsure_AReusablePolicyIsCarriedByReferenceNotRewritten` guards both
  arms and pins the pre-existing policy **by id**, because a length check alone
  is satisfied by a body holding only `membersPolicy` — the
  assign-instead-of-append failure that would delete the shared policy outright.

  Two smaller answers on the same trip: a policy created inline comes back
  `reusable: false` with a fresh id, so a gated service Purser stands up gets its
  own private gate rather than joining `Standard`; and `logo_url: ""` really does
  clear a logo, with the key absent from the read-back — with the plan naming the
  clearing first, which is the only thing standing between a forgotten `--logo`
  and a deleted icon.

  It also caught the PRSR-38 fixture lesson **recurring**: `liveGatedApp` was
  missing `destinations` and `eager_redirect_cookie_setting`, which every
  `self_hosted` application on the estate carries. `destinations` is the one with
  teeth — the modern spelling of what the application sits in front of.

  **Closed by PRSR-42 (2026-08-28).** `DNSProvisioner`'s create/update/delete and
  `TunnelProvisioner.putConfig` have now executed too — see below.
- ~~**PRSR-37 — resolve the launcher icon from Placard.**~~ **Done, 2026-08-26.**
  `ServiceSpec.Logo` is now a ref — `placard` | `none` | an https URL — defaulted
  to `placard` in `Normalized`, so a spec names a *service* and gets the right
  mark. `internal/placard` does the lookup behind a one-method
  `cloudflare.LogoResolver`, which keeps the Access provisioner from importing a
  second upstream to decorate a tile.

  Three answers, and only one of them writes:

  | answer | what happens |
  |---|---|
  | Placard has the mark | the URL is verified by the write-time fetch, then written |
  | Placard has no mark for the slug | a **note**; the tile is left exactly as it is |
  | Placard could not be asked (down, or unconfigured) | a **note**; the tile is left exactly as it is |
  | the spec says `none` | **drift**, named in the plan, and `--apply` clears it |

  That inverts PRSR-38's trap. An omitted `--logo` used to mean "clear it", so a
  forgotten flag stripped a working icon; it now means "resolve it", and only a
  keyword removes one. The last two rows must stay notes rather than drift — a
  plan reporting drift there would promise a deletion `--apply` will not perform,
  and treating "the registry is down" as "there is no icon" clears tiles
  estate-wide on a blip.

  Placard **picks**; it never **decides**. Its per-file `check` is a periodic
  monitor with a `checked_at`, so `checkLogo`'s sessionless fetch still runs at
  the moment of writing. Two live conditions justify the whole approach: argosy's
  old URL answered `200 image/png` and was the 3.6:1 wordmark — a *working*
  logo_url that is not a *correct* one, which no fetch check can distinguish —
  and a file Placard reports as `state: "missing"` still carries a fully
  populated `canonical_url`, so reading the URL without the state writes a
  guaranteed 404.

  Verified by running the binary against live Placard and live Cloudflare in plan
  mode, not only under `go test`: switchyard resolves to Placard's mark against a
  stored URL that is a live 404; argosy reports the repoint with both URLs named;
  chronicle, which Placard has never heard of, reports `adopt` with a note.
- ~~**PRSR-41 — `findApp` matched on hostname alone.**~~ **Fixed, 2026-08-26.**
  Two applications serve `switchyard.zerogravity.industries`: the service, and a
  path-scoped one on `/v1/external/github` whose only policy is `decision:
  bypass` — no Access authentication, correct for a webhook that authenticates by
  HMAC and safe only because it is confined to that path. `domainHost` strips the
  path, so both matched, and `findApp` took the first — the bypass. Since
  `desiredApp` writes `domain` from the spec, an `--apply` would have rewritten it
  to the bare hostname, widening an unauthenticated bypass to the whole service
  while the real application sat untouched.

  `findApp` now matches on `servesWholeHost`: one whole-host application is the
  spec's, none is a create (Access matches the more specific path first, so a
  hostname-wide gate in front of a narrower bypass is the shape this estate
  already runs — and the plan names the path-scoped applications it lands in
  front of), and more than one is `spinup.ErrRefused` naming them.

  The ticket's other two questions are settled here rather than left as residue.
  **All three spellings of what an app fronts are read** — `domain`,
  `destinations[].uri`, `self_hosted_domains` — since this package already models
  all three and the predicate gates a full-replacement PUT. All seven live apps
  agree across all three (checked, the bypass included). A path-carrying spelling
  disqualifies only when *nothing* names the host whole — asked per field rather
  than over the two lists pooled, since pooling lets a bare host in one excuse a
  path-only entry in the other, which is the exact disagreement the third field
  was added for. When the two lists genuinely disagree the answer is neither
  "ours" nor "not ours" but `spinup.ErrRefused`: which spelling is true depends on
  the field Access honours, and both guesses are expensively wrong — one stands up
  a duplicate whole-host application nothing afterwards reports, the other lets
  DNS publish in front of an application that may cover only a path. The naive "any same-host path" rule is wrong the other way and
  worse: an app fronting both `H` and `H/admin` really does front `H`, and calling
  it somebody else's would have `--apply` stand up a duplicate whole-host
  application. Candidate *selection* remains on `domain` alone — the only field
  every application has — so the extra spellings disqualify rather than discover. **`desiredApp` keeps writing `domain` on an
  update**: the power to widen came from which application was selected, not from
  writing the field, so base is now guaranteed whole-host and the assignment is a
  no-op except on a type change, where a bookmark's scheme-carrying domain
  genuinely differs. Converting to a bookmark drops `destinations` and
  `self_hosted_domains`, which a live bookmark does not carry.

  Review then caught that narrowing `findApp` had broken **`confirmGone`**, which
  borrowed its answer: "nothing serves this hostname" stopped meaning "our
  application is gone" once a path-scoped remnant of our own app could hide from a
  hostname-shaped re-read, so a failed DELETE would report success over a live
  gate. It now asks about the recorded id first, then falls back to the hostname —
  both, because a 404 against a recorded `external_id` is a wrong record rather
  than absent access. A remaining path-scoped application is neither, and does not
  block a teardown.

  Found by running the binary while verifying PRSR-37 — the third time on this
  axis that has caught something `go test` could not. Nothing was ever applied.
- ~~**PRSR-42 — the DNS and tunnel write verbs.**~~ **Done, 2026-08-28.** Every
  write verb this axis owns has now executed against Cloudflare through the
  provisioner that owns it. Nothing needed fixing; what it produced is the
  confirmation that the fakes modelled the right thing, plus one fixture
  correction and three retracted claims.

  **DNS**, on a disposable hostname: create in both shapes — a direct spec comes
  out **unproxied** with automatic TTL, a tunnelled one **proxied** (SERV-45,
  since an unproxied CNAME to `cfargotunnel.com` reaches nothing) — the no-op on
  a match, an **A → CNAME type change in place** on the same record id,
  `Teardown`, and the already-gone path. The two update invariants that had only
  ever been asserted against a fake are now measured: a direct-spec update
  carried a hand-set **TTL of 300** and a hand-set **`proxied: true`** across
  rather than resetting them to create-time defaults. The zone-append backstop
  ran end to end: `prsr42-probe.example.org` became
  `prsr42-probe.example.org.zerogravity.industries`, `wrongName` caught it,
  `removeStray` deleted it, and the zone diffed byte-identical to its snapshot.

  **The tunnel** ran on a **disposable tunnel seeded with a verbatim copy of
  production's ingress document** — same six rules, same `warp-routing`, same
  sha256 — so the bytes round-tripped were real while the blast radius stayed at
  zero. `putConfig` inserted before the terminal catch-all; `version` moved
  **1 → 2, exactly one**; `warp-routing` survived byte-for-byte; all six original
  rules returned unchanged and in order. A re-run wrote nothing. `Teardown`
  restored the document byte-identical to the seed. Then the case production
  cannot exercise: with a `*.zerogravity.industries` holding-page rule present,
  the route was inserted **before the wildcard** and the plan named it — the
  shadowing case, which without `scanRoute` lands in the dead region behind it
  and still passes its own read-back. Production's document was untouched
  throughout, `version` 5 and identical sha either side.

  **What it corrected.** Neither a create response nor a GET carries `zone_id` or
  `zone_name` on this API version, so `dnsRecord` decodes two fields that are
  always empty: `zoneOf` always takes its fallback (which is why stray removal
  works at all) and `wrongName`'s zone-naming branch cannot fire. The fake zone
  had been *supplying* both under a comment calling them "the zone fields the API
  echoes back" — the PRSR-38 trap in its inverse direction, a fixture inventing
  what the API withholds, and the third time this package has met that shape.
  `wrongName`'s doc block also still claimed a Zone→DNS→Edit token "cannot read
  the zone object itself", which PRSR-38 had already disproved and PRSR-39 has
  since built on. All three claims corrected; the fallbacks kept, since they cost
  one branch and are what make the paths work.
- **PRSR-33** — the `dev` tunnel ref, which resolves to a refusal today rather
  than falling back to prod. Adding it is one line in `tunnelSet`; the rest of
  the ticket is the dev hostname convention and whether dev apps share the prod
  Access group.
- **PRSR-34** — orchestrating `Teardown`. The interface method exists and the
  resource table exists to give it concrete ids to target rather than a hostname
  to guess from, but nothing walks it. Its ordering is almost certainly the
  inverse of the table above — remove the DNS record first, so the hostname stops
  resolving, before pulling the route or the gate — and the open question is
  whether a recorded hostname is still that service's to remove.
