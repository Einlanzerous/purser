// Package catenary is Purser's connector for Catenary, the Construct's chat
// service (PRSR-50). It replaces R6's spike stub (catenary/spike/r6-purser,
// IDEA-29) with a connector against the real, pinned surface: CANT-131's
// `provision/openapi.yaml`, tagged `provision-v1` in the catenary repository.
//
// Three operations, not R6's five — Catenary's admin API turned out narrower
// than the spike guessed, because offboarding one person is a single
// server-side transaction rather than a fan-out this connector has to drive:
//
//	POST   /accounts                  create, find-existing, or reactivate
//	GET    /accounts?email=           read-only lookup, keyed on status
//	POST   /accounts/{id}/deactivate  end access everywhere, idempotently
//
// R6 asked whether connector.Connector generalizes to a per-device identity
// model and found that it does, because at provisioning time the person has
// zero devices — the only credential that can exist is a single bootstrap
// enrollment token, so Result.Secret fits without strain and Result.Extra is
// never needed. It also asked about Deprovision's fan-out and found the
// ordering of "disable" vs "revoke devices" load-bearing when a connector has
// to drive both steps itself. That hazard is now Catenary's own problem, not
// this connector's: deactivateAccount does both inside one transaction, so
// there is no ordering for this file to get right or wrong.
package catenary

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

// Config configures the connector.
type Config struct {
	// BaseURL is Catenary's provisioning surface — its own http.Server on
	// CATENARY_PROVISION_ADDR, never the routed listener. PURSER_CATENARY_BASE_URL.
	BaseURL string
	// ProvisionToken authenticates every request as
	// "Authorization: Bearer <ProvisionToken>". It is CATENARY_PROVISION_TOKEN,
	// generated in Signet, at least 32 bytes, and is presented VERBATIM — it is
	// never trimmed or otherwise transformed here, because the credential is
	// compared constant-time on the other end and a silently-altered value
	// would fail as an unrelated-looking 401 rather than a config error.
	// PURSER_CATENARY_PROVISION_TOKEN.
	ProvisionToken string
	HTTPClient     *http.Client
}

// Connector provisions Catenary accounts against the pinned provision-v1
// surface.
type Connector struct {
	cfg  Config
	http *http.Client
}

// New builds the connector. BaseURL and ProvisionToken are required.
func New(cfg Config) (*Connector, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("catenary: BaseURL is required")
	}
	if cfg.ProvisionToken == "" {
		return nil, errors.New("catenary: ProvisionToken is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Connector{cfg: cfg, http: hc}, nil
}

func (c *Connector) Key() string         { return "catenary" }
func (c *Connector) DisplayName() string { return "Catenary" }
func (c *Connector) Icon() string        { return "🚋" }

// account is components.schemas.Account in provision-v1.
type account struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}

// ensureRequest is components.schemas.EnsureRequest. It carries exactly the
// two fields the contract describes — "unknown fields are refused with 400,
// not ignored" — and DisplayName is omitted rather than sent empty, because an
// omitted field is what makes ensureAccount fall back to the derived handle;
// sending "" is a different (and unspecified) request.
type ensureRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
}

// ensureResponse is components.schemas.EnsureResponse.
type ensureResponse struct {
	Account             account `json:"account"`
	EnrollmentToken     string  `json:"enrollment_token"`
	EnrollmentExpiresAt string  `json:"enrollment_expires_at"`
	Outcome             string  `json:"outcome"`
	Note                string  `json:"note,omitempty"`
}

// lookupResponse is components.schemas.LookupResponse — FLAT, unlike
// EnsureResponse's nested account, per the contract's own note on the schema.
type lookupResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	LiveDevices int    `json:"live_devices"`
}

// errorBody is components.schemas.Error — a closed six-value set.
type errorBody struct {
	Error string `json:"error"`
}

// Provision creates the person's Catenary account, or ensures/reactivates an
// existing one, and returns the one enrollment token that call issued.
//
// ONE idempotent call — ensureAccount has no 409-then-reissue second step, so
// unlike Argosy/Switchyard/Lyceum there is no conflict branch here at all: 201
// and 200 both land in the same success path, because Catenary already
// resolved "created vs existing vs reactivated" into `outcome` before
// responding.
//
// `outcome: reactivated` and any `note` are carried into Result.Instructions
// rather than dropped, because the operator reading the credential block never
// sees Catenary's own response — only what this connector reports. A
// reactivation means a previously offboarded person was just let back in
// (every earlier device, token and pending invitation was revoked first, on
// Catenary's side, inside the same transaction); a note means Catenary
// assigned a suffixed handle because an email-less person already held the
// plain one, and the operator may need `catenary user set-email`.
func (c *Connector) Provision(ctx context.Context, in connector.Input) (connector.Result, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return connector.Result{}, errors.New("catenary: an email is required to create an account")
	}

	status, raw, err := c.do(ctx, http.MethodPost, "/accounts", ensureRequest{
		Email:       email,
		DisplayName: strings.TrimSpace(in.PersonName),
	})
	if err != nil {
		return connector.Result{}, err
	}

	switch status {
	case http.StatusCreated, http.StatusOK:
		var er ensureResponse
		if err := json.Unmarshal(raw, &er); err != nil {
			return connector.Result{}, fmt.Errorf("catenary: decode ensureAccount response: %w", err)
		}
		return result(er), nil
	default:
		return connector.Result{}, apiError("create account", status, raw)
	}
}

