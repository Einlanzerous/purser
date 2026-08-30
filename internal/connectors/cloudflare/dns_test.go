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

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

const (
	testZoneID   = "zone123"
	testZoneName = "zerogravity.industries"
	testTunnelID = "aef21667-0000-4000-8000-000000000001"
)

// --- specs -----------------------------------------------------------------

// directTarget is Argosy's shape: the epic's pilot, already up, on the direct
// path with a static endpoint.
func directTarget(upstream string) spinup.Target {
	return spinup.Target{Spec: spinup.ServiceSpec{
		Key:      "argosy",
		Hostname: "argosy." + testZoneName,
		Mode:     spinup.ModeDirect,
		Upstream: upstream,
		Access:   spinup.AccessBookmark,
	}}
}

// tunnelledTarget is the other shape: a gated service behind the prod tunnel,
// with the tunnel id already resolved by the orchestrator.
func tunnelledTarget() spinup.Target {
	return spinup.Target{
		Spec: spinup.ServiceSpec{
			Key:      "interlock",
			Hostname: "interlock." + testZoneName,
			Mode:     spinup.ModeTunnelled,
			Upstream: "http://interlock:8080",
			Access:   spinup.AccessGated,
			Tunnel:   spinup.TunnelProd,
		},
		TunnelID: testTunnelID,
	}
}

// --- a fake zone -----------------------------------------------------------

// fakeZone is an in-memory Cloudflare zone speaking enough of the DNS API to run
// the ticket's verification shape end to end: create a record, read it back,
// delete it. It records every call, because the read-only guarantee is a claim
// about which requests are made and can only be checked by counting them.
type fakeZone struct {
	mu      sync.Mutex
	records map[string]dnsRecord
	next    int
	calls   []string

	// zoneName is what GET /zones/{id} answers with — the read PRSR-39's
	// pre-flight makes before it will look a hostname up. Empty models a 200
	// that named no zone, which must read as "could not tell" rather than as a
	// zone every hostname is inside of.
	zoneName string
	// zoneUnreadable fails that read. It is the state the create-path backstop
	// is now the only guard in, so it is a fixture knob rather than a detail:
	// see TestDNS_Ensure_OutOfZoneHostnameIsRefusedAndCleanedUp.
	zoneUnreadable bool
	// nameOnCreate, when set, decides the name the zone actually stores for a
	// create, overriding the zone-append rule below.
	//
	// It models a normalisation surprise — Cloudflare writing something other
	// than what was asked for a hostname that *is* in the zone — which since the
	// pre-flight landed is the only way to reach wrongName on a run where the
	// zone could be read.
	nameOnCreate func(asked string) string
}

func newZone(seed ...dnsRecord) *fakeZone {
	z := &fakeZone{records: map[string]dnsRecord{}, zoneName: testZoneName}
	for _, r := range seed {
		z.put(r)
	}
	return z
}

// put stores a record and assigns an id.
//
// It deliberately does NOT fill ZoneID or ZoneName. It used to, with a comment
// calling them "the zone fields the API echoes back" — and PRSR-42 measured the
// live API and found it echoes back neither, on a create response or a GET.
// dnsRecord decodes both and both are always empty in production.
//
// That matters because a fixture supplying a field the API withholds is the
// PRSR-38 trap in its inverse direction: the suite would exercise wrongName's
// zone-naming branch, which cannot fire against Cloudflare, and would leave
// zoneOf's first operand looking load-bearing when it never is. Third time this
// package has met this — PRSR-38 through fixture data, PRSR-40 through missing
// keys, and now through extra ones.
func (z *fakeZone) put(r dnsRecord) dnsRecord {
	if r.ID == "" {
		z.next++
		r.ID = fmt.Sprintf("rec%d", z.next)
	}
	z.records[r.ID] = r
	return r
}

func (z *fakeZone) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cf_token" {
			t.Errorf("missing bearer auth on %s %s", r.Method, r.URL.Path)
		}
		z.mu.Lock()
		defer z.mu.Unlock()
		z.calls = append(z.calls, r.Method+" "+r.URL.Path)

		if r.Method == http.MethodGet && r.URL.Path == "/zones/"+testZoneID {
			z.zone(w)
			return
		}

		rest, ok := strings.CutPrefix(r.URL.Path, "/zones/"+testZoneID+"/dns_records")
		if !ok {
			cfFail(w, http.StatusNotFound, 7003, "Could not route to "+r.URL.Path)
			return
		}
		id := strings.TrimPrefix(rest, "/")

		switch {
		case r.Method == http.MethodGet && id == "":
			z.list(w, r.URL.Query().Get("name"))
		case r.Method == http.MethodPost:
			z.create(t, w, r)
		case r.Method == http.MethodGet:
			z.one(w, id)
		case r.Method == http.MethodPatch:
			z.update(t, w, r, id)
		case r.Method == http.MethodDelete:
			z.remove(w, id)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// zone answers the pre-flight's GET /zones/{id}.
//
// The key set is the subset this package reads plus the two an operator would
// recognise; the live response carries far more. Only `name` is decoded, which
// is the point of keeping the fixture honest about there being other keys.
func (z *fakeZone) zone(w http.ResponseWriter) {
	if z.zoneUnreadable {
		cfFail(w, http.StatusInternalServerError, 10000, "internal error")
		return
	}
	cfOK(w, map[string]any{"id": testZoneID, "name": z.zoneName, "status": "active"})
}

func (z *fakeZone) list(w http.ResponseWriter, name string) {
	out := []dnsRecord{}
	for _, r := range z.records {
		if name == "" || strings.EqualFold(r.Name, name) {
			out = append(out, r)
		}
	}
	cfOK(w, out)
}

func (z *fakeZone) create(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var in recordBody
	decodeBody(t, r, &in)
	name := in.Name
	switch {
	case z.nameOnCreate != nil:
		name = z.nameOnCreate(name)
	// Cloudflare treats a name outside the zone as relative to it and appends
	// the zone silently — the trap wrongName() exists to catch.
	case !strings.EqualFold(name, testZoneName) && !strings.HasSuffix(strings.ToLower(name), "."+testZoneName):
		name += "." + testZoneName
	}
	cfOK(w, z.put(dnsRecord{Type: in.Type, Name: name, Content: in.Content, Proxied: in.Proxied, TTL: in.TTL}))
}

func (z *fakeZone) one(w http.ResponseWriter, id string) {
	r, ok := z.records[id]
	if !ok {
		cfFail(w, http.StatusNotFound, errCodeRecordNotFound, "Record does not exist.")
		return
	}
	cfOK(w, r)
}

func (z *fakeZone) update(t *testing.T, w http.ResponseWriter, r *http.Request, id string) {
	t.Helper()
	cur, ok := z.records[id]
	if !ok {
		cfFail(w, http.StatusNotFound, errCodeRecordNotFound, "Record does not exist.")
		return
	}
	var in recordBody
	decodeBody(t, r, &in)
	cur.Type, cur.Name, cur.Content, cur.Proxied, cur.TTL = in.Type, in.Name, in.Content, in.Proxied, in.TTL
	cfOK(w, z.put(cur))
}

func (z *fakeZone) remove(w http.ResponseWriter, id string) {
	if _, ok := z.records[id]; !ok {
		cfFail(w, http.StatusNotFound, errCodeRecordNotFound, "Record does not exist.")
		return
	}
	delete(z.records, id)
	// Deliberately NOT cfOK: Cloudflare's DNS delete is the one route in this
	// client's reach that answers with a bare result and no {success, errors}
	// envelope. Wrapping it here would make the suite assert this package's
	// model of the API instead of the API, which is how the envelope bug reached
	// review — the delete is the first DELETE this client has ever sent.
	_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]string{"id": id}})
}

