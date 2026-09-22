package catenary

// The pinned contract is checked, not trusted. testdata/provision-v1.openapi.yaml
// is vendored byte-for-byte from the catenary repository's `provision-v1` tag
// (see testdata/README.md for the blob hash), and this file validates every
// request the connector actually sends, and every response fakeCatenary
// actually emits, against that document — using kin-openapi, the same parser
// version (v0.135.0) the pinned contract's own header comment says Catenary's
// CI validates it with.
//
// kin-openapi is a new dependency for Purser (go.mod had no OpenAPI/YAML
// library). It is confined to _test.go files in this package on purpose: it
// is not needed to build or run `purser`, only to prove this package's double
// hasn't drifted from the contract it stands in for.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/Einlanzerous/purser/internal/connector"
)

func contractPath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("catenary: runtime.Caller failed while locating testdata")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "provision-v1.openapi.yaml")
}

// loadContractDoc parses the vendored, pinned document. Tests that mutate it
// (the control below) call this again for their own private copy rather than
// sharing one — openapi3.T is not obviously safe to mutate concurrently, and
// each caller wants an independent document to perturb.
func loadContractDoc(t *testing.T) *openapi3.T {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(contractPath())
	if err != nil {
		t.Fatalf("load %s: %v", contractPath(), err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("testdata/provision-v1.openapi.yaml does not validate as OpenAPI 3: %v", err)
	}
	return doc
}

func newContractRouter(t *testing.T, doc *openapi3.T) routers.Router {
	t.Helper()
	r, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	return r
}

// checkExchange validates one captured request/response pair against router.
// AuthenticationFunc is a no-op: this checks BODY AND PARAMETER shape against
// the schema, which is the drift this file exists to catch — not bearer-token
// mechanics, which fake_test.go's own 401 handling already covers directly.
func checkExchange(ctx context.Context, router routers.Router, ex *exchange) error {
	return checkExchangeOpts(ctx, router, ex, true)
}

// checkResponseOnly is checkExchange without the request-shape half. It
// exists for the one deliberately-invalid probe this suite sends on purpose —
// a blank `email` query parameter, which the contract itself declares
// `minLength: 1` on, so kin-openapi correctly refuses to call that request
// well-formed. That refusal isn't drift; it's the same reason the real
// service also answers 400 rather than treating it as a lookup. What still
// needs checking is that the 400 body this double sends is itself a real,
// schema-shaped Error.
func checkResponseOnly(ctx context.Context, router routers.Router, ex *exchange) error {
	return checkExchangeOpts(ctx, router, ex, false)
}

func checkExchangeOpts(ctx context.Context, router routers.Router, ex *exchange, validateRequest bool) error {
	route, pathParams, err := router.FindRoute(ex.req)
	if err != nil {
		return &exchangeError{op: "find route for " + ex.req.Method + " " + ex.req.URL.Path, err: err}
	}
	reqInput := &openapi3filter.RequestValidationInput{
		Request:    ex.req,
		PathParams: pathParams,
		Route:      route,
		Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	if validateRequest {
		if err := openapi3filter.ValidateRequest(ctx, reqInput); err != nil {
			return &exchangeError{op: "request " + ex.req.Method + " " + ex.req.URL.Path, err: err}
		}
	}
	respInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 ex.status,
		Header:                 ex.respHeader,
	}
	respInput.SetBodyBytes(ex.respBody)
	if err := openapi3filter.ValidateResponse(ctx, respInput); err != nil {
		return &exchangeError{op: "response", err: err}
	}
	return nil
}

type exchangeError struct {
	op  string
	err error
}

func (e *exchangeError) Error() string { return e.op + ": " + e.err.Error() }
func (e *exchangeError) Unwrap() error { return e.err }

// exchange is one recorded HTTP round trip: a request whose body (if any) has
// already been read into an independent, re-readable buffer, and the
// response actually returned.
type exchange struct {
	req        *http.Request
	status     int
	respHeader http.Header
	respBody   []byte
}

// recordingTransport wraps a RoundTripper and keeps a copy of every exchange
// that passes through it, without disturbing the real request/response that
// the connector and the fake actually see.
type recordingTransport struct {
	mu        sync.Mutex
	exchanges []*exchange
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		reqBody = b
		req.Body = io.NopCloser(bytes.NewReader(b))
	}

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	clone := req.Clone(req.Context())
	if reqBody != nil {
		clone.Body = io.NopCloser(bytes.NewReader(reqBody))
		clone.ContentLength = int64(len(reqBody))
	}

	rt.mu.Lock()
	rt.exchanges = append(rt.exchanges, &exchange{
		req: clone, status: resp.StatusCode, respHeader: resp.Header.Clone(), respBody: respBody,
	})
	rt.mu.Unlock()
	return resp, nil
}

