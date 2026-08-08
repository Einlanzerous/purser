package invite

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/store"
)

// Offboarding is the one genuinely destructive thing Purser does, so it inverts
// every default the provisioning path takes (PRSR-17).
//
// `invite` acts by default because creating access twice is merely wasteful.
// Offboard *reports* by default and acts only under Apply, because the mistake it
// can make — revoking the wrong person — is not fixed by running it again. The
// dry run and the apply walk the same code path, so what the preview lists is
// exactly what the apply does; that is the same property `audit`/`reconcile`
// have, and for the same reason.
//
// It is per-person by construction: there is no "offboard everyone" mode, and
// Email is required. `reconcile --all` needed a flag to make a bulk write
// deliberate; here the bulk case simply doesn't exist, which is a stronger
// version of the same guard.
//
// What it does *not* do is delete anything of Purser's. The `account` row is
// marked deprovisioned and kept, so the record of what someone once held — and
// when it was taken away — survives the offboarding. Deleting the row would
// destroy exactly the history an audit exists to read, and would silently re-arm
// provisioning: the orchestrator's idempotency skip keys on an *active* account,
// so a missing row and a deprovisioned one mean opposite things to the next
// invite.

// OffboardAction is what happened to one service, or would happen under Apply.
type OffboardAction string

const (
	// ActionRevoke — Purser holds an active account and the connector can act.
	ActionRevoke OffboardAction = "revoke"
	// ActionNothingToDo — no active account here; nothing to take away.
	ActionNothingToDo OffboardAction = "nothing-to-do"
	// ActionUnavailable — the connector cannot revoke (connector.ErrPending).
	// Deliberately distinct from failure: nothing broke, nobody built it yet, and
	// the access is still there and needs a human. Mirrors model.TaskUnavailable.
	ActionUnavailable OffboardAction = "unavailable"
	// ActionFailed — the connector tried and errored. The account row is left
	// active, because recording a revoke that didn't happen is the one outcome
	// worse than the failure itself.
	ActionFailed OffboardAction = "failed"
	// ActionRevokedNotRecorded — the connector revoked and the database write
	// that records it did not land.
	//
	// Its own state, not a flavour of failed, because the two point opposite
	// ways: failed means access is probably still live, this means it is
	// definitely gone and Purser's records disagree. Telling an operator to
	// "check whether they still have access" when the answer is no wastes the
	// one thing the report is for. Re-running fixes it — the revoke is
	// idempotent — so the advice differs too.
	ActionRevokedNotRecorded OffboardAction = "revoked-not-recorded"
)

// OffboardFinding is one service's verdict.
type OffboardFinding struct {
	ServiceKey  string
	DisplayName string
	Username    string
	ExternalID  string
	Action      OffboardAction
	// Applied reports that the connector actually revoked and the record was
	// updated. False on every dry run, and on any Apply that didn't get that far.
	Applied bool
	Err     string
}

// OffboardRequest scopes an offboard.
type OffboardRequest struct {
	// Email identifies the person. Required — there is no bulk mode.
	Email string
	// Services limits which services are revoked. Empty means every service the
	// person holds an active account in, so partial offboarding ("drop Argosy,
	// keep Lyceum") is expressible.
	Services []string
	// Apply turns the report into revocations. False is a pure dry run: no
	// connector call that mutates, no record written.
	Apply bool
}

// OffboardResult is the whole report.
type OffboardResult struct {
	Person   model.Person
	Findings []OffboardFinding
	Applied  bool
	// SkippedActive names services the person still holds an *active* account in
	// that --to excluded, so a scoped run can say what it left behind. A finding
	// cannot carry this: an out-of-scope service produces no finding at all, so
	// "not in the table" is ambiguous between "they never had it" and "you
	// scoped it out" — and those need opposite advice.
	SkippedActive []string
}

// Counts summarizes findings by action, for a one-line summary.
func (r *OffboardResult) Counts() map[OffboardAction]int {
	out := map[OffboardAction]int{}
	for _, f := range r.Findings {
		out[f.Action]++
	}
	return out
}

