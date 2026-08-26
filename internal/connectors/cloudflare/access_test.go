package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
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
		Logo:        spinup.LogoRef(logo),
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
// So the id is what this pins. Strip it and the policy stops being a reference
// and becomes an inline definition Cloudflare has never seen: it would create a
// *second* policy rather than reusing the shared one, and the application would
// end up gated by a private copy that no longer tracks the group everyone
// else's does. Nothing would error, and the plan would say "fix a logo".
//
// The lever that can do that is livePolicies, and desiredApp's append through
// it — not serverOwned, which already lists "id" and is applied only to the
// top-level application map, never walking into the policy objects inside it.
// The invitation is symmetry: a carried policy really does arrive carrying
// server-assigned created_at, updated_at and uid (see liveGatedApp), so the
// obvious tidy-up is a policy-level strip modelled on serverOwned — and "id"
// goes into that list with them, because at the callsite it looks like exactly
// the same kind of field.
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
		// wantID is the pre-existing policy that must still be in the body,
		// identified by id rather than merely counted.
		wantID string
	}{
		{
			// Already gated: the list goes back untouched, ids and all.
			//
			// wantID is the assertion doing the work here. Checking only that
			// *an* id survives is satisfied by a body holding nothing but
			// membersPolicy — which is the "assigned, never appended" failure
			// that would delete the estate's shared policy and replace it with
			// Purser's own, passing green.
			name:     "carried through",
			policies: liveGatedApp("")["policies"].([]any),
			wantLen:  1,
			wantID:   "e9054499-3680-40e3-a03b-96e8eff3f3e5",
		},
		{
			// Not gated yet: membersPolicy is appended and the existing policy
			// must survive alongside it, still carrying its id.
			name:     "appended alongside",
			policies: []any{foreign},
			wantLen:  2,
			wantID:   "033c9a30-8011-4362-b9f4-50adcdbc7206",
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
			// The policy that was already there must still be there, by id.
			found := false
			for _, raw := range pols {
				if pol, ok := raw.(map[string]any); ok && pol["id"] == tc.wantID {
					found = true
				}
			}
			if !found {
				t.Errorf("the pre-existing policy %s is not in the body — a gated update appends to the policy list, it never assigns over it: %v", tc.wantID, pols)
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

// A bookmark's empty policy list is accepted on write, not merely returned on
// read.
//
// Observed live (PRSR-40): a disposable bookmark application was created and then
// updated through this exact code. Cloudflare took `policies: []`, echoed it back
// as `[]`, kept the scheme on `domain`, preserved `tags`, and returned a much
// smaller object than a self_hosted app — no `destinations`, no
// `self_hosted_domains`, no `session_duration`.
//
// Worth its own test because the bookmark branch is a different body rather than
// a variant of the gated one, and because `policies` is the **one assignment** in
// desiredApp: everywhere else the rule is append-never-assign, and here it is
// inverted on purpose. A bookmark has no policies by definition, so a shape
// converted from gated must not keep its old gate, and there is nothing anyone
// could have added deliberately.
func TestEnsure_BookmarkAssignsAnEmptyPolicyListWhichCloudflareAccepts(t *testing.T) {
	logo := logoServer(t, http.StatusOK, "image/png")
	api := &accessAPI{}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, LogoClient: logo.Client()})

	spec := gatedSpec(t, logo.URL)
	spec.Access = spinup.AccessBookmark

	if _, err := p.Ensure(context.Background(), spinup.Target{Spec: spec}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if api.lastBody["type"] != "bookmark" {
		t.Errorf("type = %v, want bookmark", api.lastBody["type"])
	}
	// A bookmark's domain carries a scheme; a self_hosted app's does not.
	if got, want := api.lastBody["domain"], "https://"+testHost; got != want {
		t.Errorf("domain = %v, want %q", got, want)
	}
	pols, ok := api.lastBody["policies"].([]any)
	if !ok {
		t.Fatalf("a bookmark must be sent with an explicitly empty policy list, got %#v", api.lastBody["policies"])
	}
	if len(pols) != 0 {
		t.Errorf("a bookmark has no policies by definition, got %v", pols)
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

// ─── the Placard resolver (PRSR-37) ─────────────────────────────────────────

// fakeLogos answers all three of LogoResolver's outcomes without a server.
//
// Three, not two, is the whole point of the type: "Placard has no mark for this
// slug" and "Placard could not be asked" are different facts, and the second one
// treated as the first clears working icons across the estate every time the
// registry blinks.
type fakeLogos struct {
	marks map[string]string // key → canonical url
	err   error             // set to make every lookup unanswerable
	calls int
}

func (f *fakeLogos) Mark(_ context.Context, key string) (string, bool, error) {
	f.calls++
	if f.err != nil {
		return "", false, f.err
	}
	url, ok := f.marks[key]
	return url, ok, nil
}

// A spec that names no logo resolves one from Placard, and the resolved URL is
// still verified before it is written.
//
// Resolving is what makes the spec name a *service* rather than carry a CDN path
// somebody typed — the failure PRSR-38 measured, where switchyard's stored URL
// was a live 404 and argosy's resolved to the 3.6:1 wordmark instead of the tile
// mark. But Placard's own file `check` is a periodic monitor with a `checked_at`
// and can be stale, so it picks the URL and PRSR-29's write-time fetch is still
// what decides.
func TestEnsure_AnUnspecifiedLogoIsResolvedFromPlacard(t *testing.T) {
	logo := logoServer(t, http.StatusOK, "image/png")
	logos := &fakeLogos{marks: map[string]string{"argosy": logo.URL}}
	api := &accessAPI{}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, LogoClient: logo.Client(), Logos: logos})

	spec := gatedSpec(t, "")             // no --logo
	if spec.Logo != spinup.LogoPlacard { // Normalized's default
		t.Fatalf("an omitted logo should normalize to %q, got %q", spinup.LogoPlacard, spec.Logo)
	}
	if _, err := p.Ensure(context.Background(), spinup.Target{Spec: spec}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if logos.calls == 0 {
		t.Error("Placard was never asked")
	}
	if got := api.lastBody["logo_url"]; got != logo.URL {
		t.Errorf("logo_url = %v, want the resolved mark %q", got, logo.URL)
	}
}

// The two answers that must never clear an icon.
//
// Neither is drift and neither is a failed step: a gated Access application is a
// DNS prerequisite, so refusing one over an icon would leave a service
// unpublished. Both report a note and leave the tile exactly as it is.
func TestEnsure_AnUnresolvableLogoLeavesTheLiveOneAlone(t *testing.T) {
	const live = "https://cdn.example/existing-mark.png"

	cases := []struct {
		name  string
		logos LogoResolver
		note  string
	}{
		{
			// Placard answered and has nothing for this slug. Ordinary: its
			// registry covers seven services, chronicle is not one of them, and
			// a brand-new service is stood up before its mark is drawn.
			name:  "Placard has no mark for the slug",
			logos: &fakeLogos{marks: map[string]string{"somethingelse": "https://x/y.png"}},
			note:  "no mark",
		},
		{
			// Placard is down. Emphatically not evidence the icon is wrong.
			name:  "Placard could not be asked",
			logos: &fakeLogos{err: errors.New("connection refused")},
			note:  "could not be asked",
		},
		{
			// This deployment has no Placard at all.
			name:  "no resolver is configured",
			logos: nil,
			note:  "PURSER_PLACARD_URL",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logo := logoServer(t, http.StatusOK, "image/png")
			app := liveGatedApp(live)
			api := &accessAPI{apps: []map[string]any{app}}
			p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, LogoClient: logo.Client(), Logos: tc.logos})

			spec := gatedSpec(t, "")

			// The plan says so, and says it as a note rather than as drift: an
			// --apply will not touch the icon, and a plan that reported one
			// would be promising a change that will not happen.
			st, err := p.Inspect(context.Background(), spinup.Target{Spec: spec})
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			if !strings.Contains(st.Detail, tc.note) {
				t.Errorf("the plan should explain why no icon was resolved; %q missing from %q", tc.note, st.Detail)
			}
			if strings.Contains(st.Detail, "spec sets none") {
				t.Errorf("an unresolvable logo must not be reported as a spec that asked for none: %q", st.Detail)
			}

			if _, err := p.Ensure(context.Background(), spinup.Target{Spec: spec}); err != nil {
				t.Fatalf("ensure: %v", err)
			}
			if got := api.lastBody["logo_url"]; got != live {
				t.Errorf("logo_url = %v, want the live icon kept (%q) — an unreachable registry is not evidence the icon is wrong", got, live)
			}
		})
	}
}

