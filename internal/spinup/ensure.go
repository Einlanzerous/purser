package spinup

import (
	"context"
	"fmt"
	"strings"

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
	StepOrphaned StepStatus = "orphaned"
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
	// StepUnknown — the current state could not be determined, so nothing may
	// be decided from it. Never collapsed into "absent": acting on an
	// unverifiable answer is how a spin-up creates a second copy of something,
	// and on the tunnel it would mean rewriting a shared document from a read
	// that just failed. Apply does not act on an unknown step.
	//
	// Two different things reach it, and they want opposite responses from an
	// operator. Usually the read failed, and re-running is the whole fix. But a
	// provisioner also reports this for a read that *succeeded* and returned
	// something it will not act on — the tunnel's ingress document is one
	// object holding every service's routes, so a shape it cannot write into
	// safely is refused here rather than previewed as a `create` that `--apply`
	// then declines (PRSR-30). That one is permanent: re-running repeats it
	// until somebody fixes what is upstream, and the Err string is where the
	// difference is spelled out.
	StepUnknown StepStatus = "unknown"
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
}

// Result is the whole plan, or the whole apply.
type Result struct {
	// Spec is the normalized spec the run worked from, not the one passed in.
	Spec     ServiceSpec
	Findings []StepFinding
	Applied  bool
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
// a human (unavailable, unknown, failed) are not counted here, and neither is
// blocked: re-running with --apply does not fix any of them, because the reason
// they didn't happen was never the missing flag.
func (r *Result) Pending() int {
	n := 0
	for _, f := range r.Findings {
		switch f.Status {
		case StepCreate, StepUpdate, StepAdopt, StepMissing:
			if !f.Applied {
				n++
			}
		}
	}
	return n
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

	res := &Result{Spec: spec, Applied: req.Apply}
	// Findings so far, so a step can see whether the steps it depends on are in
	// place. KindOrder is an apply order, so a dependency has always been
	// decided by the time its dependent is reached.
	done := make(map[model.ResourceKind]StepFinding, len(model.KindOrder))
	for _, kind := range model.KindOrder {
		rec, hasRec := active[kind]
		var f StepFinding
		if !spec.callsFor(kind) {
			f = notApplicable(kind, spec, rec, hasRec, s.registry)
		} else {
			f = s.ensureOne(ctx, target, kind, rec, hasRec, req.Apply, unmetDeps(spec, kind, done))
		}
		done[kind] = f
		res.Findings = append(res.Findings, f)
	}
	return res, nil
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
func notApplicable(kind model.ResourceKind, spec ServiceSpec, rec model.ServiceResource, hasRec bool, reg *Registry) StepFinding {
	f := StepFinding{Kind: kind, DisplayName: string(kind), Status: StepSkipped, Detail: spec.skipReason(kind)}
	if p, ok := reg.Get(kind); ok {
		f.DisplayName = p.DisplayName()
	}
	if hasRec {
		f.Status = StepOrphaned
		f.ExternalID = rec.ExternalID
		f.Detail = fmt.Sprintf("%s, but Purser recorded one here on an earlier run and has not removed it — it is presumably still live", f.Detail)
	}
	return f
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
	case err != nil:
		f.Status, f.Err = StepFailed, err.Error()
		return f
	}
	// Overwritten, not merged: Detail describes what is there *now*, and keeping
	// Inspect's description of the state this call just replaced would present
	// the old world as the result. A provisioner that returns none leaves the
	// line without a description, which is honest.
	f.Detail = res.Detail

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
