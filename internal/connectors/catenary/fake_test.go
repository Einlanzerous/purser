package catenary

// fakeCatenary is an in-memory stand-in for Catenary's pinned provision-v1
// surface (testdata/provision-v1.openapi.yaml), built for this package's own
// tests. It is checked against that document by contract_test.go rather than
// trusted on its own — PRSR-50's brief is explicit that a hand-shaped double
// is not good enough here.
//
// It models the one thing R6's stub got right and this ticket's brief makes
// explicit: Catenary's offboard is ONE transaction. There is no device list to
// fan out over here — deactivateAccount either lands whole or doesn't land at
// all, which is what FailDeactivate exists to simulate.

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// fakeAccount is the double's internal record — a superset of what either
// wire shape (Account, nested in EnsureResponse; LookupResponse, flat) reports.
type fakeAccount struct {
	ID          string
	Email       string // "" for a bot: a bot holds no email and is unreachable via lookup
	Handle      string
	DisplayName string
	Status      string // "active" | "deactivated"
	Bot         bool
	LiveDevices int
}

type fakeCatenary struct {
	mu      sync.Mutex
	token   string
	baseURL string                  // set by start(); lets a test probe the fake directly
	byEmail map[string]*fakeAccount // keyed on lowercased email; bots are absent
	byID    map[string]*fakeAccount
	handles map[string]bool

	// FailDeactivate, set to an account id, makes THAT id's deactivate call
	// answer 500 and change nothing — modeling a failed mutating call so a
	// test can assert the account was left untouched.
	FailDeactivate string

	// Calls records every request as "METHOD path", in order, so a test can
	// assert exactly which — and how many — calls the connector made.
	Calls []string
}

func newFake(token string) *fakeCatenary {
	return &fakeCatenary{
		token:   token,
		byEmail: map[string]*fakeAccount{},
		byID:    map[string]*fakeAccount{},
		handles: map[string]bool{},
	}
}

func (f *fakeCatenary) start() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(f.route))
	f.baseURL = srv.URL
	return srv
}

// seed adds an active account directly, standing in for someone who already
// has one, bypassing ensureAccount so a test can control the handle exactly.
func (f *fakeCatenary) seed(email, handle, displayName string) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := &fakeAccount{
		ID: uuid.NewString(), Email: strings.ToLower(email), Handle: handle,
		DisplayName: displayName, Status: "active",
	}
	f.byEmail[a.Email] = a
	f.byID[a.ID] = a
	f.handles[handle] = true
	return a
}

// seedBot adds a service account with no email — reachable only by id, and
// deactivateAccount must answer exactly the same 404 for it as for an id
// nothing holds (the surface will not confirm that a bot exists here at all).
func (f *fakeCatenary) seedBot(id string) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := &fakeAccount{ID: id, Bot: true, Status: "active"}
	f.byID[a.ID] = a
	return a
}

// occupyHandle reserves a handle without an email, standing in for the
// "email-less person already holds the plain handle" case that produces
// EnsureResponse.note on a later create.
func (f *fakeCatenary) occupyHandle(handle string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handles[handle] = true
}

// account returns the live record by email, for assertions.
func (f *fakeCatenary) account(email string) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byEmail[strings.ToLower(email)]
}

func (f *fakeCatenary) record(s string) {
	f.Calls = append(f.Calls, s)
}

