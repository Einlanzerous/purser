package spinup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/model"
)

// Standing up a service creates real edge infrastructure, so it inherits
// `offboard`'s default rather than `invite`'s (PRSR-27, settled here so the DNS,
// Access and tunnel provisioners cannot disagree about it).
//
// `invite` acts by default because granting access twice is merely wasteful.
// This reports by default and acts only under Apply, because one of its three
// steps — appending a tunnel ingress rule — is a read-modify-write of a single
// document holding every *other* service's routes, and the mistake it can make
// is unrouting them. Creation being additive and idempotent argues the other
// way, but that argument covers two of the three steps and the third is the one
// with the blast radius.
//
// The preview and the apply walk the same code path. Every step's action is
// decided from one Inspect call, which is read-only by contract; --apply is that
// same decision followed by the write. So a plan is not a description of what an
// apply would probably do, it is the first half of the apply itself — the
// property `offboard` and `audit` both have, and the only thing that makes
// previewing worth the extra flag.
//
// The other half of that property is honesty about what is already there. A
// spin-up tool that cannot recognize an existing deployment is one nobody will
// point at production, so an already-correct resource that Purser never recorded
// is *adopted* rather than recreated: the run writes the row and touches nothing
// upstream.

// Store is the persistence the orchestrator needs. *store.Store satisfies it;
// tests supply an in-memory fake.
//
// It is deliberately narrow, and deliberately free of a not-found sentinel — the
// lookup returns a list — so this package depends on internal/model alone and
// the provisioner packages that import it don't pull in the store.
type Store interface {
	// ServiceResourcesForHostname returns every recorded resource for a
	// hostname, including removed ones.
	ServiceResourcesForHostname(ctx context.Context, hostname string) ([]model.ServiceResource, error)
	// UpsertServiceResource records a resource as active.
	UpsertServiceResource(ctx context.Context, r model.ServiceResource) (model.ServiceResource, error)
	// MarkServiceResourceRemoved records that a recorded resource is gone.
	//
	// Separate from an upsert-with-a-status because only one of the two may ever
	// be called speculatively: this one asserts the resource is gone *upstream*,
	// and the invariant offboard learned the expensive way (PRSR-17) applies
	// here unchanged — a teardown that didn't happen must never be recorded as
	// one, because the lie outlives the error message and the next run reads the
	// column.
	MarkServiceResourceRemoved(ctx context.Context, id uuid.UUID) error
}

// Service orchestrates spin-ups over a provisioner registry and store.
type Service struct {
	store    Store
	registry *Registry
	tunnels  TunnelSet
}

// Option customizes a Service at construction.
type Option func(*Service)

// WithTunnels supplies the ref → id map for tunnel resolution. Without it, a
// tunnelled spec is refused for want of a configured tunnel, which is the
// correct answer for a deployment that has set no tunnel id.
func WithTunnels(ts TunnelSet) Option {
	return func(s *Service) { s.tunnels = ts }
}

