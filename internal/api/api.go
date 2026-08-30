// Package api is Purser's thin HTTP surface over the two orchestrators. It is
// deliberately small: a health check, an endpoint to run an invite, one to read
// an invite's status, and one to stand up a service's edge. The CLI shares the
// same orchestrators, so this is a convenience for automation (n8n, scripts)
// rather than the primary interface.
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
	"github.com/Einlanzerous/purser/internal/spinup"
	"github.com/Einlanzerous/purser/internal/store"
	"github.com/Einlanzerous/purser/internal/version"
)

// Server serves the Purser HTTP API.
type Server struct {
	svc      *invite.Service
	spin     *spinup.Service
	store    *store.Store
	apiToken string
}

// New builds the API server. apiToken, when non-empty, is required as a bearer
// token on the /v1 endpoints.
//
// spin may be nil, which disables the spin-up endpoint rather than panicking on
// it — the composition root always supplies one, and a caller constructing this
// for a narrower purpose should get a 503 naming the omission rather than a
// crash on the first request.
func New(svc *invite.Service, spin *spinup.Service, st *store.Store, apiToken string) *Server {
	if apiToken == "" {
		log.Printf("api: PURSER_API_TOKEN is empty — /v1 endpoints are UNAUTHENTICATED (fine only behind construct_net/Tailscale)")
	}
	return &Server{svc: svc, spin: spin, store: st, apiToken: apiToken}
}

// Handler returns the mux with all routes registered.
func (s *Server) Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/invites", s.auth(s.handleCreateInvite))
	mux.HandleFunc("GET /v1/invites/{id}", s.auth(s.handleGetInvite))
	mux.HandleFunc("POST /v1/spinups", s.auth(s.handleSpinup))
	mux.HandleFunc("POST /v1/teardowns", s.auth(s.handleTeardown))
	return mux
}

// healthResponse is the body of `GET /healthz`.
//
// ── Why this grew a version and a sha (PRSR-32) ────────────────────────────
//
// Switchyard's delivery reconciler polls this endpoint and records what is
// actually running, which is the observed half of the estate's delivery ledger
// (SWY-192 defines the contract; SERV-128 owns the rollout across services).
// Before these two fields purser probed as `no_version`: reachable and
// speaking, but unable to say WHICH build was speaking — so no deploy of purser
// could ever be corroborated.
//
// The field names and types are the contract, not a local choice:
//
//	version  bare semver ("0.14.0") or the literal "dev". Never a "v" prefix —
//	         it is compared with strict equality against the image's
//	         org.opencontainers.image.version label, which docker's
//	         metadata-action stamps bare. A prefix here files every deploy
//	         report as `claimed_not_confirmed`, permanently.
//	sha      the full 40-char commit, or JSON null. Never abbreviated: the
//	         cross-service comparison is an equality test, not a prefix match.
//
// A struct rather than the previous map[string]string, because `sha` has to be
// able to marshal as null and a map of strings cannot express that.
type healthResponse struct {
	Status  string  `json:"status"`
	Service string  `json:"service"`
	Version string  `json:"version"`
	SHA     *string `json:"sha"`
}

// handleHealth answers the liveness probe and the build-identity contract.
//
// Deliberately does not consult Postgres or any upstream connector. Liveness and
// readiness answer different questions, and a liveness probe that fails on a
// degraded dependency gets the container killed and restarted at exactly the
// moment somebody wants to look at it.
//
// That is also why there is only a 200 path here rather than the 200/503 pair
// the contract permits. The contract's rule is that a 503 must carry the SAME
// body shape — a degraded service is still running a version, and it is the one
// most worth identifying — so if a readiness verdict is ever added it belongs
// in this struct on both branches, not in a second shape.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	id := version.Get()
	writeJSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Service: "purser",
		Version: id.Version,
		SHA:     id.SHA,
	})
}

