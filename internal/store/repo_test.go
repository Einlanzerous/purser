package store

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/model"
)

// testStore connects to the DB named by PURSER_TEST_DATABASE_URL, applies
// migrations, and truncates the tables. It skips when the env var is unset, so
// `go test ./...` stays green without a database (CI provides one; see
// .github/workflows/backend.yml).
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("PURSER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("PURSER_TEST_DATABASE_URL not set; skipping DB-backed test")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`TRUNCATE provision_task, invite, account, service, person RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return New(pool)
}

// The bug PRSR-16's migration 0003 closes: person_email_key was case-sensitive
// while every lookup matched lower(email), so a row that arrived by hand as
// 'Ada@Example.com' did not collide with the lowercased address the code
// writes. The insert then added a *second* person for the same human — the
// duplicate identity `person add` exists to prevent.
//
// This can only be caught against a real index; an in-memory fake keyed by a
// lowercased string is case-insensitive by construction.
func TestPersonEmail_UniqueCaseInsensitively(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if _, err := st.pool.Exec(ctx,
		`INSERT INTO person (name, email, type) VALUES ('Ada', 'Ada@Example.com', 'human')`); err != nil {
		t.Fatal(err)
	}

	_, created, err := st.InsertPersonIfAbsent(ctx, "Ada Lovelace", "ada@example.com", model.PersonHuman)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("the differently-cased address is taken; creating is a duplicate identity")
	}
	people, err := st.ListPeople(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 1 {
		t.Fatalf("got %d person rows for one human, want 1 (duplicate identity)", len(people))
	}
	// The conflict is reported, not applied: the hand-entered row keeps its name
	// and its case.
	if people[0].Name != "Ada" || people[0].Email != "Ada@Example.com" {
		t.Errorf("existing row was modified: %+v", people[0])
	}
	got, err := st.PersonByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != people[0].ID {
		t.Error("the lookup and the index disagree about which row holds this address")
	}
}

// An occupied address is never modified — that is what lets `person add` refuse
// and `invite` report the disagreement instead of writing it.
func TestInsertPersonIfAbsent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	p, created, err := st.InsertPersonIfAbsent(ctx, "Ada", "ada@example.com", model.PersonHuman)
	if err != nil || !created {
		t.Fatalf("first insert: created=%v err=%v", created, err)
	}

	// Same address in different case: taken, and left exactly as it was.
	got, created, err := st.InsertPersonIfAbsent(ctx, "Someone Else", "ADA@Example.com", model.PersonAgent)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("created a second row for the same address in different case")
	}
	if got.ID != (model.Person{}).ID {
		t.Error("a non-create should not return a person")
	}
	existing, err := st.PersonByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if existing.ID != p.ID || existing.Name != "Ada" || existing.Type != model.PersonHuman {
		t.Errorf("existing row was modified: %+v", existing)
	}
}

// RenamePerson reports the name it replaced, read in the same statement that
// replaced it — so a caller can't announce a rename that didn't land.
func TestRenamePerson(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	p, _, err := st.InsertPersonIfAbsent(ctx, "Ada", "ada@example.com", model.PersonHuman)
	if err != nil {
		t.Fatal(err)
	}

	got, previous, err := st.RenamePerson(ctx, "ADA@Example.com", "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	if previous != "Ada" {
		t.Errorf("previous = %q, want %q", previous, "Ada")
	}
	if got.ID != p.ID {
		t.Error("rename hit a different row")
	}
	if got.Name != "Ada Lovelace" {
		t.Errorf("name = %q, want the new one", got.Name)
	}

	if _, _, err := st.RenamePerson(ctx, "nobody@example.com", "Ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("renaming an absent person: err = %v, want ErrNotFound", err)
	}
}

