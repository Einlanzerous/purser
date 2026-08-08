package invite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/model"
)

// rosterFixture builds a store with the given connectors seeded as services.
func rosterFixture(t *testing.T, conns ...connector.Connector) (*Service, *fakeStore) {
	t.Helper()
	st := newFakeStore()
	reg := connector.NewRegistry(conns...)
	return New(seededStore(t, st, reg), reg, nil), st
}

// addPerson records someone directly in the fake, the way `person add` does.
func addPerson(t *testing.T, st *fakeStore, name, email string, typ model.PersonType) model.Person {
	t.Helper()
	p, created, err := st.InsertPersonIfAbsent(context.Background(), name, email, typ)
	if err != nil || !created {
		t.Fatalf("seed person %s: created=%v err=%v", email, created, err)
	}
	return p
}

// addAccount records access to a service at a given status.
func addAccount(t *testing.T, st *fakeStore, p model.Person, serviceKey string, status model.AccountStatus) {
	t.Helper()
	svc, ok := st.services[serviceKey]
	if !ok {
		t.Fatalf("service %q is not seeded", serviceKey)
	}
	if _, err := st.UpsertAccount(context.Background(), model.Account{
		PersonID: p.ID, ServiceID: svc.ID, Username: p.Name, ExternalID: "u-" + serviceKey,
		// A hash is written here precisely because nothing downstream may show
		// it: store.AccountRecord has no field to carry it into the roster.
		SecretHash: "5eec2e7", Status: status,
	}); err != nil {
		t.Fatal(err)
	}
}

func entryFor(res *RosterResult, email string) (RosterEntry, bool) {
	for _, e := range res.Entries {
		if e.Person.Email == email {
			return e, true
		}
	}
	return RosterEntry{}, false
}