// inviteRequest is the POST /v1/invites body.
type inviteRequest struct {
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Services []string `json:"services"`
	Bundle   string   `json:"bundle"` // named bundle; with no services, omitting both uses the default
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
		Bundle:   req.Bundle,
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
		// A refused email delivery is the caller's to fix, not an outage, and the
		// message names what disagreed — returning "provisioning failed" with a
		// 500 would hide the one thing they need in order to correct it.
		// Validate can't catch this: it takes a database read to know.
		if errors.Is(err, invite.ErrNameConflictOnEmail) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		log.Printf("api: invite run failed: %v", err)
		writeError(w, http.StatusInternalServerError, "provisioning failed")
		return
	}
	// Logged as well as returned. name_conflict is an optional field, and a
	// caller that doesn't read it would otherwise get exactly the silent rename
	// behaviour PRSR-20 removed — server-side there would be no record at all.
	if c := res.NameConflict; c != nil {
		log.Printf("api: invite %s: %s is recorded as %q, not %q — kept the recorded name",
			res.InviteID, c.Email, c.Stored, c.Requested)
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

// spinupRequest is the POST /v1/spinups body: a ServiceSpec, plus whether to
// act on it.
//
// `apply` defaults to false, so the unadorned request is a plan. That is the
// same default the CLI has and it matters more here: this endpoint creates real
// edge infrastructure, and a caller who forgets the field gets a report rather
// than a rewritten ingress document.
type spinupRequest struct {
	Service     string `json:"service"`
	DisplayName string `json:"display_name"`
	Hostname    string `json:"hostname"`
	Mode        string `json:"mode"`
	Upstream    string `json:"upstream"`
	Access      string `json:"access"`
	// Logo is a ref, not a URL: "placard", "none", or an https URL. Renamed
	// from logo_url with the field's meaning in PRSR-37 — a name that still said
	// URL would invite one, and the commonest value is now a keyword.
	Logo string `json:"logo"`
	// LogoURL only exists to refuse. encoding/json drops unknown fields, so a
	// caller written against the previous release would have had its explicit
	// icon silently discarded and the spec silently defaulted to "placard" —
	// non-destructive, since no unresolved answer clears an icon, but the
	// caller's instruction would vanish with no error and nothing in the
	// response to say so. POST /v1/spinups shipped one release ago (PRSR-31),
	// so the population that could send it is small and worth telling rather
	// than guessing for: the value is still legal as a Logo ref, but "carry it
	// across" and "you probably want placard now" are different intentions and
	// this is not the layer to pick between them.
	LogoURL string `json:"logo_url"`
	Tunnel  string `json:"tunnel"`
	Apply   bool   `json:"apply"`
	// Prune asks the run to remove what the spec no longer calls for (PRSR-46).
	//
	// Both this and `apply` are needed to remove anything, and both default to
	// false — so a caller written against an earlier release gets exactly the
	// behaviour it was written for, and the one destructive thing this endpoint
	// can do takes two fields nobody sets by accident.
	Prune bool `json:"prune"`
}

func (s *Server) handleSpinup(w http.ResponseWriter, r *http.Request) {
	if s.spin == nil {
		writeError(w, http.StatusServiceUnavailable, "this build has no spin-up orchestrator wired")
		return
	}
	var req spinupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.LogoURL != "" {
		writeError(w, http.StatusBadRequest,
			`"logo_url" was renamed to "logo" in PRSR-37 and is now a ref rather than a url: "placard" resolves the mark by service key, "none" clears the icon, or pass an https url to keep naming one yourself`)
		return
	}
	spec := spinup.ServiceSpec{
		Key:         req.Service,
		DisplayName: req.DisplayName,
		Hostname:    req.Hostname,
		Mode:        spinup.Mode(req.Mode),
		Upstream:    req.Upstream,
		Access:      spinup.AccessShape(req.Access),
		Logo:        spinup.LogoRef(req.Logo),
		Tunnel:      spinup.TunnelRef(req.Tunnel),
	}
	// Validated here so a malformed spec is a 400 rather than a 500. Ensure
	// validates again — it is the orchestrator's own precondition and not
	// something a caller may skip — and this call is what decides whose fault
	// the refusal is.
	if _, err := spec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.spin.Ensure(r.Context(), spinup.Request{Spec: spec, Apply: req.Apply, Prune: req.Prune})
	if err != nil {
		// Everything a provisioner did or failed to do is a finding, so this is
		// only reached for a request that could not be attempted: an
		// unresolvable tunnel ref, or a failed read of Purser's own records. The
		// first is the caller's to fix and names itself; the second is an
		// outage. They are told apart by the spec having already validated
		// above, which is what leaves the tunnel ref as the one caller-side
		// refusal Ensure can still raise.
		if errors.Is(err, spinup.ErrTunnelUnconfigured) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("api: spin-up of %s failed: %v", spec.Hostname, err)
		writeError(w, http.StatusInternalServerError, "spin-up failed")
		return
	}
	// Logged as well as returned, unlike the per-step details, because a caller
	// that ignores these fields would otherwise leave no trace of them anywhere
	// (compare name_conflict above). Both are things a 200 with sensible counts
	// does not reveal:
	//
	//   applied-not-recorded — the edge changed and the row didn't, so Purser
	//     cannot tear down what it just created.
	//   warning — the step *succeeded*, and something around it may not have.
	//     The tunnel's is the one that matters: another service's ingress route
	//     may have been dropped from the shared document, which is somebody
	//     else's outage and appears nowhere in this response's status or counts.
	//
	// The CLI does not double-log these; it prints them itself, and `log` writes
	// to the same stderr its report does.
	for _, f := range res.Findings {
		if f.Status == spinup.StepAppliedNotRecorded {
			log.Printf("api: spin-up of %s: %s changed upstream but was not recorded: %s",
				spec.Hostname, f.Kind, f.Err)
		}
		if f.Status == spinup.StepPrunedNotRecorded {
			// The mirror of the line above and logged for its reason: the
			// resource is gone and Purser holds a live-looking row for it, which
			// a later run reads as something to adopt.
			log.Printf("api: spin-up of %s: %s was removed but not recorded: %s",
				spec.Hostname, f.Kind, f.Err)
		}
		if f.Warning != "" {
			log.Printf("api: spin-up of %s: %s: %s", spec.Hostname, f.Kind, f.Warning)
		}
	}
	writeJSON(w, http.StatusOK, newSpinupResponse(res))
}

// teardownRequest is the POST /v1/teardowns body (PRSR-34).
//
// Both identifiers are required, and that is the endpoint's guard rather than a
// formality: `service` is checked against Purser's own records before anything
// is removed, and a disagreement refuses the whole request. A hostname alone
// would have this endpoint delete whatever the rows happen to say, which is the
// question PRSR-34 was filed to answer — a row proves Purser created something
// here, not that the hostname is still this service's.
//
// `apply` defaults to false, so the unadorned request is a plan — the same
// default the CLI has, and it matters more here than on POST /v1/spinups: every
// step of this one is a deletion, and a caller who forgets the field gets a
// report rather than a hostname that stops resolving.
type teardownRequest struct {
	Service  string `json:"service"`
	Hostname string `json:"hostname"`
	Apply    bool   `json:"apply"`
}

func (s *Server) handleTeardown(w http.ResponseWriter, r *http.Request) {
	if s.spin == nil {
		writeError(w, http.StatusServiceUnavailable, "this build has no spin-up orchestrator wired")
		return
	}
	var req teardownRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	tr := spinup.TeardownRequest{
		ServiceKey: req.Service,
		Hostname:   req.Hostname,
		Apply:      req.Apply,
	}
	// Validated here so a malformed request is a 400 rather than a 500. Teardown
	// validates again — it is the orchestrator's own precondition and not
	// something a caller may skip — and this call is what decides whose fault
	// the refusal is, exactly as handleSpinup's spec.Validate does.
	if _, err := tr.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.spin.Teardown(r.Context(), tr)
	if err != nil {
		// Everything a provisioner did or failed to do is a finding, and the
		// request itself validated above, so this is only reached for the
		// ownership refusal or a failed read of Purser's own records.
		if errors.Is(err, spinup.ErrHostnameNotThisService) {
			// 409 rather than 400: the request is well-formed, and it is the
			// *state* that refuses it — the same reading ErrNameConflictOnEmail
			// gets on the invite path. It is also the one refusal here worth a
			// distinct code, because the fix is to look at who owns the hostname
			// rather than to correct the request.
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		log.Printf("api: teardown of %s failed: %v", req.Hostname, err)
		writeError(w, http.StatusInternalServerError, "teardown failed")
		return
	}
	// Logged as well as returned, for handleSpinup's reason: a caller that
	// ignores these fields would otherwise leave no trace of them anywhere, and
	// a 200 with sensible counts reveals neither.
	//
	//   removed-not-recorded — the resource is gone and the row still says it is
	//     live, so a later spin-up reads it as something to adopt.
	//   warning — the removal *succeeded*, and something around it may not have.
	//     The tunnel's is the one that matters: another service's ingress route
	//     may have been dropped from the shared document, which is somebody
	//     else's outage and appears nowhere in this response's status or counts.
	for _, f := range res.Findings {
		if f.Status == spinup.TeardownRemovedNotRecorded {
			log.Printf("api: teardown of %s: %s was removed upstream but not recorded: %s",
				res.Hostname, f.Kind, f.Err)
		}
		if f.Warning != "" {
			log.Printf("api: teardown of %s: %s: %s", res.Hostname, f.Kind, f.Warning)
		}
	}
	writeJSON(w, http.StatusOK, newTeardownResponse(res))
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
