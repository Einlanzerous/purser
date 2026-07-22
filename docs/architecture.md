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
- **Argosy** (media server) has accounts (username + password) → profiles →
  device bearer tokens, on the *direct* (non-tunnelled) path — its own login, no
  Cloudflare Access.
- **Lyceum** (ebooks) has no per-user account model yet.

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
Provision(ctx, Input) (Result, error)   // create/ensure the account, return a one-time secret
Reconcile(ctx, Input) error             // repair drift (idempotent)
Deprovision(ctx, Input) error           // remove access (stubbed in Phase 1)
```

| Connector    | What it does                                                        | Status |
|--------------|---------------------------------------------------------------------|--------|
| `switchyard` | `POST /v1/users` (email set) → `POST /v1/users/{id}/tokens`         | ✅ live |
| `cloudflare` | Adds the email to a shared Access group (email-OTP SSO gate)         | ✅ live when a CF API token is configured; otherwise prints the manual dashboard step |
| `argosy`     | Placeholder — pending an admin create-account endpoint (ARGY ticket) | ⏳ pending |
| `lyceum`     | Future — Lyceum needs a per-user account model first                 | — |

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
`zero-gravity-industries.cloudflareaccess.com`. Today, adding a person is a
manual dashboard operation — the host has a tunnel token and a DNS-01 token but
**no Access-scoped API token**. The `cloudflare` connector is written against
the real Cloudflare Access API and works the moment such a token + a shared
Access group are provisioned (see the SERV follow-up ticket); until then it
degrades to printing the exact manual step. Recommended model: one shared Access
**group** referenced by every app's policy, so a single grant covers all apps
and Purser has one place to add/remove people.

## Data model

`migrations/0001_init.up.sql`:

- `person` — who we invite (email unique when present; the SSO join key).
- `service` — target systems, seeded from the connector registry on boot.
- `account` — durable "person P has access to service S"; **unique (person,
  service)** — the idempotency key. Secrets are never stored plaintext, only a
  sha256 hash (`secret_ref` is reserved for a future vault).
- `invite` — one provisioning run for a person across services.
- `provision_task` — one service's slice of an invite; tracks attempts +
  last_error so a re-run retries only what failed.

## Idempotency

Re-running the same invite is safe and **retries only failed services**: a
service with an active `account` row (upstream id present) is *skipped* — no
duplicate upstream user, no fresh secret — while a previously-failed service is
retried. Per-service connector failures never abort the whole invite; they are
recorded and surfaced in the credential block's operator note.

## Delivery

The credential block is plain text (pastes cleanly into any chat platform).
`--deliver copypaste` (default) returns it for the operator to paste;
`--deliver email` sends it over SMTP to the person. One-time secrets appear once
and are never retrievable afterward.

## Security notes

- Secrets are delivered once and persisted only as a hash.
- The HTTP API is bearer-token protected (`PURSER_API_TOKEN`); it also relies on
  `construct_net`/Tailscale isolation.
- Purser holds an admin-capable Switchyard token and (when configured) a
  Cloudflare Access-edit API token — treat the `.env` as sensitive.

## Phasing

- **Phase 0+1 (this repo, IDEA-14):** spine — schema, connector interface,
  Switchyard connector, idempotent invites, credential block. **Extended per the
  owner's ask** with the Cloudflare Access connector and email/copy-paste
  delivery.
- **Follow-ups (SERV/ARGY tickets):** provision the Cloudflare Access API token
  + shared group; Argosy admin create-account endpoint + connector; Lyceum
  account model + connector; Deprovision; a web UI.

## Future direction: service spin-up (SERV-46)

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

See SERV-46 for the full assessment and the proposed epic breakdown.
