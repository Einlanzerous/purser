// Package argosy is Purser's connector for Argosy (the media server). Argosy
// shipped the admin create-account endpoint in ARGY-132, exposing
// `POST /api/v1/admin/accounts` as the hook this connector calls (PRSR-13).
//
// Provisioning a person creates their Argosy account (email is the login
// identity as of ARGY-159) plus its initial admin profile, and hands back the
// server-generated password. Argosy delivers that password exactly once — it
// stores only the bcrypt hash — so it goes straight into the credential block.
//
// Auth note: a bearer session can't authorize creating a *new* account (a
// session is always scoped to an existing one), so the route is gated on a
// static service secret sent as `X-Provision-Token`, matching Argosy's
// ARGOSY_PROVISION_TOKEN. Argosy registers the route only when that env is set;
// otherwise it 404s. When unconfigured here, Purser registers Argosy as
// Unavailable.
//
// Unlike Switchyard and Lyceum, Argosy is on the direct (non-tunneled) path —
// argosy.zerogravity.industries via Traefik — so there is no Cloudflare Access
// half to pair with. The invitee signs in with email + password and pairs
// devices from there.
package argosy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Einlanzerous/purser/internal/connector"
)

// Config configures the Argosy connector.
type Config struct {
	// BaseURL is the internal API base, e.g. http://argosy:8096 on construct_net.
	BaseURL string
	// ProvisionToken is Argosy's ARGOSY_PROVISION_TOKEN, sent as X-Provision-Token.
	ProvisionToken string
	// AppURL is the public sign-in URL shown to the invited person (optional).
	AppURL     string
	HTTPClient *http.Client
}

// Connector provisions Argosy accounts.
type Connector struct {
	cfg  Config
	http *http.Client
}

