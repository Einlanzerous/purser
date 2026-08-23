package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Einlanzerous/purser/internal/version"
)

// What the delivery reconciler actually parses. Decoded into a raw map rather
// than into healthResponse so this asserts the JSON *wire* shape — reusing the
// struct would make a renamed json tag invisible, which is exactly the break
// that would silently stop observations.
func decodeHealthz(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not JSON: %v (body=%s)", err, body)
	}
	return got
}

func TestHandleHealthReportsBuildIdentity(t *testing.T) {
	origVersion, origCommit := version.Version, version.Commit
	t.Cleanup(func() { version.Version, version.Commit = origVersion, origCommit })

	const sha = "c8fee69cf1c55ef22c7ffa27e73eff3e01b4adf9"
	version.Version, version.Commit = "0.14.0", sha

	// A zero Server is enough: handleHealth deliberately consults nothing.
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		// The reconciler falls back to sniffing a leading "<" when the content
		// type is absent and files a markup body as `unreachable`. This header
		// is what keeps a healthy service off the red list.
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	got := decodeHealthz(t, rec.Body.Bytes())
	if got["version"] != "0.14.0" {
		t.Errorf("version = %v, want 0.14.0", got["version"])
	}
	if got["sha"] != sha {
		t.Errorf("sha = %v, want the full 40-char commit %s", got["sha"], sha)
	}
	// The pre-existing fields keep working — the compose HEALTHCHECK and
	// uptime-kuma both read this endpoint and neither should notice the change.
	if got["status"] != "ok" {
		t.Errorf("status = %v, want ok", got["status"])
	}
	if got["service"] != "purser" {
		t.Errorf("service = %v, want purser", got["service"])
	}
}

// An unstamped build must say so, and `sha` must be JSON null rather than "".
// The reconciler reads a blank version as "reports no version"; an empty STRING
// for either field is a third state nothing expects.
func TestHandleHealthUnstampedBuild(t *testing.T) {
	origVersion, origCommit := version.Version, version.Commit
	t.Cleanup(func() { version.Version, version.Commit = origVersion, origCommit })

	// Exactly what an image built with no --build-arg produces.
	version.Version, version.Commit = "", ""

	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	got := decodeHealthz(t, rec.Body.Bytes())
	if got["version"] != "dev" {
		t.Errorf("version = %v, want dev — a blank ARG must not report an empty version", got["version"])
	}
	if _, present := got["sha"]; !present {
		t.Error("sha key is missing; the contract wants it present and null")
	}
	if got["sha"] != nil {
		t.Errorf("sha = %v, want null", got["sha"])
	}
}

// /healthz must stay unauthenticated. It is registered outside `s.auth`, and
// the reconciler, the compose HEALTHCHECK and uptime-kuma all reach it with no
// credentials — a token requirement here reads as `unreachable` on the matrix
// for a service that is running perfectly well.
func TestHealthzIsUnauthenticated(t *testing.T) {
	s := &Server{apiToken: "a-token-that-is-set"}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d with an API token configured, want 200", rec.Code)
	}
}
