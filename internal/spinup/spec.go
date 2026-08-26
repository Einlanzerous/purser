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
	"errors"
	"fmt"
	"net"
	"net/url"
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

// tunnelRefs is every ref a spec may name, in the order the refusals list them.
//
// It is the single source for that set: knownTunnel, the two error messages, and
// TunnelSet's "what is configured" all read it, so wiring the dev tunnel
// (PRSR-33) is one edit rather than four that must agree — and a missed one
// would fail by silently rejecting a ref that is supposed to work.
//
// A ref outside the set is a typo. Catching it here matters because a typo that
// reached TunnelSet would be reported as "not configured", which reads like
// something an operator should go and configure.
var tunnelRefs = []TunnelRef{TunnelProd, TunnelDev}

// LogoRef names where a service's launcher icon comes from.
//
// Three states, because the icon has three and a plain URL string only had two.
// "The spec named nothing", "the spec asked for no icon" and "here is exactly
// the URL" are different instructions, and collapsing the first two is what made
// a forgotten flag destructive: PRSR-38's first live plan reported `update` on
// argosy because the spec named no logo and the live tile carried one, so an
// --apply would have stripped a working icon. Preview-by-default is the only
// reason nothing was lost.
type LogoRef string

const (
	// LogoPlacard resolves the icon from Placard by Key, and is what an
	// unspecified logo means (see Normalized).
	//
	// Defaulting to *resolve* rather than to *clear* is the correction PRSR-37
	// makes to that trap. It is also the only default that is right for this
	// estate: PRSR-38 found seven of ten launcher tiles rendering grey initials
	// and switchyard's stored URL answering a live 404, all of which Placard can
	// already fix by name. When Placard has no mark for the slug — or cannot be
	// asked at all — the icon is left exactly as it is, so the default is never
	// the destructive answer.
	LogoPlacard LogoRef = "placard"
	// LogoNone means the service is to have no icon, deliberately, and clears
	// whatever is there. It is the only value that removes one, which is what
	// makes a deletion something an operator asked for by name.
	LogoNone LogoRef = "none"
)

// knownTunnel reports whether ref is a name a spec may use.
func knownTunnel(ref TunnelRef) bool {
	for _, r := range tunnelRefs {
		if r == ref {
			return true
		}
	}
	return false
}

// knownTunnelList renders the accepted refs for a refusal.
func knownTunnelList() string {
	parts := make([]string, len(tunnelRefs))
	for i, r := range tunnelRefs {
		parts[i] = string(r)
	}
	return strings.Join(parts, " or ")
}

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
	// Upstream is where traffic goes, and it is a **different shape per Mode**
	// because the two consume it in different places:
	//
	//   ModeTunnelled → cloudflared's ingress `service` value, an origin URL
	//                   with a scheme, e.g. "http://argosy:8096".
	//   ModeDirect    → the DNS record's value: an IP address (A/AAAA) or a
	//                   hostname (CNAME). *Not* a URL — a record's value has
	//                   nowhere to put a scheme, a port or a path.
	//
	// Validate checks the shape per mode rather than trusting the field name.
	// One field carrying two shapes is the kind of thing that reads fine and
	// then writes "https://100.64.0.7:8096" into an A record, which resolves for
	// nobody and looks like a DNS problem for a day.
	Upstream string
	// Access is the Access surface to create.
	Access AccessShape
	// Logo names where the launcher tile's icon comes from: LogoPlacard,
	// LogoNone, or an explicit https URL.
	//
	// Cloudflare stores whatever URL it is given and never validates it, and the
	// launcher falls back to two grey initials when it fails to load — so a
	// wrong URL is indistinguishable from an unset one, with no error anywhere.
	// Of the ten Access apps live when PRSR-38 audited them, three had a working
	// icon. Validated here only for shape; whether an app may be created without
	// a *reachable* one is the Access connector's call, since it is the half
	// that can fetch it.
	//
	// A ref rather than a bare URL for the reason Tunnel is one: a spec should
	// name a thing, not carry an opaque account-specific value somebody has to
	// get right by hand. It also gives the field the three states the icon
	// actually has — resolve it, remove it, or use exactly this — where a plain
	// string had two and made the wrong one the default (see LogoPlacard).
	Logo LogoRef
	// Tunnel names which tunnel carries this hostname. Required for
	// ModeTunnelled — it decides both the ingress document written to and the
	// DNS record's target — and must be empty for ModeDirect, where nothing
	// would read it.
	Tunnel TunnelRef
}

