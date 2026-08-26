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
		// A pointer, so "the field was absent" is distinguishable from
		// "success was false".
		//
		// Not every v4 route need answer with the envelope, and a plain bool
		// would read an absent `success` as failure — reporting a deletion that
		// happened as one that didn't. That is the PRSR-17 lie pointed
		// backwards: `Teardown` would leave a row active for a record it had
		// just removed.
		//
		// The route this was written for turned out not to need it. This comment
		// used to assert as fact that `DELETE /zones/{zone}/dns_records/{id}`
		// answers with a bare {"result":{"id":…}}; that was read off the
		// published schema, and PRSR-38 probed the live API on 2026-08-26 and
		// found the full envelope —
		// {"result":{"id":…},"success":true,"errors":[],"messages":[]}. So the
		// premise was wrong and the pointer is merely defensive here. Keep it:
		// it costs one dereference, and a route that *does* omit the envelope is
		// by definition the one nobody will think to re-check.
		Success *bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	// An empty body is the same question as an absent `success`, one step
	// further out, and it has to be answered before the decode rather than after
	// — json.Unmarshal fails on "" and would take the error branch below,
	// reporting a 204 as a failure. That is the bug the *bool above fixes,
	// arriving through the door the *bool cannot cover. No route this client
	// calls has actually been seen to answer empty — PRSR-38 got a body from
	// every one it probed, the DNS delete included — so this is kept for the
	// same reason as the pointer above, not because it is known to fire: a
	// deletion that happened, reported as one that didn't, is worth a branch.
	if len(bytes.TrimSpace(raw)) == 0 {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return raw, nil
		}
		return nil, &apiError{Method: method, Path: path, Status: resp.StatusCode}
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &apiError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	switch {
	case env.Success != nil && *env.Success:
		return raw, nil
	case env.Success == nil && resp.StatusCode >= 200 && resp.StatusCode < 300:
		// No envelope to judge by, so the transport status is the answer. A
		// route that *does* send one is unaffected: it takes the branch above.
		return raw, nil
	}
	e := &apiError{Method: method, Path: path, Status: resp.StatusCode}
	if len(env.Errors) > 0 {
		e.Code, e.Message = env.Errors[0].Code, env.Errors[0].Message
	}
	return nil, e
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

// errorCode returns Cloudflare's own error code from err, and whether err was a
// Cloudflare API error at all.
//
// This is as far as the *shared* client goes on the subject of "is this thing
// already gone", deliberately. Error codes are per-product — 81044 means a DNS
// record does not exist and says nothing about an Access application or a tunnel
// route — so a single `notFound()` living here would be a general-looking
// predicate that only answers for one resource type, in the one file the other
// two provisioners were told to share. Each resource type spells its own
// absence, from codes it has actually seen (see dnsRecordNotFound in dns.go).
func errorCode(err error) (int, bool) {
	var ae *apiError
	if !errors.As(err, &ae) {
		return 0, false
	}
	return ae.Code, true
}
