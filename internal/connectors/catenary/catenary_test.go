package catenary

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/connector"
)

const testToken = "not-a-secret-just-a-Test-token-1"

func newFixture(t *testing.T) (*Connector, *fakeCatenary) {
	t.Helper()
	fake := newFake(testToken)
	srv := fake.start()
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, ProvisionToken: testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, fake
}

func TestNew_RequiresBaseURLAndToken(t *testing.T) {
	if _, err := New(Config{ProvisionToken: testToken}); err == nil {
		t.Error("expected error without BaseURL")
	}
	if _, err := New(Config{BaseURL: "http://catenary:4010"}); err == nil {
		t.Error("expected error without ProvisionToken")
	}
}

// roundTripFunc adapts a function to http.RoundTripper, so a test can inspect
// the *http.Request Go object this connector built without ever putting it on
// a wire — HTTP itself strips leading/trailing optional whitespace (RFC 7230)
// from a header value in transit, in any implementation, regardless of what
// this connector's own code does. So the only place "did we transform the
// token" is actually observable is before that transport layer touches it.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The token is presented VERBATIM — never trimmed or otherwise transformed,
// because it is compared constant-time on the other end and a silently
// altered value would fail as an unrelated-looking 401 rather than a config
// error. A real Signet-generated token has no such whitespace, but the rule
// is "don't transform it", not "it happens not to matter today".
func TestNew_TokenIsPresentedVerbatim(t *testing.T) {
	const withPadding = "  " + testToken + "  "
	var gotAuth string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"no such account"}`)),
		}, nil
	})

	c, err := New(Config{
		BaseURL: "http://catenary.internal", ProvisionToken: withPadding,
		HTTPClient: &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Reconcile(context.Background(), connector.Input{Email: "a@example.com"})
	if want := "Bearer " + withPadding; gotAuth != want {
		t.Errorf("Authorization header = %q, want %q (untrimmed)", gotAuth, want)
	}
}

// GATE QUESTION 1, restated for the real API: the existing connectors mint one
// secret per person per service; Catenary's unit is a device. At provisioning
// time the person has no devices yet, so ensureAccount hands back exactly one
// enrollment token and Result.Extra stays empty — nothing had to be smuggled
// through it.
func TestProvision_ReturnsExactlyOneEnrollmentToken(t *testing.T) {
	c, fake := newFixture(t)

	res, err := c.Provision(context.Background(), connector.Input{
		PersonName: "Nadia Ruiz", Email: "Nadia@Example.com",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(res.Secret) != 43 {
		t.Errorf("expected a 43-character enrollment token, got %d chars: %q", len(res.Secret), res.Secret)
	}
	if res.ExternalID == "" {
		t.Error("expected an upstream account id")
	}
	if res.Username == "" {
		t.Error("expected the derived handle as Username")
	}
	if len(res.Extra) != 0 {
		t.Errorf("Result.Extra should be unnecessary, got %v", res.Extra)
	}
	a := fake.account("nadia@example.com")
	if a == nil {
		t.Fatal("account not created")
	}
	if a.Status != "active" {
		t.Errorf("a freshly created account must be active, got %q", a.Status)
	}
}

// A re-invite of an existing account must hand over something fresh and
// redeemable — a single-use token that has already been redeemed can't be
// handed out again, so Provision re-issues rather than failing.
func TestProvision_ReinviteOfAnExistingAccountYieldsAFreshRedeemableToken(t *testing.T) {
	c, fake := newFixture(t)
	fake.seed("nadia@example.com", "nadia", "Nadia Ruiz")

	first, err := c.Provision(context.Background(), connector.Input{Email: "nadia@example.com"})
	if err != nil {
		t.Fatalf("Provision (1st): %v", err)
	}
	second, err := c.Provision(context.Background(), connector.Input{Email: "nadia@example.com"})
	if err != nil {
		t.Fatalf("Provision (2nd): %v", err)
	}
	if first.Secret == second.Secret {
		t.Error("a re-invite must mint a fresh token, not repeat the last one")
	}
	if second.ExternalID != first.ExternalID {
		t.Errorf("re-invite must resolve to the same account: %q != %q", second.ExternalID, first.ExternalID)
	}
	if strings.Contains(second.Instructions, "reactivated") {
		t.Errorf("an already-active account's re-invite must not claim a reactivation: %q", second.Instructions)
	}
}

// display_name is honored on creation only — a re-invite with a different
// name must not rename the account, and this connector must not promise
// otherwise anywhere in the result.
func TestProvision_DisplayNameIsHonoredOnCreationOnly(t *testing.T) {
	c, fake := newFixture(t)

	if _, err := c.Provision(context.Background(), connector.Input{
		PersonName: "Nadia Ruiz", Email: "nadia@example.com",
	}); err != nil {
		t.Fatalf("Provision (create): %v", err)
	}
	if _, err := c.Provision(context.Background(), connector.Input{
		PersonName: "A Totally Different Name", Email: "nadia@example.com",
	}); err != nil {
		t.Fatalf("Provision (re-invite): %v", err)
	}
	if got := fake.account("nadia@example.com").DisplayName; got != "Nadia Ruiz" {
		t.Errorf("display_name should survive a re-invite unchanged, got %q", got)
	}
}

// outcome: reactivated must reach the operator, because it means a previously
// offboarded person was just let back in.
func TestProvision_ReactivatesTheAccount(t *testing.T) {
	c, fake := newFixture(t)
	a := fake.seed("nadia@example.com", "nadia", "Nadia Ruiz")
	a.Status = "deactivated"

	if _, err := c.Provision(context.Background(), connector.Input{Email: "nadia@example.com"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if fake.account("nadia@example.com").Status != "active" {
		t.Error("a reactivated account must be active again")
	}
}

// outcome: reactivated must NOT reach Result.Instructions. PRSR-50's own
// description and comment both ask for exactly that ("the operator may need
// `catenary user set-email`"), but Result.Instructions is recipient-facing —
// internal/invite/credential.go's RenderCredentialBlock, the only thing
// `--deliver email` ever sends, renders it directly into the message the
// invited person receives (internal/invite/service.go copies it verbatim
// onto ServiceOutcome). A reactivation is exactly the kind of thing the
// invitee should not learn from their own welcome email: it says their
// account was previously offboarded. Caught in PR #64's review; see
// result's doc comment in catenary.go.
func TestProvision_ReactivatedOutcomeDoesNotReachInstructions(t *testing.T) {
	c, fake := newFixture(t)
	a := fake.seed("nadia@example.com", "nadia", "Nadia Ruiz")
	a.Status = "deactivated"

	res, err := c.Provision(context.Background(), connector.Input{Email: "nadia@example.com"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if strings.Contains(strings.ToLower(res.Instructions), "reactivat") || strings.Contains(strings.ToLower(res.Instructions), "offboard") {
		t.Errorf("Instructions is recipient-facing and must not mention the reactivation, got %q", res.Instructions)
	}
}

// note must NOT reach Result.Instructions (or Extra — also recipient-facing,
// same rendering path) for the same reason: it names another account's plain
// handle and an admin CLI command, neither of which is the invited person's
// business. See TestProvision_ReactivatedOutcomeDoesNotReachInstructions and
// result's doc comment in catenary.go. There is currently no channel from a
// successful Provision to Purser's own operator note
// (internal/invite.RenderOperatorNote only ever reads Status/Error, for
// failed/unavailable outcomes) — so the note is silently dropped here rather
// than leaked, and surfacing it properly is a decision for whoever owns
// internal/invite, not this connector.
func TestProvision_NoteDoesNotReachInstructionsOrExtra(t *testing.T) {
	c, fake := newFixture(t)
	fake.occupyHandle("nadia") // an email-less person already holds it

	res, err := c.Provision(context.Background(), connector.Input{Email: "nadia@example.com"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if res.Username != "nadia-2" {
		t.Errorf("expected the suffixed handle, got %q", res.Username)
	}
	if strings.Contains(res.Instructions, "nadia") && res.Username != "nadia" {
		t.Errorf("Instructions must not name the other account's plain handle, got %q", res.Instructions)
	}
	if strings.Contains(res.Instructions, "set-email") {
		t.Errorf("Instructions must not carry an admin CLI instruction to the recipient, got %q", res.Instructions)
	}
	if len(res.Extra) != 0 {
		t.Errorf("Extra is recipient-facing too; the note must not be smuggled through it, got %v", res.Extra)
	}
}

// Only email and display_name are ever sent, and display_name is OMITTED
// (not sent empty) when PersonName is blank — an omitted field is what lets
// ensureAccount fall back to the derived handle; Catenary rejects unknown
// fields with 400, so accidentally forwarding e.g. Input.Role would break
// every Provision call outright.
func TestProvision_SendsOnlyEmailAndDisplayName(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"account":{"id":"11111111-2222-4333-8444-555555555555","email":"a@example.com","handle":"a","display_name":"A","status":"active"},"enrollment_token":"` + mintToken() + `","enrollment_expires_at":"2026-01-01T00:00:00.000Z","outcome":"created"}`))
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: testToken})

	if _, err := c.Provision(context.Background(), connector.Input{
		Email: "a@example.com", PersonName: "A", Role: "admin",
	}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if len(body) != 2 {
		t.Errorf("expected exactly {email, display_name}, got %v", body)
	}
	if body["email"] != "a@example.com" || body["display_name"] != "A" {
		t.Errorf("unexpected body: %v", body)
	}
}

