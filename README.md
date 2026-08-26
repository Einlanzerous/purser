# Purser

Cross-service provisioning & invite service for the Construct. One action
invites a person into multiple ecosystem services at once, mints starter
credentials, grants Cloudflare Access SSO, and hands back a copy-pasteable
credential block — or emails it.

A single static Go binary that is both a CLI and a thin HTTP API, a sibling to
the other construct-server Go services (shared Postgres 16, `construct_net`,
Tailscale/Cloudflare edge).

```
purser invite --name "Ada Lovelace" --email ada@example.com \
    --to switchyard,lyceum,argosy,cloudflare --deliver copypaste
```
```
invite 738258c0-… for Ada Lovelace (delivery=copypaste)
  ✓ Switchyard               succeeded
  ✓ Lyceum                   succeeded
  ✓ Argosy                   succeeded
  ✓ Cloudflare Access (SSO)  succeeded

--- credential block (stdout) ---
Hi Ada — you've been granted access to the Construct.

🚀 Start here: https://zero-gravity-industries.cloudflareaccess.com
    Sign in with the email one-time-PIN sent to ada@example.com — no password.
    The Construct apps behind single sign-on are listed there.

Per-app details, including anything the launcher can't sign you into:

🚉 Switchyard
    URL:      https://switchyard.zerogravity.industries
    Username: Ada Lovelace
    API token: sw_…
    → Through the tunnel you'll be signed in automatically after the Cloudflare
      email one-time-PIN. On the LAN, paste the API token on the login screen.

📚 Lyceum
    URL:      https://lyceum.zerogravity.industries
    Username: Ada Lovelace
    invite token (single-use, expires in 7 days): lyc_…
    → Redeem this invite at https://lyceum.zerogravity.industries (Settings → Sign in) within 7 days.

🎬 Argosy
    URL:      https://argosy.zerogravity.industries
    Username: ada@example.com
    password (shown once — change it after signing in): …
    → Sign in at https://argosy.zerogravity.industries with this email and password,
      then pair your devices from the app.

Keep any secrets above private — they are shown once and cannot be retrieved later.
```

See [`docs/architecture.md`](docs/architecture.md) for the full design (this is
the IDEA-14 canonical reference).

## Why several services for one person?

The apps don't share a user model. **Switchyard** and **Lyceum** are the accounts
*inside* the apps; **Cloudflare Access** is the SSO gate *in front of* them. Both
apps match the Cloudflare-verified email against their own user record and
**never auto-create** the account — so Purser creates the app user (with the
email set) *and* adds that email to the Cloudflare Access allow-list.

> **The invariant:** both halves or neither. An Access entry without a matching
> app account is worse than no access at all — the person clears the edge gate,
> then gets refused by the app with no way to self-serve. Granting Access to a
> group without provisioning the app accounts is the standard way to create this
> state; `--to <app>,cloudflare` in one invocation is what avoids it.

Argosy is on the direct path with its own login (no Cloudflare Access).

## Connectors

| Service      | Action                                                           | Status |
|--------------|------------------------------------------------------------------|--------|
| `switchyard` | create user (email set) → mint API token                         | ✅ |
| `cloudflare` | add email to a shared Access group (email-OTP SSO)               | ✅ when a CF API token is configured; else prints the manual dashboard step |
| `lyceum`     | create user (email set) → mint a single-use 7-day `lyc_` invite   | ✅ when `PURSER_LYCEUM_OWNER_TOKEN` is set **and** Lyceum runs with `LYCEUM_AUTH=true`; else registers Unavailable |
| `argosy`     | create account (email login) → return the one-time password       | ✅ when `PURSER_ARGOSY_PROVISION_TOKEN` matches the argosy service's `ARGOSY_PROVISION_TOKEN`; else registers Unavailable |

### Lyceum setup

`POST /admin/users` is owner-gated, and `/admin` needs an owner **session**
token — a `LYCEUM_API_TOKENS` entry cannot reach it. One-time, on the host:

```sh
docker exec lyceum lyceum mint-token          # → a one-time lyc_ owner invite
curl -X POST http://localhost:4005/auth/session \
  -H 'Content-Type: application/json' \
  -d '{"token":"lyc_…","device_label":"purser"}'
```

The returned `session_token` **never expires** — set it as
`PURSER_LYCEUM_OWNER_TOKEN` and recreate `purser`.

