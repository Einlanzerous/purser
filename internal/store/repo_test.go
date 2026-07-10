package store

import (
	"context"
	"errors"
	"os"
	"testing"

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

func TestUpsertPerson_IdempotentByEmail(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	p1, err := st.UpsertPerson(ctx, "Ada", "ada@example.com", model.PersonHuman)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := st.UpsertPerson(ctx, "Ada Lovelace", "ada@example.com", model.PersonHuman)
	if err != nil {
		t.Fatal(err)
	}
	if p1.ID != p2.ID {
		t.Errorf("same email should return same person: %s vs %s", p1.ID, p2.ID)
	}
	if p2.Name != "Ada Lovelace" {
		t.Errorf("name should update on upsert, got %q", p2.Name)
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

	p, _ := st.UpsertPerson(ctx, "Ada", "ada@example.com", model.PersonHuman)
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
