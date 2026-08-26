package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

const (
	testAccount = "acct-1"
	testGroup   = "grp-members"
	testHost    = "argosy.zerogravity.industries"

	// unreachableURL is https (so ServiceSpec.Validate accepts it) and points at
	// a port nothing listens on, so a fetch fails at the transport layer. That
	// is the "could not check" case, as distinct from a 404.
	unreachableURL = "https://127.0.0.1:1/mark.png"
)

// ─── harness ───────────────────────────────────────────────────────────────

// accessAPI is a fake Cloudflare Access API. It records what was sent so a test can
// assert on the request body — which is the only way to check the PUT-replacement
// behaviour that this package exists to get right.
type accessAPI struct {
	apps []map[string]any

	lastMethod string
	lastPath   string
	lastBody   map[string]any
	deleted    []string

	// deleteStatus, when non-zero, is returned by DELETE instead of success.
	deleteStatus int
	// listStatus, when non-zero, makes the list call fail.
	listStatus int
	// perPage, when non-zero, makes the list endpoint paginate and emit a
	// result_info envelope. Zero keeps the old single-response behaviour, which
	// is the shape of an endpoint that does not paginate at all.
	perPage int
	// listPages counts how many list requests were served, so a test can show
	// that more than one page was actually read.
	listPages int
}

func (f *accessAPI) server(t *testing.T) *httptest.Server {
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
			f.listPages++
			if f.perPage <= 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": f.apps})
				return
			}
			page := 1
			if v := r.URL.Query().Get("page"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					page = n
				}
			}
			total := (len(f.apps) + f.perPage - 1) / f.perPage
			if total < 1 {
				total = 1
			}
			start := (page - 1) * f.perPage
			end := start + f.perPage
			if start > len(f.apps) {
				start = len(f.apps)
			}
			if end > len(f.apps) {
				end = len(f.apps)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success":     true,
				"result":      f.apps[start:end],
				"result_info": map[string]any{"page": page, "total_pages": total},
			})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access/apps"):
			created := map[string]any{"id": "app-new"}
			for k, v := range f.lastBody {
				created[k] = v
			}
			f.apps = append(f.apps, created)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": created})

		case r.Method == http.MethodPut:
			updated := map[string]any{"id": lastPathSegment(r.URL.Path)}
			for k, v := range f.lastBody {
				updated[k] = v
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": updated})

		case r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, lastPathSegment(r.URL.Path))
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

func lastPathSegment(p string) string {
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

func newAccessProv(t *testing.T, api *accessAPI, cfg AccessConfig) *AccessProvisioner {
	t.Helper()
	srv := api.server(t)
	if cfg.APIToken == "" {
		cfg.APIToken = "tok"
	}
	if cfg.AccountID == "" {
		cfg.AccountID = testAccount
	}
	return newAccessWithBase(t, srv.URL, cfg)
}

// ─── availability ──────────────────────────────────────────────────────────

func TestInspect_UnconfiguredIsUnavailable(t *testing.T) {
	p := newAccessProv(t, &accessAPI{}, AccessConfig{APIToken: " ", GroupID: testGroup})
	p.cfg.APIToken = ""

	_, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, "")})
	if !spinup.IsUnavailable(err) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

// A gated spec needs a group id; a bookmark does not. Getting this wrong the
// lenient way would create a self_hosted app with no policy.
func TestAvailability_GatedNeedsGroupBookmarkDoesNot(t *testing.T) {
	p := newAccessProv(t, &accessAPI{}, AccessConfig{}) // no GroupID

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
	api := &accessAPI{listStatus: http.StatusForbidden}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup})

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
	p := newAccessProv(t, &accessAPI{}, AccessConfig{GroupID: testGroup})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, "")})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if st.Exists {
		t.Fatal("want Exists false")
	}
}

