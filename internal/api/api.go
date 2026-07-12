// Package api is Purser's thin HTTP surface over the invite orchestrator. It is
// deliberately small: a health check, an endpoint to run an invite, and one to
// read an invite's status. The CLI shares the same orchestrator, so this is a
// convenience for automation (n8n, scripts) rather than the primary interface.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/invite"
	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/store"
)

// Server serves the Purser HTTP API.
type Server struct {
	svc      *invite.Service
	store    *store.Store
	apiToken string
}

// New builds the API server. apiToken, when non-empty, is required as a bearer
// token on the /v1 endpoints.
func New(svc *invite.Service, st *store.Store, apiToken string) *Server {
	if apiToken == "" {
		log.Printf("api: PURSER_API_TOKEN is empty — /v1 endpoints are UNAUTHENTICATED (fine only behind construct_net/Tailscale)")
	}
	return &Server{svc: svc, store: st, apiToken: apiToken}
}

// Handler returns the mux with all routes registered.
func (s *Server) Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/invites", s.auth(s.handleCreateInvite))
	mux.HandleFunc("GET /v1/invites/{id}", s.auth(s.handleGetInvite))
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "purser"})
}

// inviteRequest is the POST /v1/invites body.
type inviteRequest struct {
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Services []string `json:"services"`
	Role     string   `json:"role"`
	Deliver  string   `json:"deliver"`
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	var req inviteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	deliver := model.DeliveryMethod(req.Deliver)
	if deliver == "" {
		deliver = model.DeliverCopyPaste
	}

	inviteReq := invite.Request{
		Name:     req.Name,
		Email:    req.Email,
		Services: req.Services,
		Role:     req.Role,
		Delivery: deliver,
	}
	// Validation errors are the caller's fault (400); a failure inside Run is an
	// infrastructure error (500) and its raw text is not leaked to the client.
	if err := s.svc.Validate(inviteReq); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.svc.Run(r.Context(), inviteReq)
	if err != nil {
		log.Printf("api: invite run failed: %v", err)
		writeError(w, http.StatusInternalServerError, "provisioning failed")
		return
	}
	writeJSON(w, http.StatusOK, newInviteResponse(res))
}

func (s *Server) handleGetInvite(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid invite id")
		return
	}
	inv, tasks, err := s.store.InviteWithTasks(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "invite not found")
		return
	}
	if err != nil {
		log.Printf("api: get invite failed: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, newStatusResponse(inv, tasks))
}

// auth wraps a handler with bearer-token check when an API token is configured.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken != "" {
			const prefix = "Bearer "
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, prefix) ||
				subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(s.apiToken)) != 1 {
				writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
				return
			}
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
