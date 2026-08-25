package spinup

import (
	"context"
	"errors"
	"fmt"
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
// path has — but "don't abort" is not "carry on regardless". An *independent*
// step still runs; a step that depends on the failed one is held.
//
// The route and the Access app are independent of each other, so the app runs
// after the route fails. DNS depends on both, so it does not.
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
	if app.ensures != 1 {
		t.Error("a failure in an unrelated step stopped the Access app from being created")
	}
	wantStatus(t, res, model.ResourceDNSRecord, StepBlocked)
	if dns.ensures != 0 {
		t.Error("DNS was published while the route it depends on had failed")
	}
}

// The whole reason DNS is ordered last: it is the step that makes the hostname
// live, so publishing it after a failed Access step is what turns "gated" into
// "reachable by anyone". Ordering alone doesn't prevent that — only a step that
// refuses to run does.
func TestEnsure_GatedAccessFailureBlocksDNS(t *testing.T) {
	app := &fakeProv{kind: model.ResourceAccessApp, ensureErr: errors.New("policy rejected")}
	dns := absent(model.ResourceDNSRecord, "rec-1")
	spec := directSpec()
	spec.Access = AccessGated // direct, so the route is out of the picture
	st := newStore()

	res, err := New(st, NewRegistry(app, dns)).Ensure(context.Background(), Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(t, res, model.ResourceAccessApp, StepFailed)
	f := wantStatus(t, res, model.ResourceDNSRecord, StepBlocked)
	if dns.ensures != 0 {
		t.Fatal("a gated service was published with no Access application — reachable, and ungated")
	}
	if !strings.Contains(f.Detail, string(model.ResourceAccessApp)) {
		t.Errorf("a blocked step must name what held it: %q", f.Detail)
	}
	if st.count() != 0 {
		t.Error("a blocked step recorded a resource")
	}
	if res.Pending() != 0 {
		t.Error("blocked steps must not count as pending — re-running with --apply is not what fixes them")
	}
}

// Every non-landed state blocks, not just failure: unavailable and unknown mean
// the gate is equally absent. Unavailable is knowable without acting, so the
// hold shows up in the *plan*, before anyone types --apply.
func TestEnsure_UnavailableOrUnknownDependencyBlocksDNS(t *testing.T) {
	spec := directSpec()
	spec.Access = AccessGated

	t.Run("unavailable, in the preview", func(t *testing.T) {
		app := NewUnavailable(model.ResourceAccessApp, "Access application", "set PURSER_CF_API_TOKEN to enable")
		dns := absent(model.ResourceDNSRecord, "rec-1")
		res, err := New(newStore(), NewRegistry(app, dns)).Ensure(context.Background(), Request{Spec: spec})
		if err != nil {
			t.Fatal(err)
		}
		wantStatus(t, res, model.ResourceAccessApp, StepUnavailable)
		wantStatus(t, res, model.ResourceDNSRecord, StepBlocked)
	})

	t.Run("unknown", func(t *testing.T) {
		app := &fakeProv{kind: model.ResourceAccessApp, inspectErr: errors.New("502 from the api")}
		dns := absent(model.ResourceDNSRecord, "rec-1")
		res, err := New(newStore(), NewRegistry(app, dns)).Ensure(context.Background(), Request{Spec: spec, Apply: true})
		if err != nil {
			t.Fatal(err)
		}
		wantStatus(t, res, model.ResourceAccessApp, StepUnknown)
		wantStatus(t, res, model.ResourceDNSRecord, StepBlocked)
		if dns.ensures != 0 {
			t.Error("published a gated hostname while the gate's state was unreadable")
		}
	})
}

// A bookmark is a launcher tile in front of a service that holds its own login,
// so its absence costs an icon, not a gate. Blocking a working service's DNS
// over a missing tile would be the wrong trade — and this is the distinction
// that makes AccessShape more than a flag.
func TestEnsure_BookmarkAccessFailureDoesNotBlockDNS(t *testing.T) {
	app := &fakeProv{kind: model.ResourceAccessApp, ensureErr: errors.New("logo fetch failed")}
	dns := absent(model.ResourceDNSRecord, "rec-1")

	res, err := New(newStore(), NewRegistry(app, dns)).
		Ensure(context.Background(), Request{Spec: directSpec(), Apply: true}) // bookmark
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, res, model.ResourceAccessApp, StepFailed)
	f := wantStatus(t, res, model.ResourceDNSRecord, StepCreate)
	if !f.Applied || dns.ensures != 1 {
		t.Error("a failed launcher tile held back a service that has its own login")
	}
}

