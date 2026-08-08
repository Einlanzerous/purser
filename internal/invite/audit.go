package invite

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/store"
)

// AuditStore is the extra persistence surface the audit needs on top of Store.
type AuditStore interface {
	ListPeople(ctx context.Context) ([]model.Person, error)
	UpdateAccountStatus(ctx context.Context, accountID uuid.UUID, status model.AccountStatus) error
}

// UpstreamState is what Reconcile could determine about a person's access.
type UpstreamState string

const (
	// UpstreamYes — the connector confirmed the account exists.
	UpstreamYes UpstreamState = "yes"
	// UpstreamNo — the connector confirmed it does not.
	UpstreamNo UpstreamState = "no"
	// UpstreamUnknown — the connector could not check (no lookup endpoint, not
	// configured, or the call failed). Deliberately distinct from "no": treating
	// an unanswerable question as absence is how you generate wrong records.
	UpstreamUnknown UpstreamState = "unknown"
)

// AuditAction is what the audit did, or would do with Apply set.
type AuditAction string

const (
	ActionNone      AuditAction = "ok"           // records already match reality
	ActionRecord    AuditAction = "record"       // upstream has it, Purser didn't
	ActionMarkStale AuditAction = "mark-stale"   // Purser has it, upstream doesn't
	ActionAbsent    AuditAction = "absent"       // neither — nothing to do
	ActionSkipped   AuditAction = "unverifiable" // couldn't check
	ActionError     AuditAction = "error"
)

// AuditFinding is one (person × service) verdict.
type AuditFinding struct {
	Person      model.Person
	ServiceKey  string
	DisplayName string
	Recorded    bool // Purser holds an *active* account row
	Upstream    UpstreamState
	Action      AuditAction
	Applied     bool   // a write actually happened
	Err         string // set when Action == ActionError
}

// AuditRequest scopes and arms an audit.
type AuditRequest struct {
	// Email limits the audit to one person. Empty audits everyone.
	Email string
	// Services limits which connectors are checked. Empty checks all registered.
	Services []string
	// Apply turns the report into writes. False is a pure dry run — no upstream
	// calls mutate anything either, since Reconcile is read-only by contract.
	Apply bool
}

// AuditResult is the full report.
type AuditResult struct {
	Findings []AuditFinding
	Applied  bool
}

// Counts summarizes findings by action, for a one-line summary.
func (r *AuditResult) Counts() map[AuditAction]int {
	out := map[AuditAction]int{}
	for _, f := range r.Findings {
		out[f.Action]++
	}
	return out
}

// Audit compares Purser's records against upstream reality for every
// (person × service) in scope, and — with Apply — repairs the records.
//
// It never provisions and never mints: every upstream call goes through
// Reconcile, which is read-only by contract. That's what makes this safe to run
// against real people, and it's the difference between this and a re-invite,
// which would hand Switchyard users a second API token (PRSR-15).
//
// Repairs are records-only, in two directions:
//
//	upstream has it, Purser doesn't  → write an account row (no secret)
//	Purser has it, upstream doesn't  → mark the row stale, re-arming provisioning
//
// A service the connector can't check is reported unverifiable and left alone.
func (s *Service) Audit(ctx context.Context, req AuditRequest) (*AuditResult, error) {
	astore, ok := s.store.(AuditStore)
	if !ok {
		return nil, errors.New("invite: the configured store does not support auditing")
	}

	people, err := s.auditScope(ctx, astore, req)
	if err != nil {
		return nil, err
	}
	conns, err := s.auditConnectors(req)
	if err != nil {
		return nil, err
	}

	res := &AuditResult{Applied: req.Apply}
	for _, p := range people {
		for _, conn := range conns {
			f, err := s.auditOne(ctx, astore, p, conn, req.Apply)
			if err != nil {
				return nil, err // infrastructure (DB) error, not a connector one
			}
			res.Findings = append(res.Findings, f)
		}
	}
	return res, nil
}

