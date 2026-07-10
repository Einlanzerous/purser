package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/invite"
	"github.com/Einlanzerous/purser/internal/model"
)

// inviteResponse is the POST /v1/invites result. The credential block (which
// contains one-time secrets) is included only for copy-paste delivery — for
// email delivery the secrets were sent to the recipient and are not echoed
// back over HTTP.
type inviteResponse struct {
	InviteID        uuid.UUID    `json:"invite_id"`
	Person          personDTO    `json:"person"`
	Delivery        string       `json:"delivery"`
	Delivered       bool         `json:"delivered"`
	Outcomes        []outcomeDTO `json:"outcomes"`
	CredentialBlock string       `json:"credential_block,omitempty"`
}

type personDTO struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Email string    `json:"email,omitempty"`
}

// outcomeDTO is the per-service result. Secrets are never serialized here; they
// live only in the credential block.
type outcomeDTO struct {
	Service      string `json:"service"`
	DisplayName  string `json:"display_name"`
	Status       string `json:"status"`
	Pending      bool   `json:"pending,omitempty"`
	Error        string `json:"error,omitempty"`
	Username     string `json:"username,omitempty"`
	LoginURL     string `json:"login_url,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

func newInviteResponse(res *invite.Result) inviteResponse {
	out := inviteResponse{
		InviteID:  res.InviteID,
		Person:    personDTO{ID: res.Person.ID, Name: res.Person.Name, Email: res.Person.Email},
		Delivery:  string(res.Delivery),
		Delivered: res.Delivered,
	}
	if res.Delivery == model.DeliverCopyPaste {
		out.CredentialBlock = res.CredentialBlock
	}
	for _, o := range res.Outcomes {
		out.Outcomes = append(out.Outcomes, outcomeDTO{
			Service:      o.ServiceKey,
			DisplayName:  o.DisplayName,
			Status:       string(o.Status),
			Pending:      o.Pending,
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
