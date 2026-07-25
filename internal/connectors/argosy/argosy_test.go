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

// Argosy exposes only a create endpoint, so Reconcile must refuse rather than
// infer absence — claiming people who demonstrably have accounts don't have
// them is exactly the drift the audit exists to find (SERV-54, ARGY-163).
func TestReconcile_IsUnsupportedAndNeverCalls(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: "prov_secret"})
	_, err := c.Reconcile(context.Background(), connector.Input{Email: "ada@example.com"})
	if !errors.Is(err, connector.ErrReconcileUnsupported) {
		t.Fatalf("want ErrReconcileUnsupported, got %v", err)
	}
	if calls != 0 {
		t.Errorf("Reconcile must not touch the upstream API, got %d call(s)", calls)
	}
}
