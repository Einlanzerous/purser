# Review instructions

Review-only guidance, higher priority than `CLAUDE.md`. `CLAUDE.md` describes
how this repo works; this file describes what a review of it is *for*.

## What this review is for

Purser is a credential-minting, access-granting, access-revoking service that
talks to four live upstreams and, on the spin-up axis, writes real edge
infrastructure. Almost nothing it gets wrong is fixed by re-running it. That is
the whole reason this reviewer exists here, and it is what the attention should
go to.

### What CI proves

| job | proves |
|---|---|
| `backend.yml` / `test` | `go build ./...`, `go vet ./...`, and `go test -race ./...` against a **real Postgres 16** service container — `PURSER_TEST_DATABASE_URL` is set, so the DB-backed store and migration tests actually run rather than skipping |

Assume that passed. Do not spend the review re-deriving that the code compiles.

### What CI does not prove — read this before crediting a green check

**A green `test` on a non-Go PR means nothing at all.** `backend.yml` is
path-filtered on `**.go`, `go.mod`, `go.sum`, `migrations/**` and its own file.
On anything else it runs a step named "Not applicable" that echoes one line and
reports a pass. PR #39, docs-only, went green in 23 seconds having compiled
nothing. On a docs, deploy, `.env.example` or workflow PR the check is
decoration.

**No linter and no formatter run anywhere in CI.** `.golangci.yml` exists and
enables `errcheck`, `staticcheck`, `revive`, `ineffassign`, `unused`, `misspell`
plus `gofmt`/`goimports` — and nothing invokes it. `make lint` is local-only and
falls back to `go vet` when `golangci-lint` is not installed. An unchecked error
return reaches `main` unremarked, so it is worth flagging where it matters.

**No test in this repo has ever contacted Cloudflare, Switchyard, Lyceum or
Argosy.** Every connector test is `httptest` against a fake the same author
wrote. A green suite says the client sends what the test author *believed* the
upstream expects — never that the upstream accepts it. For a connector change,
the question worth the review's budget is whether the request, the status-code
handling and the error taxonomy match the real API's contract, and the test
files cannot answer it.

## Ticket fidelity — check this first

Purser's tickets are unusually specific, and most carry a "Done when". When a
Switchyard ticket is linked, read its description and exit criteria before the
diff, and answer explicitly in the summary:

- Does the implementation satisfy the stated exit criteria, or only the easy
  subset of them?
- Did a requirement get silently dropped, narrowed, or deferred without saying?
  This project has twice lost the remaining half of a piece of work by closing
  the ticket that described it, so deferral without a new key is a finding, not
  a style note.
- Does the PR claim something is verified that the diff does not demonstrate?
  Note the shape this takes here: CI runs the tests, so "added tests" is
  credible — what it does not tell you is whether the test asserts the thing the
  ticket asked for. **PRSR-27 shipped a test asserting the wrong behaviour**: it
  locked in DNS publishing after a failed prerequisite. A passing test of the
  wrong behaviour is still a finding, and it is the finding this repo has
  actually produced.

A change that is clean code and wrong scope is a 🔴 **Important** finding. Say
which criterion is unmet and quote it.

When no ticket is linked, say so in one line and review the diff on its own
terms. Do not invent intent from the branch name.

## Severity

- 🔴 **Important** — grants access to the wrong person, revokes the wrong
  person's, records an outcome that did not happen, persists or transmits a
  plaintext secret, sends operator-facing content to an invitee, publishes a
  hostname before the thing gating it exists, mutates from a path documented as
  read-only, loses history the audit exists to read, or does not do what the
  ticket asked.
- 🟡 **Nit** — conventions, clarity, a comment that will mislead. Never blocking.
- 🟣 **Pre-existing** — real, not introduced here. At most two per review.

Cap nits at five; beyond that say "plus N similar" in the summary. A review that
buries one Important finding under twelve nits has failed at its job.

## Always check

