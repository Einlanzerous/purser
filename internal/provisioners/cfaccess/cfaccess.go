// Package cfaccess provisions the Cloudflare Access surface for a service on the
// spin-up axis (PRSR-29, epic PRSR-22).
//
// It is a spinup.ServiceProvisioner, not a connector.Connector: it is keyed on a
// hostname rather than on a person, and it creates the thing that gates a
// service rather than granting somebody entry to one. internal/connectors/cloudflare
// is the other half — it adds a person's email to the Access *group* this
// package's policies allow.
//
// # Two application types, not one
//
// A live audit of the launcher (2026-08-15) found both shapes in use, and they
// are different objects rather than one object with a flag:
//
//   - AccessGated    → type "self_hosted" plus a policy allowing the shared
//     members group. The gate itself. `domain` is a bare hostname.
//   - AccessBookmark → type "bookmark", no policy: a launcher tile in front of a
//     service holding its own login. `domain` is a full URL,
//     scheme included.
//
// That `domain` asymmetry is real and observed, not a guess, and it is why
// matching an existing app to a spec compares the *host* rather than the string.
//
// # The logo is the reason this ticket existed
//
// Of the six Access apps live before this axis, one had a working icon.
// Cloudflare stores whatever `logo_url` it is handed and never validates it, and
// the launcher falls back to two grey initials when the image fails to load — so
// a wrong URL is indistinguishable from an unset one, with no error anywhere.
// Argosy's had been dead since the asset was renamed in its own repo and nothing
// surfaced it; Switchyard's pointed at jsDelivr against a repo that went private,
// so it could never have resolved at any path.
//
// This package therefore verifies the URL before writing it, and treats a logo
// that does not resolve as drift rather than as a reason to refuse the gate —
// see verifyLogo and desiredApp for why that asymmetry is deliberate.
//
// Placard (IDEA-22) is where a working URL comes from now:
// https://cdn.jsdelivr.net/gh/Einlanzerous/placard@main/<service>/<service>-mark-light.png
// against a public repo. Resolving a spec's logo from Placard's /api/services
// index is a separate piece of work; this package verifies whatever URL the spec
// carries.
//
// # Nothing here has ever contacted Cloudflare
//
// Every test in this package is httptest against a hand-written fake, which is
// true of every connector in this repo (see REVIEW.md). The request shapes come
// from the live audit recorded on PRSR-29 and from Cloudflare's documentation;
// where behaviour is inferred rather than observed the comment says so. Read
// this package as "what we believe the API accepts", and treat the first real
// run as the test that has not been run.
package cfaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

const apiBase = "https://api.cloudflare.com/client/v4"

// Config configures the Access-application provisioner.
type Config struct {
	// APIToken needs Account → Access: Apps and Policies → Edit. The same token
	// the person-axis Cloudflare connector uses already carries it (SERV-36).
	APIToken string
	// AccountID is the Cloudflare account the applications live in. It is also
	// what gets recorded as the resource row's parent_id.
	AccountID string
	// GroupID is the shared members Access group a gated application's policy
	// allows. Required only for AccessGated: a bookmark has no policy, so a
	// deployment that never stands up a gated service does not need it.
	GroupID string
	// GroupName is a human label for the plan, e.g. "zerogravity-members". It is
	// never sent upstream — the policy references the group by id.
	GroupName string

	// HTTPClient talks to the Cloudflare API.
	HTTPClient *http.Client
	// LogoClient fetches candidate logo URLs, and is deliberately separate.
	//
	// It calls a third party (jsDelivr, or whatever a spec names) rather than
	// Cloudflare, so it wants its own, shorter timeout: a slow CDN must not
	// consume the budget for the call that actually creates the gate.
	LogoClient *http.Client
}

// Provisioner manages the Cloudflare Access application for a hostname.
type Provisioner struct {
	cfg     Config
	http    *http.Client
	logo    *http.Client
	baseURL string // overridden in tests
}