// An explicit URL still wins, and Placard is not consulted at all.
//
// The escape hatch matters: a service whose mark Placard has not been given yet
// can still be pointed at one by hand, and a spec that names a URL is naming it
// on purpose.
func TestEnsure_AnExplicitLogoURLBypassesPlacard(t *testing.T) {
	logo := logoServer(t, http.StatusOK, "image/png")
	logos := &fakeLogos{marks: map[string]string{"argosy": "https://cdn.example/placard-would-say-this.png"}}
	api := &accessAPI{}
	p := newAccessProv(t, api, AccessConfig{GroupID: testGroup, LogoClient: logo.Client(), Logos: logos})

	if _, err := p.Ensure(context.Background(), spinup.Target{Spec: gatedSpec(t, logo.URL)}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if logos.calls != 0 {
		t.Errorf("a spec naming a url must not consult Placard, got %d calls", logos.calls)
	}
	if got := api.lastBody["logo_url"]; got != logo.URL {
		t.Errorf("logo_url = %v, want the url the spec named", got)
	}
}

// A rotted icon is still reported when nothing was resolved to replace it.
//
// The keep note says the launcher shows the service's initials. If a dead URL is
// configured that sentence is wrong and the dead URL is invisible — which is the
// condition switchyard's tile sat in for months, and exactly what this whole area
// exists to surface. chronicle is the live case: a gated application Placard has
// never heard of, so every run takes this branch.
//
// A note rather than drift, because nothing will be written either way. It
// restores the detector without promising an action.
func TestInspect_AnUnresolvedLogoStillReportsARottedLiveOne(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer dead.Close()

	api := &accessAPI{apps: []map[string]any{liveGatedApp(dead.URL)}}
	logos := &fakeLogos{} // Placard has nothing for this slug
	p := newAccessProv(t, api, AccessConfig{
		GroupID: testGroup, LogoClient: dead.Client(), Logos: logos,
	})

	st, err := p.Inspect(context.Background(), spinup.Target{Spec: gatedSpec(t, "")})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !strings.Contains(st.Detail, "not a servable image") {
		t.Errorf("a dead live logo must still be reported when nothing replaced it: %q", st.Detail)
	}
	// Still not drift: --apply will not touch it, and a plan that said otherwise
	// would promise a change that never comes.
	if !st.Matches {
		t.Errorf("nothing will be written, so this is a note rather than drift: %q", st.Detail)
	}
}

// A spec naming an icon that does not serve must not clear the working one that
// is already there, and the plan must not promise that it will set it.
//
// The destructive shape, before the fix: live tile carries a working icon A, the
// spec resolves to B, B 404s. logoDiff reported `logo is A, spec wants B` without
// ever fetching B — so the plan said `update` naming B — and resolveLogo's
// logoBroken case discarded `current` and returned "", which desiredApp writes as
// an empty logo_url and PRSR-40 confirmed live really does remove the icon. Net:
// the tile ended with no icon and the plan had promised the opposite.
//
// logoUnknown two cases below was already making the opposite choice on the same
// question, which is the tell: "we could not check it" and "we checked and it is
// dead" want the same answer whenever there is something to lose.
//
// Reachable rather than theoretical: Placard reports a file `in_repo` from the
// repo's own contents, and jsDelivr serving a 404 for it — propagation lag after
// a rename is the obvious way — is a definite non-image answer, not a transport
// failure, so it lands on logoBroken and not logoUnknown.
func TestEnsure_ABrokenSpecLogoDoesNotClearTheWorkingLiveOne(t *testing.T) {
	const live = "https://cdn.example/working-mark.png"

	// Serves the live icon, 404s the one the spec asks for.
	const broken = "/broken-mark.png"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == broken {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	app := liveGatedApp(live)
	api := &accessAPI{apps: []map[string]any{app}}
	logos := &fakeLogos{marks: map[string]string{"argosy": srv.URL + broken}}
	p := newAccessProv(t, api, AccessConfig{
		GroupID: testGroup, LogoClient: srv.Client(), Logos: logos,
	})
	spec := gatedSpec(t, "") // resolves via Placard to the broken mark

	// The plan must not promise to set an icon that cannot be set.
	st, err := p.Inspect(context.Background(), spinup.Target{Spec: spec})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if strings.Contains(st.Detail, "spec wants") {
		t.Errorf("the plan promised an icon the apply will not set: %q", st.Detail)
	}
	if !strings.Contains(st.Detail, "is kept") {
		t.Errorf("the plan should say the existing icon is kept: %q", st.Detail)
	}

	// And the apply must not clear the working one.
	if _, err := p.Ensure(context.Background(), spinup.Target{Spec: spec}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := api.lastBody["logo_url"]; got != live {
		t.Errorf("logo_url = %v, want the working icon %q kept — a spec asking for a different icon did not ask for this one to be removed", got, live)
	}
}
