package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

// The provisioner is only useful if the orchestrator will take it.
var _ spinup.ServiceProvisioner = (*TunnelProvisioner)(nil)

// --- a stand-in for the tunnel configuration endpoint ----------------------

// fakeTunnel holds one ingress document, serves it on GET and replaces it on
// PUT, bumping the version the way Cloudflare does.
//
// It keeps *state* rather than recording request bodies because the assertions
// worth making here are about what the tunnel is left serving — that the
// catch-all is still last, that the other services are still routed — and a
// request body only shows what one call intended.
type fakeTunnel struct {
	srv *httptest.Server

	mu     sync.Mutex
	doc    map[string]json.RawMessage
	ver    int
	gets   int
	puts   int
	paths  []string
	getErr string // when set, GET fails with this Cloudflare error message

	// getDelay makes the read window wide enough for two Ensure calls to
	// overlap in it — which is the only way to show the mutex is doing
	// something.
	getDelay time.Duration
	// verBump is how much a PUT moves the version, so a concurrent writer can be
	// simulated without a second client.
	verBump int
	// afterPut runs with the lock held, immediately after a write is applied —
	// the window another writer would land in.
	afterPut func()
}

// The tunnel id these tests use. Declared in dns_test.go, which needs the same
// value to build the `<id>.cfargotunnel.com` CNAME target — one id shared by
// both spin-up provisioners is also what the real thing looks like.