Note that with Lyceum behind Cloudflare Access, tunnel users auto-sign-in from
the CF JWT and never redeem the `lyc_` invite; it matters only for LAN access and
the native Android/Windows shells.

### Argosy setup

`POST /api/v1/admin/accounts` is gated on a static service secret, not a session
— a bearer session is always scoped to an existing account, so it can't authorize
creating a new one. Argosy registers the route **only** when
`ARGOSY_PROVISION_TOKEN` is set, and 404s it otherwise.

One-time, on the host:

```sh
signet set --project construct-server --name ARGOSY_PROVISION_TOKEN --generate
# add ARGOSY_PROVISION_TOKEN to .env, then:
docker compose up -d --force-recreate argosy purser
```

Purser reads the same value as `PURSER_ARGOSY_PROVISION_TOKEN`. To confirm the
route is live, a bogus token should get `401`, not `404`:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST \
  -H 'X-Provision-Token: bogus' -H 'Content-Type: application/json' \
  -d '{}' http://localhost:8096/api/v1/admin/accounts   # → 401
```

Argosy is on the direct path (Traefik, no Cloudflare Access), so `--to argosy`
needs no paired `cloudflare` grant: the invitee signs in with their email and
the one-time password, then pairs devices from the app.

## Onboarding bundles

Most invites aren't "here's access to app X" — they're "welcome to the family,
here's everything." A **bundle** is a named service set, so that's one flag:

```sh
purser invite --name "Ada" --email ada@example.com --bundle all
purser invite --name "Gran" --email gran@example.com          # default bundle
```

| Bundle | Services | For |
|---|---|---|
| `media` (default) | `cloudflare`, `lyceum`, `argosy` | Household members who just want to watch and read |
| `all` | `cloudflare`, `switchyard`, `lyceum`, `argosy` | People who'd actually use Switchyard too |

Cloudflare is in both because **Lyceum sits behind the Access gate** — granting
the app account without the Access entry leaves the person stuck at the edge
(see the invariant above). Argosy is on the direct path and needs no grant; the
overlap costs nothing.

`media` is the default deliberately: it's the smaller grant, so an invite that
names neither `--to` nor `--bundle` can't hand out Switchyard by accident.

**Switchyard access.** A bundle grants `*:user` — Switchyard's *User* project
level on every project. That's the **project membership role**
(`viewer|user|editor|admin`), not the instance role (`member|owner`), which
stays at its preset default: a bundle widens what you can see, it doesn't
escalate privilege. Override per bundle with
`PURSER_BUNDLE_<NAME>_PROJECTS=IDEA:editor`, or per invite with `--projects`,
which always wins.

Bundles are env-configured (`PURSER_BUNDLE_*`) — see
[`.env.example`](.env.example). Defining any bundle replaces the built-ins
wholesale rather than merging, so a partial override can't silently inherit half
a default set.

`--to` still works for one-off grants, and combining it with `--bundle` takes
the union ("the family set, plus this one extra"). Since idempotency is per
(person × service), overlapping bundles and re-invites are safe by
construction — already-provisioned services are skipped and no fresh secret is
minted.

## The launcher

The credential block leads with **Cloudflare's App Launcher** — the one page
listing every Access-gated app a person can reach — instead of making them keep
a list of URLs. It's free: Cloudflare already renders it at the team domain, so
Purser gains no public surface and stays internal-only.

It appears only when the invite left the person able to use it — two conditions:

1. **They're in the Access group.** The launcher lists Access apps, so pointing
   an Argosy-only invitee at it would render them an empty page. A *failed*
   Access grant doesn't count either; a link that rejects them reads as a broken
   invite. An already-provisioned (skipped) grant does count.
2. **Everything else in the invite came through.** This is the half-open case:
   Access admits them to the edge, then the app whose provisioning failed refuses
   them, with no way to self-serve. That's the state the both-halves-or-neither
   invariant exists to prevent, so the block must not present it as a finished
   welcome. An `unavailable` service counts here too — the recipient can't act on
   the difference between a broken connector and an unconfigured one, since
   either way the app turns them away. Those invites fall back to the plain
   per-service list, with the details in the operator note.

When the launcher leads, the standalone Cloudflare entry is dropped — it carries
no URL and no secret of its own, so it would only repeat the sign-in instruction
under a second heading.

Direct-path apps still deliver their credentials inline — that's precisely why
the launcher can't be the whole message. Argosy's one-time password appears under
its own heading in the same block.

> **Operational caveat:** Cloudflare's App Launcher is itself an Access
> application with its own policy. Membership in `zerogravity-members` gets
> someone into the *apps*, but the launcher admits them only if its own policy
> also allows the group. Purser can't detect that from the API surface it uses,
> so confirm it once in the Zero Trust dashboard — otherwise the credential
> block's first instruction leads to a refusal.

`PURSER_LAUNCHER_URL` overrides the address; unset it defaults to `https://` +
`PURSER_CF_TEAM_DOMAIN`, so this works with no new configuration.