// liveGatedApp is a gated application in the shape the live API actually
// returns — read off the estate on 2026-08-26 (PRSR-38, extended by PRSR-40),
// reduced only in its ids so the existing assertions still address "app-1".
//
// Ten of its keys are modelled nowhere in this package: self_hosted_domains,
// destinations, allowed_idps, tags, auto_redirect_to_identity,
// session_duration, enable_binding_cookie, http_only_cookie_attribute,
// options_preflight_bypass, eager_redirect_cookie_setting. An update is a
// full-replacement PUT, so each one is a setting a struct round-trip would
// silently delete. session_duration is the legible example — dropping it does
// not error, it resets how long a person stays signed in.
//
// destinations and eager_redirect_cookie_setting were the two this fixture
// missed until PRSR-40 went looking, and every self_hosted application on the
// estate carries both. destinations is the one with teeth: it is the modern
// spelling of what the application sits in front of, so a body that keeps
// `domain` and drops it is not obviously wrong to read and is a gate pointed at
// nothing. Purser never edits a live application's hostname — findApp matches on
// it, so a changed hostname is a create — but the fixture should model what
// upstream sends rather than what this package happens to touch. That is the
// whole PRSR-38 lesson, and it recurred here in the same shape.
//
// The policy is the estate's real one: reusable, shared across six
// applications, admitting the members group *and* an email domain. The bare
// decision/include pair that stood here before could not show that appending to
// this list means handing Cloudflare back somebody else's shared policy — which
// PRSR-40 then measured: a reusable policy in an application write is read as a
// *reference*, id only, and its body is ignored. See
// TestEnsure_AReusablePolicyIsCarriedByReferenceNotRewritten.
func liveGatedApp(logo string) map[string]any {
	return map[string]any{
		"id":                  "app-1",
		"uid":                 "app-1",
		"aud":                 "d3404fc362067f48ff1fd6c9a7fc9a1fd723510c2681feed15e35159649963de",
		"type":                "self_hosted",
		"name":                "Argosy",
		"created_at":          "2026-07-05T23:55:28Z",
		"updated_at":          "2026-07-20T04:32:19Z",
		"domain":              testHost,
		"self_hosted_domains": []any{testHost},
		"destinations": []any{
			map[string]any{"type": "public", "uri": testHost},
		},
		"app_launcher_visible":          true,
		"logo_url":                      logo,
		"allowed_idps":                  []any{},
		"tags":                          []any{},
		"auto_redirect_to_identity":     false,
		"session_duration":              "730h",
		"enable_binding_cookie":         false,
		"http_only_cookie_attribute":    false,
		"options_preflight_bypass":      false,
		"eager_redirect_cookie_setting": true,
		"policies": []any{map[string]any{
			"id":         "e9054499-3680-40e3-a03b-96e8eff3f3e5",
			"uid":        "e9054499-3680-40e3-a03b-96e8eff3f3e5",
			"name":       "Standard",
			"decision":   "allow",
			"precedence": float64(1),
			"reusable":   true,
			"created_at": "2026-07-06T01:45:17Z",
			"updated_at": "2026-07-20T16:09:05Z",
			"exclude":    []any{},
			"require":    []any{},
			"include": []any{
				map[string]any{"email_domain": map[string]any{"domain": "dodson.mozmail.com"}},
				map[string]any{"group": map[string]any{"id": testGroup}},
			},
		}},
	}
}

func TestInspect_GatedMatching(t *testing.T) {
	api := &accessAPI{apps: []map[string]any{liveGatedApp("")}}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup})

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
	api := &accessAPI{apps: []map[string]any{{
		"id": "app-1", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		"app_launcher_visible": true,
		"policies":             []any{},
	}}}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, GroupName: "zerogravity-members"})

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
	api := &accessAPI{apps: []map[string]any{{
		"id": "app-b", "type": "bookmark", "name": "Argosy",
		"domain": "https://" + testHost, "app_launcher_visible": true,
		"policies": []any{},
	}}}
	p := newAccessProv(t, api, AccessConfig{})

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
	api := &accessAPI{apps: []map[string]any{liveGatedApp(dead.URL)}}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, LogoClient: dead.Client()})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, dead.URL)})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if st.Matches {
		t.Fatal("a logo that does not load is drift, not a match")
	}
	if !strings.Contains(st.Detail, "not a servable image") {
		t.Fatalf("detail should say the logo does not serve, got %q", st.Detail)
	}
}

// An asset behind an Access gate answers 200 with an HTML login page. A
// status-only check would pass it, and the launcher would show initials.
func TestCheckLogo_HTMLLoginPageIsBroken(t *testing.T) {
	gated := logoServer(t, http.StatusOK, "text/html; charset=utf-8")
	p := NewAccess(AccessConfig{LogoClient: gated.Client()})

	verdict, err := p.checkLogo(context.Background(), gated.URL)
	if verdict != logoBroken {
		t.Fatalf("an HTML body must not pass as an image, got verdict %v", verdict)
	}
	if !strings.Contains(err.Error(), "not an image") {
		t.Fatalf("error should name the content type, got %v", err)
	}
}

