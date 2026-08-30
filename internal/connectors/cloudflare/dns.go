package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

// This file is the service spin-up axis's DNS step (PRSR-28): the zone record
// that makes a new service's hostname resolve. It is a spinup.ServiceProvisioner,
// not a connector.Connector — the two axes share an ethos and no types, and the
// difference is visible right here. The Access connector above provisions a
// *person* into a service and is idempotent per (person × service); this
// provisions a *hostname* and is idempotent per (hostname, kind).
//
// It lives in this package because it speaks to the same API through the same
// transport (client.go). It does not share Config: the zone coordinates
// deliberately stay out of the Access connector's readiness check, because
// folding them in would take `--to cloudflare` offline for every deployment that
// hasn't set them (see internal/config).
//
// The two record shapes are the whole tunnelled/direct split at this step:
//
//	tunnelled → PROXIED CNAME → <tunnel-id>.cfargotunnel.com
//	direct    → A / AAAA / CNAME → the static endpoint
//
// The orange cloud on the tunnelled path is not a preference (SERV-45). An
// unproxied CNAME to cfargotunnel.com does not route at all: the tunnel is
// reachable only from Cloudflare's edge, so a record that bypasses the edge
// resolves to something nothing can connect to. It is checked, and a record that
// has it switched off is a mismatch rather than a match.

// DNSConfig configures the DNS provisioner. It is a separate struct from Config
// on purpose — see the package note above.
type DNSConfig struct {
	// APIToken needs Zone → DNS → Edit, scoped to the zone (PRSR-11 probed
	// exactly this with a real create/read/delete). Edit subsumes Read in
	// Cloudflare's model, so there is no second scope for the read path.
	APIToken string
	// ZoneID is the zone records are created in (PURSER_CF_ZONE_ID).
	ZoneID string

	HTTPClient *http.Client
}

// DNSProvisioner manages the zone record for a service's hostname.
type DNSProvisioner struct {
	cfg DNSConfig
	api *client

	// zoneMu guards zoneName, and is held across the read so a burst of steps
	// asks Cloudflare once rather than once each.
	zoneMu sync.Mutex
	// zoneName is the zone id resolved to its name, memoised after the first
	// successful read. See zone() for why only successes are kept, and why this
	// is not a DNSConfig field.
	zoneName string
}

// Compile-time proof this is the shape the orchestrator walks. Without it a
// signature drift would surface at the composition root rather than here.
var _ spinup.ServiceProvisioner = (*DNSProvisioner)(nil)

// NewDNS builds the DNS provisioner. Like the Access connector it never fails on
// missing credentials: an unconfigured provisioner reports spinup.ErrUnavailable
// from every method, so a plan shows the DNS step as unavailable rather than
// promising a record it cannot write.
func NewDNS(cfg DNSConfig) *DNSProvisioner {
	return &DNSProvisioner{cfg: cfg, api: newClient(cfg.APIToken, cfg.HTTPClient)}
}

func (p *DNSProvisioner) Kind() model.ResourceKind { return model.ResourceDNSRecord }
func (p *DNSProvisioner) DisplayName() string      { return "DNS record" }

func (p *DNSProvisioner) configured() bool {
	return p.cfg.APIToken != "" && p.cfg.ZoneID != ""
}

func (p *DNSProvisioner) unavailable() error {
	return fmt.Errorf("%w: set PURSER_CF_API_TOKEN (Zone → DNS → Edit, scoped to the zone) and PURSER_CF_ZONE_ID to manage DNS records",
		spinup.ErrUnavailable)
}

// tunnelSuffix is what a cloudflared tunnel's hostname CNAME points at.
const tunnelSuffix = ".cfargotunnel.com"

// ttlAuto is Cloudflare's "automatic" TTL, and the value every record this
// package *creates* gets: required for a proxied one, and the right default for
// an unproxied one, since the spec has no opinion about caching.
//
// Which is exactly why it is a create-time default and not an update-time one —
// see Ensure, where a direct record's existing TTL is carried across instead.
const ttlAuto = 1

// perPage bounds the name lookup. See records() for why a full page is refused
// rather than read.
const perPage = 100