// callLog returns a copy of the recorded calls.
func (z *fakeZone) callLog() []string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]string(nil), z.calls...)
}

// countCalls returns how many recorded calls used a method.
func (z *fakeZone) countCalls(method string) int {
	z.mu.Lock()
	defer z.mu.Unlock()
	n := 0
	for _, c := range z.calls {
		if strings.HasPrefix(c, method+" ") {
			n++
		}
	}
	return n
}

// writes counts every request that could change the zone. Inspect must make none.
func (z *fakeZone) writes() int {
	return z.countCalls(http.MethodPost) + z.countCalls(http.MethodPatch) +
		z.countCalls(http.MethodPut) + z.countCalls(http.MethodDelete)
}

// recordCount is how many records the zone holds.
func (z *fakeZone) recordCount() int {
	z.mu.Lock()
	defer z.mu.Unlock()
	return len(z.records)
}

// serveZone answers the pre-flight's GET /zones/{id} on a hand-rolled server,
// and reports whether it handled the request.
//
// Every server in this file models one route, and PRSR-39 gave the provisioner a
// second one they all now receive. A server that ignores it does not skip the
// pre-flight — it answers the zone read with whatever its single handler
// returns, which is a record list, which fails to decode as a zone, which
// disables the pre-flight for the rest of the test. That is a fake modelling the
// shape the test author assumed, and it is how TestDNS_Ensure_CreateConflict…
// briefly came to pass without ever reaching the conflict it is named for.
//
// So a server that wants the pre-flight live calls this first, and a server that
// wants it dead (the total-outage tests) says so in its comment.
func serveZone(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/zones/"+testZoneID {
		return false
	}
	cfOK(w, map[string]any{"id": testZoneID, "name": testZoneName, "status": "active"})
	return true
}

func cfOK(w http.ResponseWriter, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
}

func cfFail(w http.ResponseWriter, status, code int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": false,
		"errors":  []map[string]any{{"code": code, "message": msg}},
	})
}

func decodeBody(t *testing.T, r *http.Request, into any) {
	t.Helper()
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode request body %q: %v", raw, err)
	}
}

