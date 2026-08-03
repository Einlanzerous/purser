package invite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/model"
)

// auditFixture builds a store with one person and the given connectors
// registered + seeded.
func auditFixture(t *testing.T, conns ...connector.Connector) (*Service, *fakeStore, model.Person) {
	t.Helper()
	st := newFakeStore()
	reg := connector.NewRegistry(conns...)
	svc := New(seededStore(t, st, reg), reg, nil)
	p, _, err := st.InsertPersonIfAbsent(context.Background(), "Ada", "ada@example.com", model.PersonHuman)
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, p
}

func findingFor(res *AuditResult, key string) (AuditFinding, bool) {
	for _, f := range res.Findings {
		if f.ServiceKey == key {
			return f, true
		}
	}
	return AuditFinding{}, false
}

// The headline property: an audit must never provision. This is the whole
// reason the mode exists — a re-invite would mint a second Switchyard token.
func TestAudit_NeverProvisions(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard",
		recResult: connector.ReconcileResult{Exists: true, ExternalID: "u1", Username: "Ada"}}
	svc, _, _ := auditFixture(t, sw)

	if _, err := svc.Audit(context.Background(), AuditRequest{Apply: true}); err != nil {
		t.Fatal(err)
	}
	if sw.callCount() != 0 {
		t.Errorf("Audit must never call Provision, got %d calls", sw.callCount())
	}
	if sw.reconcileCount() == 0 {
		t.Error("Audit should have called Reconcile")
	}
}

