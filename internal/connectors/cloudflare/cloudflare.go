// Package cloudflare is Purser's connector for Cloudflare Access — the SSO gate
// in front of the tunneled Construct apps (Switchyard, Lyceum). The Zero Gravity
// edge uses Cloudflare's built-in email one-time-PIN IdP with Allow-by-email
// policies (SERV-17/25). "Adding a person to SSO" therefore means adding their
// email to an Access group whose membership every app's policy allows.
//
// Purser adds the email to a shared Access **group** (recommended: one group
// referenced by all app policies, so a single grant covers every app) via the
// Cloudflare API v4:
//
//	GET  /accounts/{account}/access/groups/{group}  -> read include rules
//	PUT  /accounts/{account}/access/groups/{group}  -> write updated rules
//
// The operation is idempotent: an email already in the include list is left as
// is. When the API is not configured (no token/account/group — the host has no
// Cloudflare Access API credentials today, only a tunnel + DNS token), the
// connector returns connector.ErrPending with the exact manual dashboard step,
// so the invite still succeeds for other services and the operator sees what to
// do by hand.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Einlanzerous/purser/internal/connector"
)

const apiBase = "https://api.cloudflare.com/client/v4"

// Config configures the Cloudflare Access connector.
type Config struct {
	APIToken  string // scoped Account: Access: Organizations, Identity Providers, and Groups: Edit
	AccountID string // Cloudflare account id
	GroupID   string // the shared Access group to add members to
	GroupName string // human label for instructions, e.g. "zerogravity-members"

	// TeamDomain is the Zero Trust team domain, e.g.
	// zero-gravity-industries.cloudflareaccess.com — used only for the human note.
	TeamDomain string
	// AppsNote is a short human description of what access is granted, e.g.
	// "Switchyard and the other tunneled apps".
	AppsNote string

	HTTPClient *http.Client
}

// Connector adds people to a Cloudflare Access group.
type Connector struct {
	cfg     Config
	http    *http.Client
	baseURL string // Cloudflare API base; overridable in tests
}

// New builds the connector. It never fails on missing credentials — an
// unconfigured connector is valid and degrades to manual instructions at
// Provision time (so `purser invite --to cloudflare` is always wired).
func New(cfg Config) *Connector {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Connector{cfg: cfg, http: hc, baseURL: apiBase}
}

func (c *Connector) Key() string         { return "cloudflare" }
func (c *Connector) DisplayName() string { return "Cloudflare Access (SSO)" }

func (c *Connector) configured() bool {
	return c.cfg.APIToken != "" && c.cfg.AccountID != "" && c.cfg.GroupID != ""
}

// otpNote is the sign-in guidance shown to the invited person.
func (c *Connector) otpNote(email string) string {
	apps := c.cfg.AppsNote
	if apps == "" {
		apps = "the Construct apps"
	}
	return fmt.Sprintf("Sign in to %s with the email one-time-PIN sent to %s (no password).", apps, email)
}

// Provision adds the person's email to the Access group (idempotently). When the
// API is not configured it returns connector.ErrPending with the manual step.
func (c *Connector) Provision(ctx context.Context, in connector.Input) (connector.Result, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return connector.Result{}, fmt.Errorf("cloudflare: an email is required to grant SSO access")
	}

	if !c.configured() {
		group := c.cfg.GroupName
		if group == "" {
			group = "the Allow-by-email Access policy"
		}
		return connector.Result{}, fmt.Errorf(
			"%w: add %s to %s in the Cloudflare Zero Trust dashboard (Access → Policies)",
			connector.ErrPending, email, group)
	}

	added, err := c.addEmailToGroup(ctx, email)
	if err != nil {
		return connector.Result{}, err
	}

	instructions := c.otpNote(email)
	if !added {
		instructions = "Already had SSO access. " + instructions
	}
	return connector.Result{
		ExternalID:   email, // Access identities are keyed by email
		Instructions: instructions,
	}, nil
}

// group models the subset of an Access group we read/write.
type group struct {
	Name    string           `json:"name"`
	Include []map[string]any `json:"include"`
	Exclude []map[string]any `json:"exclude"`
	Require []map[string]any `json:"require"`
}

// addEmailToGroup fetches the group, appends the email include rule if missing,
// and writes it back. Returns whether the email was newly added.
func (c *Connector) addEmailToGroup(ctx context.Context, email string) (bool, error) {
	g, err := c.getGroup(ctx)
	if err != nil {
		return false, err
	}
	for _, rule := range g.Include {
		if em, ok := rule["email"].(map[string]any); ok {
			if got, _ := em["email"].(string); strings.EqualFold(got, email) {
				return false, nil // already present — idempotent no-op
			}
		}
	}
	g.Include = append(g.Include, map[string]any{"email": map[string]any{"email": email}})
	if err := c.putGroup(ctx, g); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Connector) getGroup(ctx context.Context) (group, error) {
	path := fmt.Sprintf("/accounts/%s/access/groups/%s", c.cfg.AccountID, c.cfg.GroupID)
	raw, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return group{}, err
	}
	var env struct {
		Result group `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return group{}, fmt.Errorf("cloudflare: decode group: %w", err)
	}
	return env.Result, nil
}

func (c *Connector) putGroup(ctx context.Context, g group) error {
	path := fmt.Sprintf("/accounts/%s/access/groups/%s", c.cfg.AccountID, c.cfg.GroupID)
	body := map[string]any{
		"name":    g.Name,
		"include": g.Include,
		"exclude": g.Exclude,
		"require": g.Require,
	}
	_, err := c.do(ctx, http.MethodPut, path, body)
	return err
}

// Reconcile re-adds the email to the group (idempotent), repairing drift.
func (c *Connector) Reconcile(ctx context.Context, in connector.Input) error {
	if !c.configured() {
		return nil
	}
	_, err := c.addEmailToGroup(ctx, strings.ToLower(strings.TrimSpace(in.Email)))
	return err
}

// Deprovision removes the email from the Access group.
func (c *Connector) Deprovision(ctx context.Context, in connector.Input) error {
	if !c.configured() {
		return fmt.Errorf("cloudflare: not configured")
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	g, err := c.getGroup(ctx)
	if err != nil {
		return err
	}
	kept := g.Include[:0]
	for _, rule := range g.Include {
		if em, ok := rule["email"].(map[string]any); ok {
			if got, _ := em["email"].(string); strings.EqualFold(got, email) {
				continue
			}
		}
		kept = append(kept, rule)
	}
	g.Include = kept
	return c.putGroup(ctx, g)
}

// do performs a Cloudflare API request and returns the raw body, translating the
// {"success":false,"errors":[…]} envelope into a Go error.
func (c *Connector) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("cloudflare: marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("cloudflare: read body: %w", err)
	}

	var env struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("cloudflare: %s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if !env.Success {
		if len(env.Errors) > 0 {
			return nil, fmt.Errorf("cloudflare: %s %s: %d %s", method, path, env.Errors[0].Code, env.Errors[0].Message)
		}
		return nil, fmt.Errorf("cloudflare: %s %s: request unsuccessful (%d)", method, path, resp.StatusCode)
	}
	return raw, nil
}
