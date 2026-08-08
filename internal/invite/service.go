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
	"log"
	"strings"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/store"
)

// Store is the persistence surface the orchestrator needs. *store.Store
// satisfies it; tests supply an in-memory fake.
type Store interface {
	InsertPersonIfAbsent(ctx context.Context, name, email string, typ model.PersonType) (model.Person, bool, error)
	RenamePerson(ctx context.Context, email, name string) (model.Person, string, error)
	PersonByEmail(ctx context.Context, email string) (model.Person, error)
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
	emailer  Emailer   // may be nil when SMTP is unconfigured
	bundles  BundleSet // named onboarding bundles; zero value = none configured
	launcher string    // Cloudflare App Launcher URL; empty = don't mention one
}

// Option customizes a Service at construction.
type Option func(*Service)

// WithBundles supplies the named onboarding bundles (PRSR-12). Without it a
// Service has no bundles and every request must name its services explicitly —
// which is what the orchestrator tests rely on.
func WithBundles(bs BundleSet) Option {
	return func(s *Service) { s.bundles = bs }
}

// WithLauncher supplies Cloudflare's App Launcher URL, which the credential
// block leads with for anyone this invite granted Access to. Without it the
// block falls back to the plain per-service list.
func WithLauncher(url string) Option {
	return func(s *Service) { s.launcher = url }
}

