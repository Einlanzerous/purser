package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

// POST /v1/teardowns (PRSR-34). The spin-up axis's two entry points shipped
// together in PRSR-31 and its two destructive ones do here, so `purser serve`
// cannot stand an edge up and then be unable to take it down.

// teardownProv records what it was asked to remove.
type teardownProv struct {
	stubProv
	teardowns int
	warning   string
	err       error
}

func (p *teardownProv) Teardown(context.Context, spinup.Target, model.ServiceResource) (spinup.Removal, error) {
	p.teardowns++
	if p.err != nil {
		return spinup.Removal{}, p.err
	}
	return spinup.Removal{Detail: "removed", Warning: p.warning}, nil
}

// seedRows puts an active row for every kind at the hostname, as a spin-up
// would have left them.
func seedRows(st *memStore, service, host string) {
	for _, k := range model.KindOrder {
		_, _ = st.UpsertServiceResource(context.Background(), model.ServiceResource{
			ServiceKey: service, Hostname: host, Kind: k, ExternalID: "id-" + string(k),
		})
	}
}

func teardownServer(t *testing.T, provs ...spinup.ServiceProvisioner) (*httptest.Server, *memStore) {
	t.Helper()
	st := newMemStore()
	if len(provs) == 0 {
		provs = []spinup.ServiceProvisioner{
			&teardownProv{stubProv: stubProv{kind: model.ResourceDNSRecord}},
			&teardownProv{stubProv: stubProv{kind: model.ResourceAccessApp}},
			&teardownProv{stubProv: stubProv{kind: model.ResourceTunnelRoute}},
		}
	}
	svc := spinup.New(st, spinup.NewRegistry(provs...))
	srv := httptest.NewServer(New(nil, svc, nil, "").Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func postTeardown(t *testing.T, srv *httptest.Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Post(srv.URL+"/v1/teardowns", "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, out
}

// `apply` defaults to false, so the unadorned request is a plan. It matters more
// here than on POST /v1/spinups: every step of this one is a deletion, and a
// caller who forgets the field gets a report rather than a hostname that stops
// resolving.
func TestTeardown_OmittingApplyIsAPlan(t *testing.T) {
	dns := &teardownProv{stubProv: stubProv{kind: model.ResourceDNSRecord}}
	srv, st := teardownServer(t, dns,
		&teardownProv{stubProv: stubProv{kind: model.ResourceAccessApp}},
		&teardownProv{stubProv: stubProv{kind: model.ResourceTunnelRoute}})
	seedRows(st, "interlock", "interlock.zerogravity.industries")

	code, body := postTeardown(t, srv, map[string]any{
		"service": "interlock", "hostname": "interlock.zerogravity.industries",
	})
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if body["applied"] != false {
		t.Errorf("applied=%v, want false", body["applied"])
	}
	if got := body["pending"].(float64); got != 3 {
		t.Errorf("pending=%v, want 3", got)
	}
	if dns.teardowns != 0 {
		t.Errorf("the plan called a provisioner %d times; a teardown preview contacts nothing", dns.teardowns)
	}
	for _, r := range st.rows {
		if r.Status != model.ResourceActive {
			t.Errorf("a plan marked a row %q", r.Status)
		}
	}
}

func TestTeardown_ApplyRemovesAndReportsChanged(t *testing.T) {
	srv, st := teardownServer(t)
	seedRows(st, "interlock", "interlock.zerogravity.industries")

	code, body := postTeardown(t, srv, map[string]any{
		"service": "interlock", "hostname": "interlock.zerogravity.industries", "apply": true,
	})
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if got := body["changed"].(float64); got != 3 {
		t.Errorf("changed=%v, want 3", got)
	}
	if _, ok := body["needs_attention"]; ok {
		t.Errorf("needs_attention should be absent on a clean teardown: %v", body["needs_attention"])
	}
	for _, r := range st.rows {
		if r.Status != model.ResourceRemoved {
			t.Errorf("%s row is %q after a successful teardown", r.Kind, r.Status)
		}
	}
}

// The ownership refusal is a 409, not a 400: the request is well-formed and it
// is the *state* that refuses it — the reading ErrNameConflictOnEmail gets on
// the invite path — and the fix is to look at who owns the hostname rather than
// to correct the request.
func TestTeardown_AHostnameRecordedToAnotherServiceIs409(t *testing.T) {
	dns := &teardownProv{stubProv: stubProv{kind: model.ResourceDNSRecord}}
	srv, st := teardownServer(t, dns,
		&teardownProv{stubProv: stubProv{kind: model.ResourceAccessApp}},
		&teardownProv{stubProv: stubProv{kind: model.ResourceTunnelRoute}})
	seedRows(st, "chronicle", "interlock.zerogravity.industries")

	code, body := postTeardown(t, srv, map[string]any{
		"service": "interlock", "hostname": "interlock.zerogravity.industries", "apply": true,
	})
	if code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %v", code, body)
	}
	if dns.teardowns != 0 {
		t.Error("something was removed despite the refusal")
	}
	for _, r := range st.rows {
		if r.Status != model.ResourceActive {
			t.Errorf("a row was marked removed despite the refusal")
		}
	}
}

// Both identifiers are required, and a malformed one is the caller's fault
// rather than an outage — which is what validating in the handler decides.
func TestTeardown_AMalformedRequestIs400(t *testing.T) {
	srv, _ := teardownServer(t)
	for name, body := range map[string]map[string]any{
		"no service":  {"hostname": "interlock.zerogravity.industries"},
		"no hostname": {"service": "interlock"},
		"a path":      {"service": "interlock", "hostname": "interlock.zerogravity.industries/admin"},
		"not fqdn":    {"service": "interlock", "hostname": "interlock"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out := postTeardown(t, srv, body)
			if code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %v", code, out)
			}
			if out["error"] == "" {
				t.Error("a 400 must say what was wrong with the request")
			}
		})
	}
}

