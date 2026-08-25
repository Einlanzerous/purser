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

// Kind is the resource kind this provisioner owns.
func (p *TunnelProvisioner) Kind() model.ResourceKind { return model.ResourceTunnelRoute }

// DisplayName is the label the plan uses for this step.
func (p *TunnelProvisioner) DisplayName() string { return "Cloudflare Tunnel ingress route" }

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
	return inspectIngress(doc.ingress, t)
}

// inspectIngress turns a fetched ingress list into a State, or into the reason
// no honest plan can be made from it. Split out from Inspect so the shape of the
// answer is testable without a server, and so the Detail an operator reads in
// the plan is written in one place.
func inspectIngress(rules []ingressRule, t spinup.Target) (spinup.State, error) {
	// ParentID is the tunnel, and ExternalID stays empty by nature: the
	// configuration is one document per tunnel, so a route has no id of its own
	// and is identified by (tunnel, hostname). Both are set on this path and on
	// Ensure's, because the orchestrator adopts on a disagreement between them
	// and the recorded row.
	st := spinup.State{ParentID: t.TunnelID}
	host, want := t.Spec.Hostname, t.Spec.Upstream
	scan := scanRoute(rules, host)
	idx := scan.Idx
	shape := documentShape(rules)

	switch {
	case idx >= 0 && rules[idx].str("service") == want:
		// Reachable and serving what the spec asks for. Reported as in place
		// even when the document is malformed elsewhere, because *this*
		// hostname works: the resource is already published, so withholding the
		// line protects nobody, and what is broken is somebody else's dead
		// rules. Said out loud below rather than left for them to find.
		st.Exists, st.Matches = true, true
		st.Detail = fmt.Sprintf("ingress rule %d of %d on tunnel %s → %s", idx+1, len(rules), t.TunnelID, want)

	case shape != nil:
		// Every remaining answer is one --apply would act on, and this is a
		// document it will refuse to write. Reported as unreadable rather than
		// as `create` or `update`: the orchestrator turns this into `unknown`,
		// which does not act and holds the DNS step behind it — and a hostname
		// published in front of a tunnel that cannot serve it is exactly what
		// that ordering exists to prevent.
		return spinup.State{}, fmt.Errorf("cloudflare: tunnel %s: %w", t.TunnelID, shape)

	case idx >= 0:
		// Compared exactly. A difference the operator can see in the plan and
		// decline is better than one quietly tolerated, and the alternative — a
		// fuzzy match — would have to guess which differences are cosmetic on a
		// value cloudflared parses.
		st.Exists = true
		st.Detail = fmt.Sprintf("ingress rule %d of %d on tunnel %s → %s, want %s", idx+1, len(rules), t.TunnelID, rules[idx].str("service"), want)

	default:
		st.Detail = fmt.Sprintf("no ingress rule for %s on tunnel %s (%d rules)", host, t.TunnelID, len(rules))
	}

	if scan.Behind > 0 {
		// There *is* a rule for the hostname; cloudflared never reaches it. The
		// plain "no ingress rule" line would be true about what is served and
		// misleading about what is written, and the operator needs the second
		// one to understand why a create is being proposed for a hostname they
		// can see in the dashboard.
		st.Detail += fmt.Sprintf(" — %d rule(s) for it sit behind rule %d of %d (%s), which takes this hostname first, so they are never matched",
			scan.Behind, scan.ShadowedBy+1, len(rules), shadowLabel(rules[scan.ShadowedBy]))
	}
	if scan.Dups > 0 {
		// Reported rather than repaired. cloudflared matches the first rule, so
		// the route works and the duplicates are inert — but they are somebody's
		// hand edit, and silently deleting rules this provisioner did not write
		// is not a thing a re-run can undo.
		st.Detail += fmt.Sprintf(" (%d further rule(s) carry this hostname; cloudflared matches the first)", scan.Dups)
	}
	if shape != nil {
		st.Detail += fmt.Sprintf(" — this hostname is served, but the tunnel's configuration is malformed elsewhere and no route can be written into it: %v", shape)
	}
	return st, nil
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
	if err := assertWritable(next, t.Spec.Hostname); err != nil {
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
		// what it said. What may have been lost is somebody *else's* route.
		//
		// Carried on the Detail alone, and deliberately not also logged. The
		// orchestrator puts this string on the step's finding, which both
		// surfaces render — so a log line here made one event read as two on the
		// CLI, where `log` writes to stdout beside the report (PRSR-31). It is
		// the one message whose whole value is being believed when it fires, and
		// a warning printed twice is one a reader starts discounting. Teardown
		// still logs its own: nothing carries a detail back from there.
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
	// withoutRoute rather than scanRoute: the removal took every rule carrying
	// the hostname, including any behind a catch-all, so the check that it is
	// gone has to look at the same set rather than only the reachable one.
	if _, left := withoutRoute(after.ingress, host); left > 0 {
		return fmt.Errorf("cloudflare: %s is still routed on tunnel %s after the removal — another writer changed the shared configuration at the same time; re-run", host, tunnelID)
	}
	if note := concurrentWriteNote(before.version, after.version, tunnelID); note != "" {
		log.Printf("cloudflare: %s", note)
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

// isCatchAll reports whether a rule matches every request — no path, and a
// hostname that takes every host. cloudflared requires the list to end in one,
// and a rule after it is dead: it never matches, and nothing errors.
//
// Both spellings count, because cloudflared special-cases both: the live tunnel
// writes no hostname at all (observed as a trailing null), and an explicit `*`
// means the same thing there. Reading only the first would miss a mid-list
// catch-all written the other way, which is the dead-tail case.
func isCatchAll(r ingressRule) bool {
	return r.str("path") == "" && matchesEveryHostname(r.str("hostname"))
}

// isRoute reports whether a rule is *the* route for a hostname: same host,
// literally, and no path. A path-scoped rule for the same hostname is a narrower
// route someone wrote deliberately, so it is neither matched nor touched here.
//
// Literal, and deliberately not a wildcard match. A rule reading
// `*.zerogravity.industries` may well be serving this hostname, but it is not
// *ours*: adopting it would have Teardown delete a rule standing in front of
// every other hostname in the zone.
func isRoute(r ingressRule, host string) bool {
	return r.str("path") == "" && strings.EqualFold(r.str("hostname"), host)
}

// shadows reports whether cloudflared would match this rule for host before
// reaching anything later in the list.
//
// cloudflared matches ingress top-down, first match wins, and a rule's hostname
// may carry a wildcard — `*.zerogravity.industries` in front of a holding page
// is a documented, deliberate configuration, not a malformed document. So the
// terminal catch-all is not the only thing that ends the walk; it is the case
// where the pattern matches everything.
//
// A rule carrying a path is treated as narrower than the hostname and does not
// shadow. That is cloudflared's shape — it ANDs the host match with a path match
// — but not quite its arithmetic: `Path` is an unanchored *regexp*, so a rule
// whose path is `/` does in fact take every request for its host. Modelling Go
// regexp semantics to decide that is well out of proportion here, and the
// realistic path rule is `/admin`. Recorded as a known edge rather than guessed
// at.
func shadows(r ingressRule, host string) bool {
	if r.str("path") != "" {
		return false
	}
	return hostnameTakes(r.str("hostname"), host)
}

// matchesEveryHostname reports whether an ingress hostname pattern takes every
// host. cloudflared special-cases both spellings before its matcher runs.
func matchesEveryHostname(pattern string) bool {
	return pattern == "" || pattern == "*"
}

// hostnameTakes reports whether an ingress rule's hostname field matches host.
//
// This mirrors cloudflared's own matcher rather than approximating it. It was a
// general `*`-glob first, and a glob is wrong in two directions at once — one of
// them silently (PRSR-30 review). Read from the source rather than inferred:
// `Rule.Matches` in ingress/rule.go, and `matchHost` in ingress/ingress.go:
//
//	hostMatch := false
//	if r.Hostname == "" || r.Hostname == "*" {
//	        hostMatch = true
//	} else {
//	        hostMatch = matchHost(r.Hostname, hostname)
//	}
//
//	func matchHost(ruleHost, reqHost string) bool {
//	        if ruleHost == reqHost { return true }
//	        if strings.HasPrefix(ruleHost, "*.") {
//	                toMatch := strings.TrimPrefix(ruleHost, "*")   // the "*" only
//	                return strings.HasSuffix(reqHost, toMatch)     // so the dot stays
//	        }
//	        return false
//	}
//
// Two consequences a glob gets wrong:
//
//   - **Only a leading `*.` is a wildcard.** `wiki.*` and `*.*.industries` are
//     literals upstream and match nothing, so a glob stops the walk at a rule
//     cloudflared walks straight past — reporting `create` for a hostname whose
//     real rule is further down, and calling a serving rule "never matched".
//   - **`*.example.com` does not take the apex `example.com`.** Only the `*` is
//     trimmed, so the suffix tested still carries its dot. A glob that requires
//     the dot after the star agrees here by accident; one that does not would
//     wrongly hold an apex route back.
//
// Case is folded, and that is faithful rather than merely conservative:
// `Rule.Matches` falls back to a `punycodeHostname` built with
// `idna.Lookup.ToASCII`, which case-folds, so a mixed-case rule hostname does
// match a lowercase request upstream.
func hostnameTakes(pattern, host string) bool {
	if matchesEveryHostname(pattern) {
		return true
	}
	if strings.EqualFold(pattern, host) {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		// pattern[1:] keeps the dot, exactly as TrimPrefix(ruleHost, "*") does.
		return strings.HasSuffix(strings.ToLower(host), strings.ToLower(pattern[1:]))
	}
	return false
}

// routeScan is what one walk of the ingress list found for a hostname. It is a
// struct rather than a pile of return values because every caller wants a
// different subset and all of them must agree about where the walk stopped.
type routeScan struct {
	// Idx is the rule cloudflared would match for the hostname, or -1. It is
	// always ahead of ShadowedBy, by construction.
	Idx int
	// Dups is how many further *reachable* rules also carry the hostname.
	Dups int
	// ShadowedBy is the rule that ended the walk — the terminal catch-all, or a
	// wildcard that takes the hostname first — or -1 if the list ran out without
	// one, which only a malformed document does.
	//
	// It is also the insert position: a new route goes in front of the first
	// rule that would otherwise take its traffic, which for an ordinary document
	// is the terminal rule and stays so.
	ShadowedBy int
	// Behind is how many rules for the hostname sit past that shadow. They are
	// never matched, and saying so is the whole point of counting them.
	Behind int
}

// scanRoute walks the ingress list the way cloudflared does: top-down, stopping
// at the first rule that would take the hostname.
//
// The stop is the fix for two findings of the same shape (PRSR-30 review). A
// rule behind a catch-all or a wildcard is not this hostname's route — it is
// never matched — so treating it as one has the read path report a working
// service for a route that serves nothing, and the write path insert a new rule
// into the same dead region and then confirm it.
func scanRoute(rules []ingressRule, host string) routeScan {
	scan := routeScan{Idx: -1, ShadowedBy: -1}
	for i, r := range rules {
		if isRoute(r, host) {
			if scan.Idx < 0 {
				scan.Idx = i
			} else {
				scan.Dups++
			}
			continue
		}
		if shadows(r, host) {
			scan.ShadowedBy = i
			break
		}
	}
	if scan.ShadowedBy >= 0 {
		for _, r := range rules[scan.ShadowedBy+1:] {
			if isRoute(r, host) {
				scan.Behind++
			}
		}
	}
	return scan
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
//
// It answers for the read path as well as the write one (see documentShape), so
// its refusals are worded for both. Both refusals are cloudflared's own:
// `isCatchAllRule` there is `(Hostname == "" || Hostname == "*") && Path == ""`,
// the same predicate as isCatchAll below, and its validation rejects a catch-all
// that is not last with "the rules which follow it will never be triggered".
// Refusing such a document rather than rewriting it is agreeing with the thing
// that has to serve it.
func terminalIndex(rules []ingressRule) (int, error) {
	if len(rules) == 0 {
		return 0, fmt.Errorf("the ingress configuration is empty, so it has no terminal catch-all rule to insert before")
	}
	last := len(rules) - 1
	for i, r := range rules {
		if isCatchAll(r) && i != last {
			return 0, fmt.Errorf("ingress rule %d of %d matches every hostname but is not last, so every rule after it is already dead — a route there is never matched and nothing anywhere reports it; fix the tunnel's configuration first", i+1, len(rules))
		}
	}
	if !isCatchAll(rules[last]) {
		return 0, fmt.Errorf("the ingress configuration does not end in a catch-all rule (the last one serves %s for %s) — cloudflared requires one, so this is not a document Purser understands well enough to rewrite", rules[last].str("service"), rules[last].str("hostname"))
	}
	return last, nil
}

// documentShape reports why this ingress list is not one a route can be written
// into, or nil if it is.
//
// It is exactly planRoute's precondition, hoisted so the *read* path can consult
// it too. A plan that promises `create` against a document Ensure will refuse is
// the same broken promise `connector.CanDeprovision` exists to prevent on the
// offboard preview — and PRSR-27 settled that a plan is the first half of the
// apply, not a guess at it.
//
// An empty list is not a refusal: planRoute supplies the terminal rule for that
// one case, so the two agree there as well.
func documentShape(rules []ingressRule) error {
	if len(rules) == 0 {
		return nil
	}
	if err := terminalIndexErr(rules); err != nil {
		// The sentinel rides on this one predicate rather than on terminalIndex
		// itself, because this is the site that asks it about a document we
		// *fetched*. terminalIndex's other callers ask it about a document this
		// run just built (assertWritable) or just read back (confirmRoute), and
		// those failures are our own arithmetic or a concurrent writer — a
		// breakage, not a state somebody has to go and fix upstream. Both
		// callers of documentShape wrap with %w, so the refusal reaches the
		// orchestrator from the read path and the write path alike (PRSR-31).
		return fmt.Errorf("%w: %w", spinup.ErrRefused, err)
	}
	return nil
}

// terminalIndexErr is terminalIndex's refusal without its index.
func terminalIndexErr(rules []ingressRule) error {
	_, err := terminalIndex(rules)
	return err
}

// assertWritable is the check on the document this run just built, made before
// it is sent: the catch-all is still last, and the hostname's own rule is the
// one cloudflared would actually match.
//
// Belt and braces on our own arithmetic, and worth the two lines because this is
// the one class of bug here that cannot be found by watching it fail — a rule in
// a region cloudflared never reaches is not an error anywhere, it is a hostname
// that quietly does not work.
func assertWritable(rules []ingressRule, host string) error {
	if _, err := terminalIndex(rules); err != nil {
		return err
	}
	if scanRoute(rules, host).Idx < 0 {
		return fmt.Errorf("the rule for %s would sit behind one that takes the hostname first, so it would never be matched", host)
	}
	return nil
}

// planRoute produces the ingress list the hostname's route calls for, whether
// anything changed, and the line describing it.
//
// Pure, and separate from the HTTP round trip, because "inserted before the
// terminal rule" is the assertion this ticket is about and it should be
// checkable without a server.
func planRoute(rules []ingressRule, host, service string) (next []ingressRule, changed bool, detail string, err error) {
	scan := scanRoute(rules, host)
	if idx := scan.Idx; idx >= 0 {
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

	if err := documentShape(rules); err != nil {
		return nil, false, "", fmt.Errorf("refusing to add a route to the ingress configuration: %w", err)
	}
	// Inserted in front of the first rule that would otherwise take this
	// hostname, which for an ordinary document *is* the terminal catch-all — the
	// original requirement, generalized rather than replaced. Where a wildcard
	// sits in front (a holding page, a default backend), the new route goes
	// ahead of that instead: most-specific-first is cloudflared's own idiom, and
	// it is the only position where the route works at all. Nothing is
	// reordered; a narrower rule is put in front of a broader one.
	at := scan.ShadowedBy
	next = make([]ingressRule, 0, len(rules)+1)
	next = append(next, rules[:at]...)
	next = append(next, rule)
	next = append(next, rules[at:]...)
	detail = fmt.Sprintf("%s routed to %s, inserted before rule %d of %d (%s) (%d rules now)",
		host, service, at+1, len(rules), shadowLabel(rules[at]), len(next))
	if scan.Behind > 0 {
		// Left in place: a rule Purser did not write is not one it deletes, and
		// it was already dead before this run. Said out loud so the operator can
		// remove it if it was meant to be the route.
		detail += fmt.Sprintf(" — %d existing rule(s) for this hostname sit behind that and stay unmatched", scan.Behind)
	}
	return next, true, detail, nil
}

// shadowLabel names a rule for a human: its hostname pattern, or what the
// hostname-less terminal rule serves.
func shadowLabel(r ingressRule) string {
	if h := r.str("hostname"); h != "" {
		return h
	}
	return "the terminal " + r.str("service") + " rule"
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
	idx := scanRoute(rules, host).Idx
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
// since our write necessarily contains everything our own read did.
//
// **The +1 is an assumption about Cloudflare, and nothing here is evidence for
// it.** The fake in the tests bumps by one because the test author decided it
// does. So the guard is written to fail quiet rather than loud: it is skipped
// when either version is absent, and it only fires on a version that moved by
// *more* than one — a read that lagged our own write (`after <= before`) tells
// us nothing about another writer, and would otherwise cry wolf on the one
// message that most needs to be believed when it is real. One live
// GET → PUT → GET against construct-server settles it; noted on PRSR-31, which
// is where a CLI first points this at the real API.
func concurrentWriteNote(before, after int, tunnelID string) string {
	if before <= 0 || after <= 0 || after <= before+1 {
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

// remotelyManaged is the `source` value meaning this tunnel serves the
// configuration this endpoint returns. The other is "local".
const remotelyManaged = "cloudflare"

// checkTunnelSource refuses a tunnel whose ingress document is not the one in
// force.
//
// A locally-managed tunnel is configured by a YAML file on the origin machine,
// and the remote configuration this endpoint returns is not what it serves. That
// is a gap nothing else in this file can close: docMu, the fresh read inside it,
// the verbatim write-back and the version check are each about *who else wrote
// this document*, and none of them can ask whether the document is live.
//
// Refused rather than warned about, because the failure is silent end to end —
// the read reports "no ingress rule" from a document that is no evidence about
// what is served, the PUT is accepted and stored, the read-back finds the route,
// confirmRoute passes, and DNS publishes a hostname the tunnel has never heard
// of. Nothing errors anywhere, which is the exact failure mode this whole file
// is organised around, one layer below where the rest of it applies. It is also
// this axis's oldest invariant: never treat unverifiable as absent.
//
// An absent source is refused too. "We could not tell" is not "it is fine", and
// a wrong refusal here names what it saw and is one glance to diagnose, where
// the alternative is a hostname that resolves to nothing for as long as nobody
// checks.
//
// PRSR-26 established that construct-server is remotely managed — by hand, once.
// Nothing re-asserts it at run time, converting a tunnel is a dashboard toggle,
// and PRSR-33 wires a second tunnel whose mode nobody has checked yet.
func checkTunnelSource(tunnelID, source string) error {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case remotelyManaged:
		return nil
	case "":
		return fmt.Errorf("%w: cloudflare: tunnel %s did not report whether it is locally or remotely managed, so this configuration cannot be shown to be the one it serves — refusing to read a route from it or write one into it", spinup.ErrRefused, tunnelID)
	default:
		return fmt.Errorf("%w: cloudflare: tunnel %s is configured by a file on the origin machine (source %q), not over the API — a route written here would be stored and read back correctly and never served; manage its ingress in that cloudflared config, or convert the tunnel to remote management", spinup.ErrRefused, tunnelID, source)
	}
}

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
			Source  string                     `json:"source"`
			Config  map[string]json.RawMessage `json:"config"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ingressDoc{}, fmt.Errorf("cloudflare: decode tunnel configuration: %w", err)
	}
	// Before anything is read out of it: is this the document the tunnel serves?
	if err := checkTunnelSource(tunnelID, env.Result.Source); err != nil {
		return ingressDoc{}, err
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