// New builds an invite Service. emailer may be nil.
func New(st Store, reg *connector.Registry, emailer Emailer, opts ...Option) *Service {
	s := &Service{store: st, registry: reg, emailer: emailer}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Bundles exposes the configured bundles (for CLI help and the API surface).
func (s *Service) Bundles() BundleSet { return s.bundles }

// Request is a single invite: who, into which services, how to deliver.
type Request struct {
	Name     string
	Email    string
	Services []string // connector keys; combined with Bundle, see BundleSet.resolveServices
	Bundle   string   // named bundle to expand; "" with no Services uses the default bundle
	Role     string   // preset permission hint: "" (member) | "admin"
	Delivery model.DeliveryMethod

	// Fine-grained permission controls (Switchyard). Optional; empty falls back
	// to the Role preset.
	InstanceRole string                   // "" | "member" | "owner"
	Scopes       []string                 // explicit token scopes
	Projects     []connector.ProjectGrant // project memberships to assign
}

// ServiceOutcome is the per-service result surfaced to the caller and rendered
// into the credential block.
type ServiceOutcome struct {
	ServiceKey  string
	DisplayName string
	Icon        string // emoji shown next to the service in the credential block
	Status      model.TaskStatus
	Error       string // set when Status is failed or unavailable

	// Credential material (present on success; Secret is one-time plaintext and
	// is never persisted — only its hash is).
	Username     string
	Secret       string
	SecretLabel  string
	LoginURL     string
	Instructions string
	Extra        map[string]string
}

// ErrUnknownService reports a service key nothing knows about. It is a typo in
// a flag, not an outage, so callers match on it to exit like the other usage
// errors rather than like a failure.
var ErrUnknownService = errors.New("invite: unknown service")

// unknownServiceError formats that refusal with the keys that would have
// worked. known is passed in rather than read here because the right authority
// differs by caller — the connector registry for anything that will *provision*,
// the service table for a view of records — and that distinction is deliberate.
func unknownServiceError(key string, known []string) error {
	return fmt.Errorf("%w %q (known: %s)", ErrUnknownService, key, strings.Join(known, ", "))
}

// ErrNameConflictOnEmail reports that an invite asked for email delivery while
// its --name disagreed with the stored record, and was refused.
//
// Distinct from ErrNameConflict, which `person add` returns when it declines to
// edit a name: this one is not about the write at all — the invite would have
// kept the stored name quite happily. It is about the *send*. See the refusal in
// Run for why the two paths resolve the same signal differently.
var ErrNameConflictOnEmail = errors.New("invite: refusing email delivery for a name that disagrees with the record")

// NameConflict reports that the invite's --name disagreed with the name already
// recorded for that email, and that the stored name was kept.
//
// On copy-paste delivery it is not an error: the invite proceeds against the
// existing person, and the disagreement is *said out loud* rather than silently
// resolved — the entire difference from the UpsertPerson behaviour it replaced
// (PRSR-20). On email delivery the same signal is fatal; see
// ErrNameConflictOnEmail.
type NameConflict struct {
	Email     string
	Stored    string // the name on the record — kept
	Requested string // the name this invite passed — ignored
}

// Result is the outcome of an invite.
//
// CredentialBlock and OperatorNote have different audiences and are kept apart
// for that reason: the block is the invitee's message and is the only thing that
// may ever be emailed, while the note is for whoever ran the invite (PRSR-19).
type Result struct {
	Person          model.Person
	InviteID        uuid.UUID
	Delivery        model.DeliveryMethod
	Bundle          string // the bundle that supplied the service list, if any
	Outcomes        []ServiceOutcome
	CredentialBlock string // the copy-pasteable block, for the recipient
	OperatorNote    string // what didn't provision, for the operator; "" when everything did
	Delivered       bool   // true when an email was actually sent

	// NameConflict is set when --name disagreed with the stored record. The
	// stored name was kept and the invite ran anyway; operator-facing.
	NameConflict *NameConflict
}

// resolve returns a copy of req with Services expanded from its bundle and any
// bundle-default project grants applied. It also reports which bundle supplied
// the list (empty when the request named its services explicitly). Callers that
// need the final request use this; Validate uses it to check without mutating.
func (s *Service) resolve(req Request) (Request, string, error) {
	services, applied, err := s.bundles.resolveServices(req.Services, req.Bundle)
	if err != nil {
		return req, "", err
	}
	grants, err := s.bundles.projectsFor(applied, req.Projects)
	if err != nil {
		return req, "", err
	}
	req.Services = services
	req.Projects = grants
	return req, applied, nil
}

// checked is a request that has passed validation: bundle expanded, delivery
// defaulted, email normalized. Run works from one of these rather than deriving
// the same values a second time, so a request becomes usable in exactly one
// place and Run has no error branch that Validate didn't already take.
type checked struct {
	req    Request // Services expanded, Projects merged, Delivery defaulted
	bundle string  // the bundle that supplied the service list; "" if named explicitly
	email  string  // the normalized identity key
}

// Validate checks a request against the registry before any writes, and is the
// caller-facing half of validate below — the API uses it to answer 400 vs 500.
func (s *Service) Validate(req Request) error {
	_, err := s.validate(req)
	return err
}

// validate checks a request and returns it in the form Run needs. It resolves
// any bundle first, so an unknown bundle or a bundle naming an unregistered
// service is caught here — i.e. reported as the caller's error rather than
// failing midway through provisioning.
//
// Everything the database will reject is rejected here, because Run writes a
// person row before it writes anything else: a value that only the schema
// refuses (an unknown delivery method against `invite`'s CHECK constraint)
// would otherwise surface as a raw Postgres error, and a 500, after the roster
// had already grown a row for it.
func (s *Service) validate(req Request) (checked, error) {
	if strings.TrimSpace(req.Name) == "" {
		return checked{}, errors.New("invite: name is required")
	}
	// The address is required for every invite, not only an emailed one
	// (PRSR-23) — it is this person's identity key: the SSO join key upstream,
	// what the audit looks them up by, and the conflict target that makes a
	// re-invite idempotent. Normalized through the same function `person add`
	// uses, so `invite` cannot record a key the other command would have
	// rejected, and this is the value that becomes the conflict target.
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return checked{}, err
	}
	switch req.Delivery {
	case "", model.DeliverCopyPaste:
		req.Delivery = model.DeliverCopyPaste
	case model.DeliverEmail:
		if s.emailer == nil {
			return checked{}, errors.New("invite: email delivery requested but SMTP is not configured")
		}
	default:
		return checked{}, fmt.Errorf("invite: unknown delivery method %q (want %s or %s)",
			req.Delivery, model.DeliverCopyPaste, model.DeliverEmail)
	}
	resolved, applied, err := s.resolve(req)
	if err != nil {
		return checked{}, err
	}
	if len(resolved.Services) == 0 {
		return checked{}, errors.New("invite: at least one service is required")
	}
	for _, key := range resolved.Services {
		if _, ok := s.registry.Get(key); !ok {
			if req.Bundle != "" {
				return checked{}, fmt.Errorf("invite: bundle %q names unknown service %q (known services: %s)",
					req.Bundle, key, strings.Join(s.registry.Keys(), ", "))
			}
			return checked{}, unknownServiceError(key, s.registry.Keys())
		}
	}
	return checked{req: resolved, bundle: applied, email: email}, nil
}