// Revoked reports whether anything was actually taken away.
func (r *OffboardResult) Revoked() int {
	n := 0
	for _, f := range r.Findings {
		if f.Applied {
			n++
		}
	}
	return n
}

// Offboard revokes a person's access across the services they hold, or — by
// default — reports what it would revoke.
//
// The unit of work is the `account` row, not the connector list: it acts on what
// Purser recorded this person as having, so a service they never held is not
// called at all. That is the difference from `audit`, which asks every connector
// about everyone; here an unnecessary call is an unnecessary write.
func (s *Service) Offboard(ctx context.Context, req OffboardRequest) (*OffboardResult, error) {
	rstore, ok := s.store.(RosterStore)
	if !ok {
		return nil, errors.New("invite: the configured store does not support offboarding")
	}
	astore, ok := s.store.(AuditStore)
	if !ok {
		return nil, errors.New("invite: the configured store cannot update account status")
	}

	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return nil, err
	}
	person, err := s.store.PersonByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w %q", ErrPersonNotFound, email)
	}
	if err != nil {
		return nil, err
	}

	want, err := rosterServices(ctx, rstore, req.Services)
	if err != nil {
		return nil, err
	}
	accounts, err := rstore.AccountRecordsFor(ctx, person.ID)
	if err != nil {
		return nil, err
	}

	res := &OffboardResult{Person: person, Applied: req.Apply}
	for _, a := range accounts {
		if want != nil && !want[a.ServiceKey] {
			if a.Status == model.AccountActive {
				res.SkippedActive = append(res.SkippedActive, a.ServiceKey)
			}
			continue
		}
		// offboardOne never returns an error: once a revoke has landed upstream,
		// aborting would discard every finding accumulated so far and leave the
		// operator with one error line and no idea what was already taken away.
		// Failures are folded into the finding instead, so the report always
		// survives to be printed.
		res.Findings = append(res.Findings, s.offboardOne(ctx, astore, person, a, req.Apply))
	}

	// A service named explicitly but never held still gets a line. Silence would
	// read as "revoked" to an operator who asked for it by name — the report has
	// to answer the question that was put to it, not only the parts with rows.
	res.Findings = append(res.Findings, unheldFindings(want, accounts, s.registry)...)
	sort.Slice(res.Findings, func(i, j int) bool {
		return res.Findings[i].ServiceKey < res.Findings[j].ServiceKey
	})
	return res, nil
}

