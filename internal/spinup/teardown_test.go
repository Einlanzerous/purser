package spinup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/model"
)

// PRSR-34: the teardown walk. Everything here is about the two things the ticket
// said would not be guessed at — what order removals go in, and what a teardown
// is entitled to remove — plus the invariants it inherits from offboard, where
// each was learned the expensive way.

// --- fakes -----------------------------------------------------------------

// orderProv is a fakeProv that appends to a shared log when torn down, so the
// *order* of the walk is testable rather than inferred from a passing result.
type orderProv struct {
	fakeProv
	log *[]model.ResourceKind
}

func (p *orderProv) Teardown(ctx context.Context, t Target, rec model.ServiceResource) (Removal, error) {
	*p.log = append(*p.log, p.kind)
	return p.fakeProv.Teardown(ctx, t, rec)
}

// refuser is a provisioner that cannot act and says so from CanTeardown as well
// as from Teardown — the shape spinup.Unavailable has, and the one a real
// provisioner is required to have so a plan cannot promise what an apply
// refuses.
type refuser struct {
	fakeProv
	err error
}

func (p *refuser) CanTeardown(Target) error { return p.err }
func (p *refuser) Teardown(context.Context, Target, model.ServiceResource) (Removal, error) {
	p.teardowns++
	return Removal{}, p.err
}

const teardownHost = "interlock.zerogravity.industries"

// seeded builds a store holding an active row for every kind at the hostname,
// all attributed to `interlock` — a whole tunnelled, gated service as a spin-up
// would have left it.
func seeded(t *testing.T) (*fakeStore, map[model.ResourceKind]model.ServiceResource) {
	t.Helper()
	st := newStore()
	rows := map[model.ResourceKind]model.ServiceResource{}
	for _, k := range model.KindOrder {
		rows[k] = st.put(model.ServiceResource{
			ServiceKey: "interlock",
			Hostname:   teardownHost,
			Kind:       k,
			ExternalID: "id-" + string(k),
			ParentID:   "parent-" + string(k),
		})
	}
	return st, rows
}

// allKinds builds a provisioner per kind, sharing one call log.
func allKinds(log *[]model.ResourceKind) []ServiceProvisioner {
	out := make([]ServiceProvisioner, 0, len(model.KindOrder))
	for _, k := range model.KindOrder {
		out = append(out, &orderProv{fakeProv: fakeProv{kind: k}, log: log})
	}
	return out
}

func teardownFindingFor(t *testing.T, res *TeardownResult, kind model.ResourceKind) TeardownFinding {
	t.Helper()
	for _, f := range res.Findings {
		if f.Kind == kind {
			return f
		}
	}
	t.Fatalf("no finding for %s; a report must have a line per kind so silence never has to be interpreted", kind)
	return TeardownFinding{}
}

func req(apply bool) TeardownRequest {
	return TeardownRequest{ServiceKey: "interlock", Hostname: teardownHost, Apply: apply}
}

// --- ordering ---------------------------------------------------------------

// The ticket called this "almost certainly the inverse of KindOrder" and asked
// for it to be stated and tested rather than inferred. The failure it prevents
// is KindOrder's mirror image: pull the Access application first and the service
// is briefly live and ungated.
func TestTeardown_RemovesDNSFirst(t *testing.T) {
	st, _ := seeded(t)
	var log []model.ResourceKind
	svc := New(st, NewRegistry(allKinds(&log)...))

	if _, err := svc.Teardown(context.Background(), req(true)); err != nil {
		t.Fatal(err)
	}

	want := []model.ResourceKind{model.ResourceDNSRecord, model.ResourceAccessApp, model.ResourceTunnelRoute}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Errorf("removal order %v, want %v — DNS goes first because it is what makes the hostname live", log, want)
	}
}

// TeardownOrder is derived from KindOrder rather than written out again, so the
// two cannot drift into removing the gate before the record with nothing
// anywhere reporting it.
func TestTeardownOrder_IsKindOrderReversed(t *testing.T) {
	got := model.TeardownOrder()
	if len(got) != len(model.KindOrder) {
		t.Fatalf("TeardownOrder has %d kinds, KindOrder has %d", len(got), len(model.KindOrder))
	}
	for i, k := range got {
		if want := model.KindOrder[len(model.KindOrder)-1-i]; k != want {
			t.Errorf("position %d is %s, want %s", i, k, want)
		}
	}
	// And it must not alias the package-level slice: a caller reversing it in
	// place would silently invert every spin-up in the process.
	model.TeardownOrder()[0] = "tampered"
	if model.KindOrder[len(model.KindOrder)-1] == "tampered" {
		t.Error("TeardownOrder handed back a view of KindOrder; a caller writing to it would reorder every spin-up")
	}
}

