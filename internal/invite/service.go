// Package invite is Purser's orchestrator: it turns one "invite this person into
// these services" request into per-service provisioning tasks, runs each
// service's connector, persists the durable access records, and assembles a
// copy-pasteable credential block (optionally emailed).
//
// Idempotency is per (person × service): a re-run reuses the person, invite
// tasks, and account rows, and only calls Provision for services that are not
// already actively provisioned — i.e. it retries only what failed.
package invite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/store"
)

// Store is the persistence surface the orchestrator needs. *store.Store
// satisfies it; tests supply an in-memory fake.
type Store interface {
	UpsertPerson(ctx context.Context, name, email string, typ model.PersonType) (model.Person, error)
	ServiceByKey(ctx context.Context, key string) (model.Service, error)
	CreateInvite(ctx context.Context, personID uuid.UUID, delivery model.DeliveryMethod, role string) (model.Invite, error)
	MarkInviteDelivered(ctx context.Context, inviteID uuid.UUID) error
	AccountFor(ctx context.Context, personID, serviceID uuid.UUID) (model.Account, error)
	UpsertAccount(ctx context.Context, a model.Account) (model.Account, error)
	EnsureTask(ctx context.Context, inviteID, personID, serviceID uuid.UUID) (model.ProvisionTask, error)
	UpdateTask(ctx context.Context, t model.ProvisionTask) error
}

// Emailer sends the rendered credential block. Injected so delivery is
// swappable and optional (copy-paste needs no emailer).
type Emailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Service orchestrates invites over a connector registry and store.
type Service struct {
	store    Store
	registry *connector.Registry
	emailer  Emailer // may be nil when SMTP is unconfigured
}

// New builds an invite Service. emailer may be nil.
func New(st Store, reg *connector.Registry, emailer Emailer) *Service {
	return &Service{store: st, registry: reg, emailer: emailer}
}

// Request is a single invite: who, into which services, how to deliver.
type Request struct {
	Name     string
	Email    string
	Services []string // connector keys
	Role     string   // permission hint: "" (member) | "admin"
	Delivery model.DeliveryMethod
}

// ServiceOutcome is the per-service result surfaced to the caller and rendered
// into the credential block.
type ServiceOutcome struct {
	ServiceKey  string
	DisplayName string
	Icon        string // emoji shown next to the service in the credential block
	Status      model.TaskStatus
	Error       string // set when Status == failed
	Pending     bool   // connector is wired but upstream support is not ready

	// Credential material (present on success; Secret is one-time plaintext and
	// is never persisted — only its hash is).
	Username     string
	Secret       string
	SecretLabel  string
	LoginURL     string
	Instructions string
	Extra        map[string]string
}

// Result is the outcome of an invite.
type Result struct {
	Person          model.Person
	InviteID        uuid.UUID
	Delivery        model.DeliveryMethod
	Outcomes        []ServiceOutcome
	CredentialBlock string // the copy-pasteable block
	Delivered       bool   // true when an email was actually sent
}

// Validate checks a request against the registry before any writes.
func (s *Service) Validate(req Request) error {
	if strings.TrimSpace(req.Name) == "" {
		return errors.New("invite: name is required")
	}
	if len(req.Services) == 0 {
		return errors.New("invite: at least one service is required")
	}
	if req.Delivery == model.DeliverEmail && strings.TrimSpace(req.Email) == "" {
		return errors.New("invite: email delivery requires an email address")
	}
	if req.Delivery == model.DeliverEmail && s.emailer == nil {
		return errors.New("invite: email delivery requested but SMTP is not configured")
	}
	for _, key := range req.Services {
		if _, ok := s.registry.Get(key); !ok {
			return fmt.Errorf("invite: unknown service %q (known: %s)", key, strings.Join(s.registry.Keys(), ", "))
		}
	}
	return nil
}