## Usage

### CLI

```
purser                       # run the HTTP server (default)
purser serve                 # ditto
purser invite --name NAME --email EMAIL [--to svc1,svc2] [--bundle NAME] [--role member|admin] [--deliver copypaste|email]
purser person add --name NAME --email EMAIL [--type human|agent] [--rename] [--audit]
purser person list [--to svc1,svc2] [--type human|agent] [--all] [--json]   # the roster
purser person show --email EMAIL [--json]                  # one person in full
purser offboard --email EMAIL [--to svc1,svc2] [--apply]   # revoke access; previews by default
purser provision-service --service KEY --hostname HOST --mode tunnelled|direct --upstream UPSTREAM --access gated|bookmark|none [--tunnel prod] [--logo placard|none|URL] [--apply]
purser audit [--email EMAIL] [--to svc1,svc2]              # report drift, read-only
purser reconcile --email EMAIL | --all [--to svc1,svc2]    # repair records
purser migrate               # apply DB migrations and exit
purser version
```

### Reading the roster

"Who is on the roster, and what does each person have?" is a question Purser is
the system of record for, so it answers it — no psql, no schema knowledge, and
no `SELECT` that is one typo away from an `UPDATE` against live provisioning
records:

```
$ purser person list
NAME            EMAIL                TYPE   SERVICES                SINCE
Ada Lovelace    ada@example.com      human  cloudflare, switchyard  2026-03-11
Bradley Kim     bradley@example.com  human  switchyard              2026-04-02
Nightly Runner  bot@example.com      agent  argosy                  2026-05-19

3 people
1 non-active account hidden (deprovisioned or stale) — pass --all to include it
```

`--to svc1,svc2` narrows to people holding those services, `--type human|agent`
to one kind of identity, and `--json` emits the structured form — the shape an
agent reaches for before an invite, which is the case that drove this.

`person show` is one person in full: every account with its status, and the
invite history.

```
$ purser person show --email ada@example.com
Ada Lovelace <ada@example.com>
human · id a8ebb070-… · recorded 2026-03-11 · updated 2026-04-02

ACCOUNTS
SERVICE     STATUS  USERNAME  EXTERNAL ID      RECORDED    UPDATED
cloudflare  active  —         ada@example.com  2026-03-11  2026-03-11
lyceum      stale   ada       u-4              2026-03-11  2026-06-02
switchyard  active  ada       u-17             2026-03-11  2026-03-11

INVITES
WHEN        DELIVERY   ROLE    DELIVERED
2026-06-02  copypaste  member  —
2026-03-11  email      member  2026-03-11
```

Three things about these commands are deliberate:

**They read local records only** — `person`, `account` and `service`, plus
`invite` for `show`'s history — and call no connector. Asking who is on the
roster shouldn't require every upstream service to be reachable, or cost a full
reconcile sweep. `audit` is the command that compares records against upstream;
this is the one that reads the records.

**No secret, not even a hash.** Credentials are shown once, at invite time. The
roster's account type has no `secret_hash` or `secret_ref` field and the query
selects neither column, so there is nothing for a renderer or a `--json` encoder
to leak — the guarantee is the type, not a rule about what to print.

**`list` shows active accounts by default, and says when it didn't.** A stale
row is precisely what someone *doesn't* have, so it would misreport the services
column. But hiding it silently is worse: `--to lyceum` returning nobody has to
mean "nobody has Lyceum", not "nobody has Lyceum any more", so the hidden count
is always reported. `show`, being the single-person view, withholds nothing —
there a stale row is the interesting part, and it carries its status.

### Offboarding

`invite` grants access across the stack in one command. `offboard` is its
opposite, and until PRSR-17 it did not exist — removing someone meant four
manual deletes plus hand-editing Purser's records:

