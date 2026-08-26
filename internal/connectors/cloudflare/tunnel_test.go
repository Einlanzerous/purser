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
	// source is what the tunnel reports about how it is managed. Only
	// "cloudflare" means the document this endpoint serves is the one in force.
	source string

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
	f := &fakeTunnel{ver: 5, verBump: 1, source: "cloudflare"}
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
				"source":    f.source,
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

// --- the dead tail, on the read path ---------------------------------------

// deadTail is a document whose catch-all is not last: everything behind it is
// already unreachable, wiki included.
const deadTail = `{"ingress":[
	{"hostname":"lyceum.zerogravity.industries","service":"http://lyceum:8083"},
	{"service":"http_status:404"},
	{"hostname":"wiki.zerogravity.industries","service":"http://wiki:3000"},
	{"service":"http_status:404"}
]}`

func TestTunnel_InspectDoesNotReportADeadRuleAsARoute(t *testing.T) {
	// cloudflared never reaches rule 3. Reporting it as in place would give the
	// orchestrator `ok` (or `adopt`), which is inPlace(), which unblocks the DNS
	// step — and publishing a hostname in front of a tunnel that will not serve
	// it is the exact window model.KindOrder and ServiceSpec.dependsOn exist to
	// close.
	f := newFakeTunnel(t, deadTail)
	tgt := target(t, "wiki.zerogravity.industries", "http://wiki:3000")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err == nil {
		t.Fatalf("a rule in the dead tail is not a working route, got %+v", st)
	}
	if st.Exists || st.Matches {
		t.Errorf("no state should come back with the refusal, got %+v", st)
	}
	if !strings.Contains(err.Error(), "dead") {
		t.Errorf("the refusal should say why, got %v", err)
	}
}

func TestTunnel_EnsureDoesNotSkipADeadRule(t *testing.T) {
	// The other half of the same bug: planRoute's already-routed branch would
	// return "nothing to do" for a route that serves nothing.
	f := newFakeTunnel(t, deadTail)
	tgt := target(t, "wiki.zerogravity.industries", "http://wiki:3000")

	res, err := f.prov(t).Ensure(context.Background(), tgt)
	if err == nil {
		t.Fatalf("want a refusal, got %+v", res)
	}
	if _, puts := f.counts(); puts != 0 {
		t.Errorf("a refused document must not be written, saw %d PUTs", puts)
	}
}

func TestTunnel_InspectReportsAReachableRouteOnAMalformedDocument(t *testing.T) {
	// The converse, and the reason this is not just "refuse malformed
	// documents": lyceum is *before* the stray catch-all, so it is served. Its
	// resource is already published, so withholding the report protects nobody —
	// the malformation is somebody else's dead rules, and it is said out loud.
	f := newFakeTunnel(t, deadTail)
	tgt := target(t, "lyceum.zerogravity.industries", "http://lyceum:8083")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err != nil {
		t.Fatalf("a served hostname is still a served hostname: %v", err)
	}
	if !st.Exists || !st.Matches {
		t.Errorf("want in place, got %+v", st)
	}
	if !strings.Contains(st.Detail, "malformed") {
		t.Errorf("the broken document should be reported, got %q", st.Detail)
	}
}

// --- shadowing: a wildcard stops the walk too --------------------------------

// wildcardFirst is a legitimate, documented configuration — a holding page in
// front of the whole zone — not a malformed document. Its catch-all *is* last,
// so documentShape has nothing to say about it.
const wildcardFirst = `{"ingress":[
	{"hostname":"*.zerogravity.industries","service":"http://holding-page:80"},
	{"service":"http_status:404"}
]}`

// wildcardShadowing is the same, with a rule for wiki stranded behind it.
const wildcardShadowing = `{"ingress":[
	{"hostname":"*.zerogravity.industries","service":"http://holding-page:80"},
	{"hostname":"wiki.zerogravity.industries","service":"http://wiki:3000"},
	{"service":"http_status:404"}
]}`

