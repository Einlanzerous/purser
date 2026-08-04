package invite

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/Einlanzerous/purser/internal/model"
)

// ErrNameConflict reports that the email is already recorded under a different
// name and the caller did not ask for a rename. Callers match on it to offer
// the rename rather than treating it as a hard failure.
var ErrNameConflict = errors.New("invite: person already recorded under a different name")

// ErrTypeConflict reports that the email is already recorded as the other kind
// of identity. There is no flag to override it: human and agent are different
// things, so disagreeing about which one this is means the caller is not
// talking about the person in the table.
var ErrTypeConflict = errors.New("invite: person already recorded as a different type")

// AddPersonRequest records that someone exists, without provisioning them.
type AddPersonRequest struct {
	Name  string
	Email string // required — the identity key, and what the audit looks up
	// Type is the identity kind. Empty means "unspecified": a new person
	// defaults to human, and an existing one keeps whatever it is rather than
	// being silently converted.
	Type model.PersonType
	// Rename permits changing the stored name of an existing person. Without
	// it, a name that disagrees with the record is an error rather than a
	// silent edit.
	Rename bool
}

// AddPersonResult reports what the add actually did. Every field is derived
// from the row the write returned, never from the read that preceded it.
type AddPersonResult struct {
	Person       model.Person
	Created      bool   // a new row; false means the email was already known
	Renamed      bool   // an existing person's name was changed (needs Rename)
	PreviousName string // the name this replaced
}

// AddPerson records a person and nothing else: no account rows, no connector
// calls, no credentials (PRSR-16).
//
// It exists because the audit can only act on people Purser already knows — it
// walks the person table — so anyone provisioned outside Purser stays invisible
// to it until a row exists, and the audit then reports "0 errors" while
// silently omitting them. Before this the only ways to make that row were an
// invite, which tries to create accounts the person may already hold, and
// hand-written SQL, where a mistyped email mints a duplicate identity that
// reconcile afterwards dutifully populates.
//
// Every path that would modify an existing person refuses by default. The
// create is a single conditional insert, so a concurrent add loses the race
// cleanly instead of producing a second row, and the rename is a single
// targeted update — the outcome reported is the one the database performed.
func (s *Service) AddPerson(ctx context.Context, req AddPersonRequest) (*AddPersonResult, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, errors.New("invite: name is required")
	}
	email, err := NormalizeEmail(req.Email)
	if err != nil {
		return nil, err
	}
	if req.Type != "" && req.Type != model.PersonHuman && req.Type != model.PersonAgent {
		return nil, fmt.Errorf("invite: unknown person type %q (want %s or %s)",
			req.Type, model.PersonHuman, model.PersonAgent)
	}

	newType := req.Type
	if newType == "" {
		newType = model.PersonHuman
	}
	existing, created, err := s.findOrCreate(ctx, name, email, newType)
	if err != nil {
		return nil, err
	}
	if created {
		return &AddPersonResult{Person: existing, Created: true}, nil
	}

	// The address is taken. Everything below is a decision about a person who
	// already exists, and the default for all of them is to refuse and say so.
	if req.Type != "" && req.Type != existing.Type {
		return nil, fmt.Errorf("%w: %s is recorded as %s, not %s",
			ErrTypeConflict, email, existing.Type, req.Type)
	}
	if sameName(existing.Name, name) {
		return &AddPersonResult{Person: existing}, nil // nothing to write
	}
	if !req.Rename {
		return nil, fmt.Errorf("%w: %s is recorded as %q, not %q",
			ErrNameConflict, email, existing.Name, name)
	}

	renamed, previous, err := s.store.RenamePerson(ctx, email, name)
	if err != nil {
		return nil, err
	}
	// previous comes back from the update itself, so a rename that raced
	// another writer reports what it actually replaced — including the case
	// where that turns out to be nothing.
	return &AddPersonResult{
		Person:       renamed,
		Renamed:      previous != name,
		PreviousName: previous,
	}, nil
}

// NormalizeEmail lowercases, trims, and checks the address is well-formed.
//
// It cannot catch a plausible typo — ada@exmaple.com parses perfectly — so it
// is not a defense against the wrong person being added. What it does stop is a
// fragment or a display-name form ("Ada <ada@…>") becoming a person's identity
// key, since everything downstream treats that string as an address: the SSO
// join key upstream, and the audit's lookup here.
//
// Exported so the CLI can reject a malformed address before opening a database
// connection, without keeping a second copy of the rule.
//
// Both entry points call it — `person add` and, since PRSR-23, `invite` — so the
// "required" half is worded for either. They agree on what an identity key is
// because there is one function that decides.
func NormalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" {
		return "", errors.New("invite: email is required — it is the person's identity key")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", fmt.Errorf("invite: %q is not a valid email address", strings.TrimSpace(raw))
	}
	return email, nil
}