```
$ purser offboard --email ada@example.com
SERVICE     USERNAME  ACTION
argosy      ada       revoke
cloudflare  —         revoke
lyceum      ada       nothing-to-do
switchyard  ada       revoke

Ada Lovelace <ada@example.com>: 3 to revoke, 1 nothing to do, 0 unavailable, 0 failed

Preview — nothing revoked. Re-run with --apply to revoke 3 services.
```

**It previews by default.** `invite` acts without asking because granting access
twice is merely wasteful; revoking the wrong person is not undone by running it
again. `--apply` is what acts, and a dry run makes *no connector call at all*.
There is no bulk mode either — `--email` is required and always names one person,
which is a stronger guard than the `--all` flag `reconcile` needs.

**It revokes; it does not delete.** Per service:

| service | what happens |
|---|---|
| `switchyard` | every live API token revoked; the user and their tickets stay |
| `cloudflare` | the email is removed from the Access group |
| `lyceum` | **deleted** — `DELETE /admin/users/{id}` is the only operation its admin API has |
| `argosy` | **nothing** — no delete or disable endpoint exists; reported `unavailable` |

The preview knows this in advance — a connector that can't revoke says so without
being called, so the dry run never promises what `--apply` would decline.

Lyceum is called out because it is the exception, and Argosy because it is the
gap. An Argosy account is reported as still open rather than quietly skipped:

```
Still has access — Purser cannot revoke these:
  - Argosy: connector: provisioning not yet available: argosy has no account delete
    or disable endpoint — remove the account by hand until one ships
Remove them by hand.
```

That distinction is `TaskUnavailable` doing its job. A connector nobody has built
a delete for did not *fail* — but on this path it is not success either, so
`offboard` exits non-zero for it, the opposite of `invite`.

**A failed revoke leaves the record active.** Only a connector that actually
succeeded marks the `account` row `deprovisioned`. Recording a revoke that didn't
happen is worse than the failure: the error scrolls away, and the audit,
`person show`, and the next invite's idempotency skip all go on reading a column
that says access was removed while it is still live.

**The row is marked, never deleted**, so what someone held — and when it was taken
away — survives the offboarding. That last part needed its own column:
`updated_at` is bumped by the next re-invite, so `deprovisioned_at` (migration
0006) records the revocation durably and is never cleared. `person list` hides
the row and says so; `person show` displays it with a REVOKED date.

> **One thing to know:** revoking Switchyard tokens removes *API* access. The
> sign-in is gated by the Cloudflare Access group, so an offboard that skips
> `cloudflare` leaves a working login behind. The CLI warns when that happens,
> but the half-done case looks finished, which is why it is worth stating twice.

### Standing a service up

The other axis. `invite` puts a *person* into a service that already exists;
`provision-service` builds the **edge that makes a service exist** — its DNS
record, its Cloudflare Access application, and, if it is tunnelled, its ingress
route on the tunnel. It is keyed on hostname rather than on a person, and it is
idempotent per (hostname, kind).

Like `offboard` and unlike `invite`, it **plans by default**:

```
$ purser provision-service --service argosy \
    --hostname argosy.zerogravity.industries \
    --mode direct --upstream 100.64.0.7 --access bookmark

RESOURCE                         ACTION   DETAIL
Cloudflare Tunnel ingress route  skipped  direct spec: traffic does not pass through a tunnel
Access application               adopt    bookmark "argosy" → argosy.zerogravity.industries, no logo
DNS record                       adopt    DNS only A → 100.64.0.7

argosy (argosy.zerogravity.industries): 0 in place, 2 to do, 1 skipped

Plan — nothing created or changed. Re-run with --apply to act on 2 steps.
```

`adopt` is the status that makes this usable on an estate that already exists.
Argosy's edge predates Purser entirely, so the honest first exercise is running
this against a service that is **already up**: upstream already matches the spec,
Purser simply holds no record of it, so `--apply` writes the rows and makes *no
upstream call at all*. A spin-up tool that can only recognise what it built
itself is one nobody will point at production, and re-creating a live DNS record
just to learn its id is the wrong way to get one.

Every kind gets a line, including the ones this spec doesn't call for — silence
about the tunnel should never have to be read as "the tunnel is fine".

Two orderings are load-bearing rather than cosmetic:

- **DNS goes last**, because it is the step that makes the hostname resolve. The
  other two are inert until something does.
