package spinup

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/model"
)

// --- fakes -----------------------------------------------------------------

// fakeProv is a provisioner whose answers a test sets directly, and which
// counts its calls — the dry-run guarantee is a claim about calls, not about
// results, so it can only be tested by counting them.
type fakeProv struct {
	kind    model.ResourceKind
	display string

	state      State
	inspectErr error
	ensured    Resource
	ensureErr  error

	inspects  int
	ensures   int
	teardowns int
}

func (p *fakeProv) Kind() model.ResourceKind { return p.kind }
func (p *fakeProv) DisplayName() string {
	if p.display != "" {
		return p.display
	}
	return string(p.kind)
}

func (p *fakeProv) Inspect(_ context.Context, _ Target) (State, error) {
	p.inspects++
	return p.state, p.inspectErr
}

func (p *fakeProv) Ensure(_ context.Context, _ Target) (Resource, error) {
	p.ensures++
	if p.ensureErr != nil {
		return Resource{}, p.ensureErr
	}
	return p.ensured, nil
}

func (p *fakeProv) Teardown(_ context.Context, _ Target, _ model.ServiceResource) error {
	p.teardowns++
	return nil
}

// present builds a provisioner reporting a resource that exists and matches.
func present(kind model.ResourceKind, externalID, parentID string) *fakeProv {
	return &fakeProv{
		kind:  kind,
		state: State{Exists: true, Matches: true, ExternalID: externalID, ParentID: parentID, Detail: "already correct"},
	}
}

// absent builds a provisioner reporting nothing there, whose Ensure creates it.
func absent(kind model.ResourceKind, externalID string) *fakeProv {
	return &fakeProv{
		kind:    kind,
		ensured: Resource{ExternalID: externalID, Detail: "created"},
	}
}

// fakeStore is an in-memory service_resource table, keyed the way the real
// unique index is: (lower(hostname), kind).
type fakeStore struct {
	mu   sync.Mutex
	rows map[string]model.ServiceResource
	// failUpsert, when set, makes UpsertServiceResource fail — the only way to
	// reach applied-not-recorded.
	failUpsert error
	// failList, when set, makes the record read fail.
	failList error
	upserts  int
}

func newStore() *fakeStore { return &fakeStore{rows: map[string]model.ServiceResource{}} }

func key(hostname string, kind model.ResourceKind) string {
	return strings.ToLower(hostname) + "|" + string(kind)
}

