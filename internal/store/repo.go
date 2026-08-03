package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Einlanzerous/purser/internal/model"
)

// ErrNotFound is returned by lookups when no row matches.
var ErrNotFound = errors.New("store: not found")

// InsertPersonIfAbsent inserts a person and reports whether it created the row.
// It never touches an existing one — no rename, no updated_at bump — which is
// what lets `person add` treat an occupied email as a decision to refuse rather
// than an edit to apply (PRSR-16), and `invite` report the disagreement rather
// than act on it (PRSR-20).
//
// It is the only way to create a person. There used to be an UpsertPerson
// alongside it whose ON CONFLICT set the name, which is how a mistyped --name
// silently renamed someone (PRSR-20); by PRSR-23 its last caller was gone, and
// it is deleted rather than kept for a future one. A row can now only be created
// here and renamed by RenamePerson, so "which statements can change a name" is a
// question the package answers by grep.
//
// The conflict target is lower(email), matching person_email_key as rebuilt by
// migration 0003 and the case-insensitive lookups below. Inferring on the bare
// column instead would miss a differently-cased row and insert a duplicate
// identity for the same human (PRSR-16).
//
// It is a single statement resolved by the unique index, so two concurrent adds
// cannot both create the same person: the loser is told created=false and
// re-reads.
func (s *Store) InsertPersonIfAbsent(ctx context.Context, name, email string, typ model.PersonType) (model.Person, bool, error) {
	if email == "" {
		// Without an email there is no conflict target — person_email_key is
		// partial on email IS NOT NULL — so this would insert unconditionally, and
		// every call would mint another identity for the same human (PRSR-23).
		return model.Person{}, false, errors.New("store: insert person: email is required")
	}
	var p model.Person
	err := s.pool.QueryRow(ctx, `
		INSERT INTO person (name, email, type) VALUES ($1, $2, $3)
		ON CONFLICT (lower(email)) WHERE email IS NOT NULL DO NOTHING
		RETURNING id, name, COALESCE(email, ''), type, created_at, updated_at`,
		name, email, string(typ)).
		Scan(&p.ID, &p.Name, &p.Email, &p.Type, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Person{}, false, nil // taken — the caller decides what that means
	}
	if err != nil {
		return model.Person{}, false, fmt.Errorf("store: insert person: %w", err)
	}
	return p, true, nil
}

// RenamePerson changes an existing person's name, matched case-insensitively on
// email. It returns the updated row and the name it replaced, both read inside
// the same statement.
//
// Returning them rather than trusting an earlier SELECT is the point: a caller
// that infers "renamed X to Y" from a prior lookup will happily report a rename
// that landed on a different row, or on no row at all.
func (s *Store) RenamePerson(ctx context.Context, email, name string) (model.Person, string, error) {
	var p model.Person
	var previous string
	err := s.pool.QueryRow(ctx, `
		UPDATE person p SET name = $2, updated_at = now()
		FROM person old
		WHERE old.id = p.id AND lower(p.email) = lower($1)
		RETURNING old.name, p.id, p.name, COALESCE(p.email, ''), p.type, p.created_at, p.updated_at`,
		email, name).
		Scan(&previous, &p.ID, &p.Name, &p.Email, &p.Type, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Person{}, "", ErrNotFound
	}
	if err != nil {
		return model.Person{}, "", fmt.Errorf("store: rename person: %w", err)
	}
	return p, previous, nil
}

// EnsureService inserts the service if absent and returns the row. Used to seed
// the service table from the connector registry on boot.
func (s *Store) EnsureService(ctx context.Context, key, displayName string) (model.Service, error) {
	var svc model.Service
	err := s.pool.QueryRow(ctx, `
		INSERT INTO service (key, display_name) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING id, key, display_name, created_at`,
		key, displayName).
		Scan(&svc.ID, &svc.Key, &svc.DisplayName, &svc.CreatedAt)
	if err != nil {
		return model.Service{}, fmt.Errorf("store: ensure service: %w", err)
	}
	return svc, nil
}