- **...and is held back if what it depends on didn't land.** Ordering only closes
  the window when the earlier step *succeeded*, so a `gated` service whose Access
  step failed, was unavailable, or couldn't be read reports its DNS step as
  `blocked` and publishes nothing. Publishing anyway would leave a service that
  is meant to be gated reachable and **ungated** — which, unlike a 502, does not
  announce itself. A `bookmark` app is deliberately *not* a prerequisite: it is a
  launcher tile in front of a service with its own login, so its absence costs an
  icon, not a gate.

The statuses that need a human each say something different, and each gets its
own line rather than a shared count:

| | |
|---|---|
| `unavailable` | Purser isn't configured for this step — the line names the variable to set |
| `refused` | upstream is in a state Purser won't write to; re-running repeats this until it's fixed **there** |
| `unknown` | the state couldn't be read, so nothing was decided from it — re-run |
| `blocked` | held back so the hostname isn't published in front of a step that didn't land |
| `applied-not-recorded` | the edge changed and the row didn't — Purser can't tear down what it holds no id for |

`refused` and `unknown` are separate on purpose. Both decline to act, but a
failed read means "run it again", and upstream being in a state Purser won't
write to means "go and fix that, because running it again will print this until
you do". Putting that difference in an error string would make it a second field
a reader has to know to consult.

The line runs through DNS as well as the tunnel, and both sides of it are live:
several records answering for one hostname and none of them the spec's is
`refused` — the read worked, and somebody has to edit the zone — while a lookup
that comes back a *full page* is `unknown`, because the name filter narrowed
nothing and that answer was never really read.

Separately from the statuses, a step can carry a **warning**: something that went
wrong *around* a step that nonetheless succeeded. There is one today, and it is
the one worth designing for — the tunnel's ingress document is shared, so if
another process writes it between Purser's read and its PUT, this run's route is
fine and **another service's may have been dropped**. That is a different claim
from anything the step's own status can make, so it is its own field rather than
a clause inside the description, printed on its own line by the CLI and logged
server-side by `purser serve`, where a caller reading only `status` and `counts`
would otherwise see nothing at all.

Exit is non-zero for any of the above, including `unavailable` — that follows
`offboard` rather than `invite`. On an invite, unavailable means nothing was
granted and nobody is harmed by waiting; here it means a step of the edge does
not exist, so the hostname does not work.

> **Not yet exercised against the live Cloudflare API.** Every assertion in the
> test suite is against an httptest fake. The provisioners' behaviour is pinned,
> and the ingress matcher is read from cloudflared's own source rather than
> inferred — but that is not the same as having watched it run.

### Auditing and reconciling

Purser's `account` rows record what **Purser provisioned** — not who actually
has access. Those drift apart whenever someone is set up by hand, or an upstream
account is deleted. `audit` compares the two:

```
$ purser audit
PERSON                SERVICE     RECORDED  UPSTREAM  ACTION
arin.reese@…          switchyard  no        yes       record
arin.reese@…          cloudflare  no        yes       record
old-tester@…          lyceum      yes       no        mark-stale

12 checked: 8 ok, 2 to record, 1 stale, 1 unverifiable, 0 errors
Dry run — nothing written. Re-run as `purser reconcile` to apply.
```

`reconcile` applies the same findings. Both go through each connector's
`Reconcile`, which is **read-only by contract** — it never provisions, mints,
rotates, or invalidates a credential. That's the difference between this and
re-inviting someone: a re-invite would hand a Switchyard user who already has
access a *second* API token.

Repairs run in both directions:

| situation | action |
|---|---|
| upstream has it, Purser doesn't | write an `account` row (no secret — Purser never learned theirs) |
| Purser has it, upstream doesn't | mark the row `stale` |

`stale` matters more than it looks. Idempotency keys on **active**, so a row
left active with no upstream account means the orchestrator skips that person
forever and no invite can ever fix them. Marking it stale re-arms provisioning.

**All four connectors are verifiable** as of Argosy v0.18.0 (ARGY-163), which
added `GET /api/v1/admin/accounts?email=`. Switchyard and Lyceum list their
users; Cloudflare reads the Access group; Argosy looks the account up. None of
them write.

A connector whose upstream has no lookup must return `ErrReconcileUnsupported`
rather than inferring absence — reporting "no" for a question you can't answer
would claim people lack access they demonstrably have, manufacturing the exact
drift the audit exists to find. Nothing is in that state today, but the contract
is what keeps a future connector honest.

`purser reconcile` refuses to touch the whole roster without `--all`, since it's
a bulk write.