func TestProvision_OmitsDisplayNameWhenPersonNameIsBlank(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"account":{"id":"11111111-2222-4333-8444-555555555555","email":"a@example.com","handle":"a","display_name":"","status":"active"},"enrollment_token":"` + mintToken() + `","enrollment_expires_at":"2026-01-01T00:00:00.000Z","outcome":"created"}`))
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: testToken})

	if _, err := c.Provision(context.Background(), connector.Input{Email: "a@example.com"}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if _, present := body["display_name"]; present {
		t.Errorf("display_name must be omitted, not sent empty, got %v", body)
	}
}

func TestProvision_RequiresEmail(t *testing.T) {
	c, _ := newFixture(t)
	if _, err := c.Provision(context.Background(), connector.Input{PersonName: "No Email"}); err == nil {
		t.Error("expected error when email is missing")
	}
}

// GATE QUESTION 2, restated for the real API: Reconcile is read-only and must
// key on account STATUS, never on "a row came back".
func TestReconcile_KeysOnStatusNotRowExistence(t *testing.T) {
	ctx := context.Background()

	t.Run("active account reports access", func(t *testing.T) {
		c, fake := newFixture(t)
		fake.seed("nadia@example.com", "nadia", "Nadia Ruiz")

		got, err := c.Reconcile(ctx, connector.Input{Email: "nadia@example.com"})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if !got.Exists {
			t.Error("an active account must report access")
		}
		if got.ExternalID == "" || got.Username != "nadia" {
			t.Errorf("expected identity to be carried, got %+v", got)
		}
	})

	t.Run("deactivated account does not", func(t *testing.T) {
		c, fake := newFixture(t)
		a := fake.seed("nadia@example.com", "nadia", "Nadia Ruiz")
		a.Status = "deactivated"

		got, err := c.Reconcile(ctx, connector.Input{Email: "nadia@example.com"})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if got.Exists {
			t.Error("a deactivated account must not report access, however the row looks")
		}
	})

	t.Run("no account at all", func(t *testing.T) {
		c, _ := newFixture(t)
		got, err := c.Reconcile(ctx, connector.Input{Email: "nobody@example.com"})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if got.Exists {
			t.Error("expected Exists=false")
		}
	})
}

// A connector must not answer a reconcile it can't verify (Purser's own
// invariant): a 401 or a 500 is an error, never silently folded into
// Exists: false the way a 404 legitimately is.
func TestReconcile_UpstreamErrorIsNotExistsFalse(t *testing.T) {
	ctx := context.Background()

	t.Run("401", func(t *testing.T) {
		fake := newFake(testToken)
		srv := fake.start()
		defer srv.Close()
		// A connector pointed at the real double but configured with the
		// wrong credential — every operation there is behind the token.
		wrong, err := New(Config{BaseURL: srv.URL, ProvisionToken: "wrong-token"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := wrong.Reconcile(ctx, connector.Input{Email: "nadia@example.com"})
		if err == nil {
			t.Fatal("expected a 401 to surface as an error")
		}
		if got.Exists {
			t.Error("an error result must not also claim Exists=true")
		}
	})

	t.Run("500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal error"}`))
		}))
		defer srv.Close()
		c, _ := New(Config{BaseURL: srv.URL, ProvisionToken: testToken})

		_, err := c.Reconcile(ctx, connector.Input{Email: "nadia@example.com"})
		if err == nil {
			t.Fatal("expected a 500 to surface as an error, not Exists=false")
		}
	})
}

