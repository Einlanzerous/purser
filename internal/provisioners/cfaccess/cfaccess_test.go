package cfaccess

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

const (
	testAccount = "acct-1"
	testGroup   = "grp-members"
	testHost    = "argosy.zerogravity.industries"
)

// ─── harness ───────────────────────────────────────────────────────────────

// cfAPI is a fake Cloudflare Access API. It records what was sent so a test can
// assert on the request body — which is the only way to check the PUT-replacement
// behaviour that this package exists to get right.
type cfAPI struct {
	apps []map[string]any

	lastMethod string
	lastPath   string
	lastBody   map[string]any
	deleted    []string

	// deleteStatus, when non-zero, is returned by DELETE instead of success.
	deleteStatus int
	// listStatus, when non-zero, makes the list call fail.
	listStatus int
}

func (f *cfAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastMethod, f.lastPath = r.Method, r.URL.Path
		f.lastBody = nil
		if r.Body != nil {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				f.lastBody = body
			}
		}
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/access/apps"):
			if f.listStatus != 0 {
				w.WriteHeader(f.listStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": false,
					"errors":  []map[string]any{{"code": 10000, "message": "auth failure"}},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": f.apps})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access/apps"):
			created := map[string]any{"id": "app-new"}
			for k, v := range f.lastBody {
				created[k] = v
			}
			f.apps = append(f.apps, created)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": created})

		case r.Method == http.MethodPut:
			updated := map[string]any{"id": lastSegment(r.URL.Path)}
			for k, v := range f.lastBody {
				updated[k] = v
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": updated})

		case r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, lastSegment(r.URL.Path))
			if f.deleteStatus != 0 {
				w.WriteHeader(f.deleteStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": false,
					"errors":  []map[string]any{{"code": 12109, "message": "app not found"}},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})

		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func lastSegment(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[i+1:]
}

// logoServer serves one URL with the given status and content type.
//
// TLS, not plain http, because ServiceSpec.Validate refuses an http:// logo —
// the launcher loads it from the viewer's browser and blocks mixed content. The
// caller wires srv.Client() in as Config.LogoClient so the self-signed cert is
// trusted; nothing in the provisioner relaxes verification.
func logoServer(t *testing.T, status int, contentType string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("\x89PNG\r\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func gatedSpec(t *testing.T, logo string) spinup.ServiceSpec {
	t.Helper()
	spec, err := spinup.ServiceSpec{
		Key:         "argosy",
		DisplayName: "Argosy",
		Hostname:    testHost,
		Mode:        spinup.ModeTunnelled,
		Tunnel:      spinup.TunnelProd,
		Upstream:    "http://argosy:8096",
		Access:      spinup.AccessGated,
		LogoURL:     logo,
	}.Validate()
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return spec
}

func bookmarkSpec(t *testing.T) spinup.ServiceSpec {
	t.Helper()
	spec, err := spinup.ServiceSpec{
		Key:         "argosy",
		DisplayName: "Argosy",
		Hostname:    testHost,
		Mode:        spinup.ModeDirect,
		Upstream:    "100.64.0.7",
		Access:      spinup.AccessBookmark,
	}.Validate()
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return spec
}

func newProv(t *testing.T, api *cfAPI, cfg Config) *Provisioner {
	t.Helper()
	srv := api.server(t)
	if cfg.APIToken == "" {
		cfg.APIToken = "tok"
	}
	if cfg.AccountID == "" {
		cfg.AccountID = testAccount
	}
	return newWithBase(t, srv.URL, cfg)
}

// ─── availability ──────────────────────────────────────────────────────────

func TestInspect_UnconfiguredIsUnavailable(t *testing.T) {
	p := newProv(t, &cfAPI{}, Config{APIToken: " ", GroupID: testGroup})
	p.cfg.APIToken = ""

	_, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, "")})
	if !spinup.IsUnavailable(err) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

// A gated spec needs a group id; a bookmark does not. Getting this wrong the
// lenient way would create a self_hosted app with no policy.
func TestAvailability_GatedNeedsGroupBookmarkDoesNot(t *testing.T) {
	p := newProv(t, &cfAPI{}, Config{}) // no GroupID

	if err := p.available(gatedSpec(t, "")); !spinup.IsUnavailable(err) {
		t.Fatalf("gated spec without a group id should be unavailable, got %v", err)
	}
	if err := p.available(bookmarkSpec(t)); err != nil {
		t.Fatalf("bookmark spec should not need a group id, got %v", err)
	}
}