// Blocking withholds *changes*, not the report. A record that is already correct
// is already published, so refusing to look at it protects nobody and hides the
// state — and an adopt writes a row without touching the edge.
func TestEnsure_BlockingDoesNotHideAnAlreadyPublishedRecord(t *testing.T) {
	app := &fakeProv{kind: model.ResourceAccessApp, ensureErr: errors.New("policy rejected")}
	dns := present(model.ResourceDNSRecord, "rec-1", "zone-1")
	spec := directSpec()
	spec.Access = AccessGated
	st := newStore()

	res, err := New(st, NewRegistry(app, dns)).Ensure(context.Background(), Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, res, model.ResourceAccessApp, StepFailed)
	f := wantStatus(t, res, model.ResourceDNSRecord, StepAdopt)
	if !f.Applied || st.count() != 1 {
		t.Error("an adopt was blocked; it changes nothing upstream and the record is what makes teardown possible")
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

// The orchestrator walks KindOrder, so a provisioner registered under a kind
// that isn't in it is never asked for anything and never appears in a report:
// the spin-up looks complete while one of its steps was never run.
func TestRegistry_UnknownKindPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a kind the orchestrator never asks for must panic at wiring time, not run silently never")
		}
	}()
	NewRegistry(absent(model.ResourceKind("dns_recrod"), "a"))
}

