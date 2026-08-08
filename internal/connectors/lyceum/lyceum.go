// Package lyceum is Purser's connector for Lyceum (the ebook reader + sync
// service). Lyceum shipped a per-user account model in LYCM-801, exposing
// `POST /admin/users` as the hook this connector calls (PRSR-6) — mirroring the
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
	"net/url"
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

// Reconcile reports whether the Lyceum user already exists, by listing the
// household via `GET /admin/users` — read-only, and it neither creates a user
// nor mints an invite token (PRSR-15).
//
// Lyceum's admin surface is richer than the create call this connector used to
// rely on: it also exposes list, per-user invite, and delete. Only the list is
// needed here.
func (c *Connector) Reconcile(ctx context.Context, in connector.Input) (connector.ReconcileResult, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return connector.ReconcileResult{}, errors.New("lyceum: an email is required to reconcile")
	}

	status, raw, err := c.do(ctx, http.MethodGet, "/admin/users", nil)
	if err != nil {
		return connector.ReconcileResult{}, err
	}
	switch status {
	case http.StatusOK:
		var members []struct {
			ID          json.Number `json:"id"`
			Email       string      `json:"email"`
			DisplayName string      `json:"display_name"`
		}
		if err := json.Unmarshal(raw, &members); err != nil {
			return connector.ReconcileResult{}, fmt.Errorf("lyceum: decode user list: %w", err)
		}
		for _, m := range members {
			if strings.EqualFold(strings.TrimSpace(m.Email), email) {
				return connector.ReconcileResult{
					Exists: true, ExternalID: m.ID.String(), Username: m.DisplayName,
				}, nil
			}
		}
		return connector.ReconcileResult{Exists: false}, nil
	case http.StatusForbidden:
		return connector.ReconcileResult{}, fmt.Errorf("lyceum: 403 from /admin/users — is LYCEUM_AUTH=true and is PURSER_LYCEUM_OWNER_TOKEN an owner session token? (%s)", bodyMsg(raw))
	default:
		return connector.ReconcileResult{}, apiError("list users", status, raw)
	}
}

// Deprovision removes the person's Lyceum account (PRSR-17).
//
// **This connector deletes.** Everywhere else `Deprovision` means revoke — take
// away the way in and leave the account standing — but Lyceum's admin surface
// offers exactly one destructive operation, `DELETE /admin/users/{id}`, and no
// way to disable a user or invalidate their sessions short of it. So here the two
// collapse, and the honest thing is to say so rather than to let the interface's
// gentler wording imply a reversibility this has none of.
//
// Idempotent: a person with no Lyceum user is a success. The id comes from the
// account row where possible and from a lookup otherwise, both of which key on
// the email — Lyceum has no name-matching fallback, so there is no stranger to
// hit here the way there is on Switchyard.
func (c *Connector) Deprovision(ctx context.Context, in connector.Input) error {
	id, recorded, err := c.resolveUserID(ctx, in)
	if err != nil {
		return err
	}
	if id == "" {
		return nil // nothing upstream to remove
	}

	status, raw, err := c.deleteUser(ctx, id)
	if err != nil {
		return err
	}
	// A 404 against the id Purser *recorded* means the record is wrong, not that
	// the person has no account. Reporting success there would mark the account
	// deprovisioned while the real Lyceum user stays, and the next run would skip
	// it. So re-ask the authority — the email — and delete what it points at.
	if status == http.StatusNotFound && recorded {
		looked, err := c.lookupUserID(ctx, in)
		if err != nil {
			return err
		}
		if looked == "" || looked == id {
			return nil // genuinely gone
		}
		if status, raw, err = c.deleteUser(ctx, looked); err != nil {
			return err
		}
		id = looked
	}

	switch status {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	case http.StatusForbidden:
		// Two different 403s live here and they need different fixes, so don't
		// blame auth for both: requireOwner rejects a non-owner token, and
		// DeleteUser rejects the household owner as immutable. The second is not a
		// misconfiguration at all — it means Purser was asked to offboard the
		// person who owns the library.
		return fmt.Errorf("lyceum: 403 deleting user %s — either PURSER_LYCEUM_OWNER_TOKEN is not an owner session token (with LYCEUM_AUTH=true), or this is the household owner, who cannot be removed (%s)", id, bodyMsg(raw))
	default:
		return apiError("delete user", status, raw)
	}
}

func (c *Connector) deleteUser(ctx context.Context, id string) (int, []byte, error) {
	return c.do(ctx, http.MethodDelete, "/admin/users/"+url.PathEscape(id), nil)
}

// resolveUserID picks the Lyceum user id to delete, preferring the recorded one
// and reporting whether that is where it came from.
//
// The recorded value is not always an id. Provision's 409 branch — the person
// already had a Lyceum account when they were invited — records the *email* in
// ExternalID, and Lyceum's DELETE handler ParseInts the path segment, so sending
// that yields 400 on every run, forever, with no fallback: the empty-id branch
// that knows how to look them up is never reached. Anyone who pre-existed their
// invite was therefore permanently un-offboardable (PRSR-17 review).
//
// Rather than trust the column, require the shape the endpoint requires. A value
// that is not a positive integer is treated as no id at all and resolved by
// email, which is what it was standing in for.
func (c *Connector) resolveUserID(ctx context.Context, in connector.Input) (id string, recorded bool, err error) {
	if raw := strings.TrimSpace(in.ExternalID); numericID(raw) {
		return raw, true, nil
	}
	id, err = c.lookupUserID(ctx, in)
	return id, false, err
}

// lookupUserID resolves the person's Lyceum id from their email, or "" if they
// have no account. Reconcile already does exactly this and refuses an emailless
// person, so it is reused rather than restated.
func (c *Connector) lookupUserID(ctx context.Context, in connector.Input) (string, error) {
	rec, err := c.Reconcile(ctx, in)
	if err != nil || !rec.Exists {
		return "", err
	}
	return rec.ExternalID, nil
}

// numericID reports whether s is the positive integer Lyceum's admin routes
// expect. Anything else — an email, a UUID, empty — is not an id here.
func numericID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
