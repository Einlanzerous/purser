package cloudflare

// The tunnel ingress-route provisioner (PRSR-30) — the spin-up axis's third
// step, and the one with the blast radius.
//
//	GET /accounts/{acct}/cfd_tunnel/{id}/configurations  -> the whole document
//	PUT /accounts/{acct}/cfd_tunnel/{id}/configurations  -> replace it
//
// PRSR-26 is what made this an ordinary API client: the tunnel is
// remotely-managed (`source: "cloudflare"`), so its routes are settable over the
// API with no migration and no file access to the cloudflared host.
//
// # Two hazards, and they are not the same one
//
// **A lost update silently unroutes other services.** There is no per-hostname
// write. Adding one route means reading a single document that holds *every*
// hostname on the tunnel, appending to it, and putting the whole thing back — so
// a stale read doesn't corrupt this service's route, it deletes somebody else's.
// Three guards, because they cover different windows:
//
//   - docMu serializes the read-modify-write, exactly as groupMu does for the
//     Access group's include list. Ensure and Teardown take their *own* fresh
//     read inside the lock and never build a write on the plan's Inspect, which
//     ran outside it.
//   - Every key the document carries is written back verbatim — warp-routing,
//     the tunnel-wide originRequest, a per-rule originRequest, and anything this
//     build has never heard of. A PUT replaces the document, so what it omits is
//     what the tunnel loses. That is why rules are held as raw JSON per key
//     rather than decoded into a struct: a field we don't model is a field we
//     can still hand back byte-for-byte.
//   - The version the read-back reports is checked against the version we read.
//     Cloudflare bumps it once per write, so anything but +1 means another
//     writer moved the document between our read and our PUT — and *that* is the
//     one case confirming our own route cannot catch, because our write
//     necessarily contains everything our own read did.
//
// **The catch-all rule must stay last.** cloudflared requires the ingress list
// to end in a rule matching everything — no hostname, no path, typically
// `http_status:404`. A rule appended *after* it is never matched and nothing
// anywhere reports an error: the route simply does not work. So a new rule is
// inserted *before* the terminal one, the terminal one is asserted still last
// before the PUT and again after it, and a document that doesn't have that shape
// is refused rather than rewritten on a guess.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

// catchAllService is what a terminal rule serves when this package has to supply
// one: cloudflared's own convention for "matched nothing".
const catchAllService = "http_status:404"

// TunnelConfig configures the ingress-route provisioner.
//
// It is deliberately not the Access connector's Config. The two share a token
// and an account and nothing else, and folding the group fields in would make
// the ingress route's readiness depend on an Access group it never touches —
// the same reasoning that keeps ZoneID and TunnelID out of that Config in
// internal/config.
//
// There is no tunnel id here either, and that is the point of PRSR-33: the
// tunnel is a property of the spec, resolved once per run by the orchestrator
// and handed over on Target, so the ingress route and the DNS record cannot end
// up describing different tunnels.
type TunnelConfig struct {
	APIToken  string // scoped Account → Cloudflare Tunnel → Edit (PRSR-11, probed)
	AccountID string // Cloudflare account id

	HTTPClient *http.Client
}

// TunnelProvisioner routes a tunnelled hostname to its internal upstream by
// adding a rule to a cloudflared tunnel's ingress configuration.
//
// It is a spinup.ServiceProvisioner, not a connector.Connector: it provisions
// the infrastructure that makes a service exist, keyed on hostname, and knows
// nothing about people.
type TunnelProvisioner struct {
	cfg TunnelConfig
	api *client
	// docMu serializes the ingress document's read-modify-write, the way
	// groupMu does for the Access group's include list — same hazard, same
	// shape, and here the thing a lost update destroys is another service's
	// route rather than another person's access.
	//
	// Cloudflare's configuration API offers no conditional write, so this
	// guards one process. The version check in Ensure is what notices when the
	// other writer was a different one.
	docMu sync.Mutex
}

// NewTunnel builds the provisioner. Like the Access connector it never fails on
// missing credentials: an unconfigured provisioner is a valid one that reports
// every step unavailable.
func NewTunnel(cfg TunnelConfig) *TunnelProvisioner {
	return &TunnelProvisioner{cfg: cfg, api: newClient(cfg.APIToken, cfg.HTTPClient)}
}