### Adding someone Purser didn't provision

The audit walks the `person` table, so it only sees people Purser already knows.
Someone onboarded by hand is omitted entirely — and the report still says
`0 errors`, which reads as complete when it isn't. `person add` writes the row
and nothing else:

```
$ purser person add --name "Ada Lovelace" --email ada@example.com --audit
added Ada Lovelace <ada@example.com> (human)

No accounts written; the add itself called no connectors.

--- audit --email ada@example.com (read-only) ---
PERSON           SERVICE     RECORDED  UPSTREAM  ACTION
ada@example.com  argosy      no        unknown   unverifiable
ada@example.com  cloudflare  no        unknown   unverifiable
ada@example.com  lyceum      no        unknown   unverifiable
ada@example.com  switchyard  no        yes       record

4 checked: 0 ok, 1 to record, 0 stale, 3 unverifiable, 0 errors

Dry run — nothing written. Re-run as `purser reconcile` to apply.
Unverifiable services have no upstream lookup endpoint; they are left untouched.
  …

Next:
  purser reconcile --email ada@example.com                     # apply the 1 change above
  purser invite --name "Ada Lovelace" --email ada@example.com  # provision what they lack
```

(Only Switchyard is configured in that example, hence three unverifiable rows;
the per-service reasons are elided at `…`.)

The add itself writes no `account` rows, calls no connectors, and mints no
credentials — use `invite` to provision. `--audit` then runs the same read-only
preview `purser audit --email …` would print; that half *does* reach connectors,
through `Reconcile`, which never mutates. The `reconcile` hint appears only when
the preview actually found something to record.

An email that already exists is a **conflict, not an edit** — for both the name
and the type. Purser used to have an `UpsertPerson` whose `ON CONFLICT` set the
name, so a command taking `--name` renamed whoever held that address silently;
`add` uses an insert-if-absent instead and refuses:

```
$ purser person add --name "A. Lovelace" --email ada@example.com
purser: invite: person already recorded under a different name: ada@example.com is recorded as "Ada Lovelace", not "A. Lovelace"
purser: pass --rename to change the recorded name
```

`--rename` opts in, and reports the name the write actually replaced rather than
one inferred from a prior read. Omitting `--type` leaves an existing person's
type alone; passing one that disagrees is refused the same way.

`invite` never renames either, but what it does about a mismatch depends on how
the credentials are being delivered. On `--deliver copypaste` it warns and
carries on — the operator is the gate, and failing the provision over a name
typo punishes the wrong action:

```
$ purser invite --name "Ada Lovelacce" --email ada@example.com --to switchyard

invite 6f1c… for Ada Lovelace (delivery=copypaste)
  ! ada@example.com is recorded as "Ada Lovelace", not "Ada Lovelacce" — kept the recorded name
    to change it: purser person add --email ada@example.com --name "Ada Lovelacce" --rename
  ✓ Switchyard              succeeded
```

On `--deliver email` it **refuses**, before provisioning or sending anything. A
mismatch is the only sign that a mistyped `--email` landed on a *different*
person, and that path mails them working credentials:

```
$ purser invite --name "Bob Smith" --email ada@example.com --to switchyard --deliver email
purser: invite: refusing email delivery for a name that disagrees with the record: ada@example.com
is recorded as "Ada Lovelace", not "Bob Smith" — … re-run with the recorded name, or rename first
```

`person add --rename` is the only way to change a recorded name. `UpsertPerson`
is gone as of PRSR-23, when its last caller went away — so a person row is
created in exactly one place and renamed in exactly one other, and "what can
change a name?" is answerable by grep rather than by review.

`--email` is required on **both** commands — on `person add` even with
`--type agent`, and on `invite` whichever `--deliver` is used. It's the conflict
target that makes them idempotent and the key the audit looks people up by, so a
row without one could be added twice and reconciled never. It is validated as an
address, which rejects a fragment or a `Name <addr>` form — it cannot, of
course, catch a plausible typo like `ada@exmaple.com`.

Emails are unique **case-insensitively** (migration `0003`). They were not
before: the index was case-sensitive while every lookup matched `lower(email)`,
so a row entered by hand as `Ada@Example.com` didn't collide with the lowercased
address the code writes, and the insert added a second person for the same
human. That is the duplicate-identity failure this command exists to prevent, so
it had to be fixed in the schema, not worked around above it.

