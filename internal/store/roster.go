package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/model"
)

// AccountRecord is one account joined to the service it belongs to — what the
// roster commands read (PRSR-24).
//
// It is deliberately not model.Account. There is no SecretHash and no SecretRef
// field, and the queries below do not select those columns. Credentials are
// shown exactly once, at invite time, by design; a roster view that re-surfaced
// even the hash would weaken that, and `--json` serializes whatever the struct
// happens to hold. Leaving the columns out of the type is what makes "the roster
// cannot print a secret" a question you answer by reading one type, instead of
// by auditing every renderer and every DTO that will ever wrap it.
type AccountRecord struct {
	// ID is the account row's own id, carried so a caller acting on the record
	// (offboard marking it deprovisioned) doesn't have to re-read the row it
	// was just handed.
	ID          uuid.UUID
	PersonID    uuid.UUID
	ServiceKey  string
	DisplayName string
	ExternalID  string
	Username    string
	Status      model.AccountStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// DeprovisionedAt is when access was last revoked, or nil if it never was.
	// Never cleared by a re-provision — see migration 0006.
	DeprovisionedAt *time.Time
}

// AccountRecords returns every account on the roster, joined to its service,
// ordered by person and then service key.
func (s *Store) AccountRecords(ctx context.Context) ([]AccountRecord, error) {
	return s.accountRecords(ctx, nil)
}

// AccountRecordsFor returns one person's accounts, joined to their services.
func (s *Store) AccountRecordsFor(ctx context.Context, personID uuid.UUID) ([]AccountRecord, error) {
	return s.accountRecords(ctx, &personID)
}

// accountRecords is the shared body of the two above: one statement, so the
// whole-roster read and the single-person read cannot drift on which columns
// they select — which is the property the secret-free type depends on.
//
// A nil personID means the whole roster. Note the join is an inner one: an
// account always has a service row (FK, and the table is seeded from the
// registry on boot), so this drops nothing.
func (s *Store) accountRecords(ctx context.Context, personID *uuid.UUID) ([]AccountRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.person_id, s.key, s.display_name, a.external_id, a.username,
		       a.status, a.created_at, a.updated_at, a.deprovisioned_at
		FROM account a
		JOIN service s ON s.id = a.service_id
		WHERE $1::uuid IS NULL OR a.person_id = $1
		ORDER BY a.person_id, s.key`, personID)
	if err != nil {
		return nil, fmt.Errorf("store: account records: %w", err)
	}
	defer rows.Close()

	var out []AccountRecord
	for rows.Next() {
		var a AccountRecord
		if err := rows.Scan(&a.ID, &a.PersonID, &a.ServiceKey, &a.DisplayName, &a.ExternalID,
			&a.Username, &a.Status, &a.CreatedAt, &a.UpdatedAt, &a.DeprovisionedAt); err != nil {
			return nil, fmt.Errorf("store: scan account record: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListServices returns every seeded service, by key. The roster validates
// `--to` against this rather than against the connector registry: it is a view
// of records, so the services it knows about are the ones records can name.
func (s *Store) ListServices(ctx context.Context) ([]model.Service, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, key, display_name, created_at FROM service ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("store: list services: %w", err)
	}
	defer rows.Close()

	var out []model.Service
	for rows.Next() {
		var svc model.Service
		if err := rows.Scan(&svc.ID, &svc.Key, &svc.DisplayName, &svc.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan service: %w", err)
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

// InvitesFor returns a person's invite history, newest first. Uncapped: an
// invite row is one `purser invite` run, so the history is short, and silently
// truncating it would make "how often has this been re-run?" — the question the
// history is here to answer — quietly wrong.
func (s *Store) InvitesFor(ctx context.Context, personID uuid.UUID) ([]model.Invite, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, person_id, delivery, role, delivered_at, created_at
		FROM invite WHERE person_id = $1 ORDER BY created_at DESC`, personID)
	if err != nil {
		return nil, fmt.Errorf("store: invites for: %w", err)
	}
	defer rows.Close()

	var out []model.Invite
	for rows.Next() {
		var inv model.Invite
		if err := rows.Scan(&inv.ID, &inv.PersonID, &inv.Delivery, &inv.Role,
			&inv.DeliveredAt, &inv.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan invite: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}
