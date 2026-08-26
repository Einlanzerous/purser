package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/invite"
	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

// inviteResponse is the POST /v1/invites result. The credential block (which
// contains one-time secrets) is included only for copy-paste delivery — for
// email delivery the secrets were sent to the recipient and are not echoed
// back over HTTP.
//
// The operator note is returned on both delivery paths: it holds no secrets, and
// it is the caller — not the invitee — reading this response. It is deliberately
// not part of credential_block, so that echoing the block to a recipient cannot
// carry connector error text along with it (PRSR-19).
type inviteResponse struct {
	InviteID        uuid.UUID    `json:"invite_id"`
	Person          personDTO    `json:"person"`
	Delivery        string       `json:"delivery"`
	Delivered       bool         `json:"delivered"`
	Outcomes        []outcomeDTO `json:"outcomes"`
	CredentialBlock string       `json:"credential_block,omitempty"`
	OperatorNote    string       `json:"operator_note,omitempty"`

	// NameConflict is present when the request's name disagreed with the stored
	// person and the stored name was kept (PRSR-20). Absent means no
	// disagreement — a caller that ignores it gets the old silent behaviour, so
	// it is reported rather than merely available.
	NameConflict *nameConflictDTO `json:"name_conflict,omitempty"`
}

type nameConflictDTO struct {
	Email     string `json:"email"`
	Stored    string `json:"stored"`
	Requested string `json:"requested"`
}

type personDTO struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Email string    `json:"email,omitempty"`
}