// dnsFor builds a provisioner against a fake zone.
func dnsFor(t *testing.T, z *fakeZone) *DNSProvisioner {
	t.Helper()
	return newDNSWithBase(t, z.serve(t).URL, DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
}

// --- the round trip --------------------------------------------------------

// The shape of the scope probe PRSR-11 ran against the live API, as a test:
// create a record, read it back, delete it — plus the two claims that make the
// step idempotent, that a second Ensure writes nothing and that the id survives
// into the resource row.
func TestDNS_RoundTrip_CreateReadBackDelete(t *testing.T) {
	ctx := context.Background()
	z := newZone()
	p := dnsFor(t, z)
	target := directTarget("100.64.0.7")

	before, err := p.Inspect(ctx, target)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if before.Exists {
		t.Fatalf("empty zone should report nothing there, got %+v", before)
	}
	if z.writes() != 0 {
		t.Fatalf("Inspect wrote to the zone %d time(s) — it must be read-only", z.writes())
	}

	res, err := p.Ensure(ctx, target)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.ExternalID == "" {
		t.Error("the record id must come back — Teardown targets ids, not names")
	}
	if res.ParentID != testZoneID {
		t.Errorf("parent should be the zone, got %q", res.ParentID)
	}

	after, err := p.Inspect(ctx, target)
	if err != nil {
		t.Fatalf("Inspect after create: %v", err)
	}
	if !after.Exists || !after.Matches {
		t.Fatalf("the record just created should read back as matching, got %+v", after)
	}
	if after.ExternalID != res.ExternalID {
		t.Errorf("read-back id %q != created id %q", after.ExternalID, res.ExternalID)
	}

	creates := z.countCalls(http.MethodPost)
	if _, err := p.Ensure(ctx, target); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if z.countCalls(http.MethodPost) != creates {
		t.Error("a second Ensure created a second record — already-correct must be a no-op")
	}

	rec := model.ServiceResource{
		Hostname: target.Spec.Hostname, Kind: model.ResourceDNSRecord,
		ExternalID: res.ExternalID, ParentID: res.ParentID,
	}
	if _, err := p.Teardown(ctx, target, rec); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	gone, err := p.Inspect(ctx, target)
	if err != nil {
		t.Fatalf("Inspect after teardown: %v", err)
	}
	if gone.Exists {
		t.Error("the record should be gone after Teardown")
	}
	// Idempotent: tearing down what is already gone is a success, not an error.
	if _, err := p.Teardown(ctx, target, rec); err != nil {
		t.Errorf("a second Teardown should be a no-op success, got %v", err)
	}
}

// --- Inspect ---------------------------------------------------------------

func TestDNS_Inspect_AbsentDescribesWhatTheSpecWants(t *testing.T) {
	st, err := dnsFor(t, newZone()).Inspect(context.Background(), tunnelledTarget())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if st.Exists {
		t.Fatal("nothing is there")
	}
	// The plan line reads "create — <detail>", so the detail has to say what is
	// about to be created.
	if !strings.Contains(st.Detail, "proxied CNAME") || !strings.Contains(st.Detail, tunnelSuffix) {
		t.Errorf("detail should describe the record the spec wants, got %q", st.Detail)
	}
}

func TestDNS_Inspect_MatchingTunnelRecord(t *testing.T) {
	z := newZone(dnsRecord{
		Type: "CNAME", Name: "interlock." + testZoneName,
		Content: testTunnelID + tunnelSuffix, Proxied: true, TTL: 1,
	})
	st, err := dnsFor(t, z).Inspect(context.Background(), tunnelledTarget())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !st.Exists || !st.Matches {
		t.Fatalf("a correct proxied CNAME should match, got %+v", st)
	}
	if st.ExternalID == "" || st.ParentID != testZoneID {
		t.Errorf("Inspect must report the ids so an adopt can record them, got %+v", st)
	}
}

// SERV-45: an unproxied CNAME to cfargotunnel.com resolves to something nothing
// can connect to. It is the easy thing to miss, so it must read as a mismatch
// rather than as a record that is already fine.
func TestDNS_Inspect_UnproxiedTunnelRecordIsAMismatch(t *testing.T) {
	z := newZone(dnsRecord{
		Type: "CNAME", Name: "interlock." + testZoneName,
		Content: testTunnelID + tunnelSuffix, Proxied: false, TTL: 1,
	})
	st, err := dnsFor(t, z).Inspect(context.Background(), tunnelledTarget())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !st.Exists {
		t.Fatal("the record is there")
	}
	if st.Matches {
		t.Error("an unproxied CNAME to cfargotunnel.com does not route — it must not report as matching")
	}
}

// The direct path expresses no opinion about the orange cloud, so an existing
// record's proxy setting is not drift. Reporting it would have --apply flip the
// traffic path of a service that is already up.
func TestDNS_Inspect_DirectIgnoresTheProxyFlag(t *testing.T) {
	for _, proxied := range []bool{true, false} {
		z := newZone(dnsRecord{Type: "A", Name: "argosy." + testZoneName, Content: "100.64.0.7", Proxied: proxied, TTL: 1})
		st, err := dnsFor(t, z).Inspect(context.Background(), directTarget("100.64.0.7"))
		if err != nil {
			t.Fatalf("Inspect (proxied=%v): %v", proxied, err)
		}
		if !st.Matches {
			t.Errorf("a direct record with the right value should match regardless of proxying (proxied=%v)", proxied)
		}
	}
}

func TestDNS_Inspect_WrongValueIsAnUpdateAndSaysSo(t *testing.T) {
	z := newZone(dnsRecord{Type: "A", Name: "argosy." + testZoneName, Content: "10.0.0.1", Proxied: false, TTL: 1})
	st, err := dnsFor(t, z).Inspect(context.Background(), directTarget("100.64.0.7"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !st.Exists || st.Matches {
		t.Fatalf("a record with the wrong value exists and does not match, got %+v", st)
	}
	if !strings.Contains(st.Detail, "10.0.0.1") || !strings.Contains(st.Detail, "100.64.0.7") {
		t.Errorf("an update's detail should show what is there and what is wanted, got %q", st.Detail)
	}
}

// A lookup that fails is `unknown`, never absent: the orchestrator declines to
// act on unknown, and the difference between the two is a duplicate record.
func TestDNS_Inspect_LookupFailureIsNotAbsence(t *testing.T) {
	// No serveZone here, deliberately: this models everything being down, the
	// pre-flight's zone read included. That is the case the pre-flight is
	// required to wave through rather than refuse — a zone that could not be
	// read is not evidence about the hostname — and the step must still end up
	// `unknown` off the record lookup below.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cfFail(w, http.StatusInternalServerError, 10000, "internal error")
	}))
	defer srv.Close()

	p := newDNSWithBase(t, srv.URL, DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
	st, err := p.Inspect(context.Background(), directTarget("100.64.0.7"))
	if err == nil {
		t.Fatalf("a failed lookup must be an error, got state %+v", st)
	}
	if st.Exists {
		t.Error("a failed lookup must not report state at all")
	}
}

// If the name filter is ever ignored, the first page of a large zone would read
// as "nothing here" and Ensure would create a duplicate. A full page is not read.
//
// Named for what it is rather than "refused", which since PRSR-31 is a *status*
// meaning something this one deliberately is not: here the answer genuinely was
// not read, so re-running is the fix and the step is `unknown`. See
// TestSpinup_AFullPageIsUnknownNotRefused, which pins that at the orchestrator.
func TestDNS_Inspect_FullPageIsNotReadAsAnAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveZone(w, r) {
			return
		}
		out := make([]dnsRecord, perPage)
		for i := range out {
			out[i] = dnsRecord{ID: fmt.Sprint(i), Type: "A", Name: fmt.Sprintf("other%d.%s", i, testZoneName), Content: "10.0.0.1"}
		}
		cfOK(w, out)
	}))
	defer srv.Close()

	p := newDNSWithBase(t, srv.URL, DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
	_, err := p.Inspect(context.Background(), directTarget("100.64.0.7"))
	if err == nil || !strings.Contains(err.Error(), "full page") {
		t.Fatalf("a full page means the filter did not narrow — want a refusal, got %v", err)
	}
	if spinup.IsRefused(err) {
		t.Error("this is an answer that was not read, not a state somebody must go and fix — it must stay unknown")
	}
}