// Normalized returns the spec with its defaults applied and its identifiers
// case-folded. Validate calls it, and the orchestrator works from the result, so
// a spec is interpreted in exactly one place.
//
// Key is folded as well as Hostname. It is what `service_key` is matched on, and
// that column is compared exactly — so "Argosy" and "argosy" would otherwise be
// two services holding one hostname's resources between them.
func (s ServiceSpec) Normalized() ServiceSpec {
	// These three come first, because the rest of this function *reads* them —
	// and the ordering is load-bearing rather than tidy. Mode decides whether
	// Upstream is case-folded below, so trimming Mode afterwards left `"direct "`
	// skipping that fold: two spellings of one spec normalized to two different
	// specs, and since validHostname assumes what Normalized produced, an
	// upstream like "Origin.Example.Com" was then refused outright on one
	// spelling and accepted on the other (PRSR-31).
	//
	// Trimmed rather than trimmed-at-each-caller: all three are compared against
	// constants, so a stray space is a refusal — and it was a refusal on the HTTP
	// path only, because the CLI trimmed them itself and this did not. One
	// surface accepting `"direct "` while the other answers `unknown mode
	// "direct "` is a difference nobody would guess at.
	//
	// Trimmed but deliberately NOT case-folded, unlike Key and Hostname. Those
	// are identity keys, where two spellings of one value would split a service's
	// resources between them; these are a closed set matched against constants,
	// and quietly accepting `"Direct"` widens what a spec may say without anybody
	// deciding it should.
	s.Mode = Mode(strings.TrimSpace(string(s.Mode)))
	s.Access = AccessShape(strings.TrimSpace(string(s.Access)))
	s.Tunnel = TunnelRef(strings.TrimSpace(string(s.Tunnel)))

	s.Key = strings.ToLower(strings.TrimSpace(s.Key))
	// TrimRight, not TrimSuffix: "host.example.com.." is as much a trailing-dot
	// mistake as one dot is, and leaving the second one turns an empty label
	// into part of the identity key.
	s.Hostname = strings.ToLower(strings.TrimRight(strings.TrimSpace(s.Hostname), "."))
	s.Upstream = strings.TrimSpace(s.Upstream)
	if s.Mode == ModeDirect {
		// A DNS record value, and DNS is case-insensitive.
		s.Upstream = strings.ToLower(s.Upstream)
	}
	// Trimmed like the other refs above, and defaulted here rather than at each
	// surface so the CLI and the HTTP API cannot disagree about what an omitted
	// logo means.
	if s.Logo = LogoRef(strings.TrimSpace(string(s.Logo))); s.Logo == "" {
		s.Logo = LogoPlacard
	}
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
			return s, fmt.Errorf("spinup: a %s spec must name its tunnel (%s)", ModeTunnelled, knownTunnelList())
		}
		if !knownTunnel(s.Tunnel) {
			return s, fmt.Errorf("spinup: unknown tunnel %q (known: %s)", s.Tunnel, knownTunnelList())
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
	if err := validUpstream(s.Mode, s.Upstream); err != nil {
		return s, err
	}
	switch s.Logo {
	case LogoPlacard, LogoNone:
	default:
		if !strings.HasPrefix(strings.ToLower(string(s.Logo)), "https://") {
			// https only, and case-insensitively checked, because the launcher
			// renders this as an <img> inside its own https page: an http:// icon
			// is blocked as mixed content and falls back to the grey initials
			// this field exists to avoid — silently, since Cloudflare never
			// validates the URL it was given (PRSR-29). A scheme-relative or
			// relative path can't resolve from the viewer's browser at all.
			//
			// The message names the two keywords as well as the URL rule,
			// because the commonest way to land here is now a typo in one of
			// them, and "must be an absolute https:// url" is unhelpful advice
			// to somebody who typed --logo plackard.
			return s, fmt.Errorf("spinup: unknown logo %q (want %s, %s, or an absolute https:// url — the launcher loads it from the viewer's browser, and blocks http as mixed content)", s.Logo, LogoPlacard, LogoNone)
		}
	}
	return s, nil
}

// validUpstream checks Upstream against the shape its mode actually consumes.
// The field carries two different things (see ServiceSpec.Upstream), and the one
// that matters is the one nobody would catch by reading: a URL where a DNS
// record's value belongs resolves for nobody and looks like a DNS fault.
func validUpstream(mode Mode, upstream string) error {
	switch mode {
	case ModeTunnelled:
		// cloudflared's ingress `service`: scheme://host[:port].
		u, err := url.Parse(upstream)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("spinup: %s upstream %q must be an origin url cloudflared can forward to, e.g. http://argosy:8096", ModeTunnelled, upstream)
		}
	case ModeDirect:
		if net.ParseIP(upstream) != nil {
			return nil // an A/AAAA value
		}
		if err := validHostname(upstream); err != nil {
			// Deliberately not the hostname error: the useful thing to say here
			// is what this field becomes, not what a hostname looks like.
			return fmt.Errorf("spinup: %s upstream %q must be an ip address or a hostname — it becomes a DNS record's value, which has nowhere to put a scheme, a port or a path", ModeDirect, upstream)
		}
	}
	return nil
}