// ServiceByKey returns the service row for a connector key.
func (s *Store) ServiceByKey(ctx context.Context, key string) (model.Service, error) {
	var svc model.Service
	err := s.pool.QueryRow(ctx, `
		SELECT id, key, display_name, created_at FROM service WHERE key = $1`, key).
		Scan(&svc.ID, &svc.Key, &svc.DisplayName, &svc.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Service{}, ErrNotFound
	}
	if err != nil {
		return model.Service{}, fmt.Errorf("store: service by key: %w", err)
	}
	return svc, nil
}

// CreateInvite records a new provisioning run.
func (s *Store) CreateInvite(ctx context.Context, personID uuid.UUID, delivery model.DeliveryMethod, role string) (model.Invite, error) {
	var inv model.Invite
	err := s.pool.QueryRow(ctx, `
		INSERT INTO invite (person_id, delivery, role) VALUES ($1, $2, $3)
		RETURNING id, person_id, delivery, role, delivered_at, created_at`,
		personID, string(delivery), role).
		Scan(&inv.ID, &inv.PersonID, &inv.Delivery, &inv.Role, &inv.DeliveredAt, &inv.CreatedAt)
	if err != nil {
		return model.Invite{}, fmt.Errorf("store: create invite: %w", err)
	}
	return inv, nil
}

// MarkInviteDelivered stamps the invite's delivered_at.
func (s *Store) MarkInviteDelivered(ctx context.Context, inviteID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE invite SET delivered_at = now() WHERE id = $1`, inviteID)
	if err != nil {
		return fmt.Errorf("store: mark invite delivered: %w", err)
	}
	return nil
}

// AccountFor returns the durable account for (person, service), or ErrNotFound.
func (s *Store) AccountFor(ctx context.Context, personID, serviceID uuid.UUID) (model.Account, error) {
	var a model.Account
	err := s.pool.QueryRow(ctx, `
		SELECT id, person_id, service_id, external_id, username, secret_hash, secret_ref, status, created_at, updated_at
		FROM account WHERE person_id = $1 AND service_id = $2`, personID, serviceID).
		Scan(&a.ID, &a.PersonID, &a.ServiceID, &a.ExternalID, &a.Username,
			&a.SecretHash, &a.SecretRef, &a.Status, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Account{}, ErrNotFound
	}
	if err != nil {
		return model.Account{}, fmt.Errorf("store: account for: %w", err)
	}
	return a, nil
}

// ListPeople returns every person, oldest first. Used by the audit to walk the
// whole roster (PRSR-15).
func (s *Store) ListPeople(ctx context.Context) ([]model.Person, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, COALESCE(email, ''), type, created_at, updated_at
		FROM person ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list people: %w", err)
	}
	defer rows.Close()
	var out []model.Person
	for rows.Next() {
		var p model.Person
		if err := rows.Scan(&p.ID, &p.Name, &p.Email, &p.Type, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan person: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PersonByEmail looks a person up by email (lowercased), or ErrNotFound.
func (s *Store) PersonByEmail(ctx context.Context, email string) (model.Person, error) {
	var p model.Person
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, COALESCE(email, ''), type, created_at, updated_at
		FROM person WHERE lower(email) = lower($1)`, email).
		Scan(&p.ID, &p.Name, &p.Email, &p.Type, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Person{}, ErrNotFound
	}
	if err != nil {
		return model.Person{}, fmt.Errorf("store: person by email: %w", err)
	}
	return p, nil
}

