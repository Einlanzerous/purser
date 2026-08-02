package invite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/model"
)

// personFixture is auditFixture without the person — AddPerson is what creates
// them here.
func personFixture(t *testing.T, conns ...connector.Connector) (*Service, *fakeStore) {
	t.Helper()
	st := newFakeStore()
	reg := connector.NewRegistry(conns...)
	return New(seededStore(t, st, reg), reg, nil), st
}

// The headline property: adding a person provisions nothing. No account rows,
// no connector calls of any kind — not even the read-only Reconcile.
func TestAddPerson_ProvisionsNothing(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard"}
	svc, st := personFixture(t, sw)

	res, err := svc.AddPerson(context.Background(), AddPersonRequest{
		Name: "Ada Lovelace", Email: "Ada@Example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Error("want Created")
	}
	if res.Person.Email != "ada@example.com" {
		t.Errorf("email not normalized: %q", res.Person.Email)
	}
	if res.Person.Type != model.PersonHuman {
		t.Errorf("type = %q, want human by default", res.Person.Type)
	}
	if len(st.accounts) != 0 {
		t.Errorf("AddPerson wrote %d account rows, want 0", len(st.accounts))
	}
	if sw.callCount() != 0 || sw.reconcileCount() != 0 {
		t.Errorf("connector called: %d provision, %d reconcile; want 0 of each",
			sw.callCount(), sw.reconcileCount())
	}
}

// The point of the command (PRSR-16): the audit walks the person table, so
// someone provisioned outside Purser is invisible to it until this row exists.
func TestAddPerson_MakesThemAuditable(t *testing.T) {
	sw := &fakeConn{key: "switchyard", display: "Switchyard",
		recResult: connector.ReconcileResult{Exists: true, ExternalID: "u1", Username: "Ada"}}
	svc, _ := personFixture(t, sw)
	ctx := context.Background()

	before, err := svc.Audit(ctx, AuditRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Findings) != 0 {
		t.Fatalf("empty roster produced %d findings", len(before.Findings))
	}

	if _, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Ada", Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}

	after, err := svc.Audit(ctx, AuditRequest{Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	f, ok := findingFor(after, "switchyard")
	if !ok {
		t.Fatal("no switchyard finding after adding the person")
	}
	if f.Action != ActionRecord {
		t.Errorf("action = %q, want %q — the access upstream should now be recordable", f.Action, ActionRecord)
	}
}

// UpsertPerson's ON CONFLICT ... SET name renames silently. A bare add must not
// reach it: a name that disagrees with the record is a conflict, not an edit.
func TestAddPerson_RefusesSilentRename(t *testing.T) {
	svc, st := personFixture(t)
	ctx := context.Background()

	if _, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Ada", Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}

	_, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Someone Else", Email: "ada@example.com"})
	if !errors.Is(err, ErrNameConflict) {
		t.Fatalf("err = %v, want ErrNameConflict", err)
	}
	p, err := st.PersonByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Ada" {
		t.Errorf("name = %q, want it untouched by the refused add", p.Name)
	}
}

func TestAddPerson_RenameApplies(t *testing.T) {
	svc, st := personFixture(t)
	ctx := context.Background()

	first, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Ada", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.AddPerson(ctx, AddPersonRequest{
		Name: "Ada Lovelace", Email: "ada@example.com", Rename: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created {
		t.Error("a rename is not a creation")
	}
	if !res.Renamed || res.PreviousName != "Ada" {
		t.Errorf("Renamed=%v PreviousName=%q, want true/%q", res.Renamed, res.PreviousName, "Ada")
	}
	if res.Person.ID != first.Person.ID {
		t.Error("rename created a second identity instead of reusing the row")
	}
	p, _ := st.PersonByEmail(ctx, "ada@example.com")
	if p.Name != "Ada Lovelace" {
		t.Errorf("stored name = %q, want the renamed one", p.Name)
	}
}

// Re-adding the same person is a no-op, not an error — same house rule as a
// re-invite.
func TestAddPerson_IdempotentOnExactMatch(t *testing.T) {
	svc, _ := personFixture(t)
	ctx := context.Background()

	first, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Ada", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Ada", Email: "ADA@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Created || again.Renamed {
		t.Errorf("Created=%v Renamed=%v, want both false", again.Created, again.Renamed)
	}
	if again.Person.ID != first.Person.ID {
		t.Error("re-add produced a different person")
	}
}

func TestAddPerson_Validation(t *testing.T) {
	svc, _ := personFixture(t)
	cases := []struct {
		name string
		req  AddPersonRequest
		want string // substring the error must name, so a wrong rejection fails
	}{
		// Without an email there is no conflict target and no way for the audit
		// to look them up, so every add would mint another duplicate.
		{"no email", AddPersonRequest{Name: "Ada"}, "email is required"},
		{"blank email", AddPersonRequest{Name: "Ada", Email: "   "}, "email is required"},
		{"no name", AddPersonRequest{Email: "ada@example.com"}, "name is required"},
		{"unknown type", AddPersonRequest{Name: "Ada", Email: "ada@example.com", Type: "robot"}, "unknown person type"},
		{"not an address", AddPersonRequest{Name: "Ada", Email: "ada-at-example"}, "not a valid email"},
		{"bare local part", AddPersonRequest{Name: "Ada", Email: "ada@"}, "not a valid email"},
		{"display-name form", AddPersonRequest{Name: "Ada", Email: "Ada <ada@example.com>"}, "not a valid email"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.AddPerson(context.Background(), tc.req)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The other half of the silent-edit class the --rename guard closes: the upsert
// only ever wrote `name` on conflict, so a --type that disagreed with the
// record was quietly dropped and the caller was told the add succeeded.
func TestAddPerson_RefusesSilentTypeChange(t *testing.T) {
	svc, st := personFixture(t)
	ctx := context.Background()

	if _, err := svc.AddPerson(ctx, AddPersonRequest{
		Name: "purser-bot", Email: "bot@example.com", Type: model.PersonAgent,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := svc.AddPerson(ctx, AddPersonRequest{
		Name: "purser-bot", Email: "bot@example.com", Type: model.PersonHuman,
	})
	if !errors.Is(err, ErrTypeConflict) {
		t.Fatalf("err = %v, want ErrTypeConflict", err)
	}
	p, _ := st.PersonByEmail(ctx, "bot@example.com")
	if p.Type != model.PersonAgent {
		t.Errorf("type = %q, want it untouched by the refused add", p.Type)
	}
}

// An unspecified type must not assert one: re-adding an agent without --type
// keeps them an agent rather than converting them to the default.
func TestAddPerson_UnspecifiedTypeKeepsTheRecord(t *testing.T) {
	svc, _ := personFixture(t)
	ctx := context.Background()

	if _, err := svc.AddPerson(ctx, AddPersonRequest{
		Name: "purser-bot", Email: "bot@example.com", Type: model.PersonAgent,
	}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.AddPerson(ctx, AddPersonRequest{Name: "purser-bot", Email: "bot@example.com"})
	if err != nil {
		t.Fatalf("re-adding without --type should not conflict: %v", err)
	}
	if res.Person.Type != model.PersonAgent {
		t.Errorf("type = %q, want agent preserved", res.Person.Type)
	}
}

// A rename must report the name the write actually replaced. Inferring it from
// the preceding read is what let the command announce a rename it hadn't done.
func TestAddPerson_RenameReportsWhatTheWriteReplaced(t *testing.T) {
	svc, st := personFixture(t)
	ctx := context.Background()

	if _, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Ada", Email: "ada@example.com"}); err != nil {
		t.Fatal(err)
	}
	// Someone else renames the row between our read and our write.
	if _, _, err := st.RenamePerson(ctx, "ada@example.com", "Ada Byron"); err != nil {
		t.Fatal(err)
	}

	res, err := svc.AddPerson(ctx, AddPersonRequest{
		Name: "Ada Lovelace", Email: "ada@example.com", Rename: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.PreviousName != "Ada Byron" {
		t.Errorf("PreviousName = %q, want %q — the value the update replaced",
			res.PreviousName, "Ada Byron")
	}
}

func TestAddPerson_AgentType(t *testing.T) {
	svc, _ := personFixture(t)
	res, err := svc.AddPerson(context.Background(), AddPersonRequest{
		Name: "purser-bot", Email: "bot@example.com", Type: model.PersonAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Person.Type != model.PersonAgent {
		t.Errorf("type = %q, want agent", res.Person.Type)
	}
}
