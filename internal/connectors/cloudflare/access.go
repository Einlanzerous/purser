package cloudflare

// This file is the service spin-up axis's Access step (PRSR-29): the Cloudflare
// Access application that gates — or merely advertises — a service's hostname.
//
// It is a spinup.ServiceProvisioner, not a connector.Connector, and the
// difference is visible against its neighbour in this package. cloudflare.go
// provisions a *person* into the shared Access group and is idempotent per
// (person × service); this provisions a *hostname* and is idempotent per
// (hostname, kind). The two meet at the group: the policy written here is what
// the emails added there are admitted by.
//
// It shares client.go with the Access connector and the DNS provisioner, and
// like DNSConfig it keeps its own config — an Access application needs the
// account and the group, a DNS record needs the zone, and folding either into
// the other's readiness check would take a working surface offline over a
// setting it never reads.
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
// Placard (IDEA-22) is where a working URL comes from, and since PRSR-37 this
// package no longer asks a spec author to type one. A spec names a *ref* —
// spinup.LogoPlacard, spinup.LogoNone, or an explicit https URL — and
// wantedLogo resolves the first of those through the LogoResolver interface,
// implemented by internal/placard. A spec that says nothing about its icon means
// LogoPlacard, so the ordinary case is that a service gets the right mark by
// being named.
//
// Two things that look like details and are not. Placard is asked which asset is
// the *tile* asset, which no fetch check can answer: argosy's old URL resolved
// 200 image/png and was the 3.6:1 wordmark, illegible at tile size, and Placard
// publishes the ship glyph alone precisely for that reason — so a working
// logo_url is not a correct one. And resolution never *decides*: Placard's own
// per-file check is a periodic monitor carrying a checked_at and can be stale, so
// it picks the URL and this package's own sessionless fetch is still what runs at
// the moment of writing.
//
// Every answer other than "here is the mark" leaves the icon alone — Placard has
// none for the slug, Placard could not be asked, no Placard configured. Only
// spinup.LogoNone removes one, which is what makes a deletion something an
// operator asked for by name rather than something a forgotten flag did.
//
// # Every test here is a fake; the write verbs themselves have run live
//
// Every test covering this file is httptest against a hand-written fake, which
// is true of every connector in this repo (see REVIEW.md). That distinction is
// worth keeping in view, because the two halves of it now point different ways.
//
// The request shapes come from the live audit recorded on PRSR-29 and from
// Cloudflare's documentation; where behaviour is inferred rather than observed
// the comment says so. **PRSR-40 (2026-08-26) then ran this file's write verbs
// against the live API** — the gated create, the full-replacement update on both
// of its branches, the bookmark create and update, the logo clear, and Teardown
// including its confirm-by-reading path — driven through this exact code rather
// than through curl, on disposable hostnames. So "what we believe the API
// accepts" is no longer the right way to read the writes.
//
// It is still the right way to read the *tests*, which is why the heading says
// what it says: a green suite here proves this file agrees with a fake somebody
// wrote, and PRSR-38 is the standing example of a fixture that modelled the
// spec instead of the API and hid a live bug behind five passing tests.