func TestReconcile_RequiresEmail(t *testing.T) {
	c, _ := newFixture(t)
	if _, err := c.Reconcile(context.Background(), connector.Input{}); err == nil {
		t.Error("expected error when email is missing")
	}
}

// GATE QUESTION 3, restated for the real API: Catenary's offboard is ONE
// transaction server-side, so R6's ordering hazard cannot occur — there is no
// "disable, then fan out over devices" for this connector to get right or
// wrong. What replaces it: exactly one mutating call, on either path.
func TestDeprovision_WithRecordedExternalID_MakesExactlyOneMutatingCall(t *testing.T) {
	c, fake := newFixture(t)
	a := fake.seed("nadia@example.com", "nadia", "Nadia Ruiz")

	if err := c.Deprovision(context.Background(), connector.Input{ExternalID: a.ID}); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	if len(fake.Calls) != 1 || fake.Calls[0] != "POST /accounts/"+a.ID+"/deactivate" {
		t.Errorf("expected exactly one deactivate call, got %v", fake.Calls)
	}
	if fake.account("nadia@example.com").Status != "deactivated" {
		t.Error("account should be deactivated")
	}
}

func TestDeprovision_WithoutExternalID_LooksUpThenDeactivates(t *testing.T) {
	c, fake := newFixture(t)
	a := fake.seed("nadia@example.com", "nadia", "Nadia Ruiz")

	if err := c.Deprovision(context.Background(), connector.Input{Email: "nadia@example.com"}); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	want := []string{"GET /accounts?email=nadia@example.com", "POST /accounts/" + a.ID + "/deactivate"}
	if len(fake.Calls) != 2 || fake.Calls[0] != want[0] || fake.Calls[1] != want[1] {
		t.Errorf("calls = %v, want %v", fake.Calls, want)
	}
}

