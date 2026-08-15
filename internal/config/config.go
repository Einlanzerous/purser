// Package config loads Purser's configuration from environment variables,
// following the construct-server house convention: 12-factor env vars, a
// <SERVICE>_-prefixed namespace, and a DATABASE_URL fallback. There are no
// config files.
package config

import (
	"log"
	"os"
	"strconv"
	"strings"
)

// Config is Purser's fully-resolved configuration.
type Config struct {
	Addr        string // HTTP listen address (PURSER_ADDR)
	DatabaseURL string // Postgres DSN (PURSER_DATABASE_URL | DATABASE_URL)
	APIToken    string // bearer token protecting the HTTP API (PURSER_API_TOKEN)

	Switchyard SwitchyardConfig
	Cloudflare CloudflareConfig
	Lyceum     LyceumConfig
	Argosy     ArgosyConfig
	SMTP       SMTPConfig
	Bundles    BundleConfig
}

// Bundle is one named onboarding set as configured: the services granted, plus
// the Switchyard project-grant spec that comes with them.
type Bundle struct {
	Services []string // connector keys
	Projects string   // raw "*:user" spec; empty => the invite layer's default
}

// BundleConfig holds the named onboarding bundles (PRSR-12). A bundle is a
// named set of services, so the common onboard is one flag instead of a
// hand-assembled --to list.
type BundleConfig struct {
	// Named maps a lowercased bundle name to its definition, loaded from
	//   PURSER_BUNDLE_<NAME>=svc1,svc2               (services)
	//   PURSER_BUNDLE_<NAME>_PROJECTS=*:user         (optional project grants)
	Named map[string]Bundle
	// Default is the bundle used when an invite names neither services nor a
	// bundle (PURSER_DEFAULT_BUNDLE).
	Default string
}

// DefaultBundleName is the bundle assumed when PURSER_DEFAULT_BUNDLE is unset.
// `media` is the safer default of the two built-ins: it's the smaller grant, so
// an unqualified invite can't accidentally hand someone Switchyard.
const DefaultBundleName = "media"

// Built-in bundles, used when nothing is configured via PURSER_BUNDLE_*.
//
//   - media: non-technical household members — the things you watch and read.
//     Cloudflare is in here because Lyceum sits behind the Access gate; without
//     it the person clears no edge and can't reach Lyceum at all. Argosy is on
//     the direct path and needs no Access grant, but the overlap is free.
//   - all: everything, for people who'd actually want Switchyard too.
var builtinBundles = map[string]Bundle{
	"media": {Services: []string{"cloudflare", "lyceum", "argosy"}},
	"all":   {Services: []string{"cloudflare", "switchyard", "lyceum", "argosy"}},
}

// bundleEnvPrefix is the env namespace a bundle definition lives under.
const bundleEnvPrefix = "PURSER_BUNDLE_"

// projectsEnvSuffix marks the project-grant variant of a bundle variable.
const projectsEnvSuffix = "_PROJECTS"

// loadBundles reads every PURSER_BUNDLE_<NAME> from the environment. When no
// bundle is defined at all, the built-ins are supplied so a fresh install still
// has a working onboard. Configuring any bundle replaces the built-in set
// wholesale rather than merging — partial overrides silently inheriting halves
// of a default set is worse than an explicit, complete definition.
func loadBundles() BundleConfig {
	named := make(map[string]Bundle)
	projects := make(map[string]string)

	for _, kv := range os.Environ() {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, bundleEnvPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, bundleEnvPrefix)
		if strings.HasSuffix(name, projectsEnvSuffix) {
			if n := strings.ToLower(strings.TrimSuffix(name, projectsEnvSuffix)); n != "" {
				projects[n] = strings.TrimSpace(val)
			}
			continue
		}
		name = strings.ToLower(name)
		if name == "" {
			continue
		}
		if services := splitCSV(val); len(services) > 0 {
			named[name] = Bundle{Services: services}
		} else {
			log.Printf("config: %s is empty — ignoring bundle %q", key, name)
		}
	}

	if len(named) == 0 {
		for n, b := range builtinBundles {
			named[n] = b
		}
	}
	// Attach project specs, warning about ones that name no bundle — almost
	// always a typo, and silently ignoring it would grant the wrong level.
	for n, spec := range projects {
		b, ok := named[n]
		if !ok {
			log.Printf("config: %s%s%s names unknown bundle %q — ignoring",
				bundleEnvPrefix, strings.ToUpper(n), projectsEnvSuffix, n)
			continue
		}
		b.Projects = spec
		named[n] = b
	}

	return BundleConfig{Named: named, Default: envOr("PURSER_DEFAULT_BUNDLE", DefaultBundleName)}
}