// UpdateAccountStatus changes only an account's status, deliberately leaving
// secret_hash and identity columns untouched — marking a row stale must not
// destroy the record of what was provisioned (PRSR-15).
func (s *Store) UpdateAccountStatus(ctx context.Context, accountID uuid.UUID, status model.AccountStatus) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE account SET status = $2, updated_at = now() WHERE id = $1`,
		accountID, string(status))
	if err != nil {
		return fmt.Errorf("store: update account status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpsertAccount writes the durable access record for (person, service),
// updating external_id/username/secret_hash/status on conflict. Returns the
// stored row (with its id).
func (s *Store) UpsertAccount(ctx context.Context, a model.Account) (model.Account, error) {
	var out model.Account
	err := s.pool.QueryRow(ctx, `
		INSERT INTO account (person_id, service_id, external_id, username, secret_hash, secret_ref, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (person_id, service_id) DO UPDATE SET
			external_id = EXCLUDED.external_id,
			username    = EXCLUDED.username,
			secret_hash = EXCLUDED.secret_hash,
			secret_ref  = EXCLUDED.secret_ref,
			status      = EXCLUDED.status,
			updated_at  = now()
		RETURNING id, person_id, service_id, external_id, username, secret_hash, secret_ref, status, created_at, updated_at`,
		a.PersonID, a.ServiceID, a.ExternalID, a.Username, a.SecretHash, a.SecretRef, string(a.Status)).
		Scan(&out.ID, &out.PersonID, &out.ServiceID, &out.ExternalID, &out.Username,
			&out.SecretHash, &out.SecretRef, &out.Status, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return model.Account{}, fmt.Errorf("store: upsert account: %w", err)
	}
	return out, nil
}

// EnsureTask returns the provision_task for (invite, service), creating it if
// absent. This lets a re-run reuse an invite's tasks.
func (s *Store) EnsureTask(ctx context.Context, inviteID, personID, serviceID uuid.UUID) (model.ProvisionTask, error) {
	var t model.ProvisionTask
	err := s.pool.QueryRow(ctx, `
		INSERT INTO provision_task (invite_id, person_id, service_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (invite_id, service_id) DO UPDATE SET updated_at = now()
		RETURNING id, invite_id, person_id, service_id, account_id, status, attempts, last_error, created_at, updated_at`,
		inviteID, personID, serviceID).
		Scan(&t.ID, &t.InviteID, &t.PersonID, &t.ServiceID, &t.AccountID,
			&t.Status, &t.Attempts, &t.LastError, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return model.ProvisionTask{}, fmt.Errorf("store: ensure task: %w", err)
	}
	return t, nil
}

// UpdateTask persists a task's status, attempt count, last error and account.
func (s *Store) UpdateTask(ctx context.Context, t model.ProvisionTask) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE provision_task
		SET status = $2, attempts = $3, last_error = $4, account_id = $5, updated_at = now()
		WHERE id = $1`,
		t.ID, string(t.Status), t.Attempts, t.LastError, t.AccountID)
	if err != nil {
		return fmt.Errorf("store: update task: %w", err)
	}
	return nil
}

// InviteWithTasks loads an invite and its tasks for the status endpoint.
func (s *Store) InviteWithTasks(ctx context.Context, inviteID uuid.UUID) (model.Invite, []model.ProvisionTask, error) {
	var inv model.Invite
	err := s.pool.QueryRow(ctx, `
		SELECT id, person_id, delivery, role, delivered_at, created_at FROM invite WHERE id = $1`, inviteID).
		Scan(&inv.ID, &inv.PersonID, &inv.Delivery, &inv.Role, &inv.DeliveredAt, &inv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Invite{}, nil, ErrNotFound
	}
	if err != nil {
		return model.Invite{}, nil, fmt.Errorf("store: get invite: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, invite_id, person_id, service_id, account_id, status, attempts, last_error, created_at, updated_at
		FROM provision_task WHERE invite_id = $1 ORDER BY created_at`, inviteID)
	if err != nil {
		return model.Invite{}, nil, fmt.Errorf("store: list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []model.ProvisionTask
	for rows.Next() {
		var t model.ProvisionTask
		if err := rows.Scan(&t.ID, &t.InviteID, &t.PersonID, &t.ServiceID, &t.AccountID,
			&t.Status, &t.Attempts, &t.LastError, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return model.Invite{}, nil, err
		}
		tasks = append(tasks, t)
	}
	return inv, tasks, rows.Err()
}
