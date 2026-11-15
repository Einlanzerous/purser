# Spin-up axis invariants: rationale and history (`internal/spinup`, PRSR-27)

`CLAUDE.md` carries each spin-up rule as a short statement. This file keeps the
reasoning, incident history and live-probe measurements behind them, moved out
of `CLAUDE.md` under SERV-317 with the text preserved. For the design narrative
see "Service spin-up" in `architecture.md`. One edit was made in the move: the
reference to `refuseContested` (removed from the code) is reworded in the prune-ownership bullet.

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
- **The teardown walk works from rows, and the rows are the plan** (PRSR-34).
  `Ensure` takes a spec and asks upstream what is there; `Teardown` takes a
  hostname and asks *Purser* what it put there, because the recorded id is the
  only thing it may target — deleting a record somebody made by hand is not a
  mistake re-running fixes. So there is no spec argument, no `Inspect`, and a dry
  run makes **no provisioner call at all** — not even a read-only one, which is
  `offboard`'s rule and stronger than `Ensure`'s: `Ensure` previews because one
  of its three steps rewrites shared state, and here every step is a deletion.
  The `Target` a provisioner is handed therefore carries only `Spec.Key` and
  `Spec.Hostname`, with `TunnelID` empty; `rec.ParentID` is the container the
  resource actually went into, which is the answer a teardown wants and a spec
  cannot give now that the tunnel is per-spec.
- **DNS comes off first, and `model.TeardownOrder()` is `KindOrder` reversed
  rather than a second list.** The argument is `KindOrder`'s read backwards: the
  record is what makes the hostname live, so pull the Access application while it
  still resolves and a gated service is reachable *ungated* — not
  self-announcing, unlike the 502 a removed ingress route gives. Derived rather
  than written out again, because the way a second literal fails is by removing
  the gate before the record with nothing anywhere reporting it. And ordering
  only closes the window when the earlier removal *landed*, so
  `teardownDependsOn` holds the other two behind DNS (`blocked`) exactly as
  `ServiceSpec.dependsOn` holds DNS behind them on the way up.
  It takes no spec and cannot, which costs one distinction: a *bookmark* Access
  app is deliberately not a DNS prerequisite going up, and a row records the kind
  rather than whether the application was a gate or a tile. Blocking both is the
  only answer available and the right direction to be wrong in — wrong about a
  bookmark costs a re-run, wrong about a gate costs an ungated service.
- **"Is this hostname still someone's?" is answered by two coordinates that must
  agree, not by Purser guessing.** A `service_resource` row proves Purser created
  something here; it does not prove the hostname is still that service's to
  remove, and a reassigned key, a hostname now fronting something else and a
  hand-edited record each want a different answer. So `teardown-service` requires
  `--service` as well as `--hostname` and refuses the **whole run** —
  `spinup.ErrHostnameNotThisService`, 409 over HTTP — when any active row
  disagrees, removing nothing and contacting nothing. That is `offboard`'s shape
  (`--email` required, always one target, no bulk mode) and the comparison is
  load-bearing rather than ceremonial: `UpsertServiceResource` overwrites
  `service_key` on conflict and `ensureOne` adopts on a mismatch, so a hostname
  that changed hands names its new owner from the first spin-up run. *Every*
  active row is checked, since a hostname whose kinds disagree with each other
  has been half-reassigned. A **removed** row from a former owner does not
  refuse: that is the history of a hostname that legitimately changed hands.
- **A removal that didn't happen must never be recorded as one**, and
  `removed-not-recorded` is its own status. Only a successful `Teardown` marks
  the row removed; `failed`, `unavailable`, `refused` and `blocked` leave it
  **active** so the next run retries — PRSR-17's invariant unchanged, because the
  lie outlives the error message and the row is what the next run reads. The
  inverse points the other way: the resource is gone and Purser holds an active
  row for it, which a later spin-up reads as something to adopt and a later
  teardown targets an id nobody can delete. It counts toward `NeedsAttention` for
  that reason even though the edge is correct.
