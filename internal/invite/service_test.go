package invite

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/store"
)

// fakeConn is a controllable connector that counts Provision calls so tests can
// assert idempotent skips and failed-only retries.
type fakeConn struct {
	key     string
	display string
	icon    string
	mu      sync.Mutex
	calls   int
	fail    error // when non-nil, Provision returns this
	result  connector.Result
	lastIn  connector.Input // the most recent Provision input, for assertions

	// Reconcile behavior (PRSR-15). recErr wins over recResult.
	recResult connector.ReconcileResult
	recErr    error
	recCalls  int
}

func (f *fakeConn) Key() string         { return f.key }
func (f *fakeConn) DisplayName() string { return f.display }
func (f *fakeConn) Icon() string        { return f.icon }
func (f *fakeConn) Provision(_ context.Context, in connector.Input) (connector.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastIn = in
	if f.fail != nil {
		return connector.Result{}, f.fail
	}
	return f.result, nil
}

func (f *fakeConn) lastInput() connector.Input {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastIn
}

// Reconcile returns whatever the test configured. recErr wins over recResult,
// so a test can simulate an unverifiable connector.
func (f *fakeConn) Reconcile(_ context.Context, in connector.Input) (connector.ReconcileResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recCalls++
	if f.recErr != nil {
		return connector.ReconcileResult{}, f.recErr
	}
	return f.recResult, nil
}

func (f *fakeConn) reconcileCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recCalls
}

func (f *fakeConn) Deprovision(context.Context, connector.Input) error { return nil }
func (f *fakeConn) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		people:   map[string]model.Person{},
		services: map[string]model.Service{},
		accounts: map[string]model.Account{},
		tasks:    map[string]model.ProvisionTask{},
	}
}

func TestRun_HappyPath_RendersCredentialBlock(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", icon: "🚉", result: connector.Result{
		ExternalID: "u-1", Username: "Ada", Secret: "sw_TOKEN", SecretLabel: "API token",
		LoginURL: "https://switchyard.example", Instructions: "sign in",
	}}
	cf := &fakeConn{key: "cloudflare", display: "Cloudflare Access (SSO)", icon: "🔐", result: connector.Result{
		ExternalID: "ada@example.com", Instructions: "use the email OTP",
	}}
	reg := connector.NewRegistry(sw, cf)
	svc := New(seededStore(t, st, reg), reg, nil)

	res, err := svc.Run(context.Background(), Request{
		Name: "Ada Lovelace", Email: "Ada@Example.com",
		Services: []string{"switchyard", "cloudflare"}, Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Person.Email != "ada@example.com" {
		t.Errorf("email not lowercased: %q", res.Person.Email)
	}
	if len(res.Outcomes) != 2 {
		t.Fatalf("want 2 outcomes, got %d", len(res.Outcomes))
	}
	for _, o := range res.Outcomes {
		if o.Status != model.TaskSucceeded {
			t.Errorf("%s: want succeeded, got %s (%s)", o.ServiceKey, o.Status, o.Error)
		}
	}
	if !strings.Contains(res.CredentialBlock, "sw_TOKEN") {
		t.Errorf("credential block missing token:\n%s", res.CredentialBlock)
	}
	if !strings.Contains(res.CredentialBlock, "email OTP") {
		t.Errorf("credential block missing SSO instructions:\n%s", res.CredentialBlock)
	}
	if !strings.Contains(res.CredentialBlock, "🚉 Switchyard") || !strings.Contains(res.CredentialBlock, "🔐 Cloudflare") {
		t.Errorf("credential block missing service emojis:\n%s", res.CredentialBlock)
	}
	// Secret must never be persisted in plaintext.
	acct := st.accounts[keyOf(res.Person.ID, st.services["switchyard"].ID)]
	if acct.SecretHash == "" || acct.SecretHash == "sw_TOKEN" {
		t.Errorf("secret hash wrong: %q", acct.SecretHash)
	}
}

func TestRun_FailedOnlyRetry_IsIdempotent(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", result: connector.Result{
		ExternalID: "u-1", Username: "Ada", Secret: "sw_TOKEN",
	}}
	cf := &fakeConn{key: "cloudflare", display: "Cloudflare Access (SSO)",
		fail: errors.New("cloudflare API down")}
	reg := connector.NewRegistry(sw, cf)
	svc := New(seededStore(t, st, reg), reg, nil)

	req := Request{Name: "Ada", Email: "ada@example.com",
		Services: []string{"switchyard", "cloudflare"}, Delivery: model.DeliverCopyPaste}

	// Run 1: switchyard succeeds, cloudflare fails.
	res1, err := svc.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run1: %v", err)
	}
	if got := outcome(res1, "cloudflare").Status; got != model.TaskFailed {
		t.Fatalf("run1 cloudflare: want failed, got %s", got)
	}

	// Cloudflare recovers.
	cf.mu.Lock()
	cf.fail = nil
	cf.result = connector.Result{ExternalID: "ada@example.com"}
	cf.mu.Unlock()

	// Run 2: same request. switchyard must be SKIPPED (not re-provisioned),
	// cloudflare retried and now succeeds.
	res2, err := svc.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	if got := outcome(res2, "switchyard").Status; got != model.TaskSkipped {
		t.Errorf("run2 switchyard: want skipped, got %s", got)
	}
	if got := outcome(res2, "cloudflare").Status; got != model.TaskSucceeded {
		t.Errorf("run2 cloudflare: want succeeded, got %s", got)
	}
	if sw.callCount() != 1 {
		t.Errorf("switchyard provisioned %d times, want exactly 1 (idempotent)", sw.callCount())
	}
	if cf.callCount() != 2 {
		t.Errorf("cloudflare provisioned %d times, want 2 (retry)", cf.callCount())
	}
}

