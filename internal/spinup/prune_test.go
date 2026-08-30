package spinup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/model"
)

// PRSR-46: `--prune` gives the orphan a command. Everything here is about the
// two things that make it safe rather than merely possible — that it runs after
// the additive pass, and that it declines when the additive pass did not land —
// plus the invariants it inherits by being a teardown.

// orderedProv is a fakeProv that appends every call it takes to a shared log, so
// "the additive pass finishes before anything is removed" is testable as an
// observation rather than inferred from a passing result.
type orderedProv struct {
	fakeProv
	log *[]string
}

func (p *orderedProv) Ensure(ctx context.Context, t Target) (Resource, error) {
	*p.log = append(*p.log, "ensure:"+string(p.kind))
	return p.fakeProv.Ensure(ctx, t)
}

func (p *orderedProv) Teardown(ctx context.Context, t Target, rec model.ServiceResource) (Removal, error) {
	*p.log = append(*p.log, "teardown:"+string(p.kind))
	return p.fakeProv.Teardown(ctx, t, rec)
}

// narrowedSpec is a service that used to be tunnelled and gated and is now
// direct with no Access surface, so both other kinds are orphans.
func narrowedSpec(t *testing.T) ServiceSpec {
	t.Helper()
	spec, err := ServiceSpec{
		Key: "interlock", Hostname: teardownHost,
		Mode: ModeDirect, Upstream: "100.64.0.7", Access: AccessNone,
	}.Validate()
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return spec
}

// widerRows seeds the rows a *previous*, wider spec would have left: all three
// kinds active at the hostname.
func widerRows(t *testing.T) *fakeStore {
	t.Helper()
	st, _ := seeded(t)
	return st
}

// dnsInPlace is a DNS provisioner whose answer matches the row seeded() wrote,
// so the step reads `ok` rather than `adopt`. The ids have to agree or the
// additive pass has work outstanding, which is a different test than any of
// these are trying to be.
func dnsInPlace() *fakeProv {
	return present(model.ResourceDNSRecord, "id-"+string(model.ResourceDNSRecord), "parent-"+string(model.ResourceDNSRecord))
}

func pruneReq(t *testing.T, apply, prune bool) Request {
	t.Helper()
	return Request{Spec: narrowedSpec(t), Apply: apply, Prune: prune}
}

// --- the flag is what makes the difference ----------------------------------

