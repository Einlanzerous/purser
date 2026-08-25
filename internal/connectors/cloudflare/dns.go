package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

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

// ttlAuto is Cloudflare's "automatic" TTL. Required for a proxied record, and
// the right answer for an unproxied one too — the spec has no opinion about
// caching, so it does not get one here either.
const ttlAuto = 1

// perPage bounds the name lookup. See records() for why a full page is refused
// rather than read.
const perPage = 100

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
		if !want.Proxied {
			// A direct spec has no opinion about proxying, so an update that is
			// really about the record's *value* must not also switch the orange
			// cloud off on a service that is running behind it.
			write.Proxied = got.Proxied
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

// wrongName reports that what Cloudflare wrote does not answer for the hostname
// that was asked for.
//
// Cloudflare treats a record name that is not already inside the zone as
// *relative to it* and silently appends the zone, so a spec whose hostname
// belongs to some other domain produces `svc.example.org.zerogravity.industries`
// — a live record for a name nobody asked about, and no error anywhere.
// ServiceSpec.validHostname cannot catch it: that checks the shape of a
// hostname, not which zone the token points at. This is the first moment the
// real zone is knowable, because a token scoped to Zone → DNS → Edit cannot read
// the zone object itself — only the records in it, which carry its name.
func wrongName(got, want dnsRecord) error {
	if strings.EqualFold(trimDot(got.Name), trimDot(want.Name)) {
		return nil
	}
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
	if err := p.delete(ctx, p.zoneOf(stray), stray.ID); err != nil && !notFound(err) {
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
func (p *DNSProvisioner) Teardown(ctx context.Context, _ spinup.Target, rec model.ServiceResource) error {
	if !p.configured() {
		return p.unavailable()
	}
	if strings.TrimSpace(rec.ExternalID) == "" {
		return fmt.Errorf("cloudflare: no DNS record id was recorded for %s — Purser deletes only ids it recorded, since a record found by name may be one somebody created by hand",
			rec.Hostname)
	}
	// The recorded parent, not today's config: a zone id read from configuration
	// answers "where would we write now", and a teardown needs "where did this
	// actually go" (migration 0007 records it for exactly this reason).
	zone := firstNonEmpty(rec.ParentID, p.cfg.ZoneID)

	got, err := p.recordByID(ctx, zone, rec.ExternalID)
	switch {
	case notFound(err):
		return nil // already gone
	case err != nil:
		return err
	}
	if !strings.EqualFold(trimDot(got.Name), trimDot(rec.Hostname)) {
		// The id outlived what it referred to. Deleting whatever it names now
		// would remove a record for a hostname nobody asked about.
		return fmt.Errorf("cloudflare: recorded DNS record %s answers for %q, not %q — refusing to delete a record that is no longer this hostname's",
			rec.ExternalID, got.Name, rec.Hostname)
	}
	if err := p.delete(ctx, zone, rec.ExternalID); err != nil && !notFound(err) {
		return err
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
// Refusing is the whole point. The orchestrator turns an error here into
// `unknown` and does not act on it, so an ambiguous name costs a re-run after a
// human looks — where a guess costs whichever record was not this service's.
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
	return dnsRecord{}, fmt.Errorf("cloudflare: %d records already answer for %s (%s) and none matches the spec — Purser will not guess which one is this service's; resolve it in the Cloudflare dashboard",
		len(found), want.Name, describeAll(found))
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

// zoneOf prefers the zone Cloudflare reported over the configured one, so the
// recorded parent describes where the record actually is.
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