// The server-side filter narrows; the exact match here decides. A loose filter
// must not let a different hostname's record be adopted or updated.
func TestDNS_Inspect_IgnoresRecordsForOtherNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveZone(w, r) {
			return
		}
		cfOK(w, []dnsRecord{{ID: "x", Type: "A", Name: "argosy-staging." + testZoneName, Content: "100.64.0.7"}})
	}))
	defer srv.Close()

	p := newDNSWithBase(t, srv.URL, DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
	st, err := p.Inspect(context.Background(), directTarget("100.64.0.7"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if st.Exists {
		t.Errorf("a record for a different hostname is not this hostname's, got %+v", st)
	}
}

// Several records at the name and none of them the spec's: refusing is the
// point. The orchestrator turns this into `unknown` and does not act.
func TestDNS_Inspect_AmbiguousNameRefusesToGuess(t *testing.T) {
	host := "argosy." + testZoneName
	z := newZone(
		dnsRecord{Type: "A", Name: host, Content: "10.0.0.1"},
		dnsRecord{Type: "A", Name: host, Content: "10.0.0.2"},
	)
	_, err := dnsFor(t, z).Inspect(context.Background(), directTarget("100.64.0.7"))
	if err == nil {
		t.Fatal("two candidate records must not be resolved by guessing")
	}
	if !strings.Contains(err.Error(), "10.0.0.1") || !strings.Contains(err.Error(), "10.0.0.2") {
		t.Errorf("the refusal should name what it found, got %v", err)
	}
}

// A dual-stack hostname has an A and an AAAA. That is not ambiguity — the spec
// claims one record, not exclusive ownership of the name.
func TestDNS_Inspect_DualStackIsNotAmbiguous(t *testing.T) {
	host := "argosy." + testZoneName
	z := newZone(
		dnsRecord{Type: "A", Name: host, Content: "100.64.0.7"},
		dnsRecord{Type: "AAAA", Name: host, Content: "2001:db8::1"},
	)
	st, err := dnsFor(t, z).Inspect(context.Background(), directTarget("100.64.0.7"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !st.Matches {
		t.Errorf("the A record satisfies the spec; the AAAA alongside it is not drift, got %+v", st)
	}
}

// --- Ensure ----------------------------------------------------------------

func TestDNS_Ensure_CreatesTheProxiedTunnelCNAME(t *testing.T) {
	var got recordBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveZone(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			cfOK(w, []dnsRecord{})
		case http.MethodPost:
			decodeBody(t, r, &got)
			cfOK(w, dnsRecord{ID: "new", Type: got.Type, Name: got.Name, Content: got.Content, Proxied: got.Proxied, ZoneID: testZoneID, ZoneName: testZoneName})
		}
	}))
	defer srv.Close()

	p := newDNSWithBase(t, srv.URL, DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
	if _, err := p.Ensure(context.Background(), tunnelledTarget()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	want := recordBody{
		Type: "CNAME", Name: "interlock." + testZoneName,
		Content: testTunnelID + tunnelSuffix, Proxied: true, TTL: ttlAuto,
	}
	if got != want {
		t.Errorf("tunnelled create body:\n got %+v\nwant %+v", got, want)
	}
}

func TestDNS_Ensure_RecordTypeFollowsTheUpstreamShape(t *testing.T) {
	cases := []struct{ upstream, wantType string }{
		{"100.64.0.7", "A"},
		{"2001:db8::1", "AAAA"},
		{"origin.example.net", "CNAME"},
	}
	for _, tc := range cases {
		t.Run(tc.upstream, func(t *testing.T) {
			z := newZone()
			p := dnsFor(t, z)
			res, err := p.Ensure(context.Background(), directTarget(tc.upstream))
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if !strings.Contains(res.Detail, tc.wantType+" → "+tc.upstream) {
				t.Errorf("want a %s record for %q, got detail %q", tc.wantType, tc.upstream, res.Detail)
			}
			// Direct records are created DNS-only: proxying a direct endpoint
			// changes how the service is reached, and the spec did not ask for it.
			if !strings.HasPrefix(res.Detail, "DNS only") {
				t.Errorf("a direct record should be created unproxied, got %q", res.Detail)
			}
		})
	}
}

func TestDNS_Ensure_AlreadyCorrectWritesNothing(t *testing.T) {
	z := newZone(dnsRecord{
		Type: "CNAME", Name: "interlock." + testZoneName,
		Content: testTunnelID + tunnelSuffix, Proxied: true, TTL: 1,
	})
	p := dnsFor(t, z)
	res, err := p.Ensure(context.Background(), tunnelledTarget())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if z.writes() != 0 {
		t.Errorf("an already-correct record must be success and no write, made %d", z.writes())
	}
	if res.ExternalID == "" {
		t.Error("the existing record's id must still come back")
	}
}

// An update on the direct path is about the record's value. It must not also
// switch the orange cloud off on a service that is running behind it.
func TestDNS_Ensure_DirectUpdateLeavesProxyingAlone(t *testing.T) {
	var patched recordBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveZone(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			cfOK(w, []dnsRecord{{ID: "rec1", Type: "A", Name: "argosy." + testZoneName, Content: "10.0.0.1", Proxied: true}})
		case http.MethodPatch:
			decodeBody(t, r, &patched)
			cfOK(w, dnsRecord{ID: "rec1", Type: patched.Type, Name: patched.Name, Content: patched.Content, Proxied: patched.Proxied})
		default:
			t.Errorf("unexpected %s", r.Method)
		}
	}))
	defer srv.Close()

	p := newDNSWithBase(t, srv.URL, DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
	if _, err := p.Ensure(context.Background(), directTarget("100.64.0.7")); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if patched.Content != "100.64.0.7" {
		t.Errorf("the value should be updated, got %q", patched.Content)
	}
	if !patched.Proxied {
		t.Error("a direct spec says nothing about proxying — the update must not turn the orange cloud off")
	}
}

// The tunnelled path does have an opinion, and an update must apply it.
func TestDNS_Ensure_TunnelledUpdateTurnsProxyingOn(t *testing.T) {
	z := newZone(dnsRecord{
		Type: "CNAME", Name: "interlock." + testZoneName,
		Content: testTunnelID + tunnelSuffix, Proxied: false, TTL: 1,
	})
	p := dnsFor(t, z)
	res, err := p.Ensure(context.Background(), tunnelledTarget())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !strings.HasPrefix(res.Detail, "proxied") {
		t.Errorf("the tunnelled record must end up proxied, got %q", res.Detail)
	}
	if z.countCalls(http.MethodPatch) != 1 {
		t.Errorf("want one update, got %d", z.countCalls(http.MethodPatch))
	}
}

// One record of the wrong type at the name is a type change of this hostname's
// record, not an ambiguity.
func TestDNS_Ensure_ChangesTheTypeOfALoneRecord(t *testing.T) {
	z := newZone(dnsRecord{Type: "A", Name: "interlock." + testZoneName, Content: "10.0.0.1", TTL: 1})
	p := dnsFor(t, z)
	res, err := p.Ensure(context.Background(), tunnelledTarget())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !strings.Contains(res.Detail, "CNAME") || !strings.Contains(res.Detail, tunnelSuffix) {
		t.Errorf("the A record should have become the tunnel CNAME, got %q", res.Detail)
	}
	if z.countCalls(http.MethodPost) != 0 {
		t.Error("changing a record's type is an update, not a second record")
	}
}

// A hostname outside the zone is silently rewritten by Cloudflare into
// <name>.<zone>. Purser refuses it and takes the stray record back out: nothing
// records a failed step, so leaving it would put a live record in the zone that
// nothing knows about.
//
// Since PRSR-39 this is the *backstop*, reached only when the pre-flight could
// not answer — hence the unreadable zone, which is what makes the test still
// about the thing it is named for rather than about the new check. The zone
// being unreadable is exactly the state in which the pre-flight waves a hostname
// through, so this is not a contrived fixture: it is the one live case where
// create-then-delete is the only guard there is.
func TestDNS_Ensure_OutOfZoneHostnameIsRefusedAndCleanedUp(t *testing.T) {
	z := newZone()
	z.zoneUnreadable = true
	p := dnsFor(t, z)
	target := directTarget("100.64.0.7")
	target.Spec.Hostname = "argosy.example.org"

	_, err := p.Ensure(context.Background(), target)
	if err == nil {
		t.Fatal("a hostname outside the zone must not silently become a record in it")
	}
	if !strings.Contains(err.Error(), "argosy.example.org."+testZoneName) {
		t.Errorf("the error should show what Cloudflare actually wrote, got %v", err)
	}
	if left := z.recordCount(); left != 0 {
		t.Errorf("the stray record should have been removed, %d left", left)
	}
	if !strings.Contains(err.Error(), "removed") {
		t.Errorf("the error should say the stray was cleaned up, got %v", err)
	}
}

// "Already exists" upstream is success, not a conflict — the house rule, applied
// to the window between the lookup and the create.
func TestDNS_Ensure_CreateConflictAcceptsAMatchingRecord(t *testing.T) {
	var lists int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveZone(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			lists++
			if lists == 1 {
				cfOK(w, []dnsRecord{}) // nothing there yet
				return
			}
			cfOK(w, []dnsRecord{{ID: "raced", Type: "A", Name: "argosy." + testZoneName, Content: "100.64.0.7", ZoneID: testZoneID}})
		case http.MethodPost:
			cfFail(w, http.StatusBadRequest, 81057, "Record already exists.")
		default:
			t.Errorf("unexpected %s", r.Method)
		}
	}))
	defer srv.Close()

	p := newDNSWithBase(t, srv.URL, DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
	res, err := p.Ensure(context.Background(), directTarget("100.64.0.7"))
	if err != nil {
		t.Fatalf("a record that appeared under us and matches the spec is success, got %v", err)
	}
	if res.ExternalID != "raced" {
		t.Errorf("want the racing record's id, got %q", res.ExternalID)
	}
}

