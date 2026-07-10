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

// ErrPending is returned by a connector that is wired up but whose target
// system does not yet support programmatic provisioning (e.g. Argosy, which
// needs an admin create-account endpoint first). The orchestrator surfaces the
// message to the operator rather than treating it as a hard failure to retry
// blindly.
var ErrPending = errors.New("connector: provisioning not yet available")

// Input carries everything a connector needs to provision one person. It is
// intentionally system-agnostic; each connector maps these onto its own API.
type Input struct {
	PersonName string // display name
	Email      string // lowercased; the SSO / login identity
	Role       string // optional permission hint: "member" (default) | "admin"
	// InviteRef is a stable per-(invite×service) string suitable for an
	// idempotency key on upstream APIs that support one.
	InviteRef string
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
	// Provision creates or ensures the person's account in the service and
	// returns the identity + any one-time secret. Implementations should be
	// idempotent where the upstream API allows (treat "already exists" as
	// success and reuse it) so a failed-only retry is safe.
	Provision(ctx context.Context, in Input) (Result, error)
	// Reconcile re-checks/repairs an existing account without minting new
	// secrets. Default connectors may no-op.
	Reconcile(ctx context.Context, in Input) error
	// Deprovision removes the person's access. Stubbed for now (Phase 1 is
	// invite-only); connectors may return a not-implemented error.
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