// --- preview by default -----------------------------------------------------

// offboard's rule, inherited: a dry run makes NO provisioner call at all — not
// a read-only one. It has nothing to ask, because the records are the plan.
func TestTeardown_ADryRunCallsNoProvisioner(t *testing.T) {
	st, _ := seeded(t)
	provs := allKinds(new([]model.ResourceKind))
	svc := New(st, NewRegistry(provs...))

	res, err := svc.Teardown(context.Background(), req(false))
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range provs {
		if n := p.(*orderProv).teardowns; n != 0 {
			t.Errorf("%s was called %d times on a dry run; the plan must contact nothing", p.Kind(), n)
		}
	}
	if st.removals != 0 {
		t.Errorf("a dry run marked %d rows removed", st.removals)
	}
	if res.Changed() != 0 {
		t.Errorf("Changed()=%d on a dry run", res.Changed())
	}
	if res.Pending() != len(model.KindOrder) {
		t.Errorf("Pending()=%d, want %d — every recorded resource is one --apply would remove", res.Pending(), len(model.KindOrder))
	}
	for _, f := range res.Findings {
		if f.Status != TeardownRemove {
			t.Errorf("%s: %s, want %s", f.Kind, f.Status, TeardownRemove)
		}
		if f.Applied {
			t.Errorf("%s reports Applied on a dry run", f.Kind)
		}
	}
}

// The apply removes, and records that it did.
func TestTeardown_ApplyRemovesAndRecords(t *testing.T) {
	st, rows := seeded(t)
	provs := allKinds(new([]model.ResourceKind))
	svc := New(st, NewRegistry(provs...))

	res, err := svc.Teardown(context.Background(), req(true))
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed() != len(model.KindOrder) {
		t.Fatalf("Changed()=%d, want %d", res.Changed(), len(model.KindOrder))
	}
	if len(res.NeedsAttention()) != 0 {
		t.Errorf("NeedsAttention: %v", res.NeedsAttention())
	}
	for _, k := range model.KindOrder {
		if got := st.rows[key(teardownHost, k)].Status; got != model.ResourceRemoved {
			t.Errorf("%s row is %q after a successful teardown, want %q", k, got, model.ResourceRemoved)
		}
	}
	// The row is marked, never deleted — it is the record that this hostname
	// once held this resource, and the audit exists to read it.
	if st.count() != len(rows) {
		t.Errorf("%d rows left, want %d — a torn-down row is marked, not deleted", st.count(), len(rows))
	}

	// And a second run is a clean no-op that says which kind of nothing it is.
	again, err := svc.Teardown(context.Background(), req(true))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range again.Findings {
		if f.Status != TeardownGone {
			t.Errorf("%s re-runs as %s, want %s — a completed teardown should say 'already removed', not 'Purser never had one'", f.Kind, f.Status, TeardownGone)
		}
	}
	if again.Pending() != 0 || again.Changed() != 0 {
		t.Errorf("re-run: pending=%d changed=%d, want 0/0", again.Pending(), again.Changed())
	}
}

// --- "is this hostname still someone's?" ------------------------------------

