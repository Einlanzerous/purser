package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

// --- fakes -----------------------------------------------------------------

type memStore struct {
	mu   sync.Mutex
	rows map[string]model.ServiceResource
}

func newMemStore() *memStore { return &memStore{rows: map[string]model.ServiceResource{}} }

func (s *memStore) ServiceResourcesForHostname(_ context.Context, hostname string) ([]model.ServiceResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.ServiceResource
	for _, r := range s.rows {
		if r.Hostname == hostname {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *memStore) UpsertServiceResource(_ context.Context, r model.ServiceResource) (model.ServiceResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ID, r.Status = uuid.New(), model.ResourceActive
	s.rows[hostname(r)] = r
	return r, nil
}

func (s *memStore) MarkServiceResourceRemoved(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, r := range s.rows {
		if r.ID == id {
			r.Status = model.ResourceRemoved
			s.rows[k] = r
			return nil
		}
	}
	return fmt.Errorf("memStore: no row %s", id)
}

func hostname(r model.ServiceResource) string { return r.Hostname + "|" + string(r.Kind) }

// stubProv reports a resource that is already there and already correct, so the
// orchestrator has something to answer with that is not an error.
type stubProv struct {
	kind    model.ResourceKind
	ensures int
	// warning, when set, is returned by Ensure on a step that still succeeds.
	warning string
	// absent makes Inspect report nothing there, so Ensure is actually reached.
	absent bool
}

func (p *stubProv) Kind() model.ResourceKind { return p.kind }
func (p *stubProv) DisplayName() string      { return string(p.kind) }
func (p *stubProv) Inspect(context.Context, spinup.Target) (spinup.State, error) {
	if p.absent {
		return spinup.State{}, nil
	}
	return spinup.State{Exists: true, Matches: true, ExternalID: "res-1", Detail: "already correct"}, nil
}
func (p *stubProv) Ensure(context.Context, spinup.Target) (spinup.Resource, error) {
	p.ensures++
	return spinup.Resource{ExternalID: "res-1", Detail: "already correct", Warning: p.warning}, nil
}
func (p *stubProv) Teardown(context.Context, spinup.Target, model.ServiceResource) (spinup.Removal, error) {
	return spinup.Removal{}, nil
}

func spinupServer(t *testing.T) (*httptest.Server, *memStore) {
	t.Helper()
	st := newMemStore()
	svc := spinup.New(st, spinup.NewRegistry(
		&stubProv{kind: model.ResourceDNSRecord},
		&stubProv{kind: model.ResourceAccessApp},
		&stubProv{kind: model.ResourceTunnelRoute},
	), spinup.WithTunnels(spinup.TunnelSet{spinup.TunnelProd: "tunnel-1"}))

	srv := httptest.NewServer(New(nil, svc, nil, "").Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func postSpinup(t *testing.T, srv *httptest.Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Post(srv.URL+"/v1/spinups", "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	return resp.StatusCode, out
}

// --- tests -----------------------------------------------------------------

// The default matters more here than on the CLI, where forgetting a flag is
// visible in what you typed. A caller who omits `apply` gets a plan.
func TestSpinup_OmittingApplyIsAPlan(t *testing.T) {
	srv, st := spinupServer(t)
	code, body := postSpinup(t, srv, map[string]any{
		"service": "argosy", "hostname": "argosy.zerogravity.industries",
		"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark",
	})
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if body["applied"] != false {
		t.Errorf("applied should be false with no apply field, got %v", body["applied"])
	}
	if n := len(st.rows); n != 0 {
		t.Errorf("a plan must write no rows, wrote %d", n)
	}
	findings, ok := body["findings"].([]any)
	if !ok || len(findings) != len(model.KindOrder) {
		t.Fatalf("want a line per kind, got %v", body["findings"])
	}
	// Every kind is reported, including the one a direct spec doesn't call for
	// — so silence about the tunnel can never be read as "the tunnel is fine".
	kinds := map[string]bool{}
	for _, f := range findings {
		kinds[f.(map[string]any)["kind"].(string)] = true
	}
	for _, k := range model.KindOrder {
		if !kinds[string(k)] {
			t.Errorf("no finding for %q", k)
		}
	}
}

func TestSpinup_ApplyRecordsAndReportsChanged(t *testing.T) {
	srv, st := spinupServer(t)
	code, body := postSpinup(t, srv, map[string]any{
		"service": "argosy", "hostname": "argosy.zerogravity.industries",
		"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark",
		"apply": true,
	})
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if body["applied"] != true {
		t.Error("applied should be true")
	}
	// Upstream was already correct and Purser held no rows, so both real steps
	// are adopts: rows written, nothing called.
	if n := len(st.rows); n != 2 {
		t.Errorf("want a row for each of the two kinds this spec calls for, got %d", n)
	}
	if body["changed"].(float64) != 2 {
		t.Errorf("changed = %v, want 2", body["changed"])
	}
	if body["pending"].(float64) != 0 {
		t.Errorf("nothing should be outstanding after an apply, pending = %v", body["pending"])
	}
}

// The spec is validated before the orchestrator runs, so a malformed one is the
// caller's fault and says what was wrong — not a 500 with the text swallowed.
func TestSpinup_BadSpecIs400WithTheReason(t *testing.T) {
	srv, _ := spinupServer(t)
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no mode", map[string]any{
			"service": "argosy", "hostname": "argosy.zerogravity.industries",
			"upstream": "100.64.0.7", "access": "bookmark"}, "mode is required"},
		{"a direct spec naming a tunnel", map[string]any{
			"service": "argosy", "hostname": "argosy.zerogravity.industries",
			"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark",
			"tunnel": "prod"}, "must not name a tunnel"},
		{"a url where a record's value belongs", map[string]any{
			"service": "argosy", "hostname": "argosy.zerogravity.industries",
			"mode": "direct", "upstream": "https://100.64.0.7:8096", "access": "bookmark"},
			"ip address or a hostname"},
		{"an http logo the launcher would block", map[string]any{
			"service": "argosy", "hostname": "argosy.zerogravity.industries",
			"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark",
			"logo": "http://example.com/a.png"}, "https://"},
		{
			// PRSR-37 renamed logo_url to logo and changed what it holds.
			// encoding/json drops unknown fields, so without this a caller
			// written against the previous release loses its explicit icon
			// silently and the spec defaults to "placard" — nothing is
			// destroyed, but the instruction vanishes with no error and nothing
			// in the response saying so.
			"the field renamed in PRSR-37 is refused rather than ignored",
			map[string]any{
				"service": "argosy", "hostname": "argosy.zerogravity.industries",
				"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark",
				"logo_url": "https://cdn.example/argosy.png"},
			`"logo_url" was renamed`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := postSpinup(t, srv, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %v", code, body)
			}
			msg, _ := body["error"].(string)
			if msg == "" || !contains(msg, tc.want) {
				t.Errorf("error %q should mention %q", msg, tc.want)
			}
		})
	}
}

// A legal ref this deployment has no id for is the caller's to fix — they can
// ask for prod — so it is a 400 naming the ref, not a 500 saying "spin-up
// failed". It is the one refusal Ensure can still raise after Validate has
// passed, which is why it carries a sentinel rather than being matched by text.
func TestSpinup_AnUnwiredTunnelRefIs400(t *testing.T) {
	srv, _ := spinupServer(t)
	code, body := postSpinup(t, srv, map[string]any{
		"service": "interlock", "hostname": "interlock.zerogravity.industries",
		"mode": "tunnelled", "upstream": "http://interlock:8080", "access": "gated",
		"tunnel": "dev", "apply": true,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %v", code, body)
	}
	if msg, _ := body["error"].(string); !contains(msg, "dev") {
		t.Errorf("the refusal should name the ref, got %q", msg)
	}
}

// The response echoes the *normalized* spec, not the request: a caller
// comparing what it sent against what was recorded needs the form the run
// actually used.
func TestSpinup_EchoesTheNormalizedSpec(t *testing.T) {
	srv, _ := spinupServer(t)
	_, body := postSpinup(t, srv, map[string]any{
		"service": "Argosy", "hostname": "ARGOSY.ZeroGravity.Industries.",
		"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark",
	})
	spec, ok := body["spec"].(map[string]any)
	if !ok {
		t.Fatalf("no spec echoed: %v", body)
	}
	if spec["hostname"] != "argosy.zerogravity.industries" {
		t.Errorf("hostname should be folded and the trailing dot trimmed, got %v", spec["hostname"])
	}
	if spec["service"] != "argosy" {
		t.Errorf("service key should be folded, got %v", spec["service"])
	}
	// DisplayName defaults to the key, and it is what the Access application is
	// named — so a caller that sent none should be able to see what was used.
	if spec["display_name"] != "argosy" {
		t.Errorf("display_name should default to the key, got %v", spec["display_name"])
	}
}

// A build with no orchestrator answers 503 rather than panicking on the first
// request.
func TestSpinup_NoOrchestratorIs503(t *testing.T) {
	srv := httptest.NewServer(New(nil, nil, nil, "").Handler())
	defer srv.Close()
	code, body := postSpinup(t, srv, map[string]any{"service": "argosy"})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %v", code, body)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || bytes.Contains([]byte(s), []byte(sub))
}

// The finding that a step succeeded while something *else* may have broken has
// to survive the HTTP path, and it is the one a 200 with sensible counts hides
// completely: the tunnel's concurrent-write note means another service's ingress
// route may have been dropped from the shared document. It reaches the caller as
// its own field — not as a clause inside `detail`, which a caller would have to
// pattern-match — and it is logged server-side, so a caller that ignores the
// field still leaves a trace somewhere (PRSR-31).
func TestSpinup_AWarningSurvivesTheHTTPPath(t *testing.T) {
	const note = "another writer changed the shared configuration at the same time"
	st := newMemStore()
	dns := &stubProv{kind: model.ResourceDNSRecord, absent: true, warning: note}
	svc := spinup.New(st, spinup.NewRegistry(
		dns,
		&stubProv{kind: model.ResourceAccessApp},
		&stubProv{kind: model.ResourceTunnelRoute},
	), spinup.WithTunnels(spinup.TunnelSet{spinup.TunnelProd: "tunnel-1"}))

	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	srv := httptest.NewServer(New(nil, svc, nil, "").Handler())
	defer srv.Close()

	code, body := postSpinup(t, srv, map[string]any{
		"service": "argosy", "hostname": "argosy.zerogravity.industries",
		"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark",
		"apply": true,
	})
	if code != http.StatusOK {
		t.Fatalf("a warning is not a failure, status %d: %v", code, body)
	}
	var found map[string]any
	for _, raw := range body["findings"].([]any) {
		if f := raw.(map[string]any); f["kind"] == string(model.ResourceDNSRecord) {
			found = f
		}
	}
	if found == nil {
		t.Fatalf("no DNS finding: %v", body["findings"])
	}
	if found["warning"] != note {
		t.Errorf("the warning must be its own field, got %v", found["warning"])
	}
	if found["applied"] != true {
		t.Errorf("the step succeeded, so it is applied: %v", found["applied"])
	}
	if d, _ := found["detail"].(string); contains(d, "another writer") {
		t.Errorf("the note must not also be folded into detail: %q", d)
	}
	if !contains(logged.String(), note) {
		t.Errorf("a caller that ignores the field would leave no trace anywhere; server log was %q", logged.String())
	}
}

// ...and a step with nothing to warn about carries no field at all, so the
// presence of `warning` is itself the signal.
func TestSpinup_NoWarningMeansNoField(t *testing.T) {
	srv, _ := spinupServer(t)
	_, body := postSpinup(t, srv, map[string]any{
		"service": "argosy", "hostname": "argosy.zerogravity.industries",
		"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark",
		"apply": true,
	})
	for _, raw := range body["findings"].([]any) {
		if _, present := raw.(map[string]any)["warning"]; present {
			t.Errorf("warning should be omitted when empty: %v", raw)
		}
	}
}

// The CLI trims these itself; Normalized now does it for both surfaces, so an
// automation caller no longer gets `unknown mode "direct "` for a value the CLI
// accepts.
func TestSpinup_PaddedEnumsAreAccepted(t *testing.T) {
	srv, _ := spinupServer(t)
	code, body := postSpinup(t, srv, map[string]any{
		"service": "argosy", "hostname": "argosy.zerogravity.industries",
		"mode": "direct ", "upstream": "100.64.0.7", "access": " bookmark",
	})
	if code != http.StatusOK {
		t.Fatalf("padding is not a different spec, status %d: %v", code, body)
	}
	spec := body["spec"].(map[string]any)
	if spec["mode"] != "direct" || spec["access"] != "bookmark" {
		t.Errorf("the echoed spec should be trimmed, got %v", spec)
	}
}

// `pending` and `changed` are the two fields that look like a verdict and are
// not one. An apply against a deployment with no Cloudflare credentials answers
// 200 with `pending: 0, changed: 0` — byte-identical to an edge that is already
// correct — which is the misreading that shipped on the CLI and got fixed there
// (PRSR-31). `needs_attention` is the field that tells them apart, computed from
// the same list the CLI's exit code uses.
func TestSpinup_NeedsAttentionSeparatesUnconfiguredFromAlreadyCorrect(t *testing.T) {
	unconfigured := spinup.New(newMemStore(), spinup.NewRegistry(
		spinup.NewUnavailable(model.ResourceDNSRecord, "DNS record", "set PURSER_CF_ZONE_ID"),
		&stubProv{kind: model.ResourceAccessApp},
		&stubProv{kind: model.ResourceTunnelRoute},
	))
	srv := httptest.NewServer(New(nil, unconfigured, nil, "").Handler())
	defer srv.Close()

	body := map[string]any{
		"service": "argosy", "hostname": "argosy.zerogravity.industries",
		"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark",
		"apply": true,
	}
	code, broken := postSpinup(t, srv, body)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, broken)
	}
	// The trap, stated as an assertion: these two say nothing is wrong.
	if broken["pending"].(float64) != 0 {
		t.Fatalf("precondition: an unavailable step is not pending, got %v", broken["pending"])
	}
	att, _ := broken["needs_attention"].([]any)
	if len(att) != 1 || att[0] != string(model.ResourceDNSRecord) {
		t.Errorf("want the DNS step named as needing attention, got %v", broken["needs_attention"])
	}

	// ...and an edge that really is correct carries no such field at all, so its
	// presence is the whole signal.
	good, _ := spinupServer(t)
	_, fine := postSpinup(t, good, body)
	if _, present := fine["needs_attention"]; present {
		t.Errorf("an edge that matches the spec needs no attention, got %v", fine["needs_attention"])
	}
}

