# Build identity on `/healthz` (PRSR-32): purser-specific rationale

The generic rules (bare semver, full 40-char sha or `null`, never guess, blank
build args) live in `construct-server/PRINCIPLES.md` section 4. This file keeps
what is specific to purser's `publish.yml`, Makefile and `internal/version`,
moved out of `CLAUDE.md` under SERV-317 with the text preserved.

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
- **`VERSION` is passed only on a tag that carries a version**, and on that path
  the binary and the image label agree *by construction*: `publish.yml`'s `ver`
  step resolves one output, `stamp`, which feeds `build-args: VERSION`, and the
  `type=semver` entries that produce the label match on exactly the refs that
  make `stamp` non-empty. One value, not two conditions kept in step by hand.
  **The agreement is about the release path only, and the reason is worth
  knowing before you add a tag entry.** `org.opencontainers.image.version` is not
  set by `type=semver` as such — metadata-action sets it from `version.main`, the
  value of the **highest-priority tag that matched**. On a release the semver
  entry (priority 900) wins and the label is the bare version. On a push to main
  the `type=raw` entry wins, so the label is the literal `latest` against a
  binary stamped blank and reporting `dev`; on a `vnext` tag it is `sha-<short>`
  against the same `dev`. Those are not agreements — they are an unstamped build
  against a moving tag, which is the honest state for both, and nothing compares
  them (see the `:latest` bullet below for why dev is safe).
  The trap: **adding any tag entry with a priority above 900 moves `version.main`
  off the semver value and breaks the agreement on the release path too**, which
  is the one place this invariant is load-bearing.
  **Never stamp `steps.meta.outputs.version`.** That output is only the
  highest-*priority* tag that matched, so it silently becomes `latest` or
  `sha-<short>` depending on which entries fired, and storing a moving tag as an
  identity means comparing it against the label and disagreeing for ever. It was
  read here until SERV-108.
  A prerelease is stamped for exactly this reason: `type=semver` matches
  `v0.17.0-rc.1` and labels the image `0.17.0-rc.1`, so refusing to stamp it
  would put `dev` in a binary inside an image the ledger reads as versioned.
  `GIT_SHA` goes on **both** paths: a main build has a real commit and
  should say so, it just has no release to name. The sha is the full 40
  characters, because the cross-service comparison is an equality test and not
  a prefix match, and an absent one marshals to JSON `null` rather than `""` —
  "recorded no commit" is a different claim from "built at the empty commit".
- **`:latest` is main's tip, and has exactly one writer** (SERV-108).
  `flavor: latest=false` is load-bearing: `docker/metadata-action` defaults to
  `latest=auto`, which emits `:latest` on any `type=semver` match *independently*
  of the `type=raw` entry — so a release commit published `:latest` twice, from
  its push to main and from its tag build, and whichever runner finished last
  won. On 2026-08-28 the main build won `v0.16.0` by 30 seconds, leaving
  `:latest` labelled `version=latest` with a binary reporting `dev`.
  construct-server's dev tier pins `latest`, so that coin flip decided what dev
  purser claimed to be.
  **The main build is now the sole writer, which makes dev's image consistently
  `version=latest` / `/healthz: dev` instead of intermittently so.** That is the
  intended state, not a residue of the fix: dev runs main's tip, main has no
  release to name, and `dev` is the honest answer. It is safe because nothing
  compares the two for dev — the per-merge dispatch deliberately carries no
  `version`, which keeps construct-server's ledger-enrichment branch switched
  off, so no claim is filed for dev that an observation could contradict. **If a
  dev claim ever starts being filed, this is the thing that breaks**, and the
  fix is a real version on main builds, not a relabelling.
  **The estate has not settled which way `:latest` should point.** Lyceum fixed
  the identical race the other way (LYCM-121): there `:latest` means the last
  release and main gets only `:sha-<short>`. Both are internally consistent and
  they cannot both be right while dev pins `latest` for every service and is
  supposed to run what just merged. Tracked as an open decision on SERV-108 —
  check it before changing this file's `enable=` conditions.
- **The Makefile's `dev` fallback lives inside the subshell, before the pipe.**
  `git describe ... | sed 's/^v//' || echo dev` never fires: `||` binds to the
  *pipeline*, whose status is `sed`'s, and `sed` exits 0 on empty input. An
  untagged checkout or a source tarball then linked a blank version.
