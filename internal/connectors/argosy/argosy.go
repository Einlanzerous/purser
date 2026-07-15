// Package argosy is a placeholder connector for Argosy (the media server). It is
// registered so `purser invite --to argosy` is a recognized target, but Argosy
// cannot be provisioned yet: it has no HTTP account-creation endpoint (only an
// env-var bootstrap) and no cross-account admin auth. Provision therefore
// returns connector.ErrPending with a pointer to the tracking ticket.
//
// Once Argosy ships `POST /api/v1/accounts` guarded by a service provisioning
// secret (see the ARGY ticket), this becomes a real HTTP connector: create the
// account (username + generated password) and hand the password back as the
// credential. Argosy is on the direct (non-tunneled) path, so it uses its own
// username/password — not Cloudflare Access SSO.
package argosy

import (
	"context"
	"fmt"

	"github.com/Einlanzerous/purser/internal/connector"
)

// Connector is the not-yet-available Argosy connector.
type Connector struct{}

// New builds the placeholder connector.
func New() *Connector { return &Connector{} }

func (c *Connector) Key() string         { return "argosy" }
func (c *Connector) DisplayName() string { return "Argosy" }
func (c *Connector) Icon() string        { return "🎬" }

// Provision reports that Argosy provisioning is pending upstream support.
func (c *Connector) Provision(ctx context.Context, in connector.Input) (connector.Result, error) {
	return connector.Result{}, fmt.Errorf(
		"%w: Argosy needs an admin create-account endpoint first (tracking ticket ARGY)",
		connector.ErrPending)
}

func (c *Connector) Reconcile(ctx context.Context, in connector.Input) error { return nil }

func (c *Connector) Deprovision(ctx context.Context, in connector.Input) error {
	return fmt.Errorf("%w: argosy deprovision", connector.ErrPending)
}
