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
Deprovision(ctx, Input) error                     // remove access (unimplemented — PRSR-17)
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
  last_error so a re-run retries only what failed.

## Idempotency

Re-running the same invite is safe and **retries only failed services**: a
service with an active `account` row (upstream id present) is *skipped* — no
duplicate upstream user, no fresh secret — while a previously-failed service is
retried. Per-service connector failures never abort the whole invite; they are
recorded and surfaced in the operator note — a field of its own, separate from
the credential block, because only the block is ever emailed (PRSR-19).

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

An email that already exists is a **conflict, not an edit**. `UpsertPerson` does
`ON CONFLICT … DO UPDATE SET name`, so any command taking `--name` would
otherwise rename whoever holds that address without ever saying so. `add` uses
`InsertPersonIfAbsent`, which never touches an occupied row, and `--rename` is
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

Email is required even for `--type agent`, though the schema allows it to be
null: it is the conflict target that makes the add idempotent and the key the
audit looks people up by, so a row without one could be added twice and
reconciled never.

**Emails are unique case-insensitively** (migration 0003). They weren't
originally: `person_email_key` indexed the bare column while every store lookup
matched `lower(email)`. A row entered by hand as `Ada@Example.com` therefore did
not collide with the lowercased address the code writes, and the upsert inserted
a *second* person for the same human — precisely the duplicate identity this
command exists to prevent, and one the audit would then walk and populate twice.
That had to be fixed in the schema: a guard in `AddPerson` would have left
`invite` minting the duplicates instead.

Deploying 0003 requires that no such pair already exists, since the index cannot
be built over one. It fails loudly and transactionally if so — the migration
carries the query that finds them.

## Delivery

The credential block is plain text (pastes cleanly into any chat platform).
`--deliver copypaste` (default) returns it for the operator to paste;
`--deliver email` sends it over SMTP to the person. One-time secrets appear once
and are never retrievable afterward.

An invite where *every* service failed sends no email at all: there is nothing to
tell the recipient, and a greeting on its own announces access that wasn't
granted. The invite stays undelivered, the operator is told why, and the next run
retries only what failed.

The block **leads with Cloudflare's App Launcher** — the one page listing every
Access-gated app a person can reach — and then gives per-service detail. That
page is Cloudflare's, rendered at the team domain, so Purser gains no public
surface and stays internal-only.

The link is conditional: the launcher lists *Access* apps, so it appears only for
invites that actually granted Access. An Argosy-only invitee would otherwise be
sent to an empty page, and a *failed* Access grant doesn't count either — a link
that rejects them reads as a broken invite. Direct-path apps deliver their
secrets inline in the same block, which is precisely what the launcher cannot
carry through.

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

**Open:**

- **PRSR-17 — Deprovision.** Declared on the interface, unimplemented on every
  connector except Cloudflare. Purser can onboard across the stack in one command
  and cannot remove anyone. This is why `stale` currently means "re-arm
  provisioning" and cannot mean "revoke".
- **PRSR-18 — run the audit on a schedule.** It exists and nothing triggers it,
  so drift will reaccumulate and be found the same way it was last time: by
  accident.
- **PRSR-3 — a dedicated Switchyard provisioning token.** Purser currently
  authenticates as the instance bootstrap token: functional, but not attributable
  and not independently revocable.
- **PRSR-11 — service spin-up** (below).

## Future direction: service spin-up (PRSR-11)

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

See PRSR-11 for the full assessment and the proposed epic breakdown.