// Run executes the invite. It is safe to call repeatedly with the same request:
// already-provisioned services are skipped and only previously-failed ones are
// retried. A per-service connector failure does not abort the whole invite; it
// is recorded and the remaining services still run.
func (s *Service) Run(ctx context.Context, req Request) (*Result, error) {
	// One pass decides whether this request is usable and what it means: the
	// expanded service list, the applied bundle — reported back so the operator
	// can see which set was used, in particular that an unqualified invite got
	// the default one — and the normalized identity key.
	c, err := s.validate(req)
	if err != nil {
		return nil, err
	}
	req, appliedBundle, email := c.req, c.bundle, c.email

	person, conflict, err := s.resolvePerson(ctx, strings.TrimSpace(req.Name), email)
	if err != nil {
		return nil, err
	}

	// On the one path that can't be taken back, a name mismatch is a refusal
	// rather than a warning.
	//
	// The mismatch is the only evidence that a mistyped --email landed on a
	// *different existing person*, and email delivery mails that person live
	// credentials before the operator can read a warning about it. Copy-paste
	// keeps the warning: nothing has left the building, and the operator is the
	// gate. Refusing here costs a re-run; not refusing costs a credential sent
	// to the wrong human.
	//
	// Nothing has been written at this point — no invite row, no provisioning.
	if conflict != nil && req.Delivery == model.DeliverEmail {
		return nil, fmt.Errorf("%w: %s is recorded as %q, not %q — refusing to email credentials to an address whose name disagrees; re-run with the recorded name, or rename first with `purser person add --email %s --name %q --rename`",
			ErrNameConflictOnEmail, conflict.Email, conflict.Stored, conflict.Requested, conflict.Email, conflict.Requested)
	}

	inv, err := s.store.CreateInvite(ctx, person.ID, req.Delivery, req.Role)
	if err != nil {
		return nil, err
	}

	res := &Result{Person: person, InviteID: inv.ID, Delivery: req.Delivery, Bundle: appliedBundle, NameConflict: conflict}

	for _, key := range req.Services {
		conn, _ := s.registry.Get(key) // validated
		outcome, err := s.provisionOne(ctx, inv, person, conn, req)
		if err != nil {
			return nil, err // infrastructure error (DB), not a connector failure
		}
		res.Outcomes = append(res.Outcomes, outcome)
	}

	res.CredentialBlock = RenderCredentialBlock(person, res.Outcomes, s.launcher)
	res.OperatorNote = RenderOperatorNote(res.Outcomes)

	// An invite where nothing at all succeeded has nothing to say to the
	// recipient, so it is not sent — see deliverable. The failures are still
	// recorded, still reported to the operator, and still retried by the next
	// run; Delivered stays false so the caller can see the send didn't happen.
	if req.Delivery == model.DeliverEmail && deliverable(res.Outcomes) {
		subject := fmt.Sprintf("Your access to %s", strings.Join(displayNames(res.Outcomes), ", "))
		// CredentialBlock only — OperatorNote must not reach the invitee.
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

// resolvePerson finds or creates the person this invite is for, without ever
// renaming an existing one (PRSR-20).
//
// `invite` used to call UpsertPerson, whose ON CONFLICT sets name = EXCLUDED.name.
// A re-invite is an ordinary thing to do — invites are idempotent per
// (person × service), so adding a service to someone means re-running it — and a
// mistyped --name on that re-run silently overwrote the stored name with no
// output and no way to recover the old one.
//
// It reports the disagreement instead of acting on it. Refusing outright was the
// alternative, and it's the wrong trade here: an invite's job is provisioning,
// and failing one over a name mismatch punishes the wrong action. Renaming stays
// where it already lives, behind `person add --rename`, so exactly one command
// can change a name and it's the one whose whole subject is the roster.
//
// email is never empty — Validate requires it (PRSR-23). It used to be optional,
// and an emailless invite took a separate branch through UpsertPerson, which
// with no address is not an upsert at all but an unconditional INSERT: the
// unique index is partial on email IS NOT NULL, so such a row cannot collide
// with anything, including a previous run of the same invite. Every re-run
// therefore minted a *new person id*, and the person id is what idempotency is
// keyed on — UNIQUE(person_id, service_id) for the skip, and InviteRef for the
// upstream Idempotency-Key. So the one command that promises "re-running only
// retries what failed" quietly re-provisioned everything, minting a fresh
// upstream user and a fresh secret each time, and left the audit a new person to
// walk per run. Requiring the address is what removes the branch; there is no
// second identity key to fall back to.
func (s *Service) resolvePerson(ctx context.Context, name, email string) (model.Person, *NameConflict, error) {
	person, created, err := s.findOrCreate(ctx, name, email, model.PersonHuman)
	if err != nil {
		return model.Person{}, nil, err
	}
	// Type is deliberately not compared. Unlike `person add`, an invite doesn't
	// assert what kind of identity this is — it passes human only as the default
	// for a row that may not exist yet, and an existing row keeps its own type
	// untouched either way.
	if created || sameName(person.Name, name) {
		return person, nil, nil
	}
	return person, &NameConflict{
		Email:     person.Email,
		Stored:    person.Name,
		Requested: name,
	}, nil
}

// findOrCreate inserts the person if the address is free and otherwise reads
// back whoever holds it. Either way the returned row is the one in the table —
// InsertPersonIfAbsent deliberately returns nothing when the address is taken,
// so the read-back is not optional.
//
// Shared with AddPerson so the two entry points cannot drift on what "this
// person already exists" means; they differ only in what they do about it.
func (s *Service) findOrCreate(ctx context.Context, name, email string, typ model.PersonType) (model.Person, bool, error) {
	p, created, err := s.store.InsertPersonIfAbsent(ctx, name, email, typ)
	if err != nil {
		return model.Person{}, false, err
	}
	if created {
		return p, true, nil
	}
	existing, err := s.store.PersonByEmail(ctx, email)
	if err != nil {
		return model.Person{}, false, err
	}
	return existing, false, nil
}

// sameName compares two names for conflict purposes, ignoring differences that
// nobody can see: surrounding space, and runs of internal whitespace.
//
// Case is deliberately *not* folded. "ada lovelace" against "Ada Lovelace" is a
// visible difference an operator can read in the warning and choose to fix with
// --rename, so reporting it is useful. A doubled space is invisible in every
// terminal, and a byte-exact comparison would warn about it on every re-invite
// forever — noise on the routine path, which erodes attention for the email
// refusal that depends on this same comparison.
func sameName(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}

// provisionOne handles a single service: skip if already provisioned, otherwise
// run the connector and persist the result.
func (s *Service) provisionOne(ctx context.Context, inv model.Invite, person model.Person, conn connector.Connector, req Request) (ServiceOutcome, error) {
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
		PersonName:   person.Name,
		Email:        person.Email,
		Role:         req.Role,
		InstanceRole: req.InstanceRole,
		Scopes:       req.Scopes,
		Projects:     req.Projects,
		// Stable per (person × service) — NOT per invite — so an upstream
		// Idempotency-Key actually dedupes across CLI re-runs (each run mints a
		// fresh invite id).
		InviteRef: fmt.Sprintf("purser-%s-%s", person.ID, conn.Key()),
	})
	if provErr != nil {
		// ErrPending is its own status, not a flag on failure: the connector
		// didn't break, it was never in a position to try. Both are retryable and
		// neither wrote an account, so nothing downstream of here changes — but
		// what the operator is being told does (PRSR-21).
		status := model.TaskFailed
		if errors.Is(provErr, connector.ErrPending) {
			status = model.TaskUnavailable
		}
		task.Status = status
		task.LastError = provErr.Error()
		// Failing to write down a failure must not abort the invite.
		//
		// This UPDATE records that a service *didn't* provision. The run it
		// belongs to may already be holding plaintext secrets minted for other
		// services, and those exist in exactly one place — the Result being
		// assembled by the caller, which is discarded the moment this function
		// returns an error. So a rejected write here (a schema older than the
		// binary, a constraint this status doesn't satisfy) would strand a live
		// upstream credential that nobody has a copy of and nobody can re-read.
		//
		// Losing a task row's status is recoverable — the next run re-attempts it,
		// since the idempotency skip keys on an active account and not on this
		// column. Losing the secret is not. Per-service failures don't abort the
		// invite; neither does failing to record one.
		if err := s.store.UpdateTask(ctx, task); err != nil {
			log.Printf("purser: invite %s: recording %s task for %s: %v", inv.ID, status, conn.Key(), err)
		}
		out.Status = status
		out.Error = provErr.Error()
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

// deliverable reports whether this invite produced anything worth sending the
// recipient — at least one service provisioned or already in place.
//
// When every service fails there is no message to send: the credential block is
// a greeting and nothing else, so mailing it tells the invitee they've been
// granted access while granting them none. That block used to carry the operator's
// failure list, which at least made it non-empty; splitting the two audiences
// (PRSR-19) is what left the all-failed case with an empty envelope, so the guard
// belongs here rather than in the renderer.
//
// A *partial* failure still sends. The recipient gets what they actually got,
// which is the whole reason per-service failures don't abort the invite.
//
// The test is "did anything succeed", not "did anything fail", and that is the
// deliberate reading of an all-unavailable invite too (PRSR-21): a connector
// nobody configured granted no access, so there is exactly as little to send as
// when one broke. Asking the positive question is what makes the two cases agree
// without either of them being special-cased.
func deliverable(outcomes []ServiceOutcome) bool {
	for _, o := range outcomes {
		if o.Status == model.TaskSucceeded || o.Status == model.TaskSkipped {
			return true
		}
	}
	return false
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