// The distinction the whole logo section turns on: a definite bad answer is
// actionable, a check that could not complete is not.
func TestCheckLogo_SeparatesBrokenFromUnknown(t *testing.T) {
	notFound := logoServer(t, http.StatusNotFound, "text/plain")
	serverErr := logoServer(t, http.StatusBadGateway, "text/plain")
	ok := logoServer(t, http.StatusOK, "image/png")

	cases := []struct {
		name string
		url  string
		cl   *http.Client
		want logoVerdict
	}{
		{"200 image", ok.URL, ok.Client(), logoOK},
		{"404 is a fact about the asset", notFound.URL, notFound.Client(), logoBroken},
		{"5xx is the origin, not the asset", serverErr.URL, serverErr.Client(), logoUnknown},
		{"connection refused", unreachableURL, nil, logoUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewAccess(AccessConfig{LogoClient: tc.cl})
			if got, _ := p.checkLogo(context.Background(), tc.url); got != tc.want {
				t.Fatalf("verdict = %v, want %v", got, tc.want)
			}
		})
	}
}

// ─── ensure ────────────────────────────────────────────────────────────────

func TestEnsure_CreatesGatedAppWithPolicyAndLogo(t *testing.T) {
	logo := logoServer(t, http.StatusOK, "image/png")
	api := &accessAPI{}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, GroupName: "zerogravity-members", LogoClient: logo.Client()})

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
	if groupPolicy(rawApp(api.lastBody), testGroup) != policyAdmitsGroup {
		t.Fatalf("the created policy must admit the members group, got %v", policies)
	}
}

// A logo that is definitely broken (404) is omitted rather than written, and the
// report says so — refusing the whole application over an icon would hold back
// DNS and leave the service unpublished.
func TestEnsure_BrokenLogoIsOmittedNotFatal(t *testing.T) {
	dead := logoServer(t, http.StatusNotFound, "text/plain")
	api := &accessAPI{}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, LogoClient: dead.Client()})

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
	if groupPolicy(rawApp(api.lastBody), testGroup) != policyAdmitsGroup {
		t.Fatal("the gate must still have been created")
	}
}

// The whole reason rawApp is a map. An update must carry through fields this
// package never modelled — on a gated app the one most likely to be dropped is
// the very thing that gates it.
func TestEnsure_UpdatePreservesUnmodelledFieldsAndStripsServerOwned(t *testing.T) {
	logo := logoServer(t, http.StatusOK, "image/png")
	api := &accessAPI{apps: []map[string]any{{
		"id": "app-1", "uid": "u-1", "aud": "aud-1",
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z",
		"type": "self_hosted", "name": "Old Name", "domain": testHost,
		"app_launcher_visible": true,
		"session_duration":     "24h",
		"custom_deny_message":  "go away",
		"policies":             []any{},
	}}}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, LogoClient: logo.Client()})

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
	api := &accessAPI{}
	p := newAccessProv(t, api, AccessConfig{})

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

// A check that could not complete must never clear a logo that is already set.
// Collapsing "could not check" into "broken" would erase a working icon every
// time the CDN blinked.
func TestEnsure_UncheckableLogoIsLeftAlone(t *testing.T) {
	api := &accessAPI{apps: []map[string]any{{
		"id": "app-1", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		"app_launcher_visible": true, "logo_url": unreachableURL,
		"policies": []any{},
	}}}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup})

	res, err := p.Ensure(context.Background(), spinup.Target{Spec: gatedSpec(t, unreachableURL)})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := api.lastBody["logo_url"]; got != unreachableURL {
		t.Fatalf("logo_url = %v, want the existing value kept rather than cleared", got)
	}
	if !strings.Contains(res.Detail, "left as it was") {
		t.Fatalf("the report should say the logo was left alone, got %q", res.Detail)
	}
}