// The headline property, and the reason this isn't `audit`: the roster reads
// records only. Asking who is on it must not require every upstream service to
// be reachable, and must not cost a reconcile sweep across all of them.
func TestRoster_CallsNoConnector(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard"}
	svc, st := rosterFixture(t, sw)
	p := addPerson(t, st, "Ada", "ada@example.com", model.PersonHuman)
	addAccount(t, st, p, "switchyard", model.AccountActive)

	if _, err := svc.Roster(context.Background(), RosterRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PersonDetail(context.Background(), "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if sw.callCount() != 0 {
		t.Errorf("the roster must never provision, got %d Provision calls", sw.callCount())
	}
	if sw.reconcileCount() != 0 {
		t.Errorf("the roster must not reach upstream at all, got %d Reconcile calls", sw.reconcileCount())
	}
}

// A person with no accounts is still on the roster. `person add` writes exactly
// that row, so an account-driven listing would be blind to the people it exists
// to record.
func TestRoster_IncludesPeopleWithNoAccounts(t *testing.T) {
	svc, st := rosterFixture(t, &fakeConn{key: "switchyard", display: "Switchyard"})
	addPerson(t, st, "Ada", "ada@example.com", model.PersonHuman)

	res, err := svc.Roster(context.Background(), RosterRequest{})
	if err != nil {
		t.Fatal(err)
	}
	e, ok := entryFor(res, "ada@example.com")
	if !ok {
		t.Fatalf("a person with no accounts is missing from the roster: %+v", res.Entries)
	}
	if len(e.Accounts) != 0 {
		t.Errorf("want no accounts, got %+v", e.Accounts)
	}
}

// Active-only by default — a stale row is what someone does *not* have — but
// never silently: Hidden is what the CLI turns into "pass --all".
func TestRoster_HidesNonActiveAccountsButSaysSo(t *testing.T) {
	svc, st := rosterFixture(t,
		&fakeConn{key: "switchyard", display: "Switchyard"},
		&fakeConn{key: "lyceum", display: "Lyceum"})
	p := addPerson(t, st, "Ada", "ada@example.com", model.PersonHuman)
	addAccount(t, st, p, "switchyard", model.AccountActive)
	addAccount(t, st, p, "lyceum", model.AccountStale)

	res, err := svc.Roster(context.Background(), RosterRequest{})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := entryFor(res, "ada@example.com")
	if got := keysOf(e); len(got) != 1 || got[0] != "switchyard" {
		t.Errorf("default listing should show active accounts only, got %v", got)
	}
	if res.Hidden != 1 {
		t.Errorf("Hidden = %d, want 1 — the default filter must not be silent", res.Hidden)
	}

	all, err := svc.Roster(context.Background(), RosterRequest{IncludeInactive: true})
	if err != nil {
		t.Fatal(err)
	}
	e, _ = entryFor(all, "ada@example.com")
	if got := keysOf(e); len(got) != 2 {
		t.Errorf("--all should include the stale row, got %v", got)
	}
	if all.Hidden != 0 {
		t.Errorf("nothing is hidden under --all, got %d", all.Hidden)
	}
}

// --to selects on the same accounts the output shows. If it matched stale rows
// while the listing showed active ones, the answer would be a person with an
// empty services column and no way to tell why they were returned.
func TestRoster_ServiceFilterAgreesWithWhatIsShown(t *testing.T) {
	svc, st := rosterFixture(t,
		&fakeConn{key: "switchyard", display: "Switchyard"},
		&fakeConn{key: "lyceum", display: "Lyceum"})
	ada := addPerson(t, st, "Ada", "ada@example.com", model.PersonHuman)
	addAccount(t, st, ada, "switchyard", model.AccountActive)
	gone := addPerson(t, st, "Old Tester", "old@example.com", model.PersonHuman)
	addAccount(t, st, gone, "lyceum", model.AccountStale)

	res, err := svc.Roster(context.Background(), RosterRequest{Services: []string{"lyceum"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 0 {
		t.Errorf("a stale Lyceum row is not Lyceum access, got %+v", res.Entries)
	}
	// …and the person it dropped is exactly what Hidden accounts for, so the
	// empty result can't read as "nobody ever had Lyceum".
	if res.Hidden != 1 {
		t.Errorf("Hidden = %d, want 1", res.Hidden)
	}

	res, err = svc.Roster(context.Background(), RosterRequest{Services: []string{"lyceum"}, IncludeInactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Person.Email != "old@example.com" {
		t.Errorf("--all --to lyceum should surface the stale holder, got %+v", res.Entries)
	}

	// The service filter also narrows the accounts shown, not just the people.
	res, err = svc.Roster(context.Background(), RosterRequest{Services: []string{"switchyard"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("want only Ada, got %+v", res.Entries)
	}
	if got := keysOf(res.Entries[0]); len(got) != 1 || got[0] != "switchyard" {
		t.Errorf("accounts should be narrowed to the filter, got %v", got)
	}
}

// A typo'd service must not answer "nobody has it". That is a wrong answer to
// the question asked, which is the thing this command exists to stop.
func TestRoster_UnknownServiceIsRefused(t *testing.T) {
	svc, st := rosterFixture(t, &fakeConn{key: "switchyard", display: "Switchyard"})
	addPerson(t, st, "Ada", "ada@example.com", model.PersonHuman)

	_, err := svc.Roster(context.Background(), RosterRequest{Services: []string{"swtichyard"}})
	if err == nil {
		t.Fatal("an unknown service should be an error, not an empty roster")
	}
	if !strings.Contains(err.Error(), "switchyard") {
		t.Errorf("the error should name the known services, got: %v", err)
	}
}

func TestRoster_TypeFilter(t *testing.T) {
	svc, st := rosterFixture(t, &fakeConn{key: "switchyard", display: "Switchyard"})
	addPerson(t, st, "Ada", "ada@example.com", model.PersonHuman)
	addPerson(t, st, "Runner", "bot@example.com", model.PersonAgent)

	res, err := svc.Roster(context.Background(), RosterRequest{Type: model.PersonAgent})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Person.Email != "bot@example.com" {
		t.Errorf("--type agent should return only the agent, got %+v", res.Entries)
	}
	if _, err := svc.Roster(context.Background(), RosterRequest{Type: "person"}); err == nil {
		t.Error("an unknown --type should be refused")
	}
}

// `show` is the single-person view, so it withholds nothing: a stale row is the
// interesting part there, and it carries its status so it can't read as access.
func TestPersonDetail_ShowsEveryAccountAndTheInviteHistory(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard", result: connector.Result{
		ExternalID: "u-1", Username: "Ada", Secret: "sw_TOKEN",
	}}
	svc, st := rosterFixture(t, sw)
	p := addPerson(t, st, "Ada", "ada@example.com", model.PersonHuman)
	addAccount(t, st, p, "switchyard", model.AccountStale)

	if _, err := svc.Run(context.Background(), Request{
		Name: "Ada", Email: "ada@example.com", Services: []string{"switchyard"},
		Role: "member", Delivery: model.DeliverCopyPaste,
	}); err != nil {
		t.Fatal(err)
	}

	d, err := svc.PersonDetail(context.Background(), "ADA@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	if d.Person.ID != p.ID {
		t.Error("the lookup should be case-insensitive, like every other identity lookup")
	}
	if len(d.Accounts) != 1 {
		t.Fatalf("want the switchyard account, got %+v", d.Accounts)
	}
	if d.Accounts[0].Username != "Ada" || d.Accounts[0].ExternalID != "u-1" {
		t.Errorf("account detail is wrong: %+v", d.Accounts[0])
	}
	if len(d.Invites) != 1 {
		t.Fatalf("want the invite that re-provisioned them, got %+v", d.Invites)
	}
	if d.Invites[0].Role != "member" || d.Invites[0].Delivery != model.DeliverCopyPaste {
		t.Errorf("invite history is wrong: %+v", d.Invites[0])
	}
}

// Invite history is newest first: "when were they last re-run?" is the question
// it answers, and the answer is at the top.
func TestPersonDetail_InviteHistoryIsNewestFirst(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard", fail: errors.New("upstream down")}
	svc, st := rosterFixture(t, sw)
	addPerson(t, st, "Ada", "ada@example.com", model.PersonHuman)

	var ids []uuid.UUID
	for range 3 {
		res, err := svc.Run(context.Background(), Request{
			Name: "Ada", Email: "ada@example.com", Services: []string{"switchyard"},
			Delivery: model.DeliverCopyPaste,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.InviteID)
	}

	d, err := svc.PersonDetail(context.Background(), "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Invites) != 3 {
		t.Fatalf("want all 3 runs, got %d", len(d.Invites))
	}
	if d.Invites[0].ID != ids[2] {
		t.Errorf("newest invite should be first, got %s want %s", d.Invites[0].ID, ids[2])
	}
}

// An address nobody holds is a roster gap with a command that fixes it, so it
// reports the sentinel the CLI matches on to say which.
func TestPersonDetail_UnknownEmail(t *testing.T) {
	svc, _ := rosterFixture(t, &fakeConn{key: "switchyard", display: "Switchyard"})

	_, err := svc.PersonDetail(context.Background(), "nobody@example.com")
	if !errors.Is(err, ErrPersonNotFound) {
		t.Errorf("err = %v, want ErrPersonNotFound", err)
	}
	if _, err := svc.PersonDetail(context.Background(), ""); err == nil {
		t.Error("an empty address should be refused, not looked up")
	}
}

// keysOf lists an entry's service keys, in the order the roster returned them.
func keysOf(e RosterEntry) []string {
	out := make([]string, 0, len(e.Accounts))
	for _, a := range e.Accounts {
		out = append(out, a.ServiceKey)
	}
	return out
}
