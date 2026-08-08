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

// Reconcile must find the user by lookup and stop — never minting a token.
// This is the property that makes a Switchyard backfill safe (PRSR-15):
// Provision would hand someone who already has access a second API token.
func TestReconcile_FindsUserWithoutMintingAToken(t *testing.T) {
	var mintCalls, createCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/users":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"items":[{"id":"u-42","name":"Ada","email":"ada@example.com"}],"page":{"next_cursor":null}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tokens"):
			mintCalls++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"t-1","token":"sw_SHOULD_NOT_HAPPEN"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/users":
			createCalls++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"u-99","name":"Ada"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Token: "sw_admin"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Reconcile(context.Background(), connector.Input{PersonName: "Ada", Email: "ada@example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Exists || res.ExternalID != "u-42" || res.Username != "Ada" {
		t.Errorf("unexpected reconcile result: %+v", res)
	}
	if mintCalls != 0 {
		t.Errorf("Reconcile minted %d token(s) — it must never mint", mintCalls)
	}
	if createCalls != 0 {
		t.Errorf("Reconcile created %d user(s) — it must never create", createCalls)
	}
}

func TestReconcile_ReportsAbsentUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Reconcile must only read, got %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[],"page":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Token: "sw_admin"})
	res, err := c.Reconcile(context.Background(), connector.Input{PersonName: "Ada", Email: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exists {
		t.Error("an empty user list should report Exists=false")
	}
}

// A person with no address is unanswerable, not absent and not present
// (PRSR-23). findUser falls back to matching on display name, so answering at
// all would bind this person to whoever upstream happens to share their name —
// or, on a miss, mark a real account stale and re-arm provisioning for a second
// token. The audit turns an error into "unverifiable" and writes nothing, which
// is the whole reason to fail rather than guess.
//
// Only rows predating the required --email can reach this; the guard is what
// keeps `reconcile --all` safe to run with them in the roster.
func TestReconcile_RefusesWithoutAnEmailRatherThanMatchingOnName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no address to look up, so the upstream must not be called at all: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[{"id":"u-7","name":"Ada","email":"someone.else@example.com"}],"page":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Token: "sw_admin"})
	res, err := c.Reconcile(context.Background(), connector.Input{PersonName: "Ada"})
	if err == nil {
		t.Fatal("want an error for an emailless reconcile, got none")
	}
	if res.Exists {
		t.Error("a refusal must not also claim the account exists")
	}
}

// Deprovision revokes every live token and leaves the user standing. Deleting
// the user would take their authored tickets and comments with it, which is far
// more than "this person should not have access" asks for (PRSR-17).
func TestDeprovision_RevokesLiveTokensAndKeepsTheUser(t *testing.T) {
	var revoked []string
	var listCalls, userDeletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/users/u-42/tokens":
			listCalls++
			w.WriteHeader(http.StatusOK)
			// One already revoked: it must not be revoked again.
			_, _ = w.Write([]byte(`{"items":[
				{"id":"t-1","name":"purser-provisioned","revoked_at":null},
				{"id":"t-2","name":"laptop","revoked_at":"2026-01-01T00:00:00Z"},
				{"id":"t-3","name":"cli","revoked_at":null}]}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/users/u-42/tokens/"):
			revoked = append(revoked, strings.TrimPrefix(r.URL.Path, "/v1/users/u-42/tokens/"))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/users/u-42":
			userDeletes++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL, Token: "sw_admin"})
	if err != nil {
		t.Fatal(err)
	}
	// ExternalID present, so no user lookup is needed at all.
	if err := c.Deprovision(context.Background(), connector.Input{
		PersonName: "Ada", Email: "ada@example.com", ExternalID: "u-42",
	}); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	if len(revoked) != 2 || revoked[0] != "t-1" || revoked[1] != "t-3" {
		t.Errorf("revoked = %v, want the two live tokens only", revoked)
	}
	if userDeletes != 0 {
		t.Errorf("Deprovision deleted the user %d time(s) — it must only revoke", userDeletes)
	}
	if listCalls != 1 {
		t.Errorf("listCalls = %d, want 1", listCalls)
	}
}

// A person with nothing left to revoke is a success, so a failed-only retry and
// a second offboard are both safe.
func TestDeprovision_IsIdempotentAndTolerantOfAMissingUser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/users":
			// Nobody upstream matches.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"items":[],"page":{"next_cursor":null}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/users/u-9/tokens":
			// Every token already revoked.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"items":[{"id":"t-1","revoked_at":"2026-01-01T00:00:00Z"}]}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Token: "sw_admin"})
	// No user upstream at all — nothing to take away.
	if err := c.Deprovision(context.Background(), connector.Input{
		PersonName: "Ada", Email: "ada@example.com",
	}); err != nil {
		t.Errorf("a missing user should be success, got %v", err)
	}
	// A user whose tokens are already revoked — likewise.
	if err := c.Deprovision(context.Background(), connector.Input{
		PersonName: "Ada", Email: "ada@example.com", ExternalID: "u-9",
	}); err != nil {
		t.Errorf("an already-revoked user should be success, got %v", err)
	}
}

// Without an ExternalID the connector must look the person up by email — and
// findUser falls back to display-name matching when there is no address, which
// on this path would revoke a same-named stranger's tokens.
func TestDeprovision_RefusesAnEmaillessPerson(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("nothing should be called: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL, Token: "sw_admin"})
	if err := c.Deprovision(context.Background(), connector.Input{PersonName: "Ada"}); err == nil {
		t.Error("an emailless deprovision must be refused, not guessed at")
	}
}
