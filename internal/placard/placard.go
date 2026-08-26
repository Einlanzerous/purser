// Package placard resolves a service's launcher icon from Placard (IDEA-22),
// the estate's mark registry, so that a spin-up spec names a *service* rather
// than a hand-typed CDN path (PRSR-37).
//
// # Why this exists
//
// Cloudflare stores whatever `logo_url` it is given and never validates it, and
// the App Launcher falls back to two grey initials when the fetch fails — so a
// wrong URL is indistinguishable from an unset one, with nothing erroring
// anywhere. PRSR-38's live audit found the consequence: of ten Access
// applications, three had a working icon, switchyard's stored URL was a live 404
// (its repo is private, so jsDelivr could never serve it at any path), and
// argosy's *resolved* — but to the 3.6:1 wordmark rather than the tile mark, so
// it letterboxed to a sliver in a square tile.
//
// That last case is the argument for this package rather than for a better
// checker. **A working logo_url is not a correct one**, and no fetch can tell
// the difference. Placard is the thing that knows which asset is the tile asset:
// it publishes the ship glyph alone for argosy precisely because "text is
// illegible at tile size".
//
// # Resolve, do not trust
//
// Placard's per-file `check` is a periodic monitor carrying a `checked_at`, so
// it can be stale, and a stale green is how the silent failure comes back. This
// package uses /api/services to *pick* the URL — and to skip a file Placard says
// is `missing` — and nothing more. Whether the URL actually serves an image is
// decided at write time by the Access provisioner's own sessionless fetch, which
// is the check that runs at the moment it matters. Purser's standing rule points
// the same way: never treat unverifiable as absent.
//
// # Three answers, not two
//
// Mark separates "Placard has no mark for this slug" from "Placard could not be
// asked". The first is a fact about the estate — services.json covers seven
// slugs and chronicle is not one of them, and a brand-new service is stood up
// before its mark is drawn — and the honest rendering of it is the launcher's
// initials. The second is a Placard outage, and treating it as "no mark" would
// clear working icons across the estate every time this service restarted. It is
// the same distinction logoOK/logoBroken/logoUnknown draws one layer up, for the
// same reason.
package placard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config points the resolver at Placard.
type Config struct {
	// BaseURL is Placard's API root, e.g. "http://placard:4009"
	// (PURSER_PLACARD_URL). Empty disables resolution: Mark reports that it
	// could not be asked, which leaves icons exactly as they are.
	BaseURL string
	// HTTPClient talks to Placard. It gets a short timeout by default: this is
	// an in-estate lookup decorating a tile, and it must not consume the budget
	// for the call that actually creates the gate.
	HTTPClient *http.Client
	// Variant is which rendering to resolve. Empty means VariantLight.
	//
	// On the resolver rather than on Mark's signature so that the consumer's
	// interface stays a one-method `Mark(ctx, key)` and nothing outside this
	// package has to import it to name a variant. That is the right seam today,
	// when the choice is a property of the surface — Cloudflare's App Launcher
	// is a light surface — and the wrong one the moment "dev" becomes a property
	// of the *service*, since a dev instance wants the badged mark while its
	// production sibling does not. That is PRSR-33's question, and answering it
	// means moving this to a per-call argument.
	Variant Variant
}

// Configured reports whether the resolver has somewhere to ask.
func (c Config) Configured() bool { return strings.TrimSpace(c.BaseURL) != "" }

// Resolver looks up marks in Placard.
//
// It holds no cache. A spin-up asks at most twice — once for the plan and once
// for the apply — against a service on the same host, and a cache in a process
// that runs for weeks (`purser serve`) would answer with a mark that has since
// been drawn or redrawn, which is the failure this package exists to end.
type Resolver struct {
	base    string
	http    *http.Client
	variant Variant
}

