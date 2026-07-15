// Package lyceum is Purser's connector for Lyceum (the ebook reader + sync
// service). Lyceum shipped a per-user account model in LYCM-801, exposing
// `POST /admin/users` as the hook this connector calls (SERV-38) — mirroring the
// Switchyard connector.
//
// Provisioning a person creates their Lyceum user (email is the join key, as in
// Switchyard — for the future LYCM-803 Cloudflare Access SSO) and hands back the
// one-time `lyc_…` invite token, which they redeem in the app to sign in.
//
// Auth note: `/admin/users` is owner-session-gated (Lyceum has no service-token
// path to /admin), so Purser holds the owner's durable session token
// (PURSER_LYCEUM_OWNER_TOKEN) and the Lyceum service must run with
// LYCEUM_AUTH=true. When unconfigured, Purser registers Lyceum as Unavailable.
package lyceum

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

// Config configures the Lyceum connector.
type Config struct {
	// BaseURL is the internal API base, e.g. http://lyceum:4005 on construct_net.
	BaseURL string
	// OwnerToken is a durable owner *session* token (lyc_…) — obtained once by
	// redeeming an owner invite (see `lyceum mint-token`). Not a LYCEUM_API_TOKENS
	// entry; those can't reach /admin.
	OwnerToken string
	// AppURL is shown to the invited person for redemption (optional; Lyceum has
	// no public URL until it's tunnelled).
	AppURL     string
	HTTPClient *http.Client
}

// Connector provisions Lyceum users.
type Connector struct {
	cfg  Config
	http *http.Client
}

// New builds the connector. BaseURL and OwnerToken are required.
func New(cfg Config) (*Connector, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("lyceum: BaseURL is required")
	}
	if strings.TrimSpace(cfg.OwnerToken) == "" {
		return nil, errors.New("lyceum: OwnerToken is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Connector{cfg: cfg, http: hc}, nil
}

func (c *Connector) Key() string         { return "lyceum" }
func (c *Connector) DisplayName() string { return "Lyceum" }
func (c *Connector) Icon() string        { return "📚" }

type createResponse struct {
	User struct {
		ID          json.Number `json:"id"`
		Email       string      `json:"email"`
		DisplayName string      `json:"display_name"`
	} `json:"user"`
	InviteToken string `json:"invite_token"`
}

// Provision creates the Lyceum user and returns the one-time invite token.
func (c *Connector) Provision(ctx context.Context, in connector.Input) (connector.Result, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return connector.Result{}, errors.New("lyceum: an email is required to create a user")
	}
	displayName := strings.TrimSpace(in.PersonName)
	if displayName == "" {
		displayName = email
	}

	status, raw, err := c.do(ctx, http.MethodPost, "/admin/users",
		map[string]any{"email": email, "display_name": displayName})
	if err != nil {
		return connector.Result{}, err
	}

	switch status {
	case http.StatusCreated, http.StatusOK:
		var cr createResponse
		if err := json.Unmarshal(raw, &cr); err != nil {
			return connector.Result{}, fmt.Errorf("lyceum: decode create response: %w", err)
		}
		redeem := "Redeem this invite in the Lyceum app (Settings → Sign in) within 7 days."
		if c.cfg.AppURL != "" {
			redeem = fmt.Sprintf("Redeem this invite at %s (Settings → Sign in) within 7 days.", c.cfg.AppURL)
		}
		return connector.Result{
			ExternalID:   cr.User.ID.String(),
			Username:     cr.User.DisplayName,
			Secret:       cr.InviteToken,
			SecretLabel:  "invite token (single-use, expires in 7 days)",
			LoginURL:     c.cfg.AppURL,
			Instructions: redeem,
		}, nil
	case http.StatusConflict:
		// Already provisioned — Lyceum's email is UNIQUE. Reconcile to success
		// with no new secret (consistent with "already exists = reconcile").
		return connector.Result{
			ExternalID:   email,
			Instructions: "Already provisioned — the existing Lyceum account/invite remains valid.",
		}, nil
	case http.StatusForbidden:
		return connector.Result{}, fmt.Errorf("lyceum: 403 from /admin/users — is LYCEUM_AUTH=true and is PURSER_LYCEUM_OWNER_TOKEN an owner session token? (%s)", bodyMsg(raw))
	default:
		return connector.Result{}, apiError("create user", status, raw)
	}
}

// Reconcile is a no-op today.
func (c *Connector) Reconcile(ctx context.Context, in connector.Input) error { return nil }

// Deprovision is not yet implemented (Phase 1 is invite-only).
func (c *Connector) Deprovision(ctx context.Context, in connector.Input) error {
	return errors.New("lyceum: deprovision not implemented")
}

func (c *Connector) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("lyceum: marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("lyceum: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.OwnerToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("lyceum: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("lyceum: read body: %w", err)
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
	return fmt.Errorf("lyceum: %s: %d: %s", op, status, bodyMsg(raw))
}