// offboardOne revokes a single service, or reports what it would revoke.
//
// It returns no error by design: every outcome is a finding, including the ones
// that would traditionally abort. See the call site.
func (s *Service) offboardOne(ctx context.Context, astore AuditStore, p model.Person, a store.AccountRecord, apply bool) OffboardFinding {
	f := OffboardFinding{
		ServiceKey: a.ServiceKey, DisplayName: a.DisplayName,
		Username: a.Username, ExternalID: a.ExternalID,
	}

	// Only an active account is access. A stale row means upstream already lost
	// it, and a deprovisioned one means this already ran — revoking either would
	// be a connector call that cannot change anything.
	if a.Status != model.AccountActive {
		f.Action = ActionNothingToDo
		return f
	}

	conn, ok := s.registry.Get(a.ServiceKey)
	if !ok {
		// A record naming a connector this build doesn't have. Not a failure of
		// anything that ran — but emphatically not "nothing to do" either, since
		// the access is real and Purser can't reach it.
		f.Action = ActionUnavailable
		f.Err = fmt.Sprintf("no connector registered for %q in this build", a.ServiceKey)
		return f
	}

	// Ask before promising. Argosy's Deprovision is an unconditional refusal and
	// so is every Unavailable stub, so a preview that said "revoke" here would
	// promise what --apply then declines — breaking the one property that makes a
	// preview worth having on a command you cannot take back. CanDeprovision
	// answers from config and capability alone, contacting nothing.
	if err := connector.CanDeprovision(conn); err != nil {
		f.Action, f.Err = ActionUnavailable, err.Error()
		return f
	}

	f.Action = ActionRevoke
	if !apply {
		return f // dry run: no connector call at all
	}

	err := conn.Deprovision(ctx, connector.Input{
		PersonName: p.Name,
		Email:      p.Email,
		// The id Purser recorded, so the revoke targets the account it
		// provisioned rather than whatever a fresh lookup finds.
		ExternalID: a.ExternalID,
	})
	switch {
	case connector.IsUnavailable(err):
		f.Action, f.Err = ActionUnavailable, err.Error()
		return f
	case err != nil:
		// The record stays active. Marking it deprovisioned here would tell every
		// later reader — the audit, `person show`, the next invite's skip — that
		// access was removed when it wasn't, and that lie survives long after the
		// error message scrolls away.
		f.Action, f.Err = ActionFailed, err.Error()
		return f
	}

	// The account id came back with the record, so there is no re-read here.
	if err := astore.UpdateAccountStatus(ctx, a.ID, model.AccountDeprovisioned); err != nil {
		// Upstream access is gone; only the bookkeeping failed. Reported as its
		// own state rather than as a failure, and emphatically not swallowed: the
		// row still reads active, so every later reader thinks they have access
		// they don't. Re-running fixes it, since the revoke is idempotent.
		f.Action = ActionRevokedNotRecorded
		f.Err = err.Error()
		return f
	}
	f.Applied = true
	return f
}

// unheldFindings reports the explicitly-named services the person holds no
// account for, so `--to argosy` on someone who never had Argosy says so.
func unheldFindings(want map[string]bool, held []store.AccountRecord, reg *connector.Registry) []OffboardFinding {
	if want == nil {
		return nil
	}
	seen := make(map[string]bool, len(held))
	for _, a := range held {
		seen[a.ServiceKey] = true
	}
	var out []OffboardFinding
	for key := range want {
		if seen[key] {
			continue
		}
		display := key
		if c, ok := reg.Get(key); ok {
			display = c.DisplayName()
		}
		out = append(out, OffboardFinding{
			ServiceKey: key, DisplayName: display, Action: ActionNothingToDo,
		})
	}
	return out
}

// RenderOffboardNote summarizes an offboard for the operator — what is still
// open, and what to do about it.
//
// There is no recipient-facing counterpart on purpose. Nothing here is ever
// emailed to the person being offboarded, so unlike the invite path there is no
// audience split to maintain (PRSR-19); this text is for whoever ran the command.
func RenderOffboardNote(res *OffboardResult) string {
	var b strings.Builder
	c := res.Counts()

	if n := c[ActionUnavailable]; n > 0 {
		b.WriteString("Still has access — Purser cannot revoke these:\n")
		for _, f := range res.Findings {
			if f.Action == ActionUnavailable {
				fmt.Fprintf(&b, "  - %s: %s\n", f.DisplayName, f.Err)
			}
		}
		b.WriteString("Remove them by hand.\n")
	}
	if c[ActionFailed] > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Failed — access may still be live, records left untouched:\n")
		for _, f := range res.Findings {
			if f.Action == ActionFailed {
				fmt.Fprintf(&b, "  - %s: %s\n", f.DisplayName, f.Err)
			}
		}
		b.WriteString("Re-run to retry; it acts only on what is still active.\n")
	}
	if c[ActionRevokedNotRecorded] > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		// Deliberately the opposite advice from the failure block above: the
		// access is gone, and it is Purser's own records that are now wrong.
		b.WriteString("Revoked upstream, but NOT recorded — access is gone, Purser's records still say active:\n")
		for _, f := range res.Findings {
			if f.Action == ActionRevokedNotRecorded {
				fmt.Fprintf(&b, "  - %s: %s\n", f.DisplayName, f.Err)
			}
		}
		b.WriteString("Re-run to fix the records; the revoke itself is idempotent.\n")
	}
	return b.String()
}
