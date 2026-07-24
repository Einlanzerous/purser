// Package argosy is Purser's connector for Argosy (the media server). Argosy
// shipped the admin create-account endpoint in ARGY-132, exposing
// `POST /api/v1/admin/accounts` as the hook this connector calls (SERV-50).
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
		return connector.Result{
			ExternalID:   email,
			Username:     email,
			LoginURL:     c.cfg.AppURL,
			Instructions: "Already provisioned — the existing Argosy account and password remain valid.",
		}, nil
	case http.StatusUnauthorized:
		return connector.Result{}, fmt.Errorf("argosy: 401 from /api/v1/admin/accounts — does PURSER_ARGOSY_PROVISION_TOKEN match the argosy service's ARGOSY_PROVISION_TOKEN? (%s)", bodyMsg(raw))
	case http.StatusNotFound:
		return connector.Result{}, fmt.Errorf("argosy: 404 from /api/v1/admin/accounts — argosy registers this route only when ARGOSY_PROVISION_TOKEN is set; is the service env configured and the container recreated? (%s)", bodyMsg(raw))
	default:
		return connector.Result{}, apiError("create account", status, raw)
	}
}

// Reconcile is a no-op today.
func (c *Connector) Reconcile(ctx context.Context, in connector.Input) error { return nil }

// Deprovision is not yet implemented (Phase 1 is invite-only). Argosy has no
// admin delete-account endpoint to call.
func (c *Connector) Deprovision(ctx context.Context, in connector.Input) error {
	return errors.New("argosy: deprovision not implemented")
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