- **`Teardown` returns a `spinup.Removal`, not a bare `error`.** The tunnel's
  concurrent-write note is what forced it: on the `Ensure` path that warning
  reaches an operator through `Resource.Warning`, and on this path it reached a
  server log and nothing else — so whoever ran the teardown was told nothing,
  about the one message saying another service may have just lost its route.
  Same split as on the way up: `Detail` describes this resource, `Warning` is
  about one that is not this one, and it is returned rather than logged because
  `log` and the CLI's summary share stderr.
- **A teardown plan cannot learn from an `Inspect` that never runs, so
  `TeardownChecker` exists.** Without it a plan reports `remove` for a step
  `--apply` then reports `unavailable`, and "the preview is exactly what the
  apply does" stops being true on the one command you cannot take back. It is
  `connector.RevokeChecker` one axis over, with the same rule: `CanTeardown`
  answers from capability and configuration alone, contacting nothing, and
  `Teardown` delegates to the same answer so the two cannot drift. It asks about
  the *empty* spec a teardown carries on purpose — Access's `available()` demands
  the members group id only for a gated spec, and a checker that wanted it would
  refuse to remove a bookmark tile over a credential nothing on this path reads.
  The per-direction half is the hand-fix sentence: the tunnel's "add the ingress
  rule in the dashboard" is exactly the wrong instruction to give somebody taking
  a service down, which is why `teardownUnavailable` exists beside `unavailable`.
  Found by running the binary, which is now the fourth time on this axis.