func newFakeTunnel(t *testing.T, config string) *fakeTunnel {
	t.Helper()
	f := &fakeTunnel{ver: 5, verBump: 1}
	if err := json.Unmarshal([]byte(config), &f.doc); err != nil {
		t.Fatalf("fixture is not a config object: %v", err)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTunnel) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	if got := r.Header.Get("Authorization"); got != "Bearer cf_token" {
		f.mu.Unlock()
		http.Error(w, "bad auth", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		f.gets++
		if f.getErr != "" {
			msg := f.getErr
			f.mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"success":false,"errors":[{"code":1000,"message":%q}]}`, msg)
			return
		}
		body, _ := json.Marshal(map[string]any{
			"success": true,
			"result": map[string]any{
				"tunnel_id": testTunnelID,
				"version":   f.ver,
				"source":    "cloudflare",
				"config":    f.doc,
			},
		})
		delay := f.getDelay
		// Unlocked *before* the delay on purpose: two GETs have to be able to
		// overlap, or the fake would serialize what the provisioner is supposed
		// to serialize itself.
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		_, _ = w.Write(body)

	case http.MethodPut:
		f.puts++
		f.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Config map[string]json.RawMessage `json:"config"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.doc = body.Config
		f.ver += f.verBump
		if f.afterPut != nil {
			f.afterPut()
		}
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"success":true,"result":{}}`))

	default:
		f.mu.Unlock()
		http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
	}
}

// ingress decodes what the tunnel is currently serving.
func (f *fakeTunnel) ingress(t *testing.T) []ingressRule {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var rules []ingressRule
	if raw, ok := f.doc["ingress"]; ok {
		if err := json.Unmarshal(raw, &rules); err != nil {
			t.Fatalf("stored ingress is not a list: %v", err)
		}
	}
	return rules
}

// hostnames renders the stored ingress as "host=service" lines, with the
// catch-all's absent hostname shown as "*" — the shape most assertions here are
// really about is the order.
func (f *fakeTunnel) hostnames(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, r := range f.ingress(t) {
		host := r.str("hostname")
		if host == "" {
			host = "*"
		}
		out = append(out, host+"="+r.str("service"))
	}
	return out
}

func (f *fakeTunnel) counts() (gets, puts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.puts
}

func (f *fakeTunnel) prov(t *testing.T) *TunnelProvisioner {
	t.Helper()
	return newTunnelWithBase(t, f.srv.URL, TunnelConfig{APIToken: "cf_token", AccountID: "acct"})
}

// target builds a tunnelled spec's Target, already validated the way the
// orchestrator hands it over.
func target(t *testing.T, hostname, upstream string) spinup.Target {
	t.Helper()
	spec, err := spinup.ServiceSpec{
		Key:      "interlock",
		Hostname: hostname,
		Mode:     spinup.ModeTunnelled,
		Upstream: upstream,
		Access:   spinup.AccessGated,
		Tunnel:   spinup.TunnelProd,
	}.Validate()
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	return spinup.Target{Spec: spec, TunnelID: testTunnelID}
}

// liveShape is the document as observed on the real tunnel: several hostnames,
// per-rule settings this build does not model, and a terminal rule carrying no
// hostname at all.
const liveShape = `{
  "ingress": [
    {"hostname":"switchyard.zerogravity.industries","service":"http://switchyard:4001","originRequest":{"noTLSVerify":true}},
    {"hostname":"lyceum.zerogravity.industries","service":"http://lyceum:8083","originRequest":{}},
    {"hostname":"wiki.zerogravity.industries","service":"http://wiki:3000"},
    {"service":"http_status:404"}
  ],
  "warp-routing": {"enabled": false},
  "originRequest": {"connectTimeout": "30s"}
}`

// --- unconfigured ----------------------------------------------------------

func TestTunnel_Unconfigured_IsUnavailableOnEveryPath(t *testing.T) {
	p := NewTunnel(TunnelConfig{}) // no token, no account
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	if _, err := p.Inspect(context.Background(), tgt); !spinup.IsUnavailable(err) {
		t.Errorf("Inspect: want ErrUnavailable, got %v", err)
	}
	if _, err := p.Ensure(context.Background(), tgt); !spinup.IsUnavailable(err) {
		t.Errorf("Ensure: want ErrUnavailable, got %v", err)
	}
	if err := p.Teardown(context.Background(), tgt, model.ServiceResource{}); !spinup.IsUnavailable(err) {
		t.Errorf("Teardown: want ErrUnavailable, got %v", err)
	}
}

func TestTunnel_NoResolvedTunnelIsNotUnavailable(t *testing.T) {
	// A missing tunnel id is a wiring mistake, not a deployment that hasn't been
	// configured — and the two get opposite advice, so they must not share a
	// sentinel.
	p := NewTunnel(TunnelConfig{APIToken: "cf_token", AccountID: "acct"})
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")
	tgt.TunnelID = ""

	_, err := p.Inspect(context.Background(), tgt)
	if err == nil || spinup.IsUnavailable(err) {
		t.Fatalf("want a plain error, got %v", err)
	}
}

// --- Inspect ---------------------------------------------------------------

func TestTunnel_InspectFindsTheRouteAndNeverWrites(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "lyceum.zerogravity.industries", "http://lyceum:8083")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !st.Exists || !st.Matches {
		t.Errorf("want exists+matches, got %+v", st)
	}
	if st.ParentID != testTunnelID {
		t.Errorf("ParentID should be the tunnel, got %q", st.ParentID)
	}
	if st.ExternalID != "" {
		t.Errorf("a tunnel route has no id of its own, got %q", st.ExternalID)
	}
	if _, puts := f.counts(); puts != 0 {
		t.Errorf("Inspect must not write, saw %d PUTs", puts)
	}
	if !strings.Contains(st.Detail, "http://lyceum:8083") {
		t.Errorf("Detail should say what it routes to, got %q", st.Detail)
	}
}

func TestTunnel_InspectReportsAMismatchedUpstream(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "lyceum.zerogravity.industries", "http://lyceum:9999")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !st.Exists || st.Matches {
		t.Fatalf("want exists but not matching, got %+v", st)
	}
	// Both halves, because an operator reading an `update` line needs to know
	// what is being replaced as well as with what.
	if !strings.Contains(st.Detail, "http://lyceum:8083") || !strings.Contains(st.Detail, "http://lyceum:9999") {
		t.Errorf("Detail should name what is there and what is wanted, got %q", st.Detail)
	}
}

func TestTunnel_InspectAbsentIsNotAnError(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if st.Exists {
		t.Errorf("want absent, got %+v", st)
	}
	if st.ParentID != testTunnelID {
		t.Errorf("ParentID should be the tunnel even when absent, got %q", st.ParentID)
	}
}

func TestTunnel_InspectFailureIsAnErrorNotAnAbsence(t *testing.T) {
	// The orchestrator turns an Inspect error into `unknown` and refuses to act.
	// Returning a zero State with a nil error instead would read as "the route
	// is missing" and have --apply add one that is already there.
	f := newFakeTunnel(t, liveShape)
	f.getErr = "invalid tunnel token"
	tgt := target(t, "lyceum.zerogravity.industries", "http://lyceum:8083")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err == nil {
		t.Fatalf("want an error, got state %+v", st)
	}
	if !strings.Contains(err.Error(), "invalid tunnel token") {
		t.Errorf("the API's reason should survive, got %v", err)
	}
	if spinup.IsUnavailable(err) {
		t.Error("a failed read is unknown, not unavailable")
	}
}

func TestTunnel_InspectToleratesRulesWithNoHostname(t *testing.T) {
	// The terminal rule was observed as a trailing null in the hostname list.
	// Anything walking this document has to step over that rather than trip on
	// it.
	f := newFakeTunnel(t, `{"ingress":[
		{"hostname":null,"service":"http_status:404"}
	]}`)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if st.Exists {
		t.Errorf("a hostname-less rule is not anybody's route, got %+v", st)
	}
}

func TestTunnel_InspectReportsDuplicatesWithoutTouchingThem(t *testing.T) {
	f := newFakeTunnel(t, `{"ingress":[
		{"hostname":"lyceum.zerogravity.industries","service":"http://lyceum:8083"},
		{"hostname":"lyceum.zerogravity.industries","service":"http://elsewhere:1"},
		{"service":"http_status:404"}
	]}`)
	tgt := target(t, "lyceum.zerogravity.industries", "http://lyceum:8083")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !st.Matches {
		t.Errorf("cloudflared matches the first rule, so this one matches: %+v", st)
	}
	if !strings.Contains(st.Detail, "further rule") {
		t.Errorf("the duplicate should be reported, got %q", st.Detail)
	}
}

// --- Ensure: the insert position -------------------------------------------

func TestTunnel_EnsureInsertsBeforeTheTerminalCatchAll(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	res, err := f.prov(t).Ensure(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.ParentID != testTunnelID {
		t.Errorf("ParentID should be the tunnel, got %q", res.ParentID)
	}

	got := f.hostnames(t)
	want := []string{
		"switchyard.zerogravity.industries=http://switchyard:4001",
		"lyceum.zerogravity.industries=http://lyceum:8083",
		"wiki.zerogravity.industries=http://wiki:3000",
		"interlock.zerogravity.industries=http://interlock:8080",
		"*=http_status:404",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("ingress order wrong.\n got: %v\nwant: %v", got, want)
	}
}

func TestTunnel_EnsureKeepsEveryFieldItDoesNotUnderstand(t *testing.T) {
	// A PUT replaces the whole document, so anything dropped here is a setting
	// somebody made by hand and will never be told was lost.
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	if _, err := f.prov(t).Ensure(context.Background(), tgt); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	f.mu.Lock()
	warp := string(f.doc["warp-routing"])
	origin := string(f.doc["originRequest"])
	f.mu.Unlock()
	if warp != `{"enabled":false}` {
		t.Errorf("warp-routing should survive verbatim, got %q", warp)
	}
	if origin != `{"connectTimeout":"30s"}` {
		t.Errorf("the tunnel-wide originRequest should survive verbatim, got %q", origin)
	}
	if got := string(f.ingress(t)[0]["originRequest"]); got != `{"noTLSVerify":true}` {
		t.Errorf("a per-rule originRequest should survive verbatim, got %q", got)
	}
}

func TestTunnel_EnsureIsIdempotentAndWritesNothing(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "lyceum.zerogravity.industries", "http://lyceum:8083")

	res, err := f.prov(t).Ensure(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, puts := f.counts(); puts != 0 {
		t.Errorf("an already-correct route needs no write, saw %d PUTs", puts)
	}
	if !strings.Contains(res.Detail, "already routed") {
		t.Errorf("Detail should say it was already there, got %q", res.Detail)
	}
}

func TestTunnel_EnsureRepointsInPlace(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "switchyard.zerogravity.industries", "http://switchyard:4002")

	res, err := f.prov(t).Ensure(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	rules := f.ingress(t)
	if len(rules) != 4 {
		t.Fatalf("an update should not add a rule, got %d", len(rules))
	}
	// Position is matching precedence, so an update stays where it was.
	if rules[0].str("hostname") != "switchyard.zerogravity.industries" || rules[0].str("service") != "http://switchyard:4002" {
		t.Errorf("rule 1 should be the repointed one, got %v", f.hostnames(t))
	}
	if got := string(rules[0]["originRequest"]); got != `{"noTLSVerify":true}` {
		t.Errorf("repointing must keep the rest of the rule, got %q", got)
	}
	if !strings.Contains(res.Detail, "http://switchyard:4001") {
		t.Errorf("Detail should name what it was repointed from, got %q", res.Detail)
	}
}

func TestTunnel_EnsureSuppliesATerminalRuleForAnEmptyTunnel(t *testing.T) {
	f := newFakeTunnel(t, `{"ingress":[]}`)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	res, err := f.prov(t).Ensure(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := []string{"interlock.zerogravity.industries=http://interlock:8080", "*=http_status:404"}
	if got := f.hostnames(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got %v, want %v", got, want)
	}
	if !strings.Contains(res.Detail, catchAllService) {
		t.Errorf("supplying the terminal rule should be said out loud, got %q", res.Detail)
	}
}

func TestTunnel_EnsureRefusesADocumentWithNoTerminalCatchAll(t *testing.T) {
	f := newFakeTunnel(t, `{"ingress":[
		{"hostname":"lyceum.zerogravity.industries","service":"http://lyceum:8083"}
	]}`)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	_, err := f.prov(t).Ensure(context.Background(), tgt)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if _, puts := f.counts(); puts != 0 {
		t.Errorf("a refused document must not be written, saw %d PUTs", puts)
	}
	if !strings.Contains(err.Error(), "catch-all") {
		t.Errorf("the refusal should say why, got %v", err)
	}
}

func TestTunnel_EnsureRefusesADeadTail(t *testing.T) {
	// A catch-all that is not last has already killed everything after it, so
	// inserting before the final rule would put the new route in the dead
	// section — no error, no symptom, no route.
	f := newFakeTunnel(t, `{"ingress":[
		{"hostname":"lyceum.zerogravity.industries","service":"http://lyceum:8083"},
		{"service":"http_status:404"},
		{"hostname":"wiki.zerogravity.industries","service":"http://wiki:3000"},
		{"service":"http_status:404"}
	]}`)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	_, err := f.prov(t).Ensure(context.Background(), tgt)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if _, puts := f.counts(); puts != 0 {
		t.Errorf("a refused document must not be written, saw %d PUTs", puts)
	}
	if !strings.Contains(err.Error(), "dead") {
		t.Errorf("the refusal should say the tail is dead, got %v", err)
	}
}

// --- Ensure: the shared document -------------------------------------------

func TestTunnel_EnsureReReadsInsideTheLock(t *testing.T) {
	// The plan's Inspect ran outside the lock and may be minutes old. Building a
	// PUT on it is the stale read that drops somebody else's route, so Ensure
	// takes its own.
	f := newFakeTunnel(t, liveShape)
	p := f.prov(t)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	if _, err := p.Inspect(context.Background(), tgt); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if _, err := p.Ensure(context.Background(), tgt); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// One for the plan, one before the write, one to read the write back.
	if gets, puts := f.counts(); gets != 3 || puts != 1 {
		t.Errorf("want 3 GETs and 1 PUT, got %d and %d", gets, puts)
	}
}

func TestTunnel_ConcurrentEnsuresDoNotUnrouteEachOther(t *testing.T) {
	// The headline hazard, and the reason for docMu: two spin-ups reading the
	// same document, each appending its own hostname, the later PUT erasing the
	// earlier one's. The delayed GET is what makes the window wide enough to
	// lose the race without the lock.
	f := newFakeTunnel(t, liveShape)
	f.getDelay = 25 * time.Millisecond
	p := f.prov(t)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	hosts := []string{"interlock.zerogravity.industries", "cook-book.zerogravity.industries"}
	for i, host := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = p.Ensure(context.Background(), target(t, host, "http://"+strings.Split(host, ".")[0]+":8080"))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Ensure %s: %v", hosts[i], err)
		}
	}
	got := strings.Join(f.hostnames(t), "\n")
	for _, host := range append(hosts, "switchyard.zerogravity.industries", "lyceum.zerogravity.industries", "wiki.zerogravity.industries") {
		if !strings.Contains(got, host+"=") {
			t.Errorf("%s lost its route:\n%s", host, got)
		}
	}
	rules := f.ingress(t)
	if !isCatchAll(rules[len(rules)-1]) {
		t.Errorf("the catch-all must still be last:\n%s", got)
	}
}

func TestTunnel_EnsureFailsWhenTheRouteIsNotThereAfterTheWrite(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	p := f.prov(t)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	// Somebody else's write lands on top of ours, in the window between the PUT
	// and the read-back — so the route we were about to report is not there.
	f.afterPut = func() {
		f.doc = map[string]json.RawMessage{"ingress": json.RawMessage(`[{"service":"http_status:404"}]`)}
	}

	_, err := p.Ensure(context.Background(), tgt)
	if err == nil {
		t.Fatal("a route clobbered before the read-back must not be reported as done")
	}
	if !strings.Contains(err.Error(), "another writer") {
		t.Errorf("the error should name the cause, got %v", err)
	}
}

func TestTunnel_EnsureNotesAConcurrentVersionJump(t *testing.T) {
	// Our own write moves the version by one. More than that means somebody
	// wrote between our read and our PUT, so the document we sent was built
	// without their change — the one thing confirming our own route cannot see.
	f := newFakeTunnel(t, liveShape)
	f.verBump = 3
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	res, err := f.prov(t).Ensure(context.Background(), tgt)
	if err != nil {
		t.Fatalf("this step did what it said, so it is not a failure: %v", err)
	}
	if !strings.Contains(res.Detail, "another writer") {
		t.Errorf("Detail should carry the warning, got %q", res.Detail)
	}
}

func TestTunnel_EnsureDoesNotWarnOnItsOwnWrite(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	res, err := f.prov(t).Ensure(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if strings.Contains(res.Detail, "another writer") {
		t.Errorf("a lone writer should raise nothing, got %q", res.Detail)
	}
}

func TestTunnel_EnsureSurfacesAnAPIError(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	f.getErr = "Authentication error"
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	if _, err := f.prov(t).Ensure(context.Background(), tgt); err == nil || !strings.Contains(err.Error(), "Authentication error") {
		t.Fatalf("want the API's reason, got %v", err)
	}
}

func TestTunnel_UsesTheAccountAndTunnelInThePath(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "lyceum.zerogravity.industries", "http://lyceum:8083")

	if _, err := f.prov(t).Inspect(context.Background(), tgt); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	want := "/accounts/acct/cfd_tunnel/" + testTunnelID + "/configurations"
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paths) != 1 || f.paths[0] != want {
		t.Errorf("path wrong: %v, want %q", f.paths, want)
	}
}

// --- Teardown --------------------------------------------------------------

func TestTunnel_TeardownRemovesOnlyItsOwnRule(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "lyceum.zerogravity.industries", "http://lyceum:8083")
	rec := model.ServiceResource{
		Hostname: "lyceum.zerogravity.industries",
		Kind:     model.ResourceTunnelRoute,
		ParentID: testTunnelID,
	}

	if err := f.prov(t).Teardown(context.Background(), tgt, rec); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	want := []string{
		"switchyard.zerogravity.industries=http://switchyard:4001",
		"wiki.zerogravity.industries=http://wiki:3000",
		"*=http_status:404",
	}
	if got := f.hostnames(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestTunnel_TeardownIsIdempotent(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")
	rec := model.ServiceResource{Hostname: "interlock.zerogravity.industries", ParentID: testTunnelID}

	if err := f.prov(t).Teardown(context.Background(), tgt, rec); err != nil {
		t.Fatalf("a route already gone is a success: %v", err)
	}
	if _, puts := f.counts(); puts != 0 {
		t.Errorf("nothing to remove means nothing to write, saw %d PUTs", puts)
	}
}

func TestTunnel_TeardownTargetsTheRecordedTunnel(t *testing.T) {
	// A route has no id, so its handle is (tunnel, hostname) — and the tunnel to
	// remove it from is the one it went into, which is not necessarily the one
	// the spec names today.
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "lyceum.zerogravity.industries", "http://lyceum:8083")
	rec := model.ServiceResource{Hostname: "lyceum.zerogravity.industries", ParentID: "an-older-tunnel"}

	if err := f.prov(t).Teardown(context.Background(), tgt, rec); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.paths {
		if !strings.Contains(p, "an-older-tunnel") {
			t.Fatalf("teardown hit %q, want the recorded tunnel", p)
		}
	}
}

func TestTunnel_TeardownLeavesPathScopedRulesAlone(t *testing.T) {
	f := newFakeTunnel(t, `{"ingress":[
		{"hostname":"lyceum.zerogravity.industries","path":"/admin","service":"http://admin:9000"},
		{"hostname":"lyceum.zerogravity.industries","service":"http://lyceum:8083"},
		{"service":"http_status:404"}
	]}`)
	tgt := target(t, "lyceum.zerogravity.industries", "http://lyceum:8083")
	rec := model.ServiceResource{Hostname: "lyceum.zerogravity.industries", ParentID: testTunnelID}

	if err := f.prov(t).Teardown(context.Background(), tgt, rec); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	rules := f.ingress(t)
	if len(rules) != 2 || rules[0].str("path") != "/admin" {
		t.Errorf("a narrower hand-written route is not ours to delete, got %v", f.hostnames(t))
	}
}

// --- the pure planner ------------------------------------------------------

func TestPlanRoute_InsertsAtTheTerminalIndex(t *testing.T) {
	rules := []ingressRule{
		{"hostname": jsonString("a.example.com"), "service": jsonString("http://a:1")},
		{"service": jsonString(catchAllService)},
	}
	next, changed, _, err := planRoute(rules, "b.example.com", "http://b:2")
	if err != nil || !changed {
		t.Fatalf("planRoute: changed=%v err=%v", changed, err)
	}
	if len(next) != 3 || next[1].str("hostname") != "b.example.com" || !isCatchAll(next[2]) {
		t.Fatalf("wrong placement: %v", next)
	}
	// The planner must not write through the list it was handed — Ensure asserts
	// on the result before sending it, and an aliased edit would make that
	// assertion inspect the same object it was meant to check.
	if len(rules) != 2 {
		t.Errorf("planRoute mutated its input: %v", rules)
	}
}

func TestTerminalIndex_Refusals(t *testing.T) {
	cases := []struct {
		name  string
		rules []ingressRule
		want  string
	}{
		{"empty", nil, "empty"},
		{
			"no catch-all",
			[]ingressRule{{"hostname": jsonString("a.example.com"), "service": jsonString("http://a:1")}},
			"catch-all",
		},
		{
			"catch-all not last",
			[]ingressRule{
				{"service": jsonString(catchAllService)},
				{"hostname": jsonString("a.example.com"), "service": jsonString("http://a:1")},
				{"service": jsonString(catchAllService)},
			},
			"dead",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := terminalIndex(tc.rules)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestConcurrentWriteNote(t *testing.T) {
	if note := concurrentWriteNote(5, 6, "t"); note != "" {
		t.Errorf("our own write moves the version by one: %q", note)
	}
	if note := concurrentWriteNote(0, 0, "t"); note != "" {
		t.Errorf("an API that reports no version should raise nothing: %q", note)
	}
	if note := concurrentWriteNote(5, 8, "t"); note == "" {
		t.Error("a jump of three is another writer, and must be reported")
	}
}

// --- the axis boundary -----------------------------------------------------

func TestTunnel_UnavailableIsTheSpinUpSentinelNotThePersonAxisOne(t *testing.T) {
	// The two axes share an ethos and no types. ErrPending's wording is pinned
	// by migration 0004's backfill, which is why this axis has its own.
	p := NewTunnel(TunnelConfig{})
	err := p.Teardown(context.Background(), spinup.Target{Spec: spinup.ServiceSpec{Hostname: "x.example.com"}}, model.ServiceResource{})
	if !errors.Is(err, spinup.ErrUnavailable) {
		t.Fatalf("want spinup.ErrUnavailable, got %v", err)
	}
}
