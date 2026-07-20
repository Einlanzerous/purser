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
    --to switchyard,lyceum,cloudflare --deliver copypaste
```
```
invite 738258c0-… for Ada Lovelace (delivery=copypaste)
  ✓ Switchyard               succeeded
  ✓ Lyceum                   succeeded
  ✓ Cloudflare Access (SSO)  succeeded

--- credential block (stdout) ---
Hi Ada — you've been granted access to the following:

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
| `argosy`     | pending Argosy's admin create-account endpoint                    | ⏳ |

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

## Usage

### CLI

```
purser                       # run the HTTP server (default)
purser serve                 # ditto
purser invite --name NAME --email EMAIL --to svc1,svc2 [--role member|admin] [--deliver copypaste|email]
purser migrate               # apply DB migrations and exit
purser version
```

`invite` writes a human summary to stderr and the credential block to stdout, so
`purser invite … | pbcopy` (or `> block.txt`) captures exactly the pasteable
block. Re-running the same invite is idempotent — already-provisioned services
are skipped and only previously-failed ones retried.

### HTTP API

Bearer-authenticated with `PURSER_API_TOKEN` (also relies on
construct_net/Tailscale isolation).

- `GET  /healthz`
- `POST /v1/invites` — `{ "name", "email", "services": [...], "role", "deliver" }`
- `GET  /v1/invites/{id}` — status

The credential block (with secrets) is returned only for `copypaste` delivery;
for `email` the secrets go to the recipient and are not echoed over HTTP.

## Configuration

Env vars, `PURSER_`-prefixed, with a `DATABASE_URL` fallback — see
[`.env.example`](.env.example). Key ones: `PURSER_DATABASE_URL`,
`PURSER_API_TOKEN`, `PURSER_SWITCHYARD_TOKEN`, `PURSER_LYCEUM_OWNER_TOKEN`,
`PURSER_CF_*`, `PURSER_SMTP_*`.

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