- **`Ensure` and `Teardown` must agree about who owns a shadowed ingress rule**
  (PRSR-34, out of PRSR-30's review). `planRoute`, meeting a rule for the
  hostname behind a catch-all or a wildcard, leaves it alone on a stated
  principle — a rule Purser did not write is not one it deletes — and names it in
  the plan. `withoutRoute` took every rule with the hostname and no path,
  reachable or not, so the same rule was removed without being mentioned; and it
  is *provably* not Purser's, since `assertWritable` makes it impossible to have
  written a route into a region cloudflared never reaches. It is somebody's hand
  edit, most plausibly a route they meant to work. `withoutRoute` now splits at
  `scanRoute`'s shadow, reports what it left, and the read-back checks the
  reachable region so the restraint is not misread as a removal that failed.
- **A malformed ingress document is refused on the teardown path too, before
  anything is concluded from it.** The write path already refuses it and the read
  path already refuses it; the teardown used to remove what it could see and, in
  the case with teeth, find *nothing* and return success — after which the
  orchestrator marked the row removed over a rule still in the file. "No rule for
  this hostname" out of a document nobody may read is not evidence, which is the
  revoked-not-recorded lie reached through a read rather than a write.
- **`--prune` is the orphan's command, and it is a *second* flag on purpose**
  (PRSR-46). An orphan is a resource at a hostname the spec is keeping up — a
  service that was `--access gated` and is now `--access none` — so
  `teardown-service` is the wrong tool twice over: it removes everything at the
  hostname, and it works from rows and has no spec, which is the only thing that
  can tell an orphan from a resource. `Ensure` can, so the command lives there.
  Both `--apply` and `--prune` are needed to remove anything: `--apply` is "act
  on this plan", `--prune` is "and the plan includes taking things away", and the
  overwhelmingly common operator never has to think about the second.
- **Pruning runs after the whole additive pass, and that ordering is the reason
  it is safe rather than a tidiness.** The case that decides it is a tunnelled →
  direct switch: the ingress route becomes an orphan while the DNS step
  *repoints* the record off `<tunnel>.cfargotunnel.com`. Drop the route first and
  the hostname 502s until the record catches up; repoint first and the route is
  already serving nobody when it goes. Running `KindOrder` to completion gets
  that for every spec without a second ordering rule to keep in step.
  The guard behind it is one rule, deliberately stronger than a per-kind
  dependency list: **a prune is held back when any step the spec *does* call for
  did not land** (`unmetPruneDeps`, reusing `inPlace`). `--prune` means "make the
  edge match this spec", so if the additive half didn't land the edge is not the
  one the spec describes, and removing what it no longer names is acting on a
  state nobody has. The concrete case is the ordering one gone wrong — a *failed*
  DNS repoint leaves the record pointing at the tunnel, so dropping the route
  takes a working service down — and the general rule catches it without needing
  to know it, which is what an enumeration of pairs would stop doing the moment a
  fourth kind arrives.
- **`prune` is a status, and `pruned-not-recorded` is not
  `applied-not-recorded`.** `--prune` changes what the run is *asking for*, so an
  extra resource stops being "nothing will be done about this" and becomes "this
  is going" — different instructions to a reader, which is what a status is for.
  Note this is not the distinction `--apply` makes: `--apply` never changes a
  status, because the plan is the first half of the apply. And the two
  not-recorded statuses point opposite ways: `applied-not-recorded` means
  something was *created* and Purser holds no id, so a teardown would miss it;
  `pruned-not-recorded` means something is *gone* and Purser holds a live-looking
  row, so a later run reads it as one to adopt. Same argument PRSR-34 made for
  keeping `removed-not-recorded` separate.
- **`orphaned` still does not count toward `NeedsAttention`, and the reason is
  now the durable one.** It has outlived two: "nothing orchestrates a teardown",
  then "PRSR-34's walk is whole-hostname". Neither survives `--prune`. It stays
  excluded because an orphan does not falsify the claim that method makes — "the
  spec is satisfied", and everything the spec asks for *is* in place. Reading it
  as "and nothing else is here" is the rounding-up the exclusion exists to stop.
  A run that passed `--prune` reports `prune` instead, which **is** counted by
  `Pending`: that run asked for the removal, so it is work outstanding rather
  than a state.
- **A prune removes only *this* service's resources, and the refusal is the
  whole run.** `Ensure` builds its record map from
  `ServiceResourcesForHostname`, which is keyed on hostname alone, so without a
  check every active row there is a prune candidate whatever service it is
  recorded to — and a spec naming `--access none` would delete *another*
  service's Access application, leave its hostname resolving, and mark the row
  removed so nothing mentions it again. That is the hole `checkOwnership` refuses
  on for a teardown, reached through the additive command. It reuses that
  sentinel (`ErrHostnameNotThisService`, 409 over HTTP), so one rule is stated
  once and both surfaces answer the same way.
  **Two narrower shapes were tried first and both are wrong, which is worth
  keeping because each looks obviously right.** Refusing each orphan on its own
  leaves the additive pass free to run — and *every* row-writing branch of
  `ensureOne` calls `s.record`, which stamps this spec's `service_key`. So the
  run rebound one kind while refusing the others, producing exactly the
  half-reassigned hostname `checkOwnership` refuses a whole teardown on, for
  **both** keys: it manufactured, in the act of refusing, the condition that
  stops the remedy its own message names. Holding only the `adopt` then fixed one
  branch of four and left `update` doing the same — and `update` is the *likelier*
  input, since a hostname genuinely held by another service has a record that
  does not match this spec.
  **The objection that sent it down that road was wrong, and is recorded so
  nobody re-derives it.** A whole-run refusal looked unfixable: nothing rebinds an
  *orphan's* `service_key`, because only `ensureOne`'s adopt path rewrites a row
  and an orphan is a kind the spec does not call for, so the row would refuse for
  ever with no command to type. That overlooked the remedy the refusal itself
  names. `teardown-service --service <owner>` removes the hostname's resources,
  and it works **precisely because nothing was written** — every row still names
  one service. Refusing early is what keeps that true; the narrower shapes are
  what broke it.
  That check asked the question of orphans only, on the grounds that a
  *called-for* kind whose row names another service is the reassignment an
  adopt exists to rebind. It is now asked of every row, by
  `checkSpecOwnership` — see the next bullet — which replaced the earlier
  orphan-only helper: a prune is covered by the general rule rather than being a
  special case of it.
- **A spin-up writes only its own service's edge, and a hostname moves only
  when its previous owner is named** (PRSR-48). `Ensure` is keyed on hostname:
  `ensureOne` asks the provisioner what is there and makes it match the spec,
  and never consulted `service_key`. When upstream already matched, the
  mismatch was the deliberate reassignment adopt. When it did not, the same
  mismatch was an `update`, and `--apply` wrote one service's spec onto
  another's resources — a `--access bookmark` spec against somebody's *gated*
  application strips its policies and leaves it resolving, which is a service
  admitting everyone with nothing anywhere reporting it. The plan did say
  `update`, which on a service you meant to update is not a line that invites
  suspicion: PRSR-41's lesson, met from the other side.
  So `checkSpecOwnership` asks the teardown's question one command over, and
  refuses the **whole run, before any `Inspect`**, on the plan as much as the
  apply, when any active row at the hostname names a service the request did
  not — `ErrHostnameNotThisService` again, exit 2 / 409, so all three surfaces
  answer the same way and the comparison is stated once (`recordedToOthers`).
  Whole-run for the reason PRSR-46 found over three rounds: every row-writing
  branch of `ensureOne` stamps this spec's `service_key`, so a run that refused
  one kind and ran the rest manufactures the half-owned hostname that refuses
  both owners' teardowns. `--reassign-from KEY` (`reassign_from` over HTTP) is
  the stated way to move a hostname: rows recorded to exactly that service are
  treated as the spec's own, so adopt rebinds and update writes as before, and
    **orphans move too** (`rebindOrphan`, a row write and nothing upstream), so a
  reassignment never *leaves* the hostname half-owned — the run after a move
  needs no flag, and a teardown as the new owner works. A partial failure still
  can, since a called-for kind moves only through `ensureOne`'s row-writing
  branches and a failed `Inspect` writes none; re-running with the same
  previous owner repairs it, because both keys are then permitted. Rows that
  name **two** other services — what that leaves behind if nobody re-runs —
  can be moved or torn down as no single owner, so that refusal names the one
  command that permits two keys: a spin-up as one of them naming the other,
  which collapses the rows onto one key (purser#60 review; a teardown was the
  first wording, and would itself have refused). Two coordinates that must agree, the
  second one typed rather than inferred: `offboard`'s shape, and the person
  axis's rule that only `person add --rename` may change a name. Naming a
  previous owner that holds nothing here is not an error, so re-running the
  command that moved a hostname says `ok`; naming the spec's own service is
  `ErrReassignFromSelf` (400), refused rather than ignored because a flag that
  does nothing is likelier a wrong key. Under `--prune` the orphan is
    re-attributed *first* and pruned second, so a prune that does not land leaves
  a row its new owner can still remove. A rebind is work `--apply` does, so
  `Pending` counts an `orphaned` line carrying one (`StepFinding.Rebind`): a
  plan whose only work is a row moving must not say "nothing to do" over it.
  **The guard is keyed on rows**, so a hostname Purser has *never* recorded is
  not guarded by it: `findApp` still selects whatever whole-host application
  serves the name and a bookmark spec still converts it, reported as `update`.
  The plan naming the application is the whole defence there, as it is for any
  hand-made resource this axis meets — the row is what makes ownership
  checkable, and only Purser writes rows.
- **DETAIL describes the resource; ACTION carries the verb.** The prune line
  first read "… — removing it (app-77e1 …)", which is fine under `prune` and a
  lie under every other status the path produces, since `pruneOne` leaves Detail
  alone when it declines. A live run against an unconfigured deployment printed
  `unavailable` beside "removing it" — a plan promising in one column what it
  refuses in the next. Found by running the binary, which is now the fifth time
  on this axis.
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
  not a guarantee — and this predicate gates a full-replacement PUT. PRSR-47
  found the mechanism: Cloudflare **normalises `self_hosted_domains` to
  `destinations[].uri` on write** (two disagreeing bodies came back agreeing,
  and a `domain` absent from `destinations` is refused with 12130), so
  `hostAmbiguous` cannot be produced through the API as of 2026-09-05. The
  branch stays: that is also an observation about today, and refusing is the
  safe direction.
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
  `Teardown`'s third documented outcome. PRSR-34's walk reads a `nil` there as
  licence to mark the `service_resource` row removed, after which the remaining
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
- **A correct URL is not a correctly-shaped asset** (PRSR-43). Third variant of
  one failure, and each defeats the check before it: a stored `logo_url` can be a
  lie (answered by the fetch, PRSR-29); a *working* URL can be the wrong asset
  (answered by asking Placard which one is the tile mark, PRSR-37); and a
  *correct* URL can be the right asset in the wrong proportions. Placard's
  canonical argosy mark was 1169×512 — the only non-square one in the repo,
  against 0.94:1 for switchyard and 1.00:1 for lyceum and placard — and
  Cloudflare's App Launcher tile is square and **fills** rather than fits, so it
  cropped the outer ships off the glyph. Placard's own page *fits*, so the same
  file looked right there and wrong in the launcher, with nothing anywhere
  reporting a problem.
  `checkLogo` reads the dimensions off the response it was already making —
  `measure` sniffs the format from the bytes rather than trusting `Content-Type`,
  so a header that lies produces no measurement instead of a wrong one — and
  `logoShape.note` prints when width ÷ height leaves 0.8–1.25. That band is
  symmetric because the tile has no preference about which way a mark is wrong;
  0.8 is `1/1.25` and `TestLogoShape_TheBandIsSymmetric` says so rather than
  leaving it to look like a second judgement.
  **It is emphatically not a fourth outcome of `logoVerdict`.** It is never
  drift, because there is nothing `--apply` could write that would fix it — a
  plan calling it drift would promise an update that changes nothing, for ever —
  and never fatal, because a gated app is a DNS prerequisite and a letterboxed
  icon still beats two grey initials. The icon is written and the plan says what
  the tile will do to it. An **unmeasured** mark (an SVG, a truncated body, a
  format nothing decodes) reports nothing: never treat unmeasurable as bad, which
  is `logoUnknown`'s rule one field over. Fixing the asset is Placard's job —
  placard#6 padded argosy's viewBox to 1:1 rather than cropping tight; **the
  authoring-end guard, an aspect check in Placard's own `checker`, is
  PRSR-44** and is not done. It has a key rather than this sentence for the
  reason `Teardown` does: prose in the repo that isn't going to do the work,
  attached to a ticket about to close, is how the remaining half gets lost.
  The reason it belongs here *as well* is that Purser knows the **surface**. That
  the tile is square is a fact about Cloudflare, this is the package that talks to
  Cloudflare, and this is the last point before the URL is written.
- **The Access teardown confirms absence by reading, not by an error code.**
  `dnsRecordNotFound` works because 81044 was observed; when this was written
  there was no observed Access equivalent, and the rule above forbids guessing
  one. There is one now — PRSR-47 saw **11021
  `access.api.error.unknown_application`** on both GET and DELETE of a deleted
  application — and the re-read is kept anyway, because it asserts what
  `Teardown` actually claims rather than what one code says. So a failed delete
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
  out first"; the production token answers `GET /zones/{zone_id}` with `name:
  zerogravity.industries, status: active` — that object read is the one
  `preflight` makes, and the list endpoint answers too.
- **So the pre-flight runs first, and the create-then-delete is the backstop
  behind it** (PRSR-39). `preflight` resolves `PURSER_CF_ZONE_ID` to the zone's
  *name* and refuses a hostname that is not inside it, on `Inspect` as well as
  `Ensure`, before the record lookup — so the operator reads it in the **plan**,
  no wrong record ever resolves, and the most alarming line this provisioner can
  print ("Purser deleted a record it had just made") is off the ordinary path.
  Refused, not unknown: the read succeeded and re-running says the same thing for
  ever. What needs fixing is the *spec*, which is a third thing neither status
  names — but `unknown` says "re-run" and `unavailable` says "set an env var",
  and only the refusal's message is the actual fix.
  The two guards ask **different questions** and that is why both stay:
  pre-flight asks what the spec said, `wrongName` asks what Cloudflare *did*. So
  the backstop still catches a normalisation surprise a pre-flight cannot
  predict, it is the half that has actually been exercised live (PRSR-42), and it
  is the only guard left in the one state the pre-flight waves everything
  through — **a zone that could not be read**, which is never evidence the
  hostname is wrong. Usually the same failure takes the record lookup with it and
  the step reports `unknown` anyway, but that is a tendency, not a property: a
  failure specific to `GET /zones/{id}` leaves the pre-flight silently inert, and
  the backstop is what makes that acceptable rather than any self-healing. Only a
  *successful* zone read is memoised, so one timeout cannot disable the check for
  the life of a `purser serve`; and a 200 naming no zone counts as a failed read,
  since an empty zone name would otherwise suffix-match every hostname alive. The zone name is derived from the id and **never
  configured** beside it: two settings that can disagree just relocate the
  mismatch this exists to catch.
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