// ...and it must not count as drift either. An update here is a
// full-replacement PUT of the whole application, which is not something to
// trigger on a failed fetch.
func TestInspect_UncheckableLogoIsANoteNotDrift(t *testing.T) {
	api := &accessAPI{apps: []map[string]any{{
		"id": "app-1", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		"app_launcher_visible": true, "logo_url": unreachableURL,
		"policies": []any{map[string]any{
			"decision": "allow",
			"include":  []any{map[string]any{"group": map[string]any{"id": testGroup}}},
		}},
	}}}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, unreachableURL)})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !st.Matches {
		t.Fatalf("a logo that could not be checked is not drift: %s", st.Detail)
	}
	if !strings.Contains(st.Detail, "could not be checked") {
		t.Fatalf("detail should still report the failed check, got %q", st.Detail)
	}
}

// Reading only the first page would make a miss look like an absence, and Ensure
// turns absence straight into a create — so an application on a later page would
// get a second one built on top of it, which is the opposite of the
// already-exists-is-success rule.
func TestFindApp_PaginatesRatherThanReportingAbsence(t *testing.T) {
	api := &accessAPI{perPage: 10}
	for i := 0; i < 25; i++ {
		api.apps = append(api.apps, map[string]any{
			"id": "other-" + strconv.Itoa(i), "type": "self_hosted",
			"name": "Other", "domain": "other-" + strconv.Itoa(i) + ".zerogravity.industries",
		})
	}
	// The one we are looking for sits on the third page.
	api.apps = append(api.apps, map[string]any{
		"id": "app-late", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		"app_launcher_visible": true,
		"policies": []any{map[string]any{
			"decision": "allow",
			"include":  []any{map[string]any{"group": map[string]any{"id": testGroup}}},
		}},
	})
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, "")})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !st.Exists {
		t.Fatal("an application on a later page must not be reported absent")
	}
	if st.ExternalID != "app-late" {
		t.Fatalf("external id = %q, want the app from the later page", st.ExternalID)
	}
	if api.listPages < 2 {
		t.Fatalf("only %d list request(s) were made — pagination was not exercised", api.listPages)
	}
}

// An endpoint that sends no result_info is one that does not paginate: read it
// once and stop, rather than looping on a page parameter it ignores.
func TestFindApp_NoResultInfoMeansOnePage(t *testing.T) {
	api := &accessAPI{apps: []map[string]any{{
		"id": "app-1", "type": "self_hosted", "name": "Argosy", "domain": testHost,
	}}}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup})

	if _, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, "")}); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if api.listPages != 1 {
		t.Fatalf("made %d list requests, want exactly 1", api.listPages)
	}
}

// The reviewer's scenario, and the one the round-2 test missed because its live
// app carried an empty policy list: a gated app that already admits the members
// group *and* something else. The only drift is a rotted logo, so the plan says
// "fix a logo" — and an assignment to `policies` would quietly remove the other
// policy while doing it.
func TestEnsure_UpdateKeepsPoliciesThatAreNotPursers(t *testing.T) {
	logo := logoServer(t, http.StatusOK, "image/png")
	monitor := map[string]any{
		"decision": "allow",
		"include":  []any{map[string]any{"service_token": map[string]any{"token_id": "uptime-monitor"}}},
	}
	api := &accessAPI{apps: []map[string]any{{
		"id": "app-1", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		"app_launcher_visible": true, "logo_url": "https://dead.example/mark.png",
		"policies": []any{
			map[string]any{
				"decision": "allow",
				"include":  []any{map[string]any{"group": map[string]any{"id": testGroup}}},
			},
			monitor,
		},
	}}}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, LogoClient: logo.Client()})

	if _, err := p.Ensure(context.Background(), spinup.Target{Spec: gatedSpec(t, logo.URL)}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	sent, _ := api.lastBody["policies"].([]any)
	if len(sent) != 2 {
		t.Fatalf("sent %d policies, want both kept: %v", len(sent), sent)
	}
	if groupPolicy(rawApp(api.lastBody), testGroup) != policyAdmitsGroup {
		t.Error("the members policy must survive")
	}
	var keptMonitor bool
	for _, raw := range sent {
		pol, _ := raw.(map[string]any)
		inc, _ := pol["include"].([]any)
		for _, r := range inc {
			if rule, _ := r.(map[string]any); rule != nil {
				if _, ok := rule["service_token"]; ok {
					keptMonitor = true
				}
			}
		}
	}
	if !keptMonitor {
		t.Error("the service-token policy was deleted by an update that only meant to fix a logo")
	}
}