// validHostname rejects what is not a hostname, on the strict side, because
// whatever passes here becomes this axis's identity key *and* gets written into
// a DNS record. The zone API is the real authority on what it will accept; this
// is about not handing it — or the resource table — something that was never a
// hostname in the first place.
//
// Assumes h is already lowercased by Normalized. Wildcards are rejected: a spec
// stands up one service on one hostname, and `*.zerogravity.industries` in a
// resource row would claim every hostname in the zone.
func validHostname(h string) error {
	if h == "" {
		return fmt.Errorf("spinup: hostname is required")
	}
	if len(h) > 253 {
		return fmt.Errorf("spinup: hostname %q is longer than 253 characters", h)
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return fmt.Errorf("spinup: hostname %q must be fully qualified", h)
	}
	for _, label := range labels {
		switch {
		case label == "":
			// Covers a leading dot, a trailing one Normalized didn't trim, and
			// "a..b" — each of which would otherwise be a distinct identity key
			// for the same host.
			return fmt.Errorf("spinup: hostname %q has an empty label", h)
		case len(label) > 63:
			return fmt.Errorf("spinup: hostname %q has a label longer than 63 characters", h)
		case label[0] == '-', label[len(label)-1] == '-':
			return fmt.Errorf("spinup: hostname %q has a label starting or ending with a hyphen", h)
		}
		for _, r := range label {
			// Everything else — ':' and a port, '/' and a path, '*', '?', '@',
			// spaces, newlines, control characters — is rejected by omission,
			// which is the point of an allow-list here.
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				continue
			}
			return fmt.Errorf("spinup: hostname %q contains %q, which is not valid in a hostname", h, r)
		}
	}
	return nil
}

// callsFor reports whether this spec calls for a resource kind at all. It is
// the whole tunnelled/direct split, in one predicate, and the orchestrator calls
// exactly this — so what the tests pin is what decides.
//
// The split reaches three steps rather than the two the epic first described: a
// direct service skips the ingress route *and* takes a different Access
// application type, and AccessNone skips the Access step outright.
//
// Kinds it excludes are not silently dropped by the orchestrator — they are
// reported as not applicable, so a report always has a line per kind and
// "nothing about the tunnel" can't be read as "the tunnel is fine".
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

// dependsOn reports which steps must be in place before kind may be *created or
// changed*. It is what makes KindOrder mean something: ordering alone only
// closes the window if the earlier step actually landed, and a step that failed,
// was unavailable, or couldn't be read has not landed.
//
// Only DNS has prerequisites, because DNS is the step that makes the hostname
// live:
//
//   - A gated service must have its Access application first. Publishing the
//     record while that step is unresolved is the ungated-exposure window
//     KindOrder exists to prevent — and unlike the 502 below, it is not
//     self-announcing: the service works, it just lets everyone in.
//   - A tunnelled service must have its ingress route first, or the hostname
//     resolves to a tunnel that will not serve it.
//
// A *bookmark* Access app is deliberately not a prerequisite. It is a launcher
// tile in front of a service that holds its own login, so its absence costs an
// icon, not a gate — blocking a working service's DNS over a missing tile would
// be the wrong trade, and this is the distinction that makes AccessShape worth
// having as more than a flag.
func (s ServiceSpec) dependsOn(kind model.ResourceKind) []model.ResourceKind {
	if kind != model.ResourceDNSRecord {
		return nil
	}
	var deps []model.ResourceKind
	if s.Mode == ModeTunnelled {
		deps = append(deps, model.ResourceTunnelRoute)
	}
	if s.Access == AccessGated {
		deps = append(deps, model.ResourceAccessApp)
	}
	return deps
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

// ErrTunnelUnconfigured is returned when a spec names a tunnel this deployment
// has no id for. Validate has already rejected refs that are not names at all,
// so this is only ever a legal ref nobody has wired — `dev`, until PRSR-33.
//
// A sentinel rather than a bare error because it is the one refusal Ensure can
// still raise that is the *caller's* to fix rather than an outage, and the HTTP
// surface has to tell those apart to choose a status code. Matching on the
// message would have made that distinction depend on the wording above.
var ErrTunnelUnconfigured = errors.New("spinup: tunnel is not configured")

// Resolve returns the tunnel id for a ref.
func (ts TunnelSet) Resolve(ref TunnelRef) (string, error) {
	if id := strings.TrimSpace(ts[ref]); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("%w: %q (%s)", ErrTunnelUnconfigured, ref, configuredTunnels(ts))
}

// configuredTunnels renders what *is* wired, so the refusal distinguishes "you
// named the wrong one" from "this deployment has no tunnels at all".
//
// It deliberately doesn't name the environment variable. Env keys live in
// internal/config and the composition root, which is also where the next one
// will be added (PRSR-33) — a name spelled out here would go stale silently.
func configuredTunnels(ts TunnelSet) string {
	var have []TunnelRef
	for _, ref := range tunnelRefs {
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