// Without it, nothing changes: an orphan is reported and left exactly where it
// is, which is the behaviour every release before this one had.
func TestPrune_WithoutTheFlagAnOrphanIsStillUntouched(t *testing.T) {
	st := widerRows(t)
	route := &fakeProv{kind: model.ResourceTunnelRoute}
	app := &fakeProv{kind: model.ResourceAccessApp}
	svc := New(st, NewRegistry(route, app, dnsInPlace()))

	res, err := svc.Ensure(context.Background(), pruneReq(t, true, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []model.ResourceKind{model.ResourceTunnelRoute, model.ResourceAccessApp} {
		if got := findingFor(t, res, k).Status; got != StepOrphaned {
			t.Errorf("%s: %s, want %s", k, got, StepOrphaned)
		}
	}
	if route.teardowns != 0 || app.teardowns != 0 {
		t.Errorf("an orphan was removed without --prune (route=%d app=%d)", route.teardowns, app.teardowns)
	}
	if st.removals != 0 {
		t.Errorf("%d rows marked removed without --prune", st.removals)
	}
	// And it still exits clean: an orphan does not falsify the claim
	// NeedsAttention makes, which is that the spec is satisfied.
	if len(res.NeedsAttention()) != 0 {
		t.Errorf("NeedsAttention: %v", res.NeedsAttention())
	}
	if res.Pending() != 0 {
		t.Errorf("Pending()=%d — an orphan nobody asked to remove is not work outstanding", res.Pending())
	}
}

// With the flag but without --apply, it is a plan: the line says the resource is
// going, and nothing goes. Both flags are needed, which is what stops the one
// destructive thing this command can do from riding on a single field.
func TestPrune_NeedsApplyToRemoveAnything(t *testing.T) {
	st := widerRows(t)
	route := &fakeProv{kind: model.ResourceTunnelRoute}
	app := &fakeProv{kind: model.ResourceAccessApp}
	svc := New(st, NewRegistry(route, app, dnsInPlace()))

	res, err := svc.Ensure(context.Background(), pruneReq(t, false, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []model.ResourceKind{model.ResourceTunnelRoute, model.ResourceAccessApp} {
		f := findingFor(t, res, k)
		if f.Status != StepPrune {
			t.Errorf("%s: %s, want %s", k, f.Status, StepPrune)
		}
		if f.Applied {
			t.Errorf("%s reports Applied on a plan", k)
		}
	}
	if route.teardowns != 0 || app.teardowns != 0 {
		t.Error("a plan removed something")
	}
	// A prune is work --apply would do, so it counts — unlike an orphan, which
	// is a state nobody asked about.
	if res.Pending() != 2 {
		t.Errorf("Pending()=%d, want 2", res.Pending())
	}
	if !res.Pruned {
		t.Error("Result.Pruned must echo the request, so a reader can tell the two orphan readings apart")
	}
}

func TestPrune_ApplyRemovesAndMarksTheRow(t *testing.T) {
	st := widerRows(t)
	route := &fakeProv{kind: model.ResourceTunnelRoute, removed: Removal{Detail: "removed 1 rule"}}
	app := &fakeProv{kind: model.ResourceAccessApp, removed: Removal{Detail: "deleted app-1"}}
	svc := New(st, NewRegistry(route, app, dnsInPlace()))

	res, err := svc.Ensure(context.Background(), pruneReq(t, true, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []model.ResourceKind{model.ResourceTunnelRoute, model.ResourceAccessApp} {
		f := findingFor(t, res, k)
		if f.Status != StepPrune || !f.Applied {
			t.Errorf("%s: status=%s applied=%v", k, f.Status, f.Applied)
		}
		if got := st.rows[key(teardownHost, k)].Status; got != model.ResourceRemoved {
			t.Errorf("%s row is %q after a successful prune", k, got)
		}
	}
	// The detail is what happened, not what was recorded.
	if got := findingFor(t, res, model.ResourceAccessApp).Detail; got != "deleted app-1" {
		t.Errorf("detail %q", got)
	}
	// The DNS step is untouched by any of this — the hostname stays up, which is
	// the whole difference between a prune and a teardown.
	if got := findingFor(t, res, model.ResourceDNSRecord).Status; got != StepOK {
		t.Errorf("dns_record: %s, want %s", got, StepOK)
	}
	// And a re-run has nothing left to say: the rows are removed, so the kinds
	// this spec does not call for are plainly skipped.
	again, err := svc.Ensure(context.Background(), pruneReq(t, true, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []model.ResourceKind{model.ResourceTunnelRoute, model.ResourceAccessApp} {
		if got := findingFor(t, again, k).Status; got != StepSkipped {
			t.Errorf("%s re-runs as %s, want %s", k, got, StepSkipped)
		}
	}
}

// --- ordering ---------------------------------------------------------------

// The case that decides the ordering, and the reason pruning runs after the
// whole additive pass rather than in KindOrder alongside it: a tunnelled →
// direct switch orphans the ingress route while the DNS step repoints the record
// away from the tunnel. Drop the route first and the hostname 502s until the
// record catches up.
func TestPrune_RunsAfterTheAdditivePass(t *testing.T) {
	st := widerRows(t)
	var order []string
	dns := &orderedProv{fakeProv: fakeProv{kind: model.ResourceDNSRecord,
		state:   State{Exists: true, ExternalID: "rec-1", ParentID: "zone-1"}, // exists, does not match → update
		ensured: Resource{ExternalID: "rec-1", ParentID: "zone-1", Detail: "repointed"}},
		log: &order}
	route := &orderedProv{fakeProv: fakeProv{kind: model.ResourceTunnelRoute}, log: &order}
	app := &orderedProv{fakeProv: fakeProv{kind: model.ResourceAccessApp}, log: &order}

	res, err := New(st, NewRegistry(dns, route, app)).Ensure(context.Background(), pruneReq(t, true, true))
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, res, model.ResourceDNSRecord).Status; got != StepUpdate {
		t.Fatalf("dns_record: %s, want %s — this test needs the repoint to happen", got, StepUpdate)
	}
	// The DNS write must precede every removal. Anything else leaves a window
	// where the record still points at a tunnel whose route has already gone.
	want := []string{"ensure:dns_record", "teardown:access_app", "teardown:tunnel_route"}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Errorf("call order %v,\nwant %v — the additive pass completes before anything is removed", order, want)
	}
}

// --- the guard --------------------------------------------------------------

// `--prune` means "make the edge match this spec". If the additive half did not
// land, the edge is not the one the spec describes, and removing what it no
// longer names is acting on a state nobody has. The concrete case is the one the
// ordering is for, one step worse: the DNS repoint *failed*, so the record still
// points at the tunnel and dropping the route takes a working service down.
func TestPrune_HeldBackWhenTheAdditivePassDidNotLand(t *testing.T) {
	st := widerRows(t)
	route := &fakeProv{kind: model.ResourceTunnelRoute}
	app := &fakeProv{kind: model.ResourceAccessApp}
	dns := &fakeProv{kind: model.ResourceDNSRecord, ensureErr: errors.New("cloudflare: 500")}

	res, err := New(st, NewRegistry(route, app, dns)).Ensure(context.Background(), pruneReq(t, true, true))
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, res, model.ResourceDNSRecord).Status; got != StepFailed {
		t.Fatalf("dns_record: %s, want %s", got, StepFailed)
	}
	for _, k := range []model.ResourceKind{model.ResourceTunnelRoute, model.ResourceAccessApp} {
		f := findingFor(t, res, k)
		if f.Status != StepBlocked {
			t.Errorf("%s: %s, want %s", k, f.Status, StepBlocked)
		}
		// The reason goes in Err; Detail keeps naming the resource, so a blocked
		// line still says which one it is about (PRSR-46 review).
		if !strings.Contains(f.Err, string(model.ResourceDNSRecord)) {
			t.Errorf("%s: the reason must name what held it back, got %q", k, f.Err)
		}
		if !strings.Contains(f.Detail, "id-"+string(k)) {
			t.Errorf("%s: a blocked line must still say which resource it is about, got %q", k, f.Detail)
		}
	}
	if route.teardowns != 0 || app.teardowns != 0 {
		t.Error("a blocked prune was performed anyway")
	}
	if st.removals != 0 {
		t.Errorf("%d rows marked removed", st.removals)
	}
}

// An already-correct edge satisfies the guard: `ok` is in place, so there is
// nothing half-applied and the prune proceeds. Without this the common case —
// narrow a spec that is otherwise already correct — would block for ever.
func TestPrune_AnAlreadyCorrectEdgeDoesNotBlockIt(t *testing.T) {
	st := widerRows(t)
	route := &fakeProv{kind: model.ResourceTunnelRoute}
	svc := New(st, NewRegistry(route,
		&fakeProv{kind: model.ResourceAccessApp},
		dnsInPlace()))

	res, err := svc.Ensure(context.Background(), pruneReq(t, true, true))
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, res, model.ResourceDNSRecord).Status; got != StepOK {
		t.Fatalf("dns_record: %s, want %s", got, StepOK)
	}
	if got := findingFor(t, res, model.ResourceTunnelRoute).Status; got != StepPrune {
		t.Errorf("tunnel_route: %s, want %s — an ok DNS step is in place", got, StepPrune)
	}
}

// --- inherited invariants ---------------------------------------------------

// A removal that didn't happen must never be recorded as one (PRSR-17). Every
// unhappy path leaves the row active so the next run retries.
func TestPrune_AFailedRemovalLeavesTheRowActive(t *testing.T) {
	for name, prov := range map[string]*fakeProv{
		"failed":      {kind: model.ResourceAccessApp, teardownErr: errors.New("cloudflare: 500")},
		"refused":     {kind: model.ResourceAccessApp, teardownErr: fmt.Errorf("%w: bad document", ErrRefused)},
		"unavailable": {kind: model.ResourceAccessApp, teardownErr: fmt.Errorf("%w: no token", ErrUnavailable)},
	} {
		t.Run(name, func(t *testing.T) {
			st := widerRows(t)
			res, err := New(st, NewRegistry(&fakeProv{kind: model.ResourceTunnelRoute}, prov,
				dnsInPlace())).
				Ensure(context.Background(), pruneReq(t, true, true))
			if err != nil {
				t.Fatal(err)
			}
			f := findingFor(t, res, model.ResourceAccessApp)
			if f.Applied {
				t.Error("a failed prune reports Applied")
			}
			if got := st.rows[key(teardownHost, model.ResourceAccessApp)].Status; got != model.ResourceActive {
				t.Errorf("row is %q; a removal that didn't happen must never be recorded as one", got)
			}
			if len(res.NeedsAttention()) == 0 {
				t.Error("a prune that could not happen must need attention")
			}
		})
	}
}

// pruned-not-recorded is not applied-not-recorded: that one means something was
// created and Purser holds no id for it; this means something is gone and Purser
// holds a live-looking row for it, which a later run reads as one to adopt.
func TestPrune_RemovedButNotRecordedIsItsOwnStatus(t *testing.T) {
	st := widerRows(t)
	st.failRemove = errors.New("store: connection reset")
	res, err := New(st, NewRegistry(
		&fakeProv{kind: model.ResourceTunnelRoute},
		&fakeProv{kind: model.ResourceAccessApp},
		dnsInPlace())).
		Ensure(context.Background(), pruneReq(t, true, true))
	if err != nil {
		t.Fatal(err)
	}
	f := findingFor(t, res, model.ResourceAccessApp)
	if f.Status != StepPrunedNotRecorded {
		t.Fatalf("access_app: %s, want %s", f.Status, StepPrunedNotRecorded)
	}
	if f.Status == StepAppliedNotRecorded {
		t.Error("the two point opposite ways and must not share a status")
	}
	if f.Applied {
		t.Error("a step whose record did not land must not report Applied")
	}
	if len(res.NeedsAttention()) == 0 {
		t.Error("pruned-not-recorded must need attention: the rows are now wrong")
	}
}

// A plan makes no upstream call for a prune either, so it cannot learn from an
// Inspect that a provisioner is unconfigured — CanTeardown answers it, and plan
// and apply agree about a removal that was never going to happen.
func TestPrune_AnUnconfiguredProvisionerReadsTheSameBothWays(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(fmt.Sprintf("apply=%v", apply), func(t *testing.T) {
			st := widerRows(t)
			app := &refuser{fakeProv: fakeProv{kind: model.ResourceAccessApp},
				err: fmt.Errorf("%w: set PURSER_CF_API_TOKEN", ErrUnavailable)}
			res, err := New(st, NewRegistry(&fakeProv{kind: model.ResourceTunnelRoute}, app,
				dnsInPlace())).
				Ensure(context.Background(), pruneReq(t, apply, true))
			if err != nil {
				t.Fatal(err)
			}
			f := findingFor(t, res, model.ResourceAccessApp)
			if f.Status != StepUnavailable {
				t.Errorf("access_app: %s, want %s", f.Status, StepUnavailable)
			}
			if !strings.Contains(f.Err, "PURSER_CF_API_TOKEN") {
				t.Errorf("the line must name the variable, got %q", f.Err)
			}
			if !apply && app.teardowns != 0 {
				t.Error("the plan called Teardown; CanTeardown must answer without contacting anything")
			}
		})
	}
}

// A kind with a recorded orphan and no provisioner in this build is unavailable,
// never silence: it is still there, and the row still says so.
func TestPrune_AnUnregisteredKindIsUnavailable(t *testing.T) {
	st := widerRows(t)
	res, err := New(st, NewRegistry(dnsInPlace())).
		Ensure(context.Background(), pruneReq(t, true, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []model.ResourceKind{model.ResourceTunnelRoute, model.ResourceAccessApp} {
		if got := findingFor(t, res, k).Status; got != StepUnavailable {
			t.Errorf("%s: %s, want %s", k, got, StepUnavailable)
		}
	}
}

// The plan names what is about to go. Preview-by-default is the last thing
// between a narrowed spec and a deleted Access application, so the line has to
// carry the id — there is no upstream read on this path to describe.
func TestPrune_ThePlanNamesWhatItWillRemove(t *testing.T) {
	st := widerRows(t)
	res, err := New(st, NewRegistry(
		&fakeProv{kind: model.ResourceTunnelRoute},
		&fakeProv{kind: model.ResourceAccessApp},
		dnsInPlace())).
		Ensure(context.Background(), pruneReq(t, false, true))
	if err != nil {
		t.Fatal(err)
	}
	f := findingFor(t, res, model.ResourceAccessApp)
	if !strings.Contains(f.Detail, "id-access_app") {
		t.Errorf("detail %q must name the resource it is going to remove", f.Detail)
	}
	// It describes the resource and never the action — the rule the rest of this
	// report follows, and the one a live run caught this breaking: an `unavailable`
	// prune printed "removing it" in the column beside the status refusing to.
	if strings.Contains(f.Detail, "removing") {
		t.Errorf("detail %q states the action; ACTION is the column that does that, and it is not always `prune`", f.Detail)
	}
	if f.ExternalID != "id-access_app" {
		t.Errorf("ExternalID = %q", f.ExternalID)
	}
	// A tunnel route has no id of its own, so the line names its tunnel instead
	// of leaving the operator with nothing to check.
	route := findingFor(t, res, model.ResourceTunnelRoute)
	if !strings.Contains(route.Detail, "parent-tunnel_route") {
		t.Errorf("tunnel_route detail %q must name the tunnel", route.Detail)
	}
}

// The column that carries the verb is ACTION, and a prune that cannot happen
// says so there. Detail must not contradict it — this is the live-run finding
// pinned directly.
func TestPrune_ADeclinedPruneDoesNotStillReadAsARemoval(t *testing.T) {
	st := widerRows(t)
	app := &refuser{fakeProv: fakeProv{kind: model.ResourceAccessApp},
		err: fmt.Errorf("%w: set PURSER_CF_API_TOKEN", ErrUnavailable)}
	res, err := New(st, NewRegistry(&fakeProv{kind: model.ResourceTunnelRoute}, app, dnsInPlace())).
		Ensure(context.Background(), pruneReq(t, false, true))
	if err != nil {
		t.Fatal(err)
	}
	f := findingFor(t, res, model.ResourceAccessApp)
	if f.Status != StepUnavailable {
		t.Fatalf("access_app: %s, want %s", f.Status, StepUnavailable)
	}
	if strings.Contains(f.Detail, "removing") {
		t.Errorf("status %s beside detail %q — the plan promises in one column what it refuses in the next", f.Status, f.Detail)
	}
	// It still says what is recorded, which is what the operator checks against.
	if !strings.Contains(f.Detail, "id-access_app") {
		t.Errorf("detail %q should still name the resource", f.Detail)
	}
}

// **A prune may only remove this service's own resources.** `active` is built
// from a hostname-keyed lookup, so without the guard every row at the hostname is
// a prune candidate whatever service it is recorded to — and a spec naming
// `--access none` deletes *another* service's Access application, leaves its
// hostname resolving, and marks the row removed so nothing mentions it again.
// That is the hole `teardown-service` refuses on, reached through the additive
// command (found in review of purser#58).
func TestPrune_WillNotRemoveAnotherServicesResource(t *testing.T) {
	st := newStore()
	for _, k := range model.KindOrder {
		st.put(model.ServiceResource{
			ServiceKey: "argosy", Hostname: teardownHost, Kind: k,
			ExternalID: "id-" + string(k), ParentID: "parent-" + string(k),
		})
	}
	spec, err := ServiceSpec{
		Key: "chronicle", Hostname: teardownHost,
		Mode: ModeDirect, Upstream: "100.64.0.7", Access: AccessNone,
	}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	route := &fakeProv{kind: model.ResourceTunnelRoute}
	app := &fakeProv{kind: model.ResourceAccessApp}
	res, err := New(st, NewRegistry(route, app, dnsInPlace())).
		Ensure(context.Background(), Request{Spec: spec, Apply: true, Prune: true})
	if err != nil {
		t.Fatal(err)
	}

	for _, k := range []model.ResourceKind{model.ResourceTunnelRoute, model.ResourceAccessApp} {
		f := findingFor(t, res, k)
		if f.Status != StepRefused {
			t.Errorf("%s: %s, want %s", k, f.Status, StepRefused)
		}
		// It names the owner and the command that removes it. Only one, and only
		// one that works: "run their spec with --prune" reads well and mostly
		// does not, since their spec presumably still calls for this kind and so
		// has no orphan to prune (PRSR-46 review).
		for _, want := range []string{`"argosy"`, "teardown-service"} {
			if !strings.Contains(f.Err, want) {
				t.Errorf("%s: refusal does not mention %s: %q", k, want, f.Err)
			}
		}
		if got := st.rows[key(teardownHost, k)].Status; got != model.ResourceActive {
			t.Errorf("%s row is %q — nothing was removed, so nothing may be recorded as removed", k, got)
		}
	}
	if route.teardowns != 0 || app.teardowns != 0 {
		t.Errorf("another service's resource was deleted (route=%d app=%d)", route.teardowns, app.teardowns)
	}
	// It needs attention: the operator asked for a removal that did not happen.
	if len(res.NeedsAttention()) != 3 {
		t.Errorf("NeedsAttention has %d, want 3 (two refused orphans and the held adopt)", len(res.NeedsAttention()))
	}
	// **And the adopt is held**, which is the half a first version of this got
	// wrong. An adopt would rebind only *this* row to chronicle while the refused
	// kinds keep argosy — the half-reassigned state a teardown refuses outright —
	// so the run would create, in the act of refusing, the very condition that
	// stops the remedy its own message names (PRSR-46 review).
	if got := findingFor(t, res, model.ResourceDNSRecord).Status; got != StepRefused {
		t.Errorf("dns_record: %s, want %s — rebinding one row of a contested hostname splits its ownership", got, StepRefused)
	}
	for _, k := range model.KindOrder {
		if got := st.rows[key(teardownHost, k)].ServiceKey; got != "argosy" {
			t.Errorf("%s row moved to %q; every row at a contested hostname must keep one owner", k, got)
		}
	}

	// The assertion the whole thing is for: the command the refusal names has to
	// actually run afterwards.
	if _, err := New(st, NewRegistry(&fakeProv{kind: model.ResourceTunnelRoute},
		&fakeProv{kind: model.ResourceAccessApp}, dnsInPlace())).
		Teardown(context.Background(), TeardownRequest{ServiceKey: "argosy", Hostname: teardownHost}); err != nil {
		t.Errorf("the teardown this refusal tells the operator to run is refused: %v", err)
	}
}

// Without --prune the same disagreement is just an orphan: nothing was going to
// be removed, so there is nothing to refuse.
func TestPrune_AnotherServicesRowIsAnOrdinaryOrphanWithoutTheFlag(t *testing.T) {
	st := newStore()
	st.put(model.ServiceResource{
		ServiceKey: "argosy", Hostname: teardownHost, Kind: model.ResourceAccessApp,
		ExternalID: "app-1",
	})
	spec, err := ServiceSpec{
		Key: "chronicle", Hostname: teardownHost,
		Mode: ModeDirect, Upstream: "100.64.0.7", Access: AccessNone,
	}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	res, err := New(st, NewRegistry(&fakeProv{kind: model.ResourceTunnelRoute},
		&fakeProv{kind: model.ResourceAccessApp}, dnsInPlace())).
		Ensure(context.Background(), Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, res, model.ResourceAccessApp).Status; got != StepOrphaned {
		t.Errorf("access_app: %s, want %s", got, StepOrphaned)
	}
}

// A prune only ever touches a kind this spec does not call for. A spec that
// still wants everything has nothing to prune, whatever the flag says.
func TestPrune_TouchesOnlyWhatTheSpecNoLongerCallsFor(t *testing.T) {
	st := widerRows(t)
	route := &fakeProv{kind: model.ResourceTunnelRoute}
	app := &fakeProv{kind: model.ResourceAccessApp}
	svc := New(st, NewRegistry(route, app, dnsInPlace()),
		WithTunnels(prodTunnels()))

	res, err := svc.Ensure(context.Background(), Request{Spec: tunnelledSpec(), Apply: true, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if route.teardowns != 0 || app.teardowns != 0 {
		t.Errorf("a spec that calls for every kind pruned something (route=%d app=%d)", route.teardowns, app.teardowns)
	}
	for _, f := range res.Findings {
		if f.Status == StepPrune {
			t.Errorf("%s reports prune on a spec that calls for it", f.Kind)
		}
	}
}