// …but only when it matches. Anything else is still the error Cloudflare gave.
func TestDNS_Ensure_CreateConflictOnAMismatchStaysAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveZone(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			cfOK(w, []dnsRecord{})
		case http.MethodPost:
			cfFail(w, http.StatusBadRequest, 1004, "DNS Validation Error")
		}
	}))
	defer srv.Close()

	p := newDNSWithBase(t, srv.URL, DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
	if _, err := p.Ensure(context.Background(), directTarget("100.64.0.7")); err == nil {
		t.Fatal("a create that failed for a real reason must surface")
	}
}

// --- Teardown --------------------------------------------------------------

func TestDNS_Teardown_RefusesWithoutARecordedID(t *testing.T) {
	z := newZone(dnsRecord{Type: "A", Name: "argosy." + testZoneName, Content: "100.64.0.7"})
	p := dnsFor(t, z)
	_, err := p.Teardown(context.Background(), directTarget("100.64.0.7"), model.ServiceResource{
		Hostname: "argosy." + testZoneName, Kind: model.ResourceDNSRecord,
	})
	// Asserted on the reason, not merely on "an error": without the guard the
	// call still fails, but by way of a malformed request — and a refusal that
	// only works because the URL came out wrong is one a later refactor removes
	// without noticing.
	if err == nil || !strings.Contains(err.Error(), "deletes only ids it recorded") {
		t.Fatalf("want a refusal naming the reason, got %v", err)
	}
	if z.writes() != 0 {
		t.Error("it must not fall back to deleting whatever the name finds")
	}
}

// The Removal describes what went, which on the teardown path is the whole of
// what an operator has to check the command against — there is no plan-time
// upstream read to describe.
func TestDNS_Teardown_SaysWhatItRemoved(t *testing.T) {
	z := newZone(dnsRecord{ID: "rec1", Type: "CNAME", Name: "argosy." + testZoneName, Content: "origin.example.com"})
	p := dnsFor(t, z)
	rm, err := p.Teardown(context.Background(), directTarget("100.64.0.7"), model.ServiceResource{
		Hostname: "argosy." + testZoneName, Kind: model.ResourceDNSRecord,
		ExternalID: "rec1", ParentID: testZoneID,
	})
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	for _, want := range []string{"CNAME", "argosy." + testZoneName, "rec1"} {
		if !strings.Contains(rm.Detail, want) {
			t.Errorf("detail %q does not name %q", rm.Detail, want)
		}
	}

	// A record already gone is a success, and says which kind of success it is.
	rm, err = p.Teardown(context.Background(), directTarget("100.64.0.7"), model.ServiceResource{
		Hostname: "argosy." + testZoneName, Kind: model.ResourceDNSRecord,
		ExternalID: "rec1", ParentID: testZoneID,
	})
	if err != nil {
		t.Fatalf("second Teardown: %v", err)
	}
	if !strings.Contains(rm.Detail, "already gone") {
		t.Errorf("detail %q should distinguish an absent record from one this run deleted", rm.Detail)
	}
}

// The id outlived what it pointed at. Deleting whatever it names now would
// remove a record for a hostname nobody asked about.
func TestDNS_Teardown_RefusesWhenTheIDNamesAnotherHostname(t *testing.T) {
	z := newZone(dnsRecord{ID: "rec1", Type: "A", Name: "someone-else." + testZoneName, Content: "10.0.0.1"})
	p := dnsFor(t, z)
	_, err := p.Teardown(context.Background(), directTarget("100.64.0.7"), model.ServiceResource{
		Hostname: "argosy." + testZoneName, Kind: model.ResourceDNSRecord,
		ExternalID: "rec1", ParentID: testZoneID,
	})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if z.countCalls(http.MethodDelete) != 0 {
		t.Error("nothing should have been deleted")
	}
}

// The recorded parent, not today's config: migration 0007 records it precisely
// so a teardown edits where the resource actually went.
func TestDNS_Teardown_UsesTheRecordedZone(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			cfOK(w, dnsRecord{ID: "rec1", Type: "A", Name: "argosy." + testZoneName, Content: "100.64.0.7"})
		case http.MethodDelete:
			cfOK(w, map[string]string{"id": "rec1"})
		}
	}))
	defer srv.Close()

	p := newDNSWithBase(t, srv.URL, DNSConfig{APIToken: "cf_token", ZoneID: "todays-zone"})
	_, err := p.Teardown(context.Background(), directTarget("100.64.0.7"), model.ServiceResource{
		Hostname: "argosy." + testZoneName, Kind: model.ResourceDNSRecord,
		ExternalID: "rec1", ParentID: "the-zone-it-went-into",
	})
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	for _, path := range paths {
		if !strings.Contains(path, "the-zone-it-went-into") {
			t.Errorf("teardown should target the recorded zone, called %s", path)
		}
	}
}

// --- unconfigured ----------------------------------------------------------