// --- prune (PRSR-46) --------------------------------------------------------

// `prune` and `apply` are both needed to remove anything, and both default to
// false — so a caller written against an earlier release gets exactly the
// behaviour it was written for.
func TestSpinup_PruneDefaultsOffAndNeedsApply(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"neither":    {},
		"apply only": {"apply": true},
		"prune only": {"prune": true},
	} {
		t.Run(name, func(t *testing.T) {
			st := newMemStore()
			prov := &stubProv{kind: model.ResourceTunnelRoute}
			svc := spinup.New(st, spinup.NewRegistry(
				&stubProv{kind: model.ResourceDNSRecord},
				&stubProv{kind: model.ResourceAccessApp},
				prov,
			), spinup.WithTunnels(spinup.TunnelSet{spinup.TunnelProd: "tunnel-1"}))
			srv := httptest.NewServer(New(nil, svc, nil, "").Handler())
			defer srv.Close()

			// A row for a kind this direct spec does not call for.
			_, _ = st.UpsertServiceResource(context.Background(), model.ServiceResource{
				ServiceKey: "argosy", Hostname: "argosy.zerogravity.industries",
				Kind: model.ResourceTunnelRoute, ExternalID: "", ParentID: "tunnel-1",
			})

			req := map[string]any{
				"service": "argosy", "hostname": "argosy.zerogravity.industries",
				"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark",
			}
			for k, v := range body {
				req[k] = v
			}
			buf, _ := json.Marshal(req)
			resp, err := srv.Client().Post(srv.URL+"/v1/spinups", "application/json", bytes.NewReader(buf))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			var out map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&out)

			for _, raw := range out["findings"].([]any) {
				f := raw.(map[string]any)
				if f["kind"] != string(model.ResourceTunnelRoute) {
					continue
				}
				wantStatus := "orphaned"
				if body["prune"] == true {
					wantStatus = "prune"
				}
				if f["status"] != wantStatus {
					t.Errorf("tunnel_route status = %v, want %v", f["status"], wantStatus)
				}
				if f["applied"] == true {
					t.Error("something was removed without both flags")
				}
			}
			if st.rows[hostname(model.ServiceResource{Hostname: "argosy.zerogravity.industries", Kind: model.ResourceTunnelRoute})].Status != model.ResourceActive {
				t.Error("the row was marked removed without both flags")
			}
		})
	}
}