// scriptedFixture wires a Connector to a fake through a recordingTransport, so
// a scenario driven purely through the connector's own methods yields exactly
// the exchanges that method call produced on the wire.
func scriptedFixture(t *testing.T) (*Connector, *fakeCatenary, *recordingTransport) {
	t.Helper()
	fake := newFake(testToken)
	srv := fake.start()
	t.Cleanup(srv.Close)
	rt := &recordingTransport{}
	c, err := New(Config{
		BaseURL: srv.URL, ProvisionToken: testToken,
		HTTPClient: &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, fake, rt
}

// TestContract_ExchangesSatisfySpec drives every operation this connector and
// its double use — through the connector where the scenario is one it would
// really produce, and directly against the fake for shapes the connector's
// own guards mean it never sends (a blank email, an unauthenticated request)
// — and validates every recorded exchange against the pinned document.
func TestContract_ExchangesSatisfySpec(t *testing.T) {
	ctx := context.Background()
	c, fake, rt := scriptedFixture(t)

	// created, then existing (re-invite), then reactivated, then with a note.
	if _, err := c.Provision(ctx, connector.Input{PersonName: "Nadia Ruiz", Email: "nadia@example.com"}); err != nil {
		t.Fatalf("Provision (create): %v", err)
	}
	if _, err := c.Provision(ctx, connector.Input{Email: "nadia@example.com"}); err != nil {
		t.Fatalf("Provision (existing): %v", err)
	}
	if a := fake.account("nadia@example.com"); a != nil {
		a.Status = "deactivated"
	}
	if _, err := c.Provision(ctx, connector.Input{Email: "nadia@example.com"}); err != nil {
		t.Fatalf("Provision (reactivated): %v", err)
	}
	fake.occupyHandle("olga")
	if _, err := c.Provision(ctx, connector.Input{Email: "olga@example.com"}); err != nil {
		t.Fatalf("Provision (note): %v", err)
	}

	// Reconcile: active, deactivated (via the account just flipped above,
	// re-activated by the reactivate call — seed a second one to get a
	// deactivated read), not found.
	other := fake.seed("ines@example.com", "ines", "Ines")
	other.Status = "deactivated"
	if _, err := c.Reconcile(ctx, connector.Input{Email: "nadia@example.com"}); err != nil {
		t.Fatalf("Reconcile (active): %v", err)
	}
	if _, err := c.Reconcile(ctx, connector.Input{Email: "ines@example.com"}); err != nil {
		t.Fatalf("Reconcile (deactivated): %v", err)
	}
	if _, err := c.Reconcile(ctx, connector.Input{Email: "ghost@example.com"}); err != nil {
		t.Fatalf("Reconcile (not found): %v", err)
	}

	// Deprovision: recorded id, then lookup-then-deactivate, then a recorded
	// id that 404s (a wrong record — still an exchange worth checking).
	nadia := fake.account("nadia@example.com")
	if err := c.Deprovision(ctx, connector.Input{ExternalID: nadia.ID}); err != nil {
		t.Fatalf("Deprovision (recorded): %v", err)
	}
	fake.seed("paulo@example.com", "paulo", "Paulo")
	if err := c.Deprovision(ctx, connector.Input{Email: "paulo@example.com"}); err != nil {
		t.Fatalf("Deprovision (lookup): %v", err)
	}
	_ = c.Deprovision(ctx, connector.Input{ExternalID: "11111111-2222-4333-8444-555555555555"}) // expected error; still a real exchange

	// Direct-against-the-fake shapes the connector's own guards never send:
	// an unauthenticated request (401), plus a deactivate against a bot id
	// (404, byte-identical to an unknown id).
	probeRaw(t, rt, http.MethodGet, fake.baseURL+"/accounts?email=a@example.com", "")
	bot := fake.seedBot("33333333-4444-4555-8666-777777777777")
	probeRaw(t, rt, http.MethodPost, fake.baseURL+"/accounts/"+bot.ID+"/deactivate", "Bearer "+testToken)

	doc := loadContractDoc(t)
	router := newContractRouter(t, doc)

	if len(rt.exchanges) == 0 {
		t.Fatal("no exchanges recorded")
	}
	for _, ex := range rt.exchanges {
		if err := checkExchange(ctx, router, ex); err != nil {
			t.Errorf("%s %s -> %d: %v", ex.req.Method, ex.req.URL.Path, ex.status, err)
		}
	}

	// The blank-email probe is its own case: `email` carries `minLength: 1`
	// in the contract itself, so kin-openapi correctly calls the REQUEST
	// malformed — that's not drift, it's the same reason the real service
	// also answers 400 instead of a lookup. Only the response shape is
	// checked here.
	blankRT := &recordingTransport{}
	probeRaw(t, blankRT, http.MethodGet, fake.baseURL+"/accounts?email=", "Bearer "+testToken)
	if len(blankRT.exchanges) != 1 {
		t.Fatalf("expected one exchange from the blank-email probe, got %d", len(blankRT.exchanges))
	}
	if err := checkResponseOnly(ctx, router, blankRT.exchanges[0]); err != nil {
		t.Errorf("blank-email 400 response does not satisfy the contract: %v", err)
	}
}

// probeRaw sends one request straight through the recording transport,
// bypassing the connector entirely, for the response shapes the connector's
// own guards mean it never triggers on its own (a blank query parameter, a
// missing credential). An empty auth sends no Authorization header at all.
func probeRaw(t *testing.T, rt *recordingTransport, method, url, auth string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
}

// TestContract_MutationControlCatchesDrift is the control: it proves the
// checker above actually has teeth by narrowing the loaded contract in a way
// that a real exchange used to satisfy, and confirming that exchange now
// fails against the mutated copy.
func TestContract_MutationControlCatchesDrift(t *testing.T) {
	ctx := context.Background()
	c, _, rt := scriptedFixture(t)

	if _, err := c.Provision(ctx, connector.Input{Email: "nadia@example.com"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(rt.exchanges) != 1 {
		t.Fatalf("expected exactly one exchange, got %d", len(rt.exchanges))
	}
	created := rt.exchanges[0]

	// Sanity: the real, unmutated contract accepts this exchange.
	goodDoc := loadContractDoc(t)
	if err := checkExchange(ctx, newContractRouter(t, goodDoc), created); err != nil {
		t.Fatalf("the real contract should accept a real created-account exchange, got: %v", err)
	}

	// Mutate: narrow EnsureResponse.outcome's enum so "created" is no longer
	// an allowed value — simulating the contract having drifted out from
	// under the double without either being updated to match.
	mutated := loadContractDoc(t)
	outcomeSchema := mutated.Components.Schemas["EnsureResponse"].Value.Properties["outcome"].Value
	outcomeSchema.Enum = []any{"existing", "reactivated"}

	if err := checkExchange(ctx, newContractRouter(t, mutated), created); err == nil {
		t.Fatal("expected the narrowed contract to reject a real created-account response, but it passed")
	}
}