// Unconfigured is `unavailable`, never a breakage and never "nothing there":
// the plan shows the DNS step as unavailable rather than promising a record it
// cannot write.
func TestDNS_Unconfigured_IsUnavailableOnEveryPath(t *testing.T) {
	ctx := context.Background()
	target := directTarget("100.64.0.7")
	for _, tc := range []struct {
		name string
		cfg  DNSConfig
	}{
		{"no token", DNSConfig{ZoneID: testZoneID}},
		{"no zone", DNSConfig{APIToken: "cf_token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewDNS(tc.cfg)
			if _, err := p.Inspect(ctx, target); !spinup.IsUnavailable(err) {
				t.Errorf("Inspect: want ErrUnavailable, got %v", err)
			}
			if _, err := p.Ensure(ctx, target); !spinup.IsUnavailable(err) {
				t.Errorf("Ensure: want ErrUnavailable, got %v", err)
			}
			if _, err := p.Teardown(ctx, target, model.ServiceResource{ExternalID: "x"}); !spinup.IsUnavailable(err) {
				t.Errorf("Teardown: want ErrUnavailable, got %v", err)
			}
		})
	}
}

// --- guards ----------------------------------------------------------------

// The orchestrator resolves the tunnel ref once, before any step, so the ingress
// route and this record cannot name different tunnels. A provisioner reached
// without that resolution must refuse rather than guess.
func TestDNS_TunnelledWithoutAResolvedTunnelIDRefuses(t *testing.T) {
	target := tunnelledTarget()
	target.TunnelID = ""
	z := newZone()
	if _, err := dnsFor(t, z).Inspect(context.Background(), target); err == nil {
		t.Fatal("want a refusal when the tunnel id is unresolved")
	}
	if calls := z.callLog(); len(calls) != 0 {
		t.Errorf("it should refuse before calling Cloudflare at all, got %v", calls)
	}
}

func TestDNS_InvalidSpecIsRefusedBeforeAnyCall(t *testing.T) {
	z := newZone()
	target := directTarget("100.64.0.7")
	target.Spec.Hostname = "not a hostname"
	if _, err := dnsFor(t, z).Ensure(context.Background(), target); err == nil {
		t.Fatal("want the spec's own refusal")
	}
	if calls := z.callLog(); len(calls) != 0 {
		t.Errorf("nothing should have been called, got %v", calls)
	}
}

// dnsRecordNotFound has to answer from structure, not prose: Teardown reads it
// to decide
// that an already-absent record is a success, which is a claim about the world.
//
// The bare-404 cases are the point. A 404 is also what the API answers when the
// *request* could not be routed, so treating it as conclusive would have a
// teardown that never reached the zone report a deletion — silent, and not
// fixed by re-running, because the next run reads the row as already removed.
func TestDNSRecordNotFound_RequiresTheRecordCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"record code", &apiError{Status: http.StatusNotFound, Code: errCodeRecordNotFound}, true},
		{"record code without a 404", &apiError{Status: http.StatusBadRequest, Code: errCodeRecordNotFound}, true},
		{"wrapped", fmt.Errorf("wrapped: %w", &apiError{Code: errCodeRecordNotFound}), true},
		{"bare 404 — could be an unroutable path", &apiError{Status: http.StatusNotFound}, false},
		{"404 from a routing error", &apiError{Status: http.StatusNotFound, Code: 7003, Message: "Could not route"}, false},
		{"404 with a non-envelope body", &apiError{Status: http.StatusNotFound, Body: "404 page not found"}, false},
		{"other api error", &apiError{Status: http.StatusForbidden, Code: 10000}, false},
		{"not an api error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := dnsRecordNotFound(tc.err); got != tc.want {
			t.Errorf("%s: dnsRecordNotFound = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A direct spec pins the record's value. An update about the value must not also
// rewrite the TTL a human set — and nothing in the plan the operator approved
// would have mentioned it, since neither recordMatches nor describeRecord looks
// at TTL.
func TestDNS_Ensure_DirectUpdateLeavesTTLAlone(t *testing.T) {
	var patched recordBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveZone(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			cfOK(w, []dnsRecord{{ID: "rec1", Type: "A", Name: "argosy." + testZoneName, Content: "10.0.0.1", TTL: 300}})
		case http.MethodPatch:
			decodeBody(t, r, &patched)
			cfOK(w, dnsRecord{ID: "rec1", Type: patched.Type, Name: patched.Name, Content: patched.Content, TTL: patched.TTL})
		default:
			t.Errorf("unexpected %s", r.Method)
		}
	}))
	defer srv.Close()

	p := newDNSWithBase(t, srv.URL, DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
	if _, err := p.Ensure(context.Background(), directTarget("100.64.0.7")); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if patched.TTL != 300 {
		t.Errorf("the existing TTL should be carried across, got %d", patched.TTL)
	}
}

// The tunnelled path does pin the TTL: a proxied record must be on automatic.
func TestDNS_Ensure_TunnelledUpdateSetsAutomaticTTL(t *testing.T) {
	var patched recordBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveZone(w, r) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			cfOK(w, []dnsRecord{{ID: "rec1", Type: "CNAME", Name: "interlock." + testZoneName, Content: "old" + tunnelSuffix, TTL: 300}})
		case http.MethodPatch:
			decodeBody(t, r, &patched)
			cfOK(w, dnsRecord{ID: "rec1", Type: patched.Type, Name: patched.Name, Content: patched.Content, Proxied: patched.Proxied, TTL: patched.TTL})
		}
	}))
	defer srv.Close()

	p := newDNSWithBase(t, srv.URL, DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
	if _, err := p.Ensure(context.Background(), tunnelledTarget()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if patched.TTL != ttlAuto || !patched.Proxied {
		t.Errorf("a proxied record must be on automatic TTL, got ttl=%d proxied=%v", patched.TTL, patched.Proxied)
	}
}

// A 404 that is about the request rather than about the record must not read as
// "already gone" — Teardown would report a deletion it never performed, and the
// row would be marked removed for a record that is still live.
func TestDNS_Teardown_UnroutableRequestIsNotAbsence(t *testing.T) {
	var deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
		}
		cfFail(w, http.StatusNotFound, 7003, "Could not route to "+r.URL.Path)
	}))
	defer srv.Close()

	p := newDNSWithBase(t, srv.URL, DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
	_, err := p.Teardown(context.Background(), directTarget("100.64.0.7"), model.ServiceResource{
		Hostname: "argosy." + testZoneName, Kind: model.ResourceDNSRecord,
		ExternalID: "rec1", ParentID: "a-zone-this-token-cannot-address",
	})
	if err == nil {
		t.Fatal("a teardown that could not reach the zone must not report success")
	}
	if deletes != 0 {
		t.Error("it should not have gone on to delete anything")
	}
}

