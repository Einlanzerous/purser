package lyceum

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/connector"
)

func TestNew_RequiresBaseAndToken(t *testing.T) {
	if _, err := New(Config{OwnerToken: "lyc_x"}); err == nil {
		t.Error("expected error without BaseURL")
	}
	if _, err := New(Config{BaseURL: "http://lyceum:4005"}); err == nil {
		t.Error("expected error without OwnerToken")
	}
}

func TestProvision_CreatesUserReturnsInvite(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer lyc_owner" {
			t.Errorf("missing owner auth: %q", r.Header.Get("Authorization"))
		}
		if r.Method != http.MethodPost || r.URL.Path != "/admin/users" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"user":{"id":42,"email":"mara@example.com","display_name":"Mara"},"invite_token":"lyc_INVITE"}`))
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, OwnerToken: "lyc_owner"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Provision(context.Background(), connector.Input{PersonName: "Mara", Email: "Mara@Example.com"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if body["email"] != "mara@example.com" {
		t.Errorf("email should be lowercased and sent, got %v", body["email"])
	}
	if body["display_name"] != "Mara" {
		t.Errorf("display_name: %v", body["display_name"])
	}
	if res.ExternalID != "42" || res.Secret != "lyc_INVITE" {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestProvision_ConflictIsReconciledSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"email already exists"}`))
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, OwnerToken: "lyc_owner"})
	res, err := c.Provision(context.Background(), connector.Input{Email: "mara@example.com"})
	if err != nil {
		t.Fatalf("409 should reconcile to success, got %v", err)
	}
	if res.Secret != "" || res.ExternalID != "mara@example.com" {
		t.Errorf("conflict result should carry no secret + email as id: %+v", res)
	}
}

func TestProvision_ForbiddenIsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"household administration requires LYCEUM_AUTH"}`))
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, OwnerToken: "lyc_owner"})
	_, err := c.Provision(context.Background(), connector.Input{Email: "mara@example.com"})
	if err == nil || !strings.Contains(err.Error(), "LYCEUM_AUTH") {
		t.Fatalf("403 should surface the LYCEUM_AUTH hint, got %v", err)
	}
}

func TestProvision_RequiresEmail(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://x", OwnerToken: "lyc_owner"})
	if _, err := c.Provision(context.Background(), connector.Input{PersonName: "No Email"}); err == nil {
		t.Error("expected error when email is missing")
	}
}

// Reconcile lists the household read-only: no user created, no invite minted.
func TestReconcile_FindsUserWithoutCreatingOrMinting(t *testing.T) {
	var writes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			t.Errorf("Reconcile must only read, got %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Path != "/admin/users" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":7,"email":"mara@example.com","display_name":"Mara","is_owner":false}]`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, OwnerToken: "lyc_owner"})
	res, err := c.Reconcile(context.Background(), connector.Input{Email: "Mara@Example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Exists || res.ExternalID != "7" || res.Username != "Mara" {
		t.Errorf("unexpected reconcile result: %+v", res)
	}
	if writes != 0 {
		t.Errorf("Reconcile performed %d write(s)", writes)
	}
}

func TestReconcile_ReportsAbsentUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":1,"email":"someone@else.com","display_name":"Other"}]`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, OwnerToken: "lyc_owner"})
	res, err := c.Reconcile(context.Background(), connector.Input{Email: "mara@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exists {
		t.Error("a non-member should report Exists=false")
	}
}

// A 403 must surface the LYCEUM_AUTH hint rather than looking like absence —
// otherwise a misconfigured owner token would mark every record stale.
func TestReconcile_ForbiddenIsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"owner only"}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, OwnerToken: "lyc_owner"})
	if _, err := c.Reconcile(context.Background(), connector.Input{Email: "mara@example.com"}); err == nil ||
		!strings.Contains(err.Error(), "LYCEUM_AUTH") {
		t.Fatalf("403 should surface the LYCEUM_AUTH hint, got %v", err)
	}
}

