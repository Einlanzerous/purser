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

// UpsertPerson inserts a person, or returns the existing one matched by email.
// When email is empty (agent identities), it always inserts a new row. Email is
// lowercased by the caller; name updates are applied on conflict.
func (s *Store) UpsertPerson(ctx context.Context, name, email string, typ model.PersonType) (model.Person, error) {
	var p model.Person
	if email == "" {
		err := s.pool.QueryRow(ctx, `
			INSERT INTO person (name, type) VALUES ($1, $2)
			RETURNING id, name, COALESCE(email, ''), type, created_at, updated_at`,
			name, string(typ)).
			Scan(&p.ID, &p.Name, &p.Email, &p.Type, &p.CreatedAt, &p.UpdatedAt)
		if err != nil {
			return model.Person{}, fmt.Errorf("store: insert person: %w", err)
		}
		return p, nil
	}

	err := s.pool.QueryRow(ctx, `
		INSERT INTO person (name, email, type) VALUES ($1, $2, $3)
		ON CONFLICT (email) WHERE email IS NOT NULL
		DO UPDATE SET name = EXCLUDED.name, updated_at = now()
		RETURNING id, name, COALESCE(email, ''), type, created_at, updated_at`,
		name, email, string(typ)).
		Scan(&p.ID, &p.Name, &p.Email, &p.Type, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return model.Person{}, fmt.Errorf("store: upsert person: %w", err)
	}
	return p, nil
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