func (f *fakeCatenary) route(w http.ResponseWriter, r *http.Request) {
	// Every operation is behind the credential, identically for all three.
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	switch {
	case r.URL.Path == "/accounts" && r.Method == http.MethodPost:
		f.handleEnsure(w, r)
	case r.URL.Path == "/accounts" && r.Method == http.MethodGet:
		f.handleLookup(w, r)
	case strings.HasPrefix(r.URL.Path, "/accounts/") && strings.HasSuffix(r.URL.Path, "/deactivate") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/accounts/"), "/deactivate")
		f.handleDeactivate(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeCatenary) handleEnsure(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("POST /accounts")

	// additionalProperties: false — strict decode is what makes this branch a
	// faithful double of "unknown fields are refused with 400, not ignored".
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	}
	if err := dec.Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "request is not valid")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if len(email) < 3 || !strings.Contains(email, "@") {
		writeErr(w, http.StatusBadRequest, "email is not a valid address")
		return
	}

	if existing, ok := f.byEmail[email]; ok {
		outcome := "existing"
		if existing.Status != "active" {
			existing.Status = "active"
			outcome = "reactivated"
		}
		writeJSON(w, http.StatusOK, f.ensureWire(existing, mintToken(), outcome, ""))
		return
	}

	handle, note := f.deriveHandle(email, body.DisplayName)
	a := &fakeAccount{
		ID: uuid.NewString(), Email: email, Handle: handle,
		DisplayName: body.DisplayName, Status: "active",
	}
	f.byEmail[email] = a
	f.byID[a.ID] = a
	f.handles[handle] = true
	writeJSON(w, http.StatusCreated, f.ensureWire(a, mintToken(), "created", note))
}

// deriveHandle mirrors the contract's own description: derived from the
// email's local part, de-duplicated. A collision only ever comes from a
// handle occupied by an email-less person here (occupyHandle) — two accounts
// with real, distinct emails never collide, since the base handle is the
// local part of THIS email.
func (f *fakeCatenary) deriveHandle(email, _ string) (handle, note string) {
	local := email
	if i := strings.Index(email, "@"); i >= 0 {
		local = email[:i]
	}
	if !f.handles[local] {
		return local, ""
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", local, n)
		if !f.handles[candidate] {
			return candidate, fmt.Sprintf(
				"assigned %s because %s has no email; if that is the same person, run `catenary user set-email`",
				candidate, local)
		}
	}
}

func (f *fakeCatenary) handleLookup(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	email := r.URL.Query().Get("email")
	f.record("GET /accounts?email=" + email)
	if strings.TrimSpace(email) == "" {
		writeErr(w, http.StatusBadRequest, "the email query parameter is required")
		return
	}
	a, ok := f.byEmail[strings.ToLower(strings.TrimSpace(email))]
	if !ok {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	writeJSON(w, http.StatusOK, lookupWire(a))
}

func (f *fakeCatenary) handleDeactivate(w http.ResponseWriter, _ *http.Request, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("POST /accounts/" + id + "/deactivate")

	if id != "" && id == f.FailDeactivate {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Byte-identical 404 for an unknown id, a bot's id, and (implicitly, since
	// it is never a map key) an id that is not a UUID at all.
	a, ok := f.byID[id]
	if !ok || a.Bot {
		writeErr(w, http.StatusNotFound, "no such account")
		return
	}
	a.Status = "deactivated"
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeCatenary) ensureWire(a *fakeAccount, token, outcome, note string) map[string]any {
	body := map[string]any{
		"account": map[string]any{
			"id": a.ID, "email": a.Email, "handle": a.Handle,
			"display_name": a.DisplayName, "status": a.Status,
		},
		"enrollment_token":      token,
		"enrollment_expires_at": time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z"),
		"outcome":               outcome,
	}
	if note != "" {
		body["note"] = note
	}
	return body
}

func lookupWire(a *fakeAccount) map[string]any {
	return map[string]any{
		"id": a.ID, "email": a.Email, "handle": a.Handle,
		"display_name": a.DisplayName, "status": a.Status,
		"live_devices": a.LiveDevices,
	}
}

// mintToken produces a Token per the contract's own pattern: 32 CSPRNG bytes,
// base64url without padding, exactly 43 characters.
func mintToken() string {
	var buf [32]byte
	_, _ = rand.Read(buf[:])
	return base64.RawURLEncoding.EncodeToString(buf[:])
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