// splitCSV parses a comma-separated list, trimming blanks.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ArgosyConfig configures the Argosy connector.
type ArgosyConfig struct {
	BaseURL        string // PURSER_ARGOSY_BASE_URL (internal API base)
	ProvisionToken string // PURSER_ARGOSY_PROVISION_TOKEN (== argosy's ARGOSY_PROVISION_TOKEN)
	AppURL         string // PURSER_ARGOSY_URL (public sign-in URL for the block)
}

// Configured reports whether the Argosy connector can run.
func (c ArgosyConfig) Configured() bool { return c.BaseURL != "" && c.ProvisionToken != "" }

// LyceumConfig configures the Lyceum connector.
type LyceumConfig struct {
	BaseURL    string // PURSER_LYCEUM_BASE_URL (internal API base)
	OwnerToken string // PURSER_LYCEUM_OWNER_TOKEN (owner session token, lyc_…)
	AppURL     string // PURSER_LYCEUM_URL (public app URL for the block; optional)
}

// Configured reports whether the Lyceum connector can run.
func (c LyceumConfig) Configured() bool { return c.BaseURL != "" && c.OwnerToken != "" }

// SwitchyardConfig configures the Switchyard connector.
type SwitchyardConfig struct {
	BaseURL  string // PURSER_SWITCHYARD_BASE_URL (internal API base)
	Token    string // PURSER_SWITCHYARD_TOKEN (admin sw_ token)
	LoginURL string // PURSER_SWITCHYARD_URL (public login URL for the block)
}

// Configured reports whether the Switchyard connector can run.
func (c SwitchyardConfig) Configured() bool { return c.BaseURL != "" && c.Token != "" }

// CloudflareConfig configures the Cloudflare Access connector.
type CloudflareConfig struct {
	APIToken   string // PURSER_CF_API_TOKEN
	AccountID  string // PURSER_CF_ACCOUNT_ID
	GroupID    string // PURSER_CF_ACCESS_GROUP_ID
	GroupName  string // PURSER_CF_ACCESS_GROUP_NAME
	TeamDomain string // PURSER_CF_TEAM_DOMAIN
	AppsNote   string // PURSER_CF_APPS_NOTE
	Launcher   string // PURSER_LAUNCHER_URL; empty => derived from TeamDomain

	// ZoneID and TunnelID are the service spin-up axis's coordinates (PRSR-22):
	// the zone DNS records get created in, and the cloudflared tunnel whose
	// ingress those hostnames route through. The tunnel is remotely-managed
	// (PRSR-26), so both are reachable over the API.
	//
	// Nothing reads them yet — the spin-up connectors aren't built. They are
	// carried here so the deploy-side plumbing (.env, the PROD_ENV_FILE secret,
	// the compose file) lands once instead of twice, and they stay out of the
	// Access connector's Config on purpose: it does not use them, and folding
	// them into its readiness check would take `--to cloudflare` offline for
	// every deployment that hasn't set them.
	//
	// Neither gets a default, unlike TeamDomain above. They are account-specific
	// opaque ids, and a wrong one here would have Purser writing DNS records
	// into somebody else's zone rather than merely rendering a wrong link.
	ZoneID   string // PURSER_CF_ZONE_ID
	TunnelID string // PURSER_CF_TUNNEL_ID
}

// LauncherURL is Cloudflare's App Launcher — the page listing the Access-gated
// apps a person can reach. It falls back to the team domain, so the launcher
// works with no configuration beyond what the connector already needs.
//
// It returns "" for a zero-value config, which is a guard for direct callers
// rather than a reachable production state: Load gives TeamDomain a hardcoded
// default, so a deployment always has *some* launcher. That cuts both ways — a
// tenant that sets PURSER_CF_* but leaves PURSER_CF_TEAM_DOMAIN unset inherits
// this repo's default domain rather than getting no link. Set the team domain
// explicitly when pointing the connector at a different Cloudflare account.
func (c CloudflareConfig) LauncherURL() string {
	if u := strings.TrimSpace(c.Launcher); u != "" {
		return normalizeURL(u)
	}
	return normalizeURL(c.TeamDomain)
}

