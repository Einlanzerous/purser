// Package switchyard is Purser's connector for Switchyard (the self-hosted
// Jira replacement). Provisioning a person means: create a Switchyard user with
// their email set (the SSO join key — Switchyard's Cloudflare Access SSO maps
// the CF-verified email to users.email and never auto-provisions), then mint a
// one-time API token to hand back as the credential.
//
// It talks to Switchyard's /v1 REST API (contract: switchyard/openapi.yaml):
//
//	POST /v1/users              -> create user
//	GET  /v1/users              -> list (used to reconcile a name/email conflict)
//	POST /v1/users/{id}/tokens  -> mint an API token (sw_… returned once)
package switchyard

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

// Config configures the Switchyard connector.
type Config struct {
	// BaseURL is the internal API base, e.g. http://switchyard:4002 on
	// construct_net. The /v1 prefix is appended by the connector.
	BaseURL string
	// Token is an admin-capable Switchyard token (sw_…) owned by an owner-human
	// or agent user, scoped `admin` (or at least users:manage). Typically the
	// instance BOOTSTRAP_TOKEN.
	Token string
	// LoginURL is the public URL shown to the invited person, e.g.
	// https://switchyard.zerogravity.industries.
	LoginURL string
	// HTTPClient is optional; defaults to a 15s-timeout client.
	HTTPClient *http.Client
}

// Connector provisions Switchyard users + tokens.
type Connector struct {
	cfg  Config
	http *http.Client
}

// New builds the connector. BaseURL and Token are required.
func New(cfg Config) (*Connector, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("switchyard: BaseURL is required")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("switchyard: Token is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Connector{cfg: cfg, http: hc}, nil
}

func (c *Connector) Key() string         { return "switchyard" }
func (c *Connector) DisplayName() string { return "Switchyard" }

type user struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type tokenResponse struct {
	Token string `json:"token"`
}

// Provision creates (or reconciles) the Switchyard user and mints an API token.
func (c *Connector) Provision(ctx context.Context, in connector.Input) (connector.Result, error) {
	u, err := c.ensureUser(ctx, in)
	if err != nil {
		return connector.Result{}, err
	}

	scopes := memberScopes
	if strings.EqualFold(in.Role, "admin") {
		scopes = adminScopes
	}

	token, err := c.mintToken(ctx, u.ID, in.InviteRef, scopes)
	if err != nil {
		return connector.Result{}, err
	}

	return connector.Result{
		ExternalID:  u.ID,
		Username:    u.Name,
		Secret:      token,
		SecretLabel: "API token",
		LoginURL:    c.cfg.LoginURL,
		Instructions: "Through the tunnel you'll be signed in automatically after the " +
			"Cloudflare email one-time-PIN. On the LAN, paste the API token on the login screen.",
	}, nil
}

// ensureUser creates the user, or reconciles an existing one on a 409 conflict
// (name/email already taken) by finding it in the user list — making Provision
// safe to retry.
func (c *Connector) ensureUser(ctx context.Context, in connector.Input) (user, error) {
	instanceRole := "member"
	if strings.EqualFold(in.Role, "admin") {
		instanceRole = "owner"
	}
	body := map[string]any{
		"name":          in.PersonName,
		"type":          "human",
		"instance_role": instanceRole,
	}
	if in.Email != "" {
		body["email"] = in.Email
	}

	status, raw, err := c.do(ctx, http.MethodPost, "/v1/users", in.InviteRef, body)
	if err != nil {
		return user{}, err
	}
	switch {
	case status == http.StatusCreated || status == http.StatusOK:
		var u user
		if err := json.Unmarshal(raw, &u); err != nil {
			return user{}, fmt.Errorf("switchyard: decode created user: %w", err)
		}
		return u, nil
	case status == http.StatusConflict:
		// Name or email already exists — reconcile to that user.
		if u, ok, err := c.findUser(ctx, in.PersonName, in.Email); err != nil {
			return user{}, err
		} else if ok {
			return u, nil
		}
		return user{}, fmt.Errorf("switchyard: user conflict but no match found for %q/%q", in.PersonName, in.Email)
	default:
		return user{}, apiError("create user", status, raw)
	}
}

// findUser pages the user list looking for a match by email (preferred) or name.
func (c *Connector) findUser(ctx context.Context, name, email string) (user, bool, error) {
	email = strings.ToLower(email)
	cursor := ""
	for pageNum := 0; pageNum < 100; pageNum++ { // bounded: avoid runaway pagination
		path := "/v1/users?limit=100"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		status, raw, err := c.do(ctx, http.MethodGet, path, "", nil)
		if err != nil {
			return user{}, false, err
		}
		if status != http.StatusOK {
			return user{}, false, apiError("list users", status, raw)
		}
		var list struct {
			Items []user `json:"items"`
			Page  struct {
				NextCursor *string `json:"next_cursor"`
			} `json:"page"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return user{}, false, fmt.Errorf("switchyard: decode user list: %w", err)
		}
		for _, u := range list.Items {
			if email != "" && strings.ToLower(u.Email) == email {
				return u, true, nil
			}
			if email == "" && u.Name == name {
				return u, true, nil
			}
		}
		if list.Page.NextCursor == nil || *list.Page.NextCursor == "" {
			break
		}
		cursor = *list.Page.NextCursor
	}
	return user{}, false, nil
}

var (
	adminScopes  = []string{"admin"}
	memberScopes = []string{"tickets:read", "tickets:write", "comments:write", "attachments:write"}
)

func (c *Connector) mintToken(ctx context.Context, userID, inviteRef string, scopes []string) (string, error) {
	body := map[string]any{
		"name":   "purser-provisioned",
		"kind":   "personal",
		"scopes": scopes,
	}
	status, raw, err := c.do(ctx, http.MethodPost, "/v1/users/"+userID+"/tokens", inviteRef, body)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", apiError("mint token", status, raw)
	}
	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return "", fmt.Errorf("switchyard: decode token: %w", err)
	}
	if tr.Token == "" {
		return "", errors.New("switchyard: token response had no secret")
	}
	return tr.Token, nil
}

// Reconcile is a no-op today: an existing Switchyard user needs no periodic
// repair from Purser's side.
func (c *Connector) Reconcile(ctx context.Context, in connector.Input) error { return nil }

// Deprovision is not yet implemented (Phase 1 is invite-only).
func (c *Connector) Deprovision(ctx context.Context, in connector.Input) error {
	return errors.New("switchyard: deprovision not implemented")
}

// do performs a JSON request against the Switchyard API and returns the status
// code and raw body. inviteRef, when non-empty, is sent as Idempotency-Key.
func (c *Connector) do(ctx context.Context, method, path, inviteRef string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("switchyard: marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("switchyard: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if inviteRef != "" {
		req.Header.Set("Idempotency-Key", inviteRef)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("switchyard: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("switchyard: read body: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// apiError renders a Switchyard error envelope ({"error":{code,message}}) or the
// raw body into a Go error.
func apiError(op string, status int, raw []byte) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
		return fmt.Errorf("switchyard: %s: %d %s: %s", op, status, env.Error.Code, env.Error.Message)
	}
	return fmt.Errorf("switchyard: %s: %d: %s", op, status, strings.TrimSpace(string(raw)))
}
