// Package spinup is Purser's second provisioning axis: standing up the edge for
// a service, rather than provisioning a person into one (PRSR-27, epic PRSR-22).
//
// Everything in internal/connector and internal/invite is person-shaped —
// person / account / invite / provision_task, idempotent per
// (person × service), and Connector.Provision takes a name and an email. This
// axis provisions the infrastructure that makes a service *exist*: a DNS record,
// a tunnel ingress route, a Cloudflare Access application. It is keyed on
// hostname, its idempotency key is (hostname, kind), and it has its own
// registry and its own orchestrator.
//
// It is a second interface rather than an extension of the first on purpose.
// The two share an ethos — treat already-exists as success, never treat
// unverifiable as absent, never record something that didn't happen, don't let
// one resource's failure abort the rest — but they share no types, because the
// only way to make Connector serve both would be to widen Input until neither
// axis's fields mean anything.
package spinup

import (
	"fmt"
	"strings"

	"github.com/Einlanzerous/purser/internal/model"
)

// Mode is how traffic reaches the service, and it is the spec's central
// distinction: it changes what the DNS record points at, and whether there is a
// tunnel ingress route at all.
type Mode string

const (
	// ModeTunnelled — the service is reachable only through a cloudflared
	// tunnel. Its DNS record is a proxied CNAME to <tunnel-id>.cfargotunnel.com
	// and its hostname needs an ingress rule on that tunnel.
	ModeTunnelled Mode = "tunnelled"
	// ModeDirect — the hostname resolves straight to an endpoint that is already
	// reachable, and no tunnel is involved. Argosy is the epic's pilot and is on
	// this path, which is why the pilot needs no tunnel connector.
	ModeDirect Mode = "direct"
)

// AccessShape is what Cloudflare Access surface the service gets. It is a
// separate axis from Mode, because all the combinations are real: a tunnelled
// service is usually gated but need not be (placard.zerogravity.industries is
// ungated by design), and a direct service is usually a bookmark in front of its
// own login.
type AccessShape string

const (
	// AccessGated — a `self_hosted` application plus a policy allowing the
	// shared members group. The gate itself.
	AccessGated AccessShape = "gated"
	// AccessBookmark — a `bookmark` application: a launcher tile in front of a
	// service that holds its own login, with no policy.
	//
	// This is a *different application type*, not a gated app minus its policy,
	// which is the finding that most shapes this package (PRSR-27/PRSR-29): a
	// spec that could only express `self_hosted` could not describe the direct
	// path at all, and the direct path is what the pilot is on.
	AccessBookmark AccessShape = "bookmark"
	// AccessNone — no Access object of any kind. The service gets no launcher
	// tile and no gate; the step is reported as not applicable rather than
	// silently omitted.
	AccessNone AccessShape = "none"
)

// TunnelRef names a tunnel without naming its id, so specs stay free of opaque
// account-specific values and a spec is readable by whoever writes it.
//
// The account has two healthy tunnels — construct-server (prod) and
// construct-dev — and a dev instance of a service is the same spin-up shape
// pointed at a different one (PRSR-33). Carrying the choice on the spec is what
// stops dev needing a config fork or a second Purser; resolving the ref to an id
// is TunnelSet's job, once per run, in the orchestrator.
type TunnelRef string

const (
	// TunnelProd is the construct-server tunnel, serving the prod edge.
	TunnelProd TunnelRef = "prod"
	// TunnelDev is the construct-dev tunnel. The name is accepted here so specs
	// can be written against it, but nothing wires an id to it yet — that is
	// PRSR-33, along with the dev hostname convention and the question of
	// whether dev apps share the prod Access group. Until then a dev spec
	// resolves to a clear refusal rather than to prod's tunnel, which is the
	// whole reason the ref is a name and not a bare id.
	TunnelDev TunnelRef = "dev"
)

// knownTunnels is every ref a spec may name. A ref outside this set is a typo,
// and a typo that reached TunnelSet would be reported as "not configured" —
// which reads like something an operator should go and configure.
var knownTunnels = map[TunnelRef]bool{TunnelProd: true, TunnelDev: true}