// normalizeURL turns a configured host-or-URL into something clickable. The two
// sources disagree on shape by nature — a team domain is a bare host, an
// explicit override is usually a full URL — and the credential block is plain
// text, so whatever lands here has to already be a working link. Applying
// "https://" blindly would produce https://https://… for a scheme-bearing
// value; applying it never would emit unclickable text for a bare host.
func normalizeURL(v string) string {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://") {
		return v
	}
	return "https://" + v
}

// SMTPConfig configures email delivery.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	TLS      string
}

// Configured reports whether email delivery is available.
func (c SMTPConfig) Configured() bool { return c.Host != "" && c.From != "" }

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	return Config{
		Addr:        envOr("PURSER_ADDR", ":4006"),
		DatabaseURL: firstEnv([]string{"PURSER_DATABASE_URL", "DATABASE_URL"}, defaultDatabaseURL),
		APIToken:    os.Getenv("PURSER_API_TOKEN"),
		Switchyard: SwitchyardConfig{
			BaseURL:  envOr("PURSER_SWITCHYARD_BASE_URL", "http://switchyard:4002"),
			Token:    os.Getenv("PURSER_SWITCHYARD_TOKEN"),
			LoginURL: envOr("PURSER_SWITCHYARD_URL", "https://switchyard.zerogravity.industries"),
		},
		Cloudflare: CloudflareConfig{
			APIToken:   os.Getenv("PURSER_CF_API_TOKEN"),
			AccountID:  os.Getenv("PURSER_CF_ACCOUNT_ID"),
			GroupID:    os.Getenv("PURSER_CF_ACCESS_GROUP_ID"),
			GroupName:  envOr("PURSER_CF_ACCESS_GROUP_NAME", "zerogravity-members"),
			TeamDomain: envOr("PURSER_CF_TEAM_DOMAIN", "zero-gravity-industries.cloudflareaccess.com"),
			AppsNote:   envOr("PURSER_CF_APPS_NOTE", "Switchyard and the other tunneled Construct apps"),
			Launcher:   os.Getenv("PURSER_LAUNCHER_URL"),
			ZoneID:     os.Getenv("PURSER_CF_ZONE_ID"),
			TunnelID:   os.Getenv("PURSER_CF_TUNNEL_ID"),
		},
		Lyceum: LyceumConfig{
			BaseURL:    envOr("PURSER_LYCEUM_BASE_URL", "http://lyceum:4005"),
			OwnerToken: os.Getenv("PURSER_LYCEUM_OWNER_TOKEN"),
			AppURL:     os.Getenv("PURSER_LYCEUM_URL"),
		},
		Argosy: ArgosyConfig{
			BaseURL:        envOr("PURSER_ARGOSY_BASE_URL", "http://argosy:8096"),
			ProvisionToken: os.Getenv("PURSER_ARGOSY_PROVISION_TOKEN"),
			AppURL:         envOr("PURSER_ARGOSY_URL", "https://argosy.zerogravity.industries"),
		},
		Bundles: loadBundles(),
		SMTP: SMTPConfig{
			Host:     os.Getenv("PURSER_SMTP_HOST"),
			Port:     envOrInt("PURSER_SMTP_PORT", 587),
			Username: os.Getenv("PURSER_SMTP_USERNAME"),
			Password: os.Getenv("PURSER_SMTP_PASSWORD"),
			From:     os.Getenv("PURSER_SMTP_FROM"),
			TLS:      envOr("PURSER_SMTP_TLS", "starttls"),
		},
	}
}

// defaultDatabaseURL points at the purser database on the shared construct-server
// Postgres. The password is supplied out of band via DATABASE_URL / PGPASSWORD.
const defaultDatabaseURL = "postgres://purser_user@localhost:5432/purser?sslmode=disable"

func firstEnv(keys []string, def string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("config: %s=%q is not an integer; using %d", key, os.Getenv(key), def)
	}
	return def
}