// A failed read is never "there is nothing there" — the orchestrator turns the
// error into StepUnknown, and acting on a state that could not be read is how a
// spin-up creates a second copy of something.
func TestInspect_ReadFailureIsAnErrorNotAbsence(t *testing.T) {
	api := &cfAPI{listStatus: http.StatusForbidden}
	p := newProv(t, api, Config{GroupID: testGroup})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, "")})
	if err == nil {
		t.Fatal("want an error when the list call fails")
	}
	if st.Exists {
		t.Fatal("a failed read must not report Exists")
	}
	if spinup.IsUnavailable(err) {
		t.Fatal("an API failure is not the unavailable sentinel — it is a real failure")
	}
}

// ─── inspect ───────────────────────────────────────────────────────────────

func TestInspect_NoApplication(t *testing.T) {
	p := newProv(t, &cfAPI{}, Config{GroupID: testGroup})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, "")})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if st.Exists {
		t.Fatal("want Exists false")
	}
}

func TestInspect_GatedMatching(t *testing.T) {
	api := &cfAPI{apps: []map[string]any{{
		"id": "app-1", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		"app_launcher_visible": true,
		"policies": []any{map[string]any{
			"decision": "allow",
			"include":  []any{map[string]any{"group": map[string]any{"id": testGroup}}},
		}},
	}}}
	p := newProv(t, api, Config{GroupID: testGroup})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, "")})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !st.Exists || !st.Matches {
		t.Fatalf("want exists+matches, got %+v", st)
	}
	if st.ExternalID != "app-1" {
		t.Fatalf("external id = %q", st.ExternalID)
	}
	if st.ParentID != testAccount {
		t.Fatalf("parent id = %q, want the account", st.ParentID)
	}
}

// The failure that matters most: an app that exists and looks right but whose
// policy does not admit the members group is a gate in name only.
func TestInspect_GatedWithoutMembersPolicyDoesNotMatch(t *testing.T) {
	api := &cfAPI{apps: []map[string]any{{
		"id": "app-1", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		"app_launcher_visible": true,
		"policies":             []any{},
	}}}
	p := newProv(t, api, Config{GroupID: testGroup, GroupName: "zerogravity-members"})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, "")})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if st.Matches {
		t.Fatal("an app with no members policy must not match")
	}
	if !strings.Contains(st.Detail, "members group") {
		t.Fatalf("detail should name the missing policy, got %q", st.Detail)
	}
}

// A bookmark's domain carries a scheme; a self_hosted app's does not. Matching
// compares the host so the same hostname is recognised either way.
func TestInspect_BookmarkDomainWithSchemeMatchesHostname(t *testing.T) {
	api := &cfAPI{apps: []map[string]any{{
		"id": "app-b", "type": "bookmark", "name": "Argosy",
		"domain": "https://" + testHost, "app_launcher_visible": true,
		"policies": []any{},
	}}}
	p := newProv(t, api, Config{})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: bookmarkSpec(t)})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !st.Exists || !st.Matches {
		t.Fatalf("want exists+matches for the bookmark, got %+v (%s)", st, st.Detail)
	}
}

// The argosy case: the stored URL is exactly what the spec asks for, and it
// 404s. A string comparison alone reports "ok" for an app showing grey initials.
func TestInspect_LogoUrlMatchesButDoesNotLoad(t *testing.T) {
	dead := logoServer(t, http.StatusNotFound, "text/plain")
	api := &cfAPI{apps: []map[string]any{{
		"id": "app-1", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		"app_launcher_visible": true, "logo_url": dead.URL,
		"policies": []any{map[string]any{
			"decision": "allow",
			"include":  []any{map[string]any{"group": map[string]any{"id": testGroup}}},
		}},
	}}}
	p := newProv(t, api, Config{GroupID: testGroup, LogoClient: dead.Client()})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, dead.URL)})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if st.Matches {
		t.Fatal("a logo that does not load is drift, not a match")
	}
	if !strings.Contains(st.Detail, "does not load") {
		t.Fatalf("detail should say the logo does not load, got %q", st.Detail)
	}
}