// result builds the credential-block Result from a successful ensureAccount
// response. display_name is deliberately not echoed back into a rename claim
// anywhere here — Catenary honors it on creation only, so a re-invite with a
// different PersonName does not change what a caller with an existing account
// already sees on their profile.
func result(er ensureResponse) connector.Result {
	var b strings.Builder
	b.WriteString("Install Catenary, then paste this enrollment token on the sign-in screen — ")
	b.WriteString("it redeems once, into your first device. Add further devices from an ")
	b.WriteString("already-signed-in one.")
	if er.Outcome == "reactivated" {
		b.WriteString(" This account was previously offboarded and has just been reactivated: ")
		b.WriteString("every earlier device, token and pending invitation was already revoked, ")
		b.WriteString("so this token starts them fresh.")
	}
	if er.Note != "" {
		b.WriteString(" Note from Catenary: ")
		b.WriteString(er.Note)
	}
	return connector.Result{
		ExternalID:   er.Account.ID,
		Username:     er.Account.Handle,
		Secret:       er.EnrollmentToken,
		SecretLabel:  "enrollment token (single-use, redeems into your first device)",
		Instructions: b.String(),
	}
}

// Reconcile reports whether the person currently has access, read-only.
//
// Exists means status == "active", never "a row came back" — a deactivated
// account is Exists: false, a 404 is Exists: false, and anything else (a 401,
// a 500, a network failure) is an error, never silently folded into
// Exists: false. That distinction is the whole of Purser's own rule that no
// connector may answer a reconcile it cannot verify: an unreachable Catenary
// is unknown, not "nobody has access".
func (c *Connector) Reconcile(ctx context.Context, in connector.Input) (connector.ReconcileResult, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		return connector.ReconcileResult{}, errors.New("catenary: an email is required to reconcile")
	}
	a, err := c.lookup(ctx, email)
	if err != nil {
		return connector.ReconcileResult{}, err
	}
	if a == nil || a.Status != "active" {
		return connector.ReconcileResult{Exists: false}, nil
	}
	return connector.ReconcileResult{Exists: true, ExternalID: a.ID, Username: a.Handle}, nil
}

// lookup performs the read-only GET /accounts?email= call. It returns (nil,
// nil) for a 404 — "nobody holds that address", including a malformed one,
// per the contract's own refusal to run a second address normalizer — and a
// non-nil error for anything else that isn't a 200, so a caller can tell
// "no such account" apart from "could not tell".
func (c *Connector) lookup(ctx context.Context, email string) (*lookupResponse, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/accounts?email="+url.QueryEscape(email), nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var lr lookupResponse
		if err := json.Unmarshal(raw, &lr); err != nil {
			return nil, fmt.Errorf("catenary: decode lookup response: %w", err)
		}
		return &lr, nil
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, apiError("lookup account", status, raw)
	}
}

// Deprovision ends the person's access to Catenary, everywhere, at once.
//
// Two paths, both ending in exactly one mutating call:
//
//   - Input.ExternalID set (the normal offboard path — Purser's own record of
//     account.id): deactivate that id directly. No lookup.
//   - Input.ExternalID empty: a read-only lookup by email finds the id first.
//     A 404 there is success — nothing to revoke.
//
// A 404 from deactivate itself is never success, on EITHER path: Catenary
// never deletes accounts, so an id that resolved a moment ago (via a lookup
// this call just made) or that Purser itself recorded from a past Provision
// cannot legitimately 404 here. That is a wrong record or a live bug, and an
// operator needs to see it — not a quiet "nothing to revoke".
//
// Idempotent: deactivateAccount is 204 every time, including the second and
// subsequent calls, because a deactivated account still resolves by both id
// and email. So a retry after a transient failure — or an offboard that simply
// runs twice — always converges rather than erroring on an already-gone
// account.
func (c *Connector) Deprovision(ctx context.Context, in connector.Input) error {
	id := strings.TrimSpace(in.ExternalID)
	recorded := id != ""

	if !recorded {
		email := strings.ToLower(strings.TrimSpace(in.Email))
		if email == "" {
			return errors.New("catenary: an email is required to deprovision when no account id is recorded")
		}
		a, err := c.lookup(ctx, email)
		if err != nil {
			return err
		}
		if a == nil {
			return nil // nothing upstream: a success, so a failed-only retry is safe
		}
		id = a.ID
	}

	status, raw, err := c.do(ctx, http.MethodPost, "/accounts/"+url.PathEscape(id)+"/deactivate", nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		if recorded {
			return fmt.Errorf("catenary: deactivate %s: no such account, but Purser recorded this id and Catenary never deletes accounts — the record is wrong, not the access: %s", id, bodyMsg(raw))
		}
		return fmt.Errorf("catenary: deactivate %s: no such account, moments after a successful lookup — Catenary never deletes accounts, so this should not happen: %s", id, bodyMsg(raw))
	default:
		return apiError("deactivate account", status, raw)
	}
}

// Note: this connector does not implement connector.RevokeChecker. Catenary
// can always revoke a configured account (deactivateAccount has no
// "can't" state short of the credential itself being wrong, which New already
// refuses to construct without), so it relies on the documented default: "a
// connector that does not implement it is assumed able."

func (c *Connector) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("catenary: marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("catenary: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.ProvisionToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("catenary: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("catenary: read body: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// bodyMsg extracts the {"error": "..."} message, or returns the trimmed raw
// body when it doesn't parse as the contract's error shape.
func bodyMsg(raw []byte) string {
	var eb errorBody
	if err := json.Unmarshal(raw, &eb); err == nil && eb.Error != "" {
		return eb.Error
	}
	return strings.TrimSpace(string(raw))
}

func apiError(op string, status int, raw []byte) error {
	return fmt.Errorf("catenary: %s: %d: %s", op, status, bodyMsg(raw))
}

// Compile-time proof this satisfies the real contract.
var _ connector.Connector = (*Connector)(nil)
