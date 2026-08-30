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
// orchestrator): Failed and Unavailable tasks are retried, Succeeded ones are
// skipped.
//
// Failed and Unavailable are separate states because they answer different
// questions. Failed means something broke and someone should look at it;
// Unavailable means the connector was never in a position to try. Both are
// retryable and neither provisioned anything, which is why they were one state
// for a while — but every consumer that buckets by status (the operator note,
// the launcher gate, the CLI marks) needs to tell them apart, and doing that on
// a bool riding alongside the status meant each new consumer had to remember the
// bool existed (PRSR-21).
type TaskStatus string

const (
	TaskPending TaskStatus = "pending" // created, not yet run
	TaskRunning TaskStatus = "running" // connector.Provision in flight
	// TaskUnavailable — the connector returned connector.ErrPending: it is
	// registered but could not provision, because it isn't configured or its
	// upstream has no provisioning API yet. Retryable, but retrying changes
	// nothing until a human configures something.
	//
	// Named for connector.Unavailable, which is the usual source, rather than
	// "pending" — TaskPending is already the *queued* state, and having the same
	// word mean both "not run yet" and "can't be run" is the collision this
	// status exists to avoid.
	TaskUnavailable TaskStatus = "unavailable"
	TaskSucceeded   TaskStatus = "succeeded" // account provisioned
	TaskFailed      TaskStatus = "failed"    // connector returned an error; retryable
	TaskSkipped     TaskStatus = "skipped"   // already provisioned on a prior invite
)

// AccountStatus tracks whether a person currently holds access to a service.
type AccountStatus string

const (
	AccountActive        AccountStatus = "active"
	AccountDeprovisioned AccountStatus = "deprovisioned"
	// AccountStale means Purser holds a record but Reconcile found no matching
	// account upstream — it was deleted or never really existed (PRSR-15).
	//
	// This is distinct from deprovisioned, which is Purser deliberately removing
	// access. Stale is drift Purser didn't cause and can't explain. It matters
	// because the orchestrator's idempotency skip keys on *active*: an account
	// stuck active with nothing upstream can never be re-provisioned by any
	// invite, so marking it stale is what re-arms provisioning.
	AccountStale AccountStatus = "stale"
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

// ResourceKind is one class of edge object the service spin-up axis creates
// (PRSR-27). Unlike everything above it, these are keyed on hostname rather than
// on a person: they are the infrastructure that makes a service exist, not
// somebody's access to one.
//
// The values match service_resource.kind's CHECK constraint, and their order in
// KindOrder is the order they are applied in — see that variable for why DNS
// comes last.
type ResourceKind string

const (
	// ResourceTunnelRoute is a hostname rule in a cloudflared tunnel's ingress
	// configuration. It has no id of its own: the configuration is one document
	// per tunnel, so the route is identified by its tunnel plus its hostname.
	ResourceTunnelRoute ResourceKind = "tunnel_route"
	// ResourceAccessApp is a Cloudflare Access application — either a gated
	// `self_hosted` app with a policy, or a `bookmark` launcher tile. Which one
	// is a property of the spec, not of this kind.
	ResourceAccessApp ResourceKind = "access_app"
	// ResourceDNSRecord is the zone record that makes the hostname resolve.
	ResourceDNSRecord ResourceKind = "dns_record"
)

// KindOrder is every resource kind in the order a spin-up applies them, and the
// order reports list them in.
//
// **DNS is last, because DNS is what makes the hostname live.** The other two
// steps are inert until something resolves: an ingress route for a hostname that
// doesn't resolve serves nobody, and an Access application in front of a
// hostname that doesn't resolve gates nobody. Publishing the record first
// inverts that — a tunnelled service answers 502 until its route lands, and,
// far worse, a service meant to be gated is reachable *ungated* for as long as
// its Access app takes to create. Ordering is the only thing standing between a
// spin-up and that window, so it lives here rather than in each caller.
var KindOrder = []ResourceKind{ResourceTunnelRoute, ResourceAccessApp, ResourceDNSRecord}

// TeardownOrder is the order a teardown removes them in: KindOrder reversed.
//
// **DNS goes first, because DNS is what makes the hostname live.** It is the
// same argument that puts it last on the way up, read backwards, and the failure
// it prevents is the mirror image: pull the Access application first and the
// service is briefly reachable *ungated*, which is the one outcome this axis
// must not produce quietly. Pull the ingress route first and a tunnelled service
// answers 502 until the record goes — noisy, self-announcing, and the lesser of
// the two, exactly as it is on the way up.
//
// Derived from KindOrder rather than written out again. A second literal is a
// second thing to keep in step, and the way it would fail is by removing the
// gate before the record with nothing anywhere reporting it.
//
// Ordering alone only closes the window when the earlier removal actually
// landed, which is what ServiceSpec has dependsOn for on the way up and what
// spinup's teardownDependsOn has here.
func TeardownOrder() []ResourceKind {
	out := make([]ResourceKind, len(KindOrder))
	for i, k := range KindOrder {
		out[len(KindOrder)-1-i] = k
	}
	return out
}

// ResourceStatus is whether a recorded edge resource is still in place.
//
// There is deliberately no counterpart to AccountStale. That status exists on
// the person axis to re-arm provisioning, because the invite orchestrator's
// idempotency skip reads the account row; the spin-up path reads *upstream*
// instead, so a record that disagrees with reality changes nothing about what it
// does next.
type ResourceStatus string

const (
	ResourceActive ResourceStatus = "active"
	// ResourceRemoved — torn down. The row is kept, like a deprovisioned
	// account: it is the record that this hostname once held this resource.
	ResourceRemoved ResourceStatus = "removed"
)

// ServiceResource is one edge object Purser created for a service, and the
// coordinates needed to find it again. It maps 1:1 onto service_resource
// (migration 0007).
//
// A row exists only for a resource that actually exists upstream: a step that
// failed writes nothing, so "no row" means "we did not put anything here",
// never "we tried". That is what lets Teardown target ids rather than guessing
// by hostname — deleting a DNS record somebody else created by hand is not
// recoverable by re-running.
type ServiceResource struct {
	ID uuid.UUID
	// ServiceKey names the service this belongs to. It is not a reference to
	// Service.Key: that type is a target system for *invites*, and a service
	// being stood up here need never be one.
	ServiceKey string
	Hostname   string
	Kind       ResourceKind
	// ExternalID is the resource's own upstream id, or "" for a kind that has
	// none (ResourceTunnelRoute).
	ExternalID string
	// ParentID is the container it lives in — zone, account, or tunnel id.
	// Recorded per row because the tunnel is a per-spec choice (PRSR-33), so
	// config is not a reliable answer to "which document did this go into?".
	ParentID  string
	Status    ResourceStatus
	CreatedAt time.Time
	UpdatedAt time.Time
	// RemovedAt is when it was torn down, or nil if it never was. Never cleared
	// by standing the hostname back up — see migration 0006 for the same
	// decision on the person axis, and why it isn't derived from UpdatedAt.
	RemovedAt *time.Time
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
