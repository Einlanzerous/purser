package switchyard

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

func TestProvision_CreatesUserAndMintsToken(t *testing.T) {
	var createdBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sw_admin" {
			t.Errorf("missing admin auth: %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/users":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &createdBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"u-42","name":"Ada","email":"ada@example.com"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/users/u-42/tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"t-1","token":"sw_SECRETTOKEN"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Token: "sw_admin", LoginURL: "https://sw.example"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Provision(context.Background(), connector.Input{
		PersonName: "Ada", Email: "ada@example.com", Role: "member", InviteRef: "purser-1-switchyard",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.ExternalID != "u-42" || res.Secret != "sw_SECRETTOKEN" {
		t.Errorf("unexpected result: %+v", res)
	}
	if createdBody["email"] != "ada@example.com" {
		t.Errorf("email not sent on user create: %v", createdBody["email"])
	}
	if createdBody["instance_role"] != "member" {
		t.Errorf("instance_role: want member, got %v", createdBody["instance_role"])
	}
}

func TestProvision_AdminRole_SetsOwnerAndAdminScope(t *testing.T) {
	var createBody, tokenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/users":
			_ = json.Unmarshal(body, &createBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"u-7","name":"Boss"}`))
		default:
			_ = json.Unmarshal(body, &tokenBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"sw_X"}`))
		}
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Token: "sw_admin"})
	if _, err := c.Provision(context.Background(), connector.Input{PersonName: "Boss", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if createBody["instance_role"] != "owner" {
		t.Errorf("admin role should map to instance_role=owner, got %v", createBody["instance_role"])
	}
	scopes, _ := tokenBody["scopes"].([]any)
	if len(scopes) != 1 || scopes[0] != "admin" {
		t.Errorf("admin role should mint admin scope, got %v", tokenBody["scopes"])
	}
}

func TestProvision_ConflictReconcilesToExistingUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/users":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"email taken"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/users":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"items":[{"id":"u-99","name":"Ada","email":"ada@example.com"}],"page":{"next_cursor":null}}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/users/u-99/tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"sw_REUSED"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Token: "sw_admin"})
	res, err := c.Provision(context.Background(), connector.Input{PersonName: "Ada", Email: "ada@example.com"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.ExternalID != "u-99" || res.Secret != "sw_REUSED" {
		t.Errorf("did not reconcile to existing user: %+v", res)
	}
}

func TestProvision_APIErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"boom"}}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Token: "sw_admin"})
	if _, err := c.Provision(context.Background(), connector.Input{PersonName: "Ada"}); err == nil {
		t.Fatal("expected error")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should surface API message, got %v", err)
	}
}

func TestProvision_InstanceRoleScopesAndProjects(t *testing.T) {
	var createBody, tokenBody map[string]any
	projectRoles := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/users":
			_ = json.Unmarshal(body, &createBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"u-5","name":"Mara"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/users/u-5/tokens":
			_ = json.Unmarshal(body, &tokenBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"sw_TOK"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[{"key":"AAA"},{"key":"IDEA"}]`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/projects/"):
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			key := strings.Split(r.URL.Path, "/")[3]
			projectRoles[key], _ = m["role"].(string)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"role":"` + projectRoles[key] + `"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Token: "sw_admin"})
	res, err := c.Provision(context.Background(), connector.Input{
		PersonName:   "Mara",
		Email:        "mara@example.com",
		InstanceRole: "owner",
		Scopes:       []string{"tickets:read", "webhooks:manage"},
		Projects: []connector.ProjectGrant{
			{Key: "*", Role: "viewer"},
			{Key: "IDEA", Role: "editor"},
		},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if createBody["instance_role"] != "owner" {
		t.Errorf("instance_role: want owner, got %v", createBody["instance_role"])
	}
	gotScopes, _ := tokenBody["scopes"].([]any)
	if len(gotScopes) != 2 || gotScopes[0] != "tickets:read" || gotScopes[1] != "webhooks:manage" {
		t.Errorf("scopes: want explicit list, got %v", tokenBody["scopes"])
	}
	if projectRoles["AAA"] != "viewer" {
		t.Errorf("AAA should get the wildcard viewer, got %q", projectRoles["AAA"])
	}
	if projectRoles["IDEA"] != "editor" {
		t.Errorf("IDEA override should win (editor), got %q", projectRoles["IDEA"])
	}
	if a := res.Extra["Access"]; !strings.Contains(a, "viewer") || !strings.Contains(a, "editor") {
		t.Errorf("access summary wrong: %q", a)
	}
}

func TestAssignOne_PatchFallbackWhenAlreadyMember(t *testing.T) {
	var patched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"already a member"}}`))
		case http.MethodPatch:
			patched = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"role":"editor"}`))
		}
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Token: "sw_admin"})
	if err := c.assignOne(context.Background(), "IDEA", "u-1", "editor"); err != nil {
		t.Fatalf("assignOne: %v", err)
	}
	if !patched {
		t.Error("expected PATCH fallback when POST returns 409")
	}
}