// errCodeRecordNotFound is Cloudflare's "Record does not exist." It is the only
// code listed here on purpose: it decides that a teardown succeeded, so a code
// guessed rather than observed could turn an unrelated failure into a reported
// deletion. Add one when a real response shows it, not before.
const errCodeRecordNotFound = 81044

// dnsRecordNotFound reports whether err is Cloudflare saying *this DNS record*
// isn't there.
//
// It lives here rather than in client.go, and it says "dnsRecord" rather than
// "notFound", because the answer is per-product: 81044 is DNS's code and means
// nothing to an Access application or a tunnel route. A generally-named helper
// in the shared client would be reached for by the two provisioners that share
// it and would answer false for ever — safe, since a teardown of something
// already gone would merely report a retryable error, but silently wrong.
//
// The code decides it, and a bare 404 deliberately does not. A 404 is also the
// answer when the *request* could not be routed — a zone id in a recorded
// parent that the current token can no longer address, a base URL that moved —
// and reading that as "already gone" is how Teardown comes to report a deletion
// it never performed, leaving a live record recorded as removed. That is
// `revoked-not-recorded`'s neighbour from PRSR-17, and it is worse than the
// error it replaces: an error is retried, and a re-run reads the row as already
// removed.
//
// So the two mistakes are not symmetric, and this leans the safe way. Being
// wrong here means a genuinely-absent record reports as a failure — noisy,
// retryable, and visible. Being wrong the other way is silent and permanent.
func dnsRecordNotFound(err error) bool {
	code, ok := errorCode(err)
	return ok && code == errCodeRecordNotFound
}

// dnsRecord is the subset of a Cloudflare DNS record this axis reads.
type dnsRecord struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	Proxied  bool   `json:"proxied"`
	TTL      int    `json:"ttl"`
	ZoneID   string `json:"zone_id"`
	ZoneName string `json:"zone_name"`
}

// recordBody is what a create or an update sends. Deliberately narrower than dnsRecord: sending
// back the read-only fields (id, zone_id, zone_name) on a create or a patch
// invites Cloudflare to reject the whole request over a field Purser does not own.
type recordBody struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
}

func writeBody(r dnsRecord) recordBody {
	return recordBody{Type: r.Type, Name: r.Name, Content: r.Content, Proxied: r.Proxied, TTL: r.TTL}
}

// desiredRecord is the record the spec calls for — the one place the two shapes
// are decided, so Inspect's comparison and Ensure's write cannot disagree about
// what "correct" means.
//
// It revalidates the spec rather than trusting the caller. The orchestrator has
// already done so, making this a no-op there; the cost of being wrong is a
// record written for an un-normalized hostname, which becomes this axis's
// identity key.
func desiredRecord(t spinup.Target) (dnsRecord, error) {
	spec, err := t.Spec.Validate()
	if err != nil {
		return dnsRecord{}, err
	}
	switch spec.Mode {
	case spinup.ModeTunnelled:
		if strings.TrimSpace(t.TunnelID) == "" {
			// The orchestrator resolves the ref before any step runs, precisely
			// so the ingress route and this record name the same tunnel. Reaching
			// here means something called the provisioner directly; guessing a
			// tunnel would publish a hostname pointing into the wrong one.
			return dnsRecord{}, fmt.Errorf("cloudflare: %s spec for %s has no resolved tunnel id — the record would have nothing to point at",
				spinup.ModeTunnelled, spec.Hostname)
		}
		return dnsRecord{
			Type:    "CNAME",
			Name:    spec.Hostname,
			Content: t.TunnelID + tunnelSuffix,
			// Not negotiable on this path — see the package note.
			Proxied: true,
			TTL:     ttlAuto,
		}, nil
	case spinup.ModeDirect:
		rec := dnsRecord{Name: spec.Hostname, Content: spec.Upstream, TTL: ttlAuto}
		switch ip := net.ParseIP(spec.Upstream); {
		case ip == nil:
			rec.Type = "CNAME"
		case ip.To4() != nil:
			rec.Type = "A"
		default:
			rec.Type = "AAAA"
		}
		// Unproxied. "Direct" means the endpoint is already reachable and the
		// record just says where it is; routing it through the edge would change
		// how the service is reached — non-HTTP ports stop working, and a
		// long-lived connection acquires a proxy in the middle. A direct service
		// that wants the orange cloud is a spec field somebody adds, not a
		// default guessed here. Note that this is the *create* value only:
		// recordMatches and Ensure both leave an existing record's proxy setting
		// alone on this path, because the spec expresses no opinion about it.
		rec.Proxied = false
		return rec, nil
	}
	return dnsRecord{}, fmt.Errorf("cloudflare: unknown mode %q for %s", spec.Mode, spec.Hostname)
}