// New builds the connector. BaseURL and ProvisionToken are required.
func New(cfg Config) (*Connector, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("argosy: BaseURL is required")
	}
	if strings.TrimSpace(cfg.ProvisionToken) == "" {
		return nil, errors.New("argosy: ProvisionToken is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Connector{cfg: cfg, http: hc}, nil
}

func (c *Connector) Key() string         { return "argosy" }
func (c *Connector) DisplayName() string { return "Argosy" }
func (c *Connector) Icon() string        { return "🎬" }

type createResponse struct {
	Account struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"account"`
	GeneratedPassword string `json:"generatedPassword"`
}

// Provision creates the Argosy account and returns the one-time password.
func (c *Connector) Provision(ctx context.Context, in connector.Input) (connector.Result, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return connector.Result{}, errors.New("argosy: an email is required to create an account")
	}
	// accountName names both the account and its initial admin profile, and is
	// required upstream — fall back to the email so provisioning never 400s on a
	// person record that has no display name.
	accountName := strings.TrimSpace(in.PersonName)
	if accountName == "" {
		accountName = email
	}

	// Omit `password` so Argosy generates one and returns it once.
	status, raw, err := c.do(ctx, http.MethodPost, "/api/v1/admin/accounts",
		map[string]any{"email": email, "accountName": accountName})
	if err != nil {
		return connector.Result{}, err
	}

	switch status {
	case http.StatusCreated, http.StatusOK:
		var cr createResponse
		if err := json.Unmarshal(raw, &cr); err != nil {
			return connector.Result{}, fmt.Errorf("argosy: decode create response: %w", err)
		}
		signIn := "Sign in with this email and password, then pair your devices from the app."
		if c.cfg.AppURL != "" {
			signIn = fmt.Sprintf("Sign in at %s with this email and password, then pair your devices from the app.", c.cfg.AppURL)
		}
		return connector.Result{
			ExternalID:   cr.Account.ID,
			Username:     email,
			Secret:       cr.GeneratedPassword,
			SecretLabel:  "password (shown once — change it after signing in)",
			LoginURL:     c.cfg.AppURL,
			Instructions: signIn,
		}, nil
	case http.StatusConflict:
		// Already provisioned — accounts.email is UNIQUE upstream. Reconcile to
		// success with no new secret, so a failed-only retry doesn't mint a
		// second password for someone who already has one.
		//
		// ARGY-163 added the existing account to the 409 body, so this can record
		// the real upstream id. Older Argosy builds send only {"error":…}; fall
		// back to the email then rather than failing, since the conflict itself
		// is still valid information.
		out := connector.Result{
			ExternalID:   email,
			Username:     email,
			LoginURL:     c.cfg.AppURL,
			Instructions: "Already provisioned — the existing Argosy account and password remain valid.",
		}
		var cr createResponse
		if err := json.Unmarshal(raw, &cr); err == nil && cr.Account.ID != "" {
			out.ExternalID = cr.Account.ID
		}
		return out, nil
	case http.StatusUnauthorized:
		return connector.Result{}, fmt.Errorf("argosy: 401 from /api/v1/admin/accounts — does PURSER_ARGOSY_PROVISION_TOKEN match the argosy service's ARGOSY_PROVISION_TOKEN? (%s)", bodyMsg(raw))
	case http.StatusNotFound:
		return connector.Result{}, fmt.Errorf("argosy: 404 from /api/v1/admin/accounts — argosy registers this route only when ARGOSY_PROVISION_TOKEN is set; is the service env configured and the container recreated? (%s)", bodyMsg(raw))
	default:
		return connector.Result{}, apiError("create account", status, raw)
	}
}

// Reconcile reports whether the Argosy account already exists, via the
// read-only lookup ARGY-163 added (PRSR-15).
//
// This was the last connector that couldn't be checked: Argosy's admin API used
// to be create-only, so the only existence signal was a 409 from an attempted
// create — which Reconcile must never do. `GET /api/v1/admin/accounts?email=`
// closes that, gated on the same X-Provision-Token and matching email
// case-insensitively.
func (c *Connector) Reconcile(ctx context.Context, in connector.Input) (connector.ReconcileResult, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return connector.ReconcileResult{}, errors.New("argosy: an email is required to reconcile")
	}

	status, raw, err := c.do(ctx, http.MethodGet,
		"/api/v1/admin/accounts?email="+url.QueryEscape(email), nil)
	if err != nil {
		return connector.ReconcileResult{}, err
	}
	switch status {
	case http.StatusOK:
		var lr createResponse // {account:{id,name}} — same shape as create
		if err := json.Unmarshal(raw, &lr); err != nil {
			return connector.ReconcileResult{}, fmt.Errorf("argosy: decode lookup response: %w", err)
		}
		return connector.ReconcileResult{
			Exists: true, ExternalID: lr.Account.ID, Username: email,
		}, nil
	case http.StatusNotFound:
		// A 404 here is a genuine "no such account" — distinct from the route
		// itself being unregistered, which answers 404 on the *create* path when
		// ARGOSY_PROVISION_TOKEN is unset. The token is required to reach this
		// handler at all, so an unconfigured server 401s first.
		return connector.ReconcileResult{Exists: false}, nil
	case http.StatusUnauthorized:
		return connector.ReconcileResult{}, fmt.Errorf("argosy: 401 from the account lookup — does PURSER_ARGOSY_PROVISION_TOKEN match the argosy service's ARGOSY_PROVISION_TOKEN? (%s)", bodyMsg(raw))
	default:
		return connector.ReconcileResult{}, apiError("lookup account", status, raw)
	}
}

// Deprovision is not yet implemented (Phase 1 is invite-only). Argosy has no
// admin delete-account endpoint to call.
// Deprovision reports that Argosy cannot revoke access yet (PRSR-17).
//
// The admin surface has create (`POST /api/v1/admin/accounts`) and lookup
// (`GET /api/v1/admin/accounts?email=`, ARGY-163) but no delete, no disable, and
// no way to invalidate a device bearer token. So there is nothing to call.
//
// ErrPending rather than a plain error, deliberately: it records
// model.TaskUnavailable, which is the difference between "this broke" and
// "nobody has built this yet". An offboard that revokes Switchyard, Cloudflare
// and Lyceum then reports Argosy as unavailable is telling the truth — three
// services closed, one still open and needing a hand — where returning nil would
// claim access was removed that demonstrably wasn't, on the one path where that
// claim is dangerous.
func (c *Connector) Deprovision(ctx context.Context, in connector.Input) error {
	return fmt.Errorf("%w: argosy has no account delete or disable endpoint — remove the account by hand until one ships", connector.ErrPending)
}

func (c *Connector) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("argosy: marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("argosy: new request: %w", err)
	}
	req.Header.Set("X-Provision-Token", c.cfg.ProvisionToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("argosy: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("argosy: read body: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// bodyMsg extracts a human message from an error body, or returns the trimmed raw.
func bodyMsg(raw []byte) string {
	var env struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		if env.Message != "" {
			return env.Message
		}
		if env.Error != "" {
			return env.Error
		}
	}
	return strings.TrimSpace(string(raw))
}

func apiError(op string, status int, raw []byte) error {
	return fmt.Errorf("argosy: %s: %d: %s", op, status, bodyMsg(raw))
}
