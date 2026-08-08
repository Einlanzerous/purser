package argosy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/connector"
)

func TestNew_RequiresBaseAndToken(t *testing.T) {
	if _, err := New(Config{ProvisionToken: "tok"}); err == nil {
		t.Error("expected error without BaseURL")
	}
	if _, err := New(Config{BaseURL: "http://argosy:8096"}); err == nil {
		t.Error("expected error without ProvisionToken")
	}
}

func TestProvision_CreatesAccountReturnsGeneratedPassword(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Provision-Token"); got != "prov_secret" {
			t.Errorf("provision token header: %q", got)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/admin/accounts" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"account":{"id":"11111111-2222-3333-4444-555555555555","name":"Mara"},"generatedPassword":"hunter2hunter2"}`))
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, ProvisionToken: "prov_secret", AppURL: "https://argosy.zerogravity.industries"})
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
	if body["accountName"] != "Mara" {
		t.Errorf("accountName: %v", body["accountName"])
	}
	// Omitting password is what makes Argosy generate and return one.
	if _, sent := body["password"]; sent {
		t.Errorf("password must be omitted so the server generates one, got %v", body["password"])
	}
	if res.ExternalID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("ExternalID should be the account id: %q", res.ExternalID)
	}
	if res.Secret != "hunter2hunter2" {
		t.Errorf("Secret should be the generated password: %q", res.Secret)
	}
	if res.Username != "mara@example.com" {
		t.Errorf("Username should be the login email: %q", res.Username)
	}
	if !strings.Contains(res.Instructions, "https://argosy.zerogravity.industries") {
		t.Errorf("instructions should name the sign-in URL: %q", res.Instructions)
	}
}

func TestProvision_FallsBackToEmailAsAccountName(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"account":{"id":"abc","name":"mara@example.com"},"generatedPassword":"pw"}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: "prov_secret"})
	if _, err := c.Provision(context.Background(), connector.Input{Email: "mara@example.com"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// accountName is required upstream — a person with no display name must not 400.
	if body["accountName"] != "mara@example.com" {
		t.Errorf("accountName should fall back to the email, got %v", body["accountName"])
	}
}

func TestProvision_ConflictIsReconciledSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"that email is already registered"}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: "prov_secret"})
	res, err := c.Provision(context.Background(), connector.Input{Email: "mara@example.com"})
	if err != nil {
		t.Fatalf("409 should reconcile to success, got %v", err)
	}
	if res.Secret != "" {
		t.Errorf("conflict must not mint a second password: %q", res.Secret)
	}
	if res.ExternalID != "mara@example.com" {
		t.Errorf("conflict result should carry the email as id: %+v", res)
	}
}

func TestProvision_UnauthorizedNamesTheTokenMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid provisioning token"}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: "wrong"})
	_, err := c.Provision(context.Background(), connector.Input{Email: "mara@example.com"})
	if err == nil || !strings.Contains(err.Error(), "ARGOSY_PROVISION_TOKEN") {
		t.Fatalf("401 should point at the token mismatch, got %v", err)
	}
}

func TestProvision_NotFoundNamesTheUnsetEnv(t *testing.T) {
	// Argosy registers the route only when ARGOSY_PROVISION_TOKEN is set, so a
	// 404 means the service env is missing — not that the URL is wrong.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`404 page not found`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: "prov_secret"})
	_, err := c.Provision(context.Background(), connector.Input{Email: "mara@example.com"})
	if err == nil || !strings.Contains(err.Error(), "ARGOSY_PROVISION_TOKEN") {
		t.Fatalf("404 should explain the unset env, got %v", err)
	}
}

func TestProvision_RequiresEmail(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://x", ProvisionToken: "prov_secret"})
	if _, err := c.Provision(context.Background(), connector.Input{PersonName: "No Email"}); err == nil {
		t.Error("expected error when email is missing")
	}
}

// Reconcile uses the read-only lookup (ARGY-163): no account created.
func TestReconcile_FindsAccountViaLookup(t *testing.T) {
	var writes int
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			t.Errorf("Reconcile must only read, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/admin/accounts" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Provision-Token"); got != "prov_secret" {
			t.Errorf("provision token header: %q", got)
		}
		gotQuery = r.URL.Query().Get("email")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"account":{"id":"acct-uuid","name":"Mara"}}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: "prov_secret"})
	res, err := c.Reconcile(context.Background(), connector.Input{Email: "Mara@Example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if gotQuery != "mara@example.com" {
		t.Errorf("email should be lowercased in the query, got %q", gotQuery)
	}
	if !res.Exists || res.ExternalID != "acct-uuid" {
		t.Errorf("unexpected result: %+v", res)
	}
	if writes != 0 {
		t.Errorf("Reconcile performed %d write(s)", writes)
	}
}

func TestReconcile_404IsAbsentNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: "prov_secret"})
	res, err := c.Reconcile(context.Background(), connector.Input{Email: "mara@example.com"})
	if err != nil {
		t.Fatalf("a 404 from the lookup means absent, not an error: %v", err)
	}
	if res.Exists {
		t.Error("404 should report Exists=false")
	}
}

func TestReconcile_401NamesTheTokenMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid provisioning token"}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: "wrong"})
	_, err := c.Reconcile(context.Background(), connector.Input{Email: "mara@example.com"})
	if err == nil || !strings.Contains(err.Error(), "ARGOSY_PROVISION_TOKEN") {
		t.Fatalf("401 should point at the token mismatch, got %v", err)
	}
}

// ARGY-163 also put the existing account in the create 409, so the conflict
// path can record the real upstream id instead of falling back to the email.
func TestProvision_ConflictRecordsTheRealAccountID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"account":{"id":"acct-uuid","name":"Mara"},"error":"an account with that email already exists"}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: "prov_secret"})
	res, err := c.Provision(context.Background(), connector.Input{Email: "mara@example.com", PersonName: "Mara"})
	if err != nil {
		t.Fatalf("409 should reconcile to success: %v", err)
	}
	if res.ExternalID != "acct-uuid" {
		t.Errorf("conflict should record the upstream id, got %q", res.ExternalID)
	}
	if res.Secret != "" {
		t.Errorf("conflict must not mint a password, got %q", res.Secret)
	}
}

// Older Argosy builds send only {"error":…} on 409 — still usable.
func TestProvision_ConflictFallsBackToEmailWhenNoAccountInBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"an account with that email already exists"}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: "prov_secret"})
	res, err := c.Provision(context.Background(), connector.Input{Email: "mara@example.com", PersonName: "Mara"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExternalID != "mara@example.com" {
		t.Errorf("should fall back to the email, got %q", res.ExternalID)
	}
}

// Argosy cannot revoke, and must say so as ErrPending rather than as a plain
// error or — far worse — as success. The orchestrator records it as
// TaskUnavailable, which is what lets a three-of-four offboard report honestly
// instead of claiming access was removed that wasn't (PRSR-17).
func TestDeprovision_ReportsUnavailableNotSuccess(t *testing.T) {
	c, err := New(Config{BaseURL: "http://argosy:4004", ProvisionToken: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Deprovision(context.Background(), connector.Input{
		Email: "ada@example.com", ExternalID: "a-1",
	})
	if err == nil {
		t.Fatal("Deprovision must not report success — the account is still there")
	}
	if !errors.Is(err, connector.ErrPending) {
		t.Errorf("err = %v, want it to wrap connector.ErrPending", err)
	}
}