// recordMatches reports whether an existing record already satisfies the spec.
func recordMatches(got, want dnsRecord) bool {
	if !strings.EqualFold(got.Type, want.Type) {
		return false
	}
	if !sameTarget(got.Content, want.Content) {
		return false
	}
	// The orange cloud is compared only when the spec requires it — that is, on
	// the tunnelled path, where proxying is part of the record working at all.
	// A direct spec says nothing about proxying, so neither does this: whether
	// an existing record sits behind the edge is a decision that predates this
	// axis for every service already up, and reporting it as drift would have
	// --apply quietly flip a live service's traffic path.
	return !want.Proxied || got.Proxied
}

// sameTarget compares two record values the way DNS does: case-insensitively,
// and ignoring a trailing dot Cloudflare may or may not echo back.
func sameTarget(a, b string) bool {
	return strings.EqualFold(trimDot(a), trimDot(b))
}

func trimDot(s string) string { return strings.TrimRight(strings.TrimSpace(s), ".") }

// Inspect reports what is at the hostname now, reading only. It is this axis's
// reconcile: the plan an operator reads and the writes --apply performs are both
// decided from this one answer, so a version that repaired anything would
// destroy the difference it exists to report.
//
// A lookup that fails returns an error and never an empty State. The
// orchestrator turns that into `unknown` and declines to act on it, which is the
// point: "no record" and "could not ask" differ by a duplicate record.
func (p *DNSProvisioner) Inspect(ctx context.Context, t spinup.Target) (spinup.State, error) {
	if !p.configured() {
		return spinup.State{}, p.unavailable()
	}
	want, err := desiredRecord(t)
	if err != nil {
		return spinup.State{}, err
	}
	// Before the lookup, not after: an out-of-zone hostname has no records to
	// read and the plan should say why rather than report "create".
	if err := p.preflight(ctx, want.Name); err != nil {
		return spinup.State{}, err
	}
	found, err := p.records(ctx, want.Name)
	if err != nil {
		return spinup.State{}, err
	}
	if len(found) == 0 {
		// Detail describes what the spec wants, since there is nothing there to
		// describe and "create — proxied CNAME → …" is the line worth reading.
		return spinup.State{Detail: describeRecord(want)}, nil
	}
	if got, ok := pickMatch(found, want); ok {
		return spinup.State{
			Exists: true, Matches: true,
			ExternalID: got.ID,
			ParentID:   p.zoneOf(got),
			Detail:     describeRecord(got),
		}, nil
	}
	got, err := pickCandidate(found, want)
	if err != nil {
		return spinup.State{}, err
	}
	return spinup.State{
		Exists: true, Matches: false,
		ExternalID: got.ID,
		ParentID:   p.zoneOf(got),
		Detail:     fmt.Sprintf("%s; the spec wants %s", describeRecord(got), describeRecord(want)),
	}, nil
}