// Missing the members policy is an append, not a replacement: the gate goes in
// and whatever else was there stays.
func TestEnsure_MissingGroupPolicyIsAppended(t *testing.T) {
	other := map[string]any{
		"decision": "allow",
		"include":  []any{map[string]any{"email": map[string]any{"email": "ops@example.com"}}},
	}
	api := &accessAPI{apps: []map[string]any{{
		"id": "app-1", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		"app_launcher_visible": true,
		"policies":             []any{other},
	}}}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup})

	if _, err := p.Ensure(context.Background(), spinup.Target{Spec: gatedSpec(t, "")}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	sent, _ := api.lastBody["policies"].([]any)
	if len(sent) != 2 {
		t.Fatalf("sent %d policies, want the existing one plus ours: %v", len(sent), sent)
	}
	if groupPolicy(rawApp(api.lastBody), testGroup) != policyAdmitsGroup {
		t.Error("the members policy should have been appended")
	}
}

// A policy list this cannot interpret is left exactly as it is, and reported as
// a note rather than as drift — rewriting a gate on the strength of not being
// able to read it is worse than not knowing.
func TestUnreadablePolicyListIsLeftAlone(t *testing.T) {
	live := []any{"pol-abc123", "pol-def456"} // bare references
	api := &accessAPI{apps: []map[string]any{{
		"id": "app-1", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		"app_launcher_visible": true,
		"policies":             live,
	}}}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, GroupName: "zerogravity-members"})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, "")})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !st.Matches {
		t.Fatalf("an unreadable policy list is not drift: %s", st.Detail)
	}
	if !strings.Contains(st.Detail, "could not be read") {
		t.Fatalf("detail should report the unverified policies, got %q", st.Detail)
	}

	if _, err := p.Ensure(context.Background(), spinup.Target{Spec: gatedSpec(t, "")}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	sent, _ := api.lastBody["policies"].([]any)
	if len(sent) != 2 {
		t.Fatalf("the reference list must be carried through untouched, got %v", sent)
	}
}

// ─── teardown ──────────────────────────────────────────────────────────────

func TestTeardown_DeletesRecordedID(t *testing.T) {
	api := &accessAPI{}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup})

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
	api := &accessAPI{}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup})

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
	api := &accessAPI{deleteStatus: http.StatusNotFound}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup})

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
	api := &accessAPI{
		deleteStatus: http.StatusNotFound,
		apps: []map[string]any{{
			"id": "app-real", "type": "self_hosted", "name": "Argosy", "domain": testHost,
		}},
	}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup})

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
	var _ spinup.ServiceProvisioner = NewAccess(AccessConfig{})

	if got := NewAccess(AccessConfig{}).Kind(); got != model.ResourceAccessApp {
		t.Fatalf("kind = %q", got)
	}
	// The registry panics on a kind outside model.KindOrder, so this is also the
	// check that the provisioner can actually be wired.
	spinup.NewRegistry(NewAccess(AccessConfig{}))
}

