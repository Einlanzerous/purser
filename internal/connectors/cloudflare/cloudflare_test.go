package cloudflare

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

func TestProvision_Unconfigured_ReturnsPendingWithManualStep(t *testing.T) {
	c := New(Config{GroupName: "zerogravity-members"}) // no token/account/group
	_, err := c.Provision(context.Background(), connector.Input{Email: "ada@example.com"})
	if !errors.Is(err, connector.ErrPending) {
		t.Fatalf("want ErrPending, got %v", err)
	}
	if !strings.Contains(err.Error(), "zerogravity-members") || !strings.Contains(err.Error(), "ada@example.com") {
		t.Errorf("manual step should name the group and email, got: %v", err)
	}
}

func TestProvision_AddsEmailToGroup(t *testing.T) {
	var putBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cf_token" {
			t.Errorf("missing CF auth")
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"success":true,"result":{"name":"zerogravity-members","include":[{"email":{"email":"existing@example.com"}}]}}`))
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &putBody)
			_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
		}
	}))
	defer srv.Close()

	c := newWithBase(t, srv.URL, Config{APIToken: "cf_token", AccountID: "acct", GroupID: "grp", GroupName: "zerogravity-members"})
	res, err := c.Provision(context.Background(), connector.Input{Email: "Ada@Example.com"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.ExternalID != "ada@example.com" {
		t.Errorf("external id should be the lowercased email, got %q", res.ExternalID)
	}
	// The PUT should include both the existing and the new (lowercased) email.
	include, _ := putBody["include"].([]any)
	if !includesEmail(include, "existing@example.com") || !includesEmail(include, "ada@example.com") {
		t.Errorf("include rules wrong: %v", putBody["include"])
	}
}

func TestProvision_AlreadyPresent_IsIdempotentNoPut(t *testing.T) {
	putCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putCalled = true
		}
		_, _ = w.Write([]byte(`{"success":true,"result":{"name":"g","include":[{"email":{"email":"ada@example.com"}}]}}`))
	}))
	defer srv.Close()

	c := newWithBase(t, srv.URL, Config{APIToken: "cf_token", AccountID: "acct", GroupID: "grp"})
	if _, err := c.Provision(context.Background(), connector.Input{Email: "ada@example.com"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if putCalled {
		t.Error("PUT should not be called when the email is already in the group")
	}
}

func TestProvision_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1000,"message":"invalid token"}]}`))
	}))
	defer srv.Close()

	c := newWithBase(t, srv.URL, Config{APIToken: "cf_token", AccountID: "acct", GroupID: "grp"})
	_, err := c.Provision(context.Background(), connector.Input{Email: "ada@example.com"})
	if err == nil || !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("want API error surfaced, got %v", err)
	}
}

func includesEmail(rules []any, email string) bool {
	for _, r := range rules {
		m, _ := r.(map[string]any)
		em, _ := m["email"].(map[string]any)
		if got, _ := em["email"].(string); got == email {
			return true
		}
	}
	return false
}