// New builds the provisioner. Like the person-axis connectors it never fails on
// missing credentials: an unconfigured provisioner is valid and reports
// spinup.ErrUnavailable when asked to act, so a spin-up plan says "Cloudflare is
// not configured" instead of "no provisioner for access_app", which reads like a
// missing build.
func New(cfg Config) *Provisioner {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	lc := cfg.LogoClient
	if lc == nil {
		lc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Provisioner{cfg: cfg, http: hc, logo: lc, baseURL: apiBase}
}

// Kind is the resource kind this provisioner owns.
func (p *Provisioner) Kind() model.ResourceKind { return model.ResourceAccessApp }

// DisplayName is the label the plan uses for this step.
func (p *Provisioner) DisplayName() string { return "Access application" }

// available reports whether this provisioner can act on the given spec, from
// config alone and contacting nothing.
//
// The gated case needs a group id and the bookmark case does not, so readiness
// is a question about the spec rather than a single configured() bool. Getting
// this wrong in the lenient direction is the expensive one: a gated spec that
// proceeds without a group would create a `self_hosted` application with **no
// policy**, and an Access app with no policy is not a half-built gate — it is an
// app that admits nobody, or worse, depending on account defaults. Refusing up
// front means the DNS step stays blocked and the hostname never goes live
// half-gated.
func (p *Provisioner) available(spec spinup.ServiceSpec) error {
	var missing []string
	if p.cfg.APIToken == "" {
		missing = append(missing, "PURSER_CF_API_TOKEN")
	}
	if p.cfg.AccountID == "" {
		missing = append(missing, "PURSER_CF_ACCOUNT_ID")
	}
	if spec.Access == spinup.AccessGated && p.cfg.GroupID == "" {
		missing = append(missing, "PURSER_CF_ACCESS_GROUP_ID (a gated app's policy has to name the members group)")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: cloudflare access is not configured (set %s)",
		spinup.ErrUnavailable, strings.Join(missing, ", "))
}

// Inspect reports the current Access application for the hostname. Read-only:
// it lists applications and, at most, fetches the logo URL with GET. It creates,
// updates and deletes nothing.
func (p *Provisioner) Inspect(ctx context.Context, t spinup.Target) (spinup.State, error) {
	if err := p.available(t.Spec); err != nil {
		return spinup.State{}, err
	}
	found, err := p.findApp(ctx, t.Spec.Hostname)
	if err != nil {
		return spinup.State{}, err
	}
	if found == nil {
		return spinup.State{}, nil // Exists false: nothing here, and we could read to be sure
	}

	st := spinup.State{
		Exists:     true,
		ExternalID: str(found, "id"),
		ParentID:   p.cfg.AccountID,
	}
	diffs := p.diff(ctx, found, t.Spec)
	st.Matches = len(diffs) == 0
	st.Detail = describe(found, diffs)
	return st, nil
}

// Ensure creates or updates the Access application so it matches the spec.
func (p *Provisioner) Ensure(ctx context.Context, t spinup.Target) (spinup.Resource, error) {
	if err := p.available(t.Spec); err != nil {
		return spinup.Resource{}, err
	}
	found, err := p.findApp(ctx, t.Spec.Hostname)
	if err != nil {
		return spinup.Resource{}, err
	}

	logo, logoNote := p.resolveLogo(ctx, t.Spec.LogoURL)

	if found == nil {
		created, err := p.createApp(ctx, p.desiredApp(nil, t.Spec, logo))
		if err != nil {
			return spinup.Resource{}, err
		}
		return spinup.Resource{
			ExternalID: str(created, "id"),
			ParentID:   p.cfg.AccountID,
			Detail:     joinNote(describe(created, nil), logoNote),
		}, nil
	}

	// Already exists: merge onto what is there and PUT the whole object back.
	id := str(found, "id")
	updated, err := p.updateApp(ctx, id, p.desiredApp(found, t.Spec, logo))
	if err != nil {
		return spinup.Resource{}, err
	}
	return spinup.Resource{
		ExternalID: firstNonEmpty(str(updated, "id"), id),
		ParentID:   p.cfg.AccountID,
		Detail:     joinNote(describe(updated, nil), logoNote),
	}, nil
}

// Teardown deletes the recorded application.
//
// It targets rec.ExternalID rather than looking the hostname up, because
// deleting an Access application somebody created by hand is not recoverable by
// re-running. A 404 against that id is the interesting case and is handled the
// way the person axis handles it (see the `offboard` invariants): it is treated
// as "this record is wrong", not as "there is nothing here" — so before
// reporting success this re-reads by hostname, and refuses if some *other*
// application is still gating it. Claiming a teardown that left a live gate in
// place is the failure that outlives its error message.
func (p *Provisioner) Teardown(ctx context.Context, t spinup.Target, rec model.ServiceResource) error {
	if err := p.available(t.Spec); err != nil {
		return err
	}
	if rec.ExternalID == "" {
		return fmt.Errorf("cfaccess: no recorded application id for %s — refusing to guess by hostname", t.Spec.Hostname)
	}

	path := fmt.Sprintf("/accounts/%s/access/apps/%s", p.cfg.AccountID, rec.ExternalID)
	_, status, err := p.do(ctx, http.MethodDelete, path, nil)
	if err == nil {
		return nil
	}
	if status != http.StatusNotFound {
		return err
	}

	// The recorded id is gone. Either the app was already deleted — success — or
	// the row points at the wrong object and the real one is still live.
	found, lookupErr := p.findApp(ctx, t.Spec.Hostname)
	if lookupErr != nil {
		// Unverifiable is never absent: say the delete could not be confirmed
		// rather than reporting a removal nobody checked.
		return fmt.Errorf("cfaccess: recorded application %s is gone, but %s could not be re-checked: %w", rec.ExternalID, t.Spec.Hostname, lookupErr)
	}
	if found != nil {
		return fmt.Errorf("cfaccess: recorded application %s no longer exists, but %q is still served by application %s — the record is wrong, and removing the right one is not something this should guess at",
			rec.ExternalID, t.Spec.Hostname, str(found, "id"))
	}
	return nil
}

// ─── the application object ────────────────────────────────────────────────

// rawApp is an Access application as Cloudflare returned it.
//
// A map rather than a struct, and that is the single most important decision in
// this file. Updates are **PUT — full replacement**; `PATCH` returns "Method not
// allowed for this authentication scheme". A struct round-trip silently drops
// every field this package does not happen to model, so the first time
// Cloudflare adds one — or the first time an operator sets one in the dashboard
// that we never modelled — an otherwise innocent update would erase it. On a
// gated app the field most likely to be erased that way is `policies`, and
// erasing those un-gates the service.
//
// So: read the whole object, change only the keys the spec owns, strip the
// server-owned ones, and put it back.
type rawApp map[string]any

// serverOwned are the fields Cloudflare assigns and rejects (or silently
// mangles) on the way back in.
var serverOwned = []string{"id", "uid", "aud", "created_at", "updated_at"}

// desiredApp builds the object to send, starting from what is already there.
//
// base is nil for a create and the current object for an update. Everything not
// named here is carried through untouched — see rawApp.
func (p *Provisioner) desiredApp(base rawApp, spec spinup.ServiceSpec, logo string) rawApp {
	out := rawApp{}
	for k, v := range base {
		out[k] = v
	}
	for _, k := range serverOwned {
		delete(out, k)
	}

	out["type"] = appType(spec.Access)
	out["name"] = spec.DisplayName
	out["domain"] = appDomain(spec)
	out["app_launcher_visible"] = true

	// An empty logo is written as an empty string rather than left off, so that
	// *clearing* a rotted URL is expressible. The alternative — omitting the key
	// — would carry the old broken value forward from base for ever, which is
	// precisely the state argosy sat in.
	out["logo_url"] = logo

	switch spec.Access {
	case spinup.AccessGated:
		out["policies"] = []any{p.membersPolicy()}
	case spinup.AccessBookmark:
		// A bookmark has no policy. Sent explicitly rather than omitted so a
		// shape that was converted from gated does not keep its old gate.
		out["policies"] = []any{}
	}
	return out
}

// membersPolicy is the allow-the-members-group policy a gated app carries.
//
// Inline on the application object, which is the shape the live audit recorded
// (PRSR-29). Cloudflare also exposes /accounts/{a}/access/apps/{id}/policies;
// if the inline form turns out to be rejected, that endpoint is the fallback and
// this is the one function that would move.
func (p *Provisioner) membersPolicy() map[string]any {
	name := p.cfg.GroupName
	if name == "" {
		name = "members"
	}
	return map[string]any{
		"name":     fmt.Sprintf("Allow %s", name),
		"decision": "allow",
		"include": []any{
			map[string]any{"group": map[string]any{"id": p.cfg.GroupID}},
		},
	}
}

// appType maps the spec's Access shape onto Cloudflare's application type.
// AccessNone never reaches here — the orchestrator skips the step entirely.
func appType(shape spinup.AccessShape) string {
	if shape == spinup.AccessBookmark {
		return "bookmark"
	}
	return "self_hosted"
}

// appDomain renders the `domain` field, which differs by type.
//
// A bookmark carries a full URL — the live argosy entry is
// "https://argosy.zerogravity.industries" — because the tile is a link. A
// self_hosted application carries a bare hostname, because it names what Access
// sits in front of. Observed, not inferred.
func appDomain(spec spinup.ServiceSpec) string {
	if spec.Access == spinup.AccessBookmark {
		return "https://" + spec.Hostname
	}
	return spec.Hostname
}

// ─── matching ──────────────────────────────────────────────────────────────

// diff lists the ways the live application differs from the spec. Empty means
// State.Matches.
//
// Returned as a list of human phrases rather than a bool so the plan can say
// *what* is wrong: "update" on its own tells an operator nothing about whether
// they are about to fix a logo or convert a gate into a bookmark.
func (p *Provisioner) diff(ctx context.Context, live rawApp, spec spinup.ServiceSpec) []string {
	var diffs []string

	if got, want := str(live, "type"), appType(spec.Access); got != want {
		diffs = append(diffs, fmt.Sprintf("type is %q, spec wants %q", got, want))
	}
	if got, want := str(live, "name"), spec.DisplayName; got != want {
		diffs = append(diffs, fmt.Sprintf("name is %q, spec wants %q", got, want))
	}
	if got, want := domainHost(str(live, "domain")), spec.Hostname; got != want {
		diffs = append(diffs, fmt.Sprintf("domain resolves to %q, spec wants %q", got, want))
	}
	if v, ok := live["app_launcher_visible"].(bool); ok && !v {
		diffs = append(diffs, "hidden from the App Launcher")
	}

	if spec.Access == spinup.AccessGated && !allowsGroup(live, p.cfg.GroupID) {
		// The one difference that matters more than the rest put together: a
		// self_hosted app whose policy does not admit the members group is a gate
		// nobody can pass, and one that lost its policies is a gate that may not
		// be gating.
		diffs = append(diffs, fmt.Sprintf("no policy admits the members group (%s)", p.displayGroup()))
	}

	diffs = append(diffs, p.logoDiff(ctx, str(live, "logo_url"), spec.LogoURL)...)
	return diffs
}

// logoDiff compares the live icon against the spec's, and checks that what is
// live actually loads.
//
// The second half is the point. An icon that 404s renders exactly like an unset
// one, so "the URL string matches the spec" is not evidence the launcher shows
// anything. Reporting it as drift is what makes a rotted asset visible at all —
// argosy's went unnoticed for months because nothing ever asked.
func (p *Provisioner) logoDiff(ctx context.Context, live, want string) []string {
	switch {
	case want == "" && live == "":
		return nil
	case want == "" && live != "":
		return []string{fmt.Sprintf("has a logo (%s), spec sets none", live)}
	case live != want:
		return []string{fmt.Sprintf("logo is %q, spec wants %q", live, want)}
	}
	// Same URL on both sides — but is it serving?
	if err := p.verifyLogo(ctx, live); err != nil {
		return []string{fmt.Sprintf("logo url is set correctly but does not load (%v) — the launcher is showing initials", err)}
	}
	return nil
}

// allowsGroup reports whether any of the application's policies allows the
// group id.
//
// Tolerant of the two shapes an app's `policies` array is known to take: full
// policy objects, and bare id references on applications whose policies are
// managed separately. A reference-only list cannot be checked from here, so it
// is treated as *not* verifiably allowing the group — the conservative
// direction, which costs an unnecessary update rather than leaving a gate
// unverified.
func allowsGroup(live rawApp, groupID string) bool {
	if groupID == "" {
		return false
	}
	policies, ok := live["policies"].([]any)
	if !ok {
		return false
	}
	for _, raw := range policies {
		pol, ok := raw.(map[string]any)
		if !ok {
			continue // a bare id reference: not checkable here
		}
		if decision, _ := pol["decision"].(string); decision != "allow" {
			continue
		}
		include, ok := pol["include"].([]any)
		if !ok {
			continue
		}
		for _, r := range include {
			rule, ok := r.(map[string]any)
			if !ok {
				continue
			}
			grp, ok := rule["group"].(map[string]any)
			if !ok {
				continue
			}
			if id, _ := grp["id"].(string); id == groupID {
				return true
			}
		}
	}
	return false
}

// domainHost reduces an application's `domain` to a bare lowercase hostname, so
// a bookmark's "https://argosy.zerogravity.industries/" and a self_hosted app's
// "argosy.zerogravity.industries" compare equal. A self_hosted domain may carry
// a path, which is not part of the identity here.
func domainHost(domain string) string {
	d := strings.TrimSpace(strings.ToLower(domain))
	if i := strings.Index(d, "://"); i >= 0 {
		d = d[i+3:]
	}
	if i := strings.IndexAny(d, "/?#"); i >= 0 {
		d = d[:i]
	}
	return strings.TrimRight(d, ".")
}

// ─── the logo ──────────────────────────────────────────────────────────────

// resolveLogo decides what to write into logo_url, and returns a note for the
// report when that is not what the spec asked for.
//
// **An unverifiable logo never blocks the gate.** Refusing to create an Access
// application because a CDN is slow would hold back the DNS step (a gated app is
// a DNS prerequisite) and leave the service unpublished over an icon — the wrong
// trade by a wide margin. Writing a URL that does not load is also wrong, and is
// the exact failure this ticket was filed about.
//
// So the third option: create the app with **no** logo and say so. The gate goes
// in, the report carries the reason, and because the spec still wants a logo the
// next Inspect reports drift until the asset is actually published. A visible,
// converging "still not right" beats a silent, permanent wrong value.
func (p *Provisioner) resolveLogo(ctx context.Context, want string) (string, string) {
	if want == "" {
		return "", ""
	}
	if err := p.verifyLogo(ctx, want); err != nil {
		return "", fmt.Sprintf("logo omitted: %s did not verify (%v) — the app was created without one rather than with a URL that renders as grey initials", want, err)
	}
	return want, ""
}

// verifyLogo fetches the URL and reports whether it is a publicly servable
// image.
//
// GET rather than HEAD: HEAD is not universally implemented, and a 405 would
// read as a broken asset. The body is read only far enough to release the
// connection — the status and content type are the whole answer.
//
// No credentials are sent, deliberately. The launcher renders this as an <img>
// in the *viewer's* browser, so the only check that means anything is the one
// made as the sessionless public. An asset behind an Access gate would answer
// this request with an HTML login page, which the content-type test catches.
func (p *Provisioner) verifyLogo(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("not a fetchable url: %w", err)
	}
	req.Header.Set("Accept", "image/*")
	resp, err := p.logo.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	ct := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if !strings.HasPrefix(ct, "image/") {
		// The login-page case lands here: an Access-gated asset answers 200 with
		// text/html, which would otherwise pass a status-only check.
		return fmt.Errorf("content-type is %q, not an image", resp.Header.Get("Content-Type"))
	}
	return nil
}