// The gated half of PRSR-38's fixture correction.
//
// TestEnsure_UpdatePreservesUnmodelledFieldsAndStripsServerOwned already asserts
// the principle, but against *invented* keys — a `custom_deny_message` somebody
// made up to stand for "a field we don't model". This runs the same assertion
// against the key set the live API was observed to return, so the suite pins
// what Cloudflare actually sends rather than what the author imagined it might.
// That distinction is the whole subject of PRSR-38: a fake built from the model
// asserts the model.
//
// The keys below are not decorative. `session_duration` is how long a person
// stays signed in, and a full-replacement PUT that omits it does not error — it
// silently resets it.
func TestEnsure_GatedUpdateCarriesTheObservedKeySetThrough(t *testing.T) {
	logo := logoServer(t, http.StatusOK, "image/png")
	api := &accessAPI{apps: []map[string]any{liveGatedApp("")}}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, LogoClient: logo.Client()})

	if _, err := p.Ensure(context.Background(), spinup.Target{Spec: gatedSpec(t, logo.URL)}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if api.lastMethod != http.MethodPut {
		t.Fatalf("an existing app is updated with PUT, got %s", api.lastMethod)
	}

	// Every key observed on the live application that this package models
	// nowhere. Each is a real setting; the PUT replaces the whole object.
	for k, want := range map[string]any{
		"session_duration":              "730h",
		"auto_redirect_to_identity":     false,
		"enable_binding_cookie":         false,
		"http_only_cookie_attribute":    false,
		"options_preflight_bypass":      false,
		"eager_redirect_cookie_setting": true,
	} {
		if got, ok := api.lastBody[k]; !ok || got != want {
			t.Errorf("%q was dropped or altered by the PUT: got %v (present=%v), want %v", k, got, ok, want)
		}
	}
	for _, k := range []string{"self_hosted_domains", "destinations", "allowed_idps", "tags"} {
		if _, ok := api.lastBody[k]; !ok {
			t.Errorf("%q was dropped by the full-replacement PUT — rawApp exists so unmodelled keys survive", k)
		}
	}
	for _, k := range serverOwned {
		if _, present := api.lastBody[k]; present {
			t.Errorf("server-owned field %q must be stripped before PUT", k)
		}
	}

	// The gate itself. The estate's policy is reusable and shared across six
	// applications, so it must go back exactly as it came: this app already
	// admits the members group, so nothing is appended, and nothing about
	// somebody else's shared policy is rewritten. PRSR-40 settled what Cloudflare
	// does with it — the body is ignored and only the id is read — which is why
	// the id assertion below is the load-bearing one.
	pols, ok := api.lastBody["policies"].([]any)
	if !ok || len(pols) != 1 {
		t.Fatalf("the app already admits the members group, so its policy list must be unchanged, got %v", api.lastBody["policies"])
	}
	pol, ok := pols[0].(map[string]any)
	if !ok {
		t.Fatalf("policy came back as %T", pols[0])
	}
	if pol["id"] != "e9054499-3680-40e3-a03b-96e8eff3f3e5" || pol["reusable"] != true {
		t.Errorf("the shared reusable policy was not carried through verbatim: %v", pol)
	}
	if inc, ok := pol["include"].([]any); !ok || len(inc) != 2 {
		t.Errorf("the policy's include list lost a rule — the email_domain grant is somebody's access: %v", pol["include"])
	}
	if got := api.lastBody["logo_url"]; got != logo.URL {
		t.Errorf("the drift this update exists to fix was not written: logo_url = %v", got)
	}
}