// A Service with no registry reports every step unavailable — what an
// unconfigured deployment should look like — rather than panicking partway
// through a spec.
func TestRegistry_NilIsEveryStepUnavailable(t *testing.T) {
	res, err := New(newStore(), nil).Ensure(context.Background(), Request{Spec: directSpec(), Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, res, model.ResourceDNSRecord, StepUnavailable)
	wantStatus(t, res, model.ResourceAccessApp, StepUnavailable)
}

// Purser recorded it and upstream doesn't have it: the apply recreates, so the
// action is a create — but reporting it as one never tells the operator that
// something they created was deleted outside Purser.
func TestEnsure_RecordedButGoneUpstreamIsMissing(t *testing.T) {
	spec := func() ServiceSpec { s := directSpec(); s.Access = AccessNone; return s }()
	st := newStore()
	st.put(model.ServiceResource{
		ServiceKey: "argosy", Hostname: spec.Hostname, Kind: model.ResourceDNSRecord,
		ExternalID: "rec-1", ParentID: "zone-1",
	})
	dns := absent(model.ResourceDNSRecord, "rec-2")

	res, err := New(st, NewRegistry(dns)).Ensure(context.Background(), Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f := wantStatus(t, res, model.ResourceDNSRecord, StepMissing)
	if !f.Applied || f.ExternalID != "rec-2" {
		t.Errorf("the recreate should have rebound the row to the new id: %+v", f)
	}
}

// A provisioner that returns a partial Resource must not blank out a known-good
// external_id: that id is Teardown's only handle, and an empty one means "this
// kind has none" rather than "we lost it".
func TestEnsure_PartialResourceKeepsTheKnownID(t *testing.T) {
	dns := &fakeProv{
		kind:  model.ResourceDNSRecord,
		state: State{Exists: true, Matches: false, ExternalID: "rec-1", ParentID: "zone-1", Detail: "old target"},
		// A successful update that reported nothing back.
		ensured: Resource{},
	}
	spec := func() ServiceSpec { s := directSpec(); s.Access = AccessNone; return s }()
	st := newStore()

	res, err := New(st, NewRegistry(dns)).Ensure(context.Background(), Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f := wantStatus(t, res, model.ResourceDNSRecord, StepUpdate)
	if f.ExternalID != "rec-1" {
		t.Errorf("external id = %q, want the id Inspect found rather than an empty one", f.ExternalID)
	}
	row := st.rows[key(spec.Hostname, model.ResourceDNSRecord)]
	if row.ExternalID != "rec-1" || row.ParentID != "zone-1" {
		t.Errorf("recorded %+v; a partial result wiped the coordinates teardown needs", row)
	}
	// And Detail is the result, not the state that was just replaced.
	if f.Detail == "old target" {
		t.Error("the finding describes the pre-change state as though it were the outcome")
	}
}

// A hostname reassigned to another service must be re-attributed, or
// ServiceResourcesFor answers for the old owner for ever.
func TestEnsure_ReassignedHostnameIsAdopted(t *testing.T) {
	spec := func() ServiceSpec { s := directSpec(); s.Access = AccessNone; s.Key = "interlock"; return s }()
	st := newStore()
	st.put(model.ServiceResource{
		ServiceKey: "argosy", Hostname: spec.Hostname, Kind: model.ResourceDNSRecord,
		ExternalID: "rec-1", ParentID: "zone-1",
	})
	dns := present(model.ResourceDNSRecord, "rec-1", "zone-1")

	res, err := New(st, NewRegistry(dns)).Ensure(context.Background(), Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, res, model.ResourceDNSRecord, StepAdopt)
	if dns.ensures != 0 {
		t.Error("re-attributing a record is a row change, not an upstream one")
	}
	if got := st.rows[key(spec.Hostname, model.ResourceDNSRecord)].ServiceKey; got != "interlock" {
		t.Errorf("service_key = %q, want the spec's owner", got)
	}
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

// --- refused vs unknown (PRSR-31) -------------------------------------------

// The split PRSR-30's review filed and PRSR-31 settled. Both states decline to
// act; they differ in what an operator should do next, and until this the
// difference lived in the Err string — a second field a reader has to know to
// consult, which is the shape PRSR-21 removed from the person axis.
func TestEnsure_RefusedIsNotUnknown(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want StepStatus
	}{
		{
			// The read never completed. Re-running is the whole fix.
			name: "a transport failure is unknown",
			err:  errors.New("502 from the api"),
			want: StepUnknown,
		},
		{
			// The read completed and came back with a document nobody may write
			// to. Re-running repeats it until somebody fixes it upstream.
			name: "an unwritable upstream is refused",
			err:  fmt.Errorf("%w: the catch-all rule is not last", ErrRefused),
			want: StepRefused,
		},
		{
			// Wrapped several layers deep, the way a provisioner actually
			// returns it — inspectIngress wraps documentShape, which wraps the
			// sentinel.
			name: "refused through several wrappings",
			err:  fmt.Errorf("cloudflare: tunnel abc: %w", fmt.Errorf("%w: no catch-all at all", ErrRefused)),
			want: StepRefused,
		},
		{
			// And unavailable still wins over both: an unconfigured provisioner
			// is Purser's own gap, fixed here rather than upstream.
			name: "unavailable is neither",
			err:  fmt.Errorf("%w: set PURSER_CF_ZONE_ID", ErrUnavailable),
			want: StepUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := &fakeProv{kind: model.ResourceTunnelRoute, inspectErr: tc.err}
			st := newStore()
			res, err := New(st, NewRegistry(route), WithTunnels(prodTunnels())).
				Ensure(context.Background(), Request{Spec: tunnelledSpec(), Apply: true})
			if err != nil {
				t.Fatal(err)
			}
			f := wantStatus(t, res, model.ResourceTunnelRoute, tc.want)
			if f.Err == "" {
				t.Error("every one of these must carry the reason")
			}
			if route.ensures != 0 {
				t.Error("--apply acted on a step it could not decide from")
			}
		})
	}
}

// A refusal reaching Ensure rather than Inspect is the document changing shape
// between the plan and the apply. It is still refused rather than failed: the
// operator's next move is the same, and nothing was written either way.
func TestEnsure_RefusalOnTheWritePathIsAlsoRefused(t *testing.T) {
	route := &fakeProv{
		kind:      model.ResourceTunnelRoute,
		ensureErr: fmt.Errorf("%w: the catch-all rule moved", ErrRefused),
	}
	res, err := New(newStore(), NewRegistry(route), WithTunnels(prodTunnels())).
		Ensure(context.Background(), Request{Spec: tunnelledSpec(), Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, res, model.ResourceTunnelRoute, StepRefused)
}

// Refused behaves like unknown everywhere it should: it is not in place, so it
// holds the DNS step, and it is not pending, because --apply is not what fixes
// it.
func TestEnsure_RefusedBlocksDNSAndIsNotPending(t *testing.T) {
	route := &fakeProv{
		kind:       model.ResourceTunnelRoute,
		inspectErr: fmt.Errorf("%w: the catch-all rule is not last", ErrRefused),
	}
	dns := absent(model.ResourceDNSRecord, "rec-1")
	st := newStore()

	res, err := New(st, NewRegistry(route, dns), WithTunnels(prodTunnels())).
		Ensure(context.Background(), Request{Spec: tunnelledSpec(), Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, res, model.ResourceTunnelRoute, StepRefused)
	wantStatus(t, res, model.ResourceDNSRecord, StepBlocked)
	if dns.ensures != 0 {
		t.Fatal("a hostname was published in front of a tunnel that cannot serve it")
	}
	if res.Pending() != 0 {
		t.Error("neither a refused step nor the step it blocks is fixed by --apply, so neither is pending")
	}
	if st.count() != 0 {
		t.Error("nothing should have been recorded")
	}
}

// A warning belongs to a step that *succeeded*, so it must not be confused with
// Err and must not quietly become part of the description. The tunnel's is the
// one that matters: it says another service's ingress route may have been
// dropped from the shared document, which the step's own status cannot convey.
func TestEnsure_CarriesAWarningFromAStepThatSucceeded(t *testing.T) {
	route := &fakeProv{
		kind:    model.ResourceTunnelRoute,
		ensured: Resource{Detail: "routed to http://interlock:8080", Warning: "another writer changed the shared configuration"},
	}
	res, err := New(newStore(), NewRegistry(route), WithTunnels(prodTunnels())).
		Ensure(context.Background(), Request{Spec: tunnelledSpec(), Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f := wantStatus(t, res, model.ResourceTunnelRoute, StepCreate)
	if !f.Applied {
		t.Error("a warning is not a failure — the step did what it said")
	}
	if f.Warning != "another writer changed the shared configuration" {
		t.Errorf("the warning should reach the finding, got %q", f.Warning)
	}
	if f.Err != "" {
		t.Errorf("a warning is not an error, got Err=%q", f.Err)
	}
	if strings.Contains(f.Detail, "another writer") {
		t.Errorf("it must not also be folded into the description: %q", f.Detail)
	}
}

// An orphan is the one status where "nothing needs attention" is true and "the
// edge holds only what the spec asks for" is not — everything the spec calls
// for is in place, and something it no longer calls for is still live.
//
// Excluded on purpose: Ensure only adds and updates, so counting it would have
// every run of a deliberately narrowed spec report trouble for ever with no
// command to type. Pinned so the exclusion is a decision rather than an
// oversight, and so PRSR-34 has to come past this test to change it.
func TestNeedsAttention_ExcludesAnOrphanButNotTheRest(t *testing.T) {
	// A spec that no longer wants an Access app, with one still recorded.
	spec := directSpec()
	spec.Access = AccessNone
	st := newStore()
	st.put(model.ServiceResource{
		ServiceKey: spec.Key, Hostname: spec.Hostname,
		Kind: model.ResourceAccessApp, ExternalID: "app-1", Status: model.ResourceActive,
	})

	res, err := New(st, NewRegistry(present(model.ResourceDNSRecord, "rec-1", "zone-1"))).
		Ensure(context.Background(), Request{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, res, model.ResourceAccessApp, StepOrphaned)
	if n := len(res.NeedsAttention()); n != 0 {
		t.Errorf("an orphan is not something --apply or this axis can act on, got %d needing attention", n)
	}

	// ...and the exclusion is specific to orphaned, not a hole in the list.
	for _, status := range []StepStatus{
		StepUnavailable, StepRefused, StepUnknown, StepBlocked, StepFailed, StepAppliedNotRecorded,
	} {
		r := &Result{Findings: []StepFinding{{Kind: model.ResourceDNSRecord, Status: status}}}
		if len(r.NeedsAttention()) != 1 {
			t.Errorf("%s should need attention", status)
		}
	}
	for _, status := range []StepStatus{StepOK, StepAdopt, StepCreate, StepUpdate, StepSkipped, StepMissing, StepOrphaned} {
		r := &Result{Findings: []StepFinding{{Kind: model.ResourceDNSRecord, Status: status}}}
		if len(r.NeedsAttention()) != 0 {
			t.Errorf("%s should not need attention", status)
		}
	}
}