// Lyceum is the one connector where Deprovision deletes: DELETE /admin/users/{id}
// is the only destructive operation the admin surface offers, and there is no
// disable. Documented rather than disguised (PRSR-17).
func TestDeprovision_DeletesTheUser(t *testing.T) {
	var deleted []string
	var listCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/admin/users/"):
			deleted = append(deleted, strings.TrimPrefix(r.URL.Path, "/admin/users/"))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/admin/users":
			listCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":7,"email":"ada@example.com","display_name":"Ada"}]`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, OwnerToken: "lyc_owner"})
	if err != nil {
		t.Fatal(err)
	}
	// With the recorded id, no lookup is needed.
	if err := c.Deprovision(context.Background(), connector.Input{
		Email: "ada@example.com", ExternalID: "7",
	}); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "7" {
		t.Errorf("deleted = %v, want [7]", deleted)
	}
	if listCalls != 0 {
		t.Errorf("a recorded id should skip the lookup, got %d list calls", listCalls)
	}

	// Without one it falls back to the email lookup.
	if err := c.Deprovision(context.Background(), connector.Input{Email: "ada@example.com"}); err != nil {
		t.Fatalf("Deprovision via lookup: %v", err)
	}
	if listCalls != 1 || len(deleted) != 2 {
		t.Errorf("want one lookup and a second delete, got listCalls=%d deleted=%v", listCalls, deleted)
	}
}

// Idempotent both ways: a person Lyceum has never heard of, and a 404 from the
// delete itself, are both the state the caller asked for.
func TestDeprovision_MissingUserIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/users":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, OwnerToken: "lyc_owner"})
	if err := c.Deprovision(context.Background(), connector.Input{Email: "ghost@example.com"}); err != nil {
		t.Errorf("an unknown person should be success, got %v", err)
	}
	if err := c.Deprovision(context.Background(), connector.Input{
		Email: "ghost@example.com", ExternalID: "99",
	}); err != nil {
		t.Errorf("a 404 from delete should be success, got %v", err)
	}
}

// Provision's 409 branch records the *email* in ExternalID, and Lyceum's DELETE
// handler ParseInts the path segment — so passing it straight through yielded 400
// on every run, forever, for anyone who already had a Lyceum account when they
// were invited. The empty-id fallback that knows how to look them up was never
// reached, because the id was not empty (PRSR-17 review).
func TestDeprovision_RecoversWhenExternalIDIsAnEmail(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/users":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":7,"email":"ada@example.com","display_name":"Ada"}]`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/admin/users/"):
			id := strings.TrimPrefix(r.URL.Path, "/admin/users/")
			// Mirror the real handler: a non-numeric id is a 400, not a delete.
			if _, err := strconv.ParseInt(id, 10, 64); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			deleted = append(deleted, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, OwnerToken: "lyc_owner"})
	if err := c.Deprovision(context.Background(), connector.Input{
		Email: "ada@example.com", ExternalID: "ada@example.com",
	}); err != nil {
		t.Fatalf("an email in ExternalID must resolve by lookup, got %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "7" {
		t.Errorf("deleted = %v, want the looked-up numeric id [7]", deleted)
	}
}

// A 404 against the *recorded* id means the record is wrong, not that the person
// has no account. Reporting success would mark them deprovisioned while the real
// Lyceum user stays — and the next run would skip them.
func TestDeprovision_StaleExternalIDFallsBackToTheLookup(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/users":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"id":7,"email":"ada@example.com","display_name":"Ada"}]`))
		case r.Method == http.MethodDelete && r.URL.Path == "/admin/users/999":
			w.WriteHeader(http.StatusNotFound) // the recorded id is stale
		case r.Method == http.MethodDelete && r.URL.Path == "/admin/users/7":
			deleted = append(deleted, "7")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, OwnerToken: "lyc_owner"})
	if err := c.Deprovision(context.Background(), connector.Input{
		Email: "ada@example.com", ExternalID: "999",
	}); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "7" {
		t.Errorf("deleted = %v — a stale id must not be reported as success", deleted)
	}
}

// Lyceum's DELETE returns 403 for two unrelated reasons and they need different
// fixes: a non-owner token, or the household owner being immutable. The message
// must not blame only the first.
func TestDeprovision_403MentionsBothCauses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("the owner account cannot be removed"))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, OwnerToken: "lyc_owner"})
	err := c.Deprovision(context.Background(), connector.Input{Email: "ada@example.com", ExternalID: "1"})
	if err == nil {
		t.Fatal("a 403 must be an error")
	}
	if !strings.Contains(err.Error(), "owner, who cannot be removed") {
		t.Errorf("the message should name the immutable-owner case too, got: %v", err)
	}
}