// PRSR-34's open question. Purser cannot settle it from the outside, so it
// requires two coordinates that agree and refuses the whole run — removing
// nothing — when they don't.
func TestTeardown_RefusesAHostnameRecordedToAnotherService(t *testing.T) {
	st, _ := seeded(t)
	// The hostname changed hands: somebody stood chronicle up on it, and
	// UpsertServiceResource rebound the row's service_key on conflict.
	row := st.rows[key(teardownHost, model.ResourceDNSRecord)]
	row.ServiceKey = "chronicle"
	st.rows[key(teardownHost, model.ResourceDNSRecord)] = row

	provs := allKinds(new([]model.ResourceKind))
	_, err := New(st, NewRegistry(provs...)).Teardown(context.Background(), req(true))
	if !errors.Is(err, ErrHostnameNotThisService) {
		t.Fatalf("want ErrHostnameNotThisService, got %v", err)
	}
	// Named, both ways round, because the operator has to know which answer to
	// go and check.
	for _, want := range []string{"chronicle", "interlock", string(model.ResourceDNSRecord)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
	// Nothing removed, and nothing contacted: the refusal is the whole run, not
	// a per-resource finding, because a hostname that changed hands is not a
	// thing to half-tear-down.
	for _, p := range provs {
		if n := p.(*orderProv).teardowns; n != 0 {
			t.Errorf("%s was torn down despite the refusal (%d calls)", p.Kind(), n)
		}
	}
	if st.removals != 0 {
		t.Errorf("%d rows marked removed despite the refusal", st.removals)
	}
}

// Every active row is checked, not just the first: a hostname whose kinds
// disagree with each other has been half-reassigned, which is the same question
// with a worse answer.
func TestTeardown_RefusesWhenOneKindDisagrees(t *testing.T) {
	st, _ := seeded(t)
	row := st.rows[key(teardownHost, model.ResourceTunnelRoute)]
	row.ServiceKey = "chronicle"
	st.rows[key(teardownHost, model.ResourceTunnelRoute)] = row

	_, err := New(st, NewRegistry(allKinds(new([]model.ResourceKind))...)).
		Teardown(context.Background(), req(true))
	if !errors.Is(err, ErrHostnameNotThisService) {
		t.Fatalf("want ErrHostnameNotThisService, got %v", err)
	}
}

// A *removed* row naming another service is the history of a hostname that
// changed hands, which legitimately happened. It must not refuse a teardown of
// what is there now.
func TestTeardown_ARemovedRowFromAFormerOwnerDoesNotRefuse(t *testing.T) {
	st := newStore()
	st.put(model.ServiceResource{
		ServiceKey: "chronicle", Hostname: teardownHost, Kind: model.ResourceDNSRecord,
		ExternalID: "old", Status: model.ResourceRemoved,
	})
	st.put(model.ServiceResource{
		ServiceKey: "interlock", Hostname: teardownHost, Kind: model.ResourceAccessApp,
		ExternalID: "app-1",
	})

	res, err := New(st, NewRegistry(allKinds(new([]model.ResourceKind))...)).
		Teardown(context.Background(), req(false))
	if err != nil {
		t.Fatalf("a removed row from a former owner must not refuse the run: %v", err)
	}
	if got := teardownFindingFor(t, res, model.ResourceAccessApp).Status; got != TeardownRemove {
		t.Errorf("access_app: %s, want %s", got, TeardownRemove)
	}
}

// Both identifiers are required, and refused before anything is read.
func TestTeardown_RequiresBothIdentifiers(t *testing.T) {
	svc := New(newStore(), NewRegistry())
	for name, r := range map[string]TeardownRequest{
		"no service":  {Hostname: teardownHost},
		"no hostname": {ServiceKey: "interlock"},
		// A hostname with a path matches no row, so without the shape check it
		// would report a clean sweep of a hostname nobody ever spelled that way.
		"a path": {ServiceKey: "interlock", Hostname: teardownHost + "/admin"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.Teardown(context.Background(), r); err == nil {
				t.Fatal("want a refusal")
			}
		})
	}
}

// Identity keys are folded exactly as a spin-up folded them when it wrote the
// row. A hostname folded one way going up and another coming down simply fails
// to find what it was going to remove.
func TestTeardown_FoldsItsIdentifiersLikeTheSpecDoes(t *testing.T) {
	st, _ := seeded(t)
	res, err := New(st, NewRegistry(allKinds(new([]model.ResourceKind))...)).
		Teardown(context.Background(), TeardownRequest{
			ServiceKey: "  Interlock ",
			Hostname:   "  INTERLOCK.zerogravity.industries.  ",
		})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hostname != teardownHost || res.ServiceKey != "interlock" {
		t.Fatalf("normalized to %q/%q", res.ServiceKey, res.Hostname)
	}
	if res.Pending() != len(model.KindOrder) {
		t.Errorf("Pending()=%d — the padded spelling found none of its own rows", res.Pending())
	}
}

// --- what a teardown may remove ---------------------------------------------