// Ensure makes the zone record match the spec and returns what now exists.
//
// It looks the name up itself rather than trusting Inspect's answer, so it holds
// to the interface's "safe to call when it already exists" rule on its own: an
// existing record with the right target is a success and no write at all, which
// is what makes a failed-only re-run of a spin-up harmless.
func (p *DNSProvisioner) Ensure(ctx context.Context, t spinup.Target) (spinup.Resource, error) {
	if !p.configured() {
		return spinup.Resource{}, p.unavailable()
	}
	want, err := desiredRecord(t)
	if err != nil {
		return spinup.Resource{}, err
	}
	// Ensure asks for itself rather than trusting Inspect's answer, exactly as
	// it re-runs the lookup below: an apply must refuse on its own account, and
	// the zone name is memoised, so on the ordinary path this is free.
	if err := p.preflight(ctx, want.Name); err != nil {
		return spinup.Resource{}, err
	}
	found, err := p.records(ctx, want.Name)
	if err != nil {
		return spinup.Resource{}, err
	}
	if got, ok := pickMatch(found, want); ok {
		return p.resource(got), nil // already correct — no write
	}
	if len(found) > 0 {
		got, err := pickCandidate(found, want)
		if err != nil {
			return spinup.Resource{}, err
		}
		write := want
		if t.Spec.Mode == spinup.ModeDirect {
			// A direct spec pins the record's *value* and nothing else, so an
			// update about the value must carry the rest across rather than
			// reset it to this file's create-time defaults. Proxying, because
			// switching the orange cloud off changes how a running service is
			// reached; TTL, because a human may have set 300 deliberately and
			// ttlAuto would quietly overwrite it. Neither is compared by
			// recordMatches or printed by describeRecord, so an operator
			// approving the plan would never have seen either change.
			write.Proxied, write.TTL = got.Proxied, got.TTL
		}
		updated, err := p.patch(ctx, got.ID, write)
		if err != nil {
			return spinup.Resource{}, err
		}
		if err := wrongName(updated, want); err != nil {
			// Not cleaned up, unlike the create path below: this record existed
			// before Purser touched it, so removing it would destroy something
			// nobody asked to have removed.
			return spinup.Resource{}, err
		}
		return p.resource(updated), nil
	}

	created, err := p.create(ctx, want)
	if err != nil {
		// "Already exists" upstream is success, not a conflict — the house rule
		// on the person axis, and it applies to the window between the lookup
		// above and this call. Only a record that actually matches the spec
		// counts: anything else is still the error Cloudflare returned.
		if got, ok := p.matchAfterConflict(ctx, want); ok {
			return p.resource(got), nil
		}
		return spinup.Resource{}, err
	}
	if err := wrongName(created, want); err != nil {
		return spinup.Resource{}, p.removeStray(ctx, created, err)
	}
	return p.resource(created), nil
}

// --- the zone pre-flight ----------------------------------------------------

// preflight refuses a hostname that is not inside the token's own zone, before
// anything is looked up and long before anything is created (PRSR-39).
//
// Cloudflare treats a record name it does not recognise as *relative* to the
// zone and silently appends it, so a spec naming some other domain produces
// svc.example.org.zerogravity.industries with no error anywhere. Until now the
// only guard was wrongName, which reads the name off a record that has already
// been created and then deletes it. That backstop stays — see wrongName — but
// it is second-best three ways over: the operator learns about it after an
// apply rather than in the plan, a wrong record resolves for the length of two
// API calls, and "Purser deleted a record it had just made" is the most
// alarming line this provisioner can print.
//
// This is available at all only because the premise CLAUDE.md gave for *not*
// doing it was false. It said a Zone → DNS → Edit token cannot read the zone
// object; PRSR-38 probed the production token and this exact call —
// GET /zones/{zone_id} — answered `name: zerogravity.industries, status:
// active`. (Its sibling GET /zones answers ["zerogravity.industries"] too, but
// that is the list endpoint and not the one below; the probe that backs this
// code is the object one.) It is /user/tokens/verify that this token cannot
// call, which is a different endpoint again.
//
// **A zone that could not be read is not evidence the hostname is wrong**, so a
// failed read falls through to today's behaviour rather than refusing. That is
// the same rule as everywhere else on this axis. In practice the same failure
// usually takes the records() lookup with it — an outage, a revoked token — and
// the step then reports `unknown` on its own account. That is a tendency and not
// a guarantee: a failure specific to this route, with the record lookup still
// answering, leaves the pre-flight silently inert and the read re-issued and
// re-discarded on every call, since only successes are memoised. What makes that
// acceptable is not self-healing, it is the create-path backstop, which is
// exactly the state wrongName was the only guard in before this existed.
func (p *DNSProvisioner) preflight(ctx context.Context, hostname string) error {
	zone, err := p.zone(ctx)
	if err != nil {
		return nil
	}
	if inZone(hostname, zone) {
		return nil
	}
	// Refused rather than unknown: the read *succeeded*, and re-running will say
	// this for ever. What needs fixing is neither upstream nor a Purser
	// credential but the spec itself, which is a third thing — but of the three
	// statuses available it is the only one whose sentence to the operator is
	// right, since `unknown` says "re-run" and `unavailable` says "set an env
	// var". The message names the fix.
	return fmt.Errorf("%w: cloudflare: %s is not in %s, the zone PURSER_CF_ZONE_ID points at — Cloudflare would take the name as relative to the zone and create %s.%s instead",
		spinup.ErrRefused, hostname, zone, hostname, zone)
}