// The warning is its own field, and survives the HTTP path — the whole reason
// Teardown returns a Removal rather than an error. A caller must be able to find
// "another service may have lost its route" without pattern-matching a detail
// string.
func TestTeardown_AWarningSurvivesTheHTTPPath(t *testing.T) {
	srv, st := teardownServer(t,
		&teardownProv{stubProv: stubProv{kind: model.ResourceDNSRecord}},
		&teardownProv{stubProv: stubProv{kind: model.ResourceAccessApp}},
		&teardownProv{stubProv: stubProv{kind: model.ResourceTunnelRoute},
			warning: "another writer changed the shared configuration"})
	seedRows(st, "interlock", "interlock.zerogravity.industries")

	_, body := postTeardown(t, srv, map[string]any{
		"service": "interlock", "hostname": "interlock.zerogravity.industries", "apply": true,
	})
	findings, _ := body["findings"].([]any)
	var found bool
	for _, raw := range findings {
		f := raw.(map[string]any)
		if f["kind"] != string(model.ResourceTunnelRoute) {
			if _, ok := f["warning"]; ok {
				t.Errorf("%v carries a warning it was not given", f["kind"])
			}
			continue
		}
		found = true
		if f["warning"] != "another writer changed the shared configuration" {
			t.Errorf("warning = %v", f["warning"])
		}
		// A warning is not a failure: the removal happened.
		if f["applied"] != true {
			t.Errorf("the removal reports applied=%v", f["applied"])
		}
	}
	if !found {
		t.Fatal("no finding for the tunnel route")
	}
}

// pending and changed are the two fields that look like a verdict and are not
// one: an unconfigured deployment answers 200 with both at zero, which is
// byte-identical to a hostname that is already clear.
func TestTeardown_NeedsAttentionSeparatesUnconfiguredFromAlreadyClear(t *testing.T) {
	st := newMemStore()
	seedRows(st, "interlock", "interlock.zerogravity.industries")
	svc := spinup.New(st, spinup.NewRegistry(
		spinup.NewUnavailable(model.ResourceDNSRecord, "DNS record", "set PURSER_CF_ZONE_ID"),
		spinup.NewUnavailable(model.ResourceAccessApp, "Access application", "set PURSER_CF_API_TOKEN"),
		spinup.NewUnavailable(model.ResourceTunnelRoute, "ingress route", "set PURSER_CF_API_TOKEN"),
	))
	srv := httptest.NewServer(New(nil, svc, nil, "").Handler())
	defer srv.Close()

	code, body := postTeardown(t, srv, map[string]any{
		"service": "interlock", "hostname": "interlock.zerogravity.industries", "apply": true,
	})
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if body["pending"].(float64) != 0 || body["changed"].(float64) != 0 {
		t.Fatal("the two counts must be indistinguishable from a clean run — that is the point")
	}
	needs, _ := body["needs_attention"].([]any)
	if len(needs) != 3 {
		t.Errorf("needs_attention = %v, want all three kinds", needs)
	}
}

func TestTeardown_NoOrchestratorIs503(t *testing.T) {
	srv := httptest.NewServer(New(nil, nil, nil, "").Handler())
	defer srv.Close()
	code, _ := postTeardown(t, srv, map[string]any{"service": "argosy", "hostname": "argosy.example.com"})
	if code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", code)
	}
}
