package spinup

import (
	"context"
	"errors"
	"fmt"

	"github.com/Einlanzerous/purser/internal/model"
)

// ErrUnavailable is returned by a provisioner that is registered but cannot act
// — its credential isn't configured, or nobody has built the upstream call yet.
// It is this axis's counterpart to connector.ErrPending and means the same
// thing: nothing broke, and nothing was done.
//
// It is a separate sentinel rather than a reuse of connector.ErrPending for two
// reasons. The axes are independent by design, and the person axis's sentinel is
// schema-adjacent — migration 0004's backfill matches its exact text as a
// literal prefix — so it cannot be reworded into something that reads correctly
// for a DNS record.
var ErrUnavailable = errors.New("spinup: provisioner not available")

// IsUnavailable reports whether err is the "not wired up" sentinel. Callers
// bucket on this rather than on errors.Is directly, so a provisioner that wraps
// it several layers deep is still recognized as unavailable rather than as a
// breakage.
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }

// Target is a spec with the orchestrator's per-run resolutions applied. It is
// what provisioners are handed, so no provisioner resolves a ref itself and
// none of them can disagree about what "prod" means.
type Target struct {
	// Spec is the validated, normalized spec.
	Spec ServiceSpec
	// TunnelID is Spec.Tunnel resolved to an id, empty for a direct spec.
	//
	// Both tunnelled steps need it and they must not disagree: the ingress route
	// is written into that tunnel's configuration, and the DNS record points at
	// <TunnelID>.cfargotunnel.com. Resolving once, before any step runs, is what
	// makes "the record points at the tunnel the route went into" structural
	// rather than a thing two connectors each remember to do.
	TunnelID string
}

// State is what Inspect found upstream. It carries no secret and no side
// effect: Inspect is read-only by contract.
type State struct {
	// Exists reports whether a resource of this kind is present for the
	// hostname.
	Exists bool
	// Matches reports whether what is there already satisfies the spec. False
	// with Exists true means the resource needs updating, which is a materially
	// different thing to preview than a creation — on the tunnel it is a
	// read-modify-write of a document holding every other service's routes.
	Matches bool
	// ExternalID and ParentID identify what was found, and are recorded verbatim
	// when a run adopts an existing resource.
	ExternalID string
	ParentID   string
	// Detail is a short human description of what is there now ("proxied CNAME
	// → aef21667….cfargotunnel.com"), for the plan the operator reads.
	Detail string
}

// Resource is what Ensure created or confirmed.
type Resource struct {
	ExternalID string
	ParentID   string
	Detail     string
}

// ServiceProvisioner manages one kind of edge resource for a service. It is the
// spin-up axis's Connector: registered by kind, asked to make upstream match a
// spec, and required to be idempotent.
//
// Implementations must hold to the same four rules the person axis learned, for
// the same reasons:
//
//   - Already-exists upstream is success, not a conflict, so a re-run that
//     retries only what failed is safe.
//   - Inspect must never mutate. A reconcile that repairs as a side effect
//     cannot be used to preview, because running it destroys the difference it
//     was meant to report.
//   - A failed call is never absence. Report the error and let the orchestrator
//     record "unknown"; answering "not there" for a question you couldn't ask
//     is how a spin-up creates a second copy of something that already exists.
//   - Teardown reports success only when the resource is actually gone.
type ServiceProvisioner interface {
	// Kind is the resource kind this provisioner owns. It is the registry key,
	// so exactly one provisioner serves each kind.
	Kind() model.ResourceKind
	// DisplayName is a human label for the plan, e.g. "DNS record".
	DisplayName() string
	// Inspect reports the current upstream state for the target, WITHOUT
	// creating, updating or deleting anything.
	//
	// It is the whole basis of the preview: the plan an operator reads and the
	// actions --apply takes are decided from this one answer, so a dry run and
	// an apply cannot disagree about what is already there.
	Inspect(ctx context.Context, t Target) (State, error)
	// Ensure makes upstream match the target, creating or updating as needed,
	// and returns what now exists. It must be safe to call when the resource
	// already exists and already matches.
	Ensure(ctx context.Context, t Target) (Resource, error)
	// Teardown removes the resource recorded in rec. rec is passed rather than
	// looked up by hostname because deleting a record somebody created by hand
	// is not recoverable by re-running — the recorded id is the only target
	// Purser can prove it owns.
	//
	// Like Ensure it is idempotent: a resource already gone is a success. Like
	// Deprovision on the person axis it must never claim more than it did — a
	// provisioner that cannot delete returns ErrUnavailable rather than nil.
	Teardown(ctx context.Context, t Target, rec model.ServiceResource) error
}