// An asset behind an Access gate answers 200 with an HTML login page. A
// status-only check would pass it, and the launcher would show initials.
func TestVerifyLogo_RejectsHTMLLoginPage(t *testing.T) {
	gated := logoServer(t, http.StatusOK, "text/html; charset=utf-8")
	p := New(Config{LogoClient: gated.Client()})

	err := p.verifyLogo(context.Background(), gated.URL)
	if err == nil {
		t.Fatal("an HTML body must not pass as an image")
	}
	if !strings.Contains(err.Error(), "not an image") {
		t.Fatalf("error should name the content type, got %v", err)
	}
}

// ─── ensure ────────────────────────────────────────────────────────────────

func TestEnsure_CreatesGatedAppWithPolicyAndLogo(t *testing.T) {
	logo := logoServer(t, http.StatusOK, "image/png")
	api := &cfAPI{}
	p := newProv(t, api, Config{GroupID: testGroup, GroupName: "zerogravity-members", LogoClient: logo.Client()})

	res, err := p.Ensure(context.Background(), spinup.Target{Spec: gatedSpec(t, logo.URL)})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.ExternalID != "app-new" {
		t.Fatalf("external id = %q", res.ExternalID)
	}
	if got := api.lastBody["type"]; got != "self_hosted" {
		t.Fatalf("type = %v", got)
	}
	if got := api.lastBody["domain"]; got != testHost {
		t.Fatalf("a self_hosted domain is a bare hostname, got %v", got)
	}
	if got := api.lastBody["logo_url"]; got != logo.URL {
		t.Fatalf("logo_url = %v, want the verified url", got)
	}
	policies, _ := api.lastBody["policies"].([]any)
	if len(policies) != 1 {
		t.Fatalf("want one policy, got %v", api.lastBody["policies"])
	}
	if !allowsGroup(rawApp(api.lastBody), testGroup) {
		t.Fatalf("the created policy must admit the members group, got %v", policies)
	}
}

// A logo that will not load is omitted rather than written, and the report says
// so — refusing the whole application over an icon would hold back DNS and leave
// the service unpublished.
func TestEnsure_UnverifiableLogoIsOmittedNotFatal(t *testing.T) {
	dead := logoServer(t, http.StatusNotFound, "text/plain")
	api := &cfAPI{}
	p := newProv(t, api, Config{GroupID: testGroup, LogoClient: dead.Client()})

	res, err := p.Ensure(context.Background(), spinup.Target{Spec: gatedSpec(t, dead.URL)})
	if err != nil {
		t.Fatalf("an unverifiable logo must not fail the step: %v", err)
	}
	if got := api.lastBody["logo_url"]; got != "" {
		t.Fatalf("logo_url = %v, want it omitted", got)
	}
	if !strings.Contains(res.Detail, "logo omitted") {
		t.Fatalf("the report must say the logo was dropped, got %q", res.Detail)
	}
	if !allowsGroup(rawApp(api.lastBody), testGroup) {
		t.Fatal("the gate must still have been created")
	}
}