// A record as the live API actually returns it carries no zone fields, and the
// code paths that read them fall back correctly.
//
// PRSR-42 measured all three routes this package reads records from — a create,
// a get-by-id, and the **list** that records() uses on every Inspect. None
// populates `zone_id` or `zone_name`. The list matters most and was the one
// review caught me generalising past: it is what every plan and every --apply
// calls, so it is the response resource() and zoneOf() actually see.
//
// dnsRecord decodes both fields, so both are always empty in production, and two
// call sites read them:
//
//   - zoneOf, whose first operand is therefore never taken. The stray-removal
//     path depends on the fallback, which is why it works at all.
//   - wrongName, whose zone-naming branch can never fire, so the "configured
//     zone" wording is the only text an operator ever sees.
//
// Neither is a bug — both were written with the fallback — but the fake used to
// supply both fields under a comment calling them "the zone fields the API
// echoes back", which made the suite exercise branches Cloudflare cannot reach
// and left the first operand of zoneOf looking load-bearing.
func TestDNS_RealResponsesCarryNoZoneFields(t *testing.T) {
	// Exactly the key set observed on a live create response (PRSR-42).
	const liveCreate = `{"result":{
		"id":"983cfa92ab76da97243529d69b44b56f",
		"type":"A",
		"name":"prsr42-probe.zerogravity.industries",
		"content":"192.0.2.10",
		"proxied":false,
		"ttl":1,
		"created_on":"2026-08-28T04:33:31.187278Z",
		"modified_on":"2026-08-28T04:33:31.187278Z",
		"comment":null,"tags":[],"meta":{},"proxiable":true,"settings":{}
	},"success":true,"errors":[],"messages":[]}`

	// And the key set observed on a live LIST response — the route records()
	// uses. Note `comment_modified_on`, which the create response does not carry:
	// the two routes are not the same shape, which is the reason to pin both.
	const liveList = `{"result":[{
		"id":"c1a0b1e2d3f4a5b6c7d8e9f0a1b2c3d4",
		"type":"CNAME",
		"name":"lyceum.zerogravity.industries",
		"content":"aef21667-03ce-45d3-b83c-d634822661cd.cfargotunnel.com",
		"proxied":true,
		"ttl":1,
		"created_on":"2026-07-20T15:03:32Z",
		"modified_on":"2026-07-20T15:03:32Z",
		"comment":null,"comment_modified_on":null,
		"tags":[],"meta":{},"proxiable":true,"settings":{}
	}],"success":true,"errors":[],"messages":[]}`

	var env struct {
		Result dnsRecord `json:"result"`
	}
	if err := json.Unmarshal([]byte(liveCreate), &env); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	got := env.Result

	if got.ZoneID != "" || got.ZoneName != "" {
		t.Fatalf("the live create sends neither zone field; fixture drift: zone_id=%q zone_name=%q", got.ZoneID, got.ZoneName)
	}

	var listEnv struct {
		Result []dnsRecord `json:"result"`
	}
	if err := json.Unmarshal([]byte(liveList), &listEnv); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listEnv.Result) != 1 {
		t.Fatalf("list decoded %d records", len(listEnv.Result))
	}
	if l := listEnv.Result[0]; l.ZoneID != "" || l.ZoneName != "" {
		t.Errorf("the live list sends neither zone field either; this is the route every Inspect reads: zone_id=%q zone_name=%q", l.ZoneID, l.ZoneName)
	}

	// zoneOf must therefore reach the configured zone, which is what makes
	// removeStray able to delete what it just created.
	p := NewDNS(DNSConfig{APIToken: "cf_token", ZoneID: testZoneID})
	if z := p.zoneOf(got); z != testZoneID {
		t.Errorf("zoneOf = %q, want the configured zone %q — the stray-removal path depends on this fallback", z, testZoneID)
	}

	// wrongName's message uses the fallback wording, since it has no zone name
	// to use. Asserted so the branch's unreachability is visible rather than
	// something a reader has to work out.
	err := wrongName(dnsRecord{Name: "svc.example.org.zerogravity.industries"}, dnsRecord{Name: "svc.example.org"})
	if err == nil {
		t.Fatal("a zone-appended name must be refused")
	}
	if !strings.Contains(err.Error(), "the configured zone") {
		t.Errorf("with no zone_name to name, the fallback wording is what an operator sees, got %v", err)
	}

	// And the fake must not supply what the API withholds. Asserted through
	// fakeZone rather than only against the literal above, because the literal
	// pins what Cloudflare sends while this pins what the *suite* sends — and it
	// was the second one that drifted.
	z := newZone()
	stored := z.put(dnsRecord{Type: "A", Name: "argosy." + testZoneName, Content: "10.0.0.1"})
	if stored.ZoneID != "" || stored.ZoneName != "" {
		t.Errorf("fakeZone fabricates zone fields the live API does not send (zone_id=%q zone_name=%q), which would exercise branches Cloudflare cannot reach",
			stored.ZoneID, stored.ZoneName)
	}
}

// --- the zone pre-flight (PRSR-39) -----------------------------------------

// The headline: a hostname in another domain is refused in the *plan*, and the
// zone is never even searched for it.
//
// Before this, Inspect reported `create` for such a hostname — a plan promising
// a record — and only --apply found out, by making one and deleting it again.
func TestDNS_Preflight_OutOfZoneHostnameIsRefusedByInspect(t *testing.T) {
	z := newZone()
	target := directTarget("100.64.0.7")
	target.Spec.Hostname = "argosy.example.org"

	st, err := dnsFor(t, z).Inspect(context.Background(), target)
	if err == nil {
		t.Fatalf("a hostname outside the zone must not preview as a record to create, got %+v", st)
	}
	if !spinup.IsRefused(err) {
		// unknown says "re-run", and re-running says this for ever; unavailable
		// says "set an env var", and they are all set. Only refused names a fix
		// that is actually the fix.
		t.Errorf("the zone read succeeded, so this is refused rather than unknown: %v", err)
	}
	for _, want := range []string{"argosy.example.org", testZoneName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %q, got %v", want, err)
		}
	}
	// The whole point of doing it up front: nothing was looked up, so there is
	// no window in which a wrong record exists and no lookup to misread.
	for _, c := range z.callLog() {
		if strings.Contains(c, "dns_records") {
			t.Errorf("an out-of-zone hostname must be refused before the zone is searched, got %v", z.callLog())
			break
		}
	}
}

