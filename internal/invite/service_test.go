package invite

import (
	"context"
	"errors"
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
	mu      sync.Mutex
	calls   int
	fail    error // when non-nil, Provision returns this
	result  connector.Result
}

func (f *fakeConn) Key() string         { return f.key }
func (f *fakeConn) DisplayName() string { return f.display }
func (f *fakeConn) Provision(_ context.Context, in connector.Input) (connector.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		return connector.Result{}, f.fail
	}
	return f.result, nil
}
func (f *fakeConn) Reconcile(context.Context, connector.Input) error   { return nil }
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
	sw := &fakeConn{key: "switchyard", display: "Switchyard", result: connector.Result{
		ExternalID: "u-1", Username: "Ada", Secret: "sw_TOKEN", SecretLabel: "API token",
		LoginURL: "https://switchyard.example", Instructions: "sign in",
	}}
	cf := &fakeConn{key: "cloudflare", display: "Cloudflare Access (SSO)", result: connector.Result{
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

func TestRun_PendingConnector_SurfacesPending(t *testing.T) {
	st := newFakeStore()
	// pendingErr unwraps to connector.ErrPending, so the outcome is flagged Pending.
	pending := &fakeConn{key: "argosy", display: "Argosy", fail: pendingErr{}}
	reg := connector.NewRegistry(pending)
	svc := New(seededStore(t, st, reg), reg, nil)

	res, err := svc.Run(context.Background(), Request{
		Name: "Ada", Email: "ada@example.com", Services: []string{"argosy"},
		Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	o := outcome(res, "argosy")
	if o.Status != model.TaskFailed || !o.Pending {
		t.Errorf("want failed+pending, got status=%s pending=%v", o.Status, o.Pending)
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

func (pendingErr) Error() string { return "argosy pending" }
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

func (s *fakeStore) UpdateTask(_ context.Context, t model.ProvisionTask) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[keyOf(t.InviteID, t.ServiceID)] = t
	return nil
}