// A row exists only for a resource Purser created, so "no row" is not "nothing
// is there" — it is "Purser put nothing here", and anything upstream at the
// hostname is somebody else's. The two nothings are separate statuses because
// they are separate news.
func TestTeardown_NoRecordIsNotTheSameAsAlreadyRemoved(t *testing.T) {
	st := newStore()
	st.put(model.ServiceResource{
		ServiceKey: "interlock", Hostname: teardownHost, Kind: model.ResourceAccessApp,
		ExternalID: "app-1", Status: model.ResourceRemoved,
	})
	provs := allKinds(new([]model.ResourceKind))

	res, err := New(st, NewRegistry(provs...)).Teardown(context.Background(), req(true))
	if err != nil {
		t.Fatal(err)
	}
	if got := teardownFindingFor(t, res, model.ResourceAccessApp).Status; got != TeardownGone {
		t.Errorf("access_app: %s, want %s", got, TeardownGone)
	}
	if got := teardownFindingFor(t, res, model.ResourceDNSRecord).Status; got != TeardownNone {
		t.Errorf("dns_record: %s, want %s", got, TeardownNone)
	}
	for _, p := range provs {
		if n := p.(*orderProv).teardowns; n != 0 {
			t.Errorf("%s was called for a kind with no active row (%d times) — Purser may only target ids it recorded", p.Kind(), n)
		}
	}
}

// --- ordering is enforced, not merely intended ------------------------------

// The mirror of StepBlocked. Ordering alone only closes the window when the
// earlier removal actually landed: take the gate away behind a DNS record that
// is still live and the service is reachable ungated.
func TestTeardown_AFailedDNSRemovalBlocksTheRest(t *testing.T) {
	st, _ := seeded(t)
	boom := errors.New("cloudflare: 500")
	dns := &orderProv{fakeProv: fakeProv{kind: model.ResourceDNSRecord, teardownErr: boom}, log: new([]model.ResourceKind)}
	app := &orderProv{fakeProv: fakeProv{kind: model.ResourceAccessApp}, log: new([]model.ResourceKind)}
	route := &orderProv{fakeProv: fakeProv{kind: model.ResourceTunnelRoute}, log: new([]model.ResourceKind)}

	res, err := New(st, NewRegistry(dns, app, route)).Teardown(context.Background(), req(true))
	if err != nil {
		t.Fatal(err)
	}
	if got := teardownFindingFor(t, res, model.ResourceDNSRecord).Status; got != TeardownFailed {
		t.Fatalf("dns_record: %s, want %s", got, TeardownFailed)
	}
	for _, k := range []model.ResourceKind{model.ResourceAccessApp, model.ResourceTunnelRoute} {
		f := teardownFindingFor(t, res, k)
		if f.Status != TeardownBlocked {
			t.Errorf("%s: %s, want %s — the hostname still resolves", k, f.Status, TeardownBlocked)
		}
		if !strings.Contains(f.Detail, string(model.ResourceDNSRecord)) {
			t.Errorf("%s: blocked detail must name what held it back, got %q", k, f.Detail)
		}
	}
	if app.teardowns != 0 || route.teardowns != 0 {
		t.Errorf("a blocked step was still performed (app=%d route=%d)", app.teardowns, route.teardowns)
	}
	// Blocked counts as needing attention: the resource did not go, and the
	// operator asked for a service to be taken down.
	if len(res.NeedsAttention()) != 3 {
		t.Errorf("NeedsAttention has %d, want 3 (one failure and two blocked)", len(res.NeedsAttention()))
	}
	// And nothing was recorded as removed — the whole point of PRSR-17.
	for _, k := range model.KindOrder {
		if got := st.rows[key(teardownHost, k)].Status; got != model.ResourceActive {
			t.Errorf("%s row is %q; a removal that didn't happen must never be recorded as one", k, got)
		}
	}
}