// auditScope resolves which people to audit.
func (s *Service) auditScope(ctx context.Context, astore AuditStore, req AuditRequest) ([]model.Person, error) {
	if email := strings.TrimSpace(req.Email); email != "" {
		p, err := s.store.PersonByEmail(ctx, email)
		if errors.Is(err, store.ErrNotFound) {
			// Same sentinel `person show` returns, and the same text it always
			// had: one meaning of "that address is not on the roster", so a
			// caller can offer `person add` without matching on a message.
			return nil, fmt.Errorf("%w %q", ErrPersonNotFound, email)
		}
		if err != nil {
			return nil, err
		}
		return []model.Person{p}, nil
	}
	return astore.ListPeople(ctx)
}

// auditConnectors resolves which connectors to check, in stable key order.
func (s *Service) auditConnectors(req AuditRequest) ([]connector.Connector, error) {
	if len(req.Services) == 0 {
		keys := s.registry.Keys()
		sort.Strings(keys)
		conns := make([]connector.Connector, 0, len(keys))
		for _, k := range keys {
			c, _ := s.registry.Get(k)
			conns = append(conns, c)
		}
		return conns, nil
	}
	conns := make([]connector.Connector, 0, len(req.Services))
	for _, k := range req.Services {
		c, ok := s.registry.Get(k)
		if !ok {
			return nil, unknownServiceError(k, s.registry.Keys())
		}
		conns = append(conns, c)
	}
	return conns, nil
}

// auditOne reconciles a single (person × service).
func (s *Service) auditOne(ctx context.Context, astore AuditStore, p model.Person, conn connector.Connector, apply bool) (AuditFinding, error) {
	f := AuditFinding{
		Person: p, ServiceKey: conn.Key(), DisplayName: conn.DisplayName(),
	}

	svc, err := s.store.ServiceByKey(ctx, conn.Key())
	if err != nil {
		return f, err
	}
	acct, acctErr := s.store.AccountFor(ctx, p.ID, svc.ID)
	if acctErr != nil && !errors.Is(acctErr, store.ErrNotFound) {
		return f, acctErr
	}
	hasRow := acctErr == nil
	f.Recorded = hasRow && acct.Status == model.AccountActive

	rec, rerr := conn.Reconcile(ctx, connector.Input{PersonName: p.Name, Email: p.Email})
	switch {
	case errors.Is(rerr, connector.ErrReconcileUnsupported), errors.Is(rerr, connector.ErrPending):
		f.Upstream, f.Action, f.Err = UpstreamUnknown, ActionSkipped, rerr.Error()
		return f, nil
	case rerr != nil:
		f.Upstream, f.Action, f.Err = UpstreamUnknown, ActionError, rerr.Error()
		return f, nil
	}

	if rec.Exists {
		f.Upstream = UpstreamYes
		if f.Recorded {
			f.Action = ActionNone
			return f, nil
		}
		f.Action = ActionRecord
		if apply {
			// No secret: the person keeps whatever credential they already have,
			// and Purser never learned it. secret_hash stays empty rather than
			// hashing something we don't possess.
			if _, err := s.store.UpsertAccount(ctx, model.Account{
				PersonID:   p.ID,
				ServiceID:  svc.ID,
				ExternalID: rec.ExternalID,
				Username:   rec.Username,
				SecretHash: "",
				Status:     model.AccountActive,
			}); err != nil {
				return f, err
			}
			f.Applied = true
		}
		return f, nil
	}

	f.Upstream = UpstreamNo
	if !f.Recorded {
		f.Action = ActionAbsent
		return f, nil
	}
	// Purser says active, upstream says gone. Marking it stale is what lets a
	// future invite re-provision — while active, the orchestrator skips forever.
	f.Action = ActionMarkStale
	if apply {
		if err := astore.UpdateAccountStatus(ctx, acct.ID, model.AccountStale); err != nil {
			return f, err
		}
		f.Applied = true
	}
	return f, nil
}
