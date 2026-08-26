package cloudflare

import (
	"context"
	"testing"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

// The epic's stated first exercise, and the reason PRSR-31 names Argosy rather
// than a greenfield hostname: run the whole axis against a service that is
// *already up*.
//
// "A spin-up tool that cannot recognise an existing deployment is one nobody
// will point at production." Argosy's edge predates this axis entirely — a
// static A record and a launcher tile somebody made in the dashboard — so the
// honest test is that all three steps report no-ops and nothing upstream is
// touched. That exercises the reconcile paths, which is where the mistakes are,
// rather than the happy path.
//
// It runs the three real provisioners through the real orchestrator. Each points
// at its own fake, which the live API would serve from one host; that difference
// is invisible to everything being asserted here, and keeping the fakes separate
// keeps each one the shape its own tests already pin.

// argosySpec is the pilot: direct path, static endpoint, bookmark tile, no
// tunnel. The Access application's name defaults to the service key.
func argosySpec() spinup.ServiceSpec {
	return spinup.ServiceSpec{
		Key:      "argosy",
		Hostname: "argosy." + testZoneName,
		Mode:     spinup.ModeDirect,
		Upstream: "100.64.0.7",
		Access:   spinup.AccessBookmark,
	}
}

// liveBookmark is the tile as the dashboard left it: right type, right name,
// right domain, visible, no logo.
func liveBookmark() map[string]any {
	return map[string]any{
		"id":     "app-argosy",
		"type":   "bookmark",
		"name":   "argosy",
		"domain": "argosy." + testZoneName,
	}
}

// argosyEdge wires the three real provisioners over fakes and returns the
// orchestrator, the store, and the two fakes a test asserts writes against.
func argosyEdge(t *testing.T, z *fakeZone, apps *accessAPI) (*spinup.Service, *memStore) {
	t.Helper()
	access := newAccessWithBase(t, apps.server(t).URL, AccessConfig{
		APIToken:  "cf_token",
		AccountID: "acct123",
		GroupID:   "group123",
		GroupName: "zerogravity-members",
	})
	// The tunnel provisioner is registered even though a direct spec never calls
	// for it. That is the point: the step is reported as `skipped` rather than
	// omitted, so silence about the tunnel can never be read as "the tunnel is
	// fine". Its fake is deliberately absent — a direct spec must not reach it,
	// and an unreachable base URL is how this test would notice if it did.
	tunnel := newTunnelWithBase(t, "http://127.0.0.1:1", TunnelConfig{
		APIToken: "cf_token", AccountID: "acct123",
	})
	st := newMemStore()
	svc := spinup.New(st, spinup.NewRegistry(dnsFor(t, z), access, tunnel))
	return svc, st
}

// findingFor pulls one kind's line out of a result. Every kind always has one,
// so a missing line is a bug rather than nothing to say.
func findingFor(t *testing.T, res *spinup.Result, kind model.ResourceKind) spinup.StepFinding {
	t.Helper()
	for _, f := range res.Findings {
		if f.Kind == kind {
			return f
		}
	}
	t.Fatalf("no %s finding in %+v", kind, res.Findings)
	return spinup.StepFinding{}
}

// The headline: Argosy as it stands. The plan adopts what is there and writes
// nothing; the apply records the rows and still calls nothing upstream.
func TestArgosy_AlreadyUpIsAdoptedWithoutTouchingTheEdge(t *testing.T) {
	ctx := context.Background()
	z := newZone(dnsRecord{Type: "A", Name: "argosy." + testZoneName, Content: "100.64.0.7", TTL: 1})
	apps := &accessAPI{apps: []map[string]any{liveBookmark()}}
	svc, st := argosyEdge(t, z, apps)
	spec := argosySpec()

	plan, err := svc.Ensure(ctx, spinup.Request{Spec: spec})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// Upstream is correct and Purser holds no row, so both real steps are
	// adopts: the fix is a row, not an API call.
	for _, kind := range []model.ResourceKind{model.ResourceDNSRecord, model.ResourceAccessApp} {
		if f := findingFor(t, plan, kind); f.Status != spinup.StepAdopt {
			t.Errorf("%s: want adopt for an already-correct resource, got %s (detail=%q err=%q)",
				kind, f.Status, f.Detail, f.Err)
		}
	}
	if f := findingFor(t, plan, model.ResourceTunnelRoute); f.Status != spinup.StepSkipped {
		t.Errorf("a direct spec has no ingress route, want skipped, got %s (%s)", f.Status, f.Err)
	}
	if z.writes() != 0 {
		t.Errorf("a plan must not write to the zone: %v", z.callLog())
	}
	if apps.lastMethod != "" && apps.lastMethod != "GET" {
		t.Errorf("a plan must only read the Access API, last call was %s %s", apps.lastMethod, apps.lastPath)
	}

	applied, err := svc.Ensure(ctx, spinup.Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, kind := range []model.ResourceKind{model.ResourceDNSRecord, model.ResourceAccessApp} {
		f := findingFor(t, applied, kind)
		if f.Status != spinup.StepAdopt || !f.Applied {
			t.Errorf("%s: want an applied adopt, got %s applied=%v err=%q", kind, f.Status, f.Applied, f.Err)
		}
		row, ok := st.rows[rowKey(spec.Hostname, kind)]
		if !ok {
			t.Fatalf("%s: the adopt should have recorded a row", kind)
		}
		if row.ExternalID == "" || row.ExternalID != f.ExternalID {
			t.Errorf("%s: the row must carry the live id so a teardown can target it, got %q want %q",
				kind, row.ExternalID, f.ExternalID)
		}
		if row.ServiceKey != "argosy" {
			t.Errorf("%s: the row should be attributed to the service, got %q", kind, row.ServiceKey)
		}
	}
	// The whole claim of this test: an adopt is a row and nothing else.
	if z.writes() != 0 {
		t.Errorf("an adopt must touch nothing upstream, the zone took %d write(s): %v", z.writes(), z.callLog())
	}
	if apps.lastMethod != "GET" {
		t.Errorf("an adopt must touch nothing upstream, the Access API took a %s %s", apps.lastMethod, apps.lastPath)
	}
	if len(apps.deleted) != 0 {
		t.Errorf("nothing should have been deleted, got %v", apps.deleted)
	}
}

// Run it twice. Once the rows exist, the same spec is `ok` rather than `adopt` —
// and `Pending` drops to zero, which is what makes "nothing to do" reportable
// as distinct from "re-run with --apply".
func TestArgosy_SecondRunIsAllOKAndPendingIsZero(t *testing.T) {
	ctx := context.Background()
	z := newZone(dnsRecord{Type: "A", Name: "argosy." + testZoneName, Content: "100.64.0.7", TTL: 1})
	apps := &accessAPI{apps: []map[string]any{liveBookmark()}}
	svc, _ := argosyEdge(t, z, apps)
	spec := argosySpec()

	if _, err := svc.Ensure(ctx, spinup.Request{Spec: spec, Apply: true}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	writesAfterFirst := z.writes()

	again, err := svc.Ensure(ctx, spinup.Request{Spec: spec})
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	for _, kind := range []model.ResourceKind{model.ResourceDNSRecord, model.ResourceAccessApp} {
		if f := findingFor(t, again, kind); f.Status != spinup.StepOK {
			t.Errorf("%s: a recorded, correct resource is ok, got %s (%s)", kind, f.Status, f.Err)
		}
	}
	if again.Pending() != 0 {
		t.Errorf("nothing is outstanding on a service that is already up, Pending()=%d", again.Pending())
	}
	if again.Changed() != 0 {
		t.Errorf("a plan changes nothing, Changed()=%d", again.Changed())
	}
	if z.writes() != writesAfterFirst {
		t.Errorf("re-running wrote to the zone: %v", z.callLog())
	}
}

// The DNS step is last so a hostname is never published in front of a gate that
// did not land — but a *bookmark* is deliberately not a prerequisite, because
// its absence costs an icon and not a gate. Argosy is the case that proves it:
// with the Access API refusing every call, the record still publishes.
func TestArgosy_ABrokenBookmarkDoesNotHoldBackDNS(t *testing.T) {
	ctx := context.Background()
	z := newZone()
	apps := &accessAPI{listStatus: 500}
	svc, _ := argosyEdge(t, z, apps)

	plan, err := svc.Ensure(ctx, spinup.Request{Spec: argosySpec()})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if f := findingFor(t, plan, model.ResourceAccessApp); f.Status != spinup.StepUnknown {
		t.Fatalf("a failed read is unknown, never absent, got %s", f.Status)
	}
	f := findingFor(t, plan, model.ResourceDNSRecord)
	if f.Status != spinup.StepCreate {
		t.Errorf("a bookmark is not a gate, so DNS is not held behind it — want create, got %s (%s)", f.Status, f.Detail)
	}
}

// The other half of that, and the one this axis must never get wrong: a *gated*
// service whose Access step could not be read does not get its hostname
// published. The window that closes is a service meant to be gated answering
// ungated, which is self-concealing in a way a 502 is not.
func TestArgosy_AGatedServiceHoldsDNSWhenAccessIsUnreadable(t *testing.T) {
	ctx := context.Background()
	z := newZone()
	apps := &accessAPI{listStatus: 500}
	svc, _ := argosyEdge(t, z, apps)

	spec := argosySpec()
	spec.Access = spinup.AccessGated

	plan, err := svc.Ensure(ctx, spinup.Request{Spec: spec})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if f := findingFor(t, plan, model.ResourceDNSRecord); f.Status != spinup.StepBlocked {
		t.Fatalf("a gated service must not publish in front of an unreadable Access step, got %s (%s)", f.Status, f.Detail)
	}
	// And an apply on the same plan writes nothing, because blocked is decided
	// before the write rather than reported after one.
	applied, err := svc.Ensure(ctx, spinup.Request{Spec: spec, Apply: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if f := findingFor(t, applied, model.ResourceDNSRecord); f.Status != spinup.StepBlocked {
		t.Errorf("--apply must respect the block, got %s", f.Status)
	}
	if z.writes() != 0 {
		t.Errorf("a blocked DNS step must not write: %v", z.callLog())
	}
}