import (
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

// AccessConfig configures the Access-application provisioner.
type AccessConfig struct {
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
	// Logos resolves a service key to its launcher mark, for a spec that says
	// spinup.LogoPlacard rather than naming a URL (PRSR-37).
	//
	// An interface rather than *placard.Resolver so this package does not import
	// a second upstream to decorate a tile, and so a test can answer all three
	// of its outcomes without a server. A nil Logos is the unconfigured
	// deployment: resolution reports that it could not be done, which leaves
	// every icon exactly as it is. Never a failed step — see resolveLogo.
	Logos LogoResolver
}

// LogoResolver turns a service key into the canonical URL of its launcher mark.
//
// found is false when the source answered and has no usable mark for that key —
// a service it has never heard of, or one whose file it records as missing. A
// non-nil error means it could not be asked, which is a different answer and
// must not be collapsed into the first: "there is no icon for this service" is a
// fact worth acting on, while "the registry is down" is not, and treating the
// second as the first clears working icons across the estate on every blip.
//
// Implemented by *placard.Resolver.
type LogoResolver interface {
	Mark(ctx context.Context, key string) (url string, found bool, err error)
}

// AccessProvisioner manages the Cloudflare Access application for a hostname.
type AccessProvisioner struct {
	cfg  AccessConfig
	api  *client
	logo *http.Client
}

// NewAccess builds the provisioner. Like the person-axis connectors it never
// fails on missing credentials: an unconfigured provisioner is valid and reports
// spinup.ErrUnavailable when asked to act, so a spin-up plan says "Cloudflare is
// not configured" instead of "no provisioner for access_app", which reads like a
// missing build.
func NewAccess(cfg AccessConfig) *AccessProvisioner {
	lc := cfg.LogoClient
	if lc == nil {
		lc = &http.Client{Timeout: 10 * time.Second}
	}
	return &AccessProvisioner{cfg: cfg, api: newClient(cfg.APIToken, cfg.HTTPClient), logo: lc}
}

// Compile-time proof this is the shape the orchestrator walks, matching
// DNSProvisioner above. Without it a signature drift would surface at the
// composition root rather than here.
var _ spinup.ServiceProvisioner = (*AccessProvisioner)(nil)

// Kind is the resource kind this provisioner owns.
func (p *AccessProvisioner) Kind() model.ResourceKind { return model.ResourceAccessApp }

// DisplayName is the label the plan uses for this step.
func (p *AccessProvisioner) DisplayName() string { return "Access application" }

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
func (p *AccessProvisioner) available(spec spinup.ServiceSpec) error {
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
func (p *AccessProvisioner) Inspect(ctx context.Context, t spinup.Target) (spinup.State, error) {
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
		ExternalID: appStr(found, "id"),
		ParentID:   p.cfg.AccountID,
	}
	diffs, notes := p.diff(ctx, found, t.Spec)
	st.Matches = len(diffs) == 0
	st.Detail = describeApp(found, append(diffs, notes...))
	return st, nil
}

// Ensure creates or updates the Access application so it matches the spec.
func (p *AccessProvisioner) Ensure(ctx context.Context, t spinup.Target) (spinup.Resource, error) {
	if err := p.available(t.Spec); err != nil {
		return spinup.Resource{}, err
	}
	found, err := p.findApp(ctx, t.Spec.Hostname)
	if err != nil {
		return spinup.Resource{}, err
	}

	// The current value matters: it is what an unreadable check falls back to,
	// so a CDN blip carries the existing icon forward instead of clearing it.
	// str tolerates a nil found, which is the create path — nothing to carry.
	logo, logoNote := p.resolveLogo(ctx, t.Spec, appStr(found, "logo_url"))

	if found == nil {
		created, err := p.createApp(ctx, p.desiredApp(nil, t.Spec, logo))
		if err != nil {
			return spinup.Resource{}, err
		}
		return spinup.Resource{
			ExternalID: appStr(created, "id"),
			ParentID:   p.cfg.AccountID,
			Detail:     joinNote(describeApp(created, nil), logoNote),
		}, nil
	}

	// Already exists: merge onto what is there and PUT the whole object back.
	id := appStr(found, "id")
	updated, err := p.updateApp(ctx, id, p.desiredApp(found, t.Spec, logo))
	if err != nil {
		return spinup.Resource{}, err
	}
	return spinup.Resource{
		ExternalID: firstNonEmpty(appStr(updated, "id"), id),
		ParentID:   p.cfg.AccountID,
		Detail:     joinNote(describeApp(updated, nil), logoNote),
	}, nil
}

// Teardown deletes the recorded application.
//
// It targets rec.ExternalID rather than looking the hostname up, because
// deleting an Access application somebody created by hand is not recoverable by
// re-running — the recorded id is the only handle Purser can prove it owns.
//
// # Absence is confirmed by reading, not by an error code
//
// The DNS provisioner decides "already gone" from Cloudflare's error code
// (dnsRecordNotFound, 81044), and deliberately not from a bare 404, because a
// 404 is also how the API answers a request it could not route. That reasoning
// applies here too — but 81044 is DNS's code and there is no *observed* Access
// equivalent, and the invariant is explicit that a guessed code turns an
// unrelated failure into a reported deletion.
//
// So this does not guess one. On any failed delete it re-reads the hostname and
// lets the answer decide:
//
//   - nothing serves the hostname → the application really is gone, and the
//     delete failed because there was nothing to delete. Success.
//   - something still serves it → whatever went wrong, the gate is still up.
//     Refuse, and name the application that is still there.
//   - the re-read itself failed → unverifiable, which is never absent. Say the
//     removal could not be confirmed.
//
// That is stronger than a code test rather than a workaround for lacking one: it
// asserts the thing Teardown actually claims. It also covers the case a code
// test cannot — a delete that failed at the transport after Cloudflare had
// already applied it.
func (p *AccessProvisioner) Teardown(ctx context.Context, t spinup.Target, rec model.ServiceResource) error {
	if err := p.available(t.Spec); err != nil {
		return err
	}
	if rec.ExternalID == "" {
		return fmt.Errorf("cloudflare: no recorded Access application id for %s — Purser deletes only ids it recorded, since an application found by hostname may be one somebody created by hand",
			t.Spec.Hostname)
	}

	path := fmt.Sprintf("/accounts/%s/access/apps/%s", p.cfg.AccountID, rec.ExternalID)
	if _, err := p.api.do(ctx, http.MethodDelete, path, nil); err != nil {
		return p.confirmGone(ctx, t.Spec.Hostname, rec.ExternalID, err)
	}
	return nil
}

// confirmGone decides what a failed delete meant, by reading the hostname back.
func (p *AccessProvisioner) confirmGone(ctx context.Context, hostname, recordedID string, cause error) error {
	found, lookupErr := p.findApp(ctx, hostname)
	switch {
	case lookupErr != nil:
		return fmt.Errorf("cloudflare: deleting Access application %s failed (%v), and %s could not be re-read to find out whether it is gone: %w",
			recordedID, cause, hostname, lookupErr)
	case found != nil:
		return fmt.Errorf("cloudflare: deleting Access application %s failed (%v), and %q is still served by application %s — the gate is still up",
			recordedID, cause, hostname, appStr(found, "id"))
	}
	return nil // nothing serves the hostname: it really is gone
}

// ─── the application object ────────────────────────────────────────────────

// rawApp is an Access application as Cloudflare returned it, held as a map
// rather than as a struct.
//
// The reason is that changing one field means sending the **whole application
// back**. Cloudflare rejects PATCH on this endpoint — "Method not allowed for
// this authentication scheme" — so the only way to update is PUT, and PUT
// replaces the application with exactly what the request body contains.
// Anything left out of that body is deleted.
//
// With a struct, "left out" means "any field nobody wrote a Go field for", and
// that set is invisible: encoding/json drops unknown keys when it decodes, so
// decode → change the name → encode → PUT sends back an object that is missing
// every field this package never thought about. Cloudflare then removes them.
// That includes fields an operator set in the dashboard and fields Cloudflare
// adds after this was written — neither of which will show up in a test.
//
// On a gated application the field that disappears is `policies`. An update
// meaning only to correct a logo would delete the rule that gates the service,
// and the service would stay up, keep resolving, and admit everyone.
//
// A map keeps every key the API sent, understood or not. desiredApp then
// changes only the keys the spec owns and removes only the server-owned ones.
// TestEnsure_UpdatePreservesUnmodelledFieldsAndStripsServerOwned is the guard.
type rawApp map[string]any

// serverOwned are the fields Cloudflare assigns and rejects (or silently
// mangles) on the way back in.
var serverOwned = []string{"id", "uid", "aud", "created_at", "updated_at"}

// desiredApp builds the object to send, starting from what is already there.
//
// base is nil for a create and the current object for an update. Everything not
// named here is carried through untouched — see rawApp.
func (p *AccessProvisioner) desiredApp(base rawApp, spec spinup.ServiceSpec, logo string) rawApp {
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
		// Appended, never assigned. Assigning `[membersPolicy]` here would delete
		// every *other* policy the application carries — a service token for an
		// uptime monitor, a second group — which is the identical outcome to the
		// naive PUT that rawApp exists to prevent, reached by a different route.
		//
		// It is also invisible in the plan: diff only emits a policy line when the
		// group is NOT admitted, so an app that already has the members policy plus
		// one more reports its rotted logo as the single drift, and the operator
		// approves "fix a logo" while the apply removes somebody's access.
		//
		// The spec says this service is gated by the members group. It does not
		// say the members group is the only thing that may reach it, and the
		// difference is somebody else's access.
		//
		// What goes back matters less than it looks, and PRSR-40 measured which
		// half is which. A `reusable: true` policy in an application body is read
		// as a **reference**: Cloudflare takes the id and ignores every other
		// field. The probe sent one back with `name` rewritten and `decision`
		// flipped to "deny"; the write returned 200 echoing the policy's real
		// name and decision, the standalone policy's updated_at did not move, and
		// a second application sharing it was untouched. So echoing the estate's
		// shared `Standard` policy back cannot edit it — the outcome PRSR-40 was
		// filed to rule out is structurally impossible, and the id is why.
		//
		// A `reusable: false` policy is the opposite: its body *is* honoured, so
		// carrying one through is a real write of the policy's content. Safe
		// here only because Ensure takes its own fresh read immediately above,
		// which is the same read-then-write discipline the tunnel's docMu
		// enforces for the shared ingress document.
		switch groupPolicy(base, p.cfg.GroupID) {
		case policyMissingGroup:
			out["policies"] = append(livePolicies(base), p.membersPolicy())
		default:
			// Already admitted, or a list this cannot read (see groupPolicy).
			// Either way the existing value carries through from base untouched —
			// rewriting a policy list we could not verify is the same mistake as
			// clearing a logo we could not fetch.
		}
	case spinup.AccessBookmark:
		// A bookmark is the one case that *is* an assignment, because a bookmark
		// has no policies by definition: a shape converted from gated must not
		// keep its old gate, and there is nothing here anyone could have added
		// deliberately.
		out["policies"] = []any{}
	}
	return out
}