func TestTunnel_InspectDoesNotReportAShadowedRuleAsARoute(t *testing.T) {
	// cloudflared matches top-down, first match wins, and a hostname may carry
	// wildcards — so the catch-all is not the only thing that ends the walk.
	// Reporting rule 2 as in place gives the orchestrator ok/adopt, both
	// inPlace(), and DNS publishes a hostname that serves the holding page.
	f := newFakeTunnel(t, wildcardShadowing)
	tgt := target(t, "wiki.zerogravity.industries", "http://wiki:3000")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if st.Exists || st.Matches {
		t.Fatalf("a rule behind a wildcard is not a working route, got %+v", st)
	}
	// The plain "no ingress rule" line would be true about what is served and
	// misleading about what is written.
	if !strings.Contains(st.Detail, "never matched") || !strings.Contains(st.Detail, "*.zerogravity.industries") {
		t.Errorf("Detail should name the shadow and the stranded rule, got %q", st.Detail)
	}
}

func TestTunnel_EnsureInsertsAheadOfAShadowingWildcard(t *testing.T) {
	// The worse half: here Purser is the one creating the dead rule. Inserting
	// before the *terminal* rule puts the new route behind the wildcard, and
	// confirmRoute — reading through the same helper — then reports success.
	f := newFakeTunnel(t, wildcardFirst)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	res, err := f.prov(t).Ensure(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := []string{
		"interlock.zerogravity.industries=http://interlock:8080",
		"*.zerogravity.industries=http://holding-page:80",
		"*=http_status:404",
	}
	if got := f.hostnames(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the new route must go in front of the rule that would take it.\n got: %v\nwant: %v", got, want)
	}
	if !strings.Contains(res.Detail, "*.zerogravity.industries") {
		t.Errorf("Detail should name what it was inserted before, got %q", res.Detail)
	}
}

func TestTunnel_EnsureLeavesAStrandedRuleAloneAndSaysSo(t *testing.T) {
	f := newFakeTunnel(t, wildcardShadowing)
	tgt := target(t, "wiki.zerogravity.industries", "http://wiki:3000")

	res, err := f.prov(t).Ensure(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	rules := f.ingress(t)
	if len(rules) != 4 || rules[0].str("hostname") != "wiki.zerogravity.industries" {
		t.Fatalf("the working route must go first, got %v", f.hostnames(t))
	}
	// A rule Purser did not write is not one it deletes — it was already dead
	// before this run — but leaving it silently would be the same trap again.
	if !strings.Contains(res.Detail, "stay unmatched") {
		t.Errorf("the stranded rule should be reported, got %q", res.Detail)
	}
}

func TestTunnel_AWildcardIsNeverAdoptedAsOurOwnRoute(t *testing.T) {
	// Even when the wildcard happens to serve exactly what the spec asks for.
	// Adopting it would have Teardown delete a rule standing in front of every
	// other hostname in the zone.
	f := newFakeTunnel(t, `{"ingress":[
		{"hostname":"*.zerogravity.industries","service":"http://interlock:8080"},
		{"service":"http_status:404"}
	]}`)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if st.Exists {
		t.Fatalf("a wildcard is somebody else's rule, got %+v", st)
	}
	if _, err := f.prov(t).Ensure(context.Background(), tgt); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := f.ingress(t); len(got) != 3 || got[0].str("hostname") != "interlock.zerogravity.industries" {
		t.Errorf("want our own literal rule in front, got %v", f.hostnames(t))
	}
}

func TestHostnameTakes(t *testing.T) {
	// Pins cloudflared's rule, read from ingress/rule.go and ingress/ingress.go
	// rather than inferred. A general `*`-glob was wrong here in two directions
	// at once, and the apex row is the one that decides the dangerous one.
	cases := []struct {
		pattern, host string
		want          bool
		why           string
	}{
		{"", "wiki.zerogravity.industries", true, "no hostname is the terminal catch-all"},
		{"*", "wiki.zerogravity.industries", true, "cloudflared special-cases a bare star"},
		{"*.zerogravity.industries", "wiki.zerogravity.industries", true, "the documented wildcard"},
		{"*.zerogravity.industries", "zerogravity.industries", false,
			"only the star is trimmed, so the suffix tested is \".zerogravity.industries\" and the apex lacks the dot"},
		{"*.zerogravity.industries", "a.b.zerogravity.industries", true, "HasSuffix, so any depth"},
		{"*.example.com", "wiki.zerogravity.industries", false, "different zone"},
		{"wiki.*", "wiki.zerogravity.industries", false, "only a LEADING \"*.\" is a wildcard; this is a literal upstream"},
		{"*.*.industries", "wiki.zerogravity.industries", false, "likewise — the suffix tested is \".*.industries\""},
		{"wiki.zerogravity.industries", "wiki.zerogravity.industries", true, "exact"},
		{"WIKI.zerogravity.industries", "wiki.zerogravity.industries", true, "case folded, where cloudflared compares bytes"},
		{"lyceum.zerogravity.industries", "wiki.zerogravity.industries", false, "different host"},
	}
	for _, tc := range cases {
		if got := hostnameTakes(tc.pattern, tc.host); got != tc.want {
			t.Errorf("hostnameTakes(%q, %q) = %v, want %v — %s", tc.pattern, tc.host, got, tc.want, tc.why)
		}
	}
}

func TestTunnel_AWildcardDoesNotHoldBackTheApex(t *testing.T) {
	// cloudflared's `*.zerogravity.industries` does not take the bare apex, so
	// the apex route belongs in front of the terminal rule and works there.
	// Blocking it would be a wrong refusal, and putting it behind the wildcard
	// would be a dead route — this pins which one is right.
	f := newFakeTunnel(t, wildcardFirst)
	tgt := target(t, "zerogravity.industries", "http://apex:8080")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if st.Exists {
		t.Fatalf("want a create, got %+v", st)
	}
	if _, err := f.prov(t).Ensure(context.Background(), tgt); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := []string{
		"*.zerogravity.industries=http://holding-page:80",
		"zerogravity.industries=http://apex:8080",
		"*=http_status:404",
	}
	if got := f.hostnames(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the apex is not shadowed by *.zone, so it goes before the terminal rule.\n got: %v\nwant: %v", got, want)
	}
}

func TestTunnel_ALiteralWithAStarInItDoesNotStopTheWalk(t *testing.T) {
	// The other direction a glob got wrong. Only a leading "*." is a wildcard
	// upstream, so `wiki.*` matches nothing there — treating it as a pattern
	// would report `create` for a hostname whose real rule is right below it,
	// insert a duplicate in front, and call a serving rule "never matched".
	f := newFakeTunnel(t, `{"ingress":[
		{"hostname":"wiki.*","service":"http://never-matched:80"},
		{"hostname":"wiki.zerogravity.industries","service":"http://wiki:3000"},
		{"service":"http_status:404"}
	]}`)
	tgt := target(t, "wiki.zerogravity.industries", "http://wiki:3000")

	st, err := f.prov(t).Inspect(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !st.Exists || !st.Matches {
		t.Fatalf("the real rule is reachable, got %+v", st)
	}
	if strings.Contains(st.Detail, "never matched") {
		t.Errorf("nothing is shadowing this route, got %q", st.Detail)
	}
	if _, err := f.prov(t).Ensure(context.Background(), tgt); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, puts := f.counts(); puts != 0 {
		t.Errorf("an already-correct route needs no write, saw %d PUTs", puts)
	}
}

func TestTunnel_AStarHostnameIsACatchAll(t *testing.T) {
	// cloudflared treats hostname "*" exactly like no hostname, so a mid-list
	// one kills the tail just the same.
	f := newFakeTunnel(t, `{"ingress":[
		{"hostname":"*","service":"http://holding-page:80"},
		{"hostname":"wiki.zerogravity.industries","service":"http://wiki:3000"},
		{"service":"http_status:404"}
	]}`)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	if _, err := f.prov(t).Inspect(context.Background(), tgt); err == nil {
		t.Fatal("a star hostname before the end is a dead tail")
	}
	if _, err := f.prov(t).Ensure(context.Background(), tgt); err == nil {
		t.Fatal("and must not be written into")
	}
	if _, puts := f.counts(); puts != 0 {
		t.Errorf("saw %d PUTs", puts)
	}
}

func TestTunnel_PreviewAndApplyAgreeOnEveryDocument(t *testing.T) {
	// PRSR-27's property: a plan is the first half of the apply, not a guess at
	// it. The failure this pins is a preview that promises `create` against a
	// document Ensure then refuses — which is what the dead-tail fix above is
	// really about, since the absent-hostname case has the same shape as the
	// dead-rule one.
	cases := []struct {
		name     string
		doc      string
		host     string
		upstream string
	}{
		{"well-formed, absent", liveShape, "interlock.zerogravity.industries", "http://interlock:8080"},
		{"well-formed, present", liveShape, "lyceum.zerogravity.industries", "http://lyceum:8083"},
		{"well-formed, mismatched", liveShape, "lyceum.zerogravity.industries", "http://lyceum:9999"},
		{"empty ingress", `{"ingress":[]}`, "interlock.zerogravity.industries", "http://interlock:8080"},
		{"dead tail, absent", deadTail, "interlock.zerogravity.industries", "http://interlock:8080"},
		{"dead tail, dead rule", deadTail, "wiki.zerogravity.industries", "http://wiki:3000"},
		{"dead tail, live rule", deadTail, "lyceum.zerogravity.industries", "http://lyceum:8083"},
		{"no catch-all", `{"ingress":[{"hostname":"a.example.com","service":"http://a:1"}]}`, "interlock.zerogravity.industries", "http://interlock:8080"},
		{"wildcard first, absent", wildcardFirst, "interlock.zerogravity.industries", "http://interlock:8080"},
		{"wildcard first, shadowed rule", wildcardShadowing, "wiki.zerogravity.industries", "http://wiki:3000"},
		{"wildcard first, live rule", wildcardShadowing, "lyceum.zerogravity.industries", "http://lyceum:8083"},
		{"wildcard first, apex spec", wildcardFirst, "zerogravity.industries", "http://apex:8080"},
		{"star hostname mid-list", `{"ingress":[{"hostname":"*","service":"http://h:80"},{"hostname":"a.example.com","service":"http://a:1"},{"service":"http_status:404"}]}`, "interlock.zerogravity.industries", "http://interlock:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTunnel(t, tc.doc)
			p := f.prov(t)
			tgt := target(t, tc.host, tc.upstream)

			_, inspectErr := p.Inspect(context.Background(), tgt)
			_, ensureErr := p.Ensure(context.Background(), tgt)

			if (inspectErr == nil) != (ensureErr == nil) {
				t.Fatalf("the preview and the apply disagree: Inspect=%v, Ensure=%v", inspectErr, ensureErr)
			}
		})
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
	// On Warning, not Detail. Detail describes *this* resource and a surface may
	// truncate it; this says another service's route may have been dropped from
	// the shared document, which a caller must be able to find without picking a
	// substring out of a description (PRSR-31).
	if !strings.Contains(res.Warning, "another writer") {
		t.Errorf("Warning should carry the note, got %q", res.Warning)
	}
	if strings.Contains(res.Detail, "another writer") {
		t.Errorf("the note belongs on Warning alone, not also on Detail: %q", res.Detail)
	}
}

func TestTunnel_EnsureDoesNotWarnOnItsOwnWrite(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	res, err := f.prov(t).Ensure(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Warning != "" {
		t.Errorf("a lone writer should raise nothing, got %q", res.Warning)
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

// --- is this document the one in force? -------------------------------------

func TestTunnel_RefusesALocallyManagedTunnel(t *testing.T) {
	// A locally-managed tunnel serves a YAML file on the origin machine, so the
	// remote configuration is not what it runs. Every other guard here is about
	// *who else wrote this document*; none of them can ask whether it is live.
	//
	// Left unchecked the whole sequence succeeds: the read reports "no ingress
	// rule", the PUT is stored, the read-back finds the route, confirmRoute
	// passes, DNS publishes — and the tunnel has never heard of the hostname.
	f := newFakeTunnel(t, liveShape)
	f.source = "local"
	p := f.prov(t)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	st, err := p.Inspect(context.Background(), tgt)
	if err == nil {
		t.Fatalf("want a refusal, got %+v", st)
	}
	if st.Exists {
		t.Errorf("no state should come back with the refusal, got %+v", st)
	}
	if spinup.IsUnavailable(err) {
		t.Error("the provisioner is configured and working; the tunnel is the problem")
	}
	if !strings.Contains(err.Error(), "origin machine") {
		t.Errorf("the refusal should say what to do instead, got %v", err)
	}

	if _, err := p.Ensure(context.Background(), tgt); err == nil {
		t.Error("Ensure must refuse too")
	}
	if err := p.Teardown(context.Background(), tgt, model.ServiceResource{
		Hostname: "lyceum.zerogravity.industries", ParentID: testTunnelID,
	}); err == nil {
		// Removing a route from a document that isn't in force removes nothing,
		// and Teardown may only report success when the resource is gone.
		t.Error("Teardown must refuse too")
	}
	if _, puts := f.counts(); puts != 0 {
		t.Errorf("nothing should be written to a tunnel that would not serve it, saw %d PUTs", puts)
	}
}

func TestTunnel_RefusesATunnelThatReportsNoSource(t *testing.T) {
	// "We could not tell" is not "it is fine" — the axis's oldest invariant.
	f := newFakeTunnel(t, liveShape)
	f.source = ""
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	_, err := f.prov(t).Inspect(context.Background(), tgt)
	if err == nil {
		t.Fatal("an unverifiable management mode is not a verified one")
	}
	if !strings.Contains(err.Error(), "did not report") {
		t.Errorf("the refusal should name what was missing, got %v", err)
	}
}

func TestTunnel_ConversionBetweenPlanAndApplyIsCaught(t *testing.T) {
	// Ensure re-reads inside the lock, so a tunnel converted after the plan was
	// made is refused rather than written to on the strength of the old read.
	f := newFakeTunnel(t, liveShape)
	p := f.prov(t)
	tgt := target(t, "interlock.zerogravity.industries", "http://interlock:8080")

	if _, err := p.Inspect(context.Background(), tgt); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	f.mu.Lock()
	f.source = "local"
	f.mu.Unlock()

	if _, err := p.Ensure(context.Background(), tgt); err == nil {
		t.Fatal("want a refusal from the fresh read")
	}
	if _, puts := f.counts(); puts != 0 {
		t.Errorf("saw %d PUTs", puts)
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
	if note := concurrentWriteNote(5, 5, "t"); note != "" {
		t.Errorf("a read that lagged our own write says nothing about another writer: %q", note)
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

// The refusals this file already makes are only half of what the orchestrator
// needs: it has to be able to tell them apart from a read that never completed,
// because the two want opposite things from an operator (PRSR-31). A missing
// sentinel here is invisible — every one of these still refuses, and every one
// would report as `unknown`, telling the operator to re-run something that will
// say this until they go and fix the tunnel.
func TestTunnel_UnwritableDocumentsAreRefusedNotMerelyUnknown(t *testing.T) {
	const noCatchAll = `{"ingress":[
		{"hostname":"lyceum.zerogravity.industries","service":"http://lyceum:8083"}
	]}`
	host := "interlock.zerogravity.industries"
	cases := []struct {
		name   string
		config string
		source string
	}{
		{"a catch-all that is not last has already killed the tail", deadTail, "cloudflare"},
		{"no catch-all at all is not a document cloudflared would serve", noCatchAll, "cloudflare"},
		{"a locally-managed tunnel is not serving this document", liveShape, "local"},
		{"a tunnel that will not say is not evidence either way", liveShape, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTunnel(t, tc.config)
			f.source = tc.source
			p := f.prov(t)
			// A hostname the document does not already serve, so this is an
			// answer --apply would act on rather than an already-published one.
			tgt := target(t, host, "http://interlock:8080")

			_, err := p.Inspect(context.Background(), tgt)
			if err == nil {
				t.Fatal("want a refusal")
			}
			if !spinup.IsRefused(err) {
				t.Errorf("Inspect: the orchestrator must read this as refused, not as a failed read: %v", err)
			}
			if spinup.IsUnavailable(err) {
				t.Error("the provisioner is configured; what is wrong is upstream")
			}

			if _, err := p.Ensure(context.Background(), tgt); err == nil {
				t.Error("Ensure must refuse too")
			} else if !spinup.IsRefused(err) {
				t.Errorf("Ensure: want a refusal the orchestrator can recognise, got %v", err)
			}
			if _, puts := f.counts(); puts != 0 {
				t.Errorf("a refused document must not be written, saw %d PUTs", puts)
			}
		})
	}
}

// The converse, and the reason the sentinel rides on documentShape rather than
// on terminalIndex: a read that did not produce a document must stay `unknown`.
// Telling an operator to go and fix a tunnel that is healthy is the same wrong
// answer pointed the other way, and it is the more likely of the two.
func TestTunnel_AFailedReadIsNotARefusal(t *testing.T) {
	f := newFakeTunnel(t, liveShape)
	f.getErr = "service temporarily unavailable"
	p := f.prov(t)

	_, err := p.Inspect(context.Background(), target(t, "interlock.zerogravity.industries", "http://interlock:8080"))
	if err == nil {
		t.Fatal("want an error")
	}
	if spinup.IsRefused(err) {
		t.Errorf("a failed read is not a document anybody has judged: %v", err)
	}
}
