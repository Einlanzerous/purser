package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/model"
)

// This file is the service spin-up axis's persistence (PRSR-27): what Purser
// created at the edge for a service, keyed on hostname. It shares nothing with
// the person-axis queries in repo.go beyond the pool — see migration 0007 for
// why the two are deliberately unjoined.

// UpsertServiceResource records an edge resource for (hostname, kind), or
// updates the one already recorded. Returns the stored row.
//
// The conflict target is (lower(hostname), kind), matching
// service_resource_hostname_kind_key. Inferring on the bare column would miss a
// differently-cased hostname and record the same resource twice — the bug
// migration 0003 fixed on person.email, which had inserted duplicate identities
// for one human. Hostnames are case-insensitive by specification, so this axis
// gets the case-folded index from the start.
//
// It writes status='active' unconditionally: this method is only ever called
// for a resource that was just observed or created upstream, so a row that had
// been torn down and is now back is correctly active again. removed_at is
// deliberately left alone rather than cleared — the hostname *was* taken down
// once, and that doesn't stop having happened. Read status for the current
// state.
func (s *Store) UpsertServiceResource(ctx context.Context, r model.ServiceResource) (model.ServiceResource, error) {
	hostname := strings.TrimSpace(r.Hostname)
	if hostname == "" {
		// Without a hostname there is no conflict target, so this would insert a
		// fresh row on every run and Teardown would have several to choose from.
		return model.ServiceResource{}, fmt.Errorf("store: upsert service resource: hostname is required")
	}
	var out model.ServiceResource
	err := s.pool.QueryRow(ctx, `
		INSERT INTO service_resource (service_key, hostname, kind, external_id, parent_id, status)
		VALUES ($1, $2, $3, $4, $5, 'active')
		ON CONFLICT (lower(hostname), kind) DO UPDATE SET
			service_key = EXCLUDED.service_key,
			external_id = EXCLUDED.external_id,
			parent_id   = EXCLUDED.parent_id,
			status      = 'active',
			updated_at  = now()
		RETURNING id, service_key, hostname, kind, external_id, parent_id, status,
		          created_at, updated_at, removed_at`,
		r.ServiceKey, hostname, string(r.Kind), r.ExternalID, r.ParentID).
		Scan(&out.ID, &out.ServiceKey, &out.Hostname, &out.Kind, &out.ExternalID, &out.ParentID,
			&out.Status, &out.CreatedAt, &out.UpdatedAt, &out.RemovedAt)
	if err != nil {
		return model.ServiceResource{}, fmt.Errorf("store: upsert service resource: %w", err)
	}
	return out, nil
}

// MarkServiceResourceRemoved records that a resource was torn down, stamping
// removed_at on the transition and never re-stamping it.
//
// Separate from UpsertServiceResource because the two are called at opposite
// moments and only one of them may be called speculatively: this one asserts
// that the resource is *gone upstream*, and the invariant the offboard path
// learned the hard way (PRSR-17) applies unchanged here — a teardown that didn't
// happen must never be recorded as one, or the next run skips something that is
// still live.
func (s *Store) MarkServiceResourceRemoved(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE service_resource SET
			status     = 'removed',
			removed_at = COALESCE(removed_at, now()),
			updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: mark service resource removed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ServiceResourcesForHostname returns every resource recorded for a hostname,
// in KindOrder — including removed ones, so a caller can tell "never created"
// from "torn down". Matched case-insensitively, like the index.
func (s *Store) ServiceResourcesForHostname(ctx context.Context, hostname string) ([]model.ServiceResource, error) {
	return s.serviceResources(ctx, &hostname, nil)
}

// ServiceResourcesFor returns every resource recorded for a service, across all
// of its hostnames — a dev and a prod hostname are separate rows for the same
// service_key (PRSR-33).
//
// Matched exactly: ServiceSpec.Normalized lowercases the key before anything
// writes it, so the folding happens once, at the boundary, rather than on every
// read of an unindexed column.
func (s *Store) ServiceResourcesFor(ctx context.Context, serviceKey string) ([]model.ServiceResource, error) {
	return s.serviceResources(ctx, nil, &serviceKey)
}

// serviceResources is the shared body of the two lookups above: one column list
// and one ordering, so they cannot drift.
//
// Both filters are nullable parameters rather than a SQL fragment the caller
// passes in — the same shape accountRecords uses. A concatenated WHERE has to
// agree with hardcoded placeholder numbers further down the statement, so the
// first predicate that needs two parameters binds the kind-order array to the
// wrong one, and it fails as wrong rows rather than as an error.
//
// The ordering is by kind's position in model.KindOrder rather than
// alphabetically, so a report reads in the order a spin-up applies the steps.
func (s *Store) serviceResources(ctx context.Context, hostname, serviceKey *string) ([]model.ServiceResource, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, service_key, hostname, kind, external_id, parent_id, status,
		       created_at, updated_at, removed_at
		FROM service_resource
		WHERE ($1::text IS NULL OR lower(hostname) = lower($1))
		  AND ($2::text IS NULL OR service_key = $2)
		ORDER BY hostname, array_position($3::text[], kind)`,
		hostname, serviceKey, kindOrderText())
	if err != nil {
		return nil, fmt.Errorf("store: service resources: %w", err)
	}
	defer rows.Close()

	var out []model.ServiceResource
	for rows.Next() {
		var r model.ServiceResource
		if err := rows.Scan(&r.ID, &r.ServiceKey, &r.Hostname, &r.Kind, &r.ExternalID,
			&r.ParentID, &r.Status, &r.CreatedAt, &r.UpdatedAt, &r.RemovedAt); err != nil {
			return nil, fmt.Errorf("store: scan service resource: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// kindOrderText renders model.KindOrder for array_position, so the SQL ordering
// is driven by the same list the orchestrator applies steps in instead of a
// second copy that can disagree with it.
func kindOrderText() []string {
	out := make([]string, len(model.KindOrder))
	for i, k := range model.KindOrder {
		out[i] = string(k)
	}
	return out
}