// Run executes the invite. It is safe to call repeatedly with the same request:
// already-provisioned services are skipped and only previously-failed ones are
// retried. A per-service connector failure does not abort the whole invite; it
// is recorded and the remaining services still run.
func (s *Service) Run(ctx context.Context, req Request) (*Result, error) {
	if err := s.Validate(req); err != nil {
		return nil, err
	}
	if req.Delivery == "" {
		req.Delivery = model.DeliverCopyPaste
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))

	person, err := s.store.UpsertPerson(ctx, strings.TrimSpace(req.Name), email, model.PersonHuman)
	if err != nil {
		return nil, err
	}
	inv, err := s.store.CreateInvite(ctx, person.ID, req.Delivery, req.Role)
	if err != nil {
		return nil, err
	}

	res := &Result{Person: person, InviteID: inv.ID, Delivery: req.Delivery}

	for _, key := range req.Services {
		conn, _ := s.registry.Get(key) // validated
		outcome, err := s.provisionOne(ctx, inv, person, conn, req.Role)
		if err != nil {
			return nil, err // infrastructure error (DB), not a connector failure
		}
		res.Outcomes = append(res.Outcomes, outcome)
	}

	res.CredentialBlock = RenderCredentialBlock(person, res.Outcomes)

	if req.Delivery == model.DeliverEmail {
		subject := fmt.Sprintf("Your access to %s", strings.Join(displayNames(res.Outcomes), ", "))
		if err := s.emailer.Send(ctx, email, subject, res.CredentialBlock); err != nil {
			return nil, fmt.Errorf("invite: deliver email: %w", err)
		}
		if err := s.store.MarkInviteDelivered(ctx, inv.ID); err != nil {
			return nil, err
		}
		res.Delivered = true
	}

	return res, nil
}

// provisionOne handles a single service: skip if already provisioned, otherwise
// run the connector and persist the result.
func (s *Service) provisionOne(ctx context.Context, inv model.Invite, person model.Person, conn connector.Connector, role string) (ServiceOutcome, error) {
	out := ServiceOutcome{ServiceKey: conn.Key(), DisplayName: conn.DisplayName(), Icon: conn.Icon()}

	svc, err := s.store.ServiceByKey(ctx, conn.Key())
	if err != nil {
		return out, err
	}
	task, err := s.store.EnsureTask(ctx, inv.ID, person.ID, svc.ID)
	if err != nil {
		return out, err
	}

	// Idempotency: an active account means this person is already provisioned
	// into this service (an account row is written only after a successful
	// Provision). Skip re-provisioning — no duplicate upstream user, no fresh
	// secret.
	if acct, err := s.store.AccountFor(ctx, person.ID, svc.ID); err == nil &&
		acct.Status == model.AccountActive {
		task.Status = model.TaskSkipped
		task.AccountID = &acct.ID
		if err := s.store.UpdateTask(ctx, task); err != nil {
			return out, err
		}
		out.Status = model.TaskSkipped
		out.Username = acct.Username
		out.Instructions = "Already provisioned on a previous invite — existing credentials remain valid."
		return out, nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return out, err
	}

	task.Status = model.TaskRunning
	task.Attempts++
	if err := s.store.UpdateTask(ctx, task); err != nil {
		return out, err
	}

	provRes, provErr := conn.Provision(ctx, connector.Input{
		PersonName: person.Name,
		Email:      person.Email,
		Role:       role,
		// Stable per (person × service) — NOT per invite — so an upstream
		// Idempotency-Key actually dedupes across CLI re-runs (each run mints a
		// fresh invite id).
		InviteRef: fmt.Sprintf("purser-%s-%s", person.ID, conn.Key()),
	})
	if provErr != nil {
		task.Status = model.TaskFailed
		task.LastError = provErr.Error()
		if err := s.store.UpdateTask(ctx, task); err != nil {
			return out, err
		}
		out.Status = model.TaskFailed
		out.Error = provErr.Error()
		out.Pending = errors.Is(provErr, connector.ErrPending)
		return out, nil
	}

	acct, err := s.store.UpsertAccount(ctx, model.Account{
		PersonID:   person.ID,
		ServiceID:  svc.ID,
		ExternalID: provRes.ExternalID,
		Username:   provRes.Username,
		SecretHash: hashSecret(provRes.Secret),
		Status:     model.AccountActive,
	})
	if err != nil {
		return out, err
	}

	task.Status = model.TaskSucceeded
	task.AccountID = &acct.ID
	task.LastError = ""
	if err := s.store.UpdateTask(ctx, task); err != nil {
		return out, err
	}

	out.Status = model.TaskSucceeded
	out.Username = provRes.Username
	out.Secret = provRes.Secret
	out.SecretLabel = provRes.SecretLabel
	out.LoginURL = provRes.LoginURL
	out.Instructions = provRes.Instructions
	out.Extra = provRes.Extra
	return out, nil
}

// hashSecret returns the sha256 hex of a one-time secret, or "" for no secret.
// We persist only this — never the plaintext.
func hashSecret(secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func displayNames(outcomes []ServiceOutcome) []string {
	names := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		if o.Status == model.TaskSucceeded || o.Status == model.TaskSkipped {
			names = append(names, o.DisplayName)
		}
	}
	if len(names) == 0 {
		return []string{"the Construct"}
	}
	return names
}
