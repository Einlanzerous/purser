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
    Every app you can reach is listed on that page.

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

🔐 Cloudflare Access (SSO)
    → Sign in to Switchyard and the other tunneled Construct apps with the email
      one-time-PIN sent to ada@example.com (no password).

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

It appears **only when the invite granted Cloudflare Access**. The launcher lists
Access apps, so pointing an Argosy-only invitee at it would render them an empty
page; those invites fall back to the plain per-service list. A *failed* Access
grant doesn't count either — they can't sign in yet, and a link that rejects them
reads as a broken invite. An already-provisioned (skipped) grant does count.

Direct-path apps still deliver their credentials inline — that's precisely why
the launcher can't be the whole message. Argosy's one-time password appears under
its own heading in the same block.

`PURSER_LAUNCHER_URL` overrides the address; unset it defaults to `https://` +
`PURSER_CF_TEAM_DOMAIN`, so this works with no new configuration.

## Usage

### CLI

```
purser                       # run the HTTP server (default)
purser serve                 # ditto
purser invite --name NAME --email EMAIL [--to svc1,svc2] [--bundle NAME] [--role member|admin] [--deliver copypaste|email]
purser audit [--email EMAIL] [--to svc1,svc2]              # report drift, read-only
purser reconcile --email EMAIL | --all [--to svc1,svc2]    # repair records
purser migrate               # apply DB migrations and exit
purser version
```

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

`invite` writes a human summary to stderr and the credential block to stdout, so
`purser invite … | pbcopy` (or `> block.txt`) captures exactly the pasteable
block. Re-running the same invite is idempotent — already-provisioned services
are skipped and only previously-failed ones retried.

### HTTP API

Bearer-authenticated with `PURSER_API_TOKEN` (also relies on
construct_net/Tailscale isolation).

- `GET  /healthz`
- `POST /v1/invites` — `{ "name", "email", "services": [...], "bundle", "role", "deliver" }`
  (omit both `services` and `bundle` to grant the default bundle)
- `GET  /v1/invites/{id}` — status

The credential block (with secrets) is returned only for `copypaste` delivery;
for `email` the secrets go to the recipient and are not echoed over HTTP.

## Configuration

Env vars, `PURSER_`-prefixed, with a `DATABASE_URL` fallback — see
[`.env.example`](.env.example). Key ones: `PURSER_DATABASE_URL`,
`PURSER_API_TOKEN`, `PURSER_SWITCHYARD_TOKEN`, `PURSER_LYCEUM_OWNER_TOKEN`,
`PURSER_CF_*`, `PURSER_LAUNCHER_URL`, `PURSER_SMTP_*`.

Each connector registers as Unavailable rather than failing when its
credentials are absent, so a partial config is safe — `--to` a service with no
credentials reports the gap instead of half-provisioning the person.

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
   added only to the local `.env` is lost on the next deploy.
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
