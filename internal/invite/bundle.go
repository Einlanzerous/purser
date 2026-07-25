package invite

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Einlanzerous/purser/internal/connector"
)

// DefaultProjectRole is the Switchyard *project membership* role a bundle grants
// when the invite doesn't say otherwise (SERV-47, owner's call).
//
// Note this is the project role (viewer | user | editor | admin), not the
// instance role (member | owner) — Switchyard has both, and they're set
// independently. A bundle grants "user" on projects while leaving the instance
// role at its preset default of member.
const DefaultProjectRole = "user"

// DefaultProjectGrant is the grant applied when a bundle names no projects of
// its own. "*" means every project; scope a bundle down by setting
// PURSER_BUNDLE_<NAME>_PROJECTS to an explicit key list instead.
const DefaultProjectGrant = "*:" + DefaultProjectRole

// Bundle is a named onboarding set: which services a person gets, and the
// Switchyard project access that comes with it.
type Bundle struct {
	// Services are the connector keys granted, in credential-block order.
	Services []string
	// Projects is the raw project-grant spec (e.g. "*:user"), applied only when
	// the invite itself carries no explicit --projects. Empty means
	// DefaultProjectGrant.
	Projects string
}

// BundleSet is the named onboarding bundles available to invites (SERV-47).
//
// A bundle is a named list of service keys expanded into the existing
// per-service orchestration — it introduces no new provisioning path and no new
// idempotency rules. That matters: idempotency is per (person × service), so
// overlapping bundles, a bundle overlapping an explicit --to, and re-inviting
// someone who already has half the set are all safe by construction.
type BundleSet struct {
	// Named maps a bundle name to its definition. Names match case-insensitively.
	Named map[string]Bundle
	// Default is the bundle used when a request names neither services nor a
	// bundle. Empty means "no default" — such a request is then rejected.
	Default string
}

// Names returns the known bundle names, sorted, for error messages and help.
func (b BundleSet) Names() []string {
	names := make([]string, 0, len(b.Named))
	for n := range b.Named {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Lookup returns a bundle by name. The bool reports whether it exists.
func (b BundleSet) Lookup(name string) (Bundle, bool) {
	bundle, ok := b.Named[strings.ToLower(strings.TrimSpace(name))]
	return bundle, ok
}

// resolveServices works out the final service list for a request, combining an
// explicit service list with a bundle. It returns the services and the name of
// the bundle that contributed (empty when the request was purely explicit).
//
// The three cases, in order:
//
//	--to only        → exactly those services
//	--bundle only    → the bundle's services
//	neither          → the default bundle
//
// Supplying both is allowed and takes the union, bundle first: "the family set,
// plus this one extra" is a real onboarding case, and since provisioning is per
// (person × service) the overlap costs nothing. Order is preserved and
// duplicates collapse, so the credential block reads in a stable order.
func (b BundleSet) resolveServices(services []string, bundle string) ([]string, string, error) {
	bundle = strings.ToLower(strings.TrimSpace(bundle))

	// Neither given: fall back to the default bundle, if there is one.
	if len(services) == 0 && bundle == "" {
		if b.Default == "" {
			return nil, "", fmt.Errorf("invite: no services given and no default bundle configured (set PURSER_DEFAULT_BUNDLE or pass --to/--bundle)")
		}
		bundle = strings.ToLower(b.Default)
		if _, ok := b.Lookup(bundle); !ok {
			return nil, "", fmt.Errorf("invite: default bundle %q is not defined (known bundles: %s)",
				bundle, b.namesForError())
		}
	}

	var out []string
	seen := make(map[string]bool)
	add := func(keys []string) {
		for _, k := range keys {
			if k = strings.TrimSpace(k); k != "" && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}

	applied := ""
	if bundle != "" {
		def, ok := b.Lookup(bundle)
		if !ok {
			return nil, "", fmt.Errorf("invite: unknown bundle %q (known bundles: %s)", bundle, b.namesForError())
		}
		add(def.Services)
		applied = bundle
	}
	add(services)

	if len(out) == 0 {
		return nil, "", fmt.Errorf("invite: bundle %q expands to no services", bundle)
	}
	return out, applied, nil
}

// projectsFor returns the project grants a bundle contributes. An invite that
// carries its own grants always wins; otherwise the bundle's spec applies, and
// a bundle with no spec of its own falls back to DefaultProjectGrant.
func (b BundleSet) projectsFor(bundleName string, explicit []connector.ProjectGrant) ([]connector.ProjectGrant, error) {
	if len(explicit) > 0 || bundleName == "" {
		return explicit, nil
	}
	def, ok := b.Lookup(bundleName)
	if !ok {
		return explicit, nil
	}
	spec := strings.TrimSpace(def.Projects)
	if spec == "" {
		spec = DefaultProjectGrant
	}
	grants, err := ParseProjectGrants(spec)
	if err != nil {
		return nil, fmt.Errorf("invite: bundle %q: %w", bundleName, err)
	}
	return grants, nil
}

// namesForError renders the known bundle names for an error message, or a
// placeholder when none are configured.
func (b BundleSet) namesForError() string {
	names := b.Names()
	if len(names) == 0 {
		return "none configured"
	}
	return strings.Join(names, ", ")
}

// ParseProjectGrants parses a spec like "*:viewer,IDEA:editor" into grants.
// "*" is the wildcard meaning every project; a specific key overrides it.
// Malformed entries are an error rather than a silent skip, so a typo in a
// bundle definition surfaces at invite time instead of quietly granting nothing.
func ParseProjectGrants(spec string) ([]connector.ProjectGrant, error) {
	var out []connector.ProjectGrant
	for _, p := range strings.Split(spec, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		key, role, ok := strings.Cut(p, ":")
		key, role = strings.TrimSpace(key), strings.TrimSpace(role)
		if !ok || key == "" || role == "" {
			return nil, fmt.Errorf("malformed project grant %q (want KEY:ROLE, e.g. %s)", p, DefaultProjectGrant)
		}
		out = append(out, connector.ProjectGrant{Key: key, Role: role})
	}
	return out, nil
}