// A connector that is registered but not configured gets its own status, not
// `failed` with a flag beside it (PRSR-21). The distinction is checked on the
// persisted task as well as the outcome: the task row is what a later audit or
// report reads, and it is the half that a status-shaped fix could most easily
// leave behind.
func TestRun_UnavailableConnector_IsNotAFailure(t *testing.T) {
	st := newFakeStore()
	// pendingErr unwraps to connector.ErrPending.
	pending := &fakeConn{key: "unconfigured", display: "Unconfigured Service", fail: pendingErr{}}
	reg := connector.NewRegistry(pending)
	svc := New(seededStore(t, st, reg), reg, nil)

	res, err := svc.Run(context.Background(), Request{
		Name: "Ada", Email: "ada@example.com", Services: []string{"unconfigured"},
		Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	o := outcome(res, "unconfigured")
	if o.Status != model.TaskUnavailable {
		t.Errorf("want status=%s, got %s", model.TaskUnavailable, o.Status)
	}
	// The error text still rides along — it's the operator's "why", and for
	// Cloudflare it's the manual step to perform.
	if o.Error == "" {
		t.Error("an unavailable outcome must still carry the connector's reason")
	}
	if got := st.taskStatus(res.InviteID, "unconfigured"); got != model.TaskUnavailable {
		t.Errorf("persisted task status = %s, want %s", got, model.TaskUnavailable)
	}
}

// A connector that genuinely broke keeps `failed`. The pair of tests is the
// point: one status each, neither borrowing the other's.
func TestRun_BrokenConnector_IsAFailure(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", fail: errors.New("switchyard: 500")}
	reg := connector.NewRegistry(sw)
	svc := New(seededStore(t, st, reg), reg, nil)

	res, err := svc.Run(context.Background(), Request{
		Name: "Ada", Email: "ada@example.com", Services: []string{"switchyard"},
		Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if o := outcome(res, "switchyard"); o.Status != model.TaskFailed {
		t.Errorf("want status=%s, got %s", model.TaskFailed, o.Status)
	}
	if got := st.taskStatus(res.InviteID, "switchyard"); got != model.TaskFailed {
		t.Errorf("persisted task status = %s, want %s", got, model.TaskFailed)
	}
}

// An unavailable service is retried like a failed one: the idempotency skip keys
// on an *active account*, not on the task status, so giving ErrPending its own
// status must not have quietly parked those tasks. This is the invariant that
// makes "configure the token, re-run the same invite" work.
func TestRun_UnavailableConnector_IsRetriedOnRerun(t *testing.T) {
	st := newFakeStore()
	conn := &fakeConn{key: "lyceum", display: "Lyceum", fail: pendingErr{}}
	reg := connector.NewRegistry(conn)
	svc := New(seededStore(t, st, reg), reg, nil)

	req := Request{
		Name: "Ada", Email: "ada@example.com", Services: []string{"lyceum"},
		Delivery: model.DeliverCopyPaste,
	}
	if _, err := svc.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The token gets configured between runs.
	conn.fail = nil
	conn.result = connector.Result{ExternalID: "u-1", Username: "Ada"}

	res, err := svc.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := outcome(res, "lyceum").Status; got != model.TaskSucceeded {
		t.Errorf("re-run after configuring the connector should provision, got %s", got)
	}
	if conn.callCount() != 2 {
		t.Errorf("lyceum provisioned %d times, want 2 (the unavailable task is retryable)", conn.callCount())
	}
}

// fakeEmailer captures what was actually handed to delivery, so a test can
// assert on the message the invitee receives rather than on what the renderer
// happened to return.
type fakeEmailer struct {
	mu      sync.Mutex
	sent    int
	to      string
	subject string
	body    string
}

func (f *fakeEmailer) Send(_ context.Context, to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent++
	f.to, f.subject, f.body = to, subject, body
	return nil
}

// PRSR-19: the operator's failure list must never reach the invitee. It used to
// be appended to the credential block, and --deliver email mails that block
// verbatim — so one failed connector put raw err.Error() text (status codes,
// upstream bodies, internal hostnames) into an external inbox, under a heading
// announcing it wasn't for them.
//
// This asserts on the delivered body specifically. Asserting on
// res.CredentialBlock would pass even if Run went back to emailing a
// concatenation of the two.
func TestRun_EmailDelivery_OmitsTheOperatorNote(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", icon: "🚉", result: connector.Result{
		ExternalID: "u-1", Username: "Ada", Secret: "sw_TOKEN",
	}}
	// An error written for an operator, carrying exactly what shouldn't travel.
	lyc := &fakeConn{key: "lyceum", display: "Lyceum", icon: "📚",
		fail: errors.New("lyceum: 502 from lyceum.internal:8080")}
	reg := connector.NewRegistry(sw, lyc)
	mail := &fakeEmailer{}
	svc := New(seededStore(t, st, reg), reg, mail)

	res, err := svc.Run(context.Background(), Request{
		Name: "Ada Lovelace", Email: "ada@example.com",
		Services: []string{"switchyard", "lyceum"}, Delivery: model.DeliverEmail,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mail.sent != 1 {
		t.Fatalf("want exactly 1 email sent, got %d", mail.sent)
	}
	// Who received it is the point of the whole ticket — assert it, don't just
	// capture it.
	if mail.to != "ada@example.com" {
		t.Errorf("emailed the wrong recipient: %q", mail.to)
	}
	for _, leak := range []string{"lyceum.internal", "502", "Operator note", "✗"} {
		if strings.Contains(mail.body, leak) {
			t.Errorf("emailed body leaks operator-only text %q:\n%s", leak, mail.body)
		}
	}
	if strings.Contains(mail.subject, "Lyceum") {
		t.Errorf("subject names a service that failed to provision: %q", mail.subject)
	}
	// The recipient still gets what the invite is for.
	if !strings.Contains(mail.body, "sw_TOKEN") {
		t.Errorf("emailed body is missing the recipient's credentials:\n%s", mail.body)
	}
	// And the operator still learns what broke — just not through that inbox.
	if !strings.Contains(res.OperatorNote, "lyceum.internal:8080") {
		t.Errorf("operator note should carry the connector error:\n%s", res.OperatorNote)
	}
	if !res.Delivered {
		t.Error("a per-service failure must not block delivery of the rest")
	}
}

// Removing the operator note from the credential block left the all-failed case
// with an empty envelope: a greeting, and nothing else. Mailing that tells the
// invitee they've been granted access while granting them none, and marks the
// invite delivered — so a later "did they get it?" answers yes.
func TestRun_EmailDelivery_SendsNothingWhenEveryServiceFailed(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", icon: "🚉", fail: errors.New("switchyard: 500")}
	lyc := &fakeConn{key: "lyceum", display: "Lyceum", icon: "📚", fail: errors.New("lyceum: 502")}
	reg := connector.NewRegistry(sw, lyc)
	mail := &fakeEmailer{}
	svc := New(seededStore(t, st, reg), reg, mail)

	res, err := svc.Run(context.Background(), Request{
		Name: "Ada Lovelace", Email: "ada@example.com",
		Services: []string{"switchyard", "lyceum"}, Delivery: model.DeliverEmail,
	})
	// Still not an error: per-service failures don't abort the invite, and the
	// caller needs the Result to see what broke.
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mail.sent != 0 {
		t.Errorf("sent %d emails for an invite that provisioned nothing:\n%s", mail.sent, mail.body)
	}
	if res.Delivered {
		t.Error("Delivered must stay false when no email was sent")
	}
	// The operator still gets the full picture.
	for _, want := range []string{"switchyard: 500", "lyceum: 502"} {
		if !strings.Contains(res.OperatorNote, want) {
			t.Errorf("operator note missing %q:\n%s", want, res.OperatorNote)
		}
	}
}

// The partial case still sends: the recipient gets what they actually got, which
// is why a per-service failure doesn't abort the invite in the first place.
func TestRun_EmailDelivery_SendsWhenSomethingSucceeded(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", icon: "🚉", result: connector.Result{
		ExternalID: "u-1", Username: "Ada", Secret: "sw_TOKEN",
	}}
	lyc := &fakeConn{key: "lyceum", display: "Lyceum", icon: "📚", fail: errors.New("lyceum: 502")}
	reg := connector.NewRegistry(sw, lyc)
	mail := &fakeEmailer{}
	svc := New(seededStore(t, st, reg), reg, mail)

	res, err := svc.Run(context.Background(), Request{
		Name: "Ada Lovelace", Email: "ada@example.com",
		Services: []string{"switchyard", "lyceum"}, Delivery: model.DeliverEmail,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mail.sent != 1 || !res.Delivered {
		t.Fatalf("a partly-successful invite must still be delivered (sent=%d delivered=%v)", mail.sent, res.Delivered)
	}
}

// PRSR-20: `invite` called UpsertPerson, whose ON CONFLICT sets the name. Since
// invites are idempotent per (person × service), re-inviting someone to add a
// service is routine — and a mistyped --name on that re-run renamed them,
// silently, with the previous name unrecoverable.
func TestRun_DoesNotRenameAnExistingPerson(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", result: connector.Result{
		ExternalID: "u-1", Username: "Ada", Secret: "sw_TOKEN",
	}}
	reg := connector.NewRegistry(sw)
	svc := New(seededStore(t, st, reg), reg, nil)
	ctx := context.Background()

	if _, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Ada Lovelace", Email: "ada@example.com"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res, err := svc.Run(ctx, Request{
		Name: "Ada Lovelacce", Email: "ada@example.com", // typo
		Services: []string{"switchyard"}, Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Person.Name != "Ada Lovelace" {
		t.Errorf("stored name was overwritten: %q", res.Person.Name)
	}
	stored, err := st.PersonByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatalf("PersonByEmail: %v", err)
	}
	if stored.Name != "Ada Lovelace" {
		t.Errorf("row was renamed in the store: %q", stored.Name)
	}

	// Silence is the actual bug — keeping the name but saying nothing would
	// leave the operator believing the typo took.
	c := res.NameConflict
	if c == nil {
		t.Fatal("a disagreeing name must be reported, not just ignored")
	}
	if c.Stored != "Ada Lovelace" || c.Requested != "Ada Lovelacce" || c.Email != "ada@example.com" {
		t.Errorf("conflict misreported: %+v", c)
	}

	// And the invite still did its job — refusing would punish provisioning for
	// a name mismatch.
	if got := outcome(res, "switchyard").Status; got != model.TaskSucceeded {
		t.Errorf("provisioning should proceed regardless, got %s", got)
	}
}

// The mismatch is the only evidence that a mistyped --email landed on a
// *different existing person*, and email delivery mails that person live
// credentials. Warning after the fact is no use on a path that can't be undone,
// so it refuses — before writing an invite row, provisioning, or sending.
func TestRun_EmailDelivery_RefusesOnNameConflict(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", result: connector.Result{
		ExternalID: "u-1", Secret: "sw_TOKEN",
	}}
	reg := connector.NewRegistry(sw)
	mail := &fakeEmailer{}
	svc := New(seededStore(t, st, reg), reg, mail)
	ctx := context.Background()

	if _, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Ada Lovelace", Email: "ada@example.com"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Operator meant bob@example.com and hit Ada's address instead.
	_, err := svc.Run(ctx, Request{
		Name: "Bob Smith", Email: "ada@example.com",
		Services: []string{"switchyard"}, Delivery: model.DeliverEmail,
	})
	if !errors.Is(err, ErrNameConflictOnEmail) {
		t.Fatalf("want ErrNameConflictOnEmail, got %v", err)
	}
	if mail.sent != 0 {
		t.Errorf("mailed credentials to a person whose name disagreed:\n%s", mail.body)
	}
	if sw.callCount() != 0 {
		t.Error("provisioned before refusing — the refusal must precede any write")
	}
	// The message has to carry what disagreed, or it can't be acted on.
	for _, want := range []string{"Ada Lovelace", "Bob Smith", "ada@example.com", "--rename"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// Copy-paste keeps the warning: the operator is the gate, nothing has left the
// building, and failing the provision would punish the wrong action.
func TestRun_CopyPaste_WarnsRatherThanRefusing(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", result: connector.Result{ExternalID: "u-1"}}
	reg := connector.NewRegistry(sw)
	svc := New(seededStore(t, st, reg), reg, nil)
	ctx := context.Background()

	if _, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Ada Lovelace", Email: "ada@example.com"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := svc.Run(ctx, Request{
		Name: "Bob Smith", Email: "ada@example.com",
		Services: []string{"switchyard"}, Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("copy-paste must not refuse: %v", err)
	}
	if res.NameConflict == nil {
		t.Error("but it must still warn")
	}
}