// A carried-through policy keeps its id, because on a reusable policy the id is
// the only field Cloudflare reads.
//
// PRSR-40 measured this against the live API on 2026-08-26, on a disposable
// application and a disposable reusable policy shared by two disposable apps.
// An application write carrying a `reusable: true` policy inline was accepted,
// and every field of that policy other than `id` was **ignored**: the probe sent
// `name: "MUTATED BY PROBE"` and `decision: "deny"`, Cloudflare answered 200 and
// echoed back the policy's real name and `decision: "allow"`, the standalone
// policy's updated_at did not move, and the second application sharing it was
// untouched. A reusable policy in an application body is a reference.
//
// That retires the outcome PRSR-40 was filed to rule out — one service's logo
// fix silently editing the gate on the six applications sharing the estate's
// `Standard` policy. It cannot happen, and the reason it cannot is the id.
//
// So the id is what this pins. Strip it — by adding "id" to serverOwned, say,
// which is exactly the kind of tidying that looks right at the callsite — and
// the policy stops being a reference and becomes an inline definition
// Cloudflare has never seen. It would create a *second* policy rather than
// reusing the shared one, and the application would end up gated by a private
// copy that no longer tracks the group everyone else's does. Nothing would
// error, and the plan would say "fix a logo".
//
// The non-reusable half of the same measurement is why this is not merely
// defensive: an app-scoped (`reusable: false`) policy's body **is** honoured on
// an application write. The probe flipped one to `decision: "deny"` and the
// read-back confirmed it. Purser only ever echoes back what it read moments
// earlier — Ensure takes its own fresh read immediately before desiredApp — so
// that is safe, but it does mean a gated update is a real write of the policy
// content and not a no-op.
func TestEnsure_AReusablePolicyIsCarriedByReferenceNotRewritten(t *testing.T) {
	// A reusable policy that does NOT admit the members group, so the append
	// branch is the one under test. Every application on the estate today
	// already admits the group, which is why this shape has to be built rather
	// than read: it is the state a *new* gated service arrives in.
	foreign := map[string]any{
		"id":         "033c9a30-8011-4362-b9f4-50adcdbc7206",
		"uid":        "033c9a30-8011-4362-b9f4-50adcdbc7206",
		"name":       "Standard",
		"decision":   "allow",
		"precedence": float64(1),
		"reusable":   true,
		"include": []any{
			map[string]any{"email_domain": map[string]any{"domain": "dodson.mozmail.com"}},
		},
	}

	cases := []struct {
		name     string
		policies []any
		wantLen  int
	}{
		{
			// Already gated: the list goes back untouched, ids and all.
			name:     "carried through",
			policies: liveGatedApp("")["policies"].([]any),
			wantLen:  1,
		},
		{
			// Not gated yet: membersPolicy is appended and the existing policy
			// must survive alongside it, still carrying its id.
			name:     "appended alongside",
			policies: []any{foreign},
			wantLen:  2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logo := logoServer(t, http.StatusOK, "image/png")
			app := liveGatedApp("")
			app["policies"] = tc.policies
			api := &accessAPI{apps: []map[string]any{app}}
			p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, LogoClient: logo.Client()})

			if _, err := p.Ensure(context.Background(), spinup.Target{Spec: gatedSpec(t, logo.URL)}); err != nil {
				t.Fatalf("ensure: %v", err)
			}

			pols, ok := api.lastBody["policies"].([]any)
			if !ok || len(pols) != tc.wantLen {
				t.Fatalf("policies = %v, want %d of them", api.lastBody["policies"], tc.wantLen)
			}
			// Every policy that arrived with an id must leave with it. The one
			// membersPolicy adds has none, and must not acquire one.
			for i, raw := range pols {
				pol, ok := raw.(map[string]any)
				if !ok {
					continue // a bare reference is already the reference form
				}
				id, _ := pol["id"].(string)
				isNew := pol["name"] == "Allow members"
				switch {
				case isNew && id != "":
					t.Errorf("the appended policy does not exist yet, so it must carry no id: %v", pol)
				case !isNew && id == "":
					t.Errorf("policy %d went back without its id; a reusable policy is matched on id alone, so this creates a duplicate private copy instead of reusing the shared one: %v", i, pol)
				}
			}
		})
	}
}

// The create path builds its policy from nothing, and that one has no id to
// keep — it is an inline definition, and Cloudflare turns it into an
// application-scoped policy.
//
// Observed on the live create (PRSR-40): POSTing an application whose body
// carried membersPolicy's exact output was accepted, and the response echoed
// the policy back expanded with a fresh id, `reusable: false`, `precedence: 1`,
// and empty `exclude`/`require` lists. It does not appear in
// /accounts/{a}/access/policies, which lists only the reusable ones. So a gated
// service Purser stands up gets its own private gate rather than joining the
// estate's shared `Standard` policy — which is the safe direction, and worth
// knowing rather than assuming.
func TestEnsure_CreateSendsTheInlinePolicyWithNoID(t *testing.T) {
	logo := logoServer(t, http.StatusOK, "image/png")
	api := &accessAPI{} // nothing there: the create path
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, LogoClient: logo.Client()})

	if _, err := p.Ensure(context.Background(), spinup.Target{Spec: gatedSpec(t, logo.URL)}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if api.lastMethod != http.MethodPost {
		t.Fatalf("a missing app is created with POST, got %s", api.lastMethod)
	}
	pols, ok := api.lastBody["policies"].([]any)
	if !ok || len(pols) != 1 {
		t.Fatalf("a gated create carries exactly its own policy, got %v", api.lastBody["policies"])
	}
	pol, ok := pols[0].(map[string]any)
	if !ok {
		t.Fatalf("policy came back as %T", pols[0])
	}
	if _, present := pol["id"]; present {
		t.Errorf("a created policy has no id yet — sending one names a policy that does not exist: %v", pol)
	}
	if pol["decision"] != "allow" {
		t.Errorf("decision = %v, want allow", pol["decision"])
	}
	inc, ok := pol["include"].([]any)
	if !ok || len(inc) != 1 {
		t.Fatalf("include = %v", pol["include"])
	}
	grp, _ := inc[0].(map[string]any)["group"].(map[string]any)
	if grp == nil || grp["id"] != testGroup {
		t.Errorf("the policy must admit the configured members group, got %v", inc[0])
	}
}