func (p *TunnelProvisioner) Kind() model.ResourceKind { return model.ResourceTunnelRoute }
func (p *TunnelProvisioner) DisplayName() string      { return "Cloudflare Tunnel ingress route" }

func (p *TunnelProvisioner) configured() bool {
	return p.cfg.APIToken != "" && p.cfg.AccountID != ""
}

// unavailable is the "registered but cannot act" answer, in the spin-up axis's
// own sentinel — so an unconfigured deployment previews as `unavailable` rather
// than as a breakage, and never as "the route is missing".
func (p *TunnelProvisioner) unavailable() error {
	return fmt.Errorf("%w: cloudflare is not configured (set PURSER_CF_API_TOKEN and PURSER_CF_ACCOUNT_ID); add the ingress rule for the hostname on the tunnel in the Zero Trust dashboard (Networks → Tunnels → Public Hostnames)",
		spinup.ErrUnavailable)
}

// usable reports whether this provisioner can act on t at all.
//
// A missing tunnel id is a plain error rather than ErrUnavailable: Ensure
// resolves the ref before any step runs and refuses the whole request when it
// can't, so reaching here without one means a caller asked the ingress step
// about a spec that has no tunnel — a wiring mistake, not a deployment that
// hasn't been configured.
func (p *TunnelProvisioner) usable(t spinup.Target) error {
	if !p.configured() {
		return p.unavailable()
	}
	if t.TunnelID == "" {
		return fmt.Errorf("cloudflare: no tunnel resolved for %s — an ingress route needs one, and only a %s spec has one",
			t.Spec.Hostname, spinup.ModeTunnelled)
	}
	return nil
}

// Inspect reports whether the hostname is routed on the tunnel, and to where.
// It reads the configuration and nothing else — a failed read is the
// orchestrator's `unknown`, never "the route is missing", because answering
// absent to a question you couldn't ask is how a re-run adds a route that is
// already there.
func (p *TunnelProvisioner) Inspect(ctx context.Context, t spinup.Target) (spinup.State, error) {
	if err := p.usable(t); err != nil {
		return spinup.State{}, err
	}
	doc, err := p.getConfig(ctx, t.TunnelID)
	if err != nil {
		return spinup.State{}, err
	}
	return inspectIngress(doc.ingress, t), nil
}

// inspectIngress turns a fetched ingress list into a State. Split out from
// Inspect so the shape of the answer is testable without a server, and so the
// Detail an operator reads in the plan is written in one place.
func inspectIngress(rules []ingressRule, t spinup.Target) spinup.State {
	// ParentID is the tunnel, and ExternalID stays empty by nature: the
	// configuration is one document per tunnel, so a route has no id of its own
	// and is identified by (tunnel, hostname). Both are set on this path and on
	// Ensure's, because the orchestrator adopts on a disagreement between them
	// and the recorded row.
	st := spinup.State{ParentID: t.TunnelID}

	idx, dups := findRoute(rules, t.Spec.Hostname)
	if idx < 0 {
		st.Detail = fmt.Sprintf("no ingress rule for %s on tunnel %s (%d rules)", t.Spec.Hostname, t.TunnelID, len(rules))
		return st
	}

	st.Exists = true
	svc := rules[idx].str("service")
	st.Matches = svc == t.Spec.Upstream
	if st.Matches {
		st.Detail = fmt.Sprintf("ingress rule %d of %d on tunnel %s → %s", idx+1, len(rules), t.TunnelID, svc)
	} else {
		st.Detail = fmt.Sprintf("ingress rule %d of %d on tunnel %s → %s, want %s", idx+1, len(rules), t.TunnelID, svc, t.Spec.Upstream)
	}
	if dups > 0 {
		// Reported rather than repaired. cloudflared matches the first rule, so
		// the route works and the duplicates are inert — but they are somebody's
		// hand edit, and silently deleting rules this provisioner did not write
		// is not a thing a re-run can undo.
		st.Detail += fmt.Sprintf(" (%d further rule(s) carry this hostname; cloudflared matches the first)", dups)
	}
	return st
}