func (s *fakeStore) ServiceResourcesForHostname(_ context.Context, hostname string) ([]model.ServiceResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failList != nil {
		return nil, s.failList
	}
	var out []model.ServiceResource
	for _, k := range model.KindOrder {
		if r, ok := s.rows[key(hostname, k)]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *fakeStore) UpsertServiceResource(_ context.Context, r model.ServiceResource) (model.ServiceResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts++
	if s.failUpsert != nil {
		return model.ServiceResource{}, s.failUpsert
	}
	r.ID = uuid.New()
	r.Status = model.ResourceActive
	s.rows[key(r.Hostname, r.Kind)] = r
	return r, nil
}

func (s *fakeStore) put(r model.ServiceResource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Status == "" {
		r.Status = model.ResourceActive
	}
	r.ID = uuid.New()
	s.rows[key(r.Hostname, r.Kind)] = r
}

func (s *fakeStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

// --- helpers ---------------------------------------------------------------

func findingFor(t *testing.T, res *Result, kind model.ResourceKind) StepFinding {
	t.Helper()
	for _, f := range res.Findings {
		if f.Kind == kind {
			return f
		}
	}
	t.Fatalf("no finding for %q; a report must have a line per kind", kind)
	return StepFinding{}
}

func wantStatus(t *testing.T, res *Result, kind model.ResourceKind, want StepStatus) StepFinding {
	t.Helper()
	f := findingFor(t, res, kind)
	if f.Status != want {
		t.Errorf("%s: status = %q, want %q (err: %s)", kind, f.Status, want, f.Err)
	}
	return f
}

// prodTunnels is the only wired tunnel today (PRSR-33 wires dev).
func prodTunnels() TunnelSet {
	return TunnelSet{TunnelProd: "aef21667-03ce-45d3-b83c-d634822661cd"}
}

// --- tests -----------------------------------------------------------------

// The default is a preview, and a preview must not touch anything: the only
// upstream call a dry run makes is the read that decides the plan. This is the
// property that makes `--apply` worth having, so it is asserted by counting
// calls rather than by trusting the result.
func TestEnsure_DryRunTouchesNothing(t *testing.T) {
	dns := absent(model.ResourceDNSRecord, "rec-1")
	app := absent(model.ResourceAccessApp, "app-1")
	st := newStore()
	svc := New(st, NewRegistry(dns, app))

	res, err := svc.Ensure(context.Background(), Request{Spec: directSpec()})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(t, res, model.ResourceDNSRecord, StepCreate)
	wantStatus(t, res, model.ResourceAccessApp, StepCreate)
	if dns.ensures+app.ensures != 0 {
		t.Errorf("dry run called Ensure %d times; it must make no mutating call at all", dns.ensures+app.ensures)
	}
	if dns.inspects != 1 || app.inspects != 1 {
		t.Errorf("inspects = %d/%d, want one read per step", dns.inspects, app.inspects)
	}
	if st.count() != 0 {
		t.Errorf("dry run wrote %d rows", st.count())
	}
	if res.Changed() != 0 {
		t.Errorf("Changed() = %d on a dry run", res.Changed())
	}
	if res.Pending() != 2 {
		t.Errorf("Pending() = %d, want 2 — the count that says 're-run with --apply'", res.Pending())
	}
}

// The plan is the first half of the apply, not a description of it: the same
// Inspect answer drives both, so the statuses an --apply reports are the ones
// the dry run showed.
func TestEnsure_ApplyMatchesThePlan(t *testing.T) {
	spec := directSpec()

	plan, err := New(newStore(), NewRegistry(
		absent(model.ResourceDNSRecord, "rec-1"), absent(model.ResourceAccessApp, "app-1"),
	)).Ensure(context.Background(), Request{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}

	dns, app := absent(model.ResourceDNSRecord, "rec-1"), absent(model.ResourceAccessApp, "app-1")
	st := newStore()
	applied, err := New(st, NewRegistry(dns, app)).Ensure(context.Background(), Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatal(err)
	}

	for i, f := range plan.Findings {
		if applied.Findings[i].Status != f.Status {
			t.Errorf("%s: plan said %q, apply did %q", f.Kind, f.Status, applied.Findings[i].Status)
		}
	}
	if dns.ensures != 1 || app.ensures != 1 {
		t.Errorf("ensures = %d/%d, want one each", dns.ensures, app.ensures)
	}
	if applied.Changed() != 2 || st.count() != 2 {
		t.Errorf("changed = %d, rows = %d, want 2 and 2", applied.Changed(), st.count())
	}
	if applied.Pending() != 0 {
		t.Errorf("Pending() = %d after a successful apply", applied.Pending())
	}
}

// The pilot case, and the one that decides whether anybody trusts this against
// production: Argosy's edge already exists and predates the resource table
// entirely. An `Ensure` that cannot recognize it would re-create a live service's
// DNS record to obtain a row.
func TestEnsure_AdoptsAnExistingDeployment(t *testing.T) {
	dns := present(model.ResourceDNSRecord, "rec-1", "zone-1")
	app := present(model.ResourceAccessApp, "app-1", "acct-1")
	st := newStore()
	svc := New(st, NewRegistry(dns, app))

	res, err := svc.Ensure(context.Background(), Request{Spec: directSpec(), Apply: true})
	if err != nil {
		t.Fatal(err)
	}

	f := wantStatus(t, res, model.ResourceDNSRecord, StepAdopt)
	if !f.Applied || f.ExternalID != "rec-1" {
		t.Errorf("adopt did not record the id it found: %+v", f)
	}
	if dns.ensures+app.ensures != 0 {
		t.Errorf("adopting called Ensure %d times; an already-correct resource must not be rewritten to gain a row", dns.ensures+app.ensures)
	}
	if st.count() != 2 {
		t.Errorf("rows = %d, want the two ids adopted", st.count())
	}

	// Second run: recorded and correct, so there is nothing left to do at all.
	again, err := svc.Ensure(context.Background(), Request{Spec: directSpec(), Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, again, model.ResourceDNSRecord, StepOK)
	wantStatus(t, again, model.ResourceAccessApp, StepOK)
	if again.Changed() != 0 {
		t.Errorf("a second apply changed %d steps; the run must be idempotent", again.Changed())
	}
}

// A read that failed is not an answer. Reporting "absent" for a question the
// provisioner could not ask would have --apply create a second copy of something
// that already exists — and on the tunnel, rebuild a shared document from a read
// that just failed.
func TestEnsure_FailedInspectIsUnknownAndDoesNotAct(t *testing.T) {
	dns := &fakeProv{kind: model.ResourceDNSRecord, inspectErr: errors.New("502 from the api")}
	app := absent(model.ResourceAccessApp, "app-1")
	st := newStore()

	res, err := New(st, NewRegistry(dns, app)).Ensure(context.Background(),
		Request{Spec: directSpec(), Apply: true})
	if err != nil {
		t.Fatal(err)
	}

	f := wantStatus(t, res, model.ResourceDNSRecord, StepUnknown)
	if f.Err == "" {
		t.Error("an unknown step must carry the reason it could not be read")
	}
	if dns.ensures != 0 {
		t.Error("--apply acted on a step whose current state could not be read")
	}
	// And the other step still ran: one unreadable resource does not abort the
	// spec.
	if app.ensures != 1 {
		t.Error("a failed read of one step stopped the others from running")
	}
}

// Per-resource failures must not abort the whole spec — the same rule the invite
// path has, and it matters more here because the earlier steps have already
// changed the edge by the time a later one fails.
func TestEnsure_PerStepFailureDoesNotAbort(t *testing.T) {
	route := &fakeProv{kind: model.ResourceTunnelRoute, ensureErr: errors.New("ingress write rejected")}
	app := absent(model.ResourceAccessApp, "app-1")
	dns := absent(model.ResourceDNSRecord, "rec-1")

	res, err := New(newStore(), NewRegistry(route, app, dns), WithTunnels(prodTunnels())).
		Ensure(context.Background(), Request{Spec: tunnelledSpec(), Apply: true})
	if err != nil {
		t.Fatalf("a connector failure must be a finding, not an error: %v", err)
	}

	f := wantStatus(t, res, model.ResourceTunnelRoute, StepFailed)
	if f.Applied {
		t.Error("a failed step reported Applied")
	}
	wantStatus(t, res, model.ResourceAccessApp, StepCreate)
	wantStatus(t, res, model.ResourceDNSRecord, StepCreate)
	if app.ensures != 1 || dns.ensures != 1 {
		t.Error("one step's failure stopped the rest of the spec")
	}
}

// Unavailable is not a flavour of failed: nothing broke, nobody wired it up.
// Both the missing-provisioner case and the registered-but-unconfigured stub
// land in the same bucket, because they are the same news to an operator.
func TestEnsure_Unavailable(t *testing.T) {
	stub := NewUnavailable(model.ResourceAccessApp, "Access application",
		"set PURSER_CF_API_TOKEN to enable")
	// No DNS provisioner registered at all — a step this spec calls for that
	// this build cannot take.
	res, err := New(newStore(), NewRegistry(stub)).Ensure(context.Background(),
		Request{Spec: directSpec(), Apply: true})
	if err != nil {
		t.Fatal(err)
	}

	app := wantStatus(t, res, model.ResourceAccessApp, StepUnavailable)
	if !strings.Contains(app.Err, "PURSER_CF_API_TOKEN") {
		t.Errorf("an unavailable step must say what would fix it, got %q", app.Err)
	}
	dns := wantStatus(t, res, model.ResourceDNSRecord, StepUnavailable)
	if !strings.Contains(dns.Err, "no provisioner") {
		t.Errorf("err = %q", dns.Err)
	}
	if res.Changed() != 0 {
		t.Error("an unavailable step reported a change")
	}
}

// The inverse of a failure, and its own status for that reason: the edge changed
// and the row did not land, so Purser cannot target what it just created. Saying
// "failed" here would send an operator looking for something that isn't there.
func TestEnsure_AppliedNotRecorded(t *testing.T) {
	dns := absent(model.ResourceDNSRecord, "rec-1")
	app := absent(model.ResourceAccessApp, "app-1")
	st := newStore()
	st.failUpsert = errors.New("connection reset")

	res, err := New(st, NewRegistry(dns, app)).Ensure(context.Background(),
		Request{Spec: directSpec(), Apply: true})
	if err != nil {
		t.Fatal(err)
	}

	f := wantStatus(t, res, model.ResourceDNSRecord, StepAppliedNotRecorded)
	if f.Applied {
		t.Error("Applied must be false when the record didn't land — it is what a report counts as done")
	}
	if f.ExternalID != "rec-1" {
		t.Errorf("the id that was created must still be reported so a human can find it, got %q", f.ExternalID)
	}
	if dns.ensures != 1 {
		t.Error("the upstream write should have happened")
	}
}

// A record write that fails when nothing upstream was touched is an ordinary
// failure — not applied-not-recorded, which asserts the edge changed.
func TestEnsure_AdoptRecordFailureIsFailed(t *testing.T) {
	dns := present(model.ResourceDNSRecord, "rec-1", "zone-1")
	st := newStore()
	st.failUpsert = errors.New("connection reset")

	res, err := New(st, NewRegistry(dns)).Ensure(context.Background(),
		Request{Spec: func() ServiceSpec { s := directSpec(); s.Access = AccessNone; return s }(), Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, res, model.ResourceDNSRecord, StepFailed)
	if dns.ensures != 0 {
		t.Error("an adopt must not call Ensure even when recording fails")
	}
}

// A direct spec has no ingress route. The step is reported as not applicable
// rather than omitted, so "nothing about the tunnel" never has to be
// interpreted.
func TestEnsure_SkipsTheStepsTheSpecDoesNotCallFor(t *testing.T) {
	res, err := New(newStore(), NewRegistry(
		absent(model.ResourceDNSRecord, "rec-1"),
		absent(model.ResourceAccessApp, "app-1"),
		absent(model.ResourceTunnelRoute, ""),
	)).Ensure(context.Background(), Request{Spec: directSpec()})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != len(model.KindOrder) {
		t.Fatalf("got %d findings, want one per kind", len(res.Findings))
	}
	f := wantStatus(t, res, model.ResourceTunnelRoute, StepSkipped)
	if !strings.Contains(f.Detail, string(ModeDirect)) {
		t.Errorf("a skipped step must say why: %q", f.Detail)
	}
}

// A spec that used to be tunnelled and is now direct leaves a live ingress route
// behind. Nothing else in the report would mention it, and "skipped" would file
// it under nothing-to-think-about.
func TestEnsure_RecordedButNoLongerCalledForIsOrphaned(t *testing.T) {
	st := newStore()
	st.put(model.ServiceResource{
		ServiceKey: "argosy",
		Hostname:   directSpec().Hostname,
		Kind:       model.ResourceTunnelRoute,
		ParentID:   "aef21667-03ce-45d3-b83c-d634822661cd",
	})

	res, err := New(st, NewRegistry(absent(model.ResourceDNSRecord, "rec-1"),
		absent(model.ResourceAccessApp, "app-1"))).
		Ensure(context.Background(), Request{Spec: directSpec()})
	if err != nil {
		t.Fatal(err)
	}
	f := wantStatus(t, res, model.ResourceTunnelRoute, StepOrphaned)
	if !strings.Contains(f.Detail, "still live") {
		t.Errorf("an orphan must say the resource is presumably still there: %q", f.Detail)
	}
}

// A removed row is the history of a teardown, not a record of something that
// exists — reading it as one would make an adopt look like an ok and leave the
// resource unrecorded.
func TestEnsure_RemovedRecordDoesNotCountAsRecorded(t *testing.T) {
	st := newStore()
	spec := func() ServiceSpec { s := directSpec(); s.Access = AccessNone; return s }()
	st.put(model.ServiceResource{
		ServiceKey: "argosy", Hostname: spec.Hostname, Kind: model.ResourceDNSRecord,
		ExternalID: "rec-1", ParentID: "zone-1", Status: model.ResourceRemoved,
	})

	res, err := New(st, NewRegistry(present(model.ResourceDNSRecord, "rec-1", "zone-1"))).
		Ensure(context.Background(), Request{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, res, model.ResourceDNSRecord, StepAdopt)
}

// Both tunnelled steps are handed the same resolved id: the route is written
// into that tunnel's configuration and the DNS record points at
// <id>.cfargotunnel.com, so a disagreement would publish a hostname pointing at
// a tunnel that does not serve it.
func TestEnsure_ResolvesTheTunnelOnceForEveryStep(t *testing.T) {
	var seen []string
	spy := func(kind model.ResourceKind) *spyProv {
		return &spyProv{fakeProv: fakeProv{kind: kind}, onInspect: func(t Target) { seen = append(seen, t.TunnelID) }}
	}
	route, app, dns := spy(model.ResourceTunnelRoute), spy(model.ResourceAccessApp), spy(model.ResourceDNSRecord)

	if _, err := New(newStore(), NewRegistry(route, app, dns), WithTunnels(prodTunnels())).
		Ensure(context.Background(), Request{Spec: tunnelledSpec()}); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 3 {
		t.Fatalf("saw %d targets, want 3", len(seen))
	}
	for _, id := range seen {
		if id != "aef21667-03ce-45d3-b83c-d634822661cd" {
			t.Errorf("a step was handed tunnel id %q; every step must get the same resolved id", id)
		}
	}
}

// A tunnelled spec whose tunnel nobody configured is refused before any step
// runs — not half-built and then reported. The DNS record's target depends on
// the same id, so there is no useful subset of this spec to apply.
func TestEnsure_UnresolvableTunnelRefusesBeforeAnyCall(t *testing.T) {
	route := absent(model.ResourceTunnelRoute, "")
	dns := absent(model.ResourceDNSRecord, "rec-1")

	spec := tunnelledSpec()
	spec.Tunnel = TunnelDev // real tunnel, no id wired yet (PRSR-33)
	_, err := New(newStore(), NewRegistry(route, dns), WithTunnels(prodTunnels())).
		Ensure(context.Background(), Request{Spec: spec, Apply: true})
	if err == nil {
		t.Fatal("want a refusal for an unconfigured tunnel")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("err = %q", err)
	}
	if route.inspects+dns.inspects != 0 {
		t.Error("the refusal came after contacting upstream")
	}
}

// An invalid spec is refused before anything is read or written, which is the
// cheapest possible place to catch "this points at the wrong thing".
func TestEnsure_InvalidSpecRefusedBeforeAnyCall(t *testing.T) {
	dns := absent(model.ResourceDNSRecord, "rec-1")
	spec := directSpec()
	spec.Mode = ""

	if _, err := New(newStore(), NewRegistry(dns)).Ensure(context.Background(),
		Request{Spec: spec, Apply: true}); err == nil {
		t.Fatal("want a validation error")
	}
	if dns.inspects != 0 {
		t.Error("an invalid spec reached a provisioner")
	}
}

// The store's own failure is an error, not a finding: without knowing what is
// recorded, every step's verdict would be a guess.
func TestEnsure_RecordReadFailureIsAnError(t *testing.T) {
	st := newStore()
	st.failList = errors.New("db down")
	dns := absent(model.ResourceDNSRecord, "rec-1")

	if _, err := New(st, NewRegistry(dns)).Ensure(context.Background(),
		Request{Spec: directSpec()}); err == nil {
		t.Fatal("want an error when Purser's own records can't be read")
	}
	if dns.inspects != 0 {
		t.Error("inspected upstream without knowing what was recorded")
	}
}

func TestRegistry_DuplicateKindPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("two provisioners for one kind must panic at wiring time; one of them would silently never run")
		}
	}()
	NewRegistry(absent(model.ResourceDNSRecord, "a"), absent(model.ResourceDNSRecord, "b"))
}

// An existing resource that does not match the spec is an update, not a create:
// on the tunnel that is the read-modify-write of a document holding every other
// service's routes, and a plan that called it "create" would hide that.
func TestEnsure_MismatchIsAnUpdate(t *testing.T) {
	dns := &fakeProv{
		kind:    model.ResourceDNSRecord,
		state:   State{Exists: true, Matches: false, ExternalID: "rec-1", Detail: "points at the old endpoint"},
		ensured: Resource{ExternalID: "rec-1", Detail: "retargeted"},
	}
	spec := func() ServiceSpec { s := directSpec(); s.Access = AccessNone; return s }()

	res, err := New(newStore(), NewRegistry(dns)).Ensure(context.Background(),
		Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f := wantStatus(t, res, model.ResourceDNSRecord, StepUpdate)
	if !f.Applied || f.Detail != "retargeted" {
		t.Errorf("finding = %+v", f)
	}
}

// spyProv is a fakeProv that reports the Target it was handed.
type spyProv struct {
	fakeProv
	onInspect func(Target)
}

func (p *spyProv) Inspect(ctx context.Context, t Target) (State, error) {
	p.onInspect(t)
	return p.fakeProv.Inspect(ctx, t)
}

var _ ServiceProvisioner = (*spyProv)(nil)
var _ ServiceProvisioner = (*Unavailable)(nil)