// outcomeDTO is the per-service result. Secrets are never serialized here; they
// live only in the credential block.
//
// A connector that was registered but couldn't provision reports
// status="unavailable" — it used to be status="failed" alongside pending=true,
// and that field is gone (PRSR-21). Deriving it back from the new status would
// have kept every caller compatible, at the cost of handing each of them the
// same two-fields-one-question ambiguity this change exists to remove.
type outcomeDTO struct {
	Service      string `json:"service"`
	DisplayName  string `json:"display_name"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	Username     string `json:"username,omitempty"`
	LoginURL     string `json:"login_url,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

func newInviteResponse(res *invite.Result) inviteResponse {
	out := inviteResponse{
		InviteID:     res.InviteID,
		Person:       personDTO{ID: res.Person.ID, Name: res.Person.Name, Email: res.Person.Email},
		Delivery:     string(res.Delivery),
		Delivered:    res.Delivered,
		OperatorNote: res.OperatorNote,
	}
	if res.Delivery == model.DeliverCopyPaste {
		out.CredentialBlock = res.CredentialBlock
	}
	if c := res.NameConflict; c != nil {
		out.NameConflict = &nameConflictDTO{Email: c.Email, Stored: c.Stored, Requested: c.Requested}
	}
	for _, o := range res.Outcomes {
		out.Outcomes = append(out.Outcomes, outcomeDTO{
			Service:      o.ServiceKey,
			DisplayName:  o.DisplayName,
			Status:       string(o.Status),
			Error:        o.Error,
			Username:     o.Username,
			LoginURL:     o.LoginURL,
			Instructions: o.Instructions,
		})
	}
	return out
}

// statusResponse is the GET /v1/invites/{id} result.
type statusResponse struct {
	InviteID    uuid.UUID  `json:"invite_id"`
	PersonID    uuid.UUID  `json:"person_id"`
	Delivery    string     `json:"delivery"`
	Role        string     `json:"role,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Tasks       []taskDTO  `json:"tasks"`
}

type taskDTO struct {
	ServiceID uuid.UUID `json:"service_id"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
}

func newStatusResponse(inv model.Invite, tasks []model.ProvisionTask) statusResponse {
	out := statusResponse{
		InviteID:    inv.ID,
		PersonID:    inv.PersonID,
		Delivery:    string(inv.Delivery),
		Role:        inv.Role,
		DeliveredAt: inv.DeliveredAt,
		CreatedAt:   inv.CreatedAt,
	}
	for _, t := range tasks {
		out.Tasks = append(out.Tasks, taskDTO{
			ServiceID: t.ServiceID,
			Status:    string(t.Status),
			Attempts:  t.Attempts,
			LastError: t.LastError,
		})
	}
	return out
}

// spinupResponse is the POST /v1/spinups result: the normalized spec the run
// worked from, one finding per resource kind, and the counts.
//
// The spec echoed back is the *normalized* one, not the request — hostnames are
// lowercased and a display name defaults to the key, and a caller comparing what
// it sent against what was recorded needs the form the run actually used.
type spinupResponse struct {
	Spec     spinupSpecDTO  `json:"spec"`
	Applied  bool           `json:"applied"`
	Findings []stepDTO      `json:"findings"`
	Counts   map[string]int `json:"counts"`
	// Pending is how many steps still want doing — what distinguishes "nothing
	// to do" from "re-run with apply". Statuses needing a human are excluded,
	// because re-running with apply does not fix any of them.
	Pending int `json:"pending"`
	// Changed is how many steps this run actually changed. Always 0 on a plan.
	Changed int `json:"changed"`
	// NeedsAttention names the kinds in a state a person has to resolve.
	//
	// Present because `pending` and `changed` are the two fields that look like
	// a verdict and are not one: an apply against an unconfigured deployment
	// answers 200 with `pending: 0, changed: 0`, which is byte-identical to an
	// edge that is already correct. Everything needed to tell them apart is in
	// `counts` and each finding's `status`, but so was the CLI's — and reading
	// the pending count as success is exactly the bug PRSR-31 shipped and then
	// fixed on that surface. It is computed from the same list the CLI's exit
	// code uses, so the two cannot disagree about what counts as fine.
	//
	// Empty means everything the spec asks for is in place — which is not quite
	// "the edge holds nothing else". A resource recorded at this hostname that
	// the spec no longer calls for reports `orphaned` and is excluded here on
	// purpose (see Result.NeedsAttention), because nothing this endpoint can do
	// would remove it. Read the finding's own status for that.
	NeedsAttention []string `json:"needs_attention,omitempty"`
}

type spinupSpecDTO struct {
	Service     string `json:"service"`
	DisplayName string `json:"display_name"`
	Hostname    string `json:"hostname"`
	Mode        string `json:"mode"`
	Upstream    string `json:"upstream"`
	Access      string `json:"access"`
	Logo        string `json:"logo,omitempty"`
	Tunnel      string `json:"tunnel,omitempty"`
}

// stepDTO is one resource kind's verdict.
//
// `status` carries the whole answer and there is no modifier beside it — the
// rule PRSR-21 established on the person axis, and the reason `refused` is a
// status of its own here rather than an `unknown` whose error text has to be
// read to tell a transport failure from a document nobody may write to
// (PRSR-31).
//
// external_id is omitted when empty rather than sent as "", because for a tunnel
// route it is empty *by nature*: the ingress configuration is one document per
// tunnel, so a route is identified by (tunnel, hostname) and has no id at all.
type stepDTO struct {
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	Detail      string `json:"detail,omitempty"`
	// Warning is trouble around a step that succeeded — see spinup.Resource.
	// Its own field rather than a clause inside detail, so a caller can find it
	// without pattern-matching a description.
	Warning    string `json:"warning,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	Applied    bool   `json:"applied"`
	Error      string `json:"error,omitempty"`
}

func newSpinupResponse(res *spinup.Result) spinupResponse {
	out := spinupResponse{
		Spec: spinupSpecDTO{
			Service:     res.Spec.Key,
			DisplayName: res.Spec.DisplayName,
			Hostname:    res.Spec.Hostname,
			Mode:        string(res.Spec.Mode),
			Upstream:    res.Spec.Upstream,
			Access:      string(res.Spec.Access),
			Logo:        string(res.Spec.Logo),
			Tunnel:      string(res.Spec.Tunnel),
		},
		Applied: res.Applied,
		Counts:  make(map[string]int),
		Pending: res.Pending(),
		Changed: res.Changed(),
	}
	for st, n := range res.Counts() {
		out.Counts[string(st)] = n
	}
	for _, f := range res.NeedsAttention() {
		out.NeedsAttention = append(out.NeedsAttention, string(f.Kind))
	}
	for _, f := range res.Findings {
		out.Findings = append(out.Findings, stepDTO{
			Kind:        string(f.Kind),
			DisplayName: f.DisplayName,
			Status:      string(f.Status),
			Detail:      f.Detail,
			Warning:     f.Warning,
			ExternalID:  f.ExternalID,
			Applied:     f.Applied,
			Error:       f.Err,
		})
	}
	return out
}