// Ensure adds or repoints the hostname's ingress rule, and returns what the
// tunnel now serves.
//
// Idempotent by the axis's rule: a hostname already routed to the right
// upstream is a success with no write at all.
func (p *TunnelProvisioner) Ensure(ctx context.Context, t spinup.Target) (spinup.Resource, error) {
	if err := p.usable(t); err != nil {
		return spinup.Resource{}, err
	}

	p.docMu.Lock()
	defer p.docMu.Unlock()

	// A fresh read, inside the lock, and deliberately not the one the plan was
	// decided from. Inspect ran outside this lock and may be minutes old; a PUT
	// built on it is precisely the stale read that drops another service's
	// route. The plan decides *whether* to act, this read decides *what to
	// write*.
	before, err := p.getConfig(ctx, t.TunnelID)
	if err != nil {
		return spinup.Resource{}, err
	}

	next, changed, detail, err := planRoute(before.ingress, t.Spec.Hostname, t.Spec.Upstream)
	if err != nil {
		return spinup.Resource{}, err
	}
	if !changed {
		// Already correct — possibly because another run added it between the
		// plan and here. Nothing to write, and saying so is the honest Detail.
		return spinup.Resource{ParentID: t.TunnelID, Detail: detail}, nil
	}
	if _, err := terminalIndex(next); err != nil {
		// Belt and braces on our own arithmetic: the document we are about to
		// send must still end in the rule that matches everything. A rule after
		// it never matches and nothing errors, so this is the one class of bug
		// that cannot be found by watching it fail.
		return spinup.Resource{}, fmt.Errorf("cloudflare: refusing to write the ingress configuration for tunnel %s: %w", t.TunnelID, err)
	}

	if err := p.putConfig(ctx, t.TunnelID, before.config, next); err != nil {
		return spinup.Resource{}, err
	}

	after, err := p.getConfig(ctx, t.TunnelID)
	if err != nil {
		// The write went out and we cannot confirm it. Reported as a failure
		// rather than a success, because the alternative is recording a route
		// nobody has seen: a re-run re-reads, finds it, and adopts.
		return spinup.Resource{}, fmt.Errorf("cloudflare: the ingress configuration for %s was written but could not be read back to confirm — re-run to see what landed: %w", t.Spec.Hostname, err)
	}
	if err := confirmRoute(after.ingress, t.Spec.Hostname, t.Spec.Upstream); err != nil {
		return spinup.Resource{}, err
	}
	if note := concurrentWriteNote(before.version, after.version, t.TunnelID); note != "" {
		// Not an error: the route above is confirmed in place, so this step did
		// what it said. What may have been lost is somebody *else's* route, and
		// the only useful response is to say so loudly in both places an
		// operator looks.
		log.Printf("spinup: %s", note)
		detail += " — " + note
	}
	return spinup.Resource{ParentID: t.TunnelID, Detail: detail}, nil
}