`invite` writes a human summary to stderr and the credential block to stdout, so
`purser invite … | pbcopy` (or `> block.txt`) captures exactly the pasteable
block. Re-running the same invite is idempotent — already-provisioned services
are skipped and only previously-failed ones retried.

That guarantee is why `invite` requires `--email` (PRSR-23). It used to accept an
invite without one, and the unique index is partial on `email IS NOT NULL`, so
the row it wrote could never collide with the row the *previous* run wrote: each
run recorded a new person, and the person id is what every downstream guarantee
is keyed on — `UNIQUE(person_id, service_id)` for the skip above, and the
`InviteRef` an upstream `Idempotency-Key` dedupes on. So the emailless invite
quietly re-provisioned everything on every run, minting a fresh upstream user and
a fresh secret each time, and left the audit one more person to walk per run.
There is no second identity key to fall back to, which is why the address is
required rather than defaulted.

Migration `0005` states the same rule in the schema — `CHECK (email IS NOT NULL)`,
declared `NOT VALID` so it binds every new row without having to decide at boot
what to do with any the old path left behind. Those are stranded either way: no
command can address a person who has no address, so repairing one means hand SQL
(which the constraint deliberately still allows). Relatedly, all four connectors
now refuse an emailless `Reconcile` rather than guessing — Switchyard used to
fall back to matching on display name, which as an *audit* answer would record
someone against a same-named stranger.

### HTTP API

Bearer-authenticated with `PURSER_API_TOKEN` (also relies on
construct_net/Tailscale isolation).

- `GET  /healthz`
- `POST /v1/invites` — `{ "name", "email", "services": [...], "bundle", "role", "deliver" }`
  (omit both `services` and `bundle` to grant the default bundle; `name` and
  `email` are required and a request without either is a `400`)
