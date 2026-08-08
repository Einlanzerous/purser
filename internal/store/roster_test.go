package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/model"
)

// The roster's read of an account must not carry credential material, and the
// guarantee is structural: AccountRecord has no secret_hash or secret_ref field
// and the query selects neither column.
//
// That makes the property invisible to an ordinary assertion — there is nothing
// to check for absence — so this marshals the whole record and looks for the
// stored values instead. It passes vacuously today and stops passing the moment
// someone adds a secret field to the type or the SELECT, which is the edit worth
// catching: `--json` serializes whatever the struct holds, and credentials are
// shown exactly once, at invite time, by design (PRSR-24).
func TestAccountRecords_CarryNoSecretMaterial(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	const (
		secretHash = "9f2c4b1e-this-is-the-hash"
		secretRef  = "vault://purser/ada/switchyard"
	)
	svc, err := st.EnsureService(ctx, "switchyard", "Switchyard")
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := st.InsertPersonIfAbsent(ctx, "Ada", "ada@example.com", model.PersonHuman)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertAccount(ctx, model.Account{
		PersonID: p.ID, ServiceID: svc.ID, ExternalID: "u-1", Username: "ada",
		SecretHash: secretHash, SecretRef: secretRef, Status: model.AccountActive,
	}); err != nil {
		t.Fatal(err)
	}

	records, err := st.AccountRecordsFor(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("want 1 record, got %d", len(records))
	}
	encoded, err := json.Marshal(records[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{secretHash, secretRef} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("a roster record reached JSON carrying %q:\n%s", secret, encoded)
		}
	}

	// The account is still whole where it belongs — this is a narrower read of
	// the same row, not a narrower row.
	acct, err := st.AccountFor(ctx, p.ID, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if acct.SecretHash != secretHash || acct.SecretRef != secretRef {
		t.Errorf("the account row itself should be untouched: %+v", acct)
	}
}

// The join is what makes the roster readable: an account's service key and
// display name, not a UUID the operator would have to resolve by hand.
func TestAccountRecords_JoinServiceAndScope(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	sw, err := st.EnsureService(ctx, "switchyard", "Switchyard")
	if err != nil {
		t.Fatal(err)
	}
	ly, err := st.EnsureService(ctx, "lyceum", "Lyceum")
	if err != nil {
		t.Fatal(err)
	}
	ada, _, err := st.InsertPersonIfAbsent(ctx, "Ada", "ada@example.com", model.PersonHuman)
	if err != nil {
		t.Fatal(err)
	}
	bob, _, err := st.InsertPersonIfAbsent(ctx, "Bob", "bob@example.com", model.PersonHuman)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []model.Account{
		{PersonID: ada.ID, ServiceID: sw.ID, Username: "ada", Status: model.AccountActive},
		{PersonID: ada.ID, ServiceID: ly.ID, Username: "ada", Status: model.AccountStale},
		{PersonID: bob.ID, ServiceID: sw.ID, Username: "bob", Status: model.AccountActive},
	} {
		if _, err := st.UpsertAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	all, err := st.AccountRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want every account, got %d", len(all))
	}

	mine, err := st.AccountRecordsFor(ctx, ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 {
		t.Fatalf("want Ada's two accounts, got %+v", mine)
	}
	// Ordered by service key, so output doesn't depend on insertion order.
	if mine[0].ServiceKey != "lyceum" || mine[1].ServiceKey != "switchyard" {
		t.Errorf("records should be ordered by service key, got %s then %s",
			mine[0].ServiceKey, mine[1].ServiceKey)
	}
	if mine[0].DisplayName != "Lyceum" || mine[0].Status != model.AccountStale {
		t.Errorf("joined service/status is wrong: %+v", mine[0])
	}
	if mine[0].PersonID != ada.ID {
		t.Errorf("person id should come back for grouping, got %s", mine[0].PersonID)
	}
}

func TestListServicesAndInvitesFor(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if _, err := st.EnsureService(ctx, "switchyard", "Switchyard"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureService(ctx, "argosy", "Argosy"); err != nil {
		t.Fatal(err)
	}
	services, err := st.ListServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 2 || services[0].Key != "argosy" || services[1].Key != "switchyard" {
		t.Errorf("services should come back by key: %+v", services)
	}

	p, _, err := st.InsertPersonIfAbsent(ctx, "Ada", "ada@example.com", model.PersonHuman)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := st.InsertPersonIfAbsent(ctx, "Bob", "bob@example.com", model.PersonHuman)
	if err != nil {
		t.Fatal(err)
	}
	// Explicit timestamps: the ordering is the assertion, and two inserts a
	// microsecond apart would make it a coin flip rather than a check. Written
	// and read back in UTC, so the check is about ordering rather than about
	// which side of midnight the test machine's zone lands on.
	for _, at := range []string{"2026-01-01T00:00:00Z", "2026-03-01T00:00:00Z", "2026-02-01T00:00:00Z"} {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO invite (person_id, delivery, role, created_at) VALUES ($1, 'copypaste', 'member', $2)`,
			p.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateInvite(ctx, other.ID, model.DeliverEmail, "admin"); err != nil {
		t.Fatal(err)
	}

	invites, err := st.InvitesFor(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invites) != 3 {
		t.Fatalf("want Ada's three invites and nobody else's, got %d", len(invites))
	}
	if got := invites[0].CreatedAt.UTC().Format("2006-01-02"); got != "2026-03-01" {
		t.Errorf("history should be newest first, got %s", got)
	}
	if got := invites[2].CreatedAt.UTC().Format("2006-01-02"); got != "2026-01-01" {
		t.Errorf("oldest invite should be last, got %s", got)
	}
}
