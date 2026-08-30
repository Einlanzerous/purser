package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

// The orchestrator's Store interface says *store.Store satisfies it. Nothing
// wires the two together yet — the provisioners and the CLI are still to come —
// so without this the claim is a comment, and the first thing to try wiring them
// would be the first thing to find out it wasn't true.
//
// It lives here rather than in spinup because the dependency only runs one way:
// spinup imports internal/model and nothing else of ours, deliberately, so that
// the provisioner packages don't pull the store in behind it. A test-only import
// in this direction costs nothing and cannot become a cycle.
var _ spinup.Store = (*Store)(nil)

// resourceStore is testStore plus the spin-up table, which the person-axis
// truncate deliberately doesn't touch — the two axes share nothing.
func resourceStore(t *testing.T) *Store {
	t.Helper()
	st := testStore(t)
	if _, err := st.pool.Exec(context.Background(),
		`TRUNCATE service_resource RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return st
}

func dnsRow(hostname string) model.ServiceResource {
	return model.ServiceResource{
		ServiceKey: "argosy",
		Hostname:   hostname,
		Kind:       model.ResourceDNSRecord,
		ExternalID: "rec-1",
		ParentID:   "zone-1",
	}
}

// (hostname, kind) is this axis's idempotency key, so a second run over the same
// hostname must reuse the row rather than record the resource twice.
func TestUpsertServiceResource_IsIdempotentPerHostnameAndKind(t *testing.T) {
	st := resourceStore(t)
	ctx := context.Background()

	first, err := st.UpsertServiceResource(ctx, dnsRow("argosy.zerogravity.industries"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.UpsertServiceResource(ctx, dnsRow("argosy.zerogravity.industries"))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Errorf("a re-run recorded a second row (%s vs %s)", first.ID, second.ID)
	}

	rows, err := st.ServiceResourcesForHostname(ctx, "argosy.zerogravity.industries")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows for one resource, want 1", len(rows))
	}
}

// Hostnames are case-insensitive, and the unique index folds case to match. An
// index that didn't would let the same hostname be recorded twice and torn down
// once — the shape of the bug migration 0003 fixed on person.email, which can
// only be caught against a real index.
func TestUpsertServiceResource_HostnameIsCaseInsensitive(t *testing.T) {
	st := resourceStore(t)
	ctx := context.Background()

	if _, err := st.UpsertServiceResource(ctx, dnsRow("argosy.zerogravity.industries")); err != nil {
		t.Fatal(err)
	}
	mixed := dnsRow("Argosy.ZeroGravity.Industries")
	mixed.ExternalID = "rec-2"
	if _, err := st.UpsertServiceResource(ctx, mixed); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ServiceResourcesForHostname(ctx, "ARGOSY.zerogravity.industries")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows for one hostname in two cases, want 1", len(rows))
	}
	if rows[0].ExternalID != "rec-2" {
		t.Errorf("external_id = %q, want the upsert to have updated the existing row", rows[0].ExternalID)
	}
}

// A different kind on the same hostname is a different resource: one hostname
// legitimately holds a DNS record, an Access app and an ingress route.
func TestUpsertServiceResource_KindsAreSeparateRows(t *testing.T) {
	st := resourceStore(t)
	ctx := context.Background()
	host := "interlock.zerogravity.industries"

	for _, kind := range model.KindOrder {
		r := dnsRow(host)
		r.ServiceKey, r.Kind = "interlock", kind
		if _, err := st.UpsertServiceResource(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := st.ServiceResourcesForHostname(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(model.KindOrder) {
		t.Fatalf("got %d rows, want one per kind", len(rows))
	}
	// Ordered the way a spin-up applies them, so a report reads in step order
	// rather than alphabetically.
	for i, want := range model.KindOrder {
		if rows[i].Kind != want {
			t.Errorf("row %d is %q, want %q — rows must come back in KindOrder", i, rows[i].Kind, want)
		}
	}
}

// removed_at is stamped on the transition and never moved again, and standing
// the hostname back up must not erase it. account.deprovisioned_at (migration
// 0006) exists because status + updated_at lost exactly this to a re-invite.
func TestMarkServiceResourceRemoved_StampsOnceAndSurvivesReuse(t *testing.T) {
	st := resourceStore(t)
	ctx := context.Background()

	row, err := st.UpsertServiceResource(ctx, dnsRow("argosy.zerogravity.industries"))
	if err != nil {
		t.Fatal(err)
	}
	if row.RemovedAt != nil {
		t.Error("a freshly recorded resource is not removed")
	}

	if err := st.MarkServiceResourceRemoved(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ServiceResourcesForHostname(ctx, "argosy.zerogravity.industries")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != model.ResourceRemoved || rows[0].RemovedAt == nil {
		t.Fatalf("after removal: status=%q removed_at=%v", rows[0].Status, rows[0].RemovedAt)
	}
	stamped := *rows[0].RemovedAt

	// Re-running a teardown must not move the date.
	if err := st.MarkServiceResourceRemoved(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.ServiceResourcesForHostname(ctx, "argosy.zerogravity.industries")
	if !rows[0].RemovedAt.Equal(stamped) {
		t.Errorf("a second teardown moved removed_at from %v to %v", stamped, *rows[0].RemovedAt)
	}

	// Standing it back up makes it active again and keeps the history.
	back, err := st.UpsertServiceResource(ctx, dnsRow("argosy.zerogravity.industries"))
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != row.ID {
		t.Error("standing the hostname back up recorded a new row instead of reusing the slot")
	}
	if back.Status != model.ResourceActive {
		t.Errorf("status = %q, want active", back.Status)
	}
	if back.RemovedAt == nil || !back.RemovedAt.Equal(stamped) {
		t.Errorf("removed_at = %v, want the original stamp kept — it did happen", back.RemovedAt)
	}
}

func TestMarkServiceResourceRemoved_UnknownID(t *testing.T) {
	st := resourceStore(t)
	if err := st.MarkServiceResourceRemoved(context.Background(), uuid.New()); err == nil {
		t.Error("removing a row that isn't there must not report success")
	}
}

// "What does this service hold at the edge?" spans hostnames: a dev and a prod
// instance are separate hostnames under one service key (PRSR-33).
func TestServiceResourcesFor_SpansHostnames(t *testing.T) {
	st := resourceStore(t)
	ctx := context.Background()

	for _, host := range []string{"interlock.zerogravity.industries", "interlock-dev.zerogravity.industries"} {
		r := dnsRow(host)
		r.ServiceKey = "interlock"
		if _, err := st.UpsertServiceResource(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	other := dnsRow("argosy.zerogravity.industries")
	if _, err := st.UpsertServiceResource(ctx, other); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ServiceResourcesFor(ctx, "interlock")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows for interlock, want both hostnames", len(rows))
	}
	for _, r := range rows {
		if r.ServiceKey != "interlock" {
			t.Errorf("leaked a row for %q", r.ServiceKey)
		}
	}
}

// The spin-up axis must not need a `service` row: the services it stands up have
// no connector, and requiring one would make "can Purser invite someone into it"
// a precondition for "can Purser deploy it".
func TestUpsertServiceResource_NeedsNoServiceRow(t *testing.T) {
	st := resourceStore(t)
	r := dnsRow("centrifuge.zerogravity.industries")
	r.ServiceKey = "centrifuge" // no connector, no service row, no seeding
	if _, err := st.UpsertServiceResource(context.Background(), r); err != nil {
		t.Fatalf("recording a resource for a service with no connector failed: %v", err)
	}
}

func TestUpsertServiceResource_RequiresHostname(t *testing.T) {
	st := resourceStore(t)
	r := dnsRow("")
	if _, err := st.UpsertServiceResource(context.Background(), r); err == nil {
		t.Error("without a hostname there is no conflict target, so every run would insert a new row")
	}
}

// --- the teardown walk, over the real table (PRSR-34) -----------------------

// tearProv is a provisioner whose removals succeed and which counts them. It
// exists so the *store* half of a teardown is exercised against real SQL: the
// orchestrator's own logic has its own tests with a fake, and what those cannot
// show is that MarkServiceResourceRemoved does what Teardown's contract needs.
//
// PRSR-27 shipped that method with the note "untested against a real
// orchestrator because there isn't one". There is now.
type tearProv struct {
	kind      model.ResourceKind
	teardowns int
	err       error
}

func (p *tearProv) Kind() model.ResourceKind { return p.kind }
func (p *tearProv) DisplayName() string      { return string(p.kind) }
func (p *tearProv) Inspect(context.Context, spinup.Target) (spinup.State, error) {
	return spinup.State{}, nil
}
func (p *tearProv) Ensure(context.Context, spinup.Target) (spinup.Resource, error) {
	return spinup.Resource{}, nil
}
func (p *tearProv) Teardown(context.Context, spinup.Target, model.ServiceResource) (spinup.Removal, error) {
	p.teardowns++
	return spinup.Removal{Detail: "removed"}, p.err
}

func TestTeardownWalk_MarksRowsRemovedOverRealSQL(t *testing.T) {
	st := resourceStore(t)
	ctx := context.Background()
	const host = "argosy.zerogravity.industries"
	for _, k := range model.KindOrder {
		if _, err := st.UpsertServiceResource(ctx, model.ServiceResource{
			ServiceKey: "argosy", Hostname: host, Kind: k, ExternalID: "id-" + string(k),
		}); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	svc := spinup.New(st, spinup.NewRegistry(
		&tearProv{kind: model.ResourceDNSRecord},
		&tearProv{kind: model.ResourceAccessApp},
		&tearProv{kind: model.ResourceTunnelRoute},
	))

	res, err := svc.Teardown(ctx, spinup.TeardownRequest{ServiceKey: "argosy", Hostname: host, Apply: true})
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if res.Changed() != len(model.KindOrder) {
		t.Fatalf("changed=%d, want %d (%v)", res.Changed(), len(model.KindOrder), res.NeedsAttention())
	}

	rows, err := st.ServiceResourcesForHostname(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(model.KindOrder) {
		t.Fatalf("%d rows, want %d — a torn-down row is marked, not deleted", len(rows), len(model.KindOrder))
	}
	for _, r := range rows {
		if r.Status != model.ResourceRemoved {
			t.Errorf("%s is %q, want %q", r.Kind, r.Status, model.ResourceRemoved)
		}
		if r.RemovedAt == nil {
			t.Errorf("%s has no removed_at; the column is what makes 'when was it taken away' durable", r.Kind)
		}
	}

	// And a re-run reads them back as `gone` rather than as never-recorded,
	// which is the distinction the two statuses exist for.
	again, err := svc.Teardown(ctx, spinup.TeardownRequest{ServiceKey: "argosy", Hostname: host, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range again.Findings {
		if f.Status != spinup.TeardownGone {
			t.Errorf("%s re-runs as %s, want %s", f.Kind, f.Status, spinup.TeardownGone)
		}
	}
}

// A removal that didn't happen must never be recorded as one — PRSR-17's
// invariant, over the real UPDATE this time. The lie outlives the error message,
// and this column is what the next run reads.
func TestTeardownWalk_AFailedRemovalLeavesTheRowActive(t *testing.T) {
	st := resourceStore(t)
	ctx := context.Background()
	const host = "argosy.zerogravity.industries"
	if _, err := st.UpsertServiceResource(ctx, dnsRow(host)); err != nil {
		t.Fatal(err)
	}
	svc := spinup.New(st, spinup.NewRegistry(
		&tearProv{kind: model.ResourceDNSRecord, err: errors.New("cloudflare: 500")},
	))

	res, err := svc.Teardown(ctx, spinup.TeardownRequest{ServiceKey: "argosy", Hostname: host, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed() != 0 {
		t.Errorf("changed=%d after a failed removal", res.Changed())
	}
	rows, err := st.ServiceResourcesForHostname(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != model.ResourceActive || rows[0].RemovedAt != nil {
		t.Errorf("row = %+v; a failed removal must leave it active so the next run retries", rows[0])
	}
}
