package placard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// liveServices is Placard's real /api/services response, trimmed to two
// services and the two files this package reads from each. Every field and
// value here was observed on 2026-08-26 (PRSR-37), not imagined from the shape
// this package wanted.
//
// That is deliberate and it is the lesson PRSR-38 paid for twice: a fixture
// built to match the consumer asserts the consumer's assumptions rather than
// the API, and five green tests could not see that argosy's live tile carried a
// logo the fixture did not.
//
// The two services chosen are the two cases that matter. argosy is `in_repo`
// with a working mark. **wiki is the trap**: its status is `unset` and its files
// are `missing` — yet `canonical_url` is still fully populated, because Placard
// reports where the file *would* live. Reading the URL without reading the state
// writes a guaranteed 404 into the launcher, which is precisely the condition
// this whole area exists to end.
const liveServices = `{
  "repo": "Einlanzerous/placard",
  "canonical_base": "https://cdn.jsdelivr.net/gh/Einlanzerous/placard@main/",
  "uploads_enabled": true,
  "services": [
    {
      "slug": "argosy",
      "name": "Argosy",
      "note": "The canonical copies live here now: the ship glyph alone (the full wordmark lockup stays in the argosy repo; text is illegible at tile size).",
      "legacy_source": "argosy@main/assets/argosy_white_transparent.png",
      "status": "in_repo",
      "files": [
        {
          "name": "argosy-mark-light.png",
          "path": "argosy/argosy-mark-light.png",
          "role": "for light surfaces",
          "state": "in_repo",
          "canonical_url": "https://cdn.jsdelivr.net/gh/Einlanzerous/placard@main/argosy/argosy-mark-light.png",
          "mirror_url": "/argosy/argosy-mark-light.png",
          "check": {"url": "https://cdn.jsdelivr.net/gh/Einlanzerous/placard@main/argosy/argosy-mark-light.png",
                    "ok": true, "http_status": 200, "content_type": "image/png",
                    "content_length": 19777, "error": null, "checked_at": "2026-08-26T05:53:52.816313Z"}
        },
        {
          "name": "argosy-mark-dark.png",
          "path": "argosy/argosy-mark-dark.png",
          "role": "for dark surfaces",
          "state": "in_repo",
          "canonical_url": "https://cdn.jsdelivr.net/gh/Einlanzerous/placard@main/argosy/argosy-mark-dark.png",
          "mirror_url": "/argosy/argosy-mark-dark.png",
          "check": {"url": "https://cdn.jsdelivr.net/gh/Einlanzerous/placard@main/argosy/argosy-mark-dark.png",
                    "ok": true, "http_status": 200, "content_type": "image/png",
                    "content_length": 21901, "error": null, "checked_at": "2026-08-26T05:53:52.816667Z"}
        }
      ]
    },
    {
      "slug": "wiki",
      "name": "Wiki",
      "note": "No logo_url set on the Access application; renders as two grey initials.",
      "legacy_source": "none",
      "status": "unset",
      "files": [
        {
          "name": "wiki-mark-light.png",
          "path": "wiki/wiki-mark-light.png",
          "role": "for light surfaces",
          "state": "missing",
          "canonical_url": "https://cdn.jsdelivr.net/gh/Einlanzerous/placard@main/wiki/wiki-mark-light.png",
          "mirror_url": "/wiki/wiki-mark-light.png",
          "check": null
        }
      ]
    }
  ]
}`

