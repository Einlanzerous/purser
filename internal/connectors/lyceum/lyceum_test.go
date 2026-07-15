package lyceum

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