// And the apply refuses on its own account rather than trusting the plan, which
// is the same rule Ensure already follows by re-running the record lookup.
func TestDNS_Preflight_OutOfZoneHostnameIsRefusedByEnsureWithoutWriting(t *testing.T) {
	z := newZone()
	target := directTarget("100.64.0.7")
	target.Spec.Hostname = "argosy.example.org"

	_, err := dnsFor(t, z).Ensure(context.Background(), target)
	if !spinup.IsRefused(err) {
		t.Fatalf("want a refusal, got %v", err)
	}
	if z.writes() != 0 {
		t.Errorf("nothing may be written for a hostname the zone cannot hold: %v", z.callLog())
	}
	if n := z.recordCount(); n != 0 {
		t.Errorf("no record should exist to clean up, got %d", n)
	}
	// The line PRSR-39 exists to stop printing.
	if strings.Contains(err.Error(), "removed") {
		t.Errorf("nothing was created, so nothing should be reported as removed: %v", err)
	}
}

// The apex is in its own zone. A spec may legitimately claim it, and refusing
// there would be the pre-flight blocking a hostname Cloudflare handles fine.
func TestDNS_Preflight_TheApexIsInTheZone(t *testing.T) {
	z := newZone()
	target := directTarget("100.64.0.7")
	target.Spec.Hostname = testZoneName

	if _, err := dnsFor(t, z).Inspect(context.Background(), target); err != nil {
		t.Fatalf("the zone apex is inside the zone: %v", err)
	}
}

// A zone id is fixed for a deployment, so the name behind it is read once and
// remembered — `purser serve` must not ask Cloudflare on every request.
func TestDNS_Preflight_TheZoneIsReadOnce(t *testing.T) {
	z := newZone(dnsRecord{Type: "A", Name: "argosy." + testZoneName, Content: "100.64.0.7"})
	p := dnsFor(t, z)
	ctx := context.Background()
	for range 3 {
		if _, err := p.Inspect(ctx, directTarget("100.64.0.7")); err != nil {
			t.Fatalf("Inspect: %v", err)
		}
	}
	reads := 0
	for _, c := range z.callLog() {
		if c == "GET /zones/"+testZoneID {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("the zone name cannot change under a fixed zone id; want 1 read, got %d: %v", reads, z.callLog())
	}
}

// Never treat unverifiable as absent, here too: a zone that could not be read is
// not evidence the hostname is wrong, so the run proceeds exactly as it did
// before the pre-flight existed.
func TestDNS_Preflight_AnUnreadableZoneDoesNotRefuse(t *testing.T) {
	z := newZone(dnsRecord{Type: "A", Name: "argosy." + testZoneName, Content: "100.64.0.7"})
	z.zoneUnreadable = true

	st, err := dnsFor(t, z).Inspect(context.Background(), directTarget("100.64.0.7"))
	if err != nil {
		t.Fatalf("a failed zone read must not fail the step: %v", err)
	}
	if !st.Matches {
		t.Errorf("the record is correct and the zone read is irrelevant to that, got %+v", st)
	}
}

// …and a failed read is not remembered as an answer. Caching it would disable
// the pre-flight for the life of the process on the strength of one timeout,
// which for `purser serve` means until somebody restarts it.
func TestDNS_Preflight_AFailedZoneReadIsNotCached(t *testing.T) {
	z := newZone()
	z.zoneUnreadable = true
	p := dnsFor(t, z)
	target := directTarget("100.64.0.7")
	target.Spec.Hostname = "argosy.example.org"
	ctx := context.Background()

	if _, err := p.Inspect(ctx, target); spinup.IsRefused(err) {
		t.Fatalf("with the zone unreadable there is nothing to refuse on: %v", err)
	}
	z.mu.Lock()
	z.zoneUnreadable = false
	z.mu.Unlock()

	if _, err := p.Inspect(ctx, target); !spinup.IsRefused(err) {
		t.Errorf("once the zone can be read the pre-flight must work, got %v", err)
	}
}

// A 200 that named no zone is a read that did not answer, not a zone called ""
// — which inZone's suffix test would otherwise match every hostname against.
func TestDNS_Preflight_AZoneWithNoNameIsNotAnAnswer(t *testing.T) {
	z := newZone()
	z.zoneName = ""
	target := directTarget("100.64.0.7")
	target.Spec.Hostname = "argosy.example.org"

	if _, err := dnsFor(t, z).Inspect(context.Background(), target); spinup.IsRefused(err) {
		t.Fatalf("a nameless zone is not an answer to refuse on: %v", err)
	}
}

// The backstop still earns its place with the pre-flight in front of it, because
// the two answer different questions: the pre-flight asks what the spec said,
// wrongName asks what Cloudflare did. A hostname that IS in the zone and comes
// back written under another name is caught only by the second.
func TestDNS_Ensure_ANormalisationSurpriseIsStillCaught(t *testing.T) {
	z := newZone()
	z.nameOnCreate = func(string) string { return "somethingelse." + testZoneName }
	target := directTarget("100.64.0.7") // argosy.zerogravity.industries — in zone

	_, err := dnsFor(t, z).Ensure(context.Background(), target)
	if err == nil {
		t.Fatal("a record written under a name nobody asked for must not be accepted")
	}
	if !strings.Contains(err.Error(), "somethingelse."+testZoneName) {
		t.Errorf("the error should show what Cloudflare actually wrote, got %v", err)
	}
	if left := z.recordCount(); left != 0 {
		t.Errorf("the stray record should have been removed, %d left", left)
	}
}

// inZone mirrors what Cloudflare treats as inside the zone, and the row that
// matters is the third: a suffix test that drops the dot would read
// "notzerogravity.industries" as being inside "zerogravity.industries" and let a
// hostname on somebody else's domain straight through the check that exists to
// stop exactly that.
func TestInZone(t *testing.T) {
	const zone = "zerogravity.industries"
	for _, tc := range []struct {
		host string
		want bool
		why  string
	}{
		{"argosy.zerogravity.industries", true, "the ordinary case"},
		{"zerogravity.industries", true, "the apex is in its own zone"},
		{"a.b.zerogravity.industries", true, "depth is not the question"},
		{"ARGOSY.ZeroGravity.Industries", true, "hostnames are case-insensitive"},
		{"argosy.zerogravity.industries.", true, "a trailing dot is the same name"},
		{"notzerogravity.industries", false, "a suffix that is not a label boundary"},
		{"argosy.example.org", false, "another domain entirely"},
		{"zerogravity.industries.example.org", false, "the zone as a prefix is not the zone"},
		{"industries", false, "a parent of the zone is not inside it"},
	} {
		if got := inZone(tc.host, zone); got != tc.want {
			t.Errorf("inZone(%q, %q) = %v, want %v — %s", tc.host, zone, got, tc.want, tc.why)
		}
	}
	// Whatever the caller passed, an empty zone matches nothing: zone() reports
	// a nameless response as a failed read rather than handing one here, and
	// this is the second half of that guarantee.
	if inZone("argosy.example.org", "") {
		t.Error(`inZone(_, "") must not match — an unknown zone is not a zone everything is in`)
	}
}
