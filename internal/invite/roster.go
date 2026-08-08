package invite

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/store"
)

// The roster reads records and nothing else (PRSR-24).
//
// Neither function below touches s.registry, and that is the point rather than
// an accident of implementation. "Who is on the roster, and what does each
// person have?" is a question Purser is the system of record for, so answering
// it must not depend on every upstream service being reachable — an operator
// asking what Paul already holds, in order to match it, gets an answer while
// Cloudflare is down. `audit` is the command that compares records against
// upstream; this is the one that reads the records.
//
// The other half of that is going *around* Purser: before these existed the
// only way to ask was psql, which needs schema knowledge, bypasses every
// invariant the CLI enforces, and sits one typo away from an UPDATE against
// live provisioning records.

// ErrPersonNotFound reports that no person holds the given address. Callers
// match on it to offer `person add` rather than treating it as a failure.
var ErrPersonNotFound = errors.New("invite: no person with email")

// RosterStore is the read-only persistence surface the roster needs on top of
// Store, asserted at runtime like AuditStore. Every method is a SELECT: there is
// no write anywhere in this file, and nothing here should ever acquire one.
type RosterStore interface {
	ListPeople(ctx context.Context) ([]model.Person, error)
	ListServices(ctx context.Context) ([]model.Service, error)
	AccountRecords(ctx context.Context) ([]store.AccountRecord, error)
	AccountRecordsFor(ctx context.Context, personID uuid.UUID) ([]store.AccountRecord, error)
	InvitesFor(ctx context.Context, personID uuid.UUID) ([]model.Invite, error)
}

// RosterRequest scopes `person list`.
type RosterRequest struct {
	// Services keeps only people holding one of these service keys. Empty lists
	// everyone.
	Services []string
	// Type keeps only humans or only agents. Empty lists both.
	Type model.PersonType
	// IncludeInactive shows deprovisioned and stale accounts alongside active
	// ones. They are hidden by default because the roster answers "what does
	// this person have", and a stale row is precisely what they do not have.
	IncludeInactive bool
}

// RosterEntry is one person and the accounts in scope for the request.
type RosterEntry struct {
	Person   model.Person
	Accounts []store.AccountRecord
}

// RosterResult is the whole roster in scope.
type RosterResult struct {
	Entries []RosterEntry
	// Hidden counts accounts left out for not being active. Reported so the
	// default filter can never be silent: a person whose only Lyceum account is
	// stale is absent from `person list --to lyceum` entirely, and without this
	// the empty result reads as "nobody has Lyceum" rather than "nobody has
	// Lyceum *any more*".
	Hidden           int
	IncludedInactive bool
}

// Roster answers "who is on the roster, and what does each person have?" from
// local records alone — person, account and service, no connector calls.
//
// A person with no accounts is still on the roster. That is what `person add`
// writes, and omitting them would make this command unable to see exactly the
// people that command exists to record.
func (s *Service) Roster(ctx context.Context, req RosterRequest) (*RosterResult, error) {
	rstore, ok := s.store.(RosterStore)
	if !ok {
		return nil, errors.New("invite: the configured store does not support the roster")
	}
	if req.Type != "" && req.Type != model.PersonHuman && req.Type != model.PersonAgent {
		return nil, fmt.Errorf("invite: unknown person type %q (want %s or %s)",
			req.Type, model.PersonHuman, model.PersonAgent)
	}
	want, err := rosterServices(ctx, rstore, req.Services)
	if err != nil {
		return nil, err
	}

	people, err := rstore.ListPeople(ctx)
	if err != nil {
		return nil, err
	}
	records, err := rstore.AccountRecords(ctx)
	if err != nil {
		return nil, err
	}
	// One query for every account rather than one per person: the roster is
	// small, but an N+1 that only shows up once there are people to list is a
	// poor trade for the three lines it saves.
	byPerson := make(map[uuid.UUID][]store.AccountRecord, len(people))
	for _, a := range records {
		byPerson[a.PersonID] = append(byPerson[a.PersonID], a)
	}

	res := &RosterResult{IncludedInactive: req.IncludeInactive}
	for _, p := range people {
		if req.Type != "" && p.Type != req.Type {
			continue
		}
		// matching is what the request asked about; visible is what survives the
		// status filter. The service filter below then selects on *visible*, so
		// the accounts that decide whether someone is listed are the same ones
		// the output shows — otherwise `--to switchyard` returns people with an
		// empty services column and no way to tell why.
		var matching, visible []store.AccountRecord
		for _, a := range byPerson[p.ID] {
			if want != nil && !want[a.ServiceKey] {
				continue
			}
			matching = append(matching, a)
			if req.IncludeInactive || a.Status == model.AccountActive {
				visible = append(visible, a)
			}
		}
		res.Hidden += len(matching) - len(visible)
		if want != nil && len(visible) == 0 {
			continue
		}
		res.Entries = append(res.Entries, RosterEntry{Person: p, Accounts: visible})
	}
	return res, nil
}

// rosterServices resolves --to into a set, refusing a key no service row holds.
//
// An unknown key must not quietly return an empty roster: "nobody has
// swtichyard" is a wrong answer to the question that was asked, and this whole
// command exists so that questions about who holds what stop being answered
// wrongly. Validated against the service table rather than the connector
// registry — records can name a service whose connector is no longer wired, and
// this view is about the records.
func rosterServices(ctx context.Context, rstore RosterStore, keys []string) (map[string]bool, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	services, err := rstore.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(services))
	names := make([]string, 0, len(services))
	for _, svc := range services {
		known[svc.Key] = true
		names = append(names, svc.Key)
	}
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		if !known[k] {
			return nil, fmt.Errorf("invite: unknown service %q (known: %s)", k, strings.Join(names, ", "))
		}
		want[k] = true
	}
	if len(want) == 0 {
		return nil, nil
	}
	return want, nil
}

// PersonDetail is one person in full: the row, every account whatever its
// status, and the invite history.
type PersonDetail struct {
	Person model.Person
	// Accounts is unfiltered, unlike the roster's. This is the single-person
	// view, so a deprovisioned or stale row is the interesting part rather than
	// noise — and it is shown with its status, so nothing is presented as access
	// the person still holds.
	Accounts []store.AccountRecord
	// Invites is the history, newest first. It carries no credential material:
	// the secrets an invite delivered were never persisted, only their hashes,
	// and those aren't here either.
	Invites []model.Invite
}

// PersonDetail reads everything Purser records about one person. Local only —
// no connector is called, so this answers even while upstream is unreachable.
func (s *Service) PersonDetail(ctx context.Context, rawEmail string) (*PersonDetail, error) {
	rstore, ok := s.store.(RosterStore)
	if !ok {
		return nil, errors.New("invite: the configured store does not support the roster")
	}
	// The same normalization every other command keys identity on, so `show`
	// cannot fail to find a person that `invite` would have matched.
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return nil, err
	}
	p, err := s.store.PersonByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w %q", ErrPersonNotFound, email)
	}
	if err != nil {
		return nil, err
	}
	accounts, err := rstore.AccountRecordsFor(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	invites, err := rstore.InvitesFor(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	return &PersonDetail{Person: p, Accounts: accounts, Invites: invites}, nil
}