// New builds a spin-up Service.
func New(st Store, reg *Registry, opts ...Option) *Service {
	s := &Service{store: st, registry: reg}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// StepStatus is what happened to one resource, or what would happen under
// Apply.
//
// These are statuses, not one status plus modifiers. The person axis spent a
// while with a Pending bool riding alongside TaskFailed, and every consumer that
// bucketed by status had to remember the bool existed (PRSR-21); each value here
// is a distinct instruction to whoever is reading the report.
type StepStatus string

const (
	// StepOK — upstream already matches the spec and Purser's record agrees.
	StepOK StepStatus = "ok"
	// StepAdopt — upstream already matches the spec, but Purser holds no record
	// of it (or holds a stale one). Apply writes the record and makes NO
	// upstream call. This is what "recognize an existing deployment" means in
	// practice: Argosy's edge predates this axis entirely, and re-creating it
	// to gain a row would be the wrong way to learn its ids.
	StepAdopt StepStatus = "adopt"
	// StepCreate — nothing is there; apply creates it.
	StepCreate StepStatus = "create"
	// StepMissing — Purser recorded this resource and upstream does not have
	// it. Apply recreates it, so the action is a create; the *news* is not.
	//
	// Distinct from StepCreate because something Purser created was removed
	// outside Purser, and a report that calls that "create" never says so. It is
	// this axis's AccountStale, with one difference: marking the row there
	// re-arms provisioning, because the invite orchestrator reads the row —
	// here Ensure reads upstream, so there is nothing to re-arm and nothing to
	// mark. The row is rebound to whatever the recreate returns.
	StepMissing StepStatus = "missing"
	// StepUpdate — something is there and does not match the spec; apply
	// updates it in place. Kept distinct from create because on the tunnel this
	// is the read-modify-write of shared state, and an operator reading a plan
	// should see the difference.
	StepUpdate StepStatus = "update"
	// StepSkipped — this spec doesn't call for the kind at all (a direct spec
	// has no ingress route). Reported rather than omitted, so silence about a
	// step never has to be interpreted.
	StepSkipped StepStatus = "skipped"
	// StepOrphaned — this spec doesn't call for the kind, but Purser recorded
	// one here on an earlier run and never removed it, so it is presumably still
	// live. Distinct from skipped: the same line would otherwise say "nothing to
	// think about" for a resource that is still serving traffic.
	//
	// Reported and not acted on, unless the run asked for it — see StepPrune.
	StepOrphaned StepStatus = "orphaned"
	// StepPrune — an orphan on a run that passed Prune, so this run removes it.
	//
	// A separate status from StepOrphaned rather than the same one plus a flag,
	// and the distinction is not the one --apply makes. `--apply` never changes a
	// status: `create` reads `create` on both paths, because the plan is the
	// first half of the apply. `--prune` changes what the run is *asking for*,
	// so an extra resource genuinely stops being "nothing will be done about
	// this" and becomes "this is going". Those are different instructions to
	// whoever reads the line, which is what a status is for (PRSR-46).
	StepPrune StepStatus = "prune"
	// StepPrunedNotRecorded — a prune removed the resource and the row saying so
	// did not land.
	//
	// Not StepAppliedNotRecorded, which points the opposite way: that one means
	// something was *created* and Purser holds no id for it, so a teardown would
	// miss it until a later run adopts it back. This means something is *gone*
	// and Purser holds a live-looking row for it, so a later run reads it as a
	// resource to adopt and a later teardown targets an id nobody can delete.
	// Same argument PRSR-34 made for keeping TeardownRemovedNotRecorded separate.
	StepPrunedNotRecorded StepStatus = "pruned-not-recorded"
	// StepBlocked — this step would have acted, and was held back because a step
	// it depends on is not in place. See ServiceSpec.dependsOn.
	//
	// It is what makes KindOrder more than a preference. Ordering puts DNS last
	// so a gated service never resolves before its Access application exists,
	// but ordering alone only closes that window when the earlier step
	// *succeeded* — publish the record anyway after a failed, unavailable or
	// unreadable Access step and the service is live and ungated, which is the
	// one outcome this axis must not produce quietly.
	//
	// Only creates and changes are blocked. A resource that already matches the
	// spec is reported normally: it is already published, so withholding a row
	// (or a report line) protects nobody and hides the state.
	StepBlocked StepStatus = "blocked"
	// StepUnavailable — no provisioner for the kind, or one that isn't
	// configured. Nothing broke and nothing was done; mirrors
	// model.TaskUnavailable.
	StepUnavailable StepStatus = "unavailable"
	// StepUnknown — the current state could not be *read*, so nothing may be
	// decided from it. Never collapsed into "absent": acting on an unverifiable
	// answer is how a spin-up creates a second copy of something, and on the
	// tunnel it would mean rewriting a shared document from a read that just
	// failed. Apply does not act on an unknown step.
	//
	// This is the transient half, and re-running is the whole fix. The
	// permanent half is StepRefused.
	StepUnknown StepStatus = "unknown"
	// StepRefused — the read *succeeded* and came back with something the
	// provisioner will not act on. Apply does not act on it either.
	//
	// Split from StepUnknown because the two want opposite responses from an
	// operator, and until PRSR-31 the difference lived in the Err string. A
	// failed read says "run it again". This says "go and fix the thing
	// upstream, because running it again will print this until you do" — the
	// tunnel's ingress document with a catch-all that is not last, or a tunnel
	// that is locally managed and therefore is not serving the document the API
	// returns (PRSR-30).
	//
	// It is a status rather than a flag beside `unknown` for the reason PRSR-21
	// removed `TaskFailed` + `Pending bool` from the person axis: a status
	// carrying a modifier makes every consumer that buckets by status remember
	// the modifier exists, and the note filed under "what failed" then had to be
	// relabelled at the point of rendering. The one thing this distinction is
	// ever consulted for is the difference, so it switches on a status.
	//
	// Distinct from StepUnavailable in the other direction: unavailable is
	// Purser's own configuration missing a credential, which the operator fixes
	// here. Refused is upstream being in a shape nobody can safely write to,
	// which they fix there.
	//
	// Every one of these comes from a provisioner — IsRefused on an Inspect,
	// Ensure or Teardown call. PRSR-46 briefly widened it to cover a `--prune`
	// meeting another service's record, and that is no longer true: an ownership
	// disagreement refuses the whole run with ErrHostnameNotThisService, before
	// any step is decided, so it produces no finding at all (see
	// refuseContested).
	StepRefused StepStatus = "refused"
	// StepFailed — the write was attempted and errored. Nothing is recorded,
	// so a re-run reconsiders the step from scratch.
	StepFailed StepStatus = "failed"
	// StepAppliedNotRecorded — upstream was changed and the row recording it
	// did not land.
	//
	// Its own status for the reason offboard's revoked-not-recorded is
	// (PRSR-17): it points the opposite way from failed. Failed means nothing
	// was created; this means something was, and Purser doesn't know its id — so
	// a teardown would miss it until a later run adopts it back.
	StepAppliedNotRecorded StepStatus = "applied-not-recorded"
)

// StepFinding is one resource kind's verdict.
type StepFinding struct {
	Kind        model.ResourceKind
	DisplayName string
	Status      StepStatus
	// Detail describes what is upstream, or what was just written — the line an
	// operator reads to check the plan is what they meant.
	Detail string
	// ExternalID is the resource's upstream id, when it has one. Empty for a
	// tunnel route by nature, not by omission.
	ExternalID string
	// Warning is trouble around a step that nonetheless succeeded — see
	// Resource.Warning. Distinct from Err, which belongs to a step that did not
	// do what it said.
	Warning string
	// Applied reports that this run changed something — upstream, the record, or
	// both. Always false on a dry run.
	Applied bool
	Err     string
}

// Request is one spin-up.
type Request struct {
	// Spec describes the service's edge. Validated before anything runs.
	Spec ServiceSpec
	// Apply turns the plan into writes. False is a pure dry run: every upstream
	// call made is an Inspect, which mutates nothing.
	Apply bool
	// Prune asks the run to remove what the spec no longer calls for — the
	// resources otherwise reported as `orphaned` and left alone (PRSR-46).
	//
	// A separate flag from Apply, and both are needed to remove anything. Apply
	// is "act on this plan"; Prune is "and the plan includes taking things
	// away", which is a different question and the only destructive thing this
	// orchestrator does. An operator who wants the additive half and not the
	// subtractive one — the overwhelmingly common case — never has to think
	// about it.
	Prune bool
	// ReassignFrom names the service this hostname is being moved away from
	// (PRSR-48). Empty on every ordinary run.
	//
	// A spin-up writes only its own service's edge: an active row at the
	// hostname recorded to any other service refuses the whole run, on the plan
	// as well as the apply and before any Inspect (see checkSpecOwnership).
	// Naming the previous owner is the stated way past that. Rows recorded to
	// exactly this key are then treated as the spec's own, so a resource that
	// already matches is adopted — a row rebind, nothing upstream — and one that
	// differs is updated, which is what moving a hostname means. It is
	// offboard's shape: two coordinates that must agree, the second one typed
	// rather than inferred, and the person axis's rule that only `person add
	// --rename` may change a name, with a hostname in place of the name.
	//
	// Naming a service that holds nothing here is not an error, so re-running
	// the command that moved a hostname reports `ok` rather than refusing.
	ReassignFrom string
}

// Result is the whole plan, or the whole apply.
type Result struct {
	// Spec is the normalized spec the run worked from, not the one passed in.
	Spec     ServiceSpec
	Findings []StepFinding
	Applied  bool
	// Pruned echoes the request's Prune, so a reader can tell an `orphaned` line
	// that was never going to be acted on from one on a run that asked.
	Pruned bool
}

// Counts summarizes findings by status, for a one-line summary.
func (r *Result) Counts() map[StepStatus]int {
	out := map[StepStatus]int{}
	for _, f := range r.Findings {
		out[f.Status]++
	}
	return out
}

// Changed reports how many steps this run actually changed. Zero on every dry
// run, and zero on an apply against a service that was already correct.
func (r *Result) Changed() int {
	n := 0
	for _, f := range r.Findings {
		if f.Applied {
			n++
		}
	}
	return n
}

// Pending reports how many steps still want doing — the count that makes
// "nothing to do" distinguishable from "re-run with --apply". Statuses that need
// a human (unavailable, refused, unknown, failed) are not counted here, and
// neither is blocked: re-running with --apply does not fix any of them, because
// the reason they didn't happen was never the missing flag.
//
// So a zero here is NOT the claim that the edge is as the spec asks — only that
// the flag would add nothing. Anything reporting an overall verdict has to
// consult the statuses too; reading this alone as success is what had the CLI
// sign off an unavailable DNS step as a service that was up (PRSR-31).
func (r *Result) Pending() int {
	n := 0
	for _, f := range r.Findings {
		switch f.Status {
		case StepCreate, StepUpdate, StepAdopt, StepMissing, StepPrune:
			if !f.Applied {
				n++
			}
		}
	}
	return n
}

// NeedsAttention reports the steps in a state a person has to resolve — the
// answer to "is this service's edge as the spec asks?", which neither Pending
// nor Changed answers.
//
// It exists because the two counts that *look* like a verdict are not one.
// Pending excludes every status here on purpose (--apply fixes none of them), so
// a plan against an unconfigured deployment reports `pending: 0, changed: 0` and
// nothing anywhere contradicts it — which is exactly how the CLI came to print
// "the edge already matches this spec" over a hostname that does not resolve
// (PRSR-31).
//
// Living here rather than in a renderer is the point: the CLI's exit code and
// the HTTP response answer it from the same list, so the two surfaces cannot
// drift about what counts as fine. `blocked` is included — the step did not
// happen, and the hostname does not work — even though what needs attention is
// really its prerequisite.
//
// `orphaned` is deliberately NOT included, and it is the one exclusion worth
// arguing rather than assuming. Everything the spec asks for is in place; what
// is extra is a resource this spec no longer calls for, left over from an
// earlier one. So the claim this answers is "the spec is satisfied", not
// "nothing else is here" — the honest weaker one, and the caller-facing wording
// must not round it up.
//
// The exclusion has now outlived two different reasons for it, which is worth
// recording because the durable one is the third and it is not about tooling at
// all. It was first excluded because nothing orchestrated a teardown; then
// because PRSR-34's walk is whole-hostname and an orphan sits at a hostname that
// is *staying up*; and PRSR-46 has since given it a command (`--prune`), so
// neither reason survives. It stays excluded anyway, because **an orphan does
// not falsify the claim this method makes.** The claim is "the spec is
// satisfied", and everything the spec asks for is in place — what is extra is
// extra. Reading it as "and nothing else is here" is the rounding-up the doc
// comment above forbids.
//
// That is also the answer to the obvious objection. Now that `--prune` exists
// there *is* something to type, so the old "no command to type" argument would
// have flipped this to counted; but a run that deliberately keeps a resource the
// spec no longer names would then report trouble and exit non-zero for ever, and
// an operator who is doing that on purpose is exactly who learns to ignore the
// signal — `offboard`'s SSO warning is the precedent.
//
// It is reported loudly on its own line either way, since nothing else in the
// report would mention a resource that is still serving traffic. A run that
// passed Prune reports `prune` instead, which *is* counted by Pending: that run
// asked for the removal, so it is work outstanding rather than a state.
func (r *Result) NeedsAttention() []StepFinding {
	var out []StepFinding
	for _, f := range r.Findings {
		switch f.Status {
		case StepUnavailable, StepRefused, StepUnknown, StepBlocked, StepFailed,
			StepAppliedNotRecorded, StepPrunedNotRecorded:
			out = append(out, f)
		}
	}
	return out
}

// inPlace reports whether this step leaves its resource in place for a later
// step to depend on. It is the predicate behind StepBlocked.
//
// StepCreate and StepUpdate count because they only *survive* as those statuses
// when the write landed — an apply that failed reports StepFailed — and on a dry
// run they describe what the apply is going to do, which is what makes a plan's
// DNS line read "create" rather than "blocked" when the whole spec is new.
// StepAppliedNotRecorded counts too: the edge changed, and only the bookkeeping
// didn't.
//
// Everything else is false, including a failed adopt — where upstream was
// already correct and only the row write failed. Over-blocking there is
// deliberate: Purser's records are wrong at that moment, and publishing a
// hostname is the wrong response to not knowing what you have.
func (f StepFinding) inPlace() bool {
	switch f.Status {
	case StepOK, StepAdopt, StepCreate, StepUpdate, StepAppliedNotRecorded:
		return true
	}
	return false
}

// Ensure runs a spin-up, or — by default — reports what it would do.
//
// It returns an error only for a request that cannot be attempted at all: an
// invalid spec, an unresolvable tunnel, or a failed read of Purser's own
// records. Everything a provisioner does or fails to do becomes a finding, so a
// partially-broken run still returns a full report rather than one error line
// and no idea what landed. Per-resource failures must not abort the spec, for
// the same reason per-service failures don't abort an invite — and here the
// earlier steps may already have changed the edge.
func (s *Service) Ensure(ctx context.Context, req Request) (*Result, error) {
	spec, err := req.Spec.Validate()
	if err != nil {
		return nil, err
	}
	from := normalizeKey(req.ReassignFrom)
	if from != "" && from == spec.Key {
		return nil, ErrReassignFromSelf
	}

	target := Target{Spec: spec}
	if spec.Mode == ModeTunnelled {
		// Resolved once, before anything runs, so both tunnelled steps write to
		// and point at the same tunnel — and so a spec naming a tunnel nobody
		// configured is refused before it has half-built an edge.
		id, err := s.tunnels.Resolve(spec.Tunnel)
		if err != nil {
			return nil, err
		}
		target.TunnelID = id
	}

	recorded, err := s.store.ServiceResourcesForHostname(ctx, spec.Hostname)
	if err != nil {
		return nil, err
	}
	// Only active rows count as "Purser recorded this". A removed row is the
	// history of a teardown, and treating it as a record would have an adopt
	// look like an ok.
	active := make(map[model.ResourceKind]model.ServiceResource, len(recorded))
	for _, r := range recorded {
		if r.Status == model.ResourceActive {
			active[r.Kind] = r
		}
	}

	// Findings so far, so a step can see whether the steps it depends on are in
	// place. KindOrder is an apply order, so a dependency has always been
	// decided by the time its dependent is reached.
	// A prune on a hostname holding somebody else's resource is refused whole,
	// before anything is read or written. See refuseContested.
	if err := checkSpecOwnership(spec, active, from); err != nil {
		return nil, err
	}

	done := make(map[model.ResourceKind]StepFinding, len(model.KindOrder))
	for _, kind := range model.KindOrder {
		rec, hasRec := active[kind]
		var f StepFinding
		if !spec.callsFor(kind) {
			f = notApplicable(kind, spec, rec, hasRec, s.registry, req.Prune)
			if hasRec && rec.ServiceKey != spec.Key {
				// Only reachable under a permitted reassignment — any other
				// owner refused above — so this is the previous owner's orphan,
				// and it moves with the hostname.
				f, active[kind] = s.rebindOrphan(ctx, target, kind, rec, f, req.Apply)
			}
		} else {
			f = s.ensureOne(ctx, target, kind, rec, hasRec, req.Apply, unmetDeps(spec, kind, done))
		}
		done[kind] = f
	}

	// Pruning runs after the whole additive pass, and the order is the reason it
	// works rather than a tidiness (PRSR-46).
	//
	// The case that decides it is a service moving from tunnelled to direct: the
	// ingress route becomes an orphan while the DNS step *repoints* the record
	// from <tunnel>.cfargotunnel.com to the direct upstream. Drop the route first
	// and the hostname 502s until the record catches up; repoint first and the
	// route is already serving nobody when it goes. Running the additive pass to
	// completion gets that for free, on every spec, without a second ordering
	// rule to keep in step with KindOrder.
	if req.Prune {
		for _, kind := range model.TeardownOrder() {
			if done[kind].Status != StepPrune {
				continue
			}
			done[kind] = s.pruneOne(ctx, target, kind, active[kind], done[kind], req.Apply, unmetPruneDeps(spec, done))
		}
	}

	// Built at the end rather than appended as we go, so the report stays in
	// KindOrder while the prune pass above walks TeardownOrder. A reader gets one
	// line per kind in the order a spin-up applies them, which is the order every
	// other surface on this axis lists them in.
	res := &Result{Spec: spec, Applied: req.Apply, Pruned: req.Prune}
	for _, kind := range model.KindOrder {
		res.Findings = append(res.Findings, done[kind])
	}
	return res, nil
}

// ErrReassignFromSelf is a request naming the spec's own service as the one a
// hostname is being reassigned from. A hostname moves from somebody else or not
// at all, and a flag that means nothing is likelier a wrong key than a no-op —
// so it is refused rather than ignored, and it is a sentinel so the HTTP
// surface can call it the caller's mistake (400) without matching on a message.
var ErrReassignFromSelf = errors.New("spinup: reassign-from names this spec's own service — a hostname is reassigned from somebody else, or not at all")

// checkSpecOwnership refuses a spin-up of a hostname whose active rows are
// recorded to a service the request did not name (PRSR-48, and the prune half
// PRSR-46 settled first).
//
// Ensure is keyed on hostname: ensureOne asks the provisioner what is there and
// makes it match the spec, and until PRSR-48 never consulted service_key. When
// upstream already matched, the mismatch was the deliberate reassignment adopt.
// When it did not, the same mismatch was an `update`, and --apply wrote one
// service's spec onto another service's resources — a `--access bookmark` spec
// against somebody's *gated* application strips its policies and leaves it
// resolving, which is a service admitting everyone with nothing anywhere
// reporting it. The plan did say `update`, which on a service you meant to
// update is not a line that invites suspicion: PRSR-41's lesson, met from the
// other side.
//
// So the question a teardown asks is asked here too, one command over: every
// active row must name this spec's service or the one the request says the
// hostname is moving from, and any other refuses the **whole run, before
// anything is read or written**. Whole-run rather than per-step for the reason
// PRSR-46 found over three review rounds — every row-writing branch of
// ensureOne stamps this spec's service_key, so a run that refused one kind and
// ran the rest produced a half-reassigned hostname that refuses *both* owners'
// teardowns, manufacturing in the act of refusing the condition that stops the
// remedy its own message names. Refusing early is what keeps "every row names
// one service" true, and that is what makes the remedies the message names
// work afterwards. The prune-only refusal this replaces asked the same question
// of orphans alone; this asks it of every row, so a prune is covered without
// having to be a special case.
//
// A hostname genuinely moving between services still ends in an adopt, it just
// has to be asked for — see Request.ReassignFrom.
func checkSpecOwnership(spec ServiceSpec, active map[model.ResourceKind]model.ServiceResource, from string) error {
	wrong := recordedToOthers(active, spec.Key, from)
	if len(wrong) == 0 {
		return nil
	}
	if from != "" {
		return fmt.Errorf("%w: %q asks to spin up %s as reassigned from %q, but %s — every active row must name one of those two services. If the hostname is moving from somebody else, name them; if the rows disagree with each other, remove the hostname as its owner first (`purser teardown-service --service <owner> --hostname %s`)",
			ErrHostnameNotThisService, spec.Key, spec.Hostname, from, strings.Join(wrong, ", "), spec.Hostname)
	}
	return fmt.Errorf("%w: %q asks to spin up %s, but %s — a spin-up writes only its own service's edge. If the hostname is moving to %q, say so by naming the previous owner (--reassign-from, or reassign_from over HTTP); if it is being retired, remove it as its owner (`purser teardown-service --service <owner> --hostname %s`); otherwise check the spelling",
		ErrHostnameNotThisService, spec.Key, spec.Hostname, strings.Join(wrong, ", "), spec.Key, spec.Hostname)
}

// rebindOrphan re-attributes a resource the spec does not call for to the
// spec's service, under a reassignment that named its previous owner.
//
// An orphan is a kind ensureOne never visits, so nothing else rewrites its row.
// A reassignment that moved the called-for kinds and left this one behind
// would produce exactly the half-owned hostname checkSpecOwnership and
// checkOwnership both refuse on: the next spin-up refuses until the previous
// owner is typed again, and a teardown refuses for either owner. Moving every
// row together is what "the hostname is reassigned" means.
//
// The row moves first and a prune, if asked for, runs afterwards on the moved
// row — so a prune that does not land leaves an orphan its new owner can still
// remove, rather than one nobody can. A row write and nothing upstream, an
// adopt's shape, and ticked the way an adopt is when it is the whole of what
// happened to this kind; under a prune the tick is the prune's to give.
//
// DETAIL describes the resource and ACTION carries the verb (PRSR-46): the line
// says whose the row is and that the reassignment moves it, and the tick says
// whether that has happened. A failed write gets the description back without
// the clause, so `failed` never sits beside a line saying the row moved — found
// by running the binary, where the first wording said "re-attributed" on a plan.
func (s *Service) rebindOrphan(ctx context.Context, t Target, kind model.ResourceKind, rec model.ServiceResource, f StepFinding, apply bool) (StepFinding, model.ServiceResource) {
	recorded := fmt.Sprintf("%s; recorded to %q", f.Detail, rec.ServiceKey)
	f.Detail = fmt.Sprintf("%s, moving to %q with the hostname", recorded, t.Spec.Key)
	if !apply {
		return f, rec
	}
	moved, err := s.record(ctx, t, kind, rec.ExternalID, rec.ParentID)
	if err != nil {
		// Nothing upstream was touched: an ordinary failure to write a row.
		f.Status, f.Detail, f.Err = StepFailed, recorded, err.Error()
		return f, rec
	}
	if f.Status == StepOrphaned {
		f.Applied = true
	}
	return f, moved
}

// unmetPruneDeps returns the steps this spec *does* call for that did not land.
//
// It is one rule covering both prunable kinds, and it is deliberately stronger
// than a per-kind dependency list would be. `--prune` means "make the edge match
// this spec"; if the additive half of that spec did not land, the edge is not the
// one the spec describes, and removing things on the strength of a spec only
// half-applied is acting on a state nobody has.
//
// The concrete case is the one the ordering above is for: a tunnelled → direct
// switch whose DNS repoint failed leaves the record pointing at the tunnel, so
// dropping the now-"orphaned" route takes a working service down. The general
// rule catches that without needing to know it, which is what makes it safer than
// enumerating the pairs — the enumeration is the thing that goes stale when a
// fourth kind arrives.
func unmetPruneDeps(spec ServiceSpec, done map[model.ResourceKind]StepFinding) []model.ResourceKind {
	var unmet []model.ResourceKind
	for _, kind := range model.KindOrder {
		if !spec.callsFor(kind) {
			continue
		}
		if f, decided := done[kind]; !decided || !f.inPlace() {
			unmet = append(unmet, kind)
		}
	}
	return unmet
}

// prunedBlockedDetail explains which steps held a prune back.
func prunedBlockedDetail(unmet []model.ResourceKind) string {
	names := make([]string, len(unmet))
	for i, k := range unmet {
		names[i] = string(k)
	}
	return fmt.Sprintf("held back: %s did not land, so this spec is not the edge yet and removing what it no longer names would act on a state nobody has",
		strings.Join(names, " and "))
}

// pruneOne removes one orphan, or — without apply — reports that it would.
//
// It is the only destructive thing this orchestrator does, and it does it
// through the same two calls `Teardown` uses: the provisioner's Teardown for the
// edge, and MarkServiceResourceRemoved for the row, in that order and never the
// other way round. A removal that didn't happen must never be recorded as one
// (PRSR-17), so every unhappy path below leaves the row active for the next run.
func (s *Service) pruneOne(ctx context.Context, t Target, kind model.ResourceKind, rec model.ServiceResource, f StepFinding, apply bool, unmet []model.ResourceKind) StepFinding {
	prov, ok := s.registry.Get(kind)
	if !ok {
		// Still there, and the row still says so. Never "nothing to do".
		f.Status = StepUnavailable
		f.Err = fmt.Sprintf("no provisioner registered for %q in this build", kind)
		return f
	}
	// Asked before the dry run returns and contacting nothing, so a plan cannot
	// promise a removal --apply then declines — the property TeardownChecker
	// exists for (PRSR-34), which this path inherits by being a teardown.
	if err := CanTeardown(prov, t); err != nil {
		if IsRefused(err) {
			f.Status, f.Err = StepRefused, err.Error()
			return f
		}
		f.Status, f.Err = StepUnavailable, err.Error()
		return f
	}
	if len(unmet) > 0 {
		// The reason goes in Err, not over Detail. Every other decline in this
		// function leaves Detail standing, so displacing it here would make
		// `blocked` the one status whose line no longer says which resource it
		// is about — and the id is the whole of what an operator checks a prune
		// against, since there is no upstream read on this path (PRSR-46
		// review). Err is printed in full below the table.
		f.Status, f.Err = StepBlocked, prunedBlockedDetail(unmet)
		return f
	}
	if !apply {
		return f // still StepPrune: this is what --apply would remove
	}

	rm, err := prov.Teardown(ctx, t, rec)
	switch {
	case IsUnavailable(err):
		f.Status, f.Err = StepUnavailable, err.Error()
		return f
	case IsRefused(err):
		// Upstream is in a shape no provisioner may act on — the tunnel's
		// ingress document, mostly. Re-running repeats this until it is fixed
		// there, and nothing was removed either way.
		f.Status, f.Err = StepRefused, err.Error()
		return f
	case err != nil:
		f.Status, f.Err = StepFailed, err.Error()
		return f
	}
	if rm.Detail != "" {
		// Overwritten: the line described what was recorded, and after the call
		// the useful thing is what happened to it.
		f.Detail = rm.Detail
	}
	if rm.Warning != "" {
		f.Warning = rm.Warning
	}

	if err := s.store.MarkServiceResourceRemoved(ctx, rec.ID); err != nil {
		// Gone upstream, live in the records — the opposite advice from failed,
		// and never swallowed into it.
		f.Status, f.Err = StepPrunedNotRecorded, err.Error()
		return f
	}
	f.Applied = true
	return f
}

// unmetDeps returns the prerequisites of kind that this run has not put in
// place. Empty is the normal case: only DNS has any.
func unmetDeps(spec ServiceSpec, kind model.ResourceKind, done map[model.ResourceKind]StepFinding) []model.ResourceKind {
	var unmet []model.ResourceKind
	for _, dep := range spec.dependsOn(kind) {
		if f, decided := done[dep]; !decided || !f.inPlace() {
			unmet = append(unmet, dep)
		}
	}
	return unmet
}

// blockedDetail explains which prerequisites held a step back, naming them so
// the operator doesn't have to infer it from the other lines.
func blockedDetail(unmet []model.ResourceKind) string {
	names := make([]string, len(unmet))
	for i, k := range unmet {
		names[i] = string(k)
	}
	return fmt.Sprintf("held back: %s did not land, and publishing this first is what it is ordered to prevent",
		strings.Join(names, " and "))
}

// notApplicable reports a kind this spec doesn't call for — and says so louder
// when Purser recorded one here anyway, since that resource is presumably still
// live and nothing else in the report would mention it.
func notApplicable(kind model.ResourceKind, spec ServiceSpec, rec model.ServiceResource, hasRec bool, reg *Registry, prune bool) StepFinding {
	f := StepFinding{Kind: kind, DisplayName: string(kind), Status: StepSkipped, Detail: spec.skipReason(kind)}
	if p, ok := reg.Get(kind); ok {
		f.DisplayName = p.DisplayName()
	}
	if !hasRec {
		return f
	}
	f.ExternalID = rec.ExternalID
	if !prune {
		f.Status = StepOrphaned
		f.Detail = fmt.Sprintf("%s, but Purser recorded one here on an earlier run and has not removed it — it is presumably still live", f.Detail)
		return f
	}
	// No ownership branch here: refuseContested has already rejected the whole
	// run if any orphan at this hostname belongs to somebody else, so anything
	// reaching this line is this service's to remove.
	f.Status = StepPrune
	f.Detail = fmt.Sprintf("%s, and Purser recorded one here on an earlier run%s", f.Detail, prunedTarget(rec))
	return f
}

// prunedTarget names the recorded resource, since on this line there is no
// upstream read to describe — the row is the whole of what the operator has to
// check the plan against, exactly as on the teardown walk.
//
// It describes the resource and never the action, which is the rule the rest of
// this report follows: DETAIL is what is there, ACTION is what happens to it.
// Writing "— removing it" here read fine under `prune` and became a lie under
// every other status the prune path can produce, since pruneOne leaves Detail
// alone when it declines. A live run against an unconfigured deployment printed
// `unavailable` beside "removing it (app-77e1 …)" — a plan promising in one
// column what it refuses in the next (PRSR-46, found by running the binary).
func prunedTarget(rec model.ServiceResource) string {
	switch {
	case rec.ExternalID != "" && rec.ParentID != "":
		return fmt.Sprintf(" (%s in %s)", rec.ExternalID, rec.ParentID)
	case rec.ExternalID != "":
		return fmt.Sprintf(" (%s)", rec.ExternalID)
	case rec.ParentID != "":
		// A tunnel route, which has no id of its own: its handle is
		// (tunnel, hostname), so naming the tunnel is naming the resource.
		return fmt.Sprintf(" (in %s)", rec.ParentID)
	}
	return ""
}

// ensureOne decides and, under apply, performs one step.
//
// Like offboardOne it returns no error: every outcome is a finding. Once a step
// has changed the edge, aborting would discard the findings accumulated so far
// and leave the operator with an error and no idea what was already created.
func (s *Service) ensureOne(ctx context.Context, t Target, kind model.ResourceKind, rec model.ServiceResource, hasRec bool, apply bool, unmet []model.ResourceKind) StepFinding {
	f := StepFinding{Kind: kind, DisplayName: string(kind)}

	prov, ok := s.registry.Get(kind)
	if !ok {
		// A step this spec calls for and this build cannot take. Emphatically
		// not "nothing to do": the hostname will not work without it.
		f.Status = StepUnavailable
		f.Err = fmt.Sprintf("no provisioner registered for %q in this build", kind)
		return f
	}
	f.DisplayName = prov.DisplayName()

	// One read decides everything below, on both paths. This is the call a dry
	// run makes and the only upstream call it makes.
	state, err := prov.Inspect(ctx, t)
	switch {
	case IsUnavailable(err):
		f.Status, f.Err = StepUnavailable, err.Error()
		return f
	case IsRefused(err):
		// The read worked and upstream is in a shape this provisioner will not
		// write into. Apply declines for the same reason a dry run reports it,
		// and re-running will say this until somebody changes what is upstream.
		f.Status, f.Err = StepRefused, err.Error()
		return f
	case err != nil:
		// Unknown, never absent — and apply stops here rather than writing
		// blind. A retry costs one re-run; acting on a state that could not be
		// read costs a duplicate resource, or on the tunnel a document rebuilt
		// from a read that failed.
		f.Status, f.Err = StepUnknown, err.Error()
		return f
	}
	f.Detail, f.ExternalID = state.Detail, state.ExternalID

	switch {
	case !state.Exists && hasRec:
		// Purser created this and something removed it. The apply recreates it,
		// like a create, but an operator reading "create" would never learn that
		// a resource of theirs was deleted outside Purser.
		f.Status = StepMissing
	case !state.Exists:
		f.Status = StepCreate
	case !state.Matches:
		f.Status = StepUpdate
	case !hasRec || rec.ExternalID != state.ExternalID || rec.ParentID != state.ParentID || rec.ServiceKey != t.Spec.Key:
		// Correct upstream, but Purser's records don't have it, don't agree
		// about which object it is, or still attribute it to the service that
		// held this hostname before. The fix is a row, not an API call — and the
		// service_key comparison is what stops a reassigned hostname reporting
		// `ok` forever while `ServiceResourcesFor` answers with the old owner.
		f.Status = StepAdopt
	default:
		f.Status = StepOK
		return f
	}

	// Held back rather than run. Only acting statuses are gated: an adopt writes
	// a row and changes nothing upstream, and an already-correct resource is
	// already published, so neither can open the window this guards.
	if len(unmet) > 0 {
		switch f.Status {
		case StepCreate, StepUpdate, StepMissing:
			f.Status, f.Detail = StepBlocked, blockedDetail(unmet)
			return f
		}
	}

	if !apply {
		return f // dry run: the Inspect above was the only call
	}

	if f.Status == StepAdopt {
		if _, err := s.record(ctx, t, kind, state.ExternalID, state.ParentID); err != nil {
			// Nothing upstream was touched, so this is an ordinary failure to
			// write a row — not applied-not-recorded, which asserts that the
			// edge changed.
			f.Status, f.Err = StepFailed, err.Error()
			return f
		}
		f.Applied = true
		return f
	}

	res, err := prov.Ensure(ctx, t)
	switch {
	case IsUnavailable(err):
		f.Status, f.Err = StepUnavailable, err.Error()
		return f
	case IsRefused(err):
		// Reachable only when upstream changed shape between the Inspect above
		// and this call — the plan was made from a document that was writable
		// and the write found one that isn't. Reported as refused rather than
		// failed because the operator's next move is the same as if the plan had
		// said so: fix what is upstream. Nothing was written either way.
		f.Status, f.Err = StepRefused, err.Error()
		return f
	case err != nil:
		f.Status, f.Err = StepFailed, err.Error()
		return f
	}
	// Overwritten, not merged: Detail describes what is there *now*, and keeping
	// Inspect's description of the state this call just replaced would present
	// the old world as the result. A provisioner that returns none leaves the
	// line without a description, which is honest.
	f.Detail, f.Warning = res.Detail, res.Warning

	// The ids are merged the other way round, and deliberately. A provisioner
	// that returns a partial Resource — an update that had nothing new to say,
	// a create whose response omitted the id — must not blank out a known-good
	// external_id, because that id is the only handle Teardown has: an empty one
	// means "this kind has none" (a tunnel route), so a wiped id reads as a
	// resource that can never be targeted rather than as a lost value. Falls
	// back to what Inspect just saw, then to what was already recorded.
	f.ExternalID = firstNonEmpty(res.ExternalID, state.ExternalID, rec.ExternalID)
	parentID := firstNonEmpty(res.ParentID, state.ParentID, rec.ParentID)

	if _, err := s.record(ctx, t, kind, f.ExternalID, parentID); err != nil {
		// The edge changed and the bookkeeping didn't. Reported as its own state
		// and never swallowed: a teardown targets recorded ids, so until a later
		// run adopts this back, Purser cannot remove what it just created.
		f.Status, f.Err = StepAppliedNotRecorded, err.Error()
		return f
	}
	f.Applied = true
	return f
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// record writes the durable "this exists at the edge" row.
func (s *Service) record(ctx context.Context, t Target, kind model.ResourceKind, externalID, parentID string) (model.ServiceResource, error) {
	return s.store.UpsertServiceResource(ctx, model.ServiceResource{
		ServiceKey: t.Spec.Key,
		Hostname:   t.Spec.Hostname,
		Kind:       kind,
		ExternalID: externalID,
		ParentID:   parentID,
		Status:     model.ResourceActive,
	})
}
