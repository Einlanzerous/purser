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
Deprovision(ctx, Input) error                     // remove access (no caller — PRSR-17)
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

**Open:**

- **PRSR-17 — Deprovision.** Nothing calls it. Cloudflare's implementation works
  and is unreachable; the other three connectors return not-implemented; there is
  no CLI verb and no orchestrator path. Purser can onboard across the stack in one
  command and cannot remove anyone. This is why `stale` means "re-arm
  provisioning" and cannot mean "revoke" — and why `model.AccountDeprovisioned`
  is a state the schema permits, the roster commands render, and nothing writes.
- **PRSR-18 — run the audit on a schedule.** It exists and nothing triggers it,
  so drift will reaccumulate and be found the same way it was last time: by
  accident.
- **PRSR-25 — a dedicated Switchyard provisioning token.** Purser authenticates
  as the instance bootstrap token: functional, but not attributable and not
  independently revocable. PRSR-3 shipped that fallback and is closed; this is the
  hardening half, and it needs an operator with a `users:manage`-capable token.
- **PRSR-22 — service spin-up** (below), gated on its prereq **PRSR-11**.

## Future direction: service spin-up (epic PRSR-22)

Everything above provisions **people into existing services** (the person ×
service axis). A separate, larger direction is standing up the Cloudflare edge
for a *new* Construct app in one command — DNS record, tunnel ingress route, and
Access application + policy — so bringing `argosy`, `interlock`, `cook_book`,
`centrifuge`, etc. online stops being a manual dashboard operation.

This is a **different axis** (keyed on hostname/service, not person × service),
so it does *not* extend the person-shaped `Connector`. The plan is a parallel
`ServiceProvisioner` interface (`Ensure(ServiceSpec) / Teardown`) with its own
orchestrator path — a `purser provision-service` sibling to `purser invite` —
reusing the existing CF API client, registry, store, and idempotency ethos.

- **Reusable today:** the CF `do()` client (bearer + `{success,errors}`
  envelope), the registry / `ErrPending`-degrade idiom, config, store/migrator.
- **New work:** DNS, tunnel-route, and Access-*application* operations (the
  connector only manages Access *group* membership today); a `ServiceSpec` +
  resource table recording created CF resource IDs for idempotent teardown.
- **Token scopes:** Access *Apps & Policies* Edit is already held; **Zone → DNS
  → Edit** and **Account → Cloudflare Tunnel → Edit** are not yet provisioned.
- **Open blocker:** whether the cloudflared tunnel is remotely-managed (routes
  settable via the CF API) or driven by a local `config.yml` (not API-settable).
  Argosy sidesteps it entirely — it's on the *direct / non-tunnelled* path, so
  its spin-up is DNS-to-static-IP + Access app only, and is the natural pilot.

Epic PRSR-22 carries the full assessment and breakdown; PRSR-11 is its only
currently-actionable part — the tunnel decision above plus the token scopes, both
decisions and access changes rather than code.