func TestAccountAndTask_Idempotency(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	svc, err := st.EnsureService(ctx, "switchyard", "Switchyard")
	if err != nil {
		t.Fatal(err)
	}
	// EnsureService is idempotent.
	if svc2, _ := st.EnsureService(ctx, "switchyard", "Switchyard"); svc2.ID != svc.ID {
		t.Errorf("EnsureService not idempotent")
	}

	p, _, _ := st.InsertPersonIfAbsent(ctx, "Ada", "ada@example.com", model.PersonHuman)
	inv, err := st.CreateInvite(ctx, p.ID, model.DeliverCopyPaste, "member")
	if err != nil {
		t.Fatal(err)
	}

	// AccountFor before provisioning -> not found.
	if _, err := st.AccountFor(ctx, p.ID, svc.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}

	// EnsureTask twice returns the same task row.
	tk1, _ := st.EnsureTask(ctx, inv.ID, p.ID, svc.ID)
	tk2, _ := st.EnsureTask(ctx, inv.ID, p.ID, svc.ID)
	if tk1.ID != tk2.ID {
		t.Errorf("EnsureTask not idempotent: %s vs %s", tk1.ID, tk2.ID)
	}

	// UpsertAccount twice (same person+service) returns the same account row.
	a1, err := st.UpsertAccount(ctx, model.Account{
		PersonID: p.ID, ServiceID: svc.ID, ExternalID: "u-1", Username: "Ada",
		SecretHash: "hash1", Status: model.AccountActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := st.UpsertAccount(ctx, model.Account{
		PersonID: p.ID, ServiceID: svc.ID, ExternalID: "u-1", Username: "Ada",
		SecretHash: "hash2", Status: model.AccountActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if a1.ID != a2.ID {
		t.Errorf("UpsertAccount not idempotent per (person,service): %s vs %s", a1.ID, a2.ID)
	}
	if a2.SecretHash != "hash2" {
		t.Errorf("secret hash should update, got %q", a2.SecretHash)
	}

	// Mark the task succeeded and read it back via InviteWithTasks.
	tk1.Status = model.TaskSucceeded
	tk1.AccountID = &a2.ID
	tk1.Attempts = 1
	if err := st.UpdateTask(ctx, tk1); err != nil {
		t.Fatal(err)
	}
	gotInv, tasks, err := st.InviteWithTasks(ctx, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotInv.ID != inv.ID || len(tasks) != 1 || tasks[0].Status != model.TaskSucceeded {
		t.Errorf("InviteWithTasks wrong: inv=%s tasks=%+v", gotInv.ID, tasks)
	}
}

// provision_task.status is CHECK-constrained, not free text, so PRSR-21's new
// status needed migration 0004 to exist at all. Without it every unavailable
// connector turns a cosmetic modelling fix into a hard write failure on the
// invite path — and no fake store can catch that, because the constraint is the
// thing being tested.
func TestUpdateTask_PersistsUnavailableStatus(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	svc, err := st.EnsureService(ctx, "lyceum", "Lyceum")
	if err != nil {
		t.Fatal(err)
	}
	p, _, _ := st.InsertPersonIfAbsent(ctx, "Ada", "ada@example.com", model.PersonHuman)
	inv, err := st.CreateInvite(ctx, p.ID, model.DeliverCopyPaste, "member")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := st.EnsureTask(ctx, inv.ID, p.ID, svc.ID)
	if err != nil {
		t.Fatal(err)
	}

	tk.Status = model.TaskUnavailable
	tk.Attempts = 1
	tk.LastError = "connector: provisioning not yet available: PURSER_LYCEUM_TOKEN is unset"
	if err := st.UpdateTask(ctx, tk); err != nil {
		t.Fatalf("UpdateTask with %s: %v", model.TaskUnavailable, err)
	}
	_, tasks, err := st.InviteWithTasks(ctx, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Status != model.TaskUnavailable {
		t.Errorf("status did not round-trip: %+v", tasks)
	}
}

// The one-shot backfill in migration 0004. Tasks recorded as 'failed' whose
// error came from connector.ErrPending were never failures, and leaving them
// that way means the split this migration introduces reads the past wrong
// forever.
//
// The migration has already run by the time testStore returns, so this re-execs
// its SQL — which is safe by construction (DROP … IF EXISTS, ADD, UPDATE … WHERE
// status = 'failed') and is loaded from the embedded file rather than retyped,
// so the test cannot drift from what actually ships.
func TestMigration0004_ReclassifiesOnlyErrPendingFailures(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	// Setup errors are checked rather than discarded: EnsureTask returning a
	// zero-value task would make UpdateTask a no-op UPDATE … WHERE id = NULL that
	// reports no error, and the assertions below would then fail on a status that
	// was never written — pointing at the migration instead of at the setup.
	svc, err := st.EnsureService(ctx, "lyceum", "Lyceum")
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := st.InsertPersonIfAbsent(ctx, "Ada", "ada@example.com", model.PersonHuman)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := st.CreateInvite(ctx, p.ID, model.DeliverCopyPaste, "member")
	if err != nil {
		t.Fatal(err)
	}

	// Two rows as the pre-0004 orchestrator would have left them: both 'failed',
	// distinguishable only by whether the error came from ErrPending.
	unavailable, err := st.EnsureTask(ctx, inv.ID, p.ID, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	unavailable.Status = model.TaskFailed
	unavailable.LastError = connector.ErrPending.Error() + ": PURSER_LYCEUM_TOKEN is unset"
	if err := st.UpdateTask(ctx, unavailable); err != nil {
		t.Fatal(err)
	}

	other, err := st.EnsureService(ctx, "switchyard", "Switchyard")
	if err != nil {
		t.Fatal(err)
	}
	broken, err := st.EnsureTask(ctx, inv.ID, p.ID, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	broken.Status = model.TaskFailed
	broken.LastError = "switchyard: 500 Internal Server Error"
	if err := st.UpdateTask(ctx, broken); err != nil {
		t.Fatal(err)
	}

	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, m := range migs {
		if m.version == "0004" {
			sql = m.sql
		}
	}
	if sql == "" {
		t.Fatal("migration 0004 not found in the embedded FS")
	}
	if _, err := st.pool.Exec(ctx, sql); err != nil {
		t.Fatalf("re-apply 0004: %v", err)
	}

	_, tasks, err := st.InviteWithTasks(ctx, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]model.TaskStatus{}
	for _, tk := range tasks {
		got[tk.ID] = tk.Status
	}
	if got[unavailable.ID] != model.TaskUnavailable {
		t.Errorf("an ErrPending task should reclassify, got %s", got[unavailable.ID])
	}
	// The conservative half, and the one worth protecting: a real failure whose
	// text we can't attribute stays a failure rather than being guessed at.
	if got[broken.ID] != model.TaskFailed {
		t.Errorf("a genuine failure must not be reclassified, got %s", got[broken.ID])
	}
}