// zone resolves the configured zone id to the zone's name, once.
//
// Memoised because a deployment's zone id is fixed and the answer cannot change
// under it, so `purser serve` asks at most once for its lifetime and the CLI at
// most once per run. Only a *success* is cached: a failed read is not an answer,
// and caching it would disable the pre-flight for the rest of the process on the
// strength of one timeout.
//
// Deliberately not a DNSConfig field. The zone *name* must be derived from the
// zone id and never configured alongside it, because two settings that can
// disagree just move the mismatch this exists to catch — a hand-set
// PURSER_CF_ZONE_NAME pointing somewhere the id does not would make the
// pre-flight confidently wrong in both directions.
func (p *DNSProvisioner) zone(ctx context.Context) (string, error) {
	p.zoneMu.Lock()
	defer p.zoneMu.Unlock()
	if p.zoneName != "" {
		return p.zoneName, nil
	}
	raw, err := p.api.do(ctx, http.MethodGet, "/zones/"+p.cfg.ZoneID, nil)
	if err != nil {
		return "", err
	}
	var env struct {
		Result struct {
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("cloudflare: decode zone %s: %w", p.cfg.ZoneID, err)
	}
	name := trimDot(env.Result.Name)
	if name == "" {
		// A 200 that named no zone. Treated as a failed read rather than as an
		// empty zone name, which inZone would otherwise match every hostname
		// against the suffix ".".
		return "", fmt.Errorf("cloudflare: zone %s came back with no name", p.cfg.ZoneID)
	}
	p.zoneName = name
	return name, nil
}

// inZone reports whether hostname is the zone or sits inside it.
//
// The apex counts: a spec may legitimately claim zerogravity.industries itself.
// The suffix test keeps its dot, so "notzerogravity.industries" is outside
// "zerogravity.industries" rather than a match — the same care hostnameTakes
// takes over cloudflared's wildcards, for the same reason.
func inZone(hostname, zone string) bool {
	h, z := strings.ToLower(trimDot(hostname)), strings.ToLower(trimDot(zone))
	return h == z || strings.HasSuffix(h, "."+z)
}

// wrongName reports that what Cloudflare wrote does not answer for the hostname
// that was asked for.
//
// Cloudflare treats a record name that is not already inside the zone as
// *relative to it* and silently appends the zone, so a spec whose hostname
// belongs to some other domain produces `svc.example.org.zerogravity.industries`
// — a live record for a name nobody asked about, and no error anywhere.
// ServiceSpec.validHostname cannot catch it: that checks the shape of a
// hostname, not which zone the token points at.
//
// The justification this comment used to give for checking *afterwards* was
// wrong twice over, and both halves are now measured rather than argued.
//
//   - "a token scoped to Zone → DNS → Edit cannot read the zone object itself" —
//     false. PRSR-38 found the production token answers GET /zones with exactly
//     ["zerogravity.industries"], so a pre-flight is available and would refuse
//     an out-of-zone hostname in the *plan*, before anything exists to delete.
//     PRSR-39 built it: see preflight above, which now runs on both paths and
//     catches the ordinary case — a spec naming another domain — without
//     anything being written.
//   - "only the records in it, which carry its name" — also false, and PRSR-42
//     measured it on all three routes this package reads records from: a create
//     response, a get-by-id, and the **list** that records() uses on every
//     Inspect. None carries `zone_name` or `zone_id` on this API version.
//     dnsRecord decodes both and both are always empty, so the branch below that
//     names the zone never fires and the "configured zone" fallback is the only
//     text this ever prints.
//
// The check stays regardless, and the reason is unchanged by the pre-flight
// landing in front of it. It answers a different question: preflight asks what
// the spec said, this asks what Cloudflare *did*, so it still catches a
// normalisation surprise a pre-flight cannot predict, and it is the only guard
// left when the zone read fails — which is precisely when preflight waves the
// hostname through. It is also the half that has actually been exercised
// against the live API: PRSR-42 asked for prsr42-probe.example.org, got
// prsr42-probe.example.org.zerogravity.industries, and wrongName caught it and
// removeStray deleted it, leaving the zone byte-identical to its snapshot.
func wrongName(got, want dnsRecord) error {
	if strings.EqualFold(trimDot(got.Name), trimDot(want.Name)) {
		return nil
	}
	// Always the fallback today: see the note above — no response on this API
	// version populates zone_name. Kept rather than simplified away, because a
	// field Cloudflare stops omitting should improve this message rather than
	// need it rewritten, and the cost is one branch.
	zone := got.ZoneName
	if zone == "" {
		zone = "the configured zone"
	}
	return fmt.Errorf("cloudflare: asked for %s and Cloudflare wrote %s — %s is not in %s, so the name was taken as relative to it",
		want.Name, got.Name, want.Name, zone)
}

// removeStray deletes a record Purser created one call ago that turned out to
// carry the wrong name, and folds the outcome into the error it reports.
//
// Only ever called on the create path, where the record is provably Purser's and
// provably not what was asked for. Leaving it would put a live record in the zone
// that nothing records — a failed step writes no row — so the id is named when
// the cleanup itself fails, since that message is the only trace of it.
func (p *DNSProvisioner) removeStray(ctx context.Context, stray dnsRecord, cause error) error {
	if err := p.delete(ctx, p.zoneOf(stray), stray.ID); err != nil && !dnsRecordNotFound(err) {
		return fmt.Errorf("%w; the stray record %s could not be removed either (%v) — delete it by hand", cause, stray.ID, err)
	}
	return fmt.Errorf("%w; the stray record was removed", cause)
}

// Teardown deletes the recorded record.
//
// It targets rec.ExternalID and never a name lookup. A record matched by name
// may be one somebody created by hand years ago, and deleting that is not
// recoverable by re-running — the recorded id is the only handle Purser can
// prove it owns. A record already gone is a success, because it is: a teardown
// that reported failure for an absent record would leave a removed resource on
// the books as live, which is offboard's "revoke that didn't happen" (PRSR-17)
// pointed the other way.
func (p *DNSProvisioner) Teardown(ctx context.Context, t spinup.Target, rec model.ServiceResource) (spinup.Removal, error) {
	if err := p.CanTeardown(t); err != nil {
		return spinup.Removal{}, err
	}
	if strings.TrimSpace(rec.ExternalID) == "" {
		return spinup.Removal{}, fmt.Errorf("cloudflare: no DNS record id was recorded for %s — Purser deletes only ids it recorded, since a record found by name may be one somebody created by hand",
			rec.Hostname)
	}
	// The recorded parent, not today's config: a zone id read from configuration
	// answers "where would we write now", and a teardown needs "where did this
	// actually go" (migration 0007 records it for exactly this reason).
	zone := firstNonEmpty(rec.ParentID, p.cfg.ZoneID)

	got, err := p.recordByID(ctx, zone, rec.ExternalID)
	switch {
	case dnsRecordNotFound(err):
		return spinup.Removal{Detail: fmt.Sprintf("DNS record %s was already gone from zone %s", rec.ExternalID, zone)}, nil
	case err != nil:
		return spinup.Removal{}, err
	}
	if !strings.EqualFold(trimDot(got.Name), trimDot(rec.Hostname)) {
		// The id outlived what it referred to. Deleting whatever it names now
		// would remove a record for a hostname nobody asked about.
		return spinup.Removal{}, fmt.Errorf("cloudflare: recorded DNS record %s answers for %q, not %q — refusing to delete a record that is no longer this hostname's",
			rec.ExternalID, got.Name, rec.Hostname)
	}
	if err := p.delete(ctx, zone, rec.ExternalID); err != nil {
		if !dnsRecordNotFound(err) {
			return spinup.Removal{}, err
		}
		// Read a moment ago and absent now. Reported as its own line rather than
		// folded into the ordinary success: somebody else deleted it between the
		// two calls, which is worth an operator seeing on a hostname they are
		// taking down.
		return spinup.Removal{Detail: fmt.Sprintf("DNS record %s (%s %s) went between the read and the delete — it is gone, and something other than this run removed it", rec.ExternalID, got.Type, trimDot(got.Name))}, nil
	}
	return spinup.Removal{Detail: fmt.Sprintf("deleted the %s record for %s (%s) from zone %s", got.Type, trimDot(got.Name), rec.ExternalID, zone)}, nil
}

// CanTeardown answers from configuration alone, so a teardown *plan* can report
// this step honestly without calling anything. Teardown delegates to it rather
// than repeating the check, which is what stops the plan and the apply drifting
// (spinup.TeardownChecker, after connector.CanDeprovision).
func (p *DNSProvisioner) CanTeardown(spinup.Target) error {
	if !p.configured() {
		return p.unavailable()
	}
	return nil
}

// --- lookups and writes ----------------------------------------------------

// records returns every record in the zone whose name is exactly name.
//
// The name filter goes to the API to narrow the answer, and the exact match is
// re-applied here rather than trusted: Cloudflare has spelled this filter more
// than one way across API revisions, and a filter that silently matched loosely
// would have Purser adopt or update a record for a different hostname.
//
// A full page is refused rather than read. No hostname legitimately has a
// hundred records, so a full page means the filter did not narrow anything and
// what came back is page one of the zone — an answer that would read as "no
// record here" and create a second one. Unreadable is not absent.
func (p *DNSProvisioner) records(ctx context.Context, name string) ([]dnsRecord, error) {
	q := url.Values{"name": {name}, "per_page": {fmt.Sprint(perPage)}}
	path := fmt.Sprintf("/zones/%s/dns_records?%s", p.cfg.ZoneID, q.Encode())
	raw, err := p.api.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var env struct {
		Result []dnsRecord `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("cloudflare: decode dns records for %s: %w", name, err)
	}
	if len(env.Result) >= perPage {
		return nil, fmt.Errorf("cloudflare: the lookup for %s came back a full page (%d records), so the name filter did not narrow it — refusing to read a truncated answer as the state of the hostname",
			name, len(env.Result))
	}
	var out []dnsRecord
	for _, r := range env.Result {
		if strings.EqualFold(trimDot(r.Name), trimDot(name)) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (p *DNSProvisioner) recordByID(ctx context.Context, zone, id string) (dnsRecord, error) {
	raw, err := p.api.do(ctx, http.MethodGet, fmt.Sprintf("/zones/%s/dns_records/%s", zone, id), nil)
	if err != nil {
		return dnsRecord{}, err
	}
	return decodeRecord(raw, "read dns record "+id)
}

func (p *DNSProvisioner) create(ctx context.Context, want dnsRecord) (dnsRecord, error) {
	raw, err := p.api.do(ctx, http.MethodPost, fmt.Sprintf("/zones/%s/dns_records", p.cfg.ZoneID), writeBody(want))
	if err != nil {
		return dnsRecord{}, err
	}
	return decodeRecord(raw, "create dns record for "+want.Name)
}

// patch updates an existing record in place. PATCH rather than PUT so the fields
// Purser does not own — comments, tags, whatever a human set in the dashboard —
// survive an update to the record's value.
func (p *DNSProvisioner) patch(ctx context.Context, id string, want dnsRecord) (dnsRecord, error) {
	raw, err := p.api.do(ctx, http.MethodPatch, fmt.Sprintf("/zones/%s/dns_records/%s", p.cfg.ZoneID, id), writeBody(want))
	if err != nil {
		return dnsRecord{}, err
	}
	return decodeRecord(raw, "update dns record "+id)
}

func (p *DNSProvisioner) delete(ctx context.Context, zone, id string) error {
	_, err := p.api.do(ctx, http.MethodDelete, fmt.Sprintf("/zones/%s/dns_records/%s", zone, id), nil)
	return err
}

func decodeRecord(raw []byte, op string) (dnsRecord, error) {
	var env struct {
		Result dnsRecord `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return dnsRecord{}, fmt.Errorf("cloudflare: decode %s: %w", op, err)
	}
	return env.Result, nil
}

// matchAfterConflict re-reads the name after a create failed and reports a
// record that now satisfies the spec. A read that fails here is not an error of
// its own: the caller still has the create's error, which is the more useful one.
func (p *DNSProvisioner) matchAfterConflict(ctx context.Context, want dnsRecord) (dnsRecord, bool) {
	found, err := p.records(ctx, want.Name)
	if err != nil {
		return dnsRecord{}, false
	}
	return pickMatch(found, want)
}

// --- choosing among what is there ------------------------------------------

// pickMatch returns a record that already satisfies the spec.
//
// Other records at the same name are not a problem for it: a dual-stack service
// with an A and an AAAA has both, and the spec claims one record rather than
// exclusive ownership of the name.
func pickMatch(found []dnsRecord, want dnsRecord) (dnsRecord, bool) {
	for _, r := range found {
		if recordMatches(r, want) {
			return r, true
		}
	}
	return dnsRecord{}, false
}

// pickCandidate chooses which existing record an update would change, and
// refuses when the answer is not one record.
//
// Refusing is the whole point: a guess costs whichever record was not this
// service's.
//
// The refusal carries spinup.ErrRefused, so the orchestrator reports `refused`
// rather than `unknown` (PRSR-31). The read *succeeded* — several records
// genuinely answer for this name — and nothing changes until a human edits the
// zone, which is what the message already says. Reported as `unknown` it drew
// "could not be read, so nothing was decided from it — re-run", and re-running
// reprints it for ever: the exact sentence the refused/unknown split exists to
// stop printing.
//
// records()'s full-page refusal is deliberately on the other side of that line
// and stays `unknown`: there the filter narrowed nothing, so the answer really
// was not read, and a re-run is the fix.
func pickCandidate(found []dnsRecord, want dnsRecord) (dnsRecord, error) {
	var sameType []dnsRecord
	for _, r := range found {
		if strings.EqualFold(r.Type, want.Type) {
			sameType = append(sameType, r)
		}
	}
	switch {
	case len(sameType) == 1:
		return sameType[0], nil
	case len(sameType) == 0 && len(found) == 1:
		// One record of the wrong type — an A where the spec wants a CNAME, say.
		// That is a type change of this hostname's record, not an ambiguity: the
		// name has exactly one record and it is the one the spec is about.
		return found[0], nil
	}
	return dnsRecord{}, fmt.Errorf("%w: cloudflare: %d records already answer for %s (%s) and none matches the spec — Purser will not guess which one is this service's; resolve it in the Cloudflare dashboard",
		spinup.ErrRefused, len(found), want.Name, describeAll(found))
}

// --- rendering -------------------------------------------------------------

// describeRecord is the line an operator reads in a plan, e.g.
// "proxied CNAME → aef21667….cfargotunnel.com".
func describeRecord(r dnsRecord) string {
	proxy := "DNS only"
	if r.Proxied {
		proxy = "proxied"
	}
	return fmt.Sprintf("%s %s → %s", proxy, r.Type, r.Content)
}

func describeAll(rs []dnsRecord) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = describeRecord(r)
	}
	return strings.Join(parts, "; ")
}

// resource is what Ensure hands back for the resource row.
func (p *DNSProvisioner) resource(r dnsRecord) spinup.Resource {
	return spinup.Resource{ExternalID: r.ID, ParentID: p.zoneOf(r), Detail: describeRecord(r)}
}

// zoneOf is the zone a record lives in — the record's own, falling back to the
// configured one.
//
// In practice always the fallback. PRSR-42 measured all three routes this
// package reads records from — create, get-by-id, and the **list** that records()
// uses on every Inspect — and none carries `zone_id` on this API version, so
// r.ZoneID is invariably empty. That is why the stray-removal path works at all,
// and it is worth knowing before anyone reads the first operand as load-bearing:
// the sentence that stood here said zoneOf "prefers the zone Cloudflare
// reported", which is exactly the misreading.
func (p *DNSProvisioner) zoneOf(r dnsRecord) string {
	return firstNonEmpty(r.ZoneID, p.cfg.ZoneID)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