// Teardown removes the hostname's ingress rule from the tunnel it was recorded
// on. Idempotent: a route already gone is a success, and makes no write.
func (p *TunnelProvisioner) Teardown(ctx context.Context, t spinup.Target, rec model.ServiceResource) error {
	if !p.configured() {
		return p.unavailable()
	}
	// The recorded coordinates win over the spec's, for the reason the interface
	// takes a record at all: a route has no id, so its handle is (tunnel,
	// hostname), and the tunnel to remove it from is the one it actually went
	// into — not necessarily the one the spec names today, since that is a
	// per-spec choice (PRSR-33).
	tunnelID := firstNonEmpty(rec.ParentID, t.TunnelID)
	host := firstNonEmpty(strings.ToLower(strings.TrimSpace(rec.Hostname)), t.Spec.Hostname)
	if tunnelID == "" || host == "" {
		return fmt.Errorf("cloudflare: cannot remove an ingress route without both a tunnel and a hostname (tunnel %q, hostname %q)", tunnelID, host)
	}

	p.docMu.Lock()
	defer p.docMu.Unlock()

	before, err := p.getConfig(ctx, tunnelID)
	if err != nil {
		return err
	}
	next, removed := withoutRoute(before.ingress, host)
	if removed == 0 {
		return nil // nothing there — idempotent
	}
	if _, err := terminalIndex(next); err != nil {
		return fmt.Errorf("cloudflare: refusing to write the ingress configuration for tunnel %s: %w", tunnelID, err)
	}
	if err := p.putConfig(ctx, tunnelID, before.config, next); err != nil {
		return err
	}

	after, err := p.getConfig(ctx, tunnelID)
	if err != nil {
		return fmt.Errorf("cloudflare: the ingress route for %s was removed but the configuration could not be read back to confirm — re-run to see what landed: %w", host, err)
	}
	// Teardown reports success only when the resource is actually gone, so the
	// read-back is the claim, not the PUT's 200.
	if idx, _ := findRoute(after.ingress, host); idx >= 0 {
		return fmt.Errorf("cloudflare: %s is still routed on tunnel %s after the removal — another writer changed the shared configuration at the same time; re-run", host, tunnelID)
	}
	if note := concurrentWriteNote(before.version, after.version, tunnelID); note != "" {
		log.Printf("spinup: %s", note)
	}
	return nil
}

// --- the ingress document --------------------------------------------------

// ingressRule is one entry of a tunnel's ingress list, held as raw JSON per key.
//
// Not a struct, on purpose. A PUT replaces the whole document, so any field this
// build does not model is a field the tunnel loses the moment we write — and
// per-rule `originRequest` overrides (noTLSVerify, httpHostHeader, timeouts) are
// exactly the sort of thing somebody sets once by hand and never mentions.
// Raw JSON per key means a rule can be carried back byte-for-byte while still
// having its `service` rewritten.
type ingressRule map[string]json.RawMessage