func TestAudit_DryRunWritesNothing(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard",
		recResult: connector.ReconcileResult{Exists: true, ExternalID: "u1", Username: "Ada"}}
	svc, st, p := auditFixture(t, sw)

	res, err := svc.Audit(context.Background(), AuditRequest{Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := findingFor(res, "switchyard")
	if f.Action != ActionRecord {
		t.Errorf("want %s, got %s", ActionRecord, f.Action)
	}
	if f.Applied {
		t.Error("a dry run must not report Applied")
	}
	if _, err := st.AccountFor(context.Background(), p.ID, st.services["switchyard"].ID); err == nil {
		t.Error("a dry run must not create an account row")
	}
}

func TestAudit_ApplyRecordsUpstreamAccountWithoutSecret(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard",
		recResult: connector.ReconcileResult{Exists: true, ExternalID: "u1", Username: "Ada"}}
	svc, st, p := auditFixture(t, sw)

	res, err := svc.Audit(context.Background(), AuditRequest{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := findingFor(res, "switchyard")
	if f.Action != ActionRecord || !f.Applied {
		t.Fatalf("want an applied record action, got %s applied=%v", f.Action, f.Applied)
	}
	acct, err := st.AccountFor(context.Background(), p.ID, st.services["switchyard"].ID)
	if err != nil {
		t.Fatalf("expected an account row: %v", err)
	}
	if acct.Status != model.AccountActive || acct.ExternalID != "u1" {
		t.Errorf("unexpected recorded account: %+v", acct)
	}
	// Purser never learned their credential, so there is nothing to hash —
	// storing a hash of "" would falsely imply a Purser-issued secret.
	if acct.SecretHash != "" {
		t.Errorf("a reconciled account must carry no secret hash, got %q", acct.SecretHash)
	}
}

// The reverse-drift case: Purser says active, upstream says gone. Left active,
// the orchestrator's skip means this person can never be re-provisioned.
func TestAudit_MarksStaleWhenUpstreamIsGone(t *testing.T) {
	ly := &fakeConn{key: "lyceum", display: "Lyceum",
		recResult: connector.ReconcileResult{Exists: false}}
	svc, st, p := auditFixture(t, ly)

	svcRow := st.services["lyceum"]
	if _, err := st.UpsertAccount(context.Background(), model.Account{
		PersonID: p.ID, ServiceID: svcRow.ID, Status: model.AccountActive, Username: "Ada",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Audit(context.Background(), AuditRequest{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := findingFor(res, "lyceum")
	if f.Action != ActionMarkStale || !f.Applied {
		t.Fatalf("want an applied mark-stale, got %s applied=%v", f.Action, f.Applied)
	}
	acct, _ := st.AccountFor(context.Background(), p.ID, svcRow.ID)
	if acct.Status != model.AccountStale {
		t.Errorf("account should be stale, got %s", acct.Status)
	}
}

// An unverifiable connector must not be reported as "no" — that would claim
// people lack access they demonstrably have, which is the drift we're hunting.
func TestAudit_UnsupportedIsUnknownNotAbsent(t *testing.T) {
	ar := &fakeConn{key: "argosy", display: "Argosy",
		recErr: fmt.Errorf("%w: argosy has no lookup endpoint", connector.ErrReconcileUnsupported)}
	svc, _, _ := auditFixture(t, ar)

	res, err := svc.Audit(context.Background(), AuditRequest{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := findingFor(res, "argosy")
	if f.Upstream != UpstreamUnknown {
		t.Errorf("unverifiable must be %s, got %s", UpstreamUnknown, f.Upstream)
	}
	if f.Action != ActionSkipped {
		t.Errorf("want %s, got %s", ActionSkipped, f.Action)
	}
	if f.Applied {
		t.Error("an unverifiable service must never be written")
	}
}

// A connector that isn't configured registers as Unavailable, whose Reconcile
// returns ErrPending — also unknown, not absent.
func TestAudit_PendingConnectorIsUnknown(t *testing.T) {
	un := connector.NewUnavailable("lyceum", "Lyceum", "set PURSER_LYCEUM_OWNER_TOKEN")
	svc, _, _ := auditFixture(t, un)

	res, err := svc.Audit(context.Background(), AuditRequest{})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := findingFor(res, "lyceum")
	if f.Upstream != UpstreamUnknown || f.Action != ActionSkipped {
		t.Errorf("unconfigured connector should be unknown/unverifiable, got %s/%s", f.Upstream, f.Action)
	}
}

// A connector error must not be mistaken for absence either, or a transient
// outage would mark everyone's records stale.
func TestAudit_ConnectorErrorDoesNotMarkStale(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard",
		recErr: errors.New("switchyard: 503 upstream unavailable")}
	svc, st, p := auditFixture(t, sw)

	svcRow := st.services["switchyard"]
	if _, err := st.UpsertAccount(context.Background(), model.Account{
		PersonID: p.ID, ServiceID: svcRow.ID, Status: model.AccountActive,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Audit(context.Background(), AuditRequest{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := findingFor(res, "switchyard")
	if f.Action != ActionError {
		t.Fatalf("want %s, got %s", ActionError, f.Action)
	}
	acct, _ := st.AccountFor(context.Background(), p.ID, svcRow.ID)
	if acct.Status != model.AccountActive {
		t.Errorf("a failed check must leave the record alone, got %s", acct.Status)
	}
}

func TestAudit_RecordsInAgreementAreOK(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard",
		recResult: connector.ReconcileResult{Exists: true, ExternalID: "u1", Username: "Ada"}}
	svc, st, p := auditFixture(t, sw)
	if _, err := st.UpsertAccount(context.Background(), model.Account{
		PersonID: p.ID, ServiceID: st.services["switchyard"].ID,
		Status: model.AccountActive, ExternalID: "u1", SecretHash: "deadbeef",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Audit(context.Background(), AuditRequest{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := findingFor(res, "switchyard")
	if f.Action != ActionNone {
		t.Errorf("matching records should be %s, got %s", ActionNone, f.Action)
	}
	// Crucially, an in-agreement row must not be rewritten — that would blank
	// the secret_hash of a genuinely Purser-provisioned account.
	acct, _ := st.AccountFor(context.Background(), p.ID, st.services["switchyard"].ID)
	if acct.SecretHash != "deadbeef" {
		t.Errorf("an in-agreement account must not be rewritten, hash is now %q", acct.SecretHash)
	}
}

func TestAudit_NeitherSideHasItIsAbsent(t *testing.T) {
	ar := &fakeConn{key: "argosy", display: "Argosy",
		recResult: connector.ReconcileResult{Exists: false}}
	svc, _, _ := auditFixture(t, ar)

	res, err := svc.Audit(context.Background(), AuditRequest{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := findingFor(res, "argosy")
	if f.Action != ActionAbsent || f.Applied {
		t.Errorf("want an unapplied %s, got %s applied=%v", ActionAbsent, f.Action, f.Applied)
	}
}

func TestAudit_ScopeByEmail(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard",
		recResult: connector.ReconcileResult{Exists: false}}
	svc, st, _ := auditFixture(t, sw)
	if _, _, err := st.InsertPersonIfAbsent(context.Background(), "Bob", "bob@example.com", model.PersonHuman); err != nil {
		t.Fatal(err)
	}

	res, err := svc.Audit(context.Background(), AuditRequest{Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0].Person.Email != "ada@example.com" {
		t.Errorf("scoping by email should yield one person's findings, got %d", len(res.Findings))
	}
}

func TestAudit_UnknownEmailIsAnError(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard"}
	svc, _, _ := auditFixture(t, sw)
	_, err := svc.Audit(context.Background(), AuditRequest{Email: "nobody@example.com"})
	if err == nil || !strings.Contains(err.Error(), "nobody@example.com") {
		t.Fatalf("want an error naming the missing person, got %v", err)
	}
}

func TestAudit_ScopeByService(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard",
		recResult: connector.ReconcileResult{Exists: true}}
	ar := &fakeConn{key: "argosy", display: "Argosy",
		recResult: connector.ReconcileResult{Exists: true}}
	svc, _, _ := auditFixture(t, sw, ar)

	res, err := svc.Audit(context.Background(), AuditRequest{Services: []string{"argosy"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || res.Findings[0].ServiceKey != "argosy" {
		t.Errorf("--to should scope the audit, got %+v", res.Findings)
	}
	if sw.reconcileCount() != 0 {
		t.Error("an out-of-scope connector must not be called")
	}
}