// The one case teardownDependsOn deliberately lets through: Purser holds no DNS
// record, so it cannot take one away, and blocking would hold the rest for ever
// with no command to type. It says so instead — the shape offboard's SSO warning
// has, and for the same reason.
func TestTeardown_NoRecordedDNSWarnsRatherThanBlocking(t *testing.T) {
	st := newStore()
	st.put(model.ServiceResource{
		ServiceKey: "interlock", Hostname: teardownHost, Kind: model.ResourceAccessApp, ExternalID: "app-1",
	})
	res, err := New(st, NewRegistry(allKinds(new([]model.ResourceKind))...)).
		Teardown(context.Background(), req(false))
	if err != nil {
		t.Fatal(err)
	}
	f := teardownFindingFor(t, res, model.ResourceAccessApp)
	if f.Status != TeardownRemove {
		t.Fatalf("access_app: %s, want %s — a record Purser never held cannot block for ever", f.Status, TeardownRemove)
	}
	if !strings.Contains(f.Warning, "ungated") {
		t.Errorf("the removal must say the hostname may still resolve, got warning %q", f.Warning)
	}
	// The route gets no such warning: removed under a live hostname it produces
	// a 502, which announces itself. The gate does not.
	if w := teardownFindingFor(t, res, model.ResourceTunnelRoute).Warning; w != "" {
		t.Errorf("tunnel_route carries a warning it has no use for: %q", w)
	}
}

// --- statuses ---------------------------------------------------------------

// A plan makes no call, so it cannot learn from an Inspect that a provisioner is
// unconfigured — which is what CanTeardown is for. Without it the plan says
// `remove` and the apply says `unavailable`, and "the preview is exactly what
// the apply does" stops being true on the one command you cannot take back.
func TestTeardown_AnUnconfiguredProvisionerReadsTheSameInThePlanAndTheApply(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(fmt.Sprintf("apply=%v", apply), func(t *testing.T) {
			st, _ := seeded(t)
			dns := &refuser{fakeProv: fakeProv{kind: model.ResourceDNSRecord},
				err: fmt.Errorf("%w: set PURSER_CF_ZONE_ID", ErrUnavailable)}
			res, err := New(st, NewRegistry(dns,
				&fakeProv{kind: model.ResourceAccessApp},
				&fakeProv{kind: model.ResourceTunnelRoute})).
				Teardown(context.Background(), req(apply))
			if err != nil {
				t.Fatal(err)
			}
			f := teardownFindingFor(t, res, model.ResourceDNSRecord)
			if f.Status != TeardownUnavailable {
				t.Errorf("dns_record: %s, want %s", f.Status, TeardownUnavailable)
			}
			if !strings.Contains(f.Err, "PURSER_CF_ZONE_ID") {
				t.Errorf("the line must name the variable to set, got %q", f.Err)
			}
			// And the steps behind it are held: an unavailable DNS removal is
			// still a hostname that resolves.
			if got := teardownFindingFor(t, res, model.ResourceAccessApp).Status; got != TeardownBlocked {
				t.Errorf("access_app: %s, want %s", got, TeardownBlocked)
			}
			if !apply && dns.teardowns != 0 {
				t.Error("the plan called Teardown; CanTeardown must answer without contacting anything")
			}
		})
	}
}

// A refusal and a failure want opposite things from an operator — fix something
// upstream, or just run it again — so the difference is a status rather than a
// clause in Err. This is PRSR-31's split, on the way down.
func TestTeardown_RefusedIsNotFailed(t *testing.T) {
	st, _ := seeded(t)
	dns := &fakeProv{kind: model.ResourceDNSRecord,
		teardownErr: fmt.Errorf("%w: the catch-all is not last", ErrRefused)}
	res, err := New(st, NewRegistry(dns,
		&fakeProv{kind: model.ResourceAccessApp},
		&fakeProv{kind: model.ResourceTunnelRoute})).
		Teardown(context.Background(), req(true))
	if err != nil {
		t.Fatal(err)
	}
	if got := teardownFindingFor(t, res, model.ResourceDNSRecord).Status; got != TeardownRefused {
		t.Errorf("dns_record: %s, want %s", got, TeardownRefused)
	}
	if got := st.rows[key(teardownHost, model.ResourceDNSRecord)].Status; got != model.ResourceActive {
		t.Errorf("a refused removal marked the row %q; nothing was removed", got)
	}
}