// ServiceSpec describes one service's edge: what hostname it answers on, how
// traffic reaches it, and what Access surface it gets. It is the unit the
// orchestrator works from, and it is not persisted — the resource table records
// what was *created*, which is a different claim.
type ServiceSpec struct {
	// Key names the service, e.g. "argosy". It labels the resource rows so
	// "what does this service hold at the edge?" is answerable, and it is not a
	// connector key: a service being stood up here need never be an invite
	// target.
	Key string
	// DisplayName is the human label, used for the Access application's name.
	// Defaults to Key.
	DisplayName string
	// Hostname is the public hostname, e.g. "argosy.zerogravity.industries".
	// Normalized to lowercase — it is this axis's identity key, and hostnames
	// are case-insensitive.
	Hostname string
	// Mode is tunnelled or direct. There is no default: the two produce
	// different DNS records, and guessing one would publish a record pointing
	// somewhere the service isn't.
	Mode Mode
	// Upstream is where traffic goes. For ModeTunnelled it is the internal
	// origin cloudflared should forward to (e.g. "http://argosy:8096"); for
	// ModeDirect it is the endpoint the DNS record resolves to.
	Upstream string
	// Access is the Access surface to create.
	Access AccessShape
	// LogoURL is the launcher tile's icon.
	//
	// Cloudflare stores whatever URL it is given and never validates it, and the
	// launcher falls back to two grey initials when it fails to load — so a
	// wrong URL is indistinguishable from an unset one, with no error anywhere.
	// Of the six Access apps live before this axis was designed, one had a
	// working icon (PRSR-29). Validated here only for shape; whether an app may
	// be created without a *reachable* one is the Access connector's call, since
	// it is the half that can fetch it.
	LogoURL string
	// Tunnel names which tunnel carries this hostname. Required for
	// ModeTunnelled — it decides both the ingress document written to and the
	// DNS record's target — and must be empty for ModeDirect, where nothing
	// would read it.
	Tunnel TunnelRef
}

// Normalized returns the spec with its defaults applied and its hostname
// case-folded. Validate calls it, and the orchestrator works from the result, so
// a spec is interpreted in exactly one place.
func (s ServiceSpec) Normalized() ServiceSpec {
	s.Key = strings.TrimSpace(s.Key)
	s.Hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s.Hostname), "."))
	s.Upstream = strings.TrimSpace(s.Upstream)
	s.LogoURL = strings.TrimSpace(s.LogoURL)
	if s.DisplayName = strings.TrimSpace(s.DisplayName); s.DisplayName == "" {
		s.DisplayName = s.Key
	}
	return s
}

// Validate reports whether the spec describes something coherent, and returns
// the normalized form the orchestrator should use.
//
// Everything it refuses is refused before any upstream call and before any row
// is written, which matters more here than on the invite path: the cheapest
// place to catch "this spec points at the wrong tunnel" is before a shared
// ingress document has been rewritten.
func (s ServiceSpec) Validate() (ServiceSpec, error) {
	s = s.Normalized()
	if s.Key == "" {
		return s, fmt.Errorf("spinup: service key is required")
	}
	if s.Hostname == "" {
		return s, fmt.Errorf("spinup: hostname is required")
	}
	if err := validHostname(s.Hostname); err != nil {
		return s, err
	}
	if s.Upstream == "" {
		return s, fmt.Errorf("spinup: upstream is required (%s: where cloudflared forwards to; %s: what the record resolves to)",
			ModeTunnelled, ModeDirect)
	}
	switch s.Mode {
	case ModeTunnelled:
		if s.Tunnel == "" {
			// Not defaulted to prod on purpose. There are two tunnels now, the
			// ingress configuration is one shared document per tunnel, and the
			// cost of assuming wrong is rewriting the document that carries
			// every *other* service's routes (PRSR-30, PRSR-33). A spec written
			// rarely and read carefully can name its tunnel.
			return s, fmt.Errorf("spinup: a %s spec must name its tunnel (%s or %s)", ModeTunnelled, TunnelProd, TunnelDev)
		}
		if !knownTunnels[s.Tunnel] {
			return s, fmt.Errorf("spinup: unknown tunnel %q (known: %s, %s)", s.Tunnel, TunnelProd, TunnelDev)
		}
	case ModeDirect:
		if s.Tunnel != "" {
			// A contradiction rather than a harmless extra: a direct spec skips
			// the ingress step and points DNS at its endpoint, so nothing would
			// read this — and an operator who set it believes otherwise.
			return s, fmt.Errorf("spinup: a %s spec must not name a tunnel (it has no ingress route and its record does not point at one)", ModeDirect)
		}
	case "":
		return s, fmt.Errorf("spinup: mode is required (%s or %s)", ModeTunnelled, ModeDirect)
	default:
		return s, fmt.Errorf("spinup: unknown mode %q (want %s or %s)", s.Mode, ModeTunnelled, ModeDirect)
	}
	switch s.Access {
	case AccessGated, AccessBookmark, AccessNone:
	case "":
		return s, fmt.Errorf("spinup: access shape is required (%s, %s or %s)", AccessGated, AccessBookmark, AccessNone)
	default:
		return s, fmt.Errorf("spinup: unknown access shape %q (want %s, %s or %s)", s.Access, AccessGated, AccessBookmark, AccessNone)
	}
	if s.LogoURL != "" && !strings.HasPrefix(s.LogoURL, "https://") && !strings.HasPrefix(s.LogoURL, "http://") {
		// The launcher renders it as an <img> in the viewer's browser, so a
		// relative or scheme-less path cannot resolve for anybody.
		return s, fmt.Errorf("spinup: logo url %q must be absolute (the launcher loads it from the viewer's browser)", s.LogoURL)
	}
	return s, nil
}

