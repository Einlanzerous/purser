// Package model holds Purser's core domain types: the people we invite, the
// services we provision them into, and the bookkeeping (accounts, invites,
// provision tasks) that makes an invite idempotent per (person × service).
//
// These map 1:1 onto the tables created by migrations/0001_init.up.sql.
package model

import (
	"time"

	"github.com/google/uuid"
)

// PersonType distinguishes humans from machine/agent identities. Purser mostly
// invites humans, but the field mirrors Switchyard's user model so an agent
// provisioning path is representable later.
type PersonType string

const (
	PersonHuman PersonType = "human"
	PersonAgent PersonType = "agent"
)

// TaskStatus is the lifecycle of a single per-service provisioning attempt.
// Idempotent re-runs act only on tasks that are not Succeeded (see the invite
// orchestrator): Failed tasks are retried, Succeeded ones are skipped.
type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"   // created, not yet run
	TaskRunning   TaskStatus = "running"   // connector.Provision in flight
	TaskSucceeded TaskStatus = "succeeded" // account provisioned
	TaskFailed    TaskStatus = "failed"    // connector returned an error; retryable
	TaskSkipped   TaskStatus = "skipped"   // already provisioned on a prior invite
)

// AccountStatus tracks whether a person currently holds access to a service.
type AccountStatus string

const (
	AccountActive        AccountStatus = "active"
	AccountDeprovisioned AccountStatus = "deprovisioned"
)

// DeliveryMethod is how the resulting credential block reaches the person.
type DeliveryMethod string

const (
	// DeliverCopyPaste renders the credential block to stdout / the API
	// response so the operator can paste it into a chat platform.
	DeliverCopyPaste DeliveryMethod = "copypaste"
	// DeliverEmail sends the credential block to the person's email via SMTP.
	DeliverEmail DeliveryMethod = "email"
)

// Person is someone we provision access for. Email is the join key for
// Cloudflare Access SSO (email one-time-PIN) and for Switchyard's SSO login, so
// it is unique when present.
type Person struct {
	ID        uuid.UUID
	Name      string
	Email     string // lowercased; may be empty for agent identities
	Type      PersonType
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Service is a target system Purser can provision into (switchyard, cloudflare,
// argosy, …). Rows are seeded from the connector registry on boot, so Key
// always matches a registered Connector.
type Service struct {
	ID          uuid.UUID
	Key         string // stable connector key, e.g. "switchyard"
	DisplayName string
	CreatedAt   time.Time
}

// Account is the durable record that a person holds access to a service. The
// (PersonID, ServiceID) pair is unique — it is the idempotency key: a second
// invite for the same person+service reuses this row rather than creating a
// duplicate upstream user.
//
// Secrets are never stored in plaintext. SecretHash is the sha256 of the
// one-time credential we delivered (so we can prove which one was issued);
// SecretRef is reserved for a future vault reference and is empty today.
type Account struct {
	ID         uuid.UUID
	PersonID   uuid.UUID
	ServiceID  uuid.UUID
	ExternalID string // id of the account in the target system
	Username   string // login handle in the target system, if any
	SecretHash string // sha256 hex of the delivered secret; empty if none
	SecretRef  string // reserved: future vault ref
	Status     AccountStatus
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Invite groups one provisioning run for a person across one or more services.
type Invite struct {
	ID          uuid.UUID
	PersonID    uuid.UUID
	Delivery    DeliveryMethod
	Role        string // optional permission hint passed to connectors
	DeliveredAt *time.Time
	CreatedAt   time.Time
}

// ProvisionTask is one service's slice of an invite. It records attempts and
// the last error so a re-run can retry only what failed.
type ProvisionTask struct {
	ID        uuid.UUID
	InviteID  uuid.UUID
	PersonID  uuid.UUID
	ServiceID uuid.UUID
	AccountID *uuid.UUID // set once the task succeeds
	Status    TaskStatus
	Attempts  int
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
}