// The inverse of applied-not-recorded, and it points the opposite way from
// failed: the resource IS gone, and Purser's rows say otherwise.
func TestTeardown_RemovedNotRecordedIsItsOwnStatus(t *testing.T) {
	st, _ := seeded(t)
	st.failRemove = errors.New("store: connection reset")
	res, err := New(st, NewRegistry(allKinds(new([]model.ResourceKind))...)).
		Teardown(context.Background(), req(true))
	if err != nil {
		t.Fatal(err)
	}
	f := teardownFindingFor(t, res, model.ResourceDNSRecord)
	if f.Status != TeardownRemovedNotRecorded {
		t.Fatalf("dns_record: %s, want %s", f.Status, TeardownRemovedNotRecorded)
	}
	if f.Applied {
		t.Error("a step whose record did not land must not report Applied")
	}
	// It needs attention — Purser now holds an active row for something that
	// does not exist, which a later spin-up reads as a resource to adopt.
	if len(res.NeedsAttention()) == 0 {
		t.Error("removed-not-recorded must need attention")
	}
	// But the resource IS gone, so the steps ordered behind it are not held:
	// blocking there would refuse to remove a gate because the record that is
	// already gone could not be written down.
	if got := teardownFindingFor(t, res, model.ResourceAccessApp).Status; got == TeardownBlocked {
		t.Error("access_app was blocked behind a DNS record that is actually gone")
	}
}

// A provisioner's warning reaches the finding rather than a log nobody reading
// the report will see. This is the gap PRSR-30's review filed and the reason
// Teardown returns a Removal at all.
func TestTeardown_AProvisionerWarningReachesTheReport(t *testing.T) {
	st, _ := seeded(t)
	route := &fakeProv{kind: model.ResourceTunnelRoute,
		removed: Removal{Detail: "removed 1 rule", Warning: "another writer changed the shared configuration"}}
	res, err := New(st, NewRegistry(&fakeProv{kind: model.ResourceDNSRecord},
		&fakeProv{kind: model.ResourceAccessApp}, route)).
		Teardown(context.Background(), req(true))
	if err != nil {
		t.Fatal(err)
	}
	f := teardownFindingFor(t, res, model.ResourceTunnelRoute)
	if !strings.Contains(f.Warning, "another writer") {
		t.Errorf("warning %q — the one message that says another service may have lost its route", f.Warning)
	}
	if f.Detail != "removed 1 rule" {
		t.Errorf("detail %q, want the removal's own description", f.Detail)
	}
	// A warning does not make a step a failure.
	if f.Status != TeardownRemove || !f.Applied {
		t.Errorf("status=%s applied=%v; the removal succeeded", f.Status, f.Applied)
	}
}

// --- verdicts ---------------------------------------------------------------

// Pending counts only what --apply would act on, so it is not a verdict:
// unavailable, refused, blocked and failed are all excluded because the missing
// flag was never why they didn't happen. NeedsAttention is the verdict, and it
// lives on the result so the CLI's exit code and the HTTP response cannot drift.
func TestTeardownResult_PendingIsNotAVerdict(t *testing.T) {
	st, _ := seeded(t)
	res, err := New(st, NewRegistry(
		NewUnavailable(model.ResourceDNSRecord, "DNS record", "set PURSER_CF_ZONE_ID"),
		NewUnavailable(model.ResourceAccessApp, "Access application", "set PURSER_CF_API_TOKEN"),
		NewUnavailable(model.ResourceTunnelRoute, "ingress route", "set PURSER_CF_API_TOKEN"),
	)).Teardown(context.Background(), req(true))
	if err != nil {
		t.Fatal(err)
	}
	if res.Pending() != 0 || res.Changed() != 0 {
		t.Fatalf("pending=%d changed=%d — an unconfigured deployment looks identical to a clean one on these two", res.Pending(), res.Changed())
	}
	if len(res.NeedsAttention()) != len(model.KindOrder) {
		t.Errorf("NeedsAttention has %d, want %d — this is the field that has to disagree",
			len(res.NeedsAttention()), len(model.KindOrder))
	}
}

// A kind whose provisioner this build does not have is unavailable, never
// "nothing to do": the resource is still there and the row still says so.
func TestTeardown_AnUnregisteredKindIsUnavailableNotSilence(t *testing.T) {
	st, _ := seeded(t)
	res, err := New(st, NewRegistry(&fakeProv{kind: model.ResourceDNSRecord})).
		Teardown(context.Background(), req(false))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []model.ResourceKind{model.ResourceAccessApp, model.ResourceTunnelRoute} {
		if got := teardownFindingFor(t, res, k).Status; got != TeardownUnavailable {
			t.Errorf("%s: %s, want %s", k, got, TeardownUnavailable)
		}
	}
}