// A doubled space is invisible in every terminal. A byte-exact comparison would
// warn about it on every re-invite forever, and on the email path would now
// block delivery outright.
func TestRun_InvisibleNameDifferenceIsNotAConflict(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", result: connector.Result{ExternalID: "u-1"}}
	reg := connector.NewRegistry(sw)
	mail := &fakeEmailer{}
	svc := New(seededStore(t, st, reg), reg, mail)
	ctx := context.Background()

	if _, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Ada  Lovelace", Email: "ada@example.com"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := svc.Run(ctx, Request{
		Name: "Ada Lovelace", Email: "ada@example.com",
		Services: []string{"switchyard"}, Delivery: model.DeliverEmail,
	})
	if err != nil {
		t.Fatalf("whitespace-only difference must not block delivery: %v", err)
	}
	if res.NameConflict != nil {
		t.Errorf("invisible difference reported as a conflict: %+v", res.NameConflict)
	}

	// Case is a *visible* difference, so it still reports — an operator can read
	// it and decide whether to fix the capitalization.
	res2, err := svc.Run(ctx, Request{
		Name: "ada lovelace", Email: "ada@example.com",
		Services: []string{"switchyard"}, Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res2.NameConflict == nil {
		t.Error("a case difference is visible and worth reporting")
	}
}

// invite writes the identity key every other command looks people up by, so it
// must not accept an address `person add` would reject.
func TestValidate_RejectsMalformedEmail(t *testing.T) {
	reg := connector.NewRegistry(&fakeConn{key: "switchyard", display: "Switchyard"})
	svc := New(newFakeStore(), reg, nil)

	err := svc.Validate(Request{
		Name: "Ada", Email: "Ada <ada@example.com>", Services: []string{"switchyard"},
	})
	if err == nil {
		t.Fatal("a display-name form must not become an identity key")
	}
}

// The overwhelmingly common re-invite: same person, same name, adding a service.
// It must stay completely quiet.
func TestRun_MatchingNameReportsNoConflict(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", result: connector.Result{ExternalID: "u-1"}}
	reg := connector.NewRegistry(sw)
	svc := New(seededStore(t, st, reg), reg, nil)
	ctx := context.Background()

	if _, err := svc.AddPerson(ctx, AddPersonRequest{Name: "Ada Lovelace", Email: "ada@example.com"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	res, err := svc.Run(ctx, Request{
		Name: "Ada Lovelace", Email: "ada@example.com",
		Services: []string{"switchyard"}, Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.NameConflict != nil {
		t.Errorf("no disagreement, so no warning: %+v", res.NameConflict)
	}
}

// An unknown address is not a conflict — the invite names a new person and that
// name is what gets recorded.
func TestRun_NewEmailRecordsTheGivenName(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", result: connector.Result{ExternalID: "u-1"}}
	reg := connector.NewRegistry(sw)
	svc := New(seededStore(t, st, reg), reg, nil)

	res, err := svc.Run(context.Background(), Request{
		Name: "Grace Hopper", Email: "Grace@Example.com",
		Services: []string{"switchyard"}, Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.NameConflict != nil {
		t.Errorf("new person, no conflict: %+v", res.NameConflict)
	}
	if res.Person.Name != "Grace Hopper" || res.Person.Email != "grace@example.com" {
		t.Errorf("person recorded wrong: %+v", res.Person)
	}
}

// An emailless invite has no conflict target — the unique index is partial on
// email IS NOT NULL — so it keeps the old unguarded path and must still work.
func TestRun_WithoutEmailStillProvisions(t *testing.T) {
	st := newFakeStore()
	sw := &fakeConn{key: "switchyard", display: "Switchyard", result: connector.Result{ExternalID: "u-1"}}
	reg := connector.NewRegistry(sw)
	svc := New(seededStore(t, st, reg), reg, nil)

	res, err := svc.Run(context.Background(), Request{
		Name: "Anon", Services: []string{"switchyard"}, Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.NameConflict != nil {
		t.Errorf("no email means nothing to conflict with: %+v", res.NameConflict)
	}
	if got := outcome(res, "switchyard").Status; got != model.TaskSucceeded {
		t.Errorf("want succeeded, got %s", got)
	}
}

func TestValidate_Errors(t *testing.T) {
	reg := connector.NewRegistry(&fakeConn{key: "switchyard", display: "Switchyard"})
	svc := New(newFakeStore(), reg, nil)

	cases := []struct {
		name string
		req  Request
	}{
		{"no name", Request{Services: []string{"switchyard"}}},
		{"no services", Request{Name: "Ada"}},
		{"unknown service", Request{Name: "Ada", Services: []string{"nope"}}},
		{"email delivery without email", Request{Name: "Ada", Services: []string{"switchyard"}, Delivery: model.DeliverEmail}},
	}
	for _, tc := range cases {
		if err := svc.Validate(tc.req); err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}
}

type pendingErr struct{}

func (pendingErr) Error() string { return "connector not configured" }
func (pendingErr) Unwrap() error { return connector.ErrPending }

func outcome(res *Result, key string) ServiceOutcome {
	for _, o := range res.Outcomes {
		if o.ServiceKey == key {
			return o
		}
	}
	return ServiceOutcome{}
}

// seededStore ensures the fake store has service rows for the registry, mirroring
// what main does on boot.
func seededStore(t *testing.T, st *fakeStore, reg *connector.Registry) *fakeStore {
	t.Helper()
	for _, c := range reg.All() {
		st.services[c.Key()] = model.Service{ID: uuid.New(), Key: c.Key(), DisplayName: c.DisplayName()}
	}
	return st
}

func keyOf(a, b uuid.UUID) string { return a.String() + ":" + b.String() }

// fakeStore is an in-memory invite.Store.
type fakeStore struct {
	mu       sync.Mutex
	people   map[string]model.Person        // by email (or id when no email)
	services map[string]model.Service       // by key
	accounts map[string]model.Account       // by person:service
	tasks    map[string]model.ProvisionTask // by invite:service
}

func (s *fakeStore) UpsertPerson(_ context.Context, name, email string, typ model.PersonType) (model.Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if email != "" {
		if p, ok := s.people[email]; ok {
			p.Name = name
			s.people[email] = p
			return p, nil
		}
	}
	p := model.Person{ID: uuid.New(), Name: name, Email: email, Type: typ}
	if email != "" {
		s.people[email] = p
	}
	return p, nil
}

// InsertPersonIfAbsent mirrors the real store: the email is unique
// case-insensitively, and an occupied one is never modified.
func (s *fakeStore) InsertPersonIfAbsent(_ context.Context, name, email string, typ model.PersonType) (model.Person, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if email == "" {
		return model.Person{}, false, errors.New("store: insert person: email is required")
	}
	k := strings.ToLower(email)
	if _, ok := s.people[k]; ok {
		return model.Person{}, false, nil
	}
	p := model.Person{ID: uuid.New(), Name: name, Email: k, Type: typ}
	s.people[k] = p
	return p, true, nil
}

func (s *fakeStore) RenamePerson(_ context.Context, email, name string) (model.Person, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.ToLower(email)
	p, ok := s.people[k]
	if !ok {
		return model.Person{}, "", store.ErrNotFound
	}
	previous := p.Name
	p.Name = name
	s.people[k] = p
	return p, previous, nil
}

func (s *fakeStore) ServiceByKey(_ context.Context, key string) (model.Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.services[key]
	if !ok {
		return model.Service{}, store.ErrNotFound
	}
	return svc, nil
}

func (s *fakeStore) CreateInvite(_ context.Context, personID uuid.UUID, d model.DeliveryMethod, role string) (model.Invite, error) {
	return model.Invite{ID: uuid.New(), PersonID: personID, Delivery: d, Role: role}, nil
}

func (s *fakeStore) MarkInviteDelivered(context.Context, uuid.UUID) error { return nil }

func (s *fakeStore) AccountFor(_ context.Context, personID, serviceID uuid.UUID) (model.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[keyOf(personID, serviceID)]
	if !ok {
		return model.Account{}, store.ErrNotFound
	}
	return a, nil
}

func (s *fakeStore) UpsertAccount(_ context.Context, a model.Account) (model.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := keyOf(a.PersonID, a.ServiceID)
	if existing, ok := s.accounts[k]; ok {
		a.ID = existing.ID
	} else {
		a.ID = uuid.New()
	}
	s.accounts[k] = a
	return a, nil
}

func (s *fakeStore) EnsureTask(_ context.Context, inviteID, personID, serviceID uuid.UUID) (model.ProvisionTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := keyOf(inviteID, serviceID)
	if t, ok := s.tasks[k]; ok {
		return t, nil
	}
	t := model.ProvisionTask{ID: uuid.New(), InviteID: inviteID, PersonID: personID, ServiceID: serviceID, Status: model.TaskPending}
	s.tasks[k] = t
	return t, nil
}

// --- AuditStore (PRSR-15) ---

func (s *fakeStore) ListPeople(context.Context) ([]model.Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Person, 0, len(s.people))
	for _, p := range s.people {
		out = append(out, p)
	}
	// Stable order so audit findings are deterministic across map iteration.
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

func (s *fakeStore) PersonByEmail(_ context.Context, email string) (model.Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.people[strings.ToLower(email)]; ok {
		return p, nil
	}
	return model.Person{}, store.ErrNotFound
}

func (s *fakeStore) UpdateAccountStatus(_ context.Context, accountID uuid.UUID, status model.AccountStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, a := range s.accounts {
		if a.ID == accountID {
			a.Status = status
			s.accounts[k] = a
			return nil
		}
	}
	return store.ErrNotFound
}

func (s *fakeStore) UpdateTask(_ context.Context, t model.ProvisionTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[keyOf(t.InviteID, t.ServiceID)] = t
	return nil
}

// taskStatus reads back what was persisted for one service of an invite, so a
// test can assert on the durable record rather than only on the returned
// outcome. They are set from the same value and could drift apart in exactly one
// edit; this is what notices.
func (s *fakeStore) taskStatus(inviteID uuid.UUID, serviceKey string) model.TaskStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.services[serviceKey]
	if !ok {
		return ""
	}
	return s.tasks[keyOf(inviteID, svc.ID)].Status
}