// The whole reason rawApp is a map. An update must carry through fields this
// package never modelled — on a gated app the one most likely to be dropped is
// the very thing that gates it.
func TestEnsure_UpdatePreservesUnmodelledFieldsAndStripsServerOwned(t *testing.T) {
	logo := logoServer(t, http.StatusOK, "image/png")
	api := &cfAPI{apps: []map[string]any{{
		"id": "app-1", "uid": "u-1", "aud": "aud-1",
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z",
		"type": "self_hosted", "name": "Old Name", "domain": testHost,
		"app_launcher_visible": true,
		"session_duration":     "24h",
		"custom_deny_message":  "go away",
		"policies":             []any{},
	}}}
	p := newProv(t, api, Config{GroupID: testGroup, LogoClient: logo.Client()})

	if _, err := p.Ensure(context.Background(), spinup.Target{Spec: gatedSpec(t, logo.URL)}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if api.lastMethod != http.MethodPut {
		t.Fatalf("an existing app must be updated with PUT, got %s", api.lastMethod)
	}
	for _, k := range serverOwned {
		if _, present := api.lastBody[k]; present {
			t.Fatalf("server-owned field %q must be stripped before PUT", k)
		}
	}
	if got := api.lastBody["session_duration"]; got != "24h" {
		t.Fatalf("session_duration was dropped (%v) — a PUT is a full replacement", got)
	}
	if got := api.lastBody["custom_deny_message"]; got != "go away" {
		t.Fatalf("an unmodelled field was dropped (%v)", got)
	}
	if got := api.lastBody["name"]; got != "Argosy" {
		t.Fatalf("name should have been updated to the spec's, got %v", got)
	}
}

func TestEnsure_BookmarkGetsSchemeDomainAndNoPolicy(t *testing.T) {
	api := &cfAPI{}
	p := newProv(t, api, Config{})

	if _, err := p.Ensure(context.Background(), spinup.Target{Spec: bookmarkSpec(t)}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := api.lastBody["type"]; got != "bookmark" {
		t.Fatalf("type = %v", got)
	}
	if got := api.lastBody["domain"]; got != "https://"+testHost {
		t.Fatalf("a bookmark's domain carries the scheme, got %v", got)
	}
	policies, ok := api.lastBody["policies"].([]any)
	if !ok || len(policies) != 0 {
		t.Fatalf("a bookmark must be sent with an explicitly empty policy list, got %v", api.lastBody["policies"])
	}
}

// ─── teardown ──────────────────────────────────────────────────────────────

func TestTeardown_DeletesRecordedID(t *testing.T) {
	api := &cfAPI{}
	p := newProv(t, api, Config{GroupID: testGroup})

	err := p.Teardown(context.Background(), spinup.Target{Spec: gatedSpec(t, "")},
		model.ServiceResource{ExternalID: "app-1"})
	if err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != "app-1" {
		t.Fatalf("deleted = %v, want the recorded id", api.deleted)
	}
}

func TestTeardown_RefusesWithoutARecordedID(t *testing.T) {
	api := &cfAPI{}
	p := newProv(t, api, Config{GroupID: testGroup})

	err := p.Teardown(context.Background(), spinup.Target{Spec: gatedSpec(t, "")},
		model.ServiceResource{})
	if err == nil {
		t.Fatal("want a refusal rather than a guess by hostname")
	}
	if len(api.deleted) != 0 {
		t.Fatalf("nothing should have been deleted, got %v", api.deleted)
	}
}

// Already gone is success — but only once we have checked that nothing else is
// still serving the hostname.
func TestTeardown_AlreadyGoneIsSuccess(t *testing.T) {
	api := &cfAPI{deleteStatus: http.StatusNotFound}
	p := newProv(t, api, Config{GroupID: testGroup})

	err := p.Teardown(context.Background(), spinup.Target{Spec: gatedSpec(t, "")},
		model.ServiceResource{ExternalID: "app-gone"})
	if err != nil {
		t.Fatalf("an app already deleted should be a success: %v", err)
	}
}

// A 404 against a recorded id with a live app still on the hostname means the
// record is wrong, not that access is gone. Reporting success there would leave
// a gate standing while Purser recorded it removed.
func TestTeardown_StaleRecordWithLiveAppIsAnError(t *testing.T) {
	api := &cfAPI{
		deleteStatus: http.StatusNotFound,
		apps: []map[string]any{{
			"id": "app-real", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		}},
	}
	p := newProv(t, api, Config{GroupID: testGroup})

	err := p.Teardown(context.Background(), spinup.Target{Spec: gatedSpec(t, "")},
		model.ServiceResource{ExternalID: "app-stale"})
	if err == nil {
		t.Fatal("a stale record over a live application must not report success")
	}
	if !strings.Contains(err.Error(), "app-real") {
		t.Fatalf("the error should name the application still serving, got %v", err)
	}
}

// ─── interface conformance ─────────────────────────────────────────────────

func TestImplementsServiceProvisioner(t *testing.T) {
	var _ spinup.ServiceProvisioner = New(Config{})

	if got := New(Config{}).Kind(); got != model.ResourceAccessApp {
		t.Fatalf("kind = %q", got)
	}
	// The registry panics on a kind outside model.KindOrder, so this is also the
	// check that the provisioner can actually be wired.
	spinup.NewRegistry(New(Config{}))
}