- `GET  /v1/invites/{id}` — status
- `POST /v1/spinups` — `{ "service", "hostname", "mode", "upstream", "access",
  "display_name", "logo", "tunnel", "apply" }` — stand up a service's edge.
  Omitting `apply` returns a plan and writes nothing. `logo` is a ref rather than
  a URL — `"placard"` (the default; resolve the mark by service key), `"none"`
  (clear the icon), or an explicit `https://` URL. The old `logo_url` is refused
  with a `400` naming the replacement rather than ignored, since a dropped field
  would silently discard the caller's icon.

  Each entry in `findings` carries a `status` (see the table under [Standing a
  service up](#standing-a-service-up)) and, occasionally, a `warning` — trouble
  around a step that *succeeded*, which no status or count can express. It is
  also written to the server log, so a caller that reads only `status` still
  leaves a trace of it somewhere. `spec` echoes the **normalized** spec, so a
  caller can compare what it sent against what the run actually used.

  **Read `needs_attention`, not `pending`.** `pending` counts only what `apply`
  would act on, so an apply against a deployment that isn't configured answers
  `pending: 0, changed: 0` — identical to an edge that was already correct.
  `needs_attention` names the kinds a person has to resolve and is absent when
  there are none; it is computed from the same list the CLI's exit code uses.

The credential block (with secrets) is returned only for `copypaste` delivery;
for `email` the secrets go to the recipient and are not echoed over HTTP.
`operator_note` — the list of services that didn't provision — is returned on
both paths and is never part of `credential_block`, so nothing addressed to the
operator can travel to the invitee.

In `POST /v1/invites`, each entry in `outcomes` has a terminal `status`:
`succeeded`, `skipped`, `failed`, or `unavailable`. The last means the connector
is registered but wasn't in a position to try — no token configured, or an
upstream with no provisioning API yet — and it's a separate status precisely so a
caller bucketing by `status` doesn't read "nobody wired this up" as "this broke".
Both are retryable, but only a `failed` one is worth retrying before someone
changes a config. The note groups them under separate headings for the same
reason.

`GET /v1/invites/{id}` reports the stored `provision_task` rows instead, so its
`tasks[].status` can also be `pending` (created, not yet run) or `running` — the
two non-terminal states an outcome never carries. Note that `pending` there means
*queued*, which is why the new status is `unavailable` and not `pending`.

> `unavailable` replaced a `pending: true` flag that rode alongside
> `status: "failed"`; the flag is gone rather than derived, so there is exactly
> one field to read.

`name_conflict` is present when the request's `name` disagreed with the stored
person; the stored name was kept and the invite ran anyway. It is also written
to the server log, so a caller that ignores the field still leaves a record.

```json
"name_conflict": { "email": "ada@example.com", "stored": "Ada Lovelace", "requested": "Ada Lovelacce" }
```

The same disagreement on `"deliver": "email"` is a **`409 Conflict`** instead —
nothing is provisioned and no mail is sent. The response body names both sides
so the caller can correct the address or the name.

## Configuration

Env vars, `PURSER_`-prefixed, with a `DATABASE_URL` fallback — see
[`.env.example`](.env.example). Key ones: `PURSER_DATABASE_URL`,
`PURSER_API_TOKEN`, `PURSER_SWITCHYARD_TOKEN`, `PURSER_LYCEUM_OWNER_TOKEN`,
`PURSER_CF_*`, `PURSER_LAUNCHER_URL`, `PURSER_SMTP_*`.

Each connector registers as Unavailable rather than failing when its
credentials are absent, so a partial config is safe — `--to` a service with no
credentials reports the gap instead of half-provisioning the person.

### The Cloudflare API token

One token (`PURSER_CF_API_TOKEN`) covers every Cloudflare call. The scopes it
actually needs, which had drifted from what the docs claimed:

| scope | level | used by |
|---|---|---|
| Access: Organizations, Identity Providers, and Groups → Edit | Account | `invite` — the Access group's member list |
| Access: Apps → Edit | Account | service spin-up |
| Access: Policies → Edit | Account | service spin-up |
| Cloudflare Tunnel → Edit | Account | service spin-up — tunnel ingress |
| DNS → Edit | **Zone**, scoped to the one zone | service spin-up — DNS records |

Only the first is needed to run `invite`; the rest belong to the service
spin-up axis. **Edit subsumes Read**, so there is no separate Read scope to
grant — and keeping `reconcile` read-only is a property of the code, not
something the token enforces.

Three naming traps in the dashboard, all of which cost time at least once:

- **"Account DNS Settings"** and **"Zone DNS Settings"** are both *settings*.
  **"DNS Views"** is the Internal DNS product. The one that grants records is
  the plain **DNS** group at **Zone** scope. The first column of each
  permission row is the scope selector; if it says Account, you're in the wrong
  family whatever the label reads.
- Every row's description says *"Grants read access to…"* until you actually
  select Edit, at which point it updates. The radio is authoritative, not the
  blurb.
- **Cloudflare Tunnel** is still **"Argo Tunnel"** in older docs and UI copy.

`GET /user/tokens/verify` returns `success: false` for this token even when
every functional call succeeds — that endpoint is user-scoped and is not a
health check here. Probe the real endpoints instead.

## Development

```
make build          # bin/purser
make test           # unit tests (DB-backed tests skip without a database)
make test-db        # spins a throwaway Postgres 16 and runs the full suite
make docker-build   # production image
```

Go 1.26, pgx/v5, embedded SQL migrations auto-applied on boot (no external
migration tool). DB-backed tests run against `PURSER_TEST_DATABASE_URL`.

## Deploying to construct-server

Purser is deployed as a container pulled from
`ghcr.io/einlanzerous/purser:latest`. Three coordinated edits in
`construct-server`, then bring it up:

1. **`db/init-db.sh`** — add `ensure_db purser_user "$PURSER_DB_PASSWORD" purser`.
2. **`postgres` service env** in `docker-compose.yml` — add
   `- PURSER_DB_PASSWORD=${PURSER_DB_PASSWORD}`.
3. **`.env` / `.env.example`** — add `PURSER_DB_PASSWORD`, `PURSER_API_TOKEN`,
   `PURSER_SWITCHYARD_TOKEN`, and (optionally) `PURSER_LYCEUM_OWNER_TOKEN` /
   `PURSER_CF_*` / `PURSER_SMTP_*`. On construct-server, deploys write `.env`
   from the **`PROD_ENV_FILE`** secret on the `home-server` environment — a var
   added only to the local `.env` is lost on the next deploy. That applies to
   every new var, including `PURSER_CF_ZONE_ID` / `PURSER_CF_TUNNEL_ID`.
4. Paste the `purser:` service from
   [`deploy/construct-server.compose.yml`](deploy/construct-server.compose.yml)
   into `docker-compose.yml`.

```
docker compose up -d postgres && make db-init      # create the purser role/DB
docker compose up -d purser                          # start Purser
```

Migrations apply automatically on boot.

## License

Private — part of the Construct home-ops stack.