// A look-up 404 (nobody to revoke) is success — the empty-ExternalID path's
// own idempotency, distinct from a deactivate 404.
func TestDeprovision_LookupNotFoundIsSuccess(t *testing.T) {
	c, fake := newFixture(t)
	if err := c.Deprovision(context.Background(), connector.Input{Email: "ghost@example.com"}); err != nil {
		t.Errorf("a lookup 404 must be a success, got %v", err)
	}
	for _, call := range fake.Calls {
		if strings.Contains(call, "deactivate") {
			t.Errorf("no deactivate call should have been made, got %v", fake.Calls)
		}
	}
}

// THE RESTATED FINDING. Catenary's offboard is one transaction, so there is no
// "disable first" vs "revoke devices first" for this connector to choose
// between. What R6's ordering hazard becomes here: a FAILED deactivate call
// must change nothing, and Reconcile must still see the account as it was.
func TestDeprovision_PartialFailureLeavesAccessIntactAndVisible(t *testing.T) {
	ctx := context.Background()
	c, fake := newFixture(t)
	a := fake.seed("nadia@example.com", "nadia", "Nadia Ruiz")
	fake.FailDeactivate = a.ID

	if err := c.Deprovision(ctx, connector.Input{ExternalID: a.ID}); err == nil {
		t.Fatal("expected the failed deactivate to surface as an error")
	}

	got, err := c.Reconcile(ctx, connector.Input{Email: "nadia@example.com"})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !got.Exists {
		t.Error("a failed deactivate must change nothing — the account must still report access")
	}
	if fake.account("nadia@example.com").Status != "active" {
		t.Error("a failed deactivate must not have flipped the account's status")
	}

	// And the retry completes it, which is what makes a failure safe to leave
	// and retry rather than something that needs manual cleanup.
	fake.FailDeactivate = ""
	if err := c.Deprovision(ctx, connector.Input{ExternalID: a.ID}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got, _ = c.Reconcile(ctx, connector.Input{Email: "nadia@example.com"})
	if got.Exists {
		t.Error("after the retry the account must no longer report access")
	}
}

// A 404 from deactivate for an id Purser itself recorded is an error to show
// the operator — Catenary never deletes accounts, so a recorded id cannot
// legitimately vanish.
func TestDeprovision_RecordedExternalID404IsAnError(t *testing.T) {
	c, _ := newFixture(t)
	err := c.Deprovision(context.Background(), connector.Input{ExternalID: "11111111-2222-4333-8444-555555555555"})
	if err == nil {
		t.Fatal("expected a 404 against a recorded id to be an error, not success")
	}
	if !strings.Contains(err.Error(), "never deletes accounts") {
		t.Errorf("error should explain why this is a wrong record rather than absent access: %v", err)
	}
}

// A bot's id and a non-UUID both 404 byte-identically upstream (the same
// branch as "unknown id"), and since Purser recorded this one, it is still an
// operator-visible error rather than success.
func TestDeprovision_RecordedExternalIDThatIsABot_IsAlsoAnError(t *testing.T) {
	c, fake := newFixture(t)
	bot := fake.seedBot("22222222-3333-4444-8555-666666666666")
	err := c.Deprovision(context.Background(), connector.Input{ExternalID: bot.ID})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestDeprovision_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	c, fake := newFixture(t)

	if err := c.Deprovision(ctx, connector.Input{Email: "ghost@example.com"}); err != nil {
		t.Errorf("deprovisioning a person with no account must be a success, got %v", err)
	}

	a := fake.seed("nadia@example.com", "nadia", "Nadia Ruiz")
	for i := range 3 {
		if err := c.Deprovision(ctx, connector.Input{ExternalID: a.ID}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}

func TestDeprovision_RequiresEmailWhenNoExternalID(t *testing.T) {
	c, _ := newFixture(t)
	if err := c.Deprovision(context.Background(), connector.Input{}); err == nil {
		t.Error("expected error when neither ExternalID nor Email is given")
	}
}

// Catenary can always revoke a configured account, so it must NOT advertise
// itself as unable — the offboard preview promises exactly what --apply does.
// This connector deliberately does not implement connector.RevokeChecker,
// relying on the documented default ("a connector that does not implement it
// is assumed able"), so this pins that the default really does hold here.
func TestCanDeprovision_IsNotRefused(t *testing.T) {
	c, _ := newFixture(t)
	if err := connector.CanDeprovision(c); err != nil {
		t.Errorf("Catenary can revoke; CanDeprovision should be nil, got %v", err)
	}
}
