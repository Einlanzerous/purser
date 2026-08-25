package cloudflare

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
)

// This file is the Cloudflare API v4 transport: bearer auth, the
// {"success":false,"errors":[…]} envelope, and nothing service-specific. It is
// shared by both of Purser's axes — the Access connector above it (person ×
// service) and the DNS provisioner in dns.go (service spin-up, PRSR-28).
//
// It is a *move*, not a rework. do() already took a free-form path appended to
// the base URL, so it served /zones/{zone}/… exactly as well as
// /accounts/{acct}/…; all that changed is that it now hangs off a small type
// two callers can hold instead of off the Access connector, which carries group
// credentials a DNS provisioner has no business being handed. Keeping the zone
// coordinates out of Config is deliberate (see internal/config): folding them
// into the Access connector's readiness check would take `--to cloudflare`
// offline for every deployment that hasn't set them.

const apiBase = "https://api.cloudflare.com/client/v4"

// client performs authenticated Cloudflare API requests.
type client struct {
	token   string
	http    *http.Client
	baseURL string // overridable in tests
}

// newClient builds a transport for a token, defaulting the HTTP client.
func newClient(token string, hc *http.Client) *client {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &client{token: token, http: hc, baseURL: apiBase}
}

// do performs a Cloudflare API request and returns the raw body, translating the
// {"success":false,"errors":[…]} envelope into an *apiError.
func (c *client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
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
	req.Header.Set("Authorization", "Bearer "+c.token)
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
		return nil, &apiError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if !env.Success {
		e := &apiError{Method: method, Path: path, Status: resp.StatusCode}
		if len(env.Errors) > 0 {
			e.Code, e.Message = env.Errors[0].Code, env.Errors[0].Message
		}
		return nil, e
	}
	return raw, nil
}

// apiError is a Cloudflare request that did not succeed, carrying the transport
// status and the envelope's first error code alongside the message.
//
// The fields exist so a caller can ask "was this a 404?" without matching on
// prose — Teardown needs it, because a DNS record that is already gone is a
// successful teardown and an error message is the wrong way to learn that. The
// rendered text is unchanged from the fmt.Errorf calls this replaced.
type apiError struct {
	Method  string
	Path    string
	Status  int
	Code    int    // Cloudflare's own error code, 0 if the envelope carried none
	Message string // Cloudflare's error message
	Body    string // set instead of Code/Message when the body wasn't the envelope
}

func (e *apiError) Error() string {
	switch {
	case e.Body != "":
		return fmt.Sprintf("cloudflare: %s %s: %d: %s", e.Method, e.Path, e.Status, e.Body)
	case e.Message != "":
		return fmt.Sprintf("cloudflare: %s %s: %d %s", e.Method, e.Path, e.Code, e.Message)
	default:
		return fmt.Sprintf("cloudflare: %s %s: request unsuccessful (%d)", e.Method, e.Path, e.Status)
	}
}

// Cloudflare's DNS "there is no such record" codes. A 404 alone is not enough to
// go on — a mistyped base URL 404s too — so the code is checked as well, and
// either one being conclusive is deliberate: both spellings mean the object is
// not there.
const (
	errCodeRecordNotFound = 81044 // "Record does not exist."
	errCodeRecordMissing  = 81045 // "Record does not exist. (81045)" on some routes
)

// notFound reports whether err is Cloudflare saying the object isn't there.
//
// "Already gone" is success for a teardown and must never surface as a failure:
// the person axis learned the inverse of this the hard way (a revoke that didn't
// happen recorded as one, PRSR-17), and reporting a failure for a record that is
// genuinely absent is the same class of lie pointed the other way — it leaves a
// removed resource recorded as live.
func notFound(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusNotFound || ae.Code == errCodeRecordNotFound || ae.Code == errCodeRecordMissing
}
