// Package connector defines the per-service provisioning contract. Every target
// system (Switchyard, Cloudflare Access, Argosy, …) hides its own user model
// behind this interface, so the invite orchestrator can treat "provision person
// P into service S" uniformly.
package connector

import (
	"context"
	"errors"
	"fmt"
)

// ErrPending is returned by a connector that is registered but cannot provision
// right now — most often an Unavailable connector standing in for a service
// whose token isn't configured, or one whose target system doesn't yet support
// programmatic provisioning. The orchestrator surfaces the message to the
// operator rather than treating it as a hard failure to retry blindly.
//
// It records the task as model.TaskUnavailable, not model.TaskFailed. Anything
// wrapping this sentinel is therefore asserting "nobody has wired this up yet",
// which is a different claim from "this broke" — don't reach for it to soften a
// genuine error. The message is also matched as a literal prefix by migration
// 0004's one-shot backfill, so changing its text is a schema-adjacent edit.
var ErrPending = errors.New("connector: provisioning not yet available")

// ErrReconcileUnsupported is returned by Reconcile when the target system has
// no way to look a person up without creating them — i.e. the only "does this
// account exist?" signal available is a 409 from the create endpoint, which is
// not usable here because Reconcile must never create anything.
//
// Lyceum and Argosy are both in this position today (create-only admin APIs).
// Returning this is deliberately louder than guessing: a reconcile that
// silently reports "not found" for a service it cannot actually check would
// produce exactly the wrong records.
var ErrReconcileUnsupported = errors.New("connector: reconcile requires an upstream lookup endpoint")

// ReconcileResult is what Reconcile reports about a person's *existing* access.
// It carries identity only — never a secret, because Reconcile never mints one.
type ReconcileResult struct {
	// Exists reports whether the person currently has access upstream.
	Exists bool
	// ExternalID is the upstream account id, when Exists and the API exposes it.
	ExternalID string
	// Username is the upstream login handle, when Exists.
	Username string
}

// Input carries everything a connector needs to provision one person. The core
// fields are system-agnostic; the permission fields below are optional hints a
// connector may map onto its own model (Switchyard uses all of them; connectors
// that don't understand a field ignore it).
type Input struct {
	PersonName string // display name
	Email      string // lowercased; the SSO / login identity
	Role       string // preset hint: "member" (default) | "admin"

	// InstanceRole overrides the instance-wide role the preset would pick
	// ("member" | "owner"). Empty → derive from Role.
	InstanceRole string
	// Scopes is an explicit least-privilege token scope list. Empty → the
	// connector derives a default from Role.
	Scopes []string
	// Projects are project memberships to assign after the account is created.
	Projects []ProjectGrant

	// InviteRef is a stable per-(person×service) string suitable for an
	// idempotency key on upstream APIs that support one.
	InviteRef string

	// ExternalID is the upstream account id Purser already recorded for this
	// person and service, when it has one. Empty on the Provision path — there is
	// nothing to know yet — and set on Deprovision from the `account` row.
	//
	// It exists so a revoke targets the account Purser actually provisioned
	// rather than whatever a fresh lookup turns up. That distinction matters most
	// on Switchyard, whose findUser falls back to matching on display name: a
	// lookup is a guess that can land on a same-named stranger, and on the
	// offboard path the cost of guessing wrong is revoking the wrong person's
	// access. Connectors should prefer it and fall back to a lookup only when it
	// is empty (a record written before this field existed, or by an audit that
	// found no id upstream).
	ExternalID string
}

// ProjectGrant assigns a person a role on a project. Key "*" is a wildcard
// meaning "every project" (a connector expands it, and a specific-key grant for
// the same project overrides the wildcard).
type ProjectGrant struct {
	Key  string // project key, or "*" for all projects
	Role string // e.g. "viewer" | "user" | "editor" | "admin"
}

// Result is what a successful Provision hands back: the upstream identity plus a
// one-time secret (if the service issues one) and human-facing login guidance
// for the credential block.
type Result struct {
	ExternalID   string            // id of the created/ensured account upstream
	Username     string            // login handle, if the service has one
	Secret       string            // one-time plaintext credential; "" if none
	SecretLabel  string            // e.g. "API token", "Password"
	LoginURL     string            // where the person signs in
	Instructions string            // extra human note (e.g. "sign in with the email OTP")
	Extra        map[string]string // freeform key/value for the credential block
}

// Connector provisions and manages access to a single service.
type Connector interface {
	// Key is the stable service identifier, e.g. "switchyard". It matches the
	// Service.Key stored in the database.
	Key() string
	// DisplayName is a human label for the credential block, e.g. "Switchyard".
	DisplayName() string
	// Icon is an emoji shown next to the service in the credential block (e.g.
	// "🚉" for Switchyard). May be empty; the renderer falls back to a bullet.
	Icon() string
	// Provision creates or ensures the person's account in the service and
	// returns the identity + any one-time secret. Implementations should be
	// idempotent where the upstream API allows (treat "already exists" as
	// success and reuse it) so a failed-only retry is safe.
	Provision(ctx context.Context, in Input) (Result, error)
	// Reconcile reports whether the person already has access upstream, WITHOUT
	// creating anything and WITHOUT minting, rotating or invalidating a
	// credential. That constraint is the whole point: it makes Reconcile safe to
	// run against real people at any time, which is what lets it back a
	// record-only backfill and, later, a scheduled consistency check.
	//
	// Connectors whose upstream has no lookup endpoint must return
	// ErrReconcileUnsupported rather than inferring absence.
	Reconcile(ctx context.Context, in Input) (ReconcileResult, error)
	// Deprovision revokes the person's access to the service (PRSR-17).
	//
	// It means *revoke*, not delete. Where the upstream distinguishes them, take
	// away the ability to get in and leave the account and anything it authored
	// alone: revoking is reversible and preserves the record, and "they can't get
	// in" is the actual requirement. A connector whose upstream offers only a
	// delete says so in its own doc comment rather than quietly destroying more
	// than the contract implies.
	//
	// Like Provision it must be idempotent: a person with nothing left to revoke
	// is a success, not an error, so a failed-only retry is safe. And like
	// Reconcile it must never claim more than it did — a connector that cannot
	// revoke returns ErrPending (recorded as TaskUnavailable) rather than nil,
	// because "we removed it" and "we couldn't" must not collapse into the same
	// answer on the one path that is hard to undo.
	Deprovision(ctx context.Context, in Input) error
}

// Registry is the set of connectors Purser knows about, keyed by Connector.Key.
type Registry struct {
	byKey map[string]Connector
}

// NewRegistry builds a registry from the given connectors. It panics on a
// duplicate key, since that is a programming error at wiring time.
func NewRegistry(cs ...Connector) *Registry {
	r := &Registry{byKey: make(map[string]Connector, len(cs))}
	for _, c := range cs {
		if _, dup := r.byKey[c.Key()]; dup {
			panic(fmt.Sprintf("connector: duplicate key %q", c.Key()))
		}
		r.byKey[c.Key()] = c
	}
	return r
}

// Get returns the connector for key, or (nil, false) if none is registered.
func (r *Registry) Get(key string) (Connector, bool) {
	c, ok := r.byKey[key]
	return c, ok
}

// Keys returns the registered connector keys in no particular order.
func (r *Registry) Keys() []string {
	keys := make([]string, 0, len(r.byKey))
	for k := range r.byKey {
		keys = append(keys, k)
	}
	return keys
}

// All returns every registered connector.
func (r *Registry) All() []Connector {
	cs := make([]Connector, 0, len(r.byKey))
	for _, c := range r.byKey {
		cs = append(cs, c)
	}
	return cs
}
