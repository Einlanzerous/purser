package connector

import (
	"context"
	"fmt"
)

// Unavailable is a connector that is registered (so its service is a recognized
// invite target) but cannot provision because it is not configured — e.g. the
// Switchyard connector when no admin token is set. Provision returns ErrPending
// wrapping the given reason, so the operator gets a precise "why" instead of an
// "unknown service" error.
type Unavailable struct {
	ServiceKey string
	Display    string
	Reason     string
}

// NewUnavailable builds an Unavailable connector.
func NewUnavailable(key, display, reason string) *Unavailable {
	return &Unavailable{ServiceKey: key, Display: display, Reason: reason}
}

func (u *Unavailable) Key() string         { return u.ServiceKey }
func (u *Unavailable) DisplayName() string { return u.Display }

// Icon returns an empty string; an unconfigured connector has no service icon
// and the renderer falls back to a bullet. Unavailable outcomes surface only in
// the operator note anyway.
func (u *Unavailable) Icon() string { return "" }

func (u *Unavailable) Provision(ctx context.Context, in Input) (Result, error) {
	return Result{}, fmt.Errorf("%w: %s", ErrPending, u.Reason)
}

func (u *Unavailable) Reconcile(ctx context.Context, in Input) error { return nil }

func (u *Unavailable) Deprovision(ctx context.Context, in Input) error {
	return fmt.Errorf("%w: %s", ErrPending, u.Reason)
}