// validHostname rejects the shapes that are obviously not a hostname. It is
// deliberately shallow — the zone API is the real authority — but a URL or a
// path here would otherwise be recorded as this axis's identity key and then
// written into a DNS record.
func validHostname(h string) error {
	switch {
	case strings.Contains(h, "://"), strings.ContainsAny(h, "/ \t"):
		return fmt.Errorf("spinup: hostname %q must be a bare hostname, not a URL", h)
	case !strings.Contains(h, "."):
		return fmt.Errorf("spinup: hostname %q must be fully qualified", h)
	}
	return nil
}

// Steps reports which resource kinds this spec calls for, in apply order.
//
// This is the whole tunnelled/direct split, in one readable place. It reaches
// three steps rather than the two the epic first described: a direct service
// skips the ingress route *and* takes a different Access application type, and
// AccessNone skips the Access step outright.
//
// Kinds it omits are not silently dropped by the orchestrator — they are
// reported as not applicable, so a report always has a line per kind and
// "nothing about the tunnel" can't be read as "the tunnel is fine".
func (s ServiceSpec) Steps() []model.ResourceKind {
	steps := make([]model.ResourceKind, 0, len(model.KindOrder))
	for _, k := range model.KindOrder {
		if s.callsFor(k) {
			steps = append(steps, k)
		}
	}
	return steps
}

// callsFor answers Steps' question for one kind, and doubles as the source of
// the reason a skipped step is skipped (see skipReason).
func (s ServiceSpec) callsFor(kind model.ResourceKind) bool {
	switch kind {
	case model.ResourceTunnelRoute:
		return s.Mode == ModeTunnelled
	case model.ResourceAccessApp:
		return s.Access != AccessNone
	case model.ResourceDNSRecord:
		return true // every hostname has to resolve
	}
	return false
}

// skipReason explains, for the report, why a spec doesn't call for a kind.
func (s ServiceSpec) skipReason(kind model.ResourceKind) string {
	switch kind {
	case model.ResourceTunnelRoute:
		return fmt.Sprintf("%s spec: traffic does not pass through a tunnel", ModeDirect)
	case model.ResourceAccessApp:
		return fmt.Sprintf("access %s: no application or policy", AccessNone)
	}
	return "not applicable to this spec"
}

// TunnelSet maps tunnel refs to the ids they stand for, built from config in
// the composition root so this package reads no environment — the same
// arrangement invite.BundleSet has.
//
// Only prod is wired today (PRSR-33 wires dev, along with the second id in
// config and the dev hostname convention). An unwired ref is reported as
// unconfigured, which is the honest answer: the tunnel exists, Purser just
// hasn't been told its id.
type TunnelSet map[TunnelRef]string

// Resolve returns the tunnel id for a ref.
func (ts TunnelSet) Resolve(ref TunnelRef) (string, error) {
	if id := strings.TrimSpace(ts[ref]); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("spinup: tunnel %q is not configured (%s)", ref, configuredTunnels(ts))
}

// configuredTunnels renders what *is* wired, so the refusal distinguishes "you
// named the wrong one" from "this deployment has no tunnels at all".
//
// It deliberately doesn't name the environment variable. Env keys live in
// internal/config and the composition root, which is also where the next one
// will be added (PRSR-33) — a name spelled out here would go stale silently.
func configuredTunnels(ts TunnelSet) string {
	var have []TunnelRef
	for _, ref := range []TunnelRef{TunnelProd, TunnelDev} {
		if strings.TrimSpace(ts[ref]) != "" {
			have = append(have, ref)
		}
	}
	if len(have) == 0 {
		return "this deployment has no tunnel ids configured"
	}
	parts := make([]string, len(have))
	for i, ref := range have {
		parts[i] = string(ref)
	}
	return "configured: " + strings.Join(parts, ", ")
}