// str reads a string-valued key, treating absent, null and non-string alike as
// "". The terminal catch-all rule carries *no* hostname — it was observed as a
// trailing null on 2026-08-15 — so every reader of this list has to tolerate its
// absence rather than trip over it.
func (r ingressRule) str(key string) string {
	raw, ok := r[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// clone copies a rule so one key can be rewritten without writing through the
// map the fetched document still holds.
func (r ingressRule) clone() ingressRule {
	out := make(ingressRule, len(r))
	for k, v := range r {
		out[k] = v
	}
	return out
}

// ingressDoc is one tunnel's configuration as fetched: the whole `config`
// object kept verbatim, its ingress list decoded, and the server-assigned
// version that makes a concurrent write detectable.
type ingressDoc struct {
	config  map[string]json.RawMessage
	ingress []ingressRule
	version int
}

// isCatchAll reports whether a rule matches everything — no hostname and no
// path. cloudflared requires the list to end in one, and a rule after it is
// dead: it never matches, and nothing errors.
func isCatchAll(r ingressRule) bool {
	return r.str("hostname") == "" && r.str("path") == ""
}

// isRoute reports whether a rule is *the* route for a hostname: same host, and
// no path. A path-scoped rule for the same hostname is a narrower route someone
// wrote deliberately, so it is neither matched nor touched here.
func isRoute(r ingressRule, host string) bool {
	return r.str("path") == "" && strings.EqualFold(r.str("hostname"), host)
}

// findRoute returns the index of the hostname's rule and how many *further*
// rules also carry it. cloudflared matches the first, so the first is the one
// that decides what the hostname does, and the rest are reported rather than
// silently rewritten.
func findRoute(rules []ingressRule, host string) (idx, dups int) {
	idx = -1
	for i, r := range rules {
		if !isRoute(r, host) {
			continue
		}
		if idx < 0 {
			idx = i
			continue
		}
		dups++
	}
	return idx, dups
}

// terminalIndex returns the index the catch-all rule sits at, refusing any
// document whose shape would make an inserted rule silently dead.
//
// Both refusals matter and they are different failures. A list that does not end
// in a catch-all is one cloudflared would reject anyway, and is not a document
// worth guessing at. A catch-all that is *not* last has already killed every
// rule after it — inserting before the final rule there would put the new route
// in the dead tail, which is the exact outcome this ticket exists to prevent,
// one step generalized.
func terminalIndex(rules []ingressRule) (int, error) {
	if len(rules) == 0 {
		return 0, fmt.Errorf("the ingress configuration is empty, so it has no terminal catch-all rule to insert before")
	}
	last := len(rules) - 1
	for i, r := range rules {
		if isCatchAll(r) && i != last {
			return 0, fmt.Errorf("ingress rule %d of %d matches every hostname but is not last, so every rule after it is already dead — a route inserted here would never match and nothing would report it; fix the tunnel's configuration first", i+1, len(rules))
		}
	}
	if !isCatchAll(rules[last]) {
		return 0, fmt.Errorf("the ingress configuration does not end in a catch-all rule (the last one serves %s for %s) — cloudflared requires one, so this is not a document Purser understands well enough to rewrite", rules[last].str("service"), rules[last].str("hostname"))
	}
	return last, nil
}

// planRoute produces the ingress list the hostname's route calls for, whether
// anything changed, and the line describing it.
//
// Pure, and separate from the HTTP round trip, because "inserted before the
// terminal rule" is the assertion this ticket is about and it should be
// checkable without a server.
func planRoute(rules []ingressRule, host, service string) (next []ingressRule, changed bool, detail string, err error) {
	if idx, _ := findRoute(rules, host); idx >= 0 {
		was := rules[idx].str("service")
		if was == service {
			return rules, false, fmt.Sprintf("%s already routed to %s", host, service), nil
		}
		// Repointed in place: the rule keeps its position (order is matching
		// precedence) and every other key it carries.
		next = make([]ingressRule, len(rules))
		copy(next, rules)
		updated := rules[idx].clone()
		updated["service"] = jsonString(service)
		next[idx] = updated
		return next, true, fmt.Sprintf("%s repointed from %s to %s", host, was, service), nil
	}

	rule := ingressRule{"hostname": jsonString(host), "service": jsonString(service)}

	if len(rules) == 0 {
		// A tunnel with no ingress at all. The rule cannot just be appended —
		// cloudflared requires a rule matching everything last — so supply the
		// terminal one rather than writing a document the API would reject.
		// This is the only case where this provisioner adds a rule that is not
		// the service's own, and it says so in the plan.
		return []ingressRule{rule, catchAll()}, true,
			fmt.Sprintf("%s routed to %s, with a terminal %s rule (the tunnel had no ingress configuration)", host, service, catchAllService), nil
	}

	at, err := terminalIndex(rules)
	if err != nil {
		return nil, false, "", fmt.Errorf("cloudflare: %w", err)
	}
	next = make([]ingressRule, 0, len(rules)+1)
	next = append(next, rules[:at]...)
	next = append(next, rule)
	next = append(next, rules[at:]...)
	return next, true,
		fmt.Sprintf("%s routed to %s, inserted before the terminal %s rule (%d rules now)", host, service, rules[at].str("service"), len(next)), nil
}

// withoutRoute drops every rule that is the hostname's own route, and reports
// how many went.
//
// Only rules this provisioner would have written are removed: same hostname, no
// path. A path-scoped rule for the hostname was somebody's deliberate, narrower
// route, and deleting a record made by hand is not a thing re-running fixes.
func withoutRoute(rules []ingressRule, host string) ([]ingressRule, int) {
	next := make([]ingressRule, 0, len(rules))
	removed := 0
	for _, r := range rules {
		if isRoute(r, host) {
			removed++
			continue
		}
		next = append(next, r)
	}
	return next, removed
}

// confirmRoute is the post-write assertion: the hostname is routed where the
// spec says, and the catch-all is still last.
//
// The second half is the one worth having. A rule that ended up after the
// terminal rule works exactly like a rule that was never written — no error, no
// symptom, no route — so it has to be *checked*, not assumed.
func confirmRoute(rules []ingressRule, host, service string) error {
	idx, _ := findRoute(rules, host)
	if idx < 0 {
		return fmt.Errorf("cloudflare: the ingress configuration was written but %s is not in it — another writer changed the shared document at the same time; re-run, and check the other services on this tunnel", host)
	}
	if got := rules[idx].str("service"); got != service {
		return fmt.Errorf("cloudflare: the ingress configuration was written but %s routes to %s, not %s — another writer changed the shared document at the same time; re-run", host, got, service)
	}
	if _, err := terminalIndex(rules); err != nil {
		return fmt.Errorf("cloudflare: the ingress configuration was written and %s is in it, but the catch-all is no longer last, so some rule is now dead: %w", host, err)
	}
	return nil
}

// concurrentWriteNote reports whether somebody else wrote the document between
// our read and our own write.
//
// Cloudflare assigns the configuration a version and bumps it once per write.
// Ours moves it by exactly one; anything more means another writer landed in
// between, and the document we PUT was built without their change — so a route
// they added may have just been dropped.
//
// This is the only guard that can see that, and it is worth its ten lines
// because the obvious check cannot: confirming our own route always passes,
// since our write necessarily contains everything our own read did. Skipped
// when either version is absent, so an API or a fixture that doesn't report one
// produces no false alarm.
func concurrentWriteNote(before, after int, tunnelID string) string {
	if before <= 0 || after <= 0 || after == before+1 {
		return ""
	}
	return fmt.Sprintf("tunnel %s went from ingress version %d to %d across this write, not %d: another writer changed the shared configuration at the same time and a route it added may have been lost — check the other services on this tunnel",
		tunnelID, before, after, before+1)
}

// catchAll builds the terminal rule, for the one case that needs one supplied.
func catchAll() ingressRule {
	return ingressRule{"service": jsonString(catchAllService)}
}

// jsonString encodes a string as raw JSON. Marshalling a string cannot fail.
func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// --- the API round trip ----------------------------------------------------

func configPath(accountID, tunnelID string) string {
	return fmt.Sprintf("/accounts/%s/cfd_tunnel/%s/configurations", accountID, tunnelID)
}

// getConfig fetches one tunnel's configuration, keeping the `config` object
// whole so putConfig can hand back every key it didn't touch.
func (p *TunnelProvisioner) getConfig(ctx context.Context, tunnelID string) (ingressDoc, error) {
	raw, err := p.api.do(ctx, http.MethodGet, configPath(p.cfg.AccountID, tunnelID), nil)
	if err != nil {
		return ingressDoc{}, err
	}
	var env struct {
		Result struct {
			Version int                        `json:"version"`
			Config  map[string]json.RawMessage `json:"config"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ingressDoc{}, fmt.Errorf("cloudflare: decode tunnel configuration: %w", err)
	}
	doc := ingressDoc{config: env.Result.Config, version: env.Result.Version}
	if doc.config == nil {
		doc.config = map[string]json.RawMessage{}
	}
	if err := json.Unmarshal(orNull(doc.config["ingress"]), &doc.ingress); err != nil {
		// Refused rather than treated as an empty list: "we could not read the
		// routes" and "there are no routes" differ by every other service on the
		// tunnel, and only one of them is safe to write from.
		return ingressDoc{}, fmt.Errorf("cloudflare: decode tunnel %s ingress: %w", tunnelID, err)
	}
	return doc, nil
}

// putConfig writes the document back with a new ingress list and every other
// key exactly as it was read.
func (p *TunnelProvisioner) putConfig(ctx context.Context, tunnelID string, cfg map[string]json.RawMessage, ingress []ingressRule) error {
	encoded, err := json.Marshal(ingress)
	if err != nil {
		return fmt.Errorf("cloudflare: encode tunnel ingress: %w", err)
	}
	// A copy, not the fetched map: warp-routing, the tunnel-wide originRequest
	// and anything this build has never heard of go back untouched, because the
	// PUT replaces the whole document and what it omits is what the tunnel
	// loses.
	next := make(map[string]json.RawMessage, len(cfg)+1)
	for k, v := range cfg {
		next[k] = v
	}
	next["ingress"] = encoded
	_, err = p.api.do(ctx, http.MethodPut, configPath(p.cfg.AccountID, tunnelID), map[string]any{"config": next})
	return err
}

// orNull makes an absent key decode as JSON null (which unmarshals to a nil
// slice) instead of as an empty document (which is a syntax error).
func orNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}
