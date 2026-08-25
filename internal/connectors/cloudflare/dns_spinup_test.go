package cloudflare

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

// The DNS provisioner is only half of the step — the other half is which
// StepStatus the orchestrator reads out of it. These run the real provisioner
// through spinup.Ensure against the fake zone, because the two claims that
// matter most are joint ones: that a dry run makes no write, and that a service
// which is already up comes back as `adopt` rather than as something to create.

// memStore is the smallest spinup.Store: the resource rows, keyed the way the
// real unique index is.
type memStore struct {
	mu   sync.Mutex
	rows map[string]model.ServiceResource
}

func newMemStore() *memStore { return &memStore{rows: map[string]model.ServiceResource{}} }

func rowKey(hostname string, kind model.ResourceKind) string {
	return strings.ToLower(hostname) + "|" + string(kind)
}

func (s *memStore) ServiceResourcesForHostname(_ context.Context, hostname string) ([]model.ServiceResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.ServiceResource
	for _, k := range model.KindOrder {
		if r, ok := s.rows[rowKey(hostname, k)]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *memStore) UpsertServiceResource(_ context.Context, r model.ServiceResource) (model.ServiceResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ID, r.Status = uuid.New(), model.ResourceActive
	s.rows[rowKey(r.Hostname, r.Kind)] = r
	return r, nil
}

// dnsFinding pulls the DNS line out of a result. Every kind always has one, so
// a missing line is a bug rather than a "nothing to say".
func dnsFinding(t *testing.T, res *spinup.Result) spinup.StepFinding {
	t.Helper()
	for _, f := range res.Findings {
		if f.Kind == model.ResourceDNSRecord {
			return f
		}
	}
	t.Fatalf("no DNS finding in %+v", res.Findings)
	return spinup.StepFinding{}
}

// spinupFor wires the real provisioner into an orchestrator over a fake zone.
func spinupFor(t *testing.T, z *fakeZone) (*spinup.Service, *memStore) {
	t.Helper()
	st := newMemStore()
	return spinup.New(st, spinup.NewRegistry(dnsFor(t, z))), st
}

// Argosy's shape, and the epic's first honest exercise: a service whose edge
// predates this axis entirely. Upstream is already correct and Purser holds no
// row, which is an adopt — the apply writes the row and calls nothing.
func TestSpinup_AlreadyLiveRecordIsAdopted(t *testing.T) {
	ctx := context.Background()
	z := newZone(dnsRecord{Type: "A", Name: "argosy." + testZoneName, Content: "100.64.0.7", TTL: 1})
	svc, st := spinupFor(t, z)
	spec := directTarget("100.64.0.7").Spec

	plan, err := svc.Ensure(ctx, spinup.Request{Spec: spec})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if f := dnsFinding(t, plan); f.Status != spinup.StepAdopt {
		t.Fatalf("a correct record Purser never recorded is an adopt, got %s (%s %s)", f.Status, f.Detail, f.Err)
	}
	if z.writes() != 0 {
		t.Errorf("a dry run must not write: %v", z.callLog())
	}

	applied, err := svc.Ensure(ctx, spinup.Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	f := dnsFinding(t, applied)
	if f.Status != spinup.StepAdopt || !f.Applied {
		t.Fatalf("apply should have adopted, got %s applied=%v", f.Status, f.Applied)
	}
	if z.writes() != 0 {
		t.Errorf("an adopt writes a row and touches nothing upstream, made %d write(s)", z.writes())
	}
	row, ok := st.rows[rowKey(spec.Hostname, model.ResourceDNSRecord)]
	if !ok {
		t.Fatal("the adopt should have recorded a row")
	}
	if row.ExternalID != f.ExternalID || row.ExternalID == "" {
		t.Errorf("the row must carry the live record's id so Teardown can target it, got %q", row.ExternalID)
	}
	if row.ParentID != testZoneID {
		t.Errorf("the row should record the zone it lives in, got %q", row.ParentID)
	}
}

// The other end: nothing there. The plan says create and makes no write; the
// apply creates once and records the id it got back.
func TestSpinup_NewHostnamePreviewsThenCreates(t *testing.T) {
	ctx := context.Background()
	z := newZone()
	svc, st := spinupFor(t, z)
	spec := directTarget("100.64.0.7").Spec

	plan, err := svc.Ensure(ctx, spinup.Request{Spec: spec})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if f := dnsFinding(t, plan); f.Status != spinup.StepCreate || f.Applied {
		t.Fatalf("want an unapplied create, got %s applied=%v", f.Status, f.Applied)
	}
	if z.writes() != 0 {
		t.Fatalf("the preview wrote to the zone: %v", z.callLog())
	}

	applied, err := svc.Ensure(ctx, spinup.Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	f := dnsFinding(t, applied)
	if f.Status != spinup.StepCreate || !f.Applied {
		t.Fatalf("want an applied create, got %s applied=%v (%s)", f.Status, f.Applied, f.Err)
	}
	if n := z.countCalls(http.MethodPost); n != 1 {
		t.Errorf("want exactly one create, got %d", n)
	}
	if st.rows[rowKey(spec.Hostname, model.ResourceDNSRecord)].ExternalID != f.ExternalID {
		t.Error("the recorded id should be the one the create returned")
	}

	// And the third run is a no-op: upstream matches and the row agrees.
	again, err := svc.Ensure(ctx, spinup.Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if f := dnsFinding(t, again); f.Status != spinup.StepOK {
		t.Errorf("a settled hostname should report ok, got %s", f.Status)
	}
	if n := z.countCalls(http.MethodPost); n != 1 {
		t.Errorf("re-applying created a second record (%d creates)", n)
	}
}

// An answer Purser can't read is `unknown`, and --apply does not act on it.
// Two candidate records is the realistic way to get there: acting would mean
// changing whichever one wasn't this service's.
func TestSpinup_AmbiguousNameIsUnknownAndNotActedOn(t *testing.T) {
	host := "argosy." + testZoneName
	z := newZone(
		dnsRecord{Type: "A", Name: host, Content: "10.0.0.1"},
		dnsRecord{Type: "A", Name: host, Content: "10.0.0.2"},
	)
	svc, st := spinupFor(t, z)

	res, err := svc.Ensure(context.Background(), spinup.Request{Spec: directTarget("100.64.0.7").Spec, Apply: true})
	if err != nil {
		t.Fatalf("a step that cannot be read is a finding, not a failed run: %v", err)
	}
	f := dnsFinding(t, res)
	if f.Status != spinup.StepUnknown {
		t.Fatalf("want unknown, got %s (%s)", f.Status, f.Err)
	}
	if z.writes() != 0 {
		t.Errorf("--apply must not act on an unknown step, made %d write(s)", z.writes())
	}
	if len(st.rows) != 0 {
		t.Error("nothing happened, so nothing should be recorded")
	}
}

// A tunnelled spec is held at the DNS step while the steps in front of it are
// unresolved — here because nothing provisions them in this build. Publishing
// the record anyway is the ungated/502 window KindOrder exists to prevent.
func TestSpinup_TunnelledDNSIsBlockedByItsPrerequisites(t *testing.T) {
	z := newZone()
	st := newMemStore()
	svc := spinup.New(st, spinup.NewRegistry(dnsFor(t, z)),
		spinup.WithTunnels(spinup.TunnelSet{spinup.TunnelProd: testTunnelID}))

	res, err := svc.Ensure(context.Background(), spinup.Request{Spec: tunnelledTarget().Spec, Apply: true})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if f := dnsFinding(t, res); f.Status != spinup.StepBlocked {
		t.Fatalf("want the DNS step held back, got %s (%s)", f.Status, f.Detail)
	}
	if z.writes() != 0 {
		t.Errorf("a blocked step writes nothing, made %d write(s)", z.writes())
	}
}