// livePolicies returns the application's current policy list, or nil.
func livePolicies(app rawApp) []any {
	if app == nil {
		return nil
	}
	ps, _ := app["policies"].([]any)
	return ps
}

// membersPolicy is the allow-the-members-group policy a gated app carries.
//
// Inline on the application object, which is the shape the live audit recorded
// (PRSR-29) and which PRSR-40 then confirmed is *accepted* on write, not merely
// returned on read: a POST carrying exactly this object was taken, and the
// response echoed it back with a fresh id, `reusable: false` and `precedence: 1`.
// It does not appear in /accounts/{a}/access/policies, which lists only the
// reusable ones — so a gated service Purser stands up gets its own private gate
// rather than joining the estate's shared `Standard` policy. That is the safe
// direction, and it was worth measuring rather than assuming.
//
// No id here, deliberately: the policy does not exist yet, and on the update
// path an id is the whole of what Cloudflare reads (see desiredApp).
// /accounts/{a}/access/apps/{id}/policies remains available and is no longer
// needed as a fallback.
func (p *AccessProvisioner) membersPolicy() map[string]any {
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
func (p *AccessProvisioner) diff(ctx context.Context, live rawApp, spec spinup.ServiceSpec) (diffs, notes []string) {

	if got, want := appStr(live, "type"), appType(spec.Access); got != want {
		diffs = append(diffs, fmt.Sprintf("type is %q, spec wants %q", got, want))
	}
	if got, want := appStr(live, "name"), spec.DisplayName; got != want {
		diffs = append(diffs, fmt.Sprintf("name is %q, spec wants %q", got, want))
	}
	if got, want := domainHost(appStr(live, "domain")), spec.Hostname; got != want {
		diffs = append(diffs, fmt.Sprintf("domain resolves to %q, spec wants %q", got, want))
	}
	if v, ok := live["app_launcher_visible"].(bool); ok && !v {
		diffs = append(diffs, "hidden from the App Launcher")
	}

	if spec.Access == spinup.AccessGated {
		switch groupPolicy(live, p.cfg.GroupID) {
		case policyMissingGroup:
			// The one difference that matters more than the rest put together: a
			// self_hosted app whose policy does not admit the members group is a
			// gate nobody can pass, and one that lost its policies is a gate that
			// may not be gating.
			diffs = append(diffs, fmt.Sprintf("no policy admits the members group (%s)", p.displayGroup()))
		case policyUnreadable:
			// A note, not drift. An update is a full-replacement PUT, and the one
			// thing worse than not knowing whether the gate is in place is
			// rewriting the list that holds it on the strength of not knowing.
			notes = append(notes, fmt.Sprintf("policies could not be read, so whether %s is admitted is unverified — they are left as they are", p.displayGroup()))
		}
	}

	logoDiffs, logoNotes := p.logoDiff(ctx, appStr(live, "logo_url"), spec)
	return append(diffs, logoDiffs...), append(notes, logoNotes...)
}

// logoDiff compares the live icon against the spec's, and checks that what is
// live actually loads. It returns drift and, separately, notes — things worth
// printing that are not grounds for an update.
//
// The load check is the point. An icon that 404s renders exactly like an unset
// one, so "the URL string matches the spec" is not evidence the launcher shows
// anything. Reporting it as drift is what makes a rotted asset visible at all —
// argosy's went unnoticed for months because nothing ever asked.
//
// A check that could not complete is a **note, not drift**. Counting it as drift
// would mark the step StepUpdate over a network blip, and an update here is a
// full-replacement PUT of the whole application — not something to trigger on
// evidence this thin.
func (p *AccessProvisioner) logoDiff(ctx context.Context, live string, spec spinup.ServiceSpec) (diffs, notes []string) {
	want, src, note := p.wantedLogo(ctx, spec)
	switch src {
	case logoSourceClear:
		if live == "" {
			return nil, nil
		}
		// Named as drift, deliberately and loudly. An --apply really will delete
		// a working icon here, and the plan naming it is the only thing between
		// that and a surprise — PRSR-38 reported exactly this line against
		// argosy's live tile, and it was correct to.
		return []string{fmt.Sprintf("has a logo (%s), spec sets none", live)}, nil
	case logoSourceKeep:
		// A note rather than drift: there is nothing to compare the live value
		// against, and --apply will not touch it. Reporting "logo is X, spec
		// wants ''" here would be a plan promising a deletion that will not
		// happen, which is the same broken promise CanDeprovision exists to stop
		// making on offboard.
		if note != "" {
			notes = append(notes, note)
		}
		// Whatever is already there is still worth checking, and this is the
		// only branch where forgetting to would matter. The keep note says the
		// launcher will show the service's initials; if a rotted URL is
		// configured that sentence is wrong, and the rotted URL is invisible —
		// which is precisely the condition switchyard's tile sat in for months.
		// chronicle is the live example: a gated application Placard has never
		// heard of, so every run of this command takes this branch.
		//
		// Still a note and not drift, because nothing will be written either
		// way. It restores the detector without promising an action.
		if live != "" {
			if verdict, err := p.checkLogo(ctx, live); verdict == logoBroken {
				notes = append(notes, fmt.Sprintf("the logo already set (%s) is not a servable image (%v) — the launcher is showing initials, and no icon was resolved to replace it", live, err))
			}
		}
		return nil, notes
	}

	switch {
	case want == "" && live == "":
		return nil, nil
	case want == "" && live != "":
		return []string{fmt.Sprintf("has a logo (%s), spec sets none", live)}, nil
	case live != want:
		// Check the candidate before calling this drift. Reporting `update`
		// here without fetching `want` is how the plan came to promise the
		// opposite of what the apply does: resolveLogo fetches it, and on a
		// definite non-image it keeps whatever is already there — so a plan
		// saying "logo is A, spec wants B" would be followed by an apply that
		// changed nothing, or, before the fix above, by one that cleared A.
		//
		// A preview is the first half of an apply, not a guess at it. Both read
		// the same fetch, unconditionally — an earlier version guarded this on
		// `live != ""`, on the reasoning that the fetch only matters when there
		// is an icon to lose. That reads like the create path and is not: this
		// function runs only when the application exists, so `live == ""` is an
		// existing application that has no icon yet, which was **seven of the
		// ten** PRSR-38 audited.
		//
		// Skipping it there does not destroy anything, which is why it is the
		// smaller sibling of the bug above rather than a repeat of it. What it
		// does is fail to converge: the plan names a URL it never checked and
		// says it will set it, the apply fetches it, finds it dead and writes
		// nothing, and the next run says exactly the same — with each --apply
		// performing a full-replacement PUT of a gated application for a change
		// that will not happen.
		if verdict, err := p.checkLogo(ctx, want); verdict == logoBroken {
			if live != "" {
				return nil, []string{fmt.Sprintf("the icon this spec asks for (%s) is not a servable image (%v), so the one already set is kept", want, err)}
			}
			return nil, []string{fmt.Sprintf("the icon this spec asks for (%s) is not a servable image (%v), so none is written — the launcher shows the service's initials", want, err)}
		}
		return []string{fmt.Sprintf("logo is %q, spec wants %q", live, want)}, nil
	}
	// Same URL on both sides — but is it serving?
	switch verdict, err := p.checkLogo(ctx, live); verdict {
	case logoBroken:
		return []string{fmt.Sprintf("logo url is set correctly but is not a servable image (%v) — the launcher is showing initials", err)}, nil
	case logoUnknown:
		return nil, []string{fmt.Sprintf("logo could not be checked (%v)", err)}
	}
	return nil, nil
}

// policyVerdict is what an application's policy list says about the members
// group. Three values rather than a bool, for the reason the logo has three:
// "cannot tell" and "no" want different actions, and collapsing them is how a
// list nobody could read gets rewritten anyway.
type policyVerdict int

const (
	// policyAdmitsGroup — a policy object allows the members group.
	policyAdmitsGroup policyVerdict = iota
	// policyMissingGroup — the list was readable and nothing in it admits the
	// group. The gate is not in place; append one.
	policyMissingGroup
	// policyUnreadable — the list holds something this cannot interpret, so
	// whether the group is admitted is unknown. Cloudflare is documented to
	// return bare policy *references* on applications whose policies are managed
	// through /apps/{id}/policies. PRSR-40 looked: this estate's API always
	// answers with full objects, including for a reusable policy shared by six
	// applications, so the branch is unreached here rather than wrong. It is kept
	// because the reference form is demonstrably a shape the API *accepts* —
	// `{"id": …}` in an application body was taken and expanded on read-back — so
	// it is a shape it may one day send, and an unreadable list left alone is the
	// same rule as a logo that could not be fetched: never treat unverifiable as
	// absent.
	policyUnreadable
)

// groupPolicy reports what the live application's policies say about groupID.
//
// An empty groupID is policyMissingGroup rather than unreadable: `available`
// refuses a gated spec without one long before this is reached, so the only way
// here is a bookmark, which never consults it.
func groupPolicy(live rawApp, groupID string) policyVerdict {
	policies, ok := live["policies"].([]any)
	if !ok {
		// No policies key at all — a create, or an app that carries none.
		// Readable, and it plainly does not admit the group.
		return policyMissingGroup
	}
	unreadable := false
	for _, raw := range policies {
		pol, ok := raw.(map[string]any)
		if !ok {
			// A bare id reference. Nothing about it can be checked from here.
			unreadable = true
			continue
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
			if id, _ := grp["id"].(string); id != "" && id == groupID {
				return policyAdmitsGroup
			}
		}
	}
	if unreadable {
		return policyUnreadable
	}
	return policyMissingGroup
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

// logoVerdict is the outcome of checking a logo URL, and the distinction it
// draws is the whole of this section.
//
// "We could not check it" and "we checked it and it is broken" are different
// facts that want opposite handling, and collapsing them is how a transient CDN
// timeout ends up **erasing a working icon**. It is the same distinction the
// audit draws between UpstreamUnknown and UpstreamNo, and the same one the
// orchestrator draws between StepUnknown and StepCreate: never treat
// unverifiable as absent.
type logoVerdict int

const (
	// logoOK — fetched as the public would, 200, and an image.
	logoOK logoVerdict = iota
	// logoBroken — a definite answer that was not a servable image: 404 for a
	// path that rotted, 403 for something not public, or a 200 that is not an
	// image at all. This is a fact about the asset and it will not change on a
	// retry, so acting on it is safe.
	logoBroken
	// logoUnknown — the check itself did not complete: DNS failure, connection
	// refused, TLS failure, a timeout, or a 5xx from the origin. Says nothing
	// about the asset. Acting on it would mean clearing a logo because a CDN
	// blinked.
	logoUnknown
)

// logoSource says what kind of answer wantedLogo produced. Three values, for the
// reason logoVerdict has three and policyVerdict has three: "use this url",
// "remove the icon" and "we could not work out what the icon should be" want
// different actions, and the last one must never be executed as the middle one.
type logoSource int

const (
	// logoSourceURL — a concrete URL to verify and write.
	logoSourceURL logoSource = iota
	// logoSourceClear — the spec named spinup.LogoNone. Write the empty string.
	logoSourceClear
	// logoSourceKeep — nothing was resolved. Leave whatever is there.
	logoSourceKeep
)

// wantedLogo turns the spec's LogoRef into a candidate URL.
//
// Both the plan and the apply read it, which is what keeps them agreeing: a
// preview is the first half of an apply, not a guess at it. It is the only place
// that calls the resolver, so a spin-up asks Placard once for the plan and once
// for the apply and no more.
func (p *AccessProvisioner) wantedLogo(ctx context.Context, spec spinup.ServiceSpec) (string, logoSource, string) {
	switch spec.Logo {
	case spinup.LogoNone:
		return "", logoSourceClear, ""

	case spinup.LogoPlacard:
		if p.cfg.Logos == nil {
			// Unconfigured, which is Purser's own gap rather than anything about
			// this service. It is deliberately not a failed step and not an
			// unavailable one: the Access application is a DNS prerequisite when
			// it is gated, so refusing here would leave a service unpublished
			// over an icon. The same argument the logo has always made.
			return "", logoSourceKeep, fmt.Sprintf("logo left as it is: %s asks for the icon to come from Placard, and PURSER_PLACARD_URL is not set", spinup.LogoPlacard)
		}
		url, found, err := p.cfg.Logos.Mark(ctx, spec.Key)
		switch {
		case err != nil:
			return "", logoSourceKeep, fmt.Sprintf("logo left as it is: Placard could not be asked for %q (%v) — an unreachable registry is not evidence the icon is wrong", spec.Key, err)
		case !found:
			// Placard answered, and it has nothing for this slug. An ordinary
			// state rather than a fault: its registry covers seven services, and
			// a spin-up necessarily runs before a brand-new service's mark is
			// drawn. The launcher rendering the service's initials is the honest
			// picture of "there is no icon yet".
			return "", logoSourceKeep, fmt.Sprintf("no icon: Placard has no mark for %q, so the launcher shows the service's initials and anything already set is left alone", spec.Key)
		}
		return url, logoSourceURL, ""

	default:
		// An explicit https URL, already shape-checked by Validate.
		return string(spec.Logo), logoSourceURL, ""
	}
}

// checkLogo fetches the URL and classifies it.
//
// GET rather than HEAD: HEAD is not universally implemented, and a 405 would
// read as a broken asset. The body is read only far enough to release the
// connection — the status and content type are the whole answer.
//
// No credentials are sent, deliberately. The launcher renders this as an <img>
// in the *viewer's* browser, so the only check that means anything is the one
// made as the sessionless public. An asset behind an Access gate answers with an
// HTML login page, which is a 200 — the content-type test is what catches it.
func (p *AccessProvisioner) checkLogo(ctx context.Context, url string) (logoVerdict, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		// Not a URL that can ever be fetched. Definite.
		return logoBroken, fmt.Errorf("not a fetchable url: %w", err)
	}
	req.Header.Set("Accept", "image/*")
	resp, err := p.logo.Do(req)
	if err != nil {
		// Transport-level: refused, timed out, TLS, DNS. Nothing was learned
		// about the asset.
		return logoUnknown, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()
	switch {
	case resp.StatusCode >= 500:
		// The origin is unwell, which is not the same as the asset being wrong.
		return logoUnknown, fmt.Errorf("http %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return logoBroken, fmt.Errorf("http %d", resp.StatusCode)
	}
	ct := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if !strings.HasPrefix(ct, "image/") {
		return logoBroken, fmt.Errorf("content-type is %q, not an image", resp.Header.Get("Content-Type"))
	}
	return logoOK, nil
}

// resolveLogo decides what to write into logo_url, given what the spec wants and
// what the application currently carries. The second return is a note for the
// report when the answer is not simply "what the spec asked for".
//
// Three outcomes, and the middle one is the point:
//
//   - Verified → write it.
//   - Broken → write nothing. The launcher falls back to the service's first
//     two letters, which is the honest rendering of "there is no icon" and is
//     exactly what an unset logo does anyway. Writing a URL that 404s produces
//     the identical initials while *claiming* an icon is configured, and that
//     claim is what let argosy's stay dead for months.
//   - Could not check → change nothing. Keep whatever is already there, even
//     though it is unverified, because the alternative is clearing a working
//     icon over a network blip.
//
// What this never does is fail the step. A gated Access application is a DNS
// prerequisite, so refusing to create it would hold the hostname back — leaving
// a service unpublished over an icon is the wrong trade by a wide margin. And
// because the spec still asks for a logo, the next Inspect keeps reporting drift
// until the asset really is published: a visible, converging "still not right"
// rather than a silent permanent wrong value.
func (p *AccessProvisioner) resolveLogo(ctx context.Context, spec spinup.ServiceSpec, current string) (string, string) {
	want, src, note := p.wantedLogo(ctx, spec)
	switch src {
	case logoSourceClear:
		// The spec asks for no icon, by name. Clearing is intended, not a
		// fallback — which is the whole reason spinup.LogoNone exists as a
		// separate value from an unspecified logo.
		return "", ""
	case logoSourceKeep:
		// Nothing could be resolved, and that is not evidence the icon is
		// wrong. Returning the current value writes it straight back, so an
		// existing tile survives a Placard outage, an unconfigured deployment,
		// and a slug Placard has never been given a mark for. On a create there
		// is nothing to carry and this is the empty string, which is the honest
		// rendering of "no icon yet": the launcher shows the initials.
		return current, note
	}

	verdict, err := p.checkLogo(ctx, want)
	switch verdict {
	case logoOK:
		return want, ""
	case logoBroken:
		// `current != want` is the whole of the condition this keep was reasoned
		// about, and leaving it off made the fix over-broad by exactly one case.
		//
		// When the live icon *is* the URL the spec asks for and that URL is dead,
		// `current` is not a working tile to protect — it is the thing checkLogo
		// just proved is broken. Writing it back makes the drift permanent: the
		// plan reports "set correctly but not a servable image" for ever,
		// NeedsAttention never clears, and every --apply performs a
		// full-replacement PUT of a gated application for a change that cannot
		// happen. Clearing converges instead, and the next plan then reports a
		// note rather than drift.
		//
		// The note's own wording was the tell — "clearing a working icon is not
		// what a spec asking for *a different one* meant" is self-contradictory
		// when the spec asked for this one.
		if current != "" && current != want {
			// The spec named an icon that does not serve, and something that
			// does is already on the tile. Keeping it is not a fallback, it is
			// the only non-destructive answer: writing "" here clears a working
			// icon, which PRSR-40 confirmed live really does remove it, and the
			// plan that authorised this run said "set this url" rather than
			// "remove the one you have". Losing a working tile is a strictly
			// worse outcome than not gaining the new one.
			//
			// This used to fall through to the empty string with logoUnknown two
			// cases below already making the opposite choice on the same
			// question, which is the tell: "we could not check it" and "we
			// checked and it is dead" want the same answer whenever there is
			// something to lose.
			return current, fmt.Sprintf("logo left as it was: %s is not a servable image (%v), and clearing a working icon is not what a spec asking for a different one meant", want, err)
		}
		return "", fmt.Sprintf("logo omitted: %s is not a servable image (%v) — the launcher shows the service's initials either way, and writing a dead url would claim an icon that isn't there", want, err)
	default: // logoUnknown
		if current != "" {
			return current, fmt.Sprintf("logo left as it was: %s could not be checked (%v) — an unreachable cdn is not evidence the icon is wrong", want, err)
		}
		return "", fmt.Sprintf("logo not set yet: %s could not be checked (%v) — nothing was written rather than an unverified url", want, err)
	}
}

// ─── describing what is there ──────────────────────────────────────────────

// describe renders the line an operator reads in the plan.
func describeApp(app rawApp, diffs []string) string {
	kind := appStr(app, "type")
	if kind == "" {
		kind = "application"
	}
	base := fmt.Sprintf("%s %q → %s", kind, appStr(app, "name"), appStr(app, "domain"))
	if logo := appStr(app, "logo_url"); logo != "" {
		base += ", logo set"
	} else {
		base += ", no logo"
	}
	if len(diffs) == 0 {
		return base
	}
	return base + "; " + strings.Join(diffs, "; ")
}

func (p *AccessProvisioner) displayGroup() string {
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

// str reads a string field, tolerating absence and a non-string value.
func appStr(m rawApp, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// ─── the API ───────────────────────────────────────────────────────────────

// accessAppsPerPage is the page size requested when listing applications, and
// maxAccessAppPages is a runaway guard — an account with more than 2,000 Access
// applications is a different problem from the one this is solving.
const (
	accessAppsPerPage = 50
	maxAccessAppPages = 40
)

// findApp returns the application serving hostname, or nil if there is none.
//
// A list-and-match rather than a filtered query: Cloudflare's apps endpoint has
// no documented exact-domain filter, and matching here means one definition of
// "this hostname's app" shared by Inspect, Ensure and Teardown. Two apps on one
// hostname is a state this does not try to resolve — the first match wins and
// the caller's plan will describe it, which is more useful than an error that
// stops the run.
//
// # Why this pages, and why "not found" is the dangerous answer
//
// Reading only the first page would make a miss indistinguishable from an
// absence. Ensure turns "no application for this hostname" straight into a
// create, so an app sitting on page two would get a **second** application
// created over the top of it — the exact opposite of the already-exists-is-
// success rule this package is required to hold. And it is self-concealing: the
// two applications can then drift apart, one admitting the members group and one
// not, with nothing reporting it until a run happens to read the other page.
//
// Pagination is driven by `result_info.total_pages`, Cloudflare's documented v4
// list envelope, and PRSR-38 confirmed the live list really sends it:
// {"page":1,"per_page":1000,"count":10,"total_count":10,"total_pages":1}. The
// default page size is **1000**, so this account's ten applications will never
// paginate in practice — the loop is correct rather than exercised, which is
// worth knowing before anybody deletes it as dead. That is also why it keys on
// `total_pages` being present and greater than one rather than on "the page came
// back full": an endpoint that ignores the page parameter would return the same
// full page for ever, and a loop that trusted fullness would never terminate.
// Absent result_info means one page, which is the right reading of an endpoint
// that does not paginate.
func (p *AccessProvisioner) findApp(ctx context.Context, hostname string) (rawApp, error) {
	for page := 1; page <= maxAccessAppPages; page++ {
		path := fmt.Sprintf("/accounts/%s/access/apps?page=%d&per_page=%d", p.cfg.AccountID, page, accessAppsPerPage)
		raw, err := p.api.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var env struct {
			Result     []rawApp `json:"result"`
			ResultInfo struct {
				Page       int `json:"page"`
				TotalPages int `json:"total_pages"`
			} `json:"result_info"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("cloudflare: decode applications: %w", err)
		}
		for _, app := range env.Result {
			if domainHost(appStr(app, "domain")) == hostname {
				return app, nil
			}
		}
		if env.ResultInfo.TotalPages <= 1 || page >= env.ResultInfo.TotalPages {
			return nil, nil
		}
	}
	// Ran out of pages to read. Reported rather than answered: "not found" here
	// would send Ensure off to create a duplicate, which is the one outcome this
	// whole function exists to avoid.
	return nil, fmt.Errorf("cloudflare: gave up listing applications for %s after %d pages — refusing to report it absent on a partial read", hostname, maxAccessAppPages)
}

func (p *AccessProvisioner) createApp(ctx context.Context, body rawApp) (rawApp, error) {
	path := fmt.Sprintf("/accounts/%s/access/apps", p.cfg.AccountID)
	raw, err := p.api.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	return decodeApp(raw)
}

// updateApp replaces the application. PUT, not PATCH — see rawApp.
func (p *AccessProvisioner) updateApp(ctx context.Context, id string, body rawApp) (rawApp, error) {
	path := fmt.Sprintf("/accounts/%s/access/apps/%s", p.cfg.AccountID, id)
	raw, err := p.api.do(ctx, http.MethodPut, path, body)
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
		return nil, fmt.Errorf("cloudflare: decode application: %w", err)
	}
	return env.Result, nil
}