**`CLAUDE.md`'s "Invariants — don't break these" is the checklist.** Read it
before the diff, and when a change touches one, name the invariant rather than
re-deriving it. Do not restate them here — they are maintained there, each one
learned from a `fix(...)` commit that got past exactly this pipeline. What
follows is the set of questions those invariants do not phrase as questions.

**Recording what did not happen.** The repo has two named states for this —
`revoked-not-recorded` and `applied-not-recorded` — because the inverse advice
is the dangerous one.

- Does a new path mark an account `deprovisioned`, or write a
  `service_resource` row, on anything short of a confirmed upstream success?
- Does a failure or an unreachable upstream get recorded as absence? `UpstreamUnknown`
  is deliberately distinct from `UpstreamNo`, and a failed `Inspect` is
  `unknown`, not "missing". A transient outage must never wipe access records or
  cause a second copy of something to be created.

**Read-only paths acquiring writes.** `Reconcile`, `audit`, `person list`,
`person show` and every `Inspect` are read-only by contract, and a version that
repairs as a side effect cannot be used to audit. Does the diff add a create, a
mint, a rotate, a revoke — or, for the roster commands, a connector call at all?

**Secrets.** `account.secret_hash` is sha256 and plaintext exists only in the
returned or emailed credential block.

- Does a new store query select `secret_hash` or `secret_ref` into a struct that
  `--json` will serialize? The roster's guarantee is that the field does not
  exist, not that no renderer prints it.
- Does new operator-facing text reach `RenderCredentialBlock`? The recipient's
  block and the operator's note are split at the source precisely so the emailer
  has nothing to get wrong.
- Does a new `PURSER_*` value get logged, or land in an error string that the
  API returns?

**Destructive paths default to preview.** `offboard` and `spinup.Ensure` both
preview by default and act only on `--apply`, and both share one code path so
the preview is exactly what the apply does. Does a new step read state at plan
time and *act* at plan time? Does a new flag default to acting?

**The two axes must not merge.** `internal/connector` is person-shaped and
idempotent per (person × service); `internal/spinup` is hostname-shaped and
idempotent per (hostname, kind). `spinup` deliberately does not import
`connector`. A PR that makes it, or that widens `connector.Input` with
hostname-shaped fields, is 🔴 whatever else it does.

**Migrations.** `migrations/NNNN_name.{up,down}.sql` are embedded and applied
automatically at boot.

- Is there a `.down.sql`, and does it actually reverse the up?
- Does a new `status` value have a matching CHECK-constraint migration?
  `provision_task.status` and `account.status` are constrained, not free text.
- Is a new uniqueness constraint case-insensitive where its neighbours are?
  `person.email` and `service_resource.hostname` both are.

**Configuration lands in three places or nowhere.** A new `PURSER_*` variable
needs `internal/config/config.go`, `.env.example` *and*
`deploy/construct-server.compose.yml`. Two of the three ships a service that
cannot be configured in production, and CI proves nothing about it — a PR
touching only the last two does not even compile anything.

## Verification bar

Report a finding only when you can point at the line that causes it and name the
concrete failure — the input, state, or sequence that produces the wrong
outcome. "This could be risky" is not a finding.

Behaviour inferred from a name is not evidence. If you find yourself writing
"this may not handle…", go read the implementation or drop it.

Purser's own version of that rule: **a claim about what an upstream does is not
evidence either.** The tests are fakes, so if a finding depends on how
Cloudflare, Switchyard, Lyceum or Argosy actually behaves — a status code, a
pagination shape, whether a PATCH replaces or merges — say that you are
reasoning from the API's documentation rather than from anything in this repo,
and say what would confirm it. That uncertainty is worth reporting; disguising
it as certainty is not.

## Re-reviews

Round three should be shorter than round one. After the first review of a PR:
report **new Important findings only**. No new nits, no restating open findings,
no re-raising something the author explicitly declined. Note in one line what
got fixed, then move on.

## Summary shape

Open with a one-line tally — `2 important, 1 nit` — or **No blocking issues**.
Then ticket fidelity in a sentence. Then findings, most severe first.

If the diff is clean, say so in one line and stop. Do not pad.
