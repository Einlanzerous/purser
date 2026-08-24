package spinup

import (
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/model"
)

// tunnelledSpec is a valid tunnelled spec, for tests that vary one field.
func tunnelledSpec() ServiceSpec {
	return ServiceSpec{
		Key:      "interlock",
		Hostname: "interlock.zerogravity.industries",
		Mode:     ModeTunnelled,
		Upstream: "http://interlock:4010",
		Access:   AccessGated,
		Tunnel:   TunnelProd,
	}
}

// directSpec is Argosy's shape: the epic's pilot, and the case a spec that could
// only describe tunnelled services would not be able to express.
//
// Upstream is a bare address, not a URL — on this path it becomes a DNS
// record's value.
func directSpec() ServiceSpec {
	return ServiceSpec{
		Key:      "argosy",
		Hostname: "argosy.zerogravity.industries",
		Mode:     ModeDirect,
		Upstream: "100.64.0.7",
		Access:   AccessBookmark,
	}
}

func TestValidate_Accepts(t *testing.T) {
	for _, spec := range []ServiceSpec{tunnelledSpec(), directSpec()} {
		if _, err := spec.Validate(); err != nil {
			t.Errorf("%s: %v", spec.Key, err)
		}
	}
}

func TestValidate_Refusals(t *testing.T) {
	tests := []struct {
		name string
		spec func(ServiceSpec) ServiceSpec
		base ServiceSpec
		want string
	}{
		{
			// The ingress configuration is one document per tunnel, and there
			// are two tunnels now. Defaulting would write into whichever one
			// config happened to name.
			name: "tunnelled spec must name its tunnel",
			base: tunnelledSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.Tunnel = ""; return s },
			want: "must name its tunnel",
		},
		{
			name: "unknown tunnel ref",
			base: tunnelledSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.Tunnel = "staging"; return s },
			want: `unknown tunnel "staging"`,
		},
		{
			// Nothing would read it, so an operator who set it believes
			// something about this spec that isn't true.
			name: "direct spec must not name a tunnel",
			base: directSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.Tunnel = TunnelProd; return s },
			want: "must not name a tunnel",
		},
		{
			name: "mode is required",
			base: directSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.Mode = ""; return s },
			want: "mode is required",
		},
		{
			name: "unknown mode",
			base: directSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.Mode = "proxied"; return s },
			want: `unknown mode "proxied"`,
		},
		{
			name: "access shape is required",
			base: directSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.Access = ""; return s },
			want: "access shape is required",
		},
		{
			name: "hostname is required",
			base: directSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.Hostname = ""; return s },
			want: "hostname is required",
		},
		{
			name: "hostname must be fully qualified",
			base: directSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.Hostname = "argosy"; return s },
			want: "fully qualified",
		},
		{
			name: "upstream is required",
			base: directSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.Upstream = ""; return s },
			want: "upstream is required",
		},
		{
			// A DNS record's value has nowhere to put a scheme, a port or a
			// path, so this resolves for nobody and reads as a DNS fault.
			name: "direct upstream must not be a url",
			base: directSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.Upstream = "https://100.64.0.7:8096"; return s },
			want: "ip address or a hostname",
		},
		{
			// cloudflared's ingress `service` value needs one.
			name: "tunnelled upstream must carry a scheme",
			base: tunnelledSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.Upstream = "interlock:4010"; return s },
			want: "origin url",
		},
		{
			// The launcher loads it from the viewer's browser, so a relative
			// path cannot resolve for anyone.
			name: "logo url must be absolute",
			base: directSpec(),
			spec: func(s ServiceSpec) ServiceSpec { s.LogoURL = "/assets/argosy.png"; return s },
			want: "must be an absolute https:// url",
		},
		{
			// Blocked as mixed content inside the launcher's https page, which
			// is the silent grey-initials failure the field exists to avoid.
			name: "logo url must be https",
			base: directSpec(),
			spec: func(s ServiceSpec) ServiceSpec {
				s.LogoURL = "http://placard.zerogravity.industries/argosy.png"
				return s
			},
			want: "must be an absolute https:// url",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.spec(tc.base).Validate()
			if err == nil {
				t.Fatal("want a refusal, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// The hostname is this axis's identity key and hostnames are case-insensitive,
// so it is folded once, here, rather than at each of the places that compare it.
func TestNormalized_FoldsHostname(t *testing.T) {
	s := directSpec()
	s.Hostname = "  Argosy.ZeroGravity.Industries.  "
	got, err := s.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "argosy.zerogravity.industries" {
		t.Errorf("hostname = %q, want it lowercased and with the trailing dot trimmed", got.Hostname)
	}
	if got.DisplayName != "argosy" {
		t.Errorf("display name = %q, want it defaulted to the key", got.DisplayName)
	}
}

// Key is folded too. service_key is compared exactly by the store, so two
// casings would be two services sharing one hostname's resources — and
// ServiceResourcesFor would answer each of them with half the truth.
func TestNormalized_FoldsKey(t *testing.T) {
	s := directSpec()
	s.Key = " Argosy "
	got, err := s.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "argosy" {
		t.Errorf("key = %q, want it lowercased", got.Key)
	}
}

// Whatever passes validation becomes this axis's identity key *and* the name in
// a DNS record, so the check is an allow-list. These are the shapes a looser one
// lets through.
func TestValidate_RejectsNonHostnames(t *testing.T) {
	for _, host := range []string{
		"argosy.zerogravity.industries:8096", // a port
		"https://argosy.zerogravity.industries",
		"argosy.zerogravity.industries/path",
		"*.zerogravity.industries", // would claim every hostname in the zone
		".zerogravity.industries",  // leading dot: an empty label
		"argosy..zerogravity.industries",
		"argo\nsy.zerogravity.industries",
		"argosy zerogravity.industries",
		"-argosy.zerogravity.industries",
		"argosy-.zerogravity.industries",
		"argosy@zerogravity.industries",
		"argosy?.zerogravity.industries",
		strings.Repeat("a", 64) + ".zerogravity.industries",
	} {
		s := directSpec()
		s.Hostname = host
		if _, err := s.Validate(); err == nil {
			t.Errorf("accepted %q as a hostname", host)
		}
	}
}

// Trailing dots are trimmed however many there are: one left behind is an empty
// label, and an empty label is a second identity key for the same host.
func TestValidate_AcceptsOrdinaryHostnames(t *testing.T) {
	for _, host := range []string{
		"argosy.zerogravity.industries",
		"argosy.zerogravity.industries..",
		"argosy-dev.zerogravity.industries",
		"a.b.c.zerogravity.industries",
	} {
		s := directSpec()
		s.Hostname = host
		if _, err := s.Validate(); err != nil {
			t.Errorf("rejected %q: %v", host, err)
		}
	}
}

// The tunnelled/direct split reaches three steps, not two: a direct service
// skips the ingress route entirely and takes a different Access application
// type. Getting this wrong is the design risk the foundation ticket names.
//
// Asserted through callsFor, which is the predicate the orchestrator itself
// calls — a separate "which steps does this spec have" helper would be a second
// implementation of the same decision, and the tested one would not be the one
// that runs.
func TestCallsFor(t *testing.T) {
	tests := []struct {
		name string
		spec ServiceSpec
		want map[model.ResourceKind]bool
	}{
		{
			name: "tunnelled and gated: all three",
			spec: tunnelledSpec(),
			want: map[model.ResourceKind]bool{
				model.ResourceTunnelRoute: true, model.ResourceAccessApp: true, model.ResourceDNSRecord: true,
			},
		},
		{
			name: "direct: no ingress route",
			spec: directSpec(),
			want: map[model.ResourceKind]bool{
				model.ResourceTunnelRoute: false, model.ResourceAccessApp: true, model.ResourceDNSRecord: true,
			},
		},
		{
			name: "ungated tunnelled service: no access app",
			spec: func() ServiceSpec { s := tunnelledSpec(); s.Access = AccessNone; return s }(),
			want: map[model.ResourceKind]bool{
				model.ResourceTunnelRoute: true, model.ResourceAccessApp: false, model.ResourceDNSRecord: true,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for kind, want := range tc.want {
				if got := tc.spec.callsFor(kind); got != want {
					t.Errorf("callsFor(%q) = %v, want %v", kind, got, want)
				}
			}
		})
	}
}

// DNS is ordered last so a gated service never resolves before its gate exists,
// and ordering only means something if the later step knows what it waited for.
// A bookmark is deliberately not a prerequisite: it is a tile, not a gate.
func TestDependsOn(t *testing.T) {
	tests := []struct {
		name string
		spec ServiceSpec
		want []model.ResourceKind
	}{
		{
			name: "tunnelled and gated waits for both",
			spec: tunnelledSpec(),
			want: []model.ResourceKind{model.ResourceTunnelRoute, model.ResourceAccessApp},
		},
		{
			name: "direct bookmark waits for nothing",
			spec: directSpec(),
			want: nil,
		},
		{
			name: "direct and gated waits for the access app",
			spec: func() ServiceSpec { s := directSpec(); s.Access = AccessGated; return s }(),
			want: []model.ResourceKind{model.ResourceAccessApp},
		},
		{
			name: "tunnelled and ungated waits for the route",
			spec: func() ServiceSpec { s := tunnelledSpec(); s.Access = AccessNone; return s }(),
			want: []model.ResourceKind{model.ResourceTunnelRoute},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.dependsOn(model.ResourceDNSRecord)
			if len(got) != len(tc.want) {
				t.Fatalf("dependsOn(dns) = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("dependsOn(dns) = %v, want %v", got, tc.want)
				}
			}
			// Nothing else has prerequisites; the route and the app are
			// independent of each other.
			for _, k := range []model.ResourceKind{model.ResourceTunnelRoute, model.ResourceAccessApp} {
				if deps := tc.spec.dependsOn(k); deps != nil {
					t.Errorf("dependsOn(%q) = %v, want none", k, deps)
				}
			}
		})
	}
}

// DNS is what makes the hostname live, so it is applied last: publishing the
// record first leaves a gated service reachable ungated until its Access app
// lands.
func TestKindOrder_DNSIsLast(t *testing.T) {
	last := model.KindOrder[len(model.KindOrder)-1]
	if last != model.ResourceDNSRecord {
		t.Errorf("KindOrder ends with %q; DNS must be last or a spin-up publishes a hostname before it is routed or gated", last)
	}
}

// A ref that names a real tunnel nobody has configured an id for is reported as
// unconfigured — and the refusal says what *is* configured, because the way out
// differs (set the env var vs. fix the spec).
func TestTunnelSet_Resolve(t *testing.T) {
	ts := TunnelSet{TunnelProd: "aef21667-03ce-45d3-b83c-d634822661cd"}

	id, err := ts.Resolve(TunnelProd)
	if err != nil {
		t.Fatal(err)
	}
	if id != "aef21667-03ce-45d3-b83c-d634822661cd" {
		t.Errorf("id = %q", id)
	}

	_, err = ts.Resolve(TunnelDev)
	if err == nil {
		t.Fatal("dev is not wired yet (PRSR-33); resolving it must refuse rather than fall back to prod")
	}
	if !strings.Contains(err.Error(), "prod") {
		t.Errorf("refusal %q should name the tunnels that are configured", err)
	}

	if _, err := (TunnelSet{}).Resolve(TunnelProd); err == nil {
		t.Error("an empty set must refuse rather than resolve to an empty id")
	}
}