// `pruned` echoes the request, so a caller can tell an `orphaned` line that was
// never going to be acted on from one on a run that asked.
func TestSpinup_PrunedIsEchoed(t *testing.T) {
	srv, _ := spinupServer(t)
	_, body := postSpinup(t, srv, map[string]any{
		"service": "argosy", "hostname": "argosy.zerogravity.industries",
		"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark", "prune": true,
	})
	if body["pruned"] != true {
		t.Errorf("pruned = %v, want true", body["pruned"])
	}
}

// A prune of a hostname holding another service's resource is a 409, matching
// POST /v1/teardowns' reading of the same sentinel: the request is well-formed
// and it is the state that refuses it (PRSR-46).
func TestSpinup_PruningAnotherServicesHostnameIs409(t *testing.T) {
	st := newMemStore()
	prov := &stubProv{kind: model.ResourceTunnelRoute}
	svc := spinup.New(st, spinup.NewRegistry(
		&stubProv{kind: model.ResourceDNSRecord},
		&stubProv{kind: model.ResourceAccessApp},
		prov,
	), spinup.WithTunnels(spinup.TunnelSet{spinup.TunnelProd: "tunnel-1"}))
	srv := httptest.NewServer(New(nil, svc, nil, "").Handler())
	defer srv.Close()

	_, _ = st.UpsertServiceResource(context.Background(), model.ServiceResource{
		ServiceKey: "argosy", Hostname: "argosy.zerogravity.industries",
		Kind: model.ResourceTunnelRoute, ParentID: "tunnel-1",
	})

	body := map[string]any{
		"service": "chronicle", "hostname": "argosy.zerogravity.industries",
		"mode": "direct", "upstream": "100.64.0.7", "access": "bookmark",
		"apply": true, "prune": true,
	}
	buf, _ := json.Marshal(body)
	resp, err := srv.Client().Post(srv.URL+"/v1/spinups", "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if msg, _ := out["error"].(string); !strings.Contains(msg, "argosy") {
		t.Errorf("the 409 must name the owner, got %q", msg)
	}
	if prov.ensures != 0 {
		t.Error("the refusal acted")
	}

	// Without prune the same request is an ordinary 200 — nothing was going to
	// be removed, so there is nothing to refuse.
	delete(body, "prune")
	buf, _ = json.Marshal(body)
	resp2, err := srv.Client().Post(srv.URL+"/v1/spinups", "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("status %d without prune, want 200", resp2.StatusCode)
	}
}