func serving(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/services" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMark(t *testing.T) {
	srv := serving(t, http.StatusOK, liveServices)

	cases := []struct {
		name      string
		key       string
		variant   Variant
		wantURL   string
		wantFound bool
	}{
		{
			name:      "a service with a mark",
			key:       "argosy",
			wantURL:   "https://cdn.jsdelivr.net/gh/Einlanzerous/placard@main/argosy/argosy-mark-light.png",
			wantFound: true,
		},
		{
			// Light is the default because Cloudflare's App Launcher is a light
			// surface and stores exactly one URL per application.
			name:      "the variant selects the file",
			key:       "argosy",
			variant:   VariantDark,
			wantURL:   "https://cdn.jsdelivr.net/gh/Einlanzerous/placard@main/argosy/argosy-mark-dark.png",
			wantFound: true,
		},
		{
			// The trap. Placard populates canonical_url for a file it is telling
			// you is not there, so a resolver that reads the URL and not the
			// state writes a guaranteed 404 — the exact state switchyard's tile
			// has been sitting in.
			name:      "a missing file is not a mark, however complete its url looks",
			key:       "wiki",
			wantFound: false,
		},
		{
			// chronicle is a live gated application; Placard has never heard of
			// it. Not an error — a spin-up necessarily runs before a new
			// service's mark is drawn.
			name:      "a service Placard has never heard of",
			key:       "chronicle",
			wantFound: false,
		},
		{
			// The slug is an identity key and hostnames and keys are lowercased
			// upstream of here, but a caller reaching this directly should not
			// get a different answer for a different spelling.
			name:      "the key is matched case-insensitively",
			key:       "ARGOSY",
			wantURL:   "https://cdn.jsdelivr.net/gh/Einlanzerous/placard@main/argosy/argosy-mark-light.png",
			wantFound: true,
		},
		{
			// The variant is published for argosy but this one is not, which is
			// the same answer as an unknown slug rather than an error.
			name:    "a variant that is not published",
			key:     "wiki",
			variant: VariantDark,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(Config{BaseURL: srv.URL, Variant: tc.variant})
			url, found, err := r.Mark(context.Background(), tc.key)
			if err != nil {
				t.Fatalf("Mark: %v", err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v (url %q)", found, tc.wantFound, url)
			}
			if url != tc.wantURL {
				t.Errorf("url = %q, want %q", url, tc.wantURL)
			}
		})
	}
}

// Every way of not getting an answer must be an error, never "no mark".
//
// This is the distinction the whole package turns on. A caller treats found=false
// as "this service has no icon", which on an existing application means "leave it
// alone" but on a fresh one means "the initials are correct" — and reporting that
// for a registry that was merely unreachable is how a working icon gets cleared
// the next time somebody restarts Placard. Purser's standing rule, one layer
// down: never treat unverifiable as absent.
func TestMarkNeverReportsAbsentWhenItSimplyCouldNotAsk(t *testing.T) {
	cases := []struct {
		name string
		r    *Resolver
	}{
		{"not configured", New(Config{})},
		{"http error", New(Config{BaseURL: serving(t, http.StatusInternalServerError, `{}`).URL})},
		{"not found", New(Config{BaseURL: serving(t, http.StatusNotFound, `{}`).URL})},
		{"undecodable body", New(Config{BaseURL: serving(t, http.StatusOK, `<html>a proxy error page</html>`).URL})},
		{"nothing listening", New(Config{BaseURL: "http://127.0.0.1:1"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, found, err := tc.r.Mark(context.Background(), "argosy")
			if err == nil {
				t.Fatalf("want an error, got url=%q found=%v", url, found)
			}
			if found {
				t.Error("found must be false when nothing was learned")
			}
		})
	}
}

// The refusal names the variable to set, the way an unconfigured provisioner
// does one layer up — a generic "not configured" leaves an operator guessing.
func TestUnconfiguredNamesTheVariable(t *testing.T) {
	_, _, err := New(Config{}).Mark(context.Background(), "argosy")
	if err == nil || !strings.Contains(err.Error(), "PURSER_PLACARD_URL") {
		t.Errorf("want a refusal naming PURSER_PLACARD_URL, got %v", err)
	}
}

// A trailing slash on the base URL must not produce //api/services.
func TestBaseURLTolerateATrailingSlash(t *testing.T) {
	srv := serving(t, http.StatusOK, liveServices)
	r := New(Config{BaseURL: srv.URL + "/"})
	if _, found, err := r.Mark(context.Background(), "argosy"); err != nil || !found {
		t.Errorf("found=%v err=%v", found, err)
	}
}

func TestConfigured(t *testing.T) {
	empty := Config{}
	blank := Config{BaseURL: "  "}
	set := Config{BaseURL: "http://placard:4009"}
	if empty.Configured() || blank.Configured() {
		t.Error("a blank base url is not configured")
	}
	if !set.Configured() {
		t.Error("a base url is configured")
	}
}