// Registry is the set of provisioners, keyed by resource kind. It is separate
// from connector.Registry — same idea, different axis, and a shared one would
// have to key on something both a service key and a resource kind could be.
type Registry struct {
	byKind map[model.ResourceKind]ServiceProvisioner
}

// NewRegistry builds a registry. It panics on a duplicate kind, and on a kind
// outside model.KindOrder — both are wiring errors rather than runtime
// conditions, and both fail the same silent way if tolerated.
//
// The unknown-kind check is the less obvious of the two and the more dangerous.
// The orchestrator walks KindOrder, so a provisioner registered under a typo'd
// or not-yet-supported kind is never asked to do anything and never appears in a
// report: the spin-up looks complete while a step of it was never run. A panic
// at wiring time is the only place that can be caught cheaply.
func NewRegistry(ps ...ServiceProvisioner) *Registry {
	r := &Registry{byKind: make(map[model.ResourceKind]ServiceProvisioner, len(ps))}
	for _, p := range ps {
		kind := p.Kind()
		if !knownKind(kind) {
			panic(fmt.Sprintf("spinup: provisioner kind %q is not one the orchestrator runs (known: %v)", kind, model.KindOrder))
		}
		if _, dup := r.byKind[kind]; dup {
			panic(fmt.Sprintf("spinup: duplicate provisioner kind %q", kind))
		}
		r.byKind[kind] = p
	}
	return r
}

// knownKind reports whether the orchestrator will ever ask for this kind.
func knownKind(kind model.ResourceKind) bool {
	for _, k := range model.KindOrder {
		if k == kind {
			return true
		}
	}
	return false
}

// Get returns the provisioner for a kind, or (nil, false).
//
// Nil-safe: a Service built with no registry reports every step unavailable —
// which is what an unconfigured deployment should look like — rather than
// panicking partway through a spec.
func (r *Registry) Get(kind model.ResourceKind) (ServiceProvisioner, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byKind[kind]
	return p, ok
}

// Kinds returns the registered kinds, in model.KindOrder.
func (r *Registry) Kinds() []model.ResourceKind {
	if r == nil {
		return nil
	}
	out := make([]model.ResourceKind, 0, len(r.byKind))
	for _, k := range model.KindOrder {
		if _, ok := r.byKind[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// Unavailable stands in for a provisioner that is registered — so its kind is a
// recognized step rather than an unknown one — but cannot act, because its
// credential isn't configured. It mirrors connector.Unavailable, and exists for
// the same reason: "Cloudflare's token is unset" is a better answer than "no
// provisioner for dns_record", which reads like a missing build.
type Unavailable struct {
	ResourceKind model.ResourceKind
	Display      string
	Reason       string
}

// NewUnavailable builds an Unavailable provisioner.
func NewUnavailable(kind model.ResourceKind, display, reason string) *Unavailable {
	return &Unavailable{ResourceKind: kind, Display: display, Reason: reason}
}

func (u *Unavailable) Kind() model.ResourceKind { return u.ResourceKind }
func (u *Unavailable) DisplayName() string      { return u.Display }

// Inspect can't check what it isn't configured to reach. It reports
// ErrUnavailable rather than "not found", so a plan shows the step as
// unavailable instead of promising to create something it can't.
func (u *Unavailable) Inspect(context.Context, Target) (State, error) {
	return State{}, fmt.Errorf("%w: %s", ErrUnavailable, u.Reason)
}

func (u *Unavailable) Ensure(context.Context, Target) (Resource, error) {
	return Resource{}, fmt.Errorf("%w: %s", ErrUnavailable, u.Reason)
}

func (u *Unavailable) Teardown(context.Context, Target, model.ServiceResource) error {
	return fmt.Errorf("%w: %s", ErrUnavailable, u.Reason)
}