// New builds a Resolver.
func New(cfg Config) *Resolver {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	v := cfg.Variant
	if v == "" {
		v = VariantLight
	}
	return &Resolver{
		base:    strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		http:    hc,
		variant: v,
	}
}

// Variant names which rendering of a mark a surface wants.
//
// Placard publishes <slug>/<slug>-mark-{light,dark}.png plus generated -dev
// siblings carrying a yellow DEV badge. The names describe the surface the mark
// is drawn *for*, not the ink: "light" is the mark for a light background.
type Variant string

const (
	// VariantLight is the mark for a light surface, which is what Cloudflare's
	// App Launcher is. Cloudflare stores exactly one URL per application and the
	// launcher has both themes, so one of them has to be chosen; light is the
	// one the estate already uses, including on Placard's own tile.
	VariantLight Variant = "light"
	// VariantDark is the mark for a dark surface. Nothing selects it yet.
	VariantDark Variant = "dark"
)

// Mark returns the canonical URL of a service's launcher mark.
//
// found is false when Placard answered and has no usable mark for the slug —
// either it has never heard of the service, or the file is recorded `missing`.
// A non-nil error means Placard could not be asked or could not be read, which
// is emphatically not the same answer and must not be collapsed into it.
func (r *Resolver) Mark(ctx context.Context, key string) (string, bool, error) {
	if r.base == "" {
		return "", false, fmt.Errorf("placard: not configured — set PURSER_PLACARD_URL to resolve a service's icon by name")
	}
	slug := strings.ToLower(strings.TrimSpace(key))
	if slug == "" {
		return "", false, fmt.Errorf("placard: no service key to look up")
	}

	doc, err := r.services(ctx)
	if err != nil {
		return "", false, err
	}
	for _, svc := range doc.Services {
		if !strings.EqualFold(strings.TrimSpace(svc.Slug), slug) {
			continue
		}
		// Matched on name rather than on `role`, which is prose Placard writes
		// for humans ("for light surfaces") and may reword. The filename is the
		// documented convention and is what the repo's own layout guarantees.
		want := fmt.Sprintf("%s-mark-%s.png", slug, r.variant)
		for _, f := range svc.Files {
			if f.Name != want {
				continue
			}
			// `missing` is Placard telling us the file is not in the repo. Its
			// canonical_url is still populated, and writing it would put a
			// guaranteed 404 into the launcher — which is the exact state
			// switchyard has been in.
			if f.State != stateInRepo {
				return "", false, nil
			}
			if u := strings.TrimSpace(f.CanonicalURL); u != "" {
				return u, true, nil
			}
			return "", false, nil
		}
		// The slug is known but this variant is not published.
		return "", false, nil
	}
	// Placard has never heard of this service. Not an error: services.json
	// covers seven slugs, and a spin-up necessarily runs before a brand-new
	// service's mark is drawn.
	return "", false, nil
}

// stateInRepo is the only file state whose canonical_url is worth writing.
const stateInRepo = "in_repo"

// servicesDoc is the shape of GET /api/services, reduced to the fields this
// package reads. Placard carries more per file — a `check` block with a
// `checked_at`, a `mirror_url` served from Placard's own hostname — and both are
// deliberately ignored: see the package doc for the check, and Mark for why the
// canonical (CDN) URL is the one written rather than the mirror.
type servicesDoc struct {
	Services []struct {
		Slug  string `json:"slug"`
		Files []struct {
			Name         string `json:"name"`
			State        string `json:"state"`
			CanonicalURL string `json:"canonical_url"`
		} `json:"files"`
	} `json:"services"`
}

// services fetches and decodes the registry.
func (r *Resolver) services(ctx context.Context) (*servicesDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/api/services", nil)
	if err != nil {
		return nil, fmt.Errorf("placard: new request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("placard: GET /api/services: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("placard: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("placard: GET /api/services: http %d", resp.StatusCode)
	}
	var doc servicesDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("placard: decode /api/services: %w", err)
	}
	return &doc, nil
}
