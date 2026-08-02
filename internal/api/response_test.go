package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/invite"
	"github.com/Einlanzerous/purser/internal/model"
)

// result builds an invite.Result with one succeeded service carrying a secret
// and one failed service carrying an operator-only error.
func result(delivery model.DeliveryMethod) *invite.Result {
	return &invite.Result{
		Person:   model.Person{ID: uuid.New(), Name: "Ada Lovelace", Email: "ada@example.com"},
		InviteID: uuid.New(),
		Delivery: delivery,
		Outcomes: []invite.ServiceOutcome{
			{
				ServiceKey: "switchyard", DisplayName: "Switchyard", Status: model.TaskSucceeded,
				Username: "Ada", Secret: "sw_TOKEN", LoginURL: "https://switchyard.example",
			},
			{
				ServiceKey: "lyceum", DisplayName: "Lyceum", Status: model.TaskFailed,
				Error: "lyceum: 502 from lyceum.internal:8080",
			},
		},
		CredentialBlock: "Hi Ada — …\n    API token: sw_TOKEN\n",
		OperatorNote:    "Operator note — not for the recipient:\n  ✗ Lyceum: lyceum: 502 from lyceum.internal:8080 (failed)\n",
		Delivered:       delivery == model.DeliverEmail,
	}
}

// The block carries one-time secrets, so it comes back only when the caller is
// the one who has to hand it over. On the email path it already went to the
// recipient and must not be echoed over HTTP.
func TestNewInviteResponse_WithholdsCredentialBlockOnEmailDelivery(t *testing.T) {
	body, err := json.Marshal(newInviteResponse(result(model.DeliverEmail)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "sw_TOKEN") {
		t.Errorf("email delivery echoed a one-time secret over HTTP:\n%s", body)
	}
	if strings.Contains(string(body), `"credential_block"`) {
		t.Errorf("credential_block should be omitted entirely on the email path:\n%s", body)
	}
}

func TestNewInviteResponse_ReturnsCredentialBlockOnCopyPaste(t *testing.T) {
	out := newInviteResponse(result(model.DeliverCopyPaste))
	if !strings.Contains(out.CredentialBlock, "sw_TOKEN") {
		t.Errorf("copy-paste delivery needs the block — that's how the operator hands it over: %q", out.CredentialBlock)
	}
}

// The operator note is the caller's, not the invitee's, so it comes back on both
// paths — including the one that withholds the block (PRSR-19).
func TestNewInviteResponse_ReturnsOperatorNoteOnBothPaths(t *testing.T) {
	for _, delivery := range []model.DeliveryMethod{model.DeliverCopyPaste, model.DeliverEmail} {
		out := newInviteResponse(result(delivery))
		if !strings.Contains(out.OperatorNote, "lyceum.internal:8080") {
			t.Errorf("%s: operator note missing the failure: %q", delivery, out.OperatorNote)
		}
	}
}

// Nothing failed => no note, and the field drops out of the JSON rather than
// serializing as an empty string a caller might render as a blank warning.
func TestNewInviteResponse_OmitsEmptyOperatorNote(t *testing.T) {
	res := result(model.DeliverCopyPaste)
	res.Outcomes = res.Outcomes[:1]
	res.OperatorNote = ""

	body, err := json.Marshal(newInviteResponse(res))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "operator_note") {
		t.Errorf("empty note should be omitted:\n%s", body)
	}
}

// A name disagreement is reported, not silently resolved (PRSR-20). Over HTTP
// there is no stderr to warn on, so the field is the only channel.
func TestNewInviteResponse_SurfacesNameConflict(t *testing.T) {
	res := result(model.DeliverCopyPaste)
	res.NameConflict = &invite.NameConflict{
		Email: "ada@example.com", Stored: "Ada Lovelace", Requested: "Ada Lovelacce",
	}
	body, err := json.Marshal(newInviteResponse(res))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"name_conflict"`, `"stored":"Ada Lovelace"`, `"requested":"Ada Lovelacce"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}
}

// No disagreement => the key is absent, so a caller can test for it rather than
// comparing names itself.
func TestNewInviteResponse_OmitsAbsentNameConflict(t *testing.T) {
	body, err := json.Marshal(newInviteResponse(result(model.DeliverCopyPaste)))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "name_conflict") {
		t.Errorf("no conflict, so no key:\n%s", body)
	}
}

// Per-service errors are operator-facing too, but they ride in the structured
// outcomes where a caller can key off them — that's deliberate, and distinct
// from the block, which is the only field that ever reaches an invitee.
func TestNewInviteResponse_OutcomesCarryErrorsButNeverSecrets(t *testing.T) {
	out := newInviteResponse(result(model.DeliverCopyPaste))
	if len(out.Outcomes) != 2 {
		t.Fatalf("want 2 outcomes, got %d", len(out.Outcomes))
	}
	body, err := json.Marshal(out.Outcomes)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "sw_TOKEN") {
		t.Errorf("secrets must never be serialized into outcomes:\n%s", body)
	}
	if !strings.Contains(string(body), "lyceum: 502") {
		t.Errorf("outcomes should carry the connector error:\n%s", body)
	}
}