// ─── describing what is there ──────────────────────────────────────────────

// describe renders the line an operator reads in the plan.
func describe(app rawApp, diffs []string) string {
	kind := str(app, "type")
	if kind == "" {
		kind = "application"
	}
	base := fmt.Sprintf("%s %q → %s", kind, str(app, "name"), str(app, "domain"))
	if logo := str(app, "logo_url"); logo != "" {
		base += ", logo set"
	} else {
		base += ", no logo"
	}
	if len(diffs) == 0 {
		return base
	}
	return base + "; " + strings.Join(diffs, "; ")
}

func (p *Provisioner) displayGroup() string {
	if p.cfg.GroupName != "" {
		return p.cfg.GroupName
	}
	return p.cfg.GroupID
}

func joinNote(detail, note string) string {
	if note == "" {
		return detail
	}
	return detail + " — " + note
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// str reads a string field, tolerating absence and a non-string value.
func str(m rawApp, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// ─── the API ───────────────────────────────────────────────────────────────

// findApp returns the application serving hostname, or nil if there is none.
//
// A list-and-match rather than a filtered query: Cloudflare's apps endpoint has
// no documented exact-domain filter, and matching here means one definition of
// "this hostname's app" shared by Inspect, Ensure and Teardown. Two apps on one
// hostname is a state this does not try to resolve — the first match wins and
// the caller's plan will describe it, which is more useful than an error that
// stops the run.
func (p *Provisioner) findApp(ctx context.Context, hostname string) (rawApp, error) {
	path := fmt.Sprintf("/accounts/%s/access/apps", p.cfg.AccountID)
	raw, _, err := p.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Result []rawApp `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("cfaccess: decode applications: %w", err)
	}
	for _, app := range env.Result {
		if domainHost(str(app, "domain")) == hostname {
			return app, nil
		}
	}
	return nil, nil
}

func (p *Provisioner) createApp(ctx context.Context, body rawApp) (rawApp, error) {
	path := fmt.Sprintf("/accounts/%s/access/apps", p.cfg.AccountID)
	raw, _, err := p.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	return decodeApp(raw)
}

// updateApp replaces the application. PUT, not PATCH — see rawApp.
func (p *Provisioner) updateApp(ctx context.Context, id string, body rawApp) (rawApp, error) {
	path := fmt.Sprintf("/accounts/%s/access/apps/%s", p.cfg.AccountID, id)
	raw, _, err := p.do(ctx, http.MethodPut, path, body)
	if err != nil {
		return nil, err
	}
	return decodeApp(raw)
}

func decodeApp(raw []byte) (rawApp, error) {
	var env struct {
		Result rawApp `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("cfaccess: decode application: %w", err)
	}
	return env.Result, nil
}

// do performs a Cloudflare API request, returning the raw body and the HTTP
// status. The status is returned alongside the error because Teardown has to
// tell a 404 from every other failure, and the {"success":false} envelope does
// not carry it.
func (p *Provisioner) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("cfaccess: marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("cfaccess: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("cfaccess: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("cfaccess: read body: %w", err)
	}

	var env struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("cfaccess: %s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if !env.Success {
		if len(env.Errors) > 0 {
			return nil, resp.StatusCode, fmt.Errorf("cfaccess: %s %s: %d %s", method, path, env.Errors[0].Code, env.Errors[0].Message)
		}
		return nil, resp.StatusCode, fmt.Errorf("cfaccess: %s %s: request unsuccessful (%d)", method, path, resp.StatusCode)
	}
	return raw, resp.StatusCode, nil
}
