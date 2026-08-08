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

// deprovConn counts Deprovision calls and records what it was given, so tests can
// assert both that a dry run never calls it and that a real one targets the
// recorded upstream id.
type deprovConn struct {
	fakeConn
	deprovCalls int
	deprovIn    connector.Input
	deprovErr   error
}

func (d *deprovConn) Deprovision(_ context.Context, in connector.Input) error {
	d.deprovCalls++
	d.deprovIn = in
	return d.deprovErr
}

func offboardFixture(t *testing.T, conns ...connector.Connector) (*Service, *fakeStore, model.Person) {
	t.Helper()
	st := newFakeStore()
	reg := connector.NewRegistry(conns...)
	svc := New(seededStore(t, st, reg), reg, nil)
	p := addPerson(t, st, "Ada", "ada@example.com", model.PersonHuman)
	return svc, st, p
}

func offboardFindingFor(res *OffboardResult, key string) (OffboardFinding, bool) {
	for _, f := range res.Findings {
		if f.ServiceKey == key {
			return f, true
		}
	}
	return OffboardFinding{}, false
}

// The headline property, and the reason the default is a preview: a dry run must
// not touch the connector at all. `invite` acts by default because a duplicate
// grant is merely wasteful; revoking the wrong person is not undone by a re-run.
func TestOffboard_DryRunRevokesNothing(t *testing.T) {
	sw := &deprovConn{fakeConn: fakeConn{key: "switchyard", display: "Switchyard"}}
	svc, st, p := offboardFixture(t, sw)
	addAccount(t, st, p, "switchyard", model.AccountActive)

	res, err := svc.Offboard(context.Background(), OffboardRequest{Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if sw.deprovCalls != 0 {
		t.Errorf("a preview must not call Deprovision, got %d calls", sw.deprovCalls)
	}
	f, ok := offboardFindingFor(res, "switchyard")
	if !ok || f.Action != ActionRevoke {
		t.Fatalf("want a revoke finding, got %+v", res.Findings)
	}
	if f.Applied {
		t.Error("nothing may be marked applied on a dry run")
	}
	// And the record is untouched, which is what makes the preview safe.
	if got := st.accountStatus(p.ID, st.services["switchyard"].ID); got != model.AccountActive {
		t.Errorf("account status = %s, want it left active", got)
	}
}

// Apply revokes, marks the record deprovisioned, and — the part worth pinning —
// hands the connector the id Purser recorded rather than making it look the
// person up again.
func TestOffboard_ApplyRevokesAndRecords(t *testing.T) {
	sw := &deprovConn{fakeConn: fakeConn{key: "switchyard", display: "Switchyard"}}
	svc, st, p := offboardFixture(t, sw)
	addAccount(t, st, p, "switchyard", model.AccountActive)

	res, err := svc.Offboard(context.Background(), OffboardRequest{Email: "ada@example.com", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if sw.deprovCalls != 1 {
		t.Fatalf("want 1 Deprovision call, got %d", sw.deprovCalls)
	}
	if sw.deprovIn.ExternalID != "u-switchyard" {
		t.Errorf("Deprovision should target the recorded external id, got %q", sw.deprovIn.ExternalID)
	}
	if sw.deprovIn.Email != "ada@example.com" {
		t.Errorf("Deprovision should carry the email too, got %q", sw.deprovIn.Email)
	}
	f, _ := offboardFindingFor(res, "switchyard")
	if !f.Applied {
		t.Error("a successful revoke should be marked applied")
	}
	// deprovisioned, not deleted: the record of what they held survives, and the
	// status stops being one nothing can produce.
	if got := st.accountStatus(p.ID, st.services["switchyard"].ID); got != model.AccountDeprovisioned {
		t.Errorf("account status = %s, want %s", got, model.AccountDeprovisioned)
	}
}

// A connector that failed must not leave a record saying access was removed.
// That lie outlives the error message and is read as truth by the audit, by
// `person show`, and by the next invite's idempotency skip.
func TestOffboard_FailedRevokeLeavesTheRecordActive(t *testing.T) {
	sw := &deprovConn{
		fakeConn:  fakeConn{key: "switchyard", display: "Switchyard"},
		deprovErr: errors.New("switchyard: 500 Internal Server Error"),
	}
	svc, st, p := offboardFixture(t, sw)
	addAccount(t, st, p, "switchyard", model.AccountActive)

	res, err := svc.Offboard(context.Background(), OffboardRequest{Email: "ada@example.com", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := offboardFindingFor(res, "switchyard")
	if f.Action != ActionFailed {
		t.Errorf("action = %s, want %s", f.Action, ActionFailed)
	}
	if f.Applied {
		t.Error("a failed revoke must never be marked applied")
	}
	if got := st.accountStatus(p.ID, st.services["switchyard"].ID); got != model.AccountActive {
		t.Errorf("account status = %s — a failed revoke must leave it active so a re-run retries it", got)
	}
}

// ErrPending is not failure, and on this path it is not success either: the
// person still has access and a human has to go take it away.
func TestOffboard_UnavailableIsItsOwnAnswer(t *testing.T) {
	ar := &deprovConn{
		fakeConn:  fakeConn{key: "argosy", display: "Argosy"},
		deprovErr: fmt.Errorf("%w: argosy has no delete endpoint", connector.ErrPending),
	}
	svc, st, p := offboardFixture(t, ar)
	addAccount(t, st, p, "argosy", model.AccountActive)

	res, err := svc.Offboard(context.Background(), OffboardRequest{Email: "ada@example.com", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := offboardFindingFor(res, "argosy")
	if f.Action != ActionUnavailable {
		t.Errorf("action = %s, want %s", f.Action, ActionUnavailable)
	}
	if f.Applied {
		t.Error("nothing was revoked, so nothing may be marked applied")
	}
	// The record must stay active: claiming otherwise would tell the audit the
	// access is gone while it demonstrably is not.
	if got := st.accountStatus(p.ID, st.services["argosy"].ID); got != model.AccountActive {
		t.Errorf("account status = %s, want it left active", got)
	}
	// And the operator is told, in the "still has access" section rather than the
	// failure one.
	note := RenderOffboardNote(res)
	if !strings.Contains(note, "Still has access") || !strings.Contains(note, "Argosy") {
		t.Errorf("the note should name what is still open:\n%s", note)
	}
	if strings.Contains(note, "Failed") {
		t.Errorf("an unavailable connector didn't fail:\n%s", note)
	}
}

// Only an active account is access. A stale row means upstream already lost it
// and a deprovisioned one means this already ran; calling the connector for
// either is a mutation that cannot change anything.
func TestOffboard_SkipsAccountsThatAreNotActive(t *testing.T) {
	sw := &deprovConn{fakeConn: fakeConn{key: "switchyard", display: "Switchyard"}}
	ly := &deprovConn{fakeConn: fakeConn{key: "lyceum", display: "Lyceum"}}
	svc, st, p := offboardFixture(t, sw, ly)
	addAccount(t, st, p, "switchyard", model.AccountStale)
	addAccount(t, st, p, "lyceum", model.AccountDeprovisioned)

	res, err := svc.Offboard(context.Background(), OffboardRequest{Email: "ada@example.com", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if sw.deprovCalls != 0 || ly.deprovCalls != 0 {
		t.Errorf("neither should be called: switchyard=%d lyceum=%d", sw.deprovCalls, ly.deprovCalls)
	}
	for _, f := range res.Findings {
		if f.Action != ActionNothingToDo {
			t.Errorf("%s: action = %s, want %s", f.ServiceKey, f.Action, ActionNothingToDo)
		}
	}
}

// Partial offboarding is the point of --to, and a service that was named but is
// not held still gets a line — silence would read as "revoked".
func TestOffboard_ServiceFilterAndUnheldServices(t *testing.T) {
	sw := &deprovConn{fakeConn: fakeConn{key: "switchyard", display: "Switchyard"}}
	ly := &deprovConn{fakeConn: fakeConn{key: "lyceum", display: "Lyceum"}}
	svc, st, p := offboardFixture(t, sw, ly)
	addAccount(t, st, p, "switchyard", model.AccountActive)

	res, err := svc.Offboard(context.Background(), OffboardRequest{
		Email: "ada@example.com", Services: []string{"lyceum"}, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sw.deprovCalls != 0 {
		t.Error("switchyard was not in scope and must not be touched")
	}
	if ly.deprovCalls != 0 {
		t.Error("lyceum was never held; there is nothing to revoke")
	}
	f, ok := offboardFindingFor(res, "lyceum")
	if !ok {
		t.Fatalf("a named-but-unheld service still needs a line, got %+v", res.Findings)
	}
	if f.Action != ActionNothingToDo {
		t.Errorf("action = %s, want %s", f.Action, ActionNothingToDo)
	}
	if _, ok := offboardFindingFor(res, "switchyard"); ok {
		t.Error("an out-of-scope service should not appear at all")
	}
}

// There is no bulk mode: the address is required, and an unknown one is the same
// refusal `person show` gives.
func TestOffboard_RequiresAKnownPerson(t *testing.T) {
	sw := &deprovConn{fakeConn: fakeConn{key: "switchyard", display: "Switchyard"}}
	svc, _, _ := offboardFixture(t, sw)

	if _, err := svc.Offboard(context.Background(), OffboardRequest{}); err == nil {
		t.Error("an empty address must be refused, not treated as everyone")
	}
	_, err := svc.Offboard(context.Background(), OffboardRequest{Email: "nobody@example.com"})
	if !errors.Is(err, ErrPersonNotFound) {
		t.Errorf("err = %v, want ErrPersonNotFound", err)
	}
	if _, err := svc.Offboard(context.Background(), OffboardRequest{
		Email: "ada@example.com", Services: []string{"swtichyard"},
	}); !errors.Is(err, ErrUnknownService) {
		t.Errorf("a typo'd service must be refused, got %v", err)
	}
	if sw.deprovCalls != 0 {
		t.Error("a refused request must not have revoked anything")
	}
}

// Re-running is safe and reports honestly: the second pass finds the record
// already deprovisioned and calls nobody.
func TestOffboard_IsIdempotent(t *testing.T) {
	sw := &deprovConn{fakeConn: fakeConn{key: "switchyard", display: "Switchyard"}}
	svc, st, p := offboardFixture(t, sw)
	addAccount(t, st, p, "switchyard", model.AccountActive)

	req := OffboardRequest{Email: "ada@example.com", Apply: true}
	if _, err := svc.Offboard(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Offboard(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if sw.deprovCalls != 1 {
		t.Errorf("the second run should call nobody, got %d total calls", sw.deprovCalls)
	}
	f, _ := offboardFindingFor(res, "switchyard")
	if f.Action != ActionNothingToDo {
		t.Errorf("action = %s, want %s", f.Action, ActionNothingToDo)
	}
}
